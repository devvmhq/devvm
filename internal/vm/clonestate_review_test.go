package vm

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// secretThenReset commits a .env in dir, then takes it back with git reset
// --hard, so ORIG_HEAD names the commit that holds it, which it returns.
func secretThenReset(t *testing.T, dir string) string {
	t.Helper()
	writeIn(t, dir, ".env", "SECRET=1\n")
	gitIn(t, dir, "add", "-f", ".env")
	gitIn(t, dir, "commit", "-q", "--no-verify", "-m", "oops")
	secret := gitIn(t, dir, "rev-parse", "HEAD")
	gitIn(t, dir, "reset", "-q", "--hard", "HEAD~")
	if got := gitIn(t, dir, "rev-parse", "ORIG_HEAD"); got != secret {
		t.Fatalf("ORIG_HEAD %q, not the reset commit", got)
	}
	return secret
}

// ORIG_HEAD never goes, even while an operation is in progress, as a
// reset made meanwhile points it at what was taken back: during a
// stopped rebase, and with a sequencer folder left from a cherry-pick of
// several commits.
func TestSeedNoOrigHeadInProgress(t *testing.T) {
	for _, c := range []struct {
		name  string
		start func(t *testing.T, dir string)
	}{
		{"rebase", func(t *testing.T, dir string) {
			t.Helper()
			gitIn(t, dir, "checkout", "-q", "topic")
			stopped(t, dir, "rebase", "main")
			// Resolved, so a commit can be made, the rebase still going on.
			writeIn(t, dir, "f", "resolved\n")
			gitIn(t, dir, "add", "f")
		}},
		{"sequencer", func(t *testing.T, dir string) {
			t.Helper()
			gitIn(t, dir, "checkout", "-q", "-b", "two", "topic")
			gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "second")
			gitIn(t, dir, "checkout", "-q", "main")
			stopped(t, dir, "cherry-pick", "topic", "two")
			writeIn(t, dir, "f", "resolved\n")
			gitIn(t, dir, "add", "f")
			gitIn(t, dir, "-c", "core.editor=true", "commit", "-q", "--no-edit")
			if _, err := os.Stat(filepath.Join(dir, ".git", "sequencer")); err != nil {
				t.Fatal("no sequencer left")
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := conflictRepo(t)
			c.start(t, dir)
			secret := secretThenReset(t, dir)
			vm := seedOK(t, dir)
			if _, err := plainGit(t, vm, "cat-file", "-e", secret); err == nil {
				t.Error("the reset commit is in the VM")
			}
			if out, err := plainGit(t, vm, "rev-parse", "-q", "--verify", "ORIG_HEAD"); err == nil {
				t.Errorf("ORIG_HEAD in the VM: %s", out)
			}
		})
	}
}

// git stash --all that took back an uncommitted .gitignore change, which
// ignored the .env it stashed: the entry stays, as the rules at stash
// time ignored it, though today's don't.
func TestStashAllIgnoreEdit(t *testing.T) {
	dir := conflictRepo(t)
	writeIn(t, dir, ".gitignore", "*.log\n")
	gitIn(t, dir, "add", ".gitignore")
	gitIn(t, dir, "commit", "-q", "-m", "ignore")
	writeIn(t, dir, ".gitignore", "*.log\n.env\n")
	writeIn(t, dir, ".env", "SECRET=1\n")
	gitIn(t, dir, "stash", "push", "-q", "--all", "-m", "all")
	sent, all, _, err := stashEntries(t.Context(), dir, stashMax)
	if err != nil {
		t.Fatal(err)
	}
	if len(sent) != 0 || !slices.Equal(all, []string{"stash@{0}"}) {
		t.Errorf("sent %q, all %q", sent, all)
	}
}

// The clone e2e's repo (tests/e2e's inProgressRepo), step for step: a
// reset whose ORIG_HEAD alone reached a commit, a stash of a plain entry
// and a git stash --all one, then a rebase of topic onto main, with
// --autostash, stopped on f's conflict. In the VM, git rebase --continue
// finishes it and brings the autostash back; the stash is the repo's but
// for the --all entry.
func TestSeedE2ERebase(t *testing.T) {
	gitEnv(t)
	dir := filepath.Join(t.TempDir(), "host")
	gitIn(t, filepath.Dir(dir), "init", "-q", "-b", "main", dir)
	writeIn(t, dir, "f", "base\n")
	writeIn(t, dir, "g", "g\n")
	writeIn(t, dir, ".gitignore", "*.log\n")
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-q", "-m", "base")
	gitIn(t, dir, "checkout", "-q", "-b", "topic")
	writeIn(t, dir, "f", "topic\n")
	gitIn(t, dir, "commit", "-q", "-am", "topic")
	gitIn(t, dir, "checkout", "-q", "main")
	writeIn(t, dir, "f", "main\n")
	gitIn(t, dir, "commit", "-q", "-am", "main")
	gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "undone")
	gitIn(t, dir, "reset", "-q", "--hard", "HEAD~")
	gitIn(t, dir, "checkout", "-q", "topic")
	writeIn(t, dir, "g", "one\n")
	gitIn(t, dir, "stash", "push", "-q", "-m", "one")
	writeIn(t, dir, "x.log", "secret\n")
	writeIn(t, dir, "g", "all\n")
	gitIn(t, dir, "stash", "push", "-q", "--all", "-m", "all")
	writeIn(t, dir, "g", "wip\n")
	stopped(t, dir, "rebase", "--autostash", "main")
	vm := seedOK(t, dir)
	if got := gitIn(t, vm, "stash", "list", "--format=%gs"); got != "On topic: one" {
		t.Errorf("the VM's stash:\n%s", got)
	}
	writeIn(t, vm, "f", "resolved\n")
	gitIn(t, vm, "add", "f")
	if out, err := plainGit(t, vm, "-c", "core.editor=true", "rebase", "--continue"); err != nil {
		t.Fatalf("rebase --continue: %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(filepath.Join(vm, "g")); string(b) != "wip\n" { //nolint:gosec // the test's own files
		t.Errorf("the autostash: g = %q", b)
	}
}

// A branch or tag named like a pseudo-ref is no operation in progress.
func TestPseudoRefNotBranch(t *testing.T) {
	dir := conflictRepo(t)
	gitIn(t, dir, "branch", "CHERRY_PICK_HEAD")
	gitIn(t, dir, "tag", "REVERT_HEAD")
	s, err := readRepoState(t.Context(), dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.pseudo) != 0 || slices.ContainsFunc(s.files, func(e tarEntry) bool { return e.name == "state/MERGE_MSG" }) {
		t.Errorf("pseudo %q, files %d", s.pseudo, len(s.files))
	}
}

// A copied change to a file that info/attributes converts (eol=crlf)
// isn't the VM's work while the VM leaves it.
func TestSeededChangesEOL(t *testing.T) {
	dir := safetyRepo(t, map[string]string{"a": "a\n", "b": "b\n"})
	writeIn(t, dir, ".git/info/attributes", "* text eol=crlf\n")
	writeIn(t, dir, "a", "staged\r\n")
	gitIn(t, dir, "add", "a")
	writeIn(t, dir, "b", "changed\r\n")
	vm := seedOK(t, dir)
	if got := vmChanges(t, vm); got != "" {
		t.Errorf("the copied changes counted as the VM's:\n%s", got)
	}
}

// The ids a rebase keeps in squash-onto and update-refs go too.
func TestStateIDsRebase(t *testing.T) {
	a, b, c := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	files := []tarEntry{
		{"state/rebase-merge/squash-onto", []byte(a + "\n")},
		{"state/rebase-merge/update-refs", []byte("refs/heads/x\n" + b + "\n" + c + "\n")},
	}
	if got := stateIDs(files, nil); !slices.Equal(got, []string{a, b, c}) {
		t.Errorf("stateIDs %q", got)
	}
}

// todoCommits are the commits a rebase's or a sequencer's todo list
// names, which the VM may lack.
func TestTodoCommits(t *testing.T) {
	a := strings.Repeat("a", 40)
	files := []tarEntry{
		{"state/rebase-merge/git-rebase-todo", []byte("pick 1234567 one\n# pick " + a + " no\nexec make\nfixup -C " + a + " two\n\n")},
		{"state/sequencer/todo", []byte("revert abcdef0 three\n")},
		{"state/MERGE_MSG", []byte("pick " + a + "\n")},
	}
	if got := todoCommits(files); !slices.Equal(got, []string{"1234567", a, "abcdef0"}) {
		t.Errorf("todoCommits %q", got)
	}
}

// devvm clone fetch saves only what ownWork prints, a detached HEAD and
// stash entries: no other line the VM prints makes a saved branch.
func TestOwnLines(t *testing.T) {
	a := strings.Repeat("a", 40)
	in := a + " HEAD\n" + a + " stash@{3}\n" + a + " refs/heads/evil\n" + a + " stash@{x}\n" + a + " ../../x\n"
	if got := string(ownLines([]byte(in))); got != a+" HEAD\n"+a+" stash@{3}\n" {
		t.Errorf("ownLines %q", got)
	}
}

// Stash entries past stashMax are named, and one stored twice is stored
// twice.
func TestStashEntriesMax(t *testing.T) {
	dir := conflictRepo(t)
	for i := range 3 {
		writeIn(t, dir, "f", strings.Repeat("x", i+1)+"\n")
		gitIn(t, dir, "stash", "push", "-q", "-m", "s")
	}
	sent, _, over, err := stashEntries(t.Context(), dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(sent) != 2 || !slices.Equal(over, []string{"stash@{2}"}) {
		t.Errorf("sent %d, left %q", len(sent), over)
	}
	// Not stash@{0}: storing the top again changes nothing.
	sha := gitIn(t, dir, "rev-parse", "stash@{1}")
	gitIn(t, dir, "stash", "store", "-m", "again", sha)
	vm := seedOK(t, dir)
	if got := gitIn(t, vm, "stash", "list", "--format=%H"); strings.Count(got, sha) != 2 {
		t.Errorf("the VM's stash:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(vm, ".git", seedStashRecord)); err != nil {
		t.Error(err)
	}
}
