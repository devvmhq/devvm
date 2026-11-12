package vm

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/stubtest"
)

// conflictRepo is a repo whose main and topic change f each its own way,
// from base, and g, tracked besides.
func conflictRepo(t *testing.T, opts ...string) string {
	t.Helper()
	gitEnv(t)
	dir := filepath.Join(t.TempDir(), "host")
	gitIn(t, filepath.Dir(dir), append(append([]string{"init", "-q", "-b", "main"}, opts...), dir)...)
	writeIn(t, dir, "f", "base\n")
	writeIn(t, dir, "g", "g\n")
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-q", "-m", "base")
	gitIn(t, dir, "checkout", "-q", "-b", "topic")
	writeIn(t, dir, "f", "topic\n")
	gitIn(t, dir, "commit", "-q", "-am", "topic")
	gitIn(t, dir, "checkout", "-q", "main")
	writeIn(t, dir, "f", "main\n")
	gitIn(t, dir, "commit", "-q", "-am", "main")
	return dir
}

// stopped runs plain git in dir, which must stop on a conflict.
func stopped(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := plainGit(t, dir, args...); err == nil {
		t.Fatalf("git %v didn't stop: %s", args, out)
	}
}

// seedOK seeds dir into a new VM folder, which it returns.
func seedOK(t *testing.T, dir string) string {
	t.Helper()
	vm := filepath.Join(t.TempDir(), "vm")
	if out, _, err := seedTreeInto(t, dir, vm); err != nil {
		t.Fatalf("seed: %v (%q)", err, out)
	}
	return vm
}

// A rebase stopped on a conflict, started with --autostash: the VM's
// clone is in the same rebase, git rebase --continue there finishes it,
// and the autostashed change comes back.
func TestSeedRebaseAutostash(t *testing.T) {
	dir := conflictRepo(t)
	gitIn(t, dir, "checkout", "-q", "topic")
	writeIn(t, dir, "g", "wip\n")
	stopped(t, dir, "rebase", "--autostash", "main")
	vm := seedOK(t, dir)
	if _, err := os.Stat(filepath.Join(vm, ".git", "rebase-merge", "autostash")); err != nil {
		t.Fatalf("no rebase in the VM: %v", err)
	}
	writeIn(t, vm, "f", "resolved\n")
	gitIn(t, vm, "add", "f")
	if out, err := plainGit(t, vm, "-c", "core.editor=true", "rebase", "--continue"); err != nil {
		t.Fatalf("rebase --continue: %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(filepath.Join(vm, "g")); string(b) != "wip\n" { //nolint:gosec // the test's own files
		t.Errorf("the autostash: g = %q", b)
	}
	if got := gitIn(t, vm, "log", "--format=%s", "-2"); got != "topic\nmain" {
		t.Errorf("log %q", got)
	}
}

// A merge stopped on a conflict: the VM's clone has MERGE_HEAD, and git
// commit there makes the merge, of both parents.
func TestSeedMergeConflict(t *testing.T) {
	dir := conflictRepo(t)
	stopped(t, dir, "merge", "topic")
	vm := seedOK(t, dir)
	if got, want := gitIn(t, vm, "rev-parse", "MERGE_HEAD"), gitIn(t, dir, "rev-parse", "topic"); got != want {
		t.Errorf("MERGE_HEAD %q, want %q", got, want)
	}
	writeIn(t, vm, "f", "resolved\n")
	gitIn(t, vm, "add", "f")
	gitIn(t, vm, "commit", "-q", "--no-edit")
	if got := gitIn(t, vm, "rev-list", "--parents", "-n1", "HEAD"); len(strings.Fields(got)) != 3 {
		t.Errorf("not a merge: %q", got)
	}
}

// A cherry-pick stopped on a conflict in a reftable repo, which keeps
// CHERRY_PICK_HEAD as a ref, not a file: the VM's clone has it, and git
// cherry-pick --continue there finishes the pick.
func TestSeedCherryPickReftable(t *testing.T) {
	dir := conflictRepo(t, "--ref-format=reftable")
	stopped(t, dir, "cherry-pick", "topic")
	if _, err := os.Stat(filepath.Join(dir, ".git", "CHERRY_PICK_HEAD")); err == nil {
		t.Fatal("a file: not the reftable case")
	}
	vm := seedOK(t, dir)
	if got, want := gitIn(t, vm, "rev-parse", "CHERRY_PICK_HEAD"), gitIn(t, dir, "rev-parse", "topic"); got != want {
		t.Errorf("CHERRY_PICK_HEAD %q, want %q", got, want)
	}
	writeIn(t, vm, "f", "resolved\n")
	gitIn(t, vm, "add", "f")
	if out, err := plainGit(t, vm, "-c", "core.editor=true", "cherry-pick", "--continue"); err != nil {
		t.Fatalf("cherry-pick --continue: %v\n%s", err, out)
	}
	if got := gitIn(t, vm, "log", "--format=%s", "-1"); got != "topic" {
		t.Errorf("log %q", got)
	}
}

// Bisect: the VM's clone is bisecting as the repo is, and goes on.
func TestSeedBisect(t *testing.T) {
	dir := conflictRepo(t)
	for i := range 4 {
		writeIn(t, dir, "n", strings.Repeat("x", i)+"\n")
		gitIn(t, dir, "add", "n")
		gitIn(t, dir, "commit", "-q", "-m", "n")
	}
	gitIn(t, dir, "bisect", "start", "HEAD", "HEAD~4")
	vm := seedOK(t, dir)
	if got, want := gitIn(t, vm, "rev-parse", "refs/bisect/bad"), gitIn(t, dir, "rev-parse", "refs/bisect/bad"); got != want {
		t.Errorf("bisect/bad %q, want %q", got, want)
	}
	if got, want := gitIn(t, vm, "rev-parse", "HEAD"), gitIn(t, dir, "rev-parse", "HEAD"); got != want {
		t.Errorf("HEAD %q, want %q", got, want)
	}
	if out, err := plainGit(t, vm, "bisect", "good"); err != nil {
		t.Errorf("bisect good: %v\n%s", err, out)
	}
}

// What no operation in progress needs stays: ORIG_HEAD of a reset long
// done, and a commit only a merge message names, which no file that
// holds object ids does.
func TestSeedNoStaleState(t *testing.T) {
	dir := conflictRepo(t)
	writeIn(t, dir, ".env", "SECRET=1\n")
	gitIn(t, dir, "add", ".env")
	gitIn(t, dir, "commit", "-q", "-m", "oops")
	secret := gitIn(t, dir, "rev-parse", "HEAD")
	gitIn(t, dir, "reset", "-q", "--hard", "HEAD~")
	vm := seedOK(t, dir)
	if out, err := plainGit(t, vm, "rev-parse", "-q", "--verify", "ORIG_HEAD"); err == nil {
		t.Errorf("ORIG_HEAD in the VM: %s", out)
	}
	if _, err := plainGit(t, vm, "cat-file", "-e", secret); err == nil {
		t.Error("the reset commit is in the VM")
	}
	// A merge in progress whose message names the commit.
	stopped(t, dir, "merge", "topic")
	msgFile := filepath.Join(dir, ".git", "MERGE_MSG")
	b, err := os.ReadFile(msgFile) //nolint:gosec // the test's own files
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(msgFile, append(b, []byte("This reverts commit "+secret+".\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	vm = seedOK(t, dir)
	if _, err := plainGit(t, vm, "cat-file", "-e", secret); err == nil {
		t.Error("a commit the merge message names is in the VM")
	}
	if _, err := plainGit(t, vm, "rev-parse", "-q", "--verify", "MERGE_HEAD"); err != nil {
		t.Error("no merge in the VM")
	}
}

// The stash: every entry, oldest first, with its message, a -u entry's
// untracked files too, but a git stash --all entry, which holds ignored
// files, which is named.
func TestSeedStash(t *testing.T) {
	dir := conflictRepo(t)
	writeIn(t, dir, ".gitignore", "*.log\n")
	gitIn(t, dir, "add", ".gitignore")
	gitIn(t, dir, "commit", "-q", "-m", "ignore")
	writeIn(t, dir, "f", "one\n")
	gitIn(t, dir, "stash", "push", "-q", "-m", "one")
	writeIn(t, dir, "new", "untracked\n")
	gitIn(t, dir, "stash", "push", "-q", "-u", "-m", "untracked")
	writeIn(t, dir, "x.log", "ignored secret\n")
	writeIn(t, dir, "f", "all\n")
	gitIn(t, dir, "stash", "push", "-q", "--all", "-m", "all")
	s, err := planSeed(t.Context(), ".", dir)
	if err != nil {
		t.Fatal(err)
	}
	data, err := readSeedState(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(data.state.stashAll, []string{"stash@{0}"}) || len(data.state.stash) != 2 {
		t.Errorf("stash %q, all %q", data.state.stash, data.state.stashAll)
	}
	vm := seedOK(t, dir)
	if got := gitIn(t, vm, "stash", "list", "--format=%gs"); got != "On main: untracked\nOn main: one" {
		t.Errorf("the VM's stash:\n%s", got)
	}
	gitIn(t, vm, "stash", "apply", "-q", "stash@{0}")
	if b, _ := os.ReadFile(filepath.Join(vm, "new")); string(b) != "untracked\n" { //nolint:gosec // the test's own files
		t.Errorf("the -u entry's file: %q", b)
	}
	if _, err := os.Stat(filepath.Join(vm, "x.log")); err == nil {
		t.Error("the --all entry's ignored file is in the VM")
	}
	if _, err := plainGit(t, vm, "cat-file", "-e", gitIn(t, dir, "rev-parse", "stash@{0}")); err == nil {
		t.Error("the --all entry is in the VM")
	}
}

// A linked worktree's seed sends no stash, which is the whole repo's.
func TestSeedStashWorktree(t *testing.T) {
	dir := conflictRepo(t)
	writeIn(t, dir, "f", "one\n")
	gitIn(t, dir, "stash", "push", "-q", "-m", "one")
	wt := filepath.Join(t.TempDir(), "wt")
	gitIn(t, dir, "worktree", "add", "-q", "-b", "wt", wt)
	s, err := planSeed(t.Context(), ".", wt)
	if err != nil {
		t.Fatal(err)
	}
	data, err := readSeedState(t.Context(), s)
	if err != nil || len(data.state.stash) != 0 {
		t.Errorf("a worktree's stash %q, %v", data.state.stash, err)
	}
}

// The clone check reports the VM's own stash entries, as stash@{N}, never
// those its seed stored, and they come back as stash-N, by their commit.
func TestCloneStateStash(t *testing.T) {
	dir := conflictRepo(t)
	writeIn(t, dir, "f", "host\n")
	gitIn(t, dir, "stash", "push", "-q", "-m", "host")
	vm := seedOK(t, dir)
	writeIn(t, vm, "g", "vm\n")
	gitIn(t, vm, "stash", "push", "-q", "-m", "vm")
	out, err := plainCloneState(t, vm)
	if err != nil {
		t.Fatal(err)
	}
	mine := gitIn(t, vm, "rev-parse", "stash@{0}")
	if got := strings.TrimSpace(string(out[0].refs)); !strings.HasSuffix(got, mine+" stash@{0}") || strings.Contains(got, "stash@{1}") {
		t.Errorf("refs %q", got)
	}
	refs := unfetchedRefs(t.Context(), dir, out[0].refs)
	if !slices.Contains(refs, fetchRef{name: "stash-0", src: mine, stash: true}) {
		t.Errorf("unfetched %v", refs)
	}
}

// rm of a VM whose clone has stash entries of its own offers to fetch
// them, and on yes fetches each by its commit, even one no ref names, and
// saves it as saved/devvm/stash-N without its untracked files, which go
// to saved/devvm/stash-N-untracked, named, so git stash apply plants none.
func TestRmFetchesStash(t *testing.T) {
	e, f, vmfs := cloneEnv(t)
	st := existingVM(t, e, Options{Network: "open", Clone: true})
	clone := vmfs + e.Dir
	if err := os.WriteFile(filepath.Join(clone, "s"), []byte("stashed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, clone, "stash", "push", "-q", "-u", "-m", "vm")
	gitIn(t, clone, "commit", "-q", "--allow-empty", "-m", "on main")
	if err := os.WriteFile(filepath.Join(clone, "s"), []byte("again\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, clone, "stash", "push", "-q", "-u", "-m", "vm2")
	older := gitIn(t, clone, "rev-parse", "stash@{1}")
	got := printed(t, e, false)
	asked := answers(t, e, true)
	if err := (Lifecycle{e}).Rm(t.Context(), RmOptions{Force: true}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if _, ok := f.Instances[st.Name()]; ok || !slices.Equal(*asked, []string{"fetch_before_delete prefix=saved/devvm"}) {
		t.Errorf("deleted %v, asked %q", !ok, *asked)
	}
	// stash@{1}, which only the stash's reflog names: its tree and first
	// two parents, without the third, its untracked files, which go to a
	// branch of their own.
	refs := gitIn(t, e.Dir, "for-each-ref", "--format=%(refname)", "refs/heads/saved")
	if !slices.Contains(*got, `clone_stash_untracked branch=saved/devvm/stash-1 count=1 files="s" untracked=saved/devvm/stash-1-untracked`) {
		t.Errorf("printed %q", *got)
	}
	saved := "refs/heads/saved/devvm/stash-1"
	for _, rev := range []string{"^{tree}", "^1", "^2"} {
		if got, want := gitIn(t, e.Dir, "rev-parse", saved+rev), gitIn(t, clone, "rev-parse", older+rev); got != want {
			t.Errorf("%s%s is %q, want %q (saved: %s)", saved, rev, got, want, refs)
		}
	}
	if out, err := plainGit(t, e.Dir, "rev-parse", "-q", "--verify", saved+"^3"); err == nil {
		t.Errorf("saved with its untracked files: %s", out)
	}
	if got, want := gitIn(t, e.Dir, "rev-parse", saved+"-untracked"), gitIn(t, clone, "rev-parse", older+"^3"); got != want {
		t.Errorf("%s-untracked is %q, want %q", saved, got, want)
	}
	// rm doesn't take an entry saved so for one not fetched.
	if unfetched := unfetchedRefs(t.Context(), e.Dir, []byte(older+" stash@{1}\n")); len(unfetched) != 0 {
		t.Errorf("unfetched %v", unfetched)
	}
}

// haveObjects of many ids, as a partial monorepo's tips are, half of
// them there, with repeats: each one there once, in order, in seconds,
// not the minutes a scan of the ids for each answer took; the caller's
// ids as they were.
func TestHaveObjectsMany(t *testing.T) {
	gitEnv(t)
	dir := filepath.Join(t.TempDir(), "r")
	gitIn(t, filepath.Dir(dir), "init", "-q", dir)
	const n = 50000
	var in strings.Builder
	for i := range n {
		body := strconv.Itoa(i)
		fmt.Fprintf(&in, "blob\ndata %d\n%s\n", len(body), body)
	}
	cmd := stubtest.Git(t.Context(), "-C", dir, "fast-import", "--quiet")
	cmd.Stdin = strings.NewReader(in.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fast-import: %v\n%s", err, out)
	}
	var have, ids []string
	for line := range strings.Lines(gitIn(t, dir, "cat-file", "--batch-all-objects", "--batch-check=%(objectname)")) {
		have = append(have, strings.TrimSpace(line))
	}
	for i := range n {
		ids = append(ids, fmt.Sprintf("%040x", i+1)) // none of them there
	}
	ids = append(append(ids, have...), have[:100]...)
	before := slices.Clone(ids)
	start := time.Now()
	got, err := haveObjects(t.Context(), dir, ids)
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 20*time.Second {
		t.Errorf("took %s", took)
	}
	slices.Sort(have)
	if !slices.Equal(got, have) {
		t.Errorf("got %d of %d", len(got), len(have))
	}
	if !slices.Equal(ids, before) {
		t.Error("the caller's ids changed")
	}
}

// stateIDs takes object ids only from the files and fields that hold
// them, never free text, and only what looks like one.
func TestStateIDs(t *testing.T) {
	a, b, c, d := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 64), strings.Repeat("d", 40)
	files := []tarEntry{
		{"state/rebase-merge/onto", []byte(a + "\n")},
		{"state/rebase-merge/rewritten-list", []byte(b + " " + c + " extra " + d + "\n")},
		{"state/rebase-merge/message", []byte("This reverts commit " + d + ".\n")},
		{"state/MERGE_MSG", []byte(d + "\n")},
		{"state/MERGE_HEAD", []byte("not-an-id\n" + a + "\n")},
		{"state/BISECT_LOG", []byte("git bisect start '" + d + "'\ngit bisect fixed " + b + "\n# bad: [" + d + "]\n")},
	}
	got := stateIDs(files, []string{"broken", "fixed"})
	if want := []string{a, b, c, a, b}; !slices.Equal(got, want) {
		t.Errorf("stateIDs %q", got)
	}
}

// plainCloneState is what cloneState says of the clone at vm, parsed.
func plainCloneState(t *testing.T, vm string) ([]repoInVM, error) {
	t.Helper()
	out, err := runSeedScript(t, cloneState, nil, filepath.Dir(vm), "101", filepath.Base(vm))
	if err != nil {
		return nil, err
	}
	return parseCloneState([]byte(out), filepath.Dir(vm), 1)
}
