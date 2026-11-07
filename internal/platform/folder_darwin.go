package platform

import (
	"fmt"
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
	return fmt.Sprintf("%d:%d:%d.%09d", st.Dev, st.Ino, st.Btim.Sec, st.Btim.Nsec), nil
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
