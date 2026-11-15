package vm

import (
	"fmt"
	"os"
	"sync"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/vmname"
)

// holdCreate takes the creation lock of vm's project folder
// (paths.Dirs.CreateLock), by the hash vm's name ends with, and refuses
// while another devvm holds it (create_running), at once: it never waits
// on that devvm's questions. Up holds it from when it finds the folder has
// no VM, or one whose creation didn't finish, until the new VM's creation
// is recorded (creation.created); devvm rm holds it while it deletes a VM
// (holdForRm), and devvm gc --orphans while it forgets one
// (Leftovers.Forget). release deletes the lock file, then lets
// go of it, so no file of the VM's stays outside its folder; it may be
// called more than once.
func holdCreate(e *Env, vm vmname.VM) (release func(), err error) {
	path, err := createLock(e, vm)
	if err != nil {
		return nil, err
	}
	if err := state.PrivateDir(e.Dirs.Run()); err != nil {
		return nil, err
	}
	unlock, ok, err := platform.TryLockFile(path)
	switch {
	case err != nil:
		return nil, err
	case !ok:
		return nil, msg.CreateRunning(vm)
	}
	return sync.OnceFunc(func() {
		_ = os.Remove(path)
		unlock()
	}), nil
}

// createLock is the creation lock of vm's project folder, by the hash
// vm's name ends with.
func createLock(e *Env, vm vmname.VM) (string, error) {
	h, ok := vm.Hash()
	if !ok {
		return "", fmt.Errorf("invalid VM name %q", vm)
	}
	return e.Dirs.CreateLock(h), nil
}

// dropCreateLock deletes the creation lock of vm's project folder that a
// devvm killed while it held it left, for the VM's teardown; one a devvm
// holds stays, and its holder deletes it.
func dropCreateLock(e *Env, vm vmname.VM) {
	path, err := createLock(e, vm)
	if err != nil {
		return
	}
	if _, err := os.Lstat(path); err != nil {
		return
	}
	if unlock, ok, err := platform.TryLockFile(path); err == nil && ok {
		_ = os.Remove(path)
		unlock()
	}
}

// holdForRm is holdCreate for t, for devvm rm: a VM whose creation
// didn't finish may be one another devvm is making, and one whose
// machine the backend lost may be being made again. Under the lock it
// reads t's state again: one that changed since t was found was made
// meanwhile, and is refused (create_running) as one still being made is.
func holdForRm(e *Env, t *target) (release func(), err error) {
	if release, err = holdCreate(e, t.f.Name); err != nil {
		return nil, err
	}
	st, err := state.ReadVM(e.Dirs, t.f.Name)
	switch {
	case err != nil:
		release()
		return nil, err
	case !sameCreation(st, t.st):
		release()
		return nil, msg.CreateRunning(t.f.Name)
	}
	return release, nil
}

// sameCreation reports whether a and b, a folder's VM's state read twice,
// are the one creation at the one point: made (Create) as the one VM, and
// both recorded as made (created), or neither.
func sameCreation(a, b *state.VM) bool {
	return a.Exists() == b.Exists() && a.ID == b.ID && a.Created.Equal(b.Created)
}
