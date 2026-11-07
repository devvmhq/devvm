package vm

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/build"
	"github.com/devvmhq/devvm/internal/devcontainer"
	"github.com/devvmhq/devvm/internal/from"
	"github.com/devvmhq/devvm/internal/gitprotect"
	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/netpolicy"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/ports"
	"github.com/devvmhq/devvm/internal/project"
	"github.com/devvmhq/devvm/internal/settings"
	"github.com/devvmhq/devvm/internal/sshagent"
	"github.com/devvmhq/devvm/internal/sshconfig"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/tools"
	"github.com/devvmhq/devvm/internal/trust"
)

// Options is what up was asked for.
type Options struct {
	// Toolchains, Agents and Layers are --tools, --agents and --layers:
	// each the exact list of its part a new VM gets, empty for none; nil
	// for the part's default.
	Toolchains, Agents, Layers *[]string
	Network                    netpolicy.Tier // --network: a new VM's tier, "" for its default
	Size                       Size           // --cpus, --memory, --disk as given
	Ports                      string         // --ports as given
	UnsafeFolder               bool           // --unsafe-folder
	NoGitProtect               bool           // --no-git-protect
	Clone                      bool           // --clone
	SSHAgent                   bool           // --ssh-agent: a new VM forwards the user's SSH agent
	From                       string         // --from, or its default: the container image a new VM's base starts from
	FromSet                    bool           // --from was given, not its default
	NoDevcontainer             bool           // --no-devcontainer: a new VM ignores the folder's devcontainer.json
}

// Launch opens a shell, ssh or an editor in the folder's VM: a new VM
// made first (here), one that exists launched as it is, with what's
// changed since (existing.go).
type Launch struct{ *Env }

// Up opens a shell in the folder's VM, making it first: checked, from
// its image (built if it isn't), with only the folder mounted, .git
// protected, started; with --clone, nothing mounted but its own clone of
// the repo (seedClone). A VM that exists is opened as it is, with what's
// changed since checked (open), and one whose remake didn't finish is
// finished first (remaking.finishPending). Which VM it is, the folder's
// own or a parent folder's opened in this folder, is the finder's (e.Folder);
// --clone asks for the folder's own, as --here does. On success it
// doesn't return: the shell takes devvm's place.
func (e Launch) Up(ctx context.Context, o Options) error {
	if err := from.Check(o.From); err != nil {
		return err
	}
	// --clone asks for a new VM of the folder's own: as --here. One that
	// has a VM (or a remake of it pending) has one made for this folder.
	t, err := e.finder.target(ctx, need{own: o.Clone, folder: true})
	if err != nil {
		return err
	}
	e.finder.warnMounted(t, o.Clone)
	// A remake that didn't finish, whose machine may be gone: finished,
	// never made anew.
	if remakePending(t.st) {
		return e.finishRemake(ctx, o, t)
	}
	// None, or the folder's own whose creation didn't finish, which
	// another devvm may be making now: made under the folder's creation
	// lock.
	if !t.exists || !t.st.Finished() && t.sub == "" {
		return e.makeHeld(ctx, o, t)
	}
	return e.openMade(ctx, o, t)
}

// openMade opens t, a VM that's made. One whose creation didn't finish,
// kept, is refused before anything is checked or said, as any command
// that needs it made refuses it.
func (e Launch) openMade(ctx context.Context, o Options, t *target) error {
	if err := (need{has: hasMade}).check(t); err != nil {
		return err
	}
	return e.open(ctx, o, t)
}

// makeHeld makes the folder's VM, t, holding the folder's creation lock
// (holdCreate) from before the plan's questions until its creation is
// recorded, and refuses while another devvm holds it. Under the lock it
// finds the VM again (finder.again), as another devvm may have made it
// since t was found: one made meanwhile is opened as it is. A VM whose
// creation didn't finish is deleted and made again only when the user
// says so (remakeUnfinished). After an interrupt the lock is let go of
// last, once create's cleanup has deleted what it made.
func (e Launch) makeHeld(ctx context.Context, o Options, t *target) error {
	release, err := holdCreate(e.Env, t.f.Name)
	if err != nil {
		return err
	}
	done := e.AddCleanup(func(context.Context) { release() })
	unhold := func() { release(); done() }
	defer func() {
		if !e.Interrupted() {
			unhold()
		}
	}()
	if t, err = e.finder.again(ctx, t); err != nil {
		return err
	}
	switch {
	case remakePending(t.st):
		unhold()
		return e.finishRemake(ctx, o, t)
	case !t.exists:
		// The company proxy its builds and its proxy go through, checked
		// before anything is built.
		if _, err := e.Proxy.Upstream(ctx); err != nil {
			return err
		}
		return e.creation.create(ctx, o, t.f, t.b, unhold)
	case remakeUnfinished(e.Env, t):
		if err := e.lifecycle.delete(ctx, t); err != nil {
			return err
		}
		return e.creation.create(ctx, o, t.f, t.b, unhold)
	}
	unhold()
	return e.openMade(ctx, o, t)
}

// finishRemake finishes t's VM's remake that didn't (remaking.finishPending),
// holding its remake lock, unless another devvm finished it meanwhile,
// then opens it.
func (e Launch) finishRemake(ctx context.Context, o Options, t *target) error {
	release, err := e.remaking.hold(ctx, t)
	if err != nil {
		return err
	}
	defer release()
	if remakePending(t.st) {
		if err := e.remaking.finishPending(ctx, t); err != nil {
			return err
		}
	}
	return e.open(ctx, o, t)
}

// remakeUnfinished reports whether up deletes t, a VM whose creation
// didn't finish, and makes it again: asked ([y/N]), and no where it can't
// be. It never ran a shell, so nothing of the user's is in it. A finished
// VM is opened; an unfinished one not asked about (a parent folder's, or
// unasked) or kept is refused (vm_unfinished).
func remakeUnfinished(e *Env, t *target) bool {
	if t.st.Finished() || t.sub != "" {
		return false
	}
	yes, err := e.Ask(msg.RemakeUnfinished(t.f.Name).AnsweredBy("devvm rm, then devvm"))
	return err == nil && yes
}

// newVM is what create checked and chose for a new VM before building
// anything.
type newVM struct {
	img   image.Name // its image: base and its layers
	tools []string   // its shared tools
	size  Size
	ports string
	net   netpolicy.Tier
	agent string          // sshagent.On or Off
	clone bool            // --clone
	git   gitprotect.InVM // its .git protection (gitprotect.Protector.NewSection), or gitprotect.Clone's
	// from is the container image its base starts from: --from's, or its
	// devcontainer.json's image, pinned.
	from string
	// dc is what the folder's devcontainer.json says it gets
	// (devcontainers.prepare); nil without one.
	dc *devcontainer.Plan
	// dcGave is what it gets of its parts from the devcontainer.json
	// alone (fileParts), dcPorts of its ports (newPorts).
	dcGave, dcPorts []string
	spec            backend.VMSpec
	builder         *build.Env
	// history is the export of agents' history it gets once it's started,
	// as the user said yes to (offerHistory); "" for none.
	history string
}

// creation makes a new VM: its plan, its build and its cleanup.
type creation struct{ Launch }

// create makes the folder's VM and opens a shell in it, calling release
// once its creation is recorded, before its first start: the folder's
// creation lock is held until then (Launch.makeHeld).
func (e creation) create(ctx context.Context, o Options, f project.Folder, b backend.Backend, release func()) error {
	vm := f.Name
	n, err := e.plan(ctx, o, f, b)
	if err != nil {
		return err
	}
	// Held from before its tools are picked until it's made, so neither
	// devvm gc --tool-images nor retention deletes any of them meanwhile.
	unmaking, err := tools.Making{Dirs: e.Dirs, Reporter: e.Reporter}.Hold(ctx)
	if err != nil {
		return err
	}
	defer unmaking()
	// A new VM gets the newest tools: check now when the daily check is
	// due.
	versions := tools.Versions{Env: e.Tools}
	if len(n.tools) > 0 && versions.CheckDue() {
		versions.Check(ctx)
	}
	if err := (build.Builds{Env: n.builder}).Build(ctx, n.img); err != nil {
		return err
	}
	chain, err := n.builder.Chain(n.img)
	if err != nil {
		return err
	}
	tg, err := n.builder.ToolTarget()
	if err != nil {
		return err
	}
	files, err := versions.Files(ctx, n.tools, tg)
	if err != nil {
		return err
	}
	if err := e.buildTools(ctx, n.builder, tg, files); err != nil {
		return err
	}
	if err := os.MkdirAll(e.Dirs.Tools(), 0o700); err != nil {
		return err
	}
	id, err := platform.FolderID(f.Dir)
	if err != nil {
		return err
	}

	e.Info(msg.CreatingFrom(n.img.String(), vm))
	e.LogNote()
	// What a VM made for this folder before, deleted some other way, left
	// in its folder: the new one's state replaces it.
	if err := e.vmFolders.clear(ctx, vm); err != nil {
		return err
	}
	var locking atomic.Bool
	done := e.AddCleanup(e.undoCreate(b, f, &locking))
	// Its whole state first, written once, with all it was asked for: if
	// a later step fails, it never starts, as its creation didn't finish
	// (Created).
	st, err := state.New(e.Dirs, vm)
	if err != nil {
		return err
	}
	st.Image, st.Dir, st.Folder, st.Chain, st.From, st.Backend = n.img.String(), f.Dir, id, chain.String(), n.from, b.Name()
	st.MadeBy = e.Version
	st.BackendAt, _ = backend.LocationOf(b)
	if err := n.record(st, tg, files); err != nil {
		return err
	}
	if err := e.devcontainers.record(st, o, n.dc, n.dcGave, n.dcPorts); err != nil {
		return err
	}
	// Its alias, chosen now for good, as its name was, under the claims
	// lock: two VMs made at once never take one.
	err = st.Create(ctx, func(st *state.VM) error {
		var err error
		st.Alias, err = sshconfig.ChooseAlias(e.Dirs, vm)
		return err
	})
	if err != nil {
		return err
	}
	if err := e.makeMachine(ctx, b, st, chain.Key(), n.spec); err != nil {
		return err
	}
	// Made: another devvm in the folder opens it from now on, and its
	// start's questions never hold that one up.
	release()
	// A clone has no .git of the host's to protect, and no origin
	// (hostChecks).
	locking.Store(!o.Clone && !o.NoGitProtect)
	// Started as every start does, its folder checked as a new VM's,
	// before the setup and the shell.
	t := e.newTarget(f, st, b)
	started, err := e.starter.bringUp(ctx, t, upOpts{made: true})
	if err != nil {
		return err
	}
	// The agents' history it was offered, before its setup, which writes
	// what the import leaves out.
	if err := e.importOffered(ctx, t, n.history); err != nil {
		return err
	}
	if err := e.setUpVM(ctx, t, started); err != nil {
		return err
	}
	cmd, err := t.shell(ctx)
	if err != nil {
		return err
	}
	done()
	unmaking()
	return e.Exec(cmd)
}

// machineSpec is the machine of f's VM as devvm makes it, new
// (creation.plan) or again (remaking.spec): the project folder mounted,
// or with clone its clone's place; its agents' folders on the host
// (those of names, its shared tools), read-only; the tools share,
// read-only, from which a tool added later is mounted; its SSH agent
// forwarded or not, its size; and, where b lets devvm ssh-agent change
// that later (an emulation once it's reviewed), made ready for it,
// otherwise fixed as it's made. Its ssh port is set as it's made
// (makeMachine), and its ports are forwarded as it starts.
func (e creation) machineSpec(b backend.Backend, f project.Folder, names []string, agent bool, size Size, clone bool) backend.VMSpec {
	spec := backend.VMSpec{
		Name: f.Name, Project: f.Dir, AgentDirs: e.AgentMounts(names), Tools: e.Dirs.Tools(), ToolsAt: tools.ShareAt,
		AgentForwarding: agent, Size: size.Backend(),
		Env: trust.GuestEnv(nil),
	}
	if clone {
		spec.Project, spec.CloneAt = "", f.Dir
	}
	// Let through or not as the requirements' check has it, which tells
	// of an unreviewed emulation once.
	spec.AgentToggle = e.Registry.Allowed(b, backend.FeatureAgentToggle)
	return spec
}

// makeMachine has b make st's VM's machine as spec says, from the image
// whose build chain's key is key: its ssh port first, as its starts have
// it (none of its listed ports, recorded), then the machine, then that
// it was made recorded (created).
func (e creation) makeMachine(ctx context.Context, b backend.Backend, st *state.VM, key string, spec backend.VMSpec) error {
	var err error
	if spec.SSHPort, err = e.Proxy.Ports().SSH(ctx, st, false); err != nil {
		return err
	}
	// The folder itself, which a backend may mount by it (Want.ProjectID).
	if spec.Project != "" {
		spec.ProjectID = st.Folder
	}
	if err := b.CreateVM(ctx, key, spec); err != nil {
		return err
	}
	return e.created(ctx, b, st, spec.AgentToggle)
}

// created records that the backend made the VM, with toggle when it was
// made ready for devvm ssh-agent to change its forwarding and the backend
// emulates that, which its starts rely on: from now on it starts.
func (e creation) created(ctx context.Context, b backend.Backend, st *state.VM, toggle bool) error {
	toggle = toggle && b.Caps()[backend.FeatureAgentToggle].IsEmulated()
	return st.Update(ctx, func(st *state.VM) error {
		st.Created = time.Now().UTC()
		if !toggle {
			return nil
		}
		a, err := sshagent.Of(st)
		if err != nil {
			return err
		}
		a.Made.Toggle = true
		return sshagent.Set(st, a)
	})
}

// firstRun asks devvm's first run's questions, new VMs' size and agents
// (firstRunSize, firstRunAgents), then the features of b's base, until
// they're chosen (build's Choose), all under a line saying they set the
// defaults for every new VM and base (first_run_defaults), before the
// summary of this folder's VM. It returns the defaults in effect, a
// snapshot the rest of the plan reads: the files may be edited by hand
// while it runs.
func (e creation) firstRun(o Options, size Size, b *build.Env) (settings.Defaults, error) {
	def, err := settings.Read(e.Dirs)
	if err != nil {
		return def, err
	}
	if !def.Features.Chosen || e.asksAgents(o, def) {
		e.Logo()
		e.Info(msg.FirstRunDefaults())
	}
	if err := e.firstRunSize(size, def); err != nil {
		return def, err
	}
	if err := e.firstRunAgents(o, def); err != nil {
		return def, err
	}
	if err := (build.Choice{Env: b}).Choose(); err != nil {
		return def, err
	}
	return settings.Read(e.Dirs)
}

// unlockedAnyway asks, before a new VM is made without .git protection
// because devvm-immutable is installed but doesn't work (no capability in
// a sandbox with no_new_privs, a check that timed out), whether to make it
// so: a yes is the user's choice of off (g's Chosen), a no fails, and
// where devvm can't ask it refuses, naming --no-git-protect. A helper
// that isn't installed, or a file system without the attribute, is the
// documented fallback, which only warns.
func unlockedAnyway(r msg.Reporter, p *gitprotect.Protector, g *gitprotect.InVM) error {
	if g.Mode != gitprotect.Off || g.Chosen || !errors.Is(p.Unable, platform.ErrHelperUnusable) {
		return nil
	}
	yes, err := r.Ask(msg.GitProtectHelperBrokenAsk().AnsweredBy("--no-git-protect"))
	switch {
	case err != nil:
		return err
	case !yes:
		return msg.GitProtectHelperBroken()
	}
	g.Chosen = true
	return nil
}

// plan checks and chooses what a new VM gets, all before the build,
// which can take minutes: devvm's first run's questions (firstRun), then
// its image, size, ports and network tier, shown in its summary and
// customized when the user says so (choose), its SSH agent, its .git
// protection (unlockedAnyway) and the backend features it needs, with
// what the folder's devcontainer.json says (its From and its plan, which
// the new VM keeps: o stays as it was asked for).
func (e creation) plan(ctx context.Context, o Options, f project.Folder, b backend.Backend) (newVM, error) {
	vm := f.Name
	size, err := o.Size.Parse()
	if err != nil {
		return newVM{}, err
	}
	if o.Clone {
		if err := checkCloneRepo(ctx, f.Dir); err != nil {
			return newVM{}, err
		}
	} else if err := e.checkUnsafe(f.Dir, o.UnsafeFolder); err != nil {
		return newVM{}, err
	}
	n := newVM{agent: sshagent.Off}
	if n.from, n.dc, err = e.devcontainers.prepare(ctx, o, f.Dir); err != nil {
		return newVM{}, err
	}
	n.builder = e.Builder(b, n.from)
	def, err := e.firstRun(o, size, n.builder)
	if err != nil {
		return newVM{}, err
	}
	n.size = size.Or(defaultSize(def))
	host, err := e.Host()
	if err != nil {
		return newVM{}, err
	}
	if err := n.size.Check(host, backend.BaseDiskGiB, vm); err != nil {
		return newVM{}, err
	}
	if n.ports, n.dcPorts, err = newPorts(o.Ports, def, n.dc); err != nil {
		return newVM{}, err
	}
	parts, err := e.choose(o, f, b, def, host, &n)
	if err != nil {
		return newVM{}, err
	}
	forward, err := ports.Forwarding(n.ports)
	if err != nil {
		return newVM{}, err
	}
	n.img, n.tools = parts.image(), parts.tools()
	n.dcGave = fileParts(o, f.Dir, n.dc, def, parts)
	n.spec = e.machineSpec(b, f, n.tools, o.SSHAgent, n.size, o.Clone)
	if o.SSHAgent {
		n.agent = sshagent.On
	}
	if o.Clone {
		n.clone, n.git = true, gitprotect.InVM{Mode: gitprotect.Clone}
	} else {
		p := e.Protector(ctx, f.Dir)
		n.git = p.NewSection(o.NoGitProtect)
		if err := unlockedAnyway(e.Reporter, p, &n.git); err != nil {
			return newVM{}, err
		}
	}
	// Its ports, which devvm's forwarder forwards from the backend's ssh,
	// and a clone, copied in and fetched over ssh.
	need := needs{spec: &n.spec, ports: forward, img: n.img, ssh: o.Clone, net: n.net.Net()}
	if err := e.Registry.Require(b, vm, requirements(need)...); err != nil {
		return newVM{}, err
	}
	// Room for the VM, before anything is built or made; each build checks
	// its own.
	if err := (backend.Space{Free: e.FreeSpace}).Check(b, vm, func(f backend.Floors) uint64 { return f.VM }); err != nil {
		return newVM{}, err
	}
	n.history = e.offerHistory(vm, f.Dir, n.tools)
	return n, nil
}

// newPorts returns the ports a new VM forwards: given's (--ports,
// parsed), which win over its devcontainer.json's (dc), else def's with
// the file's added, as devvm ports add adds them (filePorts), and the
// file's ports it added (gave).
func newPorts(given string, def settings.Defaults, dc *devcontainer.Plan) (list string, gave []string, err error) {
	if given != "" {
		list, err = settings.ParsePorts(given)
		return list, nil, err
	}
	if dc == nil || dc.Ports == "" {
		return def.Ports.Value, nil, nil
	}
	return filePorts(def.Ports.Value, nil, nil, dc.Ports)
}

// chooseNet returns the tier a new VM's summary shows: --network's, else
// the default's (config.toml's, else devvm network -g's or devvm
// setup's, else allowlist), which customizing it may change.
// A backend b that may not confine a VM to its proxy (egress-proxy)
// offers open alone: any other tier is refused in one line
// (net_tier_backend), unasked, never dropped.
func (e creation) chooseNet(o Options, b backend.Backend, vm, saved string) (netpolicy.Tier, error) {
	tier := o.Network
	if tier == "" {
		tier = netpolicy.Tier(saved)
	}
	if tier != netpolicy.TierOpen && !e.Registry.Allowed(b, backend.FeatureEgressProxy) {
		return "", msg.NetTierBackend(b.Name(), vm)
	}
	return tier, nil
}

// choose works out a new VM's parts, as its flags and defaults have them
// (partsFor), and its tier (chooseNet), shows its summary with its size
// and ports (n's), and asks whether to customize it: only where the
// picker shows (CanPick) and something is left to choose (leftToChoose).
// Enter, or where it can't ask, makes it as shown; yes customizes it
// (customize). Where the question isn't asked, the summary ends with the
// flags that choose others.
func (e creation) choose(o Options, f project.Folder, b backend.Backend, def settings.Defaults, host Host, n *newVM) (Parts, error) {
	vm := f.Name
	d, p, err := e.partsFor(o, vm, f.Dir, n.dc, n.builder, def)
	if err != nil {
		return Parts{}, err
	}
	if n.net, err = e.chooseNet(o, b, vm, def.Network.Value); err != nil {
		return Parts{}, err
	}
	s := summary{
		vm: vm, o: o, d: d, p: p, agentsFrom: chosenBy(def.Agents.Source), layersFrom: chosenBy(def.Layers.Source),
		size: shownSize(n.size, host), tier: n.net, ports: n.ports,
	}
	if n.dc != nil {
		s.dc = n.dc.File
	}
	lines := s.lines()
	e.Logo() // once a run: after the first run's questions, not again
	e.Info(lines[0])
	for _, m := range lines[1:] {
		e.Say(m)
	}
	// Customizing asks for the tier only without --network, where the
	// backend may confine the VM.
	tierOpen := o.Network == "" && e.Registry.Allowed(b, backend.FeatureEgressProxy)
	switch {
	case !leftToChoose(o, tierOpen):
		return p, nil
	case !e.CanPick() || e.PickParts == nil:
		e.Say(msg.NewVMChoose())
		return p, nil
	}
	// Refused, as when the input ends, it's no: the VM made as shown.
	if yes, _ := e.Ask(msg.NewVMCustomize().AnsweredBy("--tools, --agents, --layers, --network, --ports, --cpus, --memory and --disk")); !yes {
		return p, nil
	}
	return e.customize(s, host, tierOpen, n)
}

// customize asks for what of a new VM no flag gave, starting from its
// summary s: the picker, a part a flag gave greyed out (skipped when the
// flags give every part), then its tier when tierOpen (chooseTier, from
// the one the summary showed), then its ports without --ports
// (choosePorts), then its CPUs, memory and disk (AskVM), each into n. It returns the parts chosen,
// checked as the defaults are.
func (e creation) customize(s summary, host Host, tierOpen bool, n *newVM) (Parts, error) {
	p := s.p
	if !allGiven(s.o) {
		var err error
		if p, err = e.checkParts(s.vm, e.pickNew(s.vm, s.o, s.d, p, n.builder), s.d, n.builder); err != nil {
			return Parts{}, err
		}
	}
	if tierOpen {
		n.net = e.chooseTier(s.vm, n.net)
	}
	if s.o.Ports == "" {
		if chosen := e.choosePorts(s.vm, n.ports); chosen != n.ports {
			// The devcontainer.json's ports are no longer what it added.
			n.ports, n.dcPorts = chosen, nil
		}
	}
	n.size = SizeChoice{Dirs: e.Dirs, Host: host, Reporter: e.Reporter}.AskVM(n.size, s.size, s.o.Size, s.vm)
	return p, n.size.Check(host, backend.BaseDiskGiB, s.vm)
}

// chooseTier shows the tiers in order, each with what it reaches, for
// one to be chosen, starting from shown, the one the summary showed;
// where the picker can't show, it's shown.
func (e creation) chooseTier(vm string, shown netpolicy.Tier) netpolicy.Tier {
	if e.PickOne == nil {
		return shown
	}
	rows := tierRows(shown)
	return netpolicy.Tiers()[e.PickOne(msg.NetTierHelp(vm), rows)]
}

// tierRows are the tier picker's rows: every tier, lowest to highest,
// with what it reaches, shown on.
func tierRows(shown netpolicy.Tier) []PickRow {
	tiers := netpolicy.Tiers()
	rows := make([]PickRow, len(tiers))
	for i, t := range tiers {
		rows[i] = PickRow{Name: string(t), Desc: t.Desc(), On: t == shown}
	}
	return rows
}

// record sets a new VM's choices in its state, before it's written, with
// the target and tool files it was given: each package's section.
func (n newVM) record(st *state.VM, tg tools.Target, files []string) error {
	forward := n.ports
	if forward == settings.PortsAll {
		forward = ""
	}
	for _, err := range []error{
		netpolicy.Set(st, netpolicy.Net{Tier: n.net}),
		gitprotect.Set(st, n.git),
		sshagent.Set(st, sshagent.InVM{Mode: n.agent}),
		tools.Set(st, tools.InVM{Names: n.tools, Made: tools.Made{Files: files, Libc: tg.Libc, Glibc: tg.Glibc}}),
		ports.Set(st, ports.Ports{Forward: forward}),
		state.SetSection(st, sizeSection, n.size),
		state.SetSection(st, cloneSection, cloneInVM{On: n.clone}),
	} {
		if err != nil {
			return err
		}
	}
	return nil
}

// undoCreate is create's cleanup. Until its first shell nothing of
// anyone's is in the VM: an interrupt deletes it, then what devvm keeps
// for it (teardown), and takes off .git's locks (once locking is set,
// even when its state can't be read), or removes the repo's remote for
// its clone.
func (e creation) undoCreate(b backend.Backend, f project.Folder, locking *atomic.Bool) func(context.Context) {
	vm := f.Name
	return func(ctx context.Context) {
		if !e.Interrupted() {
			return
		}
		e.Info(msg.InterruptedVM(vm))
		deleted := true
		if err := b.Delete(ctx, vm); err != nil && !errors.Is(err, backend.ErrNotFound) {
			e.Warn(msg.DeleteFailed(vm))
			deleted = false
		}
		st, err := state.ReadVM(e.Dirs, vm)
		if err != nil {
			st = new(state.VM)
		}
		if m := gitprotect.Mode(st); locking.Load() && m != gitprotect.On && m != gitprotect.Strict {
			e.Protector(ctx, f.Dir).Unlock(ctx, st)
		}
		if deleted {
			_ = e.vmFolders.teardown(ctx, e.newTarget(f, st, b), false)
		}
	}
}

// setUpVM sets up a running VM, started with conf: its setup sent, ssh
// and editors reaching it by name, its links carried (linkRuns.atLaunch),
// a --clone VM's clone made if it isn't yet, then its devcontainer.json's
// commands run (devcontainers.run).
func (e Launch) setUpVM(ctx context.Context, t *target, conf backend.Confinement) error {
	b, st, f := t.b, t.st, t.f
	if err := e.syncer.sync(ctx, b, st, f.Slug(), tools.Files(st), conf); err != nil {
		return err
	}
	if err := e.writeSSHConfig(ctx); err != nil {
		return err
	}
	// Its links, once its host key is pinned, which their session checks.
	if err := e.linkRuns.atLaunch(ctx, b, st); err != nil {
		return err
	}
	if c := sectionOf[cloneInVM](st, cloneSection); c.On && !c.Made.Seeded {
		if err := e.clones.seedClone(ctx, t); err != nil {
			return err
		}
	}
	return e.devcontainers.run(ctx, b, st, f.Dir, conf.AgentSocket)
}

// checkUnsafe refuses a folder that is or holds the home folder or
// devvm's state, or is inside devvm's own folders, unless the user means
// it (unsafe).
func (e Launch) checkUnsafe(dir string, unsafe bool) error {
	u := project.Unsafe{Home: e.Dirs.Home, Config: e.Dirs.Config, Data: e.Dirs.Data, Lima: e.Dirs.Lima}
	if holds := u.Holds(dir); holds != "" {
		if !unsafe {
			return msg.UnsafeFolder(dir, holds)
		}
		e.Warn(msg.UnsafeFolderUsed(dir, holds))
	}
	if inside := u.Inside(dir); inside != "" {
		if !unsafe {
			return msg.UnsafeFolderInside(dir, inside)
		}
		e.Warn(msg.UnsafeFolderUsed(dir, inside))
	}
	return nil
}
