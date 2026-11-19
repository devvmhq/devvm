package platform

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// ProcessStart returns when a running process started, as the kernel keeps it (kern.proc.pid's
// p_starttime): seconds and microseconds since the epoch, as one word of digits and a dot, the
// same at every read for as long as the process runs.
func ProcessStart(pid int) (string, error) {
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", err
	}
	if int(k.Proc.P_pid) != pid {
		return "", fmt.Errorf("no process %d", pid)
	}
	t := k.Proc.P_starttime
	return fmt.Sprintf("%d.%06d", t.Sec, t.Usec), nil
}
