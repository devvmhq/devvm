package cli

import (
	"context"
	"os"
	"time"

	"github.com/devvmhq/devvm/internal/settings"
	"github.com/devvmhq/devvm/internal/upgrade"
)

// releaseWait is how long a launch waits, once it's done, for the daily
// check it started to finish, so the next launch knows what it found.
const releaseWait = time.Second

// releaseNotice says, at a launch in a terminal, that a newer release is
// out, from the daily check's record (for Homebrew's devvm-rc, a newer
// prerelease or release), and starts the check beside the
// launch when it's due. It never asks and never blocks the launch: the
// check runs in the background, and the function it returns, which the
// launch calls once it's done, waits at most releaseWait for it. Nothing
// without a terminal or with --no-input (the UI's CanPick), with the
// check off (settings.ReleaseCheck), for a build from the source, or for
// a devvm neither install.sh nor Homebrew installed, which devvm upgrade
// can't upgrade.
func releaseNotice(ctx context.Context, a *app) (wait func()) {
	none := func() {}
	if !a.u.CanPick() {
		return none
	}
	d, err := a.dirs()
	if err != nil {
		return none
	}
	if on, err := settings.ReleaseCheck(d, os.Getenv); err != nil || !on {
		return none
	}
	up, err := a.upgrader()
	if err != nil || up.Install.Kind == upgrade.Other || !upgrade.Valid(up.Current) {
		return none
	}
	path := d.ReleaseCheck()
	pre := up.Install.Prereleases()
	known := upgrade.ReadKnown(path)
	if next := known.Next(up.Current, pre); next != "" {
		a.u.Info(upgrade.Announcement(up.Current, next))
	}
	now := time.Now()
	if !known.Due(now) {
		return none
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = up.Source.Refresh(ctx, path, now, pre) // offline is silent: the next day tries again
	}()
	return func() {
		select {
		case <-done:
		case <-time.After(releaseWait):
		}
	}
}
