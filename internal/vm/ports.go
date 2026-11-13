package vm

import (
	"context"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/devcontainer"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/ports"
	"github.com/devvmhq/devvm/internal/settings"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/vmssh"
)

// ParsePortList checks a list of ports and ranges, never all or none, as
// devvm ports add and rm and devvm link publish and unpublish take it.
func ParsePortList(list string) (string, error) {
	l, err := settings.ParsePorts(list)
	if err != nil {
		return "", err
	}
	if l == settings.PortsAll || l == settings.PortsNone {
		return "", msg.PortsListNeeded(strconv.Quote(list))
	}
	return l, nil
}

// AddPorts returns a parsed ports value with list's ports and ranges
// added: all stays all.
func AddPorts(old, list string) (string, error) {
	l, err := ParsePortList(list)
	switch {
	case err != nil:
		return "", err
	case old == settings.PortsAll:
		return settings.PortsAll, nil
	case old == settings.PortsNone:
		return l, nil
	}
	return settings.ParsePorts(old + "," + l)
}

// RemovePorts returns a parsed ports value without list's ports and
// ranges, each as listed in it: none once nothing is left.
func RemovePorts(old, list string) (string, error) {
	if old == settings.PortsAll {
		return "", msg.PortsRmAll()
	}
	l, err := ParsePortList(list)
	if err != nil {
		return "", err
	}
	if old, err = settings.ParsePorts(old); err != nil {
		return "", err
	}
	return dropPorts(old, l, msg.PortNotListed)
}

// filePorts returns cur, a VM's ports value, with a devcontainer.json's
// ports as devvm ports add and rm would change it (AddPorts,
// RemovePorts): was is the file's ports when it was last read, gave
// those of them the file added to the VM, and now its ports list now
// (3000,5173; "" for none). Like the file's parts (devcontainer.Reread),
// the file's ports it no longer lists go, the ones it lists anew are
// added, and the user's stay: one the user removed stays removed, and one
// cur forwards already, all of them included, is the user's. gaveNow is
// what the file then gave the VM. A port that's part of a range of cur's
// stays, as the user's.
func filePorts(cur string, was, gave []string, now string) (string, []string, error) {
	fwd, err := ports.Forwarding(cur)
	if err != nil {
		return "", nil, err
	}
	listed := portList(now)
	var has []string
	for _, p := range slices.Concat(gave, listed) {
		if n, err := strconv.Atoi(p); err == nil && fwd.Has(n) {
			has = append(has, p)
		}
	}
	add, drop, gaveNow := devcontainer.Reread(was, gave, has, listed)
	list := orAll(cur)
	for _, p := range drop {
		if list == settings.PortsAll {
			break
		}
		if l, err := RemovePorts(list, p); err == nil {
			list = l
		}
	}
	if len(add) > 0 {
		if list, err = AddPorts(list, strings.Join(add, ",")); err != nil {
			return "", nil, err
		}
	}
	return list, gaveNow, nil
}

// dropPorts returns a parsed list of ports and ranges, old, without those
// of l, a parsed list, each as listed in old (else notListed): none once
// nothing is left.
func dropPorts(old, l string, notListed func(port, ports string) msg.M) (string, error) {
	es := strings.Split(old, ",")
	for e := range strings.SplitSeq(l, ",") {
		i := slices.Index(es, e)
		if i < 0 {
			return "", notListed(e, old)
		}
		es = slices.Delete(es, i, i+1)
	}
	if len(es) == 0 {
		return settings.PortsNone, nil
	}
	return strings.Join(es, ","), nil
}

// PortsChange is a change devvm ports makes: Op is "" (set to List),
// "add" or "rm".
type PortsChange struct {
	Op, List string
	Global   bool // what new VMs forward instead
}

// apply returns old changed by c, warning through warn when it adds to
// all.
func (c PortsChange) apply(old string, warn func(msg.M)) (string, error) {
	switch c.Op {
	case "add":
		if old == settings.PortsAll {
			warn(msg.PortsAllAlready())
		}
		return AddPorts(old, c.List)
	case "rm":
		return RemovePorts(old, c.List)
	}
	return settings.ParsePorts(c.List)
}

func orAll(ports string) string {
	if ports == "" {
		return settings.PortsAll
	}
	return ports
}

// Ports returns which ports the folder's VM forwards: all, none or a
// list, and those of its listed ports another running VM forwards now,
// which it forwards once that VM gives them up, each warned (ports_held).
// A running VM's listed ports that its forwarder couldn't take are warned
// too, by why: a program on this machine has them (ports_taken), it
// forwards as many as it can already (ports_limit), or this machine
// refused them (ports_refused).
func (e Settings) Ports(ctx context.Context) (PortsShown, error) {
	t, err := e.finder.target(ctx, need{has: hasMade, noVM: msg.NoVMPorts})
	if err != nil {
		return PortsShown{}, err
	}
	fwd := sectionOf[ports.Ports](t.st, ports.Section).Forward
	out := PortsShown{List: orAll(fwd)}
	f := e.forwards()
	un := f.Unforwarded(t.f.Name, fwd)
	if len(un.InUse) > 0 {
		e.Warn(msg.PortsTaken(backend.CountPorts(un.InUse), backend.FormatPortRanges(un.InUse), t.f.Name))
	}
	if len(un.Limit) > 0 {
		e.Warn(msg.PortsLimit(backend.CountPorts(un.Limit), strconv.Itoa(vmssh.MaxForwards), backend.FormatPortRanges(un.Limit), t.f.Name))
	}
	if len(un.Refused) > 0 {
		e.Warn(msg.PortsRefused(backend.CountPorts(un.Refused), backend.FormatPortRanges(un.Refused), t.f.Name))
	}
	// Another VM's state unreadable for now: the list alone.
	held, _ := f.Held(t.f.Name, fwd)
	for _, other := range slices.Sorted(maps.Keys(held)) {
		list := backend.FormatPortRanges(held[other])
		e.Warn(msg.PortsHeld(backend.CountPorts(held[other]), other, list, t.f.Name))
		out.Held = append(out.Held, PortsHeld{VM: other, Ports: list})
	}
	return out, nil
}

// PortsShown is which ports a VM forwards, as devvm ports shows them.
type PortsShown struct {
	List string      // all, none or a list
	Held []PortsHeld // its listed ports other VMs forward now, by VM
}

// PortsHeld is some of a VM's listed ports, which another VM forwards now.
type PortsHeld struct {
	VM    string // the other VM
	Ports string // a list of ports and ranges
}

// PortsDefault returns what new VMs forward, and where that came from.
func (e Settings) PortsDefault() (settings.Value, error) {
	def, err := settings.Read(e.Dirs)
	return def.Ports, err
}

// SetPorts changes which ports the folder's VM forwards: a stopped VM's
// from its next start, a running one's at once, its forwarder started
// again with them while it runs on (vmssh.Forwards.Start). With c.Global, it changes what new
// VMs forward instead, which config.toml mustn't set.
func (e Settings) SetPorts(ctx context.Context, c PortsChange) error {
	if c.Global {
		old, err := e.PortsDefault()
		if err != nil {
			return err
		}
		list, err := c.apply(old.Value, e.Warn)
		if err != nil {
			return err
		}
		if _, err := settings.Save(e.Dirs, settings.Values{Ports: &list}); err != nil {
			return err
		}
		e.Info(msg.PortsDefault(list))
		return nil
	}
	t, err := e.finder.target(ctx, need{has: hasMade, noVM: msg.NoVMPorts})
	if err != nil {
		return err
	}
	vm, st, b := t.f.Name, t.st, t.b
	cur, err := ports.Of(st)
	if err != nil {
		return err
	}
	list, err := c.apply(orAll(cur.Forward), e.Warn)
	if err != nil {
		return err
	}
	p, err := ports.Forwarding(list)
	if err != nil {
		return err
	}
	if err := e.Registry.Require(b, vm, requirements(needs{ports: p})...); err != nil {
		return err
	}
	// Its state, which its forwarder reads: while it runs, other VMs'
	// forwarders give up and never take its listed ports (ports.Reserved),
	// its new ones included, within a recheck, unless they list them too.
	if err := st.Update(ctx, func(st *state.VM) error { return ports.SetForward(st, list) }); err != nil {
		return err
	}
	if t.in.Status != backend.StatusRunning {
		e.Info(msg.PortsNow(list, vm))
		return nil
	}
	e.forwards().Start(ctx, b, st)
	e.Info(msg.PortsForwarding(list, vm))
	return nil
}

// forwards is VMs' port forwarders as devvm runs them (Forwarders): what
// a host program has, and warnings, as the Env has them.
func (e *Env) forwards() vmssh.Forwards {
	f := e.Forwarders
	f.InUse, f.Reporter = e.PortInUse, e.Reporter
	return f
}
