package lima

import (
	"context"
	"errors"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/ubuntu"
)

// PinOrigin's wait for the Ubuntu pin's lock, held as by another devvm's
// first build, shows ubuntu_pin_waiting, and a Ctrl-C ends it with
// nothing asked or pinned.
func TestPinOriginWaits(t *testing.T) {
	l, _, _ := testLima(t, "darwin")
	fi := &testFinder{releases: []string{"release-20260301"}}
	l.Ubuntu = fi
	if err := os.MkdirAll(l.Dirs.ImageLocks(), 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, err := platform.LockFile(t.Context(), l.Dirs.UbuntuPinLock(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute) // a wait that shows nothing fails, never hangs
	defer cancel()
	var said msg.Recorder
	l.Reporter = &msg.Fake{OnStatus: func(m msg.M) { said.Add(m); cancel() }}
	if _, err := l.PinOrigin(ctx, nil, backend.PinNewest); !errors.Is(err, context.Canceled) || !slices.Equal(said.Names(), []string{"ubuntu_pin_waiting"}) || len(fi.asked) != 0 {
		t.Errorf("held: %v, showed %q, asked %q", err, said.Names(), fi.asked)
	}
}

// PinOrigin pins the newest release when none is pinned, again on
// PinNewest, and the newest older than the one pinned on PinOlder; a
// --from base pins only when none is; e2e's release stands in for the
// newest. What it pinned is recorded, and said.
func TestPinOrigin(t *testing.T) {
	from := &backend.From{Ref: "debian:12", Digest: "debian@sha256:aaaa"}
	for _, c := range []struct {
		name    string
		none    bool   // nothing pinned first
		release string // UbuntuRelease
		from    *backend.From
		how     backend.Pin
		want    string // what's pinned after: "" for the test pin, unchanged
		asked   []string
	}{
		{name: "pinned, first", how: backend.PinFirst},
		{name: "none, first", none: true, how: backend.PinFirst, want: "release-20260301", asked: []string{"newest "}},
		{name: "newest", how: backend.PinNewest, want: "release-20260301", asked: []string{"newest "}},
		{name: "older", how: backend.PinOlder, want: "release-20251201", asked: []string{"newest release-20260101"}},
		{name: "e2e's", how: backend.PinNewest, release: "release-20251201", want: "release-20251201", asked: []string{"release release-20251201"}},
		{name: "e2e's, older", how: backend.PinOlder, release: "release-20260101", want: "release-20251201", asked: []string{"newest release-20260101"}},
		{name: "--from, newest", from: from, how: backend.PinNewest},
		{name: "--from, none", none: true, from: from, how: backend.PinNewest, want: "release-20260301", asked: []string{"newest "}},
	} {
		t.Run(c.name, func(t *testing.T) {
			l, _, _ := testLima(t, "darwin")
			fi := &testFinder{releases: []string{"release-20251201", "release-20260101", "release-20260301"}}
			l.Ubuntu, l.UbuntuRelease = fi, c.release
			if c.none {
				if err := os.Remove(l.Dirs.UbuntuPin()); err != nil {
					t.Fatal(err)
				}
			}
			got, err := l.PinOrigin(t.Context(), c.from, c.how)
			if err != nil || got != c.want {
				t.Fatalf("PinOrigin = %q, %v; want %q", got, err, c.want)
			}
			if !slices.Equal(fi.asked, c.asked) {
				t.Errorf("asked %q, want %q", fi.asked, c.asked)
			}
			p, err := state.ReadUbuntuPin(l.Dirs)
			if want := ubuntu.PinArg(fi.pin(c.want)); c.want != "" && (err != nil || ubuntu.PinArg(p) != want) {
				t.Errorf("pinned %+v, %v; want %s", p, err, want)
			}
			if c.want == "" && (err != nil || p.Digest != "sha256:"+testUbuntuSum) {
				t.Errorf("the pin changed: %+v, %v", p, err)
			}
		})
	}
}

// What can't be pinned leaves the pin as it was: no release older than
// the one pinned, Ubuntu not answering, nothing pinned to be older than,
// or a pin that can't be read.
func TestPinOriginFails(t *testing.T) {
	l, _, _ := testLima(t, "darwin")
	fi := &testFinder{}
	l.Ubuntu = fi
	if _, err := l.PinOrigin(t.Context(), nil, backend.PinOlder); msg.IDOf(err) != "no_ubuntu_release" {
		t.Errorf("none older: %v", err)
	}
	fi.err = msg.UbuntuUnreachable("x", ubuntu.Version)
	if _, err := l.PinOrigin(t.Context(), nil, backend.PinNewest); msg.IDOf(err) != "ubuntu_unreachable" {
		t.Errorf("unreachable: %v", err)
	}
	if p, err := state.ReadUbuntuPin(l.Dirs); err != nil || p.Digest != "sha256:"+testUbuntuSum {
		t.Errorf("the pin changed: %+v, %v", p, err)
	}
	if err := os.Remove(l.Dirs.UbuntuPin()); err != nil {
		t.Fatal(err)
	}
	if _, err := l.PinOrigin(t.Context(), nil, backend.PinOlder); !errors.Is(err, errNoPin) {
		t.Errorf("older than nothing: %v", err)
	}
	if err := os.WriteFile(l.Dirs.UbuntuPin(), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := l.PinOrigin(t.Context(), nil, backend.PinFirst); err == nil {
		t.Error("a broken pin: no error")
	}
}

// A listing whose newest is older than what's pinned (it lost releases:
// it isn't signed) keeps the pin, with a warning.
func TestPinNewestKeepsNewer(t *testing.T) {
	l, _, warned := testLima(t, "darwin")
	l.Ubuntu = &testFinder{releases: []string{"release-20251201"}}
	if got, err := l.PinOrigin(t.Context(), nil, backend.PinNewest); err != nil || got != "" {
		t.Fatalf("got %q, %v", got, err)
	}
	if p, _ := state.ReadUbuntuPin(l.Dirs); p.Release != "release-20260101" {
		t.Errorf("pinned %s", p.Release)
	}
	if !slices.Contains(warned.Names(), "ubuntu_newest_older newest=release-20251201 pinned=release-20260101") {
		t.Errorf("warned %q", warned.Names())
	}
}

// A pin for another architecture (a data folder from another machine)
// counts as none: the next build pins one for this host.
func TestPinAnotherArch(t *testing.T) {
	l, _, _ := testLima(t, "darwin")
	l.Arch = "amd64"
	if _, ok, err := l.pin(); ok || err != nil {
		t.Errorf("another architecture's pin: ok %v, %v", ok, err)
	}
	if _, err := l.pinnedImage(); !errors.Is(err, errNoPin) {
		t.Errorf("its image: %v", err)
	}
}
