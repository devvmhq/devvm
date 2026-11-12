package proxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"path"
	"slices"
	"strings"
	"syscall"

	"github.com/devvmhq/devvm/internal/safefile"
)

// --- how devvm runs it ---
//
// devvm runs the proxy as devvm _proxy -config FILE. The file holds everything the proxy runs
// with, the VM's password and the company proxy's credentials among it, so it's in no process's
// arguments, which other users can see: devvm writes it whole, mode 600, in the VM's run folder,
// which only the user can reach, and the proxy reads it once, as it starts, only when it's a
// regular file, not a symbolic link, that only its owner, the user the proxy runs as, may read or
// write, at most configMax long, of ConfigVersion, with no field the proxy doesn't know.

// Config is what a proxy runs with: devvm _proxy -config's file, as JSON. A VM's proxy and a
// build's have the same one.
type Config struct {
	// Version is ConfigVersion: a file of another isn't read. Encode sets it.
	Version int `json:"version"`
	// Listen is the address it listens on: a loopback one, its port 0 for any free one.
	Listen string `json:"listen"`
	// VM is the VM's name, for the log.
	VM string `json:"vm"`
	// Password is the VM's password: every request carries it in Proxy-Authorization, with the
	// user ProxyUser. Required.
	Password string `json:"password"`
	// BuiltinPolicies are devvm's built-in policies, paths in its own files (IO.FS), in order:
	// read once as the proxy starts, ahead of Policies. One that's missing, or holds a line that
	// isn't a rule, is devvm's bug, and the proxy never listens.
	BuiltinPolicies []string `json:"builtin_policies,omitempty"`
	// Policies are the user's policy files, in order, each read again when it changes; a missing
	// one has no rules.
	Policies []string `json:"policies,omitempty"`
	// DenyPolicies are policy files, after Policies, whose denies alone apply: their allows are
	// skipped, a broken deny's refusal of everything included.
	DenyPolicies []string `json:"deny_policies,omitempty"`
	// Open allows every host and address on every port, internal ones and this machine's
	// included: no rule or floor applies, so it takes no policies. Links, secrets and the log
	// work as they do with rules.
	Open bool `json:"open,omitempty"`
	// Log is the JSON-lines event log, rotated past LogLimit; "": IO.Stdout.
	Log string `json:"log,omitempty"`
	// ProxyLog is the file the proxy's stderr appends to, devvm opens it for the process: the
	// proxy's own log lines go through it, rotated past LogLimit by copying.
	ProxyLog string `json:"proxy_log,omitempty"`
	// Lifeline is the pid file of the VM's hostagent (daemon.Lifeline): the proxy returns once it
	// no longer holds the process it followed. "": it runs until it's stopped.
	Lifeline string `json:"lifeline,omitempty"`
	// CA and CAKey are the VM's own CA, for interception: its certificate and key files (PEM).
	// CANext and CANextKey are its next CA's, which the VM trusts too: the proxy signs with it
	// from successorSkew after its start, so the switch needs no new proxy. Each key file must be
	// a regular file, not a symbolic link, that only its owner may read or write.
	CA        string `json:"ca,omitempty"`
	CAKey     string `json:"ca_key,omitempty"`
	CANext    string `json:"ca_next,omitempty"`
	CANextKey string `json:"ca_next_key,omitempty"`
	// Secrets is the VM's bindings file (BindingList, no values), read again when it changes; each
	// binding's value comes from IO.Secret.
	Secrets string `json:"secrets,omitempty"`
	// Captures are agents' OAuth token endpoints, intercepted on :443 with the CA:
	// their replies' logins are saved on the host through IO.Capture, and the VM gets
	// placeholders (capture.go).
	Captures []Capture `json:"captures,omitempty"`
	// UpstreamCAs are CA certificates (PEM) that the proxy's own TLS connections trust besides the
	// system's: the user's.
	UpstreamCAs string `json:"upstream_cas,omitempty"`
	// Upstream is the company proxy that allowed requests go through: nil for none.
	Upstream *Upstream `json:"upstream,omitempty"`
	// UpstreamUnresolved sends names the host can't resolve to Upstream even when only ** allows
	// them: a build's, which reaches internal addresses anyway.
	UpstreamUnresolved bool `json:"upstream_unresolved,omitempty"`
	// NixCache is the shared Nix cache's folder, which the proxy serves at
	// http://devvm.proxy/nix/: "" for none. NixCacheMax is its cap, in bytes (0: NixCacheDefault).
	NixCache    string `json:"nix_cache,omitempty"`
	NixCacheMax int64  `json:"nix_cache_max,omitempty"`
	// Links is the VM's links' list, which devvm _links writes, and LinkSockets the folder of
	// their sockets, devvm's run folder: without both, no link is reached.
	Links       string `json:"links,omitempty"`
	LinkSockets string `json:"link_sockets,omitempty"`
	// DevvmSHA256, CASHA256 and CANextSHA256 are the SHA-256 of the devvm binary, of the VM's CA
	// certificate and of its next CA's it was started with. The proxy doesn't read them: they're
	// in the file so a new binary or a new CA makes another config, and devvm starts the proxy
	// again (Sum).
	DevvmSHA256  string `json:"devvm_sha256,omitempty"`
	CASHA256     string `json:"ca_sha256,omitempty"`
	CANextSHA256 string `json:"ca_next_sha256,omitempty"`
}

// Upstream is the host's proxy settings, as devvm's environment has them (HTTPS_PROXY, HTTP_PROXY,
// NO_PROXY) or config.toml's [proxy]: a company proxy URL (ParseUpstream) for tunnels and one for
// plain HTTP, either "" for none, and the hosts that go directly. Secret names the secret
// (UpstreamSecret) whose value, looked up through IO.Secret as the proxy starts, is the password
// of each URL that names a user and no password: config.toml's, which never holds one.
type Upstream struct {
	HTTPS   string `json:"https,omitempty"`
	HTTP    string `json:"http,omitempty"`
	NoProxy string `json:"no_proxy,omitempty"`
	Secret  string `json:"secret,omitempty"`
}

// UpstreamSecret is the name of the secret devvm keeps the company proxy's password in, for
// config.toml's proxy URLs (devvm secret set proxy).
const UpstreamSecret = "proxy"

// ConfigVersion is the config's version: a file of another isn't read.
const ConfigVersion = 1

// configMax bounds the config the proxy reads.
const configMax = 1 << 20

// Encode is the config as devvm writes it, of ConfigVersion, a line at its end.
func (c Config) Encode() []byte {
	c.Version = ConfigVersion
	// Only strings, numbers and bools: it never fails. The password is meant to be in it: callers
	// write it mode 600, and ReadConfig refuses one anybody else may read.
	b, _ := json.MarshalIndent(c, "", "  ") //nolint:gosec // G117: the config is the password's one file
	return append(b, '\n')
}

// Sum is the SHA-256 of the config as Encode has it, in hex: a proxy whose config's Sum differs
// from the one devvm would start it with now runs with something else, so devvm starts it again.
func (c Config) Sum() string {
	s := sha256.Sum256(c.Encode())
	return hex.EncodeToString(s[:])
}

// ReadConfig reads the config at path as the proxy does: a regular file, not a symbolic link, that
// only its owner, this process's user, may read or write, at most configMax long, of ConfigVersion,
// holding no field Config doesn't have, and one the proxy can run with (check).
func ReadConfig(path string) (Config, error) {
	b, err := readPrivate(path, configMax, 0o077)
	if err != nil {
		return Config{}, err
	}
	var c Config
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	if d.More() {
		return Config{}, fmt.Errorf("%s: more than one config", path)
	}
	if c.Version != ConfigVersion {
		return Config{}, fmt.Errorf("%s: version %d, not %d", path, c.Version, ConfigVersion)
	}
	if err := c.check(); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// check refuses a config the proxy can't run with as it's meant to: without a password, any
// program on the host could use it, and the VM's secrets with it; an address that isn't
// loopback's would be reached from other machines.
func (c Config) check() error {
	if strings.TrimSpace(c.Password) == "" {
		return errors.New("no password")
	}
	if strings.ContainsAny(c.Password, " \t\r\n") {
		return errors.New("a password with spaces")
	}
	if ap, err := netip.ParseAddrPort(c.Listen); err != nil || !ap.Addr().IsLoopback() {
		return fmt.Errorf("listen %q isn't a loopback address and port", c.Listen)
	}
	if c.Open && len(c.BuiltinPolicies)+len(c.Policies)+len(c.DenyPolicies) > 0 {
		return errors.New("open takes no policies")
	}
	if c.NixCacheMax < 0 {
		return errors.New("nix_cache_max is below 0")
	}
	if (c.CA == "") != (c.CAKey == "") {
		return errors.New("ca and ca_key go together")
	}
	if (c.CANext == "") != (c.CANextKey == "") {
		return errors.New("ca_next and ca_next_key go together")
	}
	if c.CANext != "" && c.CA == "" {
		return errors.New("ca_next needs ca")
	}
	return checkCaptures(c.Captures, c.CA != "")
}

// checkCaptures refuses token endpoints the proxy can't capture at: without a CA, which it
// intercepts them with, or without an agent, a host name or a clean path; with a method other than
// POST or GET; with a Guard that isn't a clean path ending in /, or that the endpoint's path isn't
// below, so guard reads its replies; or with an empty token key, which every reply holds.
func checkCaptures(cs []Capture, withCA bool) error {
	for _, x := range cs {
		switch {
		case !withCA:
			return errors.New("captures need ca")
		case x.Agent == "" || !HostOK(x.Host) || !strings.HasPrefix(x.Path, "/") || path.Clean(x.Path) != x.Path:
			return fmt.Errorf("capture %q at %q, %q isn't one", x.Agent, x.Host, x.Path)
		case x.Method != "" && x.Method != http.MethodPost && x.Method != http.MethodGet:
			return fmt.Errorf("capture %q at %q: method %q", x.Agent, x.Host, x.Method)
		case x.Guard != "" && (!strings.HasSuffix(x.Guard, "/") || path.Clean(x.Guard)+"/" != x.Guard && x.Guard != "/" || !x.guards(x.Path)):
			return fmt.Errorf("capture %q at %q: guard %q", x.Agent, x.Host, x.Guard)
		case slices.Contains(x.TokenKeys, ""):
			return fmt.Errorf("capture %q at %q: an empty token key", x.Agent, x.Host)
		}
	}
	return nil
}

// ErrNotPrivate is a read's error for a file that must be its owner's alone, this process's user,
// and isn't: another user owns it, or may write it, or for the config, read it.
var ErrNotPrivate = errors.New("another user may read or change it")

// readPrivate reads a file devvm writes for the proxy alone, as safefile reads it (a regular file,
// not a symbolic link, at most limit bytes long: safefile.ErrNotRegular, safefile.ErrTooLong),
// owned by this process's user, with none of deny's permission bits (0o022: only its owner may
// write it; 0o077: nobody else may read it either): ErrNotPrivate.
func readPrivate(path string, limit int64, deny os.FileMode) ([]byte, error) {
	f, err := safefile.Open(path, safefile.Opts{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if f.Info.Mode().Perm()&deny != 0 || !ownedByMe(f.Info) {
		return nil, fmt.Errorf("%s: %w", path, ErrNotPrivate)
	}
	return f.ReadAll(limit)
}

// ReadPrivate is readPrivate for a file of devvm's that decides where the
// proxy puts a value, read outside this package: internal/secrets' list of
// your secrets, which only its owner may write (deny 0o022).
func ReadPrivate(path string, limit int64, deny os.FileMode) ([]byte, error) {
	return readPrivate(path, limit, deny)
}

// ownedByMe reports whether fi's file is this process's user's.
func ownedByMe(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Geteuid()
}
