package msg

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"slices"
	"testing"
)

// A zero Fake is quiet: it prints nothing, has no terminal, answers no
// and "", drops what's written, runs a logged command as it is, keeps no
// cleanup and isn't interrupted.
func TestFakeQuiet(t *testing.T) {
	var f Fake
	m := NoVM("/x")
	f.Info(m)
	f.Say(m)
	f.Note(m)
	f.Warn(m)
	f.LogNote()
	f.Status(m)()
	f.AddCleanup(func(context.Context) { t.Error("kept") })()
	q := m.AnsweredBy("--force")
	for _, err := range []error{second(f.Ask(q)), second(f.Confirm(q)), second(f.Answer(q))} {
		if r, ok := errors.AsType[*Refusal](err); !ok || r.Msg.Names() != "no_input answer=--force" {
			t.Errorf("not refused: %v", err)
		}
	}
	if f.CanPick() || f.Interrupted() {
		t.Error("a terminal, the picker or an interrupt")
	}
	for _, w := range []io.Writer{f.Stdout(), f.Stderr(), f.LogWriter()} {
		if w != io.Discard {
			t.Errorf("writer %v", w)
		}
	}
	ctx := t.Context()
	if f.WithSteps(ctx, 3) != ctx {
		t.Error("steps counted")
	}
	if err := f.LoggedCmd(ctx, m, exec.CommandContext(ctx, "true")); err != nil {
		t.Errorf("LoggedCmd: %v", err)
	}
}

// A Fake's fields get what its methods are given, and stand for the
// terminal, the writers and an interrupt.
func TestFakeRecords(t *testing.T) {
	var kinds, all Recorder
	var asked []string
	var out, errs, log bytes.Buffer
	var cleanups int
	f := &Fake{
		OnInfo: kinds.Add, OnSay: kinds.Add, OnNote: kinds.Add, OnWarn: kinds.Add, OnAny: all.Add,
		OnAsk:     func(m M) bool { asked = append(asked, "ask "+m.Names()); return true },
		OnConfirm: func(m M) bool { asked = append(asked, "confirm "+m.Names()); return true },
		OnAnswer:  func(m M) string { asked = append(asked, "answer "+m.Names()); return "4" },
		OnCleanup: func(func(context.Context)) func() { cleanups++; return func() {} },
		Terminal:  true, Picks: true, Interrupt: true,
		Out: &out, Err: &errs, Log: &log,
	}
	answered := func(v any, err error) bool { return err == nil && (v == true || v == "4") }
	f.Info(NoVM("/i"))
	f.Say(NoVM("/s"))
	f.Note(NoVM("/n"))
	f.Warn(NoVM("/w"))
	want := []string{"no_vm dir=/i", "no_vm dir=/s", "no_vm dir=/n", "no_vm dir=/w"}
	if !slices.Equal(kinds.Names(), want) || !slices.Equal(all.Names(), want) {
		t.Errorf("kinds %q, all %q", kinds.Names(), all.Names())
	}
	kinds = Recorder{}
	if !answered(f.Ask(NoVM("/a").AnsweredBy("--force").About(NoVM("/about")))) ||
		!answered(f.Confirm(NoVM("/c").AnsweredBy("--force"))) || !answered(f.Answer(NoVM("/v").AnsweredBy("--force"))) {
		t.Error("not answered")
	}
	if want := []string{"ask no_vm dir=/a", "confirm no_vm dir=/c", "answer no_vm dir=/v"}; !slices.Equal(asked, want) {
		t.Errorf("asked %q", asked)
	}
	if want := []string{"no_vm dir=/about"}; !slices.Equal(kinds.Names(), want) {
		t.Errorf("context %q, want %q", kinds.Names(), want)
	}
	f.AddCleanup(func(context.Context) {})
	if cleanups != 1 || !f.CanPick() || !f.Interrupted() {
		t.Error("its fields aren't what it says")
	}
	if f.Stdout() != &out || f.Stderr() != &errs || f.LogWriter() != &log {
		t.Error("not its writers")
	}
}

// A Fake gives each status line's label and each logged command's step
// to OnStatus, and shows the innermost one until it stops, as the UI's
// status line does.
func TestFakeStatus(t *testing.T) {
	var labels Recorder
	f := &Fake{OnStatus: labels.Add}
	if _, on := f.Showing(); on {
		t.Fatal("a line showing before any started")
	}
	shows := func(want string) {
		t.Helper()
		got, on := f.Showing()
		if !on && want != "" || on && got.Names() != want {
			t.Errorf("showing %q (%v), want %q", got.Names(), on, want)
		}
	}
	outer := f.Status(NoVM("/outer"))
	inner := f.Status(NoVM("/inner"))
	shows("no_vm dir=/inner")
	inner()
	inner()
	shows("no_vm dir=/outer")
	ctx := t.Context()
	if err := f.LoggedCmd(ctx, NoVM("/step"), exec.CommandContext(ctx, "true")); err != nil {
		t.Fatal(err)
	}
	shows("no_vm dir=/outer")
	// Stopped out of order, the inner one stays.
	second := f.Status(NoVM("/second"))
	outer()
	shows("no_vm dir=/second")
	second()
	shows("")
	if want := []string{"no_vm dir=/outer", "no_vm dir=/inner", "no_vm dir=/step", "no_vm dir=/second"}; !slices.Equal(labels.Names(), want) {
		t.Errorf("labels %q, want %q", labels.Names(), want)
	}
}

// second is a two-value call's error.
func second[T any](_ T, err error) error { return err }
