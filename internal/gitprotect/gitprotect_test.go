package gitprotect

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/state"
)

// fixture is a project folder with a protector whose chflags and warnings
// are recorded, and a VM state for it.
type fixture struct {
	t     *testing.T
	root  string // the test's folder, physical
	dir   string // the project folder
	p     *Protector
	vm    *state.VM
	flags []string // "uchg PATH" per chflags call
	// immutable is what has the flag now, as the calls left it.
	immutable map[string]bool
	warns     msg.Recorder
}

func newFixture(t *testing.T, name string) *fixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, root: root, dir: filepath.Join(root, name), immutable: map[string]bool{}}
	mkdirs(t, f.dir)
	f.p = &Protector{
		Dir: f.dir, Home: filepath.Join(root, "home"), CanLock: true,
		Chflags: func(_ context.Context, flags, path string) error {
			f.flags = append(f.flags, flags+" "+path)
			f.immutable[path] = flags == "uchg"
			return nil
		},
		Locked:   func(path string) bool { return f.immutable[path] },
		Reporter: &msg.Fake{OnWarn: f.warns.Add},
	}
	f.vm, err = state.New(paths.Dirs{Data: filepath.Join(root, "data")}, "devvm-p-0123456789abcd")
	if err != nil {
		t.Fatal(err)
	}
	f.vm.Image, f.vm.Dir, f.vm.Backend = "base", f.dir, "fake"
	if err := f.vm.Create(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	return f
}

// newVM sets up protection as up does for a VM just made: its mode
// (NewSection), recorded, then NewVM.
func (f *fixture) newVM(off bool) error {
	n := f.p.NewSection(off)
	if err := record(f.t.Context(), f.vm, func(g *InVM) { g.Mode, g.Chosen = n.Mode, n.Chosen }); err != nil {
		return err
	}
	return f.p.NewVM(f.t.Context(), f.vm)
}

// section is the VM's .git protection section, as its state has it.
func (f *fixture) section() InVM {
	f.t.Helper()
	again, err := state.ReadVM(paths.Dirs{Data: filepath.Join(f.root, "data")}, f.vm.Name())
	if err != nil {
		f.t.Fatal(err)
	}
	g, err := Of(again)
	if err != nil {
		f.t.Fatal(err)
	}
	return g
}

func (f *fixture) path(rel string) string { return filepath.Join(f.dir, rel) }

func (f *fixture) git(dir string, args ...string) string {
	f.t.Helper()
	cmd := exec.CommandContext(f.t.Context(), "git", append([]string{ //nolint:gosec // the test's own
		"-c", "user.email=a@b", "-c", "user.name=a",
		"-c", "init.defaultBranch=main", "-c", "protocol.file.allow=always",
	}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f *fixture) warned(want string) bool {
	return slices.ContainsFunc(f.warns.Names(), func(w string) bool { return strings.Contains(w, want) })
}

func (f *fixture) locked(path string) bool { return slices.Contains(f.flags, "uchg "+path) }

func mkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

func write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func needGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
}

func TestPathsUnderSpaces(t *testing.T) {
	f := newFixture(t, "a b")
	mkdirs(t, f.path(".git/hooks"), f.path(".git/modules/s m/hooks"), f.path(".git/modules/s m/objects"), f.path(".git/modules/s m/refs"))
	write(t, f.path(".git/config"), "")
	write(t, f.path(".git/modules/s m/config"), "")
	write(t, f.path(".git/modules/s m/HEAD"), "")
	got := f.p.paths(t.Context(), f.p.embedded(t.Context()))
	for _, want := range []string{".git/hooks", ".git/config", ".git/modules/s m/hooks", ".git/modules/s m/config"} {
		if !slices.Contains(got, f.path(want)) {
			t.Errorf("paths = %q, missing %s", got, want)
		}
	}
}

func TestPathsWorktrees(t *testing.T) {
	needGit(t)
	f := newFixture(t, "p")
	mkdirs(t, f.path(".git/hooks"), f.path(".git/worktrees/w"))
	write(t, f.path(".git/config"), "")
	write(t, f.path(".git/worktrees/w/commondir"), "../..\n")
	got := f.p.paths(t.Context(), embeds{})
	if !slices.Contains(got, f.path(".git/worktrees/w/commondir")) || slices.ContainsFunc(got, func(p string) bool {
		return strings.HasSuffix(p, "config.worktree")
	}) {
		t.Errorf("paths = %q", got)
	}
	f.git(f.dir, "config", "--file", f.path(".git/config"), "extensions.worktreeConfig", "true")
	got = f.p.paths(t.Context(), embeds{})
	for _, want := range []string{".git/config.worktree", ".git/worktrees/w/config.worktree"} {
		if !slices.Contains(got, f.path(want)) {
			t.Errorf("with worktreeConfig: paths = %q, missing %s", got, want)
		}
	}
}

// A linked worktree's .git file is all there is to lock.
func TestPathsLinkedWorktree(t *testing.T) {
	f := newFixture(t, "p")
	write(t, f.path(".git"), "gitdir: /elsewhere/.git/worktrees/p\n")
	if got := f.p.paths(t.Context(), embeds{}); !slices.Equal(got, []string{f.path(".git")}) {
		t.Errorf("paths = %q", got)
	}
}

// Nothing is created or locked through a symlink the VM planted under .git.
func TestLockNeverThroughSymlinks(t *testing.T) {
	needGit(t)
	f := newFixture(t, "p")
	out := filepath.Join(f.root, "outside")
	mkdirs(t, f.path(".git/worktrees/x"), out)
	f.git(f.dir, "config", "--file", f.path(".git/config"), "extensions.worktreeConfig", "true")
	symlink(t, filepath.Join(out, "marker"), f.path(".git/worktrees/x/config.worktree")) // dangling
	symlink(t, out, f.path(".git/worktrees/y"))
	write(t, filepath.Join(out, "commondir"), "")
	f.p.lock(t.Context(), embeds{})
	for _, p := range []string{filepath.Join(out, "marker"), filepath.Join(out, "config.worktree")} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s was created", p)
		}
	}
	for _, want := range []string{
		"git_not_locked path=" + f.path(".git/worktrees/x/config.worktree"),
		"git_not_locked path=" + f.path(".git/worktrees/y/commondir"),
	} {
		if !f.warned(want) {
			t.Errorf("warnings %q lack %s", f.warns.Names(), want)
		}
	}
	if f.warned("path="+f.path(".git/config")+" ") || !f.locked(f.path(".git/config")) {
		t.Errorf("the real .git/config: warnings %q, locked %q", f.warns.Names(), f.flags)
	}
	for _, fl := range f.flags {
		if strings.Contains(fl, out) {
			t.Errorf("locked outside the project: %s", fl)
		}
	}
}

// A missing hooks folder (and config.worktree) is created, then locked, so
// the VM can't make one.
func TestLockCreatesMissingHooks(t *testing.T) {
	needGit(t)
	f := newFixture(t, "p")
	f.git(f.root, "init", "-q", "--template=", f.dir)
	if err := os.RemoveAll(f.path(".git/hooks")); err != nil {
		t.Fatal(err)
	}
	mkdirs(t, f.path(".git/modules/s/objects"), f.path(".git/modules/s/refs"))
	write(t, f.path(".git/modules/s/HEAD"), "")
	if err := f.newVM(false); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{".git/hooks", ".git/modules/s/hooks"} {
		if fi, err := os.Stat(f.path(p)); err != nil || !fi.IsDir() || !f.locked(f.path(p)) {
			t.Errorf("%s: %v, locks %q", p, err, f.flags)
		}
	}
	// A git dir without a config gets one, empty, locked: git runs
	// without one, so the VM could make it later.
	if fi, err := os.Stat(f.path(".git/modules/s/config")); err != nil || !fi.Mode().IsRegular() || fi.Size() != 0 || !f.locked(f.path(".git/modules/s/config")) {
		t.Errorf("the submodule's config: %v, locks %q", err, f.flags)
	}
	// What it locked is recorded: .git's identity, and its one git dir
	// besides .git.
	g := f.section()
	if want := folderID(f.path(".git")); g.Made.GitID == "" || g.Made.GitID != want || !g.Made.Protected ||
		!slices.Equal(g.Made.GitDirs, []string{".git/modules/s"}) {
		t.Errorf("recorded %+v, want git_id %q", g.Made, want)
	}
}

// A symlinked .git/config is reported, not locked; the hooks are.
func TestLockOnlyRealPaths(t *testing.T) {
	f := newFixture(t, "p")
	mkdirs(t, f.path(".git/hooks"))
	write(t, f.path("cfg"), "[core]\n")
	symlink(t, "../cfg", f.path(".git/config"))
	f.p.lock(t.Context(), embeds{})
	if !f.warned("git_not_locked path="+f.path(".git/config")) || f.locked(f.path(".git/config")) || f.locked(f.path("cfg")) {
		t.Errorf("warnings %q, locks %q", f.warns.Names(), f.flags)
	}
	if !f.locked(f.path(".git/hooks")) {
		t.Errorf("hooks not locked: %q", f.flags)
	}
}

// .git a symlink to another repo: nothing of that repo is locked. A
// commondir's target is shown without its control characters.
func TestNewVMOtherRepoAndCommondir(t *testing.T) {
	f := newFixture(t, "p")
	other := filepath.Join(f.root, "other")
	mkdirs(t, filepath.Join(other, ".git", "hooks"))
	write(t, filepath.Join(other, ".git", "config"), "")
	symlink(t, filepath.Join(other, ".git"), f.path(".git"))
	if err := f.newVM(false); err != nil {
		t.Fatal(err)
	}
	if !f.warned("git_not_locked path=" + f.path(".git/config")) {
		t.Errorf("warnings %q", f.warns.Names())
	}
	for _, fl := range f.flags {
		if strings.Contains(fl, other) {
			t.Errorf("locked the other repo: %s", fl)
		}
	}
	if err := os.Remove(f.path(".git")); err != nil {
		t.Fatal(err)
	}
	mkdirs(t, f.path(".git"))
	write(t, f.path(".git/commondir"), "../x\x1b[2J\n")
	f.warns.Reset()
	if err := f.newVM(false); err != nil {
		t.Fatal(err)
	}
	if !f.warned(`git_commondir git=`+f.path(".git")+` target=../x[2J`) || f.warned("\x1b") {
		t.Errorf("warnings %q", f.warns.Names())
	}
}

// embeddedRepo makes a superproject at f.dir with a gitlink sub whose git
// dir is sub/.git, and another, rel, whose .git file points at a git dir
// elsewhere in the working tree.
func embeddedRepo(f *fixture) {
	f.t.Helper()
	f.git(f.dir, "init", "-q")
	f.git(f.dir, "commit", "-q", "--allow-empty", "-m", "x")
	mkdirs(f.t, f.path("sub"), f.path("rel"), f.path(".gitdirs"))
	f.git(f.path("sub"), "init", "-q")
	f.git(f.path("sub"), "commit", "-q", "--allow-empty", "-m", "s")
	f.git(f.dir, "update-index", "--add", "--cacheinfo", "160000,"+f.git(f.path("sub"), "rev-parse", "HEAD")+",sub")
	f.git(f.dir, "init", "-q", "--separate-git-dir="+f.path(".gitdirs/rel"), "rel")
	f.git(f.path("rel"), "commit", "-q", "--allow-empty", "-m", "r")
	f.git(f.dir, "update-index", "--add", "--cacheinfo", "160000,"+f.git(f.path("rel"), "rev-parse", "HEAD")+",rel")
}

func TestEmbedded(t *testing.T) {
	needGit(t)
	f := newFixture(t, "p")
	embeddedRepo(f)
	emb := f.p.embedded(t.Context())
	if !slices.Equal(emb.inside, []string{f.path(".gitdirs/rel"), f.path("sub/.git")}) &&
		!slices.Equal(emb.inside, []string{f.path("sub/.git"), f.path(".gitdirs/rel")}) {
		t.Errorf("embedded = %q", emb.inside)
	}
	got := f.p.paths(t.Context(), emb)
	for _, want := range []string{"sub/.git/config", ".gitdirs/rel/hooks"} {
		if !slices.Contains(got, f.path(want)) {
			t.Errorf("paths = %q, missing %s", got, want)
		}
	}
	// A nested submodule's git dir, from the submodule's own index.
	mkdirs(t, f.path("sub/inner"))
	f.git(f.path("sub/inner"), "init", "-q")
	f.git(f.path("sub/inner"), "commit", "-q", "--allow-empty", "-m", "i")
	f.git(f.path("sub"), "update-index", "--add", "--cacheinfo", "160000,"+f.git(f.path("sub/inner"), "rev-parse", "HEAD")+",inner")
	if emb := f.p.embedded(t.Context()); !slices.Contains(emb.inside, f.path("sub/inner/.git")) {
		t.Errorf("embedded = %q, without the nested one", emb.inside)
	}
}

// A .git/modules git dir with a symlinked HEAD counts; its logs folder
// doesn't.
func TestGitDirs(t *testing.T) {
	needGit(t)
	f := newFixture(t, "p")
	embeddedRepo(f)
	m := f.path(".git/modules/evil")
	mkdirs(t, f.path(".git/modules"))
	if out, err := exec.CommandContext(t.Context(), "cp", "-R", f.path("sub/.git"), m).CombinedOutput(); err != nil { //nolint:gosec // the test's own
		t.Fatalf("cp: %v %s", err, out)
	}
	if err := os.Remove(filepath.Join(m, "HEAD")); err != nil {
		t.Fatal(err)
	}
	symlink(t, "refs/heads/main", filepath.Join(m, "HEAD"))
	got := f.p.gitDirs()
	if !slices.Contains(got, m) || slices.Contains(got, filepath.Join(m, "logs")) {
		t.Errorf("gitDirs = %q", got)
	}
}

// Includes and hook symlinks into the project folder are reported when the
// VM is made, and their hash recorded.
func TestNewVMDelegations(t *testing.T) {
	needGit(t)
	f := newFixture(t, "p")
	f.git(f.dir, "init", "-q")
	cfg := f.path(".git/config")
	f.git(f.dir, "config", "--file", cfg, "include.path", "../shared.gitconfig")
	f.git(f.dir, "config", "--file", cfg, "includeIf.gitdir:~/x/.path", "../later/x.gitconfig") // not there yet
	f.git(f.dir, "config", "--file", cfg, "--add", "include.path", filepath.Join(f.p.Home, "outside.gitconfig"))
	mkdirs(t, f.path("scripts"), f.path(".git/hooks"))
	symlink(t, "../../scripts/pre-commit", f.path(".git/hooks/pre-commit"))
	if err := f.newVM(false); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"git_include_inside config=" + cfg + " path=../shared.gitconfig",
		"git_include_inside config=" + cfg + " path=../later/x.gitconfig",
		"git_hook_link_inside hook=" + f.path(".git/hooks/pre-commit") + " target=../../scripts/pre-commit",
	} {
		if !f.warned(want) {
			t.Errorf("warnings %q lack %s", f.warns.Names(), want)
		}
	}
	if f.warned("outside.gitconfig") {
		t.Errorf("an include outside the project was reported: %q", f.warns.Names())
	}
	if f.section().Made.Delegations == delegationsSum(nil) {
		t.Error("delegations is the empty list's")
	}
}

func TestNewVMGitdirOutside(t *testing.T) {
	needGit(t)
	f := newFixture(t, "p")
	embeddedRepo(f)
	out := filepath.Join(f.root, "elsewhere")
	mkdirs(t, out)
	f.git(out, "init", "-q", "--bare", "sub.git")
	if err := os.RemoveAll(f.path("sub/.git")); err != nil {
		t.Fatal(err)
	}
	write(t, f.path("sub/.git"), "gitdir: "+filepath.Join(out, "sub.git")+"\n")
	if err := f.newVM(false); err != nil {
		t.Fatal(err)
	}
	if want := "git_gitdir_outside git=" + filepath.Join(out, "sub.git") + " sub=" + f.path("sub"); !f.warned(want) {
		t.Errorf("warnings %q lack %s", f.warns.Names(), want)
	}
	if got := f.section().Made.GitDirs; !slices.Equal(got, []string{".gitdirs/rel"}) {
		t.Errorf("git_dirs = %q", got)
	}
}

func TestWorktreeStrays(t *testing.T) {
	f := newFixture(t, "p")
	mkdirs(t, f.path(".git/worktrees/ok"), f.path(".git/worktrees/moved"), f.path(".git/worktrees/gone"), f.path("evil"))
	write(t, f.path(".git/worktrees/ok/commondir"), "../..\n")
	write(t, f.path(".git/worktrees/moved/commondir"), "../../../evil\n")
	want := []string{f.path(".git/worktrees/gone"), f.path(".git/worktrees/moved")}
	if got := f.p.worktreeStrays(); !slices.Equal(got, want) {
		t.Errorf("worktreeStrays = %q, want %q", got, want)
	}
}

func TestHooksPathInside(t *testing.T) {
	f := newFixture(t, "p")
	mkdirs(t, f.path(".husky"), filepath.Join(f.root, "other"))
	f.p.Home = f.root
	for _, tc := range []struct {
		path string
		want bool
	}{
		{".husky", true},
		{f.path(".husky"), true},
		{f.path("missing/hooks"), true},
		{filepath.Join(f.root, "other", "..", "p", ".husky"), true},
		{"~/p/.husky", true},
		{filepath.Join(f.root, "other"), false},
		{f.dir + "-sibling/hooks", false},
	} {
		if got := f.p.hooksPathInside(tc.path); got != tc.want {
			t.Errorf("hooksPathInside(%s) = %v", tc.path, got)
		}
	}
}

// A path that moved while it was locked (a swapped symlink) is unlocked
// again and reported.
func TestFlagRaced(t *testing.T) {
	f := newFixture(t, "p")
	mkdirs(t, f.path(".git"))
	cfg := f.path(".git/config")
	write(t, cfg, "[core]\n")
	f.p.Chflags = func(_ context.Context, flags, path string) error {
		f.flags = append(f.flags, flags+" "+path)
		if flags == "uchg" {
			if err := os.Rename(cfg, cfg+".old"); err != nil {
				t.Fatal(err)
			}
			write(t, cfg, "x")
		}
		return nil
	}
	if f.p.flag(t.Context(), "uchg", cfg) {
		t.Error("flag succeeded")
	}
	if !slices.Equal(f.flags, []string{"uchg " + cfg, "nouchg " + cfg}) || !f.warned("git_lock_raced path="+cfg) {
		t.Errorf("flags %q, warnings %q", f.flags, f.warns.Names())
	}
}

// Unlock takes off what lock put on, never through a symlink; on hosts
// without locks it changes nothing, and names what's still locked, as
// when devvm-immutable went after it locked.
func TestUnlock(t *testing.T) {
	f := newFixture(t, "p")
	mkdirs(t, f.path(".git/hooks"))
	other := filepath.Join(f.root, "other")
	write(t, other, "")
	symlink(t, other, f.path(".git/config"))
	f.p.Unlock(t.Context(), nil)
	if !slices.Equal(f.flags, []string{"nouchg " + f.path(".git/hooks")}) {
		t.Errorf("flags %q", f.flags)
	}
	f.flags = nil
	f.p.CanLock = false
	f.p.Unlock(t.Context(), f.vm)
	if len(f.flags) > 0 || len(f.warns.Names()) > 0 {
		t.Errorf("no locks: flags %q, warnings %q", f.flags, f.warns.Names())
	}
	f.immutable[f.path(".git/hooks")] = true
	f.p.Unlock(t.Context(), f.vm)
	if len(f.flags) > 0 || !slices.Equal(f.warns.Names(), []string{"git_still_locked paths=.git/hooks"}) {
		t.Errorf("a lock left: flags %q, warnings %q", f.flags, f.warns.Names())
	}
}

// A lock that fails is named, and why goes to the run log; one undone by
// a race is named as that alone; unlocking warns of nothing.
func TestLockFailed(t *testing.T) {
	f := newFixture(t, "p")
	mkdirs(t, f.path(".git/hooks"))
	var log strings.Builder
	f.p.Reporter = &msg.Fake{OnWarn: f.warns.Add, Log: &log}
	f.p.Chflags = func(context.Context, string, string) error { return errors.New("permission denied") }
	hooks := f.path(".git/hooks")
	if !f.p.flag(t.Context(), "uchg", hooks) || !slices.Equal(f.warns.Names(), []string{"git_lock_failed path=" + hooks}) {
		t.Errorf("warnings %q", f.warns.Names())
	}
	if !strings.Contains(log.String(), "locking "+hooks+": permission denied") {
		t.Errorf("run log %q", log.String())
	}
	f.warns.Reset()
	if !f.p.flag(t.Context(), "nouchg", hooks) || len(f.warns.Names()) > 0 {
		t.Errorf("unlocking: %q", f.warns.Names())
	}
}

// Unlock takes off the locks of every git dir the VM's state recorded
// locking too, not only those found now: one whose gitlink the VM dropped
// from the index isn't found again, and stays locked otherwise.
func TestUnlockRecorded(t *testing.T) {
	needGit(t)
	f := newFixture(t, "p")
	embeddedRepo(f)
	if err := f.newVM(false); err != nil {
		t.Fatal(err)
	}
	dirs := f.section().Made.GitDirs
	if len(dirs) == 0 {
		t.Fatal("no git dir besides .git recorded")
	}
	inside := filepath.Join(f.dir, dirs[len(dirs)-1])
	if !f.locked(filepath.Join(inside, "hooks")) {
		t.Fatalf("%s's hooks weren't locked: %q", inside, f.flags)
	}
	// The VM drops every gitlink from the index: the submodule's git dir
	// isn't found now.
	f.git(f.dir, "rm", "-q", "-f", "--cached", "-r", ".")
	if slices.Contains(f.p.lockedDirs(f.p.embedded(t.Context())), inside) {
		t.Fatalf("%s is still found", inside)
	}
	f.flags = nil
	f.p.Unlock(t.Context(), f.vm)
	if f.locked(filepath.Join(inside, "hooks")) {
		t.Errorf("%s's hooks stay locked: %q", inside, f.flags)
	}
}

// A mode saved for a running VM's next stop changes nothing until Set
// sets a mode, any mode, which drops it; a later SetNext replaces it.
func TestSetNext(t *testing.T) {
	f := newFixture(t, "p")
	mkdirs(t, f.path(".git/hooks"))
	if err := f.newVM(false); err != nil {
		t.Fatal(err)
	}
	f.flags = nil
	for _, m := range []string{Off, Strict} {
		if err := SetNext(t.Context(), f.vm, m); err != nil {
			t.Fatal(err)
		}
	}
	if Next(f.vm) != Strict || Mode(f.vm) != On || len(f.flags) > 0 {
		t.Errorf("saved: next %q, mode %q, flags %q", Next(f.vm), Mode(f.vm), f.flags)
	}
	if err := f.p.Set(t.Context(), f.vm, Off); err != nil {
		t.Fatal(err)
	}
	if Next(f.vm) != "" || Mode(f.vm) != Off {
		t.Errorf("set: next %q, mode %q", Next(f.vm), Mode(f.vm))
	}
}

// Off: nothing is locked, and off is the user's choice. On hosts without
// locks it's off, not chosen, and creating says nothing: the launches
// warn (WarnUnprotected).
func TestNewVMOffAndLinux(t *testing.T) {
	f := newFixture(t, "p")
	mkdirs(t, f.path(".git/hooks"))
	if err := f.newVM(true); err != nil {
		t.Fatal(err)
	}
	if Mode(f.vm) != "off" || !f.section().Chosen || len(f.flags) > 0 || len(f.warns.Names()) > 0 {
		t.Errorf("off: %s, chosen %t, locks %q, warnings %q", Mode(f.vm), f.section().Chosen, f.flags, f.warns.Names())
	}
	f.p.CanLock = false
	if err := f.newVM(false); err != nil {
		t.Fatal(err)
	}
	if Mode(f.vm) != "off" || f.section().Chosen || len(f.flags) > 0 || len(f.warns.Names()) > 0 {
		t.Errorf("linux: %s, chosen %t, locks %q, warnings %q", Mode(f.vm), f.section().Chosen, f.flags, f.warns.Names())
	}
	if err := os.RemoveAll(f.path(".git")); err != nil {
		t.Fatal(err)
	}
	f.p.CanLock = true
	if err := f.newVM(false); err != nil || Mode(f.vm) != "on" || len(f.flags) > 0 {
		t.Errorf("no .git: %v, %s, locks %q", err, Mode(f.vm), f.flags)
	}
}

// A protector locks where the host can: on macOS, always; on Linux, with
// devvm-immutable alone, so without one it says why.
func TestNewCanLock(t *testing.T) {
	p := New(t.Context(), t.TempDir(), t.TempDir(), platform.Locker{}, nil)
	switch runtime.GOOS {
	case "darwin":
		if !p.CanLock || p.Unable != nil {
			t.Errorf("macOS: CanLock %t, %v", p.CanLock, p.Unable)
		}
	case "linux":
		if p.CanLock || !errors.Is(p.Unable, platform.ErrNoHelper) {
			t.Errorf("Linux without a helper: CanLock %t, %v", p.CanLock, p.Unable)
		}
	}
}

// Why a host can't lock is in the launches' warning and in devvm
// git-protect on's refusal.
func TestCantLockSaysWhy(t *testing.T) {
	for why, want := range map[error][2]string{
		platform.ErrNoHelper:       {"git_unprotected", "git_protect_on_needs_helper"},
		platform.ErrHelperUnusable: {"git_unprotected_helper", "git_protect_on_helper_unusable"},
		platform.ErrNoAttribute:    {"git_unprotected_fs", "git_protect_on_needs_fs"},
		errors.ErrUnsupported:      {"git_unprotected", "git_protect_on_needs_helper"},
	} {
		wrapped := fmt.Errorf("wrapped: %w", why)
		if got := [2]string{msg.IDOf(unprotected(wrapped, "vm")), msg.IDOf(cantLock(wrapped))}; got != want {
			t.Errorf("%v: %q, want %q", why, got, want)
		}
	}
}

// The real flag, on macOS: the VM's user can't add a hook or change the
// config, nor make a hooks folder the repo didn't have.
func TestRealLocks(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("chflags is macOS's")
	}
	needGit(t)
	f := newFixture(t, "p")
	t.Cleanup(func() { _ = exec.Command("chflags", "-R", "nouchg", f.root).Run() }) //nolint:noctx,gosec // cleanup, of the test's own folder
	f.p.Chflags = platform.Locker{}.Chflags
	f.git(f.root, "init", "-q", "--template=", f.dir)
	if err := f.newVM(false); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(f.path(".git/hooks/x"), 0o700); err == nil {
		t.Error("made a folder in the locked hooks")
	}
	if err := os.WriteFile(f.path(".git/hooks/pre-commit"), []byte("x"), 0o600); err == nil {
		t.Error("added a hook")
	}
	if err := os.WriteFile(f.path(".git/config"), []byte("x"), 0o600); err == nil {
		t.Error("changed the config")
	}
	if len(f.warns.Names()) > 0 {
		t.Errorf("warnings %q", f.warns.Names())
	}
	if locked := platform.Immutable; !locked(f.path(".git/config")) || locked(f.path("HEAD")) {
		t.Error("Locked doesn't see the flag")
	}
}

// checked runs Check on a stopped VM, with the warnings and locks it
// made from now.
func (f *fixture) checked(running bool) {
	f.t.Helper()
	f.warns.Reset()
	f.flags = nil
	if err := f.p.Check(f.t.Context(), f.vm, running); err != nil {
		f.t.Fatal(err)
	}
}

// A .git replaced since it was locked is reported, and nothing else is
// done; one that appeared since the VM was made is locked, and said.
func TestCheckReplacedAndAppeared(t *testing.T) {
	needGit(t)
	f := newFixture(t, "p")
	if err := f.newVM(false); err != nil {
		t.Fatal(err)
	}
	f.git(f.dir, "init", "-q")
	f.checked(false)
	if !slices.Equal(f.warns.Names(), []string{"git_appeared"}) || !f.locked(f.path(".git/config")) {
		t.Errorf("appeared: warnings %q, locks %q", f.warns.Names(), f.flags)
	}
	f.checked(false)
	if len(f.warns.Names()) > 0 {
		t.Errorf("again: warnings %q", f.warns.Names())
	}
	if err := os.Rename(f.path(".git"), f.path(".git-old")); err != nil {
		t.Fatal(err)
	}
	f.git(f.dir, "init", "-q")
	f.checked(false)
	if !slices.Equal(f.warns.Names(), []string{"git_replaced git=" + f.path(".git")}) || len(f.flags) > 0 {
		t.Errorf("replaced: warnings %q, locks %q", f.warns.Names(), f.flags)
	}
}

// Hooks in a hooks folder that wasn't locked are reported on a check, and
// it's locked; a running VM is checked, but nothing is locked.
func TestCheckHooksUnlocked(t *testing.T) {
	needGit(t)
	f := newFixture(t, "p")
	f.git(f.root, "init", "-q", "--template=", f.dir)
	if err := f.newVM(false); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.path(".git/hooks")); err != nil {
		t.Fatal(err)
	}
	mkdirs(t, f.path(".git/hooks"))
	write(t, f.path(".git/hooks/pre-commit"), "#!/bin/sh\n")
	write(t, f.path(".git/hooks/pre-push.sample"), "#!/bin/sh\n")
	f.immutable[f.path(".git/hooks")] = false // the folder made again isn't locked
	f.checked(true)
	if !slices.Equal(f.warns.Names(), []string{"git_hooks_unlocked hooks=" + f.path(".git/hooks")}) || len(f.flags) > 0 {
		t.Errorf("running: warnings %q, locks %q", f.warns.Names(), f.flags)
	}
	f.checked(false)
	if !f.locked(f.path(".git/hooks")) {
		t.Errorf("stopped: locks %q", f.flags)
	}
	f.checked(false)
	if len(f.warns.Names()) > 0 {
		t.Errorf("locked: warnings %q", f.warns.Names())
	}
}

// A submodule git dir that appeared in the working tree, or under
// .git/modules, is reported once and locked; under .git/modules, not the
// first time there's a record to compare with.
func TestCheckSubmodules(t *testing.T) {
	needGit(t)
	f := newFixture(t, "p")
	f.git(f.dir, "init", "-q")
	f.git(f.dir, "commit", "-q", "--allow-empty", "-m", "x")
	if err := f.newVM(false); err != nil {
		t.Fatal(err)
	}
	embeddedRepo(f)
	f.checked(false)
	for _, want := range []string{"git_embedded_new git=" + f.path("sub/.git"), "git_embedded_new git=" + f.path(".gitdirs/rel")} {
		if !f.warned(want) {
			t.Errorf("warnings %q lack %s", f.warns.Names(), want)
		}
	}
	if !f.locked(f.path("sub/.git/config")) {
		t.Errorf("locks %q", f.flags)
	}
	f.checked(false)
	if f.warned("git_embedded_new") {
		t.Errorf("again: warnings %q", f.warns.Names())
	}
	cp := func(to string) {
		t.Helper()
		if out, err := exec.CommandContext(t.Context(), "cp", "-R", f.path("sub/.git"), f.path(to)).CombinedOutput(); err != nil { //nolint:gosec // the test's own
			t.Fatalf("cp: %v %s", err, out)
		}
	}
	mkdirs(t, f.path(".git/modules"))
	cp(".git/modules/old")
	// Nothing recorded yet (never set up): nothing to compare with.
	if err := record(t.Context(), f.vm, func(g *InVM) { g.Made = Made{} }); err != nil {
		t.Fatal(err)
	}
	f.checked(false)
	if f.warned("git_embedded_new") {
		t.Errorf("first record: warnings %q", f.warns.Names())
	}
	cp(".git/modules/new")
	f.checked(false)
	if !slices.Equal(f.warns.Names(), []string{"git_embedded_new git=" + f.path(".git/modules/new")}) || !f.locked(f.path(".git/modules/new/config")) {
		t.Errorf("modules: warnings %q, locks %q", f.warns.Names(), f.flags)
	}
}

// Includes and hook links into the project folder are reported again only
// when they change; commondir on every check.
func TestCheckDelegationsAndCommondir(t *testing.T) {
	needGit(t)
	f := newFixture(t, "p")
	f.git(f.dir, "init", "-q")
	cfg := f.path(".git/config")
	f.git(f.dir, "config", "--file", cfg, "include.path", "../shared.gitconfig")
	if err := f.newVM(false); err != nil {
		t.Fatal(err)
	}
	f.checked(false)
	if len(f.warns.Names()) > 0 {
		t.Errorf("unchanged: warnings %q", f.warns.Names())
	}
	f.git(f.dir, "config", "--file", cfg, "--add", "include.path", "sub.gitconfig")
	f.checked(false)
	if !f.warned("path=sub.gitconfig") || !f.warned("path=../shared.gitconfig") {
		t.Errorf("changed: warnings %q", f.warns.Names())
	}
	write(t, f.path(".git/commondir"), "../x\n")
	f.checked(false)
	f.checked(false)
	if !slices.Equal(f.warns.Names(), []string{"git_commondir git=" + f.path(".git") + " target=../x"}) {
		t.Errorf("commondir: warnings %q", f.warns.Names())
	}
}

// Off, or on a host without locks: nothing. Never set up (its creation
// stopped short after its mode was recorded): set up as new, unless it
// runs.
func TestCheckOffAndUnset(t *testing.T) {
	needGit(t)
	f := newFixture(t, "p")
	f.git(f.dir, "init", "-q")
	if err := record(t.Context(), f.vm, func(g *InVM) { g.Mode = On }); err != nil {
		t.Fatal(err)
	}
	f.checked(true)
	if f.section().Made.Protected || len(f.flags) > 0 {
		t.Errorf("unset, running: %+v, locks %q", f.section(), f.flags)
	}
	f.checked(false)
	if g := f.section(); g.Mode != On || !g.Made.Protected || !f.locked(f.path(".git/config")) || f.warned("git_appeared") {
		t.Errorf("unset: %+v, locks %q, warnings %q", g, f.flags, f.warns.Names())
	}
	if err := record(t.Context(), f.vm, func(g *InVM) { g.Mode = Off }); err != nil {
		t.Fatal(err)
	}
	f.checked(false)
	if len(f.flags)+len(f.warns.Names()) > 0 {
		t.Errorf("off: locks %q, warnings %q", f.flags, f.warns.Names())
	}
}

func (f *fixture) set(mode string) {
	f.t.Helper()
	f.warns.Reset()
	f.flags = nil
	if err := f.p.Set(f.t.Context(), f.vm, mode); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) ignoreSubmodules() string {
	f.t.Helper()
	out, _ := exec.CommandContext(f.t.Context(), "git", "config", "--file", f.path(".git/config"), "diff.ignoreSubmodules").Output() //nolint:gosec // the test's own
	return strings.TrimSpace(string(out))
}

// Strict keeps host git out of submodules (diff.ignoreSubmodules=all, set
// with the config unlocked, then locked again); on and off take it back
// out. On locks .git as it is now, a replaced one included, and names
// submodules that appeared, and what's handed over, as a new VM does.
func TestSet(t *testing.T) {
	needGit(t)
	f := newFixture(t, "p")
	f.git(f.dir, "init", "-q")
	f.git(f.dir, "config", "--file", f.path(".git/config"), "include.path", "../x.gitconfig")
	if err := f.newVM(false); err != nil {
		t.Fatal(err)
	}
	cfg := f.path(".git/config")
	f.set(Strict)
	if f.ignoreSubmodules() != "all" || Mode(f.vm) != Strict || Mode(f.vm) != Strict {
		t.Errorf("strict: diff.ignoreSubmodules=%q, mode %s", f.ignoreSubmodules(), Mode(f.vm))
	}
	if !slices.Equal(f.flags[:1], []string{"nouchg " + cfg}) || !f.locked(cfg) {
		t.Errorf("strict: locks %q", f.flags)
	}
	if !f.warned("git_include_inside") {
		t.Errorf("strict: warnings %q", f.warns.Names())
	}
	f.set(On)
	if f.ignoreSubmodules() != "" || Mode(f.vm) != On || !f.locked(cfg) {
		t.Errorf("on: diff.ignoreSubmodules=%q, mode %s, locks %q", f.ignoreSubmodules(), Mode(f.vm), f.flags)
	}
	f.set(Strict)
	f.set(Off)
	if f.ignoreSubmodules() != "" || Mode(f.vm) != Off || !slices.Contains(f.flags, "nouchg "+cfg) || f.locked(cfg) {
		t.Errorf("off: diff.ignoreSubmodules=%q, mode %s, locks %q", f.ignoreSubmodules(), Mode(f.vm), f.flags)
	}
	// Off again, and checks, lock nothing.
	f.checked(false)
	if len(f.flags) > 0 {
		t.Errorf("off: locks %q", f.flags)
	}

	// A replaced .git is taken as it is, and a submodule that appeared
	// is named.
	if err := os.Rename(f.path(".git"), f.path(".git-old")); err != nil {
		t.Fatal(err)
	}
	embeddedRepo(f)
	f.set(On)
	if f.warned("git_replaced") || f.warned("git_appeared") || !f.warned("git_embedded_new git="+f.path("sub/.git")) {
		t.Errorf("replaced: warnings %q", f.warns.Names())
	}
	if !f.locked(cfg) || !f.locked(f.path("sub/.git/config")) || f.section().Made.GitID != folderID(f.path(".git")) {
		t.Errorf("replaced: locks %q, git_id %s", f.flags, f.section().Made.GitID)
	}
	f.checked(false)
	if len(f.warns.Names()) > 0 {
		t.Errorf("after on: warnings %q", f.warns.Names())
	}
}

// Strict that can't set diff.ignoreSubmodules fails, with the config
// locked again and the mode as it was.
func TestSetStrictFails(t *testing.T) {
	needGit(t)
	f := newFixture(t, "p")
	f.git(f.dir, "init", "-q")
	if err := f.newVM(false); err != nil {
		t.Fatal(err)
	}
	cfg := f.path(".git/config")
	write(t, cfg, "[core\n")
	f.flags = nil
	err := f.p.Set(t.Context(), f.vm, Strict)
	if err == nil || err.(msg.M).Names() != "git_config_failed config="+cfg { //nolint:errorlint,forcetypeassert // a message
		t.Fatalf("err = %v", err)
	}
	if !slices.Equal(f.flags, []string{"nouchg " + cfg, "uchg " + cfg}) || Mode(f.vm) != On {
		t.Errorf("locks %q, mode %s", f.flags, Mode(f.vm))
	}
}

// On a host without locks, on and strict are refused; off is taken.
// Without a .git, on is recorded for when one appears.
func TestSetLinuxAndNoGit(t *testing.T) {
	f := newFixture(t, "p")
	f.p.CanLock = false
	if err := f.newVM(false); err != nil || Mode(f.vm) != Off {
		t.Errorf("new on linux: %s, %v", Mode(f.vm), err)
	}
	for _, m := range []string{On, Strict} {
		if err := f.p.Set(t.Context(), f.vm, m); err == nil || err.(msg.M).Names() != "git_protect_on_needs_helper" { //nolint:errorlint,forcetypeassert // a message
			t.Errorf("%s on linux: %v", m, err)
		}
	}
	f.set(Off)
	if Mode(f.vm) != Off || len(f.flags) > 0 {
		t.Errorf("off on linux: %s, locks %q", Mode(f.vm), f.flags)
	}
	f.p.CanLock = true
	f.set(Strict)
	if Mode(f.vm) != Strict || len(f.flags)+len(f.warns.Names()) > 0 {
		t.Errorf("no .git: %s, locks %q, warnings %q", Mode(f.vm), f.flags, f.warns.Names())
	}
}

// ~ in git's paths is the home folder New is given, never the
// environment's HOME.
func TestNewHome(t *testing.T) {
	t.Setenv("HOME", "/elsewhere")
	p := New(t.Context(), "/p", "/h", platform.Locker{}, nil)
	for in, want := range map[string]string{"~": "/h", "~/x": "/h/x", "~x/y": "~x/y", "/a": "/a"} {
		if got := expandHome(p.Home, in); got != want {
			t.Errorf("expandHome(%q) = %q, want %q", in, got, want)
		}
	}
}
