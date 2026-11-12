//go:build e2e

package e2e

import (
	"errors"
	"strings"

	"github.com/devvmhq/devvm/internal/shell"
)

// cloneRefs: a repo whose HEAD is detached, with an annotated tag,
// origin's remote-tracking branches and an older devvm VM's: its VM's
// clone is detached at the same commit, with the tag (still annotated)
// and origin's branches, origin/HEAD a symref, and none of the VM
// remote's branches. devvm clone fetch brings back a commit only the
// VM's detached HEAD has, and rm names another. Its uncommitted changes
// (dirtyRepo) are copied too (checkUncommitted), and so are its settings
// and hooks (settingsRepo, checkSettings).
func cloneRefs(s *S, g cloneGit) {
	up, err := s.NewProject("cloned-up")
	if err == nil {
		err = g.repo(up)
	}
	d, derr := s.NewProject("cloned-refs")
	steps := [][]string{
		{"clone", "-q", "file://" + up, "."},
		{"tag", "-a", "v1", "-m", "v1"},
		{"remote", "add", "devvm-old", "devvm-old-0123456789abcd:/elsewhere"},
		{"update-ref", "refs/remotes/devvm-old/main", "HEAD"},
		{"checkout", "-q", "--detach"},
		{"-c", "user.email=e@e", "-c", "user.name=e", "commit", "-q", "--allow-empty", "-m", "detached-work"},
	}
	if err == nil {
		err = derr
	}
	for _, st := range steps {
		if err == nil {
			_, err = g.git(d, st...)
		}
	}
	if err == nil {
		err = dirtyRepo(g, d)
	}
	if err == nil {
		err = settingsRepo(g, d)
	}
	var vm string
	var out Res
	if err == nil {
		out, _ = g.devvm(d, "pwd\n", "--clone", "--network", "open")
		vm = g.made(d)
	}
	s.Check("a detached HEAD opens detached in the VM at the same commit, with the tags and origin's branches, and no devvm VM's remote-tracking branches", func() error {
		if err != nil {
			return err
		}
		if vm == "" {
			return errors.New("no VM was made")
		}
		host, err := g.git(d, "rev-parse", "HEAD")
		if err != nil {
			return err
		}
		in := "cd " + shell.Quote(d) + " && "
		head, err := s.InVM(vm, in+"if git symbolic-ref -q HEAD; then :; else git rev-parse HEAD; fi")
		switch {
		case err != nil:
			return err
		case head.Stdout != host.Stdout:
			return errors.New("the VM's HEAD isn't detached at the host's commit: " + head.Stdout)
		case !g.said(out, "clone_ready_detached", "remote=devvm"):
			return errors.New("devvm didn't say the clone is detached")
		}
		refs, err := s.InVM(vm, in+"git for-each-ref --format='%(refname) %(objecttype)' && git symbolic-ref refs/remotes/origin/HEAD")
		switch {
		case err != nil:
			return err
		case !HasLine(refs.Stdout, "refs/tags/v1 tag"):
			return errors.New("the VM hasn't the annotated tag v1")
		case !HasLine(refs.Stdout, "refs/remotes/origin/main commit") || !HasLine(refs.Stdout, "refs/remotes/origin/main"):
			return errors.New("the VM hasn't origin/main, or origin/HEAD pointing at it")
		case strings.Contains(refs.Stdout, "refs/remotes/devvm"):
			return errors.New("the VM has a devvm VM's remote-tracking branches")
		}
		return nil
	})
	checkUncommitted(s, g, d, vm, out)
	checkSettings(s, g, d, vm)
	commit := func(m string) {
		_, _ = s.InVM(vm, "cd "+shell.Quote(d)+" && git -c user.email=e@e -c user.name=e commit -q --allow-empty -m "+m)
	}
	commit("vm-detached")
	s.Check("devvm clone fetch brings back a commit only the VM's detached HEAD has, as saved/devvm/HEAD", func() error {
		if vm == "" {
			return errors.New("no VM was made")
		}
		r, err := g.devvm(d, "", "clone", "fetch")
		switch {
		case err != nil:
			return err
		case !g.said(r, "saved_branches", `branches="saved/devvm/HEAD"`):
			return errors.New("devvm clone fetch didn't name saved/devvm/HEAD")
		case !g.hasCommit(d, "saved/devvm/HEAD", "vm-detached"):
			return errors.New("saved/devvm/HEAD hasn't the VM's commit")
		}
		return nil
	})
	commit("vm-detached-2")
	s.Check("rm without a terminal names a commit only the VM's detached HEAD has, not the uncommitted changes copied in, and keeps the VM", func() error {
		if vm == "" {
			return errors.New("no VM was made")
		}
		r, err := g.devvm(d, "", "rm", "--force")
		switch {
		case err == nil:
			return errors.New("rm succeeded")
		case !g.said(r, "unfetched_commits", `refs="HEAD"`):
			return errors.New("rm didn't name the detached HEAD's commit")
		case g.said(r, "clone_dirty"):
			return errors.New("rm counted the uncommitted changes copied in as the VM's")
		case !g.listed(vm):
			return errors.New(vm + " is gone")
		}
		return nil
	})
	_, _ = g.devvm(d, "", "rm", "--force", "--no-export", "--no-clone-check")
}
