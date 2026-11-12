package vm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/gitprotect"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/vmname"
)

// chflagsCalls makes e's .git protection record what it locks and
// unlocks ("uchg PATH", "nouchg PATH"), as on macOS, and, as it locks
// .git/config, which git can't change then, the repo's remotes
// ("remotes=NAMES").
func chflagsCalls(e *Env) *[]string {
	var calls []string
	e.Protector = func(dir string) *gitprotect.Protector {
		return gitprotect.New(dir, e.Dirs.Home, gitprotect.Flags{Chflags: func(ctx context.Context, flags, path string) error {
			if flags == "uchg" && path == filepath.Join(dir, ".git", "config") {
				out, _ := hostGit(ctx, dir, "remote")
				calls = append(calls, "remotes="+out)
			}
			calls = append(calls, flags+" "+path)
			return nil
		}}, &msg.Fake{})
	}
	return &calls
}

// remotesLocked is the repo's remotes as .git/config was last locked.
func remotesLocked(calls []string) string {
	for _, c := range slices.Backward(calls) {
		if r, ok := strings.CutPrefix(c, "remotes="); ok {
			return r
		}
	}
	return "never locked"
}

// flagged reports whether calls has flags on a path in .git.
func flagged(calls []string, flags, dir string) bool {
	return slices.ContainsFunc(calls, func(c string) bool { return strings.HasPrefix(c, flags+" "+filepath.Join(dir, ".git")) })
}

// lastFlags is what calls last set on dir's .git/config: uchg, nouchg,
// or "" for nothing.
func lastFlags(calls []string, dir string) string {
	cfg := filepath.Join(dir, ".git", "config")
	for _, c := range slices.Backward(calls) {
		if flags, path, _ := strings.Cut(c, " "); path == cfg {
			return flags
		}
	}
	return ""
}

// devvm clone on: a VM that mounts the folder is made again with its own
// clone instead, nothing of the folder mounted, its branches pushed in as
// its shell opens, the repo a remote for it, and .git's locks taken off
// once it's done. Asked again, it says so and nothing changes. devvm
// clone off --no-clone-check then makes it mount the folder again
// unchecked, after a warning: .git locked as a new VM's, the remote gone.
func TestSetCloneOn(t *testing.T) {
	e, f, vmfs := cloneEnv(t)
	flags := chflagsCalls(e)
	gitIn(t, e.Dir, "branch", "other")
	st := existingVM(t, e, Options{Network: "open"})
	vm := st.Name()
	aside, _ := vmname.Aside(vm)
	if get(st, keyGitProtect) != gitprotect.On || !flagged(*flags, "uchg", e.Dir) {
		t.Fatalf("mounted: git %q, flags %q", get(st, keyGitProtect), *flags)
	}
	*flags = nil
	lc := Lifecycle{e}
	if err := lc.SetClone(t.Context(), "devvm clone on", CloneOn, RemakeOptions{Force: true, CleanHome: true}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	after := readState(t, e.Dirs, vm)
	for k, want := range map[string]string{KeyMode: ModeClone, KeySeeded: "yes", KeyCloneRemote: "devvm", keyGitProtect: gitprotect.Clone} {
		if got := get(after, k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if remakePending(after) || after.ID != st.ID {
		t.Errorf("pending %v, id %s (was %s)", remakePending(after), after.ID, st.ID)
	}
	if spec, _ := call(f, "CreateVM").Args[1].(backend.VMSpec); spec.Project != "" || spec.CloneAt != e.Dir || spec.Tools == "" {
		t.Errorf("made with %+v", spec)
	}
	if _, ok := f.Instances[aside]; ok {
		t.Error("its old machine is still there")
	}
	if lastFlags(*flags, e.Dir) != "nouchg" {
		t.Errorf("flags %q", *flags)
	}
	if got := gitIn(t, vmfs+e.Dir, "branch", "--format=%(refname:short)"); got != "main\nother" {
		t.Errorf("clone's branches %q", got)
	}
	if got := gitIn(t, e.Dir, "remote", "get-url", "devvm"); got != vm+":"+e.Dir {
		t.Errorf("remote = %q", got)
	}

	var said msg.Recorder
	fake(e).OnInfo = said.Add
	n := len(f.Calls())
	if err := lc.SetClone(t.Context(), "devvm clone on", CloneOn, RemakeOptions{Force: true}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if !slices.Equal(said.Names(), []string{"setting_already command=devvm clone on vm=" + vm}) || slices.Contains(f.Methods()[n:], "CreateVM") {
		t.Errorf("again: said %q, then %q", said.Names(), f.Methods()[n:])
	}

	// Unfetched work, unchecked: lost, after a warning.
	gitIn(t, vmfs+e.Dir, "commit", "-q", "--allow-empty", "-m", "from-vm")
	var warned msg.Recorder
	fake(e).OnWarn = warned.Add
	*flags = nil
	if err := lc.SetClone(t.Context(), "devvm clone off", CloneOff, RemakeOptions{Force: true, CleanHome: true, NoCloneCheck: true}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if !slices.Contains(warned.Names(), "clone_check_skipped command=devvm clone off vm="+vm) {
		t.Errorf("warned %q", warned.Names())
	}
	after = readState(t, e.Dirs, vm)
	for k, want := range map[string]string{KeyMode: "", KeySeeded: "", KeyCloneRemote: "", keyGitProtect: gitprotect.On} {
		if got := get(after, k); got != want {
			t.Errorf("off: %s = %q, want %q", k, got, want)
		}
	}
	if spec, _ := call(f, "CreateVM").Args[1].(backend.VMSpec); spec.Project != e.Dir || spec.CloneAt != "" {
		t.Errorf("off: made with %+v", spec)
	}
	if lastFlags(*flags, e.Dir) != "uchg" || remotesLocked(*flags) != "" {
		t.Errorf("off: flags %q", *flags)
	}
	if out, err := hostGit(t.Context(), e.Dir, "remote"); err != nil || out != "" {
		t.Errorf("off: remotes %q", out)
	}
}

// devvm clone off checks the clone as devvm rm does: without a terminal,
// --force answers only the remake's question, so unfetched commits and
// then uncommitted changes refuse; in one, a no to both keeps the VM as
// it is. A yes fetches the work into saved/devvm/*, then the VM is made
// again mounting the folder, .git locked, and the remote removed.
func TestSetCloneOff(t *testing.T) {
	e, f, vmfs := cloneEnv(t)
	flags := chflagsCalls(e)
	st := existingVM(t, e, Options{Network: "open", Clone: true})
	vm, clone := st.Name(), vmfs+e.Dir
	// A clone whose cache mode nobody chose: the folder it mounts is new,
	// so it gets none.
	change(t, st, withCache(""))
	gitIn(t, clone, "commit", "-q", "--allow-empty", "-m", "from-vm")
	lc := Lifecycle{e}
	off := func() error {
		return lc.SetClone(t.Context(), "devvm clone off", CloneOff, RemakeOptions{Force: true, CleanHome: true})
	}
	kept := func(what string) {
		t.Helper()
		if slices.Contains(f.Methods(), "Rename") || !isClone(readState(t, e.Dirs, vm)) {
			t.Errorf("%s: not kept, %q", what, f.Methods())
		}
	}
	if err := off(); msg.NamesOf(err) != "no_terminal_fetch command=devvm clone off" {
		t.Errorf("unfetched, no terminal: %v", msg.NamesOf(err))
	}
	kept("unfetched")
	gitIn(t, e.Dir, "fetch", "-q", "devvm")
	if err := os.WriteFile(filepath.Join(clone, "new"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := off(); msg.NamesOf(err) != "no_terminal_delete command=devvm clone off" {
		t.Errorf("uncommitted, no terminal: %v", msg.NamesOf(err))
	}
	kept("uncommitted")
	gitIn(t, clone, "commit", "-q", "--allow-empty", "-m", "from-vm-2")
	asked := answers(t, e, false)
	if err := off(); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if !slices.Equal(*asked, []string{"fetch_before_delete prefix=saved/devvm", "remake_anyway"}) {
		t.Errorf("declined: asked %q", *asked)
	}
	kept("declined")

	if err := os.Remove(filepath.Join(clone, "new")); err != nil {
		t.Fatal(err)
	}
	*flags = nil
	asked = answers(t, e, true)
	if err := off(); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if !slices.Equal(*asked, []string{"fetch_before_delete prefix=saved/devvm"}) {
		t.Errorf("asked %q", *asked)
	}
	if got := gitIn(t, e.Dir, "log", "--format=%s", "-1", "refs/heads/saved/devvm/main"); got != "from-vm-2" {
		t.Errorf("fetched %q", got)
	}
	// The work fetched only into devvm/* is kept too, as the remote goes.
	if got := gitIn(t, e.Dir, "for-each-ref", "--format=%(refname)", "refs/remotes"); got != "" {
		t.Errorf("remote-tracking branches %q", got)
	}
	if out, err := hostGit(t.Context(), e.Dir, "remote"); err != nil || out != "" {
		t.Errorf("remotes %q", out)
	}
	after := readState(t, e.Dirs, vm)
	if isClone(after) || get(after, keyGitProtect) != gitprotect.On || get(after, KeyCloneRemote) != "" || remakePending(after) {
		t.Errorf("clone %v, git %q, remote %q", isClone(after), get(after, keyGitProtect), get(after, KeyCloneRemote))
	}
	if spec, _ := call(f, "CreateVM").Args[1].(backend.VMSpec); spec.Project != e.Dir || spec.CloneAt != "" || spec.ProjectCache != backend.CacheNone {
		t.Errorf("made with %+v", spec)
	}
	if c := cacheOf(after); c != backend.CacheNone {
		t.Errorf("its cache mode %q, want none", c)
	}
	// The remote went while git could still change .git/config.
	if lastFlags(*flags, e.Dir) != "uchg" || remotesLocked(*flags) != "" {
		t.Errorf("flags %q", *flags)
	}
}

// A switch that fails once its new machine is made puts the VM back in
// its old mode: devvm clone on's mounts the folder again, its .git locks
// as they were; devvm clone off's has its clone again, its remote kept,
// and the locks the new machine got taken off.
func TestSetCloneUndo(t *testing.T) {
	e, f, _ := cloneEnv(t)
	flags := chflagsCalls(e)
	var cleanups []func(context.Context)
	fake(e).OnCleanup = func(c func(context.Context)) func() {
		i := len(cleanups)
		cleanups = append(cleanups, c)
		return func() { cleanups[i] = nil }
	}
	undo := func() {
		t.Helper()
		for _, c := range slices.Backward(cleanups) {
			if c != nil {
				c(t.Context())
			}
		}
		cleanups = nil
	}
	st := existingVM(t, e, Options{Network: "open"})
	vm := st.Name()
	lc := Lifecycle{e}
	f.Errs = map[string]error{"Start": errors.New("no start")}
	if err := lc.SetClone(t.Context(), "devvm clone on", CloneOn, RemakeOptions{Force: true, CleanHome: true}); err == nil {
		t.Fatal("on: no error")
	}
	undo()
	after := readState(t, e.Dirs, vm)
	if isClone(after) || get(after, keyGitProtect) != gitprotect.On || remakePending(after) {
		t.Errorf("on, undone: clone %v, git %q, pending %v", isClone(after), get(after, keyGitProtect), remakePending(after))
	}

	f.Errs = nil
	if err := lc.SetClone(t.Context(), "devvm clone on", CloneOn, RemakeOptions{Force: true, CleanHome: true}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	cleanups = nil
	*flags = nil
	f.Errs = map[string]error{"Start": errors.New("no start")}
	if err := lc.SetClone(t.Context(), "devvm clone off", CloneOff, RemakeOptions{Force: true, CleanHome: true, NoCloneCheck: true}); err == nil {
		t.Fatal("off: no error")
	}
	// What the new machine did to .git meanwhile is named before its
	// locks come off, as a clone VM's .git is never checked.
	git := filepath.Join(e.Dir, ".git")
	if err := os.CopyFS(git+".new", os.DirFS(git)); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(git, filepath.Join(t.TempDir(), "old")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(git+".new", git); err != nil {
		t.Fatal(err)
	}
	var warned msg.Recorder
	protector := e.Protector
	e.Protector = func(dir string) *gitprotect.Protector {
		p := protector(dir)
		p.Reporter = &msg.Fake{OnWarn: warned.Add}
		return p
	}
	undo()
	if want := "git_replaced git=" + git; !slices.Contains(warned.Names(), want) {
		t.Errorf("off, undone: warned %q, want %s", warned.Names(), want)
	}
	after = readState(t, e.Dirs, vm)
	if !isClone(after) || get(after, keyGitProtect) != gitprotect.Clone || get(after, KeyCloneRemote) != "devvm" || remakePending(after) {
		t.Errorf("off, undone: clone %v, git %q, remote %q", isClone(after), get(after, keyGitProtect), get(after, KeyCloneRemote))
	}
	if !flagged(*flags, "uchg", e.Dir) || lastFlags(*flags, e.Dir) != "nouchg" {
		t.Errorf("off, undone: flags %q", *flags)
	}
	if got := gitIn(t, e.Dir, "remote", "get-url", "devvm"); got != vm+":"+e.Dir {
		t.Errorf("off, undone: remote = %q", got)
	}
}

// devvm clone on refuses, before anything runs, a folder that isn't a
// repo's top, and devvm --clone, on a VM that mounts the folder, names
// devvm clone on. Bare, devvm clone shows off.
func TestSetCloneRefuses(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	st := existingVM(t, e, Options{Network: "open"})
	vm := st.Name()
	lc := Lifecycle{e}
	if err := lc.SetClone(t.Context(), "devvm clone on", CloneOn, RemakeOptions{Force: true}); msg.NamesOf(err) != "clone_not_top dir="+e.Dir {
		t.Errorf("not a repo: %v", msg.NamesOf(err))
	}
	if err := (Launch{e}).Up(t.Context(), Options{Clone: true}); msg.NamesOf(err) != "clone_needs_new_vm vm="+vm {
		t.Errorf("--clone: %v", msg.NamesOf(err))
	}
	if slices.Contains(f.Methods(), "Rename") {
		t.Errorf("ran %q", f.Methods())
	}
	if mode, err := (Settings{e}).Clone(t.Context()); err != nil || mode != CloneOff {
		t.Errorf("shown %q, %v", mode, err)
	}
}
