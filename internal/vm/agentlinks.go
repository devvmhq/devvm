package vm

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/devvmhq/devvm/internal/agents"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/safefile"
	"github.com/devvmhq/devvm/internal/settings"
)

// devvm agent import copies a link out of an agent's folder only when
// it's allowed: by config.toml's follow-links, which names the folders
// whose links it follows without asking, or for one run by
// --follow-links, or a yes to the question that names each link. What a
// link leads to, with every link on the way resolved, is read through
// that folder as a root (safefile.Root): a follow-links folder, or with
// the flag the link's own target (its folder, for a file), so a link
// inside it can't lead out of it. It never reads the places linkPlaces
// names, and it warns when what it reads is in a VM's project folder,
// which that VM can write. The store gets copies, never links.

// What one import copies through links out of the agents' folders at
// most, all of them together: devvm agent save's own bounds. A link whose
// files would pass them is left out whole.
const (
	linkMaxBytes   = saveMaxBytes
	linkMaxFile    = saveMaxFile
	linkMaxEntries = saveMaxEntries
)

// linkOut is a link among an agent's settings that leads out of its
// folder: p, its path in the folder, path, where it is on this machine,
// and target, what it leads to with every link on the way resolved, a
// folder (dir) or a file, and hops, where each link on the way is, the
// agent's folder's own included. root is the folder followLinks reads it
// through, once it's allowed.
type linkOut struct {
	p, path, target string
	dir             bool
	hops            []string
	root            string
}

// outLink returns the link at p in the agent's folder home, as a
// linkOut, when it leads out of the folder to a folder or a regular file:
// not when it leads nowhere, to something else, or inside the folder,
// where only a file is read (listEntry).
func outLink(home, p string) (linkOut, bool) {
	l := linkOut{p: p, path: filepath.Join(home, filepath.FromSlash(p))}
	target, hops, err := linkHops(l.path)
	if err != nil || isIn(target, paths.Physical(home)) {
		return l, false
	}
	fi, err := os.Stat(target)
	if err != nil || !fi.IsDir() && !fi.Mode().IsRegular() {
		return l, false
	}
	l.target, l.dir, l.hops = target, fi.IsDir(), hops
	return l, true
}

// linkHops resolves p, an absolute path, as filepath.EvalSymlinks does,
// one name at a time, and returns where it leads and where each link on
// the way is, by its physical path, so a link that a VM put in its
// project folder is seen even when it's the middle of a chain.
func linkHops(p string) (target string, hops []string, err error) {
	const sep = string(filepath.Separator)
	todo := strings.Split(filepath.Clean(p), sep)
	cur := sep
	for len(todo) > 0 {
		name := todo[0]
		todo = todo[1:]
		switch name {
		case "", ".":
			continue
		case "..":
			cur = filepath.Dir(cur)
			continue
		}
		next := filepath.Join(cur, name)
		fi, err := os.Lstat(next)
		if err != nil {
			return "", nil, err
		}
		if fi.Mode()&fs.ModeSymlink == 0 {
			cur = next
			continue
		}
		if hops = append(hops, next); len(hops) > linkMaxHops {
			return "", nil, errors.New(p + ": too many links")
		}
		dest, err := os.Readlink(next)
		if err != nil {
			return "", nil, err
		}
		if filepath.IsAbs(dest) {
			cur = sep
		}
		todo = append(strings.Split(dest, sep), todo...)
	}
	return cur, hops, nil
}

// linkMaxHops is the most links a path may lead through, as the kernel's
// own bound on a path's links.
const linkMaxHops = 40

// isIn reports whether p is dir or inside it: by name, as paths.Within,
// or by identity, p or a folder above it being the same file as dir, so
// a name that differs only in case on a file system that ignores case,
// such as ~/.SSH on macOS, is still dir.
func isIn(p, dir string) bool {
	if paths.Within(p, dir) {
		return true
	}
	d, err := os.Stat(dir)
	if err != nil {
		return false
	}
	for {
		if fi, err := os.Stat(p); err == nil && os.SameFile(fi, d) {
			return true
		}
		parent := filepath.Dir(p)
		if parent == p {
			return false
		}
		p = parent
	}
}

// linkSource is where a file read through a link comes from: name in
// root, read following a link at its end only within root (follow), and
// the import's bounds (budget).
type linkSource struct {
	root   *safefile.Root
	name   string
	follow bool
	budget *linkBudget
}

// linkBudget is what the import has copied through links so far:
// entries listed and bytes, as listed (listed) and as read (read).
type linkBudget struct {
	entries      int
	listed, read int64
}

// read reads s's file, the bytes it has left of linkMaxBytes, and at most
// linkMaxFile: a file that grew past either since it was listed fails
// the import.
func (s linkSource) read() ([]byte, fs.FileInfo, error) {
	f, err := s.root.Open(s.name, safefile.Opts{Follow: s.follow})
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := f.ReadAll(min(linkMaxFile, linkMaxBytes-s.budget.read))
	if err != nil {
		return nil, nil, err
	}
	s.budget.read += int64(len(data))
	return data, f.Info, nil
}

// linkPlaces are the folders and files devvm agent import never copies
// through a link, each by its physical path: devvm's own folders (its
// config, data and Lima home), the places that hold secrets, and every
// agent's logins and history. A link that leads to one, into one or to a
// folder holding one is left out, as is one to the home folder or a
// folder holding it (refusedPlace), and a follow-links folder that is,
// holds or is in one fails the import (linkRoots).
func linkPlaces(d paths.Dirs) []string {
	ps := []string{d.Config, d.Data, d.Lima}
	for _, rel := range slices.Concat([]string{
		".ssh", ".gnupg", ".aws", "Library/Keychains", ".config/devvm", ".netrc", ".config/gh", ".docker", ".kube",
	}, agents.History(), agents.LoginFiles()) {
		ps = append(ps, filepath.Join(d.Home, filepath.FromSlash(rel)))
	}
	var out []string
	for _, p := range ps {
		if p != "" {
			out = append(out, paths.Physical(p))
		}
	}
	return out
}

// refusedPlace returns the place p, a physical path, is, holds or is in
// (linkPlaces), or the home folder when p is it or holds it; "" for none.
// Each is compared by identity too (isIn), so another spelling of a
// place, such as ~/.SSH where case doesn't count, is still that place.
func refusedPlace(d paths.Dirs, p string) string {
	if home := paths.Physical(d.Home); isIn(home, p) {
		return home
	}
	for _, place := range linkPlaces(d) {
		if isIn(p, place) || isIn(place, p) {
			return place
		}
	}
	return ""
}

// linkRoots returns config.toml's follow-links, each folder by its
// physical path. One in a refused place (refusedPlace), or holding one,
// fails the import, naming it: a link inside it can lead anywhere in it.
func (im AgentImport) linkRoots() ([]string, error) {
	dirs, err := settings.FollowLinks(im.Dirs)
	if err != nil {
		return nil, err
	}
	var roots []string
	for _, dir := range dirs {
		p := paths.Physical(dir)
		if place := refusedPlace(im.Dirs, p); place != "" {
			return nil, msg.AgentImportRootRefused(dir, place)
		}
		roots = append(roots, p)
	}
	return roots, nil
}

// followLinks decides, for each link out of an agent's folder (h.out),
// whether the import copies what it leads to. One that leads to a refused
// place is left out with a warning. One inside a follow-links folder is
// followed through it; with FollowLinks, any other is followed through
// its own target; without, the import names them and asks, and a no, or
// no way to ask, leaves each out with a warning naming --follow-links.
// Each link followed is named, and its files listed in its agent's
// (follower.copy).
func (im AgentImport) followLinks(all []*hostAgent) error {
	var out []*linkOut
	owner := map[*linkOut]*hostAgent{}
	for _, h := range all {
		for i := range h.out {
			out = append(out, &h.out[i])
			owner[&h.out[i]] = h
		}
	}
	if len(out) == 0 {
		return nil
	}
	roots, err := im.linkRoots()
	if err != nil {
		return err
	}
	f := &follower{im: im, budget: &linkBudget{}, warned: map[string]bool{}}
	var follow, ask []*linkOut
	for _, l := range out {
		if place := refusedPlace(im.Dirs, l.target); place != "" {
			im.Warn(msg.AgentImportLinkRefused(l.path, l.target))
			continue
		}
		if vm := f.through(l); vm != "" {
			im.Warn(msg.AgentImportLinkViaVM(l.path, vm))
			continue
		}
		if i := slices.IndexFunc(roots, func(r string) bool { return paths.Within(l.target, r) }); i >= 0 {
			l.root = roots[i]
			follow = append(follow, l)
		} else {
			ask = append(ask, l)
		}
	}
	yes, err := im.askLinks(f, ask)
	if err != nil {
		return err
	}
	for _, l := range ask {
		if !yes {
			im.Warn(msg.AgentImportLinkNotFollowed(l.path, l.target))
			continue
		}
		l.root = l.target
		if !l.dir {
			l.root = filepath.Dir(l.target)
		}
		follow = append(follow, l)
	}
	for _, l := range follow {
		if err := f.copy(owner[l], l); err != nil {
			return err
		}
	}
	return nil
}

// askLinks reports whether the import follows these links: yes with
// FollowLinks, else the answer to a question that names each, and the VMs
// whose project folders they lead into; no when it can't ask.
func (im AgentImport) askLinks(f *follower, ask []*linkOut) (bool, error) {
	if len(ask) == 0 || im.FollowLinks {
		return im.FollowLinks, nil
	}
	var about []msg.M
	for _, l := range ask {
		about = append(about, msg.AgentImportLink(l.path, l.target))
		about = append(about, f.projects(l.path, l.target, l.dir)...)
	}
	yes, err := im.Ask(msg.AgentImportFollow().AnsweredBy("--follow-links").About(about...))
	if _, refused := errors.AsType[*msg.Refusal](err); refused {
		return false, nil
	}
	return yes, err
}

// follower copies what the import follows links to, within its bounds
// (budget), and warns once for each link and VM whose project folder it
// reads from (warned).
type follower struct {
	im     AgentImport
	budget *linkBudget
	vms    []projectVM
	read   bool // vms read
	warned map[string]bool
}

// projectVM is a VM's name and its project folder's physical path.
type projectVM struct{ name, dir string }

// projectVMs are the VMs devvm has, each with its project folder, read
// once.
func (f *follower) projectVMs() []projectVM {
	if !f.read {
		f.read = true
		if inv, err := ReadStates(f.im.Dirs); err == nil {
			for _, r := range inv.Recorded() {
				if r.Dir != "" {
					f.vms = append(f.vms, projectVM{name: r.Name, dir: paths.Physical(r.Dir)})
				}
			}
		}
	}
	return f.vms
}

// through returns the VM, if any, whose project folder a link on l's way
// is in (l.hops) while l's target isn't: that VM chose where l leads, so
// l could lead to any file of this machine's, and it's left out, whatever
// allows it.
func (f *follower) through(l *linkOut) string {
	for _, v := range f.projectVMs() {
		if isIn(l.target, v.dir) {
			continue
		}
		if slices.ContainsFunc(l.hops, func(h string) bool { return isIn(h, v.dir) }) {
			return v.name
		}
	}
	return ""
}

// projects returns a warning for each VM whose project folder target, a
// folder (dir) or a file that the link at path leads to, is in or holds,
// once for each link and VM: that VM can change what every VM's agent
// reads from there. A --clone VM's folder is its clone's source.
func (f *follower) projects(path, target string, dir bool) []msg.M {
	var ms []msg.M
	for _, v := range f.projectVMs() {
		key := path + "\x00" + v.name
		in := isIn(target, v.dir) || dir && isIn(v.dir, target)
		if f.warned[key] || !in {
			continue
		}
		f.warned[key] = true
		ms = append(ms, msg.AgentImportLinkProject(path, v.name))
	}
	return ms
}

// copy lists what l leads to in h, read through l.root: the file, or the
// folder's folders and files, a link in it read as the file it leads to
// in l.root, and any other link in it left out with a warning. A link
// whose files would pass the import's bounds is left out whole, with a
// warning.
func (f *follower) copy(h *hostAgent, l *linkOut) error {
	root, err := openPhysical(l.root)
	if err != nil {
		return err
	}
	h.roots = append(h.roots, root)
	rel, err := filepath.Rel(l.root, l.target)
	if err != nil {
		return err
	}
	ls := listing{f: f, root: root, l: l, base: filepath.ToSlash(rel)}
	if err := fs.WalkDir(root.FS(), ls.base, ls.visit); err != nil {
		if errors.Is(err, errTooMany) {
			f.im.Warn(msg.AgentImportLinkTooBig(l.path))
			return nil
		}
		return err
	}
	f.im.Info(msg.AgentImportLinkFollowed(l.path, l.target))
	for _, m := range slices.Concat(f.projects(l.path, l.target, l.dir), ls.warn) {
		f.im.Warn(m)
	}
	if h.from == nil {
		h.from = map[string]linkSource{}
	}
	h.dirs = append(h.dirs, ls.dirs...)
	for _, p := range ls.files {
		h.files = append(h.files, p.in)
		h.from[p.in] = linkSource{root: root, name: filepath.FromSlash(p.name), follow: p.follow, budget: f.budget}
	}
	f.budget.entries += ls.entries
	f.budget.listed += ls.bytes
	return nil
}

// errTooMany is a listing's error once a link's files pass the import's
// bounds.
var errTooMany = errors.New("past the bounds of what an import copies through links")

// listing is one link's walk: what it leads to, in root, from base, and
// what it found, each by its path in the agent's folder (in), with the
// warnings it gives once the link is followed (warn).
type listing struct {
	f       *follower
	root    *safefile.Root
	l       *linkOut
	base    string
	dirs    []string
	files   []linkFile
	warn    []msg.M
	entries int
	bytes   int64
}

// linkFile is a file a listing found: in, its path in the agent's
// folder, and name, in the root, read following a link at its end
// (follow).
type linkFile struct {
	in, name string
	follow   bool
}

// visit lists one entry of what the link leads to: a folder, a file, or
// a link to a file in the root, which is read as that file. The root
// refuses a link out of it, which Stat's error says, and such a link, or
// one to a folder, is left out with a warning.
func (ls *listing) visit(name string, d fs.DirEntry, err error) error {
	if err != nil {
		return err
	}
	var in string
	switch {
	case name == ls.base:
		in = ls.l.p
	case ls.base == ".":
		in = path.Join(ls.l.p, name)
	default:
		in = path.Join(ls.l.p, strings.TrimPrefix(name, ls.base+"/"))
	}
	ls.entries++
	if ls.f.budget.entries+ls.entries > linkMaxEntries {
		return errTooMany
	}
	host := filepath.Join(ls.l.root, filepath.FromSlash(name))
	switch {
	case d.IsDir():
		ls.dirs = append(ls.dirs, in)
		return nil
	case d.Type().IsRegular():
		return ls.file(in, name, d.Info, false)
	case d.Type()&fs.ModeSymlink != 0:
		if fi, err := fs.Stat(ls.root.FS(), name); err == nil && fi.Mode().IsRegular() {
			if target, err := filepath.EvalSymlinks(host); err == nil {
				ls.warn = append(ls.warn, ls.f.projects(ls.l.path, target, false)...)
			}
			return ls.file(in, name, func() (fs.FileInfo, error) { return fi, nil }, true)
		}
		ls.warn = append(ls.warn, msg.AgentImportLinkLeftOut(host))
	}
	return nil
}

// openPhysical opens dir, a physical path, as a Root, one folder at a
// time from /, each opened from its parent and never through a link
// (safefile.Root.OpenDir), so a link swapped into its path, as a VM could
// in its project folder, fails rather than leads elsewhere.
func openPhysical(dir string) (*safefile.Root, error) {
	root, err := safefile.OpenRoot(string(filepath.Separator))
	if err != nil {
		return nil, err
	}
	for _, name := range strings.Split(strings.Trim(dir, string(filepath.Separator)), string(filepath.Separator)) {
		if name == "" {
			continue
		}
		next, err := root.OpenDir(name)
		_ = root.Close()
		if err != nil {
			return nil, err
		}
		root = next
	}
	return root, nil
}

// file lists a regular file the listing found, within the bounds.
func (ls *listing) file(in, name string, info func() (fs.FileInfo, error), follow bool) error {
	fi, err := info()
	if err != nil {
		return err
	}
	ls.bytes += fi.Size()
	if fi.Size() > linkMaxFile || ls.f.budget.listed+ls.bytes > linkMaxBytes {
		return errTooMany
	}
	ls.files = append(ls.files, linkFile{in: in, name: name, follow: follow})
	return nil
}
