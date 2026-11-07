package upgrade

import (
	"context"
	"encoding/json"
	"os"
	"time"

	"github.com/devvmhq/devvm/internal/state"
)

// CheckEvery is how often a launch checks for a newer release.
const CheckEvery = 24 * time.Hour

// noticeTimeout bounds a launch's check, which runs beside the launch and
// is dropped when it takes longer.
const noticeTimeout = 5 * time.Second

// Known is the daily check's record: the newest release it found and
// when, and when it last tried, found or not, so an offline machine tries
// once a day too.
type Known struct {
	Latest  string    `json:"latest,omitempty"`
	Checked time.Time `json:"checked,omitzero"`
	Tried   time.Time `json:"tried,omitzero"`
}

// ReadKnown reads the record at path. A missing or unreadable one is
// none, as is one naming a prerelease, which is never announced: the
// next check writes it again.
func ReadKnown(path string) Known {
	var k Known
	data, err := os.ReadFile(path) //nolint:gosec // paths come from paths.Dirs
	if err != nil || json.Unmarshal(data, &k) != nil || (k.Latest != "" && (!Valid(k.Latest) || Prerelease(k.Latest))) {
		return Known{}
	}
	return k
}

// Due reports whether k's last try is a day old at now, or not yet made,
// or in the future (a clock set back).
func (k Known) Due(now time.Time) bool {
	return k.Tried.IsZero() || now.Sub(k.Tried) >= CheckEvery || k.Tried.After(now)
}

// Newer reports whether k's release is newer than current.
func (k Known) Newer(current string) bool {
	return k.Latest != "" && Valid(current) && Compare(k.Latest, current) > 0
}

// Refresh checks s for the newest release, within noticeTimeout, and
// records what it found at path, with now as the try's time. A failed
// check, such as offline, keeps what was known and returns its error.
func (s Source) Refresh(ctx context.Context, path string, now time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, noticeTimeout)
	defer cancel()
	k := ReadKnown(path)
	k.Tried = now
	av, err := s.Check(ctx, "")
	if err == nil {
		k.Latest, k.Checked = av.Latest, now
	}
	data, merr := json.Marshal(k)
	if merr != nil {
		return merr
	}
	if werr := state.WriteFile(path, data); werr != nil {
		return werr
	}
	return err
}
