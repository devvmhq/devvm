package vm

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/build"
	"github.com/devvmhq/devvm/internal/gitprotect"
	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/ports"
	"github.com/devvmhq/devvm/internal/sshagent"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/tools"
	"github.com/devvmhq/devvm/internal/vmname"
)

// RemakeOptions are the flags of a command that makes the folder's VM
// again (devvm layer add, devvm layer rm, devvm update, devvm clone).
type RemakeOptions struct {
	Force    bool // --force: no "make it again?" asked, the answer where it can't be
	NoExport bool // --no-export: the agents' history isn't exported first
	// CleanHome (--clean-home): the new disk has the image's home folder,
	// not the VM's.
	CleanHome bool
	// NoCloneCheck (--no-clone-check, devvm clone off's): the clone isn't
	// checked for work the repo lacks first, which is lost.
	NoCloneCheck bool
	// DefaultFrom is what devvm update starts a VM's base from once its
	// devcontainer.json no longer names the image it started from:
	// $DEVVM_FROM's, "" for Ubuntu's cloud image.
	DefaultFrom string
}

// The clone modes a remake makes a VM in (remakeInVM.Clone), as devvm
// clone takes them: with its own clone of the repo, or mounting the
// folder.
const (
	CloneOn  = "on"
	CloneOff = "off"
)

// remakeInVM is a VM's remake section, while devvm makes it again: set
// before its old machine stops, and gone once the new one has started
// with its home folder. While it's set, the next devvm in the folder
// finishes the remake (remaking.finishPending), and other commands on
// the VM refuse (remake_pending).
type remakeInVM struct {
	Image string `json:"image,omitempty"` // the image it's made again from
	// Command is the command that made it again, as typed (devvm layer
	// add desktop), which its messages name.
	Command  string `json:"command,omitempty"`
	Home     bool   `json:"home,omitempty"`      // its home folder is kept (paths.VM.Home holds it until the new disk does)
	NoExport bool   `json:"no_export,omitempty"` // --no-export
	Made     bool   `json:"made,omitempty"`      // its new machine was made, so the one by its name is no longer the old one
	// Clone is the mode its new machine is made in, CloneOn or CloneOff
	// (devvm clone on, devvm clone off); "" keeps the VM's. Once Made,
	// the clone section says the new machine's mode (switchedTo).
	Clone string `json:"clone,omitempty"`
	// Done: its new machine has its home folder and its new image is
	// recorded, so the new machine is the VM, and only deleting the old
	// one is left.
	Done bool `json:"done,omitempty"`
	// Reread is what devvm update read again of the folder's
	// devcontainer.json (devcontainers.reread), which its new machine is
	// made with; nil keeps the VM's. Its tools' files are set once its
	// image is built (remaking.build).
	Reread *rereadInVM `json:"reread,omitempty"`
	// Was is what the VM had of Reread's when its new machine was made,
	// which putBack gives it back.
	Was *rereadInVM `json:"was,omitempty"`
}

// rereadInVM is what a remake of a VM changes when devvm update reads
// its devcontainer.json again: the from its base starts from, its shared
// tools, the ports it forwards (a ports value, as devvm ports sets it)
// and its devcontainer section.
type rereadInVM struct {
	From         string           `json:"from,omitempty"`
	Tools        tools.InVM       `json:"tools"`
	Forward      string           `json:"forward,omitempty"`
	Devcontainer devcontainerInVM `json:"devcontainer"`
}

// fromOf is the from p makes t's VM again from: Reread's, else the VM's.
func fromOf(t *target, p remakeInVM) string {
	if p.Reread != nil {
		return p.Reread.From
	}
	return t.st.From
}

// remakePending reports whether a remake of the VM didn't finish.
func remakePending(st *state.VM) bool {
	return sectionOf[remakeInVM](st, remakeSection).Image != ""
}

// AddLayers makes the folder's VM again from its image with layers added
// on top, in the order given (docker with desktop added is
// docker+desktop), after the user agrees (remakeTo), then opens a shell
// in it. Layers it has already are said and left as they are; when it
// has them all, nothing changes. command is the command as typed. A
// remake of it that didn't finish is finished first.
func (e Lifecycle) AddLayers(ctx context.Context, command string, layers []string, o RemakeOptions) error {
	return e.remakeLayers(ctx, command, o, func(t *target) (string, error) {
		img, had := addedImage(image.Name(t.st.Image), layers)
		if len(had) > 0 {
			e.Info(msg.HasLayers(strings.Join(had, " "), t.f.Name))
		}
		return img.String(), nil
	})
}

// RemoveLayers makes the folder's VM again from its image with layers
// taken out, the rest in their order (docker+nix+desktop without nix is
// docker+desktop), after the user agrees (remakeTo), then opens a shell
// in it. A layer it lacks is said and left out, and one that isn't a
// layer at all is refused (CheckLayers); when it lacks them all,
// nothing changes. command is the command as typed. A remake of it that
// didn't finish is finished first.
func (e Lifecycle) RemoveLayers(ctx context.Context, command string, layers []string, o RemakeOptions) error {
	return e.remakeLayers(ctx, command, o, func(t *target) (string, error) {
		img, lacks := removedImage(image.Name(t.st.Image), layers)
		if err := CheckLayers(lacks, e.Builder(t.b, t.st.From).Layers()[1:]); err != nil {
			return "", err
		}
		if len(lacks) > 0 {
			e.Info(msg.LacksLayers(strings.Join(lacks, " "), t.f.Name))
		}
		return img.String(), nil
	})
}

// remakeLayers makes the folder's VM again from the image that layers
// works out from it (devvm layer add's, devvm layer rm's), after the user
// agrees (remakeTo). When that's the image it has, nothing changes, but
// a VM whose remake was just finished gets its shell.
func (e Lifecycle) remakeLayers(ctx context.Context, command string, o RemakeOptions, layers func(*target) (string, error)) error {
	t, resumed, release, err := e.toRemake(ctx, command, false)
	if err != nil || t == nil {
		return err
	}
	defer release()
	img, err := layers(t)
	if err != nil {
		return err
	}
	if img == t.st.Image {
		if resumed {
			return e.launcher.open(ctx, Options{}, t)
		}
		return nil
	}
	return e.remakeTo(ctx, t, plan(img, command, o), o)
}

// plan is the remake of a VM from img that command (as typed) asks for
// with o.
func plan(img, command string, o RemakeOptions) remakeInVM {
	return remakeInVM{Image: img, Command: command, Home: !o.CleanHome, NoExport: o.NoExport}
}

// Update brings the folder's VM up to date, then opens a shell in it.
// First it moves the VM to any newer shared tools (Tools.update), which
// needs no remake, as they're mounted. Then, when its own image or one
// it's built on changed since the VM was made (outdated:
// Builds.Outdated, the warning a launch gives), it says which changed,
// and when the folder's devcontainer.json isn't the one the VM was made
// from (devcontainers.reread, the warning a launch gives), it reads the
// file again; either way, it makes the VM again from its image as it
// builds now, with what the file says now, after the user agrees
// (agreed), asked before anything changes, so where devvm can't ask,
// such a VM is refused as it is. A VM with the newest tools that's
// neither is said to be current, and nothing changes. command is the
// command as typed. A remake of it that didn't finish is finished first.
func (e Lifecycle) Update(ctx context.Context, command string, o RemakeOptions) error {
	t, resumed, release, err := e.toRemake(ctx, command, false)
	if err != nil || t == nil {
		return err
	}
	defer release()
	p, remake, err := e.updatePlan(ctx, t, command, o)
	if err != nil {
		return err
	}
	agreed := false
	if remake {
		if err := e.remaking.check(t, p); err != nil {
			return err
		}
		// Asked before anything changes, so a refusal leaves it as it is.
		if agreed, err = e.agreed(t, p, o); err != nil {
			return err
		}
	}
	newer, err := e.vmTools.update(ctx, t)
	if err != nil {
		return err
	}
	switch {
	case agreed:
		return e.remakeNow(ctx, t, p)
	case newer || resumed:
		// Its newer tools take effect as its shell opens, kept or not.
		return e.launcher.open(ctx, Options{}, t)
	case !remake:
		e.Info(msg.VMCurrent(t.st.Image, t.f.Name))
	}
	return nil
}

// updatePlan is the remake devvm update makes of t's VM, and whether it
// makes one: when its image, or one it's built on, changed (said), or its
// devcontainer.json did (reread).
func (e Lifecycle) updatePlan(ctx context.Context, t *target, command string, o RemakeOptions) (remakeInVM, bool, error) {
	st := t.st
	changed, err := (build.Builds{Env: e.Builder(t.b, st.From)}).Outdated(st.Chain)
	if err != nil {
		return remakeInVM{}, false, err
	}
	if len(changed) > 0 {
		e.Info(msg.VMChanged(strings.Join(changed, ", "), t.f.Name))
	}
	p := plan(st.Image, command, o)
	img, r, err := e.devcontainers.reread(ctx, t, o.DefaultFrom)
	if err != nil {
		return remakeInVM{}, false, err
	}
	if r != nil {
		p.Image, p.Reread = img, r
	}
	return p, len(changed) > 0 || r != nil, nil
}

// addedImage is the image img makes with layers added on top, in the
// order given (inOrder), and those of layers img has already.
func addedImage(img image.Name, layers []string) (image.Name, []string) {
	was := img.Layers()
	var had []string
	for _, l := range layers {
		if slices.Contains(was, l) && !slices.Contains(had, l) {
			had = append(had, l)
		}
	}
	return image.NameOf(inOrder(was, slices.Concat(was, layers))), had
}

// removedImage is the image img makes with layers taken out, the rest in
// their order (base when none is left), and those of layers img lacks.
func removedImage(img image.Name, layers []string) (image.Name, []string) {
	was := img.Layers()
	var lacks []string
	for _, l := range layers {
		if !slices.Contains(was, l) && !slices.Contains(lacks, l) {
			lacks = append(lacks, l)
		}
	}
	return image.NameOf(slices.DeleteFunc(was, func(l string) bool { return slices.Contains(layers, l) })), lacks
}

// toRemake finds the folder's VM for a command that makes it again
// (command, as typed), and finishes a remake of it that didn't
// (resumed). A parent folder's VM is made again only after asking, which
// no flag answers; nil when the user declines. It
// refuses a VM that's missing, made for another folder, unfinished, or,
// unless cloneOK (devvm clone's), a --clone VM, whose clone is on its
// disk. Once it has the VM, it holds the VM's remake lock (hold) until
// release.
func (e Lifecycle) toRemake(ctx context.Context, command string, cloneOK bool) (t *target, resumed bool, release func(), err error) {
	if t, err = e.finder.target(ctx, need{folder: true}); err != nil {
		return nil, false, nil, err
	}
	vm := t.f.Name
	if t.sub != "" {
		yes, err := e.Ask(msg.RemakeParentVMAsk(t.f.Dir, vm).RefusedWith(msg.RemakeParentVM(command, t.f.Dir, vm)))
		if err != nil || !yes {
			return nil, false, nil, err
		}
	}
	if release, err = e.remaking.hold(ctx, t); err != nil {
		return nil, false, nil, err
	}
	if resumed, err = e.refuseRemake(ctx, t, cloneOK); err != nil {
		release()
		return nil, false, nil, err
	}
	return t, resumed, release, nil
}

// refuseRemake finishes a remake of t's VM that didn't (resumed), then
// refuses a VM that's missing, unfinished, or, unless cloneOK, a --clone
// VM.
func (e Lifecycle) refuseRemake(ctx context.Context, t *target, cloneOK bool) (resumed bool, err error) {
	vm := t.f.Name
	if remakePending(t.st) {
		if err := e.remaking.finishPending(ctx, t); err != nil {
			return false, err
		}
		resumed = true
	}
	if !t.exists {
		return false, msg.NoVM(t.f.Dir)
	}
	if err := refuseUnfinished(vm, t.st); err != nil {
		return false, err
	}
	if isClone(t.st) && !cloneOK {
		return false, msg.RemakeClone(vm)
	}
	return resumed, nil
}

// hold takes t's VM's remake lock (paths.VM.RemakeLock), which a devvm
// holds while it makes the VM again or finishes doing so, and refuses
// while another devvm holds it (remake_running). A VM without state has
// nothing to lock. Under the lock it reads the VM's state again, as
// another devvm may have made the VM again since t was found, and asks
// the backend again whether it has the VM when the remake recorded
// changed meanwhile.
func (e remaking) hold(ctx context.Context, t *target) (release func(), err error) {
	if !t.st.Exists() {
		return func() {}, nil
	}
	vm, f := t.f.Name, e.Dirs.VM(t.f.Name)
	if err := state.PrivateDir(f.Run()); err != nil {
		return nil, err
	}
	unlock, ok, err := platform.TryLockFile(f.RemakeLock())
	switch {
	case err != nil:
		return nil, err
	case !ok:
		return nil, msg.RemakeRunning(vm)
	}
	was := sectionOf[remakeInVM](t.st, remakeSection)
	st, err := state.ReadVM(e.Dirs, vm)
	if err != nil {
		unlock()
		return nil, err
	}
	t.st = st
	if !reflect.DeepEqual(sectionOf[remakeInVM](st, remakeSection), was) {
		if err := t.refresh(ctx); err != nil {
			unlock()
			return nil, err
		}
	}
	return unlock, nil
}

// remakeTo makes t's VM again as p says (remaking.remake), once
// remaking.check has checked that it can, and the user agrees (agreed).
// Then it opens a shell in it.
func (e Lifecycle) remakeTo(ctx context.Context, t *target, p remakeInVM, o RemakeOptions) error {
	if err := e.remaking.check(t, p); err != nil {
		return err
	}
	if ok, err := e.agreed(t, p, o); err != nil || !ok {
		return err
	}
	return e.remakeNow(ctx, t, p)
}

// agreed reports whether the user agrees to make t's VM again from p's
// image, once remaking.check has checked that it can: asked ([y/N]),
// which --force answers, and refused where it can't be. A no is said
// (kept).
func (e Lifecycle) agreed(t *target, p remakeInVM, o RemakeOptions) (bool, error) {
	vm := t.f.Name
	if o.Force {
		return true, nil
	}
	ask := msg.RemakeConfirm(p.Image, vm)
	if !p.Home {
		ask = msg.RemakeConfirmCleanHome(p.Image, vm)
	}
	yes, err := e.Ask(ask.AnsweredBy("--force"))
	if err != nil {
		return false, err
	}
	if !yes {
		e.Info(msg.Kept(vm))
	}
	return yes, nil
}

// remakeNow makes t's VM again as p says (remaking.remake), then opens a
// shell in it.
func (e Lifecycle) remakeNow(ctx context.Context, t *target, p remakeInVM) error {
	if err := e.remaking.remake(ctx, t, p); err != nil {
		return err
	}
	return e.launcher.open(ctx, Options{}, t)
}

// remaking makes a VM again from an image, keeping devvm's settings for
// it and its home folder: the one remake devvm layer add, devvm layer rm,
// devvm update and devvm clone use.
type remaking struct{ *Env }

// check refuses, before anything runs, to make t's VM again as p says
// when one of its image's layers is gone, or its backend can't run what
// the VM needs with it (requirements: its ports, and a --clone VM's ssh
// too), or the disk lacks room for a VM.
func (e remaking) check(t *target, p remakeInVM) error {
	vm, b, img := t.f.Name, t.b, image.Name(p.Image)
	if err := (build.Names{Env: e.Builder(b, fromOf(t, p))}).Check(img); err != nil {
		return err
	}
	spec := e.spec(t, p)
	cur, err := ports.Of(t.st)
	if err != nil {
		return err
	}
	if p.Reread != nil {
		cur.Forward = p.Reread.Forward
	}
	forward, err := ports.Forwarding(cur.Forward)
	if err != nil {
		return err
	}
	// Its clone is copied in and fetched over ssh, as up's.
	need := needs{spec: &spec, net: confinementFor(t.st).Net, ports: forward, img: img, ssh: spec.CloneAt != ""}
	if err := e.Registry.Require(b, vm, requirements(need)...); err != nil {
		return err
	}
	return (backend.Space{Free: e.FreeSpace}).Check(b, vm, func(f backend.Floors) uint64 { return f.VM })
}

// spec is the new machine of t's VM as its state asks for it
// (creation.machineSpec, as up makes a new VM's): its shared tools, SSH
// agent and size, in the clone mode p makes it in (wantsClone).
func (e remaking) spec(t *target, p remakeInVM) backend.VMSpec {
	st := t.st
	return e.creation.machineSpec(t.b, t.f, tools.Names(st), sshagent.Forwards(sshagent.Mode(st)),
		sectionOf[Size](st, sizeSection), wantsClone(st, p))
}

// wantsClone reports whether p makes st's VM's new machine with its own
// clone of the repo: as p.Clone says, else as the VM is.
func wantsClone(st *state.VM, p remakeInVM) bool {
	switch p.Clone {
	case CloneOn:
		return true
	case CloneOff:
		return false
	}
	return isClone(st)
}

// remake makes t's VM again from p's image, each step safe to interrupt:
// the image built first, so a failed build changes nothing; the agents'
// history exported (unless p.NoExport) and its home folder saved (unless
// it's not kept), so a failure there keeps the VM as it was; then its
// remake recorded in its state, the VM stopped and its old machine set
// aside under another name (vmname.Aside), and the new one made (finish).
// Until the new one has started with its home folder, a failure or an
// interrupt deletes it and puts the old one back as it was (undo); once
// it has, the old one is deleted.
func (e remaking) remake(ctx context.Context, t *target, p remakeInVM) error {
	vm := t.f.Name
	unmaking, err := e.holdTools(ctx, p)
	if err != nil {
		return err
	}
	defer unmaking()
	chain, err := e.build(ctx, t, &p)
	if err != nil {
		return err
	}
	if !p.NoExport && len(image.AgentsOf(tools.Names(t.st))) > 0 {
		if err := e.lifecycle.exportFirst(ctx, t, p.Command); err != nil {
			return err
		}
	}
	if p.Home {
		if err := t.ensureRunning(ctx); err != nil {
			return err
		}
		if err := e.saveHome(ctx, t, msg.RemakeHomeFailed(p.Command, vm)); err != nil {
			return err
		}
	}
	if err := update(ctx, t.st, remakeSection, func(r *remakeInVM) error { *r = p; return nil }); err != nil {
		return err
	}
	if err := e.setAside(ctx, t); err != nil {
		// Its old machine is as it was, stopped at most.
		return errors.Join(err, e.clear(ctx, t))
	}
	var committed atomic.Bool
	done := e.AddCleanup(e.undo(t, &committed))
	if err := e.finish(ctx, t, p, chain, &committed); err != nil {
		return err
	}
	done()
	return nil
}

// build builds p's image (as its record has it) for t's VM, from p's
// from, as a new VM's is built, and returns its build chain. A reread
// devcontainer.json's tools (p.Reread) are then built for that image's
// base, as a new VM's are, and their files set in p.
func (e remaking) build(ctx context.Context, t *target, p *remakeInVM) (chainOf, error) {
	b, img := e.Builder(t.b, fromOf(t, *p)), image.Name(p.Image)
	if err := (build.Builds{Env: b}).Build(ctx, img); err != nil {
		return chainOf{}, err
	}
	c, err := b.Chain(img)
	if err != nil {
		return chainOf{}, err
	}
	if p.Reread != nil {
		if p.Reread.Tools.Made, err = e.buildTools(ctx, b, p.Reread.Tools.Names); err != nil {
			return chainOf{}, err
		}
	}
	return chainOf{key: c.Key(), chain: c.String()}, nil
}

// buildTools builds the shared tools names for a VM of b's base, built,
// as a new VM's are, and returns what it gets of them.
func (e remaking) buildTools(ctx context.Context, b *build.Env, names []string) (tools.Made, error) {
	tg, err := b.ToolTarget()
	if err != nil {
		return tools.Made{}, err
	}
	files, err := (tools.Versions{Env: e.Tools}).Files(ctx, names, tg)
	if err != nil {
		return tools.Made{}, err
	}
	if err := e.launcher.buildTools(ctx, b, tg, files); err != nil {
		return tools.Made{}, err
	}
	return tools.Made{Files: files, Libc: tg.Libc, Glibc: tg.Glibc}, nil
}

// holdTools holds the lock VMs being made hold while p, a remake that
// changes the VM's tools (Reread), makes it, so gc keeps their images
// until its state lists them.
func (e remaking) holdTools(ctx context.Context, p remakeInVM) (func(), error) {
	if p.Reread == nil {
		return func() {}, nil
	}
	return tools.Making{Dirs: e.Dirs, Reporter: e.Reporter}.Hold(ctx)
}

// chainOf is an image's build chain, as a VM is made from it (its key)
// and its state records it.
type chainOf struct{ key, chain string }

// setAside stops t's VM, what runs for it with it, and gives its machine
// its aside name (vmname.Aside), so a new one can have its name.
func (e remaking) setAside(ctx context.Context, t *target) error {
	vm := t.f.Name
	if err := t.stop(ctx); err != nil {
		return err
	}
	e.Proxy.Stop(vm)
	e.Forwarders.Stop(vm)
	e.linkRuns.stop(ctx, vm)
	aside, _ := vmname.Aside(vm)
	return t.b.Rename(ctx, vm, aside)
}

// finish makes t's VM's new machine from p's image (built: chain) with
// what its state records, in p's clone mode, starts it as a launch does,
// puts its saved home folder in (then committed is set), records its new
// image and deletes its old machine, which ends its remake (dropOld).
func (e remaking) finish(ctx context.Context, t *target, p remakeInVM, c chainOf, committed *atomic.Bool) error {
	vm, st, b := t.f.Name, t.st, t.b
	e.Info(msg.CreatingFrom(p.Image, vm))
	e.LogNote()
	if err := e.creation.makeMachine(ctx, b, st, c.key, e.spec(t, p)); err != nil {
		return err
	}
	t.exists = true
	// The new machine is the one by its name now, in its clone mode, with
	// what p read again, and its devcontainer.json's commands run again
	// on its new disk.
	var git gitprotect.InVM
	if p.Clone == CloneOff {
		git = e.Protector(ctx, t.f.Dir).NewSection(false)
	}
	err := st.Update(ctx, func(st *state.VM) error {
		if err := madeWith(st, p); err != nil {
			return err
		}
		return switchedTo(st, p.Clone, git)
	})
	if err != nil {
		return err
	}
	if p.Clone == CloneOff {
		if err := e.mountFolder(ctx, t); err != nil {
			return err
		}
	}
	if _, err := e.starter.bringUp(ctx, t, upOpts{}); err != nil {
		return err
	}
	if p.Home {
		if err := e.restoreHome(ctx, t); err != nil {
			return err
		}
	}
	// The new machine has its home folder, so it's the VM now: no undo
	// deletes it, and once its image is recorded, only deleting the old
	// machine is left (Done), which the next devvm finishes when this
	// one doesn't. Should recording fail, the next devvm finds the old
	// machine aside and puts it back.
	committed.Store(true)
	err = st.Update(ctx, func(st *state.VM) error {
		st.Image, st.Chain = p.Image, c.chain
		return edit(st, remakeSection, func(r *remakeInVM) error { r.Done = true; return nil })
	})
	if err != nil {
		return err
	}
	return e.dropOld(ctx, t, p)
}

// madeWith records in st, inside st.Update, that its remake p made its
// new machine (Made): what the VM had of what p may read again
// (rereadOf) kept in Was the first time, for putBack, then p's reread
// devcontainer.json in its place (setReread), and its commands run again.
func madeWith(st *state.VM, p remakeInVM) error {
	r, err := state.Section[remakeInVM](st, remakeSection)
	if err != nil {
		return err
	}
	if !r.Made {
		was, err := rereadOf(st)
		if err != nil {
			return err
		}
		r.Made, r.Was = true, &was
		if err := state.SetSection(st, remakeSection, r); err != nil {
			return err
		}
	}
	if p.Reread != nil {
		return setReread(st, *p.Reread)
	}
	return edit(st, devcontainerSection, func(d *devcontainerInVM) error { d.Made = devcontainerMade{}; return nil })
}

// rereadOf is what st's VM has of what devvm update reads again of its
// devcontainer.json: its from, shared tools and devcontainer section.
func rereadOf(st *state.VM) (rereadInVM, error) {
	tl, err := tools.Of(st)
	if err != nil {
		return rereadInVM{}, err
	}
	d, err := state.Section[devcontainerInVM](st, devcontainerSection)
	if err != nil {
		return rereadInVM{}, err
	}
	p, err := ports.Of(st)
	if err != nil {
		return rereadInVM{}, err
	}
	return rereadInVM{From: st.From, Tools: tl, Forward: p.Forward, Devcontainer: d}, nil
}

// setReread gives st's VM r's from, shared tools, forwarded ports (as
// devvm ports sets them: ports.SetForward) and devcontainer section, in
// st alone: inside st.Update.
func setReread(st *state.VM, r rereadInVM) error {
	st.From = r.From
	if err := tools.Set(st, r.Tools); err != nil {
		return err
	}
	if err := ports.SetForward(st, r.Forward); err != nil {
		return err
	}
	return state.SetSection(st, devcontainerSection, r.Devcontainer)
}

// dropOld ends a remake of t's VM whose new machine is done (p.Done):
// it deletes the old machine, set aside, then what the old one's clone
// mode left on the host (leftBehind), then ends the remake (clear).
func (e remaking) dropOld(ctx context.Context, t *target, p remakeInVM) error {
	vm := t.f.Name
	aside, _ := vmname.Aside(vm)
	if err := t.b.Delete(ctx, aside); err != nil && !errors.Is(err, backend.ErrNotFound) {
		return err
	}
	if err := e.leftBehind(ctx, t, p.Clone); err != nil {
		return err
	}
	if err := e.clear(ctx, t); err != nil {
		return err
	}
	e.Info(msg.Remade(p.Image, vm))
	return nil
}

// undo is a remake's cleanup, after a failure or an interrupt, until
// committed: it deletes the new machine and puts the old one back, as it
// was (putBack), and ends the remake. When it can't, the remake stays
// recorded, for the next devvm to finish.
func (e remaking) undo(t *target, committed *atomic.Bool) func(context.Context) {
	return func(ctx context.Context) {
		if committed.Load() {
			return
		}
		vm := t.f.Name
		e.Info(msg.RemakeUndone(vm))
		if err := e.putBack(ctx, t); err != nil {
			e.Warn(msg.RemakeUndoFailed(t.f.Dir, vm))
			return
		}
		if err := e.clear(ctx, t); err != nil {
			e.Warn(msg.RemakeUndoFailed(t.f.Dir, vm))
		}
	}
}

// putBack deletes t's VM's new machine, if it was made, and gives its old
// one, set aside, its name again: its next start makes it what its state
// asks for, as for any VM.
func (e remaking) putBack(ctx context.Context, t *target) error {
	vm, b := t.f.Name, t.b
	aside, _ := vmname.Aside(vm)
	if _, err := b.Inspect(ctx, aside); err != nil {
		return err
	}
	e.Proxy.Stop(vm)
	e.Forwarders.Stop(vm)
	e.linkRuns.stop(ctx, vm)
	if err := b.Delete(ctx, vm); err != nil && !errors.Is(err, backend.ErrNotFound) {
		return err
	}
	t.exists = false
	if err := b.Rename(ctx, aside, vm); err != nil {
		return err
	}
	t.exists = true
	// The machine by its name is the old one again, in its clone mode,
	// with what it had of what the remake read again.
	r := sectionOf[remakeInVM](t.st, remakeSection)
	if r.Made && r.Clone == CloneOff {
		e.unmountFolder(ctx, t)
	}
	return t.st.Update(ctx, func(st *state.VM) error {
		if r.Made {
			if err := switchedBack(st, r.Clone); err != nil {
				return err
			}
		}
		if r.Made && r.Was != nil {
			if err := setReread(st, *r.Was); err != nil {
				return err
			}
		}
		return edit(st, remakeSection, func(r *remakeInVM) error { r.Made, r.Was = false, nil; return nil })
	})
}

// clear ends t's VM's remake: its section goes, then its saved home
// folder, which stays while the section does.
func (e remaking) clear(ctx context.Context, t *target) error {
	if err := update(ctx, t.st, remakeSection, func(r *remakeInVM) error { *r = remakeInVM{}; return nil }); err != nil {
		return err
	}
	if err := os.Remove(e.Dirs.VM(t.f.Name).Home()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// finishPending finishes a remake of t's VM that didn't (one a devvm
// that was killed left), asking nothing. While its old machine is set
// aside, or it was never set aside, the VM is put back as it was and made
// again from the start (remake). Once the old one is gone, the new one,
// made or not, never ran a shell: it's made again, with the home folder
// saved for it. Once the new one is done, only the old one is deleted
// (dropOld).
func (e remaking) finishPending(ctx context.Context, t *target) error {
	vm, b := t.f.Name, t.b
	p := sectionOf[remakeInVM](t.st, remakeSection)
	e.Info(msg.RemakeResuming(p.Image, vm))
	if p.Done {
		return e.dropOld(ctx, t, p)
	}
	aside, _ := vmname.Aside(vm)
	_, err := b.Inspect(ctx, aside)
	again := err == nil || t.exists && !p.Made
	switch {
	case err != nil && !errors.Is(err, backend.ErrNotFound):
		return err
	case again:
		if err == nil {
			if err := e.putBack(ctx, t); err != nil {
				return err
			}
		}
		if err := e.clear(ctx, t); err != nil {
			return err
		}
		if err := e.check(t, p); err != nil {
			return err
		}
		p.Made, p.Was = false, nil
		return e.remake(ctx, t, p)
	}
	return e.makeNew(ctx, t, p)
}

// makeNew finishes a remake of t's VM whose old machine is gone, for
// finishPending: checked as any remake is, it deletes the new machine,
// if it was made, and makes it again with the home folder saved for it.
func (e remaking) makeNew(ctx context.Context, t *target, p remakeInVM) error {
	vm, b := t.f.Name, t.b
	if err := e.check(t, p); err != nil {
		return err
	}
	if t.exists {
		if err := b.Delete(ctx, vm); err != nil && !errors.Is(err, backend.ErrNotFound) {
			return err
		}
		t.exists = false
	}
	if _, err := os.Stat(e.Dirs.VM(vm).Home()); p.Home && err != nil {
		e.Warn(msg.RemakeHomeLost(vm))
		p.Home = false
	}
	unmaking, err := e.holdTools(ctx, p)
	if err != nil {
		return err
	}
	defer unmaking()
	c, err := e.build(ctx, t, &p)
	if err != nil {
		return err
	}
	return e.finish(ctx, t, p, c, new(atomic.Bool))
}
