//go:build e2e

package e2e

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/devvmhq/devvm/internal/shell"
)

// dirtyRepo gives the repo at d uncommitted changes of every kind the
// copy sends or names: a staged a, an unstaged b, an intent-to-add n,
// foo, tracked, turned into a folder with a .env in it, and an untracked
// .env.
func dirtyRepo(g cloneGit, d string) error {
	write := func(name, body string) error {
		p := filepath.Join(d, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		return os.WriteFile(p, []byte(body), 0o600)
	}
	steps := []func() error{
		func() error { return write("b", "b\n") },
		func() error { return write("foo", "foo\n") },
		func() error { _, err := g.git(d, "add", "b", "foo"); return err },
		func() error {
			_, err := g.git(d, "-c", "user.email=e@e", "-c", "user.name=e", "commit", "-qm", "files")
			return err
		},
		func() error { return write("a", "staged\n") },
		func() error { _, err := g.git(d, "add", "a"); return err },
		func() error { return write("b", "unstaged\n") },
		func() error { return write("n", "new\n") },
		func() error { _, err := g.git(d, "add", "-N", "n"); return err },
		func() error { return os.Remove(filepath.Join(d, "foo")) },
		func() error { return write("foo/.env", "AWS=topsecret\n") },
		func() error { return write(".env", "SECRET=1\n") },
	}
	for _, st := range steps {
		if err := st(); err != nil {
			return err
		}
	}
	return nil
}

// checkUncommitted checks the VM's clone of d, a dirtyRepo, which devvm's
// launch (out) made: the same git status as d's, untracked files aside,
// none of them in the VM, and foo named.
func checkUncommitted(s *S, g cloneGit, d, vm string, out Res) {
	s.Check("staged, unstaged and new files show the same git status in the VM, and an untracked .env, in the repo or in a tracked file turned folder, stays out", func() error {
		if vm == "" {
			return errors.New("no VM was made")
		}
		host, err := g.git(d, "status", "--porcelain", "--untracked-files=no")
		if err != nil {
			return err
		}
		in := "cd " + shell.Quote(d) + " && "
		got, err := s.InVM(vm, in+"git status --porcelain --untracked-files=all")
		switch {
		case err != nil:
			return err
		case got.Stdout != host.Stdout:
			return errors.New("the VM's git status isn't this machine's:\n" + got.Stdout + "\nhere:\n" + host.Stdout)
		case !HasLine(got.Stdout, "M  a") || !HasLine(got.Stdout, " M b") || !HasLine(got.Stdout, " A n") || !HasLine(got.Stdout, " D foo"):
			return errors.New("the VM's git status lacks a change:\n" + got.Stdout)
		case !g.said(out, "clone_typechange_kept", "count=1", "dir="+d, `files="foo"`):
			return errors.New("devvm didn't name foo, a folder now")
		}
		env, err := s.InVM(vm, in+"if [ -e .env ] || [ -e foo ]; then echo leaked; fi")
		if err != nil {
			return err
		}
		if env.Stdout != "" {
			return errors.New("an untracked .env, or the folder foo, is in the VM")
		}
		return nil
	})
}
