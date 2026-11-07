//go:build !darwin && !linux

package platform

import "errors"

// FolderID returns what identifies a folder itself: not on this system yet.
func FolderID(string) (string, error) { return "", errors.ErrUnsupported }

// UniqueFolderID reports whether a FolderID is its folder's alone: never
// on this system.
func UniqueFolderID(_, _ string) bool { return false }
