//go:build e2e

package e2e

// projectRenumbered is macOS's: a Linux host can't number a disk anew without root, and the
// project section's move (projectMoved) checks a moved folder there.
func projectRenumbered(*S) {}
