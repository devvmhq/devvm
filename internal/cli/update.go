package cli

import (
	"github.com/spf13/cobra"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/vm"
)

// newUpdateCmd is devvm update [--force] [--no-export] [--clean-home
// [--no-worktree-check]].
func newUpdateCmd(a *app) *cobra.Command {
	var o vm.RemakeOptions
	cmd := &cobra.Command{
		Use: "update",
		Annotations: about{
			docs: "using/images#outdated-vms", group: groupVM,
			noJSON: "it makes a VM again, then opens a shell",
		}.annotations(),
		Short: "Bring this folder's VM up to date, then open a shell in it",
		Long: "Move this folder's VM to the newest shared tools, as devvm tools update does, then open a shell in it. " +
			"When its image changed since devvm made the VM, or the folder's devcontainer.json did, devvm then asks to make the VM again from the image's new build and the file as it is now, keeping its home folder, the project folder and its settings, and everything else on its disk is lost. " +
			"When its image has an agent, devvm first exports the agents' history, unless you give --no-export.",
		Example: examples(
			"devvm update", "move the VM to the newest tools, and to its image's new build after asking",
			"devvm update --force", "the same without asking, as a script must",
			"devvm update --clean-home", "start from the image's home folder too",
		),
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			o.DefaultFrom = fromRef("")
			return a.env(func(env *vm.Env) error {
				return vm.Lifecycle{Env: env}.Update(cmd.Context(), "devvm update", o)
			}, msg.RemakeFailed)
		},
	}
	addRemakeFlags(cmd, &o)
	addFolderFlags(cmd, a)
	return cmd
}
