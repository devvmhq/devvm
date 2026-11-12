package gitprotect

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/stubtest"
)

// strictAsBefore makes the fixture's VM one that an earlier devvm's
// devvm git-protect strict left: diff.ignoreSubmodules = all in the
// .git/config of each repo at rels ("." for the folder's own), and the
// mode recorded.
func (f *fixture) strictAsBefore(rels ...string) {
	f.t.Helper()
	for _, rel := range rels {
		f.git(f.path(rel), "config", "--file", ".git/config", "diff.ignoreSubmodules", "all")
	}
	if err := record(f.t.Context(), f.vm, func(g *InVM) { g.Mode = Strict }); err != nil {
		f.t.Fatal(err)
	}
}

// ignoreIn is every diff.ignoreSubmodules value in the .git/config of
// the repo at rel, one per line.
func (f *fixture) ignoreIn(rel string) string {
	f.t.Helper()
	out, _ := stubtest.Git(f.t.Context(), "config", "--file", f.path(rel+"/.git/config"), "--get-all", "diff.ignoreSubmodules").Output()
	return strings.TrimSpace(string(out))
}

func (f *fixture) converted() []string { return []string{"git_strict_converted vm=" + f.vm.Name()} }

// A strict VM that's running keeps strict until it stops, read as on; the
// first check that finds it stopped takes diff.ignoreSubmodules = all out
// of .git/config, unlocked for the moment and locked again, records on,
// and warns once.
func TestUnstrict(t *testing.T) {
	needGit(t)
	f := newFixture(t, "p")
	f.git(f.dir, "init", "-q")
	if err := f.newVM(false); err != nil {
		t.Fatal(err)
	}
	f.strictAsBefore(".")
	cfg := f.path(".git/config")
	f.checked(true)
	if f.section().Mode != Strict || Mode(f.vm) != On || f.ignoreIn(".") != "all" || len(f.warns.Names()) > 0 {
		t.Errorf("running: mode %s, %q, warnings %q", f.section().Mode, f.ignoreIn("."), f.warns.Names())
	}
	f.checked(false)
	if f.section().Mode != On || f.ignoreIn(".") != "" || !slices.Equal(f.warns.Names(), f.converted()) {
		t.Errorf("stopped: mode %s, %q, warnings %q", f.section().Mode, f.ignoreIn("."), f.warns.Names())
	}
	i := slices.Index(f.flags, "nouchg "+cfg)
	if i < 0 || !slices.Contains(f.flags[i+1:], "uchg "+cfg) || !f.immutable[cfg] {
		t.Errorf("locks %q", f.flags)
	}
	f.checked(false)
	if len(f.warns.Names()) > 0 || slices.Contains(f.flags, "nouchg "+cfg) {
		t.Errorf("again: warnings %q, locks %q", f.warns.Names(), f.flags)
	}
}

// strict recorded nothing of a value the config had before it, so only
// its own all comes out, in a check, in devvm git-protect on and off,
// and in an unlock (devvm rm): a value set since stays.
func TestUnstrictKeepsOthers(t *testing.T) {
	needGit(t)
	for _, tc := range []struct {
		name  string
		since []string // git config arguments run after strict
		want  string
	}{
		{"strict's own", nil, ""},
		{"another set over it", []string{"diff.ignoreSubmodules", "none"}, "none"},
		{"another added", []string{"--add", "diff.ignoreSubmodules", "dirty"}, "dirty"},
	} {
		for _, do := range []string{"check", On, Off, "unlock"} {
			t.Run(tc.name+", "+do, func(t *testing.T) {
				f := newFixture(t, "p")
				f.git(f.dir, "init", "-q")
				if err := f.newVM(false); err != nil {
					t.Fatal(err)
				}
				f.strictAsBefore(".")
				if tc.since != nil {
					f.git(f.dir, append([]string{"config", "--file", ".git/config"}, tc.since...)...)
				}
				f.warns.Reset()
				switch do {
				case "check":
					f.checked(false)
				case "unlock":
					f.p.Unlock(t.Context(), f.vm)
				default:
					f.set(do)
				}
				if got := f.ignoreIn("."); got != tc.want {
					t.Errorf("diff.ignoreSubmodules %q, want %q", got, tc.want)
				}
				if f.warned("git_strict_converted") != (do == "check") {
					t.Errorf("warnings %q", f.warns.Names())
				}
			})
		}
	}
}

// The nested repos the VM's state recorded before the check lose strict's
// all too; one the check finds is new, so strict set nothing there, and
// its all is the user's.
func TestUnstrictNested(t *testing.T) {
	needGit(t)
	f := newFixture(t, "ws")
	f.repo("a")
	f.repo("b")
	f.walked()
	if err := f.newVM(false); err != nil {
		t.Fatal(err)
	}
	f.strictAsBefore("a", "b")
	f.repo("c")
	f.git(f.path("c"), "config", "--file", ".git/config", "diff.ignoreSubmodules", "all")
	f.walked()
	f.checked(false)
	if f.ignoreIn("a") != "" || f.ignoreIn("b") != "" || f.ignoreIn("c") != "all" {
		t.Errorf("a %q, b %q, c %q", f.ignoreIn("a"), f.ignoreIn("b"), f.ignoreIn("c"))
	}
	if !f.locked(f.path("c/.git/config")) || slices.Contains(f.flags, "nouchg "+f.path("c/.git/config")) {
		t.Errorf("locks %q", f.flags)
	}
	if !f.warned("git_strict_converted") || f.section().Mode != On {
		t.Errorf("warnings %q, mode %s", f.warns.Names(), f.section().Mode)
	}
}

// A config another VM's protection keeps locked too is left as it is,
// as unlocking it would leave it that VM's to race, and named with that
// VM and what to do. Each later stopped check tries again, until it's
// gone.
func TestUnstrictKept(t *testing.T) {
	needGit(t)
	f := newFixture(t, "ws")
	f.repo("a")
	f.repo("b")
	f.walked()
	if err := f.newVM(false); err != nil {
		t.Fatal(err)
	}
	f.strictAsBefore("a", "b")
	f.p.Kept = func(string) func(string) msg.M {
		return func(path string) msg.M {
			if strings.HasPrefix(path, f.path("b")+"/") {
				return msg.GitKeptLocked("devvm-b-0123456789abcd")
			}
			return msg.M{}
		}
	}
	f.checked(false)
	if slices.Contains(f.flags, "nouchg "+f.path("b/.git/config")) || !slices.Contains(f.flags, "nouchg "+f.path("a/.git/config")) {
		t.Errorf("locks %q", f.flags)
	}
	if f.ignoreIn("a") != "" || f.ignoreIn("b") != "all" {
		t.Errorf("a %q, b %q", f.ignoreIn("a"), f.ignoreIn("b"))
	}
	bCfg := f.path("b/.git/config")
	if !f.warned("git_strict_kept config="+bCfg+" vm=devvm-b-0123456789abcd") || !f.warned("git_strict_converted") {
		t.Errorf("warnings %q", f.warns.Names())
	}
	if g := f.section(); g.Mode != On || !slices.Equal(g.StrictLeft, []string{"b"}) {
		t.Errorf("mode %s, left %q", g.Mode, g.StrictLeft)
	}
	// Still kept: named again, and left.
	f.checked(false)
	if !f.warned("git_strict_kept") || f.warned("git_strict_converted") || f.ignoreIn("b") != "all" {
		t.Errorf("kept again: %q, b %q", f.warns.Names(), f.ignoreIn("b"))
	}
	// No longer kept: taken out, and nothing's left.
	f.p.Kept = nil
	f.checked(false)
	if f.ignoreIn("b") != "" || len(f.section().StrictLeft) > 0 || len(f.warns.Names()) > 0 {
		t.Errorf("unkept: b %q, left %q, warnings %q", f.ignoreIn("b"), f.section().StrictLeft, f.warns.Names())
	}
	if i := slices.Index(f.flags, "nouchg "+bCfg); i < 0 || !slices.Contains(f.flags[i:], "uchg "+bCfg) {
		t.Errorf("locks %q", f.flags)
	}
}

// strict set nothing in a .git that isn't the one the VM's state
// recorded: one the VM replaced while it ran, the folder's own or a
// nested one, which the check names and locks nothing in, or one the
// state has no record of, as after devvm adopt. Its all stays, named with
// what to do, and nothing in it is unlocked or locked; a config without
// all there is named not at all.
func TestUnstrictOnlyRecorded(t *testing.T) {
	needGit(t)
	t.Run("replaced", func(t *testing.T) {
		f := newFixture(t, "ws")
		f.git(f.dir, "init", "-q")
		f.repo("b")
		f.walked()
		if err := f.newVM(false); err != nil {
			t.Fatal(err)
		}
		f.strictAsBefore()
		for _, rel := range []string{".", "b"} {
			if err := os.Rename(f.path(rel+"/.git"), f.path(rel+"/.git-old")); err != nil {
				t.Fatal(err)
			}
			f.git(f.path(rel), "init", "-q")
			f.git(f.path(rel), "config", "--file", ".git/config", "diff.ignoreSubmodules", "all")
		}
		f.checked(false)
		if !f.warned("git_replaced git="+f.path(".git")) || f.ignoreIn(".") != "all" || f.ignoreIn("b") != "all" {
			t.Errorf("warnings %q, . %q, b %q", f.warns.Names(), f.ignoreIn("."), f.ignoreIn("b"))
		}
		for _, cfg := range []string{f.path(".git/config"), f.path("b/.git/config")} {
			if slices.Contains(f.flags, "nouchg "+cfg) {
				t.Errorf("%s unlocked: %q", cfg, f.flags)
			}
		}
		for _, cfg := range []string{f.path(".git/config"), f.path("b/.git/config")} {
			if !f.warned("git_strict_unknown config=" + cfg) {
				t.Errorf("%s not named: %q", cfg, f.warns.Names())
			}
		}
		if g := f.section(); g.Mode != On || len(g.StrictLeft) > 0 {
			t.Errorf("mode %s, left %q", g.Mode, g.StrictLeft)
		}
	})
	t.Run("no record", func(t *testing.T) {
		f := newFixture(t, "p")
		f.git(f.dir, "init", "-q")
		f.git(f.dir, "config", "--file", ".git/config", "diff.ignoreSubmodules", "all")
		if err := record(t.Context(), f.vm, func(g *InVM) { g.Mode = Strict }); err != nil {
			t.Fatal(err)
		}
		f.checked(false)
		if f.ignoreIn(".") != "all" || !f.warned("git_strict_converted") || f.section().Mode != On {
			t.Errorf("%q, warnings %q, mode %s", f.ignoreIn("."), f.warns.Names(), f.section().Mode)
		}
		if !f.warned("git_strict_unknown config=" + f.path(".git/config")) {
			t.Errorf("not named: %q", f.warns.Names())
		}
		// Once on, it's named no more.
		f.checked(false)
		if f.warned("git_strict") {
			t.Errorf("again: %q", f.warns.Names())
		}
	})
	t.Run("no record, no all", func(t *testing.T) {
		f := newFixture(t, "p")
		f.git(f.dir, "init", "-q")
		if err := record(t.Context(), f.vm, func(g *InVM) { g.Mode = Strict }); err != nil {
			t.Fatal(err)
		}
		f.checked(false)
		if f.warned("git_strict_unknown") {
			t.Errorf("named: %q", f.warns.Names())
		}
	})
}

// An interrupt between unlocking .git/config and locking it again still
// locks it, and records nothing: the VM stays strict, and the next check
// converts it.
func TestUnstrictInterrupted(t *testing.T) {
	needGit(t)
	f := newFixture(t, "p")
	f.git(f.dir, "init", "-q")
	if err := f.newVM(false); err != nil {
		t.Fatal(err)
	}
	f.strictAsBefore(".")
	cfg := f.path(".git/config")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.setChflags(func(ctx context.Context, flags, path string) error {
		if ctx.Err() != nil {
			return ctx.Err() // as chflags under a cancelled context
		}
		f.flags = append(f.flags, flags+" "+path)
		f.immutable[path] = flags == "uchg"
		if flags == "nouchg" && path == cfg {
			cancel()
		}
		return nil
	})
	f.warns.Reset()
	if err := f.p.Check(ctx, f.vm, false); err == nil {
		t.Error("an interrupted check returned no error")
	}
	if !f.immutable[cfg] || f.section().Mode != Strict || f.warned("git_strict") {
		t.Errorf("locked %v, mode %s, warnings %q", f.immutable[cfg], f.section().Mode, f.warns.Names())
	}
	f.checked(false)
	if f.section().Mode != On || f.ignoreIn(".") != "" || !slices.Equal(f.warns.Names(), f.converted()) || !f.immutable[cfg] {
		t.Errorf("again: mode %s, %q, warnings %q", f.section().Mode, f.ignoreIn("."), f.warns.Names())
	}
}

// A strict VM whose mode saved for its next stop is off says nothing of
// being on: it's off next, and strict's all is out all the same.
func TestUnstrictNextOff(t *testing.T) {
	needGit(t)
	f := newFixture(t, "p")
	f.git(f.dir, "init", "-q")
	if err := f.newVM(false); err != nil {
		t.Fatal(err)
	}
	f.strictAsBefore(".")
	if err := SetNext(t.Context(), f.vm, Off); err != nil {
		t.Fatal(err)
	}
	f.checked(false)
	if f.warned("git_strict_converted") || f.ignoreIn(".") != "" || Next(f.vm) != Off {
		t.Errorf("warnings %q, %q, next %q", f.warns.Names(), f.ignoreIn("."), Next(f.vm))
	}
}

// On a host that watches, a config that changed after the check's sums
// isn't edited: it's left for the next check, which names the change.
func TestUnstrictChangedSinceCheck(t *testing.T) {
	needGit(t)
	f := newFixture(t, "p")
	f.p.Locker = Binds{}
	f.git(f.dir, "init", "-q")
	f.git(f.dir, "config", "--file", ".git/config", "diff.ignoreSubmodules", "all")
	if err := f.newVM(false); err != nil {
		t.Fatal(err)
	}
	f.strictAsBefore()
	g := f.section()
	f.git(f.dir, "config", "--file", ".git/config", "core.fsmonitor", "evil")
	d := converter{f.p}.dropIgnore(t.Context(), f.vm.Name(), converter{f.p}.strictRepos(g.Made, nil), true, g.Watched)
	if f.ignoreIn(".") != "all" || !slices.Equal(d.left, []string{"."}) || len(d.sums) > 0 {
		t.Errorf("%q, left %q, sums %q", f.ignoreIn("."), d.left, d.sums)
	}
}

// On a Linux host that pins, a strict VM is pinned as an on one is until
// it's converted, and the conversion's change to .git/config is taken
// into the watch's sums, so the next check names nothing.
func TestUnstrictPins(t *testing.T) {
	needGit(t)
	f := newFixture(t, "p")
	f.p.Locker = Binds{}
	f.git(f.dir, "init", "-q")
	f.git(f.dir, "config", "--file", ".git/config", "diff.ignoreSubmodules", "all")
	if err := f.newVM(false); err != nil {
		t.Fatal(err)
	}
	f.strictAsBefore()
	if _, readOnly, err := f.p.Pins(t.Context(), f.vm); err != nil || !slices.Contains(readOnly, ".git/config") {
		t.Errorf("strict's pins %q %v", readOnly, err)
	}
	if warns := f.check(); !slices.Equal(warns, f.converted()) || f.ignoreIn(".") != "" {
		t.Errorf("converted: %q, %q", warns, f.ignoreIn("."))
	}
	if warns := f.check(); len(warns) > 0 {
		t.Errorf("again: %q", warns)
	}
	if _, readOnly, _ := f.p.Pins(t.Context(), f.vm); !slices.Contains(readOnly, ".git/config") {
		t.Errorf("on's pins %q", readOnly)
	}
}

// On a host that can't lock any more, such as a Linux host whose kernel
// stopped letting devvm pin, a strict VM's check converts it as the
// watch's check, and off takes strict's all out too.
func TestUnstrictWatch(t *testing.T) {
	needGit(t)
	for _, do := range []string{"check", Off} {
		t.Run(do, func(t *testing.T) {
			f := newFixture(t, "p")
			f.git(f.dir, "init", "-q")
			if err := f.newVM(false); err != nil {
				t.Fatal(err)
			}
			f.strictAsBefore(".")
			f.canLock(false)
			if do == Off {
				f.set(Off)
			} else {
				f.checked(false)
			}
			if f.ignoreIn(".") != "" || f.warned("git_strict_converted") != (do == "check") {
				t.Errorf("%q, warnings %q", f.ignoreIn("."), f.warns.Names())
			}
		})
	}
}

// A mode an earlier devvm saved as strict for a running VM's next stop
// reads as on, and the check that finds it stopped records on.
func TestUnstrictNext(t *testing.T) {
	f := newFixture(t, "p")
	mkdirs(t, f.path(".git/hooks"))
	if err := f.newVM(false); err != nil {
		t.Fatal(err)
	}
	if err := SetNext(t.Context(), f.vm, Strict); err != nil {
		t.Fatal(err)
	}
	if Next(f.vm) != On {
		t.Errorf("next %q", Next(f.vm))
	}
	f.checked(false)
	if g := f.section(); g.Next != On || g.Mode != On || !slices.Equal(f.warns.Names(), f.converted()) {
		t.Errorf("next %q, mode %q, warnings %q", g.Next, g.Mode, f.warns.Names())
	}
}

// When git can't write the config, its all stays and is named, the
// config locked again, and the VM is on all the same.
func TestUnstrictFails(t *testing.T) {
	needGit(t)
	f := newFixture(t, "p")
	f.git(f.dir, "init", "-q")
	if err := f.newVM(false); err != nil {
		t.Fatal(err)
	}
	f.strictAsBefore(".")
	cfg := f.path(".git/config")
	// git writes config.lock beside it, which a folder it can't write in
	// refuses.
	if err := os.Chmod(f.path(".git"), 0o500); err != nil { //nolint:gosec // a folder, read-only to its owner
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(f.path(".git"), 0o700) }() //nolint:gosec // the folder's mode back, as git made it
	f.checked(false)
	if !f.warned("git_strict_left config="+cfg) || !f.warned("git_strict_converted") || f.ignoreIn(".") != "all" {
		t.Errorf("warnings %q, %q", f.warns.Names(), f.ignoreIn("."))
	}
	if !f.immutable[cfg] || f.section().Mode != On {
		t.Errorf("locks %q, mode %s", f.flags, f.section().Mode)
	}
}
