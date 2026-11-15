package secrets

// Your own secrets (devvm secret set NAME --host HOST): each one's
// definition, its name, the hosts the proxy puts it in for, the header and
// how it may go in there, the paths below which it does, and the VM's
// variable and placeholder's start, is in Dirs.CustomSecrets (yours.go),
// never its value, which is saved where the known secrets' are (save),
// under a key of its own (storageKey). A VM uses one only once it's bound
// to it (BindYours), and its hosts then join the VM's allowlist on 443, as
// the known secrets' do (writeFiles).

import (
	"cmp"
	"net/textproto"
	"slices"
	"strconv"
	"strings"

	"github.com/devvmhq/devvm/internal/agentlogin"
	"github.com/devvmhq/devvm/internal/agents"
	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/proxy"
	"github.com/devvmhq/devvm/internal/shell"
	"github.com/devvmhq/devvm/internal/trust"
)

// DefaultHeader is the header a secret of yours goes in when you name
// none: as Authorization: Bearer PLACEHOLDER, Token PLACEHOLDER, or the
// password of Basic credentials, which the proxy reads too.
const DefaultHeader = "Authorization"

// None is what --scheme, --path and --method take to have none.
const None = "none"

const (
	nameMax   = 40 // a secret of yours' name's length, at most
	prefixMax = 32 // a placeholder's start's length, at most
	headerMax = 64 // a header's name's length, at most
	hostsMax  = 16 // a secret of yours' hosts, at most
)

// Def is a secret of yours' definition as devvm secret set's flags give
// it: its hosts, its header ("": DefaultHeader), its variable, its
// placeholder's start, its own Authorization scheme, the path and the
// methods of the requests it goes in for, and whether it may go in as
// Basic credentials' username.
type Def struct {
	Hosts                       []string
	Header, Env, Prefix, Scheme string
	Path, Methods               string
	BasicUser                   bool
}

// Define returns the secret of yours named name that d defines, each part
// checked: its name (CheckName), its hosts (CheckHost: lower case, each
// once, at least one and at most hostsMax), its header (CheckHeader), its
// variable (CheckEnv), which it needs, as a VM finds its placeholder
// nowhere else, its placeholder's start (CheckPrefix), its scheme
// (proxy.SchemeOK), its path (proxy.PathOK) and its methods (CheckMethods,
// in their canonical form). A scheme and BasicUser go with Authorization
// alone.
func Define(name string, d Def) (Secret, error) {
	if err := CheckName(name); err != nil {
		return Secret{}, err
	}
	sec := Secret{Name: name, Header: DefaultHeader, Env: d.Env, Prefix: d.Prefix, Scheme: d.Scheme, Path: d.Path, Methods: d.Methods, BasicUser: d.BasicUser, Custom: true}
	hosts, err := checkHosts(name, d.Hosts)
	if err != nil {
		return Secret{}, err
	}
	sec.Hosts = hosts
	if d.Header != "" {
		if sec.Header, err = CheckHeader(d.Header); err != nil {
			return Secret{}, err
		}
	}
	if d.Env == "" {
		return Secret{}, msg.SecretNeedsEnv(name)
	}
	if err := CheckEnv(d.Env); err != nil {
		return Secret{}, err
	}
	if d.Prefix != "" {
		if err := CheckPrefix(d.Prefix); err != nil {
			return Secret{}, err
		}
	}
	if err := checkHow(&sec); err != nil {
		return Secret{}, err
	}
	return sec, nil
}

// checkHosts is Define's check of a secret's hosts.
func checkHosts(name string, given []string) ([]string, error) {
	var hosts []string
	for _, g := range given {
		h, err := CheckHost(g)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(hosts, h) {
			hosts = append(hosts, h)
		}
	}
	switch {
	case len(hosts) == 0:
		return nil, msg.SecretNeedsHost(name)
	case len(hosts) > hostsMax:
		return nil, msg.SecretTooManyHosts(strconv.Itoa(hostsMax), name)
	}
	return hosts, nil
}

// checkHow is Define's check of how a secret goes in: its scheme, its
// path, a path of / written as none, as it's every path, its methods,
// which it writes in their canonical form, and that a scheme and
// BasicUser go with Authorization alone.
func checkHow(sec *Secret) error {
	if sec.Scheme != "" {
		if err := CheckScheme(sec.Scheme); err != nil {
			return err
		}
	}
	if sec.Path != "" {
		if err := CheckPath(sec.Path); err != nil {
			return err
		}
	}
	if sec.Path == "/" {
		sec.Path = "" // everything below /, which is every path
	}
	if sec.Methods != "" {
		var err error
		if sec.Methods, err = CheckMethods(sec.Methods); err != nil {
			return err
		}
	}
	if (sec.Scheme != "" || sec.BasicUser) && sec.Header != DefaultHeader {
		return msg.SecretSchemeHeader(sec.Name)
	}
	return nil
}

// Change is a secret of yours as devvm secret set's flags give it: each
// part not given ("" or nil, BasicUser without BasicUserSet) stays as it's
// saved, and a scheme, path or methods of None takes the saved one away.
type Change struct {
	Def
	BasicUserSet bool // --basic-user was given, as BasicUser says
}

// Empty reports whether the flags give nothing: a new value alone.
func (c Change) Empty() bool {
	d := c.Def
	return len(d.Hosts) == 0 && d.Header == "" && d.Env == "" && d.Prefix == "" && d.Scheme == "" && d.Path == "" && d.Methods == "" && !c.BasicUserSet
}

// Apply returns the secret of yours named name with c's parts over saved,
// its definition as it's saved (nil for a new one), checked (Define), its
// ID saved's.
func (c Change) Apply(name string, saved *Secret) (Secret, error) {
	d := c.Def
	if saved != nil {
		if d.Hosts == nil {
			d.Hosts = saved.Hosts
		}
		d.Header = cmp.Or(d.Header, saved.Header)
		d.Env = cmp.Or(d.Env, saved.Env)
		d.Prefix = cmp.Or(d.Prefix, saved.Prefix)
		d.Scheme = cmp.Or(d.Scheme, saved.Scheme)
		d.Path = cmp.Or(d.Path, saved.Path)
		d.Methods = cmp.Or(d.Methods, saved.Methods)
		if !c.BasicUserSet {
			d.BasicUser = saved.BasicUser
		}
	}
	for _, f := range []*string{&d.Scheme, &d.Path, &d.Methods} {
		if *f == None {
			*f = ""
		}
	}
	sec, err := Define(name, d)
	if saved != nil {
		sec.ID = saved.ID
	}
	return sec, err
}

// CheckName checks a name for a secret of yours: lower-case letters,
// digits and dashes, starting and ending with a letter or a digit, at most
// nameMax long, and neither a known secret's (claude, github, proxy) nor a
// coding agent's, alone or before a dash, as the agents' logins' bindings
// are named (codex-key, pi-openai): the proxy's log names each binding.
func CheckName(name string) error {
	ok := name != "" && len(name) <= nameMax && name[0] != '-' && name[len(name)-1] != '-'
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			ok = false
		}
	}
	if !ok {
		return msg.BadSecretName(name, strconv.Itoa(nameMax))
	}
	if _, known := Lookup(name); known {
		return msg.SecretNameTaken(name)
	}
	for _, a := range agents.Names() {
		if name == a || strings.HasPrefix(name, a+"-") {
			return msg.SecretNameTaken(name)
		}
	}
	return nil
}

// nameOK reports whether a secret of yours may have the name (CheckName).
func nameOK(name string) bool { return CheckName(name) == nil }

// CheckHost returns a host a secret of yours goes to, as given and read as
// the proxy reads every host (proxy.NormHost): a host name the proxy takes
// (proxy.HostOK), of two labels or more, on port 443 alone (":443" may
// follow it).
// It refuses a wildcard, which could reach a name anyone may hold under a
// domain (a site's users' own, on a shared host), an address, and a name
// that's only ever internal (internalName).
func CheckHost(given string) (string, error) {
	name := given
	if rest, port, ok := strings.Cut(given, ":"); ok {
		if port != "443" {
			return "", msg.BadSecretHost(given)
		}
		name = rest
	}
	host := proxy.NormHost(name)
	h := host.String()
	if _, isAddr := host.Addr(); isAddr || strings.Contains(h, "*") || !strings.Contains(h, ".") || !host.OK() || !proxy.ValidRule(h) || internalName(h) {
		return "", msg.BadSecretHost(given)
	}
	return h, nil
}

// internalName reports whether h is a name that's only ever internal: its
// last label one that no public name ends in (RFC 2606's, 6761's, 6762's
// and 9476's .alt, and the ones networks, Consul, Kubernetes and Docker
// give themselves), or devvm's own (devvm.proxy, the links'
// devvm.internal), or a name under a domain whose names stand for any
// address, the private ones included (Tailscale's ts.net, nip.io and its
// kin). The proxy's address floor refuses an internal address whatever its
// name, so this keeps a secret from being bound to one at all.
func internalName(h string) bool {
	switch h[strings.LastIndexByte(h, '.')+1:] {
	case "localhost", "local", "localdomain", "internal", "intranet", "lan", "home", "corp", "private", "arpa", "invalid",
		"test", "example", "onion", "alt", "consul", "svc", "docker", "proxy":
		return true
	}
	for _, d := range []string{"ts.net", "nip.io", "sslip.io", "xip.io", "localtest.me", "lvh.me", "local.gd"} {
		if h == d || strings.HasSuffix(h, "."+d) {
			return true
		}
	}
	return false
}

// CheckHeader returns the header a secret of yours goes in, as given, in
// its canonical form (X-Api-Key): a header name of token characters, at
// most headerMax long. It refuses Host, Cookie, the message's own framing
// (Content-Length, Transfer-Encoding, Content-Type, Expect), the proxy's
// (Proxy-*, Via, and the Forwarded and X-Forwarded-* that its reverse
// proxy drops before it puts secrets in), the ones that end at the next
// hop (Connection, Keep-Alive, TE, Trailer, Upgrade, HTTP2-Settings), and
// the ones servers commonly log or send back, which would show the VM the
// value (Origin, Referer, User-Agent, a request ID, a trace's).
func CheckHeader(given string) (string, error) {
	if given == "" || len(given) > headerMax || strings.ContainsFunc(given, func(r rune) bool { return !tokenChar(r) }) {
		return "", msg.BadSecretHeader(given)
	}
	h := textproto.CanonicalMIMEHeaderKey(given)
	switch h {
	case "Host", "Cookie", "Content-Length", "Transfer-Encoding", "Content-Type", "Expect",
		"Connection", "Keep-Alive", "Te", "Trailer", "Upgrade", "Http2-Settings",
		"Forwarded", "Via", "Origin", "Referer", "User-Agent", "X-Request-Id", "X-Correlation-Id", "Traceparent":
		return "", msg.BadSecretHeader(given)
	}
	if strings.HasPrefix(h, "Proxy-") || strings.HasPrefix(h, "X-Forwarded-") {
		return "", msg.BadSecretHeader(given)
	}
	return h, nil
}

// tokenChar reports whether r may be in a header's name (RFC 9110's
// tchar).
func tokenChar(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", r)
}

// CheckScheme checks a secret of yours' own Authorization scheme
// (proxy.SchemeOK), such as ApiKey: what goes before its placeholder.
func CheckScheme(scheme string) error {
	if !proxy.SchemeOK(scheme) {
		return msg.BadSecretScheme(scheme)
	}
	return nil
}

// CheckPath checks a path pattern (proxy.PathOK), such as /api/ for
// everything below /api/: where a secret of yours goes in, or what a rule
// narrows its host to, as --path takes it.
func CheckPath(p string) error {
	if !proxy.PathOK(p) {
		return msg.BadRequestPath(p)
	}
	return nil
}

// CheckMethods checks a list of methods (proxy.ParseMethods), such as
// GET,HEAD: those a secret of yours goes in for, or a rule narrows its host
// to, as --method takes it. It returns the list in its canonical form.
func CheckMethods(given string) (string, error) {
	m, ok := proxy.ParseMethods(given)
	if !ok {
		return "", msg.BadMethods(given, strings.Join(proxy.Methods(), ", "))
	}
	return m, nil
}

// CheckEnv checks the variable a secret of yours gives VMs its placeholder
// in: a shell variable's name (shell.EnvName), and none that devvm or the
// VM's shell sets itself (reservedEnv).
func CheckEnv(env string) error {
	if !shell.EnvName(env) || reservedEnv(env) {
		return msg.BadSecretEnv(env)
	}
	return nil
}

// reservedEnv reports whether a VM's own environment has env, or one that
// changes what a program runs or trusts: devvm's (DEVVM_*, the known
// secrets', the agents' and the toolchains' from their registries, the CAs'
// from trust.Env, the proxy's), guestExport's, the shell's, the loader's,
// and the runtimes' and tools' that load code or certificates.
func reservedEnv(env string) bool {
	up := strings.ToUpper(env)
	for _, p := range []string{"DEVVM_", "LD_", "DYLD_", "BASH_", "LC_", "SSH_", "XDG_", "NPM_CONFIG_", "GIT_", "PYTHON", "PERL5", "MISE_"} {
		if strings.HasPrefix(up, p) {
			return true
		}
	}
	if strings.HasSuffix(up, "_PROXY") || strings.Contains(up, "TELEMETRY") || guestExport(up) {
		return true
	}
	switch up {
	case "PATH", "HOME", "USER", "LOGNAME", "SHELL", "TERM", "PWD", "OLDPWD", "IFS", "ENV", "LANG", "LANGUAGE", "TZ",
		"TMPDIR", "PS0", "PS1", "PS2", "PS4", "PROMPT_COMMAND", "MAIL", "HOSTNAME", "CDPATH", "HISTFILE",
		"EDITOR", "VISUAL", "PAGER", "LESSOPEN", "SUDO_ASKPASS", "HOSTALIASES", "RES_OPTIONS", "LOCALDOMAIN",
		"NODE_OPTIONS", "NODE_PATH", "RUBYOPT", "GCONV_PATH", "SSL_CERT_DIR", "CURL_CA_BUNDLE":
		return true
	}
	if _, ok := trust.Env[up]; ok {
		return true
	}
	var owned []string
	for _, k := range Known {
		owned = append(owned, k.Env)
	}
	for _, v := range agents.GuestEnv() {
		owned = append(owned, v.Name)
	}
	for _, v := range image.GuestEnv() {
		owned = append(owned, v.Name)
	}
	return slices.ContainsFunc(owned, func(o string) bool { return o != "" && strings.EqualFold(o, env) })
}

// guestExport reports whether the VM's own scripts export up (assets'
// layers and guest scripts, which TestReservedEnvAssets reads): the
// telemetry switches are TELEMETRY's, the proxy's _PROXY's.
func guestExport(up string) bool {
	switch up {
	case "BROWSER", "DEBIAN_FRONTEND", "DISPLAY", "DO_NOT_TRACK":
		return true
	}
	return false
}

// CheckPrefix checks what a secret of yours' placeholder starts with,
// such as sk_ for a client that checks a key's start: letters, digits,
// '_', '-' and '.', at most prefixMax long, and no placeholder's mark
// (devvm-proxy-managed), so it can't make another placeholder's shape.
func CheckPrefix(prefix string) error {
	ok := prefix != "" && len(prefix) <= prefixMax && !agentlogin.HasPlaceholder([]byte(prefix))
	for _, r := range prefix {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-' && r != '.' {
			ok = false
		}
	}
	if !ok {
		return msg.BadSecretPrefix(prefix, strconv.Itoa(prefixMax))
	}
	return nil
}
