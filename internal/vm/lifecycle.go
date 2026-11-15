package vm

import (
	"cmp"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/gitprotect"
	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/netpolicy"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/tools"
	"github.com/devvmhq/devvm/internal/vmname"
)

// Lifecycle stops the folder's VM, gives it to the folder, exports its
// agents' history, makes it again (remake.go) and deletes it.
type Lifecycle struct{ *Env }

// Stop stops the folder's VM, its proxy and its link session. Then, the
// VM being stopped, the .git protection chosen while it ran is set, .git
// is locked again and checked (on Linux, set-id files too) as on a
// launch that finds it stopped (starter.hostChecks), and its ssh config entry goes.
func (e Lifecycle) Stop(ctx context.Context) error {
	t, err := e.finder.target(ctx, need{has: hasMade})
	if err != nil {
		return err
	}
	vm := t.f.Name
	if t.in.Status == backend.StatusStopped {
		e.Info(msg.AlreadyStopped(vm))
	} else {
		// Pins it ran without, before they're gone with it.
		e.starter.checkRunningPins(ctx, t)
		if err := t.stop(ctx); err != nil {
			return err
		}
		e.Info(msg.Stopped(vm))
	}
	e.Proxy.Stop(vm)
	e.Forwarders.Stop(vm)
	e.linkRuns.stop(ctx, vm)
	if err := e.starter.hostChecks(ctx, t, false, false); err != nil {
		return err
	}
	return e.writeSSHConfig(ctx)
}

// Adopt gives the folder the VM made for an earlier folder at its path,
// after asking, or with force (--force): its files, tokens
// and allowlist. Its .git is locked as new at its next launch. Only the
// VM of this path is adopted, never a parent folder's. A VM whose folder
// moved here is moved here (finder.moveHere), --force answering its
// question too. With name, it's that VM (finder.adoptVM).
func (e Lifecycle) Adopt(ctx context.Context, name string, force bool) error {
	if name != "" {
		return e.finder.adoptVM(ctx, name, force)
	}
	mv := moveAsk
	if force {
		mv = moveForce
	}
	t, err := e.finder.target(ctx, need{own: true, has: hasMachine, move: mv})
	if err != nil || t.moved {
		return err
	}
	vm, st := t.f.Name, t.st
	if !t.other {
		e.Info(msg.VMAlreadyHere(vm))
		return nil
	}
	was := st.Dir
	if !force {
		yes, err := e.Ask(msg.AdoptVM(vm, was).RefusedWith(msg.VMOtherFolderAdopt(vm, was)))
		if err != nil || !yes {
			return err
		}
	}
	id, err := platform.FolderID(t.f.Dir)
	if err != nil {
		return err
	}
	// Its folder, and .git's protection set up as a new VM's at its next
	// launch, at once.
	err = st.Update(ctx, func(st *state.VM) error {
		st.Dir, st.Folder = t.f.Dir, id
		g, err := gitprotect.Of(st)
		if err != nil {
			return err
		}
		g.Made, g.Watched = gitprotect.Made{}, gitprotect.Watched{}
		return gitprotect.Set(st, g)
	})
	if err != nil {
		return err
	}
	e.Info(msg.VMAdopted(t.f.Dir, vm))
	return nil
}

// RmOptions are devvm rm's flags.
type RmOptions struct {
	Force    bool // --force: no "delete it?" asked, which needs a terminal otherwise
	NoExport bool // --no-export: the agents' history isn't exported first
	// NoCloneCheck (--no-clone-check): a --clone VM's clone isn't checked
	// for work the repo lacks first, which is lost.
	NoCloneCheck bool
}

// Rm deletes the folder's VM, after asking, or with --force; where it
// can't ask it refuses, naming --force. One in the middle of
// a remake is deleted too, its old machine with it (teardown). Its agents' history
// is exported first unless --no-export: an export that fails keeps it.
// A --clone VM's branches the repo lacks and uncommitted changes are
// checked first (cloneCheck), whose questions no flag answers, unless
// --no-clone-check, which warns that they're lost instead. A VM made
// for an earlier folder at this path, or a parent folder's VM found from
// a subfolder, is deleted only after asking, which --force never
// answers; the earlier folder's isn't
// exported, as its folder is gone. While another devvm makes the folder's
// VM it refuses (holdForRm).
func (e Lifecycle) Rm(ctx context.Context, o RmOptions) error {
	t, err := e.finder.target(ctx, need{has: hasVM})
	if err != nil {
		return err
	}
	// Never one another devvm is making.
	release, err := holdForRm(e.Env, t)
	if err != nil {
		return err
	}
	defer release()
	vm, st := t.f.Name, t.st
	asked := false
	if t.sub != "" {
		yes, err := e.Ask(msg.DeleteParentVM(t.f.Dir, vm).RefusedWith(msg.RmParentVM(t.f.Dir, vm)))
		if err != nil || !yes {
			return err
		}
		asked = true
	}
	if t.other {
		was := st.Dir
		yes, err := e.Ask(msg.DeleteOtherFolderVM(vm, was).RefusedWith(msg.VMOtherFolderRm(vm, was)))
		if err != nil || !yes {
			return err
		}
		asked, o.NoExport = true, true
	}
	// A VM whose creation didn't finish never ran: no history, no work.
	checkClone := st.Finished() && isClone(st) && !t.other
	if checkClone && o.NoCloneCheck {
		e.Warn(msg.CloneCheckSkipped("devvm rm", vm))
		checkClone = false
	}
	if !asked && !o.Force {
		yes, err := e.Ask(msg.RmConfirm(vm).AnsweredBy("--force"))
		if err != nil {
			return err
		}
		if !yes {
			e.Info(msg.Kept(vm))
			return nil
		}
	}
	if checkClone {
		keep, err := cloneCheck{clones: e.clones, command: "devvm rm"}.run(ctx, t)
		if err != nil || keep {
			return err
		}
	}
	// A remake that didn't finish may have left no machine by its name:
	// its history was exported then, unless --no-export.
	if !o.NoExport && t.exists && st.Finished() && len(image.AgentsOf(tools.Names(st))) > 0 {
		if err := e.exportFirst(ctx, t, "devvm rm"); err != nil {
			return err
		}
	}
	return e.delete(ctx, t)
}

// RmVM deletes the VM that name names, by its name or its ssh alias, when
// it's this folder's (Rm) or its project folder is gone or not connected
// (FolderOf); a
// VM whose folder is still there is deleted from that folder. It asks
// first, or deletes with --force, and refuses where it can't ask, as Rm
// does. Its agents' history is exported
// first unless --no-export, which needs the VM running: one stopped would
// make its folder again as it starts (its backend makes a missing mount's
// folder), so it's refused instead, unless it's a --clone VM, which
// mounts none. A --clone VM's work can't be fetched into a repo that's
// gone, so it's deleted only with --no-clone-check. A VM that records
// this folder but isn't this folder's own (a vm.json changed by hand) is
// one whose folder is there, refused.
func (e Lifecycle) RmVM(ctx context.Context, name string, o RmOptions) error {
	t, err := e.finder.target(ctx, need{name: name})
	if err != nil {
		return err
	}
	vm, st := t.f.Name, t.st
	// This folder's own VM, when it's the VM named: never another VM
	// that records this folder.
	if here, err := e.finder.dir(); err == nil && st.Dir == here {
		own, err := e.finder.folderVM(here)
		if err != nil {
			return err
		}
		if own == vm {
			return e.Rm(ctx, o)
		}
	}
	folder := FolderOf(st)
	if folder == FolderThere {
		return msg.RmVMFolderThere(st.Dir, vm)
	}
	if !t.exists {
		return msg.RmVMLost(vm)
	}
	return e.rmGone(ctx, t, folder, o)
}

// rmGone deletes t, a VM whose folder is gone or not connected, for RmVM.
// One whose folder is on a disk that isn't connected is deleted only
// after asking, which --force never answers.
func (e Lifecycle) rmGone(ctx context.Context, t *target, folder FolderState, o RmOptions) error {
	release, err := holdForRm(e.Env, t)
	if err != nil {
		return err
	}
	defer release()
	vm, st := t.f.Name, t.st
	clone := st.Finished() && isClone(st)
	if clone && !o.NoCloneCheck {
		return msg.RmGoneClone(st.Dir, vm)
	}
	export := !o.NoExport && st.Finished() && len(image.AgentsOf(tools.Names(st))) > 0
	if err := e.stillGone(ctx, t, folder, export && !clone); err != nil {
		return err
	}
	if clone {
		e.Warn(msg.CloneCheckSkipped("devvm rm", vm))
	}
	if yes, err := e.askRmGone(t, folder == FolderNotConnected, o.Force); err != nil || !yes {
		if err == nil {
			e.Info(msg.Kept(vm))
		}
		return err
	}
	// While it asked, the folder may have come back (a disk plugged in
	// again), another devvm may have made the VM again there, or stopped
	// it, which an export would start, making its folder again.
	if err := e.stillGone(ctx, t, folder, export && !clone); err != nil {
		return err
	}
	if export {
		if err := e.exportFirst(ctx, t, "devvm rm "+vm.String()); err != nil {
			return err
		}
	}
	return e.delete(ctx, t)
}

// askRmGone asks, for rmGone, whether to delete t: with force, only when
// its folder is on a disk that isn't connected (unconnected), which
// --force never answers.
func (e Lifecycle) askRmGone(t *target, unconnected, force bool) (bool, error) {
	dir, vm := t.st.Dir, t.f.Name
	switch {
	case unconnected:
		return e.Ask(msg.DeleteNotConnectedFolderVM(dir, vm).RefusedWith(msg.RmVMNotConnected(dir, vm)))
	case force:
		return true, nil
	}
	return e.Ask(msg.DeleteGoneFolderVM(dir, vm).AnsweredBy("--force"))
}

// stillGone checks, for rmGone, that t's state is still the one it read
// (its ID) and its folder still in the state it found (folder), and with
// running that the VM still runs, so its export doesn't start it.
func (e Lifecycle) stillGone(ctx context.Context, t *target, folder FolderState, running bool) error {
	vm := t.f.Name
	st, err := state.ReadVM(e.Dirs, vm)
	if err != nil {
		return err
	}
	if st.ID != t.st.ID || FolderOf(st) != folder {
		return msg.RmVMFolderThere(t.st.Dir, vm)
	}
	if !running {
		return nil
	}
	in, err := t.b.Inspect(ctx, vm)
	if err != nil {
		return err
	}
	if in.Status != backend.StatusRunning {
		return msg.RmGoneStopped(t.st.Dir, vm)
	}
	return nil
}

// Export saves the folder's VM's agents' history to file (absolute), or
// to the exports folder when it's empty, starting the VM if it's stopped.
func (e Lifecycle) Export(ctx context.Context, file string) error {
	t, err := e.finder.target(ctx, need{has: hasMade})
	if err != nil {
		return err
	}
	err = e.export(ctx, t, file, backend.Logged(msg.HistoryNotExported(t.f.Name)))
	if errors.Is(err, errHistoryTooLarge) {
		return msg.HistoryTooLarge(t.f.Name)
	}
	return err
}

// exportFirst exports t's agents' history to the exports folder before
// command deletes or remakes the VM. A failed export keeps the VM
// (export_failed_kept); one over historyMax keeps no export, which it
// warns of, and command goes on.
func (e Lifecycle) exportFirst(ctx context.Context, t *target, command string) error {
	err := e.export(ctx, t, "", msg.ExportFailedKept(command, t.f.Name))
	if errors.Is(err, errHistoryTooLarge) {
		e.Warn(msg.HistoryTooLarge(t.f.Name))
		return nil
	}
	return err
}

// historyMax is the most of an export of agents' history devvm keeps:
// past it, devvm keeps none (Env.HistoryMax, when set, in its place).
const historyMax = 2 << 30

// errHistoryTooLarge is export's error for an export over historyMax.
var errHistoryTooLarge = errors.New("the history is over the most an export keeps")

// export saves the VM's agents' history (transcripts, sessions) to a
// tarball at file, or a new one in the exports folder when it's "", with
// a file beside it naming the VM's folder (paths.ExportFolder), for devvm
// history to list once the VM is gone; starting the VM if it's stopped; failed is the error
// when its tar fails. Their logins aren't in it. It's written next to
// file and renamed into place once complete, so a failed export leaves
// nothing and replaces nothing. One over historyMax is stopped and kept
// nowhere (errHistoryTooLarge), as a VM can write without end. The file is the user's alone (600), and
// so is the exports folder, whatever mode it had; a folder of the user's
// keeps its own.
func (e Lifecycle) export(ctx context.Context, t *target, file string, failed error) error {
	vm := t.f.Name
	x := Exports{Dirs: e.Dirs}
	own := file == ""
	if own {
		file = x.newFile(vm, time.Now())
	}
	if err := t.ensureRunning(ctx); err != nil {
		return err
	}
	// devvm's exports folder is the user's alone; a folder the user named
	// keeps its mode.
	if dir := filepath.Dir(file); dir == e.Dirs.Exports() {
		if err := state.PrivateDir(dir); err != nil {
			return err
		}
	} else if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	out, err := os.CreateTemp(filepath.Dir(file), ".devvm-export-*")
	if err != nil {
		return err
	}
	tmp := out.Name()
	done := e.AddCleanup(func(context.Context) { _ = os.Remove(tmp) })
	defer func() {
		_ = os.Remove(tmp)
		done()
	}()
	run, cancel := context.WithCancel(ctx)
	defer cancel()
	w := &bounded{to: out, max: cmp.Or(e.HistoryMax, historyMax), cancel: cancel}
	err = t.exec(run, []string{"bash", "-c", historyExport()}, backend.ExecOpts{Stdout: w, Stderr: e.vmLog()})
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if w.over {
		return errHistoryTooLarge
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return failed
	}
	if err := os.Rename(tmp, file); err != nil {
		return err
	}
	if own {
		if err := x.writeFolder(file, t.f.Dir, netpolicy.TierOf(t.st)); err != nil {
			return err
		}
	}
	e.Info(msg.Exported(file, vm))
	return nil
}

// Import puts an export of agents' history into the folder's VM: file
// (absolute), or the newest export devvm saved for the VM's folder when
// it's "", never another folder's. devvm starts a stopped VM for it.
func (e Lifecycle) Import(ctx context.Context, file string) error {
	t, err := e.finder.target(ctx, need{has: hasMade})
	if err != nil {
		return err
	}
	if file == "" {
		x, ok, err := (Exports{Dirs: e.Dirs}).Newest(t.f.Dir)
		if err != nil {
			return err
		}
		if !ok {
			return msg.HistoryNoExport(t.f.Dir)
		}
		file = x.Path
	} else if _, err := os.Stat(file); err != nil {
		return err
	}
	if err := t.ensureRunning(ctx); err != nil {
		return err
	}
	return importHistory(ctx, e.Env, t, file)
}

// importHistory puts the export of agents' history at file into t's VM,
// which is running. The file is streamed in as it is, and the VM's tar
// reads it (historyImport): it comes from a VM, so the host never reads
// what's in it, and only a VM's own home gets it. An export from a VM on
// a less strict tier is warned of first (warnTier).
func importHistory(ctx context.Context, e *Env, t *target, file string) error {
	vm := t.f.Name
	warnTier(e, t, file)
	f, err := os.Open(file) //nolint:gosec // the export the user named, or the one devvm saved for this folder
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	out := &bounded{max: len(historyUnsafe) + 1}
	err = t.exec(ctx, []string{"bash", "-c", historyImport()}, backend.ExecOpts{Stdin: f, Stdout: out, Stderr: e.vmLog()})
	switch {
	case err == nil:
	case ctx.Err() != nil:
		return ctx.Err()
	case strings.TrimSpace(out.String()) == historyUnsafe:
		return msg.HistoryImportUnsafe(file)
	default:
		return backend.Logged(msg.HistoryNotImported(file, vm))
	}
	e.Info(msg.Imported(file, vm))
	return nil
}

// warnTier warns, before the export at file goes into t's VM, when it
// comes from a VM on a less strict network tier than t's
// (netpolicy.Tier.AtLeast), whose agents may have read what the VM's
// can't reach. Only an export in devvm's exports folder has its tier
// read, from the file beside it, which devvm wrote: one elsewhere, such
// as in a project folder, which its VM can write, or one without a
// tier, is warned of as if from an open VM.
func warnTier(e *Env, t *target, file string) {
	to := netpolicy.TierOf(t.st)
	var from netpolicy.Tier
	if filepath.Dir(file) == e.Dirs.Exports() {
		_, from = Exports{Dirs: e.Dirs}.record(file)
	}
	switch {
	case from == "" && !netpolicy.TierOpen.AtLeast(to):
		e.Warn(msg.HistoryImportTierUnknown(file, string(to), t.f.Name))
	case from != "" && !from.AtLeast(to):
		e.Warn(msg.HistoryImportLessStrict(file, string(from), string(to), t.f.Name))
	}
}

// offerHistory returns the export of agents' history the new VM vm, of
// the folder dir, gets: the newest devvm saved for dir, never another
// folder's, when the VM has an agent among its shared tools (names) and
// the user says yes ([y/N]: the export is an old VM's content). Where it
// can't ask, it says how to import it later, and returns "".
func (e creation) offerHistory(vm vmname.VM, dir string, names []string) string {
	if len(image.AgentsOf(names)) == 0 {
		return ""
	}
	x, ok, err := (Exports{Dirs: e.Dirs}).Newest(dir)
	if err != nil || !ok {
		return ""
	}
	yes, err := e.Ask(msg.HistoryImportAsk(x.Path, vm).AnsweredBy("devvm history import"))
	if err != nil {
		e.Note(msg.HistoryImportLater(x.Path, vm))
		return ""
	}
	if !yes {
		return ""
	}
	return x.Path
}

// importOffered imports the export the user said yes to (offerHistory)
// into t's new VM, started and not yet set up; none when file is "". The VM is
// made either way, so a failed import only warns, and devvm history
// import can try again.
func (e creation) importOffered(ctx context.Context, t *target, file string) error {
	if file == "" {
		return nil
	}
	err := importHistory(ctx, e.Env, t, file)
	switch m, ok := errors.AsType[msg.M](err); {
	case err == nil:
	case ctx.Err() != nil:
		return ctx.Err()
	case ok:
		e.Warn(m)
	default:
		e.Warn(msg.HistoryNotImported(file, t.f.Name))
	}
	return nil
}

// delete deletes the VM, then what devvm keeps for it (teardown): its
// folder, its proxy and link session, .git's locks or the repo's remote
// for its clone, other VMs' links to it, its host keys and its ssh config
// entry. A delete that failed but left no VM (it was half gone already)
// goes on like one that worked.
func (e Lifecycle) delete(ctx context.Context, t *target) error {
	vm := t.f.Name
	if err := t.b.Delete(ctx, vm); err != nil {
		if _, ierr := t.b.Inspect(ctx, vm); !errors.Is(ierr, backend.ErrNotFound) {
			return err
		}
	}
	e.Info(msg.Deleted(vm))
	return e.vmFolders.teardown(ctx, t, false)
}
