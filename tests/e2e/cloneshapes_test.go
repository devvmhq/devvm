//go:build e2e

package e2e

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/devvmhq/devvm/internal/shell"
)

// cloneShapes: --clone of repos git push couldn't copy, which the seed
// copies as a snapshot: a shallow clone (git clone --depth 1, the
// reported shallow update not allowed), a SHA-256 repo, a reftable repo
// where this machine's git makes one, and a shallow repo in a folder of
// repos. Each VM's clone is marked done, and a VM commit fetches back
// with the host repo's .git/shallow as it was. The run notes the VM's
// git version once. A partial clone is copied too (clonePartial).
func cloneShapes(s *S, g cloneGit) {
	src, srcErr := s.NewProject("cloned-src")
	if srcErr == nil {
		srcErr = g.repo(src)
	}
	if srcErr == nil {
		_, srcErr = g.git(src, "-c", "user.email=e@e", "-c", "user.name=e", "commit", "-q", "--allow-empty", "-m", "two")
	}
	if srcErr == nil {
		_, srcErr = g.git(src, "config", "uploadpack.allowFilter", "true")
	}
	noted := false
	shape := func(name, title string, prepare func(dir string) error, check func(vm, dir string) error) {
		dir, err := s.NewProject(name)
		if err == nil {
			err = srcErr
		}
		if err == nil {
			err = prepare(dir)
		}
		var vm string
		if err == nil {
			_, _ = g.devvm(dir, "pwd\n", "--clone", "--network", "open")
			vm = g.made(dir)
		}
		if vm != "" && !noted {
			if r, err := s.InVM(vm, "git --version"); err == nil {
				s.Note("the VM's " + strings.TrimSpace(r.Stdout))
				noted = true
			}
		}
		s.Check(title, func() error {
			if err != nil {
				return err
			}
			if vm == "" {
				return errors.New("no VM was made")
			}
			if err := check(vm, dir); err != nil {
				return err
			}
			return seededAndBack(s, g, vm, dir, dir)
		})
		_, _ = g.devvm(dir, "", "rm", "--force", "--no-export", "--no-clone-check")
	}
	clone := func(args ...string) func(string) error {
		return func(dir string) error {
			_, err := g.git(dir, append([]string{"clone", "-q"}, append(args, "file://"+src, ".")...)...)
			return err
		}
	}
	shape("cloned-shallow", "a git clone --depth 1 repo gets a shallow clone in the VM with its one commit, marked done; a VM commit fetches back and .git/shallow stays as it was",
		clone("--depth", "1"), func(vm, dir string) error {
			r, err := s.InVM(vm, "cd "+shell.Quote(dir)+" && git rev-parse --is-shallow-repository && git log --format=%s")
			if err != nil || r.Stdout != "true\ntwo\n" {
				return errors.New("the VM's clone isn't shallow with the one commit: " + r.Stdout)
			}
			return nil
		})
	shape("cloned-sha256", "a SHA-256 repo gets a SHA-256 clone in the VM, marked done; a VM commit fetches back", func(dir string) error {
		if _, err := g.git(dir, "init", "-q", "-b", "main", "--object-format=sha256"); err != nil {
			return err
		}
		if err := g.repo(dir); err != nil {
			return err
		}
		return formatIs(g, dir, "sha256")
	}, func(vm, dir string) error {
		r, err := s.InVM(vm, "git -C "+shell.Quote(dir)+" rev-parse --show-object-format")
		if err != nil || strings.TrimSpace(r.Stdout) != "sha256" {
			return errors.New("the VM's clone isn't SHA-256: " + r.Stdout)
		}
		return nil
	})
	reftable := func(dir string) error {
		_, err := g.git(dir, "init", "-q", "--ref-format=reftable", "-b", "main")
		return err
	}
	if probe, err := os.MkdirTemp(s.sb.work, "reftable-"); err == nil {
		ok := reftable(probe) == nil
		_ = os.RemoveAll(probe)
		if ok {
			shape("cloned-reftable", "a reftable repo gets a clone in the VM with its branches, marked done; a VM commit fetches back", func(dir string) error {
				if err := reftable(dir); err != nil {
					return err
				}
				return g.repo(dir)
			}, func(vm, dir string) error {
				if g.branches(vm, dir) != "main other " {
					return errors.New("the VM's clone hasn't main and other")
				}
				return nil
			})
		} else {
			s.Skip("a reftable repo gets a clone in the VM", "this machine's git makes no reftable repo")
		}
	}
	cloneShallowWorkspace(s, g, src)
	partial, err := s.NewProject("cloned-partial-local")
	if err == nil {
		_, err = g.git(partial, "clone", "-q", "--filter=blob:none", "file://"+src, ".")
	}
	s.Check("a partial clone of a repo on this machine is copied, and devvm names its promisor, which the VM can't reach", func() error {
		if err != nil {
			return err
		}
		r, _ := g.devvm(partial, "pwd\n", "--clone", "--network", "open")
		vm := g.made(partial)
		defer func() { _, _ = g.devvm(partial, "", "rm", "--force", "--no-export", "--no-clone-check") }()
		switch {
		case vm == "":
			return errors.New("no VM was made:\n" + r.Out)
		case !g.said(r, "clone_partial_unreachable", "dir="+partial, "remote=origin", "url=file://"+src):
			return errors.New("devvm didn't name the promisor on this machine:\n" + r.Out)
		}
		in, err := s.InVM(vm, "cd "+shell.Quote(partial)+" && git config remote.origin.promisor && git log --format=%s -1")
		if err != nil || !HasLine(in.Stdout, "true") || !HasLine(in.Stdout, "two") {
			return errors.New("the VM's clone isn't the partial clone's:\n" + in.Out)
		}
		return nil
	})
}

// cloneShallowWorkspace: a shallow repo in a folder of repos gets a
// shallow clone at its path, marked done, and its VM commit comes back
// with devvm clone fetch.
func cloneShallowWorkspace(s *S, g cloneGit, src string) {
	ws, err := s.NewProject("cloned-shallow-ws")
	app := filepath.Join(ws, "app")
	if err == nil {
		_, err = g.git(ws, "clone", "-q", "--depth", "1", "file://"+src, app)
	}
	var vm string
	if err == nil {
		_, _ = g.devvm(ws, "pwd\n", "--clone", "--network", "open", "--repos", "all")
		vm = g.made(ws)
	}
	s.Check("a shallow repo in a folder of repos gets a shallow clone at its path, marked done; a VM commit fetches back and .git/shallow stays as it was", func() error {
		if err != nil {
			return err
		}
		if vm == "" {
			return errors.New("no VM was made")
		}
		if r, err := s.InVM(vm, "git -C "+shell.Quote(app)+" rev-parse --is-shallow-repository"); err != nil || strings.TrimSpace(r.Stdout) != "true" {
			return errors.New("the VM's clone of app isn't shallow")
		}
		return seededAndBack(s, g, vm, ws, app)
	})
	_, _ = g.devvm(ws, "", "rm", "--force", "--no-export", "--no-clone-check")
}

// seededAndBack checks the VM's clone of the repo at dir, of the VM of
// the folder folder: marked done; a VM commit on main, fetched with devvm
// clone fetch, is the host's devvm/main, and the host repo's .git/shallow
// is as it was (or still not there).
func seededAndBack(s *S, g cloneGit, vm, folder, dir string) error {
	if r, err := s.InVM(vm, "cat "+shell.Quote(filepath.Join(dir, ".git", "devvm-seeded"))); err != nil || strings.TrimSpace(r.Stdout) != "done" {
		return errors.New("the VM's clone isn't marked done: " + r.Stdout)
	}
	shallow := filepath.Join(dir, ".git", "shallow")
	before, berr := os.ReadFile(shallow)
	if _, err := s.InVM(vm, "cd "+shell.Quote(dir)+" && git checkout -q main && git -c user.email=e@e -c user.name=e commit -q --allow-empty -m from-vm"); err != nil {
		return errors.New("committing in the VM failed")
	}
	if _, err := g.devvm(folder, "", "clone", "fetch"); err != nil {
		return err
	}
	if !g.hasCommit(dir, "devvm/main", "from-vm") {
		return errors.New("devvm/main hasn't the VM's commit")
	}
	after, aerr := os.ReadFile(shallow)
	if (berr == nil) != (aerr == nil) || !bytes.Equal(before, after) {
		return errors.New("the host repo's .git/shallow changed")
	}
	return nil
}

// formatIs fails unless host git says the repo at dir names its objects
// with format.
func formatIs(g cloneGit, dir, format string) error {
	r, err := g.git(dir, "rev-parse", "--show-object-format")
	if err != nil || strings.TrimSpace(r.Stdout) != format {
		return errors.New("the repo isn't " + format)
	}
	return nil
}
