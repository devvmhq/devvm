package proxy

import (
	"os"
	"syscall"
	"time"
	"unsafe"
)

// touchFile sets f's times to now through its descriptor, utimensat with no path, which the
// child's seccomp filter allows where it refuses a path (internal/landlock).
func touchFile(f *os.File, now time.Time) error {
	ts := [2]syscall.Timespec{syscall.NsecToTimespec(now.UnixNano()), syscall.NsecToTimespec(now.UnixNano())}
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var e syscall.Errno
	if err := rc.Control(func(fd uintptr) {
		_, _, e = syscall.Syscall6(syscall.SYS_UTIMENSAT, fd, 0, uintptr(unsafe.Pointer(&ts[0])), 0, 0, 0) //nolint:gosec // the kernel reads the two times
	}); err != nil {
		return err
	}
	if e != 0 {
		return e
	}
	return nil
}

// undumpable makes this process undumpable (PR_SET_DUMPABLE 0), so no process of the user's but
// root can read its memory or open its descriptors through /proc, nor trace it: the broker's,
// which hold its VM's secrets, from a child that a VM took over on a kernel without Landlock.
func undumpable() error {
	const prSetDumpable = 4
	if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, prSetDumpable, 0, 0); e != 0 {
		return e
	}
	return nil
}
