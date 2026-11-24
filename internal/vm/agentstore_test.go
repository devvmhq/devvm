package vm

import (
	"archive/tar"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/agentlogin"
	"github.com/devvmhq/devvm/internal/agents"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/secrets"
	"github.com/devvmhq/devvm/internal/state"
)

// holdStoreLock holds the agent store's lock, as another devvm writing
// the store would: the file agents.lock in devvm's data folder. The test
// lets it go once its code under test waits, under a status line
// (OnStatus), checking that nothing of the store changed meanwhile.
func holdStoreLock(t *testing.T, d paths.Dirs) (unlock func()) {
	t.Helper()
	if err := os.MkdirAll(d.Data, 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, err := platform.LockFile(t.Context(), filepath.Join(d.Data, "agents.lock"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(unlock)
	return unlock
}

// waitedFor records the status lines that say a devvm waits for the
// store's lock, letting it go at the first (unlock), when whether the
// store changed by then is unchanged's.
type waitedFor struct {
	waits     int
	unchanged bool
}

func (w *waitedFor) on(unlock func(), unchanged func() bool) func(msg.M) {
	return func(m msg.M) {
		if m.ID() != "agent_store_waiting" {
			return
		}
		if w.waits++; w.waits == 1 {
			w.unchanged = unchanged()
			unlock()
		}
	}
}

// Whatever writes devvm's agent store, an import, a save and a start's
// clean of a seed's folder, holds the store's lock while it writes: one
// that another devvm holds is waited for under a status line, with the
// store left as it was until it's let go, and then the write goes on.
// A lock that's free shows no status line.
func TestAgentStoreLock(t *testing.T) {
	t.Run("import", func(t *testing.T) {
		im := newImporter(t)
		store := filepath.Join(agentStore(im.Dirs, "codex"), ".codex", "config.toml")
		write(t, im.Dirs.Home, map[string]string{".codex/config.toml": "new"})
		var w waitedFor
		im.fake.OnStatus = w.on(holdStoreLock(t, im.Dirs), func() bool { _, err := os.Lstat(store); return errors.Is(err, fs.ErrNotExist) })
		if err := im.Run(t.Context(), []string{"codex"}, true); err != nil {
			t.Fatal(msg.NamesOf(err))
		}
		if w.waits != 1 || !w.unchanged || readFile(t, store) != "new" {
			t.Errorf("held: waited %d times, store unchanged then %v, %q", w.waits, w.unchanged, im.got)
		}
		w = waitedFor{}
		write(t, im.Dirs.Home, map[string]string{".codex/config.toml": "again"})
		if err := im.Run(t.Context(), []string{"codex"}, true); err != nil || w.waits != 0 || readFile(t, store) != "again" {
			t.Errorf("free: %v, waited %d times", msg.NamesOf(err), w.waits)
		}
	})
	t.Run("save", func(t *testing.T) {
		st := newSaveTest(t, saveTar(t, true, te{name: ".claude/CLAUDE.md", data: "new"}), "claude")
		store := filepath.Join(st.store("claude"), ".claude", "CLAUDE.md")
		plans, err := st.plans([]string{"claude"})
		if err != nil {
			t.Fatal(err)
		}
		fake, _ := st.Reporter.(*msg.Fake)
		var w waitedFor
		fake.OnStatus = w.on(holdStoreLock(t, st.Dirs), func() bool { _, err := os.Lstat(store); return errors.Is(err, fs.ErrNotExist) })
		if err := st.applyAll(t.Context(), plans); err != nil {
			t.Fatal(msg.NamesOf(err))
		}
		if w.waits != 1 || !w.unchanged || readFile(t, store) != "new" {
			t.Errorf("held: waited %d times, store unchanged then %v, %q", w.waits, w.unchanged, st.got)
		}
	})
	t.Run("a seed's clean", func(t *testing.T) {
		root := t.TempDir()
		d := paths.Dirs{Config: filepath.Join(root, "config"), Data: filepath.Join(root, "data")}
		plugins := filepath.Join(agentStore(d, agents.Claude), ".claude/plugins")
		write(t, plugins, map[string]string{"known_marketplaces.json": "{}", "data/p/x": "this machine's"})
		var w waitedFor
		fake := &msg.Fake{}
		unlock := holdStoreLock(t, d)
		fake.OnStatus = w.on(unlock, func() bool { return isDir(filepath.Join(plugins, "data")) })
		e := &Env{Dirs: d, Reporter: fake}
		if err := e.cleanStore(t.Context(), []string{"claude"}); err != nil {
			t.Fatal(msg.NamesOf(err))
		}
		if w.waits != 1 || !w.unchanged || isDir(filepath.Join(plugins, "data")) {
			t.Errorf("held: waited %d times, store unchanged then %v", w.waits, w.unchanged)
		}
		unlock()
		// Nothing to clean takes no lock, so a start never waits for an import.
		w = waitedFor{}
		fake.OnStatus = w.on(holdStoreLock(t, d), func() bool { return true })
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := e.cleanStore(ctx, []string{"claude"}); err != nil || w.waits != 0 {
			t.Errorf("nothing to clean: %v, waited %d times", err, w.waits)
		}
	})
}

// A save reads the store again once it holds the store's lock, and
// saves nothing when another devvm changed it after the save planned,
// as while the user read the diff, so it never writes over that change;
// with the store as planned, it saves. Planning again says no warning
// twice.
func TestAgentSaveStoreChanged(t *testing.T) {
	for _, changed := range []bool{false, true} {
		st := newSaveTest(t, saveTar(t, true,
			te{name: ".claude/CLAUDE.md", data: "vm's"},
			te{name: ".claude/agents->/elsewhere"},
		), "claude")
		store := filepath.Join(st.store("claude"), ".claude", "CLAUDE.md")
		write(t, st.store("claude"), map[string]string{".claude/CLAUDE.md": "old"})
		plans, err := st.plans([]string{"claude"})
		if err != nil {
			t.Fatal(err)
		}
		warned := len(st.got)
		if changed {
			write(t, st.store("claude"), map[string]string{".claude/CLAUDE.md": "an import's"})
		}
		err = st.applyAll(t.Context(), plans)
		want := "vm's"
		if changed {
			want = "an import's"
			if msg.IDOf(err) != "agent_save_store_changed" {
				t.Errorf("changed: %v", msg.NamesOf(err))
			}
		} else if err != nil {
			t.Errorf("unchanged: %v", msg.NamesOf(err))
		}
		if got := readFile(t, store); got != want {
			t.Errorf("changed %v: the store's file %q, want %q", changed, got, want)
		}
		if again := slices.ContainsFunc(st.got[warned:], func(s string) bool { return strings.HasPrefix(s, "agent_save_link_left_out") }); again {
			t.Errorf("changed %v: warned again: %q", changed, st.got[warned:])
		}
	}
}

// An import whose store can't take what it staged, at a file's rename or
// at a login, leaves the store as it was: every agent's settings, the
// files it would have replaced and taken out, and every login, the ones
// it saved before the failure included. The control: the same import
// with nothing failing replaces them all.
func TestAgentImportApplyFailsWhole(t *testing.T) {
	for _, fail := range []string{"", "rename", "login"} {
		im := newImporter(t)
		home, claude, codex := im.Dirs.Home, agentStore(im.Dirs, "claude"), agentStore(im.Dirs, "codex")
		// The store's d a file, and swap.md a folder, which the home folder has the other way round.
		write(t, claude, map[string]string{".claude/CLAUDE.md": "old", ".claude/skills/old.md": "old", ".claude/skills/d": "old", ".claude/skills/swap.md/in": "old"})
		write(t, codex, map[string]string{".codex/config.toml": "old"})
		for _, a := range []string{"claude", "codex"} {
			if err := im.Secrets.SaveLogin(t.Context(), a, []byte(`{"old": "`+a+`"}`)); err != nil {
				t.Fatal(err)
			}
		}
		write(t, home, map[string]string{
			".claude/CLAUDE.md": "new", ".claude/skills/a.md": "a", ".claude/skills/d/f.md": "f", ".claude/skills/swap.md": "new", ".claude.json": `{"new": "claude"}`,
			".codex/config.toml": "new", ".codex/auth.json": `{"new": "codex"}`,
		})
		// Claude Code's settings go in place first, then Codex's fail.
		locked := filepath.Join(codex, ".codex")
		switch fail {
		case "rename":
			if os.Geteuid() == 0 {
				continue // root writes what it can't
			}
			// A folder of the store's that nothing can be put in.
			if err := os.Chmod(locked, 0o500); err != nil { //nolint:gosec // G302: a folder, read-only to its owner
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(locked, 0o700) }) //nolint:gosec // G302: a folder, writable again for its cleanup
		case "login":
			saved := 0
			im.Secrets.ExcludeFromBackup = func(context.Context, string) error {
				if saved++; saved == 2 {
					return errors.New("no more")
				}
				return nil
			}
		}
		err := im.Run(t.Context(), []string{"claude", "codex"}, true)
		if fail == "rename" {
			_ = os.Chmod(locked, 0o700) //nolint:gosec // G302: a folder, writable again
		}
		got := storeTree(t, claude, codex)
		var logins []string
		for _, a := range []string{"claude", "codex"} {
			b, _ := os.ReadFile(secrets.LoginFile(im.Dirs, a))
			logins = append(logins, string(b))
		}
		old := []string{`{"old": "claude"}`, `{"old": "codex"}`}
		want := []string{
			"claude/.claude", "claude/.claude/CLAUDE.md=new", "claude/.claude/skills", "claude/.claude/skills/a.md=a",
			"claude/.claude/skills/d", "claude/.claude/skills/d/f.md=f", "claude/.claude/skills/swap.md=new", "codex/.codex", "codex/.codex/config.toml=new",
		}
		if fail == "" {
			if err != nil || slices.ContainsFunc(logins, func(l string) bool { return slices.Contains(old, l) }) {
				t.Errorf("nothing fails: %v, logins %q", msg.NamesOf(err), logins)
			}
		} else {
			want = []string{
				"claude/.claude", "claude/.claude/CLAUDE.md=old", "claude/.claude/skills", "claude/.claude/skills/d=old", "claude/.claude/skills/old.md=old",
				"claude/.claude/skills/swap.md", "claude/.claude/skills/swap.md/in=old", "codex/.codex", "codex/.codex/config.toml=old",
			}
			if err == nil || !slices.Equal(logins, old) {
				t.Errorf("%s fails: %v, logins %q, want %q", fail, err, logins, old)
			}
		}
		if !slices.Equal(got, want) {
			t.Errorf("%q fails: the store has %q, want %q", fail, got, want)
		}
	}
}

// storeTree lists what's in each of these agents' stores, each path
// after its agent's name, a file's with its content: a staging folder
// left behind included.
func storeTree(t *testing.T, stores ...string) []string {
	t.Helper()
	var got []string
	for _, store := range stores {
		_ = filepath.WalkDir(store, func(p string, d fs.DirEntry, err error) error {
			if err == nil && p != store {
				rel, _ := filepath.Rel(store, p)
				rel = filepath.Base(store) + "/" + filepath.ToSlash(rel)
				if !d.IsDir() {
					rel += "=" + readFile(t, p)
				}
				got = append(got, rel)
			}
			return nil
		})
	}
	return got
}

// An import whose undo fails too, as a folder of Codex's store can't be
// written once the login fails, still puts back every other agent's
// store, Claude Code's here, and says so, naming the store it couldn't
// put back and its staging folder, which it keeps, as it holds what
// devvm couldn't put back; Claude Code's staging folder goes.
func TestAgentImportUndoFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes what it can't")
	}
	im := newImporter(t)
	claude, codex := agentStore(im.Dirs, "claude"), agentStore(im.Dirs, "codex")
	write(t, claude, map[string]string{".claude/CLAUDE.md": "old", ".claude/skills/old.md": "old"})
	write(t, codex, map[string]string{".codex/config.toml": "old"})
	write(t, im.Dirs.Home, map[string]string{
		".claude/CLAUDE.md": "new", ".claude/skills/a.md": "a", ".codex/config.toml": "new", ".codex/auth.json": `{"OPENAI_API_KEY": "k"}`,
	})
	locked := filepath.Join(codex, ".codex")
	im.Secrets.ExcludeFromBackup = func(context.Context, string) error {
		if err := os.Chmod(locked, 0o500); err != nil { //nolint:gosec // G302: a folder, read-only to its owner, so the undo can't put config.toml back
			t.Fatal(err)
		}
		return errors.New("no more")
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) }) //nolint:gosec // G302: a folder, writable again for its cleanup
	err := im.Run(t.Context(), []string{"claude", "codex"}, true)
	_ = os.Chmod(locked, 0o700) //nolint:gosec // G302: a folder, writable again
	if msg.IDOf(err) != "agent_import_undo_failed" || !strings.Contains(msg.NamesOf(err), " stores="+codex+" ") {
		t.Fatalf("undo failed: %v", msg.NamesOf(err))
	}
	if got, want := storeTree(t, claude), []string{"claude/.claude", "claude/.claude/CLAUDE.md=old", "claude/.claude/skills", "claude/.claude/skills/old.md=old"}; !slices.Equal(got, want) {
		t.Errorf("Claude Code's store %q, want it put back, %q", got, want)
	}
	kept, _ := filepath.Glob(filepath.Join(codex, stagePrefix+"*"))
	if len(kept) != 1 || !strings.Contains(msg.NamesOf(err), "dirs="+kept[0]+" ") {
		t.Errorf("the staging folder kept: %q, %v", kept, msg.NamesOf(err))
	}
	// The next import takes it out.
	im.Secrets.ExcludeFromBackup = func(context.Context, string) error { return nil }
	if err := im.Run(t.Context(), []string{"claude", "codex"}, true); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if left, _ := filepath.Glob(filepath.Join(codex, stagePrefix+"*")); len(left) != 0 {
		t.Errorf("the next import left %q", left)
	}
}

// newFailingSave is a save of Claude Code's and Codex's settings, in that
// order, into a store that has older ones: Claude Code's a changed file,
// a folder whose old.md the save takes out, and its .claude.json, a login;
// Codex's a changed file.
func newFailingSave(t *testing.T) (st *saveTest, plans []agentPlan) {
	t.Helper()
	st = newSaveTest(t, saveTar(t, true,
		te{name: ".claude/CLAUDE.md", data: "new"},
		te{name: ".claude/skills/"},
		te{name: ".claude/skills/a.md", data: "a"},
		te{name: saveOwn + "/claude.json", data: `{"theme": "light"}`},
		te{name: ".codex/AGENTS.md", data: "new"},
	), "claude", "codex")
	write(t, st.store("claude"), map[string]string{".claude/CLAUDE.md": "old", ".claude/skills/old.md": "old"})
	write(t, st.store("codex"), map[string]string{".codex/AGENTS.md": "old"})
	if err := st.Proxy.Secrets.SaveLogin(t.Context(), agents.Claude, []byte(`{"theme": "dark"}`)); err != nil {
		t.Fatal(err)
	}
	plans, err := st.plans([]string{"claude", "codex"})
	if err != nil || len(plans) != 2 || plans[0].claude == nil {
		t.Fatalf("plans: %v, %+v", err, plans)
	}
	return st, plans
}

// saveOldLogin is the login a failed save leaves (newFailingSave): as it
// was before it.
const saveOldLogin = `{"theme": "dark"}`

// saveOldStores are the stores a failed save leaves (newFailingSave), as
// storeTree lists them: as they were before it.
func saveOldStores() []string {
	return []string{"claude/.claude", "claude/.claude/CLAUDE.md=old", "claude/.claude/skills", "claude/.claude/skills/old.md=old", "codex/.codex", "codex/.codex/AGENTS.md=old"}
}

// A save whose store can't take what it staged, at a file's rename (Codex's,
// after Claude Code's settings went in place) or at the .claude.json
// login, saved last, leaves every agent's store and the login as they
// were, and no staging folder. The control: the same save with nothing
// failing puts them all in place.
func TestAgentSaveApplyFailsWhole(t *testing.T) {
	for _, fail := range []string{"", "rename", "login"} {
		st, plans := newFailingSave(t)
		claude, codex := st.store("claude"), st.store("codex")
		locked := filepath.Join(codex, ".codex")
		switch fail {
		case "rename":
			if os.Geteuid() == 0 {
				continue // root writes what it can't
			}
			if err := os.Chmod(locked, 0o500); err != nil { //nolint:gosec // G302: a folder, read-only to its owner
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(locked, 0o700) }) //nolint:gosec // G302: a folder, writable again for its cleanup
		case "login":
			st.Proxy.Secrets.ExcludeFromBackup = func(context.Context, string) error { return errors.New("no more") }
		}
		err := st.applyAll(t.Context(), plans)
		_ = os.Chmod(locked, 0o700) //nolint:gosec // G302: a folder, writable again
		login := readFile(t, secrets.LoginFile(st.Dirs, agents.Claude))
		want := []string{"claude/.claude", "claude/.claude/CLAUDE.md=new", "claude/.claude/skills", "claude/.claude/skills/a.md=a", "codex/.codex", "codex/.codex/AGENTS.md=new"}
		if fail == "" {
			if err != nil || login == saveOldLogin {
				t.Errorf("nothing fails: %v, login %s", msg.NamesOf(err), login)
			}
		} else {
			want = saveOldStores()
			if err == nil || login != saveOldLogin {
				t.Errorf("%s fails: %v, login %s", fail, err, login)
			}
		}
		if got := storeTree(t, claude, codex); !slices.Equal(got, want) {
			t.Errorf("%q fails: the store has %q, want %q", fail, got, want)
		}
	}
}

// A save never puts a file where the store has a folder, or a folder where
// it has a file, which would take out what the store has there without the
// diff showing it: it saves nothing and names the store's path, the
// store as it was. The control: the same save into a store without them.
func TestAgentSaveKindChanged(t *testing.T) {
	for _, c := range []struct {
		have      map[string]string
		id, under string // the message, and the store's path it names, relative to the store
	}{
		{have: nil},
		{have: map[string]string{".claude/CLAUDE.md/mine": "store's"}, id: "agent_save_store_folder", under: ".claude/CLAUDE.md"},
		{have: map[string]string{".claude/skills": "store's"}, id: "agent_save_store_file", under: ".claude/skills"},
	} {
		have := c.have
		st := newSaveTest(t, saveTar(t, true,
			te{name: ".claude/CLAUDE.md", data: "new"},
			te{name: ".claude/skills/"},
			te{name: ".claude/skills/a.md", data: "a"},
		), "claude")
		store := st.store("claude")
		write(t, store, have)
		before := storeTree(t, store)
		plans, err := st.plans([]string{"claude"})
		if err != nil {
			t.Fatal(err)
		}
		err = st.applyAll(t.Context(), plans)
		if have == nil {
			if err != nil {
				t.Errorf("no swap: %v", msg.NamesOf(err))
			}
			continue
		}
		if want := c.id + " path=" + filepath.Join(store, filepath.FromSlash(c.under)) + " vm=vm"; msg.NamesOf(err) != want {
			t.Errorf("%v: %v, want %s", have, msg.NamesOf(err), want)
		}
		if got := storeTree(t, store); !slices.Equal(got, before) {
			t.Errorf("%v: the store changed: %q, was %q", have, got, before)
		}
	}
}

// A save whose undo fails too, as Codex's store can't be written once the
// login fails, still puts back Claude Code's store and says so
// (agent_save_undo_failed), naming the store it couldn't put back and its
// staging folder, which it keeps; the next save takes it out.
func TestAgentSaveUndoFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes what it can't")
	}
	st, plans := newFailingSave(t)
	claude, codex := st.store("claude"), st.store("codex")
	locked := filepath.Join(codex, ".codex")
	st.Proxy.Secrets.ExcludeFromBackup = func(context.Context, string) error {
		if err := os.Chmod(locked, 0o500); err != nil { //nolint:gosec // G302: a folder, read-only to its owner, so the undo can't put config.toml back
			t.Fatal(err)
		}
		return errors.New("no more")
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) }) //nolint:gosec // G302: a folder, writable again for its cleanup
	err := st.applyAll(t.Context(), plans)
	_ = os.Chmod(locked, 0o700) //nolint:gosec // G302: a folder, writable again
	if msg.IDOf(err) != "agent_save_undo_failed" || !strings.Contains(msg.NamesOf(err), " stores="+codex+" ") || !strings.Contains(msg.NamesOf(err), " vm=vm ") {
		t.Fatalf("undo failed: %v", msg.NamesOf(err))
	}
	if got, want := storeTree(t, claude), saveOldStores()[:4]; !slices.Equal(got, want) {
		t.Errorf("Claude Code's store %q, want it put back, %q", got, want)
	}
	kept, _ := filepath.Glob(filepath.Join(codex, stagePrefix+"*"))
	if len(kept) != 1 || !strings.Contains(msg.NamesOf(err), "dirs="+kept[0]+" ") {
		t.Errorf("the staging folder kept: %q, %v", kept, msg.NamesOf(err))
	}
	st.Proxy.Secrets.ExcludeFromBackup = func(context.Context, string) error { return nil }
	s, err := st.stage(plans[1])
	if err != nil {
		t.Fatal(err)
	}
	if left, _ := filepath.Glob(filepath.Join(codex, stagePrefix+"*")); len(left) != 1 || left[0] == kept[0] {
		t.Errorf("the next save's stage left %q, want only its own", left)
	}
	s.discard()
}

// A save writes the .claude.json login under its lock, the one a VM's
// proxy holds while it refreshes a login: held, the save waits under a
// status line (agent_login_waiting), the login as it was meanwhile, then
// saves. A Ctrl-C while it waits ends the wait, and the save leaves the
// stores as they were.
func TestAgentSaveLoginLock(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		st, plans := newFailingSave(t)
		file := secrets.LoginFile(st.Dirs, agents.Claude)
		unlock, err := agentlogin.LockLogin(t.Context(), file, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(unlock)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		waits, before := 0, ""
		fake, _ := st.Reporter.(*msg.Fake)
		fake.OnStatus = func(m msg.M) {
			if m.ID() != "agent_login_waiting" {
				return
			}
			if waits++; waits == 1 {
				before = readFile(t, file)
				if cancelled {
					cancel()
				} else {
					unlock()
				}
			}
		}
		err = st.applyAll(ctx, plans)
		after := readFile(t, file)
		if waits != 1 || before != saveOldLogin {
			t.Errorf("cancelled %v: waited %d times, the login then %s", cancelled, waits, before)
		}
		if cancelled {
			got := storeTree(t, st.store("claude"), st.store("codex"))
			if !errors.Is(err, context.Canceled) || after != saveOldLogin || !slices.Equal(got, saveOldStores()) {
				t.Errorf("cancelled: %v, login %s, stores %q", err, after, got)
			}
		} else if err != nil || after == saveOldLogin {
			t.Errorf("let go: %v, login %s", msg.NamesOf(err), after)
		}
	}
}

// foldsCase skips the test unless its temp folders are on a volume that
// ignores the case of names, as macOS's APFS does by default.
func foldsCase(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	write(t, dir, map[string]string{"probe": ""})
	if _, err := os.Lstat(filepath.Join(dir, "PROBE")); err != nil {
		t.Skip("this volume tells names that differ only in case apart")
	}
}

// On a volume that ignores case, a save compares the VM's names with the
// store's as the volume does: the VM's foo.md is the store's Foo.md, and
// its sub folder the store's Sub, so each is a change, never a file added
// and another removed, and the store keeps what the VM sent, under the
// store's names, while what the VM's folder lost goes.
func TestAgentSaveCaseOnly(t *testing.T) {
	foldsCase(t)
	st := newSaveTest(t, saveTar(t, true,
		te{name: ".claude/skills/"},
		te{name: ".claude/skills/foo.md", data: "new"},
		te{name: ".claude/skills/sub/"},
		te{name: ".claude/skills/sub/a.md", data: "a2"},
		te{name: ".claude/skills/ph.md", data: "devvm-redacted"}, // left out, so the store keeps its PH.md
	), "claude")
	store := st.store("claude")
	write(t, store, map[string]string{".claude/skills/Foo.md": "old", ".claude/skills/Sub/a.md": "a", ".claude/skills/Sub/gone.md": "gone", ".claude/skills/PH.md": "the store's"})
	plans, err := st.plans([]string{"claude"})
	if err != nil || len(plans) != 1 || len(plans[0].folders) != 1 {
		t.Fatalf("plans = %+v, %v", plans, err)
	}
	d := plans[0].folders[0]
	if len(d.added) != 0 || !slices.Equal(d.changed, []string{"foo.md", "sub/a.md"}) || !slices.Equal(d.removed, []string{"Sub/gone.md"}) {
		t.Errorf("added %q, changed %q, removed %q", d.added, d.changed, d.removed)
	}
	if err := st.applyAll(t.Context(), plans); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	want := []string{
		"claude/.claude", "claude/.claude/skills", "claude/.claude/skills/Foo.md=new", "claude/.claude/skills/PH.md=the store's",
		"claude/.claude/skills/Sub", "claude/.claude/skills/Sub/a.md=a2",
	}
	if got := storeTree(t, store); !slices.Equal(got, want) {
		t.Errorf("the store has %q, want %q", got, want)
	}
	if p, err := st.plan("claude"); err != nil || p.changed() {
		t.Errorf("plan again = %+v, %v", p, err)
	}
}

// On a volume that ignores case, a store's file with a second name, a
// hard link only a hand-made store has, is never the VM's file of the
// same name but its case: the diff shows the VM's foo.md added and both
// of the store's names removed, more than the save takes out (Foo.md
// gets the VM's foo.md in place), never less, as when it showed a change
// to Foo.md and the save took out Zed.md.
func TestAgentSaveCaseOnlyHardLink(t *testing.T) {
	foldsCase(t)
	st := newSaveTest(t, saveTar(t, true, te{name: ".claude/skills/"}, te{name: ".claude/skills/foo.md", data: "new"}), "claude")
	store := st.store("claude")
	write(t, store, map[string]string{".claude/skills/Foo.md": "old"})
	if err := os.Link(filepath.Join(store, ".claude/skills/Foo.md"), filepath.Join(store, ".claude/skills/Zed.md")); err != nil {
		t.Fatal(err)
	}
	plans, err := st.plans([]string{"claude"})
	if err != nil || len(plans) != 1 || len(plans[0].folders) != 1 {
		t.Fatalf("plans = %+v, %v", plans, err)
	}
	d := plans[0].folders[0]
	if !slices.Equal(d.added, []string{"foo.md"}) || len(d.changed) != 0 || !slices.Equal(d.removed, []string{"Foo.md", "Zed.md"}) {
		t.Errorf("added %q, changed %q, removed %q", d.added, d.changed, d.removed)
	}
	if err := st.applyAll(t.Context(), plans); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if got, want := storeTree(t, store), []string{"claude/.claude", "claude/.claude/skills", "claude/.claude/skills/Foo.md=new"}; !slices.Equal(got, want) {
		t.Errorf("the store has %q, want %q", got, want)
	}
}

// On a volume that ignores case, two names in the VM that differ only in
// case, files, folders or a file the save leaves out, would be one in the
// store, so a save of them fails, naming both, before the store changes.
func TestAgentSaveNamesCollide(t *testing.T) {
	foldsCase(t)
	for _, c := range []struct {
		es   []te
		a, b string
	}{
		{es: []te{{name: ".claude/skills/A.md", data: "1"}, {name: ".claude/skills/a.md", data: "2"}}, a: ".claude/skills/A.md", b: ".claude/skills/a.md"},
		{es: []te{{name: ".claude/skills/D/"}, {name: ".claude/skills/D/x.md", data: "1"}, {name: ".claude/skills/d/"}, {name: ".claude/skills/d/y.md", data: "2"}}, a: ".claude/skills/D", b: ".claude/skills/d"},
		{es: []te{{name: ".claude/skills/f.md", data: "1"}, {name: ".claude/skills/F.md", kind: tar.TypeFifo}}, a: ".claude/skills/f.md", b: ".claude/skills/F.md"},
		{es: []te{{name: ".claude/skills/E/"}, {name: ".claude/skills/e/"}}, a: ".claude/skills/E", b: ".claude/skills/e"}, // empty folders
	} {
		st := newSaveTest(t, saveTar(t, true, append([]te{{name: ".claude/skills/"}}, c.es...)...), "claude")
		store := st.store("claude")
		write(t, store, map[string]string{".claude/skills/F.md": "the store's", ".claude/skills/a.md": "the store's"})
		before := storeTree(t, store)
		plans, err := st.plans([]string{"claude"})
		if err != nil {
			t.Fatal(err)
		}
		err = st.applyAll(t.Context(), plans)
		if want := "agent_save_names_collide a=" + c.a + " b=" + c.b + " vm=vm"; msg.NamesOf(err) != want {
			t.Errorf("%s: %v, want %s", c.a, msg.NamesOf(err), want)
		}
		if got := storeTree(t, store); !slices.Equal(got, before) {
			t.Errorf("%s: the store changed: %q, was %q", c.a, got, before)
		}
	}
}

// On a volume that ignores case, an import whose file differs from the
// store's only in case replaces the store's, keeping the store's name,
// rather than taking it out with what the home folder no longer has.
func TestAgentImportCaseOnly(t *testing.T) {
	foldsCase(t)
	im := newImporter(t)
	store := agentStore(im.Dirs, "claude")
	write(t, store, map[string]string{".claude/skills/Foo.md": "old", ".claude/skills/Sub/a.md": "a", ".claude/skills/gone.md": "gone"})
	write(t, im.Dirs.Home, map[string]string{".claude/skills/foo.md": "new", ".claude/skills/sub/a.md": "a2"})
	if err := im.Run(t.Context(), []string{"claude"}, true); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	want := []string{"claude/.claude", "claude/.claude/skills", "claude/.claude/skills/Foo.md=new", "claude/.claude/skills/Sub", "claude/.claude/skills/Sub/a.md=a2"}
	if got := storeTree(t, store); !slices.Equal(got, want) {
		t.Errorf("the store has %q, want %q", got, want)
	}
}

// An import's two names that the store's volume reads as one, as A.md and
// a.md would be from a home folder on a volume that tells case apart,
// fail it while it stages them, naming both in the home folder, before
// the store changes. The home folder here ignores case too, so the second
// name is added to what the import listed, read as the first.
func TestAgentImportNamesCollide(t *testing.T) {
	foldsCase(t)
	for _, c := range []struct {
		file bool // a second file's name, or a second folder's
		a, b string
	}{
		{file: true, a: ".claude/skills/a.md", b: ".claude/skills/A.md"},
		{a: ".claude/skills/B", b: ".claude/skills/b"},
	} {
		im := newImporter(t)
		write(t, im.Dirs.Home, map[string]string{".claude/skills/a.md": "a", ".claude/skills/B/x.md": "x"})
		h, err := im.read(t.Context(), "claude", nil)
		if err != nil {
			t.Fatal(err)
		}
		if c.file {
			h.files = append(h.files, "skills/A.md")
			h.listed["skills/A.md"] = h.listed["skills/a.md"]
		} else {
			h.dirs = append(h.dirs, "skills/b")
		}
		s, err := im.stage(h)
		if s != nil {
			s.discard()
		}
		h.close()
		home := func(rel string) string { return filepath.Join(im.Dirs.Home, filepath.FromSlash(rel)) }
		if got, want := msg.NamesOf(err), "agent_import_names_collide a="+home(c.a)+" b="+home(c.b); got != want {
			t.Errorf("%s: %s, want %s", c.b, got, want)
		}
		if got := storeTree(t, agentStore(im.Dirs, "claude")); len(got) != 0 {
			t.Errorf("%s: the store has %q", c.b, got)
		}
	}
}

// Staging and pruning go by what a name is on the store's volume, on any
// volume: a second name of a file the stage wrote, as a hard link stands
// in for a name the volume reads as another here, is a clash (claim), and
// something there the stage didn't write is an error of another kind. A
// file with a second name is kept only by the name keep has (pruneIn), so
// the other goes, as the save's diff shows it (storeFiles).
func TestStoreNamesByWhatTheyAre(t *testing.T) {
	im := newImporter(t)
	s, err := newStage(im.Reporter, im.Dirs.Config, agentStore(im.Dirs, "claude"), "claude")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.discard)
	at := func(rel string) string { return filepath.Join(s.tmp, filepath.FromSlash(rel)) }
	if err := s.claim("d/x", true); err != nil {
		t.Fatal(err)
	}
	if err := state.WriteFileIn(s.root, at("d/x"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.root.Link(at("d/x"), at("d/y")); err != nil {
		t.Fatal(err)
	}
	if err := s.claim("d/x", true); err != nil {
		t.Errorf("a name staged again: %v", err)
	}
	if err := s.claim("d/y", false); !errors.Is(err, nameClash{a: "d/x", b: "d/y"}) {
		t.Errorf("a second name: %v", err)
	}
	if err := s.root.Mkdir(at("e"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.claim("e/z", true); err == nil {
		t.Error("what the stage didn't write: no error")
	} else if _, clash := errors.AsType[nameClash](err); clash {
		t.Errorf("what the stage didn't write: a clash, %v", err)
	}

	var taken []string
	folder, err := fs.Sub(s.root.FS(), s.tmp)
	if err != nil {
		t.Fatal(err)
	}
	if err := pruneIn(folder, map[string]bool{"d": true, "d/x": true}, "", func(p string) error { taken = append(taken, p); return nil }); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(taken, []string{"d/y", "e"}) {
		t.Errorf("pruned %q, want d/y and e", taken)
	}
}

// An interrupted run's cleanup never takes the staging folder out while
// the apply runs: the apply holds the stage's lock throughout, its logins'
// save included. A cleanup that ran first, before the apply, leaves it
// nothing to put in place, and the store as it was.
func TestAgentImportInterruptedApply(t *testing.T) {
	setup := func(t *testing.T) (*importer, *storeStage, *hostAgent, *[]func(context.Context)) {
		im := newImporter(t)
		write(t, agentStore(im.Dirs, "codex"), map[string]string{".codex/config.toml": "old"})
		write(t, im.Dirs.Home, map[string]string{".codex/config.toml": "new", ".codex/auth.json": `{"OPENAI_API_KEY": "k"}`})
		var cleanups []func(context.Context)
		im.fake.OnCleanup = func(f func(context.Context)) func() {
			cleanups = append(cleanups, f)
			return func() {}
		}
		im.fake.Interrupt = true
		h, err := im.read(t.Context(), "codex", nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(h.close)
		s, err := im.stage(h)
		if err != nil || len(cleanups) != 1 {
			t.Fatalf("stage: %v, %d cleanups", err, len(cleanups))
		}
		t.Cleanup(s.discard)
		return im, s, h, &cleanups
	}
	t.Run("during", func(t *testing.T) {
		im, s, h, cleanups := setup(t)
		held := false
		im.Secrets.ExcludeFromBackup = func(context.Context, string) error {
			if s.mu.TryLock() {
				s.mu.Unlock() // not held: let go, so the cleanup below doesn't wait for ever
			} else {
				held = true
			}
			return nil
		}
		if err := im.applyAll(t.Context(), []*storeStage{s}, []*hostAgent{h}); err != nil || !held {
			t.Errorf("applied: %v, the stage's lock held through the logins %v", msg.NamesOf(err), held)
		}
		(*cleanups)[0](t.Context())
		if left, _ := filepath.Glob(filepath.Join(agentStore(im.Dirs, "codex"), stagePrefix+"*")); len(left) != 0 {
			t.Errorf("the cleanup left %q", left)
		}
	})
	t.Run("first", func(t *testing.T) {
		im, s, h, cleanups := setup(t)
		(*cleanups)[0](t.Context())
		if err := im.applyAll(t.Context(), []*storeStage{s}, []*hostAgent{h}); !errors.Is(err, context.Canceled) {
			t.Errorf("applied after the cleanup: %v", err)
		}
		if got := readFile(t, filepath.Join(agentStore(im.Dirs, "codex"), ".codex/config.toml")); got != "old" || im.Secrets.HasLogin("codex") {
			t.Errorf("the store's file %q, a login saved %v", got, im.Secrets.HasLogin("codex"))
		}
	})
}
