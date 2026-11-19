package vm

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/devvmhq/devvm/internal/agentlogin"
	"github.com/devvmhq/devvm/internal/agents"
	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/safefile"
	"github.com/devvmhq/devvm/internal/secrets"
	"github.com/devvmhq/devvm/internal/state"
)

// AgentImport is devvm agent import: it copies this machine's agents'
// settings and logins from the home folder into devvm's store once, the
// one time devvm reads them there. Never their history: only the paths
// agentDirs, agentFiles and secrets.LoginRel name.
type AgentImport struct {
	Dirs    paths.Dirs
	Secrets *secrets.Store
	msg.Reporter
	// Opencode runs this machine's opencode with args and returns its
	// output; nil runs the one on PATH.
	Opencode func(ctx context.Context, args ...string) ([]byte, error)
	// FollowLinks is --follow-links: the import copies what the links out
	// of the agents' folders lead to, after naming each, without asking
	// (followLinks).
	FollowLinks bool
	// betweenOpens, when set, runs between read's two opens of an agent's
	// folder, where a test swaps the folder as a VM could.
	betweenOpens func()
}

// hostAgent is what the home folder has of an agent: the files and
// folders of its settings, relative to its folder there (agents.Def's Folder),
// and its login file, read as the store keeps it.
type hostAgent struct {
	agent  string
	root   *os.Root // its folder in the home folder; nil when it has none
	dirs   []string
	files  []string
	login  []byte
	hasLog bool
	// out is the links among its settings that lead out of its folder,
	// which followLinks copies or leaves out; from is each file of files
	// read through one of them, by its path in files; roots are the
	// folders those are read through, closed with h.
	out   []linkOut
	from  map[string]linkSource
	roots []*safefile.Root
	// safe is its folder opened one folder at a time, never through a
	// link (openPhysical), which listed's files are read through; listed
	// is each file of files in the folder, by its path in files: the file
	// it's read as and what it was when it was listed (listedFile).
	safe   *safefile.Root
	listed map[string]listedFile
	read   int64 // the bytes read of listed's files so far, at most saveMaxBytes
	// entries counts every entry the import's walk of the folder read,
	// links, sockets and the like included, at most saveMaxEntries.
	entries safefile.Bound
}

// listedFile is a file of an agent's folder as the import listed it: rel,
// its path in the folder, a link's resolved, and fi, what it was then. It's
// read without following a link and must be that same file, so a VM whose
// project folder is in the agent's folder can't swap a link or a folder
// between the listing and the read.
type listedFile struct {
	rel string
	fi  fs.FileInfo
}

// readListed reads the file l through safe, the agent's folder, opening
// each folder on its way from its parent and the file itself without
// following a link, and fails unless it's the file that was listed, or
// when it's more than limit bytes.
func readListed(safe *safefile.Root, l listedFile, limit int64) ([]byte, fs.FileInfo, error) {
	parts := strings.Split(l.rel, "/")
	dir := safe
	for _, name := range parts[:len(parts)-1] {
		next, err := dir.OpenDir(name)
		if dir != safe {
			_ = dir.Close()
		}
		if err != nil {
			return nil, nil, err
		}
		dir = next
	}
	if dir != safe {
		defer func() { _ = dir.Close() }()
	}
	f, err := dir.Open(parts[len(parts)-1], safefile.Opts{})
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = f.Close() }()
	if !os.SameFile(l.fi, f.Info) {
		return nil, nil, errors.New(l.rel + " changed while devvm read it")
	}
	data, err := f.ReadAll(limit)
	return data, f.Info, err
}

// list adds the file at p in the agent's folder to h's files, read as l.
func (h *hostAgent) list(p string, l listedFile) {
	if h.listed == nil {
		h.listed = map[string]listedFile{}
	}
	h.files = append(h.files, p)
	h.listed[p] = l
}

func (h *hostAgent) empty() bool { return len(h.dirs) == 0 && len(h.files) == 0 && !h.hasLog }

// errFolderTooMany is readSettings' error once an agent's folder holds more
// entries than an import copies (saveMaxEntries), where its walk stops.
var errFolderTooMany = errors.New("more entries than an import copies")

// tooMuch reports whether what h lists of the agent's folder itself, the
// entries its walk met and its files' sizes as listed, is more than one
// import copies of it: devvm agent save's bounds, saveMaxEntries,
// saveMaxBytes and saveMaxFile. A VM whose project folder is in the
// agent's folder could otherwise fill the store, which every VM with the
// agent mounts. Files read through links out of the folder have bounds of
// their own (linkBudget).
func (h *hostAgent) tooMuch() bool {
	var bytes int64
	for _, l := range h.listed {
		if l.fi.Size() > saveMaxFile {
			return true
		}
		bytes += l.fi.Size()
	}
	return h.entries.Seen > saveMaxEntries || bytes > saveMaxBytes
}

// grew counts n bytes more in the copy of the file p, of size bytes, than
// were read of it (claudePlugins' paths in the store are longer), against
// the bytes left of the bound it was read within: the folder's
// (saveMaxBytes) or the links' (linkMaxBytes). It reports whether the copy
// still fits those and the bound on one file (saveMaxFile), so what the
// import writes never passes what it checked.
func (h *hostAgent) grew(p string, size, n int) bool {
	if s, ok := h.from[p]; ok {
		s.budget.read += int64(n)
		return size <= linkMaxFile && s.budget.read <= linkMaxBytes
	}
	h.read += int64(n)
	return size <= saveMaxFile && h.read <= saveMaxBytes
}

// sameFolder fails unless root and safe, the agent's folder opened as a
// root and one folder at a time (openPhysical), are one folder, path: a VM
// whose project folder holds it could swap it between the two opens.
func sameFolder(root *os.Root, safe *safefile.Root, path string) error {
	a, err := root.Stat(".")
	if err != nil {
		return err
	}
	b, err := safe.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(a, b) {
		return errors.New(path + " changed while devvm read it")
	}
	return nil
}

func (h *hostAgent) close() {
	if h.root != nil {
		_ = h.root.Close()
	}
	if h.safe != nil {
		_ = h.safe.Close()
	}
	for _, r := range h.roots {
		_ = r.Close()
	}
}

// Run imports the agents named, or every agent with none named. An agent
// with neither settings nor a login in the home folder is left as it is
// in the store, with a warning when it's named. When the store has
// anything of an agent it imports, Run asks before replacing it, unless
// force.
func (im AgentImport) Run(ctx context.Context, names []string, force bool) error {
	named := len(names) > 0
	if !named {
		names = image.Agents
	}
	var all, found []*hostAgent
	defer func() {
		for _, h := range all {
			h.close()
		}
	}()
	if err := im.readAll(ctx, names, &all); err != nil {
		return err
	}
	for _, h := range all {
		if !h.empty() {
			found = append(found, h)
		} else if named {
			im.Warn(msg.AgentImportNone(h.agent))
		}
	}
	if len(found) == 0 {
		if !named {
			im.Info(msg.AgentImportNothing())
		}
		return nil
	}
	if ok, err := im.confirm(found, force); err != nil || !ok {
		return err
	}
	g := newStoreGuard(im.Dirs.Config)
	for _, h := range found {
		if err := im.checkStore(g, h); err != nil {
			return storeErr(err)
		}
	}
	return im.save(ctx, found)
}

// readAll reads what the home folder has of each agent named into all,
// for the caller to close, then follows the links out of their folders
// (followLinks), under a status line: it walks their folders, runs
// opencode and copies what links lead to.
func (im AgentImport) readAll(ctx context.Context, names []string, all *[]*hostAgent) error {
	defer im.Status(msg.AgentImportReading())()
	folders := im.warnProjectsInside(names)
	for _, a := range names {
		h, err := im.read(ctx, a, folders)
		if err != nil {
			return err
		}
		*all = append(*all, h)
	}
	return im.followLinks(*all, folders)
}

// confirm asks before an import replaces what the store has of the
// agents found, unless force or the store has nothing of them.
func (im AgentImport) confirm(found []*hostAgent, force bool) (bool, error) {
	var have []string
	for _, h := range found {
		if im.inStore(h.agent) {
			have = append(have, h.agent)
		}
	}
	if len(have) == 0 || force {
		return true, nil
	}
	ok, err := im.Ask(msg.AgentImportReplace(strings.Join(have, " ")).AnsweredBy("--force"))
	if err != nil {
		return false, err
	}
	if !ok {
		im.Info(msg.NothingImported())
	}
	return ok, nil
}

// inStore reports whether the store has anything of the agent: a
// settings folder that isn't empty, or a login.
func (im AgentImport) inStore(agent string) bool {
	es, _ := os.ReadDir(agentStore(im.Dirs, agent))
	es = slices.DeleteFunc(es, func(e fs.DirEntry) bool { return strings.HasPrefix(e.Name(), stagePrefix) }) // an import's leftover isn't the agent's
	return len(es) > 0 || im.Secrets.HasLogin(agent)
}

// read finds what the home folder has of an agent. Its settings are read
// through its folder there as a root, so a link that leads out of the
// folder is never followed here: such a link goes in h.out, for
// followLinks, and a link to a folder inside it is left out with a
// warning. A login file that's a link is left out too, always. A path a
// VM could have chosen where it leads (projectFolders.chosen), the folder's or
// the login's, is left out with a warning, the login with its folder.
func (im AgentImport) read(ctx context.Context, agent string, folders *projectFolders) (*hostAgent, error) {
	h := &hostAgent{agent: agent}
	home := im.Dirs.Home
	folder := agents.Of(agent).Folder
	path := filepath.Join(home, folder)
	// The folder is read where its path led when devvm resolved it, one link at a time; a path
	// that a link in a VM's project folder leads out of it, as one that holds the agent's folder
	// could, is that VM's choice, so the folder is left out.
	target, hops, lerr := linkHops(path)
	m, chosen := folders.chosen(path, target, hops)
	if lerr == nil && chosen {
		im.Warn(m)
	}
	if fi, err := os.Stat(target); lerr == nil && !chosen && err == nil && fi.IsDir() {
		if h.root, err = os.OpenRoot(target); err != nil {
			return nil, err
		}
		if im.betweenOpens != nil {
			im.betweenOpens()
		}
		if h.safe, err = openPhysical(target); err != nil {
			h.close()
			return nil, err
		}
		if err := sameFolder(h.root, h.safe, path); err != nil {
			h.close()
			return nil, msg.AgentImportReadFailed(path, err.Error()).Because(err)
		}
		if err := im.readSettings(h, folder); err != nil {
			h.close()
			if errors.Is(err, errFolderTooMany) {
				return nil, msg.AgentImportTooMuch(agent, path)
			}
			return nil, err
		}
		if h.tooMuch() {
			h.close()
			return nil, msg.AgentImportTooMuch(agent, path)
		}
	}
	if opencodeKeepsDB(agent) && im.opencodeMajor(ctx) >= 2 {
		if err := im.readOpencode2(ctx, h); err != nil {
			h.close()
			return nil, err
		}
		return h, nil
	}
	login := filepath.Join(home, filepath.FromSlash(secrets.LoginRel(agent)))
	if lerr == nil && chosen && paths.Within(login, path) {
		return h, nil // in the folder that the VM's link leads elsewhere, left out with it
	}
	fi, err := os.Lstat(login)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return h, nil
	case err != nil:
		h.close()
		return nil, err
	case fi.Mode()&fs.ModeSymlink != 0:
		im.Warn(msg.AgentImportLinkLeftOut(login))
		return h, nil
	case !fi.Mode().IsRegular():
		return h, nil
	}
	// Resolved once, one link at a time, and read where that led, never resolved again: a link
	// that a VM swaps in on its way since then fails the read rather than lead elsewhere.
	ltarget, lhops, err := linkHops(login)
	if err == nil {
		if m, ok := folders.chosen(login, ltarget, lhops); ok {
			im.Warn(m)
			return h, nil
		}
		h.login, err = claudeHostLogin(agent, ltarget)
	}
	if err != nil {
		h.close()
		return nil, msg.AgentLoginReadFailed(login, err.Error()).Because(err)
	}
	h.hasLog = true
	return h, nil
}

// opencode runs this machine's opencode in the home folder and returns
// its output, which is never logged, as auth export's holds logins.
func (im AgentImport) opencode(ctx context.Context, args ...string) ([]byte, error) {
	if im.Opencode != nil {
		return im.Opencode(ctx, args...)
	}
	if _, err := exec.LookPath(agents.Opencode); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, agents.Opencode, args...) //nolint:gosec // devvm's own arguments: --version or auth export
	cmd.Dir = im.Dirs.Home
	return cmd.Output()
}

// opencodeMajor is the major version of this machine's opencode, which
// opencode --version prints as 2.2.5 or opencode v2.2.5, run in the home
// folder; 0 without one, or when it prints something else.
func (im AgentImport) opencodeMajor(ctx context.Context) int {
	out, err := im.opencode(ctx, "--version")
	f := strings.Fields(string(out))
	if err != nil || len(f) == 0 {
		return 0
	}
	major, _, _ := strings.Cut(strings.TrimPrefix(f[len(f)-1], "v"), ".")
	n, err := strconv.Atoi(major)
	if err != nil {
		return 0 // something else, as above
	}
	return n
}

// readOpencode2 reads opencode 2's logins, which it keeps in its own
// database, through opencode auth export, whose output holds them and so
// is read only from its stdout. It maps them to the store's format and
// warns of each login the store can't keep.
func (im AgentImport) readOpencode2(ctx context.Context, h *hostAgent) error {
	out, err := im.opencode(ctx, "auth", "export")
	if err != nil {
		return msg.AgentImportOpencodeFailed(err.Error()).Because(err)
	}
	login, skipped, err := agentlogin.OpencodeFromExport(out)
	if err != nil {
		return msg.AgentImportOpencodeFailed(err.Error()).Because(err)
	}
	for _, s := range skipped {
		im.Warn(msg.AgentImportOpencodeSkipped(msg.Printable(s.Label), msg.Printable(s.Provider)))
	}
	h.login, h.hasLog = login, login != nil
	return nil
}

// readSettings lists the agent's settings files and folders in h.root,
// the home folder's folder, relative to it, but what of a seed's folder
// is this machine's own (agents.MachineOwn), such as Claude Code's
// plugins' data.
func (im AgentImport) readSettings(h *hostAgent, folder string) error {
	home := filepath.Join(im.Dirs.Home, folder)
	var inner []string
	for _, rel := range slices.Concat(agentDirs(im.Dirs.Home, h.agent), agentFiles(im.Dirs.Home, h.agent), agentLinks(im.Dirs.Home, h.agent)) {
		if in, ok := strings.CutPrefix(rel, folder+"/"); ok {
			inner = append(inner, in)
		}
	}
	// Every entry counts, whatever it is, as the walk reads each, so it
	// stops at the bound, never reading a folder whole first.
	h.entries = safefile.Bound{Max: saveMaxEntries}
	for _, in := range inner {
		fi, err := h.root.Lstat(filepath.FromSlash(in))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue // the agent has no such file or folder
		case err != nil:
			return err
		case fi.Mode()&fs.ModeSymlink != 0:
			if h.entries.Seen++; h.entries.Seen > h.entries.Max {
				return errFolderTooMany
			}
			im.listEntry(h, home, in, fs.FileInfoToDirEntry(fi))
			continue
		}
		// Through the folder opened one folder at a time, whose FS opens a folder only as what
		// it listed, never a link or a FIFO a VM swapped in for it (safefile.ErrSwapped).
		err = safefile.Walk(h.safe.FS(), in, &h.entries, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if agents.MachineOwn(folder + "/" + p) {
				return skipEntry(d)
			}
			im.listEntry(h, home, p, d)
			return nil
		})
		if errors.Is(err, safefile.ErrTooMany) {
			return errFolderTooMany
		} else if err != nil {
			return err
		}
	}
	im.matchKinds(h, home)
	return nil
}

// skipEntry is a walk's answer for an entry it leaves out, d: none of
// what's in it, when it's a folder.
func skipEntry(d fs.DirEntry) error {
	if d.IsDir() {
		return fs.SkipDir
	}
	return nil
}

// matchKinds leaves out, with a warning, each link out of the folder
// (h.out) that is one of the agent's settings files but leads to a
// folder, or one of its settings folders but leads to a file: a VM gets
// the store's folders mounted and its files copied, each as what it is.
// A link below a settings folder, or in a WholeFolder agent's folder, may
// lead to either.
func (im AgentImport) matchKinds(h *hostAgent, home string) {
	d := agents.Of(h.agent)
	h.out = slices.DeleteFunc(h.out, func(l linkOut) bool {
		rel := d.Folder + "/" + l.p
		isFile := slices.ContainsFunc(d.Files, func(f agents.File) bool { return f.Rel == rel })
		if isFile && l.dir || slices.Contains(d.Dirs, rel) && !l.dir {
			im.Warn(msg.AgentImportLinkLeftOut(filepath.Join(home, filepath.FromSlash(l.p))))
			return true
		}
		return false
	})
}

// listEntry adds an entry of the agent's folder to h: a folder, a file,
// or a link that leads to a file in the folder, read as that file when
// that file is one of the agent's settings (linkToSetting), and left out
// with a warning naming where it leads when it isn't. A link out of the
// folder to a file or a folder goes in h.out (outLink); any other link is
// left out with a warning; sockets and the like are left out. The root
// refuses a link out of the folder, which Stat's error says.
func (im AgentImport) listEntry(h *hostAgent, home, p string, d fs.DirEntry) {
	switch {
	case d.IsDir():
		h.dirs = append(h.dirs, p)
	case d.Type().IsRegular():
		fi, err := d.Info()
		if err != nil {
			im.Warn(msg.AgentImportReadFailed(filepath.Join(home, filepath.FromSlash(p)), err.Error()))
			return
		}
		h.list(p, listedFile{rel: p, fi: fi})
	case d.Type()&fs.ModeSymlink != 0:
		if fi, err := h.root.Stat(filepath.FromSlash(p)); err == nil && fi.Mode().IsRegular() {
			link := filepath.Join(home, filepath.FromSlash(p))
			target, ok := linkToSetting(h.agent, home, p)
			if target == "" {
				im.Warn(msg.AgentImportLinkLeftOut(link)) // it changed since the root resolved it
				return
			}
			rel, err := filepath.Rel(paths.Physical(home), target)
			tfi, lerr := os.Lstat(target)
			if !ok || err != nil || lerr != nil || !tfi.Mode().IsRegular() {
				im.Warn(msg.AgentImportLinkNotSetting(link, target))
				return
			}
			h.list(p, listedFile{rel: filepath.ToSlash(rel), fi: tfi})
			return
		}
		if l, ok := outLink(home, p); ok {
			h.out = append(h.out, l)
			return
		}
		im.Warn(msg.AgentImportLinkLeftOut(filepath.Join(home, filepath.FromSlash(p))))
	}
}

// linkToSetting returns where the link at p in the agent's folder home,
// which leads to a file inside the folder, leads, with every link on the
// way resolved, and reports whether that file is one of the agent's
// settings (listedPath): what the import copies of the folder itself, so
// a link never copies what it leaves out, such as the agent's history,
// its login or what of a seed's folder is this machine's own
// (agents.MachineOwn). The file is named as it's written, so a name that
// differs only in case is no setting.
func linkToSetting(agent, home, p string) (string, bool) {
	target, _, err := linkHops(filepath.Join(home, filepath.FromSlash(p)))
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(paths.Physical(home), target)
	if err != nil || !filepath.IsLocal(rel) {
		return target, false
	}
	_, _, _, ok := listedPath([]string{agent}, agents.Of(agent).Folder+"/"+filepath.ToSlash(rel), false)
	return target, ok
}

// save puts what the agents found have into the store, all or nothing,
// holding the store's lock (lockStore). Their settings are read and
// written whole into a staging folder of each agent's store first
// (stage), so a file that fails to read, or that passes the bounds, fails
// the import with the store as it was. Then they replace the store's in
// place (apply): each file renamed into place, then whatever the store
// has that the home folder doesn't moved aside, so the folders VMs mount
// stay the same folders. A file where the store has a folder, or a folder
// where it has a file, replaces it, as when a link that led to a folder
// now leads to a file. Their logins replace the store's last, all or none
// (secrets.SaveLogins). A failure after the first change, a rename
// or a login, puts back every agent's store as it was (applied.undo). A
// link on the way to an agent's folder in the store fails it before
// anything changes (storeGuard). It runs under a status line, as it
// copies and runs git on each marketplace's config.
func (im AgentImport) save(ctx context.Context, found []*hostAgent) error {
	defer im.Status(msg.AgentStoreSaving())()
	unlock, err := lockStore(ctx, im.Dirs, im.Reporter)
	if err != nil {
		return err
	}
	defer unlock()
	var all []*importStage
	defer func() {
		for _, s := range all {
			s.discard()
		}
	}()
	for _, h := range found {
		s, err := im.stage(h)
		if err != nil {
			return storeErr(err)
		}
		if s != nil {
			all = append(all, s)
		}
	}
	if err := im.applyAll(ctx, all, found); err != nil {
		return err
	}
	for _, s := range all {
		im.Info(msg.AgentSettingsImported(s.h.agent, agentStore(im.Dirs, s.h.agent)))
		folder := agents.Of(s.h.agent).Folder
		warnMarketplaceSecrets(ctx, im, agentStore(im.Dirs, s.h.agent), marketplaceFiles(folder, s.h.files))
	}
	for _, h := range found {
		if h.hasLog {
			im.Info(msg.AgentLoginImported(h.agent, secrets.LoginFile(im.Dirs, h.agent)))
		}
	}
	return nil
}

// applyAll puts every stage in place (apply), then saves the logins
// found, all or none, and on a failure undoes every stage's changes,
// failing with agent_import_undo_failed, which keeps the staging folder
// of what it couldn't put back, when that fails too. It holds every
// stage's lock throughout, so an interrupted run's cleanup, which may
// run meanwhile, waits until the store is whole again; interrupted
// before it starts, it puts nothing in place.
func (im AgentImport) applyAll(ctx context.Context, all []*importStage, found []*hostAgent) error {
	for _, s := range all {
		s.mu.Lock()
		defer s.mu.Unlock()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, s := range all {
		if s.gone {
			return context.Canceled // its cleanup took the staging folder out
		}
	}
	var logins []secrets.Login
	for _, h := range found {
		if h.hasLog {
			logins = append(logins, secrets.Login{Kind: h.agent, Data: h.login})
		}
	}
	var err error
	done := 0
	for _, s := range all {
		if err = im.apply(s); err != nil {
			break
		}
		done++
	}
	if err == nil {
		if err = secrets.SaveLogins(ctx, im.Secrets, logins); err == nil {
			return nil
		}
	}
	err = storeErr(err)
	// Every stage that changed anything is put back, the one that failed
	// included, whichever of them fails to be; each that fails keeps its
	// staging folder, which holds what wasn't put back.
	var undos []error
	var stores, kept []string
	for i := min(done, len(all)-1); i >= 0; i-- {
		s := all[i]
		if undo := s.undo(); undo != nil {
			s.keep = true
			undos = append(undos, undo)
			stores = append(stores, agentStore(im.Dirs, s.h.agent))
			kept = append(kept, s.tmpPath(im.Dirs))
		}
	}
	if len(undos) > 0 {
		undo := errors.Join(undos...)
		return msg.AgentImportUndoFailed(strings.Join(kept, ", "), strings.Join(stores, ", "), undo.Error(), err.Error()).Because(errors.Join(err, undo))
	}
	return err
}

// importStage is an agent's settings written into a staging folder of
// its store (AgentImport.stage), not yet in place: root is the agent's
// store, and tmp the staging folder in it, a name no setting has
// (stagePrefix).
type importStage struct {
	h    *hostAgent
	root *os.Root
	tmp  string
	// remove takes out the cleanup that takes the staging folder out of
	// an interrupted run (stage).
	remove func()
	// mu is held by the apply (applyAll) and by that cleanup, which sets
	// gone once it took the staging folder out, so neither runs while the
	// other does.
	mu   *sync.Mutex
	gone bool
	// done is what apply changed in the store, each change's undo, and
	// aside counts what it moved aside into the staging folder's replaced
	// folder (asideDir). keep keeps the staging folder at discard: it
	// holds what an undo that failed couldn't put back.
	done  []func() error
	aside int
	keep  bool
}

// asideDir is the folder of an import's staging folder that apply moves
// what it replaces or takes out of the store into, until the import is
// done: no setting's folder has its name.
const asideDir = "replaced"

// tmpPath is the staging folder's path.
func (s *importStage) tmpPath(d paths.Dirs) string {
	return filepath.Join(agentStore(d, s.h.agent), s.tmp)
}

// stagePrefix starts the name of an import's staging folder in an agent's
// store, beside the agent's folder there, so no VM mounts it. One that a
// killed import left is taken out by the next import of that agent
// (sweepStages), and counts as nothing of the agent's (inStore).
const stagePrefix = ".devvm-import-"

// sweepStages takes out the staging folders that killed imports left in
// store, the agent's store, reached from devvm's config folder config as
// openStoreDir reaches it.
func sweepStages(config, store string) error {
	root, err := openStoreDir(config, store)
	if err != nil || root == nil {
		return err
	}
	defer func() { _ = root.Close() }()
	es, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return err
	}
	for _, e := range es {
		if strings.HasPrefix(e.Name(), stagePrefix) {
			if err := root.RemoveAll(e.Name()); err != nil {
				return err
			}
		}
	}
	return nil
}

// discard takes the staging folder out, with what's left in it, unless
// keep, and closes the store; a second call does nothing.
func (s *importStage) discard() {
	if s.root == nil {
		return
	}
	if !s.keep {
		_ = s.root.RemoveAll(s.tmp) // best effort: the next import's sweepStages takes out what's left
	}
	_ = s.root.Close()
	s.root = nil
	s.remove() // its cleanup would only do what this RemoveAll did
}

// stage reads h's settings and writes them whole into a new staging
// folder of the agent's store, through the store opened from devvm's
// config folder as a root (makeStoreDir), after checking every path it
// will write in place (storeGuard). Nothing of the store's own changes, so
// a failure leaves the store as it was, the staging folder taken out, and
// so does an interrupt, through a cleanup that stays until discard. First
// it takes out what killed imports left (sweepStages), even for an agent
// with no settings, for which it returns nil.
func (im AgentImport) stage(h *hostAgent) (_ *importStage, err error) {
	config, store := im.Dirs.Config, agentStore(im.Dirs, h.agent)
	if err := sweepStages(config, store); err != nil {
		return nil, err
	}
	if len(h.dirs) == 0 && len(h.files) == 0 {
		return nil, nil
	}
	folder := agents.Of(h.agent).Folder
	if err := im.checkStore(newStoreGuard(config), h); err != nil {
		return nil, err
	}
	root, err := makeStoreDir(config, store)
	if err != nil {
		return nil, err
	}
	s := &importStage{h: h, root: root, tmp: stagePrefix + rand.Text(), mu: &sync.Mutex{}}
	// An interrupted run's cleanups may run while stage or apply still does,
	// so this one reaches the staging folder through a root of its own, and
	// waits for an apply that started (applyAll), which leaves the store
	// whole, to end.
	tmp, mu := s.tmp, s.mu
	s.remove = im.AddCleanup(func(context.Context) {
		if !im.Interrupted() {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if s.keep {
			return // it holds what an undo couldn't put back
		}
		s.gone = true
		if r, err := openStoreDir(config, store); err == nil && r != nil {
			_ = r.RemoveAll(tmp) // best effort, as discard's
			_ = r.Close()
		}
	})
	defer func() {
		if err != nil {
			s.discard()
		}
	}()
	for _, p := range h.dirs {
		if err := root.MkdirAll(filepath.Join(s.tmp, folder, filepath.FromSlash(p)), 0o700); err != nil {
			return nil, err
		}
	}
	for _, p := range h.files {
		if err := im.stageFile(s, folder, p); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// apply puts what s staged in place in the agent's store, as save says,
// through the one root that stage opened, keeping each change's undo
// (s.done): what it replaces or takes out is moved aside into the
// staging folder (asideDir), a file it replaces kept there as a hard link,
// so the file is replaced in one rename, and the folders it makes are
// noted. Nothing it replaces goes until the import is done (discard).
func (im AgentImport) apply(s *importStage) error {
	folder := agents.Of(s.h.agent).Folder
	keep := map[string]bool{}
	if err := s.root.Mkdir(filepath.Join(s.tmp, asideDir), 0o700); err != nil {
		return err
	}
	for _, p := range s.h.dirs {
		rel := path.Join(folder, p)
		if err := s.clearKind(rel, true); err != nil {
			return err
		}
		if err := s.mkdirAll(rel); err != nil {
			return err
		}
		markKept(keep, rel)
	}
	for _, p := range s.h.files {
		rel := path.Join(folder, p)
		if err := s.clearKind(rel, false); err != nil {
			return err
		}
		if err := s.mkdirAll(path.Dir(rel)); err != nil {
			return err
		}
		if err := s.replace(path.Join(s.tmp, rel), rel); err != nil {
			return err
		}
		markKept(keep, rel)
	}
	keep[s.tmp] = true
	return pruneIn(s.root, keep, s.tmp, s.moveAside)
}

// undo puts back what apply changed, the last change first, going on
// past a change it can't undo, and returns what failed; none when apply
// changed nothing.
func (s *importStage) undo() error {
	var errs []error
	for i := len(s.done) - 1; i >= 0; i-- {
		errs = append(errs, s.done[i]())
	}
	s.done = nil
	return errors.Join(errs...)
}

// moveAside moves what's at rel in the store, relative to it, into the
// staging folder's asideDir, for undo to move back.
func (s *importStage) moveAside(rel string) error {
	s.aside++
	to := path.Join(s.tmp, asideDir, strconv.Itoa(s.aside))
	if err := s.root.Rename(filepath.FromSlash(rel), filepath.FromSlash(to)); err != nil {
		return err
	}
	s.done = append(s.done, func() error { return s.root.Rename(filepath.FromSlash(to), filepath.FromSlash(rel)) })
	return nil
}

// clearKind moves aside what's at rel in the store, relative to it, when
// it isn't a folder (dir) or a regular file (!dir), so a folder can be
// made or a file put there. A link, which only a hand-made store could
// have, goes too, never followed.
func (s *importStage) clearKind(rel string, dir bool) error {
	fi, err := s.root.Lstat(filepath.FromSlash(rel))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return err
	case dir && fi.IsDir(), !dir && fi.Mode().IsRegular():
		return nil
	}
	return s.moveAside(rel)
}

// mkdirAll makes the folder rel in the store, relative to it, with the
// folders above it that aren't there (0700), each one's undo taking it
// out again.
func (s *importStage) mkdirAll(rel string) error {
	var missing []string
	for p := rel; p != "." && p != "/"; p = path.Dir(p) {
		_, err := s.root.Lstat(filepath.FromSlash(p))
		if err == nil {
			break
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		missing = append(missing, p)
	}
	for _, p := range slices.Backward(missing) {
		if err := s.root.Mkdir(filepath.FromSlash(p), 0o700); err != nil {
			return err
		}
		s.done = append(s.done, func() error { return s.root.Remove(filepath.FromSlash(p)) })
	}
	return nil
}

// replace renames from, a staged file, to rel in the store, both relative
// to it, where clearKind left nothing or a regular file: that file is
// kept as a hard link in the staging folder's asideDir first, so the
// rename replaces it at once and undo can put it back.
func (s *importStage) replace(from, rel string) error {
	src, dst := filepath.FromSlash(from), filepath.FromSlash(rel)
	_, err := s.root.Lstat(dst)
	if errors.Is(err, fs.ErrNotExist) {
		if err := s.root.Rename(src, dst); err != nil {
			return err
		}
		s.done = append(s.done, func() error { return s.root.Remove(dst) })
		return nil
	} else if err != nil {
		return err
	}
	s.aside++
	old := filepath.Join(s.tmp, asideDir, strconv.Itoa(s.aside))
	if err := s.root.Link(dst, old); err != nil {
		return err
	}
	if err := s.root.Rename(src, dst); err != nil {
		return err
	}
	s.done = append(s.done, func() error { return s.root.Rename(old, dst) })
	return nil
}

// checkStore checks every path in the store that h's settings would be
// written at (storeGuard), so a link fails the import before it writes
// them. Run checks every agent's first, before it writes any.
func (im AgentImport) checkStore(g *storeGuard, h *hostAgent) error {
	store, folder := agentStore(im.Dirs, h.agent), agents.Of(h.agent).Folder
	if len(h.dirs) == 0 && len(h.files) == 0 {
		return nil
	}
	if err := g.folder(filepath.Join(store, filepath.FromSlash(folder))); err != nil {
		return err
	}
	for _, p := range h.dirs {
		if err := g.folder(filepath.Join(store, folder, filepath.FromSlash(p))); err != nil {
			return err
		}
	}
	for _, p := range h.files {
		if err := g.file(filepath.Join(store, folder, filepath.FromSlash(p))); err != nil {
			return err
		}
	}
	return nil
}

// readFile reads the settings file p in the agent's folder, which the
// import listed: through a link's root (h.from), or as listed in the
// folder itself (h.listed, readListed).
func (im AgentImport) readFile(h *hostAgent, folder, p string) ([]byte, fs.FileInfo, error) {
	if s, ok := h.from[p]; ok {
		return s.read()
	}
	l, ok := h.listed[p]
	if !ok {
		return nil, nil, errors.New(p + " wasn't listed")
	}
	// What the folder's files grew to since they were listed counts too.
	data, fi, err := readListed(h.safe, l, min(saveMaxFile, saveMaxBytes-h.read))
	if err != nil {
		return nil, nil, msg.AgentImportReadFailed(filepath.Join(im.Dirs.Home, folder, filepath.FromSlash(p)), err.Error()).Because(err)
	}
	h.read += int64(len(data))
	return data, fi, nil
}

// stageFile copies one settings file, p in the agent's folder, into s's
// staging folder, keeping its owner's execute bit (a skill's script) but
// no other user's write. Claude Code's plugin records name their folders
// by path, so the copy names them in the store, where VMs mount them, and
// a copy that grows past the bounds the file was read within fails the
// import (hostAgent.grew). A file read through a link out of the folder
// (h.from) is read through its root, as linkSource.read says.
func (im AgentImport) stageFile(s *importStage, folder, p string) error {
	h := s.h
	data, fi, err := im.readFile(h, folder, p)
	if err != nil {
		return err
	}
	home := filepath.Join(im.Dirs.Home, folder)
	out := claudePlugins(h.agent, p, data, home, filepath.Join(agentStore(im.Dirs, h.agent), folder))
	if !h.grew(p, len(out), len(out)-len(data)) {
		over := home
		if s, ok := h.from[p]; ok {
			over = s.link // the links' bound, not the folder's
		}
		return msg.AgentImportTooMuch(h.agent, over)
	}
	mode := fi.Mode().Perm()&0o755 | 0o600
	return state.WriteFileIn(s.root, filepath.Join(s.tmp, folder, filepath.FromSlash(p)), out, mode)
}

// renamePrefix replaces the folder from, as JSON writes it in a string,
// with to, where it starts a path.
func renamePrefix(data []byte, from, to string) []byte {
	enc := func(s string) string {
		b, _ := json.Marshal(s)
		return strings.Trim(string(b), `"`)
	}
	f, t := enc(from), enc(to)
	for _, end := range []string{`/`, `"`} {
		data = []byte(strings.ReplaceAll(string(data), `"`+f+end, `"`+t+end))
	}
	return data
}

// markKept marks p, a path relative to the store, and the folders above
// it as kept.
func markKept(keep map[string]bool, p string) {
	for ; p != "." && p != "/" && !keep[p]; p = path.Dir(p) {
		keep[p] = true
	}
}

// pruneStore takes out what's in store, a folder of the store, that
// isn't kept, through it as a root reached from devvm's config folder
// (openStoreDir), so it never follows a link to the folder or out of it.
func pruneStore(config, store string, keep map[string]bool) error {
	root, err := openStoreDir(config, store)
	if err != nil || root == nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return pruneIn(root, keep, "", func(p string) error { return root.RemoveAll(filepath.FromSlash(p)) })
}

// pruneIn takes what's in root, a folder of the store opened as
// pruneStore opens one, that isn't kept, out with take, each by its path
// relative to root, but nothing of the folder skip.
func pruneIn(root *os.Root, keep map[string]bool, skip string, take func(string) error) error {
	return fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case p == skip:
			return fs.SkipDir
		case p == "." || keep[p]:
			return nil
		}
		if err := take(p); err != nil {
			return err
		}
		if d.IsDir() {
			return fs.SkipDir
		}
		return nil
	})
}
