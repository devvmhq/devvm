package vm

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/state"
)

// A repo's default branch: origin/HEAD's, else main, else master, the
// first it has, local first, else origin's; none without any.
func TestDefaultBranch(t *testing.T) {
	for _, c := range []struct {
		head       string
		refs       []string
		name, from string
	}{
		{"refs/remotes/origin/trunk", []string{"refs/heads/main", "refs/heads/trunk"}, "trunk", "refs/heads/trunk"},
		{"refs/remotes/origin/trunk", []string{"refs/heads/main", "refs/remotes/origin/trunk"}, "trunk", "refs/remotes/origin/trunk"},
		{"", []string{"refs/heads/master", "refs/heads/main"}, "main", "refs/heads/main"},
		{"", []string{"refs/heads/master"}, "master", "refs/heads/master"},
		{"", []string{"refs/remotes/origin/main", "refs/heads/master"}, "main", "refs/remotes/origin/main"},
		{"refs/remotes/origin/HEAD", []string{"refs/heads/dev"}, "", ""},
		{"", []string{"refs/heads/dev"}, "", ""},
	} {
		if name, from := defaultBranch(c.head, c.refs); name != c.name || from != c.from {
			t.Errorf("defaultBranch(%q, %q) = %q, %q; want %q, %q", c.head, c.refs, name, from, c.name, c.from)
		}
	}
}

// A folder is a workspace clone's when a repo below it can be copied; its
// own repo alone, or repos that can't be, aren't one.
func TestIsWorkspace(t *testing.T) {
	bad := msg.WorkspaceReasonNoCommits()
	for _, c := range []struct {
		repos []wsRepo
		want  bool
	}{
		{nil, false},
		{[]wsRepo{{Path: "."}}, false},
		{[]wsRepo{{Path: "."}, {Path: "sub", Reason: bad}}, false},
		{[]wsRepo{{Path: "a", Reason: bad}, {Path: "b"}}, true},
		{[]wsRepo{{Path: "."}, {Path: "b"}}, true},
	} {
		if got := isWorkspace(c.repos); got != c.want {
			t.Errorf("isWorkspace(%+v) = %v", c.repos, got)
		}
	}
}

// --repos: all takes those --clone can copy, naming the others; none
// takes none; a list takes those it names, in the folder's order, naming
// those it can't copy, and refuses a path that's no repo found.
func TestChooseRepos(t *testing.T) {
	e := &Env{}
	got := printed(t, e, false)
	c := clones{e}
	found := []wsRepo{{Path: "."}, {Path: "backend"}, {Path: "empty", Reason: msg.WorkspaceReasonNoCommits()}, {Path: "frontend"}}
	for _, tc := range []struct {
		flag, want, said string
	}{
		{ReposAll, ". backend frontend", `workspace_skipped paths="empty"`},
		{ReposNone, "", ""},
		{"frontend/,./backend, empty", "backend frontend", `workspace_skipped paths="empty"`},
		{".", ".", ""},
	} {
		*got = nil
		chosen, err := c.chooseRepos("/ws", found, tc.flag)
		if err != nil || strings.Join(chosen, " ") != tc.want || strings.Join(*got, "\n") != tc.said {
			t.Errorf("--repos %s: %q, %v, said %q", tc.flag, chosen, err, *got)
		}
	}
	if _, err := c.chooseRepos("/ws", found, "frontend,nope"); msg.NamesOf(err) != "workspace_repo_unknown dir=/ws path=nope" {
		t.Errorf("unknown: %s", msg.NamesOf(err))
	}
}

// Without --repos, the picker: those --clone can copy ticked, the others
// fixed under their heading, with why; where it can't show, the question
// is refused naming --repos.
func TestPickRepos(t *testing.T) {
	e := &Env{}
	_ = printed(t, e, false)
	c := clones{e}
	found := []wsRepo{{Path: "a"}, {Path: "c"}, {Path: "b", Reason: msg.WorkspaceReasonElsewhere()}}
	if _, err := c.chooseRepos("/ws", found, ""); msg.NamesOf(err) != "no_input answer=--repos" {
		t.Errorf("no picker: %s", msg.NamesOf(err))
	}
	fake(e).Picks = true
	var shown []PickPart
	e.PickMany = func(_ msg.M, parts []PickPart) ([]PickPart, error) {
		shown = parts
		out := slices.Clone(parts)
		out[0].Rows = slices.Clone(parts[0].Rows)
		out[0].Rows[0].On = false
		return out, nil
	}
	chosen, err := c.chooseRepos("/ws", found, "")
	if err != nil || !slices.Equal(chosen, []string{"c"}) {
		t.Fatalf("chose %q, %v", chosen, err)
	}
	if len(shown) != 2 || len(shown[0].Rows) != 2 || !shown[0].Rows[0].On || !shown[0].Rows[1].On ||
		shown[1].Heading.ID() != "workspace_pick_cannot" || !shown[1].Rows[0].Fixed || shown[1].Rows[0].Desc.ID() != "workspace_reason_elsewhere" {
		t.Errorf("shown %+v", shown)
	}
	e.PickMany = func(msg.M, []PickPart) ([]PickPart, error) { return nil, errors.New("gone") }
	if _, err := c.chooseRepos("/ws", found, ""); err == nil {
		t.Error("a picker that failed: no error")
	}
}

// The repos of a folder: its own first, then those the walk finds, each
// with why --clone can't copy it; and whether --clone takes the folder
// as a workspace, its own repo alone, or neither.
func TestFindRepos(t *testing.T) {
	gitEnv(t)
	e, _, _ := upEnv(t, allNative())
	ws := e.Dir
	for _, r := range []string{"a", "deep/er/b", "empty", "node_modules/x", "line\nbreak"} {
		gitIn(t, ws, "init", "-q", "-b", "main", r)
	}
	for _, r := range []string{"a", "deep/er/b", "node_modules/x", "line\nbreak"} {
		gitIn(t, filepath.Join(ws, r), "commit", "-q", "--allow-empty", "-m", "one")
	}
	gitIn(t, filepath.Join(ws, "a"), "checkout", "-q", "--detach")
	gitIn(t, filepath.Join(ws, "a"), "branch", "-q", "-m", "main", "dev")
	if err := os.MkdirAll(filepath.Join(ws, "not"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "not", ".git"), []byte("gitdir: /nowhere\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A linked worktree of a repo in the folder: its repo's, not one of
	// its own.
	gitIn(t, filepath.Join(ws, "deep/er/b"), "worktree", "add", "-q", "-b", "wt", filepath.Join(ws, "wt"))
	found, more := clones{e}.findRepos(t.Context(), ws, "")
	var got []string
	for _, r := range found {
		got = append(got, r.Path+" "+r.Reason.ID())
	}
	// a's HEAD is detached with no branch: copied as it is.
	want := []string{"a ", "deep/er/b ", "empty workspace_reason_no_commits", "line\nbreak workspace_reason_name", "not workspace_reason_not_repo"}
	if !slices.Equal(got, want) || more != 0 {
		t.Errorf("found %q, %d more", got, more)
	}
	if _, err := (clones{e}).planClone(t.Context(), ws, "", ""); msg.NamesOf(err) != "no_input answer=--repos" {
		t.Errorf("a workspace without a terminal: %s", msg.NamesOf(err))
	}
	c, err := clones{e}.planClone(t.Context(), ws, ReposAll, "")
	if err != nil || !c.On || !c.Workspace || len(c.Repos) != 2 || c.Repos[0].Path != "a" || c.Repos[1].Path != "deep/er/b" {
		t.Errorf("all: %+v, %v", c, err)
	}
	// Its own repo alone: a clone of the folder's own, which needs a commit.
	own, _ := filepath.EvalSymlinks(t.TempDir())
	gitIn(t, own, "init", "-q", "-b", "main")
	if _, err := (clones{e}).planClone(t.Context(), own, "", ""); msg.NamesOf(err) != "clone_no_commits" {
		t.Errorf("its own repo, no commits: %s", msg.NamesOf(err))
	}
	gitIn(t, own, "commit", "-q", "--allow-empty", "-m", "one")
	if c, err := (clones{e}).planClone(t.Context(), own, ReposAll, ""); err != nil || c.Workspace {
		t.Errorf("its own repo: %+v, %v", c, err)
	}
	if _, err := (clones{e}).planClone(t.Context(), own, "x", ""); msg.NamesOf(err) != "repos_not_workspace dir="+own {
		t.Errorf("its own repo, --repos x: %s", msg.NamesOf(err))
	}
	// A repo with repos in it: its own clone, as ever, unless --repos.
	gitIn(t, own, "init", "-q", "-b", "main", "lib")
	gitIn(t, filepath.Join(own, "lib"), "commit", "-q", "--allow-empty", "-m", "one")
	if c, err := (clones{e}).planClone(t.Context(), own, "", ""); err != nil || c.Workspace {
		t.Errorf("a repo with a repo in it: %+v, %v", c, err)
	}
	if c, err := (clones{e}).planClone(t.Context(), own, ReposAll, ""); err != nil || !c.Workspace || len(c.Repos) != 2 {
		t.Errorf("a repo with a repo in it, --repos all: %+v, %v", c, err)
	}
	// A folder of repos inside another repo's working tree: refused.
	inner := filepath.Join(own, "ws")
	gitIn(t, own, "init", "-q", "-b", "main", "ws/a")
	gitIn(t, filepath.Join(inner, "a"), "commit", "-q", "--allow-empty", "-m", "one")
	if _, err := (clones{e}).planClone(t.Context(), inner, ReposAll, ""); msg.NamesOf(err) != "workspace_inside_repo dir="+inner+" top="+own {
		t.Errorf("inside another repo: %s", msg.NamesOf(err))
	}
}

// A repo's excludes in the VM: the clone's other repos, then the host
// repo's own info/exclude; none without the folder's own repo.
func TestRepoExcludes(t *testing.T) {
	gitEnv(t)
	ws := t.TempDir()
	gitIn(t, ws, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(ws, ".git", "info", "exclude"), []byte(".worktrees/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := cloneInVM{On: true, Workspace: true, Repos: []cloneRepo{{Path: "."}, {Path: "lib"}}}
	if got := string(repoExcludes(ws, c, []string{".", "lib"}, true)); got != "/lib/\n.worktrees/\n\n" {
		t.Errorf("got %q", got)
	}
	if got := string(repoExcludes(ws, c, []string{"tools"}, false)); got != "/tools/\n" {
		t.Errorf("added: %q", got)
	}
	// git's pattern characters escaped, so each line is its folder alone;
	// a path with a control character, which no line can hold, left out.
	if got := string(repoExcludes(ws, c, []string{`lib[2]`, `a*b?c\d`, "#x", "!y", "sp ", "new\nline"}, false)); got != "/lib\\[2]/\n/a\\*b\\?c\\\\d/\n/#x/\n/!y/\n/sp /\n" {
		t.Errorf("escaped: %q", got)
	}
	// As git reads them: each leaves out its folder, and only it.
	names := []string{`lib[2]`, `a*b?c\d`, "#x", "!y", "sp "}
	for _, n := range append(slices.Clone(names), "lib2", "aXbYc") {
		if err := os.MkdirAll(filepath.Join(ws, n), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ws, n, "f"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(ws, ".git", "info", "exclude"), repoExcludes(ws, c, names, false), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := gitIn(t, ws, "-c", "core.quotePath=false", "status", "--porcelain"); got != "?? aXbYc/\n?? lib2/" {
		t.Errorf("git status with those excluded:\n%s", got)
	}
	if got := repoExcludes(ws, cloneInVM{On: true, Workspace: true, Repos: []cloneRepo{{Path: "lib"}}}, []string{"lib"}, true); got != nil {
		t.Errorf("without its own: %q", got)
	}
}

// The folder's own files: everything outside every repo, links that stay
// in it as links, permissions but set-id bits; never a repo's files, a
// .git, a package folder, a link out, a file with another hard link, a
// bare repo, a special file or a file past the bounds, each of the last
// five counted.
func TestWorkspaceFiles(t *testing.T) {
	ws := t.TempDir()
	write := func(name string, mode os.FileMode, size int) {
		t.Helper()
		p := filepath.Join(ws, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, bytes.Repeat([]byte("x"), size), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
	}
	write("Makefile", 0o644, 4)
	write("bin/run", 0o755|os.ModeSetuid, 2)
	write("repo/.git/HEAD", 0o644, 1)
	write("repo/src.go", 0o644, 1)
	write("node_modules/x/i.js", 0o644, 1)
	write("big", 0o644, wsFileMax+1)
	write("mirror.git/HEAD", 0o644, 1)
	write("mirror.git/objects/x", 0o644, 1)
	write("mirror.git/refs/heads/main", 0o644, 1)
	write("linked", 0o644, 1)
	if err := os.Link(filepath.Join(ws, "linked"), filepath.Join(ws, "linked-too")); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"in": "bin/run", "up": "../x", "abs": "/etc/passwd", "bin/back": "../Makefile"} {
		if err := os.Symlink(target, filepath.Join(ws, name)); err != nil {
			t.Fatal(err)
		}
	}
	tarball, skipped, err := workspaceFiles(ws)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	r := tar.NewReader(bytes.NewReader(tarball))
	for {
		h, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got[h.Name] = h.FileInfo().Mode().String() + " " + h.Linkname
	}
	want := map[string]string{
		"Makefile": "-rw-r--r-- ", "bin/": "drwx------ ", "bin/run": "-rwxr-xr-x ",
		"in": "Lrwxrwxrwx bin/run", "bin/back": "Lrwxrwxrwx ../Makefile",
	}
	if len(got) != len(want) || skipped != 6 {
		t.Errorf("got %q, %d skipped", got, skipped)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	// Nothing to copy: no tar.
	empty := t.TempDir()
	if tarball, _, err := workspaceFiles(empty); err != nil || tarball != nil {
		t.Errorf("empty: %d bytes, %v", len(tarball), err)
	}
}

// rm's check of a workspace clone: each repo's and each worktree's changes
// under a heading naming it, at most cloneShown lines in all, a repo past
// them still counted; a repo with too many worktrees counts as changed.
func TestCloneChangesAcrossRepos(t *testing.T) {
	e := &Env{}
	_ = printed(t, e, false)
	var said msg.Recorder
	fake(e).OnSay = said.Add
	var out bytes.Buffer
	fake(e).Out = &out
	lines := func(n int) []byte { return bytes.Repeat([]byte("?? f\n"), n) }
	got := []repoInVM{
		{repo: cloneRepo{Path: "frontend"}, status: lines(15)},
		{repo: cloneRepo{Path: "backend"}, trees: []treeInVM{{path: "/w/.worktrees/x/backend", status: lines(10)}}},
		{repo: cloneRepo{Path: "docs"}, status: lines(1)},
		{repo: cloneRepo{Path: "lib"}, more: true},
		{repo: cloneRepo{Path: "moved", Seeded: true}, gone: true},
		{repo: cloneRepo{Path: "never-made"}, gone: true},
	}
	if !(cloneCheck{clones: clones{e}}).changes(true, got) {
		t.Error("not changed")
	}
	want := []string{"clone_repo_dirty repo=frontend", "clone_worktree_dirty path=/w/.worktrees/x/backend", "clone_worktrees_unchecked max=100", "clone_repo_gone repo=moved"}
	if !slices.Equal(said.Names(), want) || strings.Count(out.String(), "\n") != cloneShown {
		t.Errorf("said %q, %d lines", said.Names(), strings.Count(out.String(), "\n"))
	}
	said, out = msg.Recorder{}, bytes.Buffer{}
	fake(e).OnSay = said.Add
	if (cloneCheck{clones: clones{e}}).changes(true, []repoInVM{{repo: cloneRepo{Path: "a"}}}) || len(said.Names()) > 0 {
		t.Errorf("clean: said %q", said.Names())
	}
}

// devvm clone fetch's list by name: each branch with its repos in the
// clone's order, sorted by name.
func TestFetchedByBranch(t *testing.T) {
	f := Fetched{Repos: []FetchedRepo{
		{Path: "frontend", Branches: []string{"agent/x", "main"}},
		{Path: "backend", Branches: []string{"agent/x"}},
		{Path: ".", Branches: []string{"docs"}},
	}}
	got, _ := json.Marshal(f.ByBranch())
	want := `[{"Name":"agent/x","Repos":["frontend","backend"]},{"Name":"docs","Repos":["."]},{"Name":"main","Repos":["frontend"]}]`
	if string(got) != want {
		t.Errorf("got %s", got)
	}
}

// A clone of the folder's own repo, as 0.1.0 recorded it, is one repo at
// ".", with its remote and seed; a workspace clone's are its repos; a
// change to "." of the one goes into Made, as 0.1.0 reads it.
func TestCloneRepos(t *testing.T) {
	var old cloneInVM
	if err := json.Unmarshal([]byte(`{"on":true,"made":{"remote":"devvm-web","seeded":true}}`), &old); err != nil {
		t.Fatal(err)
	}
	if got := old.repos(); !reflect.DeepEqual(got, []cloneRepo{{Path: ".", Remote: "devvm-web", Seeded: true}}) {
		t.Errorf("0.1.0's: %+v", got)
	}
	old.setRepo(".", func(r *cloneRepo) { r.Remote = "devvm" })
	old.setRepo("x", func(r *cloneRepo) { r.Remote = "nope" })
	if b, _ := json.Marshal(old); string(b) != `{"on":true,"made":{"remote":"devvm","seeded":true}}` {
		t.Errorf("written: %s", b)
	}
	ws := cloneInVM{On: true, Workspace: true, Repos: []cloneRepo{{Path: "a"}, {Path: "b"}}}
	ws.setRepo("b", func(r *cloneRepo) { r.Seeded = true })
	if got := ws.repos(); len(got) != 2 || got[0].Seeded || !got[1].Seeded {
		t.Errorf("workspace: %+v", got)
	}
	// devvm clone add on a clone of the folder's own repo: a workspace
	// clone of it, then the new repo.
	addRepo(&old, "lib")
	if !old.Workspace || !reflect.DeepEqual(old.repos(), []cloneRepo{{Path: ".", Remote: "devvm", Seeded: true}, {Path: "lib"}}) || !old.Made.Seeded || !old.Made.Files {
		t.Errorf("added: %+v", old)
	}
}

// A release's clones of their folder's own repo (its fixture's clones)
// read today as one repo at "." each.
func TestFixtureClonesReadAsOneRepo(t *testing.T) {
	for _, c := range fixtureCopies(t) {
		t.Run(c.name(), func(t *testing.T) {
			vms := fixtureVMs(t, c)
			for _, slug := range c.clones {
				st := vms[slug]
				if st == nil {
					t.Fatalf("no VM %s", slug)
				}
				cl := sectionOf[cloneInVM](st, cloneSection)
				if r := cl.repos(); !cl.On || cl.Workspace || len(r) != 1 || r[0].Path != "." || r[0].Remote != cl.Made.Remote || r[0].Seeded != cl.Made.Seeded {
					t.Errorf("%s: %+v", slug, r)
				}
				// Not a workspace clone: the finder never opens it in a
				// repo below its folder.
				if coversRepo(st, st.Dir, filepath.Join(st.Dir, "sub")) {
					t.Errorf("%s covers a repo below its folder", slug)
				}
			}
		})
	}
}

// The finder uses a workspace clone in a folder of one of its repos, and
// not in another repo, nor for a clone of the folder's own repo.
func TestCoversRepo(t *testing.T) {
	ws := &state.VM{}
	if err := state.SetSection(ws, cloneSection, cloneInVM{On: true, Workspace: true, Repos: []cloneRepo{{Path: "frontend"}, {Path: "deep/lib"}}}); err != nil {
		t.Fatal(err)
	}
	own := &state.VM{}
	if err := state.SetSection(own, cloneSection, cloneInVM{On: true}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		st   *state.VM
		repo string
		want bool
	}{
		{ws, "/w/frontend", true},
		{ws, "/w/deep/lib", true},
		{ws, "/w/deep", false},
		{ws, "/w/backend", false},
		{ws, "/elsewhere/frontend", false},
		{own, "/w/frontend", false},
		{&state.VM{}, "/w/frontend", false},
	} {
		if got := coversRepo(c.st, "/w", c.repo); got != c.want {
			t.Errorf("coversRepo(%s) = %v", c.repo, got)
		}
	}
}
