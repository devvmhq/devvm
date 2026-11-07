//go:build !darwin && !linux

package platform

import "io/fs"

// ChangeID is a file's inode and change time: not on this system yet.
func ChangeID(fs.FileInfo) string { return "" }
