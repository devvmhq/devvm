package ui

import (
	"bufio"
	"io"
	"os"
	"regexp"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/devvmhq/devvm/internal/msg"
)

func TestDuration(t *testing.T) {
	t.Parallel()
	for secs, want := range map[int]string{0: "0s", 5: "5s", 59: "59s", 60: "1m00s", 125: "2m05s"} {
		if got := duration(secs); got != want {
			t.Errorf("duration(%d) = %q, want %q", secs, got, want)
		}
	}
}

func TestStatusText(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		label   string
		secs    int
		logLine string
		cols    int
		want    string
	}{
		// The log line's last carriage-return segment, without tabs.
		{"running base.sh", 65, "Get:1 50%\rGet:2 https://example.com/x\t", 80, "running base.sh 1m05s  Get:2 https://example.com/x"},
		// As much as fits.
		{"running base.sh", 1, strings.Repeat("0", 100), 50, "running base.sh 1s  " + strings.Repeat("0", 23)},
		// No room: none of it.
		{"running base.sh", 1, "anything", 30, "running base.sh 1s"},
		{"starting x", 1, "", 80, "starting x 1s"},
		// No control characters from the log, C1 in UTF-8 included.
		{"x", 1, "a\033[2Jb\302\233c", 80, "x 1s  a[2Jbc"},
	} {
		if text, log := statusText(tc.label, tc.secs, tc.logLine, tc.cols); text+log != tc.want {
			got := text + log
			t.Errorf("statusText(%q, %d, %q, %d) = %q, want %q", tc.label, tc.secs, tc.logLine, tc.cols, got, tc.want)
		}
	}
}

func TestClearLine(t *testing.T) {
	t.Parallel()
	// After the terminal narrows, clearing covers the rows the last line now
	// wraps onto.
	for _, tc := range []struct {
		drawn, cols int
		want        string
	}{
		{100, 40, "\r\033[K\033[A\033[K\033[A\033[K"},
		{40, 40, "\r\033[K"},
		{100, 0, "\r\033[K\033[A\033[K"},
		{0, 80, "\r\033[K"},
	} {
		if got := clearLine(tc.drawn, tc.cols); got != tc.want {
			t.Errorf("clearLine(%d, %d) = %q, want %q", tc.drawn, tc.cols, got, tc.want)
		}
	}
}

func TestGlyphs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		env  map[string]string
		done string
	}{
		{map[string]string{"LANG": "en_US.UTF-8"}, "✓"},
		{map[string]string{"LC_ALL": "C", "LANG": "en_US.UTF-8"}, "ok"},
		{map[string]string{"LC_CTYPE": "en_US.utf8"}, "✓"},
		{map[string]string{}, "ok"},
	} {
		g := glyphs(func(k string) string { return tc.env[k] })
		if g[0] != tc.done || len(g) < 3 {
			t.Errorf("glyphs with %v = %q, want %q first, then frames", tc.env, g, tc.done)
		}
	}
	u, _, _ := testUI(t, "")
	u.getenv = env{"LC_ALL": "C"}.get
	if got, want := u.doneLine([]part{{"[1/2] starting x", ""}}, 75), "    ok [1/2] starting x (1m15s)\n"; got != want {
		t.Errorf("doneLine: got %q, want %q", got, want)
	}
}

func TestStatusLineInATerminal(t *testing.T) {
	t.Parallel()
	u, _, errOut := testUI(t, "")
	u.stderrTTY = true
	u.termWidth = func() int { return 40 }
	u.getenv = env{"LC_ALL": "C"}.get
	if err := u.logged(t.Context(), msg.MsgUnknown("x"), "sh", "-c", "echo progress"); err != nil {
		t.Fatal(err)
	}
	got := errOut.String()
	// Drawn at least once, without wrapping, then cleared; quick, so it
	// leaves nothing behind.
	if !strings.HasPrefix(got, "\r\033[K\033[?7l    | msg_unknown name=x 0s") || !strings.HasSuffix(got, "\033[?7h\r\033[K") {
		t.Errorf("status line: %q", got)
	}
	for line := range strings.SplitSeq(got, "\033[?7l") {
		text, _, _ := strings.Cut(line, "\033[?7h")
		if len([]rune(text)) > 39 {
			t.Errorf("wider than the terminal: %q", text)
		}
	}

	// Status, around work that isn't one command.
	errOut.Reset()
	stop := u.Status(msg.MsgUnknown("y"))
	stop()
	stop()
	if got := errOut.String(); !strings.Contains(got, "msg_unknown name=y 0s") || !strings.HasSuffix(got, "\r\033[K") {
		t.Errorf("Status: %q", got)
	}
	// Not in a terminal, or verbose: none.
	errOut.Reset()
	u.stderrTTY = false
	u.Status(msg.MsgUnknown("y"))()
	u.stderrTTY = true
	u.SetFlags(Flags{Verbose: true})
	u.Status(msg.MsgUnknown("y"))()
	if errOut.Len() != 0 {
		t.Errorf("status line outside a terminal: %q", errOut)
	}
	// Nor for a logged command while verbose, whose lines are streamed.
	if err := u.logged(t.Context(), msg.MsgUnknown("z"), "true"); err != nil {
		t.Fatal(err)
	}
	if got := errOut.String(); strings.Contains(got, "msg_unknown") || !strings.Contains(got, "+ true\n") {
		t.Errorf("verbose logged command: %q", got)
	}
}

// statusUI is a UI whose stderr is a 40-column terminal in the C locale,
// so its status lines draw, and what it wrote there so far, read under
// the lock the status line writes with.
func statusUI(t *testing.T, in io.Reader) (*UI, func() string) {
	t.Helper()
	u, _, errOut := testUI(t, "")
	u.stdin = bufio.NewReader(in)
	u.stderrTTY = true
	u.termWidth = func() int { return 40 }
	u.getenv = env{"LC_ALL": "C"}.get
	return u, func() string {
		u.outMu.Lock()
		defer u.outMu.Unlock()
		return errOut.String()
	}
}

// A status line inside another shows its own until it stops, then the
// outer one's is drawn again; the outer one's stop clears it, and stopping
// out of order draws nothing over the inner one.
func TestStatusLinesNest(t *testing.T) {
	t.Parallel()
	u, shown := statusUI(t, strings.NewReader(""))
	outer := u.Status(msg.MsgUnknown("outer"))
	inner := u.Status(msg.MsgUnknown("inner"))
	inner()
	after := len(shown())
	outer()
	got := shown()
	if !strings.Contains(got, "msg_unknown name=inner") {
		t.Fatalf("no inner line: %q", got)
	}
	// After the inner line's stop: cleared, then the outer one again.
	again := regexp.MustCompile(`\r\x1b\[K\r\x1b\[K\x1b\[\?7l    . msg_unknown name=outer`)
	if rest := got[strings.Index(got, "msg_unknown name=inner"):after]; !again.MatchString(rest) {
		t.Errorf("outer line not drawn again after the inner one: %q", rest)
	}
	if !strings.HasSuffix(got, "\033[?7h\r\033[K") {
		t.Errorf("not cleared at the end: %q", got)
	}

	// The outer one stopped first: the inner one stays.
	outer = u.Status(msg.MsgUnknown("outer2"))
	inner = u.Status(msg.MsgUnknown("inner2"))
	before := len(shown())
	outer()
	if got := shown()[before:]; got != "" {
		t.Errorf("an outer line's stop wrote %q over the inner one", got)
	}
	inner()
	if got := shown(); !strings.HasSuffix(got, "\r\033[K") || strings.Contains(got[before:], "outer2") {
		t.Errorf("an outer line drawn after its stop: %q", got[before:])
	}
}

// Once the run is interrupted, no status line starts: nothing would clear
// one started after Main's last stop.
func TestNoStatusLineOnceInterrupted(t *testing.T) {
	t.Parallel()
	u, shown := statusUI(t, strings.NewReader(""))
	u.interrupted.Store(true)
	u.Status(msg.MsgUnknown("s"))()
	if err := u.logged(t.Context(), msg.MsgUnknown("x"), "true"); err != nil {
		t.Fatal(err)
	}
	if got := shown(); got != "" {
		t.Errorf("drawn once interrupted: %q", got)
	}
}

// A message said under a status line clears it first and draws it again
// after, so the message never lands on the line, on stdout too when it's
// the terminal.
func TestMessagesClearTheStatusLine(t *testing.T) {
	t.Parallel()
	u, shown := statusUI(t, strings.NewReader(""))
	u.stdout, u.stdoutTTY = u.stderr, true
	stop := u.Status(msg.MsgUnknown("s"))
	defer stop()
	u.Warn(msg.MsgUnknown("w"))
	u.Info(msg.MsgUnknown("i"))
	_, _ = io.WriteString(u.Stderr(), "data\n")
	got := shown()
	for _, line := range []string{"devvm: msg_unknown name=w\n", "==> msg_unknown name=i\n", "data\n"} {
		if !strings.Contains(got, "\033[?7h\r\033[K"+line+"\r\033[K\033[?7l") {
			t.Errorf("%q not cleared before and drawn after: %q", line, got)
		}
	}

	// A line not ended keeps the status line off until one is.
	_, _ = io.WriteString(u.Stderr(), "part")
	before := len(shown())
	_, _ = io.WriteString(u.Stderr(), "ial")
	if got := shown()[before:]; got != "ial" {
		t.Errorf("drawn after an unended line: %q", got)
	}
	_, _ = io.WriteString(u.Stderr(), "\n")
	if got := shown()[before:]; !strings.HasPrefix(got, "ial\n\r\033[K\033[?7l") {
		t.Errorf("not drawn once the line ended: %q", got)
	}
}

// A question takes the status line off until it's answered, however long
// that takes, so the line never draws over its prompt or what's typed,
// and draws it again after.
func TestQuestionsHoldTheStatusLine(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		in, typed := io.Pipe()
		u, shown := statusUI(t, in)
		u.stdinTTY = true
		stop := u.Status(msg.MsgUnknown("s"))
		defer stop()
		answered := make(chan bool)
		go func() {
			yes, _ := u.Ask(msg.MsgUnknown("q").AnsweredBy("--force").About(msg.MsgUnknown("about")))
			answered <- yes
		}()
		time.Sleep(2 * time.Second) // ten redraws' time, had it any
		synctest.Wait()
		got := shown()
		prompt := strings.Index(got, "msg_unknown name=q [y/N] ")
		if prompt < 0 || !strings.HasSuffix(got, "msg_unknown name=q [y/N] ") {
			t.Fatalf("the status line drew over the prompt: %q", got)
		}
		// Cleared once, before the question's context, and not drawn
		// again between it and the prompt.
		about := strings.Index(got, "msg_unknown name=about\n")
		if about < 0 || !strings.HasSuffix(got[:about], "\r\033[K") || strings.Contains(got[about:prompt], "\033[?7l") {
			t.Errorf("the status line drawn between the question's context and its prompt: %q", got)
		}
		_, _ = io.WriteString(typed, "y\n")
		if !<-answered {
			t.Error("the answer: no")
		}
		if got := shown(); !strings.Contains(got[prompt:], "\033[?7l") {
			t.Errorf("not drawn again once answered: %q", got[prompt:])
		}
	})
}

// A secret's prompt and the picker hold the status line too, until the
// secret is read or the choice made, then draw it again.
func TestSecretAndPickerHoldTheStatusLine(t *testing.T) {
	t.Parallel()
	for name, ask := range map[string]func(u *UI){
		"secret": func(u *UI) {
			_, _ = u.Secret(msg.MsgUnknown("token").RefusedWith(msg.MsgUnknown("no")).About(msg.MsgUnknown("about")))
		},
		"picker": func(u *UI) { u.PickOne(msg.MsgUnknown("pick"), choices()) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				in, typed := io.Pipe()
				u, shown := statusUI(t, in)
				u.stdinTTY, u.stdoutTTY = true, true
				u.rawInput = func() (func(), error) { return func() {}, nil }
				u.termSize = func() (int, int) { return 60, 20 }
				read := make(chan string)
				u.readPassword = func() (string, error) { return <-read, nil }
				stop := u.Status(msg.MsgUnknown("s"))
				defer stop()
				before := len(shown())
				done := make(chan struct{})
				go func() {
					ask(u)
					close(done)
				}()
				time.Sleep(2 * time.Second) // ten redraws' time, had it any
				synctest.Wait()
				if got := shown()[before:]; strings.Contains(got, "\033[?7l") {
					t.Errorf("drawn while it waited: %q", got)
				}
				if name == "secret" {
					read <- "x"
				} else {
					_, _ = io.WriteString(typed, "\r")
				}
				<-done
				if got := shown()[before:]; !strings.Contains(got, "\033[?7l") {
					t.Errorf("not drawn again once done: %q", got)
				}
			})
		})
	}
}

// A command on the terminal (Terminal) holds the status line until its
// restore.
func TestTerminalHoldsTheStatusLine(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		u, _ := statusUI(t, strings.NewReader(""))
		f, err := os.CreateTemp(t.TempDir(), "tty")
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		u.stdout, u.stderr, u.stdinTTY, u.stdoutTTY = f, f, true, true
		u.saveTerminal = func() (func(), error) { return func() {}, nil }
		shown := func() string {
			u.outMu.Lock()
			defer u.outMu.Unlock()
			b, err := os.ReadFile(f.Name())
			if err != nil {
				t.Fatal(err)
			}
			return string(b)
		}
		stop := u.Status(msg.MsgUnknown("s"))
		defer stop()
		before := len(shown())
		_, _, restore := Terminal(u)
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if got := shown()[before:]; got != "\r\033[K" {
			t.Errorf("while the terminal was held, wrote %q, want the line cleared alone", got)
		}
		restore()
		if got := shown()[before:]; !strings.Contains(got, "\033[?7l") {
			t.Errorf("not drawn again after the restore: %q", got)
		}
	})
}

// While verbose, a logged failure names the run log, whose lines were
// streamed, instead of printing its end again.
func TestErrorLoggedVerbose(t *testing.T) {
	t.Parallel()
	u, _, errOut := testUI(t, "")
	u.SetFlags(Flags{Verbose: true})
	_ = u.logged(t.Context(), msg.M{}, "sh", "-c", "echo failing; exit 1")
	u.ErrorLogged(msg.MsgUnknown("x"))
	want := "+ sh -c echo failing; exit 1\nfailing\nlog_details file=" + u.logPath() + "\ndevvm: msg_unknown name=x\n"
	if got := errOut.String(); got != want {
		t.Errorf("stderr %q, want %q", got, want)
	}
}

func TestTailLines(t *testing.T) {
	t.Parallel()
	path := t.TempDir() + "/log"
	var b strings.Builder
	for i := range 30 {
		b.WriteString(strings.Repeat("x", i) + "\n")
	}
	writeFile(t, path, b.String())
	got := tailLines(path, 25)
	if lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n"); len(lines) != 25 || lines[24] != strings.Repeat("x", 29) {
		t.Errorf("tailLines: %q", got)
	}
	if got, want := lastLine(path), strings.Repeat("x", 29); got != want {
		t.Errorf("lastLine: %q, want %q", got, want)
	}
	if lastLine("") != "" || tailLines(path+".none", 25) != "" {
		t.Error("no file: want nothing")
	}
}
