package proxy

import (
	"bytes"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// A `quiet HOST[:PORT]` line names a host by its name alone, and it's never a rule: devvm network
// allow refuses it.
func TestParseQuiet(t *testing.T) {
	t.Parallel()
	for line, quiet := range map[string]bool{
		"quiet api3.cursor.sh": true, "quiet metrics.test:8443": true, "quiet\tm.test": true,
		"quiet **": false, "quiet *.metrics.test": false, "quiet *.metrics.test:443": false, "quiet 10.0.0.1": false, "quiet !m.test": false, "quiet a.test b.test": false,
		"QUIET m.test": false, "quiet m.test GET": false,
	} {
		ls := ParsePolicy(line)
		if len(ls) != 1 || ls[0].Quiet() != quiet || ls[0].OK || ValidRule(line) {
			t.Errorf("%q: %+v, ValidRule %v", line, ls, ValidRule(line))
		}
	}
}

// A built-in policy's quiet host is blocked, as any host no rule allows, and its block is quiet:
// logged, marked so, but never in the VM's notices (/blocked, /blocked/new) and with no command to
// allow it on the 403 page. A rule of the user's that allows it wins, and a quiet line in a file of
// the user's is skipped, never quiet.
func TestQuietHosts(t *testing.T) {
	t.Parallel()
	own := fstest.MapFS{"shared-tools/t.policy": {Data: []byte("tool.test\n# its telemetry\nquiet metrics.test\n")}}
	rs, err := readBuiltins(own, []string{"shared-tools/t.policy"}, quiet())
	if err != nil {
		t.Fatal(err)
	}
	mine := filepath.Join(t.TempDir(), "policy")
	must(t, os.WriteFile(mine, []byte("quiet other.test\n"), 0o600))
	p := newPolicy(rs, []string{mine}, nil, quiet())
	for _, c := range []struct {
		host        string
		port        int
		ok, isQuiet bool
	}{
		{"metrics.test", 443, false, true},
		{"Metrics.TEST.", 80, false, true},
		{"metrics.test", 8443, false, false},
		{"tool.test", 443, true, false},
		{"other.test", 443, false, false},
	} {
		if ok, _ := p.decide(c.host, c.port); ok != c.ok || p.quiet(c.host, c.port) != c.isQuiet {
			t.Errorf("%s:%d: allowed %v, quiet %v", c.host, c.port, ok, p.quiet(c.host, c.port))
		}
	}
	must(t, os.WriteFile(mine, []byte("metrics.test\n"), 0o600))
	if ok, _ := newPolicy(rs, []string{mine}, nil, quiet()).decide("metrics.test", 443); !ok {
		t.Error("an allow of the user's doesn't lift a quiet host")
	}

	var log bytes.Buffer
	s, err := newServer(Config{VM: "vm", BuiltinPolicies: []string{"shared-tools/t.policy"}}, IO{FS: own}, quiet(), &log)
	must(t, err)
	rec := httptest.NewRecorder()
	s.block(rec, Event{Host: "metrics.test", Port: 443, Reason: "not on the allowlist"})
	if body := rec.Body.String(); rec.Code != 403 || strings.Contains(body, "devvm network allow") {
		t.Errorf("the quiet block's page: %d %q", rec.Code, body)
	}
	if !strings.Contains(log.String(), `"quiet":true`) || !strings.Contains(log.String(), `"host":"metrics.test"`) {
		t.Errorf("the quiet block isn't logged as quiet: %s", log.String())
	}
	if rs, n := s.events.blocked(); len(rs) != 0 || n != 0 || newBlocks(rs, n, "0") != "0\n" {
		t.Errorf("the VM is told of a quiet block: %v, %d", rs, n)
	}
	s.block(httptest.NewRecorder(), Event{Host: "other.test", Port: 443, Reason: "not on the allowlist"})
	if rs, n := s.events.blocked(); len(rs) != 1 || n != 1 || rs[0].e.Quiet {
		t.Errorf("another block: %v, %d", rs, n)
	}
}

// A block is quiet only when no rule covers the quiet host: one the user allowed, denied or
// narrowed, or one refused for another reason (its address, a failed connection), is a block as
// any other, with its notice.
func TestQuietOnlyWhenNoRuleAllows(t *testing.T) {
	t.Parallel()
	own := fstest.MapFS{"shared-tools/t.policy": {Data: []byte("quiet metrics.test\n")}}
	for _, c := range []struct {
		rules, reason string
		quiet         bool
	}{
		{"", "not on the allowlist", true},
		{"", "resolves to an internal address", false},
		{"metrics.test\n", "dial tcp: connection refused", false},
		{"!metrics.test\n", "denied by !metrics.test", false},
		{"metrics.test GET /x\n", "not on the allowlist", false},
		{"!metrics.test POST\n", "not on the allowlist", false},
	} {
		s, err := newServer(Config{VM: "vm", BuiltinPolicies: []string{"shared-tools/t.policy"}, Policies: policyFiles(t, c.rules)},
			IO{FS: own}, quiet(), io.Discard)
		must(t, err)
		if got := s.blocked(Event{Host: "metrics.test", Port: 443, Reason: c.reason}).Quiet; got != c.quiet {
			t.Errorf("rules %q, %s: quiet %v", c.rules, c.reason, got)
		}
	}
}
