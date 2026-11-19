//go:build unix

package safefile

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"
)

// countFS is fsys, opening only (no ReadDir or Stat of its own, so a
// walk reads each folder through what Open gives), counting the entries
// its folders hand out (read).
type countFS struct {
	fsys fs.FS
	read *int
}

func (c countFS) Open(name string) (fs.File, error) {
	f, err := c.fsys.Open(name)
	if err != nil {
		return nil, err
	}
	if d, ok := f.(fs.ReadDirFile); ok {
		return countDir{d, c.read}, nil
	}
	return f, nil
}

type countDir struct {
	fs.ReadDirFile
	read *int
}

func (d countDir) ReadDir(n int) ([]fs.DirEntry, error) {
	es, err := d.ReadDirFile.ReadDir(n)
	*d.read += len(es)
	return es, err
}

// tree is a folder with folders, files and an empty folder.
func tree() fstest.MapFS {
	return fstest.MapFS{
		"a/b/c.txt": {}, "a/b/d.txt": {}, "a/e.txt": {}, "a/f/g/h.txt": {}, "a/i": {Mode: fs.ModeDir}, "z.txt": {},
	}
}

// Walk meets what fs.WalkDir meets, in the same order, and does as it does
// with fn's SkipDir and SkipAll, its oracle the standard library's own
// walk; its bound counts every entry it read, the root's included.
func TestWalkAsWalkDir(t *testing.T) {
	fsys := tree()
	for _, tc := range []struct {
		name string
		root string
		stop func(p string, d fs.DirEntry) error
	}{
		{"all", ".", func(string, fs.DirEntry) error { return nil }},
		{"from a folder", "a", func(string, fs.DirEntry) error { return nil }},
		{"a file", "z.txt", func(string, fs.DirEntry) error { return nil }},
		{"SkipDir on a folder", ".", func(p string, _ fs.DirEntry) error {
			if p == "a/b" {
				return fs.SkipDir
			}
			return nil
		}},
		{"SkipDir on a file", ".", func(p string, _ fs.DirEntry) error {
			if p == "a/b/c.txt" {
				return fs.SkipDir
			}
			return nil
		}},
		{"SkipAll", ".", func(p string, _ fs.DirEntry) error {
			if p == "a/e.txt" {
				return fs.SkipAll
			}
			return nil
		}},
	} {
		var want, got []string
		wantErr := fs.WalkDir(fsys, tc.root, func(p string, d fs.DirEntry, _ error) error {
			want = append(want, p)
			return tc.stop(p, d)
		})
		b := &Bound{Max: 100}
		gotErr := Walk(fsys, tc.root, b, func(p string, d fs.DirEntry, _ error) error {
			got = append(got, p)
			return tc.stop(p, d)
		})
		if !slices.Equal(got, want) || !errors.Is(gotErr, wantErr) {
			t.Errorf("%s: met %q (%v), want %q (%v)", tc.name, got, gotErr, want, wantErr)
		}
	}
	b := &Bound{Max: 100}
	if err := Walk(fsys, ".", b, func(string, fs.DirEntry, error) error { return nil }); err != nil || b.Seen != 11 {
		t.Errorf("counted %d entries (%v), want the root and 10", b.Seen, err)
	}
	// A root that isn't there is fn's to judge, as fs.WalkDir's is.
	var gotErr error
	err := Walk(fsys, "nowhere", &Bound{Max: 1}, func(_ string, _ fs.DirEntry, err error) error { gotErr = err; return err })
	if !errors.Is(err, fs.ErrNotExist) || !errors.Is(gotErr, fs.ErrNotExist) {
		t.Errorf("no root: %v, fn got %v", err, gotErr)
	}
}

// A folder of many entries is read a batch at a time and no further than
// the bound and one batch, failing with ErrTooMany before fn sees any of
// its entries; fs.WalkDir, the control, reads the folder whole first. A
// walk at its bound reads it all, and one shared bound counts several
// walks together.
func TestWalkBound(t *testing.T) {
	const n = 10 * walkBatch
	fsys := fstest.MapFS{}
	for i := range n {
		fsys["many/"+strconv.Itoa(i)] = &fstest.MapFile{}
	}
	read := 0
	met := 0
	_ = fs.WalkDir(countFS{fsys, &read}, "many", func(p string, _ fs.DirEntry, _ error) error {
		if p != "many" {
			return fs.SkipAll
		}
		return nil
	})
	if read != n {
		t.Fatalf("fs.WalkDir read %d entries before its first, want the whole folder's %d", read, n)
	}
	read = 0
	b := &Bound{Max: 100}
	err := Walk(countFS{fsys, &read}, "many", b, func(p string, _ fs.DirEntry, _ error) error {
		if p != "many" {
			met++
		}
		return nil
	})
	if !errors.Is(err, ErrTooMany) || read > 100+walkBatch || met != 0 {
		t.Errorf("past the bound: %v, read %d entries, fn met %d", err, read, met)
	}
	b = &Bound{Max: n + 1}
	if err := Walk(fsys, "many", b, func(string, fs.DirEntry, error) error { return nil }); err != nil || b.Seen != n+1 {
		t.Errorf("at the bound: %v, seen %d", err, b.Seen)
	}
	b = &Bound{Max: n + 1}
	_ = Walk(fsys, "many", b, func(string, fs.DirEntry, error) error { return nil })
	if err := Walk(fsys, "many", b, func(string, fs.DirEntry, error) error { return nil }); !errors.Is(err, ErrTooMany) {
		t.Errorf("a second walk on a spent bound: %v", err)
	}
}

// A chain of folders deeper than walkMaxDepth, or a path longer than
// walkMaxPath, stops the walk with ErrTooMany, though it holds few
// entries; a chain at the depth bound walks whole.
func TestWalkDepth(t *testing.T) {
	chain := func(depth int, name string) fstest.MapFS {
		return fstest.MapFS{strings.TrimSuffix(strings.Repeat(name+"/", depth), "/"): {Mode: fs.ModeDir}}
	}
	for _, tc := range []struct {
		name string
		fsys fstest.MapFS
		ok   bool
	}{
		{"at the depth bound", chain(walkMaxDepth, "a"), true},
		{"one deeper", chain(walkMaxDepth+1, "a"), false},
		{"a long path", chain(walkMaxPath/200+1, strings.Repeat("a", 200)), false},
	} {
		err := Walk(tc.fsys, ".", &Bound{Max: 1000}, func(_ string, _ fs.DirEntry, err error) error { return err })
		if (err == nil) != tc.ok || !tc.ok && !errors.Is(err, ErrTooMany) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}

// noDirFS opens a folder as a plain fs.File, which can't be listed.
type noDirFS struct{ fs.FS }

func (n noDirFS) Open(name string) (fs.File, error) {
	f, err := n.FS.Open(name)
	if err != nil {
		return nil, err
	}
	return struct{ fs.File }{f}, nil
}

// A folder Walk can't list goes to fn again with the error, as fs.WalkDir
// does: fn's own error ends the walk, and SkipDir goes on past the folder.
// A folder that can't be opened, or opened but not listed, is such a
// folder.
func TestWalkErrors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root lists what it can't read")
	}
	dir := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(dir, "a", "shut"), 0o700))
	must(t, os.WriteFile(filepath.Join(dir, "a", "z"), nil, 0o600))
	shut := filepath.Join(dir, "a", "shut")
	must(t, os.Chmod(shut, 0))
	t.Cleanup(func() { _ = os.Chmod(shut, 0o700) }) //nolint:gosec // G302: a folder, readable again for its cleanup
	for _, skip := range []bool{false, true} {
		var met []string
		err := Walk(os.DirFS(dir), ".", &Bound{Max: 10}, func(p string, _ fs.DirEntry, err error) error {
			if err != nil {
				met = append(met, p+" failed")
				if skip {
					return fs.SkipDir
				}
				return err
			}
			met = append(met, p)
			return nil
		})
		want := []string{".", "a", "a/shut", "a/shut failed"}
		if skip {
			want = append(want, "a/z")
		}
		if (err == nil) != skip || !slices.Equal(met, want) {
			t.Errorf("skip %v: met %q (%v), want %q", skip, met, err, want)
		}
	}
	err := Walk(noDirFS{tree()}, ".", &Bound{Max: 10}, func(_ string, _ fs.DirEntry, err error) error { return err })
	if err == nil {
		t.Error("a folder opened as a file: listed")
	}
}

// A Root's FS opens a folder only as what it looked at: one that another
// folder took the place of since fails, ErrSwapped.
func TestRootFSOpenSwapped(t *testing.T) {
	dir := t.TempDir()
	must(t, os.Mkdir(filepath.Join(dir, "a"), 0o700))
	must(t, os.Mkdir(filepath.Join(dir, "b"), 0o700))
	r, err := OpenRoot(dir)
	must(t, err)
	defer func() { _ = r.Close() }()
	other, err := os.Lstat(filepath.Join(dir, "b"))
	must(t, err)
	if f, err := (rootFS{r}).openDir("a", other); !errors.Is(err, ErrSwapped) {
		if f != nil {
			_ = f.Close()
		}
		t.Errorf("opened as another folder: %v", err)
	}
	if _, err := (rootFS{r}).openDir("missing", other); err == nil {
		t.Error("a missing folder opened")
	}
	// A FIFO swapped in for the folder after its Lstat neither holds the open nor passes.
	must(t, syscall.Mkfifo(filepath.Join(dir, "fifo"), 0o600))
	if err := within(t, "a FIFO", func() error {
		f, err := (rootFS{r}).openDir("fifo", other)
		if err == nil {
			_ = f.Close()
		}
		return err
	}); !errors.Is(err, ErrSwapped) {
		t.Errorf("a FIFO opened as the folder: %v", err)
	}
}

// A walk of a Root's FS lists its folders and never goes through a link,
// to a folder in it or out of it: a link is an entry of its own.
func TestWalkRoot(t *testing.T) {
	dir := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(dir, "a", "b"), 0o700))
	must(t, os.WriteFile(filepath.Join(dir, "a", "b", "f"), nil, 0o600))
	must(t, os.Symlink("b", filepath.Join(dir, "a", "in")))
	must(t, os.Symlink(t.TempDir(), filepath.Join(dir, "a", "out")))
	r, err := OpenRoot(dir)
	must(t, err)
	defer func() { _ = r.Close() }()
	var got []string
	err = Walk(r.FS(), ".", &Bound{Max: 10}, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		got = append(got, path.Join(p)+":"+strconv.FormatBool(d.IsDir()))
		return nil
	})
	want := []string{".:true", "a:true", "a/b:true", "a/b/f:false", "a/in:false", "a/out:false"}
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("walked %q (%v), want %q", got, err, want)
	}
}
