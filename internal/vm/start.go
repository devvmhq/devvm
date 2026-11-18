package vm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/gitprotect"
	"github.com/devvmhq/devvm/internal/hostsafety"
	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/netpolicy"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/ports"
	"github.com/devvmhq/devvm/internal/sshagent"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/tools"
	"github.com/devvmhq/devvm/internal/trust"
	"github.com/devvmhq/devvm/internal/vmname"
	"github.com/devvmhq/devvm/internal/vmssh"
)

// starter is the one way devvm starts a project VM or goes on with one
// that runs (bringUp), whichever command it's for: a launch, ssh's, a
// command that runs in it, a remake's new machine and a new VM.
type starter struct{ *Env }

// upOpts is what a start path asks of bringUp beyond what every start
// does.
type upOpts struct {
	// made: the VM was just made (creation), so its folder gets a new
	// VM's checks (hostChecks).
	made bool
	// agentDirs: its agent folder mounts are matched to the host's first
	// (Launch.updateAgentDirs), and a running VM that gets changes at
	// its next start offers to restart (restartForNextStart), as a launch
	// does.
	agentDirs bool
}

// bringUp takes t's VM, made and finished, to running, confined as its
// state says, in these steps: its instance read once; its project
// folder's identity recorded anew where a boot renumbered its disk
// (renumbered); for a launch, its
// agent folder mounts matched to the host's, and a running VM that gets
// changes at its next start stopped once the user agrees
// (restartForNextStart); its
// project folder checked on the host (hostChecks), and on a host without
// .git locks, a warning that its .git is unprotected (WarnUnprotected),
// and one of a cache mode no one chose that can lose writes
// (warnMountCache); what it needs of its
// backend refused where the backend can't (requirements); the user's
// agent checked for keys when it forwards it; then a stopped VM started
// (start), or a running one gone on with as it runs (runningConfinement),
// what its next start changes said (nextStart); then the self-check,
// which stops a VM whose network tier doesn't hold; last, its host keys
// pinned for its current start (pinHostKeys). It returns the confinement
// the VM runs with.
func (s starter) bringUp(ctx context.Context, t *target, o upOpts) (backend.Confinement, error) {
	vm, st, b := t.f.Name, t.st, t.b
	if err := refuseUnfinished(vm, st); err != nil {
		return backend.Confinement{}, err
	}
	// A size its state can't hold, refused before anything runs (startOnce reads it again).
	if _, err := storedSize(st); err != nil {
		return backend.Confinement{}, err
	}
	if err := s.instance(ctx, t, o); err != nil {
		return backend.Confinement{}, err
	}
	if err := renumbered(ctx, st); err != nil {
		return backend.Confinement{}, err
	}
	mountsLater := false
	if o.agentDirs {
		var err error
		mountsLater = s.launcher.updateAgentDirs(st, t.in)
		if mountsLater, err = s.restartForNextStart(ctx, t, mountsLater); err != nil {
			return backend.Confinement{}, err
		}
	}
	in := t.in
	running := in.Status == backend.StatusRunning
	// Its host checks act on the folder, so never on what replaced it.
	if !running && folderChanged(st) {
		return backend.Confinement{}, msg.ProjectFolderChanged(st.Dir, vm)
	}
	if err := s.hostChecks(ctx, t, running, o.made); err != nil {
		return backend.Confinement{}, err
	}
	s.Protector(t.f.Dir).WarnUnprotected(st)
	s.warnMountCache(st, in)
	s.warnOldSSH(ctx)
	s.warnSSHListed(sectionOf[ports.Ports](st, ports.Section).Forward)
	need, err := startNeeds(st, in)
	if err != nil {
		return backend.Confinement{}, err
	}
	need.pins = s.pinsLocks(st)
	if err := s.refuse(b, vm, requirements(need)); err != nil {
		return backend.Confinement{}, err
	}
	if sshagent.Mode(st) == sshagent.On {
		s.launcher.checkAgent(ctx)
	}
	conf := confinementFor(st)
	start := func(ctx context.Context) error {
		var err error
		conf, err = s.start(ctx, t, confinementFor(st))
		return err
	}
	if running {
		mode, err := runningAgent(ctx, b, st)
		if err != nil {
			return conf, err
		}
		if err := s.reloadAgent(ctx, mode); err != nil {
			return conf, err
		}
		if conf, err = s.runningConfinement(ctx, b, st, mode); err != nil {
			return conf, err
		}
		s.nextStart(st, in, conf, mountsLater)
		s.checkRunningPins(ctx, t)
		if err := s.checkMounted(ctx, t, conf.AgentSocket); err != nil {
			return conf, err
		}
	} else {
		s.Info(msg.Starting(vm.String()))
		if err := start(ctx); err != nil {
			return conf, err
		}
	}
	if err := s.Proxy.SelfCheck(ctx, b, st, start); err != nil {
		return conf, err
	}
	if err := s.pinHostKeys(ctx, b, vm); err != nil {
		return conf, err
	}
	s.remindLearning(st, o.made)
	// devvm's connections to it carry the agent it runs with.
	t.agent = conf.AgentSocket
	return conf, nil
}

// pinHostKeys pins the running VM's host keys for its current start
// (vmssh.PinHostKeys), whatever ports it forwards, once its start, or a
// launch of it running, has gone through: its forwarder pins them only
// when it has ports to forward, and devvm's own ssh (ownSSH), which a
// --clone VM's fetch and links' sessions take, only reads the record. A
// record of its current start already is kept, without reading the VM.
// A VM whose backend's ssh isn't usable has nothing to pin for, and one
// whose keys can't be pinned (vmssh.ErrNoHostKeys) runs on without them,
// as the ssh config leaves it out: what needs them then fails, naming the
// VM.
func (s starter) pinHostKeys(ctx context.Context, b backend.Backend, vm vmname.VM) error {
	if !s.Registry.Allowed(b, backend.FeatureSSH) {
		return nil
	}
	if _, err := vmssh.PinHostKeys(ctx, s.Dirs, b, vm); err != nil && !errors.Is(err, vmssh.ErrNoHostKeys) {
		return err
	}
	return nil
}

// remindLearning says, for a VM that learns the hosts it reaches, since
// when, how many it found that its allowlist doesn't allow, and the
// command that locks it to them; without the count when its log can't be
// read, or is too long to read at every launch (Log.Countable). A VM just
// made says it learns from now on.
func (s starter) remindLearning(st *state.VM, made bool) {
	lr := netpolicy.LearningOf(st)
	if !lr.On() {
		return
	}
	vm, since := st.Name(), learnedSince(lr)
	if made {
		s.Info(msg.NetLearnStarted(vm))
		return
	}
	if !netpolicy.NewLog(s.Dirs, vm).Countable() {
		s.Note(msg.NetLearningSince(since, vm))
		return
	}
	hosts, err := s.Proxy.Learned(st)
	if err != nil {
		s.Note(msg.NetLearningSince(since, vm))
		return
	}
	s.Note(msg.NetLearning(len(hosts.Hosts), since, vm))
}

// learnedSince is when a VM started learning, as devvm says it: this
// machine's time, to the minute.
func learnedSince(lr netpolicy.Learning) string { return lr.Since.Local().Format("2006-01-02 15:04") }

// instance sets t's VM as bringUp starts from it (t.in): its backend's
// answer, read again, as a remake or another devvm may have changed it
// since the finder read it, a VM its backend lacks refused; or for a VM
// just made (o.made), stopped, made as its state asks for its agent
// forwarding (creation.plan's spec).
func (s starter) instance(ctx context.Context, t *target, o upOpts) error {
	in := backend.Instance{Name: t.f.Name, Status: backend.StatusStopped, AgentForwarding: sshagent.Forwards(sshagent.Mode(t.st))}
	if !o.made {
		var err error
		if in, err = t.b.Inspect(ctx, t.f.Name); err != nil {
			return err
		}
	}
	t.in, t.exists = in, true
	return nil
}

// confinementFor is the confinement a VM starts with, as its tier says
// (Tier.Net): on its proxy alone on a confined tier, else on its
// backend's own network, with the proxy set in its environment.
func confinementFor(st *state.VM) backend.Confinement {
	return backend.Confinement{Net: netpolicy.TierOf(st).Net()}
}

// runningConfinement is the confinement a running VM runs with, its proxy
// made ready (Prepare): its proxy's alone when it runs on one, a tier
// made open while it ran confined included, as it stays on its proxy
// until it starts again, or its tier is confined; else its backend's own
// network, with the proxy. A VM whose proxy can't start is stopped
// (PrepareRunning): it never runs on without it; one refused before its
// proxy was touched runs on as it was. Its agent is its proxy's
// relay of the one its start forwarded, whatever start it is. One whose
// proxy relays none gets no agent, never the agent's own socket, whether
// devvm recorded its start as relayed (its proxy's config lost) or didn't
// record it (started outside devvm); only a start an older devvm recorded
// without the relay (sshagent.ReachOf's Raw) gets the one mode, the choice
// it runs with (runningAgent), says.
func (s starter) runningConfinement(ctx context.Context, b backend.Backend, st *state.VM, mode string) (backend.Confinement, error) {
	vm := st.Name()
	c, err := s.Proxy.PrepareRunning(ctx, b, st)
	if err != nil {
		return c, err
	}
	if c.AgentSocket == "" {
		run, err := b.Running(ctx, vm)
		if err != nil {
			return c, err
		}
		if sshagent.ReachOf(st, run.ID) == sshagent.Raw {
			c.AgentSocket = s.agentSocket(mode)
		}
	}
	return c, nil
}

// nextStart says what a running VM gets only at its next start, each
// where it differs from what its state asks for: its size, its SSH agent
// forwarding, its project folder mount's cache mode, its agent folder mounts (mountsLater: the user didn't
// restart it), its project folder's mount once the folder moved, and
// its tier made open while it ran confined (conf, as it runs).
// It warns of each change by name, then once that devvm restart applies
// them (next_start).
func (s starter) nextStart(st *state.VM, in backend.Instance, conf backend.Confinement, mountsLater bool) {
	cs := nextStartChanges(st, in, mountsLater, conf.Net == backend.NetProxy)
	if len(cs) == 0 {
		return
	}
	for _, c := range cs {
		s.Warn(c)
	}
	s.Warn(msg.NextStart(len(cs), st.Name()))
}

// projectMount is the project folder the VM's start mounts: its state's
// folder, where a move put it; none for a --clone VM.
func projectMount(st *state.VM) string {
	if isClone(st) {
		return ""
	}
	return st.Dir
}

// folderChanged reports whether the project folder the VM's start mounts
// (projectMount) isn't the folder its state records any more: a link now,
// or below one, or another folder in its place (platform.FolderID). A
// parent folder's VM mounts a nested project folder too, so code in it can
// replace the folder after the finder found the VM, and the backend
// mounts the path as it is then, following a link to anywhere on this
// machine. bringUp checks it before its host checks and start before and
// after the backend's start, each after renumbered recorded the folder's
// identity as it is now, so the identity must be the one recorded, byte
// for byte: no boot renumbers a disk within one run, and the backend
// mounts the folder by the record.
func folderChanged(st *state.VM) bool {
	dir := projectMount(st)
	if dir == "" {
		return false
	}
	if r, err := filepath.EvalSymlinks(dir); err != nil || r != dir {
		return true
	}
	id, err := platform.FolderID(dir)
	return err != nil || id != st.Folder
}

// renumbered records, under the VM's state lock, the identity its project
// folder has now, when a boot numbered the folder's disk anew since it was
// recorded (platform.SameFolder), before its start mounts the folder by
// it (startOnce, Want.ProjectID): the device's old number may be another
// disk's now. Whatever else differs is left for folderChanged to refuse,
// and a state another devvm changed meanwhile is compared again under the
// lock.
func renumbered(ctx context.Context, st *state.VM) error {
	dir := projectMount(st)
	if dir == "" {
		return nil
	}
	// A folder whose identity can't be read is folderChanged's to refuse.
	id, _ := platform.FolderID(dir)
	if !platform.Renumbered(st.Dir, st.Folder, dir, id) {
		return nil
	}
	return st.Update(ctx, func(st *state.VM) error {
		if projectMount(st) == dir && platform.Renumbered(st.Dir, st.Folder, dir, id) {
			st.Folder = id
		}
		return nil
	})
}

// nextStartChanges lists what a running VM, as in is, gets only at its
// next start, in nextStart's order, each as the line that names it;
// confined: it runs on its proxy.
func nextStartChanges(st *state.VM, in backend.Instance, mountsLater, confined bool) []msg.M {
	vm := st.Name()
	var cs []msg.M
	// A size the state can't hold is no change: the next start refuses it, naming it (storedSize).
	if p, err := pendingSize(st, in.Size); err == nil && p != (backend.Size{}) {
		cs = append(cs, msg.NextStartSize(vm))
	}
	if in.AgentForwarding != sshagent.Forwards(sshagent.Mode(st)) {
		cs = append(cs, msg.NextStartAgent(vm))
	}
	if c := cacheOf(st); c != "" && projectMount(st) != "" && in.ProjectCache != "" && in.ProjectCache != c {
		cs = append(cs, msg.NextStartMountCache(vm))
	}
	if mountsLater {
		cs = append(cs, msg.NextStartMounts(vm))
	}
	if in.Project != projectMount(st) {
		cs = append(cs, msg.NextStartFolder(st.Dir, vm))
	}
	if confined && !netpolicy.TierOf(st).Confined() {
		cs = append(cs, msg.NextStartNetOff(vm))
	}
	return cs
}

// nextStartContext is the context of restartForNextStart's question, at
// most two lines: a line naming each change, or past two, the first one
// and how many more wait (next_start_more).
func nextStartContext(cs []msg.M) []msg.M {
	if len(cs) > 2 {
		return []msg.M{cs[0], msg.NextStartMore(len(cs) - 1)}
	}
	return cs
}

// restartForNextStart asks once, for a running VM, as in is, that gets
// changes only at its next start (nextStartChanges), whether to restart
// it now (restartNow, the changes named first, nextStartContext). It's
// yes by default ([Y/n]) when its agent folder mounts wait, as an agent
// just added needs them, or its project folder moved, which it mounts
// where it was, and no ([y/N]) otherwise. A yes stops it, so
// that bringUp starts it with them; a no, or no question, leaves it
// running, and nextStart warns of each. t.in is the VM as it is then (a
// stop reads it again); it returns whether its agent folder mounts still
// wait (mountsLater).
func (s starter) restartForNextStart(ctx context.Context, t *target, mountsLater bool) (bool, error) {
	st, in := t.st, t.in
	if in.Status != backend.StatusRunning {
		return mountsLater, nil
	}
	vm := st.Name()
	confined := false
	if !netpolicy.TierOf(st).Confined() {
		// An error here is runningConfinement's to return.
		run, err := t.running(ctx)
		confined = err == nil && run.Confined
	}
	cs := nextStartChanges(st, in, mountsLater, confined)
	if len(cs) == 0 {
		return mountsLater, nil
	}
	ask := s.Ask
	if mountsLater || in.Project != projectMount(st) {
		ask = s.Confirm
	}
	if !s.restartNow(vm, ask, len(cs), nextStartContext(cs)...) {
		return mountsLater, nil
	}
	if err := t.stopToStart(ctx); err != nil {
		return mountsLater, err
	}
	return false, nil
}

// needs is what a VM asks of its backend, which requirements lists: each
// field's zero asks nothing.
type needs struct {
	spec  *backend.VMSpec // a new machine's: what making it needs
	net   backend.Net     // its confinement's network
	ports backend.Ports   // the ports devvm's forwarder forwards from its backend's ssh
	img   image.Name      // its image, for its layers
	agent bool            // it forwards an SSH agent
	// toggle: its agent forwarding is turned on or off on a machine that
	// has it the other way, or it was made ready for that (an emulation's
	// sshagent.Made.Toggle), which each start relies on.
	toggle bool
	ssh    bool // a --clone VM's clone, copied in and fetched over ssh
	pins   bool // its .git locked by pins at each start (a Linux host's on)
	cache  bool // its project folder mounted with the cache mode chosen for it (mountInVM)
}

// requirements lists the backend features n asks for, each once, in the
// order a refusal names the first: the one list every check of what a
// VM needs uses (creation.plan, remaking.check, bringUp, devvm ssh-agent,
// devvm ports).
func requirements(n needs) []backend.Feature {
	var fs []backend.Feature
	if n.spec != nil {
		fs = append(fs, n.spec.Features()...)
	}
	fs = append(fs, n.ports.Features()...)
	fs = append(fs, backend.Confinement{Net: n.net}.Features()...)
	fs = append(fs, backend.ImageFeatures(n.img)...)
	if n.agent {
		fs = append(fs, backend.FeatureAgentForwarding)
	}
	if n.toggle {
		fs = append(fs, backend.FeatureAgentToggle)
	}
	if n.ssh {
		fs = append(fs, backend.FeatureSSH)
	}
	if n.pins {
		fs = append(fs, backend.FeatureReadOnlyPaths)
	}
	if n.cache {
		fs = append(fs, backend.FeatureMountCache)
	}
	var out []backend.Feature
	for _, f := range fs {
		if !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	return out
}

// startNeeds is what a made VM needs of its backend to start, or go on,
// as in, its instance, is: its state's network tier and ports, its SSH
// agent forwarding, agent-toggle when its forwarding is to change, or
// a stopped one was made ready for it, and mount-cache when it mounts its
// folder with a cache mode chosen for it.
func startNeeds(st *state.VM, in backend.Instance) (needs, error) {
	p, err := ports.Of(st)
	if err != nil {
		return needs{}, err
	}
	forward, err := ports.Forwarding(p.Forward)
	if err != nil {
		return needs{}, err
	}
	a, err := sshagent.Of(st)
	if err != nil {
		return needs{}, err
	}
	agent := sshagent.Forwards(sshagent.Mode(st))
	return needs{
		net: confinementFor(st).Net, ports: forward, agent: agent,
		toggle: in.AgentForwarding != agent || a.Made.Toggle && in.Status != backend.StatusRunning,
		cache:  cacheOf(st) != "" && projectMount(st) != "",
	}, nil
}

// refuse refuses the first of fs that b doesn't let through now, with
// Require's refusal. A made VM got each when it was made or changed,
// which Require warned of then, so one let through warns nothing.
func (s starter) refuse(b backend.Backend, vm vmname.VM, fs []backend.Feature) error {
	for _, f := range fs {
		if !s.Registry.Allowed(b, f) {
			return s.Registry.Require(b, vm, f)
		}
	}
	return nil
}

// hostChecks is what devvm checks of a VM's project folder on the host,
// unless it's a --clone VM, which mounts none of it. A new VM (made) gets
// its .git locked as its protection says (NewVM), the SSH origin it
// can't push to without an agent named, and its set-id files. A VM that
// exists gets its .git checked under the protection it ran with
// (gitprotect's Check), so what it changed meanwhile is named (on a host
// without locks, what changed of what a lock would cover), and once it's
// stopped, what its pins recorded first (gitprotect's ReportPinEvents);
// then the protection chosen while it ran is set, and its set-id files
// are named. Both a new VM's checks and a stopped one's go by one walk of
// the folder (walk), which finds its nested repos too (not while it runs:
// the walk is slow). devvm stop does the latter too.
func (s starter) hostChecks(ctx context.Context, t *target, running, made bool) error {
	if isClone(t.st) {
		return nil
	}
	p := s.Protector(t.f.Dir)
	var found hostsafety.Found
	if made || !running {
		found = s.walk(t.f.Dir, s.OS == "linux")
		p.Nested = &found.Repos
	}
	if made {
		if err := p.NewVM(ctx, t.st); err != nil {
			return err
		}
		if !sshagent.Forwards(sshagent.Mode(t.st)) && hostsafety.SSHRemote(ctx, t.f.Dir) {
			s.Warn(msg.SSHRemoteHint(t.f.Name))
		}
		// Files an earlier VM of this folder made are named too.
		s.warnSetID(found.SetID)
		return nil
	}
	if !running {
		// What the VM's runs recorded of its pins, now it's stopped, named
		// even when the check that follows refuses.
		p.ReportPinEvents(s.Dirs.VM(t.f.Name).GitPins(".jsonl"), t.f.Name)
	}
	if err := p.Check(ctx, t.st, running); err != nil {
		return err
	}
	if running {
		return nil
	}
	if next := gitprotect.Next(t.st); next != "" {
		if err := setGitProtect(ctx, s.Env, p, t, next); err != nil {
			return err
		}
	}
	s.warnSetID(found.SetID)
	return nil
}

// walk walks the VM's project folder dir once (hostsafety.Walk), for the
// git repos nested in it, and with setID, its set-id files: on Linux
// hosts, where their bits reach the host (macOS's virtiofs drops them).
func (s starter) walk(dir string, setID bool) hostsafety.Found {
	return hostsafety.Walk(dir, setID)
}

// warnSetID names the set-id files the walk found in the folder, those
// running as root apart.
func (s starter) warnSetID(files []hostsafety.SetIDFile) {
	var root, other []string
	for _, f := range files {
		if f.Root {
			root = append(root, f.Path)
		} else {
			other = append(other, f.Path)
		}
	}
	if len(root) > 0 {
		s.Warn(msg.SetidRootFiles(strings.Join(root, " ")))
	}
	if len(other) > 0 {
		s.Warn(msg.SetidFiles(strings.Join(other, " ")))
	}
}

// reloadAgent loads the keys whose lifetime ran out into devvm's agent
// again for a running VM that runs with it (mode, from runningAgent); one
// that starts gets them in start.
func (s starter) reloadAgent(ctx context.Context, mode string) error {
	if mode != sshagent.Devvm {
		return nil
	}
	return s.Agent.Ensure(ctx)
}

// start starts a stopped VM confined as conf says (confinementFor), and
// returns the confinement it got: bringUp's, and the self-check's when it
// starts the VM again. Its proxy starts first, on every tier, and the
// backend gets the proxy's confinement; a start that fails stops the
// proxy again. Its host keys are read again once it runs: a VM whose
// backend config changed can get new ones when it boots. t is what the
// start made it once it runs (target.started). A backend that stopped the
// VM again, as its project folder couldn't be mounted again with its new
// cache mode while it ran (backend.RemountError), gets one start again,
// all of it (its proxy, pins and host keys anew), whose boot mounts the
// folder with that mode, which that start checks (Want.CheckCache).
func (s starter) start(ctx context.Context, t *target, conf backend.Confinement) (backend.Confinement, error) {
	got, err := s.startOnce(ctx, t, conf, false)
	if _, ok := errors.AsType[*backend.RemountError](err); !ok || ctx.Err() != nil {
		return got, err
	}
	return s.startOnce(ctx, t, conf, true)
}

// startOnce is one of start's starts: checkCache asks the backend to
// check the project folder's cache mode once the VM runs.
func (s starter) startOnce(ctx context.Context, t *target, conf backend.Confinement, checkCache bool) (backend.Confinement, error) {
	vm, st, b := t.f.Name, t.st, t.b
	// The size its state asks for, refused before anything starts when it isn't one.
	size, err := storedSize(st)
	if err != nil {
		return conf, err
	}
	if err := vmssh.ForgetHostKeys(s.Dirs, vm); err != nil {
		return conf, err
	}
	// A link session of its last start's, which would end on its own: the
	// launch starts one for this start.
	s.linkRuns.stop(ctx, vm)
	// The VM's backend forwards the agent it gets when it starts, and its
	// connections to the VM carry it: devvm's own, with the keys it lacks
	// loaded, for a VM that uses it; the user's; none for one that uses
	// none. It gets the proxy's relay of that agent (confine).
	mode := sshagent.Mode(st)
	conf.NoAgent = !sshagent.Forwards(mode)
	if mode == sshagent.Devvm {
		if err := s.Agent.Ensure(ctx); err != nil {
			return conf, err
		}
	}
	conf.AgentSocket = s.agentSocket(mode)
	conf, err = s.confine(ctx, b, st, conf)
	if err != nil {
		return conf, err
	}
	if conf.Pins, err = s.pins(ctx, t); err != nil {
		s.Proxy.Stop(vm)
		return conf, err
	}
	// Tools pointed at the CAs devvm adds, on every tier, whether there
	// are any or not.
	conf.Env = trust.GuestEnv(conf.Env)
	// ssh for editors, where the backend has it (an emulation once it's
	// reviewed): a VM without it starts all the same.
	conf.SSH = s.Registry.Require(b, vm, backend.FeatureSSH) == nil
	if conf.Net == backend.NetProxy {
		defer interruptCleanup(s.Env, undoStart(s.Env, b, vm))()
	}
	// What its state asks of it, which the start makes it where it
	// differs: its size, its project folder where it is now (moved),
	// and the folder itself, with the cache mode chosen for it (none
	// chosen: as it is), its agents' folders in devvm's store now, its
	// agent forwarding.
	agentDirs, _ := agentMounts(s.Dirs, tools.Names(st))
	want := backend.Want{
		Size: size, Project: projectMount(st), ProjectCache: cacheOf(st), CheckCache: checkCache,
		AgentDirs: agentDirs, AgentForwarding: sshagent.Forwards(mode),
	}
	if want.Project != "" {
		want.ProjectID = st.Folder
	}
	if folderChanged(st) {
		s.Proxy.Stop(vm)
		return conf, msg.ProjectFolderChanged(st.Dir, vm)
	}
	if err := b.Start(ctx, vm, want, conf); err != nil {
		s.Proxy.Stop(vm)
		// A folder replaced as it started can stop its start (a Linux
		// host's pins refuse a link): that's what to say, once the VM its
		// backend may have left half started is stopped.
		if folderChanged(st) {
			if err := b.Stop(ctx, vm); err != nil {
				s.Warn(msg.StopFailed(vm))
			}
			return conf, msg.ProjectFolderChangedStopped(st.Dir, vm)
		}
		return conf, err
	}
	if folderChanged(st) {
		if err := b.Stop(ctx, vm); err != nil {
			s.Warn(msg.StopFailed(vm))
		}
		s.Proxy.Stop(vm)
		return conf, msg.ProjectFolderChangedStopped(st.Dir, vm)
	}
	t.started(want)
	// Its ports, once it runs: its state's, through devvm's forwarder.
	s.forwards().Start(ctx, b, st)
	// The agent it started with, until its next start: its ssh config
	// entry's and devvm's shell's. One whose start can't be told apart
	// gets none recorded, and follows its choice.
	run, err := b.Running(ctx, vm)
	if err != nil {
		run.ID = ""
	}
	// Every start of this devvm's that forwards an agent is recorded as
	// relayed, one without an agent to relay (no SSH_AUTH_SOCK) included:
	// that VM then gets the relay's socket, where nothing listens, never
	// the agent's own, which only an older devvm's start keeps.
	return conf, sshagent.RecordStart(ctx, st, mode, run.ID)
}

// pinsLocks reports whether the VM's .git is locked by its backend's pins
// at each start: on a Linux host (macOS locks on the host itself), when
// its protection is on. Its start needs them then, and is refused where
// the backend can't pin, never started unlocked.
func (s starter) pinsLocks(st *state.VM) bool {
	return s.OS == "linux" && gitprotect.Mode(st) == gitprotect.On
}

// pins are what the VM's start pins in its project folder: .git's, as its
// protection says (gitprotect.Protector.Pins), when pinsLocks, with the
// file the namespace records what changed in (paths.VM.GitPins). A VM
// with a .git that has nothing it can pin is warned of: it runs unlocked.
func (s starter) pins(ctx context.Context, t *target) (backend.Pins, error) {
	if !s.pinsLocks(t.st) {
		return backend.Pins{}, nil
	}
	fixed, readOnly, err := s.Protector(t.f.Dir).Pins(ctx, t.st)
	if err != nil {
		return backend.Pins{}, err
	}
	v := s.Dirs.VM(t.f.Name)
	p := backend.Pins{Root: t.f.Dir, Fixed: fixed, ReadOnly: readOnly, Events: v.GitPins(".jsonl"), File: v.GitPins(".json")}
	if _, err := os.Lstat(filepath.Join(t.f.Dir, ".git")); err == nil && len(readOnly) == 0 {
		s.Warn(msg.GitPinsNone(t.f.Name))
	}
	return p, nil
}

// checkRunningPins warns when the running VM t's .git protection is on
// but its backend doesn't have its pins (gitprotect.Protector.PinsMissing):
// one started without devvm, or whose pins went. Where the backend can't
// tell, it says nothing.
func (s starter) checkRunningPins(ctx context.Context, t *target) {
	if !s.pinsLocks(t.st) {
		return
	}
	run, err := t.b.Running(ctx, t.f.Name)
	if err != nil {
		return
	}
	if missing := s.Protector(t.f.Dir).PinsMissing(ctx, t.st, run.ReadOnly, run.Mounted); len(missing) > 0 {
		s.Warn(msg.GitPinsMissing(strings.Join(missing, ", "), t.f.Name))
	}
}

// confine is start's network: the proxy's confinement for the VM's tier
// (Prepare), its proxy started, its ssh port recorded, so other VMs'
// forwarders never take it before its backend binds it, and the VM's
// links checked, with the agent conf chose, which the proxy relays: the
// VM forwards the relay's socket, never the agent's own.
func (s starter) confine(ctx context.Context, b backend.Backend, st *state.VM, conf backend.Confinement) (backend.Confinement, error) {
	vm := st.Name()
	p, err := s.Proxy.Prepare(ctx, b, st, false, conf.AgentSocket)
	if err != nil {
		return conf, err
	}
	p.NoAgent = conf.NoAgent
	if err := s.syncer.checkStartLinks(st); err != nil {
		s.Proxy.Stop(vm)
		return conf, err
	}
	return p, nil
}

// interruptCleanup adds f, a cleanup only for an interrupt, to the run's
// cleanups and returns what removes it, which keeps it once the run is
// interrupted: the run's cleanups wait for the command to return, so the
// undo outlives the function that added it.
func interruptCleanup(e *Env, f func(context.Context)) (remove func()) {
	done := e.AddCleanup(f)
	return func() {
		if !e.Interrupted() {
			done()
		}
	}
}

// undoStart is a start's cleanup for a VM on a confined tier: an
// interrupt can leave it running with its backend's start still waiting
// for it, so without the record of its confined start (Run.Confined), and
// it stops the VM and its proxy. The next launch starts both again,
// confined.
func undoStart(e *Env, b backend.Backend, vm vmname.VM) func(context.Context) {
	return func(ctx context.Context) {
		if !e.Interrupted() {
			return
		}
		e.Info(msg.InterruptedStart(vm))
		if err := b.Stop(ctx, vm); err != nil && !errors.Is(err, backend.ErrNotFound) {
			e.Warn(msg.StopFailed(vm))
		}
		e.Proxy.Stop(vm)
		e.Forwarders.Stop(vm)
	}
}
