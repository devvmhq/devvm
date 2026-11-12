package proxy

import (
	"bytes"
	"errors"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/safefile"
)

// A config devvm writes reads back as it was, and Sum tells any change apart.
func TestConfigRoundTrip(t *testing.T) {
	t.Parallel()
	c := Config{ //nolint:gosec // G101: a test's fake password
		Listen: "127.0.0.1:4000", VM: "devvm-x", Password: "k3y",
		BuiltinPolicies: []string{"layers/base.policy"}, Policies: []string{"/p/policy"},
		Log: "/l/network.jsonl", ProxyLog: "/l/proxy.log", Lifeline: "/h/ha.pid",
		CA: "/ca/ca.pem", CAKey: "/ca/ca.key", CANext: "/ca/next.pem", CANextKey: "/ca/next.key", Secrets: "/r/secrets.json",
		UpstreamCAs: "PEM", Upstream: &Upstream{HTTPS: "http://u:p@corp:3128", NoProxy: ".corp"}, //nolint:gosec // G101: a fake company proxy's fake password
		NixCache: "/n", NixCacheMax: 5, Links: "/r/links.json", LinkSockets: "/run",
		Captures:    []Capture{{Agent: "codex", Host: "auth.openai.com", Path: "/oauth/token"}},
		DevvmSHA256: "aa", CASHA256: "bb", CANextSHA256: "dd",
	}
	got, err := ReadConfig(writeConfig(t, c))
	c.Version = ConfigVersion
	if err != nil || !reflect.DeepEqual(got, c) {
		t.Fatalf("ReadConfig = %+v, %v; want %+v", got, err, c)
	}
	if !bytes.HasSuffix(c.Encode(), []byte("}\n")) || !bytes.Contains(c.Encode(), []byte(`"version": 1`)) {
		t.Errorf("Encode = %s", c.Encode())
	}
	sum := c.Sum()
	for name, change := range map[string]func(*Config){
		"policies": func(c *Config) { c.BuiltinPolicies = append(c.BuiltinPolicies, "shared-tools/node.policy") },
		"binary":   func(c *Config) { c.DevvmSHA256 = "cc" },
		"ca":       func(c *Config) { c.CASHA256 = "cc" },
		"next ca":  func(c *Config) { c.CANextSHA256 = "cc" },
		"cas":      func(c *Config) { c.UpstreamCAs = "PEM2" },
		"upstream": func(c *Config) { c.Upstream = nil },
		"listen":   func(c *Config) { c.Listen = "127.0.0.1:4001" },
		"captures": func(c *Config) { c.Captures = nil },
	} {
		o := c
		change(&o)
		if o.Sum() == sum {
			t.Errorf("%s: the same Sum", name)
		}
	}
	if o := c; func() bool { o.Version = 7; return o.Sum() != sum }() {
		t.Errorf("Sum depends on the Version a config was given; Encode sets it")
	}
}

// withCA is a good config with a CA and the capture c.
func withCA(c Capture) Config {
	return Config{Listen: "127.0.0.1:0", Password: "k3y", CA: "/ca.pem", CAKey: "/ca.key", Captures: []Capture{c}}
}

// ReadConfig refuses a file that isn't devvm's for this proxy alone, of another version, holding a
// field it doesn't know, or one the proxy can't run with: each with an error naming why.
func TestReadConfigRefuses(t *testing.T) {
	t.Parallel()
	good := Config{Listen: "127.0.0.1:0", Password: "k3y"}
	dir := t.TempDir()
	write := func(name string, b []byte, mode os.FileMode) string {
		t.Helper()
		f := filepath.Join(dir, name)
		must(t, os.WriteFile(f, b, 0o600))
		must(t, os.Chmod(f, mode))
		return f
	}
	link := filepath.Join(dir, "link")
	must(t, os.Symlink(write("target", good.Encode(), 0o600), link))
	fifo := filepath.Join(dir, "fifo")
	must(t, syscall.Mkfifo(fifo, 0o600))
	enc := func(c Config) []byte { return c.Encode() }
	for _, c := range []struct {
		name, path string
		want       error
		text       string
	}{
		{"a link", link, safefile.ErrNotRegular, ""},
		{"a FIFO", fifo, safefile.ErrNotRegular, ""},
		{"a folder", dir, safefile.ErrNotRegular, ""},
		{"others may read it", write("0640", good.Encode(), 0o640), ErrNotPrivate, ""},
		{"others may write it", write("0620", good.Encode(), 0o620), ErrNotPrivate, ""},
		{"too long", write("long", append(good.Encode(), bytes.Repeat([]byte(" "), configMax)...), 0o600), safefile.ErrTooLong, ""},
		{"another version", write("v2", bytes.Replace(good.Encode(), []byte(`"version": 1`), []byte(`"version": 2`), 1), 0o600), nil, "version 2, not 1"},
		{"no version", write("v0", []byte(`{"listen":"127.0.0.1:0","password":"k3y"}`), 0o600), nil, "version 0, not 1"},
		{"an unknown field", write("field", []byte(`{"version":1,"listen":"127.0.0.1:0","password":"k3y","auth_file":"/x"}`), 0o600), nil, `unknown field "auth_file"`},
		// Values are looked up in the proxy's own process: no program is named to run for them.
		{"a secret command", write("cmd", []byte(`{"version":1,"listen":"127.0.0.1:0","password":"k3y","secret_cmd":"/bin/devvm"}`), 0o600), nil, `unknown field "secret_cmd"`},
		{"two configs", write("two", append(good.Encode(), good.Encode()...), 0o600), nil, "more than one config"},
		{"not JSON", write("text", []byte("listen=127.0.0.1:0\n"), 0o600), nil, "invalid character"},
		{"no password", write("nopw", enc(Config{Listen: "127.0.0.1:0", Password: " "}), 0o600), nil, "no password"},
		{"a password with a space", write("pwsp", enc(Config{Listen: "127.0.0.1:0", Password: "a b"}), 0o600), nil, "a password with spaces"},
		{"no address", write("nolisten", enc(Config{Password: "k3y"}), 0o600), nil, "isn't a loopback address"},
		{"every address", write("all", enc(Config{Listen: "0.0.0.0:3128", Password: "k3y"}), 0o600), nil, "isn't a loopback address"},
		{"a name", write("name", enc(Config{Listen: "localhost:3128", Password: "k3y"}), 0o600), nil, "isn't a loopback address"},
		{"a cap below 0", write("cap", enc(Config{Listen: "[::1]:0", Password: "k3y", NixCacheMax: -1}), 0o600), nil, "below 0"},
		{"a CA without its key", write("ca", enc(Config{Listen: "127.0.0.1:0", Password: "k3y", CA: "/ca.pem"}), 0o600), nil, "ca and ca_key go together"},
		{"a next CA without its key", write("next", enc(Config{Listen: "127.0.0.1:0", Password: "k3y", CA: "/ca.pem", CAKey: "/ca.key", CANext: "/next.pem"}), 0o600), nil, "ca_next and ca_next_key go together"},
		{"a next CA alone", write("nextonly", enc(Config{Listen: "127.0.0.1:0", Password: "k3y", CANext: "/next.pem", CANextKey: "/next.key"}), 0o600), nil, "ca_next needs ca"},
		{"captures without a CA", write("capca", enc(Config{Listen: "127.0.0.1:0", Password: "k3y", Captures: []Capture{{Agent: "codex", Host: "auth.openai.com", Path: "/oauth/token"}}}), 0o600), nil, "captures need ca"},
		{"a capture without an agent", write("capagent", enc(withCA(Capture{Host: "auth.openai.com", Path: "/oauth/token"})), 0o600), nil, "isn't one"},
		{"a capture at no host", write("caphost", enc(withCA(Capture{Agent: "codex", Host: "auth openai", Path: "/oauth/token"})), 0o600), nil, "isn't one"},
		{"a capture's unclean path", write("cappath", enc(withCA(Capture{Agent: "codex", Host: "auth.openai.com", Path: "/oauth/../token"})), 0o600), nil, "isn't one"},
		{"a capture's relative path", write("caprel", enc(withCA(Capture{Agent: "codex", Host: "auth.openai.com", Path: "oauth/token"})), 0o600), nil, "isn't one"},
		{"a capture's other method", write("capmethod", enc(withCA(Capture{Agent: "cursor", Host: "api.test", Path: "/auth/poll", Method: "PUT"})), 0o600), nil, `method "PUT"`},
		{"a capture outside its guard", write("capguard", enc(withCA(Capture{Agent: "cursor", Host: "api.test", Path: "/auth/poll", Guard: "/login/"})), 0o600), nil, `guard "/login/"`},
		{"a capture's guard without its slash", write("capslash", enc(withCA(Capture{Agent: "cursor", Host: "api.test", Path: "/auth/poll", Guard: "/auth"})), 0o600), nil, `guard "/auth"`},
		{"a capture's unclean guard", write("capclean", enc(withCA(Capture{Agent: "cursor", Host: "api.test", Path: "/auth/poll", Guard: "/x/../auth/"})), 0o600), nil, `guard "/x/../auth/"`},
		{"a capture's empty token key", write("capkey", enc(withCA(Capture{Agent: "cursor", Host: "api.test", Path: "/auth/poll", TokenKeys: []string{""}})), 0o600), nil, "an empty token key"},
		{"missing", filepath.Join(dir, "missing"), os.ErrNotExist, ""},
	} {
		done := make(chan error, 1)
		go func() { _, err := ReadConfig(c.path); done <- err }()
		select {
		case err := <-done:
			switch {
			case err == nil:
				t.Errorf("%s: read", c.name)
			case c.want != nil && !errors.Is(err, c.want):
				t.Errorf("%s: %v, want %v", c.name, err, c.want)
			case c.text != "" && !strings.Contains(err.Error(), c.text):
				t.Errorf("%s: %v, want %q", c.name, err, c.text)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: the read hangs", c.name)
		}
	}
	if _, err := ReadConfig(write("ok", good.Encode(), 0o600)); err != nil {
		t.Errorf("a good config: %v", err)
	}
}

// ReadPolicy follows a link to a regular file, and refuses a FIFO, a device and a file past its cap
// with a named error, without hanging.
func TestReadPolicy(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	f := filepath.Join(dir, "policy")
	must(t, os.WriteFile(f, []byte("a.test\n"), 0o644)) //nolint:gosec // a policy file others may read, as yours may be
	link := filepath.Join(dir, "link")
	must(t, os.Symlink(f, link))
	fifo := filepath.Join(dir, "fifo")
	must(t, syscall.Mkfifo(fifo, 0o600))
	zero := filepath.Join(dir, "zero")
	must(t, os.Symlink("/dev/zero", zero))
	big := filepath.Join(dir, "big")
	must(t, os.WriteFile(big, bytes.Repeat([]byte("a.test\n"), PolicyMax/7+1), 0o600))
	for _, p := range []string{f, link} {
		if b, err := ReadPolicy(p); err != nil || string(b) != "a.test\n" {
			t.Errorf("ReadPolicy(%s) = %q, %v", p, b, err)
		}
	}
	for p, want := range map[string]error{fifo: safefile.ErrNotRegular, zero: safefile.ErrNotRegular, dir: safefile.ErrNotRegular, big: safefile.ErrTooLong} {
		done := make(chan error, 1)
		go func() { _, err := ReadPolicy(p); done <- err }()
		select {
		case err := <-done:
			if !errors.Is(err, want) || !strings.Contains(err.Error(), p) {
				t.Errorf("ReadPolicy(%s) = %v, want %v", p, err, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("ReadPolicy(%s) hangs", p)
		}
	}
}

// A policy file that becomes a FIFO or a link to /dev/zero is refused, not read: the proxy keeps
// its last rules, and logs why once.
func TestPolicyFileNotRegular(t *testing.T) {
	t.Parallel()
	errs := &syncBuffer{}
	f := filepath.Join(t.TempDir(), "policy")
	must(t, os.WriteFile(f, []byte("a.test\n!b.test\n"), 0o600))
	p := newPolicy(nil, []string{f}, nil, log.New(errs, "", 0))
	if ok, _ := p.decide("a.test", 443); !ok {
		t.Fatal("a.test isn't allowed")
	}
	for _, swap := range []func(){
		func() {
			must(t, os.Remove(f))
			must(t, syscall.Mkfifo(f, 0o600))
			later := time.Now().Add(time.Hour) // a new mtime, so it's read again
			must(t, os.Chtimes(f, later, later))
		},
		func() { must(t, os.Remove(f)); must(t, os.Symlink("/dev/zero", f)) },
	} {
		swap()
		for range 3 {
			done := make(chan bool, 1)
			go func() { ok, _ := p.decide("b.test", 443); done <- ok }()
			select {
			case ok := <-done:
				if ok {
					t.Error("the deny was dropped")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the policy's read hangs")
			}
		}
		if ok, _ := p.decide("a.test", 443); !ok {
			t.Error("its last rules weren't kept")
		}
	}
	if n := strings.Count(errs.String(), safefile.ErrNotRegular.Error()); n != 1 {
		t.Errorf("logged %d times, want once: %q", n, errs.String())
	}
}
