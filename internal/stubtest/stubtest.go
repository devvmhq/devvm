// Package stubtest is for tests: it puts the stubs (internal/stubtest/stub,
// one binary standing in for each host command devvm runs) first on
// PATH, in place of the host's, with a new folder of their own for one
// test, and reads and writes what they keep there. The stubs find that
// folder in STUB_DIR and log each call they get to STUB_LOG, its calls
// file, one line each, written whole so that stubs running at once never
// mix their lines.
package stubtest

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/stubtest/stubstate"
)

// Stubs is one test's stubs: Dir is their folder, STUB_DIR.
type Stubs struct {
	Dir string
}

// New puts every stub first on PATH for t, or only the ones named, with
// a new folder under /tmp (short, for the sockets some stubs make in it)
// that t's end removes. The stubs that run git, on their stand-in VM's
// files, run it without git's variables that pick a repo (NoGitRepo).
func New(t testing.TB, only ...string) *Stubs {
	t.Helper()
	NoGitRepo(t)
	all := Bin(t)
	dir, err := os.MkdirTemp("/tmp", "stub.")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if dir, err = filepath.EvalSymlinks(dir); err != nil {
		t.Fatal(err)
	}
	bin := all
	if len(only) > 0 {
		bin = filepath.Join(dir, "bin")
		if err := os.Mkdir(bin, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, name := range only {
			if err := os.Symlink(Command(t, name), filepath.Join(bin, name)); err != nil {
				t.Fatal(err)
			}
		}
	}
	s := &Stubs{Dir: dir}
	t.Setenv("STUB_DIR", dir)
	t.Setenv("STUB_LOG", s.Log())
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return s
}

// Bin returns the folder of the stubs: the stub binary, built once (see
// built), and a link to it named for each command it stands in for.
func Bin(t testing.TB) string {
	t.Helper()
	dir, err := built()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// Command returns the path of the stub for the command name.
func Command(t testing.TB, name string) string {
	t.Helper()
	p := filepath.Join(Bin(t), name)
	if _, err := os.Lstat(p); err != nil {
		t.Fatalf("no stub %s: %v", name, err)
	}
	return p
}

// Path returns the path of the file name in the stubs' folder.
func (s *Stubs) Path(name string) string { return filepath.Join(s.Dir, name) }

// Log returns the stubs' call log, STUB_LOG.
func (s *Stubs) Log() string { return s.Path("calls") }

// Calls returns the calls the stubs logged, one per line, in order: nil
// for none.
func (s *Stubs) Calls(t testing.TB) []string {
	t.Helper()
	text := strings.TrimSpace(s.Read(t, "calls"))
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

// Reset forgets the calls logged so far.
func (s *Stubs) Reset(t testing.TB) {
	t.Helper()
	if err := os.Remove(s.Log()); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

// Touch makes the empty file name in the stubs' folder, the switch a stub
// looks for.
func (s *Stubs) Touch(t testing.TB, name string) {
	t.Helper()
	s.Write(t, name, "")
}

// Write writes content to the file name in the stubs' folder, making the
// folders it's in.
func (s *Stubs) Write(t testing.TB, name, content string) {
	t.Helper()
	p := s.Path(name)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Read returns the file name in the stubs' folder: "" when there's none.
func (s *Stubs) Read(t testing.TB, name string) string {
	t.Helper()
	b, err := os.ReadFile(s.Path(name))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return string(b)
}

// update changes the stubs' state.
func (s *Stubs) update(t testing.TB, fn func(*stubstate.State)) {
	t.Helper()
	if err := stubstate.Update(s.Dir, func(st *stubstate.State) error { fn(st); return nil }); err != nil {
		t.Fatal(err)
	}
}

// state reads the stubs' state.
func (s *Stubs) state(t testing.TB) *stubstate.State {
	t.Helper()
	st, err := stubstate.Read(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// VM has Lima list the instance name in status, with Lima's default size
// unless it has one.
func (s *Stubs) VM(t testing.TB, name, status string) {
	t.Helper()
	s.update(t, func(st *stubstate.State) {
		if in := st.Instances[name]; in != nil {
			in.Status = status
			return
		}
		st.Instances[name] = stubstate.NewInstance(status)
	})
}

// PIDs gives the instance name the pids Lima lists for its hostagent and
// its driver (0: none), as a broken one has.
func (s *Stubs) PIDs(t testing.TB, name string, hostAgent, driver int) {
	t.Helper()
	s.update(t, func(st *stubstate.State) {
		if in := st.Instances[name]; in != nil {
			in.HostAgentPID, in.DriverPID = hostAgent, driver
		}
	})
}

// Size gives the instance name its size: memory and disk in bytes.
func (s *Stubs) Size(t testing.TB, name string, cpus int, memory, disk int64) {
	t.Helper()
	s.update(t, func(st *stubstate.State) {
		in := st.Instances[name]
		if in == nil {
			t.Fatalf("no instance %s", name)
		}
		in.CPUs, in.Memory, in.Disk = cpus, memory, disk
	})
}

// VMs returns Lima's instances, "NAME STATUS" each, sorted.
func (s *Stubs) VMs(t testing.TB) []string {
	t.Helper()
	var out []string
	for name, in := range s.state(t).Instances {
		if in.Status != "" {
			out = append(out, name+" "+in.Status)
		}
	}
	slices.Sort(out)
	return out
}

// SetKeychain saves value as the Keychain's item for account.
func (s *Stubs) SetKeychain(t testing.TB, account, value string) {
	t.Helper()
	s.update(t, func(st *stubstate.State) { st.Keychain[account] = value })
}

// Keychain returns the Keychain's item for account, and whether there's
// one.
func (s *Stubs) Keychain(t testing.TB, account string) (string, bool) {
	t.Helper()
	v, ok := s.state(t).Keychain[account]
	return v, ok
}

// ExecRunner runs commands as they are, here the stubs: it's a backend's
// runner (Run, Output, Attach) with no run log or status line.
type ExecRunner struct{}

// Run runs cmd; step goes nowhere.
func (ExecRunner) Run(_ context.Context, _ msg.M, cmd *exec.Cmd) error { return cmd.Run() }

// Output runs cmd and returns its standard output.
func (ExecRunner) Output(cmd *exec.Cmd) ([]byte, error) { return cmd.Output() }

// Attach runs cmd on the streams already set on it.
func (ExecRunner) Attach(cmd *exec.Cmd) error { return cmd.Run() }
