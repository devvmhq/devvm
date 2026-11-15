// Package ui is the only place devvm prints. A UI prints messages (Say,
// Info, Note, Warn, Error), asks questions (Ask, Confirm, Pick), runs
// commands into the run's log behind a status line (LoggedCmd), and keeps
// the run's cleanup stack. Main makes the one a run has: it runs the
// cleanups and handles Ctrl+C. A question it can't ask returns its
// refusal (msg.Refusal), which Refused prints.
package ui

import (
	"bufio"
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/term"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/redact"
)

// The UI is the Reporter devvm's packages report through.
var _ msg.Reporter = (*UI)(nil)

// UI is devvm's terminal for one run: its streams and whether each is a
// terminal, the run log and the status line over it, the cleanup stack,
// and whether the run was interrupted. Main makes the one a run has.
type UI struct {
	// Writes to stdout and stderr hold outMu, so the status line and
	// messages don't interleave mid-line.
	outMu          sync.Mutex
	stdout, stderr io.Writer
	stdin          *bufio.Reader

	stdinTTY, stdoutTTY, stderrTTY bool
	// outStyle and errStyle color what goes to stdout and stderr, each on
	// only as colorOn decides for that stream.
	outStyle, errStyle style
	// termWidth is the terminal's width in columns, 0 when unknown.
	termWidth func() int
	// termSize is the width and height of the terminal on stdout, where
	// the picker draws, each 0 when unknown.
	termSize func() (width, height int)
	// readPassword reads a line from the terminal without echoing it.
	readPassword func() (string, error)
	// rawInput puts the terminal devvm reads from into raw mode, returning
	// what undoes it.
	rawInput func() (restore func(), err error)
	// getenv reads the terminal's character set.
	getenv func(string) string
	// verboseOn streams the run log to stderr, noInput refuses every
	// question, pipedAnswers reads answers from input that isn't a
	// terminal, and pickOnPipe shows the picker with stdout that isn't one
	// (SetFlags).
	verboseOn, noInput, pipedAnswers, pickOnPipe atomic.Bool
	// asking counts the questions waiting for an answer (waiting).
	asking atomic.Int32
	// runLogs returns the run logs' folder.
	runLogs func() (string, error)
	// interruptGrace is how long a command a signal ended waits for
	// devvm's own interrupt to be noticed.
	interruptGrace time.Duration

	interrupted atomic.Bool
	// stopsItsOwn is set by StopsItsOwn: a signal waits for the run to
	// return, as cleanups do.
	stopsItsOwn atomic.Bool
	// logoShown is set once Logo printed the logo.
	logoShown atomic.Bool
	// dataOnly sends what Say, Info and LogNote print to stderr, so
	// stdout gets only data (DataOnly).
	dataOnly atomic.Bool
	// warned holds the warnings said, by their names (msg.M.Names), so
	// each is said once a run.
	warned sync.Map
	// commands is read-locked while a logged command runs, so an
	// interrupt can wait for the run's commands to end before cleaning up.
	commands sync.RWMutex
	cleanups struct {
		sync.Mutex
		next  int
		funcs []cleanup
	}
	log struct {
		sync.Mutex
		path  string
		f     *os.File
		cap   *logCap    // f, held to runLogMax
		w     *redacting // LogWriter's, made at its first call
		noted bool
		keep  int // run logs kept, the newest; keepLogs when 0
	}
	status struct {
		sync.Mutex
		s *statusLine
	}
}

// New returns a UI on these streams, none of them a terminal, with its run
// log in devvm's data folder, reading its environment with getenv
// (os.Getenv for devvm's own): they're colored only when FORCE_COLOR or
// CLICOLOR_FORCE says so, and messages show as their names with
// DEVVM_MESSAGES=names.
func New(stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) *UI {
	u := &UI{
		stdout: stdout, stderr: stderr, stdin: bufio.NewReader(stdin),
		termWidth: func() int { return 0 }, termSize: func() (int, int) { return 0, 0 }, rawInput: rawTerminal,
		getenv: getenv, runLogs: runLogs, interruptGrace: time.Second,
	}
	u.outStyle, u.errStyle = newStyle(u.getenv, false), newStyle(u.getenv, false)
	u.readPassword = u.readTerminal
	return u
}

// onTerminal returns the UI on devvm's own standard streams.
func onTerminal() *UI {
	u := New(os.Stdin, os.Stdout, os.Stderr, os.Getenv)
	u.stdinTTY, u.stdoutTTY, u.stderrTTY = isTerminal(os.Stdin), isTerminal(os.Stdout), isTerminal(os.Stderr)
	u.outStyle, u.errStyle = newStyle(u.getenv, u.stdoutTTY), newStyle(u.getenv, u.stderrTTY)
	u.termWidth = func() int {
		w, _, err := term.GetSize(fd(os.Stderr))
		if err != nil {
			return 0
		}
		return w
	}
	u.termSize = func() (int, int) {
		w, h, err := term.GetSize(fd(os.Stdout))
		if err != nil || w <= 0 || h <= 0 {
			// A terminal of unknown size: a small one's, so no row wraps.
			return 80, 24
		}
		return w, h
	}
	return u
}

// runLogs is the run logs' folder in devvm's folders.
func runLogs() (string, error) {
	d, err := paths.FromEnv()
	if err != nil {
		return "", err
	}
	return d.RunLogs(), nil
}

func fd(f *os.File) int { return int(f.Fd()) }

func isTerminal(f *os.File) bool { return term.IsTerminal(fd(f)) }

// CanPick reports whether the picker is shown: in a terminal someone can
// see, without --no-input. Without one, the picker keeps its choices as
// they are. A test build's PickOnPipe (Flags) counts stdout as one.
func (u *UI) CanPick() bool {
	return u.stdinTTY && (u.stdoutTTY || u.pickOnPipe.Load()) && !u.noInput.Load()
}

// Stdout is where data output goes: tables, values, help. Not messages: they
// go through Say and the rest.
func (u *UI) Stdout() io.Writer { return lockedWriter{u, u.stdout} }

// Stderr is where output that isn't a message goes on stderr: the log lines
// of a process devvm runs as itself in the background (devvm _proxy).
func (u *UI) Stderr() io.Writer { return lockedWriter{u, u.stderr} }

type lockedWriter struct {
	u *UI
	w io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.u.outMu.Lock()
	defer l.u.outMu.Unlock()
	return l.w.Write(p)
}

func (u *UI) write(w io.Writer, s string) {
	u.outMu.Lock()
	defer u.outMu.Unlock()
	_, _ = io.WriteString(w, s)
}

// DataOnly keeps stdout for data from now on: what Say, Info and LogNote
// print goes to stderr instead, for a command whose output is JSON.
func (u *UI) DataOnly() { u.dataOnly.Store(true) }

// says is where u's Say, Info and LogNote print, with its style: stdout,
// stderr after DataOnly.
func says(u *UI) (io.Writer, style) {
	if u.dataOnly.Load() {
		return u.stderr, u.errStyle
	}
	return u.stdout, u.outStyle
}

// Say prints the message to stdout.
func (u *UI) Say(m msg.M) {
	w, st := says(u)
	u.write(w, st.message(m)+"\n")
}

// Info prints "==> " (bold in a terminal) and the message to stdout: what
// devvm did.
func (u *UI) Info(m msg.M) {
	w, st := says(u)
	u.write(w, st.paint(sgrBold, "==>")+" "+st.message(m)+"\n")
}

// Text returns the message on one line, without its doc link, as the UI
// shows messages: its text, or its names with DEVVM_MESSAGES=names. For
// messages shown as data (a table's cell, a --json field). The zero message
// is "".
func (u *UI) Text(m msg.M) string { return u.outStyle.line(m) }

// Note prints the message to stderr.
func (u *UI) Note(m msg.M) { u.write(u.stderr, u.errStyle.message(m)+"\n") }

// Warn prints the message to stderr, with "devvm: " before each line,
// yellow in a terminal, once a run: the same warning again (its name and
// values) prints nothing, as a step done twice, such as a VM's start
// again after its project folder's remount failed, would repeat what it
// said.
func (u *UI) Warn(m msg.M) {
	if _, said := u.warned.LoadOrStore(m.Names(), true); said {
		return
	}
	u.write(u.stderr, u.errStyle.labelled(sgrWarn, m))
}

func prefixLines(prefix, s string) string {
	var b strings.Builder
	for line := range strings.SplitSeq(s, "\n") {
		b.WriteString(prefix + line + "\n")
	}
	return b.String()
}

// Error prints the message like Warn, its label red in a terminal: a
// failure, which the caller returns its own exit code for. Once the run
// is interrupted it prints nothing, as the failure is the interrupt's.
func (u *UI) Error(m msg.M) {
	if !u.Interrupted() {
		u.write(u.stderr, u.errStyle.labelled(sgrError, m))
	}
}

// ErrorLogged is Error after printing the last 25 lines of the run's log,
// if it has one, without control characters, dim in a terminal. While
// verbose, those lines are on stderr already, so only the log's path is.
func (u *UI) ErrorLogged(m msg.M) {
	flushLog(u, true)
	path := u.logPath()
	switch {
	case path == "" || u.Interrupted():
	case verbose(u):
		u.write(u.stderr, u.errStyle.paintLines(sgrDim, u.errStyle.line(msg.LogDetails(path))+"\n"))
	default:
		tail := "--- " + u.errStyle.line(msg.LogEnd(path)) + "\n" + msg.Printable(tailLines(path, 25)) + "---\n"
		u.write(u.stderr, u.errStyle.paintLines(sgrDim, tail))
	}
	u.Error(m)
}

// die is Error, then the end of the run with exit code 1, for the UI's own
// failures where no caller can return one (a run log that can't be
// opened): it panics, and Main runs the cleanups and returns 1.
func (u *UI) die(m msg.M) {
	u.Error(m)
	exit(1)
}

// Ask asks a yes-or-no question, no by default: it's true only for an
// answer starting with y. Where it can't ask (asks), it returns q's
// refusal, so nothing goes ahead unanswered.
func (u *UI) Ask(q msg.Question) (bool, error) {
	a, err := answer(u, q, u.errStyle.message(q.M())+" [y/N] ")
	return strings.HasPrefix(a, "y") || strings.HasPrefix(a, "Y"), err
}

// Confirm asks a yes-or-no question, yes by default: it's true unless the
// answer starts with n. Where it can't ask, it returns q's refusal, as
// Ask does.
func (u *UI) Confirm(q msg.Question) (bool, error) {
	a, err := answer(u, q, u.errStyle.message(q.M())+" [Y/n] ")
	return err == nil && !strings.HasPrefix(a, "n") && !strings.HasPrefix(a, "N"), err
}

// Answer asks for a value: it prints the prompt and reads a line, without
// the spaces around it, "" for Enter. Where it can't ask, it returns q's
// refusal, as Ask does.
func (u *UI) Answer(q msg.Question) (string, error) {
	a, err := answer(u, q, u.errStyle.message(q.M())+" ")
	return strings.TrimSpace(a), err
}

// readTerminal reads a line from the terminal without echoing it. A
// cleanup puts the terminal back if the run ends meanwhile.
func (u *UI) readTerminal() (string, error) {
	f := fd(os.Stdin)
	old, err := term.GetState(f)
	if err != nil {
		return "", err
	}
	remove := u.AddCleanup(func(context.Context) { _ = term.Restore(f, old) })
	defer remove()
	b, err := term.ReadPassword(f)
	return string(b), err
}

// Secret reads a value such as a token: in a terminal after q's context
// and prompt on stderr, without echoing it; with piped answers, a line
// of input. Where it can't ask, it returns q's refusal, as Ask does.
func (u *UI) Secret(q msg.Question) (redact.Secret, error) {
	if err := asks(u, q); err != nil {
		return redact.Secret{}, err
	}
	if !u.stdinTTY {
		v, err := readLine(u, q)
		return redact.NewSecret(v), err
	}
	u.write(u.stderr, u.errStyle.message(q.M())+" ")
	v, err := waiting(u, u.readPassword)
	u.write(u.stderr, "\n")
	if err != nil || u.Interrupted() {
		return redact.Secret{}, q.Refused(true)
	}
	return redact.NewSecret(v), nil
}

// waiting is read's answer, the UI counted as waiting for one meanwhile
// (asking), so an interrupt doesn't wait for a question to return.
func waiting(u *UI, read func() (string, error)) (string, error) {
	u.asking.Add(1)
	defer u.asking.Add(-1)
	return read()
}

// Line reads a line of input, without its line ending; at the end of the
// input, what's left of it. For a value a flag says to read (--stdin),
// not a question's answer.
func (u *UI) Line() string {
	line, _ := u.stdin.ReadString('\n')
	return strings.TrimRight(line, "\r\n")
}

// asks is nil where q can be asked, and says q's context first: in a
// terminal, without --no-input, or, with piped answers (Flags), from
// input that isn't a terminal, where it says nothing. Else it's q's
// refusal (msg.Question.Refused), for the caller to return.
func asks(u *UI, q msg.Question) error {
	if u.noInput.Load() || !u.stdinTTY && !u.pipedAnswers.Load() {
		return q.Refused(false)
	}
	if u.stdinTTY {
		for _, m := range q.Context() {
			u.Note(m)
		}
	}
	return nil
}

// answer asks q (asks), printing its prompt to stderr in a terminal, and
// reads a line. At the end of the input with no answer, it returns q's
// refusal.
func answer(u *UI, q msg.Question, prompt string) (string, error) {
	if err := asks(u, q); err != nil {
		return "", err
	}
	if u.stdinTTY {
		u.write(u.stderr, prompt)
	}
	return readLine(u, q)
}

// readLine reads q's answer, a line without its line ending; at the end of
// the input with none, it's q's refusal, after a newline that ends its
// prompt in a terminal. An answer read once the run is interrupted is
// refused too, so nothing goes ahead after Ctrl+C.
func readLine(u *UI, q msg.Question) (string, error) {
	line, err := waiting(u, func() (string, error) { return u.stdin.ReadString('\n') })
	if u.Interrupted() {
		return "", q.Refused(true)
	}
	if err != nil && line == "" {
		if u.stdinTTY {
			u.write(u.stderr, "\n")
		}
		return "", q.Refused(u.stdinTTY)
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// Refused prints r, a question's refusal: the question first unless its
// prompt was shown, then the refusal as Error does. Once the run is
// interrupted it prints nothing.
func (u *UI) Refused(r *msg.Refusal) {
	if u.Interrupted() {
		return
	}
	if r.Question.ID() != "" {
		u.Note(r.Question)
	}
	u.Error(r.Msg)
}
