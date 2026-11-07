package upgrade_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/upgrade"
	"github.com/devvmhq/devvm/internal/upgrade/upgradetest"
)

// The daily check's record: due once a day (or never tried, or tried in
// the future), newer only for a newer release than a release's version.
func TestKnown(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		tried time.Time
		due   bool
	}{
		{time.Time{}, true},
		{now.Add(-time.Hour), false},
		{now.Add(-upgrade.CheckEvery), true},
		{now.Add(time.Hour), true},
	} {
		if got := (upgrade.Known{Tried: c.tried}).Due(now); got != c.due {
			t.Errorf("tried %v: due %v", c.tried, got)
		}
	}
	for _, c := range []struct {
		latest, current string
		newer           bool
	}{
		{"0.2.0", "0.1.0", true},
		{"0.2.0", "0.2.0", false},
		{"0.2.0", "0.3.0", false},
		{"", "0.1.0", false},
		{"0.2.0", "", false},
		{"0.2.0", "0.2.0-rc.1", true},
		{"0.1.0", "0.2.0-rc.1", false},
	} {
		if got := (upgrade.Known{Latest: c.latest}).Newer(c.current); got != c.newer {
			t.Errorf("%q over %q: %v", c.latest, c.current, got)
		}
	}
}

// Refresh records the newest release and the try; offline it keeps what
// was known, records the try, and returns the error. A record devvm can't
// read is none.
func TestRefresh(t *testing.T) {
	srv := upgradetest.NewServer(t)
	srv.Add("0.2.0", upgradetest.NewRelease(upgradetest.NewKey(), "0.2.0", nil), true)
	src := upgrade.NewSource(srv.URL, "")
	src.Client.Trust(srv.Roots())
	path := filepath.Join(t.TempDir(), "cache", "release.json")
	if k := upgrade.ReadKnown(path); k != (upgrade.Known{}) {
		t.Fatalf("no record: %+v", k)
	}
	first := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	if err := src.Refresh(t.Context(), path, first); err != nil {
		t.Fatal(err)
	}
	if k := upgrade.ReadKnown(path); k.Latest != "0.2.0" || !k.Checked.Equal(first) || !k.Tried.Equal(first) {
		t.Fatalf("recorded %+v", k)
	}
	offline := upgrade.NewSource("https://127.0.0.1:1", "")
	next := first.Add(upgrade.CheckEvery)
	if err := offline.Refresh(t.Context(), path, next); err == nil {
		t.Fatal("offline: no error")
	}
	if k := upgrade.ReadKnown(path); k.Latest != "0.2.0" || !k.Checked.Equal(first) || !k.Tried.Equal(next) {
		t.Fatalf("offline: %+v", k)
	}
	for _, bad := range []string{"{", `{"latest":"v1; rm"}`, `{"latest":"0.3.0-rc.1"}`} {
		if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if k := upgrade.ReadKnown(path); k != (upgrade.Known{}) {
			t.Errorf("%s: %+v", bad, k)
		}
	}
}
