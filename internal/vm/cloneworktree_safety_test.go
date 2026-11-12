package vm

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/stubtest"
)

// safetyRepo is a repo at a folder of its own, its files written and
// committed, with plain git, which runs the repo's filters.
func safetyRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	gitEnv(t)
	dir := filepath.Join(t.TempDir(), "host")
	gitIn(t, filepath.Dir(dir), "init", "-q", "-b", "main", dir)
	for name, body := range files {
		writeIn(t, dir, name, body)
	}
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "one")
	return dir
}

// writeIn writes body to dir's name, making its folders.
func writeIn(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// plainGit runs plain git in dir, which may fail, and returns its output.
func plainGit(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	out, err := stubtest.Git(t.Context(), append([]string{"-C", dir}, args...)...).CombinedOutput()
	return string(out), err
}

// sentPaths is the paths of the working copies tr sends.
func sentPaths(tr *seedTree) []string {
	var out []string
	for _, c := range tr.changed {
		out = append(out, c.path)
	}
	slices.Sort(out)
	return out
}

// Reading the working tree runs no filter that an embedded repo's own
// config names, on a file of that repo whose stat changed: host git
// doesn't enter it (hostgit.DiffFiles).
func TestReadTreeEntersNoEmbeddedRepo(t *testing.T) {
	dir := safetyRepo(t, map[string]string{"a": "a\n"})
	emb := filepath.Join(dir, "emb")
	marker := filepath.Join(t.TempDir(), "ran")
	gitIn(t, dir, "init", "-q", "-b", "main", "emb")
	writeIn(t, emb, ".gitattributes", "* filter=x\n")
	writeIn(t, emb, "f", "f\n")
	gitIn(t, emb, "add", "-A")
	gitIn(t, emb, "commit", "-q", "-m", "e")
	gitIn(t, emb, "config", "filter.x.clean", "touch "+marker+"; cat")
	gitIn(t, dir, "add", "emb")
	gitIn(t, dir, "commit", "-q", "-m", "emb")
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(emb, "f"), later, later); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	tr, err := readTree(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	tr.close()
	if _, err := os.Stat(marker); err == nil {
		t.Error("the embedded repo's filter ran")
	}
}

// A tracked file under a folder that's a symbolic link here now, inside
// the repo, goes as deleted, as git has it, never as the file the link
// leads to: an ignored file's contents, or .git's config. Nor does a path
// of the index with a .git part, in any case, or a .. part.
func TestReadTreeLinkedParent(t *testing.T) {
	dir := safetyRepo(t, map[string]string{"config/app.yml": "tracked\n", "x/config": "tracked\n", ".gitignore": "config.local/\n"})
	writeIn(t, dir, "config.local/app.yml", "SECRET=ignored\n")
	for _, d := range []string{"config", "x"} {
		if err := os.RemoveAll(filepath.Join(dir, d)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("config.local", filepath.Join(dir, "config")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".git", filepath.Join(dir, "x")); err != nil {
		t.Fatal(err)
	}
	tr, err := readTree(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.close()
	if got := sentPaths(tr); len(got) != 0 {
		t.Errorf("sent %q", got)
	}
	vm := filepath.Join(t.TempDir(), "vm")
	if _, _, err := seedTreeInto(t, dir, vm); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"config", "x", "config.local"} {
		if _, err := os.Lstat(filepath.Join(vm, p)); err == nil {
			t.Errorf("%s is in the VM", p)
		}
	}
	// gitIn trims the first line's leading space.
	if got := gitIn(t, vm, "status", "--porcelain"); got != "D config/app.yml\n D x/config" {
		t.Errorf("the VM's status:\n%s", got)
	}
}

// safeIndexPath refuses a path git itself would never check out: one
// with a .git part, in any case, or an empty, . or .. part.
func TestSafeIndexPath(t *testing.T) {
	for p, want := range map[string]bool{
		"a": true, "a/b": true, ".gitignore": true, "a/.github/x": true, ".git2": true,
		".git": false, "a/.git/config": false, ".GIT/config": false, "a/.Git": false,
		"../a": false, "a/../b": false, "./a": false, "a//b": false, "": false, "a/": false, "/a": false,
	} {
		if got := safeIndexPath(p); got != want {
			t.Errorf("safeIndexPath(%q) = %v", p, got)
		}
	}
}

// The index git refuses to write on its own, v3 with skipHash, is
// written all the same, with a real hash, and its resolve-undo goes, as
// the pack carries none of its blobs: the VM's repo passes git fsck.
func TestNormaliseRewrites(t *testing.T) {
	dir := safetyRepo(t, map[string]string{"f": "base\n"})
	// A conflict with a commit no ref reaches, resolved: resolve-undo
	// keeps its blob, which the seed's pack doesn't carry.
	gitIn(t, dir, "checkout", "-q", "-b", "gone")
	writeIn(t, dir, "f", "theirs, nowhere else\n")
	gitIn(t, dir, "commit", "-q", "-am", "gone")
	theirs := gitIn(t, dir, "rev-parse", "HEAD")
	gitIn(t, dir, "checkout", "-q", "main")
	gitIn(t, dir, "branch", "-q", "-D", "gone")
	writeIn(t, dir, "f", "main\n")
	gitIn(t, dir, "commit", "-q", "-am", "main")
	if out, err := plainGit(t, dir, "merge", theirs); err == nil {
		t.Fatalf("no conflict: %s", out)
	}
	writeIn(t, dir, "f", "resolved\n")
	gitIn(t, dir, "add", "f")
	gitIn(t, dir, "-c", "index.skipHash=true", "update-index", "--index-version", "3", "--force-write-index")
	idx, err := os.ReadFile(filepath.Join(dir, ".git", "index")) //nolint:gosec // the test's own files
	if err != nil || !strings.Contains(string(idx), "REUC") {
		t.Fatalf("no resolve-undo in the index (%v)", err)
	}
	tr, err := readTree(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(tr.index)
	tr.close()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "REUC") || strings.HasSuffix(string(b), strings.Repeat("\x00", 20)) {
		t.Error("the index sent has resolve-undo or no hash")
	}
	vm := filepath.Join(t.TempDir(), "vm")
	if _, _, err := seedTreeInto(t, dir, vm); err != nil {
		t.Fatal(err)
	}
	gitIn(t, vm, "fsck", "--no-dangling")
}

// A file is filtered when the index's attributes name a filter for it,
// even once the working tree's don't (s); a filtered file that's gone
// here goes as gone (g); a file that's a symbolic link here goes as one
// (f), as the VM's git status then shows the same.
func TestReadTreeKinds(t *testing.T) {
	dir := safetyRepo(t, map[string]string{".gitattributes": "s filter=rot\ng filter=rot\n", "s": "s\n", "g": "g\n", "f": "f\n"})
	for _, p := range []string{"g", "f"} {
		if err := os.Remove(filepath.Join(dir, p)); err != nil {
			t.Fatal(err)
		}
	}
	// git reads a missing .gitattributes from the index: this one is
	// there, and names no filter.
	writeIn(t, dir, ".gitattributes", "# none\n")
	writeIn(t, dir, "s", "changed\n")
	if err := os.Symlink("s", filepath.Join(dir, "f")); err != nil {
		t.Fatal(err)
	}
	tr, err := readTree(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.close()
	link := map[string]bool{}
	for _, c := range tr.changed {
		link[c.path] = c.link
	}
	if !slices.Equal(tr.filtered, []string{"s"}) || !slices.Equal(sentPaths(tr), []string{".gitattributes", "f"}) || !link["f"] || len(tr.typechanged) != 0 {
		t.Errorf("filtered %q, sent %q, typechanged %q", tr.filtered, sentPaths(tr), tr.typechanged)
	}
	if !slices.Equal(tr.gone, []string{"g"}) {
		t.Errorf("gone %q", tr.gone)
	}
}

// In a SHA-256 repo, the record names each copy sent by its SHA-256 blob,
// as git does.
func TestRecordSHA256(t *testing.T) {
	gitEnv(t)
	dir := filepath.Join(t.TempDir(), "host")
	gitIn(t, filepath.Dir(dir), "init", "-q", "--object-format=sha256", "-b", "main", dir)
	writeIn(t, dir, "a", "a\n")
	gitIn(t, dir, "add", "a")
	gitIn(t, dir, "commit", "-q", "-m", "one")
	writeIn(t, dir, "a", "changed\n")
	tr, err := readTree(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.close()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	if err := tr.write(tw); err != nil {
		t.Fatal(err)
	}
	want := gitIn(t, dir, "hash-object", "a") + " " + "100644:" + gitIn(t, dir, "rev-parse", ":a") + ":0 a\x00"
	if got := string(tr.record(map[string]string{"a": gitIn(t, dir, "hash-object", "a")})); got != want {
		t.Errorf("record %q, want %q", got, want)
	}
	if !strings.Contains(b.String(), want) {
		t.Error("the tar's record isn't the copy's SHA-256 blob")
	}
}
