package vm

import (
	"context"
	"os"
	"path/filepath"
	"sync"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/gitprotect"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/project"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/vmname"
)

// recorded is the VMs' states, read (ReadStates) the first time a lookup
// needs them, and then kept for the rest of that lookup: the finder's
// states by the folder they record (at), and moveHere's candidates.
type recorded struct{ inv *Inventory }

// states are the VMs' states that can be read (Inventory.States).
func (r *recorded) states(d paths.Dirs) ([]*state.VM, error) {
	if r.inv == nil {
		inv, err := ReadStates(d)
		if err != nil {
			return nil, err
		}
		r.inv = inv
	}
	return r.inv.States(), nil
}

// at is the state that records dir as its project folder, the first by
// name; nil for none.
func (r *recorded) at(d paths.Dirs, dir string) (*state.VM, error) {
	sts, err := r.states(d)
	if err != nil {
		return nil, err
	}
	for _, st := range sts {
		if st.Dir == dir {
			return st, nil
		}
	}
	return nil, nil
}

// here is the folder devvm runs in as moveHere matches a VM's against it:
// its path and identity (platform.FolderID), and the two things a match
// needs of it, all read from the folder opened once (hereOf).
type here struct {
	dir, id string
	// unique: its identity is never a deleted folder's
	// (platform.UniqueFolderID), so a match is this folder, not one that
	// got a deleted folder's inode number.
	unique bool
	mine   bool // this user owns it
	// stat is the folder read with no link on its path (platform.StatFolder),
	// and volume its volume, looked up once, when a state could record it
	// (platform.MayHaveMoved); nil for none.
	stat   platform.FolderStat
	volume func() string
}

// hereOf is the folder at dir, a physical path, as moveHere matches it,
// its volume asked of e within volumeWaitMove; false when it can't be read
// from the folder opened with no link on its path (platform.StatFolder).
// Its identity, whether that's its alone, its owner and its volume all come
// from that one open folder, so the identity both matched and recorded
// (finder.move's want) is the folder's whose owner was checked, and a name
// swapped between two reads can't match one folder and record another.
func (e *Env) hereOf(ctx context.Context, dir string) (here, bool) {
	s, err := platform.StatFolder(dir)
	if err != nil {
		return here{}, false
	}
	return here{
		dir: dir, id: s.ID, unique: s.Unique, mine: int(s.Owner) == os.Getuid(), stat: s,
		volume: sync.OnceValue(func() string { return e.volumeOf(ctx, s, volumeWaitMove) }),
	}, true
}

// movedFrom reports whether st is the VM of h's folder, which moved from
// the path st records: the folder itself (its identity, the device
// included, as platform.SameFolder has it at another path), unique and
// this user's, at another path, and nothing left at the old one (away).
// The VM's creation finished, and it mounts its folder: a --clone VM
// keeps its clone at the old path, in the VM, so it never moves. A copy
// of the folder is another folder, so it never matches.
func (h here) movedFrom(st *state.VM, away func(st *state.VM, to string) bool) bool {
	return h.unique && h.mine && st.Dir != "" && st.Dir != h.dir && st.Finished() && !isClone(st) &&
		h.same(st) && away(st, h.dir)
}

// same reports whether h's folder is the one st records: its identity as
// recorded (platform.SameFolder), or, on a disk numbered anew since, its
// volume as recorded with its inode and birth time (platform.MovedFolder),
// whose volume is looked up only when the rest matches.
func (h here) same(st *state.VM) bool {
	if platform.SameFolder(st.Dir, st.Folder(), h.dir, h.id) {
		return true
	}
	return h.volume != nil && platform.SameFolder(h.dir, h.id, h.dir, h.stat.ID) &&
		platform.MayHaveMoved(st.Folder(), st.FolderVolume(), h.stat) &&
		platform.MovedFolder(st.Folder(), st.FolderVolume(), h.stat, h.volume())
}

// awayAs reports whether st's folder is no longer at the path st records,
// as to has it: nothing is there and its folder's disk is (folder, which
// tells whether it's gone: Env.FolderOf), or a symlink there leads to to.
func awayAs(st *state.VM, to string, folder func(*state.VM) FolderState) bool {
	if fi, err := os.Lstat(st.Dir); err == nil {
		if fi.Mode()&os.ModeSymlink == 0 {
			return false
		}
		resolved, err := filepath.EvalSymlinks(st.Dir)
		return err == nil && resolved == to
	}
	return folder(st) == FolderGone
}

// moveHere finds, for dir, a folder without a VM, the VM made for it at
// the path it had before it moved here (here.movedFrom), and moves it
// here as mv says: after asking, [y/N], refusing where it can't ask
// (vm_moved_here); without asking with moveForce. It returns the VM's
// folder and state, or a state that doesn't exist for none, or when the
// user says no, or when more than one VM was made for this folder, which
// it warns of (vms_moved_here). Never on its own: a VM moves only after a
// yes or devvm adopt --force.
func (e finder) moveHere(ctx context.Context, dir string, mv moves, rec *recorded) (project.Folder, *state.VM, error) {
	none := new(state.VM)
	h, ok := e.hereOf(ctx, dir)
	if !ok {
		return project.Folder{}, none, nil
	}
	sts, err := rec.states(e.Dirs)
	if err != nil {
		return project.Folder{}, nil, err
	}
	var found []*state.VM
	for _, st := range sts {
		folder := func(st *state.VM) FolderState { return e.FolderOf(ctx, st) }
		if h.movedFrom(st, func(st *state.VM, to string) bool { return awayAs(st, to, folder) }) {
			found = append(found, st)
		}
	}
	switch len(found) {
	case 0:
		return project.Folder{}, none, nil
	case 1:
	default:
		names := make([]vmname.VM, len(found))
		for i, st := range found {
			names[i] = st.Name()
		}
		e.Warn(msg.VMsMovedHere(vmname.Join(names, ", ")))
		return project.Folder{}, none, nil
	}
	st := found[0]
	vm, was := st.Name(), st.Dir
	if mv == moveAsk {
		yes, err := e.Ask(msg.MoveVM(vm, was).RefusedWith(msg.VMMovedHere(vm, was)))
		if err != nil {
			return project.Folder{}, nil, err
		}
		if !yes {
			return project.Folder{}, none, nil
		}
	}
	if err := e.move(ctx, st, dir, h.id, false); err != nil {
		return project.Folder{}, nil, err
	}
	e.Info(msg.VMAdopted(dir, vm))
	return project.Folder{Dir: dir, Name: vm}, st, nil
}

// adoptVM moves the VM that name names (its name or alias, as devvm rm VM
// finds it) to the folder devvm runs in: devvm adopt VM, for a folder
// copied to another disk, which is another folder (its identity is new),
// so moveHere never matches it. It asks first, [y/N] (adopt_move_vm),
// which force answers, and where it can't ask it refuses
// (adopt_move_refused). It refuses a --clone VM (adopt_clone), one its
// backend lost (rm_vm_lost) or whose creation didn't finish, one whose
// folder is still there (adopt_folder_there), and a folder with a VM of
// its own (adopt_has_vm). A VM of this path is devvm adopt's. The VM's
// .git protection starts again (move, fresh), as its .git is another.
func (e finder) adoptVM(ctx context.Context, name string, force bool) error {
	t, err := e.target(ctx, need{name: name})
	if err != nil {
		return err
	}
	dir, err := e.dir()
	if err != nil {
		return err
	}
	vm, st := t.f.Name, t.st
	switch {
	case st.Dir == dir:
		return e.lifecycle.Adopt(ctx, "", force)
	case isClone(st):
		return msg.AdoptClone(st.Dir, vm)
	case !t.exists:
		return msg.RmVMLost(vm)
	}
	if err := refuseUnfinished(vm, st); err != nil {
		return err
	}
	if e.FolderOf(ctx, st) == FolderThere {
		return msg.AdoptFolderThere(st.Dir, vm)
	}
	if own, err := e.folderVM(dir); err != nil {
		return err
	} else if own != "" {
		return msg.AdoptHasVM(own)
	}
	id, err := platform.FolderID(dir)
	if err != nil {
		return err
	}
	if !force {
		yes, err := e.Ask(msg.AdoptMoveVM(vm, st.Dir).RefusedWith(msg.AdoptMoveRefused(vm)))
		if err != nil || !yes {
			return err
		}
	}
	if err := e.move(ctx, st, dir, id, true); err != nil {
		return err
	}
	e.Info(msg.VMAdopted(dir, vm))
	return nil
}

// move makes dir st's project folder: its path and identity, recorded in
// its state (VM.Update), from which its next start mounts it (the
// backend's Start, projectMount). It holds the creation locks of st's VM
// and of the VM dir would get new (newName), so no devvm deletes st's VM
// or makes one for dir meanwhile (create_running), and refuses when
// another VM records dir by then, or st's folder changed since it was
// read. A folder no new VM may have (checkUnsafe: the home folder, one
// inside devvm's own) is refused, and so is a folder at dir that isn't
// want, the one asked about (move_folder_changed). fresh starts .git's
// protection again, as a new VM's (devvm adopt VM: a copy of the folder,
// whose .git is another); a folder that moved keeps its .git, its locks
// and what they were checked against.
func (e finder) move(ctx context.Context, st *state.VM, dir, want string, fresh bool) error {
	// Where no new VM may be made, as the VM gets the folder read-write.
	if err := e.launcher.checkUnsafe(dir, false); err != nil {
		return err
	}
	f, _, err := e.newName(dir)
	if err != nil {
		return err
	}
	release, err := holdCreate(e.Env, st.Name())
	if err != nil {
		return err
	}
	defer release()
	if f.Name != st.Name() {
		releaseNew, err := holdCreate(e.Env, f.Name)
		if err != nil {
			return err
		}
		defer releaseNew()
	}
	if at, err := new(recorded).at(e.Dirs, dir); err != nil {
		return err
	} else if at != nil {
		return msg.CreateRunning(at.Name())
	}
	if id, err := platform.FolderID(dir); err != nil {
		return err
	} else if id != want { // both read in this run, which no boot renumbered
		return msg.MoveFolderChanged(dir, st.Name())
	}
	if err := e.stopForMove(ctx, st); err != nil {
		return err
	}
	return recordMove(ctx, st, dir, want, e.recordedVolume(ctx, dir, want, volumeWaitMove), fresh)
}

// stopForMove stops st's VM when it runs, as devvm stop does, with its
// proxy, port forwarder and link session: a running VM mounts its folder
// where it was, and its next start mounts it where it is now.
func (e finder) stopForMove(ctx context.Context, st *state.VM) error {
	t, err := e.inspect(ctx, project.Folder{Dir: st.Dir, Name: st.Name()}, st)
	if err != nil || !t.exists || t.in.Status != backend.StatusRunning {
		return err
	}
	vm := st.Name()
	if err := t.stop(ctx); err != nil {
		return err
	}
	e.Info(msg.Stopped(vm))
	e.Proxy.Stop(vm)
	e.Forwarders.Stop(vm)
	e.linkRuns.stop(ctx, vm)
	return e.writeSSHConfig(ctx)
}

// recordMove records dir, whose identity is id, on the volume vol
// (platform.VolumeOf, "" for none), as st's project folder, under its
// lock, refusing a state whose folder changed since it was read
// (create_running), and the path the agents' state is under (movedTo),
// which the VM's next setup moves to dir; fresh clears .git's protection
// record.
func recordMove(ctx context.Context, st *state.VM, dir, id, vol string, fresh bool) error {
	was, wasID := st.Dir, st.Folder()
	return st.Update(ctx, func(st *state.VM) error {
		if st.Dir != was || st.Folder() != wasID {
			return msg.CreateRunning(st.Name())
		}
		st.Dir = dir
		st.SetFolder(id, vol)
		if err := edit(st, movedSection, func(m *movedInVM) error { *m = movedTo(*m, was, dir); return nil }); err != nil {
			return err
		}
		if !fresh {
			return nil
		}
		g, err := gitprotect.Of(st)
		if err != nil {
			return err
		}
		g.Made, g.Watched = gitprotect.Made{}, gitprotect.Watched{}
		return gitprotect.Set(st, g)
	})
}
