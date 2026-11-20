package vm

import (
	"context"
	"errors"
	"io/fs"
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
		"agent_settings_imported agent=claude dir=" + claude, "agent_settings_imported agent=codex dir=" + codex,
		"agent_settings_imported agent=opencode dir=" + opencode,
		"agent_login_imported agent=claude file=" + secrets.LoginFile(im.Dirs, "claude"),
		"agent_login_imported agent=codex file=" + secrets.LoginFile(im.Dirs, "codex"),
		"agent_login_imported agent=opencode file=" + secrets.LoginFile(im.Dirs, "opencode"),
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
		err = im.save(t.Context(), []*hostAgent{h})
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

// openStoreDir and makeStoreDir, which an import's and a save's every
// write goes through (clearKind's and pruneIn's included), reach the store
// from devvm's config folder, never through a link, so what a link leads
// to keeps everything.
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
	if r, err := openStoreDir(config, store); err == nil {
		_ = r.Close()
		t.Error("openStoreDir: opened through a link")
	} else if _, ok := errors.AsType[storeLink](err); !ok {
		t.Errorf("openStoreDir: %v, not a storeLink", err)
	}
	for _, dir := range []string{store, filepath.Join(store, ".claude")} {
		if r, err := makeStoreDir(config, dir); err == nil {
			_ = r.Close()
			t.Errorf("makeStoreDir(%s): made through a link", dir)
		} else if _, ok := errors.AsType[storeLink](err); !ok {
			t.Errorf("makeStoreDir(%s): %v, not a storeLink", dir, err)
		}
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

// An import reads the home folder under one status line, opencode's run
// included, and saves into the store under another, the logins' exclusion
// from backups included; neither shows once it's done.
func TestAgentImportStatus(t *testing.T) {
	im := newImporter(t)
	write(t, im.Dirs.Home, map[string]string{".codex/auth.json": "{}"})
	var during []string
	showing := func(what string) {
		m, _ := im.fake.Showing()
		during = append(during, what+": "+m.ID())
	}
	im.Opencode = func(context.Context, ...string) ([]byte, error) {
		showing("opencode")
		return nil, exec.ErrNotFound
	}
	im.Secrets.ExcludeFromBackup = func(context.Context, string) error {
		showing("exclude")
		return nil
	}
	if err := im.Run(t.Context(), nil, false); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if want := []string{"opencode: agent_import_reading", "exclude: agent_store_saving"}; !slices.Equal(during, want) {
		t.Errorf("showing %q, want %q", during, want)
	}
	if m, on := im.fake.Showing(); on {
		t.Errorf("still showing %s", m.Names())
	}
}

// An agent's folder that holds more settings than one import copies, more
// than 128 MiB of files, a file of more than 16 MiB, or more than 20,000
// entries of any kind (a link and a FIFO count as a file does), fails the
// import before the store changes, naming the folder. A folder at each
// bound lists whole (the other tests import folders within them). The
// folder's settings.json and skills folder are two of its entries and 2
// of its bytes.
func TestAgentImportTooMuch(t *testing.T) {
	// many fills skills with n entries: a link that leads nowhere, a FIFO,
	// and files.
	many := func(n int) func(*testing.T, string) {
		return func(t *testing.T, skills string) {
			if err := os.MkdirAll(skills, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("nowhere", filepath.Join(skills, "link")); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(filepath.Join(skills, "fifo"), 0o600); err != nil {
				t.Fatal(err)
			}
			for i := range n - 2 {
				sparse(t, filepath.Join(skills, strconv.Itoa(i)), 0)
			}
		}
	}
	// bytes fills skills with files of n bytes in all, none more than 16 MiB.
	bytes := func(n int64) func(*testing.T, string) {
		return func(t *testing.T, skills string) {
			for i := 0; n > 0; i++ {
				size := min(n, saveMaxFile)
				sparse(t, filepath.Join(skills, strconv.Itoa(i)), size)
				n -= size
			}
		}
	}
	for _, tc := range []struct {
		name string
		fill func(t *testing.T, skills string)
		fits bool
	}{
		{"128 MiB", bytes(saveMaxBytes - 2), true},
		{"a byte more than 128 MiB", bytes(saveMaxBytes - 1), false},
		{"a file of 16 MiB and a byte", func(t *testing.T, skills string) { sparse(t, filepath.Join(skills, "big"), saveMaxFile+1) }, false},
		{"20,000 entries", many(saveMaxEntries - 2), true},
		{"20,001 entries", many(saveMaxEntries - 1), false},
	} {
		im := newImporter(t)
		home := im.Dirs.Home
		write(t, home, map[string]string{".claude/settings.json": "{}"})
		tc.fill(t, filepath.Join(home, ".claude", "skills"))
		if tc.fits {
			// Listed whole, not copied: the copy's bounds are TestAgentImportReadBudget's.
			h, err := im.read(t.Context(), "claude", nil)
			if err != nil {
				t.Errorf("%s: %v", tc.name, msg.NamesOf(err))
			} else {
				h.close()
			}
			continue
		}
		err := im.Run(t.Context(), []string{"claude"}, true)
		if want := "agent_import_too_much agent=claude folder=" + filepath.Join(home, ".claude"); msg.NamesOf(err) != want {
			t.Errorf("%s: %v, want %s", tc.name, msg.NamesOf(err), want)
		}
		if _, err := os.Lstat(filepath.Join(agentStore(im.Dirs, "claude"), ".claude", "settings.json")); err == nil {
			t.Errorf("%s: imported", tc.name)
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
		err = im.save(t.Context(), []*hostAgent{h})
		h.close()
		if fits := left == 5; fits != (err == nil) {
			t.Errorf("%d bytes left: %v", left, msg.NamesOf(err))
		}
		if left == 4 && (msg.IDOf(err) != "agent_import_read_failed" || !errors.Is(err, safefile.ErrTooLong)) {
			t.Errorf("%d bytes left: %v", left, msg.NamesOf(err))
		}
	}
}

// Claude Code's plugin records name their folders in the store, a longer
// path than the home folder's, so the copy grows; what it grows counts
// against the bytes the folder has left, so the store never gets more
// than the import checked. Exactly what's left fits, one byte more fails,
// naming the folder; and a copy past 16 MiB fails whatever is left.
func TestAgentImportPluginsGrow(t *testing.T) {
	for _, over := range []int{0, 1} {
		im := newImporter(t)
		home := im.Dirs.Home
		rec := `{"a":"` + filepath.Join(home, ".claude", "plugins", "x") + `"}`
		write(t, home, map[string]string{".claude/plugins/known_marketplaces.json": rec})
		h, err := im.read(t.Context(), "claude", nil)
		if err != nil {
			t.Fatal(err)
		}
		grown := len(filepath.Join(agentStore(im.Dirs, "claude"), ".claude")) - len(filepath.Join(home, ".claude"))
		if grown <= 0 {
			t.Fatalf("the store's path is no longer than the home folder's: %d", grown)
		}
		h.read = saveMaxBytes - int64(len(rec)+grown-over)
		err = im.save(t.Context(), []*hostAgent{h})
		h.close()
		store := filepath.Join(agentStore(im.Dirs, "claude"), ".claude", "plugins", "known_marketplaces.json")
		if over == 0 {
			if err != nil || !strings.Contains(readFile(t, store), agentStore(im.Dirs, "claude")) {
				t.Errorf("exactly what's left: %v", msg.NamesOf(err))
			}
			continue
		}
		if want := "agent_import_too_much agent=claude folder=" + filepath.Join(home, ".claude"); msg.NamesOf(err) != want {
			t.Errorf("one byte more: %v, want %s", msg.NamesOf(err), want)
		}
		if _, err := os.Lstat(store); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("one byte more: written (%v)", err)
		}
	}
	h := &hostAgent{}
	if !h.grew("x", saveMaxFile, 0) || h.grew("x", saveMaxFile+1, 0) {
		t.Error("a copy of 16 MiB and one byte more: not the bound")
	}
}

// The import's two handles on an agent's folder, its root and the folder
// opened one folder at a time, must be one folder: one swapped between
// the two opens, as a VM whose project folder holds it could, fails.
func TestSameFolder(t *testing.T) {
	a, b := paths.Physical(t.TempDir()), paths.Physical(t.TempDir())
	root, err := os.OpenRoot(a)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	for dir, same := range map[string]bool{a: true, b: false} {
		safe, err := openPhysical(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := sameFolder(root, safe, a); (err == nil) != same {
			t.Errorf("%s against %s: %v", a, dir, err)
		}
		_ = safe.Close()
	}
	// The import itself: a folder swapped between the two opens fails it, naming the folder.
	im := newImporter(t)
	claude := filepath.Join(im.Dirs.Home, ".claude")
	write(t, im.Dirs.Home, map[string]string{".claude/settings.json": "{}", "other/settings.json": "{}"})
	im.betweenOpens = func() {
		if err := os.Rename(claude, claude+".old"); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(filepath.Join(im.Dirs.Home, "other"), claude); err != nil {
			t.Fatal(err)
		}
	}
	h, err := im.read(t.Context(), "claude", nil)
	if err == nil {
		h.close()
	}
	if msg.IDOf(err) != "agent_import_read_failed" || !strings.Contains(msg.NamesOf(err), "file="+claude) {
		t.Errorf("swapped between the opens: %v", msg.NamesOf(err))
	}
}

// An import interrupted while it stages puts nothing in place and leaves
// no staging folder; and an interrupted run's cleanup takes a staging
// folder out on its own, as when the run ends before the import does.
func TestAgentImportInterrupted(t *testing.T) {
	im := newImporter(t)
	home, store := im.Dirs.Home, agentStore(im.Dirs, "claude")
	write(t, store, map[string]string{".claude/CLAUDE.md": "old"})
	write(t, home, map[string]string{".claude/CLAUDE.md": "new"})
	h, err := im.read(t.Context(), "claude", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer h.close()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := im.save(ctx, []*hostAgent{h}); !errors.Is(err, context.Canceled) {
		t.Errorf("save interrupted: %v", err)
	}
	staging := func() []string {
		ms, _ := filepath.Glob(filepath.Join(store, stagePrefix+"*"))
		return ms
	}
	if got := readFile(t, filepath.Join(store, ".claude", "CLAUDE.md")); got != "old" || len(staging()) != 0 {
		t.Errorf("save interrupted: the store's file %q, staging folders %q", got, staging())
	}
	var cleanups []func(context.Context)
	im.fake.OnCleanup = func(f func(context.Context)) func() {
		cleanups = append(cleanups, f)
		return func() {}
	}
	im.fake.Interrupt = true
	s, err := im.stage(h)
	if err != nil || len(staging()) != 1 || len(cleanups) != 1 {
		t.Fatalf("stage: %v, staging folders %q, cleanups %d", err, staging(), len(cleanups))
	}
	cleanups[0](t.Context())
	if len(staging()) != 0 {
		t.Errorf("an interrupted run's cleanup left %q", staging())
	}
	s.discard()
}

// A staging folder that a killed import left isn't the agent's: the
// import doesn't ask before replacing a store that holds only that, and
// the next import of the agent takes it out, even with no settings to
// copy.
func TestAgentImportStageLeftover(t *testing.T) {
	for _, settings := range []bool{true, false} {
		im := newImporter(t)
		store := agentStore(im.Dirs, "codex")
		write(t, store, map[string]string{stagePrefix + "x/.codex/config.toml": "left"})
		files := map[string]string{".codex/auth.json": `{"OPENAI_API_KEY":"k"}`}
		if settings {
			files[".codex/config.toml"] = "model = \"x\"\n"
		}
		write(t, im.Dirs.Home, files)
		if err := im.Run(t.Context(), []string{"codex"}, false); err != nil {
			t.Fatalf("settings %v: %v", settings, msg.NamesOf(err))
		}
		for _, g := range im.got {
			if strings.HasPrefix(g, "asked ") {
				t.Errorf("settings %v: %s", settings, g)
			}
		}
		if _, err := os.Lstat(filepath.Join(store, stagePrefix+"x")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("settings %v: the leftover stays (%v)", settings, err)
		}
	}
}

// An import that fails partway, at a file that changed since it was
// listed, leaves the store as it was: none of its other files, the
// store's own kept, and no staging folder. The control: the same import
// with nothing changed replaces the store's.
func TestAgentImportFailsWhole(t *testing.T) {
	for _, change := range []bool{false, true} {
		im := newImporter(t)
		home, store := im.Dirs.Home, agentStore(im.Dirs, "claude")
		write(t, store, map[string]string{".claude/CLAUDE.md": "old", ".claude/skills/old.md": "old"})
		write(t, home, map[string]string{".claude/CLAUDE.md": "new", ".claude/skills/a.md": "a"})
		h, err := im.read(t.Context(), "claude", nil)
		if err != nil {
			t.Fatal(err)
		}
		if last := h.files[len(h.files)-1]; change {
			// Renamed aside, not removed: a removed file's inode can go to the
			// new one at once (ext4), which then is the file listed.
			p := filepath.Join(home, ".claude", filepath.FromSlash(last))
			if err := os.Rename(p, filepath.Join(home, "listed")); err != nil {
				t.Fatal(err)
			}
			write(t, home, map[string]string{".claude/" + last: "another"})
		}
		err = im.save(t.Context(), []*hostAgent{h})
		h.close()
		var got []string
		_ = filepath.WalkDir(store, func(p string, d fs.DirEntry, err error) error {
			if err == nil && p != store {
				rel, _ := filepath.Rel(store, p)
				if !d.IsDir() {
					rel += "=" + readFile(t, p)
				}
				got = append(got, filepath.ToSlash(rel))
			}
			return nil
		})
		want := []string{".claude", ".claude/CLAUDE.md=new", ".claude/skills", ".claude/skills/a.md=a"}
		if change {
			want = []string{".claude", ".claude/CLAUDE.md=old", ".claude/skills", ".claude/skills/old.md=old"}
			if msg.IDOf(err) != "agent_import_read_failed" {
				t.Errorf("changed: %v", msg.NamesOf(err))
			}
		} else if err != nil {
			t.Errorf("unchanged: %v", msg.NamesOf(err))
		}
		if !slices.Equal(got, want) {
			t.Errorf("changed %v: the store has %q, want %q", change, got, want)
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
	// A state that can't be read, whose VM's folder devvm can't check, says so, and takes no other VM's warning away.
	broken := im.Dirs.VM("devvm-gone-0123456789abc5").State()
	write(t, filepath.Dir(broken), map[string]string{filepath.Base(broken): "not json"})
	if err := im.Run(t.Context(), []string{"claude"}, true); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	want := []string{
		"agent_import_states_unread path=" + broken,
		"agent_import_project_inside dir=" + inside + " folder=" + filepath.Join(home, ".claude") + " vm=devvm-mine-0123456789abc4",
	}
	var warned []string
	for _, g := range im.got {
		if strings.HasPrefix(g, "agent_import_project_inside") || strings.HasPrefix(g, "agent_import_states_unread") {
			warned = append(warned, strings.Split(g, " why=")[0])
		}
	}
	if !slices.Equal(warned, want) {
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
