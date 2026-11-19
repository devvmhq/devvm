package lima

import (
	"context"
	"errors"
	"io/fs"
	"os"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/ubuntu"
)

// errNoPin is a build asked of Lima before PinOrigin: callers pin first.
var errNoPin = errors.New("lima: no Ubuntu cloud image is pinned for base")

// Finder finds Ubuntu's cloud image in its signed releases
// (ubuntu.Finder).
type Finder interface {
	// Newest is the newest release older than before ("" for any) with an
	// image for the host.
	Newest(ctx context.Context, before string) (state.UbuntuPin, error)
	// Release is release's image for the host.
	Release(ctx context.Context, release string) (state.UbuntuPin, error)
}

// PinOrigin pins the Ubuntu cloud image base starts from on this machine
// (state.UbuntuPin), as how asks: the newest release with an image for
// the host (UbuntuRelease in its place, in a test build that sets it), or
// the newest older than the one pinned. A --from base's conversion boots
// the image pinned, which it pins only when none is: rebuilding it looks
// up its own digest again, not Ubuntu's image, which base from Ubuntu
// starts from too. One devvm at a time pins, under UbuntuPinLock, so two
// first builds pin the same image.
func (l *images) PinOrigin(ctx context.Context, f *backend.From, how backend.Pin) (string, error) {
	if f != nil {
		how = backend.PinFirst
	}
	if err := os.MkdirAll(l.Dirs.ImageLocks(), 0o700); err != nil {
		return "", err
	}
	unlock, err := platform.LockFile(ctx, l.Dirs.UbuntuPinLock(), msg.Waiting(l.Status, msg.UbuntuPinWaiting()))
	if err != nil {
		return "", err
	}
	defer unlock()
	was, ok, err := l.pin()
	if err != nil {
		return "", err
	}
	var p state.UbuntuPin
	switch {
	case how == backend.PinFirst && ok:
		return "", nil
	case how == backend.PinOlder && !ok:
		return "", errNoPin
	case how == backend.PinOlder:
		p, err = l.Ubuntu.Newest(ctx, was.Release)
	case l.UbuntuRelease != "":
		p, err = l.Ubuntu.Release(ctx, l.UbuntuRelease)
	default:
		p, err = l.Ubuntu.Newest(ctx, "")
	}
	if err != nil {
		return "", err
	}
	// The listing isn't signed: one that has lost what's pinned's newer
	// releases keeps the pin, rather than move base back.
	if how == backend.PinNewest && l.UbuntuRelease == "" && ok && p.Release < was.Release {
		l.Warn(msg.UbuntuNewestOlder(p.Release, was.Release))
		return "", nil
	}
	if err := state.WriteUbuntuPin(l.Dirs, p); err != nil {
		return "", err
	}
	return p.Release, nil
}

// pin is the cloud image pinned on this machine for base; ok is false
// when none is, or the one pinned is for another architecture (a data
// folder copied from another machine): the next build pins one.
func (l *Lima) pin() (p state.UbuntuPin, ok bool, err error) {
	p, err = state.ReadUbuntuPin(l.Dirs)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return state.UbuntuPin{}, false, nil
	case err != nil:
		return state.UbuntuPin{}, false, err
	case p.File != ubuntu.File(l.Arch):
		return state.UbuntuPin{}, false, nil
	}
	return p, true, nil
}

// pinned is the pin as base's recipe holds it (ubuntu.PinArg), "" when
// nothing is pinned.
func (l *Lima) pinned() (arg string, ok bool, err error) {
	p, ok, err := l.pin()
	if err != nil || !ok {
		return "", false, err
	}
	return ubuntu.PinArg(p), true, nil
}

// pinnedImage is the pinned cloud image, at UbuntuURL: what base's VM, and
// a --from conversion's, are made from. It looks nothing up: the pin's
// SHA-256 was checked against Ubuntu's signed checksums when it was
// pinned, and Lima checks the image against it.
func (l *Lima) pinnedImage() (ubuntu.Image, error) {
	p, ok, err := l.pin()
	if err != nil {
		return ubuntu.Image{}, err
	}
	if !ok {
		return ubuntu.Image{}, errNoPin
	}
	return ubuntu.ImageOf(l.UbuntuURL, p), nil
}
