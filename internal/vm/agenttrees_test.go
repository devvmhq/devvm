package vm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/devvmhq/devvm/internal/agents"
	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/backend/backendtest"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/project"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/vmname"
)

// opencodeTrees is opencode's one pattern, written out from its source,
// not read from the registry.
func opencodeTrees() []treePattern {
	return []treePattern{{agent: "opencode", pattern: ".local/share/opencode/worktree/*/*"}}
}

// The script finds each git worktree opencode's pattern matches in the
// home folder, with its status and a detached HEAD's commit that nothing
// else has, skips a folder that isn't one, and past the most says more.
func TestAgentTreesState(t *testing.T) {
	gitEnv(t)
	home, repo := t.TempDir(), t.TempDir()
	gitIn(t, repo, "init", "-q", "-b", "main")
	gitIn(t, repo, "commit", "-q", "--allow-empty", "-m", "one")
	root := filepath.Join(home, agents.OpencodeData, "worktree")
	gitIn(t, repo, "worktree", "add", "-q", "-b", "a", filepath.Join(root, "abc123", "clean"))
	dirty := filepath.Join(root, "abc123", "dirty")
	gitIn(t, repo, "worktree", "add", "-q", "-b", "b", dirty)
	if err := os.WriteFile(filepath.Join(dirty, "new"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	detached := filepath.Join(root, "def456", "detached")
	gitIn(t, repo, "worktree", "add", "-q", "--detach", detached)
	gitIn(t, detached, "commit", "-q", "--allow-empty", "-m", "its own")
	if err := os.MkdirAll(filepath.Join(root, "def456", "plain"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Names that start with a dot, which a shell's * passes over.
	hidden := filepath.Join(root, ".ghi789", ".hidden")
	gitIn(t, repo, "worktree", "add", "-q", "-b", "c", hidden)
	if err := os.WriteFile(filepath.Join(hidden, "new"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(lim int) ([]agentTree, bool) {
		t.Helper()
		// sh, as the VM runs it: dash on Debian, BusyBox's on Alpine.
		globs, of := treeGlobs(opencodeTrees())
		cmd := exec.CommandContext(t.Context(), "sh", append([]string{"-c", agentTreesState, "sh", fmt.Sprint(lim)}, globs...)...) //nolint:gosec // the test's own
		cmd.Env = append(os.Environ(), "HOME="+home)
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		trees, more, err := parseAgentTrees(out, of)
		if err != nil {
			t.Fatalf("%v: %q", err, out)
		}
		return trees, more
	}
	trees, more := run(cloneWorktreesMax)
	got := map[string]agentTree{}
	for _, w := range trees {
		got[w.path] = w
	}
	at := func(name string) string { return agents.OpencodeData + "/worktree/" + name }
	want := map[string]agentTree{
		at("abc123/clean"):    {agent: "opencode", path: at("abc123/clean"), status: []byte{}},
		at("abc123/dirty"):    {agent: "opencode", path: at("abc123/dirty"), status: []byte("?? new\n")},
		at("def456/detached"): {agent: "opencode", path: at("def456/detached"), status: []byte{}, head: gitIn(t, detached, "rev-parse", "HEAD")},
		at(".ghi789/.hidden"): {agent: "opencode", path: at(".ghi789/.hidden"), status: []byte("?? new\n")},
	}
	if more || len(got) != len(want) {
		t.Fatalf("got %+v, more %v", trees, more)
	}
	for p, w := range want {
		g := got[p]
		if g.agent != w.agent || string(g.status) != string(w.status) || g.head != w.head || g.failed {
			t.Errorf("%s: %+v, want %+v", p, g, w)
		}
	}
	if trees, more := run(1); len(trees) != 1 || !more {
		t.Errorf("past the most: %+v, more %v", trees, more)
	}
}

// The VM's answer is untrusted: each record whole and of a pattern asked
// for, each path clean, matching its pattern and once, followed by its
// status, at most cloneWorktreesMax of them, and nothing after more.
func TestParseAgentTrees(t *testing.T) {
	sha := strings.Repeat("a", 40)
	w := ".local/share/opencode/worktree/abc/x"
	good := rec("path", 0, w) + rec("whead", 0, sha+"\n") + rec("wstatus", 0, "?? new\n") +
		rec("path", 0, ".local/share/opencode/worktree/abc/y") + rec("wfail", 0, "") + rec("more", 0, "")
	trees, more, err := parseAgentTrees([]byte(good), opencodeTrees())
	if err != nil || !more || len(trees) != 2 || trees[0].head != sha || string(trees[0].status) != "?? new\n" || !trees[1].failed {
		t.Fatalf("good: %+v, %v, %v", trees, more, err)
	}
	var many strings.Builder
	for i := range cloneWorktreesMax + 1 {
		many.WriteString(rec("path", 0, fmt.Sprintf(".local/share/opencode/worktree/abc/w%d", i)) + rec("wstatus", 0, ""))
	}
	for name, bad := range map[string]string{
		"another pattern's":  rec("path", 1, w) + rec("wstatus", 1, ""),
		"outside it":         rec("path", 0, ".ssh/x") + rec("wstatus", 0, ""),
		"deeper":             rec("path", 0, w+"/z") + rec("wstatus", 0, ""),
		"through ..":         rec("path", 0, ".local/share/opencode/worktree/../..") + rec("wstatus", 0, ""),
		"absolute":           rec("path", 0, "/"+w) + rec("wstatus", 0, ""),
		"twice":              rec("path", 0, w) + rec("wstatus", 0, "") + rec("path", 0, w) + rec("wstatus", 0, ""),
		"no status":          rec("path", 0, w),
		"two paths":          rec("path", 0, w) + rec("path", 0, w+"2") + rec("wstatus", 0, ""),
		"status alone":       rec("wstatus", 0, ""),
		"head alone":         rec("whead", 0, sha),
		"two heads":          rec("path", 0, w) + rec("whead", 0, sha) + rec("whead", 0, sha) + rec("wstatus", 0, ""),
		"a head not a sha":   rec("path", 0, w) + rec("whead", 0, "HEAD~1") + rec("wstatus", 0, ""),
		"after more":         rec("more", 0, "") + rec("path", 0, w) + rec("wstatus", 0, ""),
		"more mid-worktree":  rec("path", 0, w) + rec("more", 0, "") + rec("wstatus", 0, ""),
		"an unknown kind":    rec("gone", 0, ""),
		"cut short":          rec("path", 0, w)[:10],
		"past the most":      many.String(),
		"a length too large": "path 0 99999999999\n",
	} {
		if trees, _, err := parseAgentTrees([]byte(bad), opencodeTrees()); err == nil {
			t.Errorf("%s: %+v", name, trees)
		}
	}
	if trees, more, err := parseAgentTrees(nil, opencodeTrees()); err != nil || more || len(trees) != 0 {
		t.Errorf("none: %+v, %v, %v", trees, more, err)
	}
}

// devvm rm checks opencode's worktrees in the VM first, as an export
// leaves them out: one with uncommitted changes is shown and asked about,
// which --force never answers, so without a terminal rm refuses, naming
// --no-worktree-check, and no keeps the VM. --no-worktree-check warns and
// doesn't look. A VM whose worktrees are clean, or without opencode, is
// deleted without the question.
func TestRmAgentTrees(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	lc := Lifecycle{e}
	st := existingVM(t, e, Options{Agents: &[]string{"opencode"}, Network: "open"})
	vm := st.Name()
	answer, looked := "", 0
	f.ExecFunc = func(_ vmname.VM, argv []string, o backend.ExecOpts) error {
		if len(argv) > 5 && argv[2] == agentTreesState {
			looked++
			w := ".local/share/opencode/worktree/"
			globs := []string{
				fmt.Sprint(cloneWorktreesMax),
				w + "*/*", w + "*/.[!.]*", w + "*/..?*",
				w + ".[!.]*/*", w + ".[!.]*/.[!.]*", w + ".[!.]*/..?*",
				w + "..?*/*", w + "..?*/.[!.]*", w + "..?*/..?*",
			}
			if !slices.Equal(argv[4:], globs) {
				return errors.New("unexpected patterns")
			}
			_, err := o.Stdout.Write([]byte(answer))
			return err
		}
		return nil
	}
	w := ".local/share/opencode/worktree/abc/x"
	answer = rec("path", 0, w) + rec("wstatus", 0, " M a\n")
	var said msg.Recorder
	fake(e).OnSay = said.Add
	err := lc.Rm(t.Context(), RmOptions{Force: true, NoExport: true})
	if msg.NamesOf(err) != "no_terminal_delete_trees command=devvm rm" || looked != 1 {
		t.Errorf("no terminal: %v, looked %d", msg.NamesOf(err), looked)
	}
	if want := []string{"agent_tree_dirty agent=opencode path=~/" + w}; !slices.Equal(said.Names(), want) {
		t.Errorf("said %q, want %q", said.Names(), want)
	}
	asked := answers(t, e, false)
	if err := lc.Rm(t.Context(), RmOptions{Force: true, NoExport: true}); err != nil || !slices.Equal(*asked, []string{"delete_anyway"}) {
		t.Errorf("no: %v, asked %q", msg.NamesOf(err), *asked)
	}
	if _, ok := f.Instances[vm]; !ok {
		t.Fatal("deleted")
	}
	// Each kind of work it would lose is asked about, and a failed or
	// hostile answer keeps the VM, naming --no-worktree-check.
	for name, tc := range map[string]struct{ answer, want string }{
		"detached": {rec("path", 0, w) + rec("whead", 0, strings.Repeat("b", 40)) + rec("wstatus", 0, ""), "no_terminal_delete_trees command=devvm rm"},
		"unread":   {rec("path", 0, w) + rec("wfail", 0, ""), "no_terminal_delete_trees command=devvm rm"},
		"too many": {rec("more", 0, ""), "no_terminal_delete_trees command=devvm rm"},
		"hostile":  {rec("path", 0, ".ssh") + rec("wstatus", 0, ""), "trees_check_failed command=devvm rm vm=" + vm.String()},
	} {
		answer = tc.answer
		fake(e).Terminal = false
		if err := lc.Rm(t.Context(), RmOptions{Force: true, NoExport: true}); msg.NamesOf(err) != tc.want {
			t.Errorf("%s: %v, want %s", name, msg.NamesOf(err), tc.want)
		}
	}
	looked = 0
	var warned []string
	fake(e).OnWarn = func(m msg.M) { warned = append(warned, m.Names()) }
	answer = rec("path", 0, w) + rec("wstatus", 0, "")
	if err := lc.Rm(t.Context(), RmOptions{Force: true, NoExport: true, NoWorktreeCheck: true}); err != nil || looked != 0 {
		t.Errorf("--no-worktree-check: %v, looked %d", msg.NamesOf(err), looked)
	}
	if want := []string{"trees_check_skipped command=devvm rm vm=" + vm.String()}; !slices.Equal(warned, want) {
		t.Errorf("warned %q, want %q", warned, want)
	}
	if _, ok := f.Instances[vm]; ok {
		t.Error("not deleted with --no-worktree-check")
	}

	// Clean worktrees: deleted without the question.
	st = existingVM(t, e, Options{Agents: &[]string{"opencode"}, Network: "open"})
	asked = answers(t, e, false)
	if err := lc.Rm(t.Context(), RmOptions{Force: true, NoExport: true}); err != nil || looked != 1 || len(*asked) > 0 {
		t.Errorf("clean: %v, looked %d, asked %q", msg.NamesOf(err), looked, *asked)
	}
	if _, ok := f.Instances[st.Name()]; ok {
		t.Error("clean: not deleted")
	}

	// Without opencode, nothing to look at.
	st = existingVM(t, e, Options{Agents: &[]string{"claude"}, Network: "open"})
	if err := lc.Rm(t.Context(), RmOptions{Force: true, NoExport: true}); err != nil || looked != 1 {
		t.Errorf("claude: %v, looked %d", msg.NamesOf(err), looked)
	}
	if _, ok := f.Instances[st.Name()]; ok {
		t.Error("claude: not deleted")
	}
}

// silentVM is a backend whose VM never answers a command: Exec waits for
// its context.
type silentVM struct{ *backendtest.Fake }

func (silentVM) Exec(ctx context.Context, _ vmname.VM, _ []string, _ backend.ExecOpts) error {
	<-ctx.Done()
	return ctx.Err()
}

// A VM that answers the worktrees' read without end has its command
// cancelled once the answer passes the most devvm reads, and one that
// never answers is given up on after treesWait; either keeps the VM,
// naming --no-worktree-check.
func TestAgentTreesBounded(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	vm := existingVM(t, e, Options{Agents: &[]string{"opencode"}, Network: "open"}).Name()
	tg := e.newTarget(project.Folder{Dir: e.Dir, Name: vm}, nil, f)
	check := workCheck{clones: clones{e}, command: "devvm rm", trees: true}
	want := "trees_check_failed command=devvm rm vm=" + vm.String()
	cancelled := false
	f.ExecFunc = func(_ vmname.VM, _ []string, o backend.ExecOpts) error {
		chunk := []byte(strings.Repeat("y\n", 1<<15))
		for written := 0; written < 4*cloneStateMax; written += len(chunk) {
			if _, err := o.Stdout.Write(chunk); err != nil {
				cancelled = true
				return err
			}
		}
		return nil
	}
	if _, err := check.agentTrees(t.Context(), tg, opencodeTrees()); msg.NamesOf(err) != want || !cancelled {
		t.Errorf("without end: %v, cancelled %v", msg.NamesOf(err), cancelled)
	}
	synctest.Test(t, func(t *testing.T) {
		silent := e.newTarget(project.Folder{Dir: e.Dir, Name: vm}, nil, silentVM{f})
		start := time.Now()
		_, err := check.agentTrees(t.Context(), silent, opencodeTrees())
		if msg.NamesOf(err) != want || time.Since(start) != treesWait {
			t.Errorf("no answer: %v after %v", msg.NamesOf(err), time.Since(start))
		}
	})
}

// devvm rm VM, of a VM whose folder is gone, checks opencode's worktrees
// in the running VM as devvm rm does. Starting a stopped one would make
// its folder again, so it's refused, naming both flags, even with
// --no-export, and with both it's deleted unstarted after a warning.
func TestRmVMAgentTrees(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	lc := Lifecycle{e}
	vm := existingVM(t, e, Options{Agents: &[]string{"opencode"}, Network: "open"}).Name()
	dir := e.Dir
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	e.Dir = t.TempDir()
	f.ExecFunc = func(_ vmname.VM, argv []string, o backend.ExecOpts) error {
		if len(argv) > 5 && argv[2] == agentTreesState {
			_, err := o.Stdout.Write([]byte(rec("path", 0, ".local/share/opencode/worktree/abc/x") + rec("wstatus", 0, "?? new\n")))
			return err
		}
		return nil
	}
	if err := lc.RmVM(t.Context(), vm.String(), RmOptions{Force: true, NoExport: true}); msg.NamesOf(err) != "no_terminal_delete_trees command=devvm rm "+vm.String() {
		t.Errorf("running: %v", msg.NamesOf(err))
	}
	in := f.Instances[vm]
	in.Status = backend.StatusStopped
	f.Instances[vm] = in
	for _, o := range []RmOptions{{Force: true}, {Force: true, NoExport: true}} {
		if err := lc.RmVM(t.Context(), vm.String(), o); msg.NamesOf(err) != "rm_gone_stopped_trees dir="+dir+" vm="+vm.String() {
			t.Errorf("stopped, %+v: %v", o, msg.NamesOf(err))
		}
	}
	if _, ok := f.Instances[vm]; !ok {
		t.Fatal("deleted")
	}
	var warned []string
	fake(e).OnWarn = func(m msg.M) { warned = append(warned, m.Names()) }
	n := len(f.Calls())
	if err := lc.RmVM(t.Context(), vm.String(), RmOptions{Force: true, NoExport: true, NoWorktreeCheck: true}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if _, ok := f.Instances[vm]; ok || slices.Contains(f.Methods()[n:], "Start") {
		t.Errorf("both flags: still there or started: %q", f.Methods()[n:])
	}
	if want := []string{"trees_check_skipped command=devvm rm " + vm.String() + " vm=" + vm.String()}; !slices.Equal(warned, want) {
		t.Errorf("warned %q, want %q", warned, want)
	}
}

// devvm update --clean-home of an outdated VM checks opencode's worktrees
// after its question and before anything changes; without --clean-home it
// doesn't look.
func TestUpdateAgentTrees(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	home := homeFiles(t, ".bash_history", "ls\n")
	looked := 0
	f.ExecFunc = func(vm vmname.VM, argv []string, o backend.ExecOpts) error {
		switch {
		case len(argv) > 5 && argv[2] == agentTreesState:
			looked++
			_, err := o.Stdout.Write([]byte(rec("path", 0, ".local/share/opencode/worktree/abc/x") + rec("wstatus", 0, "?? new\n")))
			return err
		case len(argv) == 3 && argv[2] == homeOut:
			_, err := o.Stdout.Write(home)
			return err
		}
		return noDirect(vm, argv, o)
	}
	existingVM(t, e, Options{Agents: &[]string{"opencode"}, Network: "allowlist"})
	if err := state.BumpRebuild(e.Dirs, f.Name(), "", "base"); err != nil {
		t.Fatal(err)
	}
	lc := Lifecycle{e}
	n := len(f.Calls())
	err := lc.Update(t.Context(), "devvm update", RemakeOptions{Force: true, CleanHome: true})
	if msg.NamesOf(err) != "no_terminal_delete_trees command=devvm update --clean-home" || looked != 1 {
		t.Errorf("--clean-home: %v, looked %d", msg.NamesOf(err), looked)
	}
	if slices.ContainsFunc(f.Methods()[n:], func(m string) bool { return m == "BuildImage" || m == "Stop" || m == "Rename" }) {
		t.Errorf("kept, then %q", f.Methods()[n:])
	}
	if err := lc.Update(t.Context(), "devvm update", RemakeOptions{Force: true}); err != nil || looked != 1 {
		t.Errorf("home kept: %v, looked %d", msg.NamesOf(err), looked)
	}
}

// devvm clone off --clean-home checks the clone and opencode's worktrees
// together: both shown, one question (remake_anyway), and without a
// terminal the clone's refusal, which asks for one.
func TestSetCloneAgentTrees(t *testing.T) {
	e, f, vmfs := cloneEnv(t)
	inner := f.ExecFunc
	looked := 0
	f.ExecFunc = func(vm vmname.VM, argv []string, o backend.ExecOpts) error {
		if len(argv) > 5 && argv[2] == agentTreesState {
			looked++
			_, err := o.Stdout.Write([]byte(rec("path", 0, ".local/share/opencode/worktree/abc/x") + rec("wstatus", 0, "?? new\n")))
			return err
		}
		return inner(vm, argv, o)
	}
	st := existingVM(t, e, Options{Agents: &[]string{"opencode"}, Network: "open", Clone: true})
	vm := st.Name()
	if err := os.WriteFile(filepath.Join(vmfs+e.Dir, "dirty"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var said msg.Recorder
	fake(e).OnSay = said.Add
	lc := Lifecycle{e}
	const command = "devvm clone off"
	err := lc.SetClone(t.Context(), command, CloneOff, RemakeOptions{Force: true, CleanHome: true})
	if msg.NamesOf(err) != "no_terminal_delete command=devvm clone off --clean-home" || looked != 1 {
		t.Errorf("no terminal: %v, looked %d", msg.NamesOf(err), looked)
	}
	if names := said.Names(); !slices.Contains(names, "clone_dirty") || !slices.Contains(names, "agent_tree_dirty agent=opencode path=~/.local/share/opencode/worktree/abc/x") {
		t.Errorf("said %q", names)
	}
	asked := answers(t, e, true)
	fake(e).OnAsk = func(m msg.M) bool {
		*asked = append(*asked, m.Names())
		return msg.IDOf(m) != "remake_anyway"
	}
	if err := lc.SetClone(t.Context(), command, CloneOff, RemakeOptions{CleanHome: true}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if !slices.Equal(*asked, []string{"remake_confirm_clean_home image=base vm=" + vm.String(), "remake_anyway"}) || looked != 2 {
		t.Errorf("asked %q, looked %d", *asked, looked)
	}
	if !isClone(readState(t, e.Dirs, vm)) {
		t.Error("not kept")
	}
}

// A remake with --clean-home loses the home folder, opencode's worktrees
// with it, so it checks them as devvm rm does, after its own question,
// and before anything changes; one that keeps the home folder doesn't
// look, and --no-worktree-check warns instead.
func TestRemakeAgentTrees(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	home := homeFiles(t, ".bash_history", "ls\n")
	w := ".local/share/opencode/worktree/abc/x"
	looked := 0
	f.ExecFunc = func(vm vmname.VM, argv []string, o backend.ExecOpts) error {
		switch {
		case len(argv) > 5 && argv[2] == agentTreesState:
			looked++
			_, err := o.Stdout.Write([]byte(rec("path", 0, w) + rec("wstatus", 0, "?? new\n")))
			return err
		case len(argv) == 3 && argv[2] == homeOut:
			_, err := o.Stdout.Write(home)
			return err
		}
		return noDirect(vm, argv, o)
	}
	st := existingVM(t, e, Options{Agents: &[]string{"opencode"}, Network: "allowlist"})
	vm := st.Name()
	lc := Lifecycle{e}
	// Its messages name the command with --clean-home, which
	// --no-worktree-check needs.
	const typed, command = "devvm layer add nix", "devvm layer add nix --clean-home"
	n := len(f.Calls())
	err := lc.AddLayers(t.Context(), typed, []string{"nix"}, RemakeOptions{Force: true, CleanHome: true, NoExport: true})
	if msg.NamesOf(err) != "no_terminal_delete_trees command="+command || looked != 1 {
		t.Errorf("--clean-home: %v, looked %d", msg.NamesOf(err), looked)
	}
	if slices.ContainsFunc(f.Methods()[n:], func(m string) bool { return m == "BuildImage" || m == "Stop" || m == "Rename" }) {
		t.Errorf("kept, then %q", f.Methods()[n:])
	}
	// Yes to the remake, no to losing the worktree's changes: kept.
	asked := answers(t, e, true)
	fake(e).OnAsk = func(m msg.M) bool {
		*asked = append(*asked, m.Names())
		return msg.IDOf(m) != "remake_anyway"
	}
	n = len(f.Calls())
	if err := lc.AddLayers(t.Context(), typed, []string{"nix"}, RemakeOptions{CleanHome: true, NoExport: true}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if want := []string{"remake_confirm_clean_home image=nix vm=" + vm.String(), "remake_anyway"}; !slices.Equal(*asked, want) || looked != 2 {
		t.Errorf("asked %q, want %q, looked %d", *asked, want, looked)
	}
	if slices.ContainsFunc(f.Methods()[n:], func(m string) bool { return m == "BuildImage" || m == "Stop" || m == "Rename" }) {
		t.Errorf("said no, then %q", f.Methods()[n:])
	}
	var warned []string
	fake(e).OnWarn = func(m msg.M) { warned = append(warned, m.Names()) }
	if err := lc.AddLayers(t.Context(), typed, []string{"nix"}, RemakeOptions{Force: true, CleanHome: true, NoExport: true, NoWorktreeCheck: true}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if looked != 2 || !slices.Contains(warned, "trees_check_skipped command="+command+" vm="+vm.String()) {
		t.Errorf("--no-worktree-check: looked %d, warned %q", looked, warned)
	}
	if err := lc.RemoveLayers(t.Context(), "devvm layer rm nix", []string{"nix"}, RemakeOptions{Force: true, NoExport: true}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if looked != 2 {
		t.Errorf("home kept: looked %d", looked)
	}
}
