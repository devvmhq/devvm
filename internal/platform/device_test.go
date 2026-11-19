//go:build unix

package platform

import (
	"os"
	"testing"
)

// Writable is the system's answer: a folder of your own is, / isn't
// (unless you're root), and neither is a folder of your own that you took
// your write away from.
func TestWritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes every folder")
	}
	dir := t.TempDir()
	if !Writable(dir) {
		t.Errorf("your own folder: not writable")
	}
	if Writable("/") {
		t.Errorf("/: writable")
	}
	if err := os.Chmod(dir, 0o500); err != nil { //nolint:gosec // G302: a folder, read-only to its owner
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // G302: a folder, writable again for its cleanup
	if Writable(dir) {
		t.Errorf("a read-only folder of your own: writable")
	}
}
