package platform

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// LocksNeedHelper reports whether locking needs devvm-immutable: on
// Linux, whose immutable attribute only CAP_LINUX_IMMUTABLE sets.
const LocksNeedHelper = true

// helperProtocol is the protocol of devvm-immutable that devvm speaks,
// which its check prints.
const helperProtocol = "1"

// helperWait bounds devvm-immutable's check, which returns at once, and
// lockWait one path's set or clear, which devvm-immutable bounds by its
// count of files too.
const (
	helperWait = 10 * time.Second
	lockWait   = 2 * time.Minute
)

// Usable reports why files in dir can't be locked, nil when they can:
// devvm-immutable is missing or doesn't work (HelperUsable), or dir's
// file system has no immutable attribute (ErrNoAttribute).
func (l Locker) Usable(ctx context.Context, dir string) error {
	if err := l.HelperUsable(ctx); err != nil {
		return err
	}
	return hasAttribute(dir)
}

// HelperUsable reports why devvm-immutable can't lock files, nil when it
// can: it isn't there (ErrNoHelper), or its check fails, as it doesn't
// hold CAP_LINUX_IMMUTABLE, or it speaks another protocol
// (ErrHelperUnusable, with its reason). The check runs it, so it finds
// what reading its file capability wouldn't: a file system mounted
// nosuid, or a devvm run with no_new_privs, gives it none.
func (l Locker) HelperUsable(ctx context.Context) error {
	if l.Helper == "" {
		return ErrNoHelper
	}
	if _, err := os.Stat(l.Helper); errors.Is(err, fs.ErrNotExist) {
		return ErrNoHelper
	} else if err != nil {
		return fmt.Errorf("%w: %w", ErrHelperUnusable, err)
	}
	ctx, cancel := context.WithTimeout(ctx, helperWait)
	defer cancel()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, l.Helper, "check") //nolint:gosec // devvm's own helper
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		why := strings.TrimPrefix(strings.TrimSpace(stderr.String()), "devvm-immutable: ")
		return fmt.Errorf("%w: %s", ErrHelperUnusable, cmp.Or(why, err.Error()))
	}
	if got := strings.TrimSpace(string(out)); got != helperProtocol {
		return fmt.Errorf("%w: it speaks protocol %q, and this devvm %s", ErrHelperUnusable, got, helperProtocol)
	}
	return nil
}

// hasAttribute reports whether dir's file system has the immutable
// attribute, as statx says: ext4, XFS, Btrfs and tmpfs do, NFS and FUSE
// don't. A folder it can't read counts as one without.
func hasAttribute(dir string) error {
	var st unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, dir, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_TYPE, &st); err != nil {
		return fmt.Errorf("%w: %w", ErrNoAttribute, err)
	}
	if st.Attributes_mask&unix.STATX_ATTR_IMMUTABLE == 0 {
		return ErrNoAttribute
	}
	return nil
}

// Chflags sets (uchg) or clears (nouchg) the immutable attribute on path
// and everything under it, following no symlink, path itself included,
// through devvm-immutable; its error has what the helper said.
func (l Locker) Chflags(ctx context.Context, flags, path string) error {
	op := ""
	switch flags {
	case "uchg":
		op = "set"
	case "nouchg":
		op = "clear"
	default:
		return errors.ErrUnsupported
	}
	if l.Helper == "" {
		return ErrNoHelper
	}
	ctx, cancel := context.WithTimeout(ctx, lockWait)
	defer cancel()
	out, err := exec.CommandContext(ctx, l.Helper, op, path).CombinedOutput() //nolint:gosec // devvm's own helper and paths
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Immutable reports whether path itself (not what a symlink points to) has
// the immutable attribute, as statx reports it, so it reads a file it
// can't open too.
func Immutable(path string) bool {
	var st unix.Statx_t
	return unix.Statx(unix.AT_FDCWD, path, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_TYPE, &st) == nil &&
		st.Attributes&unix.STATX_ATTR_IMMUTABLE != 0
}
