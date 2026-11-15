//go:build darwin || linux

package platform

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// DataStart is where f's data starts, past a hole at its start: a log
// emptied while its writer went on at its own offset holds data only
// from there, and what's before it takes no disk. 0 for a file without
// such a hole, or one whose file system can't tell; f's length for one
// that is all hole.
func DataStart(f *os.File) int64 {
	off, err := unix.Seek(int(f.Fd()), 0, unix.SEEK_DATA)
	if errors.Is(err, unix.ENXIO) {
		if fi, err := f.Stat(); err == nil {
			return fi.Size()
		}
	}
	if err != nil || off < 0 {
		return 0
	}
	return off
}
