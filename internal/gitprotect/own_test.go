package gitprotect

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/devvmhq/devvm/internal/msg"
)

// OwnRepo takes a .git folder, or a linked worktree whose link git made
// both ways, and names whatever else points into another repo.
func TestOwnRepo(t *testing.T) {
	needGit(t)
	f := newFixture(t, "repo")
	root, repo := f.root, f.dir
	f.git(repo, "init", "-q")
	f.git(repo, "commit", "-q", "--allow-empty", "-m", "one")
	wt := filepath.Join(root, "wt")
	f.git(repo, "worktree", "add", "-q", "-b", "wt", wt)
	g, entry := filepath.Join(repo, ".git"), filepath.Join(repo, ".git", "worktrees", "wt")

	check := func(name, dir, want string) {
		t.Helper()
		gd, cd, err := OwnRepo(dir)
		got := msg.NamesOf(err)
		if err == nil {
			got = gd + " " + cd
		}
		if got != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
	// set writes data to path for one check, then puts back what was there.
	set := func(path, data string, fn func()) {
		t.Helper()
		had := os.Rename(path, path+".away") == nil
		write(t, path, data)
		fn()
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if had {
			if err := os.Rename(path+".away", path); err != nil {
				t.Fatal(err)
			}
		}
	}
	// replaced swaps path for a symlink to target for one check.
	replaced := func(path, target string, fn func()) {
		t.Helper()
		if err := os.Rename(path, path+".away"); err != nil {
			t.Fatal(err)
		}
		symlink(t, target, path)
		fn()
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(path+".away", path); err != nil {
			t.Fatal(err)
		}
	}
	other := filepath.Join(root, "other")
	f.git(root, "init", "-q", other)
	f.git(other, "commit", "-q", "--allow-empty", "-m", "one")
	og := filepath.Join(other, ".git")

	check("a repo", repo, g+" "+g)
	check("its worktree", wt, entry+" "+g)
	check("no .git", root, "clone_not_top dir="+root)

	set(filepath.Join(g, "commondir"), og+"\n", func() {
		check("a commondir", repo, "clone_git_elsewhere path="+g+"/commondir")
		check("its worktree, the repo's commondir elsewhere", wt, "clone_git_elsewhere path="+wt+"/.git")
	})
	set(filepath.Join(entry, "commondir"), og+"\n", func() {
		check("a worktree's commondir elsewhere", wt, "clone_git_elsewhere path="+wt+"/.git")
	})
	set(filepath.Join(entry, "gitdir"), "../../../../wt/.git\n", func() {
		check("a relative back-link", wt, entry+" "+g)
	})
	set(filepath.Join(wt, ".git"), "gitdir: "+filepath.Join(repo, ".git", "elsewhere", "wt")+"\n", func() {
		check("not in worktrees", wt, "clone_git_elsewhere path="+wt+"/.git")
	})
	set(filepath.Join(wt, ".git"), "nothing\n", func() {
		check("no gitdir line", wt, "clone_git_elsewhere path="+wt+"/.git")
	})
	set(filepath.Join(g, "objects", "info", "alternates"), og+"/objects\n", func() {
		check("alternates", repo, "clone_alternates path="+g+"/objects/info/alternates")
		check("alternates, from a worktree", wt, "clone_alternates path="+g+"/objects/info/alternates")
	})
	replaced(g, og, func() {
		check("a symlinked .git", repo, "clone_git_elsewhere path="+g)
	})
	replaced(filepath.Join(g, "objects"), filepath.Join(og, "objects"), func() {
		check("symlinked objects", repo, "clone_git_elsewhere path="+g+"/objects")
	})
	replaced(filepath.Join(g, "objects", "pack"), filepath.Join(og, "objects", "pack"), func() {
		check("a symlinked pack folder", repo, "clone_git_elsewhere path="+g+"/objects/pack")
	})
	loose := absentFanout(t, filepath.Join(g, "objects"))
	symlink(t, filepath.Join(og, "objects"), loose)
	check("a symlinked loose objects' folder", repo, "clone_git_elsewhere path="+loose)
	if err := os.Remove(loose); err != nil {
		t.Fatal(err)
	}
	replaced(filepath.Join(g, "refs", "heads"), filepath.Join(og, "refs", "heads"), func() {
		check("symlinked heads", repo, "clone_git_elsewhere path="+g+"/refs/heads")
	})
	symlink(t, filepath.Join(og, "packed-refs"), filepath.Join(g, "packed-refs"))
	check("symlinked packed refs", repo, "clone_git_elsewhere path="+g+"/packed-refs")
}

// OwnRepo takes a submodule, nested ones too, whose .git file names its
// git dir in its parent's, modules/<name>, through no link below it, with
// no commondir and a core.worktree back to it; and refuses each way one
// could point elsewhere.
func TestOwnRepoSubmodule(t *testing.T) {
	needGit(t)
	f := newFixture(t, "app")
	root, app := f.root, f.dir
	for _, name := range []string{"libsrc", "deepsrc"} {
		src := filepath.Join(root, name)
		f.git(root, "init", "-q", src)
		f.git(src, "commit", "-q", "--allow-empty", "-m", "one")
	}
	f.git(filepath.Join(root, "libsrc"), "submodule", "add", "-q", filepath.Join(root, "deepsrc"), "deep")
	f.git(filepath.Join(root, "libsrc"), "commit", "-q", "-m", "deep")
	f.git(app, "init", "-q")
	f.git(app, "submodule", "add", "-q", filepath.Join(root, "libsrc"), "lib")
	f.git(app, "commit", "-q", "-m", "lib")
	f.git(app, "submodule", "update", "-q", "--init", "--recursive")
	lib, deep := filepath.Join(app, "lib"), filepath.Join(app, "lib", "deep")
	mod := filepath.Join(app, ".git", "modules", "lib")
	deepMod := filepath.Join(mod, "modules", "deep")

	check := func(name, dir, want string) {
		t.Helper()
		gd, cd, err := OwnRepo(dir)
		got := msg.NamesOf(err)
		if err == nil {
			got = gd + " " + cd
		}
		if got != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
	// set writes data to path for one check, then puts back what was there.
	set := func(path, data string, fn func()) {
		t.Helper()
		old, err := os.ReadFile(path) //nolint:gosec // the test's own file
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		write(t, path, data)
		fn()
		if old == nil {
			_ = os.Remove(path)
			return
		}
		write(t, path, string(old))
	}
	elsewhere := "clone_git_elsewhere path=" + lib + "/.git"
	check("a submodule", lib, mod+" "+mod)
	check("a nested submodule", deep, deepMod+" "+deepMod)

	other := filepath.Join(root, "other")
	f.git(root, "init", "-q", other)
	set(filepath.Join(lib, ".git"), "gitdir: "+filepath.Join(other, ".git")+"\n", func() {
		check("a git dir outside the parent's", lib, elsewhere)
	})
	set(filepath.Join(lib, ".git"), "gitdir: ../.git\n", func() {
		check("the parent's own git dir", lib, elsewhere)
	})
	stray := filepath.Join(app, ".git", "stray")
	if err := os.Rename(mod, stray); err != nil {
		t.Fatal(err)
	}
	set(filepath.Join(lib, ".git"), "gitdir: ../.git/stray\n", func() {
		check("not below modules", lib, elsewhere)
	})
	// A link below the parent's git dir to a git dir outside it.
	outside := filepath.Join(root, "outside")
	if err := os.Rename(stray, outside); err != nil {
		t.Fatal(err)
	}
	symlink(t, outside, mod)
	check("modules/lib a link out", lib, elsewhere)
	if err := os.Remove(mod); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(outside, mod); err != nil {
		t.Fatal(err)
	}
	set(filepath.Join(mod, "commondir"), filepath.Join(other, ".git")+"\n", func() {
		check("a commondir", lib, elsewhere)
	})
	cfg := filepath.Join(mod, "config")
	orig, err := os.ReadFile(cfg) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	set(cfg, "[core]\n\tbare = false\n", func() {
		check("no core.worktree", lib, elsewhere)
	})
	set(cfg, "[core]\n\tworktree = ../../../../other\n", func() {
		check("a core.worktree elsewhere", lib, elsewhere)
	})
	set(cfg, string(orig)+"[core]\n\tworktree = ../../../lib\n", func() {
		check("two core.worktree", lib, elsewhere)
	})
	set(cfg, string(orig)+"[include]\n\tpath = x\n", func() {
		check("an include", lib, elsewhere)
	})
	set(filepath.Join(mod, "objects", "info", "alternates"), filepath.Join(other, ".git", "objects")+"\n", func() {
		check("alternates", lib, "clone_alternates path="+mod+"/objects/info/alternates")
	})
	refs := filepath.Join(mod, "refs", "heads")
	if err := os.Rename(refs, refs+".away"); err != nil {
		t.Fatal(err)
	}
	symlink(t, filepath.Join(other, ".git", "refs", "heads"), refs)
	check("symlinked refs", lib, "clone_git_elsewhere path="+refs)
	if err := os.Remove(refs); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(refs+".away", refs); err != nil {
		t.Fatal(err)
	}
	// The parent itself not its own: its submodules aren't either.
	set(filepath.Join(app, ".git", "commondir"), filepath.Join(other, ".git")+"\n", func() {
		check("a parent that isn't its own", lib, elsewhere)
	})
	check("all put back", lib, mod+" "+mod)
}

// absentFanout returns a loose objects' fan-out folder in objects that
// none of the repo's own objects use, so the test can make it.
func absentFanout(t *testing.T, objects string) string {
	t.Helper()
	for i := range 256 {
		p := filepath.Join(objects, fmt.Sprintf("%02x", i))
		if _, err := os.Lstat(p); os.IsNotExist(err) {
			return p
		}
	}
	t.Fatal("every fan-out folder exists")
	return ""
}
