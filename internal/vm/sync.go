package vm

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/devvmhq/devvm/internal/agentlogin"
	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/hostgit"
	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/netpolicy"
	"github.com/devvmhq/devvm/internal/proxy"
	"github.com/devvmhq/devvm/internal/secrets"
	"github.com/devvmhq/devvm/internal/shell"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/tools"
	"github.com/devvmhq/devvm/internal/trust"
)

// syncCmd unpacks what the host sends into a folder of its own in the VM,
// runs its sync.sh, and removes it.
const syncCmd = `d=$(mktemp -d); tar -xf - -C "$d" --warning=no-unknown-keyword --warning=no-timestamp && bash "$d/sync.sh"; rc=$?; rm -rf "$d"; exit $rc`

// syncer sends a running VM its setup.
type syncer struct{ *Env }

// sync sets up a running VM on every launch: devvm's guest/sync.sh runs in it,
// with what it needs sent over as a tar stream, staged in memory (it holds
// credentials): the host's settings, the agents' config, its shared tools,
// the user's CAs (trust.Read: a folder of them that holds anything else
// fails it), and its links' addresses and names (Env.Links, checkLinks).
// On every tier its proxy's address, devvm's CA, the tokens' and logins'
// placeholders, and the agents' settings files with their keys taken out:
// no VM holds a real secret.
func (e syncer) sync(ctx context.Context, b backend.Backend, st *state.VM, slug string, files []string, conf backend.Confinement) error {
	vm := st.Name()
	links, err := e.links(st)
	if err != nil {
		return err
	}
	moved := sectionOf[movedInVM](st, movedSection)
	s, err := e.stage(ctx, st, setupInputs{slug: slug, files: files, proxyURL: b.ProxyURL(conf), links: links, moved: moved.From})
	if err != nil {
		return err
	}
	data, err := s.tar()
	if err != nil {
		return err
	}
	out := e.vmLog()
	err = b.Exec(ctx, vm, []string{"bash", "-c", syncCmd}, backend.ExecOpts{Stdin: bytes.NewReader(data), Stdout: out, Stderr: out, Agent: conf.AgentSocket})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if bad := e.unmounted(ctx, b, vm, files, conf.AgentSocket); len(bad) > 0 {
			return backend.Logged(msg.ToolsUnmounted(tools.Label(bad), vm))
		}
		return backend.Logged(msg.SyncFailed(vm))
	}
	return followed(ctx, st, moved)
}

// followed clears st's moved section once its setup moved what it
// recorded (moved), unless another move recorded more meanwhile.
func followed(ctx context.Context, st *state.VM, moved movedInVM) error {
	if moved.From == "" {
		return nil
	}
	return update(ctx, st, movedSection, func(m *movedInVM) error {
		if *m == moved {
			*m = movedInVM{}
		}
		return nil
	})
}

// unmountedFile is where devvm-tools (guest/tools.sh) lists the tool
// images it couldn't mount or link on its last run, a line each.
const unmountedFile = "/run/devvm/tools-unmounted"

// unmountedMax caps how much of unmountedFile devvm reads.
const unmountedMax = 16 << 10

// unmountedWait bounds reading unmountedFile: root in the VM can make the
// read hang, and a failed setup's error shouldn't wait on it.
const unmountedWait = 10 * time.Second

// unmounted returns the VM's tool files (files, from its state) that its
// devvm-tools last said it couldn't mount, read after a failed setup, its
// connection carrying agent, the VM's.
func (e syncer) unmounted(ctx context.Context, b backend.Backend, vm string, files []string, agent string) []string {
	return readUnmounted(ctx, b, vm, files, agent, unmountedWait)
}

// readUnmounted is unmounted, giving up after wait: then no tool counts.
func readUnmounted(ctx context.Context, b backend.Backend, vm string, files []string, agent string, wait time.Duration) []string {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	out := &bounded{max: unmountedMax}
	if err := b.Exec(ctx, vm, []string{"cat", unmountedFile}, backend.ExecOpts{Stdout: out, Stderr: io.Discard, Agent: agent}); err != nil || ctx.Err() != nil {
		return nil
	}
	return reported(files, out.Bytes())
}

// reported returns the files that a report from the VM lists, in its
// order, each once. The VM writes the report, so only a whole line that
// is one of files counts: what's returned is always devvm's own value,
// never the VM's text.
func reported(files []string, report []byte) []string {
	var bad []string
	for line := range bytes.Lines(report) {
		f, ok := bytes.CutSuffix(line, []byte("\n"))
		if !ok {
			continue // cut short
		}
		i := slices.Index(files, string(f))
		if i >= 0 && !slices.Contains(bad, files[i]) {
			bad = append(bad, files[i])
		}
	}
	return bad
}

// setupInputs is what devvm gives a VM's setup besides its state: its
// hostname (the folder's slug), its tool files, its proxy's address (empty
// when the backend gives the guest none), its links, and the path its folder moved
// from, "" when it didn't (its moved section).
type setupInputs struct {
	slug, proxyURL, moved string
	files                 []string
	links                 []GuestLink
}

// GuestLink is one target a client VM reaches through its links: the
// address devvm chose for that target in the client, in 127.77.0.0/16
// (proxy.IsLinkAddr), and the names the client reaches it by, each one DNS
// label. The setup adds NAME.devvm.internal for each name itself.
type GuestLink struct {
	Addr  netip.Addr
	Names []string
}

// checkLinks refuses links that aren't as devvm makes them, before any of
// them reaches the VM (vm, its hostname host): an address outside
// 127.77.0.0/16 (proxy.IsLinkAddr) or given twice, or a name that isn't one lowercase DNS label, is
// localhost or the VM's hostname, or is given twice. The setup writes
// them into /etc/hosts and NO_PROXY as they are.
func checkLinks(vm, host string, links []GuestLink) error {
	var addrs []netip.Addr
	var names []string
	for _, l := range links {
		bad := func(name string) error { return msg.LinkSetupInvalid(l.Addr.String(), name, vm) }
		if !l.Addr.Is4() || !proxy.IsLinkAddr(l.Addr) || slices.Contains(addrs, l.Addr) {
			return bad(strings.Join(l.Names, " "))
		}
		addrs = append(addrs, l.Addr)
		if len(l.Names) == 0 {
			return bad("")
		}
		for _, n := range l.Names {
			if !dnsLabel(n) || n == "localhost" || n == host || slices.Contains(names, n) {
				return bad(n)
			}
			names = append(names, n)
		}
	}
	return nil
}

// dnsLabel reports whether s is one lowercase DNS label: letters, digits
// and dashes, at most 63, with a letter or digit at each end.
func dnsLabel(s string) bool {
	if s == "" || len(s) > 63 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for _, c := range s {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

// linkHosts is the setup's hosts file: a line per target, its address and
// then each of its names and NAME.devvm.internal, which guest/sync.sh
// puts between its markers in /etc/hosts.
func linkHosts(links []GuestLink) []byte {
	var b bytes.Buffer
	for _, l := range links {
		b.WriteString(l.Addr.String())
		for _, n := range l.Names {
			b.WriteString(" " + n + " " + n + "." + proxy.LinkDomain)
		}
		b.WriteString("\n")
	}
	return b.Bytes()
}

// links returns st's links (Env.Links): none without it.
func (e syncer) links(st *state.VM) ([]GuestLink, error) {
	if e.Links == nil {
		return nil, nil
	}
	return e.Links(st)
}

// checkStartLinks checks st's links before a start, as its setup checks
// them again against the VM's hostname. They change nothing of the
// start: its HTTP clients reach them through its proxy.
func (e syncer) checkStartLinks(st *state.VM) error {
	links, err := e.links(st)
	if err != nil {
		return err
	}
	return checkLinks(st.Name(), "", links)
}

// localProxy is where the VM's local proxy listens in the VM, ADDRESS:PORT, which sync.sh's
// devvm-local-proxy takes, or "" when the VM has no proxy (proxyURL "").
func localProxy(proxyURL string) string {
	if proxyURL == "" {
		return ""
	}
	return net.JoinHostPort(proxy.LocalProxyAddr, strconv.Itoa(proxy.LocalProxyPort))
}

// stage returns what sync sends; see devvm's guest/sync.sh for each file.
func (e syncer) stage(ctx context.Context, st *state.VM, in setupInputs) (*stage, error) {
	vm, names := st.Name(), tools.Names(st)
	slug, files, proxyURL := in.slug, in.files, in.proxyURL
	if err := checkLinks(vm, slug, in.links); err != nil {
		return nil, err
	}
	store := e.Proxy.Secrets
	s := &stage{}
	s.put("hosts", 0o644, linkHosts(in.links))
	if m := claudeMoved(in.moved, st.Dir); m != nil {
		s.put("moved", 0o644, m)
	}
	for _, name := range []string{"sync.sh", "tools.sh", "ca.sh", "claude.sh", "guide.md", opencodeScript} {
		data, err := fs.ReadFile(e.Assets, "guest/"+name)
		if err != nil {
			return nil, err
		}
		s.put(name, 0o755, data)
	}
	// The user's CAs, every VM's: only the certificates parsed out of
	// their files.
	cas, err := trust.Read(e.Dirs.TrustedCAs())
	if err != nil {
		return nil, err
	}
	for i, c := range cas {
		s.put("ca/"+trust.Name(i), 0o644, c)
	}
	s.put("tools", 0o644, tools.Mounts(files))
	aliases, err := tools.Aliases(e.Assets)
	if err != nil {
		return nil, err
	}
	s.put("tool-alias.toml", 0o644, aliases)

	// The host's settings, for every VM.
	lang := e.Getenv("LC_ALL")
	if lang == "" {
		lang = e.Getenv("LANG")
	}
	if !strings.HasSuffix(lang, ".UTF-8") {
		lang = ""
	}
	tz := e.Getenv("TZ")
	if tz == "" {
		tz = e.HostTZ()
	}
	vars := [][2]string{
		{"DEVVM_HOSTNAME", slug},
		{"DEVVM_TZ", tz},
		{"DEVVM_LANG", lang},
		{"DEVVM_GIT_NAME", e.GitConfig(ctx, st.Dir, "user.name")},
		{"DEVVM_GIT_EMAIL", e.GitConfig(ctx, st.Dir, "user.email")},
		{"DEVVM_PROXY", proxyURL},
		{"DEVVM_NO_PROXY", netpolicy.NoProxy},
		{"DEVVM_LOCAL_PROXY", localProxy(proxyURL)},
	}
	// The devcontainer.json's variables first: devvm's own win.
	dcEnv, err := devcontainerEnv(st)
	if err != nil {
		return nil, err
	}
	var env bytes.Buffer
	env.WriteString(dcEnv)
	if lang != "" {
		env.WriteString("export LANG=" + shell.Quote(lang) + "\n")
	}

	// The agents' settings from devvm's store: folders to link to their
	// mounts, each its path in the home folder and its mount's, files
	// copied (the logins' copies are stageSecrets'). Never their history.
	links, err := e.stageAgents(s, names)
	if err != nil {
		return nil, err
	}

	// Tokens: always a placeholder, which the proxy puts the value in for.
	salt, err := netpolicy.Salt(ctx, st)
	if err != nil {
		return nil, err
	}
	for _, name := range secrets.Names(st) {
		sec, ok := secrets.Lookup(name)
		if !ok {
			continue
		}
		v, err := store.Value(ctx, name)
		if errors.Is(err, secrets.ErrNone) {
			switch {
			case name == "claude" && store.HasLogin(agentlogin.ClaudeOAuth):
				// The VM gets the OAuth login instead (stageSecrets), never both.
			case name == "claude":
				e.Warn(msg.NoClaudeToken(vm))
			case name == "github":
				e.Warn(msg.NoGithubToken(vm))
			}
			continue
		} else if err != nil {
			return nil, err
		}
		if name == "claude" && !strings.HasPrefix(v, "sk-ant-oat01-") {
			e.Warn(msg.ClaudeTokenInvalid())
			continue
		}
		env.WriteString("export " + sec.Env + "=" + shell.Quote(sec.Placeholder(salt)) + "\n")
		if name == "github" {
			vars = append(vars, [2]string{"DEVVM_GITHUB", "1"})
		}
	}

	if err := e.stageSecrets(ctx, s, store, vm, names, salt); err != nil {
		return nil, err
	}
	var vmFile bytes.Buffer
	for _, kv := range vars {
		vmFile.WriteString(kv[0] + "=" + shell.Quote(kv[1]) + "\n")
	}
	s.put("vm", 0o644, vmFile.Bytes())
	s.put("env", 0o600, env.Bytes())
	s.put("links", 0o644, links)
	return s, nil
}

// stageAgents adds the agents' settings from devvm's store (agentStore)
// to s, and returns the setup's links file: two lines for each folder, its
// path in the VM's home and its mount's path.
func (e syncer) stageAgents(s *stage, names []string) ([]byte, error) {
	var links bytes.Buffer
	for _, a := range image.AgentsOf(names) {
		store := agentStore(e.Dirs, a)
		for _, rel := range agentDirs(store, a) {
			links.WriteString(rel + "\n" + filepath.Join(store, rel) + "\n")
		}
		for _, rel := range agentFiles(store, a) {
			if err := s.copy("home/"+rel, filepath.Join(store, rel)); err != nil {
				return nil, err
			}
		}
	}
	return links.Bytes(), nil
}

// claudeMoved is the setup's moved file for a project folder that moved
// from from to dir, its paths and their Claude Code slugs (claudeSlug),
// for guest/claude.sh's claude_project_moved; nil when it didn't move.
func claudeMoved(from, dir string) []byte {
	if from == "" || from == dir {
		return nil
	}
	var b bytes.Buffer
	for _, kv := range [][2]string{
		{"moved_from", from},
		{"moved_to", dir},
		{"claude_from", claudeSlug(from)},
		{"claude_to", claudeSlug(dir)},
	} {
		b.WriteString(kv[0] + "=" + shell.Quote(kv[1]) + "\n")
	}
	return b.Bytes()
}

// stageSecrets adds what every VM gets so that it holds no real secret:
// devvm's CA, which its proxy signs the hosts it puts secrets in for
// with, its logins' copies with placeholders ending in its salt, and its
// agents' settings files with the values under secret-looking keys taken
// out (one that can't be read that way isn't sent, nor are opencode's
// files that aren't settings or Markdown: withheld).
func (e syncer) stageSecrets(ctx context.Context, s *stage, store *secrets.Store, vm string, tools []string, salt string) error {
	ca := e.Dirs.VM(vm).CA()
	for _, f := range [][2]string{{"ca/devvm-proxy.crt", ca.Cert()}, {"ca/devvm-proxy-next.crt", ca.NextCert()}} {
		cert, err := os.ReadFile(f[1])
		if err != nil {
			return err
		}
		s.put(f[0], 0o644, cert)
	}
	for _, kind := range store.Logins(ctx, tools) {
		data, err := store.VMLogin(kind, salt)
		if err != nil {
			return msg.LoginCopyFailed(kind, err.Error(), vm).Because(err)
		}
		name := "claude.json" // merged by sync.sh, not copied
		if kind != "claude" {
			name = "home/" + secrets.LoginRel(kind)
		}
		s.put(name, 0o600, data)
		if kind == agentlogin.Opencode {
			imp, err := agentlogin.OpencodeImport(data)
			if err != nil {
				return msg.LoginCopyFailed(kind, err.Error(), vm).Because(err)
			}
			s.put("opencode-import.json", 0o600, imp)
		}
	}
	for _, f := range slices.Clone(s.files) {
		rel, ok := strings.CutPrefix(f.name, "home/")
		if !ok || f.from == "" {
			continue
		}
		shown := f.from
		format, ok := settingsFormat(rel)
		if !ok {
			if withheld(rel) {
				s.drop(f.name)
				e.Warn(msg.ConfigNotCopied(shown, vm))
			}
			continue
		}
		out, keys, err := agentlogin.Redact(format, f.data)
		if err != nil {
			s.drop(f.name)
			e.Warn(msg.ConfigNotCopied(shown, vm))
			continue
		}
		f.data = out
		s.add(f)
		if len(keys) > 0 {
			e.Note(msg.ConfigRedacted(shown, strings.Join(keys, " "), vm))
		}
	}
	return nil
}

// settingsFormat returns the format of an agent's settings file, by its
// path relative to the home folder: Claude Code's settings.json, Codex's
// config.toml, opencode's JSON, JSONC and TOML files.
func settingsFormat(rel string) (string, bool) {
	ext := filepath.Ext(rel)
	switch {
	case rel == ".claude/settings.json":
	case rel == ".codex/config.toml":
	case filepath.Dir(rel) == opencodeDir && (ext == ".json" || ext == ".jsonc" || ext == ".toml"):
	default:
		return "", false
	}
	if ext == ".toml" {
		return "toml", true
	}
	return "json", true
}

// withheld reports whether a VM doesn't get an agent's
// file, by its path relative to the home folder, because devvm can't take
// secrets out of it: opencode's config folder's files but its settings
// (settingsFormat) and its Markdown, such as AGENTS.md. A .env is one.
func withheld(rel string) bool {
	_, settings := settingsFormat(rel)
	return !settings && filepath.Dir(rel) == opencodeDir && filepath.Ext(rel) != ".md"
}

// hostTZ is the host's timezone: what /etc/localtime links to, else
// /etc/timezone.
func hostTZ() string {
	if l, err := os.Readlink("/etc/localtime"); err == nil {
		if _, tz, ok := strings.Cut(l, "zoneinfo/"); ok {
			return tz
		}
	}
	b, _ := os.ReadFile("/etc/timezone")
	return strings.TrimSpace(string(b))
}

// gitConfig is git's setting for key in dir, or "".
func gitConfig(ctx context.Context, dir, key string) string {
	cmd := hostgit.Command(ctx, "config", "--get", key)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimRight(string(out), "\n")
}

// stageFile is one file sync sends; from is the host file a copy is of.
type stageFile struct {
	name  string
	mode  int64
	mtime time.Time
	data  []byte
	from  string
}

// stage is the files sync sends, in order; a name added again replaces
// its file.
type stage struct{ files []stageFile }

func (s *stage) index(name string) int {
	return slices.IndexFunc(s.files, func(f stageFile) bool { return f.name == name })
}

func (s *stage) add(f stageFile) {
	if i := s.index(f.name); i >= 0 {
		s.files[i] = f
		return
	}
	s.files = append(s.files, f)
}

func (s *stage) put(name string, mode int64, data []byte) {
	s.add(stageFile{name: name, mode: mode, mtime: time.Now(), data: data})
}

func (s *stage) drop(name string) {
	if i := s.index(name); i >= 0 {
		s.files = slices.Delete(s.files, i, i+1)
	}
}

// copy adds the host file at path, with its mode and time, if it's a
// file (or a link to one).
func (s *stage) copy(name, path string) error {
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return nil //nolint:nilerr // nothing to copy
	}
	data, err := os.ReadFile(path) //nolint:gosec // the user's own agent config
	if err != nil {
		return err
	}
	s.add(stageFile{name: name, mode: int64(fi.Mode().Perm()), mtime: fi.ModTime(), data: data, from: path})
	return nil
}

// tar returns the files as a tar stream.
func (s *stage) tar() ([]byte, error) {
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	for _, f := range s.files {
		h := &tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.data)), ModTime: f.mtime, Typeflag: tar.TypeReg}
		if err := w.WriteHeader(h); err != nil {
			return nil, err
		}
		if _, err := w.Write(f.data); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
