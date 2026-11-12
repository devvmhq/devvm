package vm

import (
	"archive/tar"
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/msg"
)

// lfsFile writes an LFS pointer for content at name in the repo at dir,
// and, with store, its object in the repo's .git/lfs/objects, as git-lfs
// leaves them, and returns its oid.
func lfsFile(t *testing.T, dir, name, content string, store bool) string {
	t.Helper()
	sum := sha256.Sum256([]byte(content))
	oid := hex.EncodeToString(sum[:])
	writeIn(t, dir, name, "version https://git-lfs.github.com/spec/v1\noid sha256:"+oid+"\nsize "+strconv.Itoa(len(content))+"\n")
	if store {
		writeIn(t, dir, filepath.Join(".git", "lfs", "objects", oid[:2], oid[2:4], oid), content)
	}
	return oid
}

// A pointer is git-lfs's: its version first, an oid sha256 of 64
// lowercase hex characters and a size, each once, more keys allowed, and
// a newline at its end; anything else isn't one.
func TestParseLFSPointer(t *testing.T) {
	oid := "4d7a214614ab2935c943f9e0ff69d22eadbb8f32b1258daaa5e2ca24d17e2393"
	v := "version https://git-lfs.github.com/spec/v1\n"
	for _, c := range []struct {
		in string
		ok bool
	}{
		{v + "oid sha256:" + oid + "\nsize 12345\n", true},
		{v + "ext-0-foo sha256:" + oid + "\noid sha256:" + oid + "\nsize 1\n", true},
		{v + "oid sha256:" + oid + "\nsize 12345", false},
		{v + "oid sha256:" + oid[:63] + "\nsize 1\n", false},
		{v + "oid sha256:" + oid[:63] + "G\nsize 1\n", false},
		{v + "oid sha1:" + oid + "\nsize 1\n", false},
		{v + "oid sha256:" + oid + "\noid sha256:" + oid + "\nsize 1\n", false},
		{v + "oid sha256:" + oid + "\nsize -1\n", false},
		{"version 2\noid sha256:" + oid + "\nsize 1\n", false},
		{v + "size 1\n", false},
		{v + "oid sha256:" + oid + "\nsize 0\n", true},
		{v + "oid sha256:" + oid + "\n", false},
		{v + "oid sha256:" + oid + "\nx-other 1\n", false},
		{v + "oid sha256:" + oid + "\nsize 0\nsize 5\n", false},
		{v + "oid sha256:" + oid + "\nsize 1x\n", false},
		{"hello\n", false},
	} {
		o, ok := parseLFSPointer([]byte(c.in))
		if ok != c.ok || ok && o.oid != oid {
			t.Errorf("%q: %+v, %v", c.in, o, ok)
		}
	}
}

// The seed sends the LFS objects the index's pointers name that the repo
// has, into the VM's .git/lfs/objects, each checked against its oid; one
// the repo lacks isn't sent, and one that isn't what its pointer names
// fails the seed rather than go.
func TestSeedLFS(t *testing.T) {
	dir := safetyRepo(t, map[string]string{".gitattributes": "*.bin filter=lfs diff=lfs merge=lfs -text\n"})
	have := lfsFile(t, dir, "a.bin", "the content\n", true)
	lacks := lfsFile(t, dir, "b.bin", "not here\n", false)
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-q", "-m", "lfs")
	vm := seedOK(t, dir)
	if b, err := os.ReadFile(filepath.Join(vm, ".git", "lfs", "objects", have[:2], have[2:4], have)); err != nil || string(b) != "the content\n" { //nolint:gosec // the test's own files
		t.Errorf("the object in the VM: %q, %v", b, err)
	}
	if _, err := os.Lstat(filepath.Join(vm, ".git", "lfs", "objects", lacks[:2], lacks[2:4], lacks)); err == nil {
		t.Error("an object the repo lacks is in the VM")
	}
	if got := gitIn(t, vm, "status", "--porcelain"); got != "" {
		t.Errorf("status:\n%s", got)
	}
	// What the repo has under the oid isn't that content: named, not sent.
	ids := []string{gitIn(t, dir, "rev-parse", ":a.bin")}
	at := filepath.Join(".git", "lfs", "objects", have[:2], have[2:4], have)
	writeIn(t, dir, at, "the c0ntent\n")
	s, err := readLFS(t.Context(), dir, ids)
	if err != nil || len(s.objs) != 0 || !slices.Equal(s.bad, []string{have[:12]}) {
		t.Fatalf("a tampered object: %+v, %v", s, err)
	}
	// One that changes after it's read fails the seed.
	writeIn(t, dir, at, "the content\n")
	if s, err = readLFS(t.Context(), dir, ids); err != nil || len(s.objs) != 1 {
		t.Fatalf("read %+v, %v", s, err)
	}
	writeIn(t, dir, at, "the c0ntent\n")
	if err := writeLFS(tar.NewWriter(&bytes.Buffer{}), s); !errors.Is(err, errLFSObject) {
		t.Errorf("changed since: %v", err)
	}
	// With lfs.storage, none go.
	gitIn(t, dir, "config", "lfs.storage", "/elsewhere")
	if s, err = readLFS(t.Context(), dir, ids); err != nil || !s.elsewhere || len(s.objs) != 0 {
		t.Errorf("lfs.storage: %+v, %v", s, err)
	}
}

// lfsHeader reads lfsSend's line for the object it's asked for: its
// size, - for one the VM lacks, or ! for one of another size; anything
// else breaks the stream.
func TestLFSHeader(t *testing.T) {
	o := lfsObject{oid: strings.Repeat("a", 64), size: 12}
	for _, c := range []struct {
		in   string
		want lfsState
		fail bool
	}{
		{o.oid + " 12\n", lfsThere, false},
		{o.oid + " -\n", lfsAbsent, false},
		{o.oid + " !\n", lfsDiffers, false},
		{o.oid + " 13\n", 0, true},
		{o.oid + " 012\n", 0, true},
		{o.oid + " x\n", 0, true},
		{strings.Repeat("b", 64) + " 12\n", 0, true},
		{o.oid + " 12", 0, true},
		{"", 0, true},
	} {
		got, err := lfsHeader(bufio.NewReader(strings.NewReader(c.in)), o)
		if (err != nil) != c.fail || !c.fail && got != c.want {
			t.Errorf("%q: %v, %v", c.in, got, err)
		}
	}
}

// devvm clone fetch brings the LFS objects that the VM's new commits'
// pointers name and this repo lacks, each checked: one that's what its
// pointer names goes into .git/lfs/objects; one that isn't, or that the
// VM lacks too, stays out, named.
func TestFetchLFS(t *testing.T) {
	e, _, vmfs := cloneEnv(t)
	writeIn(t, e.Dir, ".gitattributes", "*.bin filter=lfs diff=lfs merge=lfs -text\n")
	gitIn(t, e.Dir, "add", "-A")
	gitIn(t, e.Dir, "commit", "-q", "-m", "attributes")
	existingVM(t, e, Options{Network: "open", Clone: true})
	clone := vmfs + e.Dir
	good := lfsFile(t, clone, "good.bin", "from the VM\n", true)
	bad := lfsFile(t, clone, "bad.bin", "what it names\n", false)
	writeIn(t, clone, filepath.Join(".git", "lfs", "objects", bad[:2], bad[2:4], bad), "something else\n")
	gone := lfsFile(t, clone, "gone.bin", "nowhere\n", false)
	gitIn(t, clone, "checkout", "-q", "-b", "vm-lfs")
	gitIn(t, clone, "add", "-A")
	gitIn(t, clone, "commit", "-q", "-m", "lfs")
	var warned msg.Recorder
	fake(e).OnWarn = warned.Add
	if _, err := (Clone{e}).Fetch(t.Context()); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	host := filepath.Join(e.Dir, ".git", "lfs", "objects")
	if b, err := os.ReadFile(filepath.Join(host, good[:2], good[2:4], good)); err != nil || string(b) != "from the VM\n" { //nolint:gosec // the test's own files
		t.Errorf("the good object: %q, %v", b, err)
	}
	for _, oid := range []string{bad, gone} {
		if _, err := os.Lstat(filepath.Join(host, oid[:2], oid[2:4], oid)); err == nil {
			t.Errorf("%s came back", oid)
		}
	}
	want := []string{bad[:12], gone[:12]}
	slices.Sort(want)
	if !slices.Contains(warned.Names(), `clone_lfs_kept count=2 objects="`+want[0]+`" "`+want[1]+`" path=.`) {
		t.Errorf("warned %q", warned.Names())
	}
	if tmp, _ := os.ReadDir(filepath.Join(e.Dir, ".git", "lfs", "tmp")); len(tmp) != 0 {
		t.Errorf("left in lfs/tmp: %v", tmp)
	}
}
