package platform

import (
	"context"
	"errors"

	"golang.org/x/sys/unix"
)

// fsidOwn are the file systems whose f_fsid is the file system's own, never its device's: ext4
// derives it from its superblock's UUID, btrfs from its UUID and the subvolume's root, so a
// snapshot has another, and ZFS from the dataset's GUID. XFS and F2FS derive it from the
// device, tmpfs per mount, so they, and the rest, have no volume.
func fsidOwn() []int64 { return []int64{unix.EXT4_SUPER_MAGIC, unix.BTRFS_SUPER_MAGIC, zfsSuperMagic} }

// VolumeUUID is macOS's: on Linux a folder's volume is StatFolder's, read in the process.
func VolumeUUID(context.Context, FolderStat) (string, error) {
	return "", errors.ErrUnsupported
}
