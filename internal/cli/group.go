package cli

import (
	"github.com/spf13/cobra"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/ui"
	"github.com/devvmhq/devvm/internal/vm"
	"github.com/devvmhq/devvm/internal/vmname"
)

// newGroupCmd is devvm group: the group this folder's VM is in, shown,
// with join and leave.
func newGroupCmd(a *app) *cobra.Command {
	u := a.u
	var asJSON bool
	cmd := &cobra.Command{
		Use:         "group",
		Annotations: about{docs: "using/links#groups", group: groupLinks}.annotations(),
		Short:       "Show the group this folder's VM is in",
		Long: "Show the group this folder's VM is in, with each of its VMs, the name the others reach it by, its folder and the ports it publishes. " +
			"The VMs in a group reach each other's published ports by name, through links the group makes in each of them.",
		Example: examples(
			"devvm group", "show this folder's VM's group",
			"devvm group join web", "join the group web, made when its first VM joins",
			"devvm group leave", "leave it, removing the links the group made",
		),
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var g vm.GroupShown
			if err := a.env(func(env *vm.Env) error {
				var err error
				g, err = vm.Groups{Env: env}.Show(cmd.Context())
				return err
			}, msg.GroupFailed); err != nil {
				return err
			}
			if asJSON {
				return printJSON(u, groupJSON(g))
			}
			if g.Group == "" {
				u.Info(msg.GroupNone(g.VM))
				return nil
			}
			return writeGroup(u.Table(), g)
		},
	}
	jsonFlag(cmd, &asJSON)
	cmd.AddCommand(newGroupJoinCmd(a), newGroupLeaveCmd(a))
	return addFolderFlags(cmd, a)
}

// groupOutput is devvm group --json: the VM's group, "" for none, and its
// members, each with the name the others reach it by, its folder and its
// published ports (none for none).
type groupOutput struct {
	Group   string            `json:"group"`
	Members []groupMemberJSON `json:"members"`
}

type groupMemberJSON struct {
	Name      string    `json:"name"`
	VM        vmname.VM `json:"vm"`
	Folder    string    `json:"folder"`
	Published string    `json:"published"`
}

func groupJSON(g vm.GroupShown) groupOutput {
	out := groupOutput{Group: g.Group, Members: make([]groupMemberJSON, 0, len(g.Members))}
	for _, m := range g.Members {
		out.Members = append(out.Members, groupMemberJSON{Name: m.Name, VM: m.VM, Folder: m.Folder, Published: m.Published})
	}
	return out
}

// writeGroup writes the group's members as a table, a header first.
func writeGroup(tab *ui.Table, g vm.GroupShown) error {
	tab.Header("NAME", "VM", "FOLDER", "PUBLISHED")
	for _, m := range g.Members {
		tab.Row(cell(m.Name), cell(m.VM.String()), cell(m.Folder), cell(m.Published))
	}
	return tab.Flush()
}

// groupNameArg checks devvm group join's NAME.
func groupNameArg(_ *cobra.Command, args []string) error { return vm.CheckGroupName(args[0]) }

func newGroupJoinCmd(a *app) *cobra.Command {
	var as string
	cmd := &cobra.Command{
		Use:         "join NAME",
		Annotations: about{docs: "using/links#joining-a-group", noJSON: "it changes a setting"}.annotations(),
		Short:       "Put this folder's VM in a group of VMs",
		Long: "Put this folder's VM in the group NAME, which its first VM makes. " +
			"Each VM in the group then reaches the ports the others publish with devvm link publish, " +
			"by each one's folder name or the name it joined with, as in db:5432 and db.devvm.internal:5432.",
		Example: examples(
			"devvm group join web", "join the group web",
			"devvm group join web --as api2", "join it as api2 instead of this folder's name",
		),
		Args: cobra.MatchAll(exactArgs(1), groupNameArg),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.env(func(env *vm.Env) error { return vm.Groups{Env: env}.Join(cmd.Context(), args[0], as) }, msg.GroupFailed)
		},
	}
	cmd.Flags().StringVar(&as, "as", "", "the `NAME` the group's other VMs reach this VM by (default: its folder name)")
	return addFolderFlags(cmd, a)
}

func newGroupLeaveCmd(a *app) *cobra.Command {
	return addFolderFlags(&cobra.Command{
		Use:         "leave",
		Annotations: about{docs: "using/links#leaving-a-group", noJSON: "it changes a setting"}.annotations(),
		Short:       "Take this folder's VM out of its group",
		Long: "Take this folder's VM out of its group, and remove the links the group made, in this VM and in the group's other VMs, naming them. " +
			"The links you made with devvm link add stay, and the group goes with its last VM.",
		Example: examples(
			"devvm group leave", "leave this folder's VM's group",
		),
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.env(func(env *vm.Env) error { return vm.Groups{Env: env}.Leave(cmd.Context()) }, msg.GroupFailed)
		},
	}, a)
}
