package vm

import (
	"context"
	"errors"
	"path/filepath"
	"slices"

	"github.com/devvmhq/devvm/internal/devcontainer"
	"github.com/devvmhq/devvm/internal/gitprotect"
	"github.com/devvmhq/devvm/internal/netpolicy"
	"github.com/devvmhq/devvm/internal/ports"
	"github.com/devvmhq/devvm/internal/secrets"
	"github.com/devvmhq/devvm/internal/sshagent"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/tools"
)

// The sections of a VM's state (state.VM) this package keeps: its size,
// its project folder mount's cache mode, a --clone VM's clone, its
// devcontainer.json, its desktop, its links, a remake that hasn't
// finished and a move its setup hasn't followed yet.
// The others are their packages':
// netpolicy's, gitprotect's, ports', sshagent's, secrets' and tools'.
const (
	sizeSection         = "size"
	mountSection        = "mount"
	cloneSection        = "clone"
	devcontainerSection = "devcontainer"
	desktopSection      = "desktop"
	linksSection        = "links"
	remakeSection       = "remake"
	movedSection        = "moved"
)

// movedInVM is a VM's moved section: the path its project folder had
// before it moved (recordMove), under which the agents in the VM keep
// what they keep by the folder's path. The VM's next setup moves that to
// the folder's path now (claudeMoved), then clears the section.
type movedInVM struct {
	From string `json:"from,omitempty"`
}

// movedTo is the moved section of a VM whose folder moved from was to
// dir, its last record from: the path the agents' state is under, kept
// across moves until a setup follows them, and none for a folder back
// where that state is.
func movedTo(last movedInVM, was, dir string) movedInVM {
	from := last.From
	if from == "" {
		from = was
	}
	if from == dir {
		return movedInVM{}
	}
	return movedInVM{From: from}
}

// cloneInVM is a VM's clone section: whether it was made with --clone,
// and what devvm made of its clone. A clone of one repo, the folder's own
// (as devvm 0.1.0 made every clone), has its remote and seed in Made; a
// workspace clone, of a folder of repos, has its repos in Repos, each
// with its own, and Made.Seeded once all of them, and the folder's own
// files (Made.Files), are in the VM. repos reads both alike.
type cloneInVM struct {
	On   bool      `json:"on"`
	Made cloneMade `json:"made,omitzero"`
	// Workspace: the VM has a clone of each of Repos, at its path, and a
	// copy of the folder's files outside them.
	Workspace bool        `json:"workspace,omitempty"`
	Repos     []cloneRepo `json:"repos,omitempty"`
}

// cloneMade is what devvm made of a --clone VM's clone.
type cloneMade struct {
	Remote string `json:"remote,omitempty"` // the repo's remote for the VM, recorded before it's added
	Seeded bool   `json:"seeded,omitempty"` // the host's branches are in the VM
	// Files: a workspace clone's copy of the folder's files outside its
	// repos is in the VM (workspaceFiles), made once.
	Files bool `json:"files,omitempty"`
	// Submodules: the folder's own repo's submodules the VM has a clone
	// of, as a workspace repo's are (cloneRepo's).
	Submodules []cloneRepo `json:"submodules,omitempty"`
}

// cloneRepo is one repo of a workspace clone: its folder, relative to the
// project folder ("." for the folder's own), the repo's remote for the
// VM, recorded before it's added, and whether its branches are in the VM.
// Submodules are its initialised submodules the VM has a clone of, each
// recorded before its copy as the repo is, with its folder relative to
// the repo's, and its own, nested; a submodule recorded here stays
// checked by rm and fetched whatever the host's repo holds since.
type cloneRepo struct {
	Path       string      `json:"path"`
	Remote     string      `json:"remote,omitempty"`
	Seeded     bool        `json:"seeded,omitempty"`
	Submodules []cloneRepo `json:"submodules,omitempty"`
	// submodule: allRepos' entry is a submodule's, not written.
	submodule bool
}

// repos are the clone's repos: a workspace clone's Repos, each at a
// local path in the folder (a path that isn't one is passed over: host
// git runs in each), else the one repo of the folder's own, at ".", with
// Made's remote, seed and submodules.
func (c cloneInVM) repos() []cloneRepo {
	if c.Workspace {
		return slices.DeleteFunc(slices.Clone(c.Repos), func(r cloneRepo) bool { return r.Path != "." && !filepath.IsLocal(r.Path) })
	}
	return []cloneRepo{{Path: ".", Remote: c.Made.Remote, Seeded: c.Made.Seeded, Submodules: c.Made.Submodules}}
}

// allRepos are the clone's repos (repos) and, after each, its submodules
// (cloneRepo's), nested ones too, each with its path in the project
// folder and no submodules of its own: those fetch, rm's check and the
// remotes' removal go over. A submodule whose path isn't local to its
// repo's is passed over.
func (c cloneInVM) allRepos() []cloneRepo {
	var out []cloneRepo
	var add func(base string, rs []cloneRepo)
	add = func(base string, rs []cloneRepo) {
		for _, r := range rs {
			path := r.Path
			if base != "" {
				if !filepath.IsLocal(r.Path) {
					continue
				}
				path = filepath.Join(base, r.Path)
			}
			out = append(out, cloneRepo{Path: path, Remote: r.Remote, Seeded: r.Seeded, submodule: base != ""})
			add(path, r.Submodules)
		}
	}
	add("", c.repos())
	return out
}

// setRepo changes the clone's repo at path, in the project folder, as
// change says: a workspace clone's entry in Repos, else Made's remote,
// seed and submodules for "."; or a submodule's entry, found below its
// repo's. It reports whether it had one; a path it hasn't is left alone.
func (c *cloneInVM) setRepo(path string, change func(*cloneRepo)) bool {
	if !c.Workspace {
		r := cloneRepo{Path: ".", Remote: c.Made.Remote, Seeded: c.Made.Seeded, Submodules: c.Made.Submodules}
		found := path == "."
		if found {
			change(&r)
		} else {
			found = setSubmodule(&r, path, change)
		}
		c.Made.Remote, c.Made.Seeded, c.Made.Submodules = r.Remote, r.Seeded, r.Submodules
		return found
	}
	found := false
	for i := range c.Repos {
		if c.Repos[i].Path == path {
			change(&c.Repos[i])
			found = true
		} else if setSubmodule(&c.Repos[i], path, change) {
			found = true
		}
	}
	return found
}

// setSubmodule changes the submodule of r, the repo at r.Path in the
// project folder, at path in it, as change says, nested ones too, and
// reports whether it had one.
func setSubmodule(r *cloneRepo, path string, change func(*cloneRepo)) bool {
	rel, err := filepath.Rel(r.Path, path)
	if err != nil || !filepath.IsLocal(rel) {
		return false
	}
	for i := range r.Submodules {
		s := &r.Submodules[i]
		if s.Path == rel {
			change(s)
			return true
		}
		// Below it: a nested one, its path in the project folder.
		nested := cloneRepo{Path: filepath.Join(r.Path, s.Path), Submodules: s.Submodules}
		if setSubmodule(&nested, path, change) {
			s.Submodules = nested.Submodules
			return true
		}
	}
	return false
}

// addSubmodule records the submodule at sub, relative to its repo's
// folder, of the clone's repo at parent, in the project folder, once,
// with its remote: before its copy, so a retry, rm and an interrupt
// find it. A parent it hasn't is an error, never a record lost.
func (c *cloneInVM) addSubmodule(parent, sub, remote string) error {
	if !c.setRepo(parent, func(r *cloneRepo) {
		for i := range r.Submodules {
			if r.Submodules[i].Path == sub {
				r.Submodules[i].Remote = remote
				return
			}
		}
		r.Submodules = append(r.Submodules, cloneRepo{Path: sub, Remote: remote})
	}) {
		return errNoParent
	}
	return nil
}

// dropSubmodule takes the record of the submodule at sub, relative to its
// repo's folder, of the clone's repo at parent out: one whose copy
// didn't go in, which the VM has nothing of to check.
func (c *cloneInVM) dropSubmodule(parent, sub string) {
	c.setRepo(parent, func(r *cloneRepo) {
		r.Submodules = slices.DeleteFunc(r.Submodules, func(s cloneRepo) bool { return s.Path == sub })
	})
}

// errNoParent is addSubmodule's error for a parent repo the clone hasn't.
var errNoParent = errors.New("the clone records no such repo")

// devcontainerInVM is a VM's devcontainer section: the devcontainer.json
// it was made from, what that maps to of the VM's own (its variables, its
// commands), what of the VM came from the file and what from the user,
// which devvm update reads the file again by (devcontainers.reread), and
// which of its commands ran.
type devcontainerInVM struct {
	Off  bool   `json:"off,omitempty"`  // made with --no-devcontainer
	File string `json:"file,omitempty"` // the file, in the project folder; "" for none
	Sum  string `json:"sum,omitempty"`  // its SHA-256 then
	// Image is the file's image, as it names it, when the VM's base
	// starts from it (pinned: the VM's From); "" when the file has none,
	// or --from won.
	Image string `json:"image,omitempty"`
	// FromFlag: the VM was made with --from, which wins over the file's
	// image.
	FromFlag bool `json:"from_flag,omitempty"`
	// Parts are the toolchains and layers the file's features mapped to;
	// Gave those of them the VM got from the file alone, not from the
	// user's flags, devvm setup's layers or the folder's other files.
	Parts []string `json:"parts,omitempty"`
	Gave  []string `json:"gave,omitempty"`
	// Ports are the file's forwardPorts when it was last read; PortsGave
	// those of them it added to the VM's ports (filePorts), not the
	// user's own.
	Ports     []string               `json:"ports,omitempty"`
	PortsGave []string               `json:"ports_gave,omitempty"`
	Env       []devcontainer.Var     `json:"env,omitempty"`
	Create    []devcontainer.Command `json:"create,omitempty"` // run once, at its first launch
	Start     []devcontainer.Command `json:"start,omitempty"`  // run once each start
	Made      devcontainerMade       `json:"made,omitzero"`
}

// devcontainerMade is which of a VM's devcontainer.json commands ran.
type devcontainerMade struct {
	Created bool   `json:"created,omitempty"` // Create ran
	Started string `json:"started,omitempty"` // the start Start last ran at, hashed
}

// desktopInVM is a VM's desktop section: what devvm made for devvm
// desktop (its port is the ports section's).
type desktopInVM struct {
	Made struct {
		Password string `json:"password,omitempty"` // the VNC password, made the first time
	} `json:"made,omitzero"`
}

// linksInVM is a VM's links section: the ports it publishes, its links
// as a client, and its group.
type linksInVM struct {
	// Publish is the ports and ranges the VM offers to links from the
	// user's other VMs (ParsePortList, never all); "" for none.
	Publish string `json:"publish,omitempty"`
	Links   []Link `json:"links,omitempty"`
	// Group is the group the VM is in, "" for none; GroupAs the name the
	// group's other VMs reach it by, "" for its folder part. Each
	// member's own state records its group, so a VM made again is in
	// none.
	Group   string `json:"group,omitempty"`
	GroupAs string `json:"group_as,omitempty"`
}

// sectionOf reads st's section name. One that can't be read reads as
// empty: the folder's VM's sections are checked as it's found
// (checkState), and other VMs' are read only to show or to skip.
func sectionOf[T any](st *state.VM, name string) T {
	t, _ := state.Section[T](st, name)
	return t
}

// edit changes st's section name as f says, in st alone: inside
// st.Update.
func edit[T any](st *state.VM, name string, f func(*T) error) error {
	t, err := state.Section[T](st, name)
	if err != nil {
		return err
	}
	if err := f(&t); err != nil {
		return err
	}
	return state.SetSection(st, name, t)
}

// update changes st's section name as f says, under the VM's lock.
func update[T any](ctx context.Context, st *state.VM, name string, f func(*T) error) error {
	return st.Update(ctx, func(st *state.VM) error { return edit(st, name, f) })
}

// checkState refuses a VM's state with a section that isn't what its
// package reads: the folder's VM's, as it's found, so a section that
// can't be read never reads as empty (sectionOf, netpolicy.On) for it.
func checkState(st *state.VM) error {
	for _, err := range []error{
		check(st, netpolicy.Of), check(st, gitprotect.Of), check(st, ports.Of), check(st, sshagent.Of),
		check(st, secrets.Of), check(st, tools.Of),
		section[Size](st, sizeSection), check(st, readMount), section[cloneInVM](st, cloneSection),
		section[devcontainerInVM](st, devcontainerSection), section[desktopInVM](st, desktopSection),
		section[remakeInVM](st, remakeSection), section[movedInVM](st, movedSection), check(st, ReadLinks),
	} {
		if err != nil {
			return err
		}
	}
	return nil
}

// check is the error of reading st's section with of.
func check[T any](st *state.VM, of func(*state.VM) (T, error)) error {
	_, err := of(st)
	return err
}

// section is the error of reading st's section name as a T.
func section[T any](st *state.VM, name string) error {
	_, err := state.Section[T](st, name)
	return err
}
