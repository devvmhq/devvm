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
	"runtime"
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
	// Err is the file its error output goes to, when it isn't Out's.
	Err *os.File
	// Files are descriptors it inherits besides its input and output,
	// its 3 and on, in order: a proxy's child gets its socket to its
	// broker and its listener.
	Files []*os.File
	// Env, when it isn't nil, is its whole environment, which its caller
	// chose, Status's variable added; nil: devvm's own, without its
	// status file's or its SSH agent's.
	Env []string
	// DieWithThread, on Linux, kills it (SIGKILL) once the thread that
	// started it ends: Spawn starts it from a goroutine that holds its
	// thread until it's reaped, so it ends with devvm too, even killed.
	// Elsewhere it does nothing.
	DieWithThread bool
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
	if c.Err != nil {
		cmd.Stderr = c.Err
	}
	cmd.ExtraFiles = c.Files
	// A session of its own also keeps a proxy's child, under Seatbelt, from every other process's
	// arguments: macOS answers kern.procargs2 to a sandboxed process within its own process group
	// alone, so the child's confinement of process info relies on this.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if c.DieWithThread {
		dieWithThread(cmd.SysProcAttr)
	}
	cmd.Env = c.Env
	if cmd.Env == nil {
		// Only its own status file: never one devvm itself was given. No SSH
		// agent: none of devvm's background processes uses one (devvm's own
		// ssh says IdentityAgent=none), and a VM's agent is only for its own
		// connections, never a process that outlives the run.
		cmd.Env = make([]string, 0, len(os.Environ())+1)
		for _, kv := range os.Environ() {
			if !strings.HasPrefix(kv, StatusEnv+"=") && !strings.HasPrefix(kv, "SSH_AUTH_SOCK=") {
				cmd.Env = append(cmd.Env, kv)
			}
		}
	}
	if c.Status != "" {
		cmd.Env = append(cmd.Env, StatusEnv+"="+c.Status)
	}
	h := &Handle{done: make(chan struct{})}
	started := make(chan error, 1)
	// One goroutine starts it and reaps it, holding its thread throughout
	// and never letting it go: Linux ties DieWithThread's signal to the
	// thread that started the process, which then ends with the goroutine,
	// once the process is reaped.
	go func() {
		runtime.LockOSThread()
		if err := cmd.Start(); err != nil {
			started <- err
			return
		}
		h.pid, h.proc = cmd.Process.Pid, cmd.Process
		started <- nil
		_ = cmd.Wait()
		h.state = cmd.ProcessState
		close(h.done)
	}()
	if err := <-started; err != nil {
		return nil, err
	}
	return h, nil
}

// A Handle is a process Spawn started, which devvm reaps once it ends.
type Handle struct {
	pid   int
	proc  *os.Process
	done  chan struct{}
	state *os.ProcessState // how it ended, once done is closed
}

// Kill kills the process, if it runs (SIGKILL).
func (h *Handle) Kill() {
	if h.Running() {
		_ = h.proc.Kill()
	}
}

// Exit is how the process ended, such as "exit status 1" or "signal:
// killed": "running" while it runs.
func (h *Handle) Exit() string {
	select {
	case <-h.done:
		return h.state.String()
	default:
		return "running"
	}
}

// ExitCode is the process's exit status once it has ended, and -1 while it runs or when a signal
// ended it.
func (h *Handle) ExitCode() int {
	select {
	case <-h.done:
		return h.state.ExitCode()
	default:
		return -1
	}
}

// ExitSignal is the signal that ended the process, once it has ended; 0 while it runs, or when it
// exited.
func (h *Handle) ExitSignal() syscall.Signal {
	select {
	case <-h.done:
		if ws, ok := h.state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return ws.Signal()
		}
	default:
	}
	return 0
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
