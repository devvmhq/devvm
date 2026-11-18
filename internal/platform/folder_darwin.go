package platform

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/sys/unix"
)

// FolderID returns what identifies the folder at path itself, not its
// path: its device, inode and birth time. APFS never gives a new folder a
// deleted one's inode number, but another volume mounted later may get
// the device number and have a folder with the same inode number, which
// the birth time tells apart.
func FolderID(path string) (string, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return "", err
	}
	return statID(&st), nil
}

// statID is a stat's FolderID.
func statID(st *unix.Stat_t) string {
	return fmt.Sprintf("%d:%d:%d.%09d", st.Dev, st.Ino, st.Btim.Sec, st.Btim.Nsec)
}

// openFolderID is the FolderID of the open folder f, and whether it's
// its alone (UniqueFolderID), both read from f itself, so nothing put at
// its path since it was opened counts.
func openFolderID(f *os.File) (string, bool, error) {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return "", false, err
	}
	var fs unix.Statfs_t
	if err := unix.Fstatfs(int(f.Fd()), &fs); err != nil {
		return "", false, err
	}
	name := unix.ByteSliceToString(fs.Fstypename[:])
	return statID(&st), name == "apfs" || name == "hfs", nil
}

// UniqueFolderID reports whether id, the FolderID of the folder at path,
// is that folder's alone, never a deleted one's or another volume's: it
// has the folder's birth time, and the folder is on APFS or HFS+, which
// never give a new folder a deleted one's inode number. Other file
// systems (exFAT, network shares) may make inode numbers up or use them
// again.
func UniqueFolderID(path, id string) bool {
	var st unix.Statfs_t
	if strings.Count(id, ":") != 2 || unix.Statfs(path, &st) != nil {
		return false
	}
	fs := unix.ByteSliceToString(st.Fstypename[:])
	return fs == "apfs" || fs == "hfs"
}

// HasImmutableFlag reports whether this system has a user-immutable flag
// (Chflags, Immutable): macOS's uchg, which the user sets without root.
const HasImmutableFlag = true

// FoldsCase reports whether file names here may match whatever their
// case: macOS's APFS and HFS+ do by default, so .GIT is .git to git.
const FoldsCase = true

// Chflags sets file flags (uchg, nouchg) on path and everything under it,
// following no symlink, path itself included: chflags -R -P.
func Chflags(ctx context.Context, flags, path string) error {
	return exec.CommandContext(ctx, "chflags", "-R", "-P", flags, path).Run() //nolint:gosec // devvm's own arguments
}

// Immutable reports whether path itself (not what a symlink points to) has
// the user-immutable flag (uchg).
func Immutable(path string) bool {
	var st unix.Stat_t
	return unix.Lstat(path, &st) == nil && st.Flags&unix.UF_IMMUTABLE != 0
}
