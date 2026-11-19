//go:build !unix

package platform

import "io/fs"

// SameDevice reports whether two files are on the same file system: on
// this system, always yes.
func SameDevice(fs.FileInfo, fs.FileInfo) bool { return true }

// Writable reports whether this process may write the folder: yes, as
// this system can't tell.
func Writable(string) bool { return true }

// Links is how many names a file has: 1, as this system can't tell.
func Links(fs.FileInfo) uint64 { return 1 }

// allocated is the disk space a file takes: on this system, its length.
func allocated(fi fs.FileInfo) int64 { return fi.Size() }

// Owner returns a file's uid and gid: none on this system.
func Owner(fs.FileInfo) (uid, gid uint32, ok bool) { return 0, 0, false }

// DevIno returns a file's device and inode: none on this system.
func DevIno(fs.FileInfo) string { return "" }
