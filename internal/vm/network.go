package vm

import (
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/netpolicy"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/settings"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/tools"
	"github.com/devvmhq/devvm/internal/vmname"
)

// Net shows and changes the folder's VM's network tier, its network log
// and its allowlist (here), and the secrets its proxy puts in
// (secret.go).
type Net struct{ *Env }

// NetworkShown is the folder's VM's network tier, as devvm network
// shows it.
type NetworkShown struct {
	Policy string // its tier or unenforced, as devvm ls shows it
	// Learning is since when it learns the hosts it reaches, the zero
	// time when it doesn't.
	Learning time.Time
	// Rules are the rules of each policy file its proxy reads that has
	// any, in order.
	Rules []netpolicy.RuleFile
	// Transparent is its transparent mode (devvm network transparent).
	Transparent netpolicy.Transparent
}

// Network returns the folder's VM's network tier and its allowlist.
func (e Net) Network(ctx context.Context) (NetworkShown, error) {
	var s NetworkShown
	t, err := e.finder.target(ctx, need{has: hasMade})
	if err != nil {
		return s, err
	}
	if s.Policy, err = netpolicy.Listed(ctx, t.b, t.st, t.in.Status == backend.StatusRunning); err != nil {
		return s, err
	}
	s.Learning = netpolicy.LearningOf(t.st).Since
	if s.Transparent, err = netpolicy.TransparentOf(e.Dirs, t.st); err != nil {
		return s, err
	}
	set := netpolicy.Policies(e.Dirs, t.f.Name, netpolicy.TierOf(t.st), image.Name(t.st.Image), tools.Names(t.st))
	s.Rules, err = netpolicy.ReadPolicies(e.Proxy.Assets, set)
	return s, err
}

// NetworkDefault returns new VMs' network tier, or ask, and where that
// came from.
func (e Net) NetworkDefault() (settings.Value, error) {
	def, err := settings.Read(e.Dirs)
	return def.Network, err
}

// SetNetworkDefault saves new VMs' network tier, or ask, as devvm setup
// does (NetworkChoice).
func (e Net) SetNetworkDefault(mode string) error {
	return NetworkChoice{Dirs: e.Dirs, Reporter: e.Reporter}.Set(mode)
}

// NetworkChoice chooses new VMs' network tier, or ask, which devvm
// network -g and devvm setup save in settings.json. devvm's --network
// still chooses for one VM.
type NetworkChoice struct {
	Dirs         paths.Dirs
	msg.Reporter // says what's saved
}

// Set saves mode (a settings.NetworkChoices value) as new VMs' network
// tier; config.toml mustn't set it (set_in_config).
func (c NetworkChoice) Set(mode string) error {
	if _, err := settings.Save(c.Dirs, settings.Values{Network: &mode}); err != nil {
		return err
	}
	c.Info(msg.NetworkDefault(mode))
	return nil
}

// SetTier gives the folder's VM the tier to, as command (devvm network
// allowlist) asks. Between confined tiers it applies at once: a running VM's
// proxy starts again with the new tier's rules. Into or out of open it
// applies from the VM's next start: a running VM restarts now once the
// user agrees (restartNow). A confined tier needs a backend that can
// confine the VM to its proxy, and tokens the VM got while it was open
// stay valid. A lower tier needs the widening check (widen). Any change
// is refused while it would break links' tier rule (tierLinked), and
// while the VM is in a group, whose VMs are all on one tier.
func (e Net) SetTier(ctx context.Context, to netpolicy.Tier, command string) error {
	return e.setTier(ctx, to, false, command)
}

// Learn makes the folder's VM learn the hosts it reaches from now on, on
// the internet tier, as command (devvm network learn) asks: its tier
// changes as SetTier changes it.
func (e Net) Learn(ctx context.Context, command string) error {
	return e.setTier(ctx, netpolicy.TierInternet, true, command)
}

// setTier gives the folder's VM the tier to, learning with learn, as
// SetTier says. A VM that learned stops, and one that learns can't go to
// the allowlist tier but through devvm network lock, which adds the hosts
// it learned (net_learning_lock). In a group, its tier can't change and
// it can't start learning.
func (e Net) setTier(ctx context.Context, to netpolicy.Tier, learn bool, command string) error {
	t, err := e.finder.target(ctx, need{has: hasMade})
	if err != nil {
		return err
	}
	vm, st := t.f.Name, t.st
	from, was := netpolicy.TierOf(st), netpolicy.LearningOf(st).On()
	switch g := groupOf(st); {
	case from == to && was == learn:
		e.Info(msg.SettingAlready(command, vm))
		return nil
	case g != "" && (from != to || learn):
		return msg.GroupTierFixed(g, vm)
	case was && to == netpolicy.TierAllowlist:
		return msg.NetLearningLock(vm)
	}
	if from != to {
		if ok, err := e.checkTier(ctx, t, from, to, command); !ok {
			return err
		}
	}
	var lr netpolicy.Learning
	if learn {
		lr.Since = time.Now().UTC()
	}
	// Under the VM's lock, the group as it is now: a join that raced this
	// change never leaves a learning VM, or another tier, in its group.
	err = st.Update(ctx, func(st *state.VM) error {
		if g := groupOf(st); g != "" && (from != to || learn) {
			return msg.GroupTierFixed(g, vm)
		}
		return netpolicy.SetTier(st, to, lr)
	})
	if err != nil {
		return err
	}
	if learn {
		e.markLearning(vm)
	}
	// Said first: a restart into or out of open opens a shell, and doesn't
	// return.
	switch {
	case learn:
		e.Info(msg.NetLearnStarted(vm))
	case was:
		e.Info(msg.NetLearnStopped(vm))
	}
	if from == to {
		return nil
	}
	return e.applyTier(ctx, t, from, to)
}

// markLearning marks in vm's network log that it starts learning now
// (Log.MarkLearning), so devvm network lock knows whether the log still
// goes back that far. Without the mark, it only warns that the log may
// have lost hosts, so a mark that can't be written is logged, not a
// failure.
func (e *Env) markLearning(vm vmname.VM) {
	if err := netpolicy.NewLog(e.Dirs, vm).MarkLearning(vm); err != nil {
		_, _ = io.WriteString(e.LogWriter(), "devvm: marking the network log of "+vm.String()+": "+err.Error()+"\n")
	}
}

// checkTier checks that t's VM may go from one tier to another, as
// command asks: links' tier rule (tierLinked), the widening check for a
// lower tier, and a backend that can confine it for a confined one. A
// running VM going between confined tiers, which its proxy applies at
// once, is checked for what would refuse that proxy's start
// (netpolicy.Proxy.Refusal) before anything is asked or written: a tier
// in its state that its proxy doesn't enforce would fail open.
func (e Net) checkTier(ctx context.Context, t *target, from, to netpolicy.Tier, command string) (bool, error) {
	if err := tierLinked(e.Dirs, t.st, to); err != nil {
		return false, err
	}
	if t.in.Status == backend.StatusRunning && from.Confined() && to.Confined() {
		if err := e.Proxy.Refusal(ctx); err != nil {
			return false, err
		}
	}
	if !to.AtLeast(from) {
		if ok, err := t.widen(command); !ok {
			return false, err
		}
	}
	if to.Confined() {
		if err := e.Registry.Require(t.b, t.f.Name, backend.Confinement{Net: to.Net()}.Features()...); err != nil {
			return false, err
		}
	}
	return true, nil
}

// applyTier applies the tier change from one tier to another that t's
// state has now, as SetTier says.
// done is said first, once the change is in force or set for later: what
// the command did besides, which a failed proxy start mustn't claim.
func (e Net) applyTier(ctx context.Context, t *target, from, to netpolicy.Tier, done ...msg.M) error {
	vm, st, b := t.f.Name, t.st, t.b
	running := t.in.Status == backend.StatusRunning
	say := func(m msg.M) {
		for _, d := range done {
			e.Info(d)
		}
		e.Info(m)
	}
	if from.Confined() && to.Confined() {
		if running {
			if _, err := e.Proxy.PrepareRunning(ctx, b, st); err != nil {
				return err
			}
			say(msg.SettingApplied(vm))
			return nil
		}
		say(msg.SettingSet(vm))
		return nil
	}
	switch {
	case !running:
		say(msg.SettingSet(vm))
	case e.restartNow(vm, e.Confirm, 1):
		return e.launcher.Restart(ctx, Options{})
	default:
		say(msg.NextStart(1, vm))
	}
	return nil
}

// LockOptions are what devvm network lock adds of the hosts the VM
// learned: those chosen in the picker, or every one (All) or none (None).
type LockOptions struct{ All, None bool }

// Learned returns the hosts the folder's VM reached since it started
// learning that its allowlist tier doesn't allow (netpolicy's Learned).
// A VM that doesn't learn is refused (net_not_learning).
func (e Net) Learned(ctx context.Context) (netpolicy.LearnedHosts, error) {
	t, err := e.lockTarget(ctx)
	if err != nil {
		return netpolicy.LearnedHosts{}, err
	}
	return e.Proxy.Learned(t.st)
}

// lockTarget finds the folder's VM for devvm network lock: one that
// learns.
func (e Net) lockTarget(ctx context.Context) (*target, error) {
	t, err := e.finder.target(ctx, need{has: hasMade})
	if err == nil && !netpolicy.LearningOf(t.st).On() {
		err = msg.NetNotLearning(t.f.Name)
	}
	return t, err
}

// Lock locks the folder's VM, which learns, to the hosts it learned that
// o chooses: it adds their rules, each an exact host and port, to the
// VM's own policy file, then gives the VM the allowlist tier, at once,
// as SetTier does between confined tiers, which ends its learning. In a
// group its tier can't change, and links' tier rule holds, both checked
// before anything is asked.
func (e Net) Lock(ctx context.Context, o LockOptions) error {
	t, err := e.lockTarget(ctx)
	if err != nil {
		return err
	}
	vm, st := t.f.Name, t.st
	if g := groupOf(st); g != "" {
		return msg.GroupTierFixed(g, vm)
	}
	from, to := netpolicy.TierOf(st), netpolicy.TierAllowlist
	if ok, err := e.checkTier(ctx, t, from, to, "devvm network lock"); !ok {
		return err
	}
	was := netpolicy.LearningOf(st)
	learned, err := e.Proxy.Learned(st)
	if err != nil {
		return err
	}
	rules, err := e.chooseLearned(vm, learned, o)
	if err != nil {
		return err
	}
	file := e.Dirs.VM(vm).Policy()
	added := 0
	// Under the VM's lock: the state as it is now, which another devvm may
	// have changed while the list was open, still learning since then.
	err = st.Update(ctx, func(st *state.VM) error {
		// An interrupt, or a terminal gone, while the list was open: nothing.
		if err := ctx.Err(); err != nil {
			return err
		}
		if now := netpolicy.LearningOf(st); !now.On() || !now.Since.Equal(was.Since) {
			return msg.NetLockChanged(vm)
		}
		for _, r := range rules {
			ok, err := netpolicy.AddRule(file, r)
			if err != nil {
				return err
			}
			if ok {
				added++
			}
		}
		return netpolicy.SetTier(st, to, netpolicy.Learning{})
	})
	if err != nil {
		return err
	}
	for _, m := range netpolicy.PolicyWarnings([]string{file}) {
		e.Warn(m)
	}
	// Whether it runs, as it does now; the rules added are said once its
	// proxy has them.
	if t.in, err = t.b.Inspect(ctx, vm); err != nil {
		return err
	}
	return e.applyTier(ctx, t, from, to, msg.NetLocked(added, vm))
}

// lockTickMax is the most learned hosts the lock's list starts with any
// ticked for: past it, none is, as nobody reads that many rows one by
// one, and devvm points at --json.
const lockTickMax = 30

// chooseLearned returns the rules of the learned hosts that o chooses:
// every one, none, or in a terminal those ticked in the picker, after a
// line saying that learning was internet access. The picker starts with a
// host ticked only when its domain is one the allowlist has already and
// it isn't risky (Learned.Ticked), and with none ticked past lockTickMax
// hosts. Where the picker can't show, it's refused, naming --all and
// --none, and so it is when the terminal won't let it show, rather than
// go on with the ticks it starts with. With no hosts, nothing is asked.
func (e Net) chooseLearned(vm vmname.VM, l netpolicy.LearnedHosts, o LockOptions) ([]string, error) {
	var all []string
	for _, h := range l.Hosts {
		all = append(all, h.Rule)
	}
	switch {
	case o.All:
		return all, nil
	case o.None || len(l.Hosts) == 0:
		return nil, nil
	}
	q := msg.NetLockHelp(vm).AnsweredBy("--all or --none")
	if !e.CanPick() || e.PickMany == nil {
		return nil, q.Refused(false)
	}
	if l.Partial {
		e.Warn(msg.NetLearnedPartial(learnedSince(netpolicy.Learning{Since: l.Since}), vm))
	}
	since, many := learnedSince(netpolicy.Learning{Since: l.Since}), len(l.Hosts) > lockTickMax
	if many {
		e.Say(msg.NetLockContextMany(len(l.Hosts), since))
	} else {
		e.Say(msg.NetLockContext(since))
	}
	picked, err := e.PickMany(q.M(), learnedParts(l, many))
	if err != nil {
		return nil, err
	}
	var chosen []string
	for _, p := range picked {
		for _, r := range p.Rows {
			if r.On {
				chosen = append(chosen, r.Name)
			}
		}
	}
	return chosen, nil
}

// learnedParts are the lock's list: the hosts without a risk, then those
// only tools without proxy settings reached (Learned.NoProxy), then the
// risky ones, each in a part of their own, under a heading that says why,
// so no width of the terminal, no long name and no User-Agent the VM chose
// can push that off their rows. A host starts ticked as Learned.Ticked
// says, but none when there are many.
func learnedParts(l netpolicy.LearnedHosts, many bool) []PickPart {
	parts := []PickPart{{}, {Heading: msg.NetLearnedNoProxyHeading()}, {Heading: msg.NetLearnedRiskyHeading()}}
	for _, h := range l.Hosts {
		i := 0
		switch {
		case h.Risk != "":
			i = 2
		case h.NoProxy:
			i = 1
		}
		parts[i].Rows = append(parts[i].Rows, PickRow{Name: h.Rule, Desc: learnedDesc(h), On: h.Ticked() && !many})
	}
	return slices.DeleteFunc(parts, func(p PickPart) bool { return len(p.Rows) == 0 })
}

// learnedDesc is what the lock's picker says of a learned host: how many
// connections it had, and the User-Agent that made them most, as a word
// of a tool name's characters alone (netpolicy's agentWord).
func learnedDesc(h netpolicy.Learned) msg.M {
	if h.Agent != "" {
		return msg.NetLearnedHost(h.Agent, h.Count)
	}
	return msg.NetLearnedHostAnon(h.Count)
}

// LogOptions are what devvm network log shows of the network log.
type LogOptions struct {
	Lines   int  // the last events shown
	Follow  bool // then each new one, until ctx ends
	Blocked bool // the blocks grouped by host instead
}

// Log writes the folder's VM's network log to w: what its proxy allowed
// and blocked, and its links' connections. An open VM
// logs only its links' connections, and devvm says so.
func (e Net) Log(ctx context.Context, w io.Writer, o LogOptions) error {
	l, vm, err := e.netLog(ctx, o.Follow)
	if err != nil || l == nil {
		if err == nil {
			e.Info(msg.LogEmpty(vm))
		}
		return err
	}
	if o.Blocked {
		return l.WriteBlocked(w)
	}
	return l.Write(ctx, w, o.Lines, o.Follow)
}

// LogEvents returns the last n events of the folder's VM's network log,
// oldest first: none when it has none.
func (e Net) LogEvents(ctx context.Context, n int) ([]netpolicy.Event, error) {
	l, _, err := e.netLog(ctx, false)
	if err != nil || l == nil {
		return nil, err
	}
	return l.Events(n)
}

// LogBlocks returns the blocks in the folder's VM's network log, one per
// host and port, by when each was last blocked: none when it has none.
func (e Net) LogBlocks(ctx context.Context) ([]netpolicy.Block, error) {
	l, _, err := e.netLog(ctx, false)
	if err != nil || l == nil {
		return nil, err
	}
	return l.Blocks()
}

// netLog returns the folder's VM's network log and the VM's name; a nil
// log when there's nothing to show (follow: nothing to follow). An open
// VM's log has only what goes through its proxy, and devvm says so.
func (e Net) netLog(ctx context.Context, follow bool) (*netpolicy.Log, vmname.VM, error) {
	t, err := e.finder.target(ctx, need{has: hasVM})
	if err != nil {
		return nil, "", err
	}
	vm := t.f.Name
	if !netpolicy.TierOf(t.st).Confined() {
		e.Note(msg.NetOpenLog(vm))
	}
	l := netpolicy.NewLog(e.Dirs, vm)
	// Its proxy makes the log as it starts: following an empty one waits.
	// One it just rotated is empty, with its events in the old one.
	fi, err := os.Stat(l.Path)
	_, oerr := os.Stat(l.Old())
	if errors.Is(err, os.ErrNotExist) || err == nil && fi.Size() == 0 && !follow && errors.Is(oerr, os.ErrNotExist) {
		return nil, vm, nil
	}
	return l, vm, nil
}

// widensPolicy returns the command, when a change to a VM's own rules gives
// it more access: adding allows, or removing a deny (a rule with its !);
// else "".
func widensPolicy(rules []string, deny, remove bool) string {
	switch {
	case !remove && !deny:
		return "devvm network allow"
	case remove && slices.ContainsFunc(rules, func(r string) bool { return strings.HasPrefix(strings.TrimSpace(r), "!") }):
		return "devvm network rm"
	}
	return ""
}

// RuleChange is how devvm network allow, deny and rm change rules: as
// denies, taking them out, in the user's file for every VM, and narrowed
// to the methods and the path pattern their --method and --path give (""
// for none).
type RuleChange struct {
	Deny, Remove, Global bool
	Methods, Path        string
}

// rules are the lines c makes of rules as given: each checked
// (netpolicy.Rule) and narrowed, or for Remove as written, a deny with its
// !, narrowed when --method or --path gives it.
func (c RuleChange) rules(rules []string) ([]string, error) {
	out := make([]string, len(rules))
	for i, r := range rules {
		var err error
		switch {
		case !c.Remove:
			out[i], err = netpolicy.Rule(r, c.Deny, c.Methods, c.Path)
		case c.Methods != "" || c.Path != "":
			out[i], err = netpolicy.Rule(r, strings.HasPrefix(r, "!"), c.Methods, c.Path)
		default:
			out[i] = r
		}
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// PolicyChange adds rules to the folder's VM's own policy file (deny: as
// denies), or with remove takes them out; with global, the user's file
// for every VM instead. Its proxy reads the file again at its next
// connection, so a change applies at once.
func (e Net) PolicyChange(ctx context.Context, given []string, c RuleChange) error {
	// Every rule is checked before any is changed.
	rules, err := c.rules(given)
	if err != nil {
		return err
	}
	deny, remove, global := c.Deny, c.Remove, c.Global
	file := e.Dirs.Policy()
	var vm vmname.VM
	var st *state.VM
	if !global {
		// The folder's VM, which its backend has, made for this folder.
		t, err := e.finder.target(ctx, need{has: hasMachine, folder: true, noVM: msg.NoVMPolicy})
		if err != nil {
			return err
		}
		switch tier := netpolicy.TierOf(t.st); {
		case !tier.Confined():
			e.Note(msg.NetOpenPolicy(t.f.Name))
		case tier != netpolicy.TierAllowlist && !deny && !remove:
			e.Note(msg.NetAllowsUnused(string(tier), t.f.Name))
		}
		if cmd := widensPolicy(rules, deny, remove); cmd != "" {
			if ok, err := t.widen(cmd); !ok {
				return err
			}
		}
		vm, st = t.f.Name, t.st
		file = e.Dirs.VM(vm).Policy()
	}
	// The file's lines that aren't rules, once it's changed (or not), and
	// the proxies that may read a rule by method and path otherwise.
	defer func() {
		for _, m := range netpolicy.PolicyWarnings([]string{file}) {
			e.Warn(m)
		}
		e.Proxy.OtherBinaries(rules, vm)
	}()
	if remove {
		for _, r := range rules {
			switch ok, err := netpolicy.RemoveRule(file, r); {
			case err != nil:
				return err
			case !ok:
				e.Warn(msg.RuleMissing(file, r))
			case global:
				e.Info(msg.RuleRemovedGlobal(r))
			default:
				e.Info(msg.RuleRemoved(r, vm))
			}
		}
		return nil
	}
	for _, r := range rules {
		switch ok, err := netpolicy.AddRule(file, r); {
		case err != nil:
			return err
		case !ok:
			e.Info(msg.RulePresent(file, r))
		case global:
			e.Info(msg.RuleAddedGlobal(r))
		default:
			e.Info(msg.RuleAdded(r, vm))
		}
	}
	e.Proxy.NarrowNotes(rules, st, deny)
	return nil
}
