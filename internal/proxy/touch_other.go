//go:build !linux

package proxy

import (
	"os"
	"syscall"
	"time"
)

// touchFile sets f's times to now through its descriptor (futimes).
func touchFile(f *os.File, now time.Time) error {
	tv := []syscall.Timeval{syscall.NsecToTimeval(now.UnixNano()), syscall.NsecToTimeval(now.UnixNano())}
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var ferr error
	if err := rc.Control(func(fd uintptr) { ferr = syscall.Futimes(int(fd), tv) }); err != nil {
		return err
	}
	return ferr
}

// undumpable does nothing here: on macOS no process reads another's memory without a debugger's
// rights, and the child's profile refuses it a look at another process.
func undumpable() error { return nil }
