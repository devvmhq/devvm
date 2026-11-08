package vm

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/agents"
	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/secrets"
)

// pi's folders, as the tests name them.
const (
	piDir      = agents.PiFolder
	piSessions = agents.PiFolder + "/sessions"
)

// golden compares got with testdata/golden/name, or writes it there with UPDATE_GOLDEN=1 in the
// test's environment: UPDATE_GOLDEN=1 go test ./internal/vm -run Golden.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	p := filepath.Join("testdata", "golden", name)
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, got, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(p) //nolint:gosec // the test's own golden file
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s changed:\n--- got\n%s\n--- want\n%s", name, got, want)
	}
}

// What devvm does with each agent's files, as text: its folder, settings folders and files (with
// a store whose opencode config folder has entries of each kind), each settings file's format,
// which files are withheld, which paths a save takes, the store's login files, and the scripts
// that export, import and save the agents' history and settings.
func TestAgentsGolden(t *testing.T) {
	store := t.TempDir()
	for _, p := range []string{".config/opencode/agent", ".config/opencode/node_modules", ".config/opencode/plugin"} {
		if err := os.MkdirAll(filepath.Join(store, p), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"opencode.json", "tui.jsonc", "x.toml", "AGENTS.md", ".env", "bun.lock", "package-lock.json", ".gitignore", ".DS_Store", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(store, ".config/opencode", f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	line := func(parts ...string) { out.WriteString(strings.Join(parts, " ") + "\n") }
	for _, a := range image.Agents {
		line("agent", a, "folder", agents.Of(a).Folder)
		line("  dirs", strings.Join(agentDirs(store, a), " "))
		line("  files", strings.Join(agentFiles(store, a), " "))
		line("  login", secrets.LoginRel(a), secrets.LoginFile(paths.Dirs{Data: "/d", Config: "/c"}, a))
		line("  save script")
		out.WriteString(agentSaveScript([]string{a}) + "\n")
	}
	line("claude-oauth login", secrets.LoginRel("claude-oauth"), secrets.LoginFile(paths.Dirs{Data: "/d", Config: "/c"}, "claude-oauth"))
	rels := []string{
		".claude/settings.json", ".claude/CLAUDE.md", ".claude/other.json", ".claude.json", ".codex/config.toml", ".codex/AGENTS.md",
		".codex/auth.json", ".config/opencode/opencode.json", ".config/opencode/tui.jsonc", ".config/opencode/x.toml",
		".config/opencode/AGENTS.md", ".config/opencode/.env", ".config/opencode/notes.txt", ".config/opencode/plugin/x.json",
		".pi/agent/settings.json", ".pi/agent/models.json", ".pi/agent/mcp.json", ".pi/agent/keybindings.json",
		".pi/agent/AGENTS.md", ".pi/agent/auth.json", ".pi/agent/other.json",
	}
	for _, rel := range rels {
		format, ok := settingsFormat(rel)
		line("file", rel, "format", format, strconv.FormatBool(ok), "withheld", strconv.FormatBool(withheld(rel)))
		for _, dir := range []bool{false, true} {
			agent, top, folder, ok := listedPath(image.Agents, rel, dir)
			line("  listed", strconv.FormatBool(dir), agent, top, strconv.FormatBool(folder), strconv.FormatBool(ok))
		}
	}
	for _, name := range []string{".claude/skills/x/SKILL.md", ".codex/prompts", ".config/opencode/agent/a.md", ".config/opencode/node_modules/x", ".pi/agent/extensions/e.ts", ".pi/agent/sessions/s"} {
		for _, dir := range []bool{false, true} {
			agent, top, folder, ok := listedPath(image.Agents, name, dir)
			line("listed", name, strconv.FormatBool(dir), agent, top, strconv.FormatBool(folder), strconv.FormatBool(ok))
		}
	}
	line("history paths", strings.Join(agents.History(), " "))
	line("history excludes", strings.Join(agents.HistoryExcludes(), " "))
	line("import skips", strings.Join(importSkips(), " "))
	line("save script, every agent")
	out.WriteString(agentSaveScript(image.Agents) + "\n")
	line("export script")
	out.WriteString(historyExport() + "\n")
	line("import script")
	out.WriteString(historyImport() + "\n")
	golden(t, "agents.txt", out.Bytes())
}
