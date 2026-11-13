package vm

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/safefile"
	"github.com/devvmhq/devvm/internal/state"
)

// link makes a symlink at p, relative to root, leading to target.
func link(t *testing.T, root, p, target string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(p))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, full); err != nil {
		t.Fatal(err)
	}
}

// followLinksIn writes config.toml's follow-links with these folders.
func followLinksIn(t *testing.T, d paths.Dirs, dirs ...string) {
	t.Helper()
	q := make([]string, len(dirs))
	for i, dir := range dirs {
		q[i] = strconv.Quote(dir)
	}
	write(t, d.Config, map[string]string{"config.toml": "follow-links = [" + strings.Join(q, ", ") + "]\n"})
}

// said reports whether the import said a message named id about the link
// at p.
func (im *importer) said(id, p string) bool {
	return slices.ContainsFunc(im.got, func(s string) bool {
		return s == id+" path="+p || strings.HasPrefix(s, id+" path="+p+" ")
	})
}

// isFile and isDir report whether p is a regular file, or a folder, and
// never a link.
func isFile(p string) bool { fi, err := os.Lstat(p); return err == nil && fi.Mode().IsRegular() }
func isDir(p string) bool  { fi, err := os.Lstat(p); return err == nil && fi.IsDir() }

// What a link out of an agent's folder leads to is copied into the store,
// as files and folders, never as a link: without a question when it's in
// a follow-links folder, after a yes or with --follow-links when it isn't,
// and left out with a warning after a no or without a terminal, which
// fails nothing.
func TestAgentImportFollowsLinks(t *testing.T) {
	for _, c := range []struct {
		name                 string
		root                 bool // dotfiles is a follow-links folder
		flag, yes, noTTY     bool
		asked, copied, named bool
	}{
		{name: "in a follow-links folder", root: true, copied: true, named: true},
		{name: "asked, yes", yes: true, asked: true, copied: true, named: true},
		{name: "asked, no", asked: true},
		{name: "no terminal", noTTY: true},
		{name: "--follow-links", flag: true, copied: true, named: true},
		{name: "--follow-links, no terminal", flag: true, noTTY: true, copied: true, named: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			im := newImporter(t)
			home := im.Dirs.Home
			im.follow, im.FollowLinks, im.fake.Terminal = c.yes, c.flag, !c.noTTY
			write(t, home, map[string]string{
				".claude/settings.json":            "{}",
				"dotfiles/CLAUDE.md":               "notes",
				"dotfiles/skills/x/SKILL.md":       "# x",
				"dotfiles/skills/x/scripts/run.sh": "echo",
			})
			if c.root {
				followLinksIn(t, im.Dirs, "~/dotfiles")
			}
			link(t, home, ".claude/CLAUDE.md", "../dotfiles/CLAUDE.md")
			link(t, home, ".claude/skills/x", filepath.Join(home, "dotfiles/skills/x"))
			if err := im.Run(t.Context(), []string{"claude"}, false); err != nil {
				t.Fatal(msg.NamesOf(err))
			}
			store := filepath.Join(agentStore(im.Dirs, "claude"), ".claude")
			md, skill := filepath.Join(home, ".claude/CLAUDE.md"), filepath.Join(home, ".claude/skills/x")
			if got := slices.Contains(im.got, "asked agent_import_follow"); got != c.asked {
				t.Errorf("asked = %v: %q", got, im.got)
			}
			if c.copied {
				if !isFile(filepath.Join(store, "CLAUDE.md")) || readFile(t, filepath.Join(store, "CLAUDE.md")) != "notes" {
					t.Error("the linked file isn't copied as a file")
				}
				if !isDir(filepath.Join(store, "skills/x")) || !isDir(filepath.Join(store, "skills/x/scripts")) ||
					readFile(t, filepath.Join(store, "skills/x/scripts/run.sh")) != "echo" {
					t.Error("the linked folder isn't copied as a folder")
				}
			} else {
				for _, p := range []string{filepath.Join(store, "CLAUDE.md"), filepath.Join(store, "skills/x")} {
					if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
						t.Errorf("%s copied: %v", p, err)
					}
				}
				if !im.notFollowed(md) || !im.notFollowed(skill) {
					t.Errorf("no warnings: %q", im.got)
				}
			}
			if got := im.said("agent_import_link_followed", md) && im.said("agent_import_link_followed", skill); got != c.named {
				t.Errorf("named = %v: %q", got, im.got)
			}
		})
	}
}

// Within what a link leads to, a link to a file in the folder it's read
// through (the follow-links folder, or with --follow-links the target)
// is copied as that file; one that leads out of it, or to a folder, is
// left out with a warning.
func TestAgentImportNestedLinks(t *testing.T) {
	for _, c := range []struct {
		name string
		root bool // dotfiles is a follow-links folder; else --follow-links
		// whether a link in the skill to a file elsewhere in dotfiles is
		// copied
		sibling bool
	}{
		{name: "follow-links", root: true, sibling: true},
		{name: "--follow-links"},
	} {
		t.Run(c.name, func(t *testing.T) {
			im := newImporter(t)
			home := im.Dirs.Home
			im.FollowLinks = !c.root
			if c.root {
				followLinksIn(t, im.Dirs, "~/dotfiles")
			}
			write(t, home, map[string]string{
				"dotfiles/skills/x/SKILL.md": "# x",
				"dotfiles/skills/x/sub/a.md": "a",
				"dotfiles/shared.md":         "shared",
				"secret.txt":                 "secret",
			})
			x := filepath.Join(home, "dotfiles/skills/x")
			link(t, x, "same", "SKILL.md")
			link(t, x, "sibling", "../../shared.md")
			link(t, x, "escape", filepath.Join(home, "secret.txt"))
			link(t, x, "folder", "sub")
			link(t, home, ".claude/skills/x", x)
			if err := im.Run(t.Context(), []string{"claude"}, false); err != nil {
				t.Fatal(msg.NamesOf(err))
			}
			store := filepath.Join(agentStore(im.Dirs, "claude"), ".claude/skills/x")
			if !isFile(filepath.Join(store, "same")) || readFile(t, filepath.Join(store, "same")) != "# x" {
				t.Error("a link to a file in the folder isn't copied as that file")
			}
			if got := isFile(filepath.Join(store, "sibling")); got != c.sibling {
				t.Errorf("a link to a file elsewhere in dotfiles copied = %v", got)
			}
			for _, n := range []string{"escape", "folder"} {
				if _, err := os.Lstat(filepath.Join(store, n)); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("%s copied: %v", n, err)
				}
			}
			physical := paths.Physical(x)
			for _, n := range []string{"escape", "folder"} {
				if !slices.Contains(im.got, "agent_import_link_left_out path="+filepath.Join(physical, n)) {
					t.Errorf("no warning for %s: %q", n, im.got)
				}
			}
		})
	}
}

// A link to the home folder, a folder holding it, devvm's own folders, a
// place that holds secrets, or an agent's login or history, or to a
// folder holding one, is left out with a warning, even with
// --follow-links; a file elsewhere in the home folder isn't.
func TestAgentImportRefusedPlaces(t *testing.T) {
	im := newImporter(t)
	home := im.Dirs.Home
	im.FollowLinks = true
	write(t, home, map[string]string{
		".ssh/id_ed25519":         "key",
		".codex/auth.json":        "{}",
		".local/state/opencode/h": "history",
		"code/.aws/credentials":   "aws",
		"notes.md":                "notes",
		".claude/settings.json":   "{}",
		".config/opencode/a.txt":  "a",
	})
	if err := os.MkdirAll(im.Dirs.Data, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{
		"home":    home,
		"above":   filepath.Dir(home),
		"ssh":     filepath.Join(home, ".ssh"),
		"key":     filepath.Join(home, ".ssh/id_ed25519"),
		"login":   filepath.Join(home, ".codex/auth.json"),
		"history": filepath.Join(home, ".local/state/opencode"),
		"codex":   filepath.Join(home, ".codex"),
		"data":    im.Dirs.Data,
		"notes":   filepath.Join(home, "notes.md"),
	} {
		link(t, home, ".claude/skills/"+name, target)
	}
	if err := im.Run(t.Context(), []string{"claude"}, false); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	store := filepath.Join(agentStore(im.Dirs, "claude"), ".claude/skills")
	for _, n := range []string{"home", "above", "ssh", "key", "login", "history", "codex", "data"} {
		if _, err := os.Lstat(filepath.Join(store, n)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s copied: %v", n, err)
		}
		if !im.said("agent_import_link_refused", filepath.Join(home, ".claude/skills", n)) {
			t.Errorf("no warning for %s: %q", n, im.got)
		}
	}
	if readFile(t, filepath.Join(store, "notes")) != "notes" {
		t.Error("a file in the home folder isn't copied")
	}
}

// A follow-links folder that is, holds or is in a refused place fails the
// import, naming it, before anything is copied; one that's fine is
// followed. config.toml's own check refuses a folder that isn't a path.
func TestAgentImportRefusedRoots(t *testing.T) {
	for _, c := range []struct {
		dir, want string
	}{
		{dir: "~/dotfiles"},
		{dir: "~/.ssh", want: "agent_import_root_refused"},
		{dir: "~/.config", want: "agent_import_root_refused"}, // holds ~/.config/devvm
		{dir: "~/.codex", want: "agent_import_root_refused"},  // holds its login
		{dir: "/", want: "follow_links_invalid"},
		{dir: "dotfiles", want: "follow_links_invalid"},
	} {
		t.Run(c.dir, func(t *testing.T) {
			im := newImporter(t)
			home := im.Dirs.Home
			write(t, home, map[string]string{"dotfiles/CLAUDE.md": "notes"})
			link(t, home, ".claude/CLAUDE.md", filepath.Join(home, "dotfiles/CLAUDE.md"))
			followLinksIn(t, im.Dirs, c.dir)
			err := im.Run(t.Context(), []string{"claude"}, false)
			if c.want == "" {
				if err != nil || !isFile(filepath.Join(agentStore(im.Dirs, "claude"), ".claude/CLAUDE.md")) {
					t.Fatalf("not followed: %v, %q", msg.NamesOf(err), im.got)
				}
				return
			}
			got := msg.NamesOf(err)
			if ce, ok := errors.AsType[*state.CorruptError](err); ok {
				got = ce.Msg.ID()
			}
			if err == nil || !strings.HasPrefix(got, c.want) {
				t.Fatalf("err = %v, want %s", got, c.want)
			}
			if _, err := os.Lstat(agentStore(im.Dirs, "claude")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the store changed: %v", err)
			}
		})
	}
}

// A link into a VM's project folder, or to a folder holding one, is
// followed with a warning naming the VM, once; in the question when
// devvm asks.
func TestAgentImportProjectFolderLinks(t *testing.T) {
	for _, c := range []struct {
		name string
		root string // the follow-links folder; "" asks
		to   string // where the link leads, below home
	}{
		{name: "a follow-links folder that is a project", root: "~/dotfiles", to: "dotfiles/skills/x"},
		{name: "a folder holding a project", root: "~/code", to: "code"},
		{name: "asked", to: "dotfiles/skills/x"},
	} {
		t.Run(c.name, func(t *testing.T) {
			im := newImporter(t)
			home := im.Dirs.Home
			im.follow = true
			write(t, home, map[string]string{"dotfiles/skills/x/SKILL.md": "# x", "code/dotfiles-x/a.md": "a"})
			vmState(t, im.Dirs, "devvm-dotfiles-0123456789abcd", func(st *state.VM) error {
				st.Dir = filepath.Join(home, "dotfiles")
				return nil
			})
			vmState(t, im.Dirs, "devvm-x-0123456789abce", func(st *state.VM) error {
				st.Dir = filepath.Join(home, "code/dotfiles-x")
				return nil
			})
			if c.root != "" {
				followLinksIn(t, im.Dirs, c.root)
			}
			link(t, home, ".claude/skills/x", filepath.Join(home, c.to))
			if err := im.Run(t.Context(), []string{"claude"}, false); err != nil {
				t.Fatal(msg.NamesOf(err))
			}
			p := filepath.Join(home, ".claude/skills/x")
			var warned []string
			for _, s := range im.got {
				if strings.HasPrefix(s, "agent_import_link_project path="+p+" ") {
					warned = append(warned, s)
				}
			}
			want := "devvm-dotfiles-0123456789abcd"
			if c.to == "code" {
				want = "devvm-x-0123456789abce"
			}
			if len(warned) != 1 || !strings.HasSuffix(warned[0], "vm="+want) {
				t.Errorf("warnings = %q, want one for %s: %q", warned, want, im.got)
			}
			if !isDir(filepath.Join(agentStore(im.Dirs, "claude"), ".claude/skills/x")) {
				t.Error("not followed")
			}
		})
	}
}

// A link whose files pass what one import copies through links is left
// out whole, with a warning: a file past linkMaxFile, files past
// linkMaxBytes in all, or more than linkMaxEntries files and folders.
// The links within the bounds are still copied.
func TestAgentImportLinkBounds(t *testing.T) {
	sized := func(t *testing.T, p string, n int64) {
		t.Helper()
		write(t, filepath.Dir(p), map[string]string{filepath.Base(p): ""})
		if err := os.Truncate(p, n); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		name string
		make func(t *testing.T, dir string)
	}{
		{name: "a big file", make: func(t *testing.T, dir string) { sized(t, filepath.Join(dir, "big"), linkMaxFile+1) }},
		{name: "big files", make: func(t *testing.T, dir string) {
			for i := range linkMaxBytes/linkMaxFile + 1 {
				sized(t, filepath.Join(dir, "f"+strconv.Itoa(i)), linkMaxFile)
			}
		}},
		{name: "many files", make: func(t *testing.T, dir string) {
			for i := range linkMaxEntries {
				if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(i)), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			im := newImporter(t)
			home := im.Dirs.Home
			im.FollowLinks = true
			big := filepath.Join(home, "dotfiles/big")
			if err := os.MkdirAll(big, 0o700); err != nil {
				t.Fatal(err)
			}
			c.make(t, big)
			write(t, home, map[string]string{"dotfiles/small/a.md": "a"})
			link(t, home, ".claude/skills/big", big)
			link(t, home, ".claude/skills/small", filepath.Join(home, "dotfiles/small"))
			if err := im.Run(t.Context(), []string{"claude"}, false); err != nil {
				t.Fatal(msg.NamesOf(err))
			}
			store := filepath.Join(agentStore(im.Dirs, "claude"), ".claude/skills")
			if _, err := os.Lstat(filepath.Join(store, "big")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("past the bounds, yet copied: %v", err)
			}
			if !im.said("agent_import_link_too_big", filepath.Join(home, ".claude/skills/big")) {
				t.Errorf("no warning: %q", im.got)
			}
			if !isFile(filepath.Join(store, "small/a.md")) {
				t.Error("a link within the bounds isn't copied")
			}
		})
	}
}

// A file that grows past the bounds after it was listed fails the
// import as it's read.
func TestLinkSourceGrown(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{"f": "12345"})
	root, err := safefile.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	s := linkSource{root: root, name: "f", budget: &linkBudget{read: linkMaxBytes - 4}}
	if _, _, err := s.read(); !errors.Is(err, safefile.ErrTooLong) {
		t.Errorf("read = %v, want ErrTooLong", err)
	}
	s.budget.read = 0
	if data, _, err := s.read(); err != nil || string(data) != "12345" || s.budget.read != 5 {
		t.Errorf("read = %q, %v; read %d", data, err, s.budget.read)
	}
}

// A link that's one of an agent's settings files must lead to a file,
// and one that's one of its settings folders to a folder; any other is
// left out with a warning.
func TestAgentImportLinkKinds(t *testing.T) {
	im := newImporter(t)
	home := im.Dirs.Home
	im.FollowLinks = true
	write(t, home, map[string]string{"dotfiles/dir/a.md": "a", "dotfiles/file.md": "f", ".claude/CLAUDE.md": "notes"})
	link(t, home, ".claude/settings.json", filepath.Join(home, "dotfiles/dir"))
	link(t, home, ".claude/skills", filepath.Join(home, "dotfiles/file.md"))
	link(t, home, ".claude/agents", filepath.Join(home, "dotfiles/dir"))
	if err := im.Run(t.Context(), []string{"claude"}, false); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	store := filepath.Join(agentStore(im.Dirs, "claude"), ".claude")
	for _, n := range []string{"settings.json", "skills"} {
		if _, err := os.Lstat(filepath.Join(store, n)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s copied: %v", n, err)
		}
		if !slices.Contains(im.got, "agent_import_link_left_out path="+filepath.Join(home, ".claude", n)) {
			t.Errorf("no warning for %s: %q", n, im.got)
		}
	}
	if !isFile(filepath.Join(store, "agents/a.md")) {
		t.Error("a settings folder that's a link to a folder isn't copied")
	}
}

// The next import keeps a followed folder's copy, the same folder, with
// what the target has now; takes it out once the link is gone; and
// replaces a folder with a file, or a file with a folder, when what the
// link leads to changes.
func TestAgentImportLinkPrune(t *testing.T) {
	im := newImporter(t)
	home := im.Dirs.Home
	im.FollowLinks = true
	write(t, home, map[string]string{
		"dotfiles/x/SKILL.md": "# x", "dotfiles/x/old.md": "old", "dotfiles/y/SKILL.md": "# y", "dotfiles/f.md": "f",
		".claude/settings.json": "{}",
	})
	link(t, home, ".claude/skills/x", filepath.Join(home, "dotfiles/x"))
	link(t, home, ".claude/skills/y", filepath.Join(home, "dotfiles/y"))
	link(t, home, ".claude/skills/z", filepath.Join(home, "dotfiles/f.md"))
	run := func() {
		t.Helper()
		if err := im.Run(t.Context(), []string{"claude"}, true); err != nil {
			t.Fatal(msg.NamesOf(err))
		}
	}
	run()
	store := filepath.Join(agentStore(im.Dirs, "claude"), ".claude/skills")
	before, err := os.Stat(filepath.Join(store, "x"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(home, "dotfiles/x/old.md")); err != nil {
		t.Fatal(err)
	}
	write(t, home, map[string]string{"dotfiles/x/SKILL.md": "# x2", "dotfiles/z/SKILL.md": "# z"})
	for n, target := range map[string]string{"y": "dotfiles/f.md", "z": "dotfiles/z"} {
		if err := os.Remove(filepath.Join(home, ".claude/skills", n)); err != nil {
			t.Fatal(err)
		}
		link(t, home, ".claude/skills/"+n, filepath.Join(home, target))
	}
	run()
	if readFile(t, filepath.Join(store, "x/SKILL.md")) != "# x2" {
		t.Error("the copy isn't replaced")
	}
	if _, err := os.Lstat(filepath.Join(store, "x/old.md")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a file the target lost stays: %v", err)
	}
	if after, err := os.Stat(filepath.Join(store, "x")); err != nil || !os.SameFile(before, after) {
		t.Errorf("the copied folder was made again: %v", err)
	}
	if !isFile(filepath.Join(store, "y")) || !isFile(filepath.Join(store, "z/SKILL.md")) {
		t.Error("a folder that became a file, or a file a folder, isn't replaced")
	}
	if err := os.Remove(filepath.Join(home, ".claude/skills/x")); err != nil {
		t.Fatal(err)
	}
	run()
	if _, err := os.Lstat(filepath.Join(store, "x")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the copy of a link that's gone stays: %v", err)
	}
}

// An agent's folder that is itself a link, such as into a dotfiles repo,
// is read as the folder it leads to, so its links to files inside it are
// copied without a question, and a link from there to a sibling folder in
// the repo still leads out of it.
func TestAgentImportLinkedAgentFolder(t *testing.T) {
	im := newImporter(t)
	home := im.Dirs.Home
	write(t, home, map[string]string{
		"dotfiles/opencode/opencode.json": "{}",
		"dotfiles/opencode/shared.md":     "notes",
		"dotfiles/other/AGENTS.md":        "other",
	})
	link(t, home, "dotfiles/opencode/AGENTS.md", "shared.md")
	link(t, home, "dotfiles/opencode/RULES.md", "../other/AGENTS.md")
	link(t, home, ".config/opencode", filepath.Join(home, "dotfiles/opencode"))
	if err := im.Run(t.Context(), []string{"opencode"}, false); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	store := filepath.Join(agentStore(im.Dirs, "opencode"), ".config/opencode")
	if !isFile(filepath.Join(store, "AGENTS.md")) || readFile(t, filepath.Join(store, "AGENTS.md")) != "notes" || !isFile(filepath.Join(store, "opencode.json")) {
		t.Errorf("the folder's files aren't copied: %q", im.got)
	}
	if !im.notFollowed(filepath.Join(home, ".config/opencode/RULES.md")) {
		t.Errorf("a link out of the folder it leads to isn't asked about: %q", im.got)
	}
}

// A link whose chain passes through a link in a VM's project folder to a
// place outside that folder is left out, naming the VM, even with
// --follow-links or in a follow-links folder: the VM chose where it
// leads. One whose target stays in the project folder is followed, with
// the project warning.
func TestAgentImportLinkThroughProject(t *testing.T) {
	for _, c := range []struct {
		name     string
		root     bool
		inside   bool // the VM's link leads within its own folder
		followed bool
	}{
		{name: "--follow-links, out of the project", followed: false},
		{name: "follow-links, out of the project", root: true, followed: false},
		{name: "--follow-links, within the project", inside: true, followed: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			im := newImporter(t)
			home := im.Dirs.Home
			im.FollowLinks = !c.root
			write(t, home, map[string]string{"proj/real/SKILL.md": "# real", "elsewhere/x/secret": "s"})
			vmState(t, im.Dirs, "devvm-proj-0123456789abcd", func(st *state.VM) error {
				st.Dir = filepath.Join(home, "proj")
				return nil
			})
			if c.root {
				followLinksIn(t, im.Dirs, "~/elsewhere")
			}
			// what the VM put in its folder
			if c.inside {
				link(t, home, "proj/skill", "real")
			} else {
				link(t, home, "proj/skill", filepath.Join(home, "elsewhere/x"))
			}
			link(t, home, ".claude/skills/x", filepath.Join(home, "proj/skill"))
			if err := im.Run(t.Context(), []string{"claude"}, false); err != nil {
				t.Fatal(msg.NamesOf(err))
			}
			p := filepath.Join(home, ".claude/skills/x")
			copied := isDir(filepath.Join(agentStore(im.Dirs, "claude"), ".claude/skills/x"))
			if copied != c.followed {
				t.Errorf("copied = %v: %q", copied, im.got)
			}
			if !c.followed && !slices.Contains(im.got, "agent_import_link_via_vm path="+p+" vm=devvm-proj-0123456789abcd") {
				t.Errorf("no warning: %q", im.got)
			}
			if c.followed && !im.said("agent_import_link_project", p) {
				t.Errorf("no project warning: %q", im.got)
			}
		})
	}
}

// A refused place spelled in another case, on a file system where case
// doesn't count, is still refused.
func TestAgentImportRefusedOtherCase(t *testing.T) {
	im := newImporter(t)
	home := im.Dirs.Home
	im.FollowLinks = true
	write(t, home, map[string]string{".ssh/id_ed25519": "key", ".claude/settings.json": "{}"})
	if _, err := os.Stat(filepath.Join(home, ".SSH")); err != nil {
		t.Skip("this file system counts case")
	}
	link(t, home, ".claude/skills/k", filepath.Join(home, ".SSH"))
	if err := im.Run(t.Context(), []string{"claude"}, false); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if _, err := os.Lstat(filepath.Join(agentStore(im.Dirs, "claude"), ".claude/skills/k")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("copied: %v", err)
	}
	if !im.said("agent_import_link_refused", filepath.Join(home, ".claude/skills/k")) {
		t.Errorf("no warning: %q", im.got)
	}
}

// linkHops resolves a path as EvalSymlinks does, naming each link on the
// way, relative, absolute and in the middle of the path, and fails on a
// loop.
func TestLinkHops(t *testing.T) {
	dir := paths.Physical(t.TempDir())
	write(t, dir, map[string]string{"a/b/f": "f"})
	link(t, dir, "rel", "a")
	link(t, dir, "abs", filepath.Join(dir, "rel"))
	link(t, dir, "a/up", "../abs/b")
	got, hops, err := linkHops(filepath.Join(dir, "abs/up/f"))
	want, _ := filepath.EvalSymlinks(filepath.Join(dir, "abs/up/f"))
	if err != nil || got != want {
		t.Fatalf("linkHops = %q, %v, want %q", got, err, want)
	}
	wantHops := []string{filepath.Join(dir, "abs"), filepath.Join(dir, "rel"), filepath.Join(dir, "a/up"), filepath.Join(dir, "abs"), filepath.Join(dir, "rel")}
	if !slices.Equal(hops, wantHops) {
		t.Errorf("hops = %q, want %q", hops, wantHops)
	}
	link(t, dir, "loop", "loop")
	if _, _, err := linkHops(filepath.Join(dir, "loop")); err == nil {
		t.Error("a loop resolved")
	}
}

// openPhysical opens a folder one folder at a time, and fails on a link
// anywhere in its path rather than follow it.
func TestOpenPhysical(t *testing.T) {
	dir := paths.Physical(t.TempDir())
	write(t, dir, map[string]string{"real/sub/f": "f"})
	link(t, dir, "via", "real")
	r, err := openPhysical(filepath.Join(dir, "real/sub"))
	if err != nil {
		t.Fatal(err)
	}
	if b, err := r.Read("f", 10, safefile.Opts{}); err != nil || string(b) != "f" {
		t.Errorf("read %q, %v", b, err)
	}
	_ = r.Close()
	if r, err := openPhysical(filepath.Join(dir, "via/sub")); err == nil {
		_ = r.Close()
		t.Error("opened through a link")
	}
}
