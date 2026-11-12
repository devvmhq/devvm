package hostsafety

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/stubtest"
)

func touch(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func mkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

func setIDFiles(dir string) []SetIDFile { return Walk(dir, true).SetID }

// gitDir makes dir a git dir, as IsGitDir has it.
func gitDir(t *testing.T, dir string) {
	t.Helper()
	mkdirs(t, filepath.Join(dir, "objects"), filepath.Join(dir, "refs"))
	touch(t, filepath.Join(dir, "HEAD"))
}

// The walk finds the folders nested in the folder, below its own, with
// anything named .git, at most RepoDepth folders down and MaxRepos of
// them, and counts the rest; never through a symlink, in a .git folder or
// in a package manager's folder, where a set-id walk counts them.
func TestWalkRepos(t *testing.T) {
	d := t.TempDir()
	gitDir(t, filepath.Join(d, ".git"))
	for _, r := range []string{"a", "a/inner", "b/c/d/e", "deep/1/2/3/4", "node_modules/x", "x/vendor/y"} {
		gitDir(t, filepath.Join(d, r, ".git"))
	}
	// A linked worktree's .git file, a .git symlink and a .git folder that
	// isn't a git dir yet count, but not a repo in .git.
	mkdirs(t, filepath.Join(d, "wt"), filepath.Join(d, "fake", ".git"), filepath.Join(d, ".git", "modules", "m"))
	touch(t, filepath.Join(d, "wt", ".git"))
	gitDir(t, filepath.Join(d, ".git", "modules", "m", ".git"))
	mkdirs(t, filepath.Join(d, "ln"))
	if err := os.Symlink(filepath.Join(d, "a", ".git"), filepath.Join(d, "ln", ".git")); err != nil {
		t.Fatal(err)
	}
	// A symlink to a repo isn't followed.
	if err := os.Symlink(filepath.Join(d, "a"), filepath.Join(d, "linked")); err != nil {
		t.Fatal(err)
	}
	// A folder shaped like a git dir, not a .git, is named as one.
	gitDir(t, filepath.Join(d, "shadow"))
	want := []string{"a", "a/inner", "b/c/d/e", "fake", "ln", "wt"}
	for _, setID := range []bool{false, true} {
		got := Walk(d, setID).Repos
		if !slices.Equal(got.Dirs, want) || got.More != 0 || !slices.Equal(got.Bare, []string{"shadow"}) {
			t.Errorf("setID %v: Repos = %+v, want %v", setID, got, want)
		}
		if skipped := map[bool]int{false: 0, true: 3}[setID]; got.Skipped != skipped {
			t.Errorf("setID %v: skipped %d, want %d", setID, got.Skipped, skipped)
		}
	}
	// Past MaxRepos, they're counted, the first of them named.
	for i := range MaxRepos + 2 {
		gitDir(t, filepath.Join(d, "many", strconv.Itoa(i), ".git"))
	}
	got := Walk(d, false).Repos
	if len(got.Dirs) != MaxRepos || got.More != len(want)+2 || len(got.Past) != min(got.More, MaxPast) {
		t.Errorf("past the bound: %d repos, %d more, %q", len(got.Dirs), got.More, got.Past)
	}
	if got := Walk(filepath.Join(d, "none"), true); got.Repos.Dirs != nil || got.SetID != nil {
		t.Errorf("no folder: %+v", got)
	}
}

// git takes for a git dir a folder with a HEAD and objects and refs, which
// may be files, there or in the folder its commondir names; a .GIT is a
// .git where names fold case.
func TestIsGitDir(t *testing.T) {
	d := t.TempDir()
	gitDir(t, filepath.Join(d, "plain"))
	mkdirs(t, filepath.Join(d, "files"), filepath.Join(d, "common"), filepath.Join(d, "none"))
	touch(t, filepath.Join(d, "files", "HEAD"), filepath.Join(d, "files", "objects"), filepath.Join(d, "files", "refs"))
	touch(t, filepath.Join(d, "common", "HEAD"))
	if err := os.WriteFile(filepath.Join(d, "common", "commondir"), []byte("../plain\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	touch(t, filepath.Join(d, "none", "HEAD"))
	for dir, want := range map[string]bool{"plain": true, "files": true, "common": true, "none": false} {
		if got := IsGitDir(filepath.Join(d, dir)); got != want {
			t.Errorf("%s: %v", dir, got)
		}
	}
	if IsGitName(".GIT") != platform.FoldsCase || !IsGitName(".git") || IsGitName(".gitx") {
		t.Error("IsGitName")
	}
}

func TestSetIDFiles(t *testing.T) {
	d := t.TempDir()
	mkdirs(t, filepath.Join(d, "sub"))
	tool := filepath.Join(d, "sub", "tool")
	touch(t, filepath.Join(d, "plain"), tool)
	if err := os.Chmod(tool, fs.ModeSetuid|0o755); err != nil {
		t.Fatal(err)
	}
	if got := setIDFiles(d); !slices.Equal(got, []SetIDFile{{Path: tool, Root: os.Getuid() == 0}}) {
		t.Errorf("SetIDFiles = %v, want %s", got, tool)
	}
	if err := os.Chmod(tool, fs.ModeSetgid|0o755); err != nil {
		t.Fatal(err)
	}
	if got := setIDFiles(d); len(got) != 1 {
		t.Errorf("set-group-id: SetIDFiles = %v", got)
	}
	if err := os.Chmod(tool, 0o755); err != nil { //nolint:gosec // the test's own file
		t.Fatal(err)
	}
	if got := setIDFiles(d); got != nil {
		t.Errorf("none: SetIDFiles = %v", got)
	}
	// None in a .git folder, the project's or a submodule's, counts.
	for _, g := range []string{filepath.Join(d, ".git", "hooks"), filepath.Join(d, "sub", ".git")} {
		mkdirs(t, g)
		p := filepath.Join(g, "hook")
		touch(t, p)
		if err := os.Chmod(p, fs.ModeSetuid|0o700); err != nil {
			t.Fatal(err)
		}
	}
	if got := setIDFiles(d); got != nil {
		t.Errorf("in .git: SetIDFiles = %v", got)
	}
	// At most five.
	for i := range 7 {
		p := filepath.Join(d, "s"+string(rune('a'+i)))
		touch(t, p)
		if err := os.Chmod(p, fs.ModeSetuid|0o700); err != nil {
			t.Fatal(err)
		}
	}
	if got := setIDFiles(d); len(got) != setIDMax {
		t.Errorf("SetIDFiles = %d files, want %d", len(got), setIDMax)
	}
	if got := setIDFiles(filepath.Join(d, "none")); got != nil {
		t.Errorf("no folder: SetIDFiles = %v", got)
	}
}

// Root's set-user-id files and root's group's set-group-id ones run as
// root; any other's, or root's without that bit, don't.
func TestRootSetID(t *testing.T) {
	for _, c := range []struct {
		name     string
		m        fs.FileMode
		uid, gid uint32
		want     bool
	}{
		{"set-user-id root's", fs.ModeSetuid | 0o755, 0, 1000, true},
		{"set-group-id root's group", fs.ModeSetgid | 0o755, 1000, 0, true},
		{"set-user-id the user's", fs.ModeSetuid | 0o755, 1000, 1000, false},
		{"set-group-id root's, the user's group", fs.ModeSetgid | 0o755, 0, 1000, false},
		{"set-user-id root's group", fs.ModeSetuid | 0o755, 1000, 0, false},
	} {
		if got := rootSetID(c.m, c.uid, c.gid); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
}

// BareExplicit reads safe.bareRepository from your global config.
func TestBareExplicit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	if BareExplicit(t.Context()) {
		t.Skip("this machine's system config sets it")
	}
	// Your own global file, as GIT_CONFIG_GLOBAL names it.
	own := filepath.Join(home, "own.gitconfig")
	if err := os.WriteFile(own, []byte("[safe]\n\tbareRepository = explicit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", own)
	if !BareExplicit(t.Context()) {
		t.Error("GIT_CONFIG_GLOBAL's not read")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", "")
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[safe]\n\tbareRepository = explicit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !BareExplicit(t.Context()) {
		t.Error("explicit not read")
	}
}

func TestSSHRemote(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	d := t.TempDir()
	if SSHRemote(t.Context(), d) {
		t.Error("no repo: SSHRemote")
	}
	git := func(args ...string) {
		t.Helper()
		if out, err := stubtest.Git(t.Context(), append([]string{"-C", d}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("remote", "add", "origin", "https://example.com/r")
	for _, tc := range []struct {
		url  string
		want bool
	}{
		{"git@github.com:o/r.git", true},
		{"ssh://git@example.com/r", true},
		{"https://github.com/o/r", false},
	} {
		git("remote", "set-url", "origin", tc.url)
		if got := SSHRemote(t.Context(), d); got != tc.want {
			t.Errorf("SSHRemote with %s = %v", tc.url, got)
		}
	}
}
