package stubtest

import (
	"strconv"
	"strings"
)

// Renumbered is id, a project folder's identity (platform.FolderID), as
// it reads after a boot that numbered the folder's device anew, which
// macOS does in the order its disks appear: its device plus one, its
// inode and birth time as they were. An id without a device is as it is.
func Renumbered(id string) string {
	dev, rest, ok := strings.Cut(id, ":")
	n, err := strconv.ParseUint(dev, 10, 64)
	if !ok || err != nil {
		return id
	}
	return strconv.FormatUint(n+1, 10) + ":" + rest
}
