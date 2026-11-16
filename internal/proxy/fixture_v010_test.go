package proxy

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/redact"
)

// Every proxy.json devvm 0.1.0 wrote, on each system, reads today, and Encode writes it again
// byte for byte, the password's value in it: its Sum is the same, so no running VM's proxy starts
// again for it.
func TestV010ProxyConfigs(t *testing.T) {
	t.Parallel()
	found, err := filepath.Glob(filepath.Join("..", "..", "testdata", "v0.1.0", "*", "data", "devvm", "vms", "*", "run", "proxy.json"))
	if err != nil || len(found) < 8 {
		t.Fatalf("0.1.0's proxy configs: %q, %v", found, err)
	}
	for _, path := range found {
		b, err := os.ReadFile(path) //nolint:gosec // the repo's own fixture
		if err != nil {
			t.Fatal(err)
		}
		// git keeps no mode, and ReadConfig takes only a file nobody else may read.
		own := filepath.Join(t.TempDir(), "proxy.json")
		if err := os.WriteFile(own, b, 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := ReadConfig(own)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		if c.Password.Empty() {
			t.Errorf("%s: no password", path)
		}
		if got := c.Encode(); !bytes.Equal(got, b) {
			t.Errorf("%s: written again as\n%s\nwant\n%s", path, got, b)
		}
	}
}

// A broker and its child start on every proxy.json devvm 0.1.0 wrote, its paths moved into the
// test's own folder with the files the fixture has (its CAs, bindings and links' list) and its
// port any free one: the child says it's ready, and ends once its broker's end of the socket does.
func TestV010ProxyConfigsStart(t *testing.T) {
	t.Parallel()
	found, err := filepath.Glob(filepath.Join("..", "..", "testdata", "v0.1.0", "*", "data", "devvm", "vms", "*", "run", "proxy.json"))
	if err != nil || len(found) < 8 {
		t.Fatalf("0.1.0's proxy configs: %q, %v", found, err)
	}
	for _, path := range found {
		b, err := os.ReadFile(path) //nolint:gosec // the repo's own fixture
		must(t, err)
		own := filepath.Join(t.TempDir(), "proxy.json")
		must(t, os.WriteFile(own, b, 0o600))
		c, err := ReadConfig(own)
		must(t, err)
		fixture, _, _ := strings.Cut(path, "/data/devvm/vms/") // testdata/v0.1.0/GOOS
		oldRoot, _, ok := strings.Cut(c.ProxyLog, "/data/devvm/vms/")
		if !ok {
			t.Fatalf("%s: a proxy log outside a VM's folder: %s", path, c.ProxyLog)
		}
		root := shortAgentDir(t)
		move := func(p string) string {
			if p == "" {
				return ""
			}
			to := filepath.Join(root, strings.TrimPrefix(p, oldRoot))
			must(t, os.MkdirAll(filepath.Dir(to), 0o700))
			if b, err := os.ReadFile(filepath.Join(fixture, strings.TrimPrefix(p, oldRoot))); err == nil { //nolint:gosec // the repo's own fixture
				must(t, os.WriteFile(to, b, 0o600))
			}
			return to
		}
		c.Listen, c.Lifeline = "127.0.0.1:0", ""
		c.Log, c.ProxyLog, c.Secrets, c.Links = move(c.Log), move(c.ProxyLog), move(c.Secrets), move(c.Links)
		c.CA, c.CAKey, c.CANext, c.CANextKey = move(c.CA), move(c.CAKey), move(c.CANext), move(c.CANextKey)
		c.LinkSockets = root
		for i := range c.Policies {
			c.Policies[i] = move(c.Policies[i])
		}
		for i := range c.DenyPolicies {
			c.DenyPolicies[i] = move(c.DenyPolicies[i])
		}
		std := inProcess(IO{
			Stdout: io.Discard, Stderr: io.Discard, FS: os.DirFS(filepath.Join("..", "..", "assets")), // devvm's own files, as the binary has them
			Secret:  func(context.Context, Binding) (redact.Secret, error) { return redact.NewSecret("v"), nil },
			Capture: capturerFunc(func(Exchange) ([]byte, []byte, error) { return nil, nil, nil }),
		})
		b2, err := newBroker(t.Context(), c, std, quiet())
		if err != nil {
			t.Errorf("%s: the broker: %v", path, err)
			continue
		}
		ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", c.Listen)
		must(t, err)
		p, err := spawnChild(ln, own, std, nil)
		_ = ln.Close()
		must(t, err)
		exe, err := selfSum()
		must(t, err)
		if ready, err := b2.handshake(p, exe, unconfined(WhyUnsupported), time.Now().Add(testStartWait)); err != nil || ready.Addr == "" {
			t.Errorf("%s: the child: %v", path, err)
		}
		_ = p.fc.close()
		select {
		case <-p.child.Done():
		case <-time.After(10 * time.Second):
			t.Errorf("%s: the child outlived its broker's end", path)
		}
	}
}

// devvm 0.1.0's grammar, frozen: its TestParsePolicy's lines and whether each is a rule, and its
// TestDecide's policy and verdicts, as `git show v0.1.0:internal/proxy/proxy_test.go` has them.
// Rules by method and path add to the grammar, so no line here changes meaning; this table is
// its own, so an edit to today's TestParsePolicy can't move it.
func TestV010Rules(t *testing.T) {
	t.Parallel()
	text := "# a comment\n" +
		"\n" +
		"example.com\n" +
		"  *.github.com   # with a comment\n" +
		"example.com:x\n" +
		"api.example.com:0\n" +
		"! bad.example.com\n" +
		"!!worse.example.com\n" +
		"a b\n" +
		"*\n" +
		"a*.example.com\n" +
		"[::1]\n" +
		"fe80::1%eth0\n" +
		"x/y\n" +
		"example.com:+22\n" +
		"*.10.0.0.1\n" +
		"a..example.com\n" +
		"10.0.0.0/8:8080\n" +
		"Example.ORG.\n" +
		"\r\n" +
		"last.example.com"
	want := []PolicyLine{
		{3, "example.com", true},
		{4, "*.github.com", true},
		{5, "example.com:x", false},
		{6, "api.example.com:0", false},
		{7, "! bad.example.com", false},
		{8, "!!worse.example.com", false},
		{9, "a b", false},
		{10, "*", false},
		{11, "a*.example.com", false},
		{12, "[::1]", false},
		{13, "fe80::1%eth0", false},
		{14, "x/y", false},
		{15, "example.com:+22", false},
		{16, "*.10.0.0.1", false},
		{17, "a..example.com", false},
		{18, "10.0.0.0/8:8080", true},
		{19, "Example.ORG.", true},
		{21, "last.example.com", true},
	}
	if got := ParsePolicy(text); !slices.Equal(got, want) {
		t.Errorf("ParsePolicy =\n%v\nwant 0.1.0's\n%v", got, want)
	}
	p := writePolicy(t, `
# comment
example.com
*.github.com          # any depth, not the apex
github.com:22
!evil.github.com
10.1.2.3
192.168.0.0/16:8080
**:9999
`)
	for _, c := range []struct {
		host string
		port int
		want bool
	}{
		{"example.com", 443, true},
		{"EXAMPLE.com", 80, true},
		{"example.com.", 443, true},
		{"example.com", 8443, false},
		{"www.example.com", 443, false},
		{"api.github.com", 443, true},
		{"a.b.github.com", 443, true},
		{"github.com", 443, false},
		{"github.com", 22, true},
		{"evil.github.com", 443, false},
		{"10.1.2.3", 443, true},
		{"10.1.2.4", 443, false},
		{"192.168.5.5", 8080, true},
		{"192.168.5.5", 443, false},
		{"anything.test", 9999, true},
		{"anything.test", 443, false},
	} {
		if got, why := p.decide(c.host, c.port); got != c.want {
			t.Errorf("decide(%s, %d) = %v (%s), want 0.1.0's %v", c.host, c.port, got, why, c.want)
		}
		// None of 0.1.0's rules narrows, so no host's requests are judged one by one.
		if p.byRequest(c.host, c.port) {
			t.Errorf("%s:%d: its requests are judged one by one", c.host, c.port)
		}
	}
}
