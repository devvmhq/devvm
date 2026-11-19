//go:build darwin || linux

package platform

import "fmt"

// StatFolder is the folder at dir as FolderStat has it, read from the folder opened with no link
// on its path (OpenNoLinks, none of the system's followed either), so nothing a VM puts at the
// path, or above it, while devvm reads it counts.
func StatFolder(dir string) (FolderStat, error) {
	f, hops, err := OpenNoLinks(dir)
	if err != nil {
		return FolderStat{}, err
	}
	defer func() { _ = f.Close() }()
	if hops != 0 {
		return FolderStat{}, fmt.Errorf("%s: a link on its path", dir)
	}
	return openFolder(f)
}
