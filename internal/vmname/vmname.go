// Package vmname is the name of every machine devvm runs on a backend,
// VM, and the grammar of a project VM's, devvm-<folder>-<hash>: what
// starts it, how long its hash is, whether a name is one, and its folder
// part. Standard library only, so the backends and the proxy read names
// as internal/project makes them.
//
// A VM is made from a string only where it's read. Parse takes a project
// VM's name from an argument or a file name, and also reads vm.json's
// name and runs in project.Nth. ParseListed takes one a backend lists,
// VM's UnmarshalText one in a JSON file (one path element, or none),
// Image names an image's machine and VM.Aside a remake's old one, and a
// backend's build VMs are typed constants. msg.M.VM reads back the VM a
// message's function took. Everywhere else a VM's name is a VM, never a
// string, which the structure test checks. A JSON field read with
// UnmarshalText is any machine's name, so one that must be a project VM's
// is still checked with IsProject.
package vmname

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// VM is the name of a machine devvm runs on a backend: a project VM's
// (devvm-<folder>-<hash>, IsProject), an image's (devvm-img-<key>,
// Image), a build VM's (a backend's constant, such as devvm-tools-build),
// or a project VM's old machine while a remake makes the new one
// (devvm-<hash>-old, Aside). Its JSON and text are the name itself.
type VM string

// String is the name.
func (v VM) String() string { return string(v) }

// Join is vms's names joined by sep, as strings.Join joins strings.
func Join(vms []VM, sep string) string {
	s := make([]string, len(vms))
	for i, v := range vms {
		s[i] = string(v)
	}
	return strings.Join(s, sep)
}

// Parse returns name as a project VM's, and an error naming it when it
// isn't one (IsVM).
func Parse(name string) (VM, error) {
	if !IsVM(name) {
		return "", fmt.Errorf("invalid VM name %q", name)
	}
	return VM(name), nil
}

// ParseListed returns name, as a backend lists a machine it has, as a VM:
// any name that is one path element, as a backend keeps each machine in a
// folder by its name, so a machine devvm didn't make (a stray) is listed
// too. A name starting with - is refused, as a name goes into a backend's
// command line (limactl's), where it would read as a flag. Whether it's a
// project VM's is IsProject's to say.
func ParseListed(name string) (VM, error) {
	if name == "" || name == "." || name == ".." || strings.HasPrefix(name, "-") || strings.ContainsAny(name, "/\x00") {
		return "", fmt.Errorf("not a machine's name: %q", name)
	}
	return VM(name), nil
}

// UnmarshalText reads a VM's name from a file (JSON's strings, map keys
// included), as ParseListed does, so a name read from one is one path
// element that isn't a flag; empty is no VM's, as a field left out is.
// It may be any machine's: IsProject says whether it's a project VM's.
func (v *VM) UnmarshalText(b []byte) error {
	if len(b) == 0 {
		*v = ""
		return nil
	}
	p, err := ParseListed(string(b))
	if err != nil {
		return err
	}
	*v = p
	return nil
}

// Prefix starts every project VM's name: devvm-<folder>-<hash>.
const Prefix = "devvm-"

// ImagePrefix starts the name of every image's machine: devvm-img-<key>.
const ImagePrefix = Prefix + "img-"

// Image is the name of the machine of the image whose key is key,
// devvm-img-<key>, and an error for a key that makes no image's machine:
// one that's empty, holds a / or a NUL, or makes a project VM's name.
func Image(key string) (VM, error) {
	v := VM(ImagePrefix + key)
	if _, ok := v.ImageKey(); !ok || strings.ContainsAny(key, "/\x00") {
		return "", fmt.Errorf("not an image's key: %q", key)
	}
	return v, nil
}

// ImageKey returns the key of the image whose machine v is (Image), and
// whether it's one's. A folder named img-… gives devvm-img-…-<hash>, a
// project VM's name, so starting with ImagePrefix alone doesn't make an
// image.
func (v VM) ImageKey() (string, bool) {
	key, ok := strings.CutPrefix(string(v), ImagePrefix)
	return key, ok && key != "" && !v.IsProject()
}

// HashLen is how many hex digits of its folder's path's hash a project
// VM's name ends with: 56 bits, so another folder with the same name is
// out of reach, and the length tells project VMs apart from images
// (devvm-img-<12>).
const HashLen = 14

// vm matches a project VM's name. Its folder part is as project.Slug makes
// it: a-z and 0-9, with one dash between them, never two in a row, as
// Lima refuses a name with separators in a row.
var vm = regexp.MustCompile(`^` + regexp.QuoteMeta(Prefix) + `[a-z0-9]+(-[a-z0-9]+)*-[0-9a-f]{` + strconv.Itoa(HashLen) + `}$`)

// IsVM reports whether name is a project VM's: devvm-<folder>-<hash>,
// its folder part as project.Slug makes it, so a name read from a file is
// safe in a hostname, an ssh config's Host line or a path. Images and
// build VMs aren't, a folder named img-… included (devvm-img-…-<hash>).
func IsVM(name string) bool { return vm.MatchString(name) }

// withRuns matches a project VM's name as releases up to 0.1.1-rc.3 made
// it, a dash for each character of a folder's name but a-z and 0-9.
var withRuns = regexp.MustCompile(`^` + regexp.QuoteMeta(Prefix) + `[a-z0-9]([a-z0-9-]*[a-z0-9])?-[0-9a-f]{` + strconv.Itoa(HashLen) + `}$`)

// HasRuns reports whether name is one those releases gave a new VM for a
// folder whose name has separators in a row (withRuns): dashes in a row
// in its folder part, which IsVM refuses. Lima refused to make a VM by
// such a name, so a state that records one is of a VM whose creation
// never finished.
func HasRuns(name string) bool { return !IsVM(name) && withRuns.MatchString(name) }

// IsProject reports whether v is a project VM's name (IsVM).
func (v VM) IsProject() bool { return IsVM(string(v)) }

// Hash returns the hash a project VM's name ends with, which names its
// folder in devvm's data folder (paths.Dirs.VMDir), and whether v is a
// project VM's (IsProject).
func (v VM) Hash() (string, bool) {
	if !v.IsProject() {
		return "", false
	}
	return string(v[len(v)-HashLen:]), true
}

// Short returns the shorter name ssh and editors may reach a project VM,
// v, by: devvm-<folder>, and whether v is a project VM's.
func (v VM) Short() (string, bool) {
	if !v.IsProject() {
		return "", false
	}
	slug, _ := v.Slug()
	return Prefix + slug, true
}

// IsAlias reports whether alias is one a project VM, v, may have besides
// its name: its Short name or the name itself, so an alias read from a
// file is never another folder's, a pattern or more than a word.
func IsAlias(alias string, v VM) bool {
	short, ok := v.Short()
	return ok && (alias == short || alias == string(v))
}

// asideSuffix ends the name a project VM's old machine has while a remake
// makes a new one by the VM's name (Aside).
const asideSuffix = "-old"

// Aside returns the name a project VM's old machine is given while a
// remake makes the new one by the VM's name, devvm-<hash>-old, and whether
// v is a project VM's. It isn't a project VM's name itself (IsProject), so
// nothing takes the old machine for a VM, and it's the same length for
// every VM, the shortest a project VM's name can be plus two.
func (v VM) Aside() (VM, bool) {
	h, ok := v.Hash()
	if !ok {
		return "", false
	}
	return VM(Prefix + h + asideSuffix), true
}

// AsideHash returns the hash of the project VM whose old machine v is, as
// Aside names it, and whether it's one's.
func (v VM) AsideHash() (string, bool) {
	h, ok := strings.CutPrefix(string(v), Prefix)
	if !ok {
		return "", false
	}
	h, ok = strings.CutSuffix(h, asideSuffix)
	if !ok || !IsHash(h) {
		return "", false
	}
	return h, true
}

// IsHash reports whether s is a project VM's hash, as Hash returns it:
// HashLen lowercase hex digits.
func IsHash(s string) bool { return hash.MatchString(s) }

// hash matches a project VM's hash.
var hash = regexp.MustCompile(`^[0-9a-f]{` + strconv.Itoa(HashLen) + `}$`)

// Slug returns the folder part of a name made as devvm-<folder>-<hash>,
// which is also a project VM's hostname, and whether it has one: the name
// starts with Prefix, and a dash follows a folder part that isn't empty.
// It reads build VMs' names alike (devvm-img-<key> gives img).
func (v VM) Slug() (string, bool) {
	s, ok := strings.CutPrefix(string(v), Prefix)
	if i := strings.LastIndexByte(s, '-'); ok && i > 0 {
		return s[:i], true
	}
	return "", false
}
