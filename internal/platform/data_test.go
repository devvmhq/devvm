package platform

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// DataStart: 0 for a file without a hole; past a hole at a file's start,
// at most where its data starts (a file system that can't tell says 0);
// a file that's all hole, its length or 0.
func TestDataStart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	open := func(name string, write func(f *os.File) error) *os.File {
		t.Helper()
		f, err := os.OpenFile(filepath.Join(dir, name), os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // the test's own file
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		if err := write(f); err != nil {
			t.Fatal(err)
		}
		return f
	}
	plain := open("plain.log", func(f *os.File) error { _, err := f.WriteString("boot\n"); return err })
	if n := DataStart(plain); n != 0 {
		t.Errorf("a file without a hole: %d", n)
	}
	const at = 4 << 20
	holed := open("holed.log", func(f *os.File) error { _, err := f.WriteAt([]byte("later\n"), at); return err })
	// macOS's and Linux's file systems that devvm's folders are on have
	// holes, and say where data starts past one.
	if n := DataStart(holed); n <= 0 && (runtime.GOOS == "darwin" || runtime.GOOS == "linux") || n < 0 || n > at {
		t.Errorf("past a hole: %d", n)
	}
	empty := open("empty.log", func(f *os.File) error { return f.Truncate(at) })
	if n := DataStart(empty); n != 0 && n != at {
		t.Errorf("all hole: %d", n)
	}
}
