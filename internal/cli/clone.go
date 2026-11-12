package cli

import (
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/vm"
)

// newCloneCmd is devvm clone [on|off] [--force] [--no-export]
// [--clean-home] [--no-clone-check] [--repos LIST], and its fetch and
// add PATH.
func newCloneCmd(a *app) *cobra.Command {
	var o vm.RemakeOptions
	var asJSON bool
	cmd := &cobra.Command{
		Use:         "clone [on|off]",
		Annotations: about{docs: "using/clone#switching-a-vm-that-exists", group: groupChange}.annotations(),
		Short:       "Show or change whether this folder's VM has its own clone of the repo",
		Long: "Show whether this folder's VM has its own clone of the repo, or of each repo in a folder of repos, on, or mounts this folder, off. " +
			"devvm clone on and devvm clone off make the VM again from its image, after asking in a terminal without --no-input, then open a shell in it, and devvm keeps its home folder and its settings, while everything else on its disk is lost. " +
			"On copies the repos' branches into the new clone, and off first offers to fetch the clone's work into this machine's repos, unless you give --no-clone-check.",
		Example: examples(
			"devvm clone", "show whether the VM has its own clone of the repo",
			"devvm clone on", "make the VM again with its own clone of the repo, after asking",
			"devvm clone off", "fetch the clone's work, then make the VM again with this folder mounted",
			"devvm clone on --force", "make it again without asking, as a script must",
			"devvm clone fetch", "fetch the VM's branches into each repo on this machine",
		),
		Args:      showsOnly(maxArgs(1), "force", "no-export", "clean-home", "no-clone-check", "repos"),
		ValidArgs: []string{vm.CloneOn, vm.CloneOff},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				command := "devvm clone " + args[0]
				return a.env(func(env *vm.Env) error {
					return vm.Lifecycle{Env: env}.SetClone(cmd.Context(), command, args[0], o)
				}, msg.RemakeFailed)
			}
			var mode string
			if err := a.env(func(env *vm.Env) error {
				var err error
				mode, err = vm.Settings{Env: env}.Clone(cmd.Context())
				return err
			}, msg.CloneShowFailed); err != nil {
				return err
			}
			return printValue(a.u, asJSON, "clone", mode)
		},
	}
	addRemakeFlags(cmd.Flags(), &o)
	addNoCloneCheckFlag(cmd.Flags(), &o.NoCloneCheck)
	addReposFlag(cmd.Flags(), &o.Repos)
	needs(cmd, "repos", vm.CloneOn)
	jsonFlag(cmd, &asJSON)
	cmd.AddCommand(newCloneFetchCmd(a), newCloneAddCmd(a))
	return addFolderFlags(cmd, a)
}

// cloneFetchOutput is devvm clone fetch --json: the branches new or moved
// in each repo's remote-tracking branches, by repo and by name.
type cloneFetchOutput struct {
	Repos    []cloneFetchRepo   `json:"repos"`
	Branches []cloneFetchBranch `json:"branches"`
}

// cloneFetchRepo is one repo's fetch: its path relative to the folder
// ("." for the folder's own), its remote, its new or moved branches, and
// the local branches the VM's own work went to: its detached HEAD's
// commits and its stash entries.
type cloneFetchRepo struct {
	Path     string   `json:"path"`
	Remote   string   `json:"remote"`
	Branches []string `json:"branches"`
	Saved    []string `json:"saved,omitempty"`
}

// cloneFetchBranch is a branch name and the repos it's new or moved in.
type cloneFetchBranch struct {
	Name  string   `json:"name"`
	Repos []string `json:"repos"`
}

// newCloneFetchCmd is devvm clone fetch [--json].
func newCloneFetchCmd(a *app) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:         "fetch",
		Annotations: about{docs: "using/clone#fetching-the-vms-work"}.annotations(),
		Short:       "Fetch the work of this folder's --clone VM into each of its repos",
		Long: "Fetch every branch of each repo in this folder's --clone VM into the repo on this machine, as remote-tracking branches of its remote for the VM, such as devvm/main, starting the VM if it's stopped. " +
			"When a repo's HEAD in the VM is detached at commits no branch has, fetch those into a new local branch, saved/devvm/HEAD, and each stash entry the VM made into saved/devvm/stash-N. " +
			"Then list the branches that are new or moved, each with the repos it's in.",
		Example: examples(
			"devvm clone fetch", "fetch every repo's branches, and list what's new",
			"devvm clone fetch --json", "list what's new for a script",
		),
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var got vm.Fetched
			if err := a.env(func(env *vm.Env) error {
				var err error
				got, err = vm.Clone{Env: env}.Fetch(cmd.Context())
				return err
			}, msg.CloneFetchRunFailed); err != nil {
				return err
			}
			return printFetched(a, got, asJSON)
		},
	}
	jsonFlag(cmd, &asJSON)
	return addFolderFlags(cmd, a)
}

// printFetched prints what devvm clone fetch brought: with asJSON, a
// cloneFetchOutput; else each branch name and its repos on a line, the
// VM's names each on one line (msg.OneLine), then how to review them, or that there was
// nothing new.
func printFetched(a *app, got vm.Fetched, asJSON bool) error {
	byBranch := got.ByBranch()
	if asJSON {
		out := cloneFetchOutput{Repos: []cloneFetchRepo{}, Branches: []cloneFetchBranch{}}
		for _, r := range got.Repos {
			out.Repos = append(out.Repos, cloneFetchRepo{Path: r.Path, Remote: r.Remote, Branches: append([]string{}, r.Branches...), Saved: append([]string(nil), r.Saved...)})
		}
		for _, b := range byBranch {
			out.Branches = append(out.Branches, cloneFetchBranch{Name: b.Name, Repos: b.Repos})
		}
		return printJSON(a.u, out)
	}
	if len(byBranch) == 0 {
		// Every repo left alone has been named: nothing was looked at. A
		// detached HEAD's commits saved were named as they were.
		if got.Checked > 0 && len(got.Repos) == 0 {
			a.u.Info(msg.CloneFetchedNone())
		}
		return nil
	}
	var b strings.Builder
	for _, br := range byBranch {
		b.WriteString(msg.OneLine(br.Name) + ": " + msg.OneLine(strings.Join(br.Repos, ", ")) + "\n")
	}
	if _, err := io.WriteString(a.u.Stdout(), b.String()); err != nil {
		return err
	}
	a.u.Info(msg.CloneFetchedReview(got.Repos[0].Remote))
	return nil
}

// newCloneAddCmd is devvm clone add PATH.
func newCloneAddCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:         "add PATH",
		Annotations: about{docs: "using/clone#adding-a-repo", noJSON: "it changes the VM"}.annotations(),
		Short:       "Copy one more repo into this folder's --clone VM",
		Long: "Copy the repo at PATH, a folder in this folder's --clone VM's folder, into the VM at the same path, with its branches, tags and remote-tracking branches, checked out as it is here, starting the VM if it's stopped. " +
			"The repo gets a remote for the VM, as each of the VM's repos has.",
		Example: examples(
			"devvm clone add tools", "copy the repo in ./tools into the VM",
			"devvm clone add ../shared", "copy a repo beside this one, in the same folder of repos",
		),
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.env(func(env *vm.Env) error {
				return vm.Clone{Env: env}.Add(cmd.Context(), args[0])
			}, msg.CloneAddFailed)
		},
	}
	return addFolderFlags(cmd, a)
}
