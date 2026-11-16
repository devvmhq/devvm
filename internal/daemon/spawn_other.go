//go:build !linux

package daemon

import "syscall"

// dieWithThread does nothing where the system can't tie a process's end to its parent's thread
// (Cmd.DieWithThread): a proxy's child reads its socket's end instead.
func dieWithThread(*syscall.SysProcAttr) {}
