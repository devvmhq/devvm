//go:build e2e

package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// output is the run's lines, from the runner and its heartbeat at once.
type output struct {
	mu sync.Mutex
	w  io.Writer
}

func newOutput(w io.Writer) *output { return &output{w: w} }

// line prints one line (format and its values, as fmt.Sprintf's).
func (o *output) line(format string, a ...any) {
	o.mu.Lock()
	defer o.mu.Unlock()
	fmt.Fprintf(o.w, format+"\n", a...)
}

// mins is d as the run shows a time: 3m07s.
func mins(d time.Duration) string {
	s := int(d.Round(time.Second) / time.Second)
	return fmt.Sprintf("%dm%02ds", s/60, s%60)
}

// The time limits: a check gets 3 times its longest time
// in its section's last complete run, at least limitFloor, or
// limitDefault with none; one that builds an image, and the steps between
// two checks, limitBuild. Past a limit, its processes get TERM, as Ctrl+C
// would, then KILL killAfter later.
const (
	limitFloor   = 2 * time.Minute
	limitDefault = 10 * time.Minute
	limitBuild   = time.Hour
	killAfter    = 30 * time.Second
	heartbeat    = time.Minute
)

// window is the time a check, or the steps before one, may take: its
// context, which the commands run in, ends at the limit.
type window struct {
	ctx     context.Context
	cancel  context.CancelFunc
	limit   time.Duration
	kind    string // check or setup
	expired chan struct{}
	why     string // why it ran out of time, once it has
}

// runner runs the sections' checks: each result with [n/N], its time and
// the time left, a heartbeat every minute, each check's time limit, the
// flaky checks' one retry, each section's timings and the check-count
// guard.
type runner struct {
	ctx context.Context
	out *output
	sb  *sandbox
	o   options

	started time.Time // the run's
	mark    time.Time // the previous check's end: a check's time counts from it
	began   time.Time // the running or last check's attempt's start: a failed one keeps the proxy logs since
	total   string    // the checks expected in all, or ?
	n       int       // the checks run so far

	mu  sync.Mutex
	win *window

	sum      string   // the checksum of this runner's sources: a count compares only with its own
	cur      string   // the section running
	timings  []string // its checks' times, label<TAB>seconds[<TAB>retried]
	last     []string // its last complete run's
	later    time.Duration
	pass     int
	fail     int
	skipped  int
	failed   []string
	retried  []string
	short    []string
	features map[string]string // the backend's features, as devvm doctor --json shows them
	hbDone   chan struct{}
}

func newRunner(ctx context.Context, out *output, sb *sandbox, o options) *runner {
	now := time.Now()
	r := &runner{ctx: ctx, out: out, sb: sb, o: o, started: now, mark: now, sum: sourceSum(), hbDone: make(chan struct{})}
	r.total = r.expected()
	_ = os.MkdirAll(sb.tdir, 0o700)
	_ = os.RemoveAll(sb.proxyLogs()) // the last run's failed checks'
	r.open(limitBuild, "setup")
	go r.heartbeat()
	return r
}

// expected is how many checks the chosen sections ran in their last
// complete runs, or ? when one has none.
func (r *runner) expected() string {
	n := 0
	for _, s := range r.o.sel {
		l := readTimings(filepath.Join(r.sb.tdir, s))
		if len(l) == 0 {
			return "?"
		}
		n += len(l)
	}
	return strconv.Itoa(n)
}

// sourceSum is the checksum of this folder's Go files, which the timings
// keep beside each section's: an edited runner may have renamed, added or
// dropped checks, so only the same one compares a section's count.
func sourceSum() string {
	m, _ := filepath.Glob("*.go")
	slices.Sort(m)
	h := sha256.New()
	for _, f := range m {
		b, _ := os.ReadFile(f)
		h.Write([]byte(f))
		h.Write(b)
	}
	return "go " + hex.EncodeToString(h.Sum(nil))
}

// readTimings is a timings file's lines, none when it has none.
func readTimings(f string) []string {
	b, err := os.ReadFile(f)
	if err != nil {
		return nil
	}
	return slices.DeleteFunc(strings.Split(string(b), "\n"), func(l string) bool { return l == "" })
}

// secsOf is a timings line's label and seconds.
func secsOf(line string) (string, int) {
	f := strings.Split(line, "\t")
	if len(f) < 2 {
		return f[0], 0
	}
	n, _ := strconv.Atoi(f[1])
	return f[0], n
}

// sumOf is what a section's lines took.
func sumOf(lines []string) time.Duration {
	var s int
	for _, l := range lines {
		_, n := secsOf(l)
		s += n
	}
	return time.Duration(s) * time.Second
}

// open starts a window of limit: the next steps' (setup) or a check's.
// The last one's context ends.
func (r *runner) open(limit time.Duration, kind string) *window {
	ctx, cancel := context.WithTimeout(r.ctx, limit)
	w := &window{ctx: ctx, cancel: cancel, limit: limit, kind: kind, expired: make(chan struct{})}
	context.AfterFunc(ctx, func() {
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return
		}
		what := "the check ran past its limit of " + mins(limit) + ", and its processes were stopped"
		if kind == "setup" {
			what = "the steps before this check ran past their limit of " + mins(limit) + ", and their processes were stopped"
		}
		w.why = "timed out: " + what + "\nthe heartbeat's last line: " + r.heartbeatLine()
		r.out.line("        ... time limit reached (%s): stopping what the %s started", mins(limit), kind)
		close(w.expired)
	})
	r.mu.Lock()
	old := r.win
	r.win = w
	r.mu.Unlock()
	if old != nil {
		old.cancel()
	}
	_ = os.WriteFile(r.sb.logFile(), nil, 0o600)
	return w
}

// window is the window open now.
func (r *runner) window() *window {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.win
}

// timedOut is why w ran out of time, when it did: once its deadline has
// passed, it waits for the reason, which a goroutine writes then.
func timedOut(w *window) (string, bool) {
	if !errors.Is(w.ctx.Err(), context.DeadlineExceeded) {
		return "", false
	}
	<-w.expired
	return w.why, true
}

// heartbeat says every minute how long since the last check, and the
// newest output, a check's or devvm's own run log.
func (r *runner) heartbeat() {
	t := time.NewTicker(heartbeat)
	defer t.Stop()
	for {
		select {
		case <-r.hbDone:
			return
		case <-t.C:
			r.out.line("        %s", r.heartbeatLine())
		}
	}
}

func (r *runner) heartbeatLine() string {
	r.mu.Lock()
	since := time.Since(r.mark)
	r.mu.Unlock()
	newest, at := "", time.Time{}
	for _, f := range append(r.sb.paths().RunLogs(), r.sb.logFile()) {
		if st, err := os.Stat(f); err == nil && st.ModTime().After(at) {
			newest, at = f, st.ModTime()
		}
	}
	return "... " + mins(since) + " since the last check: " + lastLine(newest)
}

// stop ends the heartbeat and the window open now.
func (r *runner) stop() {
	close(r.hbDone)
	r.window().cancel()
}

// section runs one section: its checks' times go to the timings once it
// ends, unless it ran fewer than its last complete run did. A section
// that panics ends there, which the count then shows.
func (r *runner) section(sec section) {
	r.cur, r.timings = sec.name, nil
	r.last = readTimings(filepath.Join(r.sb.tdir, sec.name))
	r.later = 0
	after := false
	for _, s := range sections() {
		if after && r.o.wants(s.name) {
			r.later += sumOf(readTimings(filepath.Join(r.sb.tdir, s.name)))
		}
		after = after || s.name == sec.name
	}
	s := &S{r: r, sb: r.sb, Paths: r.sb.paths()}
	func() {
		defer func() {
			if p := recover(); p != nil {
				r.out.line("e2e: the %s section stopped: %v", sec.name, p)
			}
		}()
		sec.run(s)
	}()
	r.sectionEnd()
}

// sectionEnd keeps the section's times as its timings, unless it ran
// fewer checks than the last complete run of these same sources: then it
// ended early, which the run fails on.
func (r *runner) sectionEnd() {
	if r.ctx.Err() != nil {
		return // interrupted: its times aren't a complete run's
	}
	f := filepath.Join(r.sb.tdir, r.cur)
	sum, _ := os.ReadFile(f + ".sum")
	if strings.TrimSpace(string(sum)) == r.sum && len(r.last) > 0 && len(r.timings) < len(r.last) {
		r.short = append(r.short, fmt.Sprintf("%s ran %d checks, but its last complete run ran %d: it ended early (look for errors above)", r.cur, len(r.timings), len(r.last)))
		return
	}
	body := strings.Join(r.timings, "\n")
	if body != "" {
		body += "\n"
	}
	if os.WriteFile(f+".new", []byte(body), 0o600) == nil && os.Rename(f+".new", f) == nil {
		_ = os.WriteFile(f+".sum", []byte(r.sum+"\n"), 0o600)
	}
}

// limitOf is a check's time limit: 3 times its longest time in its
// section's last complete run, at least limitFloor; limitDefault with
// none; limitBuild at least for one that builds.
func (r *runner) limitOf(label string, builds bool) time.Duration {
	lim, found := limitDefault, false
	var most int
	for _, l := range r.last {
		if name, n := secsOf(l); name == label && (!found || n > most) {
			most, found = n, true
		}
	}
	if found {
		lim = max(3*time.Duration(most)*time.Second, limitFloor)
	}
	if builds {
		lim = max(lim, limitBuild)
	}
	return lim
}

// left is the time the run has left after label, as its section's last
// complete run and the later sections' measured it.
func (r *runner) left(label string) (time.Duration, bool) {
	var s time.Duration
	found := false
	for _, l := range r.last {
		name, n := secsOf(l)
		if found {
			s += time.Duration(n) * time.Second
		}
		if name == label {
			found = true
		}
	}
	return s + r.later, found
}

// finish ends the run: its results, for a --slots run to add up, and its
// summary lines. It's whether every check passed and no section ended
// early.
func (r *runner) finish(now time.Time) bool {
	var b strings.Builder
	fmt.Fprintf(&b, "pass\t%d\nfail\t%d\nskipped\t%d\nelapsed\t%d\n", r.pass, r.fail, r.skipped, int(now.Sub(r.started).Seconds()))
	for _, l := range r.failed {
		fmt.Fprintf(&b, "failed\t%s\n", l)
	}
	for _, l := range r.retried {
		fmt.Fprintf(&b, "retried\t%s\n", l)
	}
	for _, l := range r.short {
		fmt.Fprintf(&b, "short\t%s\n", l)
	}
	f := filepath.Join(r.sb.work, "summary")
	if os.WriteFile(f+".new", []byte(b.String()), 0o600) == nil {
		_ = os.Rename(f+".new", f)
	}
	r.out.line("")
	r.out.line("e2e: %s in %s%s", counts(r.pass, r.fail, r.skipped, len(r.retried)), mins(now.Sub(r.started)), failedList(r.failed))
	for _, l := range r.retried {
		r.out.line("e2e: retried once: %s", l)
	}
	for _, l := range r.short {
		r.out.line("e2e: FAILED: %s", l)
	}
	ok := r.fail == 0 && len(r.short) == 0
	if r.o.newest {
		r.newestSummary(ok)
	}
	return ok
}

// newestSummary is a --newest-base run's last lines: what Ubuntu listed
// and devvm pinned, and a failure named as upstream drift with its
// release.
func (r *runner) newestSummary(ok bool) {
	listed, err := listedRelease(r.ctx)
	if err != nil {
		listed = "nothing"
	}
	got := "nothing"
	if b, err := os.ReadFile(filepath.Join(r.sb.images(), "ubuntu.json")); err == nil {
		var m map[string]any
		if json.Unmarshal(b, &m) == nil {
			if v, _ := m["release"].(string); v != "" {
				got = v
			}
		}
	}
	r.out.line("e2e: newest base: Ubuntu listed %s as its newest; devvm pinned %s", listed, got)
	if !ok {
		rel := got
		if rel == "nothing" {
			rel = listed
		}
		r.out.line("e2e: UPSTREAM DRIFT: base from Ubuntu's newest cloud image, %s, and the mirrors' newest packages, failed the checks above, which e2e's other runs miss as they pin %s; a user's first build of base gets this now (contributing/maintaining.md says what to do)", rel, e2eUbuntuRelease)
	}
}

// counts is a summary's "3 passed, 1 failed, 1 skipped, 1 retried".
func counts(pass, fail, skipped, retried int) string {
	s := fmt.Sprintf("%d passed, %d failed", pass, fail)
	if skipped > 0 {
		s += fmt.Sprintf(", %d skipped", skipped)
	}
	if retried > 0 {
		s += fmt.Sprintf(", %d retried", retried)
	}
	return s
}

// failedList is ": " and the failed checks, space-separated, or nothing.
func failedList(failed []string) string {
	if len(failed) == 0 {
		return ""
	}
	return ": " + strings.Join(failed, " ")
}
