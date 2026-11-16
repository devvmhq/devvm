package seatbelt

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"text/template"

	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/proxy"
)

// Sandbox starts a VM's proxy's child under Seatbelt, with sandbox-exec at Exec (the package's
// Exec, which tests change).
type Sandbox struct {
	Exec string
}

// ProxyChild is the command that starts a proxy's child confined to what it needs (n), for
// proxy.IO.Seatbelt: sandbox-exec and the child's profile (ChildProfile), given as an argument,
// so it's never a file on disk. It's fs.ErrNotExist when sandbox-exec isn't there.
func (s Sandbox) ProxyChild(n proxy.ChildNeeds) ([]string, error) {
	if _, err := os.Stat(s.Exec); err != nil {
		return nil, err
	}
	p, err := ChildProfile(n)
	if err != nil {
		return nil, err
	}
	return []string{s.Exec, "-p", p}, nil
}

// childProfile is what a proxy's child's profile allows, every path as Seatbelt sees it.
type childProfile struct {
	Exe    string   // devvm's executable, by its real path
	ExeDir string   // its folder
	Dirs   []string // the folders it writes, resolved
	Files  []string // the files it looks at, resolved
	Links  []string // the links on the way to any of them
}

// The child's profile denies everything it doesn't allow: no read of the home folder or of
// devvm's own folders but its logs and the Nix cache, no Keychain (com.apple.SecurityServer), no
// unix socket but the system's resolver's, no bind, no fork, no exec but its own start, no signal
// but to itself, and no look at another process (its arguments, its path or the process table).
// dyld-support.sb is the system's own rules for starting a program (dyld and the system's
// libraries in their cryptexes). The program's folder's metadata is what CoreFoundation looks at
// for its main bundle, which Security's trust checks need; the trust checks ask trustd
// (com.apple.trustd.agent, and the system's com.apple.trustd), and the resolver asks mDNSResponder
// through its socket. /etc, /var and /tmp are links, which a lookup through them reads. It
// connects out anywhere over TCP and UDP, this machine's loopback included, which the floor
// alone guards: Seatbelt lets an allow with a filter win over a later deny with one of the same
// kind, so the network is allowed unfiltered and the unix sockets denied after. Its folders take
// regular files alone, and nowhere a hard link, a
// folder removed, or a file's flags, ACL, owner, set-id bits or extended attributes changed.
var childTmpl = template.Must(template.New("sb").Funcs(template.FuncMap{"sb": String}).Parse(`(version 1)
(deny default)
(import "dyld-support.sb")
(allow file-read* file-map-executable (subpath "/usr/lib") (subpath "/System/Library"))
(allow process-exec (literal {{sb .Exe}}))
(allow file-read* (literal {{sb .Exe}}))
(allow file-read-metadata (literal {{sb .ExeDir}}))
(allow file-read-metadata (literal "/etc") (literal "/var") (literal "/tmp"))
(allow file-read* (literal "/private/etc/localtime") (subpath "/private/var/db/timezone") (subpath "/usr/share/zoneinfo"))
(allow file-read* (literal "/private/etc/hosts") (literal "/private/etc/resolv.conf") (literal "/private/var/run/resolv.conf") (literal "/private/etc/services"))
(allow sysctl-read)
(deny sysctl-read (sysctl-name-prefix "kern.proc"))
(deny process-info* (target others))
(deny process-info-listpids)
(allow mach-lookup (global-name "com.apple.trustd") (global-name "com.apple.trustd.agent"))
(allow network-outbound)
(deny network-outbound (remote unix-socket))
(allow network-outbound (remote unix-socket (path-literal "/private/var/run/mDNSResponder")))
(allow signal (target self))
{{- range .Dirs}}
(allow file-read* file-write* (subpath {{sb .}}))
(deny file-write-create (require-all (subpath {{sb .}}) (require-not (vnode-type REGULAR-FILE))))
{{- end}}
(deny file-link)
(deny file-write-flags file-write-acl file-write-owner file-write-setugid file-write-xattr)
(deny file-write-unlink (vnode-type DIRECTORY))
{{- range .Files}}
(allow file-read-metadata (literal {{sb .}}))
{{- end}}
{{- range .Links}}
(allow file-read-metadata (literal {{sb .}}))
{{- end}}
`))

// ChildProfile is the Seatbelt profile of a proxy's child that needs n: its executable to run
// and read, its folders to write regular files in, the files it looks at (stat alone), the links
// on the way to each, as they are now, with what the system gives every program (childTmpl). A path that isn't absolute, or that is or resolves to the root,
// which would open the whole disk, is refused.
func ChildProfile(n proxy.ChildNeeds) (string, error) {
	p := childProfile{Exe: n.Exe, ExeDir: filepath.Dir(n.Exe)}
	if err := checkPath(n.Exe, n.Exe); err != nil {
		return "", err
	}
	for _, d := range n.Folders {
		r := paths.Physical(d)
		if err := checkPath(d, r); err != nil {
			return "", err
		}
		p.Dirs = append(p.Dirs, r)
		p.Links = append(p.Links, Links(d)...)
	}
	for _, f := range n.Watched {
		pp := paths.PhysicalParent(f)
		if err := checkPath(f, pp); err != nil {
			return "", err
		}
		p.Files = append(p.Files, pp)
		if r := paths.Physical(f); r != pp {
			p.Files = append(p.Files, r) // a link's target, which a stat that follows it reads
		}
		p.Links = append(p.Links, Links(f)...)
	}
	slices.Sort(p.Links)
	p.Links = slices.Compact(p.Links)
	var b bytes.Buffer
	if err := childTmpl.Execute(&b, p); err != nil {
		panic(err) // the template and its data are devvm's own
	}
	return b.String(), nil
}

// checkPath refuses a path of the child's, p, that isn't absolute, or that is the root as
// resolved (r).
func checkPath(p, r string) error {
	if !filepath.IsAbs(p) || filepath.Clean(r) == "/" || filepath.Clean(p) == "/" {
		return fmt.Errorf("the child's profile can't take the path %q", p)
	}
	return nil
}
