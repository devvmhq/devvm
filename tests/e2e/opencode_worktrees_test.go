//go:build e2e

package e2e

import "errors"

// opencodeWorktree is where the check makes a worktree as opencode 2.4
// makes its own: worktree/<the project's ID's first 6 characters>/<name>
// in its data folder, with git worktree add.
const opencodeWorktree = "~/.local/share/opencode/worktree/e2e123/brave"

// netOpencodeWorktrees: devvm rm of vm, an opencode VM in the folder oc,
// finds a worktree in opencode's folder with an uncommitted change, which
// an export leaves out, shows it and, without a terminal, refuses, naming
// --no-worktree-check, and keeps the VM; --no-worktree-check warns and
// deletes it.
func netOpencodeWorktrees(n *netRun, oc, vm string) {
	s := n.s
	if vm == "" {
		return
	}
	env := n.home("DEVVM_MESSAGES=names")
	s.Check("devvm rm shows an opencode worktree's uncommitted change, and without a terminal keeps the VM", func() error {
		plant := "git init -q ~/e2e-repo && git -C ~/e2e-repo -c user.name=e -c user.email=e@e commit -q --allow-empty -m one && " +
			"git -C ~/e2e-repo worktree add -q -b brave " + opencodeWorktree + " && echo x > " + opencodeWorktree + "/new"
		if err := inVM(s, vm, plant); err != nil {
			return failed("making a worktree in opencode's folder", err)
		}
		r, err := s.Run(devvmCmd(s, oc, "", env, "rm", "--force", "--no-export"))
		switch {
		case err == nil:
			return errors.New("devvm rm deleted the VM")
		case !Matches(r.Out, `^agent_tree_dirty agent=opencode path=~/\.local/share/opencode/worktree/e2e123/brave$`) || !HasLine(r.Out, "?? new"):
			return errors.New("devvm rm didn't show the worktree's change")
		case !Matches(r.Out, `^devvm: no_terminal_delete_trees command=devvm rm$`):
			return errors.New("devvm rm didn't refuse, naming --no-worktree-check")
		}
		if l, _ := s.Limactl("list", "-q"); !l.Has(vm) {
			return errors.New("the VM is gone")
		}
		return nil
	})
	s.Check("... and devvm rm --no-worktree-check deletes it after a warning", func() error {
		r, err := s.Run(devvmCmd(s, oc, "", env, "rm", "--force", "--no-export", "--no-worktree-check"))
		switch {
		case err != nil:
			return failed("devvm rm --no-worktree-check", err)
		case !Matches(r.Out, `^devvm: trees_check_skipped command=devvm rm vm=`+vm+`$`):
			return errors.New("devvm rm --no-worktree-check didn't warn")
		}
		if l, _ := s.Limactl("list", "-q"); l.Has(vm) {
			return errors.New("the VM is still there")
		}
		return nil
	})
}
