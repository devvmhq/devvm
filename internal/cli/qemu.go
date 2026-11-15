package cli

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/devvmhq/devvm/internal/backend/lima"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/ui"
)

// newQEMUCmd is devvm _qemu, which the Lima backend's QEMU wrapper runs on
// Linux hosts. Hidden: it's not for users.
//
//   - _qemu exec WRAPPER ARGS...: run the real QEMU for WRAPPER in devvm's
//     place, its network restricted, in a namespace that pins what its
//     start pins.
//   - _qemu connect ADDRESS [SLOTS]: join standard input and output to
//     ADDRESS (QEMU's guestfwd runs it for each connection to the proxy),
//     holding one of the VM's slots in the folder SLOTS while it does.
//   - _qemu pinned QEMU ARGS..., _qemu drop QEMU ARGS...: in QEMU's
//     namespace, pin what DEVVM_QEMU_PINS says and run QEMU as a child,
//     keeping the pins until it ends; the child drops every capability
//     and becomes QEMU.
//   - _qemu probe DIR: in a namespace, check that DIR/f can be pinned.
func newQEMUCmd(u *ui.UI) *cobra.Command {
	return &cobra.Command{
		Use:                "_qemu exec WRAPPER [ARGS...] | _qemu connect ADDRESS [SLOTS]",
		Short:              "Run QEMU with devvm's network restrictions",
		Hidden:             true,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			switch {
			case len(args) >= 2 && args[0] == "exec":
				self, err := paths.Self()
				if err != nil {
					return err
				}
				return lima.RunQEMU(self, args[1], args[2:])
			case len(args) == 2 && args[0] == "connect":
				// A VM started before the slots.
				return lima.Connect(args[1], "", cmd.InOrStdin(), u.Stdout())
			case len(args) == 3 && args[0] == "connect":
				return lima.Connect(args[1], args[2], cmd.InOrStdin(), u.Stdout())
			case len(args) >= 2 && args[0] == "pinned":
				self, err := paths.Self()
				if err != nil {
					return err
				}
				return lima.RunPinned(self, args[1], args[2:])
			case len(args) >= 2 && args[0] == "drop":
				return lima.Drop(args[1], args[2:])
			case len(args) == 2 && args[0] == "probe":
				return lima.ProbePin(args[1])
			}
			return errors.New("usage: devvm " + cmd.Use)
		},
	}
}
