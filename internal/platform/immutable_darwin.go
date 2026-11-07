package platform

import (
	"context"
	"os/exec"

	"golang.org/x/sys/unix"
)

// LocksNeedHelper reports whether locking needs devvm-immutable: not on
// macOS, whose user-immutable flag the user sets without root.
const LocksNeedHelper = false

// Usable reports why files in dir can't be locked: never, on macOS.
func (Locker) Usable(context.Context, string) error { return nil }

// HelperUsable reports why devvm-immutable can't lock files: macOS needs
// none.
func (Locker) HelperUsable(context.Context) error { return nil }

// Chflags sets file flags (uchg, nouchg) on path and everything under it,
// following no symlink, path itself included: chflags -R -P.
func (Locker) Chflags(ctx context.Context, flags, path string) error {
	return exec.CommandContext(ctx, "chflags", "-R", "-P", flags, path).Run() //nolint:gosec // devvm's own arguments
}

// Immutable reports whether path itself (not what a symlink points to) has
// the user-immutable flag (uchg).
func Immutable(path string) bool {
	var st unix.Stat_t
	return unix.Lstat(path, &st) == nil && st.Flags&unix.UF_IMMUTABLE != 0
}
