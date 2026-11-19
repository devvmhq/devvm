//go:build unix

package platform

import (
	"errors"
	"io/fs"
	"strconv"
	"syscall"
)

// Writable reports whether this process may write the folder dir, as the
// system judges it (access(2)), its ACLs and a network or FUSE file
// system's own answer included, which a folder's mode bits alone don't
// show; true when the system can't tell.
func Writable(dir string) bool {
	err := syscall.Access(dir, 0x2) // W_OK
	return err == nil || !errors.Is(err, syscall.EACCES) && !errors.Is(err, syscall.EPERM) && !errors.Is(err, syscall.EROFS)
}

// Links is how many names the file fi describes has (its link count): 1
// when the system can't tell.
func Links(fi fs.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return widen(st.Nlink)
	}
	return 1
}

// widen is a link count as a uint64, whatever the system's type for it
// (uint16 on macOS, uint64 on Linux).
func widen[T uint16 | uint32 | uint64](n T) uint64 { return uint64(n) }

// SameDevice reports whether two files are on the same file system.
func SameDevice(a, b fs.FileInfo) bool {
	sa, oka := a.Sys().(*syscall.Stat_t)
	sb, okb := b.Sys().(*syscall.Stat_t)
	return oka && okb && sa.Dev == sb.Dev
}

// allocated is the disk space a file takes: its blocks, not its length,
// so a sparse disk image counts what it holds.
func allocated(fi fs.FileInfo) int64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Blocks * 512
	}
	return fi.Size()
}

// Owner returns a file's uid and gid, false when fi has none.
func Owner(fi fs.FileInfo) (uid, gid uint32, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return st.Uid, st.Gid, true
}

// DevIno returns a file's device and inode as "dev:ino", "" when fi has
// none: what the file is, whatever its name.
func DevIno(fi fs.FileInfo) string {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return strconv.FormatUint(uint64(st.Dev), 10) + ":" + strconv.FormatUint(st.Ino, 10) //nolint:gosec,unconvert,nolintlint // Dev's type differs between systems: each needs one of the two
}
