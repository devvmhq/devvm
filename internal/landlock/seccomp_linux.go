package landlock

import (
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"unsafe"
)

// seccomp's operation and flag, and prctl's.
const (
	seccompSetModeFilter = 1
	seccompFlagTSYNC     = 1 << 0
	prSetNoNewPrivs      = 38
)

// errNoSeccomp is installSeccomp's error on a kernel without seccomp filters.
var errNoSeccomp = errors.New("this kernel has no seccomp filters")

// installSeccomp installs the child's filter on every thread of this process at once (TSYNC),
// each of which already has no_new_privs.
func installSeccomp() error {
	a, ok := archOf(runtime.GOARCH)
	if !ok {
		return fmt.Errorf("no seccomp filter for %s", runtime.GOARCH)
	}
	p, err := filter(a)
	if err != nil {
		return fmt.Errorf("the seccomp filter: %w", err)
	}
	fs := make([]syscall.SockFilter, len(p))
	for i, in := range p {
		fs[i] = syscall.SockFilter{Code: in.code, Jt: in.jt, Jf: in.jf, K: in.k}
	}
	prog := syscall.SockFprog{Len: uint16(len(fs)), Filter: &fs[0]}                                                        //nolint:gosec // the program is a few dozen instructions
	r, _, e := syscall.Syscall(uintptr(a.seccomp), seccompSetModeFilter, seccompFlagTSYNC, uintptr(unsafe.Pointer(&prog))) //nolint:gosec // the kernel reads struct sock_fprog and the program it points to, which KeepAlive keeps
	runtime.KeepAlive(fs)
	switch {
	case e == syscall.ENOSYS:
		return errNoSeccomp
	case e != 0:
		return fmt.Errorf("seccomp: %w", e)
	case r != 0:
		// TSYNC returns the thread it couldn't give the filter.
		return fmt.Errorf("seccomp: thread %d kept its own filter", r)
	}
	return nil
}
