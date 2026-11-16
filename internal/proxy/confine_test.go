package proxy

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/devvmhq/devvm/internal/landlock"
)

// A confinement reads back as it was written, and a line that isn't one of the fixed words, in
// their order, with a why that fits its level, is refused.
func TestParseConfinement(t *testing.T) {
	t.Parallel()
	for _, c := range []Confinement{
		{Level: ConfinedFull, By: BySeatbelt},
		unconfined(WhyUnsupported),
		unconfined(WhySandboxMissing),
		unconfined(WhySandboxFailed),
		{Level: ConfinedNone, By: BySeatbelt, Why: WhyCheckFailed},
		{Level: ConfinedPartial, By: BySeatbelt, Why: WhyCheckFailed},
	} {
		got, err := ParseConfinement(c.String() + "\n")
		if err != nil || got != c {
			t.Errorf("%q: %+v, %v", c, got, err)
		}
	}
	for _, bad := range []string{
		"", "none", "full", "full unsupported", "partial", "partial unsupported", "all seatbelt",
		"full seatbelt extra", "none \x1b[31m", "none  unsupported", "none unsupported seatbelt",
		"seatbelt full", "full\tseatbelt",
	} {
		if c, err := ParseConfinement(bad); err == nil {
			t.Errorf("%q: taken as %+v", bad, c)
		}
	}
}

// The check holds when opening the probe is refused for want of permission, and fails when it
// opens, or fails another way.
func TestCheckConfined(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root opens any file")
	}
	dir := t.TempDir()
	open, closed := filepath.Join(dir, "open"), filepath.Join(dir, "closed")
	must(t, os.WriteFile(open, []byte("{}"), 0o600))
	must(t, os.WriteFile(closed, []byte("{}"), 0))
	if err := checkConfined(closed); err != nil {
		t.Errorf("refused: %v", err)
	}
	if err := checkConfined(open); err == nil {
		t.Error("opened: held")
	}
	if err := checkConfined(filepath.Join(dir, "gone")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing: %v", err)
	}
	// A child started under a sandbox keeps it only when the check holds; one started unconfined
	// stays so, unchecked, and so does one confined by the seccomp filter alone, which leaves its
	// files open.
	asSpawned := func(spawned Confinement, _ ChildNeeds) (Confinement, error) { return spawned, nil }
	confine := func(spawned Confinement, probe string) Confinement {
		t.Helper()
		c, err := confineWith(asSpawned, spawned, ChildNeeds{}, probe)
		must(t, err)
		return c
	}
	sb := Confinement{Level: ConfinedFull, By: BySeatbelt}
	if got := confine(sb, closed); got != sb {
		t.Errorf("held: %+v", got)
	}
	if got, want := confine(sb, open), (Confinement{Level: ConfinedNone, By: BySeatbelt, Why: WhyCheckFailed}); got != want {
		t.Errorf("didn't hold: %+v", got)
	}
	if got := confine(unconfined(WhySandboxMissing), open); got != unconfined(WhySandboxMissing) {
		t.Errorf("unconfined: %+v", got)
	}
	ll := Confinement{Level: ConfinedFull, By: ByLandlock}
	if got, want := confine(ll, open), (Confinement{Level: ConfinedNone, By: ByLandlock, Why: WhyCheckFailed}); got != want {
		t.Errorf("Landlock that didn't hold: %+v", got)
	}
	sc := Confinement{Level: ConfinedPartial, By: BySeccomp, Why: WhyNoLandlock}
	if got := confine(sc, open); got != sc {
		t.Errorf("the filter alone: %+v", got)
	}
	// A confinement in the process that fails fails the child's start.
	if _, err := confineWith(func(Confinement, ChildNeeds) (Confinement, error) {
		return Confinement{}, errors.New("landlock_restrict_self: EINVAL")
	}, sb, ChildNeeds{}, closed); err == nil {
		t.Error("a failure: no error")
	}
}

// A confinement in the process that takes longer than confineWait fails the start rather than hang
// it.
func TestConfineWithin(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		began := time.Now()
		_, err := confineWith(func(s Confinement, _ ChildNeeds) (Confinement, error) { <-release; return s, nil }, unconfined(WhyUnsupported), ChildNeeds{}, "")
		if err == nil || time.Since(began) != confineWait {
			t.Errorf("a confinement that hangs: %v after %s", err, time.Since(began))
		}
		close(release)
	})
}

// A Linux child's confinement, by what Landlock and the seccomp filter gave it: fully from ABI 9
// with its signals scoped, partly naming what's missing first, and not at all without either.
func TestLandlockConfinement(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		r    landlock.Result
		want string
	}{
		{landlock.Result{}, "none no-landlock-or-seccomp"},
		{landlock.Result{Seccomp: true}, "partial seccomp no-landlock"},
		{landlock.Result{ABI: 9, SignalsScoped: true}, "partial landlock no-seccomp"},
		{landlock.Result{ABI: 3, Seccomp: true}, "partial landlock landlock-below-abi4"},
		{landlock.Result{ABI: 5, Seccomp: true}, "partial landlock landlock-signals"},
		{landlock.Result{ABI: 7, Seccomp: true}, "partial landlock landlock-signals"},
		{landlock.Result{ABI: 6, SignalsScoped: true, Seccomp: true}, "full landlock"},
		{landlock.Result{ABI: 8, SignalsScoped: true, Seccomp: true}, "full landlock"},
		{landlock.Result{ABI: 9, SignalsScoped: true, Seccomp: true}, "full landlock"},
		{landlock.Result{ABI: 12, SignalsScoped: true, Seccomp: true}, "full landlock"},
	} {
		c := landlockConfinement(tc.r)
		if c.String() != tc.want {
			t.Errorf("%+v: %q, want %q", tc.r, c, tc.want)
		}
		if back, err := ParseConfinement(c.String()); err != nil || back != c {
			t.Errorf("%q reads back as %+v, %v", c, back, err)
		}
	}
}

// ReadConfined reads what the broker wrote beside its config, and nothing that isn't a
// confinement.
func TestReadConfined(t *testing.T) {
	t.Parallel()
	config := filepath.Join(t.TempDir(), "proxy.json")
	if _, err := ReadConfined(config); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("none written: %v", err)
	}
	must(t, writeAtomic(ConfinedFile(config), []byte("full seatbelt\n"), 0o600))
	if c, err := ReadConfined(config); err != nil || c != (Confinement{Level: ConfinedFull, By: BySeatbelt}) {
		t.Errorf("full seatbelt: %+v, %v", c, err)
	}
	must(t, writeAtomic(ConfinedFile(config), []byte("full \x1b[2J\n"), 0o600))
	if _, err := ReadConfined(config); err == nil {
		t.Error("a line of other words: taken")
	}
}

// limitedChild, set in the environment, makes TestLimitFiles the process it starts, which
// lowers its own limit on open files.
const limitedChild = "DEVVM_LIMIT_FILES_TEST_CHILD"

// The child lowers its limit on open files, soft and hard, to childFilesMax, and can't raise it
// again; this test's binary, run again, does it, as it would lower the test's own process's.
func TestLimitFiles(t *testing.T) {
	if os.Getenv(limitedChild) != "" {
		must(t, limitFiles(childFilesMax))
		var l syscall.Rlimit
		must(t, syscall.Getrlimit(syscall.RLIMIT_NOFILE, &l))
		if l.Max > childFilesMax || l.Cur > l.Max {
			t.Fatalf("the limit: %+v", l)
		}
		l.Max++
		if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &l); err == nil {
			t.Fatal("raised again")
		}
		// A limit already lower stays as it is.
		must(t, limitFiles(childFilesMax))
		return
	}
	t.Parallel()
	self, err := os.Executable()
	must(t, err)
	cmd := exec.CommandContext(t.Context(), self, "-test.run=^TestLimitFiles$", "-test.v") //nolint:gosec // the test binary itself
	cmd.Env = append(os.Environ(), limitedChild+"=1")
	if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(out), "--- PASS: TestLimitFiles") {
		t.Errorf("%v: %s", err, out)
	}
}
