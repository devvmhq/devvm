package gitprotect

import (
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/hostsafety"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/stubtest"
)

// walked gives the fixture's protector what the walk finds now, as a
// stopped VM's launch does.
func (f *fixture) walked() {
	found := hostsafety.Walk(f.dir, false)
	f.p.Nested = &found.Repos
}

// repo makes a git repo at rel in the project folder.
func (f *fixture) repo(rel string) {
	f.t.Helper()
	mkdirs(f.t, f.path(rel))
	f.git(f.path(rel), "init", "-q")
}

// recordedRepos are the nested repos the VM's state records protecting.
func (f *fixture) recordedRepos() []string {
	return slices.Sorted(maps.Keys(f.section().Made.Repos))
}

// Each repo nested in a new VM's folder is protected as its own repo is:
// locked, recorded, and checked again at each check, which names one that
// appeared since, and one that's gone. Submodules under .git/modules and
// the repo's own linked worktrees are its own repo's to protect; a
// submodule in the working tree is scanned as a repo of its own too, as
// the index that names it is the VM's to write.
func TestNestedProtected(t *testing.T) {
	needGit(t)
	f := newFixture(t, "p")
	embeddedRepo(f)
	f.git(f.dir, "worktree", "add", "-q", f.path("wt"))
	f.repo("ws/a")
	f.walked()
	if !slices.Equal(f.p.Nested.Dirs, []string{"rel", "sub", "ws/a", "wt"}) {
		t.Fatalf("walk: %q", f.p.Nested.Dirs)
	}
	if err := f.newVM(false); err != nil {
		t.Fatal(err)
	}
	if f.warned("git_nested") || !f.locked(f.path("ws/a/.git/config")) || !f.locked(f.path("ws/a/.git/hooks")) {
		t.Errorf("new VM: %q, locks %q", f.warns.Names(), f.flags)
	}
	if got := f.recordedRepos(); !slices.Equal(got, []string{"rel", "sub", "ws/a"}) || !f.section().Made.Nested {
		t.Errorf("recorded %q", got)
	}
	f.walked()
	f.checked(false)
	if len(f.warns.Names()) > 0 || !f.locked(f.path("ws/a/.git/config")) {
		t.Errorf("again: %q, locks %q", f.warns.Names(), f.flags)
	}
	// One the VM made while it ran.
	f.repo("ws/b")
	f.walked()
	f.checked(false)
	if !slices.Equal(f.warns.Names(), []string{"git_nested_appeared repo=ws/b vm=" + f.vm.Name()}) || !f.locked(f.path("ws/b/.git/config")) {
		t.Errorf("appeared: %q, locks %q", f.warns.Names(), f.flags)
	}
	// A running VM's check walks nothing: what's recorded is checked, and
	// nothing locked.
	f.p.Nested = nil
	f.checked(true)
	if len(f.warns.Names()) > 0 || len(f.flags) > 0 || !slices.Equal(f.recordedRepos(), []string{"rel", "sub", "ws/a", "ws/b"}) {
		t.Errorf("running: %q, locks %q, %q", f.warns.Names(), f.flags, f.recordedRepos())
	}
	// One replaced, and one gone.
	if err := os.Rename(f.path("ws/a/.git"), f.path("ws/a/.git-old")); err != nil {
		t.Fatal(err)
	}
	f.git(f.path("ws/a"), "init", "-q")
	if err := os.RemoveAll(f.path("ws/b")); err != nil {
		t.Fatal(err)
	}
	f.walked()
	f.checked(false)
	// The old .git, a git dir that isn't a .git now, is named too: git
	// run below it would take it for its repo.
	want := []string{
		"git_replaced git=" + f.path("ws/a/.git"), "git_nested_gone repo=ws/b vm=" + f.vm.Name(),
		"git_nested_bare dirs=ws/a/.git-old vm=" + f.vm.Name(),
	}
	if !slices.Equal(f.warns.Names(), want) || !slices.Equal(f.recordedRepos(), []string{"rel", "sub", "ws/a"}) {
		t.Errorf("replaced and gone: %q, %q", f.warns.Names(), f.recordedRepos())
	}
}

// A folder of repos without one of its own: each is protected; devvm
// git-protect off unlocks them, and on locks them again.
func TestNestedNoTopRepo(t *testing.T) {
	needGit(t)
	f := newFixture(t, "ws")
	f.repo("a")
	f.repo("b")
	f.walked()
	if err := f.newVM(false); err != nil {
		t.Fatal(err)
	}
	if len(f.warns.Names()) > 0 || !f.locked(f.path("a/.git/config")) || !f.locked(f.path("b/.git/config")) {
		t.Errorf("new VM: %q, locks %q", f.warns.Names(), f.flags)
	}
	f.p.Nested = nil
	f.set(Off)
	if !slices.Contains(f.flags, "nouchg "+f.path("b/.git/config")) {
		t.Errorf("off: %q", f.flags)
	}
	f.walked()
	f.set(On)
	if !f.locked(f.path("a/.git/config")) || !f.locked(f.path("b/.git/config")) {
		t.Errorf("on: %q", f.flags)
	}
	// Past the walk's bound, what isn't protected is counted.
	f.p.Nested.More, f.p.Nested.Past = 3, []string{"x", "y", "z"}
	f.set(On)
	if !slices.Equal(f.warns.Names(), []string{"git_nested_past_bound count=3 repos=x, y, z vm=" + f.vm.Name()}) {
		t.Errorf("past the bound: %q", f.warns.Names())
	}
}

// A VM an older devvm made protects its own repo alone: the first check
// that finds nested repos says once that devvm git-protect on protects
// them, and later ones name a repo that appeared since; on protects them
// all.
func TestNestedExistingVM(t *testing.T) {
	needGit(t)
	f := newFixture(t, "p")
	f.git(f.dir, "init", "-q")
	if err := f.newVM(false); err != nil {
		t.Fatal(err)
	}
	if err := record(t.Context(), f.vm, func(g *InVM) { g.Made.Nested = false }); err != nil {
		t.Fatal(err)
	}
	f.repo("a")
	f.walked()
	f.checked(false)
	if !slices.Equal(f.warns.Names(), []string{"git_nested_protectable repos=a vm=" + f.vm.Name()}) || f.locked(f.path("a/.git/config")) {
		t.Errorf("first check: %q, locks %q", f.warns.Names(), f.flags)
	}
	f.checked(false)
	if len(f.warns.Names()) > 0 {
		t.Errorf("again: %q", f.warns.Names())
	}
	f.repo("c")
	f.walked()
	f.checked(false)
	if !slices.Equal(f.warns.Names(), []string{"git_nested_unprotected repos=c vm=" + f.vm.Name()}) {
		t.Errorf("one more: %q", f.warns.Names())
	}
	f.set(On)
	if len(f.warns.Names()) > 0 || !f.locked(f.path("a/.git/config")) || !f.locked(f.path("c/.git/config")) {
		t.Errorf("on: %q, locks %q", f.warns.Names(), f.flags)
	}
	if g := f.section(); !g.Made.Nested || len(g.Nested) > 0 {
		t.Errorf("on: %+v", g)
	}
}

// Unlock leaves what another VM's protection keeps locked too, and names
// that VM; the rest it unlocks.
func TestNestedUnlockKept(t *testing.T) {
	needGit(t)
	f := newFixture(t, "ws")
	f.repo("a")
	f.repo("b")
	f.walked()
	if err := f.newVM(false); err != nil {
		t.Fatal(err)
	}
	f.p.Kept = func(vm string) func(string) msg.M {
		if vm != f.vm.Name() {
			t.Errorf("kept asked for %s", vm)
		}
		return func(path string) msg.M {
			if strings.HasPrefix(path, f.path("a")+"/") {
				return msg.GitKeptLocked("devvm-a-0123456789abcd")
			}
			return msg.M{}
		}
	}
	f.flags = nil
	f.warns.Reset()
	f.p.Unlock(t.Context(), f.vm)
	if slices.ContainsFunc(f.flags, func(s string) bool { return strings.Contains(s, "/ws/a/") }) ||
		!slices.Contains(f.flags, "nouchg "+f.path("b/.git/config")) {
		t.Errorf("unlocked %q", f.flags)
	}
	if !slices.Equal(f.warns.Names(), []string{"git_kept_locked vm=devvm-a-0123456789abcd"}) {
		t.Errorf("warned %q", f.warns.Names())
	}
}

// The VM can't hide a nested repo behind a gitlink it adds to the
// folder's own index, whose .git file names the nested repo's git dir:
// the nested repo is still scanned, recorded and locked on its own.
func TestNestedGitlinkCantHide(t *testing.T) {
	needGit(t)
	f := newFixture(t, "p")
	f.git(f.dir, "init", "-q")
	f.repo("b")
	f.git(f.path("b"), "commit", "-q", "--allow-empty", "-m", "b")
	f.walked()
	if err := f.newVM(false); err != nil {
		t.Fatal(err)
	}
	mkdirs(t, f.path("z"))
	write(t, f.path("z/.git"), "gitdir: ../b/.git\n")
	f.git(f.dir, "update-index", "--add", "--cacheinfo", "160000,"+f.git(f.path("b"), "rev-parse", "HEAD")+",z")
	f.walked()
	f.checked(false)
	if !slices.Contains(f.recordedRepos(), "b") || !f.locked(f.path("b/.git/config")) || f.warned("git_nested_gone") {
		t.Errorf("hidden: %q, %q, locks %q", f.recordedRepos(), f.warns.Names(), f.flags)
	}
}

// evilGitDir makes dir a git dir whose config runs ./evil as git's
// fsmonitor: what a VM plants.
func (f *fixture) evilGitDir(dir string) {
	f.t.Helper()
	mkdirs(f.t, filepath.Join(dir, "objects"), filepath.Join(dir, "refs"))
	write(f.t, filepath.Join(dir, "HEAD"), "ref: refs/heads/main\n")
	write(f.t, filepath.Join(dir, "config"), "[core]\n\tfsmonitor = ./evil\n")
}

// A repo the VM makes whose .git file leads to a git dir it made below
// the folder's own .git, or to one under .git/modules that reads its
// config from elsewhere (commondir), isn't left to the folder's own
// repo, whose locks don't cover it: it's named as one that appeared,
// with its fsmonitor, and as one devvm doesn't protect.
func TestNestedIntoTopGit(t *testing.T) {
	needGit(t)
	f := newFixture(t, "p")
	f.git(f.dir, "init", "-q")
	f.walked()
	if err := f.newVM(false); err != nil {
		t.Fatal(err)
	}
	f.evilGitDir(f.path(".git/evil"))
	mkdirs(t, f.path("src"), f.path(".git/modules/x"), f.path("lib"))
	write(t, f.path("src/.git"), "gitdir: ../.git/evil\n")
	write(t, f.path(".git/modules/x/HEAD"), "ref: refs/heads/main\n")
	write(t, f.path(".git/modules/x/commondir"), "../../evil\n")
	write(t, f.path("lib/.git"), "gitdir: ../.git/modules/x\n")
	f.walked()
	f.checked(false)
	for _, w := range []string{
		"git_nested_appeared_runs repo=lib runs=core.fsmonitor vm=" + f.vm.Name(),
		"git_nested_appeared_runs repo=src runs=core.fsmonitor vm=" + f.vm.Name(),
		"git_nested_unprotected repos=lib, src vm=" + f.vm.Name(),
	} {
		if !f.warned(w) {
			t.Errorf("not warned %s: %q", w, f.warns.Names())
		}
	}
	if got := f.recordedRepos(); !slices.Equal(got, []string{"lib", "src"}) {
		t.Errorf("recorded %q", got)
	}
}

// A linked worktree under the folder's own .git that's a symlink to a
// folder the VM made, whose commondir leads back, covers nothing: no lock
// reaches its commondir, which the VM can point elsewhere later, so a
// repo whose .git file names it is named.
func TestNestedSymlinkedWorktree(t *testing.T) {
	needGit(t)
	f := newFixture(t, "p")
	f.git(f.dir, "init", "-q")
	f.walked()
	if err := f.newVM(false); err != nil {
		t.Fatal(err)
	}
	mkdirs(t, f.path("x"), f.path(".git/worktrees"), f.path("src"))
	write(t, f.path("x/HEAD"), "ref: refs/heads/main\n")
	write(t, f.path("x/commondir"), f.path(".git")+"\n")
	symlink(t, f.path("x"), f.path(".git/worktrees/evil"))
	write(t, f.path("src/.git"), "gitdir: ../.git/worktrees/evil\n")
	f.walked()
	f.checked(false)
	for _, w := range []string{"git_nested_appeared repo=src", "git_nested_unprotected repos=src"} {
		if !f.warned(w) {
			t.Errorf("not warned %s: %q", w, f.warns.Names())
		}
	}
}

// The VM can't make a protected nested repo gone while git still takes
// its .git: with its objects and refs renamed and a commondir to a git
// dir it made, a .GIT where names fold case, or a .git it can't read,
// the repo stays recorded and checked.
func TestNestedNotGone(t *testing.T) {
	needGit(t)
	for _, tc := range []struct {
		name string
		do   func(f *fixture)
	}{
		{"commondir", func(f *fixture) {
			f.evilGitDir(f.path("evil"))
			for _, d := range []string{"objects", "refs"} {
				if err := os.Rename(f.path("a/.git/"+d), f.path("a/.git/"+d+"-old")); err != nil {
					f.t.Fatal(err)
				}
			}
			write(f.t, f.path("a/.git/commondir"), "../../evil\n")
		}},
		{".GIT", func(f *fixture) {
			if !platform.FoldsCase {
				f.t.Skip("names don't fold case here")
			}
			if err := os.Rename(f.path("a/.git"), f.path("a/.GIT")); err != nil {
				f.t.Fatal(err)
			}
		}},
		{"unreadable", func(f *fixture) {
			if err := os.Chmod(f.path("a/.git"), 0); err != nil {
				f.t.Fatal(err)
			}
			f.t.Cleanup(func() { _ = os.Chmod(f.path("a/.git"), 0o700) }) //nolint:gosec // the test's own folder, put back
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, "ws")
			f.repo("a")
			f.walked()
			if err := f.newVM(false); err != nil {
				t.Fatal(err)
			}
			tc.do(f)
			f.walked()
			f.checked(false)
			if f.warned("git_nested_gone") || !slices.Equal(f.recordedRepos(), []string{"a"}) {
				t.Errorf("gone: %q, %q", f.warns.Names(), f.recordedRepos())
			}
		})
	}
}

// A check's scans share one budget of submodule git dirs: past it, a
// scan follows no more, and its repo is named.
func TestScanBudget(t *testing.T) {
	needGit(t)
	f := newFixture(t, "p")
	embeddedRepo(f)
	b := newBudget()
	b.left.Store(0)
	tr := f.p.tree
	tr.budget = b
	if s := tr.scan(t.Context()); !s.emb.bounded || len(s.emb.inside) > 0 {
		t.Errorf("past the budget: %+v", s.emb)
	}
	s := f.p.scan(t.Context())
	if s.emb.bounded {
		t.Error("bounded without a budget")
	}
	f.p.protectRepo(t.Context(), &run{why: whyCheck, how: later, top: true, s: snapshot{tree: tr, git: s.git, exists: true, emb: embeds{bounded: true}}})
	if !f.warned("git_scan_bounded git=" + f.path(".git")) {
		t.Errorf("warned %q", f.warns.Names())
	}
}

// What a repo the VM made runs on this machine: config keys that name a
// program or another config, aliases that run a shell, and hooks.
func TestRunsProgram(t *testing.T) {
	for kv, want := range map[[2]string]bool{
		{"core.fsmonitor", "x"}: true, {"core.hookspath", "x"}: true, {"core.sshcommand", "x"}: true,
		{"core.pager", "x"}: true, {"filter.lfs.clean", "x"}: true, {"filter.lfs.required", "true"}: false,
		{"alias.st", "status"}: false, {"alias.x", "!sh -c evil"}: true, {"include.path", "x"}: true,
		{"includeif.gitdir:/x.path", "y"}: true, {"diff.foo.textconv", "x"}: true, {"merge.foo.driver", "x"}: true,
		{"credential.https://h.helper", "x"}: true, {"user.name", "x"}: false, {"core.bare", "false"}: false,
	} {
		if got := runsProgram(kv[0], kv[1]); got != want {
			t.Errorf("%s=%s: %v", kv[0], kv[1], got)
		}
	}
}

// A nested repo devvm stopped protecting, as it's gone, is still unlocked
// by devvm rm and devvm git-protect off; one that isn't in the folder any
// more is named, as its locks stay wherever it went.
func TestNestedLeftUnlocked(t *testing.T) {
	needGit(t)
	f := newFixture(t, "ws")
	f.repo("a")
	f.repo("b")
	f.walked()
	if err := f.newVM(false); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(f.path("b"), f.path("b-moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(f.path("b-moved/.git"), f.path("b-moved/git-kept")); err != nil {
		t.Fatal(err)
	}
	f.walked()
	f.checked(false)
	if g := f.section(); len(g.Made.Left) != 1 || g.Made.Left["b"].GitID == "" {
		t.Fatalf("left %+v", g.Made.Left)
	}
	f.flags = nil
	f.warns.Reset()
	f.p.Unlock(t.Context(), f.vm)
	if !slices.Contains(f.flags, "nouchg "+f.path("a/.git/config")) || !f.warned("git_locks_left repos=b") {
		t.Errorf("unlock: %q, %q", f.flags, f.warns.Names())
	}
}

// Gitlinks that lead back to a git dir already followed, through a
// symlinked folder, are followed once.
func TestEmbeddedOnce(t *testing.T) {
	needGit(t)
	f := newFixture(t, "p")
	embeddedRepo(f)
	symlink(t, ".", f.path("loop"))
	mkdirs(t, f.path("again"))
	write(t, f.path("again/.git"), "gitdir: ../sub/.git\n")
	head := f.git(f.dir, "rev-parse", "HEAD")
	for _, l := range []string{"loop", "again"} {
		f.git(f.dir, "update-index", "--add", "--cacheinfo", "160000,"+head+","+l)
	}
	if e := f.p.embedded(t.Context()); len(e.inside) != 2 {
		t.Errorf("inside %q", e.inside)
	}
}

// A VM's protection keeps the git dirs it locked: its own repo's, its
// submodules', and its nested repos', while it's on, or an earlier
// devvm's strict.
func TestKeeps(t *testing.T) {
	f := newFixture(t, "ws")
	g := InVM{Mode: On, Made: Made{
		Protected: true, Repo: Repo{GitDirs: []string{"sub/.git"}}, Nested: true,
		Repos: map[string]Repo{"a": {GitDirs: []string{"a/m/.git"}}},
	}}
	if err := f.vm.Update(t.Context(), func(st *state.VM) error { return Set(st, g) }); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]bool{
		f.path(".git/config"): true, f.path("sub/.git/hooks"): true, f.path("a/.git/config"): true,
		f.path("a/m/.git/config"): true, f.path("a/src"): false, f.path("b/.git/config"): false,
	} {
		if got := Keeps(f.vm, path); got != want {
			t.Errorf("%s: %v", path, got)
		}
	}
	g.Mode = Strict
	if err := f.vm.Update(t.Context(), func(st *state.VM) error { return Set(st, g) }); err != nil {
		t.Fatal(err)
	}
	if !Keeps(f.vm, f.path(".git/config")) {
		t.Error("strict doesn't keep")
	}
	g.Mode = Off
	if err := f.vm.Update(t.Context(), func(st *state.VM) error { return Set(st, g) }); err != nil {
		t.Fatal(err)
	}
	if Keeps(f.vm, f.path(".git/config")) {
		t.Error("off keeps")
	}
}

// Nested repos are recorded under repos, by their folder; the folder's
// own repo's fields stay where 0.1.0 had them.
func TestMadeJSON(t *testing.T) {
	m := Made{Protected: true, Repo: Repo{GitID: "1:2", GitDirs: []string{"s"}}, Nested: true, Repos: map[string]Repo{"a": {GitID: "3:4"}}}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"protected":true,"git_id":"1:2","git_dirs":["s"],"nested":true,"repos":{"a":{"git_id":"3:4"}}}`
	if string(b) != want {
		t.Errorf("%s", b)
	}
	var back Made
	if err := json.Unmarshal([]byte(`{"protected":true,"git_id":"1:2","delegations":"d"}`), &back); err != nil ||
		back.GitID != "1:2" || back.Delegations != "d" || back.Nested || back.Repos != nil {
		t.Errorf("0.1.0's: %+v, %v", back, err)
	}
}

// On a host without locks, nested repos are watched from the first check,
// an existing VM's included, which records them without naming them; the
// launches warn that the VM can change .git, even without a repo of the
// folder's own.
func TestNestedWatch(t *testing.T) {
	needGit(t)
	f := newFixture(t, "ws")
	f.canLock(false)
	f.repo("a")
	f.walked()
	if err := f.newVM(false); err != nil {
		t.Fatal(err)
	}
	if len(f.warns.Names()) > 0 || f.section().Watched.Sums["a/.git/config"] == "" {
		t.Errorf("new VM: %q, %v", f.warns.Names(), f.section().Watched.Sums)
	}
	f.p.WarnUnprotected(f.vm)
	if !slices.Equal(f.warns.Names(), []string{"git_unprotected vm=" + f.vm.Name()}) {
		t.Errorf("launch: %q", f.warns.Names())
	}
	f.git(f.path("a"), "config", "core.fsmonitor", "./evil")
	f.repo("b")
	f.walked()
	if w := f.check(); !slices.Equal(w, []string{"git_nested_appeared repo=b vm=" + f.vm.Name(), "git_changed paths=a/.git/config vm=" + f.vm.Name()}) {
		t.Errorf("changed: %q", w)
	}
	// One an older devvm watched records its nested repos without naming.
	if err := record(t.Context(), f.vm, func(g *InVM) { g.Made.Nested, g.Made.Repos = false, nil }); err != nil {
		t.Fatal(err)
	}
	f.walked()
	if w := f.check(); len(w) > 0 || !slices.Equal(f.recordedRepos(), []string{"a", "b"}) {
		t.Errorf("older: %q, %q", w, f.recordedRepos())
	}
}

// The start pins each nested repo the VM protects, as its own; a running
// VM missing one's pins is named.
func TestNestedPins(t *testing.T) {
	needGit(t)
	f := newFixture(t, "ws")
	f.p.Locker = Binds{}
	f.repo("a")
	f.walked()
	if err := f.newVM(false); err != nil {
		t.Fatal(err)
	}
	fixed, readOnly, err := f.p.Pins(t.Context(), f.vm)
	if err != nil || !slices.Equal(fixed, []string{"a/.git"}) || !slices.Contains(readOnly, "a/.git/config") {
		t.Errorf("pins %q %q %v", fixed, readOnly, err)
	}
	missing := f.p.PinsMissing(t.Context(), f.vm, []string{}, []string{})
	if !slices.Contains(missing, f.path("a/.git")) || !slices.Contains(missing, f.path("a/.git/config")) {
		t.Errorf("missing %q", missing)
	}
}

// A nested repo whose .git file names a git dir in the folder that no
// repo's protection covers is named; one whose git dir another nested
// repo's covers is left to it.
func TestNestedGitdirInside(t *testing.T) {
	needGit(t)
	f := newFixture(t, "ws")
	f.repo("a")
	mkdirs(t, f.path("b"), f.path("c"), f.path("store"))
	f.git(f.path("b"), "init", "-q", "--separate-git-dir="+f.path("store/b.git"))
	write(t, f.path("c/.git"), "gitdir: "+f.path("a/.git")+"\n")
	f.walked()
	if err := f.newVM(false); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.warns.Names(), []string{"git_nested_unprotected repos=b vm=" + f.vm.Name()}) {
		t.Errorf("warned %q", f.warns.Names())
	}
	if got := f.recordedRepos(); !slices.Equal(got, []string{"a", "b"}) || !f.locked(f.path("b/.git")) {
		t.Errorf("recorded %q, locks %q", got, f.flags)
	}
}

// BenchmarkNestedCheck is a check of a stopped VM whose folder holds 50
// repos, each with a commit, its walk included, with this host's own
// locker (chflags on macOS): go test ./internal/gitprotect -run '^$'
// -bench NestedCheck -benchtime 5x.
func BenchmarkNestedCheck(b *testing.B) {
	if _, err := exec.LookPath("git"); err != nil {
		b.Skip("no git")
	}
	root, err := filepath.EvalSymlinks(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	dir := filepath.Join(root, "ws")
	b.Cleanup(func() { _ = exec.Command("chflags", "-R", "nouchg", root).Run() }) //nolint:noctx,gosec // cleanup, of the benchmark's own folder
	for i := range 50 {
		r := filepath.Join(dir, "repo"+strconv.Itoa(i))
		for _, args := range [][]string{{"init", "-q", r}, {"-C", r, "-c", "user.email=a@b", "-c", "user.name=a", "commit", "-q", "--allow-empty", "-m", "x"}} {
			if out, err := stubtest.Git(b.Context(), args...).CombinedOutput(); err != nil {
				b.Fatalf("git %v: %v\n%s", args, err, out)
			}
		}
	}
	p := New(dir, root, HostLocker(false), &msg.Fake{})
	vm, err := state.New(paths.Dirs{Data: filepath.Join(root, "data")}, "devvm-ws-0123456789abcd")
	if err != nil {
		b.Fatal(err)
	}
	vm.Image, vm.Dir, vm.Backend = "base", dir, "fake"
	if err := vm.Create(b.Context(), nil); err != nil {
		b.Fatal(err)
	}
	if err := record(b.Context(), vm, func(g *InVM) { g.Mode = p.NewSection(false).Mode }); err != nil {
		b.Fatal(err)
	}
	found := hostsafety.Walk(dir, false)
	p.Nested = &found.Repos
	if err := p.NewVM(b.Context(), vm); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for b.Loop() {
		found := hostsafety.Walk(dir, false)
		p.Nested = &found.Repos
		if err := p.Check(b.Context(), vm, false); err != nil {
			b.Fatal(err)
		}
	}
}

// A path is in the deepest repo whose folder it's below, so a repo the
// watch skips, as it's new or gone, never hides one nested in it.
func TestRepoOf(t *testing.T) {
	roots := []string{"b", "b/c", "d"}
	for path, want := range map[string]string{
		"b/.git/config": "b", "b/c/.git/config": "b/c", ".git/config": "", "bc/.git/config": "", "d/x": "d",
	} {
		if got := repoOf(path, roots); got != want {
			t.Errorf("%s: %q, want %q", path, got, want)
		}
	}
}

// A repo's git dir: its .git folder, what its .git file names, where its
// .git symlink leads, or none.
func TestRepoGitDir(t *testing.T) {
	f := newFixture(t, "p")
	mkdirs(t, f.path("a/.git"), f.path("b"), f.path("c"), f.path("store/x"), f.path("none"))
	write(t, f.path("b/.git"), "gitdir: ../store/x\n")
	symlink(t, f.path("store/x"), f.path("c/.git"))
	for dir, want := range map[string]string{
		"a": f.path("a/.git"), "b": f.path("store/x"), "c": f.path("store/x"), "none": "",
	} {
		if got := repoGitDir(f.path(dir)); got != want {
			t.Errorf("%s: %q, want %q", dir, got, want)
		}
	}
}
