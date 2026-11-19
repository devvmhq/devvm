// Package project is a project folder as devvm sees it: its path and its
// VM's name, the VMs a link's TARGET names, the folders a VM must never
// get read-write, and the parent folders whose VM a command may use.
package project

import (
	"crypto/sha1" //nolint:gosec // names, not security: 56 bits tell folders apart
	"encoding/hex"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/vmname"
)

// slugMax is the most of the folder's name a VM's name has.
const slugMax = 20

// slugMin is the least a VM's name needs of the folder's name. A backend
// whose names leave less room is refused.
const slugMin = 4

// ErrNoRoom is New's error when the backend's VM names leave no room for
// the folder's name.
var ErrNoRoom = errors.New("no room for the folder's name in the VM's name")

// Folder is a project folder and its VM's name.
type Folder struct {
	Dir  string    // its physical path, as pwd -P has it
	Name vmname.VM // its VM's name: devvm-<folder>-<hash of Dir>
}

// Slug is the folder's part of its VM's name, which is also the VM's
// hostname (vmname.VM.Slug).
func (f Folder) Slug() string {
	s, _ := f.Name.Slug()
	return s
}

// Hash is the hash of dir, a physical path, that its VM's name ends with
// (vmname.VM.Hash): 56 bits of its SHA-1, in hex. It names the VM's folder
// in devvm's data folder, so a folder's VM is found by its path,
// whatever name its backend gave it, unless the VM's folder moved there
// or away (Nth): its state, which records its folder, says then.
func Hash(dir string) string {
	sum := sha1.Sum([]byte(dir)) //nolint:gosec // see the import
	return hex.EncodeToString(sum[:])[:vmname.HashLen]
}

// IsPath reports whether a link's TARGET is a folder's path: it has a / or
// starts with a dot.
func IsPath(target string) bool {
	return strings.Contains(target, "/") || strings.HasPrefix(target, ".")
}

// hashNth is the hash of dir that the nth VM name made for it ends with
// (Nth): Hash for the first, then the hash of dir and n.
func hashNth(dir string, n int) string {
	if n == 0 {
		return Hash(dir)
	}
	return Hash(dir + "\x00" + strconv.Itoa(n))
}

// ForDir returns the project VMs among vms whose state records dir, a
// physical path, as their folder: the VM made for it, or one whose folder
// moved there, whatever its name.
func ForDir(dir string, vms []Recorded) []vmname.VM {
	var out []vmname.VM
	for _, v := range vms {
		if v.Name.IsProject() && v.Dir == dir {
			out = append(out, v.Name)
		}
	}
	return out
}

// Recorded is a project VM as its state records it: its name, the alias
// ssh reaches it by, and its project folder.
type Recorded struct {
	Name       vmname.VM
	Alias, Dir string
}

// Named returns the project VM among vms whose name or alias is name, as
// given, "" when none is. An alias the VM's name can't have
// (vmname.IsAlias) names nothing.
func Named(name string, vms []Recorded) vmname.VM {
	for _, v := range vms {
		if v.Name.IsProject() && (name == v.Name.String() || name == v.Alias && vmname.IsAlias(v.Alias, v.Name)) {
			return v.Name
		}
	}
	return ""
}

// Targets returns the project VMs among vms that a link's TARGET names
// when it isn't a path (IsPath), in vms's order: the VM itself for a VM's
// name or alias, else those whose folder's name has the same Slug, as New
// makes it, whatever their backend left of it in their names.
func Targets(target string, vms []Recorded) []vmname.VM {
	if vm := Named(target, vms); vm != "" {
		return []vmname.VM{vm}
	}
	if vmname.IsVM(target) {
		return nil
	}
	want := Slug(target, slugMax)
	var out []vmname.VM
	for _, v := range vms {
		slug := Folder{Name: v.Name}.Slug()
		if v.Dir != "" {
			slug = Slug(filepath.Base(v.Dir), slugMax)
		}
		if v.Name.IsProject() && slug == want {
			out = append(out, v.Name)
		}
	}
	return out
}

// BadPathError is a folder whose path Lima can't mount: it has a comma
// (Lima splits mount options on commas) or a control character.
type BadPathError struct{ Dir string }

func (e *BadPathError) Error() string { return "can't mount " + e.Dir }

// New returns the project folder at dir, a physical path, and its VM's
// name, fitting nameMax characters (0 for no limit).
func New(dir string, nameMax int) (Folder, error) { return Nth(dir, nameMax, 0) }

// Nth is New with the nth name a VM for dir can have, from 0, each
// ending with a hash of its own (hashNth): a VM whose folder moved away
// from dir keeps its name, and so its folder in devvm's data folder, so
// a new VM for dir takes the next name.
func Nth(dir string, nameMax, n int) (Folder, error) {
	if strings.Contains(dir, ",") || strings.IndexFunc(dir, unicode.IsControl) >= 0 { //nolint:forbidigo // what a VM's mount takes, not the terminal
		return Folder{}, &BadPathError{Dir: dir}
	}
	room := slugMax
	if nameMax > 0 {
		room = min(room, nameMax-len(vmname.Prefix)-1-vmname.HashLen)
	}
	if room < slugMin {
		return Folder{}, ErrNoRoom
	}
	// Slug and hashNth make a name vmname always takes.
	name, err := vmname.Parse(vmname.Prefix + Slug(filepath.Base(dir), room) + "-" + hashNth(dir, n))
	return Folder{Dir: dir, Name: name}, err
}

// Slug is a folder's name as it goes into its VM's name: lowercase, each
// run of characters but a-z and 0-9 as one dash, as Lima refuses a name
// with separators in a row, without dashes at either end, at most n
// characters (root when nothing's left).
func Slug(base string, n int) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(base) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case !dash:
			b.WriteByte('-')
			dash = true
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		s = "root"
	}
	if len(s) > n {
		s = strings.TrimRight(s[:n], "-")
	}
	return s
}

// Unsafe is what a project folder mustn't be, hold or be inside: the user's
// home, devvm's config and data folders and its Lima home. A VM gets its
// folder read-write.
type Unsafe struct {
	Home, Config, Data, Lima string
}

// Holds returns the first of these folders that dir, a physical path, is
// or holds (any folder holds /), each by its physical path
// (paths.Physical) and by identity (paths.In), so a name in another case
// on a volume that ignores it holds it too, or "". The VM would get them
// read-write: tokens, the proxy's CA key and binary, other VMs' disks.
func (u Unsafe) Holds(dir string) string {
	for _, p := range []string{u.Home, u.Config, u.Data, u.Lima} {
		if p != "" && paths.In(paths.Physical(p), dir) {
			return p
		}
	}
	return ""
}

// Inside returns the first of devvm's own folders that dir, a physical
// path, is below, by name or identity (paths.Under), or "". A VM in the
// layers folder could rewrite the scripts every image build runs; one in
// the data folder, the binaries that run on the host.
func (u Unsafe) Inside(dir string) string {
	for _, p := range []string{u.Config, u.Data, u.Lima} {
		if p != "" && paths.Under(dir, paths.Physical(p)) {
			return p
		}
	}
	return ""
}

// Parents returns the folders dir, a physical path, is inside, nearest
// first, whose VM a command in dir may use: up to the home folder (home,
// resolved) and the root, both left out, as is everything above home
// when dir is home or in it.
func Parents(dir, home string) []string {
	home = paths.Physical(home)
	if dir == home {
		return nil
	}
	var ps []string
	for p := filepath.Dir(dir); p != home && filepath.Dir(p) != p; p = filepath.Dir(p) {
		ps = append(ps, p)
	}
	return ps
}
