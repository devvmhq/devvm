package vm

import (
	"context"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/gitprotect"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/netpolicy"
	"github.com/devvmhq/devvm/internal/tools"
	"github.com/devvmhq/devvm/internal/vmname"
)

// Listed is a project VM as devvm ls shows it. A field is "" when the VM's
// state doesn't record it (its creation didn't finish).
type Listed struct {
	Name   vmname.VM
	Status string   // as its backend reports it
	Image  string   // its image: base, or its layers joined with +
	Tools  []string // its shared tools: node claude
	From   string   // the container image its base started from (--from), if any
	Git    string   // its .git protection: on, off, or clone for a --clone VM
	Net    string   // its tier, or unenforced (netpolicy.Listed)
	// Learning is since when it learns the hosts it reaches, on the
	// internet tier; the zero time when it doesn't.
	Learning time.Time
	Backend  string
	Folder   string
	// FolderState is what's at Folder's path (Env.FolderOf): a VM whose
	// folder is gone or not connected is deleted only by name, with
	// devvm rm VM.
	FolderState FolderState
	// Links is how many links it has to other VMs' ports, as a client.
	Links int
}

// ListAll is devvm ls's list: the project VMs every backend has (List),
// and the states it couldn't read, under a status line, as it runs each
// backend's list and looks up each VM's folder.
func ListAll(ctx context.Context, e *Env) ([]Listed, []Known, error) {
	defer e.Status(msg.ReadingVMs())()
	inv, err := ReadInventory(ctx, e.Dirs, e.Registry, AllBackends)
	if err != nil {
		return nil, nil, err
	}
	ls, err := inv.List(ctx, e)
	return ls, inv.Unreadable(), err
}

// List lists the project VMs the backends' lists have, sorted by name
// (ReadInventory, AllBackends): it fails as the inventory's Err does. A
// VM whose state can't be read is listed with its status alone, as one
// with no state is. Each VM's folder is e's (Env.FolderOf), so its run
// looks each mount's volume up once.
func (inv *Inventory) List(ctx context.Context, e *Env) ([]Listed, error) {
	if err := inv.Err(); err != nil {
		return nil, err
	}
	var out []Listed
	for _, k := range inv.VMs {
		if !k.Listed {
			continue
		}
		l, err := e.listed(ctx, k)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, nil
}

// listed is one VM's row, from its backend and its state: its status
// alone for a state that can't be read.
func (e *Env) listed(ctx context.Context, k Known) (Listed, error) {
	in := k.Instance
	l := Listed{Name: in.Name, Status: string(in.Status), Backend: k.Backend.Name(), FolderState: FolderThere}
	st := k.State
	if st == nil {
		return l, nil
	}
	l.Image, l.Tools, l.Git, l.Folder = st.Image, tools.Names(st), gitprotect.Mode(st), st.Dir
	l.FolderState, l.Links = e.FolderOf(ctx, st), len(linksOf(st).Links)
	if l.Image != "" {
		l.From = st.From
	}
	var err error
	l.Net, err = netpolicy.Listed(ctx, k.Backend, st, in.Status == backend.StatusRunning)
	l.Learning = netpolicy.LearningOf(st).Since
	return l, err
}
