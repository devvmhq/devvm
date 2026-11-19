package state

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
)

func testDirs(t *testing.T) paths.Dirs {
	t.Helper()
	tmp := t.TempDir()
	return paths.Dirs{
		Data:   filepath.Join(tmp, "data", "devvm"),
		Config: filepath.Join(tmp, "config", "devvm"),
		Lima:   filepath.Join(tmp, "lima"),
	}
}

// The data and config folders are made, or made the user's alone whatever
// mode they had, and what's in them keeps its own mode.
func TestMakePrivate(t *testing.T) {
	d := testDirs(t)
	if err := os.MkdirAll(filepath.Join(d.Config, "layers"), 0o755); err != nil { //nolint:gosec // the test's own folder
		t.Fatal(err)
	}
	if err := os.Chmod(d.Config, 0o777); err != nil { //nolint:gosec // the test's own folder
		t.Fatal(err)
	}
	if err := MakePrivate(d); err != nil {
		t.Fatal(err)
	}
	for dir, want := range map[string]fs.FileMode{d.Data: 0o700, d.Config: 0o700, filepath.Join(d.Config, "layers"): 0o755} {
		if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != want {
			t.Errorf("%s: %v, want %v (%v)", dir, fi.Mode().Perm(), want, err)
		}
	}
	// One that can't be made fails.
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := MakePrivate(paths.Dirs{Data: filepath.Join(file, "devvm"), Config: d.Config}); err == nil {
		t.Error("a data folder under a file was made")
	}
}

// A CorruptError names its file and line, WriteFile writes a file whole,
// the user's alone, and removing an image refuses a bad key.
func TestFiles(t *testing.T) {
	d := testDirs(t)
	for e, want := range map[*CorruptError]string{
		{Path: "/f", Line: 3, Reason: "bad"}: "/f:3: bad", {Path: "/f", Reason: "bad"}: "/f: bad",
		{Path: "/f", Line: 2, Reason: "k: ", Msg: msg.MsgUnknown("x")}: "/f:2: k: " + msg.MsgUnknown("x").Text(),
	} {
		if e.Error() != want {
			t.Errorf("%q, want %q", e.Error(), want)
		}
		if e.Msg.ID() != "" && e.Shown(msg.M.Names) != "/f:2: k: msg_unknown name=x" {
			t.Errorf("Shown with names: %q", e.Shown(msg.M.Names))
		}
	}
	path := filepath.Join(d.Data, "sub", "f")
	if err := WriteFile(path, []byte("x")); err != nil || read(t, path) != "x" {
		t.Errorf("WriteFile: %v", err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, %v", fi.Mode().Perm(), err)
	}
	// Removing an image: a bad key is refused, and a missing one is no
	// error.
	if err := RemoveImage(d, "../x"); err == nil {
		t.Error("RemoveImage(../x): want an error")
	}
	if err := RemoveImage(d, "0123456789ab"); err != nil {
		t.Errorf("RemoveImage of none: %v", err)
	}
}

func write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // a test file
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// wantCorrupt fails unless err is a CorruptError for path at line.
func wantCorrupt(t *testing.T, err error, path string, line int) {
	t.Helper()
	var c *CorruptError
	if !errors.As(err, &c) {
		t.Fatalf("got %v, want a CorruptError", err)
	}
	if c.Path != path || c.Line != line {
		t.Errorf("got %s line %d, want %s line %d", c.Path, c.Line, path, line)
	}
}

// WriteFileMode writes the file whole with the mode asked for, whatever the
// umask, and replaces one there with another mode; nothing else is left.
func TestWriteFileMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run")
	path := filepath.Join(dir, "program")
	for _, tc := range []struct {
		data string
		mode fs.FileMode
	}{{"#!/bin/sh\n", 0o700}, {"#!/bin/sh\necho\n", 0o700}, {"data\n", 0o600}, {"cert\n", 0o644}, {"again\n", 0o755 | fs.ModeSetuid}} {
		if err := WriteFileMode(path, []byte(tc.data), tc.mode); err != nil {
			t.Fatal(err)
		}
		if got := read(t, path); got != tc.data {
			t.Errorf("got %q, want %q", got, tc.data)
		}
		if fi, err := os.Stat(path); err != nil || fi.Mode() != tc.mode.Perm() {
			t.Errorf("%v: mode %v, %v", tc.mode, fi.Mode(), err)
		}
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 {
		t.Errorf("the folder holds %v, %v", entries, err)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("folder: %v, %v", fi.Mode(), err)
	}
	// A folder that can't be made fails.
	if err := WriteFileMode(filepath.Join(path, "x"), nil, 0o700); err == nil {
		t.Error("written under a file")
	}
	// One that can't be renamed over leaves no temporary file.
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "f"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileMode(sub, []byte("x"), 0o700); err == nil {
		t.Error("a folder replaced")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Errorf("left %v", entries)
	}
}

// WriteFileIn writes a file whole in a root, with the mode asked for, its
// folders made 700 where they aren't, and replaces a link at its name
// rather than follow it; a link on the way that leads out of the root
// fails, and nothing is written outside it; nothing else is left.
func TestWriteFileIn(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	for _, mode := range []fs.FileMode{0o600, 0o700, 0o755 | fs.ModeSetuid} {
		if err := WriteFileIn(root, filepath.Join("a", "b", "f"), []byte(mode.String()), mode); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, "a", "b", "f")
		if fi, err := os.Stat(p); err != nil || fi.Mode() != mode.Perm() || read(t, p) != mode.String() {
			t.Errorf("%v: %v, %v", mode, fi, err)
		}
	}
	if fi, err := os.Stat(filepath.Join(dir, "a")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("its folder: %v, %v", fi, err)
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "a", "b")); len(entries) != 1 {
		t.Errorf("left %v", entries)
	}
	// A link at its name is replaced; one on the way out of the root fails.
	if err := os.Symlink(filepath.Join(outside, "victim"), filepath.Join(dir, "a", "l")); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileIn(root, filepath.Join("a", "l"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(filepath.Join(dir, "a", "l")); err != nil || !fi.Mode().IsRegular() {
		t.Errorf("the link: %v, %v", fi, err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "out")); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileIn(root, filepath.Join("out", "f"), []byte("x"), 0o600); err == nil {
		t.Error("written through a link out of the root")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("written outside the root: %v", entries)
	}
	// A write that fails at its rename, onto a folder, leaves no temporary file.
	if err := os.MkdirAll(filepath.Join(dir, "c", "d", "e"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileIn(root, filepath.Join("c", "d"), []byte("x"), 0o600); err == nil {
		t.Error("written over a folder")
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "c")); len(entries) != 1 {
		t.Errorf("left %v", entries)
	}
}

// PrivateDir makes a folder, with those above it, 700; corrects one there
// with another mode; and refuses a file, or a link to a folder, leaving
// what the link points to as it was.
func TestPrivateDir(t *testing.T) {
	tmp := t.TempDir()
	made := filepath.Join(tmp, "a", "b")
	if err := PrivateDir(made); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(made); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Errorf("made: %v, %v", fi.Mode(), err)
	}
	open := filepath.Join(tmp, "open")
	if err := os.Mkdir(open, 0o755); err != nil { //nolint:gosec // the mode PrivateDir corrects
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(open, "f"), nil, 0o644); err != nil { //nolint:gosec // a mode left as it is
		t.Fatal(err)
	}
	if err := PrivateDir(open); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]fs.FileMode{open: 0o700, filepath.Join(open, "f"): 0o644} {
		if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != want {
			t.Errorf("%s: %v, want %v (%v)", p, fi.Mode(), want, err)
		}
	}
	file := filepath.Join(tmp, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(tmp, "target")
	if err := os.Mkdir(target, 0o755); err != nil { //nolint:gosec // a mode a link mustn't change
		t.Fatal(err)
	}
	link := filepath.Join(tmp, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{file, link, filepath.Join(file, "under")} {
		if err := PrivateDir(bad); err == nil {
			t.Errorf("%s: made private", bad)
		}
	}
	if fi, err := os.Stat(target); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("the link's folder: %v, %v", fi.Mode(), err)
	}
}

// A folder a write makes is made as PrivateDir makes it, 700 whatever the
// umask; one already there is left as it is. Not parallel: the umask is
// the process's.
func TestWriteFileMakesPrivateDir(t *testing.T) {
	tmp := t.TempDir()
	old := syscall.Umask(0o277) // MkdirAll alone would make it 500
	t.Cleanup(func() { syscall.Umask(old) })
	path := filepath.Join(tmp, "made", "f")
	if err := WriteFile(path, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Dir(path)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("the folder made: %v, %v", fi, err)
	}
	open := filepath.Join(tmp, "open")
	if err := os.Mkdir(open, 0o755); err != nil { //nolint:gosec // a mode left as it is
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o755); err != nil { //nolint:gosec // a mode left as it is
		t.Fatal(err)
	}
	if err := WriteFile(filepath.Join(open, "f"), []byte("x")); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(open); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("a folder there: %v, %v", fi, err)
	}
	// A folder it can't make fails the write.
	shut := filepath.Join(tmp, "shut")
	if err := os.Mkdir(shut, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(shut, 0o700) }) //nolint:gosec // a folder: owner-only with search
	if err := WriteFile(filepath.Join(shut, "sub", "f"), nil); err == nil {
		t.Error("wrote below a folder it can't write")
	}
}

func TestWriteFileIsAtomicAndPrivate(t *testing.T) {
	d := testDirs(t)
	path := d.VM(vmP).State()
	for _, data := range []string{"a=1\n", "a=2\n"} {
		if err := writeFile(path, []byte(data)); err != nil {
			t.Fatal(err)
		}
		if got := read(t, path); got != data {
			t.Errorf("got %q, want %q", got, data)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("want only the state file, got %v", entries)
	}
	for path, want := range map[string]fs.FileMode{path: 0o600, filepath.Dir(path): 0o700, d.VMs(): 0o700} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s: mode %o, want %o", path, got, want)
		}
	}
}
