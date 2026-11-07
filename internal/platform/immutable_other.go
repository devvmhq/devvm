//go:build !darwin && !linux

package platform

import (
	"context"
	"errors"
)

// LocksNeedHelper reports whether locking needs devvm-immutable: there's
// no locking on this system.
const LocksNeedHelper = false

// Usable reports why files in dir can't be locked: this system has no
// immutable flag devvm knows.
func (Locker) Usable(context.Context, string) error { return errors.ErrUnsupported }

// HelperUsable reports why devvm-immutable can't lock files: there's none
// for this system.
func (Locker) HelperUsable(context.Context) error { return errors.ErrUnsupported }

// Chflags sets file flags: not on this system.
func (Locker) Chflags(context.Context, string, string) error { return errors.ErrUnsupported }

// Immutable reports whether a path has the immutable flag: not on this
// system.
func Immutable(string) bool { return false }
