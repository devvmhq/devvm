package cli

import (
	"github.com/spf13/cobra"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/vm"
)

// newHostCmd is devvm host [--ssh-config].
func newHostCmd(a *app) *cobra.Command {
	var include bool
	cmd := &cobra.Command{
		Use: "host",
		Annotations: about{
			docs: "using/ssh#devvm-host-devvm-code-and-devvm-cursor", group: groupVM,
			noJSON: "it shows one host name, for ssh as it is",
		}.annotations(),
		Short: "Get this folder's VM ready for ssh and editors, and show its host name",
		Long: "Get this folder's VM ready for ssh and editors as devvm does before it opens a shell, starting the VM if it's stopped. " +
			"Then show the host name that ssh and VS Code's Remote-SSH reach it by, such as devvm-website for a folder named website.",
		Example: examples(
			"devvm host", "get the VM ready and show its host name",
			"ssh devvm-website", "then connect by that name, for a folder named website",
		),
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.env(func(env *vm.Env) error {
				env.AddInclude = include
				return vm.Launch{Env: env}.ShowHost(cmd.Context())
			}, msg.SSHFailed)
		},
	}
	addSSHConfigFlag(cmd.Flags(), &include)
	addFolderFlags(cmd, a)
	return cmd
}
