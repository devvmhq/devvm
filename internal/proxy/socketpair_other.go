//go:build !linux

package proxy

import "syscall"

// socketpair is the broker's unix stream socketpair to its child, close-on-exec before any program
// can start: made and marked under syscall.ForkLock, as the standard library makes its own
// descriptors where SOCK_CLOEXEC isn't there, so no program the broker runs holds an end, only
// the child's descriptor 3, which its start makes of one end.
func socketpair() ([2]int, error) {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return fds, err
	}
	syscall.CloseOnExec(fds[0])
	syscall.CloseOnExec(fds[1])
	return fds, nil
}
