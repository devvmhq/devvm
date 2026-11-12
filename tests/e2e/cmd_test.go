//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"

	"github.com/devvmhq/devvm/internal/backend"
)

// Cmd is a host command a section runs, in the sandbox's environment.
type Cmd struct {
	Args  []string // the command and its arguments
	Dir   string   // its folder: the sandbox's state folder when empty
	Stdin string   // its input: none (/dev/null) when empty
	Env   []string // VAR=VALUE, on top of the sandbox's
	// Input is its input as it reads it, in place of Stdin: for input
	// that waits for what the command prints (ttyRun's answers).
	Input io.Reader
}

// Res is what a command printed: Out its output and errors as they came,
// Stdout its output alone.
type Res struct{ Out, Stdout string }

// Has is whether line is one of Out's lines (grep -qx).
func (r Res) Has(line string) bool { return HasLine(r.Out, line) }

// Run runs c in the window open now, its output in the check's log too
// (the heartbeat's and a failure's lines). It runs in a process group of
// its own, which gets TERM when the window runs out, then KILL 30 s later.
// Its error is the command and its exit status, and for git, the end of
// what it wrote on stderr (gitSaid).
func (s *S) Run(c Cmd) (Res, error) {
	w := s.r.window()
	cmd := exec.CommandContext(w.ctx, c.Args[0], c.Args[1:]...)
	cmd.Env = append(slices.Clone(s.sb.env), c.Env...)
	cmd.Dir = c.Dir
	if cmd.Dir == "" {
		cmd.Dir = s.sb.work
	}
	if c.Stdin != "" {
		cmd.Stdin = strings.NewReader(c.Stdin)
	}
	if c.Input != nil {
		cmd.Stdin = c.Input
	}
	log, err := os.OpenFile(s.sb.logFile(), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return Res{}, err
	}
	defer log.Close()
	all, out, errOut := &lockedBuffer{}, &bytes.Buffer{}, &bytes.Buffer{}
	cmd.Stdout, cmd.Stderr = io.MultiWriter(all, out, log), io.MultiWriter(all, errOut, log)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = killAfter
	err = cmd.Run()
	if w.ctx.Err() != nil && cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // whatever is left of it
	}
	res := Res{Out: all.String(), Stdout: out.String()}
	if err != nil {
		err = fmt.Errorf("%s: %w%s", strings.Join(c.Args, " "), err, gitSaid(c.Args[0], errOut.String()))
	}
	return res, err
}

// gitSaidMax is the most of a failed git's errors a failure line holds.
const gitSaidMax = 500

// gitSaid is what a failed git command (prog) wrote on stderr, its end,
// at most gitSaidMax bytes on one line, for its error: "" for another
// command, or none.
func gitSaid(prog, stderr string) string {
	if filepath.Base(prog) != "git" {
		return ""
	}
	s := strings.Join(strings.Fields(stderr), " ")
	if s == "" {
		return ""
	}
	if len(s) > gitSaidMax {
		s = "…" + strings.ToValidUTF8(s[len(s)-gitSaidMax:], "")
	}
	return " (git said: " + s + ")"
}

// lockedBuffer is a buffer two writers share.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// Devvm runs the run's devvm in dir with no input.
func (s *S) Devvm(dir string, args ...string) (Res, error) {
	return s.Run(Cmd{Args: append([]string{s.sb.devvm}, args...), Dir: dir})
}

// DevvmNames runs devvm in dir in names mode, each message printed as its
// name and values (no_vm dir=/x), which checks compare with.
func (s *S) DevvmNames(dir string, args ...string) (Res, error) {
	return s.Run(Cmd{Args: append([]string{s.sb.devvm}, args...), Dir: dir, Env: []string{"DEVVM_MESSAGES=names"}})
}

// Msg is a message's text, from devvm _msg NAME k=v…, so checks never
// repeat its wording.
func (s *S) Msg(name string, kv ...string) string {
	r, err := s.Run(Cmd{Args: append([]string{s.sb.devvm, "_msg", name}, kv...)})
	if err != nil {
		return "\x00no message " + name // matches nothing
	}
	return strings.TrimRight(r.Stdout, "\n")
}

// Limactl runs limactl in the sandbox's Lima home.
func (s *S) Limactl(args ...string) (Res, error) {
	return s.Run(Cmd{Args: append([]string{"limactl"}, args...)})
}

// InVM runs script in the VM's login shell, from /, with no input
// (limactl shell reads it otherwise).
func (s *S) InVM(vm, script string) (Res, error) {
	return s.Limactl("shell", "--workdir", "/", vm, "bash", "-lc", script)
}

// Status is the VM's status as Lima lists it (Running, Stopped), nothing
// when there's no such VM.
func (s *S) Status(vm string) string {
	r, err := s.Limactl("list", "--format", "{{.Status}}", vm)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(r.Stdout)
}

// lacks is why a check can't run on the run's backend: the first of fs it
// has neither natively nor emulated and reviewed, as devvm doctor --json
// shows its features, which doctor runs once a run.
func (r *runner) lacks(fs []backend.Feature, doctor func() (Res, error)) (string, bool) {
	if len(fs) == 0 {
		return "", false
	}
	if r.features == nil {
		r.features = map[string]string{}
		res, err := doctor()
		var d struct {
			Backend  string                       `json:"backend"`
			Features map[string]map[string]string `json:"features"`
		}
		if err == nil && json.Unmarshal([]byte(res.Stdout), &d) == nil && d.Features[d.Backend] != nil {
			r.features = d.Features[d.Backend]
		}
		r.features[""] = d.Backend
	}
	for _, f := range fs {
		switch r.features[string(f)] {
		case "native", "emulated":
		default:
			return fmt.Sprintf("the %s backend has no %s", r.features[""], f), true
		}
	}
	return "", false
}

// Matches is whether one of text's lines matches the regular expression
// re (grep -qE).
func Matches(text, re string) bool { return regexp.MustCompile("(?m)" + re).MatchString(text) }

// IsHex is whether s is lowercase hex, as an image's key is.
func IsHex(s string) bool { return s != "" && strings.Trim(s, "0123456789abcdef") == "" }

// Exists is whether path is there (a file, a folder, a link).
func Exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// AppendFile adds text to the end of the file f.
func AppendFile(f, text string) error {
	fh, err := os.OpenFile(f, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	_, err = fh.WriteString(text)
	return errors.Join(err, fh.Close())
}

// errNo is a check's failure for a value a step before it didn't get.
var errNo = errors.New("a step before this check failed: a value it needs is empty")
