// Package daemon runs devvm's background processes: the VMs'
// proxies, the port forwarders, the link sessions and the build proxies.
// It starts each one detached (Detach, Spawn), finds it again by its pid
// file and its arguments (Proc), stops it (SIGTERM, a wait, then SIGKILL),
// waits for it to say it's ready in a status file of its own (Ready,
// WaitReady), and gives each the one meaning of a lifeline, the file of
// the process that runs a VM, which it follows (Lifeline). Their accept
// loops wait after an error that leaves the listener open (AcceptRetry).
//
// It's the one place devvm signals a process by its pid (forbidigo holds
// the rest to it). Standard library only, so devvm _proxy (internal/proxy)
// uses it too; Unix only for now (syscall.Kill, Setsid), as the proxy is.
package daemon

import (
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// A Cmd is a background process to start: its program, its arguments
// (without the program), the file its output goes to (nil: none), and its status
// file, where it says it's ready (Ready); "" for one that doesn't say.
type Cmd struct {
	Path   string
	Args   []string
	Out    *os.File
	Status string
}

// A Starter starts a background process and returns its pid: Detach, or a
// test's.
type Starter func(Cmd) (int, error)

// Detach starts c in a session of its own, so it outlives devvm and the
// terminal it ran in, with no input and its output in c.Out, and returns
// its pid without waiting for it. devvm reaps it if it ends first, so a
// process that ended never looks alive (Alive).
func Detach(c Cmd) (int, error) {
	h, err := Spawn(c)
	if err != nil {
		return 0, err
	}
	return h.pid, nil
}

// Spawn is Detach, returning the process itself (a Handle): signals sent
// through it reach that process alone, never another given its pid later.
func Spawn(c Cmd) (*Handle, error) {
	cmd := exec.Command(c.Path, c.Args...) //nolint:gosec,noctx // devvm's own binary; it outlives the run's context
	// Without a file, the null device, as for its input.
	if c.Out != nil {
		cmd.Stdout, cmd.Stderr = c.Out, c.Out
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// Only its own status file: never one devvm itself was given. No SSH
	// agent: none of devvm's background processes uses one (devvm's own
	// ssh says IdentityAgent=none), and a VM's agent is only for its own
	// connections, never a process that outlives the run.
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, StatusEnv+"=") && !strings.HasPrefix(kv, "SSH_AUTH_SOCK=") {
			env = append(env, kv)
		}
	}
	if c.Status != "" {
		env = append(env, StatusEnv+"="+c.Status)
	}
	cmd.Env = env
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	h := &Handle{pid: cmd.Process.Pid, proc: cmd.Process, done: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(h.done) }()
	return h, nil
}

// A Handle is a process Spawn started, which devvm reaps once it ends.
type Handle struct {
	pid  int
	proc *os.Process
	done chan struct{}
}

// PID is the process's pid.
func (h *Handle) PID() int { return h.pid }

// Done is closed once the process has ended.
func (h *Handle) Done() <-chan struct{} { return h.done }

// Running reports whether the process hasn't ended.
func (h *Handle) Running() bool {
	select {
	case <-h.done:
		return false
	default:
		return true
	}
}

// Stop stops the process, if it runs: SIGTERM, then after wait SIGKILL,
// then up to wait more. It reports whether it has ended. A nil Handle is
// none.
func (h *Handle) Stop(wait time.Duration) bool {
	if h == nil {
		return true
	}
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		if !h.Running() {
			return true
		}
		_ = h.proc.Signal(sig)
		select {
		case <-h.done:
			return true
		case <-time.After(wait):
		}
	}
	return !h.Running()
}

// Alive reports whether a process runs with that pid (one of the user's:
// signal 0 reaches it).
func Alive(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == nil }

// Signal sends the process with that pid a signal; a pid below 1, which
// would name a group of processes, is refused.
func Signal(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return syscall.ESRCH
	}
	return syscall.Kill(pid, sig)
}

// SignalGroup sends the process with that pid a signal, and with it every
// process of its group when it leads one, as each background process
// does (Detach starts it in a session of its own): what it started, a
// forwarder's or a link session's ssh masters and relays, ends with it,
// even when it's killed and can't stop them itself. One that leads no
// group gets it alone (Signal).
func SignalGroup(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return syscall.ESRCH
	}
	if pgid, err := syscall.Getpgid(pid); err == nil && pgid == pid {
		return syscall.Kill(-pid, sig)
	}
	return Signal(pid, sig)
}
