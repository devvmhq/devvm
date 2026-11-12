//go:build e2e

package e2e

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/devvmhq/devvm/internal/shell"
)

// lfsPointer is a Git LFS pointer for content, and its oid.
func lfsPointer(content string) (pointer, oid string) {
	sum := sha256.Sum256([]byte(content))
	oid = hex.EncodeToString(sum[:])
	return "version https://git-lfs.github.com/spec/v1\noid sha256:" + oid + "\nsize " + strconv.Itoa(len(content)) + "\n", oid
}

// cloneLFS: a repo that uses Git LFS, made without git-lfs here (a
// pointer, its object in .git/lfs/objects): in the VM, whose image has
// git-lfs, the file has its content and git status is clean, git-lfs's
// filter set in that clone's config alone, never the VM's system or
// global config, so a VM that mounts its folder keeps pointers; a file the
// VM's git-lfs adds comes back with devvm clone fetch, its object in the
// repo here; and an object in the VM that isn't what its pointer names
// stays out, named.
func cloneLFS(s *S, g cloneGit) {
	d, err := s.NewProject("cloned-lfs")
	content := "an LFS file's content\n"
	pointer, oid := lfsPointer(content)
	obj := func(dir, oid string) string {
		return filepath.Join(dir, ".git", "lfs", "objects", oid[:2], oid[2:4], oid)
	}
	if err == nil {
		err = os.WriteFile(filepath.Join(d, ".gitattributes"), []byte("*.bin filter=lfs diff=lfs merge=lfs -text\n"), 0o600)
	}
	if err == nil {
		err = os.WriteFile(filepath.Join(d, "big.bin"), []byte(pointer), 0o600)
	}
	if err == nil {
		err = g.repo(d)
	}
	if err == nil {
		// g.repo commits a alone: the attributes and the pointer too.
		_, err = g.git(d, "add", ".gitattributes", "big.bin")
	}
	if err == nil {
		_, err = g.git(d, "-c", "user.email=e@e", "-c", "user.name=e", "commit", "-qm", "lfs")
	}
	if err == nil {
		err = os.MkdirAll(filepath.Dir(obj(d, oid)), 0o755) //nolint:gosec // git-lfs's own folders, as it makes them
	}
	if err == nil {
		err = os.WriteFile(obj(d, oid), []byte(content), 0o600)
	}
	var vm string
	if err == nil {
		_, _ = g.devvm(d, "pwd\n", "--clone", "--network", "open")
		vm = g.made(d)
	}
	in := "cd " + shell.Quote(d) + " && "
	s.Check("a repo's Git LFS file has its content in the VM, whose git status is clean", func() error {
		switch {
		case err != nil:
			return err
		case vm == "":
			return errors.New("no VM was made")
		}
		r, err := s.InVM(vm, in+"cat big.bin && git status --porcelain && echo clean")
		if err != nil || !strings.HasPrefix(r.Stdout, content) || !HasLine(r.Stdout, "clean") || strings.Contains(r.Stdout, " M ") {
			return errors.New("the VM's big.bin isn't its content, or the clone isn't clean:\n" + r.Out)
		}
		return nil
	})
	s.Check("git-lfs's filter is the clone's own, never in the VM's system or global config, so a mounted VM keeps pointers", func() error {
		if vm == "" {
			return errors.New("no VM was made")
		}
		r, err := s.InVM(vm, in+"git config --local --get filter.lfs.process; "+
			"git config --system --get-regexp '^filter\\.lfs' && echo system; "+
			"git config --global --get-regexp '^filter\\.lfs' && echo global; true")
		if err != nil || !HasLine(r.Stdout, "git-lfs filter-process") || HasLine(r.Stdout, "system") || HasLine(r.Stdout, "global") {
			return errors.New("the filter isn't the clone's alone:\n" + r.Out)
		}
		return nil
	})
	s.Check("an LFS file the VM adds comes back with its object checked, and a tampered one stays out, named", func() error {
		if vm == "" {
			return errors.New("no VM was made")
		}
		added := "added in the VM\n"
		bad, badOid := lfsPointer("what it names\n")
		// new.bin goes through the VM's git-lfs, which stores its object;
		// bad.bin is a pointer already, which git-lfs keeps as it is, its
		// object then written wrong.
		badDir := ".git/lfs/objects/" + badOid[:2] + "/" + badOid[2:4]
		mk := in + "git checkout -q -b vm-lfs && printf %s " + shell.Quote(added) + " > new.bin && " +
			"printf %s " + shell.Quote(bad) + " > bad.bin && git add new.bin bad.bin && " +
			"mkdir -p " + badDir + " && echo tampered > " + badDir + "/" + badOid + " && " +
			"git -c user.email=e@e -c user.name=e commit -q -m lfs"
		if r, err := s.InVM(vm, mk); err != nil {
			return errors.New("making the commit: " + r.Out)
		}
		r, err := g.devvm(d, "", "clone", "fetch")
		if err != nil {
			return errors.New("devvm clone fetch: " + r.Out)
		}
		if !g.said(r, "clone_lfs_kept", "count=1", `objects="`+badOid[:12]+`"`, "path=.") {
			return errors.New("the tampered object wasn't named:\n" + r.Out)
		}
		_, addedOid := lfsPointer(added)
		if b, err := os.ReadFile(obj(d, addedOid)); err != nil || string(b) != added {
			return errors.New("the added file's object didn't come back")
		}
		if _, err := os.Lstat(obj(d, badOid)); err == nil {
			return errors.New("the tampered object came back")
		}
		return nil
	})
	_, _ = g.devvm(d, "", "rm", "--force", "--no-export", "--no-clone-check")
}
