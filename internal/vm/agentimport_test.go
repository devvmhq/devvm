package vm

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/safefile"
	"github.com/devvmhq/devvm/internal/secrets"
	"github.com/devvmhq/devvm/internal/state"
)

// importer is an AgentImport in a sandbox: a home folder of its own and
// devvm's folders, its messages' names in got, its questions answered
// yes, but whether to follow links out of the agents' folders only with
// follow, or refused without a terminal (tty), its exclusions from
// backups in excluded.
type importer struct {
	AgentImport
	got, excluded []string
	fake          *msg.Fake
	follow        bool
}

func newImporter(t *testing.T) *importer {
	t.Helper()
	root := t.TempDir()
	d := paths.Dirs{Home: filepath.Join(root, "home"), Data: filepath.Join(root, "data"), Config: filepath.Join(root, "config")}
	im := &importer{}
	im.fake = &msg.Fake{Terminal: true}
	im.fake.OnAny = func(m msg.M) { im.got = append(im.got, m.Names()) }
	im.fake.OnAsk = func(m msg.M) bool {
		im.got = append(im.got, "asked "+m.Names())
		return m.ID() != "agent_import_follow" || im.follow
	}
	store := secrets.NewStore(d, im.fake)
	store.ExcludeFromBackup = func(_ context.Context, dir string) error {
		im.excluded = append(im.excluded, dir)
		return nil
	}
	im.AgentImport = AgentImport{
		Dirs: d, Secrets: store, Reporter: im.fake,
		Opencode: func(context.Context, ...string) ([]byte, error) { return nil, exec.ErrNotFound },
	}
	return im
}

// write writes files, by path relative to root, each its content.
func write(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, data := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p) //nolint:gosec // the test's own sandbox
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mode(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// An import copies each agent's settings and logins into devvm's store,
// laid out as in the home folder, never their history: Claude Code's
// .claude.json without its projects, logins only the user's to read in
// a folder kept out of backups, a skill's script still executable, and
// Claude Code's plugin records naming the store's folders.
func TestAgentImport(t *testing.T) {
	im := newImporter(t)
	home := im.Dirs.Home
	write(t, home, map[string]string{
		".claude/settings.json":                    `{"theme":"dark"}`,
		".claude/skills/s/SKILL.md":                "# skill",
		".claude/skills/s/run.sh":                  "echo",
		".claude/projects/p/h.jsonl":               "history",
		".claude/.credentials.json":                "secret",
		".claude/plugins/known_marketplaces.json":  `{"m":{"installLocation":"` + filepath.Join(home, ".claude/plugins") + `/marketplaces/m"}}`,
		".claude.json":                             `{"theme":"dark","projects":{"/p":{}},"primaryApiKey":"sk-x","mcpServers":{}}`,
		".codex/config.toml":                       "model = 'x'",
		".codex/auth.json":                         `{"OPENAI_API_KEY":"sk-codex"}`,
		".codex/history.jsonl":                     "history",
		".config/opencode/opencode.json":           "{}",
		".config/opencode/agents/a.md":             "agent",
		".config/opencode/node_modules/x/index.js": "x",
		".local/share/opencode/auth.json":          `{}`,
	})
	if err := os.Chmod(filepath.Join(home, ".claude/skills/s/run.sh"), 0o775); err != nil { //nolint:gosec // a skill's script, group-writable

		t.Fatal(err)
	}
	if err := im.Run(t.Context(), nil, false); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	claude, codex, opencode := agentStore(im.Dirs, "claude"), agentStore(im.Dirs, "codex"), agentStore(im.Dirs, "opencode")
	for p, want := range map[string]string{
		filepath.Join(claude, ".claude/settings.json"):            `{"theme":"dark"}`,
		filepath.Join(claude, ".claude/skills/s/SKILL.md"):        "# skill",
		filepath.Join(codex, ".codex/config.toml"):                "model = 'x'",
		filepath.Join(opencode, ".config/opencode/agents/a.md"):   "agent",
		filepath.Join(opencode, ".config/opencode/opencode.json"): "{}",
		secrets.LoginFile(im.Dirs, "codex"):                       `{"OPENAI_API_KEY":"sk-codex"}`,
		secrets.LoginFile(im.Dirs, "opencode"):                    `{}`,
		filepath.Join(claude, ".claude/plugins/known_marketplaces.json"): `{"m":{"installLocation":"` +
			filepath.Join(claude, ".claude/plugins") + `/marketplaces/m"}}`,
	} {
		if got := readFile(t, p); got != want {
			t.Errorf("%s = %q, want %q", p, got, want)
		}
	}
	for _, p := range []string{
		filepath.Join(claude, ".claude/projects"), filepath.Join(claude, ".claude/.credentials.json"),
		filepath.Join(codex, ".codex/history.jsonl"), filepath.Join(codex, ".codex/auth.json"),
		filepath.Join(opencode, ".config/opencode/node_modules"),
	} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s copied: %v", p, err)
		}
	}
	cj := readFile(t, secrets.LoginFile(im.Dirs, "claude"))
	if !strings.Contains(cj, `"theme": "dark"`) || strings.Contains(cj, "projects") || strings.Contains(cj, "sk-x") {
		t.Errorf(".claude.json's copy = %s", cj)
	}
	if m := mode(t, filepath.Join(claude, ".claude/skills/s/run.sh")); m != 0o755 {
		t.Errorf("a script's mode = %o", m)
	}
	for _, a := range []string{"claude", "codex", "opencode"} {
		if m := mode(t, secrets.LoginFile(im.Dirs, a)); m != 0o600 {
			t.Errorf("%s's login's mode = %o", a, m)
		}
		if m := mode(t, im.Dirs.AgentLogins(a)); m != 0o700 {
			t.Errorf("%s's logins folder's mode = %o", a, m)
		}
	}
	wantExcluded := []string{im.Dirs.AgentLogins("claude"), im.Dirs.AgentLogins("codex"), im.Dirs.AgentLogins("opencode")}
	if !slices.Equal(im.excluded, wantExcluded) {
		t.Errorf("excluded from backups %q", im.excluded)
	}
	want := []string{
		"agent_settings_imported agent=claude dir=" + claude, "agent_login_imported agent=claude file=" + secrets.LoginFile(im.Dirs, "claude"),
		"agent_settings_imported agent=codex dir=" + codex, "agent_login_imported agent=codex file=" + secrets.LoginFile(im.Dirs, "codex"),
		"agent_settings_imported agent=opencode dir=" + opencode, "agent_login_imported agent=opencode file=" + secrets.LoginFile(im.Dirs, "opencode"),
	}
	if !slices.Equal(im.got, want) {
		t.Errorf("said %q", im.got)
	}
}

// notFollowed reports whether the import said it left out the link at p,
// which leads out of its agent's folder, naming --follow-links.
func (im *importer) notFollowed(p string) bool {
	return slices.ContainsFunc(im.got, func(s string) bool {
		return strings.HasPrefix(s, "agent_import_link_not_followed path="+p+" target=")
	})
}

// A link in an agent's folder is followed, unasked, only to a file inside
// it: one out of the folder, relative or absolute, is left out with a
// warning after a no, and so is one to a folder inside it, and a login
// file that's a link, whatever the answer.
func TestAgentImportLinks(t *testing.T) {
	im := newImporter(t)
	home := im.Dirs.Home
	write(t, home, map[string]string{
		".claude/skills/s/SKILL.md": "# skill",
		".claude/CLAUDE.md":         "notes",
		"outside/secret.txt":        "outside",
		"outside/claude.json":       `{"theme":"x"}`,
	})
	skills := filepath.Join(home, ".claude/skills")
	for name, target := range map[string]string{
		"escape":     "../../outside/secret.txt",
		"escape-abs": filepath.Join(home, "outside/secret.txt"),
		"inside":     "../CLAUDE.md",
		"folder":     "s",
	} {
		if err := os.Symlink(target, filepath.Join(skills, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(home, "outside/claude.json"), filepath.Join(home, ".claude.json")); err != nil {
		t.Fatal(err)
	}
	if err := im.Run(t.Context(), []string{"claude"}, false); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	store := filepath.Join(agentStore(im.Dirs, "claude"), ".claude/skills")
	if got := readFile(t, filepath.Join(store, "inside")); got != "notes" {
		t.Errorf("a link to a file inside = %q", got)
	}
	if fi, err := os.Lstat(filepath.Join(store, "inside")); err != nil || !fi.Mode().IsRegular() {
		t.Errorf("a link to a file inside isn't copied as a file: %v", err)
	}
	for _, n := range []string{"escape", "escape-abs", "folder"} {
		if _, err := os.Lstat(filepath.Join(store, n)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s copied: %v", n, err)
		}
	}
	if im.Secrets.HasLogin("claude") {
		t.Error("a login file that's a link copied")
	}
	for _, p := range []string{filepath.Join(skills, "folder"), filepath.Join(home, ".claude.json")} {
		if !slices.Contains(im.got, "agent_import_link_left_out path="+p) {
			t.Errorf("no warning for %s: %q", p, im.got)
		}
	}
	for _, p := range []string{filepath.Join(skills, "escape"), filepath.Join(skills, "escape-abs")} {
		if !im.notFollowed(p) {
			t.Errorf("no warning for %s: %q", p, im.got)
		}
	}
	if !slices.Contains(im.got, "asked agent_import_follow") {
		t.Errorf("not asked: %q", im.got)
	}
}

// A link to a file inside the agent's folder is copied only when that file
// is itself among the agent's settings, which the import would copy: one
// into what of a seed's folder is this machine's own, such as a plugin's
// data, or to its history or its login, is left out with a warning naming
// where it leads. The control: a link to a settings file is a copy.
func TestAgentImportLinksInsideToSettingsOnly(t *testing.T) {
	im := newImporter(t)
	home := im.Dirs.Home
	write(t, home, map[string]string{
		".claude/CLAUDE.md":                       "notes",
		".claude/plugins/data/p/token.json":       "plugin data",
		".claude/plugins/known_marketplaces.json": "{}",
		".claude/plugins/cache/m/p/SKILL.md":      "# p",
		".claude/projects/p/h.jsonl":              "history",
		".claude/.credentials.json":               "login",
		".claude/skills/s/SKILL.md":               "# s",
	})
	claude := filepath.Join(home, ".claude")
	links := map[string]string{
		"plugins/cache/m/p/data.json": "../../../data/p/token.json",
		"skills/s/history.md":         "../../projects/p/h.jsonl",
		"skills/s/login.md":           "../../.credentials.json",
		"skills/s/notes.md":           "../../CLAUDE.md",
		// Another spelling of a setting, which a file system that ignores case finds.
		"skills/s/case.md": "../../claude.md",
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(claude, filepath.FromSlash(name))); err != nil {
			t.Fatal(err)
		}
	}
	if err := im.Run(t.Context(), []string{"claude"}, false); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	store := filepath.Join(agentStore(im.Dirs, "claude"), ".claude")
	if got := readFile(t, filepath.Join(store, "skills/s/notes.md")); got != "notes" {
		t.Errorf("a link to a settings file = %q", got)
	}
	for _, name := range []string{"plugins/cache/m/p/data.json", "skills/s/history.md", "skills/s/login.md"} {
		if _, err := os.Lstat(filepath.Join(store, filepath.FromSlash(name))); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s copied: %v", name, err)
		}
		p := filepath.Join(claude, filepath.FromSlash(name))
		target, err := filepath.EvalSymlinks(p)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(im.got, "agent_import_link_not_setting path="+p+" target="+target) {
			t.Errorf("no warning for %s: %q", name, im.got)
		}
	}
	// Another spelling: on a file system that ignores case it leads to CLAUDE.md, and still
	// isn't copied; on one that doesn't, it leads nowhere.
	if _, err := os.Lstat(filepath.Join(store, "skills/s/case.md")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("case.md copied: %v", err)
	}
	if !slices.ContainsFunc(im.got, func(s string) bool { return strings.Contains(s, "path="+filepath.Join(claude, "skills/s/case.md")) }) {
		t.Errorf("no warning for case.md: %q", im.got)
	}
}

// What the import listed is what it reads: a link checked as leading to a
// setting, then turned to the agent's login, a file turned into a link to
// its history, and a folder turned into a link to the agent's folder, as
// a VM whose project folder is in a settings folder could between the
// listing and the read, fail the import, and the store gets none of them.
// The control: the same import with nothing swapped copies each.
func TestAgentImportSwappedAfterListing(t *testing.T) {
	for _, swap := range []bool{false, true} {
		im := newImporter(t)
		home := im.Dirs.Home
		claude := filepath.Join(home, ".claude")
		write(t, home, map[string]string{
			".claude/CLAUDE.md":                 "notes",
			".claude/.credentials.json":         "SECRET LOGIN",
			".claude/history.jsonl":             "SECRET HISTORY",
			".claude/skills/vm/a.md":            "a",
			".claude/skills/vm/d/history.jsonl": "d",
		})
		vm := filepath.Join(claude, "skills/vm")
		if err := os.Symlink("../../CLAUDE.md", filepath.Join(vm, "x.md")); err != nil {
			t.Fatal(err)
		}
		h, err := im.read(t.Context(), "claude", nil)
		if err != nil {
			t.Fatal(err)
		}
		if swap {
			for name, target := range map[string]string{"x.md": "../../.credentials.json", "a.md": "../../history.jsonl", "d": "../.."} {
				if err := os.RemoveAll(filepath.Join(vm, name)); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, filepath.Join(vm, name)); err != nil {
					t.Fatal(err)
				}
			}
		}
		err = im.save(t.Context(), h)
		h.close()
		store := filepath.Join(agentStore(im.Dirs, "claude"), ".claude/skills/vm")
		if !swap {
			if err != nil || readFile(t, filepath.Join(store, "x.md")) != "notes" || readFile(t, filepath.Join(store, "a.md")) != "a" {
				t.Errorf("nothing swapped: %v", err)
			}
			continue
		}
		if err == nil {
			t.Error("swapped: no error")
		}
		_ = filepath.WalkDir(agentStore(im.Dirs, "claude"), func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				if got := readFile(t, p); strings.HasPrefix(got, "SECRET") {
					t.Errorf("%s = %q", p, got)
				}
			}
			return nil
		})
	}
}

// opencode's folder, which import copies whole, treats its links as the
// other agents' folders do: one to a file inside is copied as that file,
// and one out of the folder (after a no), or to a folder inside it, is
// left out with a warning, never dropped without one. A link named as a
// lock file or node_modules is no setting, so it's left out without one.
func TestAgentImportOpencodeLinks(t *testing.T) {
	im := newImporter(t)
	home := im.Dirs.Home
	write(t, home, map[string]string{
		".config/opencode/opencode.json": "{}",
		".config/opencode/mine/a.md":     "a",
		"dotfiles/AGENTS.md":             "notes",
		"dotfiles/agents/b.md":           "b",
	})
	folder := filepath.Join(home, ".config/opencode")
	links := map[string]string{
		"inside.json":  "opencode.json",
		"AGENTS.md":    filepath.Join(home, "dotfiles/AGENTS.md"),
		"agents":       filepath.Join(home, "dotfiles/agents"),
		"local":        "mine",
		"bun.lock":     filepath.Join(home, "dotfiles/AGENTS.md"),
		"node_modules": filepath.Join(home, "dotfiles/agents"),
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(folder, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := im.Run(t.Context(), []string{"opencode"}, false); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	store := filepath.Join(agentStore(im.Dirs, "opencode"), ".config/opencode")
	if got := readFile(t, filepath.Join(store, "inside.json")); got != "{}" {
		t.Errorf("a link to a file inside = %q", got)
	}
	for _, n := range []string{"AGENTS.md", "agents", "local", "bun.lock", "node_modules"} {
		if _, err := os.Lstat(filepath.Join(store, n)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s copied: %v", n, err)
		}
	}
	for _, n := range []string{"AGENTS.md", "agents"} {
		if !im.notFollowed(filepath.Join(folder, n)) {
			t.Errorf("no warning for %s: %q", n, im.got)
		}
	}
	if !slices.Contains(im.got, "agent_import_link_left_out path="+filepath.Join(folder, "local")) {
		t.Errorf("no warning for local: %q", im.got)
	}
	for _, n := range []string{"bun.lock", "node_modules", "inside.json"} {
		if slices.ContainsFunc(im.got, func(s string) bool { return strings.Contains(s, "path="+filepath.Join(folder, n)) }) {
			t.Errorf("a warning for %s: %q", n, im.got)
		}
	}
}

// A settings folder that is itself a link is left out with a warning,
// whether it leads out of the agent's folder (after a no) or to a folder
// inside it.
func TestAgentImportFolderLink(t *testing.T) {
	im := newImporter(t)
	home := im.Dirs.Home
	write(t, home, map[string]string{"dotfiles/skills/s/SKILL.md": "# skill", ".claude/settings.json": "{}", ".claude/mine/a.md": "a"})
	if err := os.Symlink(filepath.Join(home, "dotfiles/skills"), filepath.Join(home, ".claude/skills")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("mine", filepath.Join(home, ".claude/agents")); err != nil {
		t.Fatal(err)
	}
	if err := im.Run(t.Context(), []string{"claude"}, false); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	for _, n := range []string{"skills", "agents"} {
		if _, err := os.Lstat(filepath.Join(agentStore(im.Dirs, "claude"), ".claude", n)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a linked folder copied: %v", err)
		}
	}
	if !im.notFollowed(filepath.Join(home, ".claude/skills")) || !slices.Contains(im.got, "agent_import_link_left_out path="+filepath.Join(home, ".claude/agents")) {
		t.Errorf("said %q", im.got)
	}
}

// What the store has of an agent is replaced only after a yes, or with
// force: without a terminal the question is refused, naming --force, and
// nothing changes. A replacement keeps the store's folders, which VMs
// mount, takes out what the home folder no longer has, and leaves an
// agent the home folder has nothing of as it is.
func TestAgentImportReplaces(t *testing.T) {
	im := newImporter(t)
	home := im.Dirs.Home
	write(t, home, map[string]string{".claude/skills/a/SKILL.md": "a", ".claude/skills/b/SKILL.md": "b", ".codex/config.toml": "x"})
	if err := im.Run(t.Context(), nil, false); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	skills := filepath.Join(agentStore(im.Dirs, "claude"), ".claude/skills")
	before, err := os.Stat(skills)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(home, ".claude/skills/b")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(home, ".codex")); err != nil {
		t.Fatal(err)
	}
	write(t, home, map[string]string{".claude/skills/a/SKILL.md": "a2"})

	im.fake.Terminal, im.got = false, nil
	err = im.Run(t.Context(), nil, false)
	if _, ok := errors.AsType[*msg.Refusal](err); !ok || !strings.Contains(msg.NamesOf(err), "--force") {
		t.Fatalf("without a terminal: %v", msg.NamesOf(err))
	}
	if got := readFile(t, filepath.Join(skills, "a/SKILL.md")); got != "a" {
		t.Errorf("refused, yet replaced: %q", got)
	}

	im.fake.Terminal, im.got = true, nil
	im.fake.OnAsk = func(m msg.M) bool { im.got = append(im.got, "asked "+m.Names()); return false }
	if err := im.Run(t.Context(), nil, false); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if !slices.Equal(im.got, []string{"asked agent_import_replace agents=claude", "nothing_imported"}) {
		t.Errorf("no: said %q", im.got)
	}

	im.got = nil
	if err := im.Run(t.Context(), nil, true); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if got := readFile(t, filepath.Join(skills, "a/SKILL.md")); got != "a2" {
		t.Errorf("replaced = %q", got)
	}
	if _, err := os.Lstat(filepath.Join(skills, "b")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a folder the home folder lost stays: %v", err)
	}
	if after, err := os.Stat(skills); err != nil || !os.SameFile(before, after) {
		t.Errorf("the mounted folder was made again: %v", err)
	}
	if got := readFile(t, filepath.Join(agentStore(im.Dirs, "codex"), ".codex/config.toml")); got != "x" {
		t.Errorf("an agent the home folder has nothing of changed: %q", got)
	}
	if slices.ContainsFunc(im.got, func(s string) bool { return strings.HasPrefix(s, "asked") }) {
		t.Errorf("asked with force: %q", im.got)
	}
}

// An import never writes devvm's agent store through a link, which only
// a hand-made store could have: a link anywhere on the way to an agent's
// folder in the store, from devvm's config folder down, fails the import,
// naming it, before anything is written or taken out, so the folder the
// link leads to keeps what it has, and the store has none of the import,
// a settings file above a linked settings folder included.
func TestAgentImportLinkedStore(t *testing.T) {
	for _, at := range []string{"agents", "agents/claude", "agents/claude/.claude", "agents/claude/.claude/skills", "agents/claude/.claude/skills/s"} {
		im := newImporter(t)
		home := im.Dirs.Home
		write(t, home, map[string]string{".claude/skills/s/SKILL.md": "# s", ".claude/settings.json": "{}"})
		victim := t.TempDir()
		write(t, victim, map[string]string{"keep.txt": "mine", ".claude/keep.txt": "mine", "claude/.claude/keep.txt": "mine"})
		link := filepath.Join(im.Dirs.Config, filepath.FromSlash(at))
		if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(victim, link); err != nil {
			t.Fatal(err)
		}
		err := im.Run(t.Context(), []string{"claude"}, true)
		if want := "agent_store_link link=" + link; msg.NamesOf(err) != want {
			t.Errorf("a link at %s: %v, want %s", at, msg.NamesOf(err), want)
		}
		if _, err := os.Lstat(filepath.Join(agentStore(im.Dirs, "claude"), ".claude/settings.json")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a link at %s: the store changed: %v", at, err)
		}
		for _, f := range []string{"keep.txt", ".claude/keep.txt", "claude/.claude/keep.txt"} {
			if got := readFile(t, filepath.Join(victim, filepath.FromSlash(f))); got != "mine" {
				t.Errorf("a link at %s: %s = %q", at, f, got)
			}
		}
		es, err := os.ReadDir(victim)
		if err != nil {
			t.Fatal(err)
		}
		if len(es) != 3 {
			t.Errorf("a link at %s: written through it: %v", at, es)
		}
	}
}

// With more than one agent, a link on the way to any one's store fails the
// import before it changes any agent's: Claude Code's store stays empty
// when Codex's is a link.
func TestAgentImportLinkedStoreOfAnother(t *testing.T) {
	im := newImporter(t)
	write(t, im.Dirs.Home, map[string]string{".claude/settings.json": "{}", ".codex/config.toml": "x"})
	victim := t.TempDir()
	link := agentStore(im.Dirs, "codex")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}
	err := im.Run(t.Context(), []string{"claude", "codex"}, true)
	if want := "agent_store_link link=" + link; msg.NamesOf(err) != want {
		t.Errorf("err = %v, want %s", msg.NamesOf(err), want)
	}
	if _, err := os.Lstat(agentStore(im.Dirs, "claude")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Claude Code's store changed: %v", err)
	}
}

// pruneStore and clearKind reach the store from devvm's config folder,
// never through a link, so what a link leads to keeps everything.
func TestStoreOpsRefuseLinks(t *testing.T) {
	config := t.TempDir()
	victim := t.TempDir()
	write(t, victim, map[string]string{"keep.txt": "mine", "x/y": "mine"})
	store := filepath.Join(config, "agents", "claude")
	if err := os.MkdirAll(filepath.Dir(store), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, store); err != nil {
		t.Fatal(err)
	}
	if _, ok := errors.AsType[storeLink](pruneStore(config, store, map[string]bool{})); !ok {
		t.Error("pruneStore: no storeLink")
	}
	if _, ok := errors.AsType[storeLink](clearKind(config, store, "x", false)); !ok {
		t.Error("clearKind: no storeLink")
	}
	if r, err := makeStoreDir(config, filepath.Join(store, ".claude")); err == nil {
		_ = r.Close()
		t.Error("makeStoreDir: made through a link")
	} else if _, ok := errors.AsType[storeLink](err); !ok {
		t.Errorf("makeStoreDir: %v, not a storeLink", err)
	}
	for _, f := range []string{"keep.txt", "x/y"} {
		if got := readFile(t, filepath.Join(victim, f)); got != "mine" {
			t.Errorf("%s = %q", f, got)
		}
	}
	if _, err := os.Lstat(filepath.Join(victim, ".claude")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("made in the folder the link leads to: %v", err)
	}
	// Made where nothing is, and opened: a write through the root lands in the store.
	other := filepath.Join(config, "agents", "codex", ".codex")
	r, err := makeStoreDir(config, other)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	if err := state.WriteFileIn(r, "config.toml", []byte("x"), 0o600); err != nil || readFile(t, filepath.Join(other, "config.toml")) != "x" {
		t.Errorf("a write through it: %v", err)
	}
}

// With nothing to import, it says so: for each agent named, or once.
func TestAgentImportNothing(t *testing.T) {
	im := newImporter(t)
	if err := im.Run(t.Context(), []string{"codex", "opencode"}, false); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(im.got, []string{"agent_import_none agent=codex", "agent_import_none agent=opencode"}) {
		t.Errorf("named: said %q", im.got)
	}
	im.got = nil
	if err := im.Run(t.Context(), nil, false); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(im.got, []string{"agent_import_nothing"}) {
		t.Errorf("none named: said %q", im.got)
	}
}

// An agent's folder that holds more settings than one import copies, more
// than 128 MiB of files or more than 20,000 files and folders, fails the
// import before the store changes, naming the folder (the other tests
// import folders within them).
func TestAgentImportTooMuch(t *testing.T) {
	for _, tc := range []struct {
		name string
		fill func(t *testing.T, skills string)
		want string
	}{
		{"more than 128 MiB", func(t *testing.T, skills string) {
			sparse(t, filepath.Join(skills, "big", "a"), saveMaxBytes/2)
			sparse(t, filepath.Join(skills, "big", "b"), saveMaxBytes/2+1)
		}, "agent_import_too_much"},
		{"more than 20,000 entries", func(t *testing.T, skills string) {
			for i := range saveMaxEntries {
				sparse(t, filepath.Join(skills, "many", strconv.Itoa(i/1000), strconv.Itoa(i)), 0)
			}
		}, "agent_import_too_much"},
	} {
		im := newImporter(t)
		home := im.Dirs.Home
		write(t, home, map[string]string{".claude/settings.json": "{}"})
		tc.fill(t, filepath.Join(home, ".claude", "skills"))
		err := im.Run(t.Context(), []string{"claude"}, true)
		if msg.IDOf(err) != tc.want {
			t.Errorf("%s: %v, want %q", tc.name, msg.NamesOf(err), tc.want)
		}
		_, serr := os.Lstat(filepath.Join(agentStore(im.Dirs, "claude"), ".claude", "settings.json"))
		if imported := serr == nil; imported != (tc.want == "") {
			t.Errorf("%s: imported %v", tc.name, imported)
		}
		if tc.want != "" && !strings.Contains(msg.NamesOf(err), "folder="+filepath.Join(home, ".claude")) {
			t.Errorf("%s: names %s", tc.name, msg.NamesOf(err))
		}
	}
}

// The files of an agent's folder share the 128 MiB as they're read, so
// files that grew since they were listed fail the import once together
// they pass it, naming the file; the control reads the same file.
func TestAgentImportReadBudget(t *testing.T) {
	for _, left := range []int64{5, 4} {
		im := newImporter(t)
		write(t, im.Dirs.Home, map[string]string{".claude/CLAUDE.md": "notes"})
		h, err := im.read(t.Context(), "claude", nil)
		if err != nil {
			t.Fatal(err)
		}
		h.read = saveMaxBytes - left // as if the folder's other files had grown since
		err = im.save(t.Context(), h)
		h.close()
		if fits := left == 5; fits != (err == nil) {
			t.Errorf("%d bytes left: %v", left, msg.NamesOf(err))
		}
		if left == 4 && (msg.IDOf(err) != "agent_import_read_failed" || !errors.Is(err, safefile.ErrTooLong)) {
			t.Errorf("%d bytes left: %v", left, msg.NamesOf(err))
		}
	}
}

// sparse makes a file of size bytes at p, which takes no room on the disk.
func sparse(t *testing.T, p string, size int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(p) //nolint:gosec // the test's own sandbox
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// An import warns of each VM whose project folder is inside an agent's
// folder it reads, as that VM can change what the import copies for every
// VM; a VM whose folder is elsewhere gets no warning.
func TestAgentImportProjectInside(t *testing.T) {
	im := newImporter(t)
	home := im.Dirs.Home
	write(t, home, map[string]string{".claude/settings.json": "{}", ".claude/skills/mine/SKILL.md": "# s", "src/app/x": ""})
	inside := paths.Physical(filepath.Join(home, ".claude", "skills", "mine")) // as a state records it
	vmState(t, im.Dirs, "devvm-mine-0123456789abc4", func(st *state.VM) error { st.Dir = inside; return nil })
	vmState(t, im.Dirs, "devvm-app-0123456789abc3", func(st *state.VM) error { st.Dir = filepath.Join(home, "src", "app"); return nil })
	if err := im.Run(t.Context(), []string{"claude"}, true); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	want := "agent_import_project_inside dir=" + inside + " folder=" + filepath.Join(home, ".claude") + " vm=devvm-mine-0123456789abc4"
	var warned []string
	for _, g := range im.got {
		if strings.HasPrefix(g, "agent_import_project_inside") {
			warned = append(warned, g)
		}
	}
	if !slices.Equal(warned, []string{want}) {
		t.Errorf("warned %q, want %q", warned, want)
	}
}

// A VM whose project folder holds an agent's folder gets a warning too,
// and when a link in that project folder leads the agent's folder out of
// it, as one to the home folder in pi's place, which the VM could have
// chosen, the import leaves the folder out: nothing it leads to reaches
// the store.
func TestAgentImportProjectHolds(t *testing.T) {
	im := newImporter(t)
	home := im.Dirs.Home
	write(t, home, map[string]string{".pi/agent/settings.json": "{}", "AGENTS.md": "the host's own", "skills/x/SKILL.md": "the host's"})
	project := paths.Physical(filepath.Join(home, ".pi"))
	vmState(t, im.Dirs, "devvm-pi-0123456789abc4", func(st *state.VM) error { st.Dir = project; return nil })
	if err := im.Run(t.Context(), []string{"pi"}, true); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	folder := filepath.Join(home, ".pi", "agent")
	if want := "agent_import_project_inside dir=" + project + " folder=" + folder + " vm=devvm-pi-0123456789abc4"; !slices.Contains(im.got, want) {
		t.Errorf("said %q, want %q", im.got, want)
	}
	// The VM swaps pi's folder for a link to the home folder.
	if err := os.RemoveAll(folder); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(home, folder); err != nil {
		t.Fatal(err)
	}
	im.got = nil
	if err := im.Run(t.Context(), []string{"pi"}, true); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if want := "agent_import_link_via_vm path=" + folder + " vm=devvm-pi-0123456789abc4"; !slices.Contains(im.got, want) {
		t.Errorf("said %q, want %q", im.got, want)
	}
	for _, f := range []string{"AGENTS.md", "skills"} {
		if _, err := os.Lstat(filepath.Join(agentStore(im.Dirs, "pi"), ".pi", "agent", f)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the home folder's %s reached the store: %v", f, err)
		}
	}
	// Led to Codex's folder instead, its login isn't taken for pi's.
	write(t, home, map[string]string{".codex/auth.json": `{"codex": "CODEX LOGIN"}`})
	if err := os.Remove(folder); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, ".codex"), folder); err != nil {
		t.Fatal(err)
	}
	if err := im.Run(t.Context(), []string{"pi"}, true); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if im.Secrets.HasLogin("pi") {
		t.Error("another agent's login saved as pi's")
	}
}

// A login file, at the path it resolved to, is read there without
// following a link, as a regular file: one that a link or a FIFO took the
// place of fails, and so does one whose folder a link took the place of
// since its path was resolved, never read where that link leads.
func TestAgentImportLoginNoFollow(t *testing.T) {
	im := newImporter(t)
	home := paths.Physical(im.Dirs.Home)
	write(t, home, map[string]string{".codex/config.toml": "x", "elsewhere.json": `{"other": "another file"}`, ".pi/agent/auth.json": `{"pi": "pi's"}`})
	if _, err := claudeHostLogin("codex", filepath.Join(home, "elsewhere.json")); err != nil {
		t.Fatalf("a regular file: %v", err)
	}
	piLogin := filepath.Join(home, ".pi", "agent", "auth.json") // resolved, then its folder swapped
	if err := os.RemoveAll(filepath.Join(home, ".pi", "agent")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, ".codex"), filepath.Join(home, ".pi", "agent")); err != nil {
		t.Fatal(err)
	}
	write(t, home, map[string]string{".codex/auth.json": `{"codex": "codex's"}`})
	if got, err := claudeHostLogin("pi", piLogin); err == nil {
		t.Errorf("read through a folder swapped for a link: %q", got)
	}
	if err := os.Remove(filepath.Join(home, ".codex", "auth.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "elsewhere.json"), filepath.Join(home, ".codex", "auth.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := claudeHostLogin("codex", filepath.Join(home, ".codex", "auth.json")); err == nil {
		t.Error("read through a link")
	}
	fifo := filepath.Join(home, ".codex", "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := claudeHostLogin("codex", fifo); err == nil {
		t.Error("a FIFO read")
	}
}

// A login devvm can't read fails the import, naming the file.
func TestAgentImportBadLogin(t *testing.T) {
	im := newImporter(t)
	write(t, im.Dirs.Home, map[string]string{".claude.json": "not json"})
	err := im.Run(t.Context(), []string{"claude"}, false)
	if names := msg.NamesOf(err); !strings.HasPrefix(names, "agent_login_read_failed file="+filepath.Join(im.Dirs.Home, ".claude.json")) {
		t.Errorf("err = %s", names)
	}
}
