package vm

import (
	"context"
	"errors"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/gitprotect"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/vmname"
)

// vmFolders deletes VMs' folders in devvm's data folder: every file a VM
// had when it goes.
type vmFolders struct{ *Env }

// teardown deletes what devvm keeps for a VM its backend no longer has (t):
// deleted by devvm rm, by an interrupted creation, or some other way. It's
// the one place a VM's files go, in order: what runs for it (its proxy,
// its port forwarder, its link session), then what's
// outside its folder (its clone's remote and .git's locks, other VMs'
// links to it and its group, with leaves its backend's own leftovers,
// backend.Forgetter, and the old machine a remake that didn't finish set
// aside), then its folder, every file of its in it (its pinned host keys
// among them), its project folder's creation lock if a killed devvm left
// it (dropCreateLock), and last its ssh config entry. leaves is false after the backend
// deleted the VM itself, which took its leftovers with it.
func (e vmFolders) teardown(ctx context.Context, t *target, leaves bool) error {
	vm := t.f.Name
	e.Proxy.Stop(vm)
	e.Forwarders.Stop(vm)
	e.linkRuns.stop(ctx, vm)
	// Both, for a VM devvm clone was switching: its remote, if its clone
	// made one, and .git's locks, if it locked any.
	e.clones.removeRemote(ctx, t.f.Dir, vm, sectionOf[cloneInVM](t.st, cloneSection).Made.Remote)
	if m := gitprotect.Mode(t.st); m == gitprotect.On || m == gitprotect.Strict {
		e.Protector(ctx, t.f.Dir).Unlock(ctx, t.st)
	}
	if err := e.groups.forget(ctx, vm); err != nil {
		return err
	}
	if f, ok := backend.As[backend.Forgetter](t.b); ok && leaves {
		if err := f.Forget(ctx, vm); err != nil {
			return err
		}
	}
	// The old machine a remake that didn't finish set aside.
	if aside, ok := vmname.Aside(vm); ok && remakePending(t.st) {
		if err := t.b.Delete(ctx, aside); err != nil && !errors.Is(err, backend.ErrNotFound) {
			return err
		}
	}
	if err := state.RemoveFolder(ctx, e.Dirs, vm); err != nil {
		return err
	}
	dropCreateLock(e.Env, vm)
	return e.writeSSHConfig(ctx)
}

// clear deletes what a VM made for this folder before left in its
// folder, which a new VM by that name replaces: its state, its files, and
// what still runs for it (its proxy, its port forwarder, its link
// session).
func (e vmFolders) clear(ctx context.Context, vm string) error {
	e.Proxy.Stop(vm)
	e.Forwarders.Stop(vm)
	e.linkRuns.stop(ctx, vm)
	return state.RemoveFolder(ctx, e.Dirs, vm)
}
