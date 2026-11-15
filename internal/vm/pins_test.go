package vm

import (
	"slices"
	"testing"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/backend/backendtest"
	"github.com/devvmhq/devvm/internal/gitprotect"
	"github.com/devvmhq/devvm/internal/msg"
)

// starts are the confinements the fake was started with, in order.
func starts(t *testing.T, f *backendtest.Fake) []backend.Confinement {
	t.Helper()
	var out []backend.Confinement
	for _, c := range f.Calls() {
		if c.Method == "Start" {
			out = append(out, c.Args[2].(backend.Confinement))
		}
	}
	return out
}

// On a Linux host, a VM whose .git protection is on starts with its .git
// pinned by its backend; a backend that can't pin refuses its start,
// which never goes on unlocked. Off, it starts with no pins.
func TestStartPins(t *testing.T) {
	gitEnv(t)
	e, f, _ := upEnv(t, allNative())
	e.OS = "linux"
	e.Protector = func(dir string) *gitprotect.Protector {
		return gitprotect.New(dir, e.Dirs.Home, gitprotect.Binds{}, &msg.Fake{})
	}
	gitIn(t, e.Dir, "init", "-q")
	st := existingVM(t, e, Options{Network: "open"})
	if gitprotect.Mode(st) != gitprotect.On {
		t.Fatalf("mode %s", gitprotect.Mode(st))
	}
	got := starts(t, f)
	want := backend.Pins{Root: e.Dir, Fixed: []string{".git"}, ReadOnly: []string{".git/hooks", ".git/config"}}
	if len(got) == 0 || got[0].Pins.Root != want.Root || !slices.Equal(got[0].Pins.Fixed, want.Fixed) || !slices.Equal(got[0].Pins.ReadOnly, want.ReadOnly) {
		t.Errorf("started with %+v, want %+v", got, want)
	}
	if err := (Lifecycle{e}).Stop(t.Context()); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	f.Capabilities[backend.FeatureReadOnlyPaths] = backend.None
	err := (Launch{e}).Up(t.Context(), Options{})
	if want := "backend_lacks backend=fake feature=read-only-paths vm=" + st.Name(); msg.NamesOf(err) != want {
		t.Errorf("can't pin: %s, want %s", msg.NamesOf(err), want)
	}
	if err := (Settings{e}).SetGitProtect(t.Context(), gitprotect.Off); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if err := (Launch{e}).Up(t.Context(), Options{}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if all := starts(t, f); !all[len(all)-1].Pins.Empty() {
		t.Errorf("off: pins %+v", all[len(all)-1].Pins)
	}
}
