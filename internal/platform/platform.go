// Package platform holds the code that differs between host systems,
// behind build tags: *_unix.go for macOS and Linux, *_darwin.go and
// *_linux.go for one of them, and *_other.go for the rest, which get an
// error until they're supported.
package platform

import (
	"errors"
	"os"
	"strings"
)

// ErrNoSecret is KeychainGet's error for an item that isn't there.
var ErrNoSecret = errors.New("no such secret")

// folderDevice returns the device in a FolderID, "" when id has none. Only
// platform reads it: a boot can number an unchanged folder's device anew,
// so what compares one is SameFolder's and OnRecordedDisk's.
func folderDevice(id string) string {
	dev, _, ok := strings.Cut(id, ":")
	if !ok {
		return ""
	}
	return dev
}

// SameFolder reports whether id, the FolderID of the folder at dir now,
// names the folder recorded as was when it was at wasDir: the same ID, or
// at the same path the same inode and birth time on another device. It's
// the one place two FolderIDs are compared. macOS numbers its disks'
// devices in the order they appear at boot, so after a restart an
// unchanged folder can have another device number. Only a folder whose ID
// is its alone (FolderStat.Unique), at a path with no link on it, matches on
// another device: what a VM can put in a folder's place, a link or a new
// folder, can't take an inode and a birth time, and a copy that keeps
// both, such as a snapshot or a cloned disk, is at another path, unless
// you mount it at the folder's own. A folder at another path (moved)
// matches only its ID as recorded. On another device, id must also be
// the ID of the folder opened at dir with no link followed (OpenNoLinks),
// read from that open folder, so a link put at the path, or above it,
// while the caller read id can't pass a copy's ID off as the folder's.
func SameFolder(wasDir, was, dir, id string) bool {
	if was == "" || id == "" {
		return false
	}
	if was == id {
		return true
	}
	if !renumberedID(wasDir, was, dir, id) {
		return false
	}
	s, err := StatFolder(dir)
	return err == nil && s.Unique && s.ID == id
}

// SameOpenFolder is SameFolder for the folder s describes, as StatFolder
// read it at dir from the folder opened with no link on its path, so it
// opens nothing again: a caller that has s compares with it, once.
func SameOpenFolder(wasDir, was, dir string, s FolderStat) bool {
	if was == "" || s.ID == "" {
		return false
	}
	return was == s.ID || s.Unique && renumberedID(wasDir, was, dir, s.ID)
}

// RecordedAs reports whether s's ID is was, the ID recorded, byte for
// byte: the folder SameOpenFolder took for the recorded one, on the
// device it was recorded on, not one numbered anew since.
func RecordedAs(was string, s FolderStat) bool { return was != "" && was == s.ID }

// renumberedID reports whether id could be the folder recorded as was on
// another device, by the IDs alone: at the same path, with the same inode
// and birth time.
func renumberedID(wasDir, was, dir, id string) bool {
	wasDev, wasRest, _ := strings.Cut(was, ":")
	dev, rest, _ := strings.Cut(id, ":")
	return wasDir == dir && digits(wasDev) && digits(dev) && wasRest == rest && strings.Count(id, ":") == 2
}

// Renumbered reports whether id names the folder recorded as was
// (SameFolder) on a device numbered anew since, so the record needs the
// folder's ID as it is now.
func Renumbered(wasDir, was, dir, id string) bool {
	return was != id && SameFolder(wasDir, was, dir, id)
}

// VolPath returns the path of the folder a FolderID names by its device
// and inode, /.vol/<dev>/<ino>, which macOS's volfs opens whatever path
// the folder has now; "" when id has neither. Only macOS has /.vol.
func VolPath(id string) string {
	dev, rest, ok := strings.Cut(id, ":")
	ino, _, _ := strings.Cut(rest, ":")
	if !ok || !digits(dev) || !digits(ino) {
		return ""
	}
	return "/.vol/" + dev + "/" + ino
}

// digits reports whether s is a decimal number, without a sign.
func digits(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}

// DeviceOf returns the device of what's at path, following a symlink, as
// FolderID writes it.
func DeviceOf(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	dev := folderDevice(DevIno(fi))
	if dev == "" {
		return "", errors.ErrUnsupported
	}
	return dev, nil
}
