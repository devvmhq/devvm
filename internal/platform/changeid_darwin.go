package platform

import (
	"io/fs"
	"strconv"
	"syscall"
)

// ChangeID is a file's inode and change time, "ino I ctime S.N", which
// any change to the file moves and nobody but the kernel sets: "" when fi
// has none.
func ChangeID(fi fs.FileInfo) string {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return "ino " + strconv.FormatUint(st.Ino, 10) + " ctime " +
		strconv.FormatInt(st.Ctimespec.Sec, 10) + "." + strconv.FormatInt(st.Ctimespec.Nsec, 10)
}
