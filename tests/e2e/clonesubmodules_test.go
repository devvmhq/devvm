//go:build e2e

package e2e

import (
	"errors"
	"path/filepath"
	"strings"

	"github.com/devvmhq/devvm/internal/shell"
)

// cloneSubmodules: a repo with a submodule that has one of its own, both
// initialised: in the VM, git submodule status --recursive is as here; a
// VM commit in the submodule comes back into the submodule's repo here
// with devvm clone fetch; and rm without a terminal names a commit in the
// nested submodule that wasn't fetched, and keeps the VM.
func cloneSubmodules(s *S, g cloneGit) {
	deep, err := s.NewProject("cloned-sub-deep")
	if err == nil {
		err = g.repo(deep)
	}
	lib, lerr := s.NewProject("cloned-sub-lib")
	if err == nil {
		err = lerr
	}
	if err == nil {
		err = g.repo(lib)
	}
	app, aerr := s.NewProject("cloned-sub-app")
	if err == nil {
		err = aerr
	}
	if err == nil {
		err = g.repo(app)
	}
	file := []string{"-c", "protocol.file.allow=always"}
	steps := []struct {
		dir  string
		args []string
	}{
		{lib, append(file, "submodule", "add", "-q", "file://"+deep, "deep")},
		{lib, []string{"commit", "-q", "-m", "deep"}},
		{app, append(file, "submodule", "add", "-q", "file://"+lib, "lib")},
		{app, []string{"commit", "-q", "-m", "lib"}},
		{app, append(file, "submodule", "update", "-q", "--init", "--recursive")},
	}
	for _, st := range steps {
		if err == nil {
			_, err = g.git(st.dir, st.args...)
		}
	}
	var vm, host string
	if err == nil {
		var r Res
		if r, err = g.git(app, "submodule", "status", "--recursive"); err == nil {
			host = r.Stdout
		}
	}
	if err == nil {
		_, _ = g.devvm(app, "pwd\n", "--clone", "--network", "open")
		vm = g.made(app)
	}
	in := "cd " + shell.Quote(app) + " && "
	s.Check("a repo's submodules, nested ones too, get clones of their own in the VM, as here", func() error {
		switch {
		case err != nil:
			return err
		case vm == "":
			return errors.New("no VM was made")
		}
		r, err := s.InVM(vm, in+"git submodule status --recursive")
		if err != nil || strings.TrimSpace(r.Stdout) != strings.TrimSpace(host) {
			return errors.New("the VM's submodule status isn't this machine's:\n" + r.Out + "\nhere:\n" + host)
		}
		return nil
	})
	s.Check("a VM commit in a submodule comes back with devvm clone fetch, and rm names one in a nested submodule not fetched", func() error {
		if vm == "" {
			return errors.New("no VM was made")
		}
		commit := func(path, branch string) error {
			_, err := s.InVM(vm, "cd "+shell.Quote(filepath.Join(app, path))+" && git checkout -q -b "+branch+" && git -c user.email=e@e -c user.name=e commit -q --allow-empty -m "+branch)
			return err
		}
		if err := commit("lib", "vm-lib"); err != nil {
			return err
		}
		if _, err := g.devvm(app, "", "clone", "fetch"); err != nil {
			return err
		}
		if r, err := g.git(filepath.Join(app, "lib"), "log", "--format=%s", "-1", "devvm/vm-lib"); err != nil || strings.TrimSpace(r.Stdout) != "vm-lib" {
			return errors.New("lib's VM commit didn't come back")
		}
		if err := commit(filepath.Join("lib", "deep"), "vm-deep"); err != nil {
			return err
		}
		r, err := g.devvm(app, "", "rm", "--force")
		switch {
		case err == nil:
			return errors.New("rm succeeded")
		case !g.said(r, "unfetched_commits_in", `refs="vm-deep"`, "repo=lib/deep"):
			return errors.New("rm didn't name the nested submodule's commit:\n" + r.Out)
		case !g.listed(vm):
			return errors.New(vm + " is gone")
		}
		return nil
	})
	_, _ = g.devvm(app, "", "rm", "--force", "--no-export", "--no-clone-check")
}
