package vm

import (
	"slices"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/state"
)

// Parse checks each size as its flag gives it, naming the flag.
func TestParseSizes(t *testing.T) {
	if _, err := (Size{CPUs: "1.5"}).Parse(); msg.NamesOf(err) != `cpus_not_number flag=--cpus value="1.5"` {
		t.Errorf("Parse with bad CPUs: %v", msg.NamesOf(err))
	}
	s, err := Size{CPUs: "6", Memory: "12G", Disk: "150"}.Parse()
	if err != nil || s != (Size{CPUs: "6", Memory: "12", Disk: "150"}) {
		t.Errorf("Parse = %+v, %v", s, err)
	}
	if _, err := (Size{Disk: "x"}).Parse(); msg.NamesOf(err) != `size_not_gib flag=--disk value="x"` {
		t.Errorf("Parse with a bad disk: %v", msg.NamesOf(err))
	}
}

// The flags, else the defaults, else nothing (the image's).
func TestSizeOr(t *testing.T) {
	got := Size{CPUs: "6"}.Or(Size{CPUs: "2", Memory: "16"})
	if got != (Size{CPUs: "6", Memory: "16"}) {
		t.Errorf("Or = %+v", got)
	}
}

func TestSizeCheck(t *testing.T) {
	h := Host{CPUs: 8, MemoryGiB: 32}
	for _, tc := range []struct {
		s   Size
		err string
	}{
		{Size{CPUs: "16"}, "too_many_cpus have=8 want=16"},
		{Size{Memory: "64"}, "too_much_memory have=32 max=30 want=64"},
		{Size{Memory: "32"}, "too_much_memory have=32 max=30 want=32"},
		{Size{Memory: "30.5"}, "too_much_memory have=32 max=30 want=30.5"},
		{Size{Disk: "50"}, "disk_shrink have=100 vm=v want=50"},
		{Size{CPUs: "8", Memory: "30", Disk: "200"}, ""},
		{Size{}, ""},
	} {
		if err := tc.s.Check(h, 100, "v"); msg.NamesOf(err) != tc.err {
			t.Errorf("Check(%+v) = %v, want %s", tc.s, msg.NamesOf(err), tc.err)
		}
	}
	// The memory a VM may have keeps 2 GiB for the host, or half of it on
	// a host of 4 GiB or less.
	for _, tc := range []struct {
		host int
		mem  string
		err  string
	}{
		{8, "8", "too_much_memory have=8 max=6 want=8"},
		{8, "7", "too_much_memory have=8 max=6 want=7"},
		{8, "6", ""},
		{4, "2", ""},
		{4, "3", "too_much_memory have=4 max=2 want=3"},
		{3, "1.5", ""},
		{3, "2", "too_much_memory have=3 max=1.5 want=2"},
	} {
		if err := (Size{Memory: tc.mem}).Check(Host{CPUs: 8, MemoryGiB: tc.host}, 0, "v"); msg.NamesOf(err) != tc.err {
			t.Errorf("Check(memory %s on %d GiB) = %v, want %q", tc.mem, tc.host, msg.NamesOf(err), tc.err)
		}
	}
}

// Backend reads each part set as a number, and refuses one that isn't a
// size, naming its key and value, rather than reading it as 0 (the
// image's) or as infinity.
func TestSizeBackend(t *testing.T) {
	if b, err := (Size{CPUs: "2", Memory: "1.5", Disk: "120"}).Backend(); err != nil || b != (backend.Size{CPUs: 2, MemoryGiB: 1.5, DiskGiB: 120}) {
		t.Errorf("Backend = %+v, %v", b, err)
	}
	if b, err := (Size{}).Backend(); err != nil || b != (backend.Size{}) {
		t.Errorf("Backend of none = %+v, %v", b, err)
	}
	huge := "1" + strings.Repeat("0", 400)
	for s, want := range map[Size]string{
		{CPUs: "four"}:                 `cpus_not_number flag=cpus value="four"`,
		{CPUs: "99999999999999999999"}: `cpus_not_number flag=cpus value="99999999999999999999"`,
		{Memory: "8GB"}:                `size_not_gib flag=memory value="8GB"`,
		{Memory: huge}:                 `size_not_gib flag=memory value="` + huge + `"`,
		{CPUs: "2", Disk: "NaN"}:       `size_not_gib flag=disk value="NaN"`,
		{Disk: "-50"}:                  `size_not_gib flag=disk value="-50"`,
	} {
		if b, err := s.Backend(); b != (backend.Size{}) || msg.NamesOf(err) != want {
			t.Errorf("Backend(%+v) = %+v, %v, want %s", s, b, msg.NamesOf(err), want)
		}
	}
}

// A size in a VM's state that isn't one (a state file edited by hand) is
// refused, naming the VM, the key and the value: devvm size shows and
// sets nothing else, and the VM doesn't start; devvm size --cpus sets it
// again.
func TestStoredSizeInvalid(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	st := existingVM(t, e, Options{Network: "open"})
	vm := st.Name()
	change(t, st, func(st *state.VM) error { return state.SetSection(st, sizeSection, Size{CPUs: "four", Memory: "8"}) })
	want := `size_stored_invalid key=cpus value="four" vm=` + vm.String()
	// Running: a restart refuses before it stops the VM, which keeps running.
	before := len(f.Calls())
	if err := (Launch{e}).Restart(t.Context(), Options{}); msg.NamesOf(err) != want {
		t.Errorf("Restart: %s, want %s", msg.NamesOf(err), want)
	}
	if ms := f.Methods()[before:]; slices.Contains(ms, "Stop") || slices.Contains(ms, "Start") || f.Instances[vm].Status != backend.StatusRunning {
		t.Errorf("restart refused: calls %q, status %s", ms, f.Instances[vm].Status)
	}
	if err := f.Stop(t.Context(), vm); err != nil {
		t.Fatal(err)
	}
	if _, err := (Settings{e}).ShowSize(t.Context(), false); msg.NamesOf(err) != want {
		t.Errorf("ShowSize: %s, want %s", msg.NamesOf(err), want)
	}
	before = len(f.Calls())
	if err := (Launch{e}).Up(t.Context(), Options{}); msg.NamesOf(err) != want {
		t.Errorf("Up: %s, want %s", msg.NamesOf(err), want)
	}
	if ms := f.Methods()[before:]; slices.Contains(ms, "Start") {
		t.Errorf("started: %q", ms)
	}
	if err := (Settings{e}).SetSize(t.Context(), Size{CPUs: "2"}, false); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if err := (Launch{e}).Up(t.Context(), Options{}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if c := call(f, "Start"); c.Args[1].(backend.Want).Size != (backend.Size{CPUs: 2, MemoryGiB: 8}) {
		t.Errorf("started wanting %+v", c.Args[1])
	}
}

func TestSizeText(t *testing.T) {
	for s, want := range map[backend.Size]string{
		{CPUs: 4, MemoryGiB: 8, DiskGiB: 100}: "cpus=4 memory=8GiB disk=100GiB",
		{MemoryGiB: 1.5}:                      "memory=1.5GiB",
		{}:                                    "",
	} {
		if got := SizeText(s); got != want {
			t.Errorf("SizeText(%+v) = %q, want %q", s, got, want)
		}
	}
}

// devvm size: a VM's size changes at its next start, only where it
// differs, and shows as pending until then. A smaller disk, or a size
// the backend can't set, is refused before the state changes. -g changes
// the size file and keeps what it leaves unset.
func TestSetSize(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	s := Settings{e}
	shown := func(global bool) (string, error) {
		got, err := s.ShowSize(t.Context(), global)
		return got.Text(), err
	}
	if _, err := shown(false); msg.NamesOf(err) != "no_vm_size dir="+e.Dir {
		t.Errorf("no VM: %s", msg.NamesOf(err))
	}
	st := existingVM(t, e, Options{Network: "open"})
	vm := st.Name()
	in := f.Instances[vm]
	in.Size = backend.Size{CPUs: 4, MemoryGiB: 8, DiskGiB: 100}
	f.Instances[vm] = in

	before := len(f.Calls())
	if err := s.SetSize(t.Context(), Size{CPUs: "2", Memory: "8G"}, false); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if ms := f.Methods()[before:]; slices.Contains(ms, "Start") || slices.Contains(ms, "Stop") {
		t.Errorf("a running VM was changed: %q", ms)
	}
	if st, _ := state.ReadVM(e.Dirs, vm); get(st, KeyCPUs) != "2" || get(st, KeyMemory) != "8" {
		t.Errorf("state cpus=%q memory=%q", get(st, KeyCPUs), get(st, KeyMemory))
	}
	st, _ = state.ReadVM(e.Dirs, vm)
	if p, err := pendingSize(st, f.Instances[vm].Size); err != nil || p != (backend.Size{CPUs: 2}) {
		t.Errorf("pending = %+v, %v", p, err)
	}

	if err := s.SetSize(t.Context(), Size{Disk: "50"}, false); msg.NamesOf(err) != "disk_shrink have=100 vm="+vm.String()+" want=50" {
		t.Errorf("smaller disk: %s", msg.NamesOf(err))
	}
	caps := allNative()
	caps[backend.FeatureDisk] = backend.None
	f.Capabilities = caps
	if err := s.SetSize(t.Context(), Size{Disk: "200"}, false); !strings.HasPrefix(msg.NamesOf(err), "backend_lacks ") {
		t.Errorf("without the feature: %s", msg.NamesOf(err))
	}
	if st, _ := state.ReadVM(e.Dirs, vm); get(st, KeyDisk) != "" {
		t.Errorf("disk=%q", get(st, KeyDisk))
	}
	f.Capabilities = allNative()

	if err := f.Stop(t.Context(), vm); err != nil {
		t.Fatal(err)
	}
	var said msg.Recorder
	fake(e).OnInfo = said.Add
	before = len(f.Calls())
	if err := s.SetSize(t.Context(), Size{Disk: "120"}, false); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if ms := f.Methods()[before:]; slices.Contains(ms, "Start") || !slices.Equal(said.Names(), []string{"size_pending size=cpus=2 disk=120GiB vm=" + vm.String()}) {
		t.Errorf("stopped: calls %q, said %q", ms, said.Names())
	}
	// Its next start gives it its size.
	if err := (Launch{e}).Up(t.Context(), Options{}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if c := call(f, "Start"); c.Args[1].(backend.Want).Size != (backend.Size{CPUs: 2, MemoryGiB: 8, DiskGiB: 120}) {
		t.Errorf("started wanting %+v", c.Args[1])
	}
	if got, err := shown(false); err != nil || got != "cpus=2 memory=8GiB disk=120GiB\n" {
		t.Errorf("ShowSize = %q, %v", got, msg.NamesOf(err))
	}

	if err := s.SetSize(t.Context(), Size{CPUs: "2", Memory: "3"}, true); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if err := s.SetSize(t.Context(), Size{Memory: "4GiB"}, true); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if err := s.SetSize(t.Context(), Size{CPUs: "16"}, true); msg.NamesOf(err) != "too_many_cpus have=8 want=16" {
		t.Errorf("-g --cpus 16: %s", msg.NamesOf(err))
	}
	if got, err := shown(true); err != nil || got != "cpus=2 # devvm: settings.json\nmemory=4 # devvm: settings.json\ndisk= # devvm: default\n" {
		t.Errorf("ShowSize -g = %q, %v", got, msg.NamesOf(err))
	}
	writeFile(t, e.Dirs.ConfigFile(), "[size]\ndisk = \"80GiB\"\n")
	if got, err := shown(true); err != nil || !strings.HasSuffix(got, "disk=80 # "+e.Dirs.ConfigFile()+"\n") {
		t.Errorf("ShowSize -g with config.toml's disk = %q, %v", got, msg.NamesOf(err))
	}
	if g, err := s.ShowSize(t.Context(), true); err != nil || g.Sources[KeyDisk] != "config.toml" || g.Sources[KeyCPUs] != "settings.json" {
		t.Errorf("sources %v, %v", g.Sources, err)
	}
}
