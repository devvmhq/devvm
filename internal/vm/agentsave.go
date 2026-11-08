package vm

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/devvmhq/devvm/internal/agentlogin"
	"github.com/devvmhq/devvm/internal/agents"
	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/netpolicy"
	"github.com/devvmhq/devvm/internal/secrets"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/tools"
)

// AgentSave is devvm agent save: it reads the folder's VM's agents'
// settings, shows what differs from devvm's store, and after a yes puts
// them into the store, for every VM. It's devvm agent import's other
// direction, from a VM, whose files are untrusted: readSent checks the
// stream, and each settings file is parsed whole, its placeholders put
// back from the store or the file left out (agentlogin.Restore). It never
// saves a login: none is on its list.
type AgentSave struct{ *Env }

// Run saves the agents named, or every agent the VM has, starting the VM
// when it's stopped. It always asks, so without a terminal or with
// --no-input it shows what differs and is refused.
func (e AgentSave) Run(ctx context.Context, agents []string) error {
	t, err := e.finder.target(ctx, need{has: hasMade})
	if err != nil {
		return err
	}
	vm := t.f.Name
	if agents = e.agentsOf(t.st.Name(), tools.Names(t.st), agents); len(agents) == 0 {
		return nil
	}
	if err := t.ensureRunning(ctx); err != nil {
		return err
	}
	s, err := e.read(ctx, t, agents)
	if err != nil {
		return err
	}
	sv := saver{Env: e.Env, s: s}
	if sv.salt, err = netpolicy.Salt(ctx, t.st); err != nil {
		return err
	}
	var plans []agentPlan
	for _, a := range agents {
		p, err := sv.plan(a)
		if err != nil {
			return err
		}
		if p.changed() {
			plans = append(plans, p)
		}
	}
	if len(plans) == 0 {
		e.Info(msg.AgentSaveNothing(vm))
		return nil
	}
	if err := showPlans(e.Stdout(), vm, plans); err != nil {
		return err
	}
	yes, err := e.Ask(msg.AgentSaveAsk(vm).RefusedWith(msg.AgentSaveRefused()).About(msg.AgentSaveRuns()))
	if err != nil {
		return err
	}
	if !yes {
		e.Info(msg.NothingSaved())
		return nil
	}
	for _, p := range plans {
		if err := sv.apply(ctx, p); err != nil {
			return err
		}
		e.Info(msg.AgentSettingsSaved(p.agent, p.store, vm))
	}
	return nil
}

// agentsOf returns the agents of named the VM vm has among its shared
// tools (names), each once, warning about each it doesn't have; with
// none named, every agent it has.
func (e AgentSave) agentsOf(vm string, names, named []string) []string {
	have := image.AgentsOf(names)
	if len(named) == 0 {
		if len(have) == 0 {
			e.Warn(msg.AgentSaveNoAgent(vm))
		}
		return have
	}
	var out []string
	for _, a := range named {
		switch {
		case !slices.Contains(have, a):
			e.Warn(msg.AgentSaveNone(a, vm))
		case !slices.Contains(out, a):
			out = append(out, a)
		}
	}
	return out
}

// read runs agentSaveScript in t's VM, which is running, and reads its
// stream (readSent), at most saveMaxBytes of it.
func (e AgentSave) read(ctx context.Context, t *target, agents []string) (*sent, error) {
	vm := t.f.Name
	run, cancel := context.WithCancel(ctx)
	defer cancel()
	out := &bounded{max: saveMaxBytes, cancel: cancel}
	log := &bounded{to: e.LogWriter(), max: saveMaxStderr, cancel: cancel}
	err := t.exec(run, []string{"bash", "-c", agentSaveScript(agents)}, backend.ExecOpts{Stdout: out, Stderr: log})
	switch {
	case out.over, log.over:
		return nil, msg.AgentSaveTooMuch(vm)
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case err != nil:
		return nil, backend.Logged(msg.AgentSaveNotRead(vm))
	}
	return readSent(bytes.NewReader(out.Bytes()), vm, agents)
}

// saver makes and applies one save's plans, from what the VM sent (s),
// with the VM's salt, which ends its placeholders: every VM's copies of
// the store's files hold placeholders (syncer.stageSecrets).
type saver struct {
	*Env
	s    *sent
	salt string
}

// agentPlan is what a save changes of one agent's settings in its store
// (store): its settings files, its folders, and for claude its
// .claude.json's subset (claude), with what the store of its logins has
// (claudeOld, in claudeFile).
type agentPlan struct {
	agent      string
	store      string
	files      []planFile
	folders    []planFolder
	claude     []byte
	claudeOld  []byte
	claudeFile string
}

func (p agentPlan) changed() bool { return len(p.files) > 0 || len(p.folders) > 0 || p.claude != nil }

// planFile is a file the store gets, by its path relative to the home
// folder, with whether its owner may run it, and what the store has of
// it (old, nil for none, and oldExec).
type planFile struct {
	rel     string
	data    []byte
	exec    bool
	old     []byte
	oldExec bool
}

// planFolder is a settings folder the store gets whole, by its path
// relative to the home folder: its folders and files, what was left out
// (kept: the store's stays) and the files added, changed and removed,
// relative to it.
type planFolder struct {
	rel                     string
	dirs                    []string
	files                   []planFile
	kept                    []string
	added, changed, removed []string
}

// plan returns what a save changes of an agent's settings.
func (sv saver) plan(agent string) (agentPlan, error) {
	p := agentPlan{agent: agent, store: agentStore(sv.Dirs, agent)}
	names := sv.s.names(agent)
	in := map[string][]string{} // each folder's entries, sorted
	for _, n := range names {
		if e := sv.s.entries[n]; n != e.top {
			in[e.top] = append(in[e.top], n)
		}
	}
	for _, n := range names {
		e := sv.s.entries[n]
		if n != e.top {
			continue // planFolder's
		}
		switch {
		case e.kind == 'l' && e.folder && e.link == filepath.Join(p.store, filepath.FromSlash(n)):
			// The store's own folder, mounted: nothing of the VM's.
		case e.kind == 'l':
			sv.Warn(msg.AgentSaveLinkLeftOut(n, sv.s.vm))
		case e.kind == 'd' && e.folder:
			f, err := sv.folder(p.store, n, in[n])
			if err != nil {
				return p, err
			}
			if len(f.added)+len(f.changed)+len(f.removed) > 0 {
				p.folders = append(p.folders, f)
			}
		case e.kind == 'f' && !e.folder:
			if f, ok := sv.file(p.store, n, e); ok {
				p.files = append(p.files, f)
			}
		default:
			sv.Warn(msg.AgentSaveKindLeftOut(n, sv.s.vm))
		}
	}
	if agent == agents.Claude {
		sv.claudeJSON(&p)
	}
	return p, nil
}

// file is a settings file the store gets, when it differs from the
// store's: a settings file parsed whole and its placeholders put back
// from the store's (agentlogin.Restore), another without placeholders.
// Any other is left out, with a warning.
func (sv saver) file(store, rel string, e *sentEntry) (planFile, bool) {
	if withheld(rel) {
		// The VM never gets the store's file, such as a .env, so it can't
		// have changed it.
		sv.Warn(msg.AgentSaveWithheld(rel, sv.s.vm))
		return planFile{}, false
	}
	old, _ := readStoreFile(filepath.Join(store, filepath.FromSlash(rel)))
	data := e.data
	if format, ok := settingsFormat(rel); ok {
		given, _, _ := agentlogin.Redact(format, old)
		var err error
		if data, err = agentlogin.Restore(format, data, given, old); err != nil {
			sv.leftOut(rel, err)
			return planFile{}, false
		}
	} else if agentlogin.HasPlaceholder(data) {
		sv.Warn(msg.AgentSavePlaceholder(rel, sv.s.vm))
		return planFile{}, false
	}
	f := planFile{rel: rel, data: data, exec: e.exec, old: old, oldExec: storeExec(store, rel)}
	return f, old == nil || !bytes.Equal(old, data) || f.oldExec != e.exec
}

// leftOut warns that the settings file rel is left out: for a
// placeholder Restore couldn't put back, or as one devvm can't read.
func (sv saver) leftOut(rel string, err error) {
	if _, ok := errors.AsType[*agentlogin.PlaceholderError](err); ok {
		sv.Warn(msg.AgentSavePlaceholder(rel, sv.s.vm))
		return
	}
	sv.Warn(msg.AgentSaveUnparsed(rel, sv.s.vm))
}

// claudeJSON adds to p what devvm's store keeps of the VM's
// ~/.claude.json, when it differs from the store's: only its settings,
// account state and MCP servers (agentlogin.ClaudeSubset), its
// placeholders put back from the store's (agentlogin.Restore), its copy
// in the VM being the store's as the VM got it. Left out, with a
// warning, when it isn't JSON or a placeholder can't be put back.
func (sv saver) claudeJSON(p *agentPlan) {
	const rel = agents.ClaudeJSON
	if sv.s.claudeBad {
		sv.Warn(msg.AgentSaveUnparsed(rel, sv.s.vm))
	}
	if sv.s.claude == nil {
		return
	}
	data, err := agentlogin.ClaudeSubset(sv.s.claude)
	if err != nil {
		sv.Warn(msg.AgentSaveUnparsed(rel, sv.s.vm))
		return
	}
	store := sv.Proxy.Secrets
	file := secrets.LoginFile(sv.Dirs, agents.Claude)
	old, _ := readStoreFile(file)
	var given []byte
	if old != nil {
		given, _ = store.VMLogin(agents.Claude, sv.salt)
	}
	if data, err = agentlogin.Restore("json", data, given, old); err != nil {
		sv.leftOut(rel, err)
		return
	}
	if !bytes.Equal(old, data) {
		p.claude, p.claudeOld, p.claudeFile = data, old, file
	}
}

// folder is a settings folder the store gets whole, at rel, from the
// stream's entries in it (names, sorted): its folders,
// its files, and the links in it that lead to a file inside it, as
// copies of that file (sent.resolve). Any other link, any other kind of
// file and any file with a placeholder are left out with a warning, and
// the store keeps what it has of them. Claude Code's plugin records name
// their folders by path, so the copy names them in the store, where VMs
// mount them.
func (sv saver) folder(store, rel string, names []string) (planFolder, error) {
	f := planFolder{rel: rel}
	for _, n := range names {
		e := sv.s.entries[n]
		switch e.kind {
		case 'd':
			f.dirs = append(f.dirs, n)
			continue
		case 'l':
			target, ok := sv.s.resolve(n, rel)
			if !ok {
				sv.Warn(msg.AgentSaveLinkLeftOut(n, sv.s.vm))
				f.kept = append(f.kept, n)
				continue
			}
			e = target
		case 0:
			sv.Warn(msg.AgentSaveKindLeftOut(n, sv.s.vm))
			f.kept = append(f.kept, n)
			continue
		}
		if agentlogin.HasPlaceholder(e.data) {
			sv.Warn(msg.AgentSavePlaceholder(n, sv.s.vm))
			f.kept = append(f.kept, n)
			continue
		}
		data := e.data
		if rel == agents.ClaudePlugins && path.Dir(n) == rel && path.Ext(n) == ".json" && sv.s.home != "" {
			data = renamePrefix(data, sv.s.home+"/"+rel, filepath.Join(store, filepath.FromSlash(rel)))
		}
		f.files = append(f.files, planFile{rel: n, data: data, exec: e.exec})
	}
	return f, f.compare(store)
}

// compare lists what f adds, changes and removes of the store's folder,
// read through it as a root, so no link out of it is followed.
func (f *planFolder) compare(store string) error {
	have, err := storeFiles(filepath.Join(store, filepath.FromSlash(f.rel)))
	if err != nil {
		return err
	}
	for i, pf := range f.files {
		in := strings.TrimPrefix(pf.rel, f.rel+"/")
		old, ok := have[in]
		f.files[i].old, f.files[i].oldExec = old.data, old.exec
		switch {
		case !ok:
			f.added = append(f.added, in)
		case old.data == nil || !bytes.Equal(old.data, pf.data) || old.exec != pf.exec:
			f.changed = append(f.changed, in)
		}
		delete(have, in)
	}
	for _, k := range f.kept {
		delete(have, strings.TrimPrefix(k, f.rel+"/"))
	}
	for in := range have {
		f.removed = append(f.removed, in)
	}
	slices.Sort(f.removed)
	return nil
}

// storeFile is a file of a store's folder: its data (nil for what isn't
// a regular file, such as a link) and whether its owner may run it.
type storeFile struct {
	data []byte
	exec bool
}

// storeFiles returns what's in a folder of the store but folders, by its
// path relative to it; none when it isn't there.
func storeFiles(dir string) (map[string]storeFile, error) {
	have := map[string]storeFile{}
	root, err := os.OpenRoot(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return have, nil
	} else if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	err = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir():
			return nil
		case !d.Type().IsRegular():
			have[p] = storeFile{}
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		data, err := root.ReadFile(filepath.FromSlash(p))
		if err != nil {
			return err
		}
		have[p] = storeFile{data: data, exec: fi.Mode()&0o100 != 0}
		return nil
	})
	return have, err
}

// readStoreFile reads a file of devvm's store, when it's a regular file;
// nil otherwise.
func readStoreFile(p string) ([]byte, error) {
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() {
		return nil, err
	}
	return os.ReadFile(p) //nolint:gosec // devvm's own store
}

// storeExec reports whether the store's file at rel may be run by its
// owner.
func storeExec(store, rel string) bool {
	fi, err := os.Lstat(filepath.Join(store, filepath.FromSlash(rel)))
	return err == nil && fi.Mode()&0o100 != 0
}

// fileMode is the mode a saved file gets: the user's alone, and runnable
// by them when it was in the VM.
func fileMode(exec bool) os.FileMode {
	if exec {
		return 0o700
	}
	return 0o600
}

// apply puts p into the store: each settings file written whole, each
// folder's files written whole and what the folder in the store has that
// the VM's doesn't taken out, but what was left out, so the folders VMs
// mount stay the same folders; the .claude.json subset as the store's.
func (sv saver) apply(ctx context.Context, p agentPlan) error {
	for _, f := range p.files {
		if err := state.WriteFileMode(filepath.Join(p.store, filepath.FromSlash(f.rel)), f.data, fileMode(f.exec)); err != nil {
			return err
		}
	}
	for _, d := range p.folders {
		root := filepath.Join(p.store, filepath.FromSlash(d.rel))
		if err := os.MkdirAll(root, 0o700); err != nil {
			return err
		}
		keep := map[string]bool{}
		for _, dir := range d.dirs {
			if err := os.MkdirAll(filepath.Join(p.store, filepath.FromSlash(dir)), 0o700); err != nil {
				return err
			}
			markKept(keep, strings.TrimPrefix(dir, d.rel+"/"))
		}
		for _, f := range d.files {
			if err := state.WriteFileMode(filepath.Join(p.store, filepath.FromSlash(f.rel)), f.data, fileMode(f.exec)); err != nil {
				return err
			}
			markKept(keep, strings.TrimPrefix(f.rel, d.rel+"/"))
		}
		for _, k := range d.kept {
			markKept(keep, strings.TrimPrefix(k, d.rel+"/"))
		}
		if err := pruneStore(root, keep); err != nil {
			return err
		}
	}
	if p.claude != nil {
		return sv.Proxy.Secrets.SaveLogin(ctx, agents.Claude, p.claude)
	}
	return nil
}

// showPlans writes what the plans change, as data: a diff of each
// settings file and the .claude.json subset, the store's first, and for
// each folder its files added (A), changed (M) and removed (D).
func showPlans(w io.Writer, vm string, plans []agentPlan) error {
	var lines []string
	for _, p := range plans {
		for _, f := range p.files {
			lines = append(lines, fileDiff(filepath.Join(p.store, filepath.FromSlash(f.rel)), vm, f)...)
		}
		if p.claude != nil {
			lines = append(lines, fileDiff(p.claudeFile, vm, planFile{rel: agents.ClaudeJSON, data: p.claude, old: p.claudeOld})...)
		}
		for _, d := range p.folders {
			lines = append(lines, folderDiff(p.store, vm, d)...)
		}
	}
	if n := len(lines) - diffShowMax; n > 0 {
		lines = append(lines[:diffShowMax], "  ("+strconv.Itoa(n)+" more lines of this diff not shown)")
	}
	_, err := io.WriteString(w, strings.Join(lines, "\n")+"\n")
	return err
}

// diffShowMax is the most lines showPlans shows in all.
const diffShowMax = 20000

// folderDiff is a folder's part of showPlans: its files added (A),
// changed (M) and removed (D), then a diff of each one added or changed.
func folderDiff(store, vm string, d planFolder) []string {
	lines := []string{shownLine("~/" + d.rel + " in " + vm + ", into " + filepath.Join(store, filepath.FromSlash(d.rel)) + ":")}
	shown := map[string]bool{}
	for _, c := range []struct {
		mark  string
		paths []string
	}{{"A", d.added}, {"M", d.changed}, {"D", d.removed}} {
		for _, n := range c.paths {
			lines = append(lines, "  "+c.mark+" "+shownLine(n))
			shown[n] = c.mark != "D"
		}
	}
	for _, f := range d.files {
		if shown[strings.TrimPrefix(f.rel, d.rel+"/")] {
			lines = append(lines, fileDiff(filepath.Join(store, filepath.FromSlash(f.rel)), vm, f)...)
		}
	}
	return lines
}

// fileDiff is one file's diff in showPlans: the store's file, then the
// VM's, a change of whether its owner may run it, and its lines as
// shownFile shows them, or a line saying why it shows none.
func fileDiff(store, vm string, f planFile) []string {
	lines := []string{"--- " + shownLine(store), "+++ " + shownLine(vm+":~/"+f.rel)}
	if f.exec != f.oldExec {
		lines = append(lines, "  mode "+modeText(f.oldExec)+" -> "+modeText(f.exec))
	}
	before, after := shownFile(f.rel, f.old), shownFile(f.rel, f.data)
	if !utf8.ValidString(before+after) || strings.ContainsRune(before+after, 0) {
		return append(lines, "  (a binary file, which this diff doesn't show)")
	}
	d := lineDiff(before, after)
	if len(d) == 0 && f.exec == f.oldExec {
		d = []string{"  (only its layout, or values that look like secrets, which this diff doesn't show, changed)"}
	}
	return append(lines, d...)
}

// modeText is a saved file's mode (fileMode) as the diff shows it.
func modeText(exec bool) string { return "0" + strconv.FormatUint(uint64(fileMode(exec)), 8) }

// shownFile is a file of an agent's settings, at rel, as its diff shows
// it: a settings file and the .claude.json subset as a VM with network
// policy gets them, written again with the values that look like
// secrets taken out (agentlogin.Redact), so the diff never prints the
// store's secrets; nothing of one that doesn't parse; any other file as
// it is.
func shownFile(rel string, data []byte) string {
	format, ok := settingsFormat(rel)
	if rel == agents.ClaudeJSON {
		format, ok = "json", true
	}
	if !ok || data == nil {
		return string(data)
	}
	out, _, err := agentlogin.Redact(format, data)
	if err != nil {
		return ""
	}
	return string(out)
}
