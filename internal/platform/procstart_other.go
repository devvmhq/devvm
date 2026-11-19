//go:build !darwin && !linux

package platform

import "errors"

// ProcessStart is unsupported here.
func ProcessStart(int) (string, error) { return "", errors.ErrUnsupported }
