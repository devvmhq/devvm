package vm

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/secrets"
)

// importer is an AgentImport in a sandbox: a home folder of its own and
// devvm's folders, its messages' names in got, its questions answered
// yes, or refused without a terminal (tty), its exclusions from backups
// in excluded.
type importer struct {
	AgentImport
	got, excluded []string
	fake          *msg.Fake
}

func newImporter(t *testing.T) *importer {
	t.Helper()
	root := t.TempDir()
	d := paths.Dirs{Home: filepath.Join(root, "home"), Data: filepath.Join(root, "data"), Config: filepath.Join(root, "config")}
	im := &importer{}
	im.fake = &msg.Fake{Terminal: true}
	im.fake.OnAny = func(m msg.M) { im.got = append(im.got, m.Names()) }
	im.fake.OnAsk = func(m msg.M) bool { im.got = append(im.got, "asked "+m.Names()); return true }
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
		".claude/plugins/installed_plugins.json":   `{"p":{"installPath":"` + filepath.Join(home, ".claude/plugins") + `/cache/p"}}`,
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
		filepath.Join(claude, ".claude/plugins/installed_plugins.json"): `{"p":{"installPath":"` +
			filepath.Join(claude, ".claude/plugins") + `/cache/p"}}`,
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

// A link in an agent's folder is followed only to a file inside it: one
// out of the folder, relative or absolute, or to a folder, is left out
// with a warning, and so is a login file that's a link.
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
	for _, p := range []string{filepath.Join(skills, "escape"), filepath.Join(skills, "escape-abs"), filepath.Join(skills, "folder"), filepath.Join(home, ".claude.json")} {
		if !slices.Contains(im.got, "agent_import_link_left_out path="+p) {
			t.Errorf("no warning for %s: %q", p, im.got)
		}
	}
}

// opencode's folder, which import copies whole, treats its links as the
// other agents' folders do: one to a file inside is copied as that file,
// and one out of the folder, or to a folder, is left out with a warning,
// never dropped without one. A link named as a lock file or node_modules
// is no setting, so it's left out without one.
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
	for _, n := range []string{"AGENTS.md", "agents", "local"} {
		if !slices.Contains(im.got, "agent_import_link_left_out path="+filepath.Join(folder, n)) {
			t.Errorf("no warning for %s: %q", n, im.got)
		}
	}
	for _, n := range []string{"bun.lock", "node_modules", "inside.json"} {
		if slices.Contains(im.got, "agent_import_link_left_out path="+filepath.Join(folder, n)) {
			t.Errorf("a warning for %s: %q", n, im.got)
		}
	}
}

// A settings folder that is itself a link is left out with a warning,
// whether it leads out of the agent's folder or to a folder inside it.
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
		if !slices.Contains(im.got, "agent_import_link_left_out path="+filepath.Join(home, ".claude", n)) {
			t.Errorf("said %q", im.got)
		}
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

// A login devvm can't read fails the import, naming the file.
func TestAgentImportBadLogin(t *testing.T) {
	im := newImporter(t)
	write(t, im.Dirs.Home, map[string]string{".claude.json": "not json"})
	err := im.Run(t.Context(), []string{"claude"}, false)
	if names := msg.NamesOf(err); !strings.HasPrefix(names, "agent_login_read_failed file="+filepath.Join(im.Dirs.Home, ".claude.json")) {
		t.Errorf("err = %s", names)
	}
}
