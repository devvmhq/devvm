package vm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/devvmhq/devvm/assets"
	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/backend/backendtest"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/state"
)

// printed makes e's warnings and infos a list of their names, and its
// questions' answer yes (no terminal when tty is false).
func printed(t *testing.T, e *Env, tty bool) *[]string {
	t.Helper()
	var got []string
	if e.Reporter == nil {
		e.Reporter = testUI(t)
	}
	f := fake(e)
	f.Terminal = tty
	f.OnWarn = func(m msg.M) { got = append(got, m.Names()) }
	f.OnInfo = f.OnWarn
	f.OnConfirm = func(m msg.M) bool { got = append(got, "asked "+m.Names()); return true }
	return &got
}

func mkdirs(t *testing.T, home string, rels ...string) {
	t.Helper()
	for _, r := range rels {
		if err := os.MkdirAll(filepath.Join(home, r), 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

// A VM mounts its agents' folders that exist in devvm's store, each in
// its agent's store, one per entry, names with spaces kept whole;
// opencode's are its config folder's subfolders but node_modules. The
// home folder's are never mounted. A path with a comma is left out, with
// a warning.
func TestAgentMounts(t *testing.T) {
	home := t.TempDir()
	e := &Env{Dirs: paths.Dirs{Home: home, Config: t.TempDir()}}
	got := printed(t, e, false)
	claude, opencode := agentStore(e.Dirs, "claude"), agentStore(e.Dirs, "opencode")
	mkdirs(t, opencode, ".config/opencode/my agents", ".config/opencode/node_modules", ".config/opencode/a\nb")
	mkdirs(t, claude, ".claude/skills", ".claude/agents")
	mkdirs(t, agentStore(e.Dirs, "codex"), ".codex/prompts")
	pi := agentStore(e.Dirs, "pi")
	mkdirs(t, pi, ".pi/agent/skills", ".pi/agent/extensions", ".pi/agent/sessions")
	mkdirs(t, home, ".claude/commands", ".config/opencode/yours", ".pi/agent/prompts")
	if err := os.WriteFile(filepath.Join(claude, ".claude", "commands"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Join(claude, ".claude/skills"), filepath.Join(claude, ".claude/agents"), filepath.Join(opencode, ".config/opencode/my agents"),
		filepath.Join(pi, ".pi/agent/extensions"), filepath.Join(pi, ".pi/agent/skills"),
	}
	if ms := (Launch{e}).AgentMounts([]string{"node", "claude", "opencode", "pi"}); !slices.Equal(ms, want) {
		t.Errorf("AgentMounts = %q, want %q", ms, want)
	}
	if ms := (Launch{e}).AgentMounts([]string{"node"}); ms != nil {
		t.Errorf("AgentMounts without agents = %q", ms)
	}
	e.Dirs.Config = filepath.Join(t.TempDir(), "a,b")
	mkdirs(t, agentStore(e.Dirs, "codex"), ".codex/prompts")
	if ms := (Launch{e}).AgentMounts([]string{"codex"}); ms != nil {
		t.Errorf("AgentMounts with a comma = %q", ms)
	}
	if !slices.Equal(*got, []string{"agent_dir_comma path=" + filepath.Join(agentStore(e.Dirs, "codex"), ".codex/prompts")}) {
		t.Errorf("printed %q", *got)
	}
}

// agentVM is a VM with these shared tools (space-separated) on a fake
// backend, its agent mounts and status as given.
func agentVM(t *testing.T, tools string, status backend.Status, dirs ...string) (*backendtest.Fake, *state.VM) {
	t.Helper()
	root := t.TempDir()
	d := paths.Dirs{Data: filepath.Join(root, "data"), Config: filepath.Join(root, "config")}
	st := vmState(t, d, "devvm-p-00000000000000", withTools(strings.Fields(tools)...))
	f := backendtest.New("fake", allNative())
	f.Instances[st.Name()] = backend.Instance{Name: st.Name(), Status: status, AgentDirs: dirs}
	return f, st
}

func TestUpdateAgentDirs(t *testing.T) {
	d := paths.Dirs{Config: t.TempDir()}
	home := agentStore(d, "claude")
	skills, mine, commands := filepath.Join(home, ".claude/skills"), filepath.Join(home, ".claude/agents"), filepath.Join(home, ".claude/commands")
	mkdirs(t, home, ".claude/skills")
	vm := "devvm-p-00000000000000"
	for _, tc := range []struct {
		name    string
		tools   string
		status  backend.Status
		have    []string
		methods []string
		printed []string
		// later: it changes at its next start, which bringUp offers to
		// make now, and says.
		later bool
	}{
		{
			name: "nothing changed, nothing edited", tools: "node claude", status: backend.StatusStopped,
			have: []string{skills}, methods: []string{"Inspect"},
		},
		{
			name: "a stopped VM gets new folders and loses gone ones", tools: "node claude", status: backend.StatusStopped,
			have: []string{skills, commands}, methods: []string{"Inspect"},
			printed: []string{"agent_dirs_added dirs=" + mine + " vm=" + vm, "agent_dirs_removed dirs=" + commands + " vm=" + vm, "mounts_updated vm=" + vm},
		},
		{
			name: "a running VM changes at its next start, without a question", tools: "node claude", status: backend.StatusRunning,
			have: []string{skills}, methods: []string{"Inspect"}, printed: []string{"agent_dirs_added dirs=" + mine + " vm=" + vm}, later: true,
		},
		{
			name: "a VM without agents drops leftover agent mounts", tools: "node", status: backend.StatusStopped,
			have: []string{filepath.Join(home, ".codex/prompts")}, methods: []string{"Inspect"},
			printed: []string{"agent_dirs_removed dirs=" + filepath.Join(home, ".codex/prompts") + " vm=" + vm, "mounts_updated vm=" + vm},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &Env{Dirs: d}
			got := printed(t, e, true)
			if tc.name != "nothing changed, nothing edited" {
				mkdirs(t, home, ".claude/agents")
			}
			f, st := agentVM(t, tc.tools, tc.status, tc.have...)
			in, err := f.Inspect(t.Context(), st.Name())
			if err != nil {
				t.Fatal(err)
			}
			if later := (Launch{e}).updateAgentDirs(st, in); later != tc.later {
				t.Errorf("later %v", later)
			}
			if ms := f.Methods(); !slices.Equal(ms, tc.methods) {
				t.Errorf("calls = %q, want %q", ms, tc.methods)
			}
			if !slices.Equal(*got, tc.printed) {
				t.Errorf("printed %q, want %q", *got, tc.printed)
			}
			_ = os.Remove(mine)
		})
	}
}

// An import leaves out the agents' logins, ~/.claude.json and what
// devvm's setup writes, and the VM's tar is told to check the paths
// first, keep no owner or mode, and never take absolute paths.
func TestHistoryImportScript(t *testing.T) {
	skips := importSkips()
	for _, p := range []string{
		".claude/.credentials.json", ".codex/auth.json", ".local/share/opencode/auth.json", ".local/share/opencode/account.json",
		".claude.json", ".claude/settings.json", ".claude/CLAUDE.md", ".claude/skills", ".codex/config.toml", ".codex/prompts",
		".pi/agent/settings.json", ".pi/agent/models.json", ".pi/agent/skills",
	} {
		if !slices.Contains(skips, p) {
			t.Errorf("%s isn't left out", p)
		}
	}
	if !slices.IsSorted(skips) || len(slices.Compact(slices.Clone(skips))) != len(skips) {
		t.Errorf("skips %q aren't sorted once each", skips)
	}
	s := historyImport()
	for _, want := range []string{
		`tar -tzf "$t/export" --absolute-names --quoting-style=escape`, "grep -vqE '^(\\.claude|\\.claude\\.json|\\.codex|",
		`grep -qE '(^|/)\.\.(/|$)'`, "echo " + historyUnsafe + "; exit 1",
		`tar -xzf "$t/export" -C "$t/x" '--no-same-owner' '--no-same-permissions' '--anchored' '--exclude=`,
		"'--exclude=.claude.json' ", `find "$t/x" -type l`, "/* | .. | ../* | */.. | */../*) ",
		"for p in '.claude' '.claude.json' '.codex' '.local/share/opencode' '.local/state/opencode' '.pi/agent/sessions'",
		"tar --extract --file=- --directory ~ --skip-old-files '--no-same-owner' '--no-same-permissions'",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q:\n%s", want, s)
		}
	}
	// Only the listing takes the paths as they are; the unpacking never.
	unpacks := 0
	for line := range strings.SplitSeq(s, "\n") {
		if !strings.Contains(line, "tar -xzf") && !strings.Contains(line, "tar --extract") {
			continue
		}
		unpacks++
		for _, bad := range []string{" -P", "--absolute-names", "--same-owner", "--preserve-permissions", "--overwrite"} {
			if strings.Contains(line, bad) {
				t.Errorf("tar gets %q: %s", bad, line)
			}
		}
	}
	if unpacks != 2 || strings.Count(s, "--absolute-names") != 1 {
		t.Errorf("%d unpacks, --absolute-names beyond the listing:\n%s", unpacks, s)
	}
}

// The import needs GNU tar, which Debian and Ubuntu always have, and
// base's Alpine branch installs over busybox's: its apk add has tar.
func TestHistoryImportHasGNUTarOnAlpine(t *testing.T) {
	b, err := fs.ReadFile(assets.FS, "layers/base.sh")
	if err != nil {
		t.Fatal(err)
	}
	_, musl, ok := strings.Cut(string(b), "if [[ $devvm_libc == musl ]]; then")
	musl, _, _ = strings.Cut(musl, "\nelse\n")
	add := regexp.MustCompile(`(?m)^\s*sudo apk add [^\n]*(\\\n[^\n]*)*`).FindString(musl)
	if !ok || !slices.Contains(strings.Fields(add), "tar") {
		t.Errorf("base.sh's Alpine branch doesn't install GNU tar: %q", add)
	}
}

// exportOf is a gzipped tarball of these entries: a name ending in / is
// a folder, one with "->" a symlink, else a file whose content is its name.
func exportOf(t *testing.T, names ...string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	tw := tar.NewWriter(zw)
	for _, n := range names {
		h := &tar.Header{Name: n, Mode: 0o4755, ModTime: time.Unix(1e9, 0), Typeflag: tar.TypeReg, Size: int64(len(n))}
		if strings.HasSuffix(n, "/") {
			h.Typeflag, h.Size = tar.TypeDir, 0
		} else if name, target, ok := strings.Cut(n, "->"); ok {
			h.Name, h.Linkname, h.Typeflag, h.Size = name, target, tar.TypeSymlink, 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(n)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// The import script, run by GNU tar as the VM's does (skipped where
// there's none): a tarball with a path outside the agents' history,
// absolute or with .., imports nothing; the rest leave out importSkips
// and keep the files the VM has.
func TestHistoryImportRuns(t *testing.T) {
	if out, err := exec.CommandContext(t.Context(), "tar", "--version").Output(); err != nil || !strings.Contains(string(out), "GNU tar") {
		t.Skip("no GNU tar")
	}
	run := func(t *testing.T, home string, export []byte) (string, error) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "bash", "-c", historyImport()) //nolint:gosec // devvm's own import script
		cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
		var stderr bytes.Buffer
		cmd.Stdin, cmd.Stderr = bytes.NewReader(export), &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Logf("tar's stderr: %s", stderr.String())
		}
		return string(out), err
	}
	for _, names := range [][]string{
		{".claude/a", "/etc/devvm-import-test"},
		{".claude/a", ".claude/../.bashrc"},
		{".claude/../../devvm-import-test"},
		{".claude/../.claude.json"},
		{".bashrc"},
		{"./.claude/a"},
		{".claudex/a"},
		{".pi/agent/sessions/s", ".pi/agent/auth.json"},
		{".pi/agent/sessionsx/s"},
	} {
		t.Run(names[len(names)-1], func(t *testing.T) {
			home := t.TempDir()
			out, err := run(t, home, exportOf(t, names...))
			if err == nil || strings.TrimSpace(out) != historyUnsafe {
				t.Errorf("imported: %q, %v", out, err)
			}
			if es, _ := os.ReadDir(home); len(es) != 1 || es[0].Name() != ".cache" {
				t.Errorf("home has %v", es)
			}
			if es, _ := os.ReadDir(filepath.Join(home, ".cache")); len(es) != 0 {
				t.Errorf("its temp folder stayed: %v", es)
			}
		})
	}
	t.Run("history", func(t *testing.T) {
		home := t.TempDir()
		mkdirs(t, home, ".claude/projects", ".pi/agent")
		newer := filepath.Join(home, ".claude/projects/kept.jsonl")
		if err := state.WriteFile(newer, []byte("the VM's own\n")); err != nil {
			t.Fatal(err)
		}
		// pi's login, which the VM's setup wrote: the import leaves it and its folder as they are.
		if err := state.WriteFile(filepath.Join(home, ".pi/agent/auth.json"), []byte("{}\n")); err != nil {
			t.Fatal(err)
		}
		export := exportOf(t, ".claude/", ".claude/projects/", ".claude/projects/p.jsonl", ".claude/projects/kept.jsonl",
			".claude/.credentials.json", ".claude/settings.json", ".claude/skills/", ".claude/skills/x", ".claude.json",
			".claude/link->/etc", ".claude/projects/up->../../.bashrc", ".claude/projects/in->p.jsonl", ".codex/",
			".codex/sessions/s", ".codex/auth.json", ".local/share/opencode/auth.json", ".local/state/opencode/x",
			".pi/agent/sessions/", ".pi/agent/sessions/--p--/s.jsonl")
		if out, err := run(t, home, export); err != nil {
			t.Fatalf("%q, %v", out, err)
		}
		for p, want := range map[string]bool{
			".claude/projects/p.jsonl": true, ".codex/sessions/s": true, ".local/state/opencode/x": true, ".claude/projects/in": true,
			".pi/agent/sessions/--p--/s.jsonl": true, ".pi/agent/auth.json": true,
			".claude/.credentials.json": false, ".claude/settings.json": false, ".claude/skills": false, ".claude.json": false,
			".codex/auth.json": false, ".local/share/opencode/auth.json": false, ".claude/link": false, ".claude/projects/up": false,
		} {
			if _, err := os.Lstat(filepath.Join(home, p)); (err == nil) != want {
				t.Errorf("%s: there %v, want %v", p, err == nil, want)
			}
		}
		if data, _ := os.ReadFile(newer); string(data) != "the VM's own\n" { //nolint:gosec // the test's own file

			t.Errorf("the VM's file was replaced: %q", data)
		}
		if fi, err := os.Stat(filepath.Join(home, ".claude/projects/p.jsonl")); err != nil || fi.Mode()&os.ModeSetuid != 0 {
			t.Errorf("p.jsonl's mode: %v, %v", fi, err)
		}
	})
}

// Claude Code's slug of a path, as Claude Code 2.1 computes it (the
// values its own function gives): every UTF-16 unit but an ASCII letter
// or digit a dash, and a slug over 200 cut there, then the path's hash.
func TestClaudeSlug(t *testing.T) {
	long := "/Users/me/" + strings.Repeat("projects/", 25) + "app"
	wide := "/home/é/" + strings.Repeat("x😀", 100)
	for path, want := range map[string]string{
		"/Users/me/dev/devvm-go": "-Users-me-dev-devvm-go",
		"/home/me/my_app.v2/src": "-home-me-my-app-v2-src",
		"/x/😀é":                  "-x----",
		long:                     "-Users-me" + strings.Repeat("-projects", 21) + "-p-f0zgrx",
		wide:                     "-home---" + strings.Repeat("x--", 64) + "-m9buex",
	} {
		if got := claudeSlug(path); got != want {
			t.Errorf("claudeSlug(%q) = %q, want %q", path, got, want)
		}
	}
}

// The setup's moved file names both paths and their slugs, quoted for
// the shell; a folder that didn't move gets none.
func TestClaudeMoved(t *testing.T) {
	got := string(claudeMoved("/a/it's", "/b/c"))
	want := "moved_from='/a/it'\\''s'\nmoved_to='/b/c'\nclaude_from='-a-it-s'\nclaude_to='-b-c'\n"
	if got != want {
		t.Errorf("claudeMoved = %q, want %q", got, want)
	}
	if claudeMoved("", "/b") != nil || claudeMoved("/b", "/b") != nil {
		t.Error("a folder that didn't move got a moved file")
	}
}

// guest/claude.sh's claude_project_moved moves a project's sessions'
// folder and its .claude.json entry to its new path, merging into the new
// path's own without replacing any of it, leaves links and a file that
// isn't JSON as they are, and changes nothing run again.
func TestClaudeProjectMoved(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("no jq")
	}
	script, err := fs.ReadFile(assets.FS, "guest/claude.sh")
	if err != nil {
		t.Fatal(err)
	}
	lib := filepath.Join(t.TempDir(), "claude.sh")
	if err := state.WriteFile(lib, script); err != nil {
		t.Fatal(err)
	}
	run := func(t *testing.T, home string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "bash", "-c", `set -euo pipefail; . "$1"; claude_project_moved /old/p /new/p -old-p -new-p`, "_", lib) //nolint:gosec // devvm's own script
		cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("claude_project_moved: %v: %s", err, out)
		}
		return string(out)
	}
	write := func(t *testing.T, path, data string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := state.WriteFile(path, []byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	read := func(path string) string { b, _ := os.ReadFile(path); return string(b) } //nolint:gosec // the test's own temp files
	projects := func(t *testing.T, home string) string {
		t.Helper()
		out, err := exec.CommandContext(t.Context(), "jq", "-cS", ".projects", filepath.Join(home, ".claude.json")).Output() //nolint:gosec // the test's own temp file
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	t.Run("renamed", func(t *testing.T) {
		home := t.TempDir()
		write(t, filepath.Join(home, ".claude/projects/-old-p/s.jsonl"), "s\n")
		write(t, filepath.Join(home, ".claude.json"), `{"a":1,"projects":{"/old/p":{"trust":true},"/other":{}}}`)
		for range 2 {
			run(t, home)
			if read(filepath.Join(home, ".claude/projects/-new-p/s.jsonl")) != "s\n" {
				t.Error("the session isn't under the new slug")
			}
			if _, err := os.Lstat(filepath.Join(home, ".claude/projects/-old-p")); err == nil {
				t.Error("the old slug's folder stayed")
			}
			if got := projects(t, home); got != `{"/new/p":{"trust":true},"/other":{}}` {
				t.Errorf("projects = %s", got)
			}
		}
	})
	t.Run("merged", func(t *testing.T) {
		home := t.TempDir()
		write(t, filepath.Join(home, ".claude/projects/-old-p/a.jsonl"), "old a\n")
		write(t, filepath.Join(home, ".claude/projects/-old-p/b.jsonl"), "old b\n")
		write(t, filepath.Join(home, ".claude/projects/-new-p/b.jsonl"), "new b\n")
		write(t, filepath.Join(home, ".claude.json"), `{"projects":{"/old/p":{"o":1},"/new/p":{"n":1}}}`)
		run(t, home)
		p := filepath.Join(home, ".claude/projects")
		if read(p+"/-new-p/a.jsonl") != "old a\n" || read(p+"/-new-p/b.jsonl") != "new b\n" || read(p+"/-old-p/b.jsonl") != "old b\n" {
			t.Error("not merged without replacing")
		}
		if got := projects(t, home); got != `{"/new/p":{"n":1},"/old/p":{"o":1}}` {
			t.Errorf("projects = %s", got)
		}
	})
	t.Run("links and not JSON", func(t *testing.T) {
		home := t.TempDir()
		target := t.TempDir()
		mkdirs(t, home, ".claude/projects")
		if err := os.Symlink(target, filepath.Join(home, ".claude/projects/-old-p")); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(home, ".claude.json"), "not json")
		if out := run(t, home); !strings.Contains(out, "isn't plain JSON") {
			t.Errorf("said %q", out)
		}
		if l, err := os.Readlink(filepath.Join(home, ".claude/projects/-old-p")); err != nil || l != target {
			t.Errorf("the link: %q, %v", l, err)
		}
		if read(filepath.Join(home, ".claude.json")) != "not json" {
			t.Error(".claude.json changed")
		}
	})
	t.Run("nothing there", func(t *testing.T) {
		home := t.TempDir()
		run(t, home)
		if es, _ := os.ReadDir(home); len(es) != 0 {
			t.Errorf("made %v", es)
		}
	})
}
