package vm

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/state"
)

// A submodule whose copy fails once the VM may have it (here, the VM
// refuses its apply, after its repo was made) is named and its record
// stays, not copied: rm still checks the work the VM has in it. One with
// a record from an earlier launch that then fails its checks keeps the
// record too.
func TestUpCloneSubmoduleFailedKeepsRecord(t *testing.T) {
	e, f, vmfs := cloneEnv(t)
	submoduleRepos(t, t.TempDir(), e.Dir)
	exec := f.ExecFunc
	f.ExecFunc = func(vm string, argv []string, o backend.ExecOpts) error {
		if len(argv) > 4 && argv[0] == "sh" && argv[2] == seedApply && strings.HasSuffix(argv[4], string(filepath.Separator)+"lib") {
			return errors.New("refused")
		}
		return exec(vm, argv, o)
	}
	var warned msg.Recorder
	fake(e).OnWarn = warned.Add
	st := existingVM(t, e, Options{Network: "open", Clone: true})
	vm := st.Name()
	lib := func() (cloneRepo, bool) {
		t.Helper()
		st, err := state.ReadVM(e.Dirs, vm)
		if err != nil {
			t.Fatal(err)
		}
		all := sectionOf[cloneInVM](st, cloneSection).allRepos()
		i := slices.IndexFunc(all, func(r cloneRepo) bool { return r.Path == "lib" })
		if i < 0 {
			return cloneRepo{}, false
		}
		return all[i], true
	}
	if r, ok := lib(); !ok || r.Seeded || r.Remote != "devvm" || !slices.Contains(warned.Names(), "clone_submodule_kept path=lib") {
		t.Errorf("lib %+v, %v, warned %q", r, ok, warned.Names())
	}
	// A launch before the parent's copy is done, lib now failing its
	// checks: its earlier record stays.
	if err := update(t.Context(), st, cloneSection, func(c *cloneInVM) error { c.Made.Seeded = false; return nil }); err != nil {
		t.Fatal(err)
	}
	writeIn(t, e.Dir, filepath.Join(".git", "modules", "lib", "info", "grafts"), "")
	if err := (Launch{e}).Up(t.Context(), Options{Clone: true}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if r, ok := lib(); !ok || r.Seeded {
		t.Errorf("after a failed check: lib %+v, %v", r, ok)
	}
	if err := os.Remove(filepath.Join(e.Dir, ".git", "modules", "lib", "info", "grafts")); err != nil {
		t.Fatal(err)
	}
	// Work in what the VM has of lib: rm checks it.
	clone := filepath.Join(vmfs+e.Dir, "lib")
	gitIn(t, clone, "commit", "-q", "--allow-empty", "-m", "vm-work")
	asked := answers(t, e, false)
	if err := (Lifecycle{e}).Rm(t.Context(), RmOptions{Force: true}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if len(*asked) == 0 || (*asked)[0] != "fetch_before_delete_repos count=1" {
		t.Errorf("asked %q", *asked)
	}
}

// submoduleRepos makes lib, with deep as a submodule of its own, a
// submodule of the repo at dir, both initialised, in folders below root.
func submoduleRepos(t *testing.T, root, dir string) {
	t.Helper()
	for _, n := range []string{"deep", "lib"} {
		gitIn(t, root, "init", "-q", "-b", "main", n)
		gitIn(t, filepath.Join(root, n), "commit", "-q", "--allow-empty", "-m", n)
	}
	file := []string{"-c", "protocol.file.allow=always"}
	gitIn(t, filepath.Join(root, "lib"), append(file, "submodule", "add", "-q", filepath.Join(root, "deep"), "deep")...)
	gitIn(t, filepath.Join(root, "lib"), "commit", "-q", "-m", "deep")
	gitIn(t, dir, append(file, "submodule", "add", "-q", filepath.Join(root, "lib"), "lib")...)
	gitIn(t, dir, "commit", "-q", "-m", "lib")
	gitIn(t, dir, append(file, "submodule", "update", "-q", "--init", "--recursive")...)
}

// A --clone VM of a repo with a submodule that has one of its own: each
// gets its clone, at the commit it has here, recorded in the clone
// section with its remote; devvm clone fetch fetches each one's
// branches; rm's check names the nested one's work not fetched, fetches
// it into a saved branch there once told to, and removes each one's
// remote with the VM.
func TestUpCloneSubmodules(t *testing.T) {
	e, f, vmfs := cloneEnv(t)
	submoduleRepos(t, t.TempDir(), e.Dir)
	st := existingVM(t, e, Options{Network: "open", Clone: true})
	vm, clone := st.Name(), vmfs+e.Dir
	if got, want := gitIn(t, clone, "submodule", "status", "--recursive"), gitIn(t, e.Dir, "submodule", "status", "--recursive"); got != want {
		t.Errorf("the VM's submodules:\n%s\nhere:\n%s", got, want)
	}
	c := sectionOf[cloneInVM](st, cloneSection)
	if got := c.allRepos(); len(got) != 3 || got[1].Path != "lib" || got[2].Path != filepath.Join("lib", "deep") || !got[2].Seeded || got[2].Remote != "devvm" {
		t.Errorf("recorded: %+v", got)
	}
	if got, err := (Clone{e}).Fetch(t.Context()); err != nil || len(got.Repos) != 3 || got.Checked != 3 {
		t.Errorf("first fetch %+v, %v", got, msg.NamesOf(err))
	}
	gitIn(t, filepath.Join(clone, "lib"), "checkout", "-q", "-b", "vm-lib")
	gitIn(t, filepath.Join(clone, "lib"), "commit", "-q", "--allow-empty", "-m", "vm-lib")
	got, err := (Clone{e}).Fetch(t.Context())
	if err != nil || len(got.Repos) != 1 || got.Repos[0].Path != "lib" || !slices.Equal(got.Repos[0].Branches, []string{"vm-lib"}) {
		t.Errorf("fetched %+v, %v", got, msg.NamesOf(err))
	}
	deep := filepath.Join(clone, "lib", "deep")
	gitIn(t, deep, "checkout", "-q", "-b", "vm-deep")
	gitIn(t, deep, "commit", "-q", "--allow-empty", "-m", "vm-deep")
	asked := answers(t, e, true)
	if err := (Lifecycle{e}).Rm(t.Context(), RmOptions{Force: true}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	// The VM's lib moved on, which its repo's status shows as a change.
	if _, ok := f.Instances[vm]; ok || !slices.Equal(*asked, []string{"fetch_before_delete_repos count=1", "delete_anyway"}) {
		t.Errorf("deleted %v, asked %q", !ok, *asked)
	}
	if got := gitIn(t, filepath.Join(e.Dir, "lib", "deep"), "log", "--format=%s", "-1", "refs/heads/saved/devvm/vm-deep"); got != "vm-deep" {
		t.Errorf("saved %q", got)
	}
	for _, d := range []string{"lib", filepath.Join("lib", "deep")} {
		if out, _ := hostGit(t.Context(), filepath.Join(e.Dir, d), "remote"); out != "origin" {
			t.Errorf("%s's remotes %q", d, out)
		}
	}
}

// A clone's submodules, recorded below their repo, nested ones too, are
// gone over after it with their paths in the folder (allRepos), changed
// by those paths (setRepo), added once (addSubmodule), and written below
// made for the folder's own repo; one whose path isn't local is passed
// over. A clone with none writes nothing new.
func TestCloneSubmodules(t *testing.T) {
	var c cloneInVM
	if err := json.Unmarshal([]byte(`{"on":true,"made":{"remote":"devvm","seeded":true}}`), &c); err != nil {
		t.Fatal(err)
	}
	if got := c.allRepos(); !reflect.DeepEqual(got, []cloneRepo{{Path: ".", Remote: "devvm", Seeded: true}}) {
		t.Errorf("none: %+v", got)
	}
	for _, a := range [][3]string{{".", "lib", "devvm"}, {".", "lib", "devvm-2"}, {"lib", "deep", "devvm"}, {".", "gone", "devvm"}} {
		if err := c.addSubmodule(a[0], a[1], a[2]); err != nil { // lib once, its remote changed
			t.Fatal(err)
		}
	}
	if err := c.addSubmodule("nope", "x", "devvm"); !errors.Is(err, errNoParent) {
		t.Errorf("no parent: %v", err)
	}
	c.dropSubmodule(".", "gone")
	c.setRepo("lib/deep", func(r *cloneRepo) { r.Seeded = true })
	c.setRepo("lib", func(r *cloneRepo) { r.Seeded = true })
	if c.setRepo("nope", func(r *cloneRepo) { r.Seeded = true }) {
		t.Error("set a repo it hasn't")
	}
	want := []cloneRepo{
		{Path: ".", Remote: "devvm", Seeded: true},
		{Path: "lib", Remote: "devvm-2", Seeded: true, submodule: true},
		{Path: "lib/deep", Remote: "devvm", Seeded: true, submodule: true},
	}
	if got := c.allRepos(); !reflect.DeepEqual(got, want) {
		t.Errorf("own: %+v", got)
	}
	b, err := json.Marshal(c)
	if err != nil || string(b) != `{"on":true,"made":{"remote":"devvm","seeded":true,"submodules":[{"path":"lib","remote":"devvm-2","seeded":true,"submodules":[{"path":"deep","remote":"devvm","seeded":true}]}]}}` {
		t.Errorf("written: %s, %v", b, err)
	}
	ws := cloneInVM{On: true, Workspace: true, Repos: []cloneRepo{{Path: "a"}, {Path: "b", Submodules: []cloneRepo{{Path: "../out"}}}}}
	if err := ws.addSubmodule("a", "x", "devvm"); err != nil {
		t.Fatal(err)
	}
	ws.setRepo("a/x", func(r *cloneRepo) { r.Seeded = true })
	got := ws.allRepos()
	if !slices.ContainsFunc(got, func(r cloneRepo) bool { return r.Path == "a/x" && r.Seeded }) || len(got) != 3 {
		t.Errorf("workspace: %+v", got)
	}
	// clone add: the folder's own repo, its submodules kept.
	addRepo(&c, "other")
	if got := c.allRepos(); len(got) != 4 || got[1].Path != "lib" || c.Made.Submodules != nil {
		t.Errorf("added: %+v, made %+v", got, c.Made)
	}
}

// keptPaths are kept's paths.
func keptPaths(kept []keptSubmodule) []string {
	var out []string
	for _, k := range kept {
		out = append(out, k.path)
	}
	return out
}

// The repo's initialised submodules, from its own config and git dir:
// nested ones are their own repo's; one deinitialised isn't, silently;
// one whose core.worktree leads where the index has no submodule, and one
// whose git dir isn't its own, are kept, with why; a linked worktree's
// are kept, each named. A folder of repos' walk passes over a submodule
// of a repo in the folder, but not one whose repo is outside it.
func TestHostSubmodules(t *testing.T) {
	gitEnv(t)
	root := t.TempDir()
	for _, n := range []string{"deep", "lib", "other"} {
		gitIn(t, root, "init", "-q", "-b", "main", n)
		gitIn(t, filepath.Join(root, n), "commit", "-q", "--allow-empty", "-m", n)
	}
	file := []string{"-c", "protocol.file.allow=always"}
	gitIn(t, filepath.Join(root, "lib"), append(file, "submodule", "add", "-q", filepath.Join(root, "deep"), "deep")...)
	gitIn(t, filepath.Join(root, "lib"), "commit", "-q", "-m", "deep")
	app := filepath.Join(root, "app")
	gitIn(t, root, "init", "-q", "-b", "main", "app")
	gitIn(t, app, append(file, "submodule", "add", "-q", filepath.Join(root, "lib"), "lib")...)
	gitIn(t, app, append(file, "submodule", "add", "-q", filepath.Join(root, "other"), "other")...)
	gitIn(t, app, "commit", "-q", "-m", "subs")
	gitIn(t, app, append(file, "submodule", "update", "-q", "--init", "--recursive")...)
	if got, kept, err := hostSubmodules(t.Context(), app); err != nil || !slices.Equal(got, []string{"lib", "other"}) || kept != nil {
		t.Errorf("app: %q, kept %v, %v", got, kept, err)
	}
	if got, _, err := hostSubmodules(t.Context(), filepath.Join(app, "lib")); err != nil || !slices.Equal(got, []string{"deep"}) {
		t.Errorf("lib: %q, %v", got, err)
	}
	if !isSubmodule(t.Context(), filepath.Join(app, "lib"), root) || isSubmodule(t.Context(), filepath.Join(app, "lib", "deep"), filepath.Join(app, "lib", "x")) {
		t.Error("isSubmodule")
	}
	// A linked worktree's, whose git dirs are the common dir's: kept.
	wt := filepath.Join(root, "wt")
	gitIn(t, app, "worktree", "add", "-q", "-b", "wt", wt)
	gitIn(t, wt, append(file, "submodule", "update", "-q", "--init")...)
	if got, kept, err := hostSubmodules(t.Context(), wt); err != nil || got != nil || !slices.Equal(keptPaths(kept), []string{"lib", "other"}) || !errors.Is(kept[0].why, errLinkedSubmodule) {
		t.Errorf("a linked worktree's: %q, kept %v, %v", got, kept, err)
	}
	gitIn(t, app, "submodule", "deinit", "-q", "other")
	if got, kept, _ := hostSubmodules(t.Context(), app); !slices.Equal(got, []string{"lib"}) || kept != nil {
		t.Errorf("deinitialised: %q, kept %v", got, kept)
	}
	// lib's core.worktree pointed at a folder the index has no
	// submodule at.
	mod := filepath.Join(app, ".git", "modules", "lib")
	if err := os.Mkdir(filepath.Join(app, "plain"), 0o700); err != nil {
		t.Fatal(err)
	}
	gitIn(t, app, "config", "--file", filepath.Join(mod, "config"), "core.worktree", "../../../plain")
	if got, kept, _ := hostSubmodules(t.Context(), app); len(got) != 0 || !slices.Equal(keptPaths(kept), []string{"lib"}) || !errors.Is(kept[0].why, errNotSubmodule) {
		t.Errorf("worktree elsewhere: %q, kept %v", got, kept)
	}
	gitIn(t, app, "config", "--file", filepath.Join(mod, "config"), "core.worktree", "../../../lib")
	// lib's .git pointed at another repo's git dir.
	writeIn(t, app, "lib/.git", "gitdir: "+filepath.Join(root, "other", ".git")+"\n")
	if got, kept, _ := hostSubmodules(t.Context(), app); len(got) != 0 || !slices.Equal(keptPaths(kept), []string{"lib"}) {
		t.Errorf("not its own: %q, kept %v", got, kept)
	}
}

// One submodule that can't be copied (here, a grafts file, which
// --clone's checks refuse) stays on this machine, named, out of the record, with its own
// submodules; the repo's copy goes on, another submodule's too. Once
// deinitialised here, a copied one is named by rm's check and asked
// about, never refused; and devvm clone add of it says it's the clone's
// already.
func TestUpCloneSubmoduleKept(t *testing.T) {
	e, f, vmfs := cloneEnv(t)
	root := t.TempDir()
	submoduleRepos(t, root, e.Dir)
	gitIn(t, root, "init", "-q", "-b", "main", "other")
	gitIn(t, filepath.Join(root, "other"), "commit", "-q", "--allow-empty", "-m", "other")
	file := []string{"-c", "protocol.file.allow=always"}
	gitIn(t, e.Dir, append(file, "submodule", "add", "-q", filepath.Join(root, "other"), "other")...)
	gitIn(t, e.Dir, "commit", "-q", "-m", "other")
	writeIn(t, e.Dir, filepath.Join(".git", "modules", "lib", "info", "grafts"), "")
	var warned msg.Recorder
	fake(e).OnWarn = warned.Add
	st := existingVM(t, e, Options{Network: "open", Clone: true})
	clone := vmfs + e.Dir
	if !slices.ContainsFunc(warned.Names(), func(n string) bool { return n == "clone_submodule_kept path=lib" }) {
		t.Errorf("warned %q", warned.Names())
	}
	c := sectionOf[cloneInVM](st, cloneSection)
	if got := c.allRepos(); len(got) != 2 || got[1].Path != "other" || !got[1].Seeded || !c.Made.Seeded {
		t.Errorf("recorded %+v", got)
	}
	if _, err := os.Lstat(filepath.Join(clone, "lib", "deep", ".git")); err == nil {
		t.Error("the kept submodule's own submodule went")
	}
	gitIn(t, e.Dir, "submodule", "deinit", "-q", "-f", "other")
	if err := (Clone{e}).Add(t.Context(), "other"); err != nil {
		t.Errorf("clone add: %v", msg.NamesOf(err))
	}
	asked := answers(t, e, false)
	var said msg.Recorder
	fake(e).OnSay = said.Add
	if err := (Lifecycle{e}).Rm(t.Context(), RmOptions{Force: true}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if !slices.Contains(said.Names(), "clone_submodule_unchecked path=other") || !slices.Equal(*asked, []string{"delete_anyway"}) {
		t.Errorf("said %q, asked %q", said.Names(), *asked)
	}
	if _, ok := f.Instances[st.Name()]; !ok {
		t.Error("deleted")
	}
}

// A submodule's own folder isn't a repo --clone takes on its own: its
// .git leads into the parent's .git/modules, which a VM mounting the
// parent's folder, or for a linked worktree the main repo's, could swap
// under host git, so it's refused as a .git that points elsewhere
// (clone_git_elsewhere), its parent a linked worktree's too. As a
// submodule of a --clone repo, it's still copied.
func TestCloneSubmoduleFolderRefused(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitEnv(t)
	dir := filepath.Join(root, "app")
	gitIn(t, root, "init", "-q", "-b", "main", dir)
	gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "one")
	src := filepath.Join(root, "src")
	if err := os.Mkdir(src, 0o700); err != nil {
		t.Fatal(err)
	}
	submoduleRepos(t, src, dir)
	gitIn(t, dir, "worktree", "add", "-q", "-b", "wt", filepath.Join(root, "wt"))
	gitIn(t, filepath.Join(root, "wt"), "-c", "protocol.file.allow=always", "submodule", "update", "-q", "--init")
	for _, lib := range []string{filepath.Join(dir, "lib"), filepath.Join(root, "wt", "lib")} {
		if err := checkCloneRepo(t.Context(), lib); msg.IDOf(err) != "clone_git_elsewhere" {
			t.Errorf("%s: %v", lib, msg.NamesOf(err))
		}
	}
	if _, err := planSeedOf(t.Context(), "lib", filepath.Join(dir, "lib"), true); err != nil {
		t.Errorf("as a submodule: %v", err)
	}
}
