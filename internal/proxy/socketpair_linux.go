//go:build linux

package proxy

import "syscall"

// socketpair is the broker's unix stream socketpair to its child, close-on-exec from the start
// (SOCK_CLOEXEC), so no program the broker runs holds an end: only the child's descriptor 3,
// which its start makes of one end, does.
func socketpair() ([2]int, error) {
	return syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
}
