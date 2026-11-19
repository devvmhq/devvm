package vm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"strings"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/devcontainer"
	"github.com/devvmhq/devvm/internal/from"
	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/ports"
	"github.com/devvmhq/devvm/internal/safefile"
	"github.com/devvmhq/devvm/internal/settings"
	"github.com/devvmhq/devvm/internal/shell"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/tools"
	"github.com/devvmhq/devvm/internal/vmname"
)

// devcontainerUnreadable is the sum of a file that can't be read now.
const devcontainerUnreadable = "unreadable"

// commandScript runs a devcontainer.json's command, its arguments, in a
// login shell: the VM's PATH, and its variables (~/.profile).
const commandScript = `exec "$@"`

// devcontainers reads a project's devcontainer.json for a new VM, and runs
// its commands in the VM.
type devcontainers struct{ *Env }

// find reads the folder's devcontainer.json, never following a link out
// of it: its path in the folder ("" for none) and its contents. The folder
// is a safefile.Root, which opens only regular files, without waiting: a
// FIFO the VM made in place of the file, or swaps in, is refused at once.
func (e devcontainers) find(dir string) (string, []byte, error) {
	root, err := safefile.OpenRoot(dir)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = root.Close() }()
	return devcontainer.Find(root.FS())
}

// prepare reads the folder's devcontainer.json for a new VM, unless
// --no-devcontainer, and returns what the VM is made from with it
// (fromRef: --from's, or the file's image, one its registry gives anyone,
// pinned) and its plan (nil without one): its features' layers go under
// IMAGE's, its forwardPorts add to the default ports. What's given on the
// command line wins: --from, --ports. It says what it ignores.
func (e devcontainers) prepare(ctx context.Context, o Options, dir string) (fromRef string, dc *devcontainer.Plan, err error) {
	if o.NoDevcontainer {
		return o.From, nil, nil
	}
	file, data, err := e.find(dir)
	switch {
	case err != nil:
		return "", nil, msg.DevcontainerUnreadable(file).Because(err)
	case file == "":
		return o.From, nil, nil
	}
	p, err := devcontainer.Parse(file, data, dir)
	if err != nil {
		return "", nil, err
	}
	for _, m := range p.Notes {
		e.Warn(m)
	}
	fromRef = o.From
	switch {
	case p.Image == "":
	case o.FromSet:
		e.Warn(msg.DevcontainerFlagWins(file, "from", "image"))
	default:
		if fromRef, err = e.pin(ctx, file, p.Image); err != nil {
			return "", nil, err
		}
	}
	if p.Ports != "" && o.Ports != "" {
		// newPorts leaves the file's out, and they're recorded as the
		// file's, so devvm update adds only those it lists anew.
		e.Warn(msg.DevcontainerFlagWins(file, "ports", "forwardPorts"))
	}
	return fromRef, &p, nil
}

// pin pins ref, the image file names, to the digest its registry gives
// anyone, refusing one it doesn't.
func (e devcontainers) pin(ctx context.Context, file, ref string) (string, error) {
	if err := from.Check(ref); err != nil {
		return "", err
	}
	pinned, err := e.PublicImage(ctx, ref)
	if errors.Is(err, from.ErrNotFound) {
		return "", msg.DevcontainerImageNotPublic(file, ref).Because(err)
	}
	return pinned, err
}

// record sets what a new VM is made from in its state, before it's
// written: the file (dc, its plan), what it maps to of the VM's own (its
// variables, its commands), and what of the VM is the file's (gave: its
// parts it got from the file alone; portsGave: the ports the file added;
// its base's image) and what the user's (--from), or none, off with
// --no-devcontainer.
func (e devcontainers) record(st *state.VM, o Options, dc *devcontainer.Plan, gave, portsGave []string) error {
	d := devcontainerInVM{Off: o.NoDevcontainer}
	if !o.NoDevcontainer {
		d.FromFlag = o.FromSet
	}
	if p := dc; p != nil && !o.NoDevcontainer {
		d.File, d.Sum, d.Env, d.Create, d.Start = p.File, p.Sum, p.Env, p.Create, p.Start
		d.Parts, d.Gave = p.Layers, gave
		d.Ports, d.PortsGave = portList(p.Ports), portsGave
		if !o.FromSet {
			d.Image = p.Image
		}
	}
	return state.SetSection(st, devcontainerSection, d)
}

// fileParts is what a new VM made for the folder dir gets from its
// devcontainer.json (dc) alone of got, its parts: the toolchains and
// layers the file's features map to, unless the user's --tools or
// --layers chose that part's list, devvm setup's layers (def's) have
// it, or the folder's other files call for it.
func fileParts(o Options, dir string, dc *devcontainer.Plan, def settings.Defaults, got Parts) []string {
	if dc == nil {
		return nil
	}
	var others []string
	for _, f := range detectFiles(dir) {
		others = append(others, f.Name)
	}
	var gave []string
	for _, p := range dc.Layers {
		tool := image.IsTool(p)
		switch {
		case tool && (o.Toolchains != nil || !slices.Contains(got.Toolchains, p) || slices.Contains(others, p)):
		case !tool && (o.Layers != nil || !slices.Contains(got.Layers, p) || slices.Contains(def.Layers.Names, p)):
		default:
			gave = append(gave, p)
		}
	}
	return gave
}

// now is the folder's devcontainer.json as it is now, for a VM made
// from was: its path ("" for none), its contents and its SHA-256
// (devcontainerUnreadable for one that can't be read), and whether it's
// the one the VM was made from.
func (e devcontainers) now(was devcontainerInVM, dir string) (file string, data []byte, sum string, same bool, err error) {
	file, data, err = e.find(dir)
	switch {
	case err != nil:
		sum = devcontainerUnreadable
	case file != "":
		sum = devcontainer.Sum(data)
	}
	return file, data, sum, file == was.File && sum == was.Sum, err
}

// check warns, once a launch, when the folder's devcontainer.json isn't
// the one the VM was made from (changed, gone, or new): nothing in it
// changes an existing VM until devvm update makes it again from the
// file (reread).
func (e devcontainers) check(st *state.VM, dir string) {
	was := sectionOf[devcontainerInVM](st, devcontainerSection)
	if was.Off {
		return
	}
	file, _, _, same, _ := e.now(was, dir)
	switch {
	case same:
	case was.File == "":
		e.Warn(msg.DevcontainerNew(file, st.Name()))
	default:
		e.Warn(msg.DevcontainerChanged(was.File, st.Name()))
	}
}

// reread reads the folder's devcontainer.json again for devvm update,
// when it isn't the one t's VM was made from (changed, gone, or new), and
// returns what the VM is made again with: its image's name and what
// rereadInVM holds (its tools' files are found once that image is
// built). What the file gave the VM is replaced by what it says now
// (devcontainer.Reread for its parts, filePorts for its ports), and what
// the user chose is kept: --from over its image, and the VM's own ports.
// A file whose image is gone leaves the VM's base starting
// from defaultFrom ($DEVVM_FROM's). nil when the file is the one the VM
// was made from, or the VM was made with --no-devcontainer; an
// unreadable file, or one Parse refuses, fails.
func (e devcontainers) reread(ctx context.Context, t *target, defaultFrom string) (string, *rereadInVM, error) {
	st, vm := t.st, t.f.Name
	was := sectionOf[devcontainerInVM](st, devcontainerSection)
	if was.Off {
		return "", nil, nil
	}
	file, data, _, same, err := e.now(was, t.f.Dir)
	switch {
	case err != nil:
		return "", nil, msg.DevcontainerUnreadable(file).Because(err)
	case same:
		return "", nil, nil
	}
	var p devcontainer.Plan
	if file == "" {
		e.Info(msg.DevcontainerGone(was.File, vm))
	} else {
		if p, err = devcontainer.Parse(file, data, t.f.Dir); err != nil {
			return "", nil, err
		}
		e.Info(msg.DevcontainerReread(file, vm))
		for _, m := range p.Notes {
			e.Warn(m)
		}
	}
	r := &rereadInVM{From: st.From}
	d := devcontainerInVM{File: p.File, Sum: p.Sum, Env: p.Env, Create: p.Create, Start: p.Start, FromFlag: was.FromFlag, Image: was.Image, Parts: p.Layers}
	switch {
	case was.FromFlag:
		if p.Image != "" {
			e.Warn(msg.DevcontainerFlagWins(file, "from", "image"))
		}
	case p.Image == was.Image:
	case p.Image == "":
		r.From, d.Image = defaultFrom, ""
	default:
		if r.From, err = e.pin(ctx, file, p.Image); err != nil {
			return "", nil, err
		}
		d.Image = p.Image
	}
	cur := sectionOf[ports.Ports](st, ports.Section).Forward
	if r.Forward, d.PortsGave, err = filePorts(cur, was.Ports, was.PortsGave, p.Ports); err != nil {
		return "", nil, err
	}
	d.Ports = portList(p.Ports)
	img, names, gave := rereadParts(st, was, p.Layers)
	d.Gave = gave
	r.Tools.Names, r.Devcontainer = names, d
	return img, r, nil
}

// portList is a Plan's ports list (3000,5173) as its ports, each on its
// own; none for "".
func portList(list string) []string {
	if list == "" {
		return nil
	}
	return strings.Split(list, ",")
}

// rereadParts is st's VM's image and shared tools once what was, its
// devcontainer section, gave it of now, what the file's features map to
// now, is replaced (devcontainer.Reread), and what the VM then has from
// the file alone.
func rereadParts(st *state.VM, was devcontainerInVM, now []string) (string, []string, []string) {
	img, names := image.Name(st.Image), slices.Clone(tools.Names(st))
	add, drop, gave := devcontainer.Reread(was.Parts, was.Gave, slices.Concat(img.Layers(), names), now)
	var addLayers, dropLayers []string
	for _, p := range add {
		if image.IsTool(p) {
			names = append(names, p)
		} else {
			addLayers = append(addLayers, p)
		}
	}
	for _, p := range drop {
		if image.IsTool(p) {
			names = slices.DeleteFunc(names, func(n string) bool { return n == p })
		} else {
			dropLayers = append(dropLayers, p)
		}
	}
	img, _ = removedImage(img, dropLayers)
	img, _ = addedImage(img, addLayers)
	return img.String(), image.SortTools(names), gave
}

// run runs a VM's devcontainer.json commands, once it's set up: those
// that run once (onCreateCommand, updateContentCommand,
// postCreateCommand) at its first, then postStartCommand once each start,
// at the first launch that finds it started. They run in the VM, as its
// user, in the project folder, carrying agent, the VM's; one that fails
// is said, and those after it in its group don't run.
func (e devcontainers) run(ctx context.Context, b backend.Backend, st *state.VM, dir, agent string) error {
	at := backend.ExecOpts{Dir: dir, Agent: agent}
	d := sectionOf[devcontainerInVM](st, devcontainerSection)
	if len(d.Create) > 0 && !d.Made.Created {
		// Recorded first: a run cut short never runs them again.
		if err := update(ctx, st, devcontainerSection, func(d *devcontainerInVM) error { d.Made.Created = true; return nil }); err != nil {
			return err
		}
		if err := e.runAll(ctx, b, st.Name(), at, d.Create); err != nil {
			return err
		}
	}
	if len(d.Start) == 0 {
		return nil
	}
	run, err := b.Running(ctx, st.Name())
	if err != nil || run.ID == "" {
		return err
	}
	id := run.ID
	sum := sha256.Sum256([]byte(id))
	started := hex.EncodeToString(sum[:16])
	if d.Made.Started == started {
		return nil
	}
	if err := update(ctx, st, devcontainerSection, func(d *devcontainerInVM) error { d.Made.Started = started; return nil }); err != nil {
		return err
	}
	return e.runAll(ctx, b, st.Name(), at, d.Start)
}

// runAll runs commands in the VM in order, in at's folder with its agent,
// their output in the run log, until one fails.
func (e devcontainers) runAll(ctx context.Context, b backend.Backend, vm vmname.VM, at backend.ExecOpts, cmds []devcontainer.Command) error {
	for _, c := range cmds {
		e.Info(msg.RunningScript(c.Name, vm.String()))
		e.LogNote()
		out := e.vmLog()
		argv := append([]string{"bash", "-lc", commandScript, "devvm"}, c.Argv...)
		o := at
		o.Stdout, o.Stderr = out, out
		stop := e.Status(msg.RunningScript(c.Name, vm.String()))
		err := b.Exec(ctx, vm, argv, o)
		stop()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			e.Warn(msg.DevcontainerCommandFailed(c.Name, vm))
			return nil
		}
	}
	return nil
}

// devcontainerEnv is the exports of a VM's devcontainer.json variables,
// for its environment file; a name that isn't one is left out.
func devcontainerEnv(st *state.VM) (string, error) {
	d, err := state.Section[devcontainerInVM](st, devcontainerSection)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	for _, v := range d.Env {
		if shell.EnvName(v.Name) {
			out.WriteString("export " + v.Name + "=" + v.Value + "\n")
		}
	}
	return out.String(), nil
}
