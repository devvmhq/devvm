package platform

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"time"

	"golang.org/x/sys/unix"
)

// StatFolder is the folder at dir as FolderStat has it, read from the folder opened with no link
// on its path (OpenNoLinks, none of the system's followed either), so nothing a VM puts at the
// path, or above it, while devvm reads it counts. Its volume is VolumeUUID's, so Volume is "".
func StatFolder(dir string) (FolderStat, error) {
	f, hops, err := OpenNoLinks(dir)
	if err != nil {
		return FolderStat{}, err
	}
	defer func() { _ = f.Close() }()
	if hops != 0 {
		return FolderStat{}, fmt.Errorf("%s: a link on its path", dir)
	}
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return FolderStat{}, err
	}
	var fs unix.Statfs_t
	if err := unix.Fstatfs(int(f.Fd()), &fs); err != nil {
		return FolderStat{}, err
	}
	name := unix.ByteSliceToString(fs.Fstypename[:])
	return FolderStat{
		ID:       statID(&st),
		Unique:   name == "apfs" || name == "hfs",
		ReadOnly: fs.Flags&(unix.MNT_RDONLY|unix.MNT_SNAPSHOT) != 0,
		Mount:    unix.ByteSliceToString(fs.Mntonname[:]),
		Device:   filepath.Base(unix.ByteSliceToString(fs.Mntfromname[:])),
	}, nil
}

// diskutil is macOS's tool for its disks, which devvm runs by its absolute path.
const diskutil = "/usr/sbin/diskutil"

// uuidForm is a volume UUID's form, as diskutil writes it.
var uuidForm = regexp.MustCompile(`^[0-9A-F]{8}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{12}$`)

// VolumeUUID is the volume of the folder s describes, "uuid:<VolumeUUID>", as diskutil says for
// its mount point: the UUID macOS keeps on the volume, which no renumbering of its disk changes.
// It runs diskutil itself, not through the run log, whose LoggedCmd keeps a command's output,
// which this needs; the output holds no secret. The answer must be for that mount point and the
// device mounted there, as StatFolder read them from the open folder, so a volume mounted there
// since isn't taken for it. It fails for a read-only volume or a snapshot, which it never
// identifies, and within ctx.
func VolumeUUID(ctx context.Context, s FolderStat) (string, error) {
	if s.ReadOnly || s.Mount == "" || s.Device == "" {
		return "", errors.New("no volume to identify")
	}
	cmd := exec.CommandContext(ctx, diskutil, "info", "-plist", s.Mount) //nolint:gosec // devvm's own arguments: a mount point read from an open folder
	cmd.WaitDelay = time.Second                                          // its pipes, once ctx ends, even if a child of its holds them
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("diskutil info %s: %w", s.Mount, err)
	}
	info, err := plistStrings(out)
	if err != nil {
		return "", fmt.Errorf("diskutil info %s: %w", s.Mount, err)
	}
	if info["MountPoint"] != s.Mount || info["DeviceIdentifier"] != s.Device || !uuidForm.MatchString(info["VolumeUUID"]) {
		return "", fmt.Errorf("diskutil info %s: not the volume mounted there", s.Mount)
	}
	return "uuid:" + info["VolumeUUID"], nil
}

// plistMax bounds what of diskutil's plist devvm reads.
const plistMax = 1 << 20

// plistStrings is the string values of a plist's top dictionary, by key: an XML property list,
// read with encoding/xml, each <key> followed by a <string>; other values are skipped.
func plistStrings(data []byte) (map[string]string, error) {
	if len(data) > plistMax {
		return nil, errors.New("a plist too big")
	}
	d := xml.NewDecoder(bytes.NewReader(data))
	out := map[string]string{}
	depth, key := 0, ""
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			return out, nil
		} else if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if depth != 3 {
				key = ""
				continue
			}
			var s string
			if err := d.DecodeElement(&s, &t); err != nil {
				return nil, err
			}
			depth--
			switch t.Name.Local {
			case "key":
				key = s
			case "string":
				if key != "" {
					out[key] = s
				}
				key = ""
			default:
				key = ""
			}
		case xml.EndElement:
			depth--
		}
	}
}
