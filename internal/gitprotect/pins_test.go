package gitprotect

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/msg"
)

// What a Linux start pins for a VM that's on: .git and its other git
// dirs fixed, what a lock covers read-only, as lock made it (a missing
// hooks folder made first); nothing through a symlink, nothing for a VM
// that's off, and nothing on the host.
func TestPins(t *testing.T) {
	needGit(t)
	f := newFixture(t, "p")
	f.p.Locker = Binds{}
	f.git(f.root, "init", "-q", "--template=", f.dir)
	mkdirs(t, f.path(".git/modules/s/objects"), f.path(".git/modules/s/refs"))
	write(t, f.path(".git/modules/s/HEAD"), "")
	write(t, f.path(".git/modules/s/config"), "")
	if err := f.newVM(false); err != nil {
		t.Fatal(err)
	}
	if Mode(f.vm) != On || len(f.warns.Names()) > 0 {
		t.Fatalf("new VM: %s, %q", Mode(f.vm), f.warns.Names())
	}
	fixed, readOnly, err := f.p.Pins(t.Context(), f.vm)
	if err != nil || !slices.Equal(fixed, []string{".git", ".git/modules/s"}) ||
		!slices.Equal(readOnly, []string{".git/hooks", ".git/config", ".git/modules/s/hooks", ".git/modules/s/config"}) {
		t.Errorf("pins %q %q, %v", fixed, readOnly, err)
	}
	// A symlinked config is the VM's to have planted: never pinned.
	other := filepath.Join(f.root, "other")
	write(t, other, "")
	if err := os.Remove(f.path(".git/modules/s/config")); err != nil {
		t.Fatal(err)
	}
	symlink(t, other, f.path(".git/modules/s/config"))
	if _, readOnly, _ := f.p.Pins(t.Context(), f.vm); slices.Contains(readOnly, ".git/modules/s/config") {
		t.Errorf("pinned through a symlink: %q", readOnly)
	}
	// A config, or a hook, with another name: refused, a pin covering
	// only one of them.
	for _, l := range [][2]string{{".git/config", "cfg-copy"}, {".git/hooks/pre-commit", "hook-copy"}} {
		write(t, f.path(l[0]), "x")
		if err := os.Link(f.path(l[0]), f.path(l[1])); err != nil {
			t.Fatal(err)
		}
		if _, _, err := f.p.Pins(t.Context(), f.vm); msg.NamesOf(err) != "git_pin_linked path="+f.path(l[0])+" vm="+f.vm.Name().String() {
			t.Errorf("%s linked: %v", l[0], msg.NamesOf(err))
		}
		if err := os.Remove(f.path(l[1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.p.Set(t.Context(), f.vm, Off); err != nil {
		t.Fatal(err)
	}
	if fixed, readOnly, err := f.p.Pins(t.Context(), f.vm); len(fixed)+len(readOnly) > 0 || err != nil {
		t.Errorf("off: %q %q, %v", fixed, readOnly, err)
	}
}

// Pins leave the guest a window after git on the host replaces a pinned
// file, so a stopped VM's .git is watched as well: its first sums are
// recorded when it's made, and a check names what changed since.
func TestBindsWatch(t *testing.T) {
	needGit(t)
	f := newFixture(t, "p")
	f.p.Locker = Binds{}
	f.git(f.dir, "init", "-q")
	if err := f.newVM(false); err != nil {
		t.Fatal(err)
	}
	if w := f.section().Watched; !w.Scanned || w.Sums[".git/config"] == "" {
		t.Fatalf("first sums: %+v", w)
	}
	f.git(f.dir, "config", "core.fsmonitor", "./evil")
	if w := f.check(); !slices.Equal(w, []string{"git_changed paths=.git/config vm=" + f.vm.Name().String()}) {
		t.Errorf("changed: %q", w)
	}
	if w := f.check(); len(w) > 0 {
		t.Errorf("again: %q", w)
	}
}

// What a VM's last run recorded of its pins is named once it stops, each
// once, and the record goes; nothing without one.
func TestReportPinEvents(t *testing.T) {
	f := newFixture(t, "p")
	file := filepath.Join(f.root, "git-pins.jsonl")
	f.p.ReportPinEvents(file, "vm-1")
	again := "git_pin_again path=" + f.path(".git/config") + " vm=vm-1"
	unrecorded := "git_pins_unrecorded vm=vm-1"
	for _, c := range []struct {
		name, record string
		want         []string
	}{
		// Two runs; a path's repeats are one line.
		{"whole", `{"what":"keeping"}
{"path":".git/config","what":"pinned-again"}
{"path":".git/config","what":"pinned-again"}
{"path":".git/hooks","what":"linked"}
{"what":"keeping"}
{"path":".git/x","what":"lost"}
{"path":".git/config","what":"pinned-again"}
`, []string{
			again,
			"git_pin_linked_run path=" + f.path(".git/hooks") + " vm=vm-1",
			"git_pin_lost path=" + f.path(".git/x") + " vm=vm-1",
		}},
		{"nothing changed", `{"what":"keeping"}` + "\n", nil},
		{"not an event", `{"what":"keeping"}` + "\nnot json\n", []string{unrecorded}},
		{"unrecorded", `{"what":"keeping"}` + "\n" + `{"path":".git/config","what":"unrecorded"}` + "\n", []string{unrecorded}},
		// Made again while it ran, so its start went.
		{"lost its start", `{"path":".git/config","what":"pinned-again"}` + "\n", []string{again, unrecorded}},
		{"too long a line", `{"what":"keeping"}` + "\n" + strings.Repeat("x", 70*1024) + "\n", []string{unrecorded}},
	} {
		f.warns.Reset()
		write(t, file, c.record)
		f.p.ReportPinEvents(file, "vm-1")
		if !slices.Equal(f.warns.Names(), c.want) {
			t.Errorf("%s: warned %q, want %q", c.name, f.warns.Names(), c.want)
		}
		if _, err := os.Stat(file); err == nil {
			t.Errorf("%s: the record stays", c.name)
		}
	}
	// One it can't open (below a file: not a folder) is named, and kept.
	f.warns.Reset()
	write(t, file, `{"what":"keeping"}`+"\n")
	f.p.ReportPinEvents(filepath.Join(file, "x"), "vm-1")
	if !slices.Equal(f.warns.Names(), []string{unrecorded}) {
		t.Errorf("unreadable: warned %q", f.warns.Names())
	}
	if _, err := os.Stat(file); err != nil {
		t.Errorf("unreadable: %v", err)
	}
}

// A running VM that's on misses what its process doesn't see read-only
// of .git's and its recorded git dirs' paths; a backend that can't tell,
// or a VM that's off, misses nothing.
func TestPinsMissing(t *testing.T) {
	needGit(t)
	f := newFixture(t, "p")
	f.p.Locker = Binds{}
	f.git(f.root, "init", "-q", "--template=", f.dir)
	if err := f.newVM(false); err != nil {
		t.Fatal(err)
	}
	if got := f.p.PinsMissing(t.Context(), f.vm, nil, nil); got != nil {
		t.Errorf("can't tell: %q", got)
	}
	ro := []string{f.path(".git/hooks"), f.path(".git/config")}
	mounted := append([]string{f.path(".git")}, ro...)
	if got := f.p.PinsMissing(t.Context(), f.vm, []string{}, []string{}); !slices.Equal(got, mounted) {
		t.Errorf("none pinned: %q", got)
	}
	if got := f.p.PinsMissing(t.Context(), f.vm, ro, mounted); len(got) > 0 {
		t.Errorf("all pinned: %q", got)
	}
	// .git not a mount of its own: it can be renamed away.
	if got := f.p.PinsMissing(t.Context(), f.vm, ro, ro); !slices.Equal(got, []string{f.path(".git")}) {
		t.Errorf(".git not pinned: %q", got)
	}
	// Mounted, but not read-only.
	if got := f.p.PinsMissing(t.Context(), f.vm, ro[:1], mounted); !slices.Equal(got, ro[1:]) {
		t.Errorf("config writable: %q", got)
	}
	if err := record(t.Context(), f.vm, func(g *InVM) { g.Mode = Off }); err != nil {
		t.Fatal(err)
	}
	if got := f.p.PinsMissing(t.Context(), f.vm, []string{}, []string{}); len(got) > 0 {
		t.Errorf("off: %q", got)
	}
}

// On a Linux host that pins, a VM left off by a host that couldn't lock
// is told it can be locked; off chosen, or a clone, says nothing.
func TestWarnLockable(t *testing.T) {
	f := linuxFixture(t)
	mkdirs(t, f.path(".git/hooks"))
	f.p.Locker = Binds{}
	f.p.WarnUnprotected(f.vm)
	if want := []string{"git_protect_available vm=" + f.vm.Name().String()}; !slices.Equal(f.warns.Names(), want) {
		t.Errorf("off by the host: %q", f.warns.Names())
	}
	for _, g := range []InVM{{Mode: Off, Chosen: true}, {Mode: Clone}, {Mode: On}} {
		f.warns.Reset()
		if err := record(t.Context(), f.vm, func(s *InVM) { *s = g }); err != nil {
			t.Fatal(err)
		}
		f.p.WarnUnprotected(f.vm)
		if len(f.warns.Names()) > 0 {
			t.Errorf("%+v: %q", g, f.warns.Names())
		}
	}
}
