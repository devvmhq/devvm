package paths

import (
	"os"
	"path/filepath"
	"testing"
)

// Physical resolves every link as far as the path exists; PhysicalParent
// only the folder's.
func TestPhysical(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "dir")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ in, physical, parent string }{
		{link, dir, link},
		{filepath.Join(link, "sub"), filepath.Join(dir, "sub"), filepath.Join(dir, "sub")},
		{filepath.Join(link, "new", "deeper"), filepath.Join(dir, "new", "deeper"), filepath.Join(link, "new", "deeper")},
		{filepath.Join(link, "sub", "..", "sub"), filepath.Join(dir, "sub"), filepath.Join(dir, "sub")},
		{"/", "/", "/"},
	} {
		if got := Physical(tc.in); got != tc.physical {
			t.Errorf("Physical(%s) = %s, want %s", tc.in, got, tc.physical)
		}
		if got := PhysicalParent(tc.in); got != tc.parent {
			t.Errorf("PhysicalParent(%s) = %s, want %s", tc.in, got, tc.parent)
		}
	}
	if got := Physical("relative/none"); got != "relative/none" {
		t.Errorf("Physical of a path that never resolves = %s", got)
	}
}

// In and Under find a path in a folder by name, as Within and Below do,
// and by identity too: on a volume that ignores case, such as macOS's, a
// name in another case is the same folder. An empty folder holds nothing.
func TestInAndUnder(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "dir")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		path, dir string
		in, under bool
	}{
		{dir, dir, true, false},
		{filepath.Join(dir, "sub"), dir, true, true},
		{filepath.Join(dir, "none", "deeper"), dir, true, true},
		{dir + "x", dir, false, false},
		{root, dir, false, false},
		{"/", "/", true, false},
		{dir, "", false, false},
	}
	if _, err := os.Stat(filepath.Join(root, "DIR")); err == nil {
		cases = append(cases, []struct {
			path, dir string
			in, under bool
		}{
			{filepath.Join(root, "DIR"), dir, true, false},
			{filepath.Join(root, "Dir", "sub"), dir, true, true},
			{filepath.Join(root, "Dir", "SUB", "none"), dir, true, true},
			{dir, filepath.Join(root, "DIR"), true, false},
			{filepath.Join(root, "DIRx"), dir, false, false},
		}...)
	} else {
		t.Log("this volume tells names apart by case: only the cases by name ran")
	}
	for _, tc := range cases {
		if got := In(tc.path, tc.dir); got != tc.in {
			t.Errorf("In(%s, %s) = %v", tc.path, tc.dir, got)
		}
		if got := Under(tc.path, tc.dir); got != tc.under {
			t.Errorf("Under(%s, %s) = %v", tc.path, tc.dir, got)
		}
	}
}

// Within is the folder or inside it; Below only inside it.
func TestWithinAndBelow(t *testing.T) {
	for _, tc := range []struct {
		path, dir     string
		within, below bool
	}{
		{"/a/b", "/a/b", true, false},
		{"/a/b/c", "/a/b", true, true},
		{"/a/b/c", "/a/b/", true, true},
		{"/a/bc", "/a/b", false, false},
		{"/a", "/a/b", false, false},
		{"/a", "/", true, true},
		{"/", "/", true, false},
		{"a/b", "a", true, true},
	} {
		if got := Within(tc.path, tc.dir); got != tc.within {
			t.Errorf("Within(%s, %s) = %v", tc.path, tc.dir, got)
		}
		if got := Below(tc.path, tc.dir); got != tc.below {
			t.Errorf("Below(%s, %s) = %v", tc.path, tc.dir, got)
		}
	}
}
