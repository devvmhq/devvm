package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
	"unicode/utf8"
)

// quiet is a log that keeps nothing.
func quiet() *log.Logger { return log.New(io.Discard, "devvm proxy: ", 0) }

// policyFiles writes rules to a policy file of the test's own, and returns it and a file that's
// missing, which has no rules.
func policyFiles(t *testing.T, rules string) []string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "policy")
	must(t, os.WriteFile(f, []byte(rules), 0o600))
	return []string{f, filepath.Join(t.TempDir(), "missing")}
}

func writePolicy(t *testing.T, rules string) *policy {
	t.Helper()
	return newPolicy(nil, policyFiles(t, rules), nil, quiet())
}

// decide is p's verdict on host:port without whether a refusal is a broken deny's: the matching
// rule's text, or why not.
func (p *policy) decide(host string, port int) (bool, string) {
	ok, why, _ := p.verdict(host, port)
	return ok, why
}

// byRequest is rulesFor(host, port).byRequest, which the proxy takes from the same rulesFor as
// its verdict (egress.checkHost).
func (p *policy) byRequest(host string, port int) bool { return p.rulesFor(host, port).byRequest() }

// testServer is a proxy of the test's own, made as Main makes one (newServer), from c with a
// policy of rules (and the VM's name "vm" when c has none): it logs nothing, and nothing of it is
// any other test's.
func testServer(t *testing.T, c Config, rules string) *server {
	t.Helper()
	c.Policies = append(c.Policies, policyFiles(t, rules)...)
	if c.VM == "" {
		c.VM = "vm"
	}
	s, err := newServer(c, IO{}, quiet(), io.Discard)
	must(t, err)
	return s
}

// listen serves s for the test, on a port of its own, and returns its address.
func listen(t *testing.T, s *server) string {
	t.Helper()
	p := httptest.NewServer(http.HandlerFunc(s.handler))
	t.Cleanup(p.Close)
	return p.Listener.Addr().String()
}

// logs captures s's event log and its own log lines.
func logs(s *server) *syncBuffer {
	b := &syncBuffer{}
	s.events.mu.Lock()
	s.events.out = b
	s.events.mu.Unlock()
	s.log.SetOutput(b)
	return b
}

func TestNewBlocks(t *testing.T) {
	t.Parallel()
	rs := []numbered{
		{1, Event{Host: "old.test", Port: 443, Reason: "not in the allowlist"}},
		{2, Event{Host: "registry.example.com", Port: 443, Reason: "not in the allowlist"}},
		{3, Event{Host: "registry.example.com", Port: 443, Reason: "not in the allowlist"}},
		{4, Event{Host: "db.test", Port: 5432, Reason: "not in the allowlist"}},
	}
	newer := "4\n" +
		"devvm: blocked registry.example.com:443 (not in the allowlist)\n" +
		"devvm: blocked db.test:5432 (not in the allowlist)\n" +
		"To allow, run this on the host:\n" +
		"devvm network allow registry.example.com db.test:5432\n"
	all := "4\n" +
		"devvm: blocked old.test:443 (not in the allowlist)\n" +
		"devvm: blocked registry.example.com:443 (not in the allowlist)\n" +
		"devvm: blocked db.test:5432 (not in the allowlist)\n" +
		"To allow, run this on the host:\n" +
		"devvm network allow old.test registry.example.com db.test:5432\n"
	for _, c := range []struct{ after, want string }{
		{"1", newer}, // after the prompt that saw block 1: each new rule once
		{"4", "4\n"}, // nothing new
		{"", "4\n"},  // a new shell: only the count
		{"x", "4\n"}, // not a number: the same
		{"9", all},   // past the count: the proxy restarted, so everything it has
	} {
		got := newBlocks(rs, 4, c.after)
		if got != c.want {
			t.Errorf("after=%q:\n%s\nwant:\n%s", c.after, got, c.want)
		}
	}
	// A refused request has no rule to allow it.
	rs = append(rs, numbered{5, Event{Host: "api.github.com", Port: 443, Method: "POST", Path: "/user/keys", Reason: "makes a key", Refused: true}})
	if got, want := newBlocks(rs, 5, "4"), "5\ndevvm: refused POST api.github.com/user/keys (makes a key)\n"; got != want {
		t.Errorf("refused:\n%s\nwant:\n%s", got, want)
	}
}

func TestDecide(t *testing.T) {
	t.Parallel()
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
	cases := []struct {
		host string
		port int
		want bool
	}{
		{"example.com", 443, true},
		{"EXAMPLE.com", 80, true},
		{"example.com.", 443, true},
		{"example.com", 8443, false},    // no port in the rule: 443 and 80 only
		{"www.example.com", 443, false}, // exact only
		{"api.github.com", 443, true},   // *. covers subdomains
		{"a.b.github.com", 443, true},   // at any depth
		{"github.com", 443, false},      // but not the apex
		{"github.com", 22, true},        // port rule
		{"evil.github.com", 443, false}, // deny wins
		{"10.1.2.3", 443, true},         // address rule
		{"10.1.2.4", 443, false},
		{"192.168.5.5", 8080, true}, // cidr with port
		{"192.168.5.5", 443, false},
		{"anything.test", 9999, true}, // ** with a port
		{"anything.test", 443, false},
	}
	for _, c := range cases {
		if got, why := p.decide(c.host, c.port); got != c.want {
			t.Errorf("decide(%s, %d) = %v (%s), want %v", c.host, c.port, got, why, c.want)
		}
	}
}

// ParsePolicy is strict: a line holds one rule, as devvm network allow takes it, a comment, or
// nothing. Every other line is returned as not a rule, with its number, and the proxy skips it.
func TestParsePolicy(t *testing.T) {
	t.Parallel()
	text := "# a comment\n" +
		"\n" +
		"example.com\n" +
		"  *.github.com   # with a comment\n" +
		"example.com:x\n" + // a port that isn't one: never a host named "example.com:x"
		"api.example.com:0\n" +
		"! bad.example.com\n" + // a space after the !
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
		"last.example.com" // no newline at the end
	type line struct {
		n    int
		text string
		ok   bool
	}
	want := []line{
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
	var got []line
	for _, l := range ParsePolicy(text) {
		got = append(got, line{l.N, l.Text, l.OK})
	}
	if !slices.Equal(got, want) {
		t.Errorf("ParsePolicy =\n%v\nwant\n%v", got, want)
	}
	// Each line ParsePolicy takes is ValidRule's, and the other way round.
	for _, l := range ParsePolicy(text) {
		if ValidRule(l.Text) != l.OK {
			t.Errorf("line %d %q: ParsePolicy %v, ValidRule %v", l.N, l.Text, l.OK, ValidRule(l.Text))
		}
	}
}

// The proxy reads its policy files through ParsePolicy: it skips each line that isn't a rule,
// logging the file, the line's number and its text, and keeps the file's rules. A trailing dot
// in a rule names the same host.
func TestPolicySkipsBadLines(t *testing.T) {
	t.Parallel()
	var logged strings.Builder
	f := filepath.Join(t.TempDir(), "policy")
	must(t, os.WriteFile(f, []byte("**\nexample.com:x\n!example.org.\n!::ffff:10.0.0.0/104\n"), 0o600))
	p := newPolicy(nil, []string{f}, nil, log.New(&logged, "", 0))
	if ok, why := p.decide("example.org", 443); ok {
		t.Errorf("example.org allowed (%s): the deny with a trailing dot was skipped", why)
	}
	for _, a := range []string{"10.0.0.1", "::ffff:10.0.0.1"} {
		if !p.addrDenied(netip.MustParseAddr(a), 443) {
			t.Errorf("%s isn't denied by the IPv4-mapped prefix", a)
		}
	}
	if ok, why := p.decide("other.test", 443); !ok {
		t.Errorf("other.test blocked: %s", why)
	}
	if want := f + `:2: not a rule, skipped: "example.com:x"`; !strings.Contains(logged.String(), want) {
		t.Errorf("the log has no %q:\n%s", want, logged.String())
	}
}

// A line that reads as a deny, starting with !, but isn't a rule makes the policy deny
// everything, by name and by address, as a refusal no allow rule would lift, naming the file and
// line; fixing the line lifts it.
func TestPolicyBrokenDeny(t *testing.T) {
	t.Parallel()
	var logged strings.Builder
	dir := t.TempDir()
	good, f := filepath.Join(dir, "good"), filepath.Join(dir, "policy")
	must(t, os.WriteFile(good, []byte("**\n10.0.0.0/8\n"), 0o600))
	must(t, os.WriteFile(f, []byte("ok.test\n! bad.example.com # a space after the !\n"), 0o600))
	p := newPolicy(nil, []string{good, f}, nil, log.New(&logged, "", 0))
	for _, h := range []string{"bad.example.com", "other.test", "10.0.0.1"} {
		ok, why, refused := p.verdict(h, 443)
		if ok || !refused || !strings.Contains(why, f+":2 isn't a rule but reads as a deny") {
			t.Errorf("%s: allowed %v, refused %v (%s)", h, ok, refused, why)
		}
	}
	if !p.addrDenied(netip.MustParseAddr("10.0.0.1"), 443) || p.byName("ok.test", 443) {
		t.Error("an address or a name still allowed past a broken deny")
	}
	if want := f + `:2: not a rule but reads as a deny, so everything is denied: "! bad.example.com"`; !strings.Contains(logged.String(), want) {
		t.Errorf("the log has no %q:\n%s", want, logged.String())
	}
	// Through the proxy: 403, refused, with no allow command.
	s := testServer(t, Config{}, "")
	s.egress.pol = p
	events := logs(s)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodConnect, "http://other.test:443", nil)
	req.Host = "other.test:443"
	w := httptest.NewRecorder()
	s.handler(w, req)
	if w.Code != http.StatusForbidden || strings.Contains(w.Body.String(), "network allow") || !strings.Contains(w.Body.String(), "refused") {
		t.Errorf("answered %d %q", w.Code, w.Body.String())
	}
	if !strings.Contains(events.String(), `"refused":true`) {
		t.Errorf("logged %s", events.String())
	}
	// An absolute-form request too.
	req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://other.test/x", nil)
	w = httptest.NewRecorder()
	s.handler(w, req)
	if w.Code != http.StatusForbidden || strings.Contains(w.Body.String(), "network allow") {
		t.Errorf("absolute form: answered %d %q", w.Code, w.Body.String())
	}
	if p.addrAllowed(netip.MustParseAddr("10.0.0.1"), 443) {
		t.Error("an address rule passes the floor past a broken deny")
	}
	// Fixed, everything the rules allow is allowed again.
	must(t, os.WriteFile(f, []byte("ok.test\n!bad.example.com\n"), 0o600))
	later := time.Now().Add(2 * time.Second)
	must(t, os.Chtimes(f, later, later))
	if ok, why := p.decide("other.test", 443); !ok {
		t.Errorf("other.test still blocked once the line was fixed: %s", why)
	}
	if ok, _ := p.decide("bad.example.com", 443); ok {
		t.Error("bad.example.com allowed once its deny was fixed")
	}
}

// A policy file that goes and comes back with the same mtime (renamed back, restored with its
// times) is read again: its denies, a broken one too, hold again. A leading byte-order mark is
// no part of the first line.
func TestPolicyFileBack(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	f, away := filepath.Join(dir, "policy"), filepath.Join(dir, "away")
	must(t, os.WriteFile(f, []byte("\xef\xbb\xbf!bad.test\n**\n"), 0o600))
	p := newPolicy(nil, []string{f}, nil, quiet())
	if ok, _ := p.decide("bad.test", 443); ok {
		t.Fatal("bad.test allowed: the byte-order mark hid its deny")
	}
	must(t, os.Rename(f, away))
	if ok, _ := p.decide("bad.test", 443); ok {
		t.Fatal("bad.test allowed with no policy file")
	}
	must(t, os.Rename(away, f))
	if ok, why := p.decide("bad.test", 443); ok {
		t.Errorf("bad.test allowed once its file came back (%s)", why)
	}
	// The same with a broken deny, its file back with its old mtime.
	must(t, os.WriteFile(f, []byte("**\n! bad.test\n"), 0o600))
	later := time.Now().Add(2 * time.Second)
	must(t, os.Chtimes(f, later, later))
	if ok, _, refused := p.verdict("other.test", 443); ok || !refused {
		t.Fatal("other.test allowed past a broken deny")
	}
	must(t, os.Rename(f, away))
	p.decide("other.test", 443)
	must(t, os.Rename(away, f))
	if ok, _, refused := p.verdict("other.test", 443); ok || !refused {
		t.Error("other.test allowed once the file with a broken deny came back")
	}
}

func TestPolicyReload(t *testing.T) {
	t.Parallel()
	f := filepath.Join(t.TempDir(), "policy")
	must(t, os.WriteFile(f, []byte("a.test\n"), 0o600))
	p := newPolicy(nil, []string{f}, nil, quiet())
	if ok, _ := p.decide("b.test", 443); ok {
		t.Fatal("b.test allowed before it was added")
	}
	must(t, os.WriteFile(f, []byte("a.test\nb.test\n"), 0o600))
	must(t, os.Chtimes(f, time.Now().Add(time.Second), time.Now().Add(time.Second)))
	if ok, why := p.decide("b.test", 443); !ok {
		t.Fatalf("b.test still blocked after the file changed: %s", why)
	}
	// A file that changed but can't be read (mid-rewrite, say) keeps its last rules: its denies too.
	must(t, os.WriteFile(f, []byte("**\n!c.test\n"), 0o600))
	must(t, os.Chtimes(f, time.Now().Add(2*time.Second), time.Now().Add(2*time.Second)))
	p.decide("x.test", 443)
	must(t, os.Chmod(f, 0))
	must(t, os.Chtimes(f, time.Now().Add(3*time.Second), time.Now().Add(3*time.Second)))
	t.Cleanup(func() { _ = os.Chmod(f, 0o600) })
	if ok, _ := p.decide("c.test", 443); ok {
		t.Error("c.test's deny was dropped while its file couldn't be read")
	}
}

// Built-in policies come from devvm's own files, read once and ahead of the user's files: a
// change to the files after the start changes nothing, and a deny in the user's files still
// beats a built-in allow. One that's missing, or holds a line that isn't a rule, or with no files
// to read it from, is an error.
func TestBuiltinPolicies(t *testing.T) {
	t.Parallel()
	own := fstest.MapFS{
		"layers/base.policy":       {Data: []byte("# devvm's\nbase.test\n")},
		"shared-tools/node.policy": {Data: []byte("node.test\n*.node.test:8443\n")},
		"layers/bad.policy":        {Data: []byte("ok.test\nnot a rule\n")},
	}
	rs, err := readBuiltins(own, []string{"layers/base.policy", "shared-tools/node.policy"}, quiet())
	if err != nil || len(rs) != 3 {
		t.Fatalf("readBuiltins = %v, %v", rs, err)
	}
	f := filepath.Join(t.TempDir(), "policy")
	must(t, os.WriteFile(f, []byte("mine.test\n!node.test\n"), 0o600))
	p := newPolicy(rs, []string{f}, nil, quiet())
	all, _ := p.all()
	var texts []string
	for _, r := range all {
		texts = append(texts, r.text)
	}
	if want := []string{"base.test", "node.test", "*.node.test:8443", "mine.test", "!node.test"}; !slices.Equal(texts, want) {
		t.Errorf("rules = %q, want %q", texts, want)
	}
	own["layers/base.policy"] = &fstest.MapFile{Data: []byte("**\n")}
	for _, c := range []struct {
		host string
		port int
		ok   bool
	}{{"base.test", 443, true}, {"a.node.test", 8443, true}, {"mine.test", 443, true}, {"node.test", 443, false}, {"other.test", 443, false}} {
		if ok, why := p.decide(c.host, c.port); ok != c.ok {
			t.Errorf("%s:%d: allowed %v (%s), want %v", c.host, c.port, ok, why, c.ok)
		}
	}
	for _, c := range []struct {
		own   fs.FS
		names []string
		want  string
	}{
		{own, []string{"layers/missing.policy"}, "built-in policy layers/missing.policy: "},
		{own, []string{"layers/base.policy", "layers/bad.policy"}, `built-in policy layers/bad.policy:2 isn't a rule: "not a rule"`},
		{own, []string{"../policy"}, "built-in policy ../policy: "},
		{nil, []string{"layers/base.policy"}, "built-in policies need devvm's own files"},
	} {
		if _, err := readBuiltins(c.own, c.names, quiet()); err == nil || !strings.HasPrefix(err.Error(), c.want) {
			t.Errorf("readBuiltins(%q) = %v, want %q", c.names, err, c.want)
		}
	}
	if rs, err := readBuiltins(nil, nil, quiet()); err != nil || rs != nil {
		t.Errorf("no built-ins: %v, %v", rs, err)
	}
}

func TestInternal(t *testing.T) {
	t.Parallel()
	own := &hostAddrs{}
	for _, s := range []string{
		"127.0.0.1", "::1", "10.0.0.1", "172.16.3.4", "192.168.1.1", "169.254.169.254",
		"100.64.0.1", "fd00::1", "fe80::1", "0.0.0.0", "::ffff:127.0.0.1", "224.0.0.1",
		"0.1.2.3", "192.0.0.8", "198.18.0.1", "240.0.0.1", "64:ff9b::10.0.0.1", "64:ff9b::127.0.0.1", "64:ff9b:1::1",
		"::7f00:1", "::a00:1", "fec0::1",
	} {
		if !own.internal(netip.MustParseAddr(s)) {
			t.Errorf("%s should be internal", s)
		}
	}
	for _, s := range []string{"1.1.1.1", "140.82.112.3", "2606:4700::1111", "64:ff9b::1.1.1.1"} {
		if own.internal(netip.MustParseAddr(s)) {
			t.Errorf("%s shouldn't be internal", s)
		}
		if !Public(netip.MustParseAddr(s)) {
			t.Errorf("%s should be public", s)
		}
	}
	for _, s := range []string{"10.0.0.1", "::ffff:127.0.0.1", "64:ff9b::10.0.0.1", "fec0::1", "100.64.0.1"} {
		if Public(netip.MustParseAddr(s)) {
			t.Errorf("%s shouldn't be public", s)
		}
	}
}

// A CONNECT's event names the tool that sent it, its User-Agent, as devvm
// network lock shows it.
func TestConnectAgent(t *testing.T) {
	t.Parallel()
	s := testServer(t, Config{}, "")
	events := logs(s)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodConnect, "http://new.test:443", nil)
	req.Host = "new.test:443"
	req.Header.Set("User-Agent", "curl/8.5.0")
	s.handler(httptest.NewRecorder(), req)
	if !strings.Contains(events.String(), `"agent":"curl/8.5.0"`) {
		t.Errorf("logged %s", events.String())
	}
}

// A huge User-Agent and path are cut to eventFieldMax as they're
// logged, at a rune's start, so a VM can't bloat the log; the server
// takes no headers past maxHeaderBytes.
func TestEventFieldsCapped(t *testing.T) {
	t.Parallel()
	s := testServer(t, Config{}, "")
	events := logs(s)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://new.test/"+strings.Repeat("p", 10000), nil)
	req.Header.Set("User-Agent", strings.Repeat("é", 20000))
	s.handler(httptest.NewRecorder(), req)
	if n := len(events.String()); n > 3*eventFieldMax || n == 0 {
		t.Errorf("logged %d bytes", n)
	}
	var e Event
	if err := json.Unmarshal([]byte(strings.TrimSpace(events.String())), &e); err != nil || len(e.Agent) > eventFieldMax || !utf8.ValidString(e.Agent) || len(e.Path) > eventFieldMax {
		t.Errorf("logged %+v, %v", e, err)
	}
	if capField("short") != "short" || maxHeaderBytes != 64<<10 {
		t.Error("capField changed a short field, or the header limit isn't 64 KiB")
	}
}

// An allowlist read as the proxy reads its policies judges a host by its
// rules: the built-in ones, a file's, a deny that beats an allow, and a
// file whose denies alone apply; a built-in policy that isn't there is
// an error.
func TestReadAllowlist(t *testing.T) {
	t.Parallel()
	own := fstest.MapFS{"base.policy": {Data: []byte("github.com\n")}}
	files := policyFiles(t, "example.com:*\n!github.com:22\nother.test:8080\n")
	denies := policyFiles(t, "deniesonly.test\n!example.com:8443\n")
	a, err := ReadAllowlist(own, []string{"base.policy"}, files, denies)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		host string
		port int
		want bool
	}{
		{"github.com", 443, true},
		{"GitHub.com.", 80, true},
		{"github.com", 22, false},
		{"example.com", 443, true},
		{"example.com", 8443, false},
		{"other.test", 8080, true},
		{"other.test", 443, false},
		{"deniesonly.test", 443, false},
		{"new.test", 443, false},
	} {
		if got := a.Allows(c.host, c.port); got != c.want {
			t.Errorf("Allows(%s, %d) = %v, want %v", c.host, c.port, got, c.want)
		}
	}
	if _, err := ReadAllowlist(own, []string{"gone.policy"}, nil, nil); err == nil {
		t.Error("a missing built-in policy: want an error")
	}
	if !a.Denies("github.com", 22) || !a.Denies("example.com", 8443) || a.Denies("new.test", 443) || a.Denies("github.com", 443) {
		t.Error("Denies: a deny isn't one, or what isn't denied is")
	}
	for _, c := range []struct {
		host string
		port int
		want string
	}{{"example.com", 443, "example.com:443"}, {"example.com", 80, "example.com:80"}, {"2001:db8::1", 443, "[2001:db8::1]:443"}, {"bad host", 443, ""}, {"x.test", 0, ""}} {
		got, ok := ExactRule(c.host, c.port)
		if got != c.want || ok != (c.want != "") || ok && !ValidRule(got) {
			t.Errorf("ExactRule(%s, %d) = %q, %v", c.host, c.port, got, ok)
		}
	}
}

// :* is every port, for allows and denies alike, and for addresses too.
func TestAnyPort(t *testing.T) {
	t.Parallel()
	p := writePolicy(t, "**:*\n!evil.test:*\n!blocked.test\n10.1.0.0/16:*\n")
	for _, c := range []struct {
		host string
		port int
		want bool
	}{
		{"example.com", 443, true},
		{"example.com", 22, true},
		{"example.com", 65535, true},
		{"example.com", 1, true},
		{"example.com", 0, false},
		{"example.com", 65536, false},
		{"evil.test", 8443, false},
		{"evil.test", 443, false},
		{"blocked.test", 443, false},
		{"blocked.test", 8080, true}, // a deny without a port: 443 and 80 alone
		{"10.1.2.3", 5432, true},
		{"1.1.1.1", 53, true}, // ** covers addresses; the floor judges them apart
		{"[2606:4700::1111]", 853, true},
	} {
		if got, why := p.decide(c.host, c.port); got != c.want {
			t.Errorf("decide(%s, %d) = %v (%s), want %v", c.host, c.port, got, why, c.want)
		}
	}
	if !p.addrAllowed(netip.MustParseAddr("10.1.2.3"), 5432) || p.addrAllowed(netip.MustParseAddr("10.2.0.1"), 5432) {
		t.Error("an address rule on every port")
	}
	// ** lifts no floor: an internal address it allows is still refused.
	e := testServer(t, Config{}, "**:*\n").egress
	for _, a := range []string{"127.0.0.1", "10.0.0.1", "192.168.1.5", "169.254.169.254", "::1"} {
		if _, err := e.resolve(t.Context(), a, 22); !errors.Is(err, errFloor) {
			t.Errorf("**:* let %s past the floor: %v", a, err)
		}
	}
}

// A file whose denies alone apply: its allows are skipped, its denies
// and a broken deny hold, and the other files' rules apply whole.
func TestDenyPolicies(t *testing.T) {
	t.Parallel()
	full := policyFiles(t, "**:*\n")[:1]
	denies := policyFiles(t, "allowed.test:22\n!evil.test\n10.0.0.5\n")[:1]
	p := newPolicy(nil, full, denies, quiet())
	if ok, _ := p.decide("evil.test", 443); ok {
		t.Error("a deny-only file's deny didn't apply")
	}
	if ok, _ := p.decide("allowed.test", 22); !ok {
		t.Error("the full file's allow didn't apply")
	}
	if p.addrAllowed(netip.MustParseAddr("10.0.0.5"), 443) {
		t.Error("a deny-only file's address allow lifted the floor")
	}
	only := newPolicy(nil, nil, denies, quiet())
	if ok, _ := only.decide("allowed.test", 22); ok {
		t.Error("a deny-only file's allow applied")
	}
	must(t, os.WriteFile(denies[0], []byte("! evil.test\n"), 0o600))
	broken := newPolicy(nil, full, denies, quiet())
	if ok, _ := broken.decide("example.com", 443); ok {
		t.Error("a deny-only file's broken deny didn't deny everything")
	}
}

// An open proxy allows every host and address on every port, this
// machine's and its networks' included, and no rule or floor applies.
func TestOpen(t *testing.T) {
	t.Parallel()
	e := testServer(t, Config{Open: true}, "!example.com\n").egress
	for _, host := range []string{"example.com", "localhost", "127.0.0.1", "10.0.0.1", "192.168.1.5", "::1"} {
		if ok, rule := e.pol.decide(host, 22); !ok || rule != openRule {
			t.Errorf("open: %s blocked (%s)", host, rule)
		}
		if a, err := e.resolve(t.Context(), host, 5432); err != nil || len(a) == 0 {
			t.Errorf("open: %s's floor: %v", host, err)
		}
	}
	if !e.pol.byName("unresolvable.test", 443) || e.pol.addrDenied(netip.MustParseAddr("127.0.0.1"), 22) {
		t.Error("open: a name or an address held back")
	}
	if err := (Config{Open: true, Password: "pw", Listen: "127.0.0.1:0", Policies: []string{"/p"}}).check(); err == nil {
		t.Error("an open config with policies")
	}
}

func TestFloor(t *testing.T) {
	t.Parallel()
	e := testServer(t, Config{}, "**\n10.9.8.7\n").egress
	if _, err := e.resolve(t.Context(), "127.0.0.1", 443); !errors.Is(err, errFloor) {
		t.Errorf("loopback: got %v, want the floor", err)
	}
	if _, err := e.resolve(t.Context(), "10.9.8.6", 443); !errors.Is(err, errFloor) {
		t.Errorf("private: got %v, want the floor", err)
	}
	if a, err := e.resolve(t.Context(), "10.9.8.7", 443); err != nil || len(a) != 1 {
		t.Errorf("an address rule should pass the floor: %v %v", a, err)
	}
	if _, why := e.check(t.Context(), "localhost", 443, false); why == "" {
		t.Error("localhost passed the floor through '**'")
	}
}

// fakeLookup answers egress.lookup with errs in turn, then with 1.1.1.1, counting the calls.
func fakeLookup(calls *int, errs ...error) func(context.Context, string, string) ([]netip.Addr, error) {
	return func(context.Context, string, string) ([]netip.Addr, error) {
		*calls++
		if *calls <= len(errs) {
			return nil, errs[*calls-1]
		}
		return []netip.Addr{netip.MustParseAddr("1.1.1.1")}, nil
	}
}

// A name that doesn't resolve is blocked with why: the host's DNS has no address for it, or didn't
// answer in time, or something else the proxy's own log names. A temporary failure is asked once
// more first.
func TestUnresolvedReason(t *testing.T) {
	t.Parallel()
	notFound := &net.DNSError{Err: "no such host", Name: "x.test", Server: "10.0.0.1:53", IsNotFound: true}
	timeout := &net.DNSError{Err: "i/o timeout", Name: "x.test", Server: "10.0.0.1:53", IsTimeout: true, IsTemporary: true}
	refused := &net.DNSError{Err: "server misbehaving", Name: "x.test", Server: "10.0.0.1:53", IsTemporary: true}
	other := errors.New("lookup x.test: something else")
	for _, c := range []struct {
		errs  []error
		why   string
		calls int
	}{
		{[]error{notFound}, reasonNotFound, 1},
		{[]error{timeout, timeout}, reasonDNSTimeout, 2},
		{[]error{refused, refused}, reasonUnresolvable, 2},
		{[]error{other}, reasonUnresolvable, 1},
		{[]error{timeout}, "", 2}, // the second try answers
		{[]error{refused}, "", 2},
	} {
		s := testServer(t, Config{}, "x.test\n")
		out := logs(s)
		calls := 0
		s.egress.lookup = fakeLookup(&calls, c.errs...)
		if _, why := s.egress.check(t.Context(), "x.test", 443, false); why != c.why || calls != c.calls {
			t.Errorf("%v: %q after %d lookups, want %q after %d", c.errs, why, calls, c.why, c.calls)
		}
		if c.why != "" && !strings.Contains(out.String(), c.errs[0].Error()) {
			t.Errorf("%v: the proxy's log has %q", c.errs, out)
		}
	}
}

// cancelingLookup is a lookup whose client goes away (cancel) while it runs, counting the calls.
func cancelingLookup(cancel context.CancelFunc, calls *int) func(context.Context, string, string) ([]netip.Addr, error) {
	return func(ctx context.Context, _, host string) ([]netip.Addr, error) {
		*calls++
		cancel()
		return nil, &net.DNSError{Err: ctx.Err().Error(), Name: host, IsTemporary: true}
	}
}

// A request whose client went away while its name resolved is no block: neither the event log nor
// /blocked records it. The proxy's own log names it, as a lookup a VM made can carry data out.
func TestCanceledLookupNotBlocked(t *testing.T) {
	t.Parallel()
	s := testServer(t, Config{}, "x.test\n"+nixHost+"\n")
	out := logs(s)
	ctx, cancel := context.WithCancel(t.Context())
	calls := 0
	s.egress.lookup = cancelingLookup(cancel, &calls)
	req := httptest.NewRequestWithContext(ctx, http.MethodConnect, "http://x.test:443", nil)
	req.Host = "x.test:443"
	s.handler(httptest.NewRecorder(), req)
	if _, n := s.events.blocked(); n != 0 || strings.Contains(out.String(), `"event"`) || calls != 1 ||
		strings.Count(out.String(), "x.test:443: "+reasonClientLeft+"\n") != 1 {
		t.Errorf("%d blocks after %d lookups; logged %s", n, calls, out)
	}
	// The shared Nix cache's check gives the context's error, as its other steps do, never a block.
	nctx, ncancel := context.WithCancel(t.Context())
	s.egress.lookup = cancelingLookup(ncancel, &calls)
	if err := s.nix.upstreamAllowed(nctx); !errors.Is(err, context.Canceled) {
		t.Errorf("the Nix cache's check: %v", err)
	}
}

// The proxy's own log names at most abandonedLogMax such lookups a minute, then says it stops.
func TestCanceledLookupsLogCapped(t *testing.T) {
	t.Parallel()
	s := testServer(t, Config{}, "*.test\n")
	out := logs(s)
	calls := 0
	for i := range abandonedLogMax + 5 {
		ctx, cancel := context.WithCancel(t.Context())
		s.egress.lookup = cancelingLookup(cancel, &calls)
		if _, why := s.egress.check(ctx, "n"+strconv.Itoa(i)+".x.test", 443, false); why != reasonClientLeft {
			t.Fatalf("%d: %q", i, why)
		}
	}
	if n := strings.Count(out.String(), reasonClientLeft); n != abandonedLogMax ||
		strings.Count(out.String(), abandonedCapped) != 1 {
		t.Errorf("%d lines; logged %s", n, out)
	}
}

// Through a company proxy, a name's lookup, its one retry included, stays within its 3 seconds: a
// DNS server that never answers isn't asked again once they're up.
func TestFloorUpstreamLookupBudget(t *testing.T) {
	t.Parallel()
	e := testServer(t, Config{}, "x.test\n").egress
	calls := 0
	e.lookup = func(ctx context.Context, _, host string) ([]netip.Addr, error) {
		calls++
		<-ctx.Done()
		return nil, &net.DNSError{Err: "i/o timeout", Name: host, IsTimeout: true, IsTemporary: true}
	}
	start := time.Now()
	if err := e.floorUpstream(t.Context(), "x.test", 443); err != nil || calls != 1 || time.Since(start) > 4*time.Second {
		t.Errorf("%v after %d lookups in %s", err, calls, time.Since(start))
	}
	// One that fails at once is asked once more.
	calls = 0
	e.lookup = fakeLookup(&calls, &net.DNSError{Err: "server misbehaving", Name: "x.test", IsTemporary: true})
	if err := e.floorUpstream(t.Context(), "x.test", 443); err != nil || calls != 2 {
		t.Errorf("%v after %d lookups", err, calls)
	}
}

// A deny beats any allow, for addresses too: a name that resolves to a denied address isn't dialed,
// even when a host rule or a wider address rule allows it.
func TestAddressDenyAppliesToNames(t *testing.T) {
	t.Parallel()
	e := testServer(t, Config{}, "localhost:8080\n127.0.0.0/8:8080\n::1:8080\n!127.0.0.1:8080\n!::1:8080\n").egress
	if ok, _ := e.pol.decide("127.0.0.1", 8080); ok {
		t.Fatal("the denied address itself is allowed")
	}
	if e.pol.addrAllowed(netip.MustParseAddr("127.0.0.1"), 8080) {
		t.Error("addrAllowed ignores the deny")
	}
	if _, why := e.check(t.Context(), "localhost", 8080, false); why == "" {
		t.Error("localhost, which resolves to denied addresses, passed")
	}
	e.pol = writePolicy(t, "**\n!1.1.1.1\n")
	if a, err := e.resolve(t.Context(), "1.1.1.1", 443); err == nil {
		t.Errorf("a denied public address resolved: %v", a)
	}
	// The NAT64 form of a denied IPv4 address (64:ff9b::a.b.c.d, which DNS64 hands out on IPv6-only
	// networks) is denied too, as the floor judges it by the IPv4 address in it.
	e.pol = writePolicy(t, "**\n!203.0.113.0/24\n")
	n := netip.MustParseAddr("64:ff9b::cb00:7105")
	if !e.pol.addrDenied(n, 443) {
		t.Error("the NAT64 form of a denied address isn't denied")
	}
	if a, err := e.resolve(t.Context(), n.String(), 443); err == nil {
		t.Errorf("the NAT64 form of a denied address resolved: %v", a)
	}
}

// With a password, every request needs it (user devvm): CONNECT, absolute-form and devvm.proxy alike.
func TestProxyAuth(t *testing.T) {
	t.Parallel()
	addr := listen(t, testServer(t, Config{Password: "k3y"}, ""))
	basic := func(s string) string { return "Basic " + base64.StdEncoding.EncodeToString([]byte(s)) }
	for _, c := range []struct {
		auth string
		want int
	}{
		{"", http.StatusProxyAuthRequired},
		{basic("devvm:wrong"), http.StatusProxyAuthRequired},
		{basic("other:k3y"), http.StatusProxyAuthRequired},
		{basic("devvm:k3y"), http.StatusOK},
	} {
		for _, req := range []string{"GET http://devvm.proxy/blocked HTTP/1.1\r\nHost: devvm.proxy\r\n", "CONNECT devvm.proxy:443 HTTP/1.1\r\nHost: devvm.proxy:443\r\n"} {
			h := req
			if c.auth != "" {
				h += "Proxy-Authorization: " + c.auth + "\r\n"
			}
			got, _ := roundTrip(t, addr, h+"\r\n")
			if strings.HasPrefix(req, "CONNECT") && c.want == http.StatusOK {
				// Past the password: refused by the (empty) policy instead.
				if got == http.StatusProxyAuthRequired {
					t.Errorf("CONNECT with the password got 407")
				}
				continue
			}
			if got != c.want {
				t.Errorf("%s with %q: %d, want %d", strings.Fields(req)[0], c.auth, got, c.want)
			}
		}
	}
}

// Two proxies in one process share nothing of a VM's: each takes only its own password, logs and
// lists only its own blocks, as its own VM, and keeps its own values, looked up with its own
// lookup, and its own CA's leaves.
func TestServersApart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeCA(t, dir)
	other := filepath.Join(dir, "other")
	must(t, os.Mkdir(other, 0o700))
	writeCA(t, other)
	a := testServer(t, Config{VM: "devvm-a", Password: "pa", CA: filepath.Join(dir, "ca.pem"), CAKey: filepath.Join(dir, "ca.key")}, "")
	b := testServer(t, Config{VM: "devvm-b", Password: "pb", CA: filepath.Join(other, "ca.pem"), CAKey: filepath.Join(other, "ca.key")}, "")
	aEvents, bEvents := logs(a), logs(b)
	aAddr, bAddr := listen(t, a), listen(t, b)
	auth := func(pw string) string { return "Proxy-Authorization: " + authOf(ProxyUser, pw) + "\r\n" }
	if code, _ := roundTrip(t, bAddr, "GET http://devvm.proxy/blocked HTTP/1.1\r\nHost: devvm.proxy\r\n"+auth("pa")+"\r\n"); code != http.StatusProxyAuthRequired {
		t.Errorf("b took a's password: %d", code)
	}
	if code, _ := roundTrip(t, bAddr, "CONNECT blocked.test:443 HTTP/1.1\r\nHost: blocked.test:443\r\n"+auth("pb")+"\r\n"); code != http.StatusForbidden {
		t.Errorf("b's block: %d", code)
	}
	if _, body := roundTrip(t, aAddr, "GET http://devvm.proxy/blocked HTTP/1.1\r\nHost: devvm.proxy\r\n"+auth("pa")+"\r\n"); body != nothingBlocked {
		t.Errorf("a lists b's block: %q", body)
	}
	for _, want := range []string{"skipped the proxy", "proxy setting", "$HTTPS_PROXY", "#a-tool-that-skips-the-proxy-fails"} {
		if !strings.Contains(nothingBlocked, want) {
			t.Errorf("the empty answer lacks %q", want)
		}
	}
	if _, body := roundTrip(t, bAddr, "GET http://devvm.proxy/blocked/new?after=0 HTTP/1.1\r\nHost: devvm.proxy\r\n"+auth("pb")+"\r\n"); !strings.Contains(body, "blocked.test") {
		t.Errorf("b doesn't list its block: %q", body)
	}
	if aEvents.String() != "" || !strings.Contains(bEvents.String(), `"vm":"devvm-b","event":"blocked","host":"blocked.test"`) {
		t.Errorf("events: a %q, b %q", aEvents.String(), bEvents.String())
	}
	fakeSecret(a.secrets, "a-value")
	fakeSecret(b.secrets, "b-value")
	bind := Binding{Name: "t", Kind: KindSecret}
	if v, err := a.secrets.value(bind); v != "a-value" || err != nil {
		t.Errorf("a's value: %q, %v", v, err)
	}
	if v, err := b.secrets.value(bind); v != "b-value" || err != nil {
		t.Errorf("b's value: %q, %v", v, err)
	}
	la, err := a.ca.leafFor("api.example.com")
	must(t, err)
	lb, err := b.ca.leafFor("api.example.com")
	must(t, err)
	if la == lb || la.Leaf.CheckSignatureFrom(a.ca.cert) != nil || lb.Leaf.CheckSignatureFrom(a.ca.cert) == nil {
		t.Error("a leaf of one proxy's CA is the other's")
	}
}

// Hosts are names or addresses; anything else (shell characters, spaces) is refused before the
// policy, the log or the suggested allow command see it.
func TestOddHostsRefused(t *testing.T) {
	t.Parallel()
	s := testServer(t, Config{}, "")
	addr := listen(t, s)
	for _, req := range []string{
		"CONNECT x;sh*:443 HTTP/1.1\r\nHost: x\r\n\r\n",
		"GET http://x;sh*/ HTTP/1.1\r\nHost: x;sh*\r\n\r\n",
		"CONNECT $(id):443 HTTP/1.1\r\nHost: x\r\n\r\n",
		"CONNECT [fe80::1%25$(id)]:443 HTTP/1.1\r\nHost: x\r\n\r\n",
		// Empty labels: a company proxy that drops trailing dots would reach evil.test, which a
		// deny of evil.test never matched under an allow-all **.
		"CONNECT evil.test..:443 HTTP/1.1\r\nHost: x\r\n\r\n",
		"GET http://evil.test../ HTTP/1.1\r\nHost: evil.test..\r\n\r\n",
		"CONNECT evil..test:443 HTTP/1.1\r\nHost: x\r\n\r\n",
		"CONNECT .evil.test:443 HTTP/1.1\r\nHost: x\r\n\r\n",
	} {
		code, body := roundTrip(t, addr, req)
		if code != http.StatusBadRequest || strings.Contains(body, "network allow") {
			t.Errorf("%q: %d %q (want 400, no allow command)", strings.SplitN(req, "\r\n", 2)[0], code, body)
		}
	}
	for _, ok := range []string{"example.com", "a-b.c_d.test", "10.0.0.1", "[fd00::1]", "localhost", "1password.com", "x.0xyz.dev"} {
		if !HostOK(ok) {
			t.Errorf("HostOK(%q) = false", ok)
		}
	}
	// Addresses in shorthand, which a company proxy may read as inet_aton does: never names.
	for _, bad := range []string{"127.1", "2130706433", "0x7f000001", "10.1", "127.0.1.", "a.0x7f"} {
		if HostOK(bad) {
			t.Errorf("HostOK(%q) = true", bad)
		}
	}
	// A name is splitHostPort's form: no trailing dot, and no other empty label.
	for _, bad := range []string{"evil.test.", "evil.test..", "evil..test", ".evil.test", ".", ".."} {
		if HostOK(bad) {
			t.Errorf("HostOK(%q) = true", bad)
		}
	}
	// Under ** with a deny of evil.test, every form with more dots gets 400: the policy trims one
	// dot of its own, so evil.test.. alone (evil.test. to it) would pass the deny.
	s.egress.pol = writePolicy(t, "**\n!evil.test\n")
	if ok, _, _ := s.egress.pol.verdict("evil.test..", 443); !ok {
		t.Errorf("the policy denies evil.test..: then this test doesn't show HostOK refusing it")
	}
	for _, h := range []string{"evil.test..", "evil.test...", "evil.test...."} {
		if code, body := roundTrip(t, addr, "CONNECT "+h+":443 HTTP/1.1\r\nHost: x\r\n\r\n"); code != http.StatusBadRequest {
			t.Errorf("%s under **: %d %q, want 400", h, code, body)
		}
	}
}

func TestAllowRule(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		host string
		port int
		want string
	}{
		{"a.test", 443, "a.test"},
		{"a.test", 80, "a.test"},
		{"db.test", 5432, "db.test:5432"},
		{"fd00::1", 22, "[fd00::1]:22"},
		{"x;sh*", 443, ""},
		{"A.test", 443, ""},
		{"a.test", 0, ""},
		{"a.test", 65536, ""},
	} {
		got, ok := AllowRule(c.host, c.port)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("AllowRule(%q, %d) = %q, %v; want %q", c.host, c.port, got, ok, c.want)
		}
	}
}

func TestValidRule(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		rule string
		ok   bool
	}{
		{"example.com", true},
		{"Example.COM", true},
		{"*.example.com", true},
		{"**", true},
		{"**:22", true},
		{"api.example.com:8443", true},
		{"!bad.example.com", true},
		{"10.0.0.5", true},
		{"10.0.0.0/8:8080", true},
		{"fd00::/8", true},
		{"[fd00::1]:22", true},
		{"![::1]:8080", true},
		{"fd00::1", true},
		{"", false},
		{"!", false},
		{"a b", false},
		{"x;rm", false},
		{"example.com # c", false},
		{"#", false},
		{"*", false},
		{"*.", false},
		{"**.example.com", false},
		{"a*.example.com", false},
		{"!!example.com", false},
		{"example.com:0", false},
		{"example.com:65536", false},
		{"example.com:x", false},
		{"[::1]", false},
		{"fe80::1%eth0", false},
		{"x/y", false},
		{"a\nb", false},
		{"example.com.", true},
		{"example.com:+22", false},
		{"example.com:", false},
		{"*.10.0.0.1", false},
		{"a..example.com", false},
		{".example.com", false},
		{"[example.com]:22", false},
		{"[10.0.0.1]:22", false},
		{"! example.com", false},
		{"example.com ", false},
		{"Kevil.com", false}, // the Kelvin sign, which ToLower folds into k
		{"a\x1b[31mb", false},
		{"::ffff:10.0.0.0/104", true},
		{"::ffff:0.0.0.0/80", false},
		{"127.1", false},
		{"**:*", true},
		{"example.com:*", true},
		{"!*.example.com:*", true},
		{"10.0.0.0/8:*", true},
		{"[fd00::1]:*", true},
		{"example.com:**", false},
		{"example.com:*1", false},
		{"*:*", false},
	} {
		if got := ValidRule(c.rule); got != c.ok {
			t.Errorf("ValidRule(%q) = %v; want %v", c.rule, got, c.ok)
		}
	}
}

// roundTrip sends a raw request to the proxy at addr and returns the response's status and body.
func roundTrip(t *testing.T, addr, req string) (int, string) {
	t.Helper()
	c, err := new(net.Dialer).DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func clientHello(t *testing.T, name string) []byte {
	t.Helper()
	a, b := net.Pipe()
	//nolint:gosec // a test client; it never gets far enough to verify anything
	c := tls.Client(a, &tls.Config{ServerName: name, InsecureSkipVerify: name == ""})
	go func() { _ = c.HandshakeContext(t.Context()) }()
	r := bufio.NewReader(b)
	must(t, b.SetReadDeadline(time.Now().Add(5*time.Second)))
	rec, sni, isTLS := peekClientHello(r)
	_ = a.Close()
	if !isTLS {
		t.Fatal("not recognised as TLS")
	}
	if sni != name {
		t.Errorf("sni = %q, want %q", sni, name)
	}
	return rec
}

func TestClientHello(t *testing.T) {
	t.Parallel()
	if rec := clientHello(t, "api.example.com"); len(rec) < 50 || rec[0] != 0x16 {
		t.Errorf("record not returned for replay: %d bytes", len(rec))
	}
	clientHello(t, "")
	if _, _, isTLS := peekClientHello(bufio.NewReader(bytes.NewReader([]byte("SSH-2.0-OpenSSH_9.9\r\n")))); isTLS {
		t.Error("SSH treated as TLS")
	}
}

func TestSNIMatches(t *testing.T) {
	t.Parallel()
	if !sniMatches("API.example.com", "api.example.com") || sniMatches("other.com", "api.example.com") {
		t.Error("name check")
	}
	if !sniMatches("", "1.2.3.4") || sniMatches("", "example.com") {
		t.Error("empty SNI is only fine for address literals")
	}
}

// writeConfig writes c to a file of its own, as devvm does (Encode, mode 600), and returns its path.
func writeConfig(t *testing.T, c Config) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "proxy.json")
	must(t, os.WriteFile(f, c.Encode(), 0o600))
	return f
}

// Main: -help prints the one flag, -config; bad arguments, a missing config and one that can't run
// are errors, and nothing is served then. The proxy has no other mode: what the host does alone
// (the CA, agents' logins, redaction) isn't reachable from its arguments.
func TestMainRefuses(t *testing.T) {
	t.Parallel()
	var out, errs bytes.Buffer
	std := IO{Stdout: &out, Stderr: &errs}
	if err := Main([]string{"--help"}, std); err != nil || strings.Count("\n"+out.String(), "\n  -") != 1 || !strings.Contains(out.String(), "-config") {
		t.Errorf("--help: %v, %q", err, out.String())
	}
	for _, args := range [][]string{
		{"-nope"},
		{"extra"},
		{},
		{"-listen", "127.0.0.1:0"},
		{"-init-ca", t.TempDir()},
		{"-login", "codex", "-login-file", filepath.Join(t.TempDir(), "auth.json"), "-login-vm"},
		{"-redact", "json"},
		{"-secret-cmd", "/bin/devvm"},
		{"-config", filepath.Join(t.TempDir(), "missing")},
		{"-config", writeConfig(t, Config{Listen: "127.0.0.1:0"})},
		// Bindings without IO.Secret: no value could go in.
		{"-config", writeConfig(t, Config{Listen: "127.0.0.1:0", Password: "k3y", Secrets: filepath.Join(t.TempDir(), "secrets.json")})},
		// The company proxy's password in a secret, without IO.Secret.
		{"-config", writeConfig(t, Config{Listen: "127.0.0.1:0", Password: "k3y", Upstream: &Upstream{HTTPS: "http://u@corp:3128", Secret: UpstreamSecret}})},
	} {
		if err := Main(args, std); err == nil {
			t.Errorf("%q: no error", args)
		}
	}
	// Token endpoints without IO.Capture: no login could be saved, nor the VM given placeholders.
	if err := Main([]string{"-config", writeConfig(t, withCA(Capture{Agent: "codex", Host: "auth.openai.com", Path: "/oauth/token"}))}, std); err == nil ||
		!strings.Contains(err.Error(), "captures without a capturer") {
		t.Errorf("captures without IO.Capture: %v", err)
	}
	// A built-in policy devvm's files lack: a devvm bug, so the proxy never listens.
	std.FS = fstest.MapFS{"layers/base.policy": {Data: []byte("base.test\n")}}
	c := Config{Listen: "127.0.0.1:0", Password: "k3y", BuiltinPolicies: []string{"layers/base.policy", "layers/gone.policy"}}
	if err := Main([]string{"-config", writeConfig(t, c)}, std); err == nil || !strings.Contains(err.Error(), "built-in policy layers/gone.policy") {
		t.Errorf("a missing built-in: %v", err)
	}
	if strings.Contains(errs.String(), "listening on") {
		t.Errorf("the proxy listened: %s", errs.String())
	}
}

// A short run: the proxy listens, logs that it does to stderr, answers with the VM's password, and
// returns once the VM's hostagent (here a sleep) is gone.
func TestMainRuns(t *testing.T) {
	t.Parallel()
	const tick = 50 * time.Millisecond
	dir := t.TempDir()
	pidFile, logFile := filepath.Join(dir, "ha.pid"), filepath.Join(dir, "log.jsonl")
	sleep := exec.CommandContext(t.Context(), "sleep", "60")
	if err := sleep.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sleep.Process.Kill() })
	// The pid, then more on another line, which the lifeline doesn't read (Lima's has the pid alone).
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(sleep.Process.Pid)+"\n1792910265912021"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A free port, given back for the proxy to take.
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	errs := &syncBuffer{}
	done := make(chan error)
	go func() {
		done <- run([]string{"--config", writeConfig(t, Config{
			Listen: addr, VM: "devvm-x", Password: "k3y", Lifeline: pidFile, Log: logFile,
			Policies: []string{filepath.Join(dir, "missing")}, Links: filepath.Join(dir, "devvm-x.links.json"),
			BuiltinPolicies: []string{"layers/base.policy"},
		})}, IO{Stdout: io.Discard, Stderr: errs, FS: fstest.MapFS{"layers/base.policy": {Data: []byte("base.test:8443\n")}}}, tick)
	}()
	pu, _ := url.Parse("http://devvm:k3y@" + addr)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}}
	fetch := func(u string) (int, string, error) {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, u, nil)
		resp, err := client.Do(req)
		if err != nil {
			return 0, "", err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), nil
	}
	var code int
	var body string
	for range 100 {
		if code, body, err = fetch("http://devvm.proxy/blocked"); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	if code != http.StatusOK || body == "" {
		t.Errorf("/blocked: %d %q", code, body)
	}
	if code, _, err := fetch("http://blocked.test/"); err != nil || code != http.StatusForbidden {
		t.Errorf("blocked.test: %d %v", code, err)
	}
	b, _ := os.ReadFile(logFile) //nolint:gosec // the test's own file
	if !strings.Contains(string(b), `"vm":"devvm-x","event":"blocked","host":"blocked.test"`) {
		t.Errorf("event log = %q", b)
	}
	time.Sleep(5 * tick) // the proxy has seen the hostagent
	_ = sleep.Process.Kill()
	_ = sleep.Wait()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Main: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("still running after the hostagent was gone: %s", errs.String())
	}
	if s := errs.String(); !strings.Contains(s, "devvm proxy: ") || !strings.Contains(s, "listening on "+addr+" for devvm-x") ||
		!strings.Contains(s, "hostagent is gone") || !strings.Contains(s, "built-in policy layers/base.policy: 1 rules") {
		t.Errorf("stderr = %q", s)
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// The rule syntax: whatever a policy file holds, parsing doesn't panic, a rule's text parses
// back to the same rule, a host rule never holds a space or a slash, and ParsePolicy marks a line
// a rule exactly when ValidRule takes it.
func FuzzParseRule(f *testing.F) {
	for _, s := range []string{
		"example.com", "*.github.com  # c", "github.com:22", "!evil.github.com", "10.1.2.3",
		"192.168.0.0/16:8080", "**:9999", "[::1]:8080", "::1:8080", "!127.0.0.1:8080", "fd00::/8", "a b", "x/y", "#", ":", "!",
		"api.github.com GET,HEAD /repos/**", "!github.com post /*/*.git/git-receive-pack", "a.test\t/x/", "** GET", "10.0.0.1 GET",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, line string) {
		for _, l := range ParsePolicy(line) {
			if l.OK != ValidRule(l.Text) || l.Text == "" || strings.ContainsAny(l.Text, "#\n") {
				t.Fatalf("%q: line %+v, ValidRule %v", line, l, ValidRule(l.Text))
			}
		}
		r, ok := parseRule(line)
		if !ok {
			if ValidRule(line) {
				t.Fatalf("%q: valid, yet not a rule", line)
			}
			return
		}
		again, ok := parseRule(r.text)
		if !ok || again != r {
			t.Fatalf("%q: %+v, parsed again %+v (%v)", line, r, again, ok)
		}
		if strings.ContainsAny(r.host, " /\\#") {
			t.Fatalf("%q: host %q", line, r.host)
		}
		if h := r.host; h != "" && h != "**" && !strings.HasPrefix(h, "*.") && (!HostOK(h) || !r.matchHost(h)) {
			t.Fatalf("%q: host %q isn't a host it matches", line, h)
		}
		_ = r.matchHost("example.com")
		_ = r.matchAddr(netip.MustParseAddr("64:ff9b::a00:1"))
	})
}

// A request's target, as the handler reads it: one the proxy accepts is a plain host (no shell
// characters, for the log and the allow command it suggests), and that allow command's rule
// allows exactly that host and port.
func FuzzRequest(f *testing.F) {
	for _, s := range []string{
		"CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n",
		"CONNECT [fd00::1]:22 HTTP/1.1\r\nHost: x\r\n\r\n",
		"CONNECT x;sh*:443 HTTP/1.1\r\nHost: x\r\n\r\n",
		"CONNECT [fe80::1%25$(id)]:443 HTTP/1.1\r\nHost: x\r\n\r\n",
		"GET http://Example.COM.:8080/x HTTP/1.1\r\nHost: example.com\r\n\r\n",
		"GET https://10.0.0.1/ HTTP/1.1\r\nHost: 10.0.0.1\r\n\r\n",
		"GET / HTTP/1.1\r\nHost: devvm.proxy\r\n\r\n",
	} {
		f.Add(s)
	}
	dir := f.TempDir()
	n := 0
	f.Fuzz(func(t *testing.T, raw string) {
		r, err := http.ReadRequest(bufio.NewReader(strings.NewReader(raw)))
		if err != nil {
			return
		}
		var host string
		var port int
		if r.Method == http.MethodConnect {
			host, port = splitHostPort(r.Host, 443)
		} else {
			def := 80
			if r.URL.Scheme == "https" {
				def = 443
			}
			host, port = splitHostPort(r.URL.Host, def)
		}
		if !HostOK(host) {
			return
		}
		for _, c := range host {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && !strings.ContainsRune("-_.:[]", c) {
				t.Fatalf("%q: host %q holds %q", raw, host, c)
			}
		}
		if port < 1 || port > 65535 {
			return // never allowed: no rule names such a port
		}
		if !ValidRule(ruleFor(host, port)) {
			t.Fatalf("%q: devvm network allow refuses %q", raw, ruleFor(host, port))
		}
		n++
		file := filepath.Join(dir, strconv.Itoa(n))
		if err := os.WriteFile(file, []byte(ruleFor(host, port)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if ok, why := newPolicy(nil, []string{file}, nil, quiet()).decide(host, port); !ok {
			t.Fatalf("%q: the rule %q doesn't allow %s port %d: %s", raw, ruleFor(host, port), host, port, why)
		}
	})
}

// A tunnel's first bytes: whatever they are, reading a ClientHello doesn't panic or hang, a stream
// whose first byte starts a TLS record counts as TLS, and only a whole record of TLS's version is
// returned.
func FuzzClientHello(f *testing.F) {
	f.Add([]byte("SSH-2.0-OpenSSH_9.9\r\n"))
	f.Add([]byte{0x16, 3, 1, 0, 4, 1, 0, 0, 0})
	f.Add([]byte{0x16, 3, 1, 0xff, 0xff})
	f.Add([]byte{0x16})
	f.Add([]byte{0x16, 2, 1, 0, 1, 0})
	f.Fuzz(func(t *testing.T, b []byte) {
		rec, _, isTLS := peekClientHello(bufio.NewReader(bytes.NewReader(b)))
		if isTLS != (len(b) >= 1 && b[0] == 0x16) {
			t.Fatalf("% x: TLS = %v", b, isTLS)
		}
		if rec != nil && (!isTLS || len(b) < 5 || b[1] != 3) {
			t.Fatalf("% x: a record from a stream that isn't TLS's", b)
		}
	})
}
