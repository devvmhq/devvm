//go:build e2e

package e2e

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/devvmhq/devvm/internal/shell"
)

// cloneInProgress: a repo whose rebase stopped on a conflict, started
// with --autostash, with a stash of a plain entry and a git stash --all
// one, after a reset whose ORIG_HEAD alone reached a commit. In its VM's
// clone, git rebase --continue finishes the rebase and brings the
// autostashed change back, the stash is the repo's but for the --all
// entry, which devvm names, and the reset's commit isn't there. rm names
// a stash entry the VM made.
func cloneInProgress(s *S, g cloneGit) {
	d, err := s.NewProject("cloned-rebase")
	var undone string
	if err == nil {
		undone, err = inProgressRepo(g, d)
	}
	var vm string
	var out Res
	if err == nil {
		out, _ = g.devvm(d, "pwd\n", "--clone", "--network", "open")
		vm = g.made(d)
	}
	in := "cd " + shell.Quote(d) + " && "
	s.Check("a rebase stopped on a conflict goes on in the VM, its autostash comes back, and the stash is the repo's but for a --all entry, which devvm names", func() error {
		switch {
		case err != nil:
			return err
		case vm == "":
			return errors.New("no VM was made")
		case !g.said(out, "clone_stash_all_kept", "count=1", "dir="+d, `entries="stash@{0}"`):
			return errors.New("devvm didn't name the --all stash entry")
		}
		list, err := s.InVM(vm, in+"git stash list --format=%gs")
		if err != nil {
			return err
		}
		if strings.TrimSpace(list.Stdout) != "On topic: one" {
			return errors.New("the VM's stash isn't the repo's less the --all entry:\n" + list.Stdout)
		}
		if r, err := s.InVM(vm, in+"if git cat-file -e "+undone+" 2>/dev/null; then echo kept; fi"); err != nil || r.Stdout != "" {
			return errors.New("the commit only the reset's ORIG_HEAD reached is in the VM")
		}
		// The section's home has no git identity, so the VM's git has none:
		// the commit --continue makes needs one, as the VM's other commits do.
		r, err := s.InVM(vm, in+"echo resolved > f && git add f && git -c user.email=e@e -c user.name=e -c core.editor=true rebase --continue >/dev/null && git log --format=%s -1 && cat g")
		if err != nil {
			// The error carries git's stderr (gitSaid), and r.Out all of it.
			return errors.New("git rebase --continue failed in the VM: " + err.Error() + "\n" + r.Out)
		}
		if !HasLine(r.Stdout, "topic") || !HasLine(r.Stdout, "wip") {
			return errors.New("the rebase or its autostash didn't finish in the VM:\n" + r.Stdout)
		}
		return nil
	})
	_, _ = s.InVM(vm, in+"echo vm > g && git stash push -q -m mine")
	s.Check("rm without a terminal names a stash entry the VM made, not the one copied in, and keeps the VM", func() error {
		if vm == "" {
			return errors.New("no VM was made")
		}
		r, err := g.devvm(d, "", "rm", "--force")
		switch {
		case err == nil:
			return errors.New("rm succeeded")
		case !strings.Contains(r.Out, `"stash-0"`) || strings.Contains(r.Out, `"stash-1"`):
			return errors.New("rm didn't name the VM's own stash entry alone:\n" + r.Out)
		case !g.listed(vm):
			return errors.New(vm + " is gone")
		}
		return nil
	})
	_, _ = g.devvm(d, "", "rm", "--force", "--no-export", "--no-clone-check")
}

// inProgressRepo makes d a repo whose rebase of topic onto main, started
// with --autostash (g changed), stopped on f's conflict, after a reset
// whose ORIG_HEAD reached a commit nothing else does (undone, which it
// returns), and with a stash of two entries, the newest git stash
// --all's, holding an ignored file.
func inProgressRepo(g cloneGit, d string) (undone string, err error) {
	write := func(name, body string) func() error {
		return func() error { return os.WriteFile(filepath.Join(d, name), []byte(body), 0o600) }
	}
	git := func(args ...string) func() error {
		return func() error { _, err := g.git(d, args...); return err }
	}
	steps := []func() error{
		git("init", "-q", "-b", "main"),
		write("f", "base\n"), write("g", "g\n"), write(".gitignore", "*.log\n"),
		git("add", "-A"), git("commit", "-q", "-m", "base"),
		git("checkout", "-q", "-b", "topic"), write("f", "topic\n"), git("commit", "-q", "-am", "topic"),
		git("checkout", "-q", "main"), write("f", "main\n"), git("commit", "-q", "-am", "main"),
		git("commit", "-q", "--allow-empty", "-m", "undone"),
		func() error {
			r, err := g.git(d, "rev-parse", "HEAD")
			undone = strings.TrimSpace(r.Stdout)
			return err
		},
		git("reset", "-q", "--hard", "HEAD~"),
		git("checkout", "-q", "topic"),
		write("g", "one\n"), git("stash", "push", "-q", "-m", "one"),
		write("x.log", "secret\n"), write("g", "all\n"), git("stash", "push", "-q", "--all", "-m", "all"),
		write("g", "wip\n"),
	}
	for _, st := range steps {
		if err := st(); err != nil {
			return "", err
		}
	}
	// The rebase stops on the conflict, so it fails.
	_, _ = g.git(d, "rebase", "--autostash", "main")
	if _, err := g.git(d, "rev-parse", "-q", "--verify", "REBASE_HEAD"); err != nil {
		return "", errors.New("the rebase didn't stop")
	}
	return undone, nil
}
