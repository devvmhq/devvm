package platform

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"golang.org/x/sys/unix"
)

// A folder read with no link on its path: its FolderID, and a volume, the same at each read,
// only on a file system whose f_fsid is its own (ext4, btrfs, ZFS), never on tmpfs or XFS; a
// folder whose path has a link isn't read.
func TestStatFolder(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s, err := StatFolder(dir)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := FolderID(dir)
	var fs unix.Statfs_t
	if err := unix.Statfs(dir, &fs); err != nil {
		t.Fatal(err)
	}
	own := slices.Contains(fsidOwn(), fs.Type) && fs.Flags&unix.ST_RDONLY == 0
	if s.ID != id || (s.Volume != "") != own || int(s.Owner) != os.Getuid() {
		t.Errorf("StatFolder = %+v on a file system of type %#x, FolderID %s", s, fs.Type, id)
	}
	if again, _ := StatFolder(dir); again != s {
		t.Errorf("again = %+v, want %+v", again, s)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if _, err := StatFolder(link); err == nil {
		t.Error("a folder read through a link")
	}
}
