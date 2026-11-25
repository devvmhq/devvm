package cli

import (
	"github.com/spf13/cobra"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/vm"
)

// newRmCmd is devvm rm [VM] [--force] [--no-export] [--no-clone-check]
// [--no-worktree-check].
func newRmCmd(a *app) *cobra.Command {
	var o vm.RmOptions
	cmd := &cobra.Command{
		Use: "rm [VM]",
		Annotations: about{
			docs: "using/vms#deleting-a-vm", group: groupVM,
			noJSON: "it deletes",
		}.annotations(),
		Short: "Delete this folder's VM, or a VM whose folder is gone",
		Long: "Delete this folder's VM and what devvm keeps for it, or the VM you name when its folder is gone, after asking in a terminal without --no-input, which --force never answers when its folder is on a disk that isn't connected. " +
			"When its image has an agent, devvm first exports the agents' history, unless you give --no-export. " +
			"A --clone VM's work that this repo hasn't fetched is offered for fetching first, unless you give --no-clone-check, and devvm shows the uncommitted changes in the agents' worktrees and asks before they go, unless you give --no-worktree-check.",
		Example: examples(
			"devvm rm", "delete this folder's VM, after asking",
			"devvm rm --force", "delete it without asking, as a script must",
			"devvm rm --no-export", "delete it without exporting the agents' history",
			"devvm rm --no-clone-check", "delete a --clone VM without checking its clone, losing its unfetched work",
			"devvm rm --no-worktree-check", "delete it without checking the agents' worktrees, losing their uncommitted changes",
			"devvm rm devvm-web-71a99b1076d92c", "delete a VM whose folder is gone, by its name as devvm ls lists it",
		),
		Args: cobra.MatchAll(maxArgs(1), noFolderFlagsWithVM),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				name := args[0]
				return a.env(func(env *vm.Env) error { return vm.Lifecycle{Env: env}.RmVM(cmd.Context(), name, o) },
					func(reason string) msg.M { return msg.RmVMFailed(reason, name) })
			}
			return a.env(func(env *vm.Env) error { return vm.Lifecycle{Env: env}.Rm(cmd.Context(), o) }, msg.RmFailed)
		},
	}
	addForceFlag(cmd.Flags(), &o.Force)
	addNoExportFlag(cmd.Flags(), &o.NoExport)
	addNoCloneCheckFlag(cmd.Flags(), &o.NoCloneCheck)
	addNoWorktreeCheckFlag(cmd.Flags(), &o.NoWorktreeCheck)
	addFolderFlags(cmd, a)
	return cmd
}

// noFolderFlagsWithVM refuses --here and --parent with a VM's name: they
// choose this folder's VM, and devvm rm VM deletes the one it names.
func noFolderFlagsWithVM(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	for _, name := range []string{"here", "parent"} {
		if cmd.Flags().Changed(name) {
			return msg.RmVMFolderFlag("--" + name)
		}
	}
	return nil
}
