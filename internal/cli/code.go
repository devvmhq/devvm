package cli

import (
	"github.com/spf13/cobra"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/vm"
)

// newCodeCmd is devvm code.
func newCodeCmd(a *app) *cobra.Command {
	var include bool
	cmd := &cobra.Command{
		Use: "code",
		Annotations: about{
			docs: "using/ssh#vs-code-and-cursor", group: groupVM,
			noJSON: "it opens VS Code",
		}.annotations(),
		Short: "Open this folder's VM in VS Code over Remote-SSH",
		Long: "Get this folder's VM ready as devvm host does, then open its folder in VS Code over Remote-SSH, " +
			"with VS Code's code command.",
		Example: examples(
			"devvm code", "open this folder's VM in VS Code",
			"devvm code --ssh-config", "the same, adding devvm's Include line to ~/.ssh/config without asking",
		),
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.env(func(env *vm.Env) error {
				env.AddInclude = include
				return vm.Launch{Env: env}.Code(cmd.Context())
			}, msg.CodeFailed)
		},
	}
	addSSHConfigFlag(cmd.Flags(), &include)
	addFolderFlags(cmd, a)
	return cmd
}
