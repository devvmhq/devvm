//go:build unix

package vm

import (
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"

	"github.com/devvmhq/devvm/internal/msg"
)

// A FIFO in the place of Cursor's user settings never hangs devvm cursor: it warns, as for a file
// it can't read. On Linux the file is in the XDG config folder, wherever $XDG_CONFIG_HOME puts it.
func TestCursorAgentWarningFIFO(t *testing.T) {
	t.Parallel()
	home, config := t.TempDir(), t.TempDir()
	file := cursorUserSettings(home, config)
	want := filepath.Join(config, "Cursor", "User", "settings.json")
	if runtime.GOOS == "darwin" {
		want = filepath.Join(home, "Library", "Application Support", "Cursor", "User", "settings.json")
	}
	if file != want {
		t.Fatalf("cursorUserSettings = %s, want %s", file, want)
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(file, 0o600); err != nil {
		t.Fatal(err)
	}
	m, warn := cursorAgentWarning(home, config)
	if !warn || msg.NamesOf(m) != "cursor_agent_in_vm file="+file {
		t.Errorf("a FIFO: warn %v, %s", warn, msg.NamesOf(m))
	}
}
