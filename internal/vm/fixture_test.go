package vm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/gitprotect"
	"github.com/devvmhq/devvm/internal/netpolicy"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/ports"
	"github.com/devvmhq/devvm/internal/project"
	"github.com/devvmhq/devvm/internal/secrets"
	"github.com/devvmhq/devvm/internal/settings"
	"github.com/devvmhq/devvm/internal/sshagent"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/tools"
)

// stateFixture is a release's state, frozen in the repo
// (testdata/v<release>/GOOS, which that release itself made: never
// regenerated), with what the tests expect of it.
type stateFixture struct {
	release string // "0.1.0", in testdata/v0.1.0
	// vms are its VMs' slugs, and stateVersion the version of their
	// vm.json files.
	vms          []string
	stateVersion int
	// layout is its layout version file, which today's check of the
	// layout must leave as it is.
	layout string
	// clones are the slugs of its VMs with a clone of their folder's own
	// repo alone.
	clones []string
	// checkVMs checks what each of its VMs reads as today, checkDefaults
	// its defaults, and checkAgents its agent store.
	checkVMs      func(t *testing.T, goos string, vms map[string]*state.VM)
	checkDefaults func(t *testing.T, def settings.Defaults)
	checkAgents   func(t *testing.T, d paths.Dirs)
}

// stateFixtures are the releases with a frozen state, oldest first: one
// for each release that changed what devvm leaves on disk.
func stateFixtures() []stateFixture { return []stateFixture{v010Fixture()} }

// fixtureCopy is one system's copy of a release's state, in a folder of
// the test's own, so nothing a reader writes (the layout's lock) lands in
// the repo.
type fixtureCopy struct {
	stateFixture
	goos string
	dirs paths.Dirs
}

// name is the copy's subtest name: its release and system.
func (c fixtureCopy) name() string { return "v" + c.release + "/" + c.goos }

// fixtureCopies copies every system's state of every release in
// stateFixtures.
func fixtureCopies(t *testing.T) []fixtureCopy {
	t.Helper()
	var out []fixtureCopy
	for _, f := range stateFixtures() {
		found, err := filepath.Glob(filepath.Join("..", "..", "testdata", "v"+f.release, "*", "data"))
		if err != nil || len(found) == 0 {
			t.Fatalf("no fixture of %s's state: %v", f.release, err)
		}
		for _, data := range found {
			src := filepath.Dir(data)
			dst := t.TempDir()
			if err := os.CopyFS(dst, os.DirFS(src)); err != nil {
				t.Fatal(err)
			}
			out = append(out, fixtureCopy{stateFixture: f, goos: filepath.Base(src), dirs: paths.Dirs{
				Home: filepath.Join(dst, "home"), Data: filepath.Join(dst, "data", "devvm"),
				Config: filepath.Join(dst, "config", "devvm"), Lima: filepath.Join(dst, "lima"),
			}})
		}
	}
	return out
}

// fixtureVMs reads every VM of c by its slug.
func fixtureVMs(t *testing.T, c fixtureCopy) map[string]*state.VM {
	t.Helper()
	names, err := state.VMNames(c.dirs)
	if err != nil {
		t.Fatal(err)
	}
	vms := map[string]*state.VM{}
	for _, name := range names {
		st, err := state.ReadVM(c.dirs, name)
		if err != nil {
			t.Fatal(name, err)
		}
		slug, _ := name.Slug()
		vms[slug] = st
	}
	return vms
}

// Every release folder in testdata has its expectations in stateFixtures,
// and every release there has its folder, so adding a release's state is
// its folder and its entry.
func TestStateFixturesListed(t *testing.T) {
	found, err := filepath.Glob(filepath.Join("..", "..", "testdata", "v*"))
	if err != nil {
		t.Fatal(err)
	}
	var folders, listed []string
	for _, f := range found {
		if fi, err := os.Stat(f); err == nil && fi.IsDir() {
			folders = append(folders, strings.TrimPrefix(filepath.Base(f), "v"))
		}
	}
	for _, f := range stateFixtures() {
		listed = append(listed, f.release)
	}
	slices.Sort(folders)
	slices.Sort(listed)
	if len(folders) == 0 || !slices.Equal(folders, listed) {
		t.Errorf("testdata has the states of %q, stateFixtures lists %q", folders, listed)
	}
}

// fixtureSections are the sections a release wrote in a VM's state, each
// with what reads it today, kept as it reads and written again as it
// would be.
func fixtureSections() map[string]func(*state.VM) (any, error) {
	return map[string]func(*state.VM) (any, error){
		netpolicy.Section:  read(netpolicy.Of),
		gitprotect.Section: read(gitprotect.Of),
		ports.Section:      read(ports.Of),
		sshagent.Section:   read(sshagent.Of),
		secrets.Section:    read(secrets.Of),
		tools.Section:      read(tools.Of),
		sizeSection:        read(func(st *state.VM) (Size, error) { return state.Section[Size](st, sizeSection) }),
		cloneSection:       read(func(st *state.VM) (cloneInVM, error) { return state.Section[cloneInVM](st, cloneSection) }),
		linksSection:       read(func(st *state.VM) (linksInVM, error) { return state.Section[linksInVM](st, linksSection) }),
	}
}

// read is of, its section as any.
func read[T any](of func(*state.VM) (T, error)) func(*state.VM) (any, error) {
	return func(st *state.VM) (any, error) { return of(st) }
}

// asJSON is v as plain JSON values: maps, slices, strings, numbers.
func asJSON(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// Every VM state a release wrote reads today with nothing lost: its core
// fields as they are, each section read by its package and written again
// as the release wrote it, field for field, and the whole state as the
// finder checks it. A section devvm doesn't read fails, as nothing would
// keep it.
func TestFixtureVMStates(t *testing.T) {
	for _, c := range fixtureCopies(t) {
		t.Run(c.name(), func(t *testing.T) {
			names, err := state.VMNames(c.dirs)
			if err != nil || len(names) != len(c.vms) {
				t.Fatalf("VMs %q, %v, want %d", names, err, len(c.vms))
			}
			for _, name := range names {
				raw := map[string]json.RawMessage{}
				hash, _ := name.Hash()
				b, err := os.ReadFile(c.dirs.VM(name).State())
				if err != nil || json.Unmarshal(b, &raw) != nil {
					t.Fatalf("%s: %v", name, err)
				}
				st, err := state.ReadHash(c.dirs, hash)
				if err != nil || !st.Finished() || st.Name() != name {
					t.Fatalf("ReadHash(%s) = %v, %v", name, st, err)
				}
				if slug, _ := name.Slug(); !slices.Contains(c.vms, slug) {
					t.Errorf("%s: not one of %q", name, c.vms)
				}
				// The folder names the same VM today: its hash is the same.
				if h := project.Hash(st.Dir); h != hash {
					t.Errorf("%s's folder %s hashes to %s today", name, st.Dir, h)
				}
				checkFixtureCore(t, c.stateFixture, st, raw)
				checkFixtureSections(t, st, raw)
				if err := checkState(st); err != nil {
					t.Errorf("%s: checkState: %v", name, err)
				}
			}
		})
	}
}

// checkFixtureCore checks st's core fields against the file's, which f's
// release wrote.
func checkFixtureCore(t *testing.T, f stateFixture, st *state.VM, raw map[string]json.RawMessage) {
	t.Helper()
	core := map[string]any{
		"version": float64(f.stateVersion), "id": st.ID, "name": st.Name().String(), "alias": st.Alias, "image": st.Image,
		"chain": st.Chain, "dir": st.Dir, "folder": st.Folder, "backend": st.Backend,
		"backend_at": st.BackendAt, "made_by": st.MadeBy,
	}
	for k, want := range core {
		var got any
		if err := json.Unmarshal(raw[k], &got); err != nil || got != want {
			t.Errorf("%s: %s: the file has %s, read as %v", st.Name(), k, raw[k], want)
		}
	}
	if st.MadeBy != f.release || st.Created.IsZero() || st.Backend != "lima" {
		t.Errorf("%s: made by %q at %v on %q", st.Name(), st.MadeBy, st.Created, st.Backend)
	}
}

// checkFixtureSections checks each of st's sections, read and written
// again, against the file's.
func checkFixtureSections(t *testing.T, st *state.VM, raw map[string]json.RawMessage) {
	t.Helper()
	readers := fixtureSections()
	for k, b := range raw {
		if slices.Contains([]string{"version", "id", "name", "alias", "image", "chain", "from", "dir", "folder", "backend", "backend_at", "created", "made_by"}, k) {
			continue
		}
		of, ok := readers[k]
		if !ok {
			t.Errorf("%s: section %q: nothing reads it", st.Name(), k)
			continue
		}
		v, err := of(st)
		if err != nil {
			t.Errorf("%s: section %q: %v", st.Name(), k, err)
			continue
		}
		var want any
		if err := json.Unmarshal(b, &want); err != nil {
			t.Fatal(err)
		}
		if got := asJSON(t, v); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: section %q: the release wrote\n%s\nread and written again:\n%v", st.Name(), k, b, got)
		}
	}
}

// What each VM a release made reads as today, as its checkVMs has it.
func TestFixtureVMsRead(t *testing.T) {
	for _, c := range fixtureCopies(t) {
		t.Run(c.name(), func(t *testing.T) { c.checkVMs(t, c.goos, fixtureVMs(t, c)) })
	}
}

// A release's defaults read today as it saved them, as its checkDefaults
// has them, and checking its layout changes nothing.
func TestFixtureSettings(t *testing.T) {
	for _, c := range fixtureCopies(t) {
		t.Run(c.name(), func(t *testing.T) {
			def, err := settings.Read(c.dirs)
			if err != nil {
				t.Fatal(err)
			}
			c.checkDefaults(t, def)
			before, _ := os.ReadFile(c.dirs.Version())
			if err := state.CheckLayout(t.Context(), c.dirs); err != nil {
				t.Errorf("CheckLayout: %v", err)
			}
			if after, _ := os.ReadFile(c.dirs.Version()); string(after) != string(before) || string(before) != c.layout {
				t.Errorf("layout version %q, then %q, want %q", before, after, c.layout)
			}
		})
	}
}

// A release's agent store, as its checkAgents has it.
func TestFixtureAgentStore(t *testing.T) {
	for _, c := range fixtureCopies(t) {
		t.Run(c.name(), func(t *testing.T) { c.checkAgents(t, c.dirs) })
	}
}
