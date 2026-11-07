//go:build darwin || linux

package platform

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A file's change ID moves with any change, its times set back
// included, and a FileInfo without a stat has none.
func TestChangeID(t *testing.T) {
	f := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(f, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	stat := func() string {
		fi, err := os.Lstat(f)
		if err != nil {
			t.Fatal(err)
		}
		return ChangeID(fi)
	}
	was := stat()
	if !strings.HasPrefix(was, "ino ") || !strings.Contains(was, " ctime ") {
		t.Fatalf("ChangeID %q", was)
	}
	time.Sleep(20 * time.Millisecond)
	if err := os.WriteFile(f, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(f, time.Unix(1, 0), time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	if now := stat(); now == was {
		t.Errorf("unchanged: %q", now)
	}
	if id := ChangeID(fakeInfo{}); id != "" {
		t.Errorf("no stat: %q", id)
	}
}

// fakeInfo is a FileInfo without a stat.
type fakeInfo struct{ os.FileInfo }

func (fakeInfo) Sys() any { return nil }
