//go:build !linux && !darwin

package platform

import (
	"context"
	"errors"
)

// ReadClip is unsupported here.
func (Clipboard) ReadClip(context.Context, int64) (Clip, error) { return Clip{}, errors.ErrUnsupported }

// CopyText is unsupported here.
func (Clipboard) CopyText(context.Context, string) error { return errors.ErrUnsupported }
