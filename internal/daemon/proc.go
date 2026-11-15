package daemon

import (
	"os"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// A Proc is one of devvm's background processes, found by its pid file:
// the process that file names, while it runs and its arguments say it's
// that one (Is), so a process given the pid since, or a file left by one
// that ended, is never taken for it.
type Proc struct {
	// PIDFile is where its pid is kept, on a line of its own.
	PIDFile string
	// Is reports whether a process's arguments, its program first, are
	// this one's.
	Is func(args []string) bool
	// Args returns a running process's arguments (platform.ProcessArgs).
	Args func(pid int) ([]string, error)
	// Signal sends it a signal: Signal, or a test's.
	Signal func(pid int, sig syscall.Signal) error
	// Alive reports whether a process has pid: Alive, or a test's.
	Alive func(pid int) bool
}

// alive reports whether a process has pid (Alive).
func (p Proc) alive(pid int) bool {
	if p.Alive == nil {
		return Alive(pid)
	}
	return p.Alive(pid)
}

// HasFlag reports whether args, its program first, run devvm's command
// cmd with flag's value val (-vm NAME).
func HasFlag(args []string, cmd, flag, val string) bool {
	if len(args) < 2 || args[1] != cmd {
		return false
	}
	i := slices.Index(args, flag)
	return i >= 0 && i+1 < len(args) && args[i+1] == val
}

// PID returns the process's pid while it runs.
func (p Proc) PID() (int, bool) {
	pid, ok := readLine(p.PIDFile)
	if !ok || !p.alive(pid) {
		return 0, false
	}
	args, err := p.Args(pid)
	if err != nil || !p.Is(args) {
		return 0, false
	}
	return pid, true
}

// Running reports whether the process runs.
func (p Proc) Running() bool {
	_, ok := p.PID()
	return ok
}

// runs reports whether pid is still the process.
func (p Proc) runs(pid int) bool {
	cur, ok := p.PID()
	return ok && cur == pid
}

// Start starts c with start (Detach, or a test's), after removing the
// status file an earlier start left, and records its pid in the pid file.
func (p Proc) Start(start Starter, c Cmd) (int, error) {
	if c.Status != "" {
		if err := os.Remove(c.Status); err != nil && !os.IsNotExist(err) {
			return 0, err
		}
	}
	pid, err := start(c)
	if err != nil {
		return 0, err
	}
	return pid, writeLine(p.PIDFile, strconv.Itoa(pid))
}

// Stop stops the process, if it runs: SIGTERM, then after wait SIGKILL,
// then up to wait more, looking every twentieth of it. A process given
// the pid since is never signalled: SIGKILL goes only to a process whose
// arguments still say it's this one, so not to one still ending, whose
// arguments can't be read (ended), nor to another given the pid whose
// arguments can't be read either. Once it has ended its pid file goes,
// unless a start since has written another pid there. It reports whether
// none runs then.
func (p Proc) Stop(wait time.Duration) bool {
	pid, ok := p.PID()
	if !ok {
		return true
	}
	signal := p.Signal
	if signal == nil {
		signal = Signal
	}
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		if sig == syscall.SIGTERM || p.runs(pid) {
			_ = signal(pid, sig)
		}
		for range 20 {
			if p.ended(pid) {
				if cur, ok := readLine(p.PIDFile); ok && cur == pid {
					_ = os.Remove(p.PIDFile)
				}
				return true
			}
			time.Sleep(wait / 20)
		}
	}
	return false
}

// ended reports whether the process pid, which was this one, has ended:
// no process has the pid, or another one does now. One whose arguments
// can't be read is still ending: its memory goes before its files, so a
// port it listens on is still its own a moment after (macOS's
// kern.procargs2 fails first).
func (p Proc) ended(pid int) bool {
	if !p.alive(pid) {
		return true
	}
	args, err := p.Args(pid)
	return err == nil && !p.Is(args)
}

// Ready waits for the process pid, which Start started with status, to
// say it's ready there (WaitReady), while it runs as this one.
func (p Proc) Ready(status string, pid int, tick time.Duration, tries int) (string, bool) {
	return WaitReady(status, pid, func() bool { return p.starting(pid) }, tick, tries)
}

// starting reports whether pid, which Start just started, still runs as
// this one, as runs does, except that one whose arguments don't say so
// yet still counts while they read as none or as devvm's own: Linux lets
// the parent go on as exec begins, while the child still has the
// parent's memory, and so its arguments, then shows none as exec
// replaces that memory, and its new ones only once the program is
// loaded. One that has ended is reaped (Detach), so it no longer counts
// once it's gone.
func (p Proc) starting(pid int) bool {
	cur, ok := readLine(p.PIDFile)
	if !ok || cur != pid || !p.alive(pid) {
		return false
	}
	args, err := p.Args(pid)
	return err != nil || p.Is(args) || slices.Equal(args, os.Args)
}

// writeLine writes s as a file's one line, mode 600, in a single write:
// a reader that finds no line end yet (readLine, readStatus) has caught it
// part-way, and takes it as not written yet, as it takes no file. daemon
// is the standard library only, so it doesn't use state.WriteFile.
func writeLine(path, s string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:forbidigo,gosec // devvm's own run folder; a part-way line reads as none (readLine, readStatus)
	if err != nil {
		return err
	}
	_, err = f.WriteString(s + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// readLine reads the pid on a file's first line, written whole
// (writeLine): ok is false for none, a partial one or no pid.
func readLine(path string) (int, bool) {
	b, err := os.ReadFile(path) //nolint:gosec // devvm's own run folder
	if err != nil {
		return 0, false
	}
	first, _, whole := strings.Cut(string(b), "\n")
	pid, err := strconv.Atoi(strings.TrimSpace(first))
	return pid, whole && err == nil && pid > 0
}
