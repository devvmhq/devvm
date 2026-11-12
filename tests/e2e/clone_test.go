//go:build e2e

package e2e

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/devvmhq/devvm/internal/shell"
)

// cloneSection: --clone, a VM with its own clone of a throwaway repo; rm's
// checks of its unfetched and uncommitted work; a git worktree's clone;
// devvm clone on and off; a folder of repos; repos git push couldn't
// copy (shallow, SHA-256, reftable). Host git and devvm run with a home of their
// own: no ~/.ssh/config, so nothing of yours is read or offered the
// Include line, and none of your git settings.
func cloneSection(s *S) {
	s.Section("--clone")
	ch := filepath.Join(s.sb.work, "clone-home")
	_ = os.RemoveAll(ch)
	_ = os.MkdirAll(ch, 0o700)
	madeFile(s.sb, ch)
	g := cloneGit{s: s, home: ch}
	// A throwaway repo, with a branch besides main.
	c, err := s.NewProject("cloned")
	if err == nil {
		err = g.repo(c)
	}
	// The repo's url.*.insteadOf rewrites the VM's URL to another host, which devvm's copy never uses.
	var vm string
	var out Res
	if err == nil {
		_, _ = g.git(c, "config", "url.elsewhere.invalid:/x.insteadOf", "devvm-")
		out, _ = g.devvm(c, "pwd; git branch --show-current\n", "--clone", "--network", "open")
		_, _ = g.git(c, "config", "--unset", "url.elsewhere.invalid:/x.insteadOf")
		vm = g.made(c)
	}
	s.Check("the VM has its own clone, with every branch, though insteadOf sent its URL elsewhere, only the shared tools mounted, and the shell opens in it", func() error {
		switch {
		case vm == "" || MountLine(s, vm) != s.Paths.Share+" ":
			return errors.New("the VM mounts more than the shared tools")
		case g.branches(vm, c) != "main other ":
			return errors.New("the VM's clone hasn't every branch")
		case !g.inVMHas(vm, c, "one"):
			return errors.New("the VM's clone hasn't the repo's commit")
		case !HasLine(out.Out, c) || !HasLine(out.Out, "main"):
			return errors.New("the shell didn't open in the clone, on main")
		case s.Paths.State(vm)["clone.on"] != "true":
			return errors.New("the VM's state hasn't clone.on")
		}
		return nil
	})
	s.Check("the repo has a devvm remote, by the VM's name", func() error {
		r, err := g.git(c, "remote", "get-url", "devvm")
		if err != nil || strings.TrimSpace(r.Stdout) != vm+":"+c {
			return errors.New("the repo's devvm remote isn't " + vm + ":" + c)
		}
		return nil
	})
	s.Check("nothing of .git is locked or watched", func() error {
		if s.Paths.State(vm)["git.mode"] != "clone" {
			return errors.New("the VM's git.mode isn't clone")
		}
		if g.locked(c) {
			return errors.New(".git/config is locked")
		}
		return nil
	})
	cloneRm(s, g, vm, c)
	cloneWorktree(s, g, c)
	cloneSwitch(s, g)
	cloneWorkspace(s, g)
	cloneShapes(s, g)
	cloneRefs(s, g)
	cloneInProgress(s, g)
	clonePartial(s, g)
	cloneSubmodules(s, g)
	cloneLFS(s, g)
}

// cloneGit runs host git and devvm in the clone section's home.
type cloneGit struct {
	s    *S
	home string
}

// env is the section's: none of your git settings.
func (g cloneGit) env() []string {
	return []string{"GIT_CONFIG_GLOBAL=" + filepath.Join(g.home, ".gitconfig"), "GIT_CONFIG_NOSYSTEM=1"}
}

// git runs host git on the repo in dir.
func (g cloneGit) git(dir string, args ...string) (Res, error) {
	return g.s.Run(Cmd{Args: append([]string{"git", "-C", dir}, args...), Env: g.env()})
}

// fetch fetches the VM's work into the repo in dir, through devvm's ssh
// config.
func (g cloneGit) fetch(dir string) error {
	env := append(g.env(), "GIT_SSH_COMMAND=ssh -F "+filepath.Join(g.s.Paths.Devvm, "ssh_config"))
	_, err := g.s.Run(Cmd{Args: []string{"git", "-C", dir, "fetch", "-q", "devvm"}, Env: env})
	return err
}

// devvm runs devvm in dir with the section's home, input its input.
func (g cloneGit) devvm(dir, input string, args ...string) (Res, error) {
	return g.s.Run(Cmd{Args: append([]string{g.s.sb.devvm}, args...), Dir: dir, Stdin: input, Env: append(g.env(), "HOME="+g.home)})
}

// repo makes dir a repo of one commit on main, with a branch besides it.
// A folder that's a repo already, made with --object-format or
// --ref-format, isn't made again: git 2.54's git init of a reftable repo
// drops its extensions.refStorage, and its next commit fails.
func (g cloneGit) repo(dir string) error {
	steps := [][]string{
		{"init", "-q", "-b", "main"},
		{"add", "a"},
		{"-c", "user.email=e@e", "-c", "user.name=e", "commit", "-qm", "one"},
		{"branch", "other"},
	}
	if Exists(filepath.Join(dir, ".git")) {
		steps[0] = []string{"rev-parse", "--git-dir"}
	}
	for i, st := range steps {
		if i == 1 {
			if err := os.WriteFile(filepath.Join(dir, "a"), []byte("a\n"), 0o600); err != nil {
				return err
			}
		}
		if _, err := g.git(dir, st...); err != nil {
			return err
		}
	}
	return nil
}

// made is the VM devvm made for dir, recorded for the run's cleanup.
func (g cloneGit) made(dir string) string {
	vm := g.s.Paths.VMOf(dir)
	if vm != "" && !slices.Contains(g.s.sb.created, vm) {
		g.s.sb.created = append(g.s.sb.created, vm)
	}
	return vm
}

// branches is the branches of the VM's clone of dir, each with a space
// after it.
func (g cloneGit) branches(vm, dir string) string {
	r, err := g.s.Limactl("shell", "--workdir", "/", vm, "git", "-C", dir, "branch", "--format=%(refname:short)")
	if err != nil {
		return ""
	}
	return strings.ReplaceAll(r.Stdout, "\n", " ")
}

// inVMHas is whether the VM's clone of dir has a commit whose line has
// text.
func (g cloneGit) inVMHas(vm, dir, text string) bool {
	r, err := g.s.Limactl("shell", "--workdir", "/", vm, "git", "-C", dir, "log", "--oneline")
	return err == nil && strings.Contains(r.Stdout, text)
}

// hasCommit is whether the host repo in dir's ref has a commit whose line
// has text.
func (g cloneGit) hasCommit(dir, ref, text string) bool {
	r, err := g.git(dir, "log", "--oneline", ref)
	return err == nil && strings.Contains(r.Stdout, text)
}

// noRemote is whether the repo in dir has no devvm remote and no remote
// refs left.
func (g cloneGit) noRemote(dir string) bool {
	r, err := g.git(dir, "remote")
	refs, err2 := g.git(dir, "for-each-ref", "refs/remotes")
	return err == nil && err2 == nil && !strings.Contains(r.Stdout, "devvm") && strings.TrimSpace(refs.Stdout) == ""
}

// locked is whether dir's .git/config is locked (macOS's uchg): never
// elsewhere.
func (g cloneGit) locked(dir string) bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	r, _ := g.s.Run(Cmd{Args: []string{"ls", "-lOd", filepath.Join(dir, ".git", "config")}})
	return strings.Contains(r.Out, "uchg")
}

// listed is whether limactl lists the VM.
func (g cloneGit) listed(vm string) bool {
	r, err := g.s.Limactl("list", "-q")
	return err == nil && HasLine(r.Stdout, vm)
}

// said is whether out has the first line of the message.
func (g cloneGit) said(out Res, name string, kv ...string) bool {
	return strings.Contains(out.Out, FirstLine(g.s.Msg(name, kv...)))
}

// cloneRm: rm refuses a clone's unfetched commits and uncommitted
// changes, in the clone and a worktree in it, until --no-clone-check.
func cloneRm(s *S, g cloneGit, vm, c string) {
	_, _ = s.InVM(vm, "cd "+shell.Quote(c)+" && echo b > b && git add b && git -c user.email=e@e -c user.name=e commit -qm from-vm")
	s.Check("rm without a terminal refuses with unfetched commits, even with --force, and keeps it", func() error {
		r, err := g.devvm(c, "", "rm", "--force")
		switch {
		case err == nil:
			return errors.New("rm succeeded")
		case !g.said(r, "no_terminal_fetch", "command=devvm rm"):
			return errors.New("rm didn't say there are commits to fetch")
		case !g.listed(vm):
			return errors.New(vm + " is gone")
		}
		return nil
	})
	s.Check("fetching through devvm's ssh config brings the VM's commit", func() error {
		if err := g.fetch(c); err != nil {
			return err
		}
		if !g.hasCommit(c, "devvm/main", "from-vm") {
			return errors.New("devvm/main hasn't the VM's commit")
		}
		return nil
	})
	_, _ = s.InVM(vm, "cd "+shell.Quote(c)+" && echo c > c")
	s.Check("then rm refuses with uncommitted changes in the clone, and keeps it", func() error {
		r, err := g.devvm(c, "", "rm", "--force")
		switch {
		case err == nil:
			return errors.New("rm succeeded")
		case !g.said(r, "no_terminal_delete", "command=devvm rm") || !HasLine(r.Out, "?? c"):
			return errors.New("rm didn't show the uncommitted change")
		case !g.listed(vm):
			return errors.New(vm + " is gone")
		}
		return nil
	})
	_, _ = s.InVM(vm, "cd "+shell.Quote(c)+" && rm c && git worktree add -q .worktrees/a && mkdir -p .git/info && echo .worktrees/ >> .git/info/exclude && echo w > .worktrees/a/w")
	s.Check("then rm refuses with uncommitted changes in a worktree in the clone, shown under its path, and keeps it", func() error {
		r, err := g.devvm(c, "", "rm", "--force")
		switch {
		case err == nil:
			return errors.New("rm succeeded")
		case !g.said(r, "clone_worktree_dirty", "path="+c+"/.worktrees/a") || !HasLine(r.Out, "?? w"):
			return errors.New("rm didn't show the worktree's change under its path")
		case g.said(r, "clone_dirty"):
			return errors.New("rm said the clone itself has changes")
		case !g.said(r, "no_terminal_delete", "command=devvm rm"):
			return errors.New("rm didn't say it can't ask")
		case !g.listed(vm):
			return errors.New(vm + " is gone")
		}
		return nil
	})
	s.Check("rm --no-clone-check deletes it unchecked after a warning, the worktree's change with it, and the remote fully; the fetched work stays as a local branch", func() error {
		r, err := g.devvm(c, "", "rm", "--force", "--no-export", "--no-clone-check")
		switch {
		case err != nil:
			return err
		case !g.said(r, "clone_check_skipped", "command=devvm rm", "vm="+vm) || g.said(r, "clone_worktree_dirty", "path="+c+"/.worktrees/a"):
			return errors.New("rm didn't warn that it skipped the check, or checked")
		case g.listed(vm) || Exists(s.Paths.VMDir(vm)):
			return errors.New(vm + " or its folder is still there")
		case !g.noRemote(c):
			return errors.New("the devvm remote or its refs are still there")
		case !g.hasCommit(c, "saved/devvm/main", "from-vm") || !g.said(r, "saved_branches", `branches="saved/devvm/main"`):
			return errors.New("the fetched work isn't saved/devvm/main")
		case Exists(filepath.Join(g.home, ".ssh", "config")):
			return errors.New("devvm wrote the home's ~/.ssh/config")
		}
		return nil
	})
}

// cloneWorktree: a git worktree of the repo, on a branch of its own whose
// upstream is main, the default branch: its VM's clone has those two and
// the repo's tags, not the repo's other local branches (other, and
// saved/devvm/main from cloneRm), and devvm says so.
func cloneWorktree(s *S, g cloneGit, c string) {
	var wvm string
	var out Res
	w, err := s.NewProject("cloned-wt")
	if err == nil {
		_, err = g.git(c, "tag", "wt-tag")
	}
	if err == nil {
		_, err = g.git(c, "worktree", "add", "-q", "-b", "wtb", w, "main")
	}
	if err == nil {
		_, err = g.git(w, "branch", "-q", "--set-upstream-to=main")
	}
	if err == nil {
		out, _ = g.devvm(w, "git branch --show-current\n", "--clone", "--network", "open")
		wvm = g.made(w)
	}
	s.Check("a git worktree's VM has a clone of its branch, its upstream, the default branch and the tags, not the repo's other branches, and devvm says so", func() error {
		switch {
		case wvm == "" || g.branches(wvm, w) != "main wtb ":
			return errors.New("the worktree's VM hasn't a clone of main and wtb alone")
		case !HasLine(out.Out, "wtb"):
			return errors.New("the shell didn't open on wtb")
		case !g.said(out, "clone_worktree_tips"):
			return errors.New("devvm didn't say the worktree's clone leaves the other branches out")
		}
		if r, err := s.InVM(wvm, "git -C "+shell.Quote(w)+" tag"); err != nil || !HasLine(r.Stdout, "wt-tag") {
			return errors.New("the worktree's VM hasn't the repo's tag")
		}
		return nil
	})
	_, _ = g.git(c, "tag", "-d", "wt-tag")
	_, _ = g.devvm(w, "", "rm", "--force", "--no-export")
	_, _ = g.git(c, "worktree", "remove", "--force", w)
}

// cloneSwitch: devvm clone on and off. A VM that mounts its folder, made
// again with its own clone of the repo, then made again mounting the
// folder, its clone's work fetched first; its home and ID kept.
func cloneSwitch(s *S, g cloneGit) {
	var svm string
	sp, err := s.NewProject("cloned-switch")
	if err == nil {
		err = g.repo(sp)
	}
	if err == nil {
		_, _ = g.devvm(sp, "echo in-home > ~/e2e-home.txt\n", "--network", "open")
		svm = g.made(sp)
	}
	var aside string
	if len(svm) >= 14 {
		aside = "devvm-" + svm[len(svm)-14:] + "-old"
		s.sb.created = append(s.sb.created, aside)
	}
	st := func(k string) string { return s.Paths.State(svm)[k] }
	sid := st("id")
	s.Check("clone without a terminal refuses on, and shows off", func() error {
		r, err := g.devvm(sp, "", "clone", "on")
		if err == nil || !strings.Contains(r.Out, s.Msg("no_input", "answer=--force")) {
			return errors.New("clone on didn't refuse without a terminal")
		}
		r, err = g.devvm(sp, "", "clone")
		if err != nil || strings.TrimRight(r.Stdout, "\n") != "off" {
			return errors.New("devvm clone didn't show off")
		}
		return nil
	})
	s.Check("clone on --force makes it again with its own clone of every branch, only the shared tools mounted, .git unlocked, its home and ID kept", func() error {
		r, err := g.devvm(sp, "pwd\n", "clone", "on", "--force")
		switch {
		case err != nil:
			return err
		case !HasLine(r.Out, sp):
			return errors.New("the shell didn't open in the folder")
		case MountLine(s, svm) != s.Paths.Share+" " || g.branches(svm, sp) != "main other ":
			return errors.New("the VM mounts more than the shared tools, or its clone hasn't every branch")
		case sid == "" || st("id") != sid || st("clone.on") != "true" || st("git.mode") != "clone":
			return errors.New("the VM's state hasn't its ID, clone.on and git.mode clone")
		}
		if r, err := g.git(sp, "remote", "get-url", "devvm"); err != nil || strings.TrimSpace(r.Stdout) != svm+":"+sp {
			return errors.New("the repo's devvm remote isn't " + svm + ":" + sp)
		}
		if _, err := s.InVM(svm, "grep -qx in-home ~/e2e-home.txt"); err != nil {
			return errors.New("the VM's home wasn't kept")
		}
		if g.listed(aside) || g.locked(sp) {
			return errors.New("the VM's old one is still there, or .git/config is locked")
		}
		return nil
	})
	_, _ = s.InVM(svm, "cd "+shell.Quote(sp)+" && echo b > b && git add b && git -c user.email=e@e -c user.name=e commit -qm from-vm")
	s.Check("clone off without a terminal refuses with unfetched commits, even with --force, and keeps the clone", func() error {
		r, err := g.devvm(sp, "", "clone", "off", "--force")
		switch {
		case err == nil:
			return errors.New("clone off succeeded")
		case !g.said(r, "no_terminal_fetch", "command=devvm clone off"):
			return errors.New("clone off didn't say there are commits to fetch")
		case st("clone.on") != "true" || !g.listed(svm):
			return errors.New("the clone is gone")
		}
		return nil
	})
	cloneOff(s, g, svm, sp, aside, sid)
	_, _ = g.devvm(sp, "", "rm", "--force", "--no-export")
}

// cloneOff: once fetched, clone off --force mounts the folder again.
func cloneOff(s *S, g cloneGit, svm, sp, aside, sid string) {
	st := func(k string) string { return s.Paths.State(svm)[k] }
	s.Check("once fetched, clone off --force mounts the folder again, .git protected, its home and ID kept, the remote gone and its work a local branch", func() error {
		if err := g.fetch(sp); err != nil {
			return err
		}
		r, err := g.devvm(sp, "pwd\n", "clone", "off", "--force")
		switch {
		case err != nil:
			return err
		case !HasLine(r.Out, sp) || !strings.Contains(MountLine(s, svm), sp+" "):
			return errors.New("the shell didn't open in the folder, or the VM doesn't mount it")
		case sid == "" || st("id") != sid || st("clone.on") == "true":
			return errors.New("the VM's state hasn't its ID, or has clone.on")
		}
		if _, err := s.InVM(svm, "grep -qx in-home ~/e2e-home.txt && test -f "+sp+"/a"); err != nil {
			return errors.New("the VM's home wasn't kept, or the folder isn't mounted")
		}
		switch {
		case !g.noRemote(sp):
			return errors.New("the devvm remote or its refs are still there")
		case !g.hasCommit(sp, "saved/devvm/main", "from-vm"):
			return errors.New("the fetched work isn't saved/devvm/main")
		case g.listed(aside):
			return errors.New("the VM's old one is still there")
		case runtime.GOOS == "darwin" && (st("git.mode") != "on" || !g.locked(sp)):
			return errors.New(".git isn't protected again")
		}
		return nil
	})
}
