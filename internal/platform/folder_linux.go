package platform

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

// FolderID returns what identifies the folder at path itself, not its
// path: its device, inode and birth time. ext4 gives a folder made again
// the inode number the deleted one had, so the birth time (where the file
// system keeps one) tells them apart.
func FolderID(path string) (string, error) {
	st, err := statx(unix.AT_FDCWD, path, 0)
	if err != nil {
		return "", err
	}
	return statxID(&st), nil
}

// openFolder is the open folder f as FolderStat has it, read from f
// itself, so nothing put at its path since it was opened counts. Its
// volume is "fsid:<f_fsid>" on a file system whose f_fsid is its own
// (fsidOwn), mounted for writing, and "" elsewhere.
func openFolder(f *os.File) (FolderStat, error) {
	st, err := statx(int(f.Fd()), "", unix.AT_EMPTY_PATH)
	if err != nil {
		return FolderStat{}, err
	}
	var fs unix.Statfs_t
	if err := unix.Fstatfs(int(f.Fd()), &fs); err != nil {
		return FolderStat{}, err
	}
	id := statxID(&st)
	s := FolderStat{ID: id, Unique: strings.Count(id, ":") == 2 && uniqueFS(fs.Type), ReadOnly: fs.Flags&unix.ST_RDONLY != 0, Owner: st.Uid}
	if !s.ReadOnly && fs.Fsid.Val != [2]int32{} && slices.Contains(fsidOwn(), fs.Type) {
		s.Volume = fmt.Sprintf("fsid:%08x%08x", uint32(fs.Fsid.Val[0]), uint32(fs.Fsid.Val[1])) //nolint:gosec // G115: the ID's bits as they are, not a number's value
	}
	return s, nil
}

// statx is what statx finds at dirfd, path and flags: its device, inode,
// birth time and owner.
func statx(dirfd int, path string, flags int) (unix.Statx_t, error) {
	var st unix.Statx_t
	err := unix.Statx(dirfd, path, flags, unix.STATX_INO|unix.STATX_BTIME|unix.STATX_UID, &st)
	return st, err
}

// statxID is a statx's FolderID.
func statxID(st *unix.Statx_t) string {
	id := fmt.Sprintf("%d:%d", unix.Mkdev(st.Dev_major, st.Dev_minor), st.Ino)
	if st.Mask&unix.STATX_BTIME != 0 {
		id += fmt.Sprintf(":%d.%09d", st.Btime.Sec, st.Btime.Nsec)
	}
	return id
}

// zfsSuperMagic is OpenZFS's statfs type, which golang.org/x/sys lacks.
const zfsSuperMagic = 0x2fc12fc1

// uniqueFS reports whether a statfs type is a local file system that keeps
// each inode number for its file's life and reports a birth time through
// statx: ext4, XFS, Btrfs, tmpfs, OpenZFS, F2FS and bcachefs. On one, a
// FolderID with the folder's birth time is its alone, never a deleted
// one's or another file system's (FolderStat.Unique), as an inode number
// alone is used again; FUSE, network shares and FAT may make inode numbers
// up.
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
