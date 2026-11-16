// Package leaktest fails a test binary whose goroutines outlive its tests
// (Main), and a test whose goroutines outlive it (Check). Each reads every
// goroutine's stack (runtime.Stack), waits a moment for those still ending
// (settle), and fails on what's left but the runtime's and the standard
// library's own, which allowed lists. Main also reads Go's goroutineleak
// profile, which names the goroutines blocked for good on a channel or a
// lock nothing else can reach.
//
// It's for tests only, and standard library only, so internal/proxy's and
// internal/daemon's tests use it too.
package leaktest

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/pprof"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// settle is how long a check waits for goroutines that are ending: a
// closed listener's accept loop, a cancelled command's wait. It only waits
// that long when one doesn't end, so it's long enough for a loaded CI
// machine.
const settle = 5 * time.Second

// allowed are the functions whose goroutines outlive any test, and why:
// the standard library's own, which no test starts or can end.
var allowed = map[string]string{
	// signal.Notify's loop, which runs for the rest of the process.
	"os/signal.loop": "signal.Notify's loop, for the rest of the process",
	// An HTTP client's idle connections, which its transport keeps for
	// the next request to the same server.
	"net/http.(*persistConn).readLoop":  "an HTTP client's idle connection",
	"net/http.(*persistConn).writeLoop": "an HTTP client's idle connection",
}

// A goroutine is one goroutine's stack, as runtime.Stack prints it.
type goroutine struct {
	id     int
	parent int      // the goroutine that started it; 0 for none
	funcs  []string // its stack's functions, innermost first, then what started it
	stack  string
}

// stackNumber is a goroutine's number as runtime.Stack prints it. One it
// can't read is a format it doesn't know, which panics: read as 0, a
// goroutine would lose its parent and a leak could pass.
func stackNumber(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		panic(fmt.Sprintf("leaktest: runtime.Stack's goroutine number %q: %v", s, err))
	}
	return n
}

// parse reads runtime.Stack's goroutines.
func parse(dump []byte) []goroutine {
	var gs []goroutine
	for block := range strings.SplitSeq(strings.TrimSpace(string(dump)), "\n\n") {
		lines := strings.Split(block, "\n")
		head, ok := strings.CutPrefix(lines[0], "goroutine ")
		if !ok {
			continue
		}
		g := goroutine{id: stackNumber(strings.Fields(head)[0]), stack: block}
		for _, l := range lines[1:] {
			if strings.HasPrefix(l, "\t") || strings.HasPrefix(l, "...") {
				continue
			}
			if by, ok := strings.CutPrefix(l, "created by "); ok {
				fn, in, _ := strings.Cut(by, " in goroutine ")
				g.funcs = append(g.funcs, fn)
				if in != "" {
					g.parent = stackNumber(in)
				}
				continue
			}
			if i := strings.LastIndex(l, "("); i > 0 {
				l = l[:i]
			}
			g.funcs = append(g.funcs, l)
		}
		gs = append(gs, g)
	}
	return gs
}

// stacks are every goroutine's stack but the one asking.
func stacks() (self int, gs []goroutine) {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	all := parse(buf)
	if len(all) == 0 {
		return 0, nil
	}
	// runtime.Stack prints the caller's goroutine first.
	return all[0].id, all[1:]
}

// isAllowed is whether g is the standard library's own (allowed).
func isAllowed(g goroutine) bool {
	return slices.ContainsFunc(g.funcs, func(f string) bool { _, ok := allowed[f]; return ok })
}

// A checker finds goroutines that outlive what started them: left are
// those still running that it reports, of the stacks it's given.
type checker struct {
	stacks func() (int, []goroutine)
	settle time.Duration
	tick   time.Duration
}

func newChecker() checker {
	return checker{stacks: stacks, settle: settle, tick: 10 * time.Millisecond}
}

// wait waits, for at most c.settle, until left finds none of the
// goroutines running, and returns those it found last.
func (c checker) wait(left func(self int, gs []goroutine) []goroutine) []goroutine {
	deadline := time.Now().Add(c.settle)
	for {
		found := left(c.stacks())
		if len(found) == 0 || time.Now().After(deadline) {
			return found
		}
		time.Sleep(c.tick)
	}
}

// report is found's stacks, after what they outlived.
func report(what string, found []goroutine) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d goroutines outlived %s:\n", len(found), what)
	for _, g := range found {
		b.WriteString("\n" + g.stack + "\n")
	}
	return b.String()
}

// Main runs m's tests, then exits with their code: or 1 if they passed
// but a goroutine they started is still running after the settle, or
// the goroutineleak profile names one. A package's TestMain calls it, after
// anything the test binary does as a helper process.
func Main(m *testing.M) {
	code := m.Run()
	if code == 0 {
		code = afterAll(newChecker(), os.Stderr) //nolint:forbidigo // the test binary's own report, as go test prints
	}
	os.Exit(code) //nolint:forbidigo // the test binary
}

// afterAll checks no goroutine is left once a binary's tests have ended,
// but the allowed ones, writing what is to w: its code, 0 or 1.
func afterAll(c checker, w io.Writer) int {
	found := c.wait(func(self int, gs []goroutine) []goroutine {
		return slices.DeleteFunc(gs, func(g goroutine) bool { return g.id == self || isAllowed(g) })
	})
	code := 0
	if len(found) > 0 {
		_, _ = io.WriteString(w, "leaktest: "+report("the tests", found))
		code = 1
	}
	if leaked := leakProfile(); leaked != "" {
		_, _ = io.WriteString(w, "leaktest: the goroutineleak profile names goroutines blocked for good:\n"+leaked)
		code = 1
	}
	return code
}

// leakProfile is the goroutineleak profile, when it names any goroutine:
// those blocked on a channel, a lock or a wait group nothing that runs can
// reach. Writing it runs the collection that finds them.
func leakProfile() string {
	p := pprof.Lookup("goroutineleak")
	if p == nil {
		return ""
	}
	var b bytes.Buffer
	if err := p.WriteTo(&b, 1); err != nil {
		return "writing the profile: " + err.Error() + "\n"
	}
	head, _, _ := strings.Cut(b.String(), "\n")
	if strings.HasSuffix(head, "total 0") {
		return ""
	}
	return b.String()
}

// Check fails t if a goroutine started while it ran is still running
// once it has ended, its context cancelled and its cleanups run, after
// the settle. It counts the goroutines t started, and theirs, through
// those still running; and when one's chain ends at a goroutine that has
// ended, one started while t ran. Called first in a test, so its check
// runs after the test's other cleanups. Main finds the rest at the end.
func Check(t testing.TB) {
	t.Helper()
	newChecker().check(t)
}

func (c checker) check(t testing.TB) {
	t.Helper()
	self, gs := c.stacks()
	before := map[int]bool{self: true}
	for _, g := range gs {
		before[g.id] = true
	}
	t.Cleanup(func() {
		found := c.wait(func(_ int, gs []goroutine) []goroutine { return startedBy(self, before, gs) })
		if len(found) > 0 {
			t.Error(report(t.Name(), found))
		}
	})
}

// startedBy are the goroutines of gs, but the allowed ones, started while
// test, the goroutine of a test, ran: those that weren't running before
// it (before) and that test started, or one they lead back to through
// gs, or one that has ended since it began. Goroutine IDs aren't given in
// order (each thread takes a batch), so it's which ones ran before, not
// their IDs, that says when a goroutine began. One that has ended and
// isn't in before began while test ran: a goroutine running at its start
// is in before, and one that had ended by then started none still
// running that isn't.
func startedBy(test int, before map[int]bool, gs []goroutine) []goroutine {
	byID := map[int]goroutine{}
	for _, g := range gs {
		byID[g.id] = g
	}
	ours := func(g goroutine) bool {
		for range len(gs) + 1 { // a chain is at most every goroutine long
			switch p, running := byID[g.parent]; {
			case g.parent == test:
				return true
			case before[g.parent]:
				return false // another test's, or one from before it
			case !running:
				return true
			default:
				g = p
			}
		}
		return false
	}
	var found []goroutine
	for _, g := range gs {
		if !before[g.id] && g.id != test && !isAllowed(g) && ours(g) {
			found = append(found, g)
		}
	}
	return found
}
