//go:build !darwin && !linux

package platform

import (
	"context"
	"errors"
)

// FolderID returns what identifies a folder itself: not on this system yet.
func FolderID(string) (string, error) { return "", errors.ErrUnsupported }

// UniqueFolderID reports whether a FolderID is its folder's alone: never
// on this system.
func UniqueFolderID(_, _ string) bool { return false }

// HasImmutableFlag reports whether this system has a user-immutable flag:
// not this one.
const HasImmutableFlag = false

// FoldsCase reports whether file names here may match whatever their
// case: taken to on this system, so a .GIT counts as .git.
const FoldsCase = true

// Chflags sets file flags: not on this system.
func Chflags(context.Context, string, string) error { return errors.ErrUnsupported }

// Immutable reports whether a path has the user-immutable flag: not on
// this system.
func Immutable(string) bool { return false }
