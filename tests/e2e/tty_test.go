//go:build e2e

package e2e

import (
	"io"
	"os"
	"regexp"
	"runtime"
	"time"
)

// TTYRun runs args in dir on a terminal of its own (macOS's script,
// Python's pty on Linux, where script isn't always installed), input
// typed into it, as a question in a terminal needs. Its output is the
// terminal's. input is typed once the question ([y/N] or [Y/n]) shows,
// within 30 s, and the input stays open a moment after, as the end of
// input in a terminal is an answer of its own.
func TTYRun(s *S, dir, input string, args ...string) (Res, error) {
	var at int64
	if st, err := os.Stat(s.sb.logFile()); err == nil {
		at = st.Size()
	}
	return s.Run(Cmd{Args: OnTTY(args...), Dir: dir, Input: &typist{log: s.sb.logFile(), at: at, input: input + "\n"}})
}

// OnTTY is args run on a terminal of their own: macOS's script, or
// Python's pty on Linux, where script isn't always installed.
func OnTTY(args ...string) []string {
	if runtime.GOOS != "darwin" {
		return append([]string{"python3", "-c", "import os, pty, sys; sys.exit(os.waitstatus_to_exitcode(pty.spawn(sys.argv[1:])))"}, args...)
	}
	return append([]string{"script", "-q", "/dev/null"}, args...)
}

// typist is a terminal's input: its answer once the check's log, past
// at, shows a question, then nothing for 3 s, then its end.
type typist struct {
	log   string
	at    int64
	input string
	typed bool
}

func (t *typist) Read(p []byte) (int, error) {
	if t.typed {
		time.Sleep(3 * time.Second)
		return 0, io.EOF
	}
	question := regexp.MustCompile(`\[(y/N|Y/n)\]`)
	for range 150 {
		if b, err := os.ReadFile(t.log); err == nil && int64(len(b)) > t.at && question.Match(b[t.at:]) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.typed = true
	return copy(p, t.input), nil
}
