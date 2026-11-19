package platform

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

// ProcessStart returns when a running process started, as the kernel keeps it: /proc's
// starttime, in clock ticks since boot, as one word of digits, the same at every read for as long
// as the process runs.
func ProcessStart(pid int) (string, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", err
	}
	// The command's name, in parentheses, can hold spaces and parentheses itself, so the fields
	// are counted from the last ")": the state is the third field, and starttime the 22nd.
	i := strings.LastIndexByte(string(b), ')')
	if i < 0 {
		return "", errors.New("bad /proc stat")
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 20 {
		return "", errors.New("short /proc stat")
	}
	start := f[22-3]
	if _, err := strconv.ParseUint(start, 10, 64); err != nil {
		return "", errors.New("bad /proc starttime")
	}
	return start, nil
}
