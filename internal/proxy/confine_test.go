package proxy

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
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
	// stays so, unchecked.
	sb := Confinement{Level: ConfinedFull, By: BySeatbelt}
	if got := confineChild(sb, closed); got != sb {
		t.Errorf("held: %+v", got)
	}
	if got, want := confineChild(sb, open), (Confinement{Level: ConfinedNone, By: BySeatbelt, Why: WhyCheckFailed}); got != want {
		t.Errorf("didn't hold: %+v", got)
	}
	if got := confineChild(unconfined(WhySandboxMissing), open); got != unconfined(WhySandboxMissing) {
		t.Errorf("unconfined: %+v", got)
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
