package msg

import (
	"cmp"
	"context"
	"io"
	"os/exec"
	"slices"
	"sync"
)

// A Reporter is what devvm's packages need of the run's UI: where their
// messages go, the questions they ask and the picker, the status
// line, the run log, output that isn't a message, and the run's cleanup
// stack. internal/ui's UI is the one devvm runs with, which internal/cli
// gives each package that embeds or takes a Reporter; tests give them a
// Fake.
type Reporter interface {
	// Info says what devvm did and Say prints a line, both on stdout; Note
	// prints a line and Warn a warning, both on stderr.
	Info(M)
	Say(M)
	Note(M)
	Warn(M)
	// Status shows a status line until stop.
	Status(label M) (stop func())

	// Ask asks a yes-or-no question, no by default ([y/N]); Confirm one
	// that's yes by default ([Y/n]); Answer asks for a value, "" for
	// Enter. Each returns the question's *Refusal instead when it can't
	// ask: with --no-input, without a terminal, or at the end of the
	// input.
	Ask(Question) (bool, error)
	Confirm(Question) (bool, error)
	Answer(Question) (string, error)
	// CanPick reports whether the picker is shown, which keeps its
	// choices as they are when it isn't.
	CanPick() bool
	// Logo prints devvm's logo, once a run, where someone watches stdout.
	Logo()

	// LogNote says where the run log is, once a run; LogWriter is the run
	// log, for lines that aren't messages; LoggedCmd runs a command into
	// it; WithSteps numbers the logged commands of a phase of so many
	// steps.
	LogNote()
	LogWriter() io.Writer
	LoggedCmd(ctx context.Context, step M, cmd *exec.Cmd) error
	WithSteps(ctx context.Context, total int) context.Context
	// Stdout is where data output goes, Stderr output on stderr that
	// isn't a message.
	Stdout() io.Writer
	Stderr() io.Writer

	// AddCleanup adds to the run's cleanup stack; Interrupted reports
	// whether the run was interrupted.
	AddCleanup(f func(context.Context)) (remove func())
	Interrupted() bool
}

var _ Reporter = (*Fake)(nil)

// A Fake is a Reporter for tests. Its zero value is quiet: it prints
// nothing, has no terminal, so it refuses every question, drops the run
// log and data output, runs a logged command as it is, its output
// dropped, counts no steps, keeps no cleanups and isn't interrupted. Its
// fields change that: each On func, when set, is given what its method
// is (a Recorder's Add records a kind of message), and the rest stand
// for the terminal, the writers and an interrupt. A test may change them
// after giving it to what it tests. It keeps the status lines started
// and not stopped (Showing), so it's never copied once used.
type Fake struct {
	OnInfo, OnSay, OnNote, OnWarn func(M)
	// OnAny is given every message, whatever its kind, after its kind's
	// On func.
	OnAny func(M)
	// OnAsk, OnConfirm and OnAnswer are given the question asked, each
	// after its context goes to OnNote: no and "" without them.
	OnAsk, OnConfirm func(M) bool
	OnAnswer         func(M) string
	// OnCleanup is AddCleanup.
	OnCleanup func(f func(context.Context)) (remove func())
	// OnStatus is given each status line's label as it starts, and each
	// logged command's step as it runs (the zero M for none); Showing is
	// the one shown meanwhile, so a test can check that slow work ran
	// under its line.
	OnStatus func(M)
	// Terminal is whether questions are asked: without it each is
	// refused. Picks is CanPick.
	Terminal, Picks bool
	// Interrupt is Interrupted.
	Interrupt bool
	// Out is Stdout, Err Stderr and Log LogWriter: nil drops what's
	// written.
	Out, Err, Log io.Writer

	status struct {
		sync.Mutex
		lines []*M // the status lines started and not stopped, innermost last
	}
}

// Info gives m to OnInfo and OnAny.
func (f *Fake) Info(m M) { f.say(f.OnInfo, m) }

// Say gives m to OnSay and OnAny.
func (f *Fake) Say(m M) { f.say(f.OnSay, m) }

// Note gives m to OnNote and OnAny.
func (f *Fake) Note(m M) { f.say(f.OnNote, m) }

// Warn gives m to OnWarn and OnAny.
func (f *Fake) Warn(m M) { f.say(f.OnWarn, m) }

// Status gives label to OnStatus and keeps it shown (Showing) until stop.
func (f *Fake) Status(label M) (stop func()) {
	if f.OnStatus != nil {
		f.OnStatus(label)
	}
	line := &label
	f.status.Lock()
	f.status.lines = append(f.status.lines, line)
	f.status.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			f.status.Lock()
			defer f.status.Unlock()
			if i := slices.Index(f.status.lines, line); i >= 0 {
				f.status.lines = slices.Delete(f.status.lines, i, i+1)
			}
		})
	}
}

// Showing is the label of the innermost status line or logged command's
// step showing now, and whether one is: what the UI's status line would
// show.
func (f *Fake) Showing() (M, bool) {
	f.status.Lock()
	defer f.status.Unlock()
	if len(f.status.lines) == 0 {
		return M{}, false
	}
	return *f.status.lines[len(f.status.lines)-1], true
}

// Ask is OnAsk's answer, no without one; without Terminal, q's refusal.
func (f *Fake) Ask(q Question) (bool, error) {
	if err := f.ask(q); err != nil {
		return false, err
	}
	return f.OnAsk != nil && f.OnAsk(q.M()), nil
}

// Confirm is OnConfirm's answer, no without one; without Terminal, q's
// refusal.
func (f *Fake) Confirm(q Question) (bool, error) {
	if err := f.ask(q); err != nil {
		return false, err
	}
	return f.OnConfirm != nil && f.OnConfirm(q.M()), nil
}

// Answer is OnAnswer's answer, "" without one; without Terminal, q's
// refusal.
func (f *Fake) Answer(q Question) (string, error) {
	if err := f.ask(q); err != nil || f.OnAnswer == nil {
		return "", err
	}
	return f.OnAnswer(q.M()), nil
}

// ask is q's refusal without Terminal; with it, q's context said.
func (f *Fake) ask(q Question) error {
	if !f.Terminal {
		return q.Refused(false)
	}
	for _, m := range q.Context() {
		f.Note(m)
	}
	return nil
}

// CanPick is Picks.
func (f *Fake) CanPick() bool { return f.Picks }

// Logo prints nothing.
func (f *Fake) Logo() {}

// LogNote says nothing.
func (f *Fake) LogNote() {}

// LogWriter is Log, or nowhere.
func (f *Fake) LogWriter() io.Writer { return cmp.Or(f.Log, io.Discard) }

// LoggedCmd runs cmd as it is, its step shown meanwhile (Status).
func (f *Fake) LoggedCmd(_ context.Context, step M, cmd *exec.Cmd) error {
	defer f.Status(step)()
	return cmd.Run()
}

// WithSteps returns ctx.
func (f *Fake) WithSteps(ctx context.Context, _ int) context.Context { return ctx }

// Stdout is Out, or nowhere.
func (f *Fake) Stdout() io.Writer { return cmp.Or(f.Out, io.Discard) }

// Stderr is Err, or nowhere.
func (f *Fake) Stderr() io.Writer { return cmp.Or(f.Err, io.Discard) }

// AddCleanup gives c to OnCleanup; without one it keeps nothing.
func (f *Fake) AddCleanup(c func(context.Context)) func() {
	if f.OnCleanup == nil {
		return func() {}
	}
	return f.OnCleanup(c)
}

// Interrupted is Interrupt.
func (f *Fake) Interrupted() bool { return f.Interrupt }

// say gives m to on, then OnAny, each when it's set.
func (f *Fake) say(on func(M), m M) {
	for _, g := range []func(M){on, f.OnAny} {
		if g != nil {
			g(m)
		}
	}
}
