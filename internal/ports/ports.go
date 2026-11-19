// Package ports keeps a VM's ports section of its state: the ports it
// forwards, as the user asked, and the host ports devvm chose for it (its
// ssh's, its proxy's, its desktop's). One allocator chooses every host
// port devvm chooses, under the claims lock (state.VM.Claim), so no two
// VMs, and no VM's forwards, get one, and VMs' ssh ports come only from
// the ports browsers refuse (SSHPorts). Which VM holds a forwarded host
// port is its rule too (Reserved, Holders): the running VM that forwards
// it, never a stopped one.
package ports

import (
	"context"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/settings"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/vmname"
)

// Section is the name of the ports section in a VM's state.
const Section = "ports"

// Ports is a VM's ports section.
type Ports struct {
	// Forward is the ports it forwards to the same host port, as asked
	// (devvm ports, --ports), as settings.ParsePorts returns them: "" for
	// all.
	Forward string `json:"forward,omitempty"`
	// Host is the host ports devvm chose for it.
	Host Host `json:"host,omitzero"`
}

// Host is the host ports devvm chose for a VM: 0 for none yet.
type Host struct {
	SSH     int `json:"ssh,omitempty"`     // its ssh's, which devvm chooses so no forwarder takes it first
	Proxy   int `json:"proxy,omitempty"`   // its proxy's, on the host's loopback
	Desktop int `json:"desktop,omitempty"` // devvm desktop's, on the host's loopback
}

// Of returns a VM's ports section.
func Of(st *state.VM) (Ports, error) { return state.Section[Ports](st, Section) }

// Set sets a VM's ports section, in st alone: inside st.Update.
func Set(st *state.VM, p Ports) error { return state.SetSection(st, Section, p) }

// SetForward sets which ports a VM forwards (Ports.Forward), in st alone.
func SetForward(st *state.VM, forward string) error {
	p, err := Of(st)
	if err != nil {
		return err
	}
	p.Forward = forward
	return Set(st, p)
}

// Forwarding returns which ports a VM forwards, as a ports value
// (Ports.Forward, settings.ParsePorts) says: "" and all are every port,
// none is none. A value that isn't one (a state file edited by hand) is
// port_invalid, so a bad entry never becomes a port the VM didn't ask
// for.
func Forwarding(v string) (backend.Ports, error) {
	switch v {
	case "", settings.PortsAll:
		return backend.Ports{All: true}, nil
	case settings.PortsNone:
		return backend.Ports{}, nil
	}
	rs, err := backend.ParsePortRanges(v)
	if err != nil {
		return backend.Ports{}, msg.PortInvalid(strconv.Quote(v)).Because(err)
	}
	return backend.Ports{Ranges: rs}, nil
}

// Forwarded returns the host ports a VM forwards, as p lists them
// (5173,8000-8010, backend.ParsePortRanges), each entry on its own, so
// one that isn't a port or a range is skipped and the others are still
// avoided; none for all or none, which name no port.
func (p Ports) Forwarded() map[int]bool {
	out := map[int]bool{}
	for e := range strings.SplitSeq(p.Forward, ",") {
		rs, err := backend.ParsePortRanges(e)
		if err != nil {
			continue
		}
		for n := rs[0].From; n <= rs[0].To; n++ {
			out[n] = true
		}
	}
	return out
}

// Used returns the host ports devvm records: every VM's ssh, proxy and
// desktop ports, the ports every VM lists to forward, running or not, and
// the build VMs' ports. A port devvm chooses avoids them all, so it never
// takes a port a stopped VM lists, which that VM's forwarder takes again
// when it starts.
func Used(d paths.Dirs) (map[int]bool, error) {
	vms, err := sections(d)
	if err != nil {
		return nil, err
	}
	used := chosen(d, vms)
	for _, v := range vms {
		for n := range v.p.Forwarded() {
			used[n] = true
		}
	}
	return used, nil
}

// Live returns the host ports a VM's port forwarder forwards now, with ok
// while the VM has a claim on its listed ports: while its forwarder runs,
// or while devvm starts it again. None runs for a stopped VM, nor for one
// that crashed, whose forwarder ended with it, so neither holds a port.
type Live func(vm vmname.VM) (now []int, ok bool)

// Holders returns the VM but self whose forwarder forwards each host port
// now, by port, as live says.
func Holders(d paths.Dirs, self vmname.VM, live Live) (map[int]vmname.VM, error) {
	vms, err := sections(d)
	if err != nil {
		return nil, err
	}
	out := map[int]vmname.VM{}
	for _, v := range vms {
		if v.name == self {
			continue
		}
		now, ok := live(v.name)
		if !ok {
			continue
		}
		for _, n := range now {
			out[n] = v.name
		}
	}
	return out, nil
}

// Reserved returns the host ports VM self's port forwarder never takes:
// every VM's ssh, proxy and desktop ports and the build VMs' ports, the
// ports another VM's forwarder forwards now, and the ports another VM
// with a claim on them (live) lists to forward, unless self lists them
// too. Who holds a port is the running VM that forwards it: of two that
// list it, the first to forward it keeps it, a VM that lists a port takes
// it from one that forwards every port, and a stopped VM holds none, so
// its listed ports wait for its next start. The ports VMs' ssh comes
// from (SSHPorts) are reserved too, unless self lists them: a VM that
// forwards every port never takes one, so its guest can't fill them. A
// port self waits for is forwarded once it's no longer reserved.
func Reserved(d paths.Dirs, self vmname.VM, live Live) (map[int]bool, error) {
	vms, err := sections(d)
	if err != nil {
		return nil, err
	}
	reserved := chosen(d, vms)
	own := map[int]bool{}
	for _, v := range vms {
		if v.name == self {
			own = v.p.Forwarded()
		}
	}
	for _, n := range SSHPorts() {
		if !own[n] {
			reserved[n] = true
		}
	}
	for _, v := range vms {
		if v.name == self {
			continue
		}
		now, ok := live(v.name)
		if !ok {
			continue
		}
		for _, n := range now {
			reserved[n] = true
		}
		for n := range v.p.Forwarded() {
			if !own[n] {
				reserved[n] = true
			}
		}
	}
	return reserved, nil
}

// vmPorts is a VM's name and its ports section.
type vmPorts struct {
	name vmname.VM
	p    Ports
}

// sections returns the ports section of every VM whose state and section
// can be read.
func sections(d paths.Dirs) ([]vmPorts, error) {
	names, err := state.VMNames(d)
	if err != nil {
		return nil, err
	}
	var out []vmPorts
	for _, name := range names {
		st, err := state.ReadVM(d, name)
		if err != nil {
			continue // not a VM's state
		}
		p, err := Of(st)
		if err != nil {
			continue
		}
		out = append(out, vmPorts{name, p})
	}
	return out, nil
}

// chosen returns the host ports devvm chose: vms' ssh, proxy and desktop
// ports, and the build VMs' ports.
func chosen(d paths.Dirs, vms []vmPorts) map[int]bool {
	out := building(d)
	for _, v := range vms {
		for _, n := range []int{v.p.Host.SSH, v.p.Host.Proxy, v.p.Host.Desktop} {
			if n != 0 {
				out[n] = true
			}
		}
	}
	return out
}

// building returns the ports build VMs hold while they start (Build).
func building(d paths.Dirs) map[int]bool {
	es, _ := os.ReadDir(d.BuildPorts()) // none: nothing builds
	out := map[int]bool{}
	for _, e := range es {
		if n := recordedPort(filepath.Join(d.BuildPorts(), e.Name())); n > 0 {
			out[n] = true
		}
	}
	return out
}

// recordedPort is the port a record of one (Build's, hold's) holds: 0 for
// none, or a file that isn't one.
func recordedPort(path string) int {
	data, err := os.ReadFile(path) //nolint:gosec // devvm's own records, in its data folder
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// Allocator chooses the host ports devvm chooses for VMs.
type Allocator struct {
	Dirs paths.Dirs
	// Listening reports whether a program has a host loopback port
	// (Listening).
	Listening func(ctx context.Context, port int) bool
	// Range is the range new ports come from (platform.ChoosablePorts,
	// which the zero value means too, or a test build's own,
	// DEVVM_PORT_RANGE, as the CLI tests' sandboxes run at once).
	Range platform.PortRange
	// SSHList is the ports VMs' ssh comes from in place of SSHPorts: nil
	// for those, or a test build's own (DEVVM_SSH_PORTS), as the CLI
	// tests' sandboxes, which run at once, would share them.
	SSHList []int
}

// SSHPorts are the only host ports a VM's ssh is on: the Fetch standard's
// bad ports from 1024 up (https://fetch.spec.whatwg.org/#port-blocking)
// that Chromium blocks too, which leaves out 4190 and 6679, missing from
// its kRestrictedPorts (net/base/port_util.cc), so every browser engine
// refuses them every request, navigations and WebSockets included. Root
// in a VM can make what answers on its ssh port speak HTTP, so on any
// other port a page opened as localhost:PORT, or as any name under
// localhost, would reach a server the VM controls, with that name's
// cookies, which ignore the port. So at most this many VMs run at once.
func SSHPorts() []int {
	return []int{1719, 1720, 1723, 2049, 3659, 4045, 5060, 5061, 6000, 6566, 6665, 6666, 6667, 6668, 6669, 6697, 10080}
}

// sshPorts is the ports a VM's ssh comes from (SSHList, else SSHPorts).
func (a Allocator) sshPorts() []int {
	if a.SSHList != nil {
		return a.SSHList
	}
	return SSHPorts()
}

// OnList reports whether n is one of the ports a VM's ssh comes from. A
// VM whose recorded ssh port isn't, which an older devvm chose, moves at
// its next start.
func (a Allocator) OnList(n int) bool { return slices.Contains(a.sshPorts(), n) }

// SSHListed returns the ports of SSHPorts that a ports value lists
// (Ports.Forward), and how many: a VM forwards them only when it lists
// them, and one that does keeps them from VMs' ssh while it runs.
func SSHListed(forward string) (list string, count int) {
	listed := Ports{Forward: forward}.Forwarded()
	var out []string
	for _, n := range SSHPorts() {
		if listed[n] {
			out = append(out, strconv.Itoa(n))
		}
	}
	return strings.Join(out, ","), len(out)
}

// SSHOnly reports whether n is a port only VMs' ssh may have: one of
// SSHPorts, or of a test build's own list. A VM's proxy or desktop port
// that is, which an older devvm chose, is chosen again at its next start.
func (a Allocator) SSHOnly(n int) bool { return slices.Contains(SSHPorts(), n) || a.OnList(n) }

// Which is one of a VM's host ports, as Choose takes it.
type Which func(*Host) *int

// Proxy is a VM's proxy port, as Choose takes it.
func Proxy(h *Host) *int { return &h.Proxy }

// Desktop is a VM's desktop port, as Choose takes it.
func Desktop(h *Host) *int { return &h.Desktop }

// Choose returns the VM's host port which: the one its state records
// while keep says so (given that port and the VM's ports as they are
// now) and it isn't one only VMs' ssh may have (SSHOnly), else a new
// one, chosen (free) and recorded in st under the claims lock, so two
// devvm runs never choose the same.
func (a Allocator) Choose(ctx context.Context, st *state.VM, which Which, keep func(int, Ports) bool) (int, error) {
	var n int
	err := st.Claim(ctx, func(st *state.VM) error {
		p, err := Of(st)
		if err != nil {
			return err
		}
		if cur := *which(&p.Host); cur != 0 && !a.SSHOnly(cur) && keep(cur, p) {
			n = cur
			return nil
		}
		if n, err = a.free(ctx); err != nil {
			return err
		}
		*which(&p.Host) = n
		return Set(st, p)
	})
	return n, err
}

// SSH returns the host port the VM's ssh is forwarded from, on any tier,
// as its state records it: one of SSHPorts, which devvm chooses rather
// than the backend at its start, so every VM's port forwarder avoids it
// (Used) before the VM's backend binds it. A running VM keeps its port,
// even one an older devvm chose off the list, until its next start. A
// stopped VM keeps its port while it's on the list, no program has it, on
// any address, no other start or build holds it and the VM doesn't list
// it to forward, which its backend would refuse; else it gets another
// (freeSSH). Either way the port is held for the start (hold), as a
// stopped VM's port it may look like to another start until its backend
// binds it.
func (a Allocator) SSH(ctx context.Context, st *state.VM, running bool) (int, error) {
	if p, err := Of(st); err == nil && p.Host.SSH != 0 && running {
		return p.Host.SSH, nil
	}
	var n int
	err := st.Claim(ctx, func(st *state.VM) error {
		p, err := Of(st)
		if err != nil {
			return err
		}
		t, err := a.sshTaken(ctx, st.Name())
		if err != nil {
			return err
		}
		if cur := p.Host.SSH; a.OnList(cur) && !t.out[cur] && !a.Listening(ctx, cur) {
			n = cur
			return a.hold(st.Name(), n)
		}
		if n, err = a.freeSSH(ctx, t); err != nil {
			return err
		}
		if err := a.hold(st.Name(), n); err != nil {
			return err
		}
		p.Host.SSH = n
		return Set(st, p)
	})
	return n, err
}

// projectHold is how long a project VM's start holds its ssh port (SSH):
// its backend binds the port as the start begins, after which Listening
// sees it.
const projectHold = time.Minute

// sshHold is the file of the VM's ports a start's hold is in
// (paths.VM.Forwarder): run/ports.ssh-hold.
const sshHold = ".ssh-hold"

// hold records that VM vm's start holds port, its ssh port, kept or new,
// for projectHold, in the VM's own run folder (sshHold), where
// the next choice reads it (sshTaken), so no other start or build takes
// it before the VM's backend binds it. It goes with the VM's folder.
func (a Allocator) hold(vm vmname.VM, port int) error {
	if err := state.PrivateDir(a.Dirs.VM(vm).Run()); err != nil {
		return err
	}
	return state.WriteFile(a.Dirs.VM(vm).Forwarder(sshHold), []byte(strconv.Itoa(port)+"\n"))
}

// held is the port VM vm's start holds (hold): 0 once it's projectHold
// old, or for none.
func (a Allocator) held(vm vmname.VM) int {
	path := a.Dirs.VM(vm).Forwarder(sshHold)
	fi, err := os.Stat(path)
	if err != nil || time.Since(fi.ModTime()) >= projectHold {
		return 0
	}
	return recordedPort(path)
}

// sshTaken is what freeSSH avoids: the ssh ports VMs' states record
// (recorded), which a stopped VM gives up when none other is free, and
// the ports no VM's ssh may take (out): those another VM's start or a
// build holds, those self lists to forward, which its backend would
// refuse, and those another VM that runs or starts lists.
type sshTaken struct{ recorded, out map[int]bool }

// sshTaken returns what freeSSH avoids for VM self ("" for a build VM),
// once the build ports' stale records are gone (clearStale). A stopped
// VM's listed ports aren't avoided, as a stopped VM holds no port (Reserved):
// its forwarder finds such a port another VM's ssh at its next start. A
// VM runs, here, when a program has its recorded ssh port. Its callers
// hold the claims lock.
func (a Allocator) sshTaken(ctx context.Context, self vmname.VM) (sshTaken, error) {
	clearStale(a.Dirs)
	vms, err := sections(a.Dirs)
	if err != nil {
		return sshTaken{}, err
	}
	t := sshTaken{recorded: map[int]bool{}, out: building(a.Dirs)}
	for _, v := range vms {
		listed := v.p.Forwarded()
		if v.name == self {
			maps.Copy(t.out, listed)
			continue
		}
		if v.p.Host.SSH != 0 {
			t.recorded[v.p.Host.SSH] = true
		}
		n := a.held(v.name)
		if n != 0 {
			t.out[n] = true
		}
		if len(listed) > 0 && (n != 0 || v.p.Host.SSH != 0 && a.Listening(ctx, v.p.Host.SSH)) {
			maps.Copy(t.out, listed)
		}
	}
	return t, nil
}

// freeSSH returns one of the ports a VM's ssh comes from (sshPorts) that
// no program has (Listening) and isn't out: one no VM's state records,
// else one only a stopped VM's does, which that VM gives up, getting
// another at its next start. With none, it fails, naming the VMs that run
// on them (sshLimit). Its callers hold the claims lock.
func (a Allocator) freeSSH(ctx context.Context, t sshTaken) (int, error) {
	var stopped []int
	for _, n := range a.sshPorts() {
		if t.out[n] || a.Listening(ctx, n) {
			continue
		}
		if !t.recorded[n] {
			return n, nil
		}
		stopped = append(stopped, n)
	}
	if len(stopped) > 0 {
		return stopped[0], nil
	}
	return 0, a.sshLimit(ctx)
}

// sshLimit is the error for a start or a build that finds every port a
// VM's ssh comes from taken: ports_ssh_limit, naming the VMs whose
// recorded ssh port a program has (running), and how many of the ports
// they have and how many are taken otherwise; ports_ssh_limit_all when
// they have them all, and ports_ssh_limit_others when no VM's are.
func (a Allocator) sshLimit(ctx context.Context) error {
	held := a.running(ctx)
	limit := len(a.sshPorts())
	switch {
	case len(held) == 0:
		return msg.PortsSSHLimitOthers(strconv.Itoa(limit))
	case len(held) >= limit:
		return msg.PortsSSHLimitAll(strconv.Itoa(limit), strings.Join(held, ", "))
	}
	return msg.PortsSSHLimit(strconv.Itoa(len(held)), strconv.Itoa(limit), strconv.Itoa(limit-len(held)), strings.Join(held, ", "))
}

// running returns, for each port a VM's ssh comes from that a program
// has, the VMs whose states record it: those that run on it, by name. A
// port two states record, a stopped VM's that another took from it, names
// both, as "a or b", as Listening can't tell which runs.
func (a Allocator) running(ctx context.Context) []string {
	vms, _ := sections(a.Dirs) // none read: none named
	byPort := map[int][]string{}
	var order []int
	for _, v := range vms {
		p := v.p.Host.SSH
		if !a.OnList(p) {
			continue
		}
		if _, seen := byPort[p]; !seen {
			if !a.Listening(ctx, p) {
				continue
			}
			order = append(order, p)
		}
		byPort[p] = append(byPort[p], v.name.String())
	}
	out := make([]string, 0, len(order))
	for _, p := range order {
		out = append(out, strings.Join(byPort[p], " or "))
	}
	return out
}

// OffList returns the VMs whose recorded ssh port isn't one a VM's ssh
// comes from (OnList), which an older devvm chose, and that a program
// has: VMs that still run on it, until they start again.
func (a Allocator) OffList(ctx context.Context) []vmname.VM {
	vms, _ := sections(a.Dirs) // none read: none named
	var out []vmname.VM
	for _, v := range vms {
		if p := v.p.Host.SSH; p != 0 && !a.OnList(p) && a.Listening(ctx, p) {
			out = append(out, v.name)
		}
	}
	return out
}

// Build returns the host port a build VM (vm: an image's, the shared
// tools' or a conversion's), which has no state, starts with its ssh on,
// and release, which gives it up: one of the ports a VM's ssh comes from,
// chosen as a project VM's is (freeSSH), under the claims lock, and
// recorded by the VM's name (paths.Dirs.BuildPorts), where Used reads it,
// until release, once its backend has bound the port (Listening sees it
// then) or failed to. This process holds the record's file lock until
// release, or until it ends, killed or not, so a record whose lock no one
// holds is stale (clearStale). It's was, the port the VM's config has
// from the image it's a clone of, only when no other is free, so its start
// almost always sets a new one and a build's numbered steps are the same
// every time.
func (a Allocator) Build(ctx context.Context, vm vmname.VM, was int) (int, func(), error) {
	rec := filepath.Join(a.Dirs.BuildPorts(), vm.String())
	var n int
	var unlock func()
	err := state.Claims(ctx, a.Dirs, func() error {
		t, err := a.sshTaken(ctx, "")
		if err != nil {
			return err
		}
		other := maps.Clone(t.out)
		other[was] = true
		if n, err = a.freeSSH(ctx, sshTaken{recorded: t.recorded, out: other}); err != nil {
			if n, err = a.freeSSH(ctx, t); err != nil {
				return err
			}
		}
		if err := state.WriteFile(rec, []byte(strconv.Itoa(n)+"\n")); err != nil {
			return err
		}
		// A new file: no other process has it, so the lock is had at once.
		if unlock, err = platform.LockFile(ctx, rec, nil); err != nil {
			_ = os.Remove(rec)
		}
		return err
	})
	if err != nil {
		return 0, nil, err
	}
	return n, func() { _ = os.Remove(rec); unlock() }, nil
}

// staleAfter is how long a build port's record stays once no process
// holds its lock: longer than Lima's start waits for a VM's ssh (10
// minutes), as the backend's start, which a killed devvm leaves running,
// may still bind the port until then.
const staleAfter = 15 * time.Minute

// clearStale removes the build ports' records whose file lock no process
// holds (Build), written over staleAfter ago: left by a devvm that ended
// before it gave its port up. It runs under the claims lock, which Build
// holds from writing a record to locking it, so a record it finds
// unlocked is never one being made.
func clearStale(d paths.Dirs) {
	es, _ := os.ReadDir(d.BuildPorts()) // none: nothing builds
	for _, e := range es {
		rec := filepath.Join(d.BuildPorts(), e.Name())
		fi, err := e.Info()
		if err != nil || time.Since(fi.ModTime()) < staleAfter {
			continue
		}
		if held, err := platform.LockHeld(rec); err == nil && !held {
			_ = os.Remove(rec)
		}
	}
}

// free returns a loopback port of Range (below the host's ephemeral
// range) no program has (Listening) and no VM has, once the build ports'
// stale records are gone (clearStale), nor any of also, nor one of the
// ports VMs' ssh comes from, which are theirs alone. Its callers hold the
// claims lock.
func (a Allocator) free(ctx context.Context, also ...int) (int, error) {
	clearStale(a.Dirs)
	used, err := Used(a.Dirs)
	if err != nil {
		return 0, err
	}
	for _, n := range slices.Concat(also, SSHPorts(), a.sshPorts()) {
		used[n] = true
	}
	r := a.Range
	if r == (platform.PortRange{}) {
		r = platform.ChoosablePorts()
	}
	return platform.FreePort(ctx, r, func(ctx context.Context, port int) bool {
		return !used[port] && !a.Listening(ctx, port)
	})
}

// Listening reports whether a program has a host loopback port: one that
// has it on any address (platform.PortInUse), even if it never accepts,
// or one that accepts connections on it.
func Listening(ctx context.Context, port int) bool {
	if platform.PortInUse(ctx, port) {
		return true
	}
	d := net.Dialer{Timeout: time.Second}
	c, err := d.DialContext(ctx, "tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}
