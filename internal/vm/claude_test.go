package vm

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/agents"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
)

// Claude Code's plugins folder in a VM is the VM's own, where Claude Code writes each plugin's
// data, its installs and its marketplaces' refreshes, and the store's plugins are its read-only
// seed beside it: every shell's environment names both (Claude Code's own
// CLAUDE_CODE_PLUGIN_CACHE_DIR and CLAUDE_CODE_PLUGIN_SEED_DIR), the store's folder is linked at
// the seed's path, and a link at the plugins folder's, as a VM from before has, is replaced by a
// folder of the VM's own.
func TestClaudePluginsSeed(t *testing.T) {
	env := string(agentEnv())
	for _, line := range []string{
		`export CLAUDE_CODE_PLUGIN_CACHE_DIR="$HOME"/'.claude/plugins'` + "\n",
		`export CLAUDE_CODE_PLUGIN_SEED_DIR="$HOME"/'.claude/plugins-seed'` + "\n",
	} {
		if !strings.Contains(env, line) {
			t.Errorf("agent-env lacks %q:\n%s", line, env)
		}
	}
	d := paths.Dirs{Config: t.TempDir()}
	store := agentStore(d, agents.Claude)
	mkdirs(t, store, ".claude/plugins", ".claude/skills")
	s := &stage{}
	links, err := (syncer{Env: &Env{Dirs: d}}).stageAgents(s, []string{"claude"})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(links), "\n"), "\n")
	var rels []string
	for i := 0; i+1 < len(lines); i += 2 {
		rels = append(rels, lines[i])
		if lines[i] == ".claude/plugins-seed" && lines[i+1] != filepath.Join(store, ".claude/plugins") {
			t.Errorf("the seed links to %s", lines[i+1])
		}
	}
	if !slices.Contains(rels, ".claude/plugins-seed") || slices.Contains(rels, ".claude/plugins") {
		t.Errorf("links = %q", links)
	}
	if i := s.index("own-dirs"); i < 0 || string(s.files[i].data) != ".claude/plugins\n" {
		t.Errorf("own-dirs isn't .claude/plugins: %+v", s.files)
	}
}

// A VM's own plugins folder is its own: a remake keeps it, as it keeps the rest of the home,
// but not the seed's link, which the new VM's setup makes again. An export of its history leaves
// it out, and an import leaves out both, as each VM gets the store's plugins from the seed again
// and a plugin reaches other VMs only through devvm agent save.
func TestClaudePluginsHistory(t *testing.T) {
	skips := homeSkips(paths.Dirs{Config: t.TempDir()}, []string{"claude"})
	if skipped(".claude/plugins/data/p/state.json", skips) || !skipped(".claude/plugins-seed", skips) {
		t.Errorf("home skips = %q", skips)
	}
	if !strings.Contains(historyExport(), "'--exclude=.claude/plugins'") {
		t.Errorf("the export keeps the plugins folder:\n%s", historyExport())
	}
	for _, rel := range []string{".claude/plugins", ".claude/plugins-seed"} {
		if !slices.Contains(importSkips(), rel) {
			t.Errorf("an import takes %s: %q", rel, importSkips())
		}
	}
}

// claudePluginsHost is a plugins folder as Claude Code keeps it on this machine or in a VM: what
// a seed has (its marketplaces, their records and the plugins' cache), and what each machine
// keeps of its own (each plugin's data, its MCP bundles, the plugins claude.ai synced, its
// trash, its plugins' store, the plugins it installed and flagged).
func claudePluginsHost(t *testing.T, root, home string) {
	t.Helper()
	write(t, root, map[string]string{
		"known_marketplaces.json":            `{"m":{"installLocation":"` + home + `/.claude/plugins/marketplaces/m"}}`,
		"marketplaces/m/marketplace.json":    `{"name":"m"}`,
		"cache/m/p/1.0.0/SKILL.md":           "# p",
		"installed_plugins.json":             `{"version":2,"plugins":{}}`,
		"flagged-plugins.json":               `{}`,
		"known_marketplaces_claudeai.json":   `{}`,
		"data/p-m/state.json":                `{"token":"testfake-plugin-data"}`,
		"mcpb/b/manifest.json":               `{}`,
		"synced/s/plugin.json":               `{}`,
		".trash/t/plugin.json":               `{}`,
		"store/s.json":                       `{}`,
		"installed_plugins.set-aside.x.json": `{}`,
	})
}

// devvm agent import copies of this machine's plugins only what a VM reads as its seed, whether
// the plugins folder is in ~/.claude or a link out of it leads to it, so no VM gets this
// machine's plugins' data.
func TestAgentImportClaudePlugins(t *testing.T) {
	for _, linked := range []bool{false, true} {
		im := newImporter(t)
		home := im.Dirs.Home
		write(t, home, map[string]string{".claude/settings.json": "{}"})
		at := filepath.Join(home, ".claude/plugins")
		if linked {
			at = filepath.Join(home, "dotfiles/plugins")
			im.FollowLinks = true
			if err := os.MkdirAll(filepath.Dir(at), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(at, filepath.Join(home, ".claude/plugins")); err != nil {
				t.Fatal(err)
			}
		}
		claudePluginsHost(t, at, home)
		if err := im.Run(t.Context(), []string{"claude"}, false); err != nil {
			t.Fatal(msg.NamesOf(err))
		}
		plugins := filepath.Join(agentStore(im.Dirs, "claude"), ".claude/plugins")
		var got []string
		err := filepath.WalkDir(plugins, func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				rel, _ := filepath.Rel(plugins, p)
				got = append(got, filepath.ToSlash(rel))
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		slices.Sort(got)
		want := []string{"cache/m/p/1.0.0/SKILL.md", "known_marketplaces.json", "marketplaces/m/marketplace.json"}
		if !slices.Equal(got, want) {
			t.Errorf("linked %v: the store's plugins = %q, want %q", linked, got, want)
		}
		if k := readFile(t, filepath.Join(plugins, "known_marketplaces.json")); !strings.Contains(k, `"`+plugins+`/marketplaces/m"`) {
			t.Errorf("linked %v: known_marketplaces.json = %s", linked, k)
		}
	}
}

// A store that devvm 0.1.0's agent import filled holds this machine's whole plugins folder, its
// plugins' data included, which every VM with Claude Code mounts as its seed. A start takes out
// all but what a seed has, says so once, and leaves the folder itself, so no VM's mounts change.
// A link among its entries goes, never what it leads to, and a store whose plugins folder is a
// link, which only a hand-made store has, is left as it is.
func TestCleanSeeds(t *testing.T) {
	d := paths.Dirs{Config: t.TempDir(), Data: t.TempDir()}
	plugins := filepath.Join(agentStore(d, agents.Claude), ".claude/plugins")
	claudePluginsHost(t, plugins, "/home/u")
	outside := t.TempDir()
	write(t, outside, map[string]string{"data/p/token.json": "keep"})
	if err := os.Symlink(filepath.Join(outside, "data"), filepath.Join(plugins, "linked")); err != nil {
		t.Fatal(err)
	}
	// devvm agent import writing a seed's entry at the same time: its temporary file stays.
	write(t, plugins, map[string]string{".known_marketplaces.json.tmp-123": "{}"})
	var warned msg.Recorder
	e := &Env{Dirs: d, Reporter: &msg.Fake{OnWarn: warned.Add}}
	if err := e.cleanSeeds(t.Context(), []string{"claude"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(plugins, ".known_marketplaces.json.tmp-123")); err != nil {
		t.Errorf("the import's temporary file went: %v", err)
	}
	want := []string{"agent_seed_cleaned agent=Claude Code dir=" + plugins + " entries=.trash, data, flagged-plugins.json, " +
		"installed_plugins.json, installed_plugins.set-aside.x.json, known_marketplaces_claudeai.json, linked, mcpb, store, synced"}
	if !slices.Equal(warned.Names(), want) {
		t.Errorf("warned %q, want %q", warned.Names(), want)
	}
	var got []string
	err := filepath.WalkDir(plugins, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(plugins, p)
			got = append(got, filepath.ToSlash(rel))
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	if want := []string{"cache/m/p/1.0.0/SKILL.md", "known_marketplaces.json", "marketplaces/m/marketplace.json"}; !slices.Equal(got, want) {
		t.Errorf("the store's plugins = %q, want %q", got, want)
	}
	if readFile(t, filepath.Join(outside, "data/p/token.json")) != "keep" {
		t.Error("the link's target went")
	}
	warned.Reset()
	if err := e.cleanSeeds(t.Context(), []string{"claude"}); err != nil || len(warned.Names()) > 0 {
		t.Errorf("again: %v, warned %q", err, warned.Names())
	}

	// No store at all.
	e = &Env{Dirs: paths.Dirs{Config: t.TempDir(), Data: t.TempDir()}, Reporter: &msg.Fake{OnWarn: warned.Add}}
	if err := e.cleanSeeds(t.Context(), []string{"claude"}); err != nil || len(warned.Names()) > 0 {
		t.Errorf("no store: %v, warned %q", err, warned.Names())
	}
}

// A store the start can't clean fails the start of a VM with the agent, which would read what's
// left, and only warns for a VM without it: a link anywhere on the seed folder's path below
// devvm's config folder, the store's own included, which the VM's mount would follow to this
// machine's own plugins folder, whose data stays; and an entry that can't be taken out, after
// what could be is taken out and named.
func TestCleanSeedsFails(t *testing.T) {
	for _, at := range []string{"agents/claude", "agents/claude/.claude", "agents/claude/.claude/plugins"} {
		d := paths.Dirs{Config: t.TempDir(), Data: t.TempDir()}
		host := t.TempDir()
		claudePluginsHost(t, filepath.Join(host, ".claude/plugins"), host)
		target := filepath.Join(host, strings.TrimPrefix(strings.TrimPrefix(at, "agents/claude"), "/"))
		if err := os.MkdirAll(filepath.Dir(filepath.Join(d.Config, at)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(d.Config, at)); err != nil {
			t.Fatal(err)
		}
		var warned msg.Recorder
		e := &Env{Dirs: d, Reporter: &msg.Fake{OnWarn: warned.Add}}
		err := e.cleanSeeds(t.Context(), []string{"claude"})
		if want := "agent_seed_link agent=Claude Code dir=" + filepath.Join(agentStore(d, agents.Claude), ".claude/plugins") +
			" keep=known_marketplaces.json, marketplaces, cache link=" + filepath.Join(d.Config, at); msg.NamesOf(err) != want || len(warned.Names()) > 0 {
			t.Errorf("a link at %s: %v, warned %q", at, msg.NamesOf(err), warned.Names())
		}
		if err := e.cleanSeeds(t.Context(), []string{"codex"}); err != nil || !slices.Equal(warned.IDs(), []string{"agent_seed_link"}) {
			t.Errorf("a link at %s, a VM without Claude Code: %v, warned %q", at, err, warned.Names())
		}
		if readFile(t, filepath.Join(host, ".claude/plugins/data/p-m/state.json")) == "" {
			t.Errorf("a link at %s: this machine's plugins' data went", at)
		}
	}

	if os.Geteuid() == 0 {
		t.Skip("root removes what it can't write")
	}
	d := paths.Dirs{Config: t.TempDir(), Data: t.TempDir()}
	plugins := filepath.Join(agentStore(d, agents.Claude), ".claude/plugins")
	write(t, plugins, map[string]string{"data/p/state.json": "{}", "installed_plugins.json": "{}", "mcpb/b/m.json": "{}"})
	locked := filepath.Join(plugins, "mcpb/b")
	if err := os.Chmod(locked, 0o500); err != nil { //nolint:gosec // G302: a folder, read-only to its owner, so its entry can't go
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) }) //nolint:gosec // G302: a folder, writable again for its cleanup
	var warned msg.Recorder
	e := &Env{Dirs: d, Reporter: &msg.Fake{OnWarn: warned.Add}}
	err := e.cleanSeeds(t.Context(), []string{"claude"})
	if msg.IDOf(err) != "agent_seed_clean_failed" {
		t.Errorf("an entry that can't go: %v", msg.NamesOf(err))
	}
	if want := []string{"agent_seed_cleaned agent=Claude Code dir=" + plugins + " entries=data, installed_plugins.json"}; !slices.Equal(warned.Names(), want) {
		t.Errorf("warned %q, want %q", warned.Names(), want)
	}
}

// devvm agent save's script sends of the VM's own plugins folder only what a seed has, never
// what's the VM's own, such as a plugin's data (run with this machine's bash and tar).
func TestAgentSaveScriptClaudePlugins(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("no jq")
	}
	home := t.TempDir()
	claudePluginsHost(t, filepath.Join(home, ".claude/plugins"), home)
	cmd := exec.CommandContext(t.Context(), "bash", "-c", agentSaveScript([]string{"claude"})) //nolint:gosec // devvm's own script
	cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "COPYFILE_DISABLE=1"}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%v: %s", err, stderr.String())
	}
	s, err := readSent(bytes.NewReader(out), "vm", []string{"claude"})
	if err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	var names []string
	for n := range s.entries {
		names = append(names, n)
	}
	slices.Sort(names)
	want := []string{
		".claude/plugins", ".claude/plugins/cache", ".claude/plugins/cache/m", ".claude/plugins/cache/m/p", ".claude/plugins/cache/m/p/1.0.0",
		".claude/plugins/cache/m/p/1.0.0/SKILL.md", ".claude/plugins/known_marketplaces.json", ".claude/plugins/marketplaces",
		".claude/plugins/marketplaces/m", ".claude/plugins/marketplaces/m/marketplace.json",
	}
	if !slices.Equal(names, want) {
		t.Errorf("entries = %q, want %q", names, want)
	}
	if bytes.Contains(out, []byte("testfake")) {
		t.Error("the stream holds a plugin's data")
	}
	// An entry whose name has a wildcard's character is never a pattern of the script's: it's
	// sent, and the host refuses the save.
	write(t, home, map[string]string{".claude/plugins/*": "x"})
	cmd = exec.CommandContext(t.Context(), "bash", "-c", agentSaveScript([]string{"claude"})) //nolint:gosec // devvm's own script
	cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "COPYFILE_DISABLE=1"}
	if out, err = cmd.Output(); err != nil {
		t.Fatal(err)
	}
	if _, err := readSent(bytes.NewReader(out), "vm", []string{"claude"}); msg.IDOf(err) != "agent_save_unsafe" {
		t.Errorf("readSent of a stream with .claude/plugins/* = %v", msg.NamesOf(err))
	}
}

// A stream with anything of the VM's plugins folder but what a seed has, such as a plugin's
// data or the plugins it installed, saves nothing: the VM's own script never sends it.
func TestReadSentClaudePlugins(t *testing.T) {
	for _, name := range []string{
		".claude/plugins/data/", ".claude/plugins/installed_plugins.json", ".claude/plugins/mcpb/", ".claude/plugins/.trash/",
	} {
		stream := saveTar(t, true, te{name: ".claude/plugins/"}, te{name: name})
		_, err := readSent(bytes.NewReader(stream), "vm", []string{"claude"})
		if msg.IDOf(err) != "agent_save_unsafe" {
			t.Errorf("%s: readSent = %v", name, msg.NamesOf(err))
		}
	}
	stream := saveTar(t, true, te{name: ".claude/plugins/"}, te{name: ".claude/plugins/cache/"}, te{name: ".claude/plugins/known_marketplaces.json", data: "{}"})
	if _, err := readSent(bytes.NewReader(stream), "vm", []string{"claude"}); err != nil {
		t.Errorf("readSent = %v", msg.NamesOf(err))
	}
}

// A save of the VM's own plugins folder adds and changes the store's plugins and never takes one
// out, as the VM's folder holds only what it installed and its seed the rest: the store's
// marketplaces' records keep theirs, the VM's own join them, each naming the store's folders, and
// those Claude Code registered from the seed (s, at the seed's link, and the store's m) are
// left as the store has them, as is a record whose copy isn't in the store's marketplaces
// folder, by a clean path (d, a folder of the VM's, e, and f, whose key in another case is
// none Claude Code reads). Saved, the same folder is no change. A records file that isn't
// JSON is left out, with a warning.
func TestAgentSaveClaudePlugins(t *testing.T) {
	stream := saveTar(t, true,
		te{name: ".claude/plugins/"},
		te{name: ".claude/plugins/known_marketplaces.json", data: `{
  "v": {"installLocation": "/home/u.linux/.claude/plugins/marketplaces/v"},
  "m": {"installLocation": "/home/u.linux/.claude/plugins-seed/marketplaces/m", "autoUpdate": false},
  "s": {"installLocation": "/home/u.linux/.claude/plugins-seed/marketplaces/s", "autoUpdate": false},
  "d": {"source": {"source": "directory", "path": "/srv/p/mkt"}, "installLocation": "/srv/p/mkt"},
  "e": {"installLocation": "/home/u.linux/.claude/plugins/marketplaces/../../../../x"},
  "f": {"installLocation": "/srv/f", "InstallLocation": "/home/u.linux/.claude/plugins/marketplaces/f"}
}`},
		te{name: ".claude/plugins/cache/"},
		te{name: ".claude/plugins/cache/v/"},
		te{name: ".claude/plugins/cache/v/SKILL.md", data: "# v"},
	)
	st := newSaveTest(t, stream, "claude")
	store := st.store("claude")
	plugins := filepath.Join(store, ".claude/plugins")
	write(t, store, map[string]string{
		".claude/plugins/known_marketplaces.json":  `{"m": {"installLocation": "` + plugins + `/marketplaces/m"}}`,
		".claude/plugins/cache/m/SKILL.md":         "# m",
		".claude/plugins/marketplaces/m/README.md": "m",
	})
	p, err := st.plan("claude")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.folders) != 1 {
		t.Fatalf("folders = %+v", p.folders)
	}
	f := p.folders[0]
	if !slices.Equal(f.added, []string{"cache/v/SKILL.md"}) || !slices.Equal(f.changed, []string{"known_marketplaces.json"}) || len(f.removed) != 0 {
		t.Errorf("plugins = added %q, changed %q, removed %q", f.added, f.changed, f.removed)
	}
	if err := st.applyAll(t.Context(), []agentPlan{p}); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"cache/m/SKILL.md", "marketplaces/m/README.md", "cache/v/SKILL.md"} {
		if _, err := os.Stat(filepath.Join(plugins, rel)); err != nil {
			t.Errorf("%s: %v", rel, err)
		}
	}
	k := readFile(t, filepath.Join(plugins, "known_marketplaces.json"))
	for _, m := range []string{"m", "v"} {
		if !strings.Contains(k, `"installLocation": "`+plugins+`/marketplaces/`+m+`"`) {
			t.Errorf("no %s naming the store's folder in %s", m, k)
		}
	}
	if strings.Contains(k, "u.linux") || strings.Contains(k, `"s"`) || strings.Contains(k, "autoUpdate") || strings.Contains(k, `"d"`) || strings.Contains(k, `"e"`) || strings.Contains(k, `"f"`) {
		t.Errorf("a VM's path, a record from the seed, or one whose copy isn't the store's, in %s", k)
	}
	again := newSaveTest(t, stream, "claude")
	again.Dirs = st.Dirs
	if p, err := again.plan("claude"); err != nil || len(p.folders) != 0 {
		t.Errorf("saved again: %+v, %v", p.folders, err)
	}

	bad := newSaveTest(t, saveTar(t, true, te{name: ".claude/plugins/"}, te{name: ".claude/plugins/known_marketplaces.json", data: "{"}), "claude")
	write(t, bad.store("claude"), map[string]string{".claude/plugins/known_marketplaces.json": "{}"})
	p, err = bad.plan("claude")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.folders) != 0 || !slices.Contains(bad.got, "agent_save_unparsed path=.claude/plugins/known_marketplaces.json vm=vm") {
		t.Errorf("folders %+v, said %q", p.folders, bad.got)
	}
}
