//go:build darwin || linux

package platform

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/stubtest"
)

// A folder made again at the same path is another folder.
func TestFolderID(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "p")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	a, err := FolderID(dir)
	if err != nil || a == "" {
		t.Fatalf("FolderID = %q, %v", a, err)
	}
	if again, _ := FolderID(dir); again != a {
		t.Errorf("FolderID changed: %q, then %q", a, again)
	}
	// Another folder kept alive, so its inode can't be the one reused.
	if err := os.Rename(dir, dir+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if b, _ := FolderID(dir); b == a {
		t.Errorf("a new folder at the same path has the old one's ID %q", a)
	}
	if _, err := FolderID(filepath.Join(dir, "missing")); err == nil {
		t.Error("FolderID of a missing folder: no error")
	}
	// A folder moved keeps its ID.
	if moved, _ := FolderID(dir + ".old"); moved != a {
		t.Errorf("a moved folder's ID: %q, was %q", moved, a)
	}
}

// StatFolder's Unique: an ID with a birth time of a folder on APFS or
// HFS+ on macOS, or on one of uniqueFS's file systems on Linux; and a
// missing folder has no stat at all.
func TestStatFolderUnique(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s, err := StatFolder(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := StatFolder(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing folder's stat")
	}
	// A link on the folder's path is refused, the error about the folder, as an open's is, so a
	// message that names the folder takes its reason alone.
	if err := os.Symlink(dir, filepath.Join(dir, "via")); err != nil {
		t.Fatal(err)
	}
	via := filepath.Join(dir, "via", "x")
	if err := os.Mkdir(filepath.Join(dir, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := StatFolder(via); err == nil {
		t.Error("a folder through a link: stat")
	} else if pe, ok := errors.AsType[*fs.PathError](err); !ok || pe.Path != via {
		t.Errorf("a folder through a link: %v, not a PathError about it", err)
	}
	switch runtime.GOOS {
	case "linux":
		// The temporary folder's file system may be any (TestUniqueFS); unique needs a birth time.
		if s.Unique && strings.Count(s.ID, ":") != 2 {
			t.Errorf("unique without a birth time: %+v", s)
		}
		t.Logf("%s: %s, unique %v", dir, s.ID, s.Unique)
	case "darwin":
		// The tests' temporary folder is on the system's APFS volume.
		if !s.Unique {
			t.Errorf("StatFolder(%s) = %+v, not unique", dir, s)
		}
	}
}

// SameFolder: the ID recorded, or at the same physical path the same
// inode and birth time on a device numbered anew, as after macOS's boot,
// of a folder whose ID is its alone; never another inode, birth time or
// path, an ID without a birth time, a link, or a missing folder.
func TestSameFolder(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "p")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	id, err := FolderID(dir)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	dev, rest, _ := strings.Cut(id, ":")
	ino, birth, _ := strings.Cut(rest, ":")
	renumbered := stubtest.Renumbered(id)
	s, err := StatFolder(dir)
	if err != nil {
		t.Fatal(err)
	}
	unique := s.Unique
	gone := filepath.Join(root, "gone")
	for _, tc := range []struct {
		name                 string
		wasDir, was, at, now string
		want                 bool
	}{
		{"as recorded", dir, id, dir, id, true},
		{"moved, as recorded", filepath.Join(root, "q"), id, dir, id, true},
		{"its device numbered anew", dir, renumbered, dir, id, unique},
		{"its device numbered anew, at another path", filepath.Join(root, "q"), renumbered, dir, id, false},
		{"another inode", dir, stubtest.Renumbered(dev + ":" + ino + "1:" + birth), dir, id, false},
		{"another birth time", dir, stubtest.Renumbered(dev + ":" + ino + ":" + birth + "1"), dir, id, false},
		{"no birth time", dir, stubtest.Renumbered(dev + ":" + ino), dir, dev + ":" + ino, false},
		{"through a link", link, renumbered, link, id, false},
		// What a link swapped in for the moment the caller read the ID
		// gave: a copy of the folder, on another device.
		{"an ID that isn't the folder's at its path", dir, id, dir, renumbered, false},
		{"an ID that isn't the folder's, nor the record's", dir, stubtest.Renumbered(renumbered), dir, renumbered, false},
		{"gone", gone, renumbered, gone, id, false},
		{"nothing recorded", dir, "", dir, id, false},
		{"neither read", dir, "", dir, "", false},
	} {
		if got := SameFolder(tc.wasDir, tc.was, tc.at, tc.now); got != tc.want {
			t.Errorf("%s: SameFolder(%s, %q, %s, %q) = %v, want %v", tc.name, tc.wasDir, tc.was, tc.at, tc.now, got, tc.want)
		}
		// Renumbered: the same folder, but not as recorded.
		if got, want := Renumbered(tc.wasDir, tc.was, tc.at, tc.now), tc.want && tc.was != tc.now; got != want {
			t.Errorf("%s: Renumbered = %v, want %v", tc.name, got, want)
		}
		// The open folder's read, where the caller read the folder's own ID: the same answer,
		// without opening it again, and RecordedAs only for the ID as recorded.
		if tc.now != id || tc.at != dir {
			continue
		}
		s, err := StatFolder(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got := SameOpenFolder(tc.wasDir, tc.was, tc.at, s); got != tc.want {
			t.Errorf("%s: SameOpenFolder = %v, want %v", tc.name, got, tc.want)
		}
		if got, want := RecordedAs(tc.was, s), tc.name == "as recorded" || tc.name == "moved, as recorded"; got != want {
			t.Errorf("%s: RecordedAs = %v, want %v", tc.name, got, want)
		}
	}
	// A folder whose ID may be a deleted folder's never matches on another device.
	if SameOpenFolder(dir, renumbered, dir, FolderStat{ID: id}) {
		t.Error("SameOpenFolder took an ID that isn't the folder's alone")
	}
	if runtime.GOOS == "darwin" && !unique {
		t.Errorf("a folder on APFS: its ID %q isn't its alone", id)
	}
}

// VolPath names a FolderID's folder by its device and inode, and on macOS
// opens that folder once it's moved, never what replaced it at its path.
func TestVolPath(t *testing.T) {
	for id, want := range map[string]string{
		"": "", "12": "", "12:": "", ":34": "", "-1:34": "", "12:x": "", "1 2:34": "",
		"12:34": "/.vol/12/34", "12:34:5.6": "/.vol/12/34",
	} {
		if got := VolPath(id); got != want {
			t.Errorf("VolPath(%q) = %q, want %q", id, got, want)
		}
	}
	if runtime.GOOS != "darwin" {
		return
	}
	dir := filepath.Join(t.TempDir(), "p")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "own"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := FolderID(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir, dir+".real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(VolPath(id), "own")); err != nil {
		t.Errorf("%s, once its folder moved: %v", VolPath(id), err)
	}
}

// DeviceOf gives the device FolderID records, which folderDevice reads.
func TestDeviceOf(t *testing.T) {
	dir := t.TempDir()
	id, err := FolderID(dir)
	if err != nil {
		t.Fatal(err)
	}
	dev, err := DeviceOf(dir)
	if err != nil || dev == "" || dev != folderDevice(id) {
		t.Errorf("DeviceOf = %q, %v; FolderID = %q", dev, err, id)
	}
	if _, err := DeviceOf(filepath.Join(dir, "missing")); err == nil {
		t.Error("DeviceOf of a missing path: no error")
	}
	for id, want := range map[string]string{"": "", "12": "", "12:34": "12", "12:34:5.6": "12"} {
		if got := folderDevice(id); got != want {
			t.Errorf("folderDevice(%q) = %q, want %q", id, got, want)
		}
	}
}
