package cli

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rogpeppe/go-internal/testscript"

	"github.com/devvmhq/devvm/internal/platform"
	folders "github.com/devvmhq/devvm/internal/project" // the scripts have their own project
	"github.com/devvmhq/devvm/internal/vmname"
)

// fixtureDir is where a release's frozen state is, in the repo at repo:
// testdata/VERSION/GOOS, as that release's own generate.sh left it.
func fixtureDir(repo, version string) string {
	return filepath.Join(repo, "testdata", version, runtime.GOOS)
}

// fixtureVM is what a fixture's vm.json says that depends on where it was
// made: its folder, that folder's and its .git's identities, and where its
// backend kept it.
type fixtureVM struct {
	Name      string `json:"name"`
	Dir       string `json:"dir"`
	Folder    string `json:"folder"`
	BackendAt string `json:"backend_at"`
	Git       struct {
		Made struct {
			GitID string `json:"git_id"`
		} `json:"made"`
	} `json:"git"`
}

// fixture VERSION puts the state devvm VERSION left (testdata/VERSION/GOOS)
// into the sandbox, as that devvm would have left it here: its sandbox's
// folder becomes this one, and each VM's name's hash, its folder's and
// .git's identities and its backend's place become what they are here.
// Nothing else changes. The VMs' folders must exist first. What that
// devvm's --json printed goes in $WORK/fixture/json, changed the same way.
func fixture(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) != 1 {
		ts.Fatalf("usage: fixture VERSION")
	}
	src := fixtureDir(ts.Getenv("DEVVM_REPO"), args[0])
	root := filepath.Dir(ts.Getenv("HOME"))
	r := fixtureReplacer(ts, src, root)
	modes := fixtureModes(ts, src)
	for _, top := range []string{"data", "config", "lima", "stub", "json"} {
		into := root
		if top == "json" {
			into = filepath.Join(ts.Getenv("WORK"), "fixture")
		}
		err := filepath.WalkDir(filepath.Join(src, top), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(src, path)
			if err != nil {
				return err
			}
			mode, ok := modes[rel]
			if !ok {
				ts.Fatalf("%s: no mode in %s/modes", rel, src)
			}
			dst := filepath.Join(into, r.Replace(rel))
			if d.IsDir() {
				if err := os.MkdirAll(dst, 0o700); err != nil {
					return err
				}
				return os.Chmod(dst, mode)
			}
			b, err := os.ReadFile(path) //nolint:gosec // the repo's own fixture
			if err != nil {
				return err
			}
			if err := os.WriteFile(dst, []byte(r.Replace(string(b))), mode); err != nil {
				return err
			}
			return os.Chmod(dst, mode)
		})
		ts.Check(err)
	}
}

// fixtureReplacer replaces what the fixture at src says of where it was
// made with what it is in the sandbox at root.
func fixtureReplacer(ts *testscript.TestScript, src, root string) *strings.Replacer {
	lines := strings.Fields(ts.ReadFile(filepath.Join(src, "root")))
	if len(lines) != 2 {
		ts.Fatalf("%s/root: not the sandbox's folder and its physical path", src)
	}
	physical, err := filepath.EvalSymlinks(root)
	ts.Check(err)
	// The physical path first: on macOS the other is within it.
	pairs := []string{lines[1], physical, lines[0], root}
	where := func(dir string) string { return strings.NewReplacer(pairs...).Replace(dir) }
	lima := ts.Getenv("DEVVM_LIMA_HOME")
	states, err := filepath.Glob(filepath.Join(src, "data", "devvm", "vms", "*", "vm.json"))
	ts.Check(err)
	for _, s := range states {
		var vm fixtureVM
		ts.Check(json.Unmarshal([]byte(ts.ReadFile(s)), &vm))
		hash, ok := vmname.HashOf(vm.Name)
		if !ok {
			ts.Fatalf("%s: %q isn't a project VM's name", s, vm.Name)
		}
		dir := where(vm.Dir)
		pairs = append(pairs, hash, folders.Hash(dir))
		if vm.Folder != "" {
			pairs = append(pairs, vm.Folder, folderID(ts, dir))
		}
		if id := vm.Git.Made.GitID; id != "" {
			pairs = append(pairs, id, folderID(ts, filepath.Join(dir, ".git")))
		}
		if vm.BackendAt != "" {
			if vm.BackendAt != locationHash(filepath.Join(lines[0], "lima")) {
				ts.Fatalf("%s: backend_at %s isn't its Lima home's", s, vm.BackendAt)
			}
			pairs = append(pairs, vm.BackendAt, locationHash(lima))
		}
	}
	return strings.NewReplacer(pairs...)
}

// locationHash is a backend's place as a VM's state records it
// (backend_at): the start of the SHA-256 of its home.
func locationHash(where string) string {
	sum := sha256.Sum256([]byte(where))
	return hex.EncodeToString(sum[:8])
}

// folderID is a folder's identity here, which must exist.
func folderID(ts *testscript.TestScript, dir string) string {
	id, err := platform.FolderID(dir)
	if err != nil {
		ts.Fatalf("the fixture's folder %s: %v (make it first)", dir, err)
	}
	return id
}

// jsonSuperset [-shape] WANT GOT checks that the JSON file GOT has all
// that WANT has: each of an object's keys, with a value that has all its
// value has, each of an array's elements in GOT's array, and the same
// numbers, strings and booleans; with -shape, any of the same type.
// -changes PATH takes the value at PATH (.a.b) as changed, its type kept.
// Newer devvms' --json may add, never take away.
func jsonSuperset(ts *testscript.TestScript, neg bool, args []string) {
	var c supersetCheck
	for len(args) > 2 {
		switch args[0] {
		case "-shape":
			c.shape = true
			args = args[1:]
		case "-changes":
			c.changes = append(c.changes, args[1])
			args = args[2:]
		default:
			ts.Fatalf("json_superset: unknown flag %s", args[0])
		}
	}
	if neg || len(args) != 2 {
		ts.Fatalf("usage: json_superset [-shape] [-changes PATH]... WANT GOT")
	}
	var want, got any
	ts.Check(json.Unmarshal([]byte(ts.ReadFile(args[0])), &want))
	ts.Check(json.Unmarshal([]byte(ts.ReadFile(args[1])), &got))
	if path, ok := c.hasAll(got, want, ""); !ok {
		ts.Fatalf("%s lacks %s's %s", args[1], args[0], path)
	}
}

// supersetCheck is how json_superset compares: by shape alone, or by
// value but for the paths that change.
type supersetCheck struct {
	shape   bool
	changes []string
}

// hasAll reports whether got has all that want has, as jsonSuperset
// says, and else where it doesn't, want being at path.
func (c supersetCheck) hasAll(got, want any, path string) (string, bool) {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return path, false
		}
		for k, wv := range w {
			gv, ok := g[k]
			if !ok {
				return path + "." + k, false
			}
			if p, ok := c.hasAll(gv, wv, path+"."+k); !ok {
				return p, false
			}
		}
		return "", true
	case []any:
		g, ok := got.([]any)
		if !ok {
			return path, false
		}
		for i, wv := range w {
			if !slices.ContainsFunc(g, func(gv any) bool { _, ok := c.hasAll(gv, wv, path+"[]"); return ok }) {
				return path + "[" + strconv.Itoa(i) + "]", false
			}
		}
		return "", true
	default:
		if c.shape || slices.Contains(c.changes, path) {
			return path, reflect.TypeOf(got) == reflect.TypeOf(want)
		}
		return path, got == want
	}
}

// fixtureModes reads a fixture's modes file: each path's mode, which git
// doesn't keep.
func fixtureModes(ts *testscript.TestScript, src string) map[string]os.FileMode {
	modes := map[string]os.FileMode{}
	sc := bufio.NewScanner(strings.NewReader(ts.ReadFile(filepath.Join(src, "modes"))))
	for sc.Scan() {
		mode, path, ok := strings.Cut(sc.Text(), " ")
		m, err := strconv.ParseUint(mode, 8, 32)
		if !ok || err != nil {
			ts.Fatalf("%s/modes: %q", src, sc.Text())
		}
		modes[path] = os.FileMode(m)
	}
	return modes
}

// Every release's frozen state (testdata/v<release>/) has CLI tests of
// its own, which put it in a sandbox with fixture v<release> and run
// today's commands on it, and every fixture a script names is a folder
// there. Each release's scripts are its own, as their checks are of that
// release's VMs.
func TestFixtureScripts(t *testing.T) {
	folders, err := filepath.Glob(filepath.Join("..", "..", "testdata", "v*", runtime.GOOS))
	if err != nil {
		t.Fatal(err)
	}
	scripts, err := filepath.Glob(filepath.Join(scriptsDir(), "*.txtar"))
	if err != nil {
		t.Fatal(err)
	}
	named := map[string][]string{}
	for _, s := range scripts {
		b, err := os.ReadFile(s) //nolint:gosec // the repo's own scripts
		if err != nil {
			t.Fatal(err)
		}
		script, _, _ := strings.Cut(string(b), "\n-- ")
		for line := range strings.Lines(script) {
			if f := strings.Fields(line); len(f) == 2 && f[0] == "fixture" {
				named[f[1]] = append(named[f[1]], filepath.Base(s))
			}
		}
	}
	for _, f := range folders {
		release := filepath.Base(filepath.Dir(f))
		if len(named[release]) == 0 {
			t.Errorf("testdata/%s has no CLI test: no script runs fixture %s", release, release)
		}
	}
	for release, by := range named {
		if !slices.Contains(folders, fixtureDir(filepath.Join("..", ".."), release)) {
			t.Errorf("%q run fixture %s, which isn't a folder in testdata", by, release)
		}
	}
	if len(named) == 0 {
		t.Error("no script runs fixture")
	}
}

// json_superset's check: added keys and elements pass; a key gone, a
// value changed or an element gone fails, naming where; -shape takes any
// value of the same type, -changes one at its path.
func TestHasAll(t *testing.T) {
	want := `{"a":1,"b":{"c":"x"},"l":[{"n":"p"},{"n":"q"}]}`
	for _, tc := range []struct {
		got   string
		check supersetCheck
		path  string // "" when it has all
	}{
		{got: want},
		{got: `{"a":1,"b":{"c":"x","d":2},"l":[{"n":"q"},{"n":"p","m":1},{"n":"r"}],"z":true}`},
		{got: `{"a":1,"b":{},"l":[{"n":"p"},{"n":"q"}]}`, path: ".b.c"},
		{got: `{"a":2,"b":{"c":"x"},"l":[{"n":"p"},{"n":"q"}]}`, path: ".a"},
		{got: `{"a":1,"b":{"c":"x"},"l":[{"n":"p"}]}`, path: ".l[1]"},
		{got: `{"a":1,"b":"x","l":[{"n":"p"},{"n":"q"}]}`, path: ".b"},
		{got: `{"a":2,"b":{"c":"y"},"l":[{"n":"o"}]}`, check: supersetCheck{shape: true}},
		{got: `{"a":"1","b":{"c":"x"},"l":[{"n":"p"},{"n":"q"}]}`, check: supersetCheck{shape: true}, path: ".a"},
		{got: `{"a":1,"b":{"c":"y"},"l":[{"n":"p"},{"n":"q"}]}`, check: supersetCheck{changes: []string{".b.c"}}},
		{got: `{"a":1,"b":{"c":2},"l":[{"n":"p"},{"n":"q"}]}`, check: supersetCheck{changes: []string{".b.c"}}, path: ".b.c"},
	} {
		var w, g any
		if err := json.Unmarshal([]byte(want), &w); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(tc.got), &g); err != nil {
			t.Fatal(err)
		}
		path, ok := tc.check.hasAll(g, w, "")
		if ok != (tc.path == "") || !ok && path != tc.path {
			t.Errorf("%s %+v: %q, %v; want %q", tc.got, tc.check, path, ok, tc.path)
		}
	}
}
