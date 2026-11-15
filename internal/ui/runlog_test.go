package ui

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/msg"
)

// logged runs argv through LoggedCmd.
func (u *UI) logged(ctx context.Context, step msg.M, argv ...string) error {
	return u.LoggedCmd(ctx, step, exec.CommandContext(ctx, argv[0], argv[1:]...)) //nolint:gosec // the tests' own commands
}

func writeFile(t *testing.T, path, s string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // the test's own files
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestLoggedGoesToTheLog(t *testing.T) {
	t.Parallel()
	u, out, errOut := testUI(t, "")
	noisy := []string{"sh", "-c", `echo "lots of output"; echo "the error" >&2; exit "$0"`}
	if err := u.logged(t.Context(), msg.M{}, append(noisy, "0")...); err != nil {
		t.Fatal(err)
	}
	if out.Len()+errOut.Len() != 0 {
		t.Errorf("printed %q, %q; want nothing", out, errOut)
	}
	log := readFile(t, u.logPath())
	for _, want := range []string{"+ sh -c ", "\nlots of output\n", "\nthe error\n"} {
		if !strings.Contains(log, want) {
			t.Errorf("log %q: no %q", log, want)
		}
	}
	if logs, _ := u.runLogs(); filepath.Dir(u.logPath()) != logs {
		t.Errorf("log in %s, want it in %s", u.logPath(), logs)
	}
	// One log for the run.
	if err := u.logged(t.Context(), msg.M{}, append(noisy, "1")...); err == nil {
		t.Error("a failing command: no error")
	}
	if logs, _ := filepath.Glob(filepath.Join(filepath.Dir(u.logPath()), "*.log")); len(logs) != 1 {
		t.Errorf("logs: %q, want one", logs)
	}

	// LogNote says where it is, once.
	u.LogNote()
	u.LogNote()
	if got, want := out.String(), "    log_details file="+u.logPath()+"\n"; got != want {
		t.Errorf("LogNote: %q, want %q", got, want)
	}

	// Verbose streams what the log gets to stderr as well, the log still
	// written, stdout left alone.
	out.Reset()
	errOut.Reset()
	before := readFile(t, u.logPath())
	u.SetFlags(Flags{Verbose: true})
	if err := u.logged(t.Context(), msg.M{}, append(noisy, "0")...); err != nil {
		t.Fatal(err)
	}
	added := strings.TrimPrefix(readFile(t, u.logPath()), before)
	if !strings.Contains(added, "lots of output\n") || errOut.String() != added {
		t.Errorf("verbose: stderr %q, the log got %q", errOut, added)
	}
	if out.Len() != 0 {
		t.Errorf("verbose: stdout %q", out)
	}
}

// The stream drops control characters, a C1 one split across writes
// too, and is dim only where stderr's style is on.
func TestStreamIsPrintable(t *testing.T) {
	t.Parallel()
	u, _, errOut := testUI(t, "")
	s := &stream{u: u}
	for _, p := range []string{"a\033]0;x\007b\xc2", "\x9bc\n", "é\tok\n"} {
		_, _ = s.Write([]byte(p))
	}
	if got := errOut.String(); got != "a]0;xbc\né\tok\n" {
		t.Errorf("stream: %q", got)
	}
	errOut.Reset()
	u.errStyle = style{on: true}
	_, _ = s.Write([]byte("x\n"))
	if got := errOut.String(); got != "\033["+sgrDim+"mx\033[0m\n" {
		t.Errorf("stream, colored: %q", got)
	}
}

// The run logs are in devvm's data folder.
func TestRunLogsFolder(t *testing.T) {
	data := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", data)
	if dir, err := runLogs(); err != nil || dir != filepath.Join(data, "devvm", "logs", "runs") {
		t.Errorf("runLogs = %q, %v", dir, err)
	}
}

func TestErrorLoggedShowsTheLogsEnd(t *testing.T) {
	t.Parallel()
	u, _, errOut := testUI(t, "")
	_ = u.logged(t.Context(), msg.M{}, "sh", "-c", `printf 'guest\033]0;title\007 \302\233line\n'; exit 1`)
	u.ErrorLogged(msg.MsgUnknown("x"))
	want := "--- log_end file=" + u.logPath() + "\n+ sh -c printf 'guest\\033]0;title\\007 \\302\\233line\\n'; exit 1\nguest]0;title line\n---\ndevvm: msg_unknown name=x\n"
	if got := errOut.String(); got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
	// Interrupted, nothing.
	errOut.Reset()
	u.interrupted.Store(true)
	if u.ErrorLogged(msg.MsgUnknown("x")); errOut.Len() != 0 {
		t.Errorf("interrupted: stderr %q, want nothing", errOut)
	}
	// Without a log, just the message.
	u, _, errOut = testUI(t, "")
	u.ErrorLogged(msg.MsgUnknown("x"))
	if got, want := errOut.String(), "devvm: msg_unknown name=x\n"; got != want {
		t.Errorf("no log: got %q, want %q", got, want)
	}
}

func TestProxyKeysStayOutOfTheLog(t *testing.T) {
	t.Parallel()
	u, _, _ := testUI(t, "")
	// A VM's proxy key is 32 hex digits, the build proxy's password 64.
	key := strings.Repeat("0123456789abcdef", 2)
	build := strings.Repeat("fedcba9876543210", 4)
	if err := u.logged(t.Context(), msg.M{}, "true", "--set", `.env.HTTP_PROXY="http://devvm:`+key+`@host.lima.internal:3128"`, "http://devvm:"+build+"@10.0.2.2:3129"); err != nil {
		t.Fatal(err)
	}
	log := readFile(t, u.logPath())
	if strings.Contains(log, key) || strings.Contains(log, build) || !strings.Contains(log, "http://devvm:<key>@host") || !strings.Contains(log, "http://devvm:<key>@10.0.2.2") {
		t.Errorf("log: %q", log)
	}
	// Owner-only.
	for path, want := range map[string]os.FileMode{
		filepath.Dir(filepath.Dir(u.logPath())): 0o700,
		filepath.Dir(u.logPath()):               0o700,
		u.logPath():                             0o600,
	} {
		if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != want {
			t.Errorf("%s: %v, want %v (%v)", path, fi.Mode().Perm(), want, err)
		}
	}
}

// A password in a URL that a command prints, such as the shell's proxy settings that Lima logs
// as it reads them, stays out of the log and its stream. The URL reaches the command through its
// environment, never its arguments.
func TestURLPasswordsStayOutOfTheLog(t *testing.T) {
	t.Parallel()
	u, _, errOut := testUI(t, "")
	u.SetFlags(Flags{Verbose: true})
	pass := "e2e-not-a-secret" //nolint:gosec // a fake password
	cmd := exec.CommandContext(t.Context(), "sh", "-c", `printf '%s\n' "level=info msg=\"Replacing \\\"HTTPS_PROXY\\\" value $V\""; printf '%s' "$W"`)
	cmd.Env = append(os.Environ(), "V=`http://alice:"+pass+"@127.0.0.1:3128`", "W=HTTP_PROXY=http://bob:"+pass+"@proxy.corp")
	if err := u.LoggedCmd(t.Context(), msg.M{}, cmd); err != nil {
		t.Fatal(err)
	}
	log := readFile(t, u.logPath())
	if strings.Contains(log, pass) || strings.Contains(errOut.String(), pass) ||
		!strings.Contains(log, "value `http://alice:<password>@127.0.0.1:3128`") || !strings.Contains(log, "HTTP_PROXY=http://bob:<password>@proxy.corp") {
		t.Errorf("log %q, stderr %q", log, errOut)
	}
}

// The redacting writer finds a password split across writes, and lines that it leaves alone come
// through whole.
func TestRedactingWriter(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	r := &redacting{w: &b}
	for _, p := range []string{"plain line\nhttps://u:sec", "ret@host/x and http://devvm:", strings.Repeat("ab", 16), "@h\rno user https://host:443/a@b\n", "tail http://v:p@w"} {
		if n, err := r.Write([]byte(p)); n != len(p) || err != nil {
			t.Fatalf("Write: %d, %v", n, err)
		}
	}
	r.flush()
	want := "plain line\nhttps://u:<password>@host/x and http://devvm:<key>@h\rno user https://host:443/a@b\ntail http://v:<password>@w"
	if got := b.String(); got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}

	// A line longer than redactLineMax is written as far as its last word, and the rest is held,
	// so a password that the limit splits is still found, with or without a scheme.
	for _, url := range []string{"http://alice:e2e-not-a-secret@proxy:3128", "alice:e2e-not-a-secret@proxy:3128"} {
		b.Reset()
		r = &redacting{w: &b}
		long := strings.Repeat("ab ", redactLineMax/3)
		i := strings.Index(url, "e2e-not") + len("e2e-not")
		_, _ = r.Write([]byte(long + url[:i]))
		if strings.Contains(b.String(), "e2e-not") {
			t.Errorf("the limit wrote %q", b.String()[len(long)-8:])
		}
		_, _ = r.Write([]byte(url[i:] + " more"))
		r.flush()
		if got := b.String(); strings.Contains(got, "e2e-not") || !strings.HasSuffix(got, ":<password>@proxy:3128 more") || len(got) < len(long) {
			t.Errorf("%s: got %q", url, got[max(0, len(got)-80):])
		}
	}

	// A long line without a space keeps only its last redactTailMax bytes.
	b.Reset()
	r = &redacting{w: &b}
	_, _ = r.Write([]byte(strings.Repeat("x", redactLineMax+1)))
	if b.Len() != redactLineMax+1-redactTailMax || len(r.buf) != redactTailMax {
		t.Errorf("wrote %d, held %d", b.Len(), len(r.buf))
	}
}

// What a guest command writes through LogWriter is redacted too, a password split across writes
// and calls included: a line's end still to come is written, redacted, at a logged command's
// start, at ErrorLogged and at the run's end, and verbose streams it the same.
func TestLogWriterRedacts(t *testing.T) {
	t.Parallel()
	pass := "e2e-not-a-secret" //nolint:gosec // a fake password
	u, _, errOut := testUI(t, "")
	u.SetFlags(Flags{Verbose: true})
	_, _ = io.WriteString(u.LogWriter(), "HTTPS_PROXY=http://alice:e2e-not")
	_, _ = io.WriteString(u.LogWriter(), "-a-secret@127.0.0.1:3128\nno end http://bob:e2e-not")
	// A logged command's start writes the line as far as its last word, and holds the rest.
	if err := u.logged(t.Context(), msg.M{}, "true"); err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(u.LogWriter(), "-a-secret@h\n")
	want := "HTTPS_PROXY=http://alice:<password>@127.0.0.1:3128\nno end + true\nhttp://bob:<password>@h\n"
	if got := readFile(t, u.logPath()); got != want || errOut.String() != want {
		t.Errorf("log %q, stderr %q\nwant %q", got, errOut, want)
	}

	// ErrorLogged shows the line LogWriter holds, redacted.
	u, _, errOut = testUI(t, "")
	_, _ = io.WriteString(u.LogWriter(), "failed: https://carol:"+pass+"@proxy")
	u.ErrorLogged(msg.MsgUnknown("x"))
	if log := readFile(t, u.logPath()); log != "failed: https://carol:<password>@proxy" || strings.Contains(errOut.String(), pass) ||
		!strings.Contains(errOut.String(), "carol:<password>@proxy") {
		t.Errorf("ErrorLogged: log %q, stderr %q", log, errOut)
	}

	// The run's end writes it too.
	u, _, _ = testUI(t, "")
	u.main(func(context.Context) int {
		_, _ = io.WriteString(u.LogWriter(), "last http://dave:"+pass+"@p")
		return 0
	})
	if log := readFile(t, u.logPath()); log != "last http://dave:<password>@p" {
		t.Errorf("the run's end: log %q", log)
	}
}

func TestStepCounts(t *testing.T) {
	t.Parallel()
	u, _, _ := testUI(t, "")
	ctx := u.WithSteps(t.Context(), 2)
	for _, s := range []string{"a", "b", "c"} {
		if err := u.logged(ctx, msg.MsgUnknown(s), "true"); err != nil {
			t.Fatal(err)
		}
	}
	// A phase inside another keeps its own count; an unlabelled step and one
	// outside a phase aren't counted.
	if err := u.logged(u.WithSteps(ctx, 1), msg.MsgUnknown("inner"), "true"); err != nil {
		t.Fatal(err)
	}
	if err := u.logged(ctx, msg.M{}, "true"); err != nil {
		t.Fatal(err)
	}
	if err := u.logged(t.Context(), msg.MsgUnknown("outside"), "true"); err != nil {
		t.Fatal(err)
	}
	want := "# [1/2] msg_unknown name=a\n+ true\n# [2/2] msg_unknown name=b\n+ true\n# [3/2] msg_unknown name=c\n+ true\n" +
		"# [1/1] msg_unknown name=inner\n+ true\n+ true\n+ true\n"
	if got := readFile(t, u.logPath()); got != want {
		t.Errorf("log:\n%s\nwant:\n%s", got, want)
	}

	// The status line's label: numbered while in the count.
	ctx = u.WithSteps(context.Background(), 1)
	for _, want := range []string{"[1/1] x", "x"} {
		if got, _ := count(ctx, "x"); got != want {
			t.Errorf("label %q, want %q", got, want)
		}
	}
}

func TestKeepsTheLast20Logs(t *testing.T) {
	t.Parallel()
	u, _, _ := testUI(t, "")
	runs, _ := u.runLogs()
	logs := filepath.Dir(runs)
	if err := os.MkdirAll(runs, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	for i := range 25 {
		for _, f := range []string{
			filepath.Join(runs, fmt.Sprintf("old-%d.log", i)),
			filepath.Join(logs, fmt.Sprintf("devvm-p%d-abc123.proxy.log", i)),
		} {
			writeFile(t, f, "")
			mod := old.Add(time.Duration(i) * time.Minute)
			if err := os.Chtimes(f, mod, mod); err != nil {
				t.Fatal(err)
			}
		}
	}
	u.LogNote()
	kept, _ := filepath.Glob(filepath.Join(runs, "*.log"))
	proxy, _ := filepath.Glob(filepath.Join(logs, "*.proxy.log"))
	if len(kept) != 20 || len(proxy) != 25 {
		t.Errorf("%d run logs and %d proxy logs, want 20 and 25", len(kept), len(proxy))
	}
	for _, f := range kept {
		if f == filepath.Join(runs, "old-5.log") {
			t.Errorf("kept an older log: %s", f)
		}
	}
	if _, err := os.Stat(u.logPath()); err != nil {
		t.Errorf("this run's log: %v", err)
	}
}

// Past runLogsMax together, the oldest run logs go however many are
// kept, but never the newest, nor one whose run still runs, read from its
// name.
func TestRunLogsCapped(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	old := time.Now().Add(-time.Hour)
	// Each a third of runLogsMax long, all of it a hole: no disk taken.
	big := func(path string, n int64) {
		t.Helper()
		writeFile(t, path, "")
		if err := os.Truncate(path, n); err != nil {
			t.Fatal(err)
		}
	}
	live := fmt.Sprintf("20260101-000000-%d.log", os.Getpid()) // this test's: it runs
	names := []string{live, "20260101-000001-999999999.log", "20260101-000002-999999998.log", "20260101-000003-999999997.log", "20260101-000004-999999996.log"}
	for i, name := range names {
		f := filepath.Join(dir, name)
		big(f, runLogsMax/3)
		mod := old.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(f, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	pruneLogs(dir, 20)
	kept, _ := filepath.Glob(filepath.Join(dir, "*.log"))
	for i, f := range kept {
		kept[i] = filepath.Base(f)
	}
	// The newest three take runLogsMax; the two older go, but the live one.
	if want := []string{live, names[2], names[3], names[4]}; !slices.Equal(kept, want) {
		t.Errorf("kept %q, want %q", kept, want)
	}
	// The newest stays even past runLogsMax alone.
	single := t.TempDir()
	big(filepath.Join(single, names[1]), 2*runLogsMax)
	if pruneLogs(single, 20); len(mustGlob(t, single)) != 1 {
		t.Error("the newest log went")
	}
	for name, want := range map[string]int{names[1]: 999999999, "old-5.log": 0, "a-b-c.log": 0, "20260101-000000-x.log": 0} {
		if pid, ok := logPID(filepath.Join(dir, name)); pid != want || ok != (want > 0) {
			t.Errorf("logPID(%s) = %d, %v", name, pid, ok)
		}
	}
}

// One run writes at most runLogMax to its log: the write that would pass
// it, and every one after, is dropped, and one line says so.
func TestLogCap(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	c := &logCap{f: &b, n: runLogMax - 4}
	for _, s := range []string{"abc\n", "past\n", "more\n"} {
		if n, err := c.Write([]byte(s)); n != len(s) || err != nil {
			t.Errorf("Write(%q) = %d, %v", s, n, err)
		}
	}
	if b.String() != "abc\n"+runLogFull {
		t.Errorf("log %q", b.String())
	}
}

func mustGlob(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, "*.log"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestRunLogFailures(t *testing.T) {
	t.Parallel()
	u, _, errOut := testUI(t, "")
	u.runLogs = func() (string, error) { return "", msg.HomeNotAbsolute() }
	if code := exitCode(u.LogNote); code != 1 || errOut.String() != "devvm: home_not_absolute\n" {
		t.Errorf("exit code %d, stderr %q", code, errOut)
	}
	u, _, errOut = testUI(t, "")
	data := t.TempDir()
	writeFile(t, filepath.Join(data, "devvm"), "a file, not a folder")
	runs := filepath.Join(data, "devvm", "logs", "runs")
	u.runLogs = func() (string, error) { return runs, nil }
	if code := exitCode(u.LogNote); code != 1 || !strings.HasPrefix(errOut.String(), "devvm: run_log_failed dir="+runs+" reason=") {
		t.Errorf("exit code %d, stderr %q", code, errOut)
	}
}
