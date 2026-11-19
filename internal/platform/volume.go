package platform

import "strings"

// A folder's volume is what identifies the volume it's on whatever device number the system
// gives that volume: its kind and value, "uuid:<VolumeUUID>" on macOS (VolumeUUID), or
// "fsid:<f_fsid>" on Linux for a file system that derives f_fsid from itself (FolderStat). A
// state records it bound to the folder's inode and birth time (VolumeOf), so a record that an
// older devvm left beside a folder it moved names no volume (RecordedVolume). With it, a folder
// that moved and whose disk was numbered anew since is still recognised (MovedFolder), and a
// renumbered disk isn't taken for an unmounted one (OnRecordedDisk).

// VolumeOf is vol, a volume's kind and value, bound to the folder whose FolderID is id: vol,
// then id's inode and birth time, as a state records it; "" when either is missing.
func VolumeOf(vol, id string) string {
	rest := folderRest(id)
	if vol == "" || rest == "" || strings.Count(vol, ":") != 1 {
		return ""
	}
	return vol + ":" + rest
}

// folderRest is a FolderID's inode and birth time, "" for one without a birth time.
func folderRest(id string) string {
	dev, rest, ok := strings.Cut(id, ":")
	if !ok || !digits(dev) || strings.Count(rest, ":") != 1 {
		return ""
	}
	return rest
}

// RecordedVolume is the volume (kind and value) that recorded, a state's folder volume
// (VolumeOf), names for the folder whose recorded FolderID is id, when it's bound to that
// folder's inode and birth time; "" otherwise.
func RecordedVolume(id, recorded string) string {
	rest := folderRest(id)
	if rest == "" || !strings.HasSuffix(recorded, ":"+rest) {
		return ""
	}
	vol := strings.TrimSuffix(recorded, ":"+rest)
	if strings.Count(vol, ":") != 1 {
		return ""
	}
	return vol
}

// MovedFolder reports whether now, a folder at another path than the one recorded as wasID on
// the volume recorded (VolumeOf), is that folder, on a disk numbered anew since: its volume,
// vol (now's, as VolumeUUID or FolderStat gives it), is the recorded one, it has the recorded
// inode and birth time, and they're its alone (Unique), on a volume mounted for writing, never
// a snapshot. A folder whose ID is the recorded one is SameFolder's.
func MovedFolder(wasID, recorded string, now FolderStat, vol string) bool {
	return MayHaveMoved(wasID, recorded, now) && vol == RecordedVolume(wasID, recorded)
}

// MayHaveMoved reports whether now could be the folder recorded as wasID, on the volume
// recorded, at another path (MovedFolder), from what costs nothing to compare: a volume bound
// to it, and its inode and birth time, its alone, on a volume mounted for writing. Only then is
// now's volume worth looking up.
func MayHaveMoved(wasID, recorded string, now FolderStat) bool {
	return RecordedVolume(wasID, recorded) != "" && now.Unique && !now.ReadOnly && folderRest(now.ID) == folderRest(wasID)
}

// OnRecordedDisk reports whether a folder whose device is dev and whose volume is vol ("" when
// it can't be told) is on the disk that the folder recorded as wasID, on the volume recorded,
// was on: the same volume when both are known, else the same device, or any disk for a record
// that names no device, which can't tell another.
func OnRecordedDisk(wasID, recorded, dev, vol string) bool {
	if was := RecordedVolume(wasID, recorded); was != "" && vol != "" {
		return vol == was
	}
	return folderDevice(wasID) == "" || dev == folderDevice(wasID)
}

// KeepsVolume reports whether vol, the volume of the folder at the path recorded for the folder
// recorded as wasID, which SameFolder took for it, is the volume recorded with it, as a launch
// checks it on a disk numbered anew since (Renumbered), for a record whose volume is unchecked,
// and on Linux at every launch: the same volume, or none to compare, as for a state that records
// none bound to it or a volume that can't be told ("" ), where the device's rule alone decides.
// On another volume it's another folder that has the inode and birth time, such as a clone of
// the disk mounted at the folder's own path.
func KeepsVolume(wasID, recorded, vol string) bool {
	was := RecordedVolume(wasID, recorded)
	return was == "" || vol == "" || vol == was
}

// FolderStat is what identifies a folder and its volume, read from the folder opened with no
// link on its path (StatFolder): its FolderID; whether that's its alone (UniqueFolderID);
// whether its volume is mounted read-only, or is a snapshot; its mount point and the device
// mounted there, by name (macOS, for VolumeUUID); its volume, read in the process where the
// system gives it ("fsid:…" on Linux's ext4, btrfs and ZFS), "" otherwise; and its owner's
// user ID, from the same read, so a folder's owner is never another folder's.
type FolderStat struct {
	ID       string
	Unique   bool
	ReadOnly bool
	Mount    string
	Device   string
	Volume   string
	Owner    uint32
}
