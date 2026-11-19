package platform

import "testing"

// A volume is recorded bound to its folder's inode and birth time, and names its volume only
// while those are the folder's; a folder without a birth time, or a volume of another form,
// records none.
func TestVolumeOf(t *testing.T) {
	for _, c := range []struct{ vol, id, want string }{
		{"uuid:A", "16777234:207:1794.5", "uuid:A:207:1794.5"},
		{"fsid:0a0b", "2049:12:1.2", "fsid:0a0b:12:1.2"},
		{"", "16777234:207:1794.5", ""},
		{"uuid:A", "16777234:207", ""},
		{"uuid:A", "", ""},
		{"A", "16777234:207:1794.5", ""},
		{"uuid:A:B", "16777234:207:1794.5", ""},
	} {
		if got := VolumeOf(c.vol, c.id); got != c.want {
			t.Errorf("VolumeOf(%q, %q) = %q, want %q", c.vol, c.id, got, c.want)
		}
	}
	for _, c := range []struct{ id, recorded, want string }{
		{"16777234:207:1794.5", "uuid:A:207:1794.5", "uuid:A"},
		{"99:207:1794.5", "uuid:A:207:1794.5", "uuid:A"}, // renumbered: the binding is the inode's
		{"16777234:208:1794.5", "uuid:A:207:1794.5", ""}, // another folder's, as an older devvm's move leaves it
		{"16777234:207:1794.5", "", ""},
		{"16777234:207:1794.5", ":207:1794.5", ""},
		{"16777234:207", "uuid:A:207", ""},
	} {
		if got := RecordedVolume(c.id, c.recorded); got != c.want {
			t.Errorf("RecordedVolume(%q, %q) = %q, want %q", c.id, c.recorded, got, c.want)
		}
	}
}

// A moved folder is the recorded one on a renumbered disk only with the recorded volume, inode
// and birth time, its alone, on a volume mounted for writing; a parent is on the recorded disk
// by its volume when both are known, else by its device.
func TestMovedFolderAndRecordedDisk(t *testing.T) {
	was, rec := "9:207:1794.5", "uuid:A:207:1794.5"
	now := FolderStat{ID: "7:207:1794.5", Unique: true}
	ro, notUnique, other := now, now, now
	ro.ReadOnly, notUnique.Unique, other.ID = true, false, "7:208:1794.5"
	for _, c := range []struct {
		name string
		now  FolderStat
		vol  string
		want bool
	}{
		{"the recorded volume", now, "uuid:A", true},
		{"another volume", now, "uuid:B", false},
		{"no volume", now, "", false},
		{"read-only or a snapshot", ro, "uuid:A", false},
		{"an identity that isn't its alone", notUnique, "uuid:A", false},
		{"another inode", other, "uuid:A", false},
	} {
		if got := MovedFolder(was, rec, c.now, c.vol); got != c.want {
			t.Errorf("%s: MovedFolder = %v, want %v", c.name, got, c.want)
		}
	}
	if MovedFolder(was, "", now, "uuid:A") || MayHaveMoved(was, "", now) {
		t.Error("a record without a volume matched")
	}
	for _, c := range []struct {
		recorded, dev, vol string
		want               bool
	}{
		{rec, "7", "uuid:A", true},
		{rec, "9", "uuid:B", false},
		{rec, "7", "", false},
		{rec, "9", "", true},
		{"", "9", "uuid:A", true},
		{"", "7", "uuid:A", false},
	} {
		if got := OnRecordedDisk(was, c.recorded, c.dev, c.vol); got != c.want {
			t.Errorf("OnRecordedDisk(%q, %q, %q) = %v, want %v", c.recorded, c.dev, c.vol, got, c.want)
		}
	}
	// A record with no device can't tell another disk: the same disk, so the folder's gone.
	for _, id := range []string{"", "12"} {
		if !OnRecordedDisk(id, "", "7", "") {
			t.Errorf("OnRecordedDisk(%q) with no device recorded = false", id)
		}
	}
}

// A folder at its own path on a disk numbered anew keeps the volume recorded for it when that's
// the volume it's on, or when either can't be told; on another volume it's another folder.
func TestKeepsVolume(t *testing.T) {
	was, rec := "9:207:1794.5", "uuid:A:207:1794.5"
	for _, c := range []struct {
		recorded, vol string
		want          bool
	}{
		{rec, "uuid:A", true},
		{rec, "uuid:B", false},
		{rec, "fsid:0a", false},
		{rec, "", true},
		{"", "uuid:B", true},
		{"uuid:A:208:1794.5", "uuid:B", true}, // bound to another folder: no volume recorded
	} {
		if got := KeepsVolume(was, c.recorded, c.vol); got != c.want {
			t.Errorf("KeepsVolume(%q, %q) = %v, want %v", c.recorded, c.vol, got, c.want)
		}
	}
}
