//go:build e2e

package e2e

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
)

// S is a running section: its checks (Check, Skip) and what they run
// (cmd_test.go: devvm, limactl, a VM's shell; state_test.go: VMs' state,
// images' records, project folders). Paths are the sandbox's folders.
type S struct {
	r     *runner
	sb    *sandbox
	Paths Paths
	needs []backend.Feature // what every check of this S proves (Needing)
}

// Needing is s for checks that each prove fs too, as Needs says: a
// section whose every check proves a feature takes it once.
func (s *S) Needing(fs ...backend.Feature) *S {
	c := *s
	c.needs = append(slices.Clip(s.needs), fs...)
	return &c
}

// CheckOpt changes how a check runs.
type CheckOpt func(*checkOpts)

type checkOpts struct {
	flaky  string
	builds bool
	tail   int
	needs  []backend.Feature
}

// Flaky marks a check known to flake, for the reason given: when it
// fails, it runs once more, and only a second failure fails it. Its
// result says so, and so do its timings and the summary. Every other
// check fails on its first failure.
func Flaky(reason string) CheckOpt { return func(c *checkOpts) { c.flaky = reason } }

// Builds marks a check whose own command can build base or a --from base,
// so it gets at least an hour, however long its cached build took.
func Builds() CheckOpt { return func(c *checkOpts) { c.builds = true } }

// Tail shows n lines of a failed check's output, not 5.
func Tail(n int) CheckOpt { return func(c *checkOpts) { c.tail = n } }

// Needs is the backend features a check proves: on a backend without one
// (devvm doctor --json says none or unreviewed), it's skipped and counted.
func Needs(fs ...backend.Feature) CheckOpt {
	return func(c *checkOpts) { c.needs = append(c.needs, fs...) }
}

// Check runs one check, title its label, fn what it checks: it passes when
// fn returns nil. Its title is its key in the timings, so a changed title
// starts its times again. A check that fails prints fn's error, then
// the last lines its commands printed.
func (s *S) Check(title string, fn func() error, opts ...CheckOpt) {
	c := checkOpts{needs: slices.Clip(s.needs)}
	for _, o := range opts {
		o(&c)
	}
	if c.tail == 0 {
		c.tail = 5
	}
	r := s.r
	if r.ctx.Err() != nil {
		return // interrupted: the run ends
	}
	if why, ok := r.lacks(c.needs, func() (Res, error) { return s.Devvm("", "doctor", "--json") }); ok {
		s.Skip(title, why)
		return
	}
	lim := r.limitOf(title, c.builds)
	var err error
	var log string
	again := ""
	if late, ok := timedOut(r.window()); ok {
		err = fmt.Errorf("%s", late)
	} else if log, err = r.attempt(lim, fn); err != nil && c.flaky != "" {
		again = "retried"
		r.retried = append(r.retried, title)
		r.out.line("  RETRY [%d/%s] %s: its first run failed", r.n+1, r.total, title)
		r.tail(err, log, c.tail)
		log, err = r.attempt(lim, fn)
	}
	r.open(limitBuild, "setup")
	r.result(title, err == nil, again)
	if err != nil {
		r.tail(err, log, c.tail)
	}
}

// attempt runs fn in a window of lim; past it, fn fails, the reason last.
// It's fn's error and what its commands printed.
func (r *runner) attempt(lim time.Duration, fn func() error) (string, error) {
	w := r.open(lim, "check")
	r.began = time.Now()
	err := fn()
	log, _ := os.ReadFile(r.sb.logFile())
	if why, ok := timedOut(w); ok {
		if err == nil {
			return string(log), fmt.Errorf("%s", why)
		}
		return string(log), fmt.Errorf("%w\n%s", err, why)
	}
	return string(log), err
}

// result counts a check and prints its line: [n/N], its time since the
// previous check (so the steps before it count), the run's time and the
// time left.
func (r *runner) result(title string, pass bool, again string) {
	word := "PASS"
	if pass {
		r.pass++
	} else {
		word = "FAIL"
		r.fail++
		r.failed = append(r.failed, title)
	}
	r.n++
	now := time.Now()
	r.mu.Lock()
	dur := now.Sub(r.mark).Round(time.Second)
	r.mark = now
	r.mu.Unlock()
	line := title + "\t" + strconv.Itoa(int(dur.Seconds()))
	if again != "" {
		line += "\t" + again
	}
	r.timings = append(r.timings, line)
	if t, err := strconv.Atoi(r.total); err == nil && r.n > t {
		r.total = "?" // more checks than last time
	}
	extra := ""
	if left, ok := r.left(title); ok {
		extra = ", about " + mins(left) + " left"
	}
	if again != "" {
		extra += "; retried once"
	}
	r.out.line("  %s  [%d/%s] %s (%s; %s in%s)", word, r.n, r.total, title, mins(dur), mins(now.Sub(r.started)), extra)
}

// tail prints a failed check's error, each line led by "failed: " but
// the time limit's, then the last n lines of log, what its commands
// printed, indented, then the proxy logs, which it keeps (keepProxyLogs).
func (r *runner) tail(err error, log string, n int) {
	lines := strings.Split(strings.TrimRight(err.Error(), "\n"), "\n")
	for i, l := range lines {
		if !strings.HasPrefix(l, "timed out: ") && !strings.HasPrefix(l, "the heartbeat's") {
			lines[i] = "failed: " + l
		}
	}
	out := strings.Split(strings.TrimRight(log, "\n"), "\n")
	for _, l := range append(lines, out[max(0, len(out)-n):]...) {
		if l != "" {
			r.out.line("        %s", l)
		}
	}
	keepProxyLogs(r.sb, r.out, filepath.Join(r.sb.proxyLogs(), r.began.Format("150405.000")), r.began)
}

// keepProxyLogs copies every proxy log in the sandbox's devvm folder that has lines, its VMs' and
// its builds', into dir, each named by where it was, and prints where, with the last lines of each
// one written since since: a failed check's, so what the proxies said outlives the VMs and builds
// whose folders the run deletes. The images' and the shared tools' folders aren't searched.
func keepProxyLogs(sb *sandbox, out *output, dir string, since time.Time) {
	root, err := os.OpenRoot(sb.devvmDir())
	if err != nil {
		return
	}
	defer func() { _ = root.Close() }()
	var logs []string
	_ = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil || d.IsDir() && (p == "images" || p == "tools"):
			return fs.SkipDir // a folder it can't read, or one it leaves out
		case d.Type().IsRegular() && d.Name() == "proxy.log":
			logs = append(logs, p)
		}
		return nil
	})
	kept := false
	for _, p := range logs {
		b, err := root.ReadFile(p)
		info, err2 := root.Stat(p)
		if err != nil || err2 != nil || len(b) == 0 || os.MkdirAll(dir, 0o700) != nil ||
			os.WriteFile(filepath.Join(dir, strings.ReplaceAll(p, "/", "_")), b, 0o600) != nil {
			continue
		}
		kept = true
		if !info.ModTime().Before(since) {
			out.line("        %s ends:", p)
			for l := range strings.Lines(tailLines(string(b), 5)) {
				out.line("          %s", strings.TrimRight(l, "\n"))
			}
		}
	}
	if kept {
		out.line("        the proxy logs are kept in %s", dir)
	}
}

// Skip counts a check this run has nothing for, why saying so, so runs
// compare their counts.
func (s *S) Skip(title, why string) {
	r := s.r
	if r.ctx.Err() != nil {
		return
	}
	r.n++
	r.skipped++
	r.timings = append(r.timings, title+"\t0")
	r.out.line("  SKIP  [%d/%s] %s (%s)", r.n, r.total, title, why)
}

// Note prints a line of what the run found, such as a version, which
// isn't a check.
func (s *S) Note(text string) {
	s.r.out.line("  NOTE  %s", text)
}

// Section prints a section's heading.
func (s *S) Section(name string) {
	s.r.out.line("")
	s.r.out.line("== %s", name)
}
