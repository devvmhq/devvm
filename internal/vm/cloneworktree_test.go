package vm

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/stubtest"
)

// readTree is newSeedTree, then load, as seed does: the repo at dir's
// working tree, or nil when it has no index.
func readTree(ctx context.Context, dir string) (*seedTree, error) {
	t, err := newSeedTree()
	if err != nil {
		return nil, err
	}
	ok, err := t.load(ctx, dir)
	if err != nil || !ok {
		t.close()
		return nil, err
	}
	return t, nil
}

// treeRepo is a repo with every kind of uncommitted change the seed sends
// or names, and an untracked cache that names an untracked .env:
// staged a, unstaged b, deleted c, intent-to-add n, a changed link, a
// filtered file changed (crypt), foo turned into a folder with a .env in
// it, and an embedded repo staged with a token in its config.
func treeRepo(t *testing.T) string {
	t.Helper()
	gitEnv(t)
	dir := filepath.Join(t.TempDir(), "host")
	gitIn(t, filepath.Dir(dir), "init", "-q", "-b", "main", dir)
	write := func(name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{"a": "a\n", "b": "b\n", "c": "c\n", "foo": "foo\n", "crypt": "plaintext\n", ".gitattributes": "crypt filter=rot\n"} {
		write(name, body)
	}
	if err := os.Symlink("a", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "config", "filter.rot.clean", "tr a-z n-za-m")
	gitIn(t, dir, "config", "filter.rot.smudge", "tr a-z n-za-m")
	gitIn(t, dir, "config", "core.untrackedCache", "true")
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-q", "-m", "one")
	write("a", "staged\n")
	gitIn(t, dir, "add", "a")
	write("b", "unstaged\n")
	if err := os.Remove(filepath.Join(dir, "c")); err != nil {
		t.Fatal(err)
	}
	write("n", "new\n")
	gitIn(t, dir, "add", "-N", "n")
	if err := os.Remove(filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("b", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	write("crypt", "changed\n")
	if err := os.Remove(filepath.Join(dir, "foo")); err != nil {
		t.Fatal(err)
	}
	write("foo/.env", "AWS=topsecret\n")
	emb := filepath.Join(dir, "emb")
	gitIn(t, dir, "init", "-q", "-b", "main", "emb")
	gitIn(t, emb, "commit", "-q", "--allow-empty", "-m", "e")
	gitIn(t, emb, "remote", "add", "origin", "https://u:ghp_EMBTOKEN@github.com/x/emb.git")
	write("emb/.env", "LIBSECRET=x\n")
	gitIn(t, dir, "add", "emb")
	write(".env.production", "PROD=1\n")
	// The untracked cache, which names .env.production, written.
	gitIn(t, dir, "status", "--porcelain")
	return dir
}

// readTree sorts a repo's uncommitted changes out: the working copies of
// regular files and links that go as they are, what's gone (c), a
// filtered file, which may differ, and a file that's a folder now, which
// are named, and the paths checked out from the index; the index's
// blobs, no gitlink's.
func TestReadTree(t *testing.T) {
	dir := treeRepo(t)
	tr, err := readTree(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.close()
	var changed []string
	for _, c := range tr.changed {
		changed = append(changed, c.path+map[bool]string{true: "@"}[c.link])
	}
	slices.Sort(changed)
	if !slices.Equal(changed, []string{"b", "link@", "n"}) || !slices.Equal(tr.filtered, []string{"crypt"}) || !slices.Equal(tr.typechanged, []string{"foo"}) {
		t.Errorf("changed %q, filtered %q, typechanged %q", changed, tr.filtered, tr.typechanged)
	}
	checkout := bytes.Split(bytes.TrimSuffix(tr.checkout, []byte{0}), []byte{0})
	var got []string
	for _, p := range checkout {
		got = append(got, string(p))
	}
	if !slices.Equal(got, []string{".gitattributes", "a", "crypt", "emb"}) {
		t.Errorf("checkout %q", got)
	}
	// Unstaged changes alone are dirty: not a's, staged, nor emb's, whose
	// HEAD is what's staged.
	for p, want := range map[string]bool{"a": false, "b": true, "c": true, "emb": false, "foo": true, "link": true, "n": true, "crypt": true} {
		if tr.dirty[p] != want {
			t.Errorf("%s dirty: %v", p, tr.dirty[p])
		}
	}
	if gitlink := gitIn(t, dir, "rev-parse", ":emb"); slices.Contains(tr.blobs, gitlink) {
		t.Error("a gitlink's commit is a blob to send")
	}
	idx, err := os.ReadFile(tr.index)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(idx, []byte("UNTR")) || bytes.Contains(idx, []byte(".env.production")) || bytes.Contains(idx, []byte(dir)) {
		t.Error("the index sent has the untracked cache, an untracked file's name or this machine's path")
	}
	if !slices.Equal(tr.gone, []string{"c", "foo"}) {
		t.Errorf("gone %q", tr.gone)
	}
	// The VM's own: a differing file the seed didn't make so (mine), and
	// one it would have sent that the VM had (n); not b, sent, nor c,
	// deleted as here.
	if got := tr.ownFiles([]string{"b", "mine", "c"}, []string{"n"}); !slices.Equal(got, []string{"mine", "n"}) {
		t.Errorf("ownFiles %q", got)
	}
}

// A merge's conflicts go as they are: the index's stages, and a working
// copy with its markers; a conflict that a filter changes isn't checked
// out, which git checkout-index would fail on.
func TestSeedTreeConflict(t *testing.T) {
	gitEnv(t)
	dir := filepath.Join(t.TempDir(), "host")
	gitIn(t, filepath.Dir(dir), "init", "-q", "-b", "main", dir)
	if err := os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("g filter=rot\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "config", "filter.rot.clean", "tr a-z n-za-m")
	gitIn(t, dir, "config", "filter.rot.smudge", "tr a-z n-za-m")
	both := func(body string) {
		t.Helper()
		for _, f := range []string{"f", "g"} {
			if err := os.WriteFile(filepath.Join(dir, f), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		gitIn(t, dir, "add", ".")
		gitIn(t, dir, "commit", "-q", "-m", body)
	}
	both("base\n")
	gitIn(t, dir, "checkout", "-q", "-b", "other")
	both("other\n")
	gitIn(t, dir, "checkout", "-q", "main")
	both("main\n")
	// Plain git, which runs the filter, so g isn't taken for changed.
	cmd := stubtest.Git(t.Context(), "-C", dir, "merge", "other")
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "CONFLICT") {
		t.Fatalf("no conflict: %s", out)
	}
	vm := filepath.Join(t.TempDir(), "vm")
	if _, _, err := seedTreeInto(t, dir, vm); err != nil {
		t.Fatal(err)
	}
	if got := gitIn(t, vm, "ls-files", "-u", "f"); strings.Count(got, "\n") != 2 {
		t.Errorf("f's stages in the VM:\n%s", got)
	}
	if b, _ := os.ReadFile(filepath.Join(vm, "f")); !strings.Contains(string(b), "<<<<<<<") { //nolint:gosec // the test's own files
		t.Errorf("f = %q", b)
	}
}

// A sparse checkout's paths outside it (skip-worktree) aren't checked out,
// which git checkout-index would fail on, and stay out of the VM's
// working tree, as git leaves them out of the host's.
func TestSeedTreeSparse(t *testing.T) {
	gitEnv(t)
	dir := filepath.Join(t.TempDir(), "host")
	gitIn(t, filepath.Dir(dir), "init", "-q", "-b", "main", dir)
	for _, name := range []string{"a/f", "b/f"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-q", "-m", "one")
	gitIn(t, dir, "sparse-checkout", "set", "a")
	vm := filepath.Join(t.TempDir(), "vm")
	if _, _, err := seedTreeInto(t, dir, vm); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(vm, "a", "f")); err != nil || string(b) != "a/f\n" { //nolint:gosec // the test's own files
		t.Errorf("a/f = %q, %v", b, err)
	}
	if _, err := os.Lstat(filepath.Join(vm, "b", "f")); err == nil {
		t.Error("b/f, outside the sparse checkout, is in the VM's working tree")
	}
	if got := gitIn(t, vm, "status", "--porcelain"); got != "" {
		t.Errorf("status:\n%s", got)
	}
}

// A file marked skip-worktree by hand, with no sparse checkout, which the
// working tree has, goes as the index has it, never as it is here, where
// its edits are hidden, and is named; it stays skip-worktree in the VM.
// So does an assume-unchanged file, whose edits git doesn't look at.
func TestSeedTreeSkipWorktree(t *testing.T) {
	dir := safetyRepo(t, map[string]string{"local.yml": "committed\n", "au": "committed\n", "a": "a\n"})
	gitIn(t, dir, "update-index", "--skip-worktree", "local.yml")
	gitIn(t, dir, "update-index", "--assume-unchanged", "au")
	writeIn(t, dir, "local.yml", "password: hidden\n")
	writeIn(t, dir, "au", "edited\n")
	vm := filepath.Join(t.TempDir(), "vm")
	_, tr, err := seedTreeInto(t, dir, vm)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"local.yml": "committed\n", "au": "committed\n"} {
		if b, err := os.ReadFile(filepath.Join(vm, name)); err != nil || string(b) != want { //nolint:gosec // the test's own files
			t.Errorf("%s = %q, %v", name, b, err)
		}
	}
	if !slices.Equal(tr.skipped, []string{"local.yml"}) {
		t.Errorf("named %q", tr.skipped)
	}
	if got := gitIn(t, vm, "ls-files", "-t", "local.yml"); got != "S local.yml" {
		t.Errorf("in the VM: %q", got)
	}
	if got := gitIn(t, vm, "status", "--porcelain"); got != "" {
		t.Errorf("status:\n%s", got)
	}
}

// vmChanges is what rm and devvm clone off would show of the clone at vm
// (cloneState, then notSeeded): git status's lines for the VM's own
// changes.
func vmChanges(t *testing.T, vm string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "sh", "-c", cloneState, "sh", filepath.Dir(vm), "101", filepath.Base(vm)) //nolint:gosec // the test's own
	cmd.Env = stubtest.GitEnv(os.Environ())                                                                           // its git on the VM's clone, never the checkout's
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseCloneState(out, filepath.Dir(vm), 1)
	if err != nil {
		t.Fatal(err)
	}
	return string(notSeeded(got[0].status, got[0].same))
}

// The seed's record of the repo's uncommitted changes: rm and devvm
// clone off don't count them as the VM's work while the VM leaves them
// as they came, however the repo here changes since; a change the VM
// makes to one counts, and so does a new mode, a deletion, or a change of
// what's staged.
func TestSeededChanges(t *testing.T) {
	dir := treeRepo(t)
	vm := filepath.Join(t.TempDir(), "vm")
	if _, _, err := seedTreeInto(t, dir, vm); err != nil {
		t.Fatal(err)
	}
	if got := vmChanges(t, vm); got != "" {
		t.Fatalf("an untouched VM's changes:\n%s", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "b"), []byte("host again\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := vmChanges(t, vm); got != "" {
		t.Errorf("after a change here, an untouched VM's changes:\n%s", got)
	}
	steps := []struct {
		do   func()
		line string
	}{
		{func() { _ = os.WriteFile(filepath.Join(vm, "b"), []byte("vm\n"), 0o600) }, " M b\n"},
		{func() { _ = os.Chmod(filepath.Join(vm, "a"), 0o700) }, "MM a\n"}, //nolint:gosec // an executable bit is the change tested
		{func() { _ = os.Remove(filepath.Join(vm, "n")) }, " D n\n"},
		{func() { gitIn(t, vm, "add", "link") }, "M  link\n"},
	}
	want := ""
	for _, s := range steps {
		s.do()
		want = sortedLines(want + s.line)
		if got := vmChanges(t, vm); got != want {
			t.Errorf("changes:\n%s\nwant:\n%s", got, want)
		}
	}
	// Without the record, as in a VM an older devvm seeded, every change
	// is the VM's.
	if err := os.Remove(filepath.Join(vm, ".git", seedTreeRecord)); err != nil {
		t.Fatal(err)
	}
	if got := vmChanges(t, vm); !strings.Contains(got, "M  a\n") && !strings.Contains(got, "MM a\n") || !strings.Contains(got, " D c\n") {
		t.Errorf("without the record:\n%s", got)
	}
}

// A status line is the seed's only when every path it names is one the
// VM says is as the seed put it, quoted or not; untracked files' lines,
// and lines it can't read, stay.
func TestSeededLine(t *testing.T) {
	same := map[string]bool{"a": true, "a b": true, "é\n": true, "new": true}
	for _, c := range []struct {
		line string
		want bool
	}{
		{" M a", true},
		{"M  a b", true},
		{`MM "\303\251\n"`, true},
		{"R  a -> new", true},
		{"R  a -> other", false},
		{"R  a", false},
		{"?? a", false},
		{"!! a", false},
		{" M b", false},
		{`M  "a`, false},
		{"M", false},
	} {
		if got := seededLine(c.line, same); got != c.want {
			t.Errorf("seededLine(%q) = %v", c.line, got)
		}
	}
}

// sortedLines is s's lines, sorted by path, as git status lists them.
func sortedLines(s string) string {
	lines := strings.SplitAfter(s, "\n")
	lines = slices.DeleteFunc(lines, func(l string) bool { return l == "" })
	slices.SortFunc(lines, func(a, b string) int { return strings.Compare(a[3:], b[3:]) })
	return strings.Join(lines, "")
}

// The VM's clone gets the same status as the repo here, but for foo,
// gone there, and crypt, as git stores it; none of the untracked files,
// nor anything below foo or in emb.
func TestSeedTree(t *testing.T) {
	dir := treeRepo(t)
	vm := filepath.Join(t.TempDir(), "vm")
	out, tr, err := seedTreeInto(t, dir, vm)
	if err != nil {
		t.Fatal(err)
	}
	if k := parseKept(out); !k.tree || len(tr.ownFiles(k.files, k.pre)) != 0 || k.head || k.refs != nil {
		t.Errorf("kept %+v", k)
	}
	if got := gitIn(t, vm, "status", "--porcelain", "--untracked-files=all"); got != "M  a\n M b\n D c\nA  emb\n D foo\n M link\n A n" {
		t.Errorf("status:\n%s", got)
	}
	for name, want := range map[string]string{"a": "staged\n", "b": "unstaged\n", "n": "new\n", "crypt": "cynvagrkg\n"} {
		if b, _ := os.ReadFile(filepath.Join(vm, name)); string(b) != want { //nolint:gosec // the test's own files
			t.Errorf("%s = %q", name, b)
		}
	}
	if target, err := os.Readlink(filepath.Join(vm, "link")); err != nil || target != "b" {
		t.Errorf("link -> %q, %v", target, err)
	}
	for _, p := range []string{".env.production", "foo", "emb/.env", "emb/.git"} {
		if _, err := os.Lstat(filepath.Join(vm, p)); err == nil {
			t.Errorf("%s is in the VM", p)
		}
	}
	idx, err := os.ReadFile(filepath.Join(vm, ".git", "index")) //nolint:gosec // the test's own files
	if err != nil || bytes.Contains(idx, []byte("UNTR")) || bytes.Contains(idx, []byte(".env.production")) {
		t.Errorf("the VM's index has the untracked cache (%v)", err)
	}
}
