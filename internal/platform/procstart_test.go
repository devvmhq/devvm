//go:build darwin || linux

package platform

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// A process's start is one word of digits and dots, the same at each read while it runs, a
// child's is another than the init process's, which started at boot, and a pid no process has
// gets none.
func TestProcessStart(t *testing.T) {
	self, err := ProcessStart(os.Getpid())
	if err != nil || self == "" || strings.IndexFunc(self, func(r rune) bool { return (r < '0' || r > '9') && r != '.' }) >= 0 {
		t.Fatalf("ProcessStart(self) = %q, %v", self, err)
	}
	if again, err := ProcessStart(os.Getpid()); err != nil || again != self {
		t.Errorf("again = %q, %v, want %q", again, err, self)
	}
	cmd := exec.CommandContext(t.Context(), "sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	other, err := ProcessStart(cmd.Process.Pid)
	if init, ierr := ProcessStart(1); err != nil || ierr != nil || other == init {
		t.Errorf("a child's start = %q, %v; init's %q, %v", other, err, init, ierr)
	}
	if got, err := ProcessStart(1 << 30); err == nil {
		t.Errorf("no process: %q", got)
	}
}
