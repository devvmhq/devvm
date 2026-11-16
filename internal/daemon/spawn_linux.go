//go:build linux

package daemon

import "syscall"

// dieWithThread has Linux kill the process (SIGKILL) once the thread that started it ends
// (PR_SET_PDEATHSIG): Cmd.DieWithThread.
func dieWithThread(a *syscall.SysProcAttr) { a.Pdeathsig = syscall.SIGKILL }
