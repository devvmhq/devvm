package vm

import (
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
		if err := e.cleanSeeds(t.Context(), []string{"claude"}); err != nil {
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
		if err := e.cleanSeeds(ctx, []string{"claude"}); err != nil || w.waits != 0 {
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
