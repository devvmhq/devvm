package cli

import (
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/gitprotect"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/netpolicy"
	"github.com/devvmhq/devvm/internal/ui"
	"github.com/devvmhq/devvm/internal/vm"
	"github.com/devvmhq/devvm/internal/vmname"
)

// newLsCmd is devvm ls.
func newLsCmd(a *app) *cobra.Command {
	u := a.u
	var asJSON bool
	cmd := &cobra.Command{
		Use:         "ls",
		Annotations: about{docs: "using/vms#listing-vms", group: groupVM}.annotations(),
		Short:       "List the project VMs",
		Long: "List the project VMs, sorted by name. " +
			"Each row has the VM's status, image, .git protection, network tier, links to your other VMs and folder.",
		Example: examples(
			"devvm ls", "list every project VM",
			"devvm ls --json", "the same list as JSON, for scripts",
		),
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ls, err := listVMs(cmd, a)
			if err != nil {
				return failed(err, msg.LsFailed)
			}
			if asJSON {
				return printJSON(u, lsJSON(ls))
			}
			return writeList(u.Table(), ls)
		},
	}
	jsonFlag(cmd, &asJSON)
	return cmd
}

// lsOutput is devvm ls --json: the project VMs, by name. What a VM's
// state doesn't have is "".
type lsOutput struct {
	VMs []vmJSON `json:"vms"`
}

type vmJSON struct {
	Name   vmname.VM `json:"name"`
	Status string    `json:"status"`
	Image  string    `json:"image"`
	// Tools is its shared tools: node, claude.
	Tools []string `json:"tools"`
	From  string   `json:"from,omitempty"`
	Git   string   `json:"git"`
	Net   string   `json:"net"`
	// LearningSince is since when it learns the hosts it reaches, on the
	// internet tier, when it does.
	LearningSince string `json:"learning_since,omitempty"`
	Backend       string `json:"backend"`
	Folder        string `json:"folder"`
	// FolderState is what's at Folder's path: there, gone, or
	// not_connected (vm.FolderState); devvm rm VM deletes a VM whose
	// folder is gone or not connected.
	FolderState string `json:"folder_state"`
	// Links is how many links the VM has to other VMs' ports.
	Links int `json:"links"`
}

func lsJSON(ls []vm.Listed) lsOutput {
	out := lsOutput{VMs: make([]vmJSON, 0, len(ls))}
	for _, l := range ls {
		out.VMs = append(out.VMs, vmJSON{
			Name: l.Name, Status: l.Status, Image: l.Image, Tools: append([]string{}, l.Tools...), From: l.From, Git: l.Git, Net: l.Net,
			LearningSince: jsonTime(l.Learning), Backend: l.Backend, Folder: l.Folder, FolderState: string(l.FolderState), Links: l.Links,
		})
	}
	return out
}

// listVMs lists the VMs every backend has, warning of each state file it
// can't read.
func listVMs(cmd *cobra.Command, a *app) ([]vm.Listed, error) {
	env, err := a.vmEnv()
	if err != nil {
		return nil, err
	}
	ls, unreadable, err := vm.ListAll(cmd.Context(), env)
	if err != nil {
		return nil, err
	}
	for _, k := range unreadable {
		a.u.Warn(msg.VMStateUnreadable(k.Path).Because(k.Err))
	}
	return ls, nil
}

// writeList writes the VMs as a table, a header first; a value a VM lacks
// is -, and so are TOOLS for a VM without shared tools and LINKS for one
// without links. A VM's status, .git protection and network tier are
// colored by what they mean, and a folder that's gone or not connected is
// yellow, with (gone) or (not connected) after it.
func writeList(tab *ui.Table, ls []vm.Listed) error {
	tab.Header("NAME", "STATUS", "IMAGE", "TOOLS", "GIT", "NET", "LINKS", "FOLDER")
	for _, l := range ls {
		image := l.Image
		if l.From != "" {
			image = tab.Text(msg.ImageFrom(l.From, l.Image))
		}
		links := ""
		if l.Links > 0 {
			links = strconv.Itoa(l.Links)
		}
		folder := cell(l.Folder)
		switch l.FolderState {
		case vm.FolderGone:
			folder = ui.Cell{Text: l.Folder + " (gone)", Role: ui.Attention}
		case vm.FolderNotConnected:
			folder = ui.Cell{Text: l.Folder + " (not connected)", Role: ui.Attention}
		case vm.FolderThere:
		}
		net := cellAs(l.Net, netModes)
		if shown := netpolicy.Shown(l.Net, !l.Learning.IsZero()); shown != l.Net {
			net = ui.Cell{Text: shown, Role: ui.Attention}
		}
		tab.Row(cell(l.Name.String()), cellAs(l.Status, vmStatuses), cell(image), cell(strings.Join(l.Tools, ",")), cellAs(l.Git, gitModes),
			net, cell(links), folder)
	}
	return tab.Flush()
}

// vmStatuses, gitModes and netModes color devvm ls's STATUS, GIT and NET
// columns: a status other than running or stopped (Broken) is red, and so
// is a tier that isn't enforced; a confined tier is green.
func vmStatuses(s string) ui.Role {
	switch backend.Status(s) {
	case backend.StatusRunning:
		return ui.Good
	case backend.StatusStopped:
		return ui.Attention
	case "":
		return ui.Plain
	}
	return ui.Bad
}

func gitModes(s string) ui.Role {
	switch s {
	case gitprotect.On, gitprotect.Clone:
		return ui.Good
	}
	return ui.Plain
}

func netModes(s string) ui.Role {
	switch t := netpolicy.Tier(s); {
	case s == netpolicy.CheckUnenforced:
		return ui.Bad
	case t.Valid() && t.Confined():
		return ui.Good
	}
	return ui.Plain
}
