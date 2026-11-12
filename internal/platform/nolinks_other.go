//go:build !unix

package platform

import (
	"errors"
	"os"
)

// ErrLinkOnPath is OpenNoLinks's error for a path with a symbolic link
// in it: never on this system, where OpenNoLinks opens nothing.
var ErrLinkOnPath = errors.New("a symbolic link is on the path")

// OpenNoLinks is unsupported here.
func OpenNoLinks(string) (*os.File, int, error) { return nil, 0, errors.ErrUnsupported }
