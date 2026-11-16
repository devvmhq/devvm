package lima

import (
	"bytes"
	"text/template"

	"github.com/devvmhq/devvm/internal/seatbelt"
	"github.com/devvmhq/devvm/internal/state"
)

// profile is what a VM's Seatbelt profile allows its Lima hostagent, the
// process that runs the VM and its user-mode network.
type profile struct {
	// Internet: everything but the host's loopback (image builds).
	// Otherwise nothing but what's below.
	Internet bool
	// LimaDir is the VM's folder in Lima's home, whose unix sockets
	// (ssh's control socket, VZ's network socket) Lima uses.
	LimaDir string
	// AgentSocket is the ssh-agent Lima's ssh logs in with and forwards;
	// "" for none.
	AgentSocket string
	SSHPort     int // the VM's ssh forward on host loopback
	ProxyPort   int // devvm's proxy on host loopback, the VM's or a build's; 0 for none
	// NoBinds denies every network bind but the ssh forward's TCP port
	// and unix sockets in LimaDir, so the hostagent forwards no guest
	// port whatever its rules say (devvm's forwarder does). Seatbelt
	// matches a bind by its port alone, on any address (localhost
	// included), resolves a unix socket's path, and can't allow an
	// ephemeral port (0): Lima's host resolver, which binds those, is
	// off where binds are denied (startSets).
	NoBinds bool
}

// The profile allows everything else, so the hostagent runs as usual. No
// DNS: names resolve in the proxy.
var profileTmpl = template.Must(template.New("sb").Funcs(template.FuncMap{"sb": seatbelt.Path}).Parse(`(version 1)
(allow default)
{{- if .Internet}}
(deny network-outbound (remote ip "localhost:*"))
{{- else}}
(deny network-outbound)
(allow network-outbound (remote unix-socket (subpath {{sb .LimaDir}})))
{{- with .AgentSocket}}
(allow network-outbound (remote unix-socket (path-literal {{sb .}})))
{{- end}}
(deny mach-lookup (global-name "com.apple.dnssd.service"))
{{- end}}
(allow network-outbound (remote ip "localhost:{{.SSHPort}}"))
{{- with .ProxyPort}}
(allow network-outbound (remote ip "localhost:{{.}}"))
{{- end}}
{{- if .NoBinds}}
(deny network-bind)
(allow network-bind (local tcp "localhost:{{.SSHPort}}"))
(allow network-bind (local unix-socket (subpath {{sb .LimaDir}})))
{{- end}}
`))

func (p profile) render() string {
	var b bytes.Buffer
	if err := profileTmpl.Execute(&b, p); err != nil {
		panic(err) // the template and its data are devvm's own
	}
	return b.String()
}

// writeProfile writes a VM's profile into the run folder, owner-only.
func writeProfile(path string, p profile) error {
	return state.WriteFile(path, []byte(p.render()))
}
