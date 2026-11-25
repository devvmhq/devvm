package cli

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/devvmhq/devvm/internal/build"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/vm"
)

// newLayerCmd is devvm layer: new NAME, add LAYER... and rm LAYER....
func newLayerCmd(a *app) *cobra.Command {
	u := a.u
	cmd := &cobra.Command{
		Use: "layer",
		Annotations: about{
			docs: "using/customizing#your-own-layers", group: groupImages,
			noJSON: "it prints its help",
		}.annotations(),
		Short: "Make image layers of your own, or change this folder's VM's layers",
		Long: "Make image layers of your own, which are scripts in ~/.config/devvm/layers that an image runs on top of base, or add layers to this folder's VM and take them out. " +
			"An image names them as it names devvm's built-in layers, bottom first, such as docker+NAME, and a new VM gets them with --layers.",
		Example: examples(
			"devvm layer new mailpit", "start a layer named mailpit",
			"devvm layer add docker", "make this folder's VM again with the docker layer added",
			"devvm layer rm docker", "make this folder's VM again without it",
		),
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newLayerAddCmd(a), newLayerRmCmd(a))
	cmd.AddCommand(&cobra.Command{
		Use: "new NAME",
		Annotations: about{
			docs: "using/customizing#your-own-layers", group: groupImages,
			noJSON: "it writes a file",
		}.annotations(),
		Short: "Start a layer of your own",
		Long: "Start a layer of your own, ~/.config/devvm/layers/NAME.sh, a script to edit, and NAME.policy next to it, " +
			"the hosts that VMs with the layer can reach on the allowlist tier. " +
			"NAME is lowercase letters, digits and dashes, and not base or a built-in layer's or shared tool's name.",
		Example: examples(
			"devvm layer new mailpit", "start the mailpit layer, then edit its script",
			"devvm build mailpit", "build its image",
			"devvm --layers mailpit", "create a VM with the layer",
		),
		Args: cobra.MatchAll(exactArgs(1), func(_ *cobra.Command, args []string) error {
			return build.CheckLayerName(args[0])
		}),
		RunE: func(_ *cobra.Command, args []string) error {
			dirs, err := a.dirs()
			var script, policy string
			if err == nil {
				script, policy, err = build.NewLayer(dirs, args[0])
			}
			if err != nil {
				return failed(err, msg.LayerNewFailed)
			}
			u.Info(msg.LayerCreated(args[0], policy, script))
			return nil
		},
	})
	return cmd
}

// newLayerAddCmd is devvm layer add LAYER... [--force] [--no-export]
// [--clean-home [--no-worktree-check]].
func newLayerAddCmd(a *app) *cobra.Command {
	var o vm.RemakeOptions
	cmd := &cobra.Command{
		Use: "add LAYER...",
		Annotations: about{
			docs: "using/images#adding-layers-to-a-vm", group: groupChange,
			noJSON: "it makes a VM again, then opens a shell",
		}.annotations(),
		Short: "Make this folder's VM again with more layers, then open a shell in it",
		Long: "Make this folder's VM again from an image with each LAYER added on top, in the order given, then open a shell in it, after asking in a terminal without --no-input. " +
			"devvm keeps its home folder, the project folder and its settings, and everything else on its disk is lost. " +
			"When its image has an agent, devvm first exports the agents' history, unless you give --no-export.",
		Example: examples(
			"devvm layer add desktop", "add the desktop layer to the VM, after asking",
			"devvm layer add docker nix", "add docker, then nix on top of it",
			"devvm layer add desktop --force", "add it without asking, as a script must",
			"devvm layer add docker --clean-home", "add docker, and start from the image's home folder",
		),
		Args: cobra.MatchAll(minArgs(1), func(_ *cobra.Command, args []string) error {
			dirs, err := a.dirs()
			if err != nil {
				return err
			}
			return vm.CheckLayers(args, (&build.Env{Dirs: dirs}).Layers()[1:])
		}),
		RunE: func(cmd *cobra.Command, args []string) error {
			command := "devvm layer add " + strings.Join(args, " ")
			return a.env(func(env *vm.Env) error {
				return vm.Lifecycle{Env: env}.AddLayers(cmd.Context(), command, args, o)
			}, msg.RemakeFailed)
		},
		ValidArgsFunction: completeLayerNames(a),
	}
	addRemakeFlags(cmd, &o)
	return addFolderFlags(cmd, a)
}

// newLayerRmCmd is devvm layer rm LAYER... [--force] [--no-export]
// [--clean-home [--no-worktree-check]].
func newLayerRmCmd(a *app) *cobra.Command {
	var o vm.RemakeOptions
	cmd := &cobra.Command{
		Use: "rm LAYER...",
		Annotations: about{
			docs: "using/images#taking-layers-out-of-a-vm", group: groupChange,
			noJSON: "it makes a VM again, then opens a shell",
		}.annotations(),
		Short: "Make this folder's VM again without some of its layers, then open a shell in it",
		Long: "Make this folder's VM again from an image without each LAYER, its other layers kept in their order, then open a shell in it, after asking in a terminal without --no-input. " +
			"devvm keeps its home folder, the project folder and its settings, and everything else on its disk is lost. " +
			"When its image has an agent, devvm first exports the agents' history, unless you give --no-export.",
		Example: examples(
			"devvm layer rm desktop", "take the desktop layer out of the VM, after asking",
			"devvm layer rm docker nix", "take out docker and nix",
			"devvm layer rm desktop --force", "take it out without asking, as a script must",
			"devvm layer rm docker --clean-home", "take out docker, and start from the image's home folder",
		),
		Args: cobra.MatchAll(minArgs(1), func(_ *cobra.Command, args []string) error {
			dirs, err := a.dirs()
			if err != nil {
				return err
			}
			return vm.CheckLayerNames(args, (&build.Env{Dirs: dirs}).Layers()[1:])
		}),
		RunE: func(cmd *cobra.Command, args []string) error {
			command := "devvm layer rm " + strings.Join(args, " ")
			return a.env(func(env *vm.Env) error {
				return vm.Lifecycle{Env: env}.RemoveLayers(cmd.Context(), command, args, o)
			}, msg.RemakeFailed)
		},
		ValidArgsFunction: completeLayerNames(a),
	}
	addRemakeFlags(cmd, &o)
	return addFolderFlags(cmd, a)
}
