package ui

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/msg"
)

// env is an environment for a UI's getenv.
type env map[string]string

func (e env) get(k string) string { return e[k] }

// testUI returns a UI on buffers, with input in, an empty environment and
// its run logs in a temporary folder.
func testUI(t *testing.T, in string) (u *UI, out, errOut *bytes.Buffer) {
	t.Helper()
	out, errOut = &bytes.Buffer{}, &bytes.Buffer{}
	u = New(strings.NewReader(in), out, errOut, env{}.get)
	// Plain, whatever FORCE_COLOR says, and messages as their names.
	u.outStyle, u.errStyle = style{names: true}, style{names: true}
	logs := filepath.Join(t.TempDir(), "logs", "runs")
	u.runLogs = func() (string, error) { return logs, nil }
	u.readPassword = func() (string, error) { return "", errors.New("no terminal") }
	t.Cleanup(func() {
		u.stopStatus()
		if u.log.f != nil {
			_ = u.log.f.Close()
		}
	})
	return u, out, errOut
}

// exitCode runs f, returning the code it exits with through die, or -1.
func exitCode(f func()) (code int) {
	defer func() {
		if p := recover(); p != nil {
			e, ok := p.(exitError)
			if !ok {
				panic(p)
			}
			code = e.code
		}
	}()
	f()
	return -1
}

func TestStreamsAndPrefixes(t *testing.T) {
	t.Parallel()
	m := msg.MsgUnknown("x")
	for _, tc := range []struct {
		name          string
		print         func(*UI, msg.M)
		stdout, stder string
	}{
		{"say", (*UI).Say, "msg_unknown name=x\n", ""},
		{"info", (*UI).Info, "==> msg_unknown name=x\n", ""},
		{"note", (*UI).Note, "", "msg_unknown name=x\n"},
		{"warn", (*UI).Warn, "", "devvm: msg_unknown name=x\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			u, out, errOut := testUI(t, "")
			tc.print(u, m)
			if out.String() != tc.stdout || errOut.String() != tc.stder {
				t.Errorf("stdout %q, stderr %q; want %q, %q", out, errOut, tc.stdout, tc.stder)
			}
			// With DataOnly, every message goes to stderr.
			u, out, errOut = testUI(t, "")
			u.DataOnly()
			tc.print(u, m)
			if out.Len() != 0 || errOut.String() != tc.stdout+tc.stder {
				t.Errorf("DataOnly: stdout %q, stderr %q; want nothing, %q", out, errOut, tc.stdout+tc.stder)
			}
		})
	}
}

func TestWarnAndDiePrefixEveryLine(t *testing.T) {
	if got, want := prefixLines("devvm: ", "one\ntwo"), "devvm: one\ndevvm: two\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	u, _, errOut := testUI(t, "")
	u.errStyle = style{}
	m := msg.MsgUnknown("a")
	u.Warn(m)
	if got, want := errOut.String(), "devvm: "+m.Text()+"\n"; got != want {
		t.Errorf("warn: got %q, want %q", got, want)
	}
}

// A run says each warning once: the same name and values again print
// nothing, other values do.
func TestWarnOnce(t *testing.T) {
	u, _, errOut := testUI(t, "")
	u.Warn(msg.MsgUnknown("a"))
	u.Warn(msg.MsgUnknown("b"))
	u.Warn(msg.MsgUnknown("a"))
	if got, want := errOut.String(), "devvm: msg_unknown name=a\ndevvm: msg_unknown name=b\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestDiePrintsAndExits1(t *testing.T) {
	t.Parallel()
	u, out, errOut := testUI(t, "")
	if code := exitCode(func() { u.die(msg.MsgUnknown("VM")) }); code != 1 {
		t.Errorf("exit code %d, want 1", code)
	}
	if got, want := errOut.String(), "devvm: msg_unknown name=VM\n"; got != want || out.Len() != 0 {
		t.Errorf("stderr %q, stdout %q; want %q and nothing", got, out, want)
	}
	// Interrupted, it dies quietly.
	errOut.Reset()
	u.interrupted.Store(true)
	if code := exitCode(func() { u.die(msg.MsgUnknown("VM")) }); code != 1 || errOut.Len() != 0 {
		t.Errorf("interrupted: exit code %d, stderr %q; want 1 and nothing", code, errOut)
	}
}

// question is a question for the tests, --force its answer.
func question(name string) msg.Question { return msg.MsgUnknown(name).AnsweredBy("--force") }

// atTerminal returns a UI whose input, in, is a terminal.
func atTerminal(t *testing.T, in string) (u *UI, out, errOut *bytes.Buffer) {
	t.Helper()
	u, out, errOut = testUI(t, in)
	u.stdinTTY = true
	return u, out, errOut
}

// refusal is err's refusal, its question and message by their names, or
// "not refused".
func refusal(err error) string {
	r, ok := errors.AsType[*msg.Refusal](err)
	if !ok {
		return fmt.Sprintf("not refused: %v", err)
	}
	return r.Question.Names() + " / " + r.Msg.Names()
}

func TestConfirm(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]bool{
		"\n": true, "y\n": true, "n\n": false, "N\n": false,
		"no": false, // the last line needn't end in a newline
	} {
		u, _, _ := atTerminal(t, in)
		if got, err := u.Confirm(question("x")); got != want || err != nil {
			t.Errorf("Confirm with %q = %v, %v; want %v", in, got, err, want)
		}
	}
	u, _, _ := atTerminal(t, "")
	if got, err := u.Confirm(question("x")); got || refusal(err) != " / no_input answer=--force" {
		t.Errorf("no answer at all: %v, %s", got, refusal(err))
	}
}

func TestAsk(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]bool{"y\n": true, "Yes\n": true, "n\n": false, "\n": false, "x\n": false} {
		u, _, _ := atTerminal(t, in)
		if got, err := u.Ask(question("me")); got != want || err != nil {
			t.Errorf("Ask with %q = %v, %v; want %v", in, got, err, want)
		}
	}
	u, _, _ := atTerminal(t, "")
	if got, err := u.Ask(question("me")); got || refusal(err) != " / no_input answer=--force" {
		t.Errorf("no answer at all: %v, %s", got, refusal(err))
	}
}

// A question's prompt and context are printed in a terminal; with piped
// answers, neither is.
func TestPromptsOnlyAtATerminal(t *testing.T) {
	t.Parallel()
	q := question("me").About(msg.MsgUnknown("about"))
	u, _, errOut := testUI(t, "y\n")
	u.SetFlags(Flags{PipedAnswers: true})
	if ok, err := u.Ask(q); !ok || err != nil || errOut.Len() != 0 {
		t.Errorf("piped: %v, %v, printed %q", ok, err, errOut)
	}
	u, _, errOut = atTerminal(t, "y\n\n")
	if ok, err := u.Ask(q); !ok || err != nil {
		t.Errorf("Ask: %v, %v", ok, err)
	}
	if got, want := errOut.String(), "msg_unknown name=about\nmsg_unknown name=me [y/N] "; got != want {
		t.Errorf("prompt %q, want %q", got, want)
	}
	errOut.Reset()
	if ok, err := u.Confirm(question("me")); !ok || err != nil {
		t.Errorf("Confirm: %v, %v", ok, err)
	}
	if got, want := errOut.String(), "msg_unknown name=me [Y/n] "; got != want {
		t.Errorf("prompt %q, want %q", got, want)
	}
}

// Answer reads a value without the spaces around it, "" for Enter.
func TestAnswer(t *testing.T) {
	t.Parallel()
	u, _, errOut := atTerminal(t, " 4 \n\n")
	for _, want := range []string{"4", ""} {
		if got, err := u.Answer(question("cpus")); got != want || err != nil {
			t.Errorf("Answer = %q, %v; want %q", got, err, want)
		}
	}
	if got, want := errOut.String(), "msg_unknown name=cpus msg_unknown name=cpus "; got != want {
		t.Errorf("prompts %q, want %q", got, want)
	}
}

func TestStdoutIsForData(t *testing.T) {
	t.Parallel()
	u, out, _ := testUI(t, "")
	_, _ = u.Stdout().Write([]byte("a table\n"))
	if out.String() != "a table\n" {
		t.Errorf("got %q", out)
	}
}

func TestStderrIsForLogLines(t *testing.T) {
	t.Parallel()
	u, out, errOut := testUI(t, "")
	_, _ = u.Stderr().Write([]byte("a log line\n"))
	if errOut.String() != "a log line\n" || out.Len() != 0 {
		t.Errorf("got %q, stdout %q", errOut, out)
	}
}

// A secret is read without echo at a terminal, after its context and
// prompt; with piped answers as a line, with neither; without a terminal
// it's refused. Line reads a line, all that's left at the end of input.
func TestSecret(t *testing.T) {
	t.Parallel()
	q := msg.TokenPrompt().AnsweredBy("--stdin").About(msg.GithubTokenHowto())
	u, out, errOut := testUI(t, "ghp_x\r\nrest")
	if v, err := u.Secret(q); !v.Empty() || refusal(err) != "token_prompt / no_input answer=--stdin" {
		t.Errorf("no terminal: %q, %s", v.Reveal(), refusal(err))
	}
	u.SetFlags(Flags{PipedAnswers: true})
	if v, err := u.Secret(q); v.Reveal() != "ghp_x" || err != nil || errOut.Len() != 0 {
		t.Errorf("piped: %q, %v, stderr %q", v.Reveal(), err, errOut)
	}
	if v := u.Line(); v != "rest" {
		t.Errorf("the end of input: %q", v)
	}
	if v := u.Line(); v != "" {
		t.Errorf("after it: %q", v)
	}
	if v, err := u.Secret(q); !v.Empty() || refusal(err) != "token_prompt / no_input answer=--stdin" {
		t.Errorf("piped, at the end of input: %q, %s", v.Reveal(), refusal(err))
	}
	u.readPassword = func() (string, error) { return "ghp_y", nil }
	u.stdinTTY = true
	if v, err := u.Secret(q); v.Reveal() != "ghp_y" || err != nil || errOut.String() != "github_token_howto\ntoken_prompt \n" || out.Len() != 0 {
		t.Errorf("terminal: %q, %v, stderr %q", v.Reveal(), err, errOut)
	}
	u.readPassword = func() (string, error) { return "", errors.New("closed") }
	if _, err := u.Secret(q); refusal(err) != " / no_input answer=--stdin" {
		t.Errorf("a terminal that can't be read: %s", refusal(err))
	}
}

// prompts are the UI's four prompts, each asking q.
func prompts() map[string]func(u *UI, q msg.Question) error {
	return map[string]func(u *UI, q msg.Question) error{
		"ask":     func(u *UI, q msg.Question) error { _, err := u.Ask(q); return err },
		"confirm": func(u *UI, q msg.Question) error { _, err := u.Confirm(q); return err },
		"answer":  func(u *UI, q msg.Question) error { _, err := u.Answer(q); return err },
		"secret":  func(u *UI, q msg.Question) error { _, err := u.Secret(q); return err },
	}
}

// refuse returns what the UI shows of err, a prompt's refusal, as a
// command's failure shows it (Refused): stderr from then on.
func refuse(u *UI, errOut *bytes.Buffer, err error) string {
	r, ok := errors.AsType[*msg.Refusal](err)
	if !ok {
		return fmt.Sprintf("not refused: %v", err)
	}
	errOut.Reset()
	u.Refused(r)
	return errOut.String()
}

// With --no-input every prompt is refused before anything is read or
// printed, even in a terminal: its refusal shows the question, then names
// what answers it; one RefusedWith made is its own refusal alone.
func TestNoInput(t *testing.T) {
	t.Parallel()
	for name, ask := range prompts() {
		for _, tc := range []struct {
			q    msg.Question
			want string
		}{
			{question("x"), "msg_unknown name=x\ndevvm: no_input answer=--force\n"},
			{msg.DeleteAnyway().RefusedWith(msg.NoTerminalDelete("devvm rm")), "devvm: no_terminal_delete command=devvm rm\n"},
		} {
			want := tc.want
			u, _, errOut := atTerminal(t, "y\n")
			u.SetFlags(Flags{NoInput: true})
			err := ask(u, tc.q.About(msg.MsgUnknown("about")))
			if errOut.Len() != 0 {
				t.Errorf("%s: printed %q before the refusal", name, errOut)
			}
			if got := refuse(u, errOut, err); got != want {
				t.Errorf("%s: shown %q, want %q", name, got, want)
			}
			if line := u.Line(); line != "y" {
				t.Errorf("%s: the answer was read: %q left", name, line)
			}
		}
	}
}

// Without a terminal, every prompt is refused in the same way, with
// nothing read or printed.
func TestNoTerminal(t *testing.T) {
	t.Parallel()
	for name, ask := range prompts() {
		u, _, errOut := testUI(t, "y\n")
		err := ask(u, question("x").About(msg.MsgUnknown("about")))
		if errOut.Len() != 0 {
			t.Errorf("%s: printed %q before the refusal", name, errOut)
		}
		if got, want := refuse(u, errOut, err), "msg_unknown name=x\ndevvm: no_input answer=--force\n"; got != want {
			t.Errorf("%s: shown %q, want %q", name, got, want)
		}
		if line := u.Line(); line != "y" {
			t.Errorf("%s: the answer was read: %q left", name, line)
		}
	}
}

// At the end of the input with no answer, a prompt is refused as it is
// with --no-input, after a newline ending the prompt in a terminal, where
// the question isn't shown again; with piped answers, it is.
func TestNoAnswer(t *testing.T) {
	t.Parallel()
	for name, ask := range prompts() {
		if name == "secret" {
			continue // a terminal's secret is read without the input (TestSecret)
		}
		u, _, errOut := atTerminal(t, "")
		err := ask(u, question("x"))
		if got, want := errOut.String(), "msg_unknown name=x"; !strings.HasPrefix(got, want) || !strings.HasSuffix(got, "\n") {
			t.Errorf("%s: prompt %q, want %q and a newline", name, got, want)
		}
		if got, want := refuse(u, errOut, err), "devvm: no_input answer=--force\n"; got != want {
			t.Errorf("%s: shown %q, want %q", name, got, want)
		}
	}
	for name, ask := range prompts() {
		u, _, errOut := testUI(t, "")
		u.SetFlags(Flags{PipedAnswers: true})
		if got, want := refuse(u, errOut, ask(u, question("x"))), "msg_unknown name=x\ndevvm: no_input answer=--force\n"; got != want {
			t.Errorf("piped %s: shown %q, want %q", name, got, want)
		}
	}
}

// Piped answers answer each question with a line of input, in order,
// without a terminal: the CLI tests' way to answer.
func TestPipedAnswers(t *testing.T) {
	t.Parallel()
	u, _, errOut := testUI(t, "y\nn\n8\n")
	u.SetFlags(Flags{PipedAnswers: true})
	ask, _ := u.Ask(question("a"))
	confirm, _ := u.Confirm(question("b"))
	answer, err := u.Answer(question("c"))
	if !ask || confirm || answer != "8" || err != nil || errOut.Len() != 0 {
		t.Errorf("ask %v, confirm %v, answer %q, %v; printed %q", ask, confirm, answer, err, errOut)
	}
}

// An answer read once the run is interrupted is refused: a question that
// waited through Ctrl+C never goes ahead.
func TestAnswerAfterInterrupt(t *testing.T) {
	t.Parallel()
	for name, ask := range prompts() {
		u, _, _ := atTerminal(t, "y\n")
		u.readPassword = func() (string, error) { return "y", nil }
		u.interrupted.Store(true)
		if got, want := refusal(ask(u, question("x"))), " / no_input answer=--force"; got != want {
			t.Errorf("%s: %s, want %s", name, got, want)
		}
	}
}

// Once the run is interrupted, a refusal prints nothing.
func TestRefusedInterrupted(t *testing.T) {
	t.Parallel()
	u, _, errOut := testUI(t, "")
	u.interrupted.Store(true)
	u.Refused(question("x").Refused(false))
	if errOut.Len() != 0 {
		t.Errorf("printed %q", errOut)
	}
}

// Terminal gives stdout and stderr as files only when stdin and stdout
// are both terminals and both streams are files, and otherwise neither.
func TestTerminal(t *testing.T) {
	out, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	errOut, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer errOut.Close()
	for _, c := range []struct {
		name          string
		stdin, stdout bool
		w, errW       io.Writer
		want          bool
	}{
		{"both terminals", true, true, out, errOut, true},
		{"input piped", false, true, out, errOut, false},
		{"output piped", true, false, out, errOut, false},
		{"output not a file", true, true, &bytes.Buffer{}, errOut, false},
		{"errors not a file", true, true, out, &bytes.Buffer{}, false},
	} {
		u := New(strings.NewReader(""), c.w, c.errW, env{}.get)
		u.stdinTTY, u.stdoutTTY = c.stdin, c.stdout
		u.saveTerminal = func() (func(), error) { return func() {}, nil }
		gotOut, gotErr, restore := Terminal(u)
		if (gotOut == nil) != (gotErr == nil) || (gotOut == nil) != (restore == nil) {
			t.Errorf("%s: Terminal = %v, %v, restore %v: files and restore come together or not at all", c.name, gotOut, gotErr, restore != nil)
		}
		if got := gotOut != nil; got != c.want {
			t.Errorf("%s: Terminal = %v, %v, want files %v", c.name, gotOut, gotErr, c.want)
		}
		if c.want && (gotOut != out || gotErr != errOut) {
			t.Errorf("%s: Terminal = %v, %v, want devvm's own", c.name, gotOut, gotErr)
		}
	}
}

// Terminal saves the terminal's state, and what it returns puts it back
// once, as does a cleanup when the run ends first, as an interrupted
// one's does; a terminal whose state can't be read has nothing to put
// back.
func TestTerminalRestores(t *testing.T) {
	out, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	for _, ended := range []bool{false, true} {
		u := New(strings.NewReader(""), out, out, env{}.get)
		u.stdinTTY, u.stdoutTTY = true, true
		saved, restored := 0, 0
		u.saveTerminal = func() (func(), error) { saved++; return func() { restored++ }, nil }
		_, _, restore := Terminal(u)
		if ended {
			u.runCleanups()
		}
		restore()
		u.runCleanups()
		if saved != 1 || restored != 1 {
			t.Errorf("run ended first %v: saved %d, restored %d, want once each", ended, saved, restored)
		}
	}
	u := New(strings.NewReader(""), out, out, env{}.get)
	u.stdinTTY, u.stdoutTTY = true, true
	u.saveTerminal = func() (func(), error) { return nil, errors.New("not a terminal") }
	if _, _, restore := Terminal(u); restore == nil {
		t.Error("a state it can't read: no restore")
	} else {
		restore()
	}
}
