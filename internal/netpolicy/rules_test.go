package netpolicy

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"
	"time"

	"github.com/devvmhq/devvm/assets"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/proxy"
	"github.com/devvmhq/devvm/internal/safefile"
)

func TestRule(t *testing.T) {
	for _, c := range []struct {
		in            string
		deny          bool
		methods, path string
		want          string // or the message's names
	}{
		{"example.com", false, "", "", "example.com"},
		{"example.com", true, "", "", "!example.com"},
		{"!example.com", true, "", "", "!example.com"},
		{"[fd00::1]:22", true, "", "", "![fd00::1]:22"},
		{"!example.com", false, "", "", "not_a_rule rule=!example.com"},
		{"a b", false, "", "", "not_a_rule rule=a b"},
		{"api.github.com GET", false, "", "", "not_a_rule rule=api.github.com GET"},
		{"!!x", true, "", "", "not_a_rule rule=!!x"},
		// --method and --path narrow a host's rule, in the proxy's canonical form.
		{"API.github.com", false, "head,get", "/repos/", "api.github.com GET,HEAD /repos/"},
		{"api.github.com", true, "POST,PUT", "", "!api.github.com POST,PUT"},
		{"*.example.com:443", false, "", "/v1/*/x", "*.example.com:443 /v1/*/x"},
		{"example.com", false, "none", "none", "example.com"},
		{"10.0.0.5", false, "GET", "", "rule_not_narrowable rule=10.0.0.5"},
		{"10.0.0.0/8", true, "", "/x", "rule_not_narrowable rule=!10.0.0.0/8"},
		{"**", false, "GET", "", "rule_not_narrowable rule=**"},
		{"example.com", false, "GTE", "", "bad_methods given=GTE methods=" + strings.Join(proxy.Methods(), ", ")},
		{"example.com", false, "CONNECT", "", "bad_methods given=CONNECT methods=" + strings.Join(proxy.Methods(), ", ")},
		{"example.com", false, "", "/a/../b", "bad_request_path given=/a/../b"},
		{"example.com", false, "", "repos", "bad_request_path given=repos"},
	} {
		got, err := Rule(c.in, c.deny, c.methods, c.path)
		if err != nil {
			got = msg.NamesOf(err)
		}
		if got != c.want {
			t.Errorf("Rule(%q, %v, %q, %q) = %q; want %q", c.in, c.deny, c.methods, c.path, got, c.want)
		}
	}
}

// A rule that narrows is the same rule however its methods are written,
// and its host in any case, so add finds it there and rm takes it out; a
// path's case is its own. NarrowedHost names its host for the note.
func TestNarrowedRules(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy")
	if err := os.WriteFile(path, []byte("Api.GitHub.com  HEAD,get  /repos/ # read\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, err := AddRule(path, "api.github.com GET,HEAD /repos/"); ok || err != nil {
		t.Errorf("added again: %v, %v", ok, err)
	}
	if ok, err := AddRule(path, "api.github.com GET,HEAD /Repos/"); !ok || err != nil {
		t.Errorf("another path's case: %v, %v", ok, err)
	}
	if ok, err := RemoveRule(path, "api.github.com GET,HEAD /repos/"); !ok || err != nil {
		t.Errorf("removed: %v, %v", ok, err)
	}
	if b, _ := os.ReadFile(path); string(b) != "api.github.com GET,HEAD /Repos/\n" { //nolint:gosec // the test's own file
		t.Errorf("left %q", b)
	}
	for rule, want := range map[string]string{
		"api.github.com GET": "api.github.com", "!*.example.com:443 /x": "*.example.com", "example.com": "",
	} {
		if h, ok := NarrowedHost(rule); h != want || ok != (want != "") {
			t.Errorf("NarrowedHost(%q) = %q, %v", rule, h, ok)
		}
	}
}

// Adding and removing keep the other lines, comments included, match a
// rule in any case and beside a comment, and write through a link.
func TestAddRemoveRule(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("# mine\nExample.com # site\nother.test"), 0o600); err != nil {
		t.Fatal(err)
	}
	content := func(p string) string {
		b, _ := os.ReadFile(p) //nolint:gosec // the test's own files
		return string(b)
	}
	path := filepath.Join(dir, "sub", "policy")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	step := func(name string, ok, want bool, err error) {
		t.Helper()
		if err != nil || ok != want {
			t.Fatalf("%s: %v, %v; want %v", name, ok, err, want)
		}
	}
	ok, err := AddRule(path, "example.com")
	step("add a rule there", ok, false, err)
	ok, err = AddRule(path, "!bad.test")
	step("add", ok, true, err)
	ok, err = RemoveRule(path, "EXAMPLE.com")
	step("remove", ok, true, err)
	ok, err = RemoveRule(path, "nothere.test")
	step("remove a rule not there", ok, false, err)
	if fi, err := os.Lstat(path); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the link was replaced: %v", err)
	}
	if got, want := content(target), "# mine\nother.test\n!bad.test\n"; got != want {
		t.Errorf("file = %q; want %q", got, want)
	}

	missing := filepath.Join(dir, "new", "policy")
	ok, err = RemoveRule(missing, "x.test")
	step("remove from a missing file", ok, false, err)
	ok, err = AddRule(missing, "x.test")
	step("add to a missing file", ok, true, err)
	if got := content(missing); got != "x.test\n" {
		t.Errorf("new file = %q", got)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ok, err = AddRule(empty, "x.test")
	step("add to an empty file", ok, true, err)
	if got := content(empty); got != "x.test\n" {
		t.Errorf("empty file = %q", got)
	}

	// rm takes every line with the rule, never a commented-out one; lines
	// that aren't rules and CRLF endings are kept as they are.
	mixed := filepath.Join(dir, "mixed")
	if err := os.WriteFile(mixed, []byte("# a.test\na.test\r\nexample.com:x # bad\r\nA.test # again\nb.test"), 0o600); err != nil {
		t.Fatal(err)
	}
	ok, err = AddRule(mixed, "c.test")
	step("add beside a line that isn't a rule", ok, true, err)
	ok, err = RemoveRule(mixed, "a.test")
	step("remove duplicates", ok, true, err)
	if got, want := content(mixed), "# a.test\nexample.com:x # bad\r\nb.test\nc.test\n"; got != want {
		t.Errorf("mixed file = %q; want %q", got, want)
	}
}

// WriteRules lists the built-in policies, by name and labelled as
// devvm's, then the files with rules, comments and blank lines left out,
// and marks each line that isn't a rule with its number; missing and
// comment-only files not at all. ReadRules reads the built-in ones from
// devvm's files alone, never from disk.
func TestWriteRules(t *testing.T) {
	dir := t.TempDir()
	a, b, c := filepath.Join(dir, "a"), filepath.Join(dir, "b"), filepath.Join(dir, "c")
	if err := os.WriteFile(a, []byte("# head\n\nx.test # why\n  !y.test\nexample.com:x\na\x1b[31mb\nquiet q.test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("# only a comment\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	own := fstest.MapFS{"layers/base.policy": {Data: []byte("# devvm's\nbase.test\nquiet q.test\n")}, "shared-tools/none.policy": {Data: []byte("# none\n")}}
	files, err := ReadRules(own, []string{"layers/base.policy", "shared-tools/none.policy"}, []string{a, b, c})
	if err != nil || len(files) != 2 || files[0].Path != "layers/base.policy" || !files[0].Builtin ||
		files[1].Path != a || files[1].Builtin || strings.Join(files[1].Rules(), " ") != "x.test !y.test" {
		t.Fatalf("ReadRules = %+v, %v", files, err)
	}
	if bad := files[1].NotRules(); len(bad) != 3 || bad[0].N != 5 || bad[0].Text != "example.com:x" {
		t.Errorf("NotRules = %+v", bad)
	}
	if q := files[0].Quiet(); len(q) != 1 || q[0] != "q.test" || files[0].NotRules() != nil || files[1].Quiet() != nil {
		t.Errorf("Quiet = %q, %q; NotRules %v", q, files[1].Quiet(), files[0].NotRules())
	}
	// A quiet line of devvm's is marked so; one of the user's isn't a rule, and doctor says why.
	if ws := PolicyWarnings([]string{a}); len(ws) != 3 || ws[2].ID() != "policy_quiet_not_own" {
		t.Errorf("PolicyWarnings = %v", ws)
	}
	var out strings.Builder
	if err := WriteRules(&out, files); err != nil {
		t.Fatal(err)
	}
	// A line's control characters are dropped.
	if want := "# devvm: layers/base.policy\n  base.test\n  quiet q.test" + quietMark + "\n# " + a + "\n  x.test\n  !y.test\n  example.com:x" + notRuleMark + " (line 5)\n" +
		"  a[31mb" + notRuleMark + " (line 6)\n  quiet q.test" + notRuleMark + " (line 7)\n"; out.String() != want {
		t.Errorf("WriteRules = %q; want %q", out.String(), want)
	}
	// A built-in devvm's files lack is an error, whatever is on disk.
	if err := os.MkdirAll(filepath.Join(dir, "layers"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "layers", "gone.policy"), []byte("**\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	if _, err := ReadRules(own, []string{"layers/gone.policy"}, nil); err == nil {
		t.Error("a missing built-in policy was read")
	}
}

// Every policy file devvm ships parses cleanly: each line a rule, a
// comment or nothing. Only the desktop layer's has no rules, as its
// packages come from base's hosts, and its comment says so.
func TestBuiltinPolicies(t *testing.T) {
	n := 0
	err := fs.WalkDir(assets.FS, ".", func(path string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() || filepath.Ext(path) != ".policy" {
			return err
		}
		n++
		b, err := fs.ReadFile(assets.FS, path)
		if err != nil {
			return err
		}
		lines := proxy.ParsePolicy(string(b))
		if len(lines) == 0 && (path != "layers/desktop.policy" || !strings.HasPrefix(string(b), "# desktop: ")) {
			t.Errorf("%s has no rules", path)
		}
		for _, l := range lines {
			if !l.OK && !l.Quiet() {
				t.Errorf("%s:%d isn't a rule: %q", path, l.N, l.Text)
			}
		}
		return nil
	})
	if err != nil || n == 0 {
		t.Fatalf("%d policy files: %v", n, err)
	}
}

// A policy file that's a FIFO, a link to /dev/zero or past the cap is
// refused with a named error, never read: devvm network and doctor don't
// hang on it.
func TestPolicyNotRegular(t *testing.T) {
	dir := t.TempDir()
	fifo, zero, big := filepath.Join(dir, "fifo.policy"), filepath.Join(dir, "zero.policy"), filepath.Join(dir, "big.policy")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/dev/zero", zero); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(big, []byte(strings.Repeat("a.test\n", proxy.PolicyMax/7+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for f, want := range map[string]error{fifo: safefile.ErrNotRegular, zero: safefile.ErrNotRegular, big: safefile.ErrTooLong} {
			if _, err := ReadRules(nil, nil, []string{f}); !errors.Is(err, want) {
				t.Errorf("ReadRules(%s) = %v, want %v", f, err, want)
			}
			if _, err := AddRule(f, "b.test"); !errors.Is(err, want) {
				t.Errorf("AddRule(%s) = %v, want %v", f, err, want)
			}
			if ws := PolicyWarnings([]string{f}); ws != nil {
				t.Errorf("PolicyWarnings(%s) = %v", f, ws)
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("reading a policy that isn't a regular file hangs")
	}
}

// NotRules warns of each line that isn't a rule in the policy files you
// edit: every VM's, each VM's own and your layers'; one that reads as a
// deny warns that its VMs' proxies deny everything.
func TestNotRules(t *testing.T) {
	root := t.TempDir()
	d := paths.Dirs{Data: filepath.Join(root, "data"), Config: filepath.Join(root, "config")}
	files := map[string]string{
		d.Policy():                                "ok.test\na b\n",
		filepath.Join(d.Layers(), "pg.policy"):    "# pg\n*\n",
		d.VM("devvm-x-0123456789abcd").State():    `{"version": 1, "id": "x", "name": "devvm-x-0123456789abcd", "image": "base", "dir": "/x", "backend": "fake"}`,
		d.VM("devvm-x-0123456789abcd").Policy():   "!!x.test\nfine.test\n",
		filepath.Join(d.Layers(), "pg.sh"):        "not a policy\n",
		filepath.Join(d.VMs(), "other", "policy"): "!!not.a.vm\n",
	}
	for f, s := range files {
		if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	for _, m := range NotRules(d) {
		got = append(got, m.Names())
	}
	want := []string{
		"policy_not_rule file=" + d.Policy() + " line=2 rule=a b",
		"policy_not_rule file=" + filepath.Join(d.Layers(), "pg.policy") + " line=2 rule=*",
		"policy_broken_deny file=" + d.VM("devvm-x-0123456789abcd").Policy() + " line=1 rule=!!x.test",
	}
	if !slices.Equal(got, want) {
		t.Errorf("NotRules =\n%q\nwant\n%q", got, want)
	}
}
