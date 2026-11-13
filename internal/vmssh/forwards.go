package vmssh

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
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
// those another running VM holds included (warned, ports_held: free),
// over its sshd as b reaches it (its SSH, checked by FromTarget: a user
// and a key on a host loopback port), with the host keys devvm pins for
// its start (PinHostKeys), never on its ssh's or its proxy's port, following b's
// lifeline for it. A VM that forwards none, or whose listed ports are all
// a program's, is left without one. One that can't start is warned about
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
	// Its old forwarder's record goes with it, so a pid used again never
	// makes it the new one's.
	if err := os.Remove(f.Dirs.VM(vm).Forwarder(".forwarded")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
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
// change of its ports finds them free. A port another running VM forwards
// isn't a program's: it stays, as the VM's forwarder takes it once that
// VM gives it up (ports.Reserved), within Forwarder.Recheck from one that
// forwards every port, else once it stops forwarding it, which the VM is
// warned of either way (ports_held).
func (f Forwards) free(ctx context.Context, vm string, listed []backend.PortRange) []backend.PortRange {
	holders, err := ports.Holders(f.Dirs, vm, f.live)
	if err != nil {
		holders = nil // none known: each is asked of the host
	}
	free, taken := splitPorts(listed, func(p int) bool {
		_, held := holders[p]
		return !held && f.InUse(ctx, p)
	})
	if len(taken) > 0 {
		f.Warn(msg.PortsTaken(backend.CountPorts(taken), backend.FormatPortRanges(taken), vm))
	}
	held := heldBy(holders, listed)
	for _, other := range slices.Sorted(maps.Keys(held)) {
		f.Warn(msg.PortsHeld(backend.CountPorts(held[other]), other, backend.FormatPortRanges(held[other]), vm))
	}
	return free
}

// heldBy returns the ports of listed that another VM forwards now, as
// holders has them, by that VM's name.
func heldBy(holders map[int]string, listed []backend.PortRange) map[string][]backend.PortRange {
	out := map[string][]backend.PortRange{}
	for _, other := range holders {
		if _, done := out[other]; done {
			continue
		}
		if _, held := splitPorts(listed, func(p int) bool { return holders[p] == other }); len(held) > 0 {
			out[other] = held
		}
	}
	return out
}

// Held returns the listed ports of the VM, as its state lists them
// (ports.Forwarding's value: forward), that another VM forwards now, by
// that VM's name: the ports the VM waits for, as devvm ports shows them.
func (f Forwards) Held(vm, forward string) (map[string][]backend.PortRange, error) {
	want, err := ports.Forwarding(forward)
	if err != nil || want.All {
		return nil, err
	}
	holders, err := ports.Holders(f.Dirs, vm, f.live)
	if err != nil {
		return nil, err
	}
	return heldBy(holders, want.Ranges), nil
}

// Unforwarded is a running VM's listed ports that its forwarder tried
// and couldn't take, by why, as devvm ports shows them.
type Unforwarded struct {
	InUse   []backend.PortRange // a program's on this machine (ports_taken)
	Limit   []backend.PortRange // past the limit of forwards (ports_limit)
	Refused []backend.PortRange // refused by this machine otherwise, such as below 1024 on Linux (ports_refused)
}

// Unforwarded returns the listed ports of the running VM, as its state
// lists them (forward), that its forwarder tried and couldn't take, as its
// record says, by why, and no other VM forwards: each left until the VM's
// server listens again. It reads the forwarder's own view and binds
// nothing, so it never takes a port the forwarder is opening. A VM whose
// forwarder doesn't run, or whose record its forwarder didn't write (an
// older devvm's forwarder, or a write that failed), or whose record
// doesn't say why (an older devvm's), has none known.
func (f Forwards) Unforwarded(vm, forward string) Unforwarded {
	want, err := ports.Forwarding(forward)
	if err != nil || want.All {
		return Unforwarded{}
	}
	rec, ok := f.recorded(vm)
	if !ok {
		return Unforwarded{}
	}
	holders, err := ports.Holders(f.Dirs, vm, f.live)
	if err != nil {
		return Unforwarded{}
	}
	of := func(failed []int) []backend.PortRange {
		_, out := splitPorts(want.Ranges, func(p int) bool {
			_, held := holders[p]
			return !held && slices.Contains(failed, p)
		})
		return out
	}
	return Unforwarded{InUse: of(rec.failed.inUse), Limit: of(rec.failed.limit), Refused: of(rec.failed.refused)}
}

// Reserved returns the host ports the VM's forwarder never takes
// (ports.Reserved): the ports devvm chose for VMs, and those other
// running VMs forward now or list, as their forwarders' records say.
func (f Forwards) Reserved(vm string) (map[int]bool, error) {
	return ports.Reserved(f.Dirs, vm, f.live)
}

// live returns the host ports the VM's forwarder forwards now, as it
// records them (record), with ok while the VM has a claim on its listed
// ports: while its forwarder runs, or while devvm starts it again (its
// lock held, Start), from the old one's stop to the new one's start, so
// no other VM takes them in between. A record its process didn't write,
// left by one that ended, names none.
func (f Forwards) live(vm string) ([]int, bool) {
	if _, ok := f.proc(vm).PID(); !ok {
		held, err := platform.LockHeld(f.Dirs.VM(vm).Forwarder(".lock"))
		return nil, err == nil && held
	}
	rec, _ := f.recorded(vm)
	return rec.now, true
}

// forwarderRecord is what a VM's running forwarder records (record): the
// ports it forwards now, and those it tried and couldn't take, by why.
type forwarderRecord struct {
	now    []int
	failed failedPorts
}

// recordLines is how many lines a record has: its pid, the ports
// forwarded, and those it couldn't take because they're in use, past its
// limit, and refused. An older devvm's has three, its last any port it
// couldn't take, whatever the reason: read as none, so no port is
// wrongly said to be in use.
const recordLines = 5

// recorded returns the VM's forwarder's record, with ok when its forwarder
// runs and that process wrote it.
func (f Forwards) recorded(vm string) (forwarderRecord, bool) {
	pid, ok := f.proc(vm).PID()
	if !ok {
		return forwarderRecord{}, false
	}
	data, err := os.ReadFile(f.Dirs.VM(vm).Forwarder(".forwarded"))
	if err != nil {
		return forwarderRecord{}, false
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) < 2 || lines[0] != strconv.Itoa(pid) {
		return forwarderRecord{}, false
	}
	rec := forwarderRecord{now: parsePorts(lines[1])}
	if len(lines) >= recordLines {
		rec.failed = failedPorts{inUse: parsePorts(lines[2]), limit: parsePorts(lines[3]), refused: parsePorts(lines[4])}
	}
	return rec, true
}

// parsePorts returns the ports of a comma-separated list, skipping what
// isn't one.
func parsePorts(s string) []int {
	var out []int
	for e := range strings.SplitSeq(s, ",") {
		if n, err := strconv.Atoi(e); err == nil && backend.ValidPort(n) {
			out = append(out, n)
		}
	}
	return out
}

// record records the host ports the VM's forwarder, process pid,
// forwards now, which other VMs' forwarders read (live), and those it
// tried and couldn't take, by why (Unforwarded): its pid, then each list
// of ports, comma-separated, on a line of its own (recordLines), written
// whole. An older devvm reads its third line, those in use, alone.
func (f Forwards) record(vm string, pid int, now []int, failed failedPorts) error {
	lines := []string{strconv.Itoa(pid)}
	for _, ps := range [][]int{now, failed.inUse, failed.limit, failed.refused} {
		es := make([]string, len(ps))
		for i, n := range ps {
			es[i] = strconv.Itoa(n)
		}
		lines = append(lines, strings.Join(es, ","))
	}
	return state.WriteFile(f.Dirs.VM(vm).Forwarder(".forwarded"), []byte(strings.Join(lines, "\n")+"\n"))
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
