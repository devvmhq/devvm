package cli

import (
	"os"
	"slices"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/devvmhq/devvm/internal/from"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/vm"
)

// addFromFlag adds --from, the container image a base starts from.
func addFromFlag(f *pflag.FlagSet, p *string) {
	f.StringVar(p, "from", "", "the container `IMAGE` base starts from, such as node:24, instead of Ubuntu's cloud image (default: $DEVVM_FROM)")
}

// addAgentsFlag adds --agents, a list of coding agents: a new VM's, or
// new VMs' default with devvm setup. usage says whose.
func addAgentsFlag(f *pflag.FlagSet, p *string, usage string) {
	f.StringVar(p, "agents", "", usage)
}

// addLayersFlag adds --layers, a list of layers: a new VM's, or new VMs'
// default with devvm setup. usage says whose.
func addLayersFlag(f *pflag.FlagSet, p *string, usage string) {
	f.StringVar(p, "layers", "", usage)
}

// addForceFlag adds -f/--force, which only skips a confirmation.
func addForceFlag(f *pflag.FlagSet, p *bool) {
	f.BoolVarP(p, "force", "f", false, "don't ask to confirm, which devvm needs without a terminal or with --no-input")
}

// addNoExportFlag adds --no-export, which skips exporting the agents'
// history before a VM's disk goes (devvm rm, devvm layer add, devvm
// layer rm, devvm update, devvm clone).
func addNoExportFlag(f *pflag.FlagSet, p *bool) {
	f.BoolVar(p, "no-export", false, "don't export the agents' history first")
}

// addNoCloneCheckFlag adds --no-clone-check, which skips checking a
// --clone VM's clone before its disk goes (devvm rm, devvm clone off).
func addNoCloneCheckFlag(f *pflag.FlagSet, p *bool) {
	f.BoolVar(p, "no-clone-check", false, "don't check a --clone VM's clone for unfetched commits and uncommitted changes first, which are lost")
}

// addReposFlag adds --repos, which answers the question of a --clone VM's
// repos in a folder of repos (devvm --clone, devvm clone on).
func addReposFlag(f *pflag.FlagSet, p *string) {
	f.StringVar(p, "repos", "", "the repos a --clone VM of a folder of repos gets: all, none or a `LIST` of their paths, such as frontend,backend (default: a picker in a terminal)")
}

// addNoWorktreeCheckFlag adds devvm rm's --no-worktree-check, which skips
// checking the agents' worktrees in the VM before its disk goes; a remake
// has its own, which goes only with --clean-home (addRemakeFlags).
func addNoWorktreeCheckFlag(f *pflag.FlagSet, p *bool) {
	f.BoolVar(p, "no-worktree-check", false, "don't check the agents' worktrees in the VM for uncommitted changes first, which are lost")
}

// addRemakeFlags adds the flags of a command that makes the folder's VM
// again (devvm layer add, devvm layer rm, devvm update, devvm clone):
// --force, --no-export, --clean-home and --no-worktree-check, which goes
// only with --clean-home, as only that loses the worktrees.
func addRemakeFlags(cmd *cobra.Command, o *vm.RemakeOptions) {
	f := cmd.Flags()
	addForceFlag(f, &o.Force)
	addNoExportFlag(f, &o.NoExport)
	f.BoolVar(&o.CleanHome, "clean-home", false, "give the VM the image's home folder instead of keeping its own")
	f.BoolVar(&o.NoWorktreeCheck, "no-worktree-check", false, "with --clean-home, don't check the agents' worktrees in the VM for uncommitted changes first, which are lost")
	needs(cmd, "no-worktree-check", "--clean-home")
}

// addSSHConfigFlag adds --ssh-config, which answers the question whether
// to add devvm's Include line to ~/.ssh/config.
func addSSHConfigFlag(f *pflag.FlagSet, p *bool) {
	f.BoolVar(p, "ssh-config", false, "add devvm's Include line to ~/.ssh/config without asking")
}

// folderFlags are --here and --parent: which VM a command on this
// folder's VM uses when a folder it's inside has one.
type folderFlags struct{ here, parent bool }

// rule is the VM environment's rule for them (vm.Env.Folder).
func (ff folderFlags) rule() vm.FolderRule {
	switch {
	case ff.here:
		return vm.FolderHere
	case ff.parent:
		return vm.FolderParent
	}
	return vm.FolderAuto
}

// addFolderFlags adds --here and --parent to cmd, a command that acts on
// this folder's VM, into the run's (a.folder), which its VM environment
// finds the VM by. They can't go together, nor with -g on a command that
// has it, added before them. It returns cmd.
func addFolderFlags(cmd *cobra.Command, a *app) *cobra.Command {
	f := cmd.Flags()
	f.BoolVar(&a.folder.here, "here", false, "use this folder's own VM, never that of a folder it's inside")
	f.BoolVar(&a.folder.parent, "parent", false, "use the VM of the nearest folder this one is inside, even across a git repo of its own")
	names := []string{"here", "parent"}
	if f.Lookup("global") != nil {
		names = append(names, "global")
	}
	exclusive(cmd, names...)
	return cmd
}

// fromRef is --from as given, else its default from the environment.
func fromRef(flag string) string {
	if flag != "" {
		return flag
	}
	return os.Getenv(from.Env)
}

// noBlanks refuses a flag given an empty value, and an empty argument: no
// flag or argument takes a blank to mean none or its default. A command
// that passes its arguments on as they are (limactl) is left alone.
func noBlanks(cmd *cobra.Command, args []string) error {
	if cmd.DisableFlagParsing {
		return nil
	}
	var err error
	cmd.Flags().Visit(func(f *pflag.Flag) {
		sv, slice := f.Value.(pflag.SliceValue)
		switch {
		case err != nil:
		case f.Value.Type() == "string" && f.Value.String() == "":
			err = msg.FlagBlank(f.Name)
		case slice && (len(sv.GetSlice()) == 0 || slices.Contains(sv.GetSlice(), "")):
			// --host "", or a comma too many: --host a.com,
			err = msg.FlagBlank(f.Name)
		}
	})
	if err != nil {
		return err
	}
	if slices.Contains(args, "") {
		return msg.ArgBlank(cmd.CommandPath())
	}
	return nil
}
