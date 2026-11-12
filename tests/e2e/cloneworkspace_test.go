//go:build e2e

package e2e

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/devvmhq/devvm/internal/shell"
)

// cloneWorkspace: --clone in a folder of repos, without a repo of its
// own. One VM gets a clone of each repo at its own path, with every
// branch of the repo, and the folder's own file; an agent's
// commit in two repos comes back with devvm clone fetch, listed by branch;
// devvm clone add copies a third repo; rm refuses with unfetched work in
// any repo.
func cloneWorkspace(s *S, g cloneGit) {
	ws, err := s.NewProject("cloned-ws")
	repos := []string{"frontend", "backend"}
	for _, r := range repos {
		if err == nil {
			err = os.MkdirAll(filepath.Join(ws, r), 0o755) //nolint:gosec // a repo's folder, as a user's mkdir makes it
		}
		if err == nil {
			err = g.repo(filepath.Join(ws, r))
		}
	}
	if err == nil {
		err = os.WriteFile(filepath.Join(ws, "compose.yml"), []byte("services: {}\n"), 0o600)
	}
	var vm string
	var out Res
	if err == nil {
		out, _ = g.devvm(ws, "pwd\n", "--clone", "--network", "open", "--repos", "all")
		vm = g.made(ws)
	}
	in := func(r string) string { return filepath.Join(ws, r) }
	s.Check("a folder of repos gets one VM with a clone of each, at its path, with every branch, the folder's own file, and nothing mounted but the shared tools", func() error {
		switch {
		case vm == "" || MountLine(s, vm) != s.Paths.Share+" ":
			return errors.New("the VM mounts more than the shared tools")
		case g.branches(vm, in("frontend")) != "main other " || g.branches(vm, in("backend")) != "main other ":
			return errors.New("a repo's clone hasn't main and other")
		case !g.inVMHas(vm, in("backend"), "one"):
			return errors.New("the backend clone hasn't the repo's commit")
		case !HasLine(out.Out, ws):
			return errors.New("the shell didn't open in the folder")
		case s.Paths.State(vm)["clone.workspace"] != "true":
			return errors.New("the VM's state hasn't clone.workspace")
		}
		if r, err := s.InVM(vm, "cat "+shell.Quote(in("compose.yml"))); err != nil || !strings.Contains(r.Stdout, "services") {
			return errors.New("the VM hasn't the folder's compose.yml")
		}
		for _, r := range repos {
			url, err := g.git(in(r), "remote", "get-url", "devvm")
			if err != nil || strings.TrimSpace(url.Stdout) != vm+":"+in(r) {
				return errors.New(r + "'s devvm remote isn't " + vm + ":" + in(r))
			}
		}
		return nil
	})
	agent := "git checkout -qb agent/x && git -c user.email=e@e -c user.name=e commit -q --allow-empty -m from-agent"
	_, _ = s.InVM(vm, "cd "+shell.Quote(in("frontend"))+" && "+agent+" && cd "+shell.Quote(in("backend"))+" && "+agent)
	s.Check("devvm clone fetch brings both repos' work back and lists the branch with both repos", func() error {
		r, err := g.devvm(ws, "", "clone", "fetch")
		switch {
		case err != nil:
			return err
		case !HasLine(r.Stdout, "agent/x: backend, frontend"):
			return errors.New("devvm clone fetch didn't list agent/x in both repos:\n" + r.Stdout)
		case !g.hasCommit(in("frontend"), "devvm/agent/x", "from-agent") || !g.hasCommit(in("backend"), "devvm/agent/x", "from-agent"):
			return errors.New("a repo hasn't devvm/agent/x with the VM's commit")
		}
		return nil
	})
	err = os.MkdirAll(in("shared"), 0o755) //nolint:gosec // a repo's folder, as a user's mkdir makes it
	if err == nil {
		err = g.repo(in("shared"))
	}
	s.Check("devvm clone add copies a third repo into the VM at its path, with a remote", func() error {
		if err != nil {
			return err
		}
		r, err := g.devvm(ws, "", "clone", "add", "shared")
		switch {
		case err != nil:
			return err
		case !g.said(r, "clone_added", "path=shared", "remote=devvm"):
			return errors.New("devvm clone add didn't say it added shared")
		case !g.inVMHas(vm, in("shared"), "one"):
			return errors.New("the VM hasn't a clone of shared")
		}
		return nil
	})
	_, _ = s.InVM(vm, "cd "+shell.Quote(in("backend"))+" && git -c user.email=e@e -c user.name=e commit -q --allow-empty -m unfetched")
	s.Check("rm without a terminal refuses with unfetched work in one repo, naming it, and keeps the VM", func() error {
		r, err := g.devvm(ws, "", "rm", "--force")
		switch {
		case err == nil:
			return errors.New("rm succeeded")
		case !g.said(r, "unfetched_commits_in", `refs="agent/x"`, "repo=backend"):
			return errors.New("rm didn't name backend's unfetched branch")
		case !g.said(r, "no_terminal_fetch_repos", "command=devvm rm"):
			return errors.New("rm didn't say it can't ask to fetch")
		case !g.listed(vm):
			return errors.New(vm + " is gone")
		}
		return nil
	})
	s.Check("rm --no-clone-check deletes it and removes each repo's remote", func() error {
		if _, err := g.devvm(ws, "", "rm", "--force", "--no-export", "--no-clone-check"); err != nil {
			return err
		}
		if g.listed(vm) {
			return errors.New(vm + " is still there")
		}
		for _, r := range append(repos, "shared") {
			if out, err := g.git(in(r), "remote"); err != nil || strings.Contains(out.Stdout, "devvm") {
				return errors.New(r + " still has its devvm remote")
			}
		}
		return nil
	})
}
