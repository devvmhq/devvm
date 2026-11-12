package vm

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/project"
)

// The folders of .git/lfs, outermost first, on the way to oid's object.
func lfsLevels(oid string) []string {
	return []string{"lfs", "lfs/objects", "lfs/objects/" + oid[:2], "lfs/objects/" + oid[:2] + "/" + oid[2:4]}
}

// plantLink puts a link at level, below the repo at dir's .git, to a new
// folder outside it, with what's past level made in it, and returns that
// folder.
func plantLink(t *testing.T, dir, level, past string) string {
	t.Helper()
	out := t.TempDir()
	if past != "" {
		if err := os.MkdirAll(filepath.Join(out, past), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	at := filepath.Join(dir, ".git", level)
	if err := os.MkdirAll(filepath.Dir(at), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(at); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(out, at); err != nil {
		t.Fatal(err)
	}
	return out
}

// filesIn are the files below dir.
func filesIn(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	return files
}

// A link at any folder of .git/lfs on the way to an object, to a folder
// outside the repo, isn't followed: the seed reads nothing through it, so
// the object it leads to never reaches the VM.
func TestSeedLFSLinks(t *testing.T) {
	const content = "outside the repo\n"
	for i := range 4 {
		dir := safetyRepo(t, map[string]string{".gitattributes": "*.bin filter=lfs diff=lfs merge=lfs -text\n"})
		oid := lfsFile(t, dir, "a.bin", content, false)
		levels := lfsLevels(oid)
		past := strings.TrimPrefix(strings.TrimPrefix(levels[3], levels[i]), "/")
		out := plantLink(t, dir, levels[i], past)
		writeIn(t, out, filepath.Join(past, oid), content)
		gitIn(t, dir, "add", "-A")
		gitIn(t, dir, "commit", "-q", "-m", "lfs")
		vm := seedOK(t, dir)
		if _, err := os.Lstat(filepath.Join(vm, ".git", levels[3], oid)); err == nil {
			t.Errorf("a link at %s: the object reached the VM", levels[i])
		}
	}
}

// A link at any folder of .git/lfs that devvm clone fetch writes into,
// to a folder outside the repo, isn't followed: nothing from the VM is
// written there, and the object is named as one that didn't come back.
func TestFetchLFSLinks(t *testing.T) {
	for i := range 5 {
		e, _, vmfs := cloneEnv(t)
		writeIn(t, e.Dir, ".gitattributes", "*.bin filter=lfs diff=lfs merge=lfs -text\n")
		gitIn(t, e.Dir, "add", "-A")
		gitIn(t, e.Dir, "commit", "-q", "-m", "attributes")
		existingVM(t, e, Options{Network: "open", Clone: true})
		clone := vmfs + e.Dir
		oid := lfsFile(t, clone, "vm.bin", "from the VM\n", true)
		gitIn(t, clone, "checkout", "-q", "-b", "vm-lfs")
		gitIn(t, clone, "add", "-A")
		gitIn(t, clone, "commit", "-q", "-m", "lfs")
		level := append(lfsLevels(oid), "lfs/tmp")[i]
		out := plantLink(t, e.Dir, level, "")
		var warned msg.Recorder
		fake(e).OnWarn = warned.Add
		if _, err := (Clone{e}).Fetch(t.Context()); err != nil {
			t.Fatal(msg.NamesOf(err))
		}
		if got := filesIn(t, out); len(got) != 0 {
			t.Errorf("a link at %s: written through it: %q", level, got)
		}
		if !slices.ContainsFunc(warned.Names(), func(n string) bool { return strings.HasPrefix(n, "clone_lfs_kept count=1 ") }) {
			t.Errorf("a link at %s: warned %q", level, warned.Names())
		}
	}
}

// lfsWork gives the --clone VM of e a commit on vm-lfs whose LFS file's
// object its clone has, with content as its object's, and returns the
// oid and the clone.
func lfsWork(t *testing.T, e *Env, vmfs, content string) (oid, clone string) {
	t.Helper()
	writeIn(t, e.Dir, ".gitattributes", "*.bin filter=lfs diff=lfs merge=lfs -text\n")
	gitIn(t, e.Dir, "add", "-A")
	gitIn(t, e.Dir, "commit", "-q", "-m", "attributes")
	existingVM(t, e, Options{Network: "open", Clone: true})
	clone = vmfs + e.Dir
	oid = lfsFile(t, clone, "vm.bin", "what the pointer names\n", false)
	writeIn(t, clone, filepath.Join(".git", "lfs", "objects", oid[:2], oid[2:4], oid), content)
	gitIn(t, clone, "checkout", "-q", "-b", "vm-lfs")
	gitIn(t, clone, "add", "-A")
	gitIn(t, clone, "commit", "-q", "-m", "lfs")
	return oid, clone
}

// The LFS objects of commits this repo has already, fetched with plain
// git fetch devvm, which brings no LFS objects, come back with devvm
// clone fetch.
func TestFetchLFSFetchedBefore(t *testing.T) {
	e, _, vmfs := cloneEnv(t)
	oid, clone := lfsWork(t, e, vmfs, "what the pointer names\n")
	gitIn(t, e.Dir, "fetch", "-q", clone, "vm-lfs:refs/remotes/devvm/vm-lfs")
	if _, err := (Clone{e}).Fetch(t.Context()); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if b, err := os.ReadFile(filepath.Join(e.Dir, ".git", "lfs", "objects", oid[:2], oid[2:4], oid)); err != nil || string(b) != "what the pointer names\n" { //nolint:gosec // the test's own files
		t.Errorf("the object: %q, %v", b, err)
	}
}

// devvm rm names the LFS objects that only the VM's clone has, of
// commits this repo has, and without a terminal keeps the VM.
func TestRmLFSUnfetched(t *testing.T) {
	e, f, vmfs := cloneEnv(t)
	_, clone := lfsWork(t, e, vmfs, "what the pointer names\n")
	gitIn(t, e.Dir, "fetch", "-q", clone, "vm-lfs:refs/remotes/devvm/vm-lfs")
	vm := vmName(t, e)
	var said msg.Recorder
	fake(e).OnSay = said.Add
	err := (Lifecycle{e}).Rm(t.Context(), RmOptions{Force: true})
	if _, ok := f.Instances[vm]; !ok || err == nil {
		t.Fatalf("deleted: %v", msg.NamesOf(err))
	}
	if !slices.ContainsFunc(said.Names(), func(n string) bool { return strings.HasPrefix(n, "clone_lfs_unfetched count=1 ") }) {
		t.Errorf("said %q", said.Names())
	}
}

// Past lfsAskOver, devvm asks before it brings the objects back, and
// without a terminal it refuses, naming the size, and brings none.
func TestFetchLFSLarge(t *testing.T) {
	e, _, vmfs := cloneEnv(t)
	writeIn(t, e.Dir, ".gitattributes", "*.bin filter=lfs diff=lfs merge=lfs -text\n")
	gitIn(t, e.Dir, "add", "-A")
	gitIn(t, e.Dir, "commit", "-q", "-m", "attributes")
	existingVM(t, e, Options{Network: "open", Clone: true})
	clone := vmfs + e.Dir
	oid := strings.Repeat("c", 64)
	writeIn(t, clone, "big.bin", "version https://git-lfs.github.com/spec/v1\noid sha256:"+oid+"\nsize 3221225472\n")
	gitIn(t, clone, "checkout", "-q", "-b", "big")
	gitIn(t, clone, "add", "-A")
	gitIn(t, clone, "commit", "-q", "-m", "big")
	_, err := (Clone{e}).Fetch(t.Context())
	if got := msg.NamesOf(err); got != "no_terminal_lfs_large path=. size=3.0" {
		t.Errorf("no terminal: %s", got)
	}
	asked := answers(t, e, false)
	var warned msg.Recorder
	fake(e).OnWarn = warned.Add
	if _, err := (Clone{e}).Fetch(t.Context()); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if !slices.Equal(*asked, []string{"clone_lfs_large path=. size=3.0"}) || !slices.Contains(warned.Names(), `clone_lfs_kept count=1 objects="`+oid[:12]+`" path=.`) {
		t.Errorf("declined: asked %q, warned %q", *asked, warned.Names())
	}
}

// One LFS object that isn't what its pointer names stays on this
// machine, named, and the rest of the copy goes on.
func TestUpCloneLFSBad(t *testing.T) {
	e, _, vmfs := cloneEnv(t)
	writeIn(t, e.Dir, ".gitattributes", "*.bin filter=lfs diff=lfs merge=lfs -text\n")
	good := lfsFile(t, e.Dir, "good.bin", "fine\n", true)
	bad := lfsFile(t, e.Dir, "bad.bin", "what it names\n", false)
	writeIn(t, e.Dir, filepath.Join(".git", "lfs", "objects", bad[:2], bad[2:4], bad), "something else\n")
	gitIn(t, e.Dir, "add", "-A")
	gitIn(t, e.Dir, "commit", "-q", "-m", "lfs")
	var warned msg.Recorder
	fake(e).OnWarn = warned.Add
	existingVM(t, e, Options{Network: "open", Clone: true})
	objects := filepath.Join(vmfs+e.Dir, ".git", "lfs", "objects")
	if _, err := os.Lstat(filepath.Join(objects, good[:2], good[2:4], good)); err != nil {
		t.Errorf("the good object: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(objects, bad[:2], bad[2:4], bad)); err == nil {
		t.Error("the bad object went")
	}
	if !slices.Contains(warned.Names(), `clone_lfs_bad count=1 objects="`+bad[:12]+`" path=.`) {
		t.Errorf("warned %q", warned.Names())
	}
}

// devvm rm keeps the VM when an LFS object of the work it fetched, which
// the VM's clone has, didn't come back, here because of a link in
// .git/lfs; an object the VM's clone has nothing valid of doesn't keep it.
func TestRmLFSNotBack(t *testing.T) {
	e, f, vmfs := cloneEnv(t)
	lfsWork(t, e, vmfs, "what the pointer names\n")
	plantLink(t, e.Dir, "lfs/tmp", "")
	vm := vmName(t, e)
	answers(t, e, true)
	err := (Lifecycle{e}).Rm(t.Context(), RmOptions{Force: true})
	if _, ok := f.Instances[vm]; !ok || err == nil {
		t.Fatalf("deleted: %v", msg.NamesOf(err))
	}

	e, f, vmfs = cloneEnv(t)
	lfsWork(t, e, vmfs, "not what it names, nor its size\n")
	vm = vmName(t, e)
	answers(t, e, true)
	if err := (Lifecycle{e}).Rm(t.Context(), RmOptions{Force: true}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if _, ok := f.Instances[vm]; ok {
		t.Error("an invalid object kept the VM")
	}
}

// Pointers whose sizes add up past what an int64 holds still ask first,
// as objects past 2 GiB in all.
func TestFetchLFSOverflow(t *testing.T) {
	e, _, vmfs := cloneEnv(t)
	writeIn(t, e.Dir, ".gitattributes", "*.bin filter=lfs diff=lfs merge=lfs -text\n")
	gitIn(t, e.Dir, "add", "-A")
	gitIn(t, e.Dir, "commit", "-q", "-m", "attributes")
	existingVM(t, e, Options{Network: "open", Clone: true})
	clone := vmfs + e.Dir
	for _, c := range []string{"d", "e"} {
		writeIn(t, clone, c+".bin", "version https://git-lfs.github.com/spec/v1\noid sha256:"+strings.Repeat(c, 64)+"\nsize 4611686018427387904\n")
	}
	gitIn(t, clone, "checkout", "-q", "-b", "huge")
	gitIn(t, clone, "add", "-A")
	gitIn(t, clone, "commit", "-q", "-m", "huge")
	_, err := (Clone{e}).Fetch(t.Context())
	if got := msg.NamesOf(err); !strings.HasPrefix(got, "no_terminal_lfs_large ") {
		t.Errorf("no question: %s", got)
	}
}

// After a plain git fetch devvm and a merge of the VM's work into a
// branch here, which bring no LFS objects, devvm rm still names those
// only the VM's clone has, and keeps the VM without a terminal; devvm
// clone fetch brings them.
func TestRmLFSMerged(t *testing.T) {
	e, f, vmfs := cloneEnv(t)
	oid, clone := lfsWork(t, e, vmfs, "what the pointer names\n")
	gitIn(t, e.Dir, "fetch", "-q", clone, "vm-lfs:refs/remotes/devvm/vm-lfs")
	gitIn(t, e.Dir, "merge", "-q", "--ff-only", "devvm/vm-lfs")
	vm := vmName(t, e)
	var said msg.Recorder
	fake(e).OnSay = said.Add
	err := (Lifecycle{e}).Rm(t.Context(), RmOptions{Force: true})
	if _, ok := f.Instances[vm]; !ok || err == nil {
		t.Fatalf("deleted: %v", msg.NamesOf(err))
	}
	if !slices.ContainsFunc(said.Names(), func(n string) bool { return strings.HasPrefix(n, "clone_lfs_unfetched count=1 ") }) {
		t.Errorf("said %q", said.Names())
	}
	if _, err := (Clone{e}).Fetch(t.Context()); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if _, err := os.Lstat(filepath.Join(e.Dir, ".git", "lfs", "objects", oid[:2], oid[2:4], oid)); err != nil {
		t.Errorf("not fetched: %v", err)
	}
}

// A VM that writes without end to rm's check of its LFS objects is
// stopped, and the check fails rather than wait.
func TestLFSInVMBounded(t *testing.T) {
	e, f, _ := cloneEnv(t)
	st := existingVM(t, e, Options{Network: "open", Clone: true})
	f.ExecFunc = func(_ string, _ []string, o backend.ExecOpts) error {
		line := []byte(strings.Repeat("x", 4096))
		for {
			if _, err := o.Stdout.Write(line); err != nil {
				return err
			}
		}
	}
	tg := &target{f: project.Folder{Dir: e.Dir, Name: st.Name()}, b: f}
	done := make(chan error, 1)
	go func() {
		_, err := vmLFS{clones{e}, tg, e.Dir}.have(t.Context(), []lfsObject{{oid: strings.Repeat("a", 64), size: 1}})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("taken")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("still reading")
	}
}

// A failure to set git-lfs's filter up in the VM's repo is reported
// (lfs-setup-failed), never passed over.
func TestSeedLFSSetupFails(t *testing.T) {
	bin := t.TempDir()
	writeIn(t, bin, "git-lfs", "#!/bin/sh\nexit 1\n")
	if err := os.Chmod(filepath.Join(bin, "git-lfs"), 0o700); err != nil { //nolint:gosec // the test's own script
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	dir := safetyRepo(t, map[string]string{"a": "a\n"})
	out, err := seedInto(t, dir, filepath.Join(t.TempDir(), "vm"))
	if err != nil || !strings.Contains(out, "lfs-setup-failed\n") {
		t.Errorf("%q, %v", out, err)
	}
}

// An LFS object the VM's clone has whose pointer is only in the history
// copied in, such as one git lfs pull fetched there, isn't the VM's work:
// rm doesn't name it, as the walk for the VM's own objects stops at what
// the copy sent (the clone's devvm-seed-tips), not the whole history.
func TestRmLFSBeforeCopy(t *testing.T) {
	e, f, vmfs := cloneEnv(t)
	writeIn(t, e.Dir, ".gitattributes", "*.bin filter=lfs diff=lfs merge=lfs -text\n")
	oid := lfsFile(t, e.Dir, "old.bin", "pulled in the VM\n", false)
	gitIn(t, e.Dir, "add", "-A")
	gitIn(t, e.Dir, "commit", "-q", "-m", "an LFS file whose object this repo lacks")
	st := existingVM(t, e, Options{Network: "open", Clone: true})
	writeIn(t, vmfs+e.Dir, filepath.Join(".git", "lfs", "objects", oid[:2], oid[2:4], oid), "pulled in the VM\n")
	var said msg.Recorder
	fake(e).OnSay = said.Add
	if err := (Lifecycle{e}).Rm(t.Context(), RmOptions{Force: true}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if _, ok := f.Instances[st.Name()]; ok || slices.ContainsFunc(said.Names(), func(n string) bool { return strings.HasPrefix(n, "clone_lfs_unfetched") }) {
		t.Errorf("kept: said %q", said.Names())
	}
}
