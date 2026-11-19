package platform

import (
	"context"
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// fsidOwn are the file systems whose f_fsid is the file system's own, never its device's: ext4
// derives it from its superblock's UUID, btrfs from its UUID and the subvolume's root, so a
// snapshot has another, and ZFS from the dataset's GUID. XFS and F2FS derive it from the
// device, tmpfs per mount, so they, and the rest, have no volume.
func fsidOwn() []int64 { return []int64{unix.EXT4_SUPER_MAGIC, unix.BTRFS_SUPER_MAGIC, zfsSuperMagic} }

// StatFolder is the folder at dir as FolderStat has it, read from the folder opened with no link
// on its path (OpenNoLinks, none of the system's followed either), so nothing a VM puts at the
// path, or above it, while devvm reads it counts. Its volume is "fsid:<f_fsid>" on a file system
// whose f_fsid is its own (fsidOwn), mounted for writing, and "" elsewhere.
func StatFolder(dir string) (FolderStat, error) {
	f, hops, err := OpenNoLinks(dir)
	if err != nil {
		return FolderStat{}, err
	}
	defer func() { _ = f.Close() }()
	if hops != 0 {
		return FolderStat{}, fmt.Errorf("%s: a link on its path", dir)
	}
	id, unique, err := openFolderID(f)
	if err != nil {
		return FolderStat{}, err
	}
	var fs unix.Statfs_t
	if err := unix.Fstatfs(int(f.Fd()), &fs); err != nil {
		return FolderStat{}, err
	}
	s := FolderStat{ID: id, Unique: unique, ReadOnly: fs.Flags&unix.ST_RDONLY != 0}
	if !s.ReadOnly && fs.Fsid.Val != [2]int32{} {
		for _, t := range fsidOwn() {
			if fs.Type == t {
				s.Volume = fmt.Sprintf("fsid:%08x%08x", uint32(fs.Fsid.Val[0]), uint32(fs.Fsid.Val[1])) //nolint:gosec // G115: the ID's bits as they are, not a number's value
			}
		}
	}
	return s, nil
}

// VolumeUUID is macOS's: on Linux a folder's volume is StatFolder's, read in the process.
func VolumeUUID(context.Context, FolderStat) (string, error) {
	return "", errors.ErrUnsupported
}
