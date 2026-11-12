package vm

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/stubtest"
)

// runSeedScript runs one of the seed's scripts as the VM would, with
// bash, sudo a plain call, args its arguments and stdin its input, and
// returns its output and error. Its git gets stubtest's environment
// (GitEnv), so a git that runs the tests can't point it at another repo,
// with the global config the test set (gitEnv's, none), else none.
func runSeedScript(t *testing.T, script string, stdin io.Reader, args ...string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "bash", append([]string{"-c", `sudo() { "$@"; }; ` + script, "sh"}, args...)...) //nolint:gosec // the test's own
	global := os.Getenv("GIT_CONFIG_GLOBAL")
	if global == "" {
		global = os.DevNull
	}
	cmd.Env = append(stubtest.GitEnv(os.Environ()), "GIT_CONFIG_GLOBAL="+global, "GIT_CONFIG_NOSYSTEM=1")
	var out, errb bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, &out, &errb
	err := cmd.Run()
	if err != nil {
		t.Logf("stderr: %s", errb.String())
	}
	return out.String(), err
}

// seedInto seeds the repo at dir into vm (the folder its clone is at) as
// devvm plans it (planSeed) and the VM applies it: the init script, then
// the tar, its working tree's included (readTree), and the pack. It
// returns what the apply printed.
func seedInto(t *testing.T, dir, vm string) (string, error) {
	out, _, err := seedTreeInto(t, dir, vm)
	return out, err
}

// seedTreeInto is seedInto, which also returns what it read of the
// working tree, nil for none.
func seedTreeInto(t *testing.T, dir, vm string) (string, *seedTree, error) {
	t.Helper()
	s, err := planSeed(t.Context(), ".", dir)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := runSeedScript(t, initRepos, nil, vm, "-", ".", s.format, "-"); err != nil || out != "" {
		t.Fatalf("init: %q, %v", out, err)
	}
	data, err := readSeedState(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	if data.tree, err = readTree(t.Context(), dir); err != nil {
		t.Fatal(err)
	}
	if data.tree != nil {
		t.Cleanup(data.tree.close)
		// As seed reads them, from the index's pointers.
		if data.lfs, err = readLFS(t.Context(), dir, data.tree.blobs); err != nil {
			t.Fatal(err)
		}
	}
	var tb bytes.Buffer
	if err := data.writeTar(&tb); err != nil {
		t.Fatal(err)
	}
	if _, err := runSeedScript(t, seedStage, &tb, vm); err != nil {
		t.Fatalf("stage: %v", err)
	}
	// As sendPack makes it (packCmd).
	var stderr bytes.Buffer
	p, err := packOutput(t, dir, data, &stderr)
	if err != nil {
		t.Fatalf("pack: %v\n%s", err, stderr.String())
	}
	out, err := runSeedScript(t, seedApply, bytes.NewReader(p), vm)
	return out, data.tree, err
}

// packOutput is the pack packCmd makes of data from the repo at dir, as
// sendPack sends it (packTail), its errors into stderr: what the VM would
// read, and the error that ends it. tweak, if any, changes pack-objects
// before it starts.
func packOutput(t *testing.T, dir string, data seedData, stderr io.Writer, tweak ...func(*exec.Cmd)) ([]byte, error) {
	t.Helper()
	cmd, list, err := packCmd(t.Context(), dir, data, stderr, io.Discard)
	if err != nil {
		return nil, err
	}
	for _, f := range tweak {
		f(cmd)
	}
	pack, err := cmd.StdoutPipe()
	if err == nil && list != nil {
		err = list.cmd.Start()
	}
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		if list != nil {
			_ = list.finish()
		}
		return nil, err
	}
	var in io.Reader = pack
	if list != nil {
		list.started()
		in = &packTail{r: pack, check: list.wait}
	}
	p, err := io.ReadAll(in)
	werr := cmd.Wait()
	if list != nil {
		if lerr := list.finish(); lerr != nil && err == nil {
			err = lerr
		}
	}
	if err == nil {
		err = werr
	}
	return p, err
}

// seedMark is the VM repo's seed marker at vm, trimmed.
func seedMark(t *testing.T, vm string) string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(vm, ".git", seedMarker)) //nolint:gosec // the test's own files
	return strings.TrimSpace(string(b))
}

// The seed copies a shallow repo, which a push couldn't (shallow update
// not allowed), with its shallow file, and a SHA-256 repo, which a push
// into a SHA-1 repo couldn't, each a repo git's fsck finds whole, its
// branch checked out and marked done; a VM commit then fetches back into
// the shallow host repo with its shallow file as it was.
func TestSeedShapes(t *testing.T) {
	gitEnv(t)
	root := t.TempDir()
	src := filepath.Join(root, "src")
	gitIn(t, root, "init", "-q", "-b", "main", src)
	for _, n := range []string{"1", "2", "3"} {
		if err := os.WriteFile(filepath.Join(src, "f"+n), []byte(n), 0o600); err != nil {
			t.Fatal(err)
		}
		gitIn(t, src, "add", ".")
		gitIn(t, src, "commit", "-q", "-m", "c"+n)
	}
	shallow := filepath.Join(root, "shallow")
	gitIn(t, root, "clone", "-q", "--depth", "1", "file://"+src, shallow)
	sha256 := filepath.Join(root, "sha256")
	gitIn(t, root, "init", "-q", "-b", "main", "--object-format=sha256", sha256)
	gitIn(t, sha256, "commit", "-q", "--allow-empty", "-m", "one")
	shapes := []struct{ name, dir, format string }{{"shallow", shallow, "sha1"}, {"sha256", sha256, "sha256"}}
	// A reftable host repo, where this git makes one: its refs are read
	// with for-each-ref, so the VM's files-format repo takes them.
	reftable := filepath.Join(root, "reftable")
	if stubtest.Git(t.Context(), "init", "-q", "-b", "main", "--ref-format=reftable", reftable).Run() == nil {
		gitIn(t, reftable, "commit", "-q", "--allow-empty", "-m", "one")
		gitIn(t, reftable, "branch", "other")
		shapes = append(shapes, struct{ name, dir, format string }{"reftable", reftable, "sha1"})
	}
	for _, c := range shapes {
		vm := filepath.Join(root, "vm-"+c.name)
		if out, err := seedInto(t, c.dir, vm); err != nil || out != "applied-tree\nkept-files\n\x00" {
			t.Fatalf("%s: apply %q, %v", c.name, out, err)
		}
		gitIn(t, vm, "fsck", "--no-progress")
		if got := gitIn(t, vm, "rev-parse", "--show-object-format"); got != c.format {
			t.Errorf("%s: format %s", c.name, got)
		}
		if got, want := gitIn(t, vm, "rev-parse", "HEAD"), gitIn(t, c.dir, "rev-parse", "HEAD"); got != want {
			t.Errorf("%s: HEAD %s, want %s", c.name, got, want)
		}
		if got := gitIn(t, vm, "status", "--porcelain"); got != "" {
			t.Errorf("%s: status %q", c.name, got)
		}
		if m := seedMark(t, vm); m != "done" {
			t.Errorf("%s: marker %q", c.name, m)
		}
		for _, f := range []string{"devvm-seed", "devvm-seeded-refs"} {
			if _, err := os.Lstat(filepath.Join(vm, ".git", f)); err == nil {
				t.Errorf("%s: %s left", c.name, f)
			}
		}
	}
	vm := filepath.Join(root, "vm-shallow")
	if got, want := gitIn(t, vm, "rev-parse", "--is-shallow-repository"), "true"; got != want {
		t.Errorf("not shallow: %s", got)
	}
	before, _ := os.ReadFile(filepath.Join(shallow, ".git", "shallow")) //nolint:gosec // the test's own files
	gitIn(t, vm, "commit", "-q", "--allow-empty", "-m", "vm")
	gitIn(t, shallow, "fetch", "-q", vm, "+refs/heads/*:refs/remotes/devvm/*")
	if got := gitIn(t, shallow, "log", "--format=%s", "-1", "devvm/main"); got != "vm" {
		t.Errorf("fetched %q", got)
	}
	if after, _ := os.ReadFile(filepath.Join(shallow, ".git", "shallow")); !bytes.Equal(before, after) { //nolint:gosec // the test's own files
		t.Errorf("host's shallow file changed: %q, was %q", after, before)
	}
}

// A seed that runs again after one that failed part-way, or in a VM whose
// earlier devvm's push failed and whose folder then got files: it never
// moves a branch the VM moved, naming it, moves one only an earlier try
// of the seed set, keeps the VM's own files, naming those that differ,
// and marks the repo done.
func TestSeedAgain(t *testing.T) {
	gitEnv(t)
	root := t.TempDir()
	host, vm := filepath.Join(root, "host"), filepath.Join(root, "vm")
	gitIn(t, root, "init", "-q", "-b", "main", host)
	for _, f := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(host, f), []byte("host "+f+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, host, "add", ".")
	gitIn(t, host, "commit", "-q", "-m", "one")
	gitIn(t, host, "branch", "other")
	gitIn(t, host, "branch", "third")
	if _, err := seedInto(t, host, vm); err != nil {
		t.Fatal(err)
	}
	// As if it failed before its end: pending, its refs file still there.
	if err := os.WriteFile(filepath.Join(vm, ".git", seedMarker), []byte("pending\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	seeded := gitIn(t, vm, "rev-parse", "main")
	if err := os.WriteFile(filepath.Join(vm, ".git", "devvm-seeded-refs"), []byte(seeded+" refs/heads/main\n"+seeded+" refs/heads/third\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The VM's own: a commit on other, a changed file and an index.
	gitIn(t, vm, "checkout", "-q", "other")
	gitIn(t, vm, "commit", "-q", "--allow-empty", "-m", "vm's own")
	vmOther := gitIn(t, vm, "rev-parse", "other")
	gitIn(t, vm, "checkout", "-q", "main")
	if err := os.WriteFile(filepath.Join(vm, "a"), []byte("the VM's\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The host moves on.
	gitIn(t, host, "commit", "-q", "--allow-empty", "-m", "two")
	gitIn(t, host, "branch", "-f", "other")
	gitIn(t, host, "branch", "-f", "third")
	out, err := seedInto(t, host, vm)
	if err != nil {
		t.Fatal(err)
	}
	k := parseKept(out)
	if !slices.Equal(k.refs, []string{"other"}) || !slices.Equal(k.files, []string{"a"}) {
		t.Errorf("kept %+v from %q", k, out)
	}
	if got := gitIn(t, vm, "rev-parse", "other"); got != vmOther {
		t.Errorf("moved the VM's branch to %s", got)
	}
	for _, b := range []string{"main", "third"} {
		if got, want := gitIn(t, vm, "rev-parse", b), gitIn(t, host, "rev-parse", b); got != want {
			t.Errorf("%s at %s, want the host's %s", b, got, want)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(vm, "a")); string(b) != "the VM's\n" { //nolint:gosec // the test's own files
		t.Errorf("a = %q", b)
	}
	if m := seedMark(t, vm); m != "done" {
		t.Errorf("marker %q", m)
	}
}

// A seed that runs again leaves HEAD alone when the VM checked out a
// branch of its own, so git status there shows nothing the seed did.
func TestSeedAgainOwnBranch(t *testing.T) {
	gitEnv(t)
	root := t.TempDir()
	host, vm := filepath.Join(root, "host"), filepath.Join(root, "vm")
	gitIn(t, root, "init", "-q", "-b", "main", host)
	if err := os.WriteFile(filepath.Join(host, "a"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, host, "add", ".")
	gitIn(t, host, "commit", "-q", "-m", "one")
	if _, err := seedInto(t, host, vm); err != nil {
		t.Fatal(err)
	}
	seeded := gitIn(t, vm, "rev-parse", "main")
	gitIn(t, vm, "checkout", "-q", "-b", "own")
	if err := os.WriteFile(filepath.Join(vm, "a"), []byte("the VM's\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, vm, "commit", "-q", "-am", "own")
	for f, body := range map[string]string{seedMarker: "pending\n", "devvm-seeded-refs": seeded + " refs/heads/main\n"} {
		if err := os.WriteFile(filepath.Join(vm, ".git", f), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(host, "a"), []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, host, "commit", "-q", "-am", "two")
	// HEAD stays on the VM's branch, and devvm says so.
	if out, err := seedInto(t, host, vm); err != nil || out != "kept-head\nkept-files\n" {
		t.Fatalf("apply %q, %v", out, err)
	}
	if got := gitIn(t, vm, "symbolic-ref", "--short", "HEAD"); got != "own" {
		t.Errorf("HEAD %s", got)
	}
	if got := gitIn(t, vm, "status", "--porcelain"); got != "" {
		t.Errorf("status %q", got)
	}
	if got, want := gitIn(t, vm, "rev-parse", "main"), gitIn(t, host, "rev-parse", "main"); got != want {
		t.Errorf("main %s, want %s", got, want)
	}
}

// A detached HEAD is copied detached at its commit, with the tags (an
// annotated one as a tag), notes and origin/HEAD as a symref; a seed that
// runs again after the VM moved its detached HEAD keeps it and says so,
// and one after the VM checked out a branch leaves that alone.
func TestSeedDetached(t *testing.T) {
	gitEnv(t)
	root := t.TempDir()
	up, host, vm := filepath.Join(root, "up"), filepath.Join(root, "host"), filepath.Join(root, "vm")
	gitIn(t, root, "init", "-q", "-b", "main", up)
	gitIn(t, up, "commit", "-q", "--allow-empty", "-m", "one")
	gitIn(t, root, "clone", "-q", up, host)
	gitIn(t, host, "tag", "-a", "v1", "-m", "v1")
	gitIn(t, host, "notes", "add", "-m", "note", "HEAD")
	gitIn(t, host, "checkout", "-q", "--detach")
	gitIn(t, host, "commit", "-q", "--allow-empty", "-m", "detached")
	if out, err := seedInto(t, host, vm); err != nil || out != "applied-tree\nkept-files\n\x00" {
		t.Fatalf("apply %q, %v", out, err)
	}
	if _, err := stubtest.Git(t.Context(), "-C", vm, "symbolic-ref", "-q", "HEAD").Output(); err == nil {
		t.Error("HEAD isn't detached")
	}
	for _, ref := range []string{"HEAD", "refs/tags/v1", "refs/notes/commits", "refs/remotes/origin/main"} {
		if got, want := gitIn(t, vm, "rev-parse", ref), gitIn(t, host, "rev-parse", ref); got != want {
			t.Errorf("%s: %s, want %s", ref, got, want)
		}
	}
	if got := gitIn(t, vm, "cat-file", "-t", "refs/tags/v1"); got != "tag" {
		t.Errorf("v1 is a %s", got)
	}
	if got := gitIn(t, vm, "symbolic-ref", "refs/remotes/origin/HEAD"); got != "refs/remotes/origin/main" {
		t.Errorf("origin/HEAD -> %s", got)
	}
	gitIn(t, vm, "fsck", "--no-progress")
	again := func() string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(vm, ".git", seedMarker), []byte("pending\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := seedInto(t, host, vm)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	gitIn(t, vm, "commit", "-q", "--allow-empty", "-m", "vm's own")
	mine := gitIn(t, vm, "rev-parse", "HEAD")
	if k := parseKept(again()); !k.head || k.refs != nil {
		t.Errorf("kept %+v", k)
	}
	if got := gitIn(t, vm, "rev-parse", "HEAD"); got != mine {
		t.Errorf("moved the VM's detached HEAD to %s", got)
	}
	// The VM on a branch, the host detached: HEAD stays, named.
	gitIn(t, vm, "checkout", "-q", "-b", "own")
	if k := parseKept(again()); !k.head || k.refs != nil {
		t.Errorf("kept %+v", k)
	}
	if got := gitIn(t, vm, "symbolic-ref", "--short", "HEAD"); got != "own" {
		t.Errorf("HEAD %s", got)
	}
}

// A VM whose older devvm's push failed, where the agent then staged files
// in the empty repo: the seed's index is the branch's, every file of the
// branch is written but those the VM has, which are kept and named, and
// the VM's own new file stays, untracked.
func TestSeedOldIndex(t *testing.T) {
	gitEnv(t)
	root := t.TempDir()
	host, vm := filepath.Join(root, "host"), filepath.Join(root, "vm")
	gitIn(t, root, "init", "-q", "-b", "main", host)
	for _, f := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(host, f), []byte("host "+f+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, host, "add", ".")
	gitIn(t, host, "commit", "-q", "-m", "one")
	gitIn(t, root, "init", "-q", vm)
	for f, body := range map[string]string{"a": "the VM's\n", "x": "new\n"} {
		if err := os.WriteFile(filepath.Join(vm, f), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, vm, "add", ".")
	out, err := seedInto(t, host, vm)
	if err != nil {
		t.Fatal(err)
	}
	if k := parseKept(out); !slices.Equal(k.files, []string{"a"}) || k.refs != nil {
		t.Errorf("kept %+v from %q", k, out)
	}
	if got := gitIn(t, vm, "status", "--porcelain"); got != "M a\n?? x" {
		t.Errorf("status %q", got)
	}
	if b, _ := os.ReadFile(filepath.Join(vm, "b")); string(b) != "host b\n" { //nolint:gosec // the test's own files
		t.Errorf("b = %q", b)
	}
}

// initRepos's answers: a repo an earlier devvm's push seeded (a commit,
// no marker) or this seed (done) is landed; one made and never seeded is
// made again in place, pending; an empty SHA-1 repo for a SHA-256 one is
// named, then made again when asked to, and left when it has a ref; a VM
// git without --object-format names its version and stops.
func TestInitRepos(t *testing.T) {
	gitEnv(t)
	ws := t.TempDir()
	repo := func(p string, args ...string) string {
		d := filepath.Join(ws, p)
		gitIn(t, ws, append([]string{"init", "-q", "-b", "main"}, append(args, d)...)...)
		return d
	}
	pushed := repo("pushed")
	gitIn(t, pushed, "commit", "-q", "--allow-empty", "-m", "one")
	repo("failed")
	repo("old256")
	withRef := repo("withref")
	gitIn(t, withRef, "commit", "-q", "--allow-empty", "-m", "one")
	gitIn(t, withRef, "update-ref", "refs/x", "HEAD")
	gitIn(t, withRef, "update-ref", "-d", "refs/heads/main")
	out, err := runSeedScript(t, initRepos, nil, ws, "-", "pushed", "sha1", "-", "failed", "sha1", "-", "old256", "sha256", "-", "withref", "sha256", "remake", "new", "sha256", "-")
	if err != nil {
		t.Fatal(err)
	}
	seeds := []repoSeed{{path: "pushed"}, {path: "failed"}, {path: "old256"}, {path: "withref"}, {path: "new"}}
	a := parseInit(out, seeds)
	if !slices.Equal(a.landed, []string{"pushed"}) || !slices.Equal(a.format, []string{"old256", "withref"}) || a.oldGit != "" {
		t.Errorf("answer %+v from %q", a, out)
	}
	for p, want := range map[string]string{"failed": "pending", "new": "pending", "old256": "", "pushed": ""} {
		if m := seedMark(t, filepath.Join(ws, p)); m != want {
			t.Errorf("%s: marker %q, want %q", p, m, want)
		}
	}
	if got := gitIn(t, filepath.Join(ws, "new"), "rev-parse", "--show-object-format"); got != "sha256" {
		t.Errorf("new: %s", got)
	}
	if got := gitIn(t, filepath.Join(ws, "failed"), "config", "receive.shallowUpdate"); got != "true" {
		t.Errorf("failed: receive.shallowUpdate %q", got)
	}
	out, err = runSeedScript(t, initRepos, nil, ws, "-", "old256", "sha256", "remake")
	if err != nil || out != "" {
		t.Fatalf("remake: %q, %v", out, err)
	}
	if got := gitIn(t, filepath.Join(ws, "old256"), "rev-parse", "--show-object-format"); got != "sha256" {
		t.Errorf("old256 made again: %s", got)
	}
	if err := os.WriteFile(filepath.Join(ws, "new", ".git", seedMarker), []byte("done\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, _ := runSeedScript(t, initRepos, nil, ws, "-", "new", "sha256", "-"); out != "landed new\n" {
		t.Errorf("done: %q", out)
	}
	// A pending reftable repo, as a VM whose git makes reftable repos by
	// default (init.defaultRefFormat) has after a failed copy: the script
	// runs again without git init, whose re-init of a reftable repo drops
	// its extensions.refStorage (git 2.54), and the repo still commits.
	rt := filepath.Join(ws, "rt")
	if stubtest.Git(t.Context(), "init", "-q", "--ref-format=reftable", rt).Run() == nil {
		if err := os.WriteFile(filepath.Join(rt, ".git", seedMarker), []byte("pending\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := runSeedScript(t, initRepos, nil, ws, "-", "rt", "sha1", "-"); err != nil || out != "" {
			t.Errorf("rt: %q, %v", out, err)
		}
		if got := gitIn(t, rt, "rev-parse", "--show-ref-format"); got != "reftable" {
			t.Errorf("rt: ref format %s", got)
		}
		gitIn(t, rt, "commit", "-q", "--allow-empty", "-m", "still")
	}
	// A git that doesn't know --object-format, as before 2.29: git init
	// refuses the option, and rev-parse prints the one it doesn't know
	// and exits 0.
	bin := t.TempDir()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	fake := "#!/bin/sh\ncase \"$*\" in *rev-parse*--show-object-format*) echo --show-object-format; exit 0;; *--object-format*) echo \"error: unknown option\" >&2; exit 129;; *--version) echo 'git version 2.25.1'; exit 0;; esac\nexec '" + gitPath + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(fake), 0o700); err != nil { //nolint:gosec // the test's own script
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err = runSeedScript(t, initRepos, nil, ws, "-", "older", "sha256", "-", "after", "sha1", "-")
	if a := parseInit(out, []repoSeed{{path: "older"}}); err == nil || a.oldGit != "git version 2.25.1" {
		t.Errorf("old git: %q, %v", out, err)
	}
	if _, err := os.Lstat(filepath.Join(ws, "after")); err == nil {
		t.Error("went on past the old git")
	}
	if _, err := runSeedScript(t, initRepos, nil, ws, "-", "older", "sha1", "-"); err != nil {
		t.Errorf("SHA-1 with the old git: %v", err)
	}
	// The SHA-1 repo it made, pending, is the right format when asked again.
	if out, err := runSeedScript(t, initRepos, nil, ws, "-", "older", "sha1", "-"); err != nil || out != "" || seedMark(t, filepath.Join(ws, "older")) != "pending" {
		t.Errorf("a pending SHA-1 repo with the old git: %q, %v", out, err)
	}
}

// A remote reaches a devvm VM when its URL's host, in any form and case,
// is a project VM's full name or its shorter one; nothing else does.
func TestVMHost(t *testing.T) {
	const vm = "devvm-web-0123456789abcd"
	for _, c := range []struct {
		url  string
		want bool
	}{
		{vm + ":/p/web", true},
		{"stub@" + vm + ":/p/web", true},
		{"ssh://" + vm + "/p/web", true},
		{"SSH://u@DEVVM-WEB-0123456789ABCD:22/p/web", true},
		{"git+ssh://" + vm + "/p", true},
		{"devvm-web:/p/web", true},
		{"devvm-my-app:/p", true},
		{"git@github.com:me/r.git", false},
		{"git@github.com:p@" + vm + ":x", false},
		{"u@[" + vm + "]:p", true},
		{"https://" + vm + "/p", false},
		{"/p/" + vm + ":x", false},
		{"devvm-:/p", false},
		{"./" + vm + ":/p", false},
	} {
		if got := vmHost(urlHost(c.url)); got != c.want {
			t.Errorf("%s: %v, want %v (host %q)", c.url, got, c.want, urlHost(c.url))
		}
	}
}

// What the VM says to initRepos counts only for the seeds asked about,
// and a git version only as git prints one.
func TestParseInit(t *testing.T) {
	seeds := []repoSeed{{path: "."}, {path: "a b"}}
	for _, c := range []struct {
		out  string
		want initAnswer
	}{
		{"", initAnswer{}},
		{"landed .\nformat a b\n", initAnswer{landed: []string{"."}, format: []string{"a b"}}},
		{"landed elsewhere\nformat ../x\nlanded\n", initAnswer{}},
		{"oldgit git version 2.25.1\n", initAnswer{oldGit: "git version 2.25.1"}},
		{"oldgit git version 2.25.1 (Apple Git-99)\n", initAnswer{oldGit: "git version 2.25.1 (Apple Git-99)"}},
		{"oldgit \x1b[31mred\n", initAnswer{oldGit: "unknown"}},
		{"oldgit git version " + strings.Repeat("9", 70) + "\n", initAnswer{oldGit: "unknown"}},
	} {
		got := parseInit(c.out, seeds)
		if !slices.Equal(got.landed, c.want.landed) || !slices.Equal(got.format, c.want.format) || got.oldGit != c.want.oldGit {
			t.Errorf("parseInit(%q) = %+v, want %+v", c.out, got, c.want)
		}
	}
}

// What a seed's apply kept of the VM's own: branches without refs/heads/,
// then files as git gave them, a line break in one included; other lines
// are nothing. Shown, at most cloneShown names, each cut and quoted once.
func TestParseKept(t *testing.T) {
	for _, c := range []struct {
		out              string
		refs, files, pre []string
		head, tree       bool
	}{
		{
			"kept-ref refs/heads/agent/x\nkept-ref refs/remotes/origin/main\nkept-ref\nother line\nkept-head\nkept-files\na\tb\x00line\nbreak\x00\x00dir/f\x00",
			[]string{"agent/x", "refs/remotes/origin/main"},
			[]string{"a\tb", "line\nbreak", "dir/f"},
			nil,
			true, false,
		},
		{"kept-files\n", nil, nil, nil, false, false},
		{"kept-files\nkept-ref x\x00kept-head\x00", nil, []string{"kept-ref x", "kept-head"}, nil, false, false},
		{"", nil, nil, nil, false, false},
		// After applied-tree, an empty name ends the files, and pre's follow.
		{"applied-tree\nkept-files\na\x00b\x00\x00p\x00", nil, []string{"a", "b"}, []string{"p"}, false, true},
		{"applied-tree\nkept-files\n\x00p\x00q\x00", nil, nil, []string{"p", "q"}, false, true},
		{"applied-tree\nkept-files\n\x00", nil, nil, nil, false, true},
	} {
		got := parseKept(c.out)
		if !slices.Equal(got.refs, c.refs) || !slices.Equal(got.files, c.files) || !slices.Equal(got.pre, c.pre) || got.head != c.head || got.tree != c.tree {
			t.Errorf("parseKept(%q) = %+v", c.out, got)
		}
	}
	names := make([]string, cloneShown+5)
	for i := range names {
		names[i] = "n"
	}
	if s := shown(names); strings.Count(s, `"n"`) != cloneShown {
		t.Errorf("shown %q", s)
	}
	if s := shown([]string{"a\nb", strings.Repeat("é", shownMax)}); s != `"a\nb" "`+strings.Repeat("é", shownMax/2)+`…"` {
		t.Errorf("shown %q", s)
	}
}

// The seed's tar: refs, HEAD and, for a shallow repo alone, shallow,
// then the settings' entries; tips each once.
func TestSeedTar(t *testing.T) {
	a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)
	for _, c := range []struct {
		data  seedData
		names []string
	}{
		{seedData{refs: []seedRef{{a, "refs/heads/main"}, {a, "refs/heads/x"}, {b, "refs/heads/y"}}, head: "refs/heads/main"}, []string{"refs", "HEAD", "config", "config.keys"}},
		{seedData{refs: []seedRef{{a, "refs/heads/main"}}, head: "refs/heads/main", shallow: []byte(a + "\n")}, []string{"refs", "HEAD", "shallow", "config", "config.keys"}},
	} {
		var buf bytes.Buffer
		if err := c.data.writeTar(&buf); err != nil {
			t.Fatal(err)
		}
		tr := tar.NewReader(&buf)
		var names []string
		for {
			h, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				t.Fatal(err)
			}
			names = append(names, h.Name)
			if h.Name == "HEAD" {
				if body, _ := io.ReadAll(tr); string(body) != c.data.head+"\n" {
					t.Errorf("HEAD %q", body)
				}
			}
		}
		if !slices.Equal(names, c.names) {
			t.Errorf("entries %q, want %q", names, c.names)
		}
	}
	d := seedData{refs: []seedRef{{a, "refs/heads/main"}, {b, "refs/heads/x"}, {a, "refs/heads/y"}}}
	if got := d.tips(); got != a+"\n"+b+"\n" {
		t.Errorf("tips %q", got)
	}
}

// readSeed reads each ref's object, an annotated tag's as the tag, a
// symref as one when its target goes too, a detached HEAD's commit, and
// the shallow file; it leaves out a branch that's no commit, refuses no
// branch to check out, and reads no shallow file through a link.
func TestReadSeed(t *testing.T) {
	gitEnv(t)
	root := t.TempDir()
	dir := filepath.Join(root, "r")
	gitIn(t, root, "init", "-q", "-b", "main", dir)
	gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "one")
	head := gitIn(t, dir, "rev-parse", "HEAD")
	// git's update-ref refuses a branch at a tree; a hand-edited repo can
	// have one.
	if err := os.WriteFile(filepath.Join(dir, ".git", "refs", "heads", "tree"), []byte(gitIn(t, dir, "rev-parse", "HEAD^{tree}")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "tag", "-a", "v1", "-m", "v1")
	tag := gitIn(t, dir, "rev-parse", "refs/tags/v1")
	gitIn(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
	gitIn(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	gitIn(t, dir, "symbolic-ref", "refs/remotes/other/HEAD", "refs/remotes/other/main")
	// Its target is asked for, but isn't sent (no commit): nor is it.
	gitIn(t, dir, "symbolic-ref", "refs/remotes/x/HEAD", "refs/heads/tree")
	const main = "refs/heads/main"
	same := func(refs ...string) []refPair {
		var p []refPair
		for _, r := range refs {
			p = append(p, refPair{r, r})
		}
		return p
	}
	s, err := readSeed(t.Context(), dir, main, same(main, "refs/heads/tree", "refs/tags/v1", "refs/remotes/origin/HEAD", "refs/remotes/origin/main", "refs/remotes/other/HEAD", "refs/remotes/x/HEAD"))
	want := []seedRef{{head, main}, {tag, "refs/tags/v1"}, {head, "refs/remotes/origin/main"}}
	if err != nil || !slices.Equal(s.refs, want) || s.head != main || s.shallow != nil {
		t.Fatalf("readSeed = %+v, %v", s, err)
	}
	if !slices.Equal(s.symrefs, []seedSymref{{"refs/remotes/origin/HEAD", "refs/remotes/origin/main"}}) {
		t.Errorf("symrefs %+v", s.symrefs)
	}
	if got := s.tips(); got != head+"\n"+tag+"\n" {
		t.Errorf("tips %q", got)
	}
	if _, err := readSeed(t.Context(), dir, main, []refPair{{"refs/heads/none", main}}); err == nil {
		t.Error("read a branch to check out that the repo hasn't")
	}
	if _, err := readSeed(t.Context(), dir, "refs/heads/other", same(main)); err == nil {
		t.Error("no branch to check out, read")
	}
	// A detached HEAD: its commit is what's checked out, and a tip.
	gitIn(t, dir, "checkout", "-q", "--detach")
	gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "detached")
	detached := gitIn(t, dir, "rev-parse", "HEAD")
	if s, err := readSeed(t.Context(), dir, "", same(main)); err != nil || s.head != detached || s.tips() != head+"\n"+detached+"\n" {
		t.Errorf("detached: %+v, %v", s, err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "shallow"), []byte(head+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := readSeed(t.Context(), dir, main, same(main)); err != nil || string(s.shallow) != head+"\n" {
		t.Errorf("shallow %q, %v", s.shallow, err)
	}
	secret := filepath.Join(root, "secret")
	if err := os.WriteFile(secret, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(dir, ".git", "shallow"))
	if err := os.Symlink(secret, filepath.Join(dir, ".git", "shallow")); err != nil {
		t.Fatal(err)
	}
	if s, err := readSeed(t.Context(), dir, main, same(main)); err == nil {
		t.Errorf("read a shallow file through a link: %q", s.shallow)
	}
}

// The seed copies a partial clone, whichever way its config says so
// (partialClone), but can't copy a repo with a grafts file, which is
// refused before a VM is made.
func TestCheckShape(t *testing.T) {
	gitEnv(t)
	for _, c := range []struct {
		name    string
		set     func(dir string)
		want    string
		partial bool
	}{
		{"plain", func(string) {}, "", false},
		{"promisor", func(d string) { gitIn(t, d, "config", "remote.origin.promisor", "true") }, "", true},
		{"promisor yes", func(d string) { gitIn(t, d, "config", "remote.origin.promisor", "yes") }, "", true},
		{"promisor false", func(d string) { gitIn(t, d, "config", "remote.origin.promisor", "false") }, "", false},
		{"extension", func(d string) {
			gitIn(t, d, "config", "core.repositoryformatversion", "1")
			gitIn(t, d, "config", "extensions.partialClone", "origin")
		}, "", true},
		{"grafts", func(d string) {
			if err := os.WriteFile(filepath.Join(d, ".git", "info", "grafts"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}, "clone_grafts path=", false},
	} {
		dir, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		gitIn(t, dir, "init", "-q", "-b", "main")
		gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "one")
		c.set(dir)
		got := msg.NamesOf(checkCloneRepo(t.Context(), dir))
		if c.want == "" && got != "" || !strings.HasPrefix(got, c.want) {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
		if got := partialClone(t.Context(), dir); got != c.partial {
			t.Errorf("%s: partial %v", c.name, got)
		}
		if r := repoReason(t.Context(), dir); c.want != "" && r.ID() == "" {
			t.Errorf("%s: no reason in the picker", c.name)
		}
	}
}
