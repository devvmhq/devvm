package platform

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

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

// openFolder is the open folder f as FolderStat has it, read from f
// itself, so nothing put at its path since it was opened counts. Its
// volume is VolumeUUID's, so Volume is "".
func openFolder(f *os.File) (FolderStat, error) {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return FolderStat{}, err
	}
	var fs unix.Statfs_t
	if err := unix.Fstatfs(int(f.Fd()), &fs); err != nil {
		return FolderStat{}, err
	}
	return FolderStat{
		ID:       statID(&st),
		Unique:   uniqueFS(&fs),
		ReadOnly: fs.Flags&(unix.MNT_RDONLY|unix.MNT_SNAPSHOT) != 0,
		Mount:    unix.ByteSliceToString(fs.Mntonname[:]),
		Device:   filepath.Base(unix.ByteSliceToString(fs.Mntfromname[:])),
		Owner:    st.Uid,
	}, nil
}

// uniqueFS reports whether a statfs is APFS's or HFS+'s, which never give
// a new folder a deleted one's inode number, so a FolderID with the
// folder's birth time is its alone (FolderStat.Unique). Other file
// systems (exFAT, network shares) may make inode numbers up or use them
// again.
func uniqueFS(st *unix.Statfs_t) bool {
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
