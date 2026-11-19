package platform

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// diskutil's plist read for its top dictionary's strings, and nothing below it.
func TestPlistStrings(t *testing.T) {
	data := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>MountPoint</key><string>/System/Volumes/Data</string>
<key>Nested</key><dict><key>VolumeUUID</key><string>NOT-THIS</string></dict>
<key>Writable</key><true/>
<key>VolumeUUID</key><string>EC5E5014-21A3-4154-ACE8-0C8C1C254852</string>
</dict></plist>`)
	got, err := plistStrings(data)
	if err != nil || got["MountPoint"] != "/System/Volumes/Data" || got["VolumeUUID"] != "EC5E5014-21A3-4154-ACE8-0C8C1C254852" || len(got) != 2 {
		t.Errorf("plistStrings = %v, %v", got, err)
	}
	if _, err := plistStrings([]byte("<plist><dict><key>")); err == nil {
		t.Error("a broken plist read")
	}
}

// A folder in the test's own temporary folder, on this Mac's own volume: read with no link on
// its path, its volume's UUID is diskutil's for its mount point and device, the same at each
// read; a folder whose path has a link isn't read.
func TestStatFolderAndVolumeUUID(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s, err := StatFolder(dir)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := FolderID(dir)
	if s.ID != id || s.Mount == "" || s.Device == "" || s.ReadOnly || s.Volume != "" {
		t.Fatalf("StatFolder = %+v, FolderID %s", s, id)
	}
	if !s.Unique {
		t.Skipf("not on APFS: %+v", s)
	}
	vol, err := VolumeUUID(t.Context(), s)
	if err != nil || !strings.HasPrefix(vol, "uuid:") {
		t.Fatalf("VolumeUUID = %q, %v", vol, err)
	}
	if again, _ := VolumeUUID(t.Context(), s); again != vol {
		t.Errorf("again = %q, want %q", again, vol)
	}
	moved := s
	moved.Device = "disk999s9"
	if got, err := VolumeUUID(t.Context(), moved); err == nil {
		t.Errorf("another device's answer taken: %q", got)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if _, err := StatFolder(link); err == nil {
		t.Error("a folder read through a link")
	}
}
