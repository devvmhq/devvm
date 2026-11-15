package vm

import (
	"context"
	"slices"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/netpolicy"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/vmname"
)

// clientStates calls each with the state and links of every project VM
// but skip, by name (ReadStates). A VM whose state can't be read is left
// out: it never starts, so its links reach nothing.
func clientStates(d paths.Dirs, skip vmname.VM, each func(st *state.VM, links []Link) error) error {
	inv, err := ReadStates(d)
	if err != nil {
		return err
	}
	for _, st := range inv.States() {
		if st.Name() == skip {
			continue
		}
		links, err := ReadLinks(st)
		if err != nil {
			return err
		}
		if err := each(st, links); err != nil {
			return err
		}
	}
	return nil
}

// linkClients returns the project VMs, by name, with a link to target
// (its VM name and ID) that picks.
func linkClients(d paths.Dirs, target vmname.VM, id string, picks func(client *state.VM, l Link) bool) ([]vmname.VM, error) {
	var out []vmname.VM
	err := clientStates(d, target, func(st *state.VM, links []Link) error {
		if slices.ContainsFunc(links, func(l Link) bool { return l.Target == target && l.TargetID == id && picks(st, l) }) {
			out = append(out, st.Name())
		}
		return nil
	})
	return out, err
}

// tierLinked refuses a change of a VM's tier to to that would break
// links' tier rule: a tier lower than that of a client that links to it,
// or higher than that of a target it links to. Links to a target that's
// gone or was made again don't count.
func tierLinked(d paths.Dirs, st *state.VM, to netpolicy.Tier) error {
	vm, side := st.Name(), policySide{tier: to, ok: true}
	clients, err := linkClients(d, vm, st.ID, func(c *state.VM, _ Link) bool { return !policyRule(policyOf(c), side) })
	if err != nil {
		return err
	}
	if len(clients) > 0 {
		return msg.TierLinkedClients(string(to), vm, vmname.Join(clients, ", "))
	}
	links, err := ReadLinks(st)
	if err != nil {
		return err
	}
	var targets []vmname.VM
	for _, l := range links {
		tst, err := state.ReadVM(d, l.Target)
		if err != nil {
			return err
		}
		if tst.Exists() && tst.ID == l.TargetID && !policyRule(side, policyOf(tst)) && !slices.Contains(targets, l.Target) {
			targets = append(targets, l.Target)
		}
	}
	if len(targets) == 0 {
		return nil
	}
	return msg.TierLinkedTargets(vmname.Join(targets, ", "), string(to), vm)
}

// unlinkTarget removes every link to target, a VM devvm deletes or
// forgets, from every client's state, names the clients that lost one,
// and gives each running one its links as they are now (apply). One that
// can't be given them is warned of, and gets them at its next start.
// Each client's state changes under its own lock, read again there, one
// client at a time: the lock rule for a change across VMs.
func (e linkRuns) unlinkTarget(ctx context.Context, target vmname.VM) error {
	var lost []*state.VM
	err := clientStates(e.Dirs, target, func(st *state.VM, links []Link) error {
		if !slices.ContainsFunc(links, func(l Link) bool { return l.Target == target }) {
			return nil
		}
		lost = append(lost, st)
		return update(ctx, st, linksSection, func(l *linksInVM) error {
			l.Links = slices.DeleteFunc(l.Links, func(l Link) bool { return l.Target == target })
			return nil
		})
	})
	if err != nil || len(lost) == 0 {
		return err
	}
	names := make([]vmname.VM, len(lost))
	for i, st := range lost {
		names[i] = st.Name()
	}
	e.Info(msg.LinksTargetGone(len(lost), target, vmname.Join(names, ", ")))
	for _, st := range lost {
		c, err := e.Registry.For(st)
		if err == nil {
			_, err = e.apply(ctx, c.Backend, st, nil, 0)
		}
		if err != nil {
			e.Warn(msg.LinksUpdateFailed(st.Name()).Because(err))
		}
	}
	return nil
}

// DeadLink is a client's link to a VM that has no state any more, which
// devvm gc --orphans removes.
type DeadLink struct {
	Client, Target vmname.VM
	Name           string
	Port           int
}

// deadLinks returns every client's links to VMs that have no state.
func deadLinks(d paths.Dirs) ([]DeadLink, error) {
	var out []DeadLink
	err := clientStates(d, "", func(st *state.VM, links []Link) error {
		for _, l := range links {
			tst, err := state.ReadVM(d, l.Target)
			if err != nil {
				return err
			}
			if !tst.Exists() {
				out = append(out, DeadLink{Client: st.Name(), Name: l.Name, Target: l.Target, Port: l.Port})
			}
		}
		return nil
	})
	return out, err
}
