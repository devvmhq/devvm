package vm

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/agentlogin"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/netpolicy"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/secrets"
)

// te is an entry of a test's tar stream: a file with its data, a folder
// (a name ending in /), a link (name->target) or, with kind, another
// kind; mode its permissions (0o644 by default).
type te struct {
	name, data string
	kind       byte
	mode       int64
}

// saveTar is a tar stream of these entries, as a VM's agentSaveScript
// sends it, its own folder first.
func saveTar(t *testing.T, own bool, es ...te) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	if own {
		es = append([]te{{name: saveOwn + "/"}, {name: saveOwn + "/home", data: "/home/u.linux\n"}}, es...)
	}
	for _, e := range es {
		h := &tar.Header{Name: e.name, Mode: 0o644, Typeflag: tar.TypeReg, Size: int64(len(e.data))}
		if e.mode != 0 {
			h.Mode = e.mode
		}
		switch {
		case e.kind != 0:
			h.Typeflag, h.Size = e.kind, 0
		case strings.HasSuffix(e.name, "/"):
			h.Typeflag, h.Size, h.Mode = tar.TypeDir, 0, 0o755
		case strings.Contains(e.name, "->"):
			name, target, _ := strings.Cut(e.name, "->")
			h.Name, h.Linkname, h.Typeflag, h.Size = name, target, tar.TypeSymlink, 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.data)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// The script reads only the agents asked for: their files and folders,
// opencode's config folder without node_modules, and for claude
// ~/.claude.json's subset, never a login or history.
func TestAgentSaveScript(t *testing.T) {
	codex := agentSaveScript([]string{"codex"})
	for _, s := range []string{"'.codex/config.toml'", "'.codex/skills'", `tar -cf - "$@" -C "$t" .devvm-save`} {
		if !strings.Contains(codex, s) {
			t.Errorf("no %s in:\n%s", s, codex)
		}
	}
	for _, s := range []string{".claude", "jq", "opencode", "auth.json"} {
		if strings.Contains(codex, s) {
			t.Errorf("%s in codex's:\n%s", s, codex)
		}
	}
	all := agentSaveScript([]string{"claude", "codex", "opencode"})
	for _, s := range []string{"'.claude/plugins'", "jq --argjson k", `"mcpServers"`, ".config/opencode/*", "!= node_modules"} {
		if !strings.Contains(all, s) {
			t.Errorf("no %s in:\n%s", s, all)
		}
	}
	for _, s := range []string{"credentials", "auth.json", "projects", "history"} {
		if strings.Contains(all, s) {
			t.Errorf("%s in:\n%s", s, all)
		}
	}
}

// The script, run by bash with this machine's tar and jq as the VM's
// (skipped without jq): its stream reads back with the agents' settings,
// ~/.claude.json's subset and the home folder, never a login or history.
func TestAgentSaveScriptRuns(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("no jq")
	}
	home := t.TempDir()
	write(t, home, map[string]string{ //nolint:gosec // fake files, no real credentials
		".claude/settings.json":                    `{"theme":"dark"}`,
		".claude/skills/s/SKILL.md":                "# s",
		".claude/.credentials.json":                "testfake-credentials",
		".claude/projects/p/h.jsonl":               "testfake-history",
		".claude.json":                             `{"theme":"dark","projects":{"/p":{"history":["testfake-history"]}}}`,
		".config/opencode/opencode.json":           `{}`,
		".config/opencode/node_modules/x/index.js": "x",
		".codex/auth.json":                         "testfake-auth",
		".pi/agent/models.json":                    `{}`,
		".pi/agent/auth.json":                      "testfake-pi-auth",
		".pi/agent/sessions/--p--/s.jsonl":         "testfake-pi-history",
	})
	cmd := exec.CommandContext(t.Context(), "bash", "-c", agentSaveScript([]string{"claude", "codex", "opencode", "pi"})) //nolint:gosec // devvm's own script
	cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "COPYFILE_DISABLE=1"}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%v: %s", err, stderr.String())
	}
	s, err := readSent(bytes.NewReader(out), "vm", []string{"claude", "codex", "opencode", "pi"})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for n := range s.entries {
		names = append(names, n)
	}
	slices.Sort(names)
	want := []string{
		".claude/settings.json", ".claude/skills", ".claude/skills/s", ".claude/skills/s/SKILL.md", ".config/opencode/opencode.json",
		".pi/agent/models.json",
	}
	if !slices.Equal(names, want) {
		t.Errorf("entries = %v, want %v", names, want)
	}
	if s.home != home || !strings.Contains(string(s.claude), `"theme"`) || strings.Contains(string(s.claude), "testfake") || s.claudeBad {
		t.Errorf("home %q, claude %q, bad %v", s.home, s.claude, s.claudeBad)
	}
	if bytes.Contains(out, []byte("testfake")) {
		t.Error("the stream holds a login or history")
	}
	// A ~/.claude.json that isn't JSON: said so, not sent.
	write(t, home, map[string]string{".claude.json": "{"})
	cmd = exec.CommandContext(t.Context(), "bash", "-c", agentSaveScript([]string{"claude"})) //nolint:gosec // devvm's own script
	cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "COPYFILE_DISABLE=1"}
	if out, err = cmd.Output(); err != nil {
		t.Fatal(err)
	}
	if s, err = readSent(bytes.NewReader(out), "vm", []string{"claude"}); err != nil || !s.claudeBad || s.claude != nil {
		t.Errorf("readSent = %+v, %v", s, err)
	}
}

// What a VM may send: its agents' settings files and folders, in
// folders the stream has, with the save's own files; any other path, a
// duplicate, a file too big or too many entries saves nothing. Links and
// other kinds of file are kept for the plan to judge.
func TestReadSent(t *testing.T) {
	claude := []string{"claude"}
	ok := saveTar(t, true,
		te{name: ".claude/settings.json", data: "{}"},
		te{name: ".claude/skills/"},
		te{name: ".claude/skills/run.sh", data: "x", mode: 0o4755},
		te{name: ".claude/skills/l->run.sh"},
		te{name: ".claude/skills/fifo", kind: tar.TypeFifo},
		te{name: saveOwn + "/claude.json", data: "{}"},
	)
	s, err := readSent(bytes.NewReader(ok), "vm", claude)
	if err != nil {
		t.Fatal(err)
	}
	if e := s.entries[".claude/skills/run.sh"]; e == nil || e.kind != 'f' || !e.exec || e.top != ".claude/skills" || !e.folder {
		t.Errorf("run.sh = %+v", e)
	}
	if e := s.entries[".claude/skills/l"]; e == nil || e.kind != 'l' || e.link != "run.sh" {
		t.Errorf("l = %+v", e)
	}
	if e := s.entries[".claude/skills/fifo"]; e == nil || e.kind != 0 {
		t.Errorf("fifo = %+v", e)
	}
	if s.home != "/home/u.linux" || string(s.claude) != "{}" {
		t.Errorf("home %q, claude %q", s.home, s.claude)
	}

	for _, tc := range []struct {
		name string
		es   []te
		want string
	}{
		{"absolute", []te{{name: "/etc/passwd", data: "x"}}, "agent_save_unsafe"},
		{"dotdot", []te{{name: ".claude/skills/"}, {name: ".claude/skills/../../x", data: "x"}}, "agent_save_unsafe"},
		{"dot", []te{{name: "./.claude/settings.json", data: "x"}}, "agent_save_unsafe"},
		{"outside", []te{{name: ".bashrc", data: "x"}}, "agent_save_unsafe"},
		{"login", []te{{name: ".claude/.credentials.json", data: "x"}}, "agent_save_unsafe"},
		{"history", []te{{name: ".claude/projects/p/h.jsonl", data: "x"}}, "agent_save_unsafe"},
		{"codex's", []te{{name: ".codex/config.toml", data: "x"}}, "agent_save_unsafe"},
		{"control", []te{{name: ".claude/skills/"}, {name: ".claude/skills/a\nb", data: "x"}}, "agent_save_unsafe"},
		{"bidi", []te{{name: ".claude/skills/"}, {name: ".claude/skills/" + string(rune(0x202e)) + "dm.x", data: "x"}}, "agent_save_unsafe"},
		{"zero-width", []te{{name: ".claude/skills/"}, {name: ".claude/skills/a" + string(rune(0x200b)) + "b", data: "x"}}, "agent_save_unsafe"},
		{"twice", []te{{name: ".claude/settings.json", data: "x"}, {name: ".claude/settings.json", data: "y"}}, "agent_save_unsafe"},
		{"no folder", []te{{name: ".claude/skills/s/SKILL.md", data: "x"}}, "agent_save_unsafe"},
		{"below a link", []te{{name: ".claude/skills/"}, {name: ".claude/skills/l->/etc"}, {name: ".claude/skills/l/passwd", data: "x"}}, "agent_save_unsafe"},
		{"own", []te{{name: saveOwn + "/x", data: "x"}}, "agent_save_unsafe"},
		{"own link", []te{{name: saveOwn + "/claude.json->/etc/passwd"}}, "agent_save_unsafe"},
		{"too big", []te{{name: ".claude/settings.json", data: strings.Repeat("x", saveMaxFile+1)}}, "agent_save_too_much"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := readSent(bytes.NewReader(saveTar(t, true, tc.es...)), "vm", claude)
			if got := msg.IDOf(err); got != tc.want {
				t.Errorf("readSent = %v, want %s", err, tc.want)
			}
		})
	}
	if _, err := readSent(strings.NewReader("not a tar stream, but long enough to be read as one's header"), "vm", claude); msg.IDOf(err) != "agent_save_not_read" {
		t.Errorf("readSent of junk = %v", err)
	}
	many := []te{{name: ".claude/skills/"}}
	for i := range saveMaxEntries {
		many = append(many, te{name: ".claude/skills/f" + strconv.Itoa(i)})
	}
	if _, err := readSent(bytes.NewReader(saveTar(t, false, many...)), "vm", claude); msg.IDOf(err) != "agent_save_too_much" {
		t.Errorf("readSent of %d entries = %v", len(many), err)
	}
}

// The listed paths: each agent's files and folders, and opencode's
// config folder's entries but lock files and node_modules.
func TestListedPath(t *testing.T) {
	all := []string{"claude", "codex", "opencode", "pi"}
	for _, tc := range []struct {
		name   string
		dir    bool
		top    string
		folder bool
	}{
		{".claude/settings.json", false, ".claude/settings.json", false},
		{".claude/plugins/marketplaces/m/.git/config", false, ".claude/plugins", true},
		{".codex/prompts", true, ".codex/prompts", true},
		{".config/opencode/opencode.json", false, ".config/opencode/opencode.json", false},
		{".config/opencode/agent", true, ".config/opencode/agent", true},
		{".config/opencode/agent/a.md", false, ".config/opencode/agent", true},
		{".config/opencode/bun.lock", false, "", false},
		{".config/opencode/node_modules", true, "", false},
		{".config/opencode/node_modules/x", false, "", false},
		{".claude/settings.json.bak", false, "", false},
		{".claude/skillsx", true, "", false},
		{".claude.json", false, "", false},
		{".pi/agent/settings.json", false, ".pi/agent/settings.json", false},
		{".pi/agent/extensions/e/index.ts", false, ".pi/agent/extensions", true},
		{".pi/agent/auth.json", false, "", false},
		{".pi/agent/sessions/--p--/s.jsonl", false, "", false},
		{".pi/agent/trust.json", false, "", false},
	} {
		_, top, folder, ok := listedPath(all, tc.name, tc.dir)
		if top != tc.top || folder != tc.folder || ok != (tc.top != "") {
			t.Errorf("listedPath(%s) = %q, %v, %v", tc.name, top, folder, ok)
		}
	}
}

// A link in a settings folder leads to a file: relatively, inside the
// folder, through folders the stream has, in a few hops; any other
// leads nowhere.
func TestResolve(t *testing.T) {
	s, err := readSent(bytes.NewReader(saveTar(t, false,
		te{name: ".claude/skills/"},
		te{name: ".claude/skills/d/"},
		te{name: ".claude/skills/d/f", data: "f"},
		te{name: ".claude/skills/ok->d/f"},
		te{name: ".claude/skills/d/up->../ok"},
		te{name: ".claude/skills/out->../settings.json"},
		te{name: ".claude/skills/abs->/home/u/.claude/skills/d/f"},
		te{name: ".claude/skills/dir->d"},
		te{name: ".claude/skills/dl->d"},
		te{name: ".claude/skills/via->dl/f"},
		te{name: ".claude/skills/loop->loop"},
		te{name: ".claude/skills/gone->nope"},
		te{name: ".claude/skills/escape->d/../../skills/d/f"},
	)), "vm", []string{"claude"})
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{
		"ok": true, "d/up": true, "escape": true,
		"out": false, "abs": false, "dir": false, "via": false, "loop": false, "gone": false,
	} {
		e, ok := s.resolve(".claude/skills/"+name, ".claude/skills")
		if ok != want || ok && string(e.data) != "f" {
			t.Errorf("resolve(%s) = %+v, %v", name, e, ok)
		}
	}
}

// saveTest is a saver in a sandbox: devvm's folders of its own, its
// messages' names in got.
type saveTest struct {
	saver
	got []string
}

func newSaveTest(t *testing.T, stream []byte, agents ...string) *saveTest {
	t.Helper()
	root := t.TempDir()
	d := paths.Dirs{Home: filepath.Join(root, "home"), Data: filepath.Join(root, "data"), Config: filepath.Join(root, "config")}
	st := &saveTest{}
	fake := &msg.Fake{}
	fake.OnAny = func(m msg.M) { st.got = append(st.got, m.Names()) }
	store := secrets.NewStore(d, fake)
	store.ExcludeFromBackup = func(context.Context, string) error { return nil }
	s, err := readSent(bytes.NewReader(stream), "vm", agents)
	if err != nil {
		t.Fatal(err)
	}
	st.saver = saver{Env: &Env{Dirs: d, Reporter: fake, Proxy: &netpolicy.Proxy{Secrets: store}}, s: s}
	return st
}

func (st *saveTest) store(agent string) string { return agentStore(st.Dirs, agent) }

// A save without placeholders: a changed settings file and a new one,
// a new folder whole (a link to a file inside it a copy, its owner's
// execute bit kept, set-id bits never), ~/.claude.json's subset, and the
// plugin records naming the store's folders; what's left out warned
// about and, in a folder, kept as the store has it; what's the same as
// the store's not planned. Applied, the store has it, and only it.
func TestAgentSavePlanAndApply(t *testing.T) {
	stream := saveTar(t, true,
		te{name: ".claude/settings.json", data: "{\"theme\": \"light\"}\n"},
		te{name: ".claude/CLAUDE.md", data: "# mine\n"},
		te{name: ".claude/skills/"},
		te{name: ".claude/skills/s/"},
		te{name: ".claude/skills/s/SKILL.md", data: "# s\n"},
		te{name: ".claude/skills/s/run.sh", data: "echo", mode: 0o6755},
		te{name: ".claude/skills/copy->s/SKILL.md"},
		te{name: ".claude/skills/escape->../../outside"},
		te{name: ".claude/skills/ph.md", data: "devvm-redacted"},
		te{name: ".claude/skills/fifo", kind: tar.TypeFifo},
		te{name: ".claude/plugins/"},
		te{name: ".claude/plugins/installed_plugins.json", data: `{"p": {"installPath": "/home/u.linux/.claude/plugins/cache/p"}}`},
		te{name: ".claude/agents->/elsewhere"},
		te{name: saveOwn + "/claude.json", data: `{"theme": "light", "projects": {"/p": {}}, "primaryApiKey": "testfake-key"}`},
	)
	st := newSaveTest(t, stream, "claude")
	store := st.store("claude")
	write(t, store, map[string]string{
		".claude/settings.json":   "{\"theme\": \"dark\"}\n",
		".claude/skills/ph.md":    "the store's",
		".claude/skills/old.md":   "gone",
		".claude/commands/c.md":   "kept: the VM has no commands",
		".claude/agents/a.md":     "kept: the VM's is a link",
		".claude/plugins/x.json":  "gone",
		".claude/plugins/keep.md": "gone",
	})
	if err := os.Symlink(filepath.Join(store, ".claude/skills/s/SKILL.md"), filepath.Join(store, ".claude/skills/fifo")); err != nil {
		t.Fatal(err)
	}
	p, err := st.plan("claude")
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, f := range p.files {
		files = append(files, f.rel)
	}
	if !slices.Equal(files, []string{".claude/CLAUDE.md", ".claude/settings.json"}) {
		t.Errorf("files = %v", files)
	}
	if len(p.folders) != 2 {
		t.Fatalf("folders = %+v", p.folders)
	}
	plugins, skills := p.folders[0], p.folders[1]
	if !slices.Equal(skills.added, []string{"copy", "s/SKILL.md", "s/run.sh"}) || len(skills.changed) != 0 ||
		!slices.Equal(skills.removed, []string{"old.md"}) || !slices.Equal(skills.kept, []string{".claude/skills/escape", ".claude/skills/fifo", ".claude/skills/ph.md"}) {
		t.Errorf("skills = %+v", skills)
	}
	if !slices.Equal(plugins.added, []string{"installed_plugins.json"}) || !slices.Equal(plugins.removed, []string{"keep.md", "x.json"}) {
		t.Errorf("plugins = %+v", plugins)
	}
	if s := string(p.claude); !strings.Contains(s, `"light"`) || strings.Contains(s, "projects") || strings.Contains(s, "testfake") {
		t.Errorf("claude = %s", s)
	}
	for _, w := range []string{
		"agent_save_link_left_out path=.claude/skills/escape vm=vm", "agent_save_kind_left_out path=.claude/skills/fifo vm=vm",
		"agent_save_placeholder path=.claude/skills/ph.md vm=vm", "agent_save_link_left_out path=.claude/agents vm=vm",
	} {
		if !slices.Contains(st.got, w) {
			t.Errorf("no %s in %v", w, st.got)
		}
	}

	var out bytes.Buffer
	if err := showPlans(&out, "vm", []agentPlan{p}); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		"--- " + filepath.Join(store, ".claude/settings.json") + "\n+++ vm:~/.claude/settings.json\n@@ -1,3 +1,3 @@\n {\n-  \"theme\": \"dark\"\n+  \"theme\": \"light\"\n }\n",
		"@@ -0,0 +1,1 @@\n+# mine\n", "  A s/run.sh\n", "  D old.md\n", "+++ vm:~/.claude.json\n",
		"+++ vm:~/.claude/skills/s/run.sh\n  mode 0600 -> 0700\n@@ -0,0 +1,1 @@\n+echo\n", "+++ vm:~/.claude/skills/copy\n@@ -0,0 +1,1 @@\n+# s\n",
	} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("no %q in:\n%s", s, out.String())
		}
	}

	if err := st.apply(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]string{
		".claude/settings.json":     "{\"theme\": \"light\"}\n",
		".claude/CLAUDE.md":         "# mine\n",
		".claude/skills/copy":       "# s\n",
		".claude/skills/ph.md":      "the store's",
		".claude/commands/c.md":     "kept: the VM has no commands",
		".claude/agents/a.md":       "kept: the VM's is a link",
		".claude/skills/s/SKILL.md": "# s\n",
	} {
		if got := readFile(t, filepath.Join(store, rel)); got != want {
			t.Errorf("%s = %q, want %q", rel, got, want)
		}
	}
	if got := readFile(t, filepath.Join(store, ".claude/plugins/installed_plugins.json")); !strings.Contains(got, filepath.Join(store, ".claude/plugins/cache/p")) {
		t.Errorf("plugin record: %s", got)
	}
	for _, rel := range []string{".claude/skills/old.md", ".claude/plugins/x.json", ".claude/skills/escape"} {
		if _, err := os.Lstat(filepath.Join(store, rel)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s is there: %v", rel, err)
		}
	}
	if fi, err := os.Lstat(filepath.Join(store, ".claude/skills/fifo")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the store's own link at fifo went: %v", err)
	}
	if m := mode(t, filepath.Join(store, ".claude/skills/s/run.sh")); m != 0o700 {
		t.Errorf("run.sh's mode = %o", m)
	}
	if fi, _ := os.Stat(filepath.Join(store, ".claude/skills/s/run.sh")); fi.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
		t.Errorf("run.sh is set-id: %v", fi.Mode())
	}
	if m := mode(t, filepath.Join(store, ".claude/settings.json")); m != 0o600 {
		t.Errorf("settings.json's mode = %o", m)
	}
	if got := readFile(t, secrets.LoginFile(st.Dirs, "claude")); !strings.Contains(got, `"light"`) || strings.Contains(got, "projects") {
		t.Errorf("the store's .claude.json = %s", got)
	}

	// Saved again, nothing differs.
	p, err = st.plan("claude")
	if err != nil || p.changed() {
		t.Errorf("plan again = %+v, %v", p, err)
	}
}

// The store's own folder, mounted and linked in the VM, is no change;
// a settings file that doesn't parse, a link or another kind in place of
// a file are left out with a warning.
func TestAgentSavePlanLeavesOut(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, "config")
	mount := filepath.Join(config, "agents", "claude", ".claude", "skills")
	stream := saveTar(t, true,
		te{name: ".claude/skills->" + mount},
		te{name: ".claude/settings.json", data: "{"},
		te{name: ".claude/CLAUDE.md->/etc/passwd"},
		te{name: ".claude/plugins", data: "a file"},
		te{name: saveOwn + "/claude-unreadable"},
	)
	st := newSaveTest(t, stream, "claude")
	st.Dirs = paths.Dirs{Home: filepath.Join(root, "home"), Data: filepath.Join(root, "data"), Config: config}
	p, err := st.plan("claude")
	if err != nil || p.changed() {
		t.Errorf("plan = %+v, %v", p, err)
	}
	want := []string{
		"agent_save_link_left_out path=.claude/CLAUDE.md vm=vm", "agent_save_kind_left_out path=.claude/plugins vm=vm",
		"agent_save_unparsed path=.claude/settings.json vm=vm", "agent_save_unparsed path=.claude.json vm=vm",
	}
	slices.Sort(want)
	got := slices.Clone(st.got)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("messages = %v, want %v", got, want)
	}
}

// No VM gets opencode's files that aren't settings or Markdown, so its
// own file of that name is left out, and the store's stays.
func TestAgentSaveWithheld(t *testing.T) {
	stream := saveTar(t, true, te{name: ".config/opencode/.env", data: "A=vm"}, te{name: ".config/opencode/AGENTS.md", data: "# vm"})
	st := newSaveTest(t, stream, "opencode")
	write(t, st.store("opencode"), map[string]string{".config/opencode/.env": "A=testfake-store"})
	p, err := st.plan("opencode")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.files) != 1 || p.files[0].rel != ".config/opencode/AGENTS.md" ||
		!slices.Contains(st.got, "agent_save_withheld path=.config/opencode/.env vm=vm") {
		t.Errorf("files %+v, messages %v", p.files, st.got)
	}
}

// The VM's copies hold placeholders: the store's
// secrets go back in their place, in a settings file and in
// ~/.claude.json's MCP servers, so the store keeps them; a server whose
// URL changed around its header's placeholder is left out, so the
// store's header never goes to another host.
func TestAgentSaveNetworkPolicy(t *testing.T) {
	const salt = "testsalt"
	settings := `{"theme": "dark", "env": {"ANTHROPIC_AUTH_TOKEN": "testfake-auth"}}`
	claudeJSON := `{"theme": "dark", "mcpServers": {"s": {"type": "http", "url": "https://mcp.example.com/x", "headers": {"X-Key": "testfake-header"}}}}`
	setup := func(t *testing.T, vmSettings, vmClaude func(string) string) (*saveTest, agentPlan) {
		t.Helper()
		probe := newSaveTest(t, saveTar(t, true), "claude")
		write(t, probe.store("claude"), map[string]string{".claude/settings.json": settings})
		write(t, filepath.Dir(secrets.LoginFile(probe.Dirs, "claude")), map[string]string{".claude.json": claudeJSON})
		gotSettings, _, err := agentlogin.Redact("json", []byte(settings))
		if err != nil {
			t.Fatal(err)
		}
		gotClaude, err := probe.Proxy.Secrets.VMLogin("claude", salt)
		if err != nil {
			t.Fatal(err)
		}
		s, err := readSent(bytes.NewReader(saveTar(t, true,
			te{name: ".claude/settings.json", data: vmSettings(string(gotSettings))},
			te{name: saveOwn + "/claude.json", data: vmClaude(string(gotClaude))},
		)), "vm", []string{"claude"})
		if err != nil {
			t.Fatal(err)
		}
		probe.s, probe.salt = s, salt
		p, err := probe.plan("claude")
		if err != nil {
			t.Fatal(err)
		}
		return probe, p
	}
	light := func(s string) string { return strings.Replace(s, `"dark"`, `"light"`, 1) }
	st, p := setup(t, light, light)
	if len(p.files) != 1 || !strings.Contains(string(p.files[0].data), "testfake-auth") || !strings.Contains(string(p.files[0].data), `"light"`) {
		t.Errorf("settings = %+v", p.files)
	}
	if !strings.Contains(string(p.claude), "testfake-header") || !strings.Contains(string(p.claude), `"light"`) || agentlogin.HasPlaceholder(p.claude) {
		t.Errorf("claude = %s", p.claude)
	}
	if len(st.got) != 0 {
		t.Errorf("messages: %v", st.got)
	}
	// The diff shows the change, never the store's secrets.
	var out bytes.Buffer
	if err := showPlans(&out, "vm", []agentPlan{p}); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, `+  "theme": "light"`) || strings.Contains(s, "testfake") {
		t.Errorf("diff:\n%s", s)
	}
	// A change to a secret's value alone shows as one.
	out.Reset()
	if err := showPlans(&out, "vm", []agentPlan{{store: "s", files: []planFile{{rel: ".claude/settings.json", old: []byte(settings), data: []byte(strings.Replace(settings, "testfake-auth", "testfake-other", 1))}}}}); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, "(only its layout") || strings.Contains(s, "testfake") {
		t.Errorf("diff:\n%s", s)
	}

	evil := func(s string) string { return strings.Replace(light(s), "mcp.example.com", "evil.example.com", 1) }
	st, p = setup(t, light, evil)
	if p.claude != nil || !slices.Contains(st.got, "agent_save_placeholder path=.claude.json vm=vm") {
		t.Errorf("claude = %s, messages %v", p.claude, st.got)
	}
}

// The diff names a binary file without showing it, and shows at most
// diffShowMax lines in all, saying how many more there are.
func TestShowPlansLimits(t *testing.T) {
	var out bytes.Buffer
	bin := planFile{rel: ".claude/skills/b", data: []byte("a\x00b")}
	if err := showPlans(&out, "vm", []agentPlan{{store: "s", files: []planFile{bin}}}); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, "(a binary file") || strings.Contains(s, "a\x00b") {
		t.Errorf("diff:\n%q", s)
	}
	var files []planFile
	for i := range diffShowMax/4 + 2 { // four lines each
		files = append(files, planFile{rel: ".claude/CLAUDE.md", data: []byte(strconv.Itoa(i) + "\n")})
	}
	out.Reset()
	if err := showPlans(&out, "vm", []agentPlan{{store: "s", files: files}}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != diffShowMax+1 || lines[diffShowMax] != "  (8 more lines of this diff not shown)" {
		t.Errorf("%d lines, the last %q", len(lines), lines[len(lines)-1])
	}
}
