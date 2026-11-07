package upgrade

import (
	"cmp"
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
// once a day too. Newest is the newest release, prereleases included,
// which only the check of an install that follows them records.
type Known struct {
	Latest  string    `json:"latest,omitempty"`
	Newest  string    `json:"newest,omitempty"`
	Checked time.Time `json:"checked,omitzero"`
	Tried   time.Time `json:"tried,omitzero"`
}

// ReadKnown reads the record at path. A missing or unreadable one is
// none, as is one naming a prerelease as Latest, which is never
// announced as the newest release: the next check writes it again.
func ReadKnown(path string) Known {
	var k Known
	data, err := os.ReadFile(path) //nolint:gosec // paths come from paths.Dirs
	if err != nil || json.Unmarshal(data, &k) != nil || (k.Latest != "" && (!Valid(k.Latest) || Prerelease(k.Latest))) ||
		(k.Newest != "" && !Valid(k.Newest)) {
		return Known{}
	}
	return k
}

// Due reports whether k's last try is a day old at now, or not yet made,
// or in the future (a clock set back).
func (k Known) Due(now time.Time) bool {
	return k.Tried.IsZero() || now.Sub(k.Tried) >= CheckEvery || k.Tried.After(now)
}

// Next is the release k knows of that's newer than current, for an
// install that follows prereleases (Newest) or doesn't (Latest); "" for
// none.
func (k Known) Next(current string, prereleases bool) string {
	v := k.Latest
	if prereleases {
		v = k.Newest
	}
	if v == "" || !Valid(current) || Compare(v, current) <= 0 {
		return ""
	}
	return v
}

// Refresh checks s for the newest release, within noticeTimeout, and
// records what it found at path, with now as the try's time: for an
// install that follows prereleases, the newest of those too, from the
// releases feed, which also gives the newest release when it lists one.
// A failed check, such as offline, keeps what was known and returns its
// error.
func (s Source) Refresh(ctx context.Context, path string, now time.Time, prereleases bool) error {
	ctx, cancel := context.WithTimeout(ctx, noticeTimeout)
	defer cancel()
	k := ReadKnown(path)
	k.Tried = now
	var err error
	if prereleases {
		var newest, latest string
		if newest, latest, err = s.feed(ctx); err == nil {
			k.Newest, k.Latest, k.Checked = newest, cmp.Or(latest, k.Latest), now
		}
	} else {
		var latest string
		if latest, err = s.Latest(ctx); err == nil {
			k.Latest, k.Checked = latest, now
		}
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
