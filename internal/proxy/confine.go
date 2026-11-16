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

	"github.com/devvmhq/devvm/internal/safefile"
)

// The levels of a confinement: the child holds only what it needs (ConfinedFull), some of it
// more (ConfinedPartial), or the broker's access (ConfinedNone).
const (
	ConfinedFull    = "full"
	ConfinedPartial = "partial"
	ConfinedNone    = "none"
)

// BySeatbelt is what confines a child on macOS: Seatbelt, which the broker starts it under
// (sandbox-exec).
const BySeatbelt = "seatbelt"

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

// knownBy and knownWhy are the words a confinement may hold, so a reader never takes, or prints,
// one that a child chose.
func knownBy(w string) bool { return w == BySeatbelt }

func knownWhy(w string) bool {
	switch w {
	case WhyUnsupported, WhySandboxMissing, WhySandboxFailed, WhyCheckFailed:
		return true
	}
	return false
}

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

// confineChild is how the child is confined, given how its broker started it (spawned): it
// confines itself where its platform does that in the process (confineSelf), then checks that
// whatever confines it holds (checkConfined), refusing it probe, its broker's config, which it
// never reads.
func confineChild(spawned Confinement, probe string) Confinement {
	c := confineSelf(spawned)
	if c.Level == ConfinedNone {
		return c
	}
	if err := checkConfined(probe); err != nil {
		return Confinement{Level: ConfinedNone, By: c.By, Why: WhyCheckFailed}
	}
	return c
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
