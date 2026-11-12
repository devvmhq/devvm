//go:build e2e

package e2e

import (
	"flag"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
)

// The harness's own tests, which need no VM: mise run test runs them
// (go test -tags e2e -run '^TestHarness' ./tests/e2e).

func TestHarnessOptions(t *testing.T) {
	secs := []section{{name: "images"}, {name: "gc"}, {name: "desktop"}, {name: "from"}}
	for _, tc := range []struct {
		args    []string
		sel     []string
		every   bool
		wantErr string
	}{
		{args: nil, sel: []string{"images", "gc"}},
		{args: []string{"--only", "desktop,gc"}, sel: []string{"gc", "desktop"}},
		{args: []string{"--from"}, sel: []string{"images", "gc", "desktop", "from"}, every: true},
		{args: []string{"--only=from"}, sel: []string{"from"}, every: true},
		{args: []string{"--only=nope"}, wantErr: `no section "nope"`},
		{args: []string{"--slots=2", "--slot=1"}, wantErr: "--slots runs from slot 0"},
		{args: []string{"--slots", "10"}, wantErr: "1 to 9"},
		{args: []string{"--newest-base", "--slot=1"}, wantErr: "sandbox of its own"},
	} {
		fs := flag.NewFlagSet("e2e", flag.ContinueOnError)
		registerFlags(fs)
		if err := fs.Parse(tc.args); err != nil {
			t.Fatal(err)
		}
		o, err := readOptions(fs, secs)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%v: got %v, want an error with %q", tc.args, err, tc.wantErr)
			}
			continue
		}
		if err != nil || !slices.Equal(o.sel, tc.sel) || o.every != tc.every {
			t.Errorf("%v: got %v %v every=%v, want %v every=%v", tc.args, o.sel, err, o.every, tc.sel, tc.every)
		}
	}
}

func TestHarnessNewestTier(t *testing.T) {
	o := options{every: true, newest: true}
	if got := o.tier(); got != "features-newest" {
		t.Errorf("tier: got %q", got)
	}
}

func TestHarnessState(t *testing.T) {
	f := filepath.Join(t.TempDir(), "vm.json")
	body := `{"name": "devvm-a-0123456789abcd", "chain": "base=ab", "layers": ["docker", "nix"], "none": [],
		"net": {"tier": "allowlist", "on": true}, "links": {"links": [{"name": "db", "port": 80}]}}`
	if err := os.WriteFile(f, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"name": "devvm-a-0123456789abcd", "chain": "base=ab", "layers": "docker nix", "none": "",
		"net.tier": "allowlist", "net.on": "true", "links.links.0.name": "db", "links.links.0.port": "80",
	}
	if got := readState(f); !maps.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if got := readState(filepath.Join(t.TempDir(), "none.json")); len(got) != 0 {
		t.Errorf("a missing file: got %v", got)
	}
}

func TestHarnessPlan(t *testing.T) {
	dir := t.TempDir()
	secs := []section{
		{name: "a", guess: 100},
		{name: "b", guess: 300},
		{name: "c", guess: 200},
		{name: "from", guess: 50},
		{name: "link", guess: 500},
	}
	// a's last complete run took 400 s, more than its guess.
	if err := os.WriteFile(filepath.Join(dir, "a"), []byte("x\t300\ny\t100\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	o := options{sel: []string{"a", "b", "c", "from", "link"}, slots: 2}
	// from+link (550) first, then a (400), b (300), c (200).
	want := map[int][]string{0: {"c", "from", "link"}, 1: {"a", "b"}}
	if got := plan(o, dir, secs); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	o = options{sel: []string{"a"}, slots: 3}
	if got := plan(o, dir, secs); !reflect.DeepEqual(got, map[int][]string{0: {"a"}}) {
		t.Errorf("one section: got %v", got)
	}
}

func TestHarnessLimitsAndLeft(t *testing.T) {
	r := &runner{last: []string{"quick\t10", "slow\t100", "slow\t50", "end\t20"}, later: time.Minute}
	for _, tc := range []struct {
		label  string
		builds bool
		want   time.Duration
	}{
		{"quick", false, limitFloor},
		{"slow", false, 300 * time.Second},
		{"new", false, limitDefault},
		{"quick", true, limitBuild},
	} {
		if got := r.limitOf(tc.label, tc.builds); got != tc.want {
			t.Errorf("limitOf(%q, %v): got %v, want %v", tc.label, tc.builds, got, tc.want)
		}
	}
	if left, ok := r.left("quick"); !ok || left != 170*time.Second+time.Minute {
		t.Errorf("left after quick: got %v %v", left, ok)
	}
	if _, ok := r.left("new"); ok {
		t.Error("left after a check with no time: want none")
	}
}

func TestHarnessCountGuard(t *testing.T) {
	sb := &sandbox{tdir: t.TempDir()}
	f := filepath.Join(sb.tdir, "gc")
	if err := os.WriteFile(f, []byte("a\t1\nb\t2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f+".sum", []byte("go x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &runner{ctx: t.Context(), sb: sb, sum: "go x", cur: "gc", last: readTimings(f), timings: []string{"a\t3"}}
	r.sectionEnd()
	if len(r.short) != 1 || !strings.Contains(r.short[0], "gc ran 1 checks, but its last complete run ran 2") {
		t.Errorf("fewer checks with the same sources: got %v", r.short)
	}
	// Other sources set the count afresh.
	r = &runner{ctx: t.Context(), sb: sb, sum: "go y", cur: "gc", last: readTimings(f), timings: []string{"a\t3"}}
	r.sectionEnd()
	if len(r.short) != 0 || !slices.Equal(readTimings(f), []string{"a\t3"}) {
		t.Errorf("other sources: got %v %v", r.short, readTimings(f))
	}
	if b, _ := os.ReadFile(f + ".sum"); string(b) != "go y\n" {
		t.Errorf("sum: got %q", b)
	}
}

// A check's command past its window's limit: its process group gets TERM,
// and the window says why it ran out.
func TestHarnessTimeLimit(t *testing.T) {
	sb := &sandbox{work: t.TempDir(), env: os.Environ()}
	var b strings.Builder
	r := &runner{ctx: t.Context(), out: newOutput(&b), sb: sb, mark: time.Now()}
	s := &S{r: r, sb: sb}
	w := r.open(300*time.Millisecond, "check")
	start := time.Now()
	res, err := s.Run(Cmd{Args: []string{"sh", "-c", "echo started; sleep 30 & wait; echo late"}})
	if err == nil || time.Since(start) > 10*time.Second || strings.Contains(res.Out, "late") || !res.Has("started") {
		t.Errorf("got %q %v after %v, want it stopped at once", res.Out, err, time.Since(start))
	}
	why, ok := timedOut(w)
	if !ok || !strings.Contains(why, "the check ran past its limit of 0m00s") {
		t.Errorf("timedOut: got %q %v", why, ok)
	}
	if !strings.Contains(b.String(), "time limit reached (0m00s): stopping what the check started") {
		t.Errorf("output: got %q", b.String())
	}
	if _, ok := timedOut(r.open(time.Hour, "setup")); ok {
		t.Error("a new window: want it in time")
	}
}

// A check that needs a feature the backend lacks (none, or emulated but
// unreviewed) is skipped, titled and counted, its function never run; one
// whose features the backend has runs. The fake doctor answers once a run.
func TestHarnessNeeds(t *testing.T) {
	sb := &sandbox{work: t.TempDir(), env: os.Environ()}
	var b strings.Builder
	r := &runner{ctx: t.Context(), out: newOutput(&b), sb: sb, mark: time.Now(), total: "?"}
	asked := 0
	doctor := func() (Res, error) {
		asked++
		return Res{Stdout: `{"backend": "fake", "features": {"fake": {"ports": "native", "ports-all": "emulated", "link": "none", "nix-layer": "unreviewed"}}}`}, nil
	}
	if _, ok := r.lacks([]backend.Feature{backend.FeaturePorts}, doctor); ok {
		t.Fatal("ports: want it there")
	}
	r.open(time.Hour, "setup")
	s := &S{r: r, sb: sb}
	ran := 0
	run := func() error { ran++; return nil }
	s.Needing(backend.FeatureLink).Check("a link reaches its target", run)
	s.Check("nix runs", run, Needs(backend.FeatureNixLayer))
	s.Needing(backend.FeaturePorts).Check("a port reaches this machine", run, Needs(backend.FeaturePortsAll))
	if _, ok := r.lacks([]backend.Feature{backend.FeatureLink}, doctor); !ok || asked != 1 {
		t.Errorf("link: want it lacking, doctor asked once (%d)", asked)
	}
	for _, want := range []string{
		"SKIP  [1/?] a link reaches its target (the fake backend has no link)",
		"SKIP  [2/?] nix runs (the fake backend has no nix-layer)",
		"PASS  [3/?] a port reaches this machine",
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("output: want %q in\n%s", want, b.String())
		}
	}
	if ran != 1 || r.skipped != 2 || r.pass != 1 {
		t.Errorf("ran %d, skipped %d, passed %d: want 1, 2, 1", ran, r.skipped, r.pass)
	}
}

func TestHarnessAnonymous(t *testing.T) {
	in := `{"dir":"/Users/x/p","comment":"X Y","timezone":"Europe/Paris","env":{"A":"1","B":"2"}}`
	want := `{"dir":"/home/me/p","comment":"me","timezone":"UTC","env":{"NAME":"value"}}`
	if got := anonymous(in, "/Users/x"); got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestHarnessEnviron(t *testing.T) {
	got := environ([]string{"PATH=/bin", "SSH_AUTH_SOCK=/a", "DEVVM_MESSAGES=names", "LIMA_HOME=/mine", "GIT_DIR=/repo/.git", "GIT_CONFIG_PARAMETERS=x"},
		map[string]string{"LIMA_HOME": "/sandbox", "A": "1"})
	if want := []string{"PATH=/bin", "A=1", "LIMA_HOME=/sandbox"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestHarnessOutput(t *testing.T) {
	if got := mins(187 * time.Second); got != "3m07s" {
		t.Errorf("mins: got %q", got)
	}
	if got := counts(3, 1, 2, 1); got != "3 passed, 1 failed, 2 skipped, 1 retried" {
		t.Errorf("counts: got %q", got)
	}
	log := filepath.Join(t.TempDir(), "e2e.log")
	if err := os.WriteFile(log, []byte("one\ntw"), 0o600); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	out := newOutput(&b)
	sr := &slotRun{k: 2, log: log}
	sr.show(out, false)
	if err := os.WriteFile(log, []byte("one\ntwo\nthr"), 0o600); err != nil {
		t.Fatal(err)
	}
	sr.show(out, false)
	sr.show(out, true)
	if want := "[2] one\n[2] two\n[2] thr\n"; b.String() != want {
		t.Errorf("show: got %q, want %q", b.String(), want)
	}
	if got := lastLine(log); got != "thr" {
		t.Errorf("lastLine: got %q", got)
	}
}

func TestHarnessLeftovers(t *testing.T) {
	list := "devvm-img-0123abcd\ndevvm-x-0123456789abcd\nmine\ndevvm-img-ffee\n"
	if got, want := leftoverVMs(list), []string{"devvm-x-0123456789abcd"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if got := leftoverVMs("devvm-img-a\n"); len(got) != 0 {
		t.Errorf("images only: got %v", got)
	}
}

func TestHarnessProjectVMs(t *testing.T) {
	p := Paths{Devvm: t.TempDir()}
	for h, body := range map[string]string{
		"a": `{"name": "devvm-lib-a", "dir": "/w/p/web/src/lib"}`,
		"b": `{"name": "devvm-web-b", "dir": "/w/p/web"}`,
		"c": `{"name": "devvm-webx-c", "dir": "/w/p/webx"}`,
		"d": `{"name": "devvm-o-d", "dir": "/elsewhere"}`,
	} {
		d := filepath.Join(p.Devvm, "vms", h)
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "vm.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got := projectVMs(p, "/w/p/web")
	slices.Sort(got)
	if want := []string{"devvm-lib-a", "devvm-web-b"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
