package ui

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/msg"
)

// TestMain makes the test binary a stand-in for devvm when UI_TEST_HELPER is
// set, for the tests that send it signals.
func TestMain(m *testing.M) {
	if h := os.Getenv("UI_TEST_HELPER"); h != "" {
		os.Exit(helper(h)) //nolint:forbidigo // the test binary as a helper process
	}
	os.Exit(m.Run()) //nolint:forbidigo // the test binary
}

// helper runs a devvm-like run through Main: it registers a cleanup that
// records itself in $UI_TEST_RECORD, says it's ready, then runs a long
// command. With "slow cleanup", the cleanup takes a while, for a second
// signal to land in.
func helper(mode string) int {
	record := func(s string) {
		f, err := os.OpenFile(os.Getenv("UI_TEST_RECORD"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err == nil {
			_, _ = f.WriteString(s + "\n")
			_ = f.Close()
		}
	}
	return Main(func(ctx context.Context, u *UI) int {
		if mode == "stuck without cleanups" {
			u.Say(msg.MsgUnknown("ready"))
			return blocked(ctx, "stuck command", record)
		}
		u.AddCleanup(func(context.Context) {
			record("cleanup interrupted=" + map[bool]string{true: "yes", false: "no"}[u.Interrupted()])
			if mode == "slow cleanup" {
				time.Sleep(500 * time.Millisecond)
				record("cleanup done")
			}
		})
		if mode == "question" { // its input stays open, without an answer
			return unanswered(u, record)
		}
		u.Say(msg.MsgUnknown("ready"))
		if mode == "blocked command" || mode == "stuck command" {
			return blocked(ctx, mode, record)
		}
		argv := []string{"sleep", "30"}
		if mode == "command interrupts" { // as Ctrl+C reaches both, the command ending first
			argv = []string{"sh", "-c", "kill -INT $PPID; exit 130"}
		}
		if err := u.logged(ctx, msg.MsgUnknown("sleeping"), argv...); err != nil {
			u.ErrorLogged(msg.MsgUnknown("sleep failed"))
			return 1
		}
		record("not interrupted")
		return 0
	})
}

// unanswered asks a question its input never answers, saying it's ready
// once the question waits.
func unanswered(u *UI, record func(string)) int {
	u.SetFlags(Flags{PipedAnswers: true})
	go func() {
		for u.asking.Load() == 0 {
			time.Sleep(time.Millisecond)
		}
		u.Say(msg.MsgUnknown("ready"))
	}()
	_, _ = u.Ask(msg.MsgUnknown("q").AnsweredBy("--force"))
	record("answered")
	return 1
}

// blocked is a command whose own code takes a while to return once its
// context is canceled ("blocked command"), or never does ("stuck
// command"), as a question waiting for its answer doesn't.
func blocked(ctx context.Context, mode string, record func(string)) int {
	<-ctx.Done()
	if mode == "stuck command" {
		time.Sleep(time.Minute)
	}
	time.Sleep(300 * time.Millisecond)
	record("command returned")
	return 1
}

func TestCleanupsRunLastFirst(t *testing.T) {
	t.Parallel()
	u, _, errOut := testUI(t, "")
	code := u.main(func(context.Context) int {
		u.AddCleanup(func(context.Context) { u.Note(msg.MsgUnknown("first")) })
		u.AddCleanup(func(context.Context) { u.die(msg.MsgUnknown("dying cleanup")) })
		remove := u.AddCleanup(func(context.Context) { u.Note(msg.MsgUnknown("removed")) })
		u.AddCleanup(func(ctx context.Context) {
			if ctx.Err() != nil {
				u.Note(msg.MsgUnknown("canceled context"))
			}
			u.Note(msg.MsgUnknown("last"))
		})
		remove()
		remove()
		u.die(msg.MsgUnknown("failed"))
		return 0
	})
	if code != 1 {
		t.Errorf("exit code %d, want 1", code)
	}
	// The message, then the cleanups, last first; one that dies doesn't stop
	// the rest.
	want := "devvm: msg_unknown name=failed\nmsg_unknown name=last\ndevvm: msg_unknown name=dying cleanup\nmsg_unknown name=first\n"
	if got := errOut.String(); got != want {
		t.Errorf("stderr:\n%s\nwant:\n%s", got, want)
	}
}

func TestMainReturnsTheCode(t *testing.T) {
	t.Parallel()
	ran := 0
	u, _, _ := testUI(t, "")
	if code := u.main(func(context.Context) int {
		u.AddCleanup(func(context.Context) { ran++ })
		return 3
	}); code != 3 || ran != 1 {
		t.Errorf("exit code %d, cleanups run %d; want 3 and 1", code, ran)
	}
	// The UI's own failure (die) exits 1 too.
	if code := u.main(func(context.Context) int { u.die(msg.MsgUnknown("x")); return 0 }); code != 1 {
		t.Errorf("die: exit code %d, want 1", code)
	}
}

func TestOtherPanicsRunTheCleanupsAndPanic(t *testing.T) {
	t.Parallel()
	u, _, _ := testUI(t, "")
	ran := false
	defer func() {
		p, _ := recover().(string)
		if !strings.HasPrefix(p, "boom\n") || !strings.Contains(p, "goroutine") || !ran {
			t.Errorf("panic %q, cleanup ran %v", p, ran)
		}
	}()
	u.main(func(context.Context) int {
		u.AddCleanup(func(context.Context) { ran = true })
		panic("boom")
	})
}

// startHelper starts the test binary as the helper and waits until it's
// ready.
func startHelper(t *testing.T, mode string) (cmd *exec.Cmd, record string, stderr *strings.Builder) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	record = filepath.Join(t.TempDir(), "record")
	cmd = exec.CommandContext(t.Context(), exe) //nolint:gosec // the test binary
	cmd.Env = append(os.Environ(), "UI_TEST_HELPER="+mode, "UI_TEST_RECORD="+record,
		"DEVVM_MESSAGES=names", "HOME="+t.TempDir(), "XDG_DATA_HOME="+t.TempDir())
	stderr = &strings.Builder{}
	cmd.Stderr = stderr
	// Its input stays open until it ends, so a question waits.
	if _, err := cmd.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if line != "msg_unknown name=ready\n" {
		t.Fatalf("helper said %q (%v); stderr %s", line, err, stderr)
	}
	return cmd, record, stderr
}

func exitStatus(t *testing.T, err error) int {
	t.Helper()
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		return exitErr.ExitCode()
	}
	t.Fatalf("helper: %v", err)
	return 0
}

func TestSignalsRunTheCleanups(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			cmd, record, stderr := startHelper(t, "cleanup")
			start := time.Now()
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			if code := exitStatus(t, cmd.Wait()); code != InterruptedCode {
				t.Errorf("exit code %d, want %d", code, InterruptedCode)
			}
			// It stops the command running (sleep 30), runs the cleanup,
			// and doesn't go on; the command's failure prints nothing.
			if took := time.Since(start); took > 10*time.Second {
				t.Errorf("took %v", took)
			}
			if got, want := readFile(t, record), "cleanup interrupted=yes\n"; got != want {
				t.Errorf("record %q, want %q", got, want)
			}
			if got := stderr.String(); got != "\n" {
				t.Errorf("stderr %q, want a newline only", got)
			}
		})
	}
}

// The cleanups wait for the command to return: they'd otherwise undo its
// work while its own code still runs.
func TestSignalsWaitForTheCommandBeforeTheCleanups(t *testing.T) {
	cmd, record, stderr := startHelper(t, "blocked command")
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	if code := exitStatus(t, cmd.Wait()); code != InterruptedCode {
		t.Errorf("exit code %d, want %d", code, InterruptedCode)
	}
	if got, want := readFile(t, record), "command returned\ncleanup interrupted=yes\n"; got != want {
		t.Errorf("record %q, want %q", got, want)
	}
	if got := stderr.String(); got != "\n" {
		t.Errorf("stderr %q, want a newline only", got)
	}
}

// A command that never returns keeps the cleanups waiting for
// returnGrace, no longer.
func TestSignalsWaitForAStuckCommandAWhile(t *testing.T) {
	cmd, record, _ := startHelper(t, "stuck command")
	start := time.Now()
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	if code := exitStatus(t, cmd.Wait()); code != InterruptedCode {
		t.Errorf("exit code %d, want %d", code, InterruptedCode)
	}
	if took := time.Since(start); took < returnGrace || took > returnGrace+10*time.Second {
		t.Errorf("took %v, want about %v", took, returnGrace)
	}
	if got, want := readFile(t, record), "cleanup interrupted=yes\n"; got != want {
		t.Errorf("record %q, want %q", got, want)
	}
}

// A question waiting for its answer isn't waited for: it doesn't return
// at Ctrl+C, and an answer typed after it would be refused.
func TestSignalsDontWaitForAQuestion(t *testing.T) {
	cmd, record, _ := startHelper(t, "question")
	start := time.Now()
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	if code := exitStatus(t, cmd.Wait()); code != InterruptedCode {
		t.Errorf("exit code %d, want %d", code, InterruptedCode)
	}
	if took := time.Since(start); took >= returnGrace {
		t.Errorf("took %v, want less than %v", took, returnGrace)
	}
	if got, want := readFile(t, record), "cleanup interrupted=yes\n"; got != want {
		t.Errorf("record %q, want %q", got, want)
	}
}

// Without cleanups, nothing waits: a background process that doesn't
// watch its context (the proxy) still ends as soon as it's told to.
func TestSignalsWithoutCleanupsEndAtOnce(t *testing.T) {
	cmd, record, _ := startHelper(t, "stuck without cleanups")
	start := time.Now()
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := exitStatus(t, cmd.Wait()); code != InterruptedCode {
		t.Errorf("exit code %d, want %d", code, InterruptedCode)
	}
	if took := time.Since(start); took >= returnGrace {
		t.Errorf("took %v, want less than %v", took, returnGrace)
	}
	if _, err := os.Stat(record); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the command returned (record: %v), want it still blocked as the run ended", err)
	}
}

// A command that ends at the same Ctrl+C as devvm, before devvm notices it:
// its failure prints nothing either.
func TestACommandEndedByTheSameSignalPrintsNothing(t *testing.T) {
	for range 20 {
		cmd, record, stderr := startHelper(t, "command interrupts")
		if code := exitStatus(t, cmd.Wait()); code != InterruptedCode {
			t.Fatalf("exit code %d, want %d; stderr %q", code, InterruptedCode, stderr)
		}
		if got := stderr.String(); got != "\n" {
			t.Fatalf("stderr %q, want a newline only", got)
		}
		if got, want := readFile(t, record), "cleanup interrupted=yes\n"; got != want {
			t.Fatalf("record %q, want %q", got, want)
		}
	}
}

func TestKilled(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		script string
		want   bool
	}{{"exit 0", false}, {"exit 1", false}, {"exit 127", false}, {"exit 130", true}, {"kill -TERM $$", true}} {
		err := exec.CommandContext(t.Context(), "sh", "-c", c.script).Run() //nolint:gosec // the test's own scripts
		if got := killed(err); got != c.want {
			t.Errorf("%s: killed = %v, want %v", c.script, got, c.want)
		}
	}
}

func TestASecondSignalDoesntStopTheCleanups(t *testing.T) {
	cmd, record, _ := startHelper(t, "slow cleanup")
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if b, _ := os.ReadFile(record); len(b) > 0 { //nolint:gosec // the test's own file
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the cleanup never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	if code := exitStatus(t, cmd.Wait()); code != InterruptedCode {
		t.Errorf("exit code %d, want %d", code, InterruptedCode)
	}
	if got, want := readFile(t, record), "cleanup interrupted=yes\ncleanup done\n"; got != want {
		t.Errorf("record %q, want %q", got, want)
	}
}
