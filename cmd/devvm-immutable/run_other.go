//go:build !linux

package main

import (
	"fmt"
	"io"
)

// run refuses: the immutable attribute it sets is Linux's.
func run(_ []string, _, stderr io.Writer) int {
	_, _ = fmt.Fprintln(stderr, "devvm-immutable: runs on Linux only")
	return 2
}
