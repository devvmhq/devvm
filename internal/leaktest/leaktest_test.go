package leaktest

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// helperEnv makes the test binary a helper (leak).
const helperEnv = "LEAKTEST_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "leak" {
		os.Exit(leak()) //nolint:forbidigo // the test binary as a helper process
	}
	Main(m)
}

// A dump as runtime.Stack prints it: the caller first, then a goroutine
// blocked in a channel receive that goroutine 7 started, and signal's loop.
const dump = `goroutine 9 [running]:
github.com/devvmhq/devvm/internal/leaktest.stacks()
	/src/leaktest.go:80 +0x2c
created by testing.(*T).Run in goroutine 1
	/go/src/testing/testing.go:1997 +0x3c

goroutine 12 [chan receive, 2 minutes]:
github.com/devvmhq/devvm/internal/vmssh.(*Links).serve(0x14000123, {0x1, 0x2})
	/src/links.go:300 +0x40
github.com/devvmhq/devvm/internal/vmssh.(*Links).Run.func1()
	/src/links.go:265 +0x20
created by github.com/devvmhq/devvm/internal/vmssh.(*Links).Run in goroutine 7
	/src/links.go:264 +0x10

goroutine 5 [select, locked to thread]:
os/signal.signal_recv()
	/go/src/runtime/sigqueue.go:152 +0x2c
os/signal.loop()
	/go/src/os/signal/signal_unix.go:23 +0x1c
...additional frames elided...
created by os/signal.Notify.func1.1 in goroutine 1
	/go/src/os/signal/signal.go:151 +0x28
`

func TestParse(t *testing.T) {
	gs := parse([]byte(dump))
	if len(gs) != 3 {
		t.Fatalf("parse found %d goroutines, want 3", len(gs))
	}
	want := []goroutine{
		{id: 9, parent: 1, funcs: []string{"github.com/devvmhq/devvm/internal/leaktest.stacks", "testing.(*T).Run"}},
		{id: 12, parent: 7, funcs: []string{
			"github.com/devvmhq/devvm/internal/vmssh.(*Links).serve",
			"github.com/devvmhq/devvm/internal/vmssh.(*Links).Run.func1",
			"github.com/devvmhq/devvm/internal/vmssh.(*Links).Run",
		}},
		{id: 5, parent: 1, funcs: []string{"os/signal.signal_recv", "os/signal.loop", "os/signal.Notify.func1.1"}},
	}
	for i, g := range gs {
		if g.id != want[i].id || g.parent != want[i].parent || !slices.Equal(g.funcs, want[i].funcs) {
			t.Errorf("goroutine %d: got %d from %d %q, want %d from %d %q", i, g.id, g.parent, g.funcs, want[i].id, want[i].parent, want[i].funcs)
		}
		if !strings.HasPrefix(g.stack, "goroutine ") {
			t.Errorf("goroutine %d's stack: %q", i, g.stack)
		}
	}
	if isAllowed(gs[1]) || !isAllowed(gs[2]) {
		t.Errorf("allowed: links' %v, signal's %v; want false, true", isAllowed(gs[1]), isAllowed(gs[2]))
	}
}

// A goroutine's number, or its parent's, that parse can't read panics,
// rather than reading as 0, which would cost a goroutine its parent; a
// "created by" line without a parent's number is no parent.
func TestParseUnknownFormat(t *testing.T) {
	if gs := parse([]byte("goroutine 4 [running]:\nmain.f()\ncreated by main.g\n")); len(gs) != 1 || gs[0].parent != 0 {
		t.Errorf("no parent's number: %+v", gs)
	}
	for _, dump := range []string{
		"goroutine x [running]:\nmain.f()\n",
		"goroutine 4 [running]:\nmain.f()\ncreated by main.g in goroutine one\n",
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("parse(%q): no panic", dump)
				}
			}()
			parse([]byte(dump))
		}()
	}
}

// A test's goroutines are those it started, theirs through those still
// running, and those started while it ran by one that has ended; not
// those running before it, theirs, the allowed ones, or another test's.
// IDs say nothing of when: each thread takes a batch of them.
func TestStartedBy(t *testing.T) {
	g := func(id, parent int, funcs ...string) goroutine {
		return goroutine{id: id, parent: parent, funcs: funcs}
	}
	const test = 20
	before := map[int]bool{test: true, 3: true, 15: true, 1: true}
	gs := []goroutine{
		g(31, test), // the test's own
		g(8, 31),    // its goroutine's, still running, with a lower ID
		g(33, 25),   // started while it ran, by one that has ended
		g(35, 3),    // another test's, through 3, which ran before it
		g(40, 39),   // through 39, which runs, to 3
		g(39, 3),
		g(3, 1), // the other test, running before it
		g(36, test, "os/signal.loop"),
		g(15, test), // running before Check
	}
	var got []int
	for _, f := range startedBy(test, before, gs) {
		got = append(got, f.id)
	}
	if want := []int{31, 8, 33}; !slices.Equal(got, want) {
		t.Errorf("startedBy = %v, want %v", got, want)
	}
}

// A recorder is a test that Check reports to, whose cleanups the test
// runs.
type recorder struct {
	testing.TB
	mu       sync.Mutex
	errors   []string
	cleanups []func()
}

func (r *recorder) Cleanup(f func()) { r.cleanups = append(r.cleanups, f) }

func (r *recorder) Error(args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range args {
		r.errors = append(r.errors, a.(string))
	}
}

func (r *recorder) end() {
	for _, f := range slices.Backward(r.cleanups) {
		f()
	}
}

func quick() checker {
	c := newChecker()
	c.settle = 200 * time.Millisecond
	return c
}

// Check fails a test whose goroutine is still running once it ends,
// naming its stack, and passes one whose goroutines end during the
// settle.
func TestCheck(t *testing.T) {
	r := &recorder{TB: t}
	quick().check(r)
	stop := make(chan struct{})
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		<-stop
	}()
	r.end()
	close(stop)
	<-ended
	if len(r.errors) != 1 || !strings.Contains(r.errors[0], "1 goroutines outlived") || !strings.Contains(r.errors[0], "TestCheck.func1") {
		t.Errorf("Check reported %q, want TestCheck.func1's goroutine", r.errors)
	}

	r = &recorder{TB: t}
	quick().check(r)
	done := make(chan struct{})
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(done)
	}()
	r.end()
	<-done
	if len(r.errors) != 0 {
		t.Errorf("Check reported %q for a goroutine that ended in the settle", r.errors)
	}
}

// Once a binary's tests have ended, a goroutine still running but the
// allowed ones fails it, its stack written out.
func TestAfterAll(t *testing.T) {
	stop := make(chan struct{})
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		<-stop
	}()
	defer func() { close(stop); <-ended }()
	var out strings.Builder
	c := quick()
	if code := afterAll(c, &out); code != 1 || !strings.Contains(out.String(), "TestAfterAll.func1") {
		t.Errorf("afterAll = %d, %q; want 1 and TestAfterAll.func1's stack", code, out.String())
	}

	// Only the asking goroutine and an allowed one: it passes.
	c.stacks = func() (int, []goroutine) {
		return 1, []goroutine{{id: 1}, {id: 5, funcs: []string{"os/signal.loop"}}}
	}
	out.Reset()
	if code := afterAll(c, &out); code != 0 || out.Len() != 0 {
		t.Errorf("afterAll = %d, %q; want 0 and nothing", code, out.String())
	}
}

// leak is the test binary as a helper whose goroutine is blocked on a
// channel only it holds, which can never run again: it checks the binary
// as Main does once its tests end, and exits with afterAll's code.
func leak() int {
	go func() {
		ch := make(chan int)
		<-ch
	}()
	runtime.Gosched()
	c := newChecker()
	c.settle = 100 * time.Millisecond
	return afterAll(c, os.Stdout) //nolint:forbidigo // the helper's report, which the test reads
}

// A binary whose goroutine is left blocked for good fails, both checks
// naming it: the stacks' and the goroutineleak profile's.
func TestMainFailsALeak(t *testing.T) {
	if got := leakProfile(); got != "" {
		t.Fatalf("leakProfile with nothing leaked = %q", got)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), self) //nolint:gosec // the test binary itself, as a helper
	cmd.Env = append(os.Environ(), helperEnv+"=leak")
	out, err := cmd.Output()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("the helper: %v, want exit status 1; output:\n%s", err, out)
	}
	for _, want := range []string{"goroutines outlived the tests", "goroutineleak profile", "leaktest.leak.func1"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("the helper's output has no %q:\n%s", want, out)
		}
	}
}
