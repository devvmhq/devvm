package cli

import (
	"github.com/spf13/cobra"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/vm"
)

// newPasteCmd is devvm paste: the clipboard's image, PDF or copied file
// into this folder's VM, its path copied back, and devvm paste rm.
func newPasteCmd(a *app) *cobra.Command {
	u := a.u
	var asJSON, sendFile bool
	c := &cobra.Command{
		Use:         "paste",
		Annotations: about{docs: "using/coding-agents#pasting-images", group: groupVM}.annotations(),
		Short:       "Send the clipboard's image, PDF or copied file to this folder's VM",
		Long: "Put the image, the PDF, or the one copied file, that's on this machine's clipboard into this folder's VM, " +
			"which must be running, as a new file in its paste folder, and copy that file's path to the clipboard. " +
			"A copied file is sent after asking, showing its path on this machine, " +
			"and only a file in this folder, which the VM already reads, is sent without asking, with --send-file. " +
			"Then paste the path into the agent with your terminal's paste shortcut.",
		Example: examples(
			"devvm paste", "send the copied image, and copy its path in the VM",
			"devvm paste --json", "the same, printing the path and what was sent as JSON",
			"devvm paste --send-file", "send a copied file in this folder without asking, as a script must",
			"devvm paste rm", "delete the files devvm paste left in the VM",
		),
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var sent vm.PasteSent
			err := a.env(func(env *vm.Env) error {
				var err error
				sent, err = vm.Paste{Env: env}.Send(cmd.Context(), sendFile)
				return err
			}, msg.PasteFailed)
			switch {
			case err != nil:
				return err
			case asJSON:
				return printJSON(u, pasteJSON(sent))
			case sent.Path == "": // you said no to sending the file
				return nil
			case !sent.Copied: // the warning said copying failed
				u.Info(msg.PastePut(sent.Path, sent.VM))
				return nil
			}
			u.Info(msg.PasteSent(sent.Path, sent.VM))
			return nil
		},
	}
	jsonFlag(c, &asJSON)
	c.Flags().BoolVar(&sendFile, "send-file", false, "send a copied file in this folder without asking")
	addFolderFlags(c, a)
	c.AddCommand(newPasteRmCmd(a))
	return c
}

// pasteOutput is devvm paste --json: whether anything was sent (false
// when you said no to sending a file), the VM, the file's path in it (""
// when nothing was sent), its kind (image, pdf or file), an image's or a
// PDF's type ("" for a file), a copied file's path on this machine (""
// otherwise), its size in bytes, and whether the path reached the
// clipboard.
type pasteOutput struct {
	Sent   bool   `json:"sent"`
	VM     string `json:"vm"`
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Type   string `json:"type"`
	Source string `json:"source"`
	Bytes  int64  `json:"bytes"`
	Copied bool   `json:"copied"`
}

func pasteJSON(s vm.PasteSent) pasteOutput {
	return pasteOutput{Sent: s.Path != "", VM: s.VM, Path: s.Path, Kind: s.Kind, Type: s.Type, Source: s.Source, Bytes: s.Bytes, Copied: s.Copied}
}

// newPasteRmCmd is devvm paste rm.
func newPasteRmCmd(a *app) *cobra.Command {
	var force bool
	c := &cobra.Command{
		Use:         "rm",
		Annotations: about{docs: "using/coding-agents#pasting-images", noJSON: "it deletes"}.annotations(),
		Short:       "Delete the files devvm paste left in this folder's VM",
		Long: "Delete the images and files that devvm paste put in this folder's VM's paste folder, after asking, " +
			"and nothing else in that folder. The VM must be running.",
		Example: examples(
			"devvm paste rm", "delete them after asking",
			"devvm paste rm --force", "delete them without asking, as a script must",
		),
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.env(func(env *vm.Env) error { return vm.Paste{Env: env}.Rm(cmd.Context(), force) }, msg.PasteRmFailed)
		},
	}
	addForceFlag(c.Flags(), &force)
	addFolderFlags(c, a)
	return c
}
