//go:build e2e

package e2e

import (
	"runtime"
	"slices"
	"strings"

	"github.com/devvmhq/devvm/internal/platform"
)

// SaidLine is whether one of out's lines is one of msg's lines (grep -qxF).
func SaidLine(out, msg string) bool {
	for l := range strings.SplitSeq(msg, "\n") {
		if l != "" && HasLine(out, l) {
			return true
		}
	}
	return false
}

// FirstLine and LastLine are a message's first and last lines (m … | head -1).
func FirstLine(msg string) string { l, _, _ := strings.Cut(msg, "\n"); return l }

func LastLine(msg string) string { return tailLines(msg, 1) }

// tailLines is text's last n lines (tail -n N).
func tailLines(text string, n int) string {
	l := strings.Split(strings.TrimRight(text, "\n"), "\n")
	return strings.Join(l[max(0, len(l)-n):], "\n")
}

// Trim is a command's output as $(…) has it: its trailing newlines gone.
func Trim(out string) string { return strings.TrimRight(out, "\n") }

// Listed is whether limactl lists the VM (limactl list -q).
func Listed(s *S, vm string) bool {
	r, _ := s.Limactl("list", "-q")
	return HasLine(r.Stdout, vm)
}

// Locked is whether the file has the immutable flag, as .git protection
// sets it: macOS's user immutable flag (ls -lO shows uchg), or Linux's
// immutable attribute, which devvm-immutable sets (as statx reports it).
func Locked(s *S, f string) bool {
	if runtime.GOOS != "darwin" {
		return platform.Immutable(f)
	}
	r, err := s.Run(Cmd{Args: []string{"ls", "-lOd", f}})
	return err == nil && strings.Contains(r.Stdout, "uchg")
}

// Procs is every host process's command line, one a line (ps -axww),
// which stays out of the check's log, as the process watch's.
func Procs(s *S) []string {
	b, _ := psAll(s.r.window().ctx)
	return strings.Split(string(b), "\n")
}

// Forwarders is the command lines of the VM's port forwarders here
// (devvm _ports -vm VM).
func Forwarders(s *S, vm string) []string {
	var out []string
	for _, l := range Procs(s) {
		if strings.Contains(l, "_ports -vm "+vm+" ") {
			out = append(out, l)
		}
	}
	return out
}

// DevvmIn runs devvm args in dir with input (none when empty), env on
// top of the sandbox's (devvmCmd's command).
func DevvmIn(s *S, dir, input string, env []string, args ...string) (Res, error) {
	return s.Run(devvmCmd(s, dir, input, env, args...))
}

// OwnVM makes the VM this run's: deleted at its end.
func OwnVM(s *S, vm string) {
	if vm != "" && !slices.Contains(s.sb.created, vm) {
		s.sb.created = append(s.sb.created, vm)
	}
}
