package vm

import (
	"context"
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

	"github.com/devvmhq/devvm/internal/agentlogin"
	"github.com/devvmhq/devvm/internal/agents"
	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
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
}

func (h *hostAgent) empty() bool { return len(h.dirs) == 0 && len(h.files) == 0 && !h.hasLog }

func (h *hostAgent) close() {
	if h.root != nil {
		_ = h.root.Close()
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
	var found []*hostAgent
	defer func() {
		for _, h := range found {
			h.close()
		}
	}()
	for _, a := range names {
		h, err := im.read(ctx, a)
		if err != nil {
			return err
		}
		if h.empty() {
			h.close()
			if named {
				im.Warn(msg.AgentImportNone(a))
			}
			continue
		}
		found = append(found, h)
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
	for _, h := range found {
		if err := im.save(ctx, h); err != nil {
			return err
		}
	}
	return nil
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
	return len(es) > 0 || im.Secrets.HasLogin(agent)
}

// read finds what the home folder has of an agent. Its settings are read
// through its folder there as a root, so a link that leads out of the
// folder is never followed: such a link, and a link to a folder, is left
// out with a warning. A login file that's a link is left out too.
func (im AgentImport) read(ctx context.Context, agent string) (*hostAgent, error) {
	h := &hostAgent{agent: agent}
	home := im.Dirs.Home
	folder := agents.Of(agent).Folder
	if fi, err := os.Stat(filepath.Join(home, folder)); err == nil && fi.IsDir() {
		if h.root, err = os.OpenRoot(filepath.Join(home, folder)); err != nil {
			return nil, err
		}
		if err := im.readSettings(h, folder); err != nil {
			h.close()
			return nil, err
		}
	}
	if agent == agents.Opencode && im.opencodeMajor(ctx) >= 2 {
		if err := im.readOpencode2(ctx, h); err != nil {
			h.close()
			return nil, err
		}
		return h, nil
	}
	login := filepath.Join(home, filepath.FromSlash(secrets.LoginRel(agent)))
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
	if agent == agents.Claude {
		h.login, err = agentlogin.ClaudeCopy(login) // never its projects' history or API key
	} else {
		h.login, err = os.ReadFile(login) //nolint:gosec // the user's own login file, not a link
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
	n, _ := strconv.Atoi(major)
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
// the home folder's folder, relative to it.
func (im AgentImport) readSettings(h *hostAgent, folder string) error {
	home := filepath.Join(im.Dirs.Home, folder)
	var inner []string
	for _, rel := range slices.Concat(agentDirs(im.Dirs.Home, h.agent), agentFiles(im.Dirs.Home, h.agent)) {
		if in, ok := strings.CutPrefix(rel, folder+"/"); ok {
			inner = append(inner, in)
		}
	}
	for _, in := range inner {
		fi, err := h.root.Lstat(filepath.FromSlash(in))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue // the agent has no such file or folder
		case err != nil:
			return err
		case fi.Mode()&fs.ModeSymlink != 0:
			im.listEntry(h, home, in, fs.FileInfoToDirEntry(fi))
			continue
		}
		err = fs.WalkDir(h.root.FS(), in, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			im.listEntry(h, home, p, d)
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// listEntry adds an entry of the agent's folder to h: a folder, a file,
// or a link that leads to a file in the folder, read as that file. Any
// other link is left out with a warning; sockets and the like are left
// out. The root refuses a link out of the folder, which Stat's error
// says.
func (im AgentImport) listEntry(h *hostAgent, home, p string, d fs.DirEntry) {
	switch {
	case d.IsDir():
		h.dirs = append(h.dirs, p)
	case d.Type().IsRegular():
		h.files = append(h.files, p)
	case d.Type()&fs.ModeSymlink != 0:
		if fi, err := h.root.Stat(filepath.FromSlash(p)); err == nil && fi.Mode().IsRegular() {
			h.files = append(h.files, p)
			return
		}
		im.Warn(msg.AgentImportLinkLeftOut(filepath.Join(home, filepath.FromSlash(p))))
	}
}

// save puts what h has into the store. Its settings replace the store's
// in place: each file written whole, then whatever the store has that the
// home folder doesn't taken out, so the folders VMs mount stay the same
// folders. Its login replaces the store's.
func (im AgentImport) save(ctx context.Context, h *hostAgent) error {
	store := agentStore(im.Dirs, h.agent)
	folder := agents.Of(h.agent).Folder
	if len(h.dirs) > 0 || len(h.files) > 0 {
		keep := map[string]bool{}
		for _, p := range h.dirs {
			dir := filepath.Join(store, folder, filepath.FromSlash(p))
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return err
			}
			markKept(keep, path.Join(folder, p))
		}
		for _, p := range h.files {
			if err := im.saveFile(h, store, folder, p); err != nil {
				return err
			}
			markKept(keep, path.Join(folder, p))
		}
		if err := pruneStore(store, keep); err != nil {
			return err
		}
		im.Info(msg.AgentSettingsImported(h.agent, store))
	}
	if h.hasLog {
		if err := im.Secrets.SaveLogin(ctx, h.agent, h.login); err != nil {
			return err
		}
		im.Info(msg.AgentLoginImported(h.agent, secrets.LoginFile(im.Dirs, h.agent)))
	}
	return nil
}

// saveFile copies one settings file, p in the agent's folder, into the
// store, keeping its owner's execute bit (a skill's script) but no other
// user's write. Claude Code's plugin records name their folders by path,
// so the copy names them in the store, where VMs mount them.
func (im AgentImport) saveFile(h *hostAgent, store, folder, p string) error {
	src := filepath.FromSlash(p)
	fi, err := h.root.Stat(src)
	if err != nil {
		return err
	}
	data, err := h.root.ReadFile(src)
	if err != nil {
		return err
	}
	if h.agent == agents.Claude && path.Dir(p) == "plugins" && path.Ext(p) == ".json" {
		data = renamePrefix(data, filepath.Join(im.Dirs.Home, folder, "plugins"), filepath.Join(store, folder, "plugins"))
	}
	mode := fi.Mode().Perm()&0o755 | 0o600
	return state.WriteFileMode(filepath.Join(store, folder, src), data, mode)
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

// pruneStore takes out what's in the store that isn't kept, through the
// store as a root, so it never follows a link out of it.
func pruneStore(store string, keep map[string]bool) error {
	root, err := os.OpenRoot(store)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case p == "." || keep[p]:
			return nil
		}
		if err := root.RemoveAll(filepath.FromSlash(p)); err != nil {
			return err
		}
		if d.IsDir() {
			return fs.SkipDir
		}
		return nil
	})
}
