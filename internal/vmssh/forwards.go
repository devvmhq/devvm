package vmssh

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/daemon"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/ports"
	"github.com/devvmhq/devvm/internal/state"
)

// Cmd is devvm's hidden command that runs a VM's port forwarder.
const Cmd = "_ports"

// Forwards is the one owner of VMs' port forwarders (Forwarder), on every
// backend: devvm _ports, detached, one per running VM that forwards ports.
// Which ports it forwards is the VM's state's (ports.Forward), read at
// each start, never a copy of the backend's; how it reaches the VM is the
// backend's (its ssh as FromTarget checks it, its Lifeline). Its lock, its files and its
// process are its own: the pid, status and lock in the VM's run folder
// (paths.VM.Forwarder), its log in its logs folder, and its master's
// control socket in devvm's run folder, named by a hash. The host keys it
// checks are the VM's one record for its start (PinHostKeys).
type Forwards struct {
	Dirs paths.Dirs
	// Review is what a VM's ssh is checked against (Checked).
	Review backend.Review
	// Run is where the control sockets go instead, when set: tests' run
	// folders can be too long for a socket.
	Run string
	// Self is devvm's binary, which runs the forwarder (Cmd).
	Self string
	// Detach starts a process that outlives devvm (daemon.Detach).
	Detach daemon.Starter
	// Args returns a running process's arguments (platform.ProcessArgs).
	Args func(pid int) ([]string, error)
	// Tick is how often a start looks for the forwarder to be ready, 20
	// times at most.
	Tick time.Duration
	// InUse reports whether a program on the host has a port, on any
	// address (platform.PortInUse): a listed port it has isn't forwarded.
	InUse func(ctx context.Context, port int) bool
	// Reporter is where it warns: ports_taken, ports_forward_failed.
	msg.Reporter
}

// NewForwards returns the forwarders of devvm's folders, run by self
// (devvm's binary), as processes of their own, each VM's ssh checked
// against r. Its caller sets InUse and Reporter.
func NewForwards(d paths.Dirs, r backend.Review, self string) Forwards {
	return Forwards{Dirs: d, Review: r, Self: self, Detach: daemon.Detach, Args: platform.ProcessArgs, Tick: 500 * time.Millisecond}
}

// spec is what a VM's forwarder runs with: how it reaches the VM's sshd
// (the backend's target as FromTarget checks it, with the record of the
// host keys devvm pins for its start), its lifeline, the host ports it
// never forwards, and the ports it forwards (nil for every one).
type spec struct {
	SSH
	Lifeline string
	Reserve  []int
	Only     []backend.PortRange
}

// Log is the VM's forwarder's log.
func (f Forwards) Log(vm string) string { return f.Dirs.VM(vm).ForwarderLog() }

// Control is the master's control socket: in the run folder, named by a
// hash, as a socket's path has fewer than paths.SocketMax characters and
// ssh adds 17 while it makes it.
func (f Forwards) Control(vm string) string {
	sum := sha256.Sum256([]byte(vm))
	return filepath.Join(cmp.Or(f.Run, f.Dirs.Run()), "port-"+hex.EncodeToString(sum[:])[:16]+".ctl")
}

// logMax is the most a forwarder's log keeps from earlier starts: past
// it, a start empties it.
const logMax = 1 << 20

// ControlMax is the longest control socket path ssh can make.
const ControlMax = paths.SocketMax - 1 - 17

// Start starts the forwarder of st's VM, which b runs, after stopping any
// it had, holding its lock throughout: the ports its state forwards, its
// listed ones less those a program on the host has (warned, ports_taken),
// over its sshd as b reaches it (its SSH, checked by FromTarget: a user
// and a key on a host loopback port), with the host keys devvm pins for
// its start (PinHostKeys), never on its ssh's or its proxy's port, following b's
// lifeline for it. A VM that forwards none, or whose listed ports are all
// taken, is left without one. One that can't start is warned about
// (ports_forward_failed), naming its log, and the VM runs on. Connections
// open through its old forwarder end.
func (f Forwards) Start(ctx context.Context, b backend.Backend, st *state.VM) {
	if err := f.start(ctx, b, st); err != nil {
		f.Warn(msg.PortsForwardFailed(f.Log(st.Name()), st.Name()))
	}
}

func (f Forwards) start(ctx context.Context, b backend.Backend, st *state.VM) error {
	vm := st.Name()
	p, err := ports.Of(st)
	if err != nil {
		return err
	}
	want, err := ports.Forwarding(p.Forward)
	if err != nil {
		return err
	}
	unlock, err := f.Lock(ctx, vm)
	if err != nil {
		return err
	}
	defer unlock()
	f.Stop(vm)
	var only []backend.PortRange
	if !want.All {
		// Asked once its old forwarder has stopped: its own ports would
		// read as taken.
		if only = f.free(ctx, vm, want.Ranges); len(only) == 0 {
			return nil
		}
	}
	reach, err := Checked(ctx, f.Dirs, f.Review, b, vm, PinKeys)
	if err != nil {
		return err
	}
	s := reach.SSH
	run, err := b.Running(ctx, vm)
	if err != nil {
		return err
	}
	return f.run(vm, spec{SSH: s, Lifeline: run.Lifeline, Reserve: []int{s.Port, p.Host.Proxy}, Only: only})
}

// free returns the VM's listed ports less those a program on the host
// has on any address, which a forward would share, taking that program's
// loopback connections. They're left out, warned, until a start or a
// change of its ports finds them free.
func (f Forwards) free(ctx context.Context, vm string, listed []backend.PortRange) []backend.PortRange {
	free, taken := splitPorts(listed, func(p int) bool { return f.InUse(ctx, p) })
	if len(taken) > 0 {
		n := 0
		for _, r := range taken {
			n += r.To - r.From + 1
		}
		f.Warn(msg.PortsTaken(n, backend.FormatPortRanges(taken), vm))
	}
	return free
}

// splitPorts splits ranges into the ports inUse says are free and those it
// says aren't, each as ranges: a range wholly free stays as it is.
func splitPorts(rs []backend.PortRange, inUse func(int) bool) (free, taken []backend.PortRange) {
	add := func(out []backend.PortRange, from, p int) []backend.PortRange {
		if n := len(out); n > 0 && out[n-1].From >= from && out[n-1].To == p-1 {
			out[n-1].To = p
			return out
		}
		return append(out, backend.PortRange{From: p, To: p})
	}
	for _, r := range rs {
		for p := r.From; p <= r.To; p++ {
			if inUse(p) {
				taken = add(taken, r.From, p)
			} else {
				free = add(free, r.From, p)
			}
		}
	}
	return free, taken
}

// run starts the VM's forwarder and waits for it to say it forwards, in
// its status file, while it runs. One that doesn't is left to keep
// trying.
func (f Forwards) run(vm string, s spec) error {
	ctl := f.Control(vm)
	if len(ctl) > ControlMax {
		return fmt.Errorf("%s: its path is too long for a socket", ctl)
	}
	if err := os.MkdirAll(filepath.Dir(ctl), 0o700); err != nil {
		return err
	}
	for _, dir := range []string{f.Dirs.VM(vm).Run(), f.Dirs.VM(vm).Logs()} {
		if err := state.PrivateDir(dir); err != nil {
			return err
		}
	}
	out, err := os.OpenFile(f.Log(vm), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	fi, err := out.Stat()
	if err != nil {
		return err
	}
	if fi.Size() > logMax {
		if err := out.Truncate(0); err != nil {
			return err
		}
	}
	var reserve []string
	for _, p := range s.Reserve {
		if p > 0 {
			reserve = append(reserve, strconv.Itoa(p))
		}
	}
	args := []string{
		Cmd, "-vm", vm, "-lifeline", s.Lifeline, "-port", strconv.Itoa(s.Port),
		"-user", s.User, "-key", s.Key, "-known-hosts", s.KnownHosts, "-control", ctl, "-reserve", strings.Join(reserve, ","),
	}
	if s.Only != nil {
		args = append(args, "-only", backend.FormatPortRanges(s.Only))
	}
	status := f.Dirs.VM(vm).Forwarder(".status")
	pid, err := f.proc(vm).Start(f.Detach, daemon.Cmd{Path: f.Self, Args: args, Out: out, Status: status})
	if err != nil {
		return err
	}
	if _, ok := daemon.WaitReady(status, pid, func() bool { return daemon.Alive(pid) }, f.Tick, 20); ok {
		return nil
	}
	return fmt.Errorf("%s: its forwarder isn't forwarding", vm)
}

// proc is the VM's forwarder as a background process: its pid file in the
// VM's run folder, and devvm's hidden command (Cmd) with -vm VM in its
// arguments.
func (f Forwards) proc(vm string) daemon.Proc {
	return daemon.Proc{
		PIDFile: f.Dirs.VM(vm).Forwarder(".pid"), Args: f.Args,
		Is: func(args []string) bool { return daemon.HasFlag(args, Cmd, "-vm", vm) },
	}
}

// Lock holds the VM's forwarder's lock until unlock is called: whatever
// stops its forwarder, asks which ports are free and starts the next one
// holds it throughout, so two at once (a start and devvm ports, or two
// devvm ports) never leave a forwarder whose pid file names another, which
// nothing would stop. Another devvm holding it is waited for, until ctx
// ends.
func (f Forwards) Lock(ctx context.Context, vm string) (unlock func(), err error) {
	if err := state.PrivateDir(f.Dirs.VM(vm).Run()); err != nil {
		return nil, err
	}
	return platform.LockFile(ctx, f.Dirs.VM(vm).Forwarder(".lock"))
}

// Stop stops the VM's forwarder, if it runs, waiting up to two seconds for
// it to end, then killing it and waiting up to two more: a start that
// follows finds its ports free. Every way a VM stops through devvm, or
// goes, stops it too; one that stops otherwise ends it by its lifeline.
func (f Forwards) Stop(vm string) { f.proc(vm).Stop(2 * time.Second) }
