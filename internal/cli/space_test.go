package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/backend/backendtest"
	"github.com/devvmhq/devvm/internal/build"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/ui"
	"github.com/devvmhq/devvm/internal/vm"
	"github.com/devvmhq/devvm/internal/vmname"
)

// Counting a folder: only what's picked, and a folder that isn't there is
// empty.
func TestEntriesIn(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"a.tar.gz", "b.tar.gz", "c.txt"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{"v1", ".tmp-1"} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	c, err := filesIn(dir, func(n string) bool { return strings.HasSuffix(n, ".tar.gz") })
	if err != nil || c.count != 2 || c.size <= 0 {
		t.Errorf("filesIn = %+v, %v; want 2 files", c, err)
	}
	if c, err := dirsIn(dir); err != nil || c.count != 1 {
		t.Errorf("dirsIn = %+v, %v; want 1 folder", c, err)
	}
	if c, err := dirsIn(filepath.Join(dir, "gone")); err != nil || c.count != 0 {
		t.Errorf("dirsIn of a missing folder = %+v, %v", c, err)
	}
}

// An orphan's leftover state is its whole folder: its state, logs and
// run files, the temporary files of its unfinished writes; a VM
// its backend has is none, and its logs are logs. Orphans are what
// devvm gc --orphans would forget: a VM whose backend didn't answer is
// never one, nor one whose state changed in the last hour, which may be
// being made.
func TestLeftoverFolders(t *testing.T) {
	d := paths.Dirs{Data: t.TempDir()}
	const orphan, live, unasked, recent vmname.VM = "devvm-a-0123456789abc1", "devvm-b-0123456789abc2", "devvm-c-0123456789abc3", "devvm-e-0123456789abc5"
	for vm, be := range map[vmname.VM]string{orphan: "fake", live: "fake", unasked: "other", recent: "fake"} {
		v := d.VM(vm)
		for _, f := range []string{v.Logs() + "/network.jsonl", v.Proxy(".pid"), v.ForwarderLog(), filepath.Join(v.Run(), ".proxy.json.tmp-1")} {
			if err := state.WriteFile(f, []byte(strings.Repeat("x", 5000))); err != nil {
				t.Fatal(err)
			}
		}
		st, err := state.New(d, vm)
		if err != nil {
			t.Fatal(err)
		}
		st.Image, st.Dir, st.Backend = "base", "/p", be
		if err := st.Create(t.Context(), nil); err != nil {
			t.Fatal(err)
		}
	}
	// A VM whose state can't be read is never an orphan either: its logs
	// are logs.
	broken := d.VM("devvm-d-0123456789abc4")
	for f, data := range map[string]string{broken.State(): "[]", broken.EventLog(): strings.Repeat("x", 5000)} {
		if err := state.WriteFile(f, []byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	fake := backendtest.New("fake", nil)
	fake.Instances[live] = backend.Instance{Name: live, Status: backend.StatusStopped}
	other := backendtest.New("other", nil)
	other.Errs = map[string]error{"List": errors.New("no")}
	reg, err := backend.NewRegistry(fake, other)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	for _, n := range []vmname.VM{orphan, live, unasked} {
		if err := os.Chtimes(d.VM(n).State(), old, old); err != nil {
			t.Fatal(err)
		}
	}
	inv, err := vm.ReadInventory(t.Context(), d, reg, vm.AllBackends)
	if err != nil {
		t.Fatal(err)
	}
	left, err := vm.Leftovers{Env: &vm.Env{Dirs: d, Registry: reg}}.FindIn(t.Context(), inv)
	if err != nil {
		t.Fatal(err)
	}
	e := spaceEnv{b: &build.Env{Dirs: d}, inv: inv, answered: map[string]bool{"fake": true}, left: left, now: time.Now()}
	e.ret.OrphansAge = time.Hour
	row := e.leftovers()
	if row.Count != 1 || row.Size != du(d.VM(orphan).Dir) || row.Size == 0 || row.RetainFrees != row.Size {
		t.Errorf("leftovers = %+v; want %s's folder, %d", row, orphan, du(d.VM(orphan).Dir))
	}
	logs, err := e.vmLogs()
	// Every log of theirs: the network logs and the port forwarders'.
	var want int64
	for _, v := range []paths.VM{d.VM(live), d.VM(unasked), d.VM(recent)} {
		want += du(v.EventLog()) + du(v.ForwarderLog())
	}
	want += du(broken.EventLog())
	if err != nil || logs.count != 7 || logs.size != want {
		t.Errorf("vmLogs = %+v, %v; want 7, %d", logs, err, want)
	}
}

// The table: rows, a total, and the caches apart; what a row lacks is -.
func TestWriteSpace(t *testing.T) {
	r := spaceReport{
		Rows: []spaceRow{
			{What: "images", Backend: "lima", Count: 2, Size: 2048, Sized: true, Kept: keptRemade, GC: 1024, Rest: "images rm"},
			{What: "VMs", Backend: "fake", Count: 1, Kept: keptOnlyCopy, Rest: "devvm rm"},
			{What: "history", Count: 1, Size: 1024, Sized: true, Kept: keptOnlyCopy, Rest: "gc --history", Retain: "30d", RetainFrees: 1024},
		},
		Total:  2048,
		GC:     1024,
		Caches: []cacheRow{{Backend: "lima", Name: "download cache", Count: 1, Size: 1 << 30, Sized: true, Path: "/c/lima"}},
	}
	got, colored := tables(t, func(tab *ui.Table) error { return writeSpace(tab, r) })
	want := "WHAT     COUNT  SIZE  KEPT       GC    FREED BY      RETAIN\n" +
		"images   2      2.0K  remade     1.0K  images rm     -\n" +
		"VMs      1      -     only copy  -     devvm rm      -\n" +
		"history  1      1.0K  only copy  -     gc --history  30d (1.0K)\n" +
		"total    -      2.0K  -          1.0K  -             -\n" +
		"\n" +
		"CACHE           COUNT  SIZE  PATH\n" +
		"download cache  1      1.0G  /c/lima\n"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	wantCodes(t, colored, "\033[1mWHAT\033[0m", "\033[1mtotal\033[0m", "\033[1m2.0K\033[0m", "\033[33monly copy\033[0m", "  remade  ", "\033[1mCACHE\033[0m")
}
