package vm

import (
	"context"
	"net/url"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/devvmhq/devvm/internal/gitprotect"
	"github.com/devvmhq/devvm/internal/hostgit"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/safefile"
)

// The seed's settings: what the VM's clone gets of the repo's own config
// (.git/config, and the worktree's config.worktree with
// extensions.worktreeConfig, never an included file, never your global
// one) and its hooks. Only keys of an allowlist go, each value checked,
// never the config as a file: the remotes (their URLs and push URLs as
// git resolves them, with insteadOf and pushInsteadOf applied, and
// credentials out: cleanURL), the upstreams of the branches the seed
// sends, the submodules', sparse checkout's switches, a core.hooksPath
// inside the repo (the setting alone), and the repo's own identity.
// Nothing else goes, such as credential.*, http.*'s extra headers,
// url.*.insteadOf (applied already), core.sshCommand, core.fsmonitor,
// filter.*, include*, alias.* or the keys that describe this machine's
// filesystem (core.ignorecase, precomposeunicode, filemode, symlinks).
// The hooks go as they are, the secrets in them included: only files
// named as git's hooks (hostgit's HookEvents), regular files of at most
// hookMax each, from the common dir's hooks folder, and only when git
// here runs them, with no core.hooksPath elsewhere; a link, a folder or a
// file past the bound stays and is named.

// hookMax is the most of one hook the seed sends, and hooksMax the most
// of them all.
const (
	hookMax  = 1 << 20
	hooksMax = 16 << 20
)

// seedConfig is what the seed sends of the repo's settings: its config
// pairs (key, then value, as git config --add takes them, in order),
// each key it sets, unset first, and its hooks (hooks/NAME, each with
// its mode), and what it leaves on this machine, each named: remotes'
// URLs of a kind it doesn't send (urlsKept), remotes whose URLs git
// couldn't give (urlsUnread), remotes whose push URLs all stay
// (pushKept), hooks that are links, folders or too big (hooksKept), a
// core.hooksPath outside the repo (hooksPathKept) and a hooks folder it
// couldn't read (hooksDirKept).
type seedConfig struct {
	pairs         []string
	keys          []string
	hooks         []hookFile
	urlsKept      []string
	urlsUnread    []string
	pushKept      []string
	hooksKept     []string
	hooksPathKept string
	hooksDirKept  string
}

// hookFile is a hook the seed sends: its name, its contents and whether
// it's executable.
type hookFile struct {
	name string
	data []byte
	exec bool
}

// configRec is a key of a config file and its value, "true" for a key
// with none, which git reads as a boolean's true.
type configRec struct{ key, value string }

// readConfig reads what the seed of the repo at dir, which sends refs,
// sends of its settings (seedConfig), leaving out the remotes that reach
// a devvm VM (vmRemotes), as the refs do.
func readConfig(ctx context.Context, dir string, refs []seedRef) (seedConfig, error) {
	recs, err := configRecords(ctx, dir)
	if err != nil {
		return seedConfig{}, err
	}
	var c seedConfig
	sent := c.readRemotes(ctx, dir, recs)
	branches := map[string]bool{}
	for _, r := range refs {
		if b, ok := strings.CutPrefix(r.name, "refs/heads/"); ok {
			branches[b] = true
		}
	}
	hooksPath, local := "", false
	for _, r := range recs {
		if r.key == "core.hookspath" {
			hooksPath, local = r.value, true // the last one wins, as in git
			continue
		}
		c.setting(ctx, dir, r, sent, branches)
	}
	if err := c.readHooks(ctx, dir, hooksPath, local); err != nil {
		return seedConfig{}, err
	}
	return c, nil
}

// configRecords are the repo at dir's own config's keys, never an
// included file's, in order: its config's, then, with
// extensions.worktreeConfig, its worktree's config.worktree's, where git
// sparse-checkout puts its switches.
func configRecords(ctx context.Context, dir string) ([]configRec, error) {
	recs, err := listConfig(ctx, dir, "--local")
	if err != nil {
		return nil, err
	}
	wt := ""
	for _, r := range recs {
		if r.key == "extensions.worktreeconfig" {
			wt = r.value
		}
	}
	if !gitBool(wt) {
		return recs, nil
	}
	more, err := listConfig(ctx, dir, "--worktree")
	if err != nil {
		return nil, err
	}
	return append(recs, more...), nil
}

// listConfig is the keys of the repo at dir's config file of scope
// (--local, --worktree), never an included file's.
func listConfig(ctx context.Context, dir, scope string) ([]configRec, error) {
	out, err := hostgit.Output(ctx, "-C", dir, "config", scope, "--no-includes", "--null", "--list")
	if err != nil {
		return nil, err
	}
	var recs []configRec
	for rec := range strings.SplitSeq(string(out), "\x00") {
		if rec == "" {
			continue
		}
		key, value, ok := strings.Cut(rec, "\n")
		if !ok {
			value = "true"
		}
		recs = append(recs, configRec{key, value})
	}
	return recs, nil
}

// gitBool is whether v is a boolean's true as git reads it.
func gitBool(v string) bool {
	switch strings.ToLower(v) {
	case "true", "yes", "on":
		return true
	}
	n, err := strconv.Atoi(v)
	return err == nil && n != 0
}

// readRemotes adds the URLs of the remotes recs name (remoteURLs), but
// those that reach a devvm VM, and returns the remotes that go: those
// with a URL that goes.
func (c *seedConfig) readRemotes(ctx context.Context, dir string, recs []configRec) map[string]bool {
	vms := vmRemotes(ctx, dir)
	var remotes []string
	for _, r := range recs {
		if section, sub, name, ok := configKey(r.key); ok && section == "remote" && (name == "url" || name == "pushurl") && !slices.Contains(remotes, sub) {
			remotes = append(remotes, sub)
		}
	}
	sent := map[string]bool{}
	for _, name := range remotes {
		if !slices.Contains(vms, "refs/remotes/"+name+"/") && c.remoteURLs(ctx, dir, name) {
			sent[name] = true
		}
	}
	return sent
}

// setting adds r, a key of the repo's config, if it goes: a sent
// remote's fetch spec, promisor, partial clone filter and tag option; a
// sent branch's (branches) upstream, "." (this repo) or on a sent
// remote, its merge and rebase; a submodule's (submodule); sparse
// checkout's switches; and the repo's identity.
func (c *seedConfig) setting(ctx context.Context, dir string, r configRec, sent, branches map[string]bool) {
	section, sub, name, ok := configKey(r.key)
	if !ok {
		return
	}
	switch section {
	case "remote":
		switch {
		case !sent[sub]:
		case name == "promisor":
			// As git reads it (yes, 1, or no value at all, is true).
			c.add(r.key, strconv.FormatBool(gitBool(r.value)))
		case slices.Contains([]string{"fetch", "partialclonefilter", "tagopt"}, name):
			c.add(r.key, r.value)
		}
	case "branch":
		upstream := name == "remote" || name == "pushremote"
		if branches[sub] && (name == "merge" || name == "rebase" || upstream && (r.value == "." || sent[r.value])) {
			c.add(r.key, r.value)
		}
	case "submodule":
		c.submodule(ctx, dir, r.key, name, r.value)
	case "core":
		if name == "sparsecheckout" || name == "sparsecheckoutcone" {
			c.add(r.key, r.value)
		}
	case "index":
		if name == "sparse" {
			c.add(r.key, r.value)
		}
	case "user":
		if name == "name" || name == "email" {
			c.add(r.key, r.value)
		}
	case "extensions":
		// An older git's partial clone names its promisor here, not with
		// remote.<name>.promisor, which the VM's git reads too.
		if name == "partialclone" && sent[r.value] {
			c.add("remote."+r.value+".promisor", "true")
		}
	}
}

// configKey splits key, as git config --list writes it, into its
// section, subsection ("" for none) and name, which git writes in lower
// case.
func configKey(key string) (section, sub, name string, ok bool) {
	first, last := strings.IndexByte(key, '.'), strings.LastIndexByte(key, '.')
	if first < 0 {
		return "", "", "", false
	}
	section, name = key[:first], key[last+1:]
	if first != last {
		sub = key[first+1 : last]
	}
	return section, sub, name, name != ""
}

// add adds the pair key, value, and key to those it sets, once.
func (c *seedConfig) add(key, value string) {
	c.pairs = append(c.pairs, key, value)
	if !slices.Contains(c.keys, key) {
		c.keys = append(c.keys, key)
	}
}

// remoteURLs adds the remote name's URLs as git resolves them (remote
// get-url, insteadOf applied), each cleaned (cleanURL), and, where git
// pushes elsewhere (get-url --push, a pushurl or pushInsteadOf applied),
// its push URLs, cleaned too, but one that reaches a devvm VM. A URL of
// a kind it doesn't send is named; a remote all of whose push URLs stay
// is named and goes without them, and one git gives no URLs for is
// named and doesn't go. It reports whether it added any URL.
func (c *seedConfig) remoteURLs(ctx context.Context, dir, name string) bool {
	fetch, err := hostGit(ctx, dir, "remote", "get-url", "--all", "--", name)
	push, perr := hostGit(ctx, dir, "remote", "get-url", "--push", "--all", "--", name)
	if err != nil || perr != nil {
		c.urlsUnread = append(c.urlsUnread, name)
		return false
	}
	urls, refused := cleanURLs(fetch, false)
	if refused {
		c.urlsKept = append(c.urlsKept, name)
	}
	if len(urls) == 0 {
		return false
	}
	for _, u := range urls {
		c.add("remote."+name+".url", u)
	}
	if push == fetch {
		return true
	}
	pushes, refused := cleanURLs(push, true)
	switch {
	case len(pushes) == 0:
		c.pushKept = append(c.pushKept, name)
	case refused:
		c.urlsKept = append(c.urlsKept, name)
	}
	for _, u := range pushes {
		c.add("remote."+name+".pushurl", u)
	}
	return true
}

// cleanURLs are the URLs of out, one a line, that go (cleanURL), each
// cleaned, but with push one that reaches a devvm VM, and whether any
// didn't go.
func cleanURLs(out string, push bool) (urls []string, refused bool) {
	for u := range strings.Lines(out) {
		u = strings.TrimSuffix(u, "\n")
		clean, ok := cleanURL(u)
		if !ok || push && vmHost(urlHost(u)) {
			refused = true
			continue
		}
		urls = append(urls, clean)
	}
	return urls, refused
}

// submodule adds a submodule's url (as git resolves it, insteadOf
// applied, then cleaned), active, branch and update, but an update that
// runs a command (!command).
func (c *seedConfig) submodule(ctx context.Context, dir, key, name, value string) {
	switch name {
	case "url":
		resolved, err := hostGit(ctx, dir, "ls-remote", "--get-url", "--", value)
		if err != nil {
			resolved = value
		}
		if clean, ok := cleanURL(resolved); ok {
			c.add(key, clean)
		} else {
			c.urlsKept = append(c.urlsKept, key)
		}
	case "update":
		if !strings.HasPrefix(value, "!") {
			c.add(key, value)
		}
	case "active", "branch":
		c.add(key, value)
	}
}

// cleanURL is u as the VM's clone gets it, and whether it goes at all:
// an http(s) URL without its user, password, query or fragment, any of
// which can be a token; an ssh one (ssh://, git+ssh://, ssh+git://)
// without a password, query or fragment, its user kept, as git@ is a
// user's name, not a secret; a scp-like [user@]host:path as it is
// (scpURL); and a file:// URL or a path as it is, which names this
// machine's folder, which the VM can't reach. Anything else, such as
// git:// or ext::, doesn't go.
func cleanURL(u string) (string, bool) {
	scheme, _, hasScheme := strings.Cut(u, "://")
	switch {
	case u == "":
		return "", false
	case !hasScheme && transportHelper(u):
		return "", false
	case !hasScheme:
		if !scpURL(u) {
			return "", false
		}
		return u, true
	}
	p, err := url.Parse(u)
	if err != nil {
		return "", false
	}
	p.RawQuery, p.Fragment, p.RawFragment, p.ForceQuery = "", "", "", false
	switch strings.ToLower(scheme) {
	case "http", "https":
		p.User = nil
	case "ssh", "git+ssh", "ssh+git":
		if p.User != nil {
			p.User = url.User(p.User.Username())
		}
	case "file":
		return u, true
	default:
		return "", false
	}
	return p.String(), true
}

// transportHelper is whether u names a transport helper, as git reads
// one: a scheme's characters, then ::, such as ext:: or fd::.
func transportHelper(u string) bool {
	name, _, ok := strings.Cut(u, "::")
	return ok && strings.Trim(name, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789+.-") == ""
}

// scpURL is whether u, a scp-like [user@]host:path or a path, goes as it
// is (scpHost). Its user, before an @ ahead of the host's colon, holds no
// colon; one whose path has an @ and then a colon, as in
// user:token@host:path, which git reads as the host user and a path
// holding the token, doesn't go.
func scpURL(u string) bool {
	_, path, ok := scpHost(u)
	if !ok {
		return true // a path
	}
	at := strings.IndexByte(path, '@')
	return at < 0 || !strings.Contains(path[at:], ":")
}

// scpHost splits u, a scp-like [user@]host:path, into its host and path
// as git reads them: the host runs to the first colon, past a bracketed
// one such as [::1], and its user, before an @ there, is left out. ok is
// false for a path: no colon, or a slash before it.
func scpHost(u string) (host, path string, ok bool) {
	colon := strings.IndexByte(u, ':')
	start := 0
	if i := strings.Index(u, "@["); i >= 0 && (colon < 0 || i < colon) {
		start = i + 1
	}
	if strings.HasPrefix(u[start:], "[") {
		j := strings.IndexByte(u[start:], ']')
		if j < 0 || !strings.HasPrefix(u[start+j+1:], ":") {
			return "", "", false
		}
		colon = start + j + 1
		host = u[start+1 : start+j]
	} else if colon >= 0 {
		host = u[:colon]
		if i := strings.LastIndexByte(host, '@'); i >= 0 {
			host = host[i+1:]
		}
	}
	if slash := strings.IndexByte(u, '/'); colon < 0 || slash >= 0 && slash < colon {
		return "", "", false
	}
	return host, u[colon+1:], true
}

// readHooks reads the hooks the seed sends (seedConfig's hooks), as git
// here runs them: those of the common dir's hooks folder, when the
// core.hooksPath git uses is none, or names that folder. It's the last
// of the repo's own config and its worktree's, includes followed
// (repoHooksPath), else your global or system config's (hostgit's
// UserConfig, which reads the files your GIT_CONFIG_GLOBAL and
// GIT_CONFIG_SYSTEM name, as git does). One elsewhere inside the repo
// goes as the setting alone, as its hooks are the working tree's, and
// one outside the repo, or that an included file sets (not hooksPath,
// the repo's own last, read without includes), is named. In a linked
// worktree, whose .git is a file, a relative .git/hooks names nothing,
// so git runs no hook there, and none goes.
func (c *seedConfig) readHooks(ctx context.Context, dir, hooksPath string, local bool) error {
	gitDir, common, err := gitprotect.OwnRepo(dir)
	if err != nil {
		return err
	}
	value, own := repoHooksPath(ctx, dir)
	included := own && (!local || value != hooksPath)
	if !own {
		value, _ = hostgit.UserConfig(ctx, "core.hooksPath")
	}
	clean := filepath.Clean(value)
	switch {
	case clean == ".git/hooks" && gitDir != common:
	case value == "" || clean == ".git/hooks" || clean == filepath.Join(common, "hooks"):
		return c.readHooksFolder(common)
	case !included && inRepo(value):
		c.add("core.hookspath", value)
	default:
		c.hooksPathKept = value
	}
	return nil
}

// repoHooksPath is the last core.hooksPath of the repo at dir's own
// config and its worktree's, includes followed, and whether they have
// one: host git's own (the command's) and your global and system ones
// are left out.
func repoHooksPath(ctx context.Context, dir string) (value string, ok bool) {
	out, err := hostgit.Output(ctx, "-C", dir, "config", "--includes", "--show-scope", "--null", "--get-all", "core.hooksPath")
	if err != nil {
		return "", false
	}
	f := strings.Split(string(out), "\x00")
	for i := 0; i+1 < len(f); i += 2 {
		if f[i] == "local" || f[i] == "worktree" {
			value, ok = f[i+1], true
		}
	}
	return value, ok
}

// inRepo is whether a core.hooksPath of v is a folder inside the repo:
// relative, never through .. or a .git, and not ~/ or %(prefix)/, which
// git expands.
func inRepo(v string) bool {
	if filepath.IsAbs(v) || strings.HasPrefix(v, "~") || strings.HasPrefix(v, "%(") {
		return false
	}
	return safeIndexPath(filepath.ToSlash(filepath.Clean(v)))
}

// readHooksFolder reads the hooks of the common dir's hooks folder,
// opened from common, never through a link; one that's missing, a link
// or unreadable is named.
func (c *seedConfig) readHooksFolder(common string) error {
	root, err := safefile.OpenRoot(common)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	d, err := root.OpenDir("hooks")
	if err != nil {
		c.hooksDirKept = filepath.Join(common, "hooks")
		return nil
	}
	defer func() { _ = d.Close() }()
	entries, err := d.ReadDir()
	if err != nil {
		c.hooksDirKept = filepath.Join(common, "hooks")
		return nil
	}
	size := 0
	for _, e := range entries {
		if !slices.Contains(hostgit.HookEvents(), e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil || !e.Type().IsRegular() || info.Size() > hookMax || size+int(info.Size()) > hooksMax {
			c.hooksKept = append(c.hooksKept, e.Name())
			continue
		}
		b, err := d.Read(e.Name(), hookMax, safefile.Opts{})
		if err != nil {
			c.hooksKept = append(c.hooksKept, e.Name())
			continue
		}
		size += len(b)
		c.hooks = append(c.hooks, hookFile{name: e.Name(), data: b, exec: info.Mode().Perm()&0o100 != 0})
	}
	return nil
}

// sayConfig names what of the settings of the repo at dir its seed (c)
// left on this machine.
func (e seeder) sayConfig(dir string, c seedConfig) {
	if kept := slices.Compact(slices.Sorted(slices.Values(c.urlsKept))); len(kept) > 0 {
		e.Warn(msg.CloneUrlsKept(len(kept), dir, shown(kept)))
	}
	if kept := c.urlsUnread; len(kept) > 0 {
		e.Warn(msg.CloneUrlsUnread(len(kept), dir, shown(kept)))
	}
	if kept := c.pushKept; len(kept) > 0 {
		e.Warn(msg.ClonePushUrlsKept(len(kept), dir, shown(kept)))
	}
	if kept := c.hooksKept; len(kept) > 0 {
		e.Warn(msg.CloneHooksKept(len(kept), dir, shown(kept)))
	}
	if p := c.hooksPathKept; p != "" {
		e.Warn(msg.CloneHooksPathKept(dir, p))
	}
	if p := c.hooksDirKept; p != "" {
		e.Warn(msg.CloneHooksDirKept(dir, p))
	}
}

// entries are c's tar entries: config (its pairs, each key and value
// ending in a NUL), config.keys (each key it sets, the same) and each
// hook, hooks/NAME, an executable one 0o700.
func (c seedConfig) entries() []modeEntry {
	out := []modeEntry{
		{tarEntry{"config", nulList(c.pairs)}, 0o600},
		{tarEntry{"config.keys", nulList(c.keys)}, 0o600},
	}
	for _, h := range c.hooks {
		mode := int64(0o600)
		if h.exec {
			mode = 0o700
		}
		out = append(out, modeEntry{tarEntry{path.Join("hooks", h.name), h.data}, mode})
	}
	return out
}

// modeEntry is a tar entry with its mode.
type modeEntry struct {
	tarEntry
	mode int64
}
