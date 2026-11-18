package platform

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// FolderID returns what identifies the folder at path itself, not its
// path: its device, inode and birth time. ext4 gives a folder made again
// the inode number the deleted one had, so the birth time (where the file
// system keeps one) tells them apart.
func FolderID(path string) (string, error) {
	return statxID(unix.AT_FDCWD, path, 0)
}

// openFolderID is the FolderID of the open folder f, and whether it's
// its alone (UniqueFolderID), both read from f itself, so nothing put at
// its path since it was opened counts.
func openFolderID(f *os.File) (string, bool, error) {
	id, err := statxID(int(f.Fd()), "", unix.AT_EMPTY_PATH)
	if err != nil {
		return "", false, err
	}
	var fs unix.Statfs_t
	if err := unix.Fstatfs(int(f.Fd()), &fs); err != nil {
		return "", false, err
	}
	return id, strings.Count(id, ":") == 2 && uniqueFS(fs.Type), nil
}

// statxID is the FolderID of what statx finds at dirfd, path and flags.
func statxID(dirfd int, path string, flags int) (string, error) {
	var st unix.Statx_t
	if err := unix.Statx(dirfd, path, flags, unix.STATX_INO|unix.STATX_BTIME, &st); err != nil {
		return "", err
	}
	id := fmt.Sprintf("%d:%d", unix.Mkdev(st.Dev_major, st.Dev_minor), st.Ino)
	if st.Mask&unix.STATX_BTIME != 0 {
		id += fmt.Sprintf(":%d.%09d", st.Btime.Sec, st.Btime.Nsec)
	}
	return id, nil
}

// zfsSuperMagic is OpenZFS's statfs type, which golang.org/x/sys lacks.
const zfsSuperMagic = 0x2fc12fc1

// UniqueFolderID reports whether id, the FolderID of the folder at path,
// is that folder's alone, never a deleted one's or another file system's:
// it has the folder's birth time, as an inode number alone is used again,
// and the folder is on a local file system with stable inode numbers
// (uniqueFS). FUSE, network shares and FAT may make inode numbers up.
func UniqueFolderID(path, id string) bool {
	var st unix.Statfs_t
	if strings.Count(id, ":") != 2 || unix.Statfs(path, &st) != nil {
		return false
	}
	return uniqueFS(st.Type)
}

// uniqueFS reports whether a statfs type is a local file system that keeps
// each inode number for its file's life and reports a birth time through
// statx: ext4, XFS, Btrfs, tmpfs, OpenZFS, F2FS and bcachefs.
func uniqueFS(fsType int64) bool {
	switch fsType {
	case unix.EXT4_SUPER_MAGIC, unix.XFS_SUPER_MAGIC, unix.BTRFS_SUPER_MAGIC, unix.TMPFS_MAGIC,
		zfsSuperMagic, unix.F2FS_SUPER_MAGIC, unix.BCACHEFS_SUPER_MAGIC:
		return true
	}
	return false
}

// HasImmutableFlag reports whether this system has a user-immutable flag:
// Linux's immutable attribute needs root.
const HasImmutableFlag = false

// FoldsCase reports whether file names here may match whatever their
// case: not on Linux's file systems, as devvm's project folders use them.
const FoldsCase = false

// Chflags sets file flags: Linux has no unprivileged immutable flag.
func Chflags(context.Context, string, string) error { return errors.ErrUnsupported }

// Immutable reports whether a path has the user-immutable flag: Linux has
// none.
func Immutable(string) bool { return false }
