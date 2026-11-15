package vm

import (
	"cmp"
	"context"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/netpolicy"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/project"
	"github.com/devvmhq/devvm/internal/settings"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/vmname"
)

// groupOf is the group a VM is in, as its links section records it; ""
// for none.
func groupOf(st *state.VM) string { return linksOf(st).Group }

// Groups shows and changes the folder's VM's group: VMs that reach each
// other's published ports by name, through links the group makes in each
// of them (groupLinks), which carry connections as any link does.
type Groups struct{ *Env }

// CheckGroupName refuses a group name that isn't one word of lowercase
// letters, digits and dashes, as a link's name is.
func CheckGroupName(name string) error {
	if !linkName.MatchString(name) || strings.Trim(name, "0123456789") == "" {
		return msg.GroupNameInvalid(strconv.Quote(name))
	}
	return nil
}

// memberName is the name the group's other VMs reach member st by: the
// name it joined with, else its folder part.
func memberName(st *state.VM) string {
	return cmp.Or(linksOf(st).GroupAs, project.Folder{Name: st.Name()}.Slug())
}

// groupMembers returns the states of group's members, sorted by VM name:
// the project VMs whose state names it. "" has none.
func groupMembers(d paths.Dirs, group string) ([]*state.VM, error) {
	if group == "" {
		return nil, nil
	}
	var out []*state.VM
	err := clientStates(d, "", func(st *state.VM, _ []Link) error {
		if groupOf(st) == group {
			out = append(out, st)
		}
		return nil
	})
	return out, err
}

// publishedPorts returns each port a VM publishes, its ranges expanded.
func publishedPorts(st *state.VM) []int {
	v := linksOf(st).Publish
	if v == "" || v == settings.PortsNone {
		return nil
	}
	rs, err := backend.ParsePortRanges(v)
	if err != nil {
		return nil
	}
	var out []int
	for _, r := range rs {
		for p := r.From; p <= r.To; p++ {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// ownLinks returns the links of links that the user made, with devvm link
// add.
func ownLinks(links []Link) []Link {
	return slices.DeleteFunc(slices.Clone(links), func(l Link) bool { return l.Group })
}

// linkedName returns the name links has for target (its VM name and ID),
// or "".
func linkedName(links []Link, target vmname.VM, id string) string {
	for _, l := range links {
		if l.Target == target && l.TargetID == id {
			return l.Name
		}
	}
	return ""
}

// groupClash is a member that a client gets none of the group's links
// to, because one of the links the user made in the client has its name.
type groupClash struct {
	Name   string
	Target vmname.VM
}

// groupLinks returns client's links as its group makes them, from own,
// the links the user made there, kept as they are, prev, all its links
// before, and others, the group's other members: own, then a link to each
// port each member publishes. A member's links take the name the user's
// links to it have, else its name in the group (memberName), and the
// client's address for it, the one its links to it had in prev, so it
// stays. A port the user linked already gets no other. A member whose
// name one of the user's links to another VM has gets none: a clash, the
// user's link taking precedence. Nor does one whose name is localhost or
// the client's own hostname, which devvm group join refuses
// (checkJoinName), so none has it.
func groupLinks(client *state.VM, own, prev []Link, others []*state.VM) ([]Link, []groupClash) {
	out := slices.Clone(own)
	var clashes []groupClash
	host := project.Folder{Name: client.Name()}.Slug()
	for _, m := range others {
		target, id := m.Name(), m.ID
		ports := publishedPorts(m)
		if id == "" || len(ports) == 0 {
			continue
		}
		name := linkedName(own, target, id)
		if name == "" {
			name = memberName(m)
			if name == host || name == "localhost" {
				continue
			}
			if slices.ContainsFunc(out, func(l Link) bool { return l.Name == name }) {
				clashes = append(clashes, groupClash{Name: name, Target: target})
				continue
			}
		}
		addr, err := linkAddr(append(slices.Clone(prev), out...), target, id)
		if err != nil {
			continue // every address of the 65534 has a target
		}
		for _, p := range ports {
			if !slices.ContainsFunc(own, func(l Link) bool { return l.Target == target && l.TargetID == id && l.Port == p }) {
				out = append(out, Link{Name: name, Port: p, Target: target, TargetID: id, Addr: addr, Group: true})
			}
		}
	}
	sortLinks(out)
	return out, clashes
}

// withGroup returns a VM's links as its group makes them now
// (groupLinks), from own, the links the user made, and prev, all it had:
// only own when it's in none.
func (e Groups) withGroup(st *state.VM, own, prev []Link) ([]Link, []groupClash, error) {
	members, err := groupMembers(e.Dirs, groupOf(st))
	if err != nil {
		return nil, nil, err
	}
	others := slices.DeleteFunc(members, func(m *state.VM) bool { return m.Name() == st.Name() })
	out, clashes := groupLinks(st, own, prev, others)
	return out, clashes, nil
}

// warnClashes warns of each member a VM gets none of the group's links to
// (groupLinks), with what to do.
func (e Groups) warnClashes(vm vmname.VM, clashes []groupClash) {
	for _, c := range clashes {
		e.Warn(msg.GroupLinkNameClash(c.Name, c.Target.String(), vm))
	}
}

// groupsNote is how long lockGroups waits before it says it's waiting.
const groupsNote = 200 * time.Millisecond

// lockGroups holds devvm's groups lock (paths.Dirs.GroupsLock) until unlock is
// called or the process ends, waiting for another devvm that holds it
// until ctx ends, and saying so with info (nil says nothing) after a
// moment. Each join and leave, and each change that works a group's
// links out again, holds it throughout, from reading the members' states
// to writing them, so it reads what the one before wrote: two VMs
// joining at once are checked one after the other, and all or none
// holds. It's an OS lock on a file, which the OS lets go of when the
// process holding it ends, however it ends.
func lockGroups(ctx context.Context, d paths.Dirs, info func(msg.M)) (unlock func(), err error) {
	if err := os.MkdirAll(d.Data, 0o700); err != nil {
		return nil, err
	}
	return platform.LockFileWaiting(ctx, d.GroupsLock(), groupsNote, func() {
		if info != nil {
			info(msg.GroupWaiting())
		}
	})
}

// lock holds the groups lock (lockGroups) and reads t's state again, as
// another devvm may have changed it while this one waited.
func (e Groups) lock(ctx context.Context, t *target) (unlock func(), err error) {
	unlock, err = lockGroups(ctx, e.Dirs, e.Info)
	if err != nil {
		return nil, err
	}
	st, err := state.ReadVM(e.Dirs, t.f.Name)
	if err == nil {
		err = refuseUnfinished(t.f.Name, st)
	}
	if err != nil {
		unlock()
		return nil, err
	}
	t.st = st
	return unlock, nil
}

// syncOthers gives the other members of t's group their links as the
// group makes them now (sync), holding the groups lock: devvm link
// publish and unpublish, once t's state has its ports.
func (e Groups) syncOthers(ctx context.Context, t *target) error {
	unlock, err := e.lock(ctx, t)
	if err != nil {
		return err
	}
	defer unlock()
	return e.sync(ctx, groupOf(t.st), t.f.Name)
}

// sync gives each member of group but skip its links as the group makes
// them now (syncVM). "" has none.
func (e Groups) sync(ctx context.Context, group string, skip vmname.VM) error {
	members, err := groupMembers(e.Dirs, group)
	if err != nil {
		return err
	}
	for _, m := range members {
		if m.Name() == skip {
			continue
		}
		if err := e.syncVM(ctx, m, group, false); err != nil {
			return err
		}
	}
	return nil
}

// syncVM gives st its links as its group makes them now (withGroup), the
// group's changes named as group's, and with warn, the members it gets no
// links to warned of.
func (e Groups) syncVM(ctx context.Context, st *state.VM, group string, warn bool) error {
	links, err := ReadLinks(st)
	if err != nil {
		return err
	}
	want, clashes, err := e.withGroup(st, ownLinks(links), links)
	if err != nil {
		return err
	}
	if warn {
		e.warnClashes(st.Name(), clashes)
	}
	if slices.Equal(want, links) {
		return nil
	}
	return e.change(ctx, st, group, links, want)
}

// change records a VM's links, before and after the group changed them,
// says which of group's it gained and lost, warns of a new link's target
// whose network tier isn't in force (warnUnenforced), and applies them
// while it runs, as devvm link add does. One it can't apply them to is
// warned of, and gets them at its next start.
func (e Groups) change(ctx context.Context, st *state.VM, group string, before, after []Link) error {
	vm := st.Name()
	runs := e.linkRuns
	from := runs.logSize(vm)
	if err := writeLinks(ctx, st, after); err != nil {
		return err
	}
	added := e.sayChangedIn(group, vm, before, after, nil)
	var targets []vmname.VM
	for _, l := range added {
		if !slices.Contains(targets, l.Target) {
			targets = append(targets, l.Target)
		}
	}
	for _, t := range targets {
		tst, err := state.ReadVM(e.Dirs, t)
		if err != nil {
			return err
		}
		e.links.warnUnenforced(ctx, st, tst)
	}
	c, err := e.Registry.For(st)
	if err != nil {
		e.Warn(msg.LinksUpdateFailed(vm).Because(err))
		return nil
	}
	running, err := runs.apply(ctx, c.Backend, st, added, from)
	switch {
	case err != nil:
		e.Warn(msg.LinksUpdateFailed(vm).Because(err))
	case !running:
		e.Info(msg.SettingSet(vm))
	}
	return nil
}

// sayChanged says which of the links st's group made it gained and lost
// when its links went from before to after (groupChanged), with also, the
// group's links that took the place of the user's, as gained.
func (e Groups) sayChanged(st *state.VM, before, after, also []Link) {
	e.sayChangedIn(groupOf(st), st.Name(), before, after, also)
}

// sayChangedIn is sayChanged for vm in group, and returns what it gained.
func (e Groups) sayChangedIn(group string, vm vmname.VM, before, after, also []Link) []Link {
	added, removed := groupChanged(before, after)
	for _, l := range also {
		if !slices.Contains(added, l) {
			added = append(added, l)
		}
	}
	if len(added) > 0 {
		e.Info(msg.GroupLinksAdded(group, shownLinks(added), vm))
	}
	if len(removed) > 0 {
		e.Info(msg.GroupLinksRemoved(group, shownLinks(removed), vm))
	}
	return added
}

// groupChanged returns the links a group made that after has and before
// didn't, and those before had and after doesn't, each by its name and
// port: a link the user made in its place isn't lost.
func groupChanged(before, after []Link) (added, removed []Link) {
	key := func(l Link) string { return l.Name + ":" + strconv.Itoa(l.Port) }
	has := func(ls []Link, l Link) bool {
		return slices.ContainsFunc(ls, func(k Link) bool { return key(k) == key(l) })
	}
	for _, l := range after {
		if l.Group && !has(before, l) {
			added = append(added, l)
		}
	}
	for _, l := range before {
		if l.Group && !has(after, l) {
			removed = append(removed, l)
		}
	}
	return added, removed
}

// shownLinks is links as messages name them: NAME:PORTS for each name,
// ranges joined, as db:5432,8000-8010, api:80.
func shownLinks(links []Link) string {
	ls := slices.Clone(links)
	sortLinks(ls)
	var out []string
	for i := 0; i < len(ls); {
		j := i
		var ports []string
		for j < len(ls) && ls[j].Name == ls[i].Name {
			k := j
			for k+1 < len(ls) && ls[k+1].Name == ls[i].Name && ls[k+1].Port == ls[k].Port+1 {
				k++
			}
			p := strconv.Itoa(ls[j].Port)
			if k > j {
				p += "-" + strconv.Itoa(ls[k].Port)
			}
			ports = append(ports, p)
			j = k + 1
		}
		out = append(out, ls[i].Name+":"+strings.Join(ports, ","))
		i = j
	}
	return strings.Join(out, ", ")
}

// GroupMember is one of a group's VMs as devvm group lists it.
type GroupMember struct {
	Name      string // the name the others reach it by
	VM        vmname.VM
	Folder    string
	Published string // its published ports, or none
}

// GroupShown is the folder's VM's group as devvm group shows it: its
// name, "" for none, and its members.
type GroupShown struct {
	VM      vmname.VM
	Group   string
	Members []GroupMember
}

// Show returns the folder's VM's group, with each member's name, folder
// and published ports, sorted by name.
func (e Groups) Show(ctx context.Context) (GroupShown, error) {
	t, err := e.finder.target(ctx, need{has: hasMade, noVM: msg.NoVMGroup})
	if err != nil {
		return GroupShown{}, err
	}
	g := GroupShown{VM: t.f.Name, Group: groupOf(t.st)}
	members, err := groupMembers(e.Dirs, g.Group)
	if err != nil {
		return GroupShown{}, err
	}
	for _, m := range members {
		g.Members = append(g.Members, GroupMember{
			Name: memberName(m), VM: m.Name(), Folder: m.Dir,
			Published: cmp.Or(linksOf(m).Publish, settings.PortsNone),
		})
	}
	slices.SortFunc(g.Members, func(a, b GroupMember) int { return cmp.Compare(a.Name, b.Name) })
	return g, nil
}

// Join puts the folder's VM in group, made by its first member, as as (""
// for its folder part), once it's checked (checkJoin), and gives it and
// every other member their links to each other's published ports. A VM
// is in one group at a time.
//
// It checks the join before it asks to widen a parent folder's VM, and
// again once it holds the groups lock (lock), so a VM that joined
// meanwhile counts.
func (e Groups) Join(ctx context.Context, group, as string) error {
	t, err := e.finder.target(ctx, need{has: hasMade, noVM: msg.NoVMGroup})
	if err != nil {
		return err
	}
	asked := false
	if groupOf(t.st) == "" {
		if _, err := e.checkJoin(t, group, as); err != nil {
			return err
		}
		if ok, err := t.widen("devvm group join"); !ok {
			return err
		}
		asked = true
	}
	unlock, err := e.lock(ctx, t)
	if err != nil {
		return err
	}
	defer unlock()
	if cur := groupOf(t.st); cur != "" {
		return e.rejoin(ctx, t, cur, group, as)
	}
	members, err := e.checkJoin(t, group, as)
	if err != nil {
		return err
	}
	if !asked {
		if ok, err := t.widen("devvm group join"); !ok {
			return err
		}
	}
	return e.join(ctx, t, group, as, members)
}

// rejoin is Join for t, in the group cur already: joining it again gives
// each member its links as the group makes them, as a join cut short
// didn't, and joining another, or the same with another name, is refused.
func (e Groups) rejoin(ctx context.Context, t *target, cur, group, as string) error {
	vm := t.f.Name
	switch {
	case cur != group:
		return msg.GroupOther(cur, vm)
	case as != "" && as != memberName(t.st):
		return msg.GroupNamed(group, memberName(t.st), vm)
	}
	e.Info(msg.GroupAlready(group, vm))
	if err := e.syncVM(ctx, t.st, group, false); err != nil {
		return err
	}
	return e.sync(ctx, group, vm)
}

// join puts t in group, whose members were members, as as, once checked,
// and gives each its links.
func (e Groups) join(ctx context.Context, t *target, group, as string, members []*state.VM) error {
	vm := t.f.Name
	// Under the VM's lock: a devvm network learn that raced the join never
	// leaves a learning VM in the group.
	err := t.st.Update(ctx, func(st *state.VM) error {
		if netpolicy.LearningOf(st).On() {
			return msg.GroupJoinLearning(group, vm)
		}
		return edit(st, linksSection, func(l *linksInVM) error { l.Group, l.GroupAs = group, as; return nil })
	})
	if err != nil {
		return err
	}
	if len(members) == 0 {
		e.Info(msg.GroupMade(group, vm))
	} else {
		e.Info(msg.GroupJoined(group, vm))
	}
	e.Note(msg.GroupTrust(group))
	if err := e.syncVM(ctx, t.st, group, true); err != nil {
		return err
	}
	for _, m := range members {
		if err := e.syncVM(ctx, m, group, true); err != nil {
			return err
		}
	}
	return nil
}

// checkJoin checks that client t can join group as as ("" for its folder
// part), and returns the group's members: every member is on one tier,
// the names are as checkJoinName wants them, and
// every backend has links.
func (e Groups) checkJoin(t *target, group, as string) ([]*state.VM, error) {
	vm := t.f.Name
	// A learning VM goes to the allowlist tier once it's locked, which a
	// group's VMs can't do alone.
	if netpolicy.LearningOf(t.st).On() {
		return nil, msg.GroupJoinLearning(group, vm)
	}
	members, err := groupMembers(e.Dirs, group)
	if err != nil {
		return nil, err
	}
	if err := CheckLinkName(cmp.Or(as, t.f.Slug())); err != nil {
		return nil, err
	}
	// The group links each member to every other: the tier rule holds
	// both ways, so they're all on one tier.
	self := policyOf(t.st)
	for _, m := range members {
		if other := policyOf(m); !policyRule(self, other) || !policyRule(other, self) {
			return nil, msg.GroupTierMismatch(group, string(netpolicy.TierOf(m)), vm, string(netpolicy.TierOf(t.st)))
		}
	}
	if err := checkJoinName(group, vm, as, members); err != nil {
		return nil, err
	}
	if err := e.Registry.Require(t.b, vm, linkFeatures()...); err != nil {
		return nil, err
	}
	for _, m := range members {
		c, err := e.Registry.For(m)
		if err != nil {
			return nil, err
		}
		if err := e.Registry.Require(c.Backend, m.Name(), linkFeatures()...); err != nil {
			return nil, err
		}
	}
	return members, nil
}

// checkJoinName checks the names of vm joining group as as ("" for its
// folder part) beside its members: its name isn't localhost, no member's
// name is vm's own hostname, its folder part, which would resolve to vm
// itself there, and no member has its name, as its name or its own
// hostname.
func checkJoinName(group string, vm vmname.VM, as string, members []*state.VM) error {
	host := project.Folder{Name: vm}.Slug()
	name := cmp.Or(as, host)
	if name == "localhost" {
		return msg.GroupNameHost(vm)
	}
	for _, m := range members {
		if memberName(m) == host {
			return msg.GroupNameHostname(group, m.Name().String(), host, vm)
		}
	}
	for _, m := range members {
		if name == memberName(m) || name == (project.Folder{Name: m.Name()}).Slug() {
			return msg.GroupNameTaken(group, m.Name().String(), name)
		}
	}
	return nil
}

// Leave takes the folder's VM out of its group, and removes the links the
// group made, in it and in the other members, naming them. The links the
// user made stay. The group goes with its last member. It holds the
// groups lock throughout (lock).
func (e Groups) Leave(ctx context.Context) error {
	t, err := e.finder.target(ctx, need{has: hasMade, noVM: msg.NoVMGroup})
	if err != nil {
		return err
	}
	unlock, err := e.lock(ctx, t)
	if err != nil {
		return err
	}
	defer unlock()
	group := groupOf(t.st)
	if group == "" {
		return msg.GroupNone(t.f.Name)
	}
	// Its own group links go with its group, at once, so a leave cut
	// short leaves it none.
	links, err := ReadLinks(t.st)
	if err != nil {
		return err
	}
	own := ownLinks(links)
	err = update(ctx, t.st, linksSection, func(l *linksInVM) error {
		l.Links, l.Group, l.GroupAs = ownLinks(l.Links), "", ""
		return nil
	})
	if err != nil {
		return err
	}
	if err := e.left(ctx, t.f.Name, group); err != nil {
		return err
	}
	if len(own) == len(links) {
		return nil
	}
	return e.change(ctx, t.st, group, links, own)
}

// forget removes vm's state, its own links with it, and every other VM's
// links to it (unlinkTarget), and takes it out of its group, holding the
// groups lock throughout: devvm rm.
func (e Groups) forget(ctx context.Context, vm vmname.VM) error {
	unlock, err := lockGroups(ctx, e.Dirs, e.Info)
	if err != nil {
		return err
	}
	defer unlock()
	st, err := state.ReadVM(e.Dirs, vm)
	if err != nil {
		return err
	}
	group := groupOf(st)
	if err := st.Remove(ctx); err != nil {
		return err
	}
	if err := e.linkRuns.unlinkTarget(ctx, vm); err != nil {
		return err
	}
	if group == "" {
		return nil
	}
	return e.left(ctx, vm, group)
}

// left says vm left group, which its state no longer records, and gives
// the other members their links without it; the last member's leaving
// removes the group.
func (e Groups) left(ctx context.Context, vm vmname.VM, group string) error {
	e.Info(msg.GroupLeft(group, vm))
	members, err := groupMembers(e.Dirs, group)
	if err != nil {
		return err
	}
	if len(members) == 0 {
		e.Info(msg.GroupGone(group))
		return nil
	}
	return e.sync(ctx, group, "")
}
