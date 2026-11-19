package vm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/project"
	"github.com/devvmhq/devvm/internal/sshagent"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/vmname"
)

// finder finds the VM a command acts on (target): the folder's own, the
// nearest parent folder's, or one named, and refuses one that isn't what
// the command needs. It's the one place a VM's state is read by its
// folder's hash and the parent folders are looked through, and it reads
// the folder devvm runs in only then (dir), so the commands that act on
// no VM's folder run from one that's gone.
type finder struct{ *Env }

// target is the VM the finder finds, and what a command does with that
// VM (target.go): it's read from its backend once, by the finder, and
// again after each start and stop.
type target struct {
	e      *Env // what it runs with
	f      project.Folder
	st     *state.VM       // its state; one that doesn't exist for a new VM
	b      backend.Backend // its backend: the one its state records, else a new VM's
	in     backend.Instance
	exists bool // the backend has it: in is its answer
	// other: it was made for an earlier folder at f.Dir, deleted and made
	// again since (sameFolder).
	other bool
	// moved: the finder moved it here, from the path its folder had
	// (finder.moveHere).
	moved bool
	// sub is the folder devvm runs in when the VM is a parent folder's
	// (f.Dir), and so mounts it at the same path; "" when it's this
	// folder's own.
	sub string
	// repo is, for a parent folder's VM, the nearest folder from sub up,
	// below f.Dir, with a .git of its own; "" when none has one.
	repo string
	// agent is the SSH agent socket devvm's connections to the VM carry
	// (Env.agentSocket): the one it runs with, which a start sets
	// (starter.bringUp); "" for none.
	agent string
}

// need is what a command needs of the VM it acts on. The zero need is
// the folder's VM, which needn't exist, as e.Folder decides which it is.
type need struct {
	// own: the folder's own VM, never a parent folder's (devvm --clone,
	// devvm adopt).
	own bool
	// name: the project VM it names, by its name or its ssh alias, in the
	// folder its state records (devvm rm VM), and never the folder's.
	name string
	has  has
	// folder: one that has (exists, or its remake didn't finish) was made
	// for this folder (vm_other_folder). hasMade implies it.
	folder bool
	// noVM is the refusal of a VM that hasn't what has asks, by the
	// folder: msg.NoVM when nil.
	noVM func(dir string) msg.M
	// move is whether a VM whose folder moved here, found for a folder
	// without one, is moved here (moveHere): after asking, by default.
	move moves
}

// moves is how the finder moves a VM whose folder moved to the folder
// devvm runs in (moveHere).
type moves int

const (
	moveAsk   moves = iota // after asking, [y/N]; where it can't ask it refuses
	moveForce              // without asking (devvm adopt --force)
	moveNone               // never: the folder has no VM then
)

// has is how much of the VM a need asks to be there.
type has int

const (
	hasAny     has = iota // nothing: a new VM's name and backend, if none
	hasVM                 // its backend has it, or its remake didn't finish (its machine may be gone)
	hasMachine            // its backend has it
	// hasMade: as hasVM, made for this folder, its creation finished,
	// and no remake of it pending, which devvm finishes first.
	hasMade
)

// FolderRule is which VM a command on the folder's VM uses when a folder
// it's inside has one: --here and --parent, else the default.
type FolderRule int

const (
	// FolderAuto is the default: the folder's own VM when it has one,
	// else the nearest parent folder's, unless a repo of its own is in
	// between that isn't one of the parent's workspace clone's repos.
	FolderAuto FolderRule = iota
	// FolderHere is --here: the folder's own, never a parent's.
	FolderHere
	// FolderParent is --parent: the nearest parent folder's, across a
	// repo of its own and past the folder's own VM.
	FolderParent
)

// folderUse is what resolveFolder decides a folder uses.
type folderUse int

const (
	// useNone: neither its own VM nor a parent's, so up makes its own and
	// every other command says no_vm.
	useNone   folderUse = iota
	useOwn              // the folder's own VM, which it has
	useParent           // the nearest parent folder's VM
)

// target finds the VM a command acts on, as n says, and refuses one that
// hasn't what n asks.
func (e finder) target(ctx context.Context, n need) (*target, error) {
	var t *target
	var err error
	switch {
	case n.name != "":
		t, err = e.named(ctx, n.name)
	case n.own:
		t, err = e.own(ctx, n.move)
	default:
		t, err = e.folders(ctx, n.move)
	}
	if err != nil {
		return nil, err
	}
	return t, n.check(t)
}

// check refuses t when it hasn't what n asks, in this order: there (else
// noVM), made for this folder, no remake pending, its creation finished.
func (n need) check(t *target) error {
	noVM := n.noVM
	if noVM == nil {
		noVM = msg.NoVM
	}
	pending := remakePending(t.st)
	there := t.exists || pending
	switch {
	case n.has == hasMachine && !t.exists, (n.has == hasVM || n.has == hasMade) && !there:
		return noVM(t.f.Dir)
	case (n.folder || n.has == hasMade) && there && t.other:
		return msg.VMOtherFolder(t.f.Name, t.st.Dir)
	case n.has == hasMade && pending:
		return msg.RemakePending(t.f.Dir, t.f.Name)
	case n.has == hasMade:
		return refuseUnfinished(t.f.Name, t.st)
	}
	return nil
}

// refuseUnfinished refuses the VM vm, whose state is st, when its
// creation didn't finish (vm_unfinished): it would start without its
// network choice. The finder's refusal for a command that needs the VM
// made, and every start's (starter.bringUp); a VM found otherwise (a
// link's target, a remake, a state read again) is refused with it too.
func refuseUnfinished(vm vmname.VM, st *state.VM) error {
	if !st.Finished() {
		return msg.VMUnfinished(vm)
	}
	return nil
}

// dir is the folder devvm runs in, a physical path (e.Dir), read the
// first time it's needed.
func (e finder) dir() (string, error) {
	if e.Dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		if e.Dir, err = filepath.EvalSymlinks(wd); err != nil {
			return "", err
		}
	}
	return e.Dir, nil
}

// folders finds the folder's VM, which needn't exist, with the backend
// that has it or would make it: the folder's own (own) or the nearest
// parent folder's, as resolveFolder decides under e.Folder, a parent's
// said in an info line first. A VM whose folder moved here is its own,
// once moved as mv says.
func (e finder) folders(ctx context.Context, mv moves) (*target, error) {
	t, err := e.own(ctx, mv)
	if err != nil {
		return nil, err
	}
	// A folder with state of its own has a VM, even one its backend has
	// lost: never a parent's in its place, unless --parent.
	use, p, err := resolveFolder(t.f.Dir, t.exists || t.st.Exists(), e.Folder, func() (*target, error) { return e.parentVM(ctx) })
	if err != nil {
		return nil, err
	}
	if use == useParent {
		e.Info(msg.UsingParentVM(p.f.Dir, p.f.Name))
		return p, nil
	}
	return t, nil
}

// resolveFolder decides which VM the folder dir uses under rule: own when
// the folder has its own (its state, or its backend has it), else the VM
// of the nearest parent folder with one, which parent finds (parentVM:
// nil for none, with the repo of its own in between, if any), asked only
// when the rule needs it. With FolderAuto the folder's own wins, and a
// parent's is passed over below a repo of its own; FolderHere never asks
// for a parent's; FolderParent takes the parent's whatever the folder
// has, and refuses a folder that's inside none with a VM
// (no_parent_vm).
func resolveFolder(dir string, own bool, rule FolderRule, parent func() (*target, error)) (folderUse, *target, error) {
	ownUse := useNone
	if own {
		ownUse = useOwn
	}
	if rule == FolderHere || rule == FolderAuto && own {
		return ownUse, nil, nil
	}
	p, err := parent()
	switch {
	case err != nil:
		return useNone, nil, err
	case p == nil && rule == FolderParent:
		return useNone, nil, msg.NoParentVM(dir)
	case p == nil || rule == FolderAuto && p.repo != "":
		return ownUse, nil, nil
	}
	return useParent, p, nil
}

// own finds the folder's own VM, which needn't exist, never a parent
// folder's: the VM whose state records the folder (state), else one
// whose folder moved here, moved here as mv says (moveHere). One that
// exists has the name and the backend its state records, whichever
// backend is asked now. Only a new VM gets a name, fitting its backend's.
func (e finder) own(ctx context.Context, mv moves) (*target, error) {
	dir, err := e.dir()
	if err != nil {
		return nil, err
	}
	var rec recorded
	f, st, err := e.state(dir, &rec)
	if err != nil {
		return nil, err
	}
	moved := false
	if !st.Exists() && mv != moveNone {
		if f, st, err = e.moveHere(ctx, dir, mv, &rec); err != nil {
			return nil, err
		}
		moved = st.Exists()
	}
	if !st.Exists() {
		if f, st, err = e.newName(dir); err != nil {
			return nil, err
		}
	}
	t, err := e.inspect(ctx, f, st)
	if t != nil {
		t.moved = moved
	}
	return t, err
}

// again finds t, the folder's own VM, found before (own), again when its
// state changed since: another devvm made it, finished making it, moved
// it here or deleted it meanwhile. One whose state is as it was is t, as
// it was; one found again is refused as up's target is
// (vm_other_folder). It never moves a VM here itself.
func (e finder) again(ctx context.Context, t *target) (*target, error) {
	_, st, err := e.state(t.f.Dir, new(recorded))
	if err != nil {
		return nil, err
	}
	if !st.Exists() {
		// A new VM's state, as own found it.
		if st, err = state.ReadVM(e.Dirs, t.f.Name); err != nil {
			return nil, err
		}
	}
	if sameCreation(st, t.st) {
		return t, nil
	}
	n, err := e.own(ctx, moveNone)
	if err != nil {
		return nil, err
	}
	return n, need{folder: true}.check(n)
}

// state reads the state of dir's VM and returns the folder with the name
// it records ("" for none): the state in the VM folder its path's hash
// names (project.Hash), when it records dir, else the state that records
// dir (rec: a VM whose folder moved here, or a new VM's whose first name
// another VM's folder moved away with). A VM whose folder moved away from
// dir is no VM of dir's.
func (e finder) state(dir string, rec *recorded) (project.Folder, *state.VM, error) {
	if _, err := project.New(dir, 0); err != nil {
		if bad, ok := errors.AsType[*project.BadPathError](err); ok {
			return project.Folder{}, nil, msg.BadPath(bad.Dir)
		}
		return project.Folder{}, nil, err
	}
	st, err := state.ReadHash(e.Dirs, project.Hash(dir))
	if err == nil && (!st.Exists() || st.Dir != dir) {
		var at *state.VM
		if at, err = rec.at(e.Dirs, dir); at != nil {
			st = at
		} else if err == nil && st.Exists() {
			st = new(state.VM)
		}
	}
	if err == nil {
		err = checkState(st)
	}
	if err != nil {
		return project.Folder{}, nil, err
	}
	return project.Folder{Dir: dir, Name: st.Name()}, st, nil
}

// maxNames is how many names (project.Nth) newName tries for a folder.
const maxNames = 100

// newName is the name a new VM for dir gets, with its state, which
// doesn't exist: fitting the names of the backend it would be made on,
// and the first whose VM folder holds no state (project.Nth), as a VM
// whose folder moved away from dir keeps its own.
func (e finder) newName(dir string) (project.Folder, *state.VM, error) {
	first, err := e.Registry.For(new(state.VM))
	if err != nil {
		return project.Folder{}, nil, err
	}
	nameMax := first.Backend.NameMax()
	for n := range maxNames {
		f, err := project.Nth(dir, nameMax, n)
		if errors.Is(err, project.ErrNoRoom) {
			return project.Folder{}, nil, msg.VMNameNoRoom(first.Backend.Name(), strconv.Itoa(nameMax))
		} else if err != nil {
			return project.Folder{}, nil, err
		}
		h, _ := f.Name.Hash()
		if held, err := state.ReadHash(e.Dirs, h); err != nil || held.Exists() {
			continue // another folder's VM, or a state that can't be read
		}
		st, err := state.ReadVM(e.Dirs, f.Name)
		return f, st, err
	}
	return project.Folder{}, nil, fmt.Errorf("no free VM name for %s", dir)
}

// parentVM finds the VM of the nearest parent folder with state of its own
// (project.Parents, stopping before home and the root) for
// resolveFolder, else nil, with the nearest folder below it with a .git of
// its own (repo), the folder devvm runs in included, but none when that's
// one of a workspace clone's repos (coversRepo). Only folders without
// state are passed over: the search stops at the first with state, which
// gives nil unless its backend has its VM and it was made for that folder.
// A VM in an outer folder can replace a nested folder through its mount,
// and devvm then never skips the nested folder's VM for the outer one's.
func (e finder) parentVM(ctx context.Context) (*target, error) {
	here, err := e.dir()
	if err != nil {
		return nil, err
	}
	repo := ""
	if hasGit(here) {
		repo = here
	}
	var found *project.Folder
	var fst *state.VM
	err = e.parents(here, func(pf project.Folder, pst *state.VM) (bool, error) {
		if !pst.Exists() {
			if repo == "" && hasGit(pf.Dir) {
				repo = pf.Dir
			}
			return true, nil
		}
		found, fst = &pf, pst
		return false, nil
	})
	if err != nil || found == nil || fst == nil || !e.sameFolder(ctx, *found, fst) {
		return nil, err
	}
	p, err := e.inspect(ctx, *found, fst)
	if err != nil || !p.exists {
		return nil, err
	}
	p.sub, p.repo = here, repo
	// A workspace clone has the repo, at the same path, and mounts
	// nothing: its folders are the VM's as its own folder's are.
	if repo != "" && coversRepo(fst, found.Dir, repo) {
		p.repo = ""
	}
	return p, nil
}

// parents calls each with the folders dir is inside whose VM a command
// in dir may use (project.Parents), nearest first, but those no VM can be
// made for, and with each one's state, which needn't exist, until each
// returns false or an error, which it returns.
func (e finder) parents(dir string, each func(project.Folder, *state.VM) (bool, error)) error {
	var rec recorded
	for _, p := range project.Parents(dir, e.Dirs.Home) {
		if _, err := project.New(p, 0); err != nil {
			continue // a folder no VM can be made for
		}
		pf, pst, err := e.state(p, &rec)
		if err != nil {
			return err
		}
		if more, err := each(pf, pst); err != nil || !more {
			return err
		}
	}
	return nil
}

// mountedBy is the VM of the nearest folder dir is inside that mounts
// that folder, and so dir, read-write: one whose state records the
// folder and that was made for it, running or not, as it can write there
// whenever it runs. A --clone VM mounts nothing, so it's passed over, and
// so is a state that can't be read. "" for none.
func (e finder) mountedBy(dir string) vmname.VM {
	var vm vmname.VM
	_ = e.parents(dir, func(pf project.Folder, pst *state.VM) (bool, error) {
		if pst.Exists() && !isClone(pst) && madeFor(pf, pst) {
			vm = pf.Name
			return false, nil
		}
		return true, nil
	})
	return vm
}

// mountsRepo is the VM, other than self, that mounts the folder dir, a
// workspace clone's repo, read-write: dir's own VM, or the VM of a folder
// dir is inside, nearest first, made for that folder and not a --clone VM,
// running or not; "" for none. Code in it can swap the repo's folder, or
// what's in it, under host git, so a workspace clone leaves such a repo
// alone. self is the VM the workspace clone is for, which may still mount
// the folder while devvm clone on makes it again.
func (e finder) mountsRepo(dir string, self vmname.VM) vmname.VM {
	mounts := func(f project.Folder, st *state.VM) bool {
		return st.Exists() && !isClone(st) && madeFor(f, st) && f.Name != self
	}
	if f, st, err := e.state(dir, new(recorded)); err == nil && mounts(f, st) {
		return f.Name
	}
	var vm vmname.VM
	_ = e.parents(dir, func(pf project.Folder, pst *state.VM) (bool, error) {
		if mounts(pf, pst) {
			vm = pf.Name
			return false, nil
		}
		return true, nil
	})
	return vm
}

// warnMounted warns, once a launch has found t, when another VM mounts
// the folder t's VM mounts (mountedBy): code in that VM can change the
// project's files and replace its folder (folderChanged). A --clone VM
// (clone: a new one) mounts nothing, so it gets no warning.
func (e finder) warnMounted(t *target, clone bool) {
	if clone || t.st.Exists() && isClone(t.st) {
		return
	}
	if vm := e.mountedBy(t.f.Dir); vm != "" {
		e.Warn(msg.ProjectInVMFolder(t.f.Dir, vm))
	}
}

// named finds the project VM name names, by its name or its ssh alias,
// in the folder its state records, which may be another path than the
// one its name was made for, once its folder moved. Whether it is that
// folder's VM is sameFolder's: its path and the folder itself.
func (e finder) named(ctx context.Context, name string) (*target, error) {
	inv, err := ReadStates(e.Dirs)
	if err != nil {
		return nil, err
	}
	vm := project.Named(name, inv.Recorded())
	if vm == "" {
		return nil, msg.RmUnknownVM(name)
	}
	st, err := state.ReadVM(e.Dirs, vm)
	if err != nil {
		return nil, err
	}
	return e.inspect(ctx, project.Folder{Dir: st.Dir, Name: vm}, st)
}

// folderVM is the name of the VM the folder dir has of its own (state),
// "" for none: devvm rm VM checks it's the VM it names before it acts
// on the folder's.
func (e finder) folderVM(dir string) (vmname.VM, error) {
	_, st, err := e.state(dir, new(recorded))
	if err != nil {
		return "", err
	}
	return st.Name(), nil
}

// hasGit reports whether dir has a .git of its own: a repo, or a
// worktree's or submodule's .git file.
func hasGit(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil
}

// inspect finds the VM for folder f, whose state is st, on the backend
// its state records, else a new VM's, and reads it from its backend
// (target.refresh). One that exists gets its SSH agent (target.agent):
// a running VM's proxy's relay for a relayed start, the agent an older
// devvm's recorded start forwards, or none for a start devvm didn't
// record (sshagent.ReachOf); a stopped VM's, its choice.
func (e finder) inspect(ctx context.Context, f project.Folder, st *state.VM) (*target, error) {
	choice, err := e.Registry.For(st)
	if err != nil {
		return nil, err
	}
	b := choice.Backend
	if err := backend.Check(ctx, b); err != nil {
		return nil, err
	}
	t := e.newTarget(f, st, b)
	t.other = !e.sameFolder(ctx, f, st)
	if err := t.refresh(ctx); err != nil {
		return nil, err
	}
	if !t.exists {
		return t, nil
	}
	mode := sshagent.Mode(st)
	if t.in.Status == backend.StatusRunning {
		run, err := b.Running(ctx, st.Name())
		if err != nil {
			return nil, err
		}
		// A relayed start's VM gets its relay; only an older devvm's
		// recorded start gets the agent's own socket; one devvm didn't
		// record gets none.
		switch sshagent.ReachOf(st, run.ID) {
		case sshagent.Relay:
			t.agent = e.Dirs.AgentRelay(st.Name())
		case sshagent.Raw:
			t.agent = e.agentSocket(sshagent.Running(st, run.ID))
		case sshagent.None:
		}
		return t, nil
	}
	t.agent = e.agentSocket(mode)
	return t, nil
}

// sameFolder reports whether the VM was made for this folder (madeFor),
// on the volume the state records (Env.keepsVolume), so a clone of the
// disk mounted at the folder's own path isn't taken for it. The folder is
// opened once.
func (e finder) sameFolder(ctx context.Context, f project.Folder, st *state.VM) bool {
	if !st.Exists() {
		return true
	}
	// A folder that can't be opened isn't taken for the VM's: the launch is refused.
	s, ok, _ := madeForOpen(f, st)
	return ok && e.keepsVolume(ctx, st, s)
}

// madeFor reports whether the VM was made for this folder: its path, and
// the folder itself (platform.SameOpenFolder), on whichever device a boot
// numbered its disk. A VM without state has no folder to differ. It looks
// up no volume, so it takes a folder that sameFolder wouldn't for a VM's,
// which errs on the safe side for the warnings and the workspace clones
// that ask which VM mounts a folder.
func madeFor(f project.Folder, st *state.VM) bool {
	if !st.Exists() {
		return true
	}
	if _, ok, err := madeForOpen(f, st); err == nil {
		return ok
	}
	// A folder that can't be opened, as one whose read permission code in
	// a VM that mounts it took away, is judged by its stat, which needs
	// only search permission, so no VM hides from the checks that ask
	// which VM mounts a folder.
	id, err := platform.FolderID(f.Dir)
	return err == nil && st.Dir == f.Dir && platform.SameFolder(st.Dir, st.Folder(), f.Dir, id)
}

// madeForOpen is madeFor for a VM with state, with the folder as it read
// it once, opened with no link on its path (platform.StatFolder), and the
// error when it can't be read so.
func madeForOpen(f project.Folder, st *state.VM) (platform.FolderStat, bool, error) {
	s, err := platform.StatFolder(f.Dir)
	return s, err == nil && st.Dir == f.Dir && platform.SameOpenFolder(st.Dir, st.Folder(), f.Dir, s), err
}
