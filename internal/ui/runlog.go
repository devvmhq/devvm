package ui

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/devvmhq/devvm/internal/daemon"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/redact"
	"github.com/devvmhq/devvm/internal/state"
)

// keepLogs is how many run logs are kept, the newest.
const keepLogs = 20

// streamWait is how long a streamed command's output is read after the
// command ends.
const streamWait = 2 * time.Second

// The output of the commands devvm runs (the backends', the build
// scripts') goes to a log, one per devvm run that has any, not the
// terminal: devvm prints its own steps behind a status line, and the end
// of the log when a step fails. Verbose (-v) also streams each line the
// log gets to stderr as it's written, without control characters, dim in
// a terminal, instead of the status line.

// Flags are the run's flags that every command takes and the UI acts
// on.
type Flags struct {
	Verbose bool // -v/--verbose: the run log streamed to stderr too
	NoInput bool // --no-input: no question asked; each is refused, naming its answer
	// PipedAnswers reads questions' answers from input that isn't a
	// terminal, a line each, without their prompts: the CLI tests' way to
	// answer, which only a test build turns on.
	PipedAnswers bool
	// PickOnPipe shows the picker when stdin is a terminal and stdout
	// isn't (CanPick): the CLI tests' way to reach it, as testscript's
	// ttyin gives a command a terminal on stdin alone. Only a test build
	// turns it on.
	PickOnPipe bool
}

// SetFlags sets the run's -v/--verbose and --no-input, and whether
// answers are piped and the picker shown on a pipe.
func (u *UI) SetFlags(f Flags) {
	u.verboseOn.Store(f.Verbose)
	u.noInput.Store(f.NoInput)
	u.pipedAnswers.Store(f.PipedAnswers)
	u.pickOnPipe.Store(f.PickOnPipe)
}

// verbose reports whether u streams the run log (Flags.Verbose).
func verbose(u *UI) bool { return u.verboseOn.Load() }

// stream is the run log's copy on stderr while verbose: what's written to
// it goes to stderr printable and dim. A lead byte of a C1 control
// (0xc2) at a write's end is held for the next, so a control split
// across writes is still dropped.
type stream struct {
	u    *UI
	mu   sync.Mutex
	held []byte
}

func (s *stream) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := slices.Concat(s.held, p)
	s.held = nil
	if n := len(b); n > 0 && b[n-1] == 0xc2 {
		b, s.held = b[:n-1], []byte{0xc2}
	}
	if text := msg.Printable(string(b)); text != "" {
		s.u.write(s.u.stderr, s.u.errStyle.paintLines(sgrDim, text))
	}
	return len(p), nil
}

// logTo is where a command's output goes: the run log w (its logCap),
// and while verbose, the stream too.
func logTo(u *UI, w io.Writer) io.Writer {
	if !verbose(u) {
		return w
	}
	return io.MultiWriter(w, &stream{u: u})
}

// runLogMax is the most one run writes to its log, as a run that lasts,
// such as devvm desktop's, can be made to log without end.
const runLogMax = 128 << 20

// runLogFull is the run log's last line once it's past runLogMax.
const runLogFull = "\ndevvm: truncated: this run wrote more than 128 MiB to its log, and the rest isn't in it\n"

// logCap is the run's log file, held to runLogMax: the write that would
// take it past is dropped, with every one after it, and runLogFull says
// so once.
type logCap struct {
	f    io.Writer
	mu   sync.Mutex
	n    int64
	full bool
}

func (c *logCap) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.full {
		return len(p), nil
	}
	if c.n+int64(len(p)) > runLogMax {
		c.full = true
		_, err := io.WriteString(c.f, runLogFull)
		return len(p), err
	}
	n, err := c.f.Write(p)
	c.n += int64(n)
	return n, err
}

// logPath is the run's log, "" before it's opened.
func (u *UI) logPath() string {
	if p := u.log.path.Load(); p != nil {
		return *p
	}
	return ""
}

// openLog opens the run's log, the first time in a run, in the run logs
// folder (owner-only), and deletes all but the newest logs there
// (pruneLogs).
func (u *UI) openLog() *logCap {
	u.log.Lock()
	defer u.log.Unlock()
	if u.log.f != nil {
		return u.log.cap
	}
	dir, err := u.runLogs()
	if err != nil {
		if m, ok := errors.AsType[msg.M](err); ok {
			u.die(m)
		}
		u.die(msg.RunLogFailed("", err.Error()).Because(err))
	}
	f, err := createLog(dir, time.Now(), os.Getpid())
	if err != nil {
		u.die(msg.RunLogFailed(dir, err.Error()).Because(err))
	}
	u.log.f, u.log.cap = f, &logCap{f: f}
	name := f.Name()
	u.log.path.Store(&name)
	pruneLogs(dir, cmp.Or(u.log.keep, keepLogs))
	return u.log.cap
}

// KeepRunLogs sets how many run logs are kept, the newest (retention's
// logs.runs); 0 is the default, keepLogs.
func (u *UI) KeepRunLogs(n int) {
	u.log.Lock()
	defer u.log.Unlock()
	u.log.keep = max(n, 0)
}

func createLog(dir string, now time.Time, pid int) (*os.File, error) {
	for _, d := range []string{filepath.Dir(dir), dir} {
		if err := state.PrivateDir(d); err != nil {
			return nil, err
		}
	}
	name := now.Format("20060102-150405") + "-" + strconv.Itoa(pid) + ".log"
	return os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600) //nolint:gosec // devvm's own folder
}

// runLogsMax is as much as the run logs in the folder take together: past
// it, the oldest go, however many logs.runs keeps, but the newest.
const runLogsMax = 256 << 20

// pruneLogs deletes all but the newest keep logs in dir, and older ones
// past runLogsMax together, but never the newest, nor a log whose run
// still runs (logPID): a background process, or another devvm, writing
// it still, which the system would keep on disk after its name went.
func pruneLogs(dir string, keep int) {
	files, _ := filepath.Glob(filepath.Join(dir, "*.log"))
	type log struct {
		path string
		mod  time.Time
		size int64
	}
	var logs []log
	for _, f := range files {
		if fi, err := os.Stat(f); err == nil {
			logs = append(logs, log{f, fi.ModTime(), fi.Size()})
		}
	}
	slices.SortFunc(logs, func(a, b log) int { return b.mod.Compare(a.mod) })
	var total int64
	for i, l := range logs {
		total += l.size
		if i == 0 || i < keep && total <= runLogsMax {
			continue
		}
		if pid, ok := logPID(l.path); ok && daemon.Alive(pid) {
			continue
		}
		_ = os.Remove(l.path)
	}
}

// logPID is the pid of the run that writes the run log at path, from its
// name (createLog's): false for a name of another form. A pid the system
// gave another process since keeps its log until that one ends.
func logPID(path string) (int, bool) {
	parts := strings.Split(strings.TrimSuffix(filepath.Base(path), ".log"), "-")
	if len(parts) != 3 || len(parts[0]) != len("20060102") || len(parts[1]) != len("150405") {
		return 0, false
	}
	pid, err := strconv.Atoi(parts[2])
	return pid, err == nil && pid > 0
}

// LogNote says where the details of slow steps go, once per run.
func (u *UI) LogNote() {
	u.openLog()
	path := u.logPath()
	u.log.Lock()
	noted := u.log.noted
	u.log.noted = true
	u.log.Unlock()
	if !noted {
		w, st := says(u)
		u.write(w, "    "+st.message(msg.LogDetails(path))+"\n")
	}
}

// LogWriter is where to send the output of a command run some other way
// than LoggedCmd (a backend's Exec, say): the run log, streamed to stderr
// too while verbose, through the run's one redacting writer, so a password
// split across writes is still found. The end of a line still to come is
// written at the next flush point (flushLog): a logged command's start, as
// far as its last word, and ErrorLogged and the run's end, all of it.
func (u *UI) LogWriter() io.Writer {
	f := u.openLog()
	u.log.Lock()
	defer u.log.Unlock()
	if u.log.w == nil {
		u.log.w = &redacting{w: logTo(u, f)}
	}
	return u.log.w
}

// flushLog writes what u's LogWriter holds of a line, redacted: all of it at the end (ErrorLogged,
// the run's end), or as far as its last word while the run goes on (flushPart).
func flushLog(u *UI, end bool) {
	u.log.Lock()
	w := u.log.w
	u.log.Unlock()
	switch {
	case w == nil:
	case end:
		w.flush()
	default:
		w.flushPart()
	}
}

// redactLineMax is as much of a line without its end as redacting holds: past it, the line is
// written as far as redactPart lets it.
const redactLineMax = 64 << 10

// redactTailMax is as much of a line's last word as redacting holds back when it writes part of
// a line (redact.Split), so a URL with its password of up to this length is found whole.
const redactTailMax = 4 << 10

// redacting is a command's output on its way to the run log: it writes whole lines, each
// redacted (redact.String), so a password split across two writes is still found. A line ends at
// "\n" or "\r" (a progress bar's). A line longer than redactLineMax, or one at a flush point while
// its command still runs (flushPart), is written as far as its last word, and the rest is held
// for what comes next. flush writes it all, once the command has ended.
type redacting struct {
	w   io.Writer
	mu  sync.Mutex
	buf []byte
}

func (r *redacting) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if i := bytes.LastIndexAny(r.buf, "\n\r"); i >= 0 {
		_, _ = io.WriteString(r.w, redact.String(string(r.buf[:i+1])))
		r.buf = slices.Clone(r.buf[i+1:])
	}
	if len(r.buf) > redactLineMax {
		r.writePart()
	}
	return len(p), nil
}

// writePart writes the held line as far as redact.Split lets it, and holds the rest; r.mu is held.
func (r *redacting) writePart() {
	i := redact.Split(r.buf, redactTailMax)
	if i > 0 {
		_, _ = io.WriteString(r.w, redact.String(string(r.buf[:i])))
		r.buf = slices.Clone(r.buf[i:])
	}
}

// flushPart writes what it can of the held line while more may still come.
func (r *redacting) flushPart() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writePart()
}

func (r *redacting) flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.buf) > 0 {
		_, _ = io.WriteString(r.w, redact.String(string(r.buf)))
		r.buf = nil
	}
}

// steps counts the labelled steps in a phase (an image's build, say).
type steps struct {
	sync.Mutex
	n, total int
}

type stepsKey struct{}

// WithSteps starts a phase of total steps: each labelled LoggedCmd with the
// context returned is numbered on its status line ("[2/5] running base.sh")
// and in the log ("# [2/5] running base.sh"). A phase inside another keeps
// its own count.
func (u *UI) WithSteps(ctx context.Context, total int) context.Context {
	return context.WithValue(ctx, stepsKey{}, &steps{total: total})
}

// count numbers a labelled step in its phase, if any: its label for the
// status line, and its line for the log ("" outside a phase).
func count(ctx context.Context, step string) (label, logLine string) {
	s, _ := ctx.Value(stepsKey{}).(*steps)
	if s == nil || s.total <= 0 {
		return step, ""
	}
	s.Lock()
	defer s.Unlock()
	s.n++
	n := "[" + strconv.Itoa(s.n) + "/" + strconv.Itoa(s.total) + "]"
	if s.n <= s.total {
		label = n + " " + step
	} else {
		label = step
	}
	return label, "# " + n + " " + step + "\n"
}

// LoggedCmd runs cmd into the run log, with a status line in a terminal
// showing step. A step may be the zero msg.M: the status line then shows
// the command and its first argument. ctx carries the phase's step count.
// It returns the command's error; the caller fails with what failed,
// which ErrorLogged shows with the log's end.
func (u *UI) LoggedCmd(ctx context.Context, step msg.M, cmd *exec.Cmd) error {
	f := u.openLog()
	w := logTo(u, f)
	label := []part{{text: strings.Join(cmd.Args[:min(2, len(cmd.Args))], " ")}}
	logLine := ""
	if step.ID() != "" {
		var counted string
		shown := u.errStyle.line(step)
		counted, logLine = count(ctx, shown)
		// The count before the step, then the step, its values bold.
		label = append([]part{{text: strings.TrimSuffix(counted, shown)}}, u.errStyle.valueParts(step)...)
	}
	flushLog(u, false) // what LogWriter holds goes first, but for a last word still to be ended
	_, _ = io.WriteString(w, logLine+redact.String("+ "+strings.Join(cmd.Args, " "))+"\n")
	out := &redacting{w: w}
	defer out.flush()
	cmd.Stdout, cmd.Stderr = out, out

	var s *statusLine
	if u.stderrTTY && !verbose(u) && !u.Interrupted() {
		s = u.startStatus(label)
	}
	start := time.Now()
	err := u.run(cmd)
	if s != nil {
		s.stop()
		if took := time.Since(start); err == nil && took >= doneAfter {
			u.write(u.stderr, u.doneLine(label, int(took.Seconds())))
		}
	}
	return err
}

func (u *UI) run(cmd *exec.Cmd) error {
	u.commands.RLock()
	defer u.commands.RUnlock()
	// Streamed output is copied through a pipe, which a process the
	// command left running (a VM's own) can hold open: once the command
	// ends, its output gets a moment, then the pipe is closed.
	if _, file := cmd.Stdout.(*os.File); !file && cmd.WaitDelay == 0 {
		cmd.WaitDelay = streamWait
	}
	err := cmd.Run()
	if errors.Is(err, exec.ErrWaitDelay) {
		err = nil
	}
	if killed(err) {
		u.awaitInterrupt(u.interruptGrace)
	}
	return err
}

// killed reports whether a command ended by a signal, or with the code a
// shell gives that (128 and over).
func killed(err error) bool {
	exitErr, ok := errors.AsType[*exec.ExitError](err)
	return ok && (exitErr.ExitCode() == -1 || exitErr.ExitCode() >= 128)
}

// awaitInterrupt waits up to d for an interrupt. Ctrl+C reaches devvm and
// its command at once, and the command can end first: its failure isn't
// devvm's to report.
func (u *UI) awaitInterrupt(d time.Duration) {
	for deadline := time.Now().Add(d); !u.Interrupted() && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
}
