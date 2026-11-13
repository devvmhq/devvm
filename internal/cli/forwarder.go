package cli

import (
	"io/fs"

	"github.com/spf13/cobra"

	"github.com/devvmhq/devvm/assets"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/vmssh"
)

// newForwarderCmd is devvm _ports: a VM's port forwarder (vmssh.Main),
// on every backend, which devvm starts detached (vmssh.Forwards). It
// gets ports.sh and devvm's folders, where it reads the ports it never
// forwards (ports.Reserved); its log lines are data, written to devvm's
// stderr. Hidden: it's not for users. Its flags are the forwarder's own,
// parsed by vmssh.Main.
func newForwarderCmd(a *app) *cobra.Command {
	u := a.u
	return &cobra.Command{
		Use:                vmssh.Cmd + " [flags]",
		Short:              "Forward a VM's ports",
		Hidden:             true,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			fail := func(err error) error { return msg.ForwarderFailed(err.Error()).Because(err) }
			d, err := a.dirs()
			if err != nil {
				return fail(err)
			}
			script, err := fs.ReadFile(assets.FS, "guest/ports.sh")
			if err != nil {
				return fail(err)
			}
			if err := vmssh.Main(cmd.Context(), args, script, d, u.Stderr()); err != nil {
				return fail(err)
			}
			return nil
		},
	}
}
