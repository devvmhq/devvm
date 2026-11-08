// Package ports keeps a VM's ports section of its state: the ports it
// forwards, as the user asked, and the host ports devvm chose for it (its
// ssh's, its proxy's, its desktop's). One allocator chooses every host
// port devvm chooses, under the claims lock (state.VM.Claim), so no two
// VMs, and no VM's forwards, get one.
package ports

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/settings"
	"github.com/devvmhq/devvm/internal/state"
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

// Used returns the host ports VMs have (ssh, proxy, desktop), and the
// ports they list to forward, but self's (a VM's name, or ""): what a new
// one avoids, and what a VM's port forwarder never takes, self being that
// VM.
func Used(d paths.Dirs, self string) (map[int]bool, error) {
	names, err := state.VMNames(d)
	if err != nil {
		return nil, err
	}
	used := map[int]bool{}
	for _, name := range names {
		st, err := state.ReadVM(d, name)
		if err != nil {
			continue // not a VM's state
		}
		p, err := Of(st)
		if err != nil {
			continue
		}
		for _, n := range []int{p.Host.SSH, p.Host.Proxy, p.Host.Desktop} {
			if n != 0 {
				used[n] = true
			}
		}
		if name == self {
			continue
		}
		for n := range p.Forwarded() {
			used[n] = true
		}
	}
	for n := range building(d) {
		used[n] = true
	}
	return used, nil
}

// building returns the ports build VMs hold while they start (Build).
func building(d paths.Dirs) map[int]bool {
	es, _ := os.ReadDir(d.BuildPorts()) // none: nothing builds
	out := map[int]bool{}
	for _, e := range es {
		data, err := os.ReadFile(filepath.Join(d.BuildPorts(), e.Name()))
		if err != nil {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && n > 0 {
			out[n] = true
		}
	}
	return out
}

// Allocator chooses the host ports devvm chooses for VMs.
type Allocator struct {
	Dirs paths.Dirs
	// Listening reports whether a program has a host loopback port
	// (Listening).
	Listening func(ctx context.Context, port int) bool
	// Range is the range new ports come from (platform.ChoosablePorts,
	// which the zero value means too).
	Range platform.PortRange
}

// Which is one of a VM's host ports, as Choose takes it.
type Which func(*Host) *int

// SSH is a VM's ssh port, as Choose takes it.
func SSH(h *Host) *int { return &h.SSH }

// Proxy is a VM's proxy port, as Choose takes it.
func Proxy(h *Host) *int { return &h.Proxy }

// Desktop is a VM's desktop port, as Choose takes it.
func Desktop(h *Host) *int { return &h.Desktop }

// Choose returns the VM's host port which: the one its state records
// while keep says so (given that port and the VM's ports as they are
// now), else a new one, chosen (free) and recorded in st under the
// claims lock, so two devvm runs never choose the same.
func (a Allocator) Choose(ctx context.Context, st *state.VM, which Which, keep func(int, Ports) bool) (int, error) {
	var n int
	err := st.Claim(ctx, func(st *state.VM) error {
		p, err := Of(st)
		if err != nil {
			return err
		}
		if cur := *which(&p.Host); cur != 0 && keep(cur, p) {
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

// SSH returns the host port the VM's ssh is forwarded from, on any
// tier, as its state records it: devvm chooses it,
// rather than the backend at its start, so every VM's port forwarder
// avoids it (Used) before the VM's backend binds it. A stopped VM gets
// a new one when its port is now another program's, on any address, or
// one of its own forwards, which its backend would refuse.
func (a Allocator) SSH(ctx context.Context, st *state.VM, running bool) (int, error) {
	if p, err := Of(st); err == nil && p.Host.SSH != 0 && running {
		return p.Host.SSH, nil
	}
	return a.Choose(ctx, st, SSH, func(port int, p Ports) bool {
		return running || !p.Forwarded()[port] && !a.Listening(ctx, port)
	})
}

// Build returns the host port a build VM (vm: an image's, the shared
// tools' or a conversion's), which has no state, starts with its ssh on,
// and release, which gives it up: chosen as a VM's are (free), under the
// claims lock, and recorded by the VM's name (paths.Dirs.BuildPorts),
// where Used reads it, until release, once its backend has bound the port
// (Listening sees it then) or failed to. This process holds the record's
// file lock until release, or until it ends, killed or not, so a record
// whose lock no one holds is stale (clearStale). It's never was, the port
// the VM's config has from the image it's a clone of, so its start always
// sets the new one and a build's numbered steps are the same every time.
func (a Allocator) Build(ctx context.Context, vm string, was int) (int, func(), error) {
	rec := filepath.Join(a.Dirs.BuildPorts(), vm)
	var n int
	var unlock func()
	err := state.Claims(ctx, a.Dirs, func() error {
		var err error
		if n, err = a.free(ctx, was); err != nil {
			return err
		}
		if err := state.WriteFile(rec, []byte(strconv.Itoa(n)+"\n")); err != nil {
			return err
		}
		// A new file: no other process has it, so the lock is had at once.
		if unlock, err = platform.LockFile(ctx, rec); err != nil {
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
// stale records are gone (clearStale), nor any of also. Its callers hold
// the claims lock.
func (a Allocator) free(ctx context.Context, also ...int) (int, error) {
	clearStale(a.Dirs)
	used, err := Used(a.Dirs, "")
	if err != nil {
		return 0, err
	}
	for _, n := range also {
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
