//go:build !darwin && !linux

package platform

import (
	"context"
	"errors"
)

// StatFolder is unsupported here.
func StatFolder(string) (FolderStat, error) { return FolderStat{}, errors.ErrUnsupported }

// VolumeUUID is unsupported here.
func VolumeUUID(context.Context, FolderStat) (string, error) { return "", errors.ErrUnsupported }
