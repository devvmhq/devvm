package vm

import (
	"context"
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
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/safefile"
	"github.com/devvmhq/devvm/internal/settings"
	"github.com/devvmhq/devvm/internal/vmname"
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
	// linkMaxWalked is the most entries the walks of every link read,
	// those left out for the bounds included: once they've read it, the
	// rest are left out unread, so links that each pass the bounds can't
	// make an import read without end.
	linkMaxWalked = 4 * linkMaxEntries
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
	if err != nil || paths.In(target, paths.Physical(home)) {
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

// linkSource is where a file read through a link comes from: name in
// root, read following a link at its end only within root (follow), and
// the import's bounds (budget).
type linkSource struct {
	root   *safefile.Root
	name   string
	follow bool
	budget *linkBudget
	link   string // the link out of the agent's folder it's read through, by its path
}

// linkBudget is what the import has copied through links so far:
// entries listed and bytes, as listed (listed) and as read (read); and
// the entries every link's walk read (walked), at most about
// linkMaxWalked.
type linkBudget struct {
	entries, walked int
	listed, read    int64
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
// Each is compared by identity too (paths.In), so another spelling of a
// place, such as ~/.SSH where case doesn't count, is still that place.
func refusedPlace(d paths.Dirs, p string) string {
	if home := paths.Physical(d.Home); paths.In(home, p) {
		return home
	}
	for _, place := range linkPlaces(d) {
		if paths.In(p, place) || paths.In(place, p) {
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
// (follower.copy). One a VM could have chosen (projectFolders.chosen) is left
// out with a warning, whatever allows it. folders are the VMs' folders
// the import read at its start.
func (im AgentImport) followLinks(ctx context.Context, all []*hostAgent, folders *projectFolders) error {
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
	f := &follower{im: im, budget: &linkBudget{}, folders: folders, warned: map[string]bool{}}
	var follow, ask []*linkOut
	for _, l := range out {
		if place := refusedPlace(im.Dirs, l.target); place != "" {
			im.Warn(msg.AgentImportLinkRefused(l.path, l.target))
			continue
		}
		if m, ok := folders.chosen(l.path, l.target, l.hops); ok {
			im.Warn(m)
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
		if err := f.copy(ctx, owner[l], l); err != nil {
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
// reads from (warned). folders are the VMs' folders the import read at its
// start (warnProjectsInside).
type follower struct {
	im      AgentImport
	budget  *linkBudget
	folders *projectFolders
	warned  map[string]bool
}

// projectVM is a VM's name and its project folder's physical path.
type projectVM struct {
	name vmname.VM
	dir  string
}

// projectFolders is what devvm agent import knows of the VMs' project folders,
// as it read them at its start: each VM's (vms), and the states it
// couldn't read (unread), each by its path, whose VMs' folders it can't
// check.
type projectFolders struct {
	vms    []projectVM
	unread []string
}

// readProjectFolders reads the VMs devvm has, each with its project folder,
// and the states it couldn't read, each a warning
// (agent_import_states_unread): the VMs' folder, when it can't be read,
// or a VM's vm.json.
func readProjectFolders(d paths.Dirs) (*projectFolders, []msg.M) {
	f := &projectFolders{}
	inv, err := ReadStates(d)
	if err != nil {
		f.unread = []string{d.VMs()}
		return f, []msg.M{msg.AgentImportStatesUnread(d.VMs(), err.Error())}
	}
	var unread []msg.M
	for _, k := range inv.VMs {
		if k.Err != nil {
			f.unread = append(f.unread, k.Path)
			unread = append(unread, msg.AgentImportStatesUnread(k.Path, k.Err.Error()))
		}
	}
	for _, r := range inv.Recorded() {
		if r.Dir != "" {
			f.vms = append(f.vms, projectVM{name: r.Name, dir: paths.Physical(r.Dir)})
		}
	}
	return f, unread
}

// chosen returns the warning that leaves out path, which leads to target
// through the links hops (linkHops'), when a VM could have chosen where it
// leads, and whether there is one: when it leads through a link in a VM's
// project folder to a place outside it (throughVM,
// agent_import_link_via_vm), or through any link you could change while a
// VM's state can't be read, as devvm can't tell whether that VM's project
// folder holds it (agent_import_link_unread_state): the links on the way
// to the home folder count too, as a project folder may hold one, such as
// a VM made with --unsafe-folder on a folder that holds the home folder. A link
// that only root can change (rootOnly), such as macOS's /var or Fedora
// Silverblue's /home, doesn't count. A nil f knows of no VM.
func (f *projectFolders) chosen(path, target string, hops []string) (msg.M, bool) {
	if f == nil {
		return msg.M{}, false
	}
	if vm := throughVM(f.vms, target, hops); vm != "" {
		return msg.AgentImportLinkViaVM(path, vm), true
	}
	if len(f.unread) > 0 && slices.ContainsFunc(hops, func(h string) bool { return !rootOnly(h) }) {
		return msg.AgentImportLinkUnreadState(path, strings.Join(f.unread, ", ")), true
	}
	return msg.M{}, false
}

// rootOnly reports whether only root can change where the link at hop,
// a physical path (linkHops'), leads: every folder above it, up to /, is
// root's, no one else may write it by its mode bits, and the system says
// you may not write it either (platform.Writable, which an ACL counts in),
// and devvm doesn't run as root. A
// VM changes this machine's files only as you, through its mounts, so no
// VM can change such a link, whatever its project folder holds. Changing
// a link takes writing the folder it's in, or one above, to move that
// folder away.
func rootOnly(hop string) bool {
	if os.Getuid() == 0 {
		return false
	}
	for dir := filepath.Dir(hop); ; dir = filepath.Dir(dir) {
		fi, err := os.Lstat(dir)
		if err != nil || !fi.IsDir() || fi.Mode().Perm()&0o022 != 0 {
			return false
		}
		if uid, _, ok := platform.Owner(fi); !ok || uid != 0 || platform.Writable(dir) {
			return false // the mode bits aside, an ACL or a FUSE file system may let you write it
		}
		if dir == filepath.Dir(dir) {
			return true
		}
	}
}

// warnProjectsInside warns of each VM whose project folder is inside the
// folder of one of the agents named, or holds it (agent_import_project_inside),
// before the import reads anything: code in that VM can change what the
// import copies, and every VM with the agent then reads it. A state it
// can't read, whose VM's folder it can't check, is a warning of its own
// (agent_import_states_unread). It returns the VMs it checked, which read
// and followLinks take too.
func (im AgentImport) warnProjectsInside(names []string) *projectFolders {
	f, unread := readProjectFolders(im.Dirs)
	for _, m := range unread {
		im.Warn(m)
	}
	for _, a := range names {
		folder := filepath.Join(im.Dirs.Home, agents.Of(a).Folder)
		for _, v := range f.vms {
			if overlapsAgent(v.dir, folder) {
				im.Warn(msg.AgentImportProjectInside(v.dir, folder, v.name))
			}
		}
	}
	return f
}

// overlapsAgent reports whether dir, a project folder's physical path, is
// inside folder, an agent's folder in the home folder, or holds it, by
// identity too (paths.In): code in dir's VM can change that agent's
// settings on this machine, and what devvm agent import copies from
// there. folder is compared as named too, so one that a VM could make
// later counts.
func overlapsAgent(dir, folder string) bool {
	phys := paths.Physical(folder)
	return paths.In(dir, phys) || paths.In(phys, dir) || paths.In(folder, dir)
}

// warnAgentFolders warns once, when a launch has found t, if the folder
// t's VM mounts is inside an agent's folder in the home folder, or holds
// one (project_agent_folder): code in the VM can change that agent's
// settings on this machine. A --clone VM (clone: a new one) mounts
// nothing, so it gets no warning.
func (e Launch) warnAgentFolders(t *target, clone bool) {
	if clone || t.st.Exists() && isClone(t.st) {
		return
	}
	var folders []string
	for _, a := range agents.Names() {
		if folder := filepath.Join(e.Dirs.Home, agents.Of(a).Folder); overlapsAgent(t.f.Dir, folder) {
			folders = append(folders, folder)
		}
	}
	if len(folders) > 0 {
		e.Warn(msg.ProjectAgentFolder(t.f.Dir, strings.Join(folders, ", ")))
	}
}

// throughVM returns the VM of vms, if any, whose project folder a link on
// the way to target (hops, linkHops') is in while target isn't: that VM
// chose where the path leads, so it could lead to any file of this
// machine's.
func throughVM(vms []projectVM, target string, hops []string) vmname.VM {
	for _, v := range vms {
		if paths.In(target, v.dir) {
			continue
		}
		if slices.ContainsFunc(hops, func(h string) bool { return paths.In(h, v.dir) }) {
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
	if f.folders == nil {
		return nil
	}
	var ms []msg.M
	for _, v := range f.folders.vms {
		key := path + "\x00" + v.name.String()
		in := paths.In(target, v.dir) || dir && paths.In(v.dir, target)
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
// warning. Its walk ends with ctx (safefile.Walk).
func (f *follower) copy(ctx context.Context, h *hostAgent, l *linkOut) error {
	root, err := openPhysical(l.root)
	if err != nil {
		return err
	}
	h.roots = append(h.roots, root)
	rel, err := filepath.Rel(l.root, l.target)
	if err != nil {
		return err
	}
	ls := listing{f: f, root: root, l: l, base: filepath.ToSlash(rel), folder: agents.Of(h.agent).Folder}
	if f.budget.walked >= linkMaxWalked {
		f.im.Warn(msg.AgentImportLinkTooBig(l.path)) // the links left out already read as much as the walks may
		return nil
	}
	// The entries of every link followed count together, as the walk reads each.
	entries := safefile.Bound{Max: linkMaxEntries, Seen: f.budget.entries}
	if err := safefile.Walk(ctx, root.FS(), ls.base, &entries, ls.visit); err != nil {
		if errors.Is(err, errTooMany) || errors.Is(err, safefile.ErrTooMany) {
			// What it read counts against the walks' own bound (linkMaxWalked).
			f.budget.walked += entries.Seen - f.budget.entries
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
		h.from[p.in] = linkSource{root: root, name: filepath.FromSlash(p.name), follow: p.follow, budget: f.budget, link: l.path}
	}
	f.budget.walked += entries.Seen - f.budget.entries
	f.budget.entries = entries.Seen
	f.budget.listed += ls.bytes
	return nil
}

// errTooMany is a listing's error once a link's files pass the import's
// bounds.
var errTooMany = errors.New("past the bounds of what an import copies through links")

// listing is one link's walk: what it leads to, in root, from base, and
// what it found, each by its path in the agent's folder (folder, below
// the home folder) (in), with the warnings it gives once the link is
// followed (warn).
type listing struct {
	f      *follower
	root   *safefile.Root
	l      *linkOut
	base   string
	folder string
	dirs   []string
	files  []linkFile
	warn   []msg.M
	bytes  int64
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
// one to a folder, is left out with a warning. What of a seed's folder is
// this machine's own (agents.MachineOwn) is left out.
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
	if agents.MachineOwn(ls.folder + "/" + in) {
		return skipEntry(d)
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
