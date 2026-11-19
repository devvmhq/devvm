package state

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/vmname"
)

// VMNames returns the names of the VMs that have state, sorted: the name
// each VM's folder (paths.Dirs.VMDir) records. Anything else in the VMs'
// folder, and a folder without state, isn't a VM.
func VMNames(d paths.Dirs) ([]vmname.VM, error) {
	all, err := ReadAll(d)
	if err != nil {
		return nil, err
	}
	var names []vmname.VM
	for _, r := range all {
		if r.VM == nil {
			return nil, r.Err
		}
		names = append(names, r.VM.Name())
	}
	slices.Sort(names)
	return names, nil
}

// ReadableVMs are the states of the VMs whose state reads (ReadAll), by
// name, as VMNames has them, so one that can't be read takes no other away:
// for what only warns, never for what must count every VM, such as a
// port's allocation, which VMNames fails for.
func ReadableVMs(d paths.Dirs) []*VM {
	all, _ := ReadAll(d)
	var out []*VM
	for _, r := range all {
		if r.Err == nil && r.VM != nil {
			out = append(out, r.VM)
		}
	}
	slices.SortFunc(out, func(a, b *VM) int { return cmp.Compare(a.Name(), b.Name()) })
	return out
}

// Read is one VM folder's state as ReadAll read it: the state, or why it
// can't be used.
type Read struct {
	Hash string // the folder's name (paths.Dirs.VMDir)
	// VM is nil for a file that can't be read, and set, with Err, for a
	// state whose name isn't its folder's.
	VM  *VM
	Err error
}

// ReadAll reads the state of every VM folder that has a state file, by
// its hash, each on its own: a file that can't be read is its folder's
// Err, and a state whose name isn't its folder's (a hand-edited or copied
// vm.json) a CorruptError, so one never stops the rest being read. A
// folder without a state file, or anything else in the VMs' folder, has
// no VM. Only the VMs' folder failing to be read fails it.
func ReadAll(d paths.Dirs) ([]Read, error) {
	es, err := os.ReadDir(d.VMs())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var out []Read
	for _, e := range es {
		if !e.IsDir() || !vmname.IsHash(e.Name()) {
			continue
		}
		r := Read{Hash: e.Name()}
		v, err := ReadHash(d, r.Hash)
		switch {
		case err != nil:
			r.Err = err
		case !v.Exists():
			continue
		case !inFolder(v.Name(), r.Hash):
			r.VM, r.Err = v, &CorruptError{Path: v.file.State(), Reason: "its name " + v.Name().String() + " isn't its folder's"}
		default:
			r.VM = v
		}
		out = append(out, r)
	}
	return out, nil
}

// inFolder reports whether the VM named name keeps its state in the
// folder named hash.
func inFolder(name vmname.VM, hash string) bool {
	h, ok := name.Hash()
	return ok && h == hash
}

// vmVersion is the version of vm.json's format that this devvm writes.
const vmVersion = 1

// newID is a VM's ID: 128 random bits, in hex.
func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// VM is one VM's state, vm.json in its folder (paths.VM.State): the core
// fields every VM has, here, and one section for each package that keeps
// something of the VM's (net, git, ports…), which that package decodes
// into its own type (Section) and changes (SetSection) inside Update.
// It lives as long as the VM: creating a VM replaces it, and removing the
// VM removes it. Reads take no lock: every write replaces the file whole.
type VM struct {
	// ID is a random ID Create gives every VM, so a link can tell a VM
	// made again in the same folder from the one it linked.
	ID string
	// Alias is the name ssh and editors reach it by besides its own,
	// chosen when it's made (sshconfig.ChooseAlias), and never again.
	Alias string
	Image string // the image the VM was made from: base, or its layers joined with +
	Chain string // its image's build chain
	From  string // the image its base started from, if not the default
	Dir   string // the project folder
	// Backend is the backend running it; BackendAt where that backend
	// kept it when it was made (backend.LocationOf), "" on a backend
	// whose VMs can't move.
	Backend, BackendAt string
	// Created is when its backend finished making it: zero for a VM whose
	// creation didn't finish, which never starts.
	Created time.Time
	// MadeBy is the version of the devvm that made the VM. Later
	// changes keep it.
	MadeBy string

	// name is the VM's name, chosen when it's made for the backend it's
	// made on (project.New), and never again: a folder's VM is found by
	// its hash (ReadHash), whatever name that backend gave it.
	name     vmname.VM
	sections map[string]json.RawMessage
	// folder is the project folder's identity (platform.FolderID), to
	// notice it was replaced, and folderVolume the volume it was on, bound
	// to it (platform.VolumeOf), "" for none: SetFolder writes both.
	// folderUnchecked is whether folder is a renumbered folder's ID that
	// was recorded without its volume read (SetFolderUnchecked).
	folder, folderVolume string
	folderUnchecked      bool
	// file is its folder's files; Dir "" for a name whose folder is
	// another VM's (ReadVM), which is never written.
	file   paths.VM
	claims string // the claims lock (paths.Dirs.ClaimsLock)
	exists bool
}

// core is vm.json's core fields, as the file has them.
type core struct {
	Version   int       `json:"version"`
	ID        string    `json:"id"`
	Name      string    `json:"name"` // the VM's, which decode parses
	Alias     string    `json:"alias,omitempty"`
	Image     string    `json:"image"`
	Chain     string    `json:"chain,omitempty"`
	From      string    `json:"from,omitempty"`
	Dir       string    `json:"dir"`
	Folder    string    `json:"folder,omitempty"`
	Volume    string    `json:"folder_volume,omitempty"`
	Unchecked bool      `json:"folder_unchecked,omitempty"`
	Backend   string    `json:"backend"`
	BackendAt string    `json:"backend_at,omitempty"`
	Created   time.Time `json:"created,omitzero"`
	MadeBy    string    `json:"made_by"`
}

// coreFields are the names of core's fields, which no section takes.
func coreFields() []string {
	return []string{"version", "id", "name", "alias", "image", "chain", "from", "dir", "folder", "folder_volume", "folder_unchecked", "backend", "backend_at", "created", "made_by"}
}

// New is a new VM's state, not written yet: Create writes it, once its
// fields and sections are set.
func New(d paths.Dirs, name vmname.VM) (*VM, error) {
	if !name.IsProject() {
		return nil, fmt.Errorf("invalid VM name %q", name)
	}
	return &VM{name: name, file: d.VM(name), claims: d.ClaimsLock()}, nil
}

// ReadHash reads the state in the VM folder named by hash (a project
// folder's, project.Hash). A folder with no state file has empty state:
// every field is empty, Name too, and Exists reports false; so does one
// whose state an older release left for a VM Lima never made (decode).
func ReadHash(d paths.Dirs, hash string) (*VM, error) {
	if !vmname.IsHash(hash) {
		return nil, fmt.Errorf("invalid VM hash %q", hash)
	}
	v := &VM{file: paths.VM{Dir: d.VMDir(hash)}, claims: d.ClaimsLock()}
	if err := v.load(); err != nil {
		return nil, err
	}
	return v, nil
}

// ReadVM reads the state of the project VM named name, in the folder its
// name's hash names. A VM with no state file has empty state: every field
// is empty, and Exists reports false; so does one whose folder records
// another name, whose state is never written through this one.
func ReadVM(d paths.Dirs, name vmname.VM) (*VM, error) {
	h, ok := name.Hash()
	if !ok {
		return nil, fmt.Errorf("invalid VM name %q", name)
	}
	v, err := ReadHash(d, h)
	if err != nil {
		return nil, err
	}
	if v.exists && v.name != name {
		return &VM{name: name}, nil
	}
	v.name = name
	return v, nil
}

// load reads the VM's state file again, into v.
func (v *VM) load() error {
	data, err := os.ReadFile(v.file.State())
	switch {
	case errors.Is(err, fs.ErrNotExist):
		*v = VM{name: v.name, file: v.file, claims: v.claims}
		return nil
	case err != nil:
		return err
	}
	return v.decode(data)
}

// decode reads a state file's contents into v: a file that isn't a JSON
// object, or lacks a core field, is a CorruptError.
func (v *VM) decode(data []byte) error {
	path := v.file.State()
	var all map[string]json.RawMessage
	var c core
	if err := json.Unmarshal(data, &all); err != nil || all == nil {
		return &CorruptError{Path: path, Reason: "not a JSON object"}
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return &CorruptError{Path: path, Reason: err.Error()}
	}
	name, nameErr := vmname.Parse(c.Name)
	if nameErr != nil && vmname.HasRuns(c.Name) && c.Created.IsZero() {
		// An older release's VM for a folder whose name has separators
		// in a row, which Lima refused to make: no VM, so the folder's
		// next VM, by the name it gets now, replaces this state.
		*v = VM{name: v.name, file: v.file, claims: v.claims}
		return nil
	}
	for f, missing := range map[string]bool{
		"version": c.Version < 1, "id": c.ID == "", "name": nameErr != nil,
		"image": c.Image == "", "dir": c.Dir == "", "backend": c.Backend == "",
	} {
		if missing {
			return &CorruptError{Path: path, Reason: "no " + f}
		}
	}
	for _, f := range coreFields() {
		delete(all, f)
	}
	*v = VM{
		ID: c.ID, Alias: c.Alias, Image: c.Image, Chain: c.Chain, From: c.From, Dir: c.Dir,
		Backend: c.Backend, BackendAt: c.BackendAt, Created: c.Created, MadeBy: c.MadeBy,
		folder: c.Folder, folderVolume: c.Volume, folderUnchecked: c.Unchecked, name: name, sections: all, file: v.file, claims: v.claims, exists: true,
	}
	return nil
}

// encode is v as its state file has it: the core fields, then the
// sections by name, indented.
func (v *VM) encode() ([]byte, error) {
	c, err := json.Marshal(core{
		Version: vmVersion, ID: v.ID, Name: v.name.String(), Alias: v.Alias, Image: v.Image, Chain: v.Chain, From: v.From,
		Dir: v.Dir, Folder: v.folder, Volume: v.folderVolume, Unchecked: v.folderUnchecked, Backend: v.Backend, BackendAt: v.BackendAt, Created: v.Created, MadeBy: v.MadeBy,
	})
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	b.Write(c[:len(c)-1]) // without its }
	names := make([]string, 0, len(v.sections))
	for n := range v.sections {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		k, _ := json.Marshal(n)
		b.WriteByte(',')
		b.Write(k)
		b.WriteByte(':')
		b.Write(v.sections[n])
	}
	b.WriteByte('}')
	var out bytes.Buffer
	if err := json.Indent(&out, b.Bytes(), "", "  "); err != nil {
		return nil, err
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// Name is the VM's name.
func (v *VM) Name() vmname.VM { return v.name }

// Folder is the project folder's identity (platform.FolderID), "" for
// none recorded.
func (v *VM) Folder() string { return v.folder }

// FolderVolume is the volume the project folder was on, bound to its
// identity as SetFolder recorded it (platform.VolumeOf), "" for none: a
// record whose binding isn't Folder's, as an older devvm that moved the
// folder leaves it, names no volume (platform.RecordedVolume).
func (v *VM) FolderVolume() string { return v.folderVolume }

// SetFolder records the project folder's identity, id, and its volume,
// vol (platform.VolumeOf, "" for none), together: the one way either is
// written. The volume counts as checked.
func (v *VM) SetFolder(id, vol string) {
	v.folder, v.folderVolume, v.folderUnchecked = id, vol, false
}

// SetFolderUnchecked records id, the identity a renumbered project
// folder has now, with vol, the volume recorded before, which wasn't
// read this time: the folder's volume stays to check (FolderUnchecked)
// until a launch reads it.
func (v *VM) SetFolderUnchecked(id, vol string) {
	v.folder, v.folderVolume, v.folderUnchecked = id, vol, true
}

// FolderUnchecked reports whether Folder is a renumbered folder's
// identity recorded without its volume read (SetFolderUnchecked).
func (v *VM) FolderUnchecked() bool { return v.folderUnchecked }

// Exists reports whether the VM has a state file.
func (v *VM) Exists() bool { return v.exists }

// File is the VM's state file, vm.json, for an error that names it.
func (v *VM) File() string { return v.file.State() }

// Finished reports whether the VM's creation finished (Created).
func (v *VM) Finished() bool { return v.exists && !v.Created.IsZero() }

// Section decodes the VM's section name into a T: the zero T when it has
// none. A section that isn't a T is a CorruptError naming the file.
func Section[T any](v *VM, name string) (T, error) {
	var t T
	raw, ok := v.sections[name]
	if !ok {
		return t, nil
	}
	if err := json.Unmarshal(raw, &t); err != nil {
		return t, &CorruptError{Path: v.file.State(), Reason: name + ": " + err.Error()}
	}
	return t, nil
}

// SetSection sets the VM's section name to val, in v alone: Update writes
// it. A val that encodes as {} removes the section.
func SetSection(v *VM, name string, val any) error {
	if slices.Contains(coreFields(), name) {
		return fmt.Errorf("%q is a core field, not a section", name)
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(val); err != nil {
		return err
	}
	raw := bytes.TrimSuffix(b.Bytes(), []byte("\n"))
	if string(raw) == "{}" || string(raw) == "null" {
		delete(v.sections, name)
		return nil
	}
	if v.sections == nil {
		v.sections = map[string]json.RawMessage{}
	}
	v.sections[name] = raw
	return nil
}

// Create writes a new VM's whole state (New), replacing any its folder
// had, with a new ID, under the VM's lock. claim, when it isn't nil,
// runs first under the claims lock too, which is held until the state is
// written: what it chooses that no two VMs may have (the VM's ssh alias)
// it reads from the others' states, which no other devvm changes
// meanwhile.
func (v *VM) Create(ctx context.Context, claim func(*VM) error) error {
	id, err := newID()
	if err != nil {
		return err
	}
	return v.locked(ctx, claim != nil, func() error {
		if claim != nil {
			if err := claim(v); err != nil {
				return err
			}
		}
		v.ID = id
		return v.write()
	})
}

// Update changes the VM's state under its lock (paths.VM.Lock): it reads
// the state again, so what another devvm wrote meanwhile is kept, applies
// f, and writes the state once, so a command's changes land together or
// not at all. A failed f writes nothing. v then holds the state as
// written, or as read when f or the write failed. A VM without state, or whose folder
// records another name, is refused. f takes no other lock.
func (v *VM) Update(ctx context.Context, f func(*VM) error) error {
	return v.update(ctx, false, f)
}

// Claim is Update holding the claims lock too, until the state is
// written: f chooses what no two VMs may have (a host port), reading the
// other VMs' states, which no other devvm changes meanwhile.
func (v *VM) Claim(ctx context.Context, f func(*VM) error) error {
	return v.update(ctx, true, f)
}

func (v *VM) update(ctx context.Context, claims bool, f func(*VM) error) error {
	if v.file.Dir == "" {
		return fmt.Errorf("%s: its folder is another VM's", v.name)
	}
	name := v.name
	return v.locked(ctx, claims, func() error {
		if err := v.load(); err != nil {
			return err
		}
		if !v.exists || v.name != name {
			v.name = name
			return fmt.Errorf("%s: no state: %w", name, fs.ErrNotExist)
		}
		was := *v
		was.sections = maps.Clone(v.sections)
		err := f(v)
		if err == nil {
			err = v.write()
		}
		if err != nil {
			*v = was
		}
		return err
	})
}

// locked runs f holding the VM's lock, and with claims the claims lock
// after it.
func (v *VM) locked(ctx context.Context, claims bool, f func() error) error {
	if err := PrivateDir(v.file.Dir); err != nil {
		return err
	}
	defer held.take(vmLock, v.file.Lock())()
	unlock, err := platform.LockFile(ctx, v.file.Lock(), msg.Waiting(msg.StatusOf(ctx), msg.VMStateWaiting(v.name)))
	if err != nil {
		return err
	}
	defer unlock()
	if claims {
		return claimed(ctx, v.claims, f)
	}
	return f()
}

// Claims runs f holding the claims lock (paths.Dirs.ClaimsLock), for what
// has no state of its own to claim through (VM.Claim): a build VM's host
// port. f takes no VM's lock: the claims lock comes after a VM's.
func Claims(ctx context.Context, d paths.Dirs, f func() error) error {
	return claimed(ctx, d.ClaimsLock(), f)
}

// claimed runs f holding the claims lock, the file claims.
func claimed(ctx context.Context, claims string, f func() error) error {
	if err := PrivateDir(filepath.Dir(claims)); err != nil {
		return err
	}
	defer held.take(claimsLock, claims)()
	unclaim, err := platform.LockFile(ctx, claims, msg.Waiting(msg.StatusOf(ctx), msg.ClaimsWaiting()))
	if err != nil {
		return err
	}
	defer unclaim()
	return f()
}

func (v *VM) write() error {
	data, err := v.encode()
	if err != nil {
		return err
	}
	if err := writeFile(v.file.State(), data); err != nil {
		return err
	}
	v.exists = true
	return nil
}

// RemoveFolder deletes the whole folder of the VM named name
// (paths.Dirs.VM), lock file and all, holding the VM's lock: a change
// another devvm is making to its state lands first, and none lands after,
// as one waiting for the lock takes it again on a new lock file, finding
// no state. A missing folder is no error.
func RemoveFolder(ctx context.Context, d paths.Dirs, name vmname.VM) error {
	f := d.VM(name)
	if _, err := os.Lstat(f.Dir); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	defer held.take(vmLock, f.Lock())()
	unlock, err := platform.LockFile(ctx, f.Lock(), msg.Waiting(msg.StatusOf(ctx), msg.VMStateWaiting(name)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil // another devvm deleted it meanwhile
	} else if err != nil {
		return err
	}
	defer unlock()
	return os.RemoveAll(f.Dir)
}

// Remove deletes the VM's state file under its lock, if it has one: the
// rest of its folder is its teardown's.
func (v *VM) Remove(ctx context.Context) error {
	if v.file.Dir == "" {
		return nil
	}
	if _, err := os.Stat(v.file.State()); errors.Is(err, fs.ErrNotExist) {
		*v = VM{name: v.name, file: v.file, claims: v.claims}
		return nil
	}
	return v.locked(ctx, false, func() error {
		if err := removeFile(v.file.State()); err != nil {
			return err
		}
		*v = VM{name: v.name, file: v.file, claims: v.claims}
		return nil
	})
}
