package proxy

// How the child is confined: what the broker started it under, what its ready says, and what
// run/proxy.confined holds, which devvm doctor reads. Every platform's confinement reports
// through this one form, so devvm doctor reads them all alike.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/devvmhq/devvm/internal/landlock"
	"github.com/devvmhq/devvm/internal/safefile"
)

// The levels of a confinement: the child holds only what it needs (ConfinedFull), some of it
// more (ConfinedPartial), or the broker's access (ConfinedNone).
const (
	ConfinedFull    = "full"
	ConfinedPartial = "partial"
	ConfinedNone    = "none"
)

// What confines a child: on macOS Seatbelt, which the broker starts it under (sandbox-exec); on
// Linux Landlock, which the child confines itself with, with its seccomp filter
// (internal/landlock), or the seccomp filter alone, on a kernel without Landlock.
const (
	BySeatbelt = "seatbelt"
	ByLandlock = "landlock"
	BySeccomp  = "seccomp"
)

// Why a child isn't fully confined: this system has no way devvm confines it (WhyUnsupported),
// sandbox-exec isn't there (WhySandboxMissing), sandbox-exec failed to start it under its
// profile (WhySandboxFailed), or what confines it let it open its broker's config, which it must
// refuse it (WhyCheckFailed).
const (
	WhyUnsupported    = "unsupported"
	WhySandboxMissing = "sandbox-exec-missing"
	WhySandboxFailed  = "sandbox-exec-failed"
	WhyCheckFailed    = "check-failed"
)

// Why a Linux child isn't fully confined, by what the kernel lacks: neither Landlock nor seccomp
// (WhyNoKernel), Landlock or one at its floor, ABI 2 (WhyNoLandlock), seccomp (WhyNoSeccomp), or
// a Landlock too old to refuse it a TCP port's bind, below ABI 4 (WhyLandlockPorts), a signal to
// another process, below ABI 6 or without the fix for signals between a process's threads
// (WhyLandlockSignals). Each names the first of these, and a later one follows from it.
const (
	WhyNoKernel        = "no-landlock-or-seccomp"
	WhyNoLandlock      = "no-landlock"
	WhyNoSeccomp       = "no-seccomp"
	WhyLandlockPorts   = "landlock-below-abi4"
	WhyLandlockSignals = "landlock-signals"
)

// knownBy and knownWhy are the words a confinement may hold, so a reader never takes, or prints,
// one that a child chose.
func knownBy(w string) bool {
	switch w {
	case BySeatbelt, ByLandlock, BySeccomp:
		return true
	}
	return false
}

func knownWhy(w string) bool {
	switch w {
	case WhyUnsupported, WhySandboxMissing, WhySandboxFailed, WhyCheckFailed,
		WhyNoKernel, WhyNoLandlock, WhyNoSeccomp, WhyLandlockPorts, WhyLandlockSignals:
		return true
	}
	return false
}

// landlockConfinement is how a Linux child that confined itself with r is confined: fully, by
// Landlock and the seccomp filter from Landlock's ABI 6 with its signals scoped; partly, naming
// what the kernel lacks first; or not at all without either. Below ABI 9 Landlock doesn't refuse
// a named unix socket itself, and below ABI 10 a UDP datagram to any port, but the filter refuses
// the child every unix socket, and UDP out is as open as TCP out is at every ABI, so neither is
// a gap the user could close.
func landlockConfinement(r landlock.Result) Confinement {
	partial := func(by, why string) Confinement { return Confinement{Level: ConfinedPartial, By: by, Why: why} }
	switch {
	case r.ABI == 0 && !r.Seccomp:
		return unconfined(WhyNoKernel)
	case r.ABI == 0:
		return partial(BySeccomp, WhyNoLandlock)
	case !r.Seccomp:
		return partial(ByLandlock, WhyNoSeccomp)
	case r.ABI < landlock.ABIPorts:
		return partial(ByLandlock, WhyLandlockPorts)
	case !r.SignalsScoped:
		return partial(ByLandlock, WhyLandlockSignals)
	}
	return Confinement{Level: ConfinedFull, By: ByLandlock}
}

// confinesFiles reports whether c's sandbox keeps the child from files it doesn't need, which its
// check (checkConfined) can then see: Seatbelt's or Landlock's, not the seccomp filter alone.
func (c Confinement) confinesFiles() bool { return c.By == BySeatbelt || c.By == ByLandlock }

// Confinement is how a proxy's child is confined: its level, what confines it ("" for nothing),
// and why it isn't fully confined ("" when it is). As text it's one line of those words, in that
// order, each from a fixed set (ParseConfinement).
type Confinement struct {
	Level string
	By    string
	Why   string
}

// unconfined is a child confined by nothing, for why.
func unconfined(why string) Confinement { return Confinement{Level: ConfinedNone, Why: why} }

// String is c as one line: its words, in order.
func (c Confinement) String() string {
	return strings.Join(strings.Fields(c.Level+" "+c.By+" "+c.Why), " ")
}

// ParseConfinement reads a confinement's line (Confinement.String): its level, then what confines
// it, then why it isn't fully confined, each word from its fixed set, and only a level that isn't
// full has a why.
func ParseConfinement(s string) (Confinement, error) {
	w := strings.Fields(s)
	if len(w) == 0 || strings.Join(w, " ") != strings.TrimSpace(s) {
		return Confinement{}, errors.New("not a confinement")
	}
	c := Confinement{Level: w[0]}
	switch c.Level {
	case ConfinedFull, ConfinedPartial, ConfinedNone:
	default:
		return Confinement{}, errors.New("a confinement of no known level")
	}
	w = w[1:]
	if len(w) > 0 && knownBy(w[0]) {
		c.By, w = w[0], w[1:]
	}
	if len(w) > 0 && knownWhy(w[0]) {
		c.Why, w = w[0], w[1:]
	}
	switch {
	case len(w) > 0:
		return Confinement{}, errors.New("a confinement with an unknown word")
	case (c.Level == ConfinedFull) != (c.Why == ""):
		return Confinement{}, errors.New("a confinement whose why doesn't fit its level")
	case c.Level != ConfinedNone && c.By == "":
		return Confinement{}, errors.New("a confinement by nothing")
	}
	return c, nil
}

// confinedMax bounds run/proxy.confined: one line of a few words.
const confinedMax = 256

// ReadConfined is how the child of the broker whose config is at config is confined, as the
// broker wrote it once the child was ready (ConfinedFile): fs.ErrNotExist before then, and for a
// proxy that an older devvm started.
func ReadConfined(config string) (Confinement, error) {
	b, err := safefile.Read(ConfinedFile(config), confinedMax, safefile.Opts{})
	if err != nil {
		return Confinement{}, err
	}
	return ParseConfinement(string(b))
}

// confineChild is how the child is confined, given how its broker started it (spawned) and what
// it needs (n): it confines itself where its platform does that in the process (confineSelf),
// within confineWait, then checks that whatever confines its files holds (checkConfined), refusing
// it probe, its broker's config, which it never reads. A confinement that fails, or takes longer,
// is an error, and the child mustn't start: part of it may be confined.
func confineChild(spawned Confinement, n ChildNeeds, probe string) (Confinement, error) {
	return confineWith(confineSelf, spawned, n, probe)
}

// confineWith is confineChild with self as the confinement in the process, which tests replace:
// confineSelf would confine the test's own process.
func confineWith(self func(Confinement, ChildNeeds) (Confinement, error), spawned Confinement, n ChildNeeds, probe string) (Confinement, error) {
	c, err := within(confineWait, func() (Confinement, error) { return self(spawned, n) })
	if err != nil {
		return Confinement{}, fmt.Errorf("confining the child: %w", err)
	}
	if c.Level == ConfinedNone || !c.confinesFiles() {
		return c, nil
	}
	if !holds(probe) {
		// A check that fails says so in the line, and the child runs on with what holds.
		return Confinement{Level: ConfinedNone, By: c.By, Why: WhyCheckFailed}, nil
	}
	return c, nil
}

// holds reports whether the child's confinement refuses it probe (checkConfined).
func holds(probe string) bool { return checkConfined(probe) == nil }

// confineWait bounds the child's confinement and its check, well within the broker's startWait,
// so a sandbox that fails slowly fails the start with its reason. One that stops the whole process
// (syscall.AllThreadsSyscall stops the world) is the broker's startWait's to end.
const confineWait = 2 * time.Second

// within is f's confinement, or an error once wait has passed without it: f goes on, and the
// child exits as its start fails.
func within(wait time.Duration, f func() (Confinement, error)) (Confinement, error) {
	type result struct {
		c   Confinement
		err error
	}
	done := make(chan result, 1)
	go func() {
		c, err := f()
		done <- result{c, err}
	}()
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case r := <-done:
		return r.c, r.err
	case <-t.C:
		return Confinement{}, fmt.Errorf("it took more than %s", wait)
	}
}

// checkConfined checks that the child can't open probe, a file of the broker's that it may never
// read: an open that's refused for want of permission, and nothing else, says what confines it
// holds. An open that works is closed at once, unread.
func checkConfined(probe string) error {
	f, err := os.Open(probe) //nolint:gosec // the broker's config, opened only to see it refused, never read
	if err == nil {
		_ = f.Close()
		return fmt.Errorf("the child could open %s", probe)
	}
	if errors.Is(err, fs.ErrPermission) {
		return nil
	}
	return err
}

// ChildNeeds is what the child needs of this machine, which a sandbox that confines it from its
// first instruction allows it alone (IO.Sandbox): its executable, the folders it writes and the
// files it looks at.
type ChildNeeds struct {
	// Exe is devvm's executable by its real path, which the sandbox runs and the child reads for
	// its SHA-256.
	Exe string
	// Folders are the folders it creates regular files in, reads, writes, renames and deletes
	// them, and locks them: its logs' folder and the shared Nix cache's.
	Folders []string
	// Watched are the files it looks at with stat alone, and asks the broker for once they change:
	// the policy files, the bindings file and the links' list.
	Watched []string
}

// fits reports whether a child that the broker started as spawned may say it's confined as c: a
// sandbox the broker started it under is the one it names, and it never names one the broker
// didn't, so a child can't claim more than it was given. One it confines itself with in the
// process (confineSelf) is its own to name.
func (c Confinement) fits(spawned Confinement) bool {
	if spawned.By == BySeatbelt {
		return c.By == BySeatbelt
	}
	return c.By != BySeatbelt
}
