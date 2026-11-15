package vm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/project"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/vmname"
)

// Leftovers are what devvm keeps for VMs their backend no longer has, a
// VM deleted outside devvm (limactl delete): devvm
// gc --orphans.
type Leftovers struct{ *Env }

// leftoverRecent is how old a VM's state must be before it can be a
// leftover: a VM being made has its state before its backend has it.
const leftoverRecent = time.Hour

// leftoverAsk bounds what each backend is asked, so one that hangs makes
// its VMs unknown.
const leftoverAsk = 30 * time.Second

// Orphan is a VM whose state devvm keeps but whose backend, asked, no
// longer has it.
type Orphan struct {
	Name    vmname.VM
	Backend string
	Dir     string    // its project folder
	Changed time.Time // when its state last changed
}

// LeftoverReport is what Find found.
type LeftoverReport struct {
	Orphans []Orphan
	// Unknown are the backends that couldn't be asked (not installed, not
	// running, too slow, or answering what devvm can't read): their VMs
	// are never orphans.
	Unknown []string
	// Elsewhere are the VMs made where their backend doesn't look in this
	// run (another Lima home or HOME), and Recent those
	// whose state changed in the last hour: never orphans.
	Elsewhere, Recent []vmname.VM
	// Links are clients' links to VMs that have no state, and Sockets the
	// link sessions' sockets nothing listens on (LeftoverSockets).
	Links   []DeadLink
	Sockets []string
}

// Find finds the VMs whose state names a backend that answers and
// doesn't have them, made where that backend looks in this run, whose
// state is over an hour old. A VM whose state can't be read is never one.
// It changes nothing.
func (e Leftovers) Find(ctx context.Context) (LeftoverReport, error) {
	inv, err := ReadInventory(ctx, e.Dirs, e.Registry, UsedBackends)
	if err != nil {
		return LeftoverReport{}, err
	}
	return e.FindIn(ctx, inv)
}

// FindIn is Find by an inventory already read, which must have asked the
// backends some VM's state names: the one definition of an orphan, which
// devvm gc --orphans forgets, retention's orphans.age too, and devvm space
// counts as leftover state. A VM its backend has, its project folder gone
// or not (FolderGone), is never one.
func (e Leftovers) FindIn(ctx context.Context, inv *Inventory) (LeftoverReport, error) {
	var r LeftoverReport
	usable := map[string]bool{} // by backend: it listed, and its check passes
	for _, k := range inv.VMs {
		st := k.State
		if k.Err != nil || !st.Exists() || k.Listed {
			continue
		}
		n, bname, b := k.Name, st.Backend, k.Backend
		ok, checked := usable[bname]
		if !checked {
			ok = b != nil && inv.Asked(bname) && checks(ctx, b)
			usable[bname] = ok
		}
		if !ok {
			r.unknown(bname)
			continue
		}
		// A VM being made again may have no machine by its name: the
		// next devvm in its folder finishes it.
		if remakePending(st) {
			continue
		}
		switch gone, err := gone(ctx, b, n); {
		case err != nil:
			r.unknown(bname)
			continue
		case !gone:
			continue
		}
		if !madeHere(st, b) {
			r.Elsewhere = append(r.Elsewhere, n)
			continue
		}
		fi, err := os.Stat(e.Dirs.VM(n).State())
		if err != nil {
			return r, err
		}
		if time.Since(fi.ModTime()) < leftoverRecent {
			r.Recent = append(r.Recent, n)
			continue
		}
		r.Orphans = append(r.Orphans, Orphan{Name: n, Backend: bname, Dir: st.Dir, Changed: fi.ModTime()})
	}
	var err error
	if r.Links, err = deadLinks(e.Dirs); err != nil {
		return r, err
	}
	r.Sockets, err = LeftoverSockets(ctx, e.Dirs)
	return r, err
}

// ForgetLinks removes every client's links to VMs that have no state, as
// devvm rm of the VM would have, naming the clients that lose them, and
// deletes the link sessions' sockets nothing listens on. Both are found
// again first.
func (e Leftovers) ForgetLinks(ctx context.Context) error {
	dead, err := deadLinks(e.Dirs)
	if err != nil {
		return err
	}
	var targets []vmname.VM
	for _, l := range dead {
		if !slices.Contains(targets, l.Target) {
			targets = append(targets, l.Target)
		}
	}
	for _, t := range targets {
		if err := e.linkRuns.unlinkTarget(ctx, t); err != nil {
			return err
		}
	}
	socks, err := LeftoverSockets(ctx, e.Dirs)
	if err != nil {
		return err
	}
	for _, s := range socks {
		if err := RemoveLeftoverSocket(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

func (r *LeftoverReport) unknown(b string) {
	for _, u := range r.Unknown {
		if u == b {
			return
		}
	}
	r.Unknown = append(r.Unknown, b)
}

// checks reports whether b's own check passes (backend.Check), within
// leftoverAsk: its list alone doesn't say it can be trusted with what it
// lacks.
func checks(ctx context.Context, b backend.Backend) bool {
	ctx, cancel := context.WithTimeout(ctx, leftoverAsk)
	defer cancel()
	return backend.Check(ctx, b) == nil
}

// gone asks b for vm itself: true only when b says it has no such VM.
func gone(ctx context.Context, b backend.Backend, vm vmname.VM) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, leftoverAsk)
	defer cancel()
	_, err := b.Inspect(ctx, vm)
	if err == nil {
		return false, nil
	}
	if errors.Is(err, backend.ErrNotFound) {
		return true, nil
	}
	return false, err
}

// madeHere reports whether a VM was made where its backend looks in this
// run: where its state records, or, for a VM that records none, when
// the backend looks in its default place.
func madeHere(st *state.VM, b backend.Backend) bool {
	cur, isDefault := backend.LocationOf(b)
	if st.BackendAt != "" {
		return st.BackendAt == cur
	}
	return isDefault
}

// Forget deletes what devvm keeps for an orphan, as devvm rm does after
// deleting a VM (teardown), after asking its backend once more that it
// has no such VM, its backend's own leftovers (backend.Forgetter) with
// it. It holds the creation lock of the VM's folder (holdCreate)
// throughout: a VM a devvm is making again has its state before its
// backend has it, however old the state was, and is refused
// (create_running).
func (e Leftovers) Forget(ctx context.Context, o Orphan) error {
	release, err := holdCreate(e.Env, o.Name)
	if err != nil {
		return err
	}
	defer release()
	st, err := state.ReadVM(e.Dirs, o.Name)
	if err != nil {
		return err
	}
	b, ok := e.Registry.Lookup(st.Backend)
	if !st.Exists() || !ok || b.Name() != o.Backend {
		return fmt.Errorf("%s: its state changed", o.Name)
	}
	if isGone, err := gone(ctx, b, o.Name); err != nil {
		return err
	} else if !isGone {
		return fmt.Errorf("%s: its backend has it", o.Name)
	}
	return e.vmFolders.teardown(ctx, e.newTarget(project.Folder{Name: o.Name, Dir: st.Dir}, st, b), true)
}
