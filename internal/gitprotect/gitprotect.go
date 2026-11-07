// Package gitprotect keeps a VM from changing the parts of its project's
// .git that run code on the host. The project folder is mounted read-write,
// so without it the VM could add a hook, or set core.fsmonitor, hooksPath or
// sshCommand in .git/config, and the host's next git command would run it.
// A read-only mount wouldn't hold (root in the VM can unmount it), so on
// macOS devvm locks those paths with the user-immutable flag (chflags uchg),
// which the VM can't clear. On Linux it locks them with the immutable
// attribute (chattr +i), which needs CAP_LINUX_IMMUTABLE, so it sets it
// through its helper, devvm-immutable (platform.Locker); without it, devvm
// watches those paths instead (watch.go).
//
// A lock can't stop the VM from swapping the whole .git, or from pointing
// git elsewhere (commondir), so devvm reports what it finds and never
// repairs .git itself: the user decides.
package gitprotect

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/devvmhq/devvm/internal/hostgit"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/state"
)

// Section is the name of the .git protection section in a VM's state.
const Section = "git"

// InVM is a VM's .git protection section: its mode, as asked (recorded
// before its backend makes it, so it's never unset), whether the user
// chose it, the mode chosen while it ran, and what protecting it found;
// on hosts without locks, what watching it found.
type InVM struct {
	Mode string `json:"mode"` // On, Strict, Off, or Clone for a --clone VM
	// Chosen is set once the user chose the mode (--no-git-protect,
	// devvm git-protect), rather than the host: off chosen on a host
	// without locks quiets the launches' warning (WarnUnprotected).
	Chosen  bool    `json:"chosen,omitempty"`
	Next    string  `json:"next,omitempty"` // the mode chosen while the VM ran, set when it's next stopped
	Made    Made    `json:"made,omitzero"`
	Watched Watched `json:"watched,omitzero"`
}

// Made is what protecting a VM's .git found, and checks compare with.
type Made struct {
	// Protected is set once its protection was set up as a new VM's
	// (NewVM) or turned on: until then, there's nothing to compare with.
	Protected bool `json:"protected,omitempty"`
	// GitID is .git's identity (platform.FolderID) when it was last
	// locked.
	GitID string `json:"git_id,omitempty"`
	// GitDirs is the git dirs besides .git that it locked, relative to
	// the project folder: those under .git/modules and the submodules'
	// in the working tree.
	GitDirs []string `json:"git_dirs,omitempty"`
	// Delegations is a hash of what the locked files hand over to the
	// project folder.
	Delegations string `json:"delegations,omitempty"`
}

// Of returns a VM's .git protection section.
func Of(st *state.VM) (InVM, error) { return state.Section[InVM](st, Section) }

// Set sets a VM's .git protection section, in st alone: inside
// st.Update.
func Set(st *state.VM, g InVM) error { return state.SetSection(st, Section, g) }

// Mode returns a VM's .git protection: on, strict, off, or clone for a
// --clone VM, which mounts no .git; "" when it can't be read.
func Mode(st *state.VM) string {
	g, _ := Of(st)
	return g.Mode
}

// record changes the VM's section as f says, under its lock.
func record(ctx context.Context, vm *state.VM, f func(*InVM)) error {
	return vm.Update(ctx, func(vm *state.VM) error {
		g, err := Of(vm)
		if err != nil {
			return err
		}
		f(&g)
		return Set(vm, g)
	})
}

// maxDepth is how deep submodules of submodules are followed.
const maxDepth = 8

// Protector protects one project folder's .git.
type Protector struct {
	Dir  string // the project folder, a physical path
	Home string // for ~ in git's paths
	// CanLock reports whether the host can lock files in the folder with
	// an immutable flag (platform.Locker's Usable): on and strict need it.
	CanLock bool
	// Unable is why it can't, when it can't: platform.ErrNoHelper,
	// ErrHelperUnusable or ErrNoAttribute.
	Unable error
	// Chflags sets file flags on a path and everything under it, following
	// no symlink (platform.Locker's Chflags).
	Chflags func(ctx context.Context, flags, path string) error
	// Locked reports whether a path has the immutable flag, not following
	// a symlink (platform.Immutable).
	Locked func(path string) bool
	// Reporter is where it warns.
	msg.Reporter
}

// New returns the protector for the project folder dir, ~ in git's paths
// being home (paths.Dirs.Home), locking with l, warning through r.
func New(ctx context.Context, dir, home string, l platform.Locker, r msg.Reporter) *Protector {
	err := l.Usable(ctx, dir)
	return &Protector{
		Dir: dir, Home: home, CanLock: err == nil, Unable: err,
		Chflags: l.Chflags, Locked: platform.Immutable, Reporter: r,
	}
}

// unprotected is the launches' warning that the host can't lock .git for
// the VM vm, saying why (a Protector's Unable).
func unprotected(why error, vm string) msg.M {
	switch {
	case errors.Is(why, platform.ErrHelperUnusable):
		return msg.GitUnprotectedHelper(vm)
	case errors.Is(why, platform.ErrNoAttribute):
		return msg.GitUnprotectedFs(vm)
	}
	return msg.GitUnprotected(vm)
}

// cantLock is devvm git-protect on's refusal on a host that can't lock
// .git, saying why (a Protector's Unable).
func cantLock(why error) msg.M {
	switch {
	case errors.Is(why, platform.ErrHelperUnusable):
		return msg.GitProtectOnHelperUnusable()
	case errors.Is(why, platform.ErrNoAttribute):
		return msg.GitProtectOnNeedsFs()
	}
	return msg.GitProtectOnNeedsHelper()
}

// NewSection is the protection a new VM gets, recorded before its
// backend makes it: off when asked (chosen), or on hosts without locks,
// whose launches warn (WarnUnprotected); otherwise on, which NewVM sets
// up once it's made.
func (p *Protector) NewSection(off bool) InVM {
	switch {
	case off:
		return InVM{Mode: Off, Chosen: true}
	case !p.CanLock:
		return InVM{Mode: Off}
	}
	return InVM{Mode: On}
}

// NewVM sets up the protection of a VM just made for the folder,
// stopped, as its mode says (NewSection): on locks .git and reports what
// a lock can't cover. On a host without locks, it records the watch's
// first sums instead, so the next check names what changed since.
func (p *Protector) NewVM(ctx context.Context, vm *state.VM) error {
	g, err := Of(vm)
	switch {
	case err != nil:
		return err
	case !p.CanLock && g.Mode != Clone:
		return watcher{p}.check(ctx, vm, g)
	case g.Mode != On && g.Mode != Strict || !p.CanLock:
		return nil
	}
	made, err := p.protect(ctx, g, whyNew, lockNow)
	if err != nil {
		return err
	}
	return record(ctx, vm, func(g *InVM) { g.Made = made })
}

// Modes of .git protection, as devvm git-protect takes them. Strict is on,
// with host git status and diff kept out of submodules.
const (
	On     = "on"
	Strict = "strict"
	Off    = "off"
)

// Clone is the mode of a --clone VM, which mounts no .git to protect.
const Clone = "clone"

// Modes are the modes devvm git-protect takes.
var Modes = []string{On, Strict, Off}

// Next returns the mode chosen for a VM while it ran (SetNext), which
// the caller sets with Set once it's stopped; "" for none.
func Next(vm *state.VM) string {
	g, _ := Of(vm)
	return g.Next
}

// SetNext saves mode for a running VM, whose locks can't change until it
// stops: the caller sets it with Set then, and a later choice replaces it.
func SetNext(ctx context.Context, vm *state.VM, mode string) error {
	return record(ctx, vm, func(g *InVM) { g.Next = mode })
}

// Set changes the protection of a stopped VM: on and strict lock .git as
// it is now, as trusted, and report what a lock can't cover; strict also
// sets diff.ignoreSubmodules=all in the locked .git/config, which on and
// off take out again; off unlocks. On and strict need a host that can
// lock (CanLock), and are refused, saying why, elsewhere. Once it's
// set, a mode saved for the VM's next stop (SetNext) is dropped: both
// are recorded at once, with the mode as the user's choice (Chosen).
func (p *Protector) Set(ctx context.Context, vm *state.VM, mode string) error {
	g, err := Of(vm)
	if err != nil {
		return err
	}
	was := g.Mode
	if mode == Off {
		p.Unlock(ctx, vm)
		if was == Strict {
			if err := p.ignoreSubmodules(ctx, false); err != nil {
				return err
			}
		}
		return record(ctx, vm, func(g *InVM) { g.Mode, g.Chosen, g.Next = Off, true, "" })
	}
	if !p.CanLock {
		return cantLock(p.Unable)
	}
	if mode == Strict || was == Strict {
		if err := p.ignoreSubmodules(ctx, mode == Strict); err != nil {
			// Locked as it was.
			if cfg := filepath.Join(p.git(), "config"); was != Off && isFile(cfg) && p.realInDir(cfg) {
				p.flag(ctx, "uchg", cfg)
			}
			return err
		}
	}
	g.Mode = mode
	made, err := p.protect(ctx, g, whyOn, lockNow)
	if err != nil {
		return err
	}
	return record(ctx, vm, func(g *InVM) { g.Mode, g.Chosen, g.Next, g.Made = mode, true, "", made })
}

// ignoreSubmodules sets diff.ignoreSubmodules=all in .git/config, or takes
// it out, unlocking the file for the moment (protect locks it again).
func (p *Protector) ignoreSubmodules(ctx context.Context, on bool) error {
	cfg := filepath.Join(p.git(), "config")
	if !isFile(cfg) || !p.realInDir(cfg) {
		if on && exists(p.git()) {
			return msg.GitConfigFailed(cfg)
		}
		return nil
	}
	if !p.flag(ctx, "nouchg", cfg) {
		if on {
			return msg.GitConfigFailed(cfg)
		}
		return nil
	}
	args := []string{"config", "--file", cfg, "--unset", "diff.ignoreSubmodules"}
	if on {
		args = []string{"config", "--file", cfg, "diff.ignoreSubmodules", "all"}
	}
	// --unset fails when there's nothing to take out.
	if err := hostgit.Command(ctx, args...).Run(); err != nil && on {
		return msg.GitConfigFailed(cfg)
	}
	return nil
}

func (p *Protector) warnDelegations(ds []delegation) {
	for _, d := range ds {
		switch d.kind {
		case "include":
			p.Warn(msg.GitIncludeInside(d.where, d.target))
		case "hook":
			p.Warn(msg.GitHookLinkInside(d.where, d.target))
		case "outside":
			p.Warn(msg.GitGitdirOutside(d.target, d.where))
		}
	}
}

// Check runs on every launch of an existing VM: it locks again what's new
// (a submodule, a repo made since), and reports what a lock can't stop,
// once each: a .git replaced since it was locked (then nothing else), a
// repo that appeared, submodule git dirs that appeared, hooks in a hooks
// folder that wasn't locked, and includes or hook links into the project
// folder when they change. Locks are set only while the VM is stopped
// (running), so nothing in it can race them; a running VM gets them at
// its next start. A VM whose protection was never set up (its creation
// stopped short, or devvm adopt moved it) gets it as a new VM does. On a
// host without locks, a stopped VM's .git is watched instead, whatever
// its mode: what a lock can't stop is reported all the same, and what a
// lock would cover that changed since the last check is named
// (watcher.check). So is that of a VM whose mode is off but wasn't
// chosen, as devvm makes where it can't lock, which keeps its mode until
// the user chooses one.
func (p *Protector) Check(ctx context.Context, vm *state.VM, running bool) error {
	g, err := Of(vm)
	switch {
	case err != nil:
		return err
	case !p.CanLock || g.Mode == Off && !g.Chosen:
		if running || g.Mode == Clone {
			return nil
		}
		return watcher{p}.check(ctx, vm, g)
	case g.Mode != On && g.Mode != Strict:
		return nil
	case !g.Made.Protected:
		if running {
			return nil
		}
		return p.NewVM(ctx, vm)
	}
	h := lockNow
	if running {
		h = later
	}
	made, err := p.protect(ctx, g, whyCheck, h)
	if err != nil {
		return err
	}
	return record(ctx, vm, func(g *InVM) { g.Made = made })
}

// why is what protect runs for.
type why int

const (
	whyNew   why = iota // a VM just made
	whyOn               // devvm git-protect on or strict
	whyCheck            // a launch of an existing VM
)

// how protect treats the locks.
type how int

const (
	lockNow  how = iota // a stopped VM's: lock
	later               // a running VM's: report, and lock at its next stop
	watching            // a host without locks: report, record .git's identity, lock nothing
)

// protect locks .git, as h says, and reports what a lock can't stop; it
// returns what it found (g's Made, as it is now), for the caller to
// record with the mode it set. A new VM's git dirs are recorded, not
// reported; a check reports what changed since the last one, and the
// hooks path and delegations only when they change. Watching, nothing is
// locked, so the hooks in folders a lock would cover aren't news: the
// watch names their changes.
func (p *Protector) protect(ctx context.Context, rec InVM, w why, h how) (Made, error) {
	g := p.git()
	was := rec.Made
	if !exists(g) {
		return Made{Protected: true, GitDirs: was.GitDirs, Delegations: was.Delegations}, nil
	}
	if w == whyCheck && was.GitID != "" && was.GitID != folderID(g) {
		p.Warn(msg.GitReplaced(g))
		if h == watching {
			// Nothing to lock again: the next check compares with this one.
			was.GitID = folderID(g)
		}
		return was, nil
	}
	if isDir(g) && exists(filepath.Join(g, "commondir")) {
		target, _ := os.ReadFile(filepath.Join(g, "commondir")) //nolint:gosec // the project's own
		p.Warn(msg.GitCommondir(g, strings.TrimSpace(string(target))))
	}
	for _, s := range p.worktreeStrays() {
		p.Warn(msg.GitWorktreeStray(s))
	}
	if was.GitID == "" && w == whyCheck {
		p.Warn(msg.GitAppeared())
	}
	emb := p.embedded(ctx)
	// Git dirs that appeared since the last time, once there was one:
	// before that, nothing to compare with.
	seen := p.abs(was.GitDirs)
	dirs := p.lockedDirs(emb)
	for _, d := range dirs {
		if w != whyNew && was.Protected && !slices.Contains(seen, d) {
			p.Warn(msg.GitEmbeddedNew(d))
		}
	}
	made := Made{Protected: true, GitID: was.GitID, GitDirs: p.rel(dirs)}
	// Hooks in a hooks folder that isn't locked appeared since the last
	// lock (there was no folder to lock, or it's a new submodule's), so the
	// VM may have made them.
	if w == whyCheck && h != watching {
		for _, path := range p.paths(ctx, emb) {
			if filepath.Base(path) == "hooks" && isDir(path) && !isLink(path) && !p.Locked(path) && hasHooks(path) {
				p.Warn(msg.GitHooksUnlocked(path))
			}
		}
	}
	switch h {
	case lockNow:
		p.lock(ctx, emb)
		made.GitID = folderID(g)
	case watching:
		made.GitID = folderID(g)
	case later:
	}
	if w != whyCheck {
		if hp := gitConfig(ctx, filepath.Join(g, "config"), "core.hooksPath"); hp != "" && p.hooksPathInside(hp) {
			p.Warn(msg.GitHooksPath(hp))
		}
	}
	ds := p.delegations(ctx, emb)
	made.Delegations = delegationsSum(ds)
	if w != whyCheck || made.Delegations != was.Delegations {
		p.warnDelegations(ds)
	}
	return made, nil
}

// lockedDirs are the git dirs protect locks besides .git: those under
// .git/modules (gitDirs) and the submodules' in the working tree.
func (p *Protector) lockedDirs(emb embeds) []string {
	var out []string
	for _, d := range p.gitDirs() {
		if d != p.git() {
			out = append(out, d)
		}
	}
	return append(out, emb.inside...)
}

// abs is git dirs as Made records them, relative to the project folder,
// as absolute paths.
func (p *Protector) abs(rel []string) []string {
	out := make([]string, 0, len(rel))
	for _, r := range rel {
		out = append(out, filepath.Join(p.Dir, r))
	}
	return out
}

// rel is git dirs in the project folder as Made records them: relative
// to it.
func (p *Protector) rel(dirs []string) []string {
	var out []string
	for _, d := range dirs {
		out = append(out, relTo(p.Dir, d))
	}
	return out
}

// hasHooks reports whether a hooks folder has a file that isn't one of
// git's samples.
func hasHooks(dir string) bool {
	found := false
	_ = filepath.WalkDir(dir, func(_ string, e fs.DirEntry, err error) error {
		if err == nil && e.Type().IsRegular() && !strings.HasSuffix(e.Name(), ".sample") {
			found = true
			return fs.SkipAll
		}
		return nil // unreadable: skipped, like find
	})
	return found
}

// Unlock takes the locks off, for a VM that's being deleted or turned
// off: what's to lock now, and the hooks and config of every git dir its
// state records locking (vm; nil for none), which the VM may since have
// dropped from its index. Like locking, it never acts through a symlink.
// A host that can't unlock (devvm-immutable gone since it locked) names
// what stays locked.
func (p *Protector) Unlock(ctx context.Context, vm *state.VM) {
	todo := p.paths(ctx, p.embedded(ctx))
	if vm != nil {
		g, _ := Of(vm)
		for _, d := range p.abs(g.Made.GitDirs) {
			todo = append(todo, filepath.Join(d, "hooks"), filepath.Join(d, "config"))
		}
	}
	done := map[string]bool{}
	var stuck []string
	for _, path := range todo {
		if done[path] {
			continue
		}
		done[path] = true
		switch _, err := os.Lstat(path); {
		case err != nil || !p.realInDir(path):
		case p.CanLock:
			p.flag(ctx, "nouchg", path)
		case p.Locked(path):
			stuck = append(stuck, relTo(p.Dir, path))
		}
	}
	if len(stuck) > 0 {
		p.Warn(msg.GitStillLocked(strings.Join(stuck, ", ")))
	}
}

func (p *Protector) git() string { return filepath.Join(p.Dir, ".git") }

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

func isDir(path string) bool { fi, err := os.Stat(path); return err == nil && fi.IsDir() }

func isLink(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.Mode()&fs.ModeSymlink != 0
}

func isFile(path string) bool { fi, err := os.Stat(path); return err == nil && fi.Mode().IsRegular() }

// git runs git on the host (hostgit) and returns its output; nothing when
// it fails.
func git(ctx context.Context, args ...string) []byte {
	out, err := hostgit.Output(ctx, args...)
	if err != nil {
		return nil
	}
	return out
}

// gitConfig returns a key's value in one config file, or "".
func gitConfig(ctx context.Context, file, key string, opts ...string) string {
	args := append([]string{"config", "--file", file}, opts...)
	out := git(ctx, append(args, "--get", key)...)
	v, _, _ := strings.Cut(string(out), "\n")
	return v
}

func worktreeConfig(ctx context.Context, gitDir string) bool {
	return gitConfig(ctx, filepath.Join(gitDir, "config"), "extensions.worktreeConfig", "--bool") == "true"
}

// gitDirs returns the repo's git dirs: .git and its submodules', anything
// under .git/modules that git would take for one: a HEAD (a file, or a
// symlink, which git still accepts) next to objects and refs.
func (p *Protector) gitDirs() []string {
	g := p.git()
	if !isDir(g) {
		return nil
	}
	dirs := []string{g}
	_ = filepath.WalkDir(filepath.Join(g, "modules"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.Name() != "HEAD" || !d.Type().IsRegular() && d.Type()&fs.ModeSymlink == 0 {
			return nil //nolint:nilerr // unreadable: skipped, like find
		}
		if dir := filepath.Dir(path); isDir(filepath.Join(dir, "objects")) && isDir(filepath.Join(dir, "refs")) {
			dirs = append(dirs, dir)
		}
		return nil
	})
	return dirs
}

// worktrees returns a git dir's linked worktrees' folders, as the glob
// worktrees/*/ has them: folders and links to folders, sorted, without
// hidden ones.
func worktrees(gitDir string) []string {
	entries, _ := os.ReadDir(filepath.Join(gitDir, "worktrees"))
	var out []string
	for _, e := range entries {
		w := filepath.Join(gitDir, "worktrees", e.Name())
		if !strings.HasPrefix(e.Name(), ".") && isDir(w) {
			out = append(out, w)
		}
	}
	return out
}

// embeds are the submodule git dirs of the repo's working tree: inside
// the project folder, or (with the submodule's folder) outside it.
type embeds struct {
	inside  []string
	outside [][2]string // the submodule's folder, its git dir
}

// embedded finds the submodules whose git dir is in the working tree: a
// gitlink in the index (which the VM writes, to commit) whose folder has
// a .git folder of its own, or a .git file pointing elsewhere. Host git
// status reads their config, and the VM can make one at any time.
// Submodules' own indexes are read too (host git status recurses into
// nested submodules). The index is read with fsmonitor off; ls-files
// never enters a submodule.
func (p *Protector) embedded(ctx context.Context) embeds {
	var e embeds
	g := p.git()
	if isDir(g) && !isLink(g) {
		p.embeddedIn(ctx, &e, g, p.Dir, 0)
	}
	return e
}

func (p *Protector) embeddedIn(ctx context.Context, e *embeds, gitDir, worktree string, depth int) {
	if depth >= maxDepth {
		return
	}
	out := git(ctx, "-c", "core.fsmonitor=false", "--git-dir="+gitDir, "--work-tree="+worktree, "ls-files", "-s", "-z")
	for entry := range bytes.SplitSeq(out, []byte{0}) {
		info, path, ok := bytes.Cut(entry, []byte{'\t'})
		if !ok || !bytes.HasPrefix(info, []byte("160000 ")) {
			continue
		}
		w := filepath.Join(worktree, string(path))
		g := filepath.Join(w, ".git")
		var t string
		switch {
		case isLink(g):
			continue
		case isDir(g):
			t = g
		case isFile(g):
			t = gitdirOf(g, w)
			if t == "" {
				continue
			}
		default:
			continue
		}
		if !paths.Below(t, p.Dir) {
			// Nothing to lock here: delegations names it.
			e.outside = append(e.outside, [2]string{w, t})
			continue
		}
		if !paths.Below(t, filepath.Join(p.Dir, ".git", "modules")) { // gitDirs has those
			e.inside = append(e.inside, t)
		}
		p.embeddedIn(ctx, e, t, w, depth+1)
	}
}

// gitdirOf returns the git dir a .git file points to, resolved, or "".
func gitdirOf(file, worktree string) string {
	data, err := os.ReadFile(file) //nolint:gosec // the project's own
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		t, ok := strings.CutPrefix(line, "gitdir: ")
		if !ok {
			continue
		}
		return resolvedDir(worktree, t)
	}
	return ""
}

// resolvedDir returns p, relative to dir unless it's absolute, with its
// symlinks resolved, when it's a folder, or "".
func resolvedDir(dir, p string) string {
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	r, err := filepath.EvalSymlinks(p)
	if err != nil || !isDir(r) {
		return ""
	}
	return r
}

// paths returns what to lock: the hooks and config of the repo and of its
// submodules (lock creates a missing hooks folder or config empty, or the
// VM could make one, as git runs without a config), config.worktree with extensions.worktreeConfig (lock creates
// it), and what linked worktrees elsewhere read from here (commondir,
// config.worktree); or, in a linked worktree, its .git file.
func (p *Protector) paths(ctx context.Context, emb embeds) []string {
	g := p.git()
	if isFile(g) {
		return []string{g}
	}
	var out []string
	for _, d := range p.gitDirs() {
		out = append(out, filepath.Join(d, "hooks"), filepath.Join(d, "config"))
		wtc := worktreeConfig(ctx, d)
		if wtc {
			out = append(out, filepath.Join(d, "config.worktree"))
		}
		for _, w := range worktrees(d) {
			if isFile(filepath.Join(w, "commondir")) {
				out = append(out, filepath.Join(w, "commondir"))
			}
			if wtc {
				out = append(out, filepath.Join(w, "config.worktree"))
			}
		}
	}
	for _, d := range emb.inside {
		out = append(out, filepath.Join(d, "hooks"), filepath.Join(d, "config"))
	}
	return out
}

// worktreeStrays returns the linked worktrees (.git/worktrees/<n>) whose
// commondir doesn't lead back to their git dir: git in that worktree reads
// config and hooks from wherever it points (without one, from <n> itself).
func (p *Protector) worktreeStrays() []string {
	var out []string
	for _, d := range p.gitDirs() {
		physical, _ := filepath.EvalSymlinks(d)
		for _, w := range worktrees(d) {
			if r, ok := commonOf(w); !ok || r != physical {
				out = append(out, w)
			}
		}
	}
	return out
}

// commonOf returns where git in the git dir g reads the repo's config,
// objects and refs from: its commondir, resolved (relative to g, as git
// reads it), else g itself, resolved. It's false when that isn't there.
func commonOf(g string) (string, bool) {
	target := g
	if c, err := os.ReadFile(filepath.Join(g, "commondir")); err == nil { //nolint:gosec // the project's own
		target = strings.TrimRight(string(c), "\n")
		if !filepath.IsAbs(target) {
			target = filepath.Join(g, target)
		}
	}
	r, err := filepath.EvalSymlinks(target)
	return r, err == nil
}

// hooksPathInside reports whether core.hooksPath leads into the project
// folder, where the VM can edit the hooks. Relative paths are from the
// working tree. A path that isn't an existing folder counts by name and
// resolved as far as it exists.
func (p *Protector) hooksPathInside(hp string) bool {
	hp = expandHome(p.Home, hp)
	if !filepath.IsAbs(hp) {
		return true
	}
	if r, err := filepath.EvalSymlinks(hp); err == nil && isDir(r) {
		return paths.Within(r, p.Dir)
	}
	return paths.Within(filepath.Clean(hp), p.Dir) || paths.Within(physicalParent(hp), p.Dir)
}

// expandHome expands ~ as git does, to home.
func expandHome(home, path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		return home + path[1:]
	}
	return path
}

// delegation is something the locked files hand over to a file the VM can
// edit, or a git dir devvm can't lock.
type delegation struct {
	kind   string // include, hook or outside
	where  string // the config, the hook, or the submodule's folder
	target string // the include's path or the hook's link as written, or the git dir
	path   string // what an include leads to, absolute, for the watch
}

// delegations finds what the locked config and hooks hand over to files
// the VM can edit: include.path and includeIf.*.path that lead into the
// project folder (relative to the config's folder, as git reads them), and
// hooks that are symlinks into it; and the submodules whose git dir is
// outside the project, which devvm can't lock (another project's VM may
// write it).
func (p *Protector) delegations(ctx context.Context, emb embeds) []delegation {
	var out []delegation
	for _, d := range append(p.gitDirs(), emb.inside...) {
		cfg := filepath.Join(d, "config")
		list := git(ctx, "config", "--file", cfg, "-z", "--get-regexp", `^include(if\..*)?\.path$`)
		for rec := range bytes.SplitSeq(list, []byte{0}) {
			_, v, ok := bytes.Cut(rec, []byte{'\n'})
			if !ok || len(v) == 0 {
				continue
			}
			path := expandHome(p.Home, string(v))
			if !filepath.IsAbs(path) {
				path = filepath.Join(d, path)
			}
			if p.into(path) {
				out = append(out, delegation{"include", cfg, string(v), filepath.Clean(path)})
			}
		}
		_ = filepath.WalkDir(filepath.Join(d, "hooks"), func(path string, e fs.DirEntry, err error) error {
			if err != nil || e.Type()&fs.ModeSymlink == 0 {
				return nil //nolint:nilerr // unreadable: skipped, like find
			}
			link, err := os.Readlink(path)
			if err != nil {
				return nil //nolint:nilerr // gone meanwhile
			}
			target := link
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(path), target)
			}
			if p.into(target) {
				out = append(out, delegation{"hook", path, link, ""})
			}
			return nil
		})
	}
	for _, o := range emb.outside {
		out = append(out, delegation{"outside", o[0], o[1], ""})
	}
	return out
}

// into reports whether path, resolved, is in the project folder. A path
// whose folder is missing counts by name too: the VM can make the folder
// or change the symlinks it would be under.
func (p *Protector) into(path string) bool {
	if _, err := os.Stat(filepath.Dir(path)); err == nil {
		return paths.Below(paths.PhysicalParent(path), p.Dir)
	}
	return paths.Below(filepath.Clean(path), p.Dir) || paths.Below(physicalParent(path), p.Dir)
}

// physicalParent is path with its folder resolved as far as it exists
// (paths.Physical), so a missing folder under a symlinked prefix still
// resolves, and its last element as named.
func physicalParent(path string) string {
	return filepath.Join(paths.Physical(filepath.Dir(path)), filepath.Base(path))
}

func delegationsSum(ds []delegation) string {
	h := sha256.New()
	for _, d := range ds {
		for _, f := range []string{d.kind, d.where, d.target} {
			h.Write([]byte(strconv.Itoa(len(f)) + ":" + f))
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// realInDir reports whether path is a real path in the project folder,
// with no symlink in it below the folder. The VM can plant symlinks under
// .git: creating a file or setting flags through one would act outside the
// project, and chflags on a symlink itself does nothing.
func (p *Protector) realInDir(path string) bool {
	if !paths.Below(path, p.Dir) || isLink(path) {
		return false
	}
	parent := filepath.Dir(path)
	r, err := filepath.EvalSymlinks(parent)
	return err == nil && r == parent
}

// lock locks the paths to protect, which only happens while the VM is
// stopped, so nothing in it can race the checks.
func (p *Protector) lock(ctx context.Context, emb embeds) {
	for _, path := range p.paths(ctx, emb) {
		if !p.realInDir(path) {
			p.Warn(msg.GitNotLocked(path))
			continue
		}
		if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
			// So the VM can't create it. O_EXCL and mkdir fail on any
			// existing name, and neither follows a symlink.
			switch filepath.Base(path) {
			case "config.worktree", "config":
				if f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err == nil { //nolint:gosec // checked by realInDir
					_ = f.Close()
				}
			case "hooks":
				_ = os.Mkdir(path, 0o700)
			}
		}
		if _, err := os.Lstat(path); err == nil {
			p.flag(ctx, "uchg", path)
		}
	}
}

// flag sets a flag on a path realInDir passed, then checks it again.
// Between the check and chflags, a running VM could swap a folder above the
// path for a symlink to somewhere else: a path that moved meanwhile is
// unlocked again where it landed, and reported. A lock that failed (a
// file the user can't read, one that isn't a file or a folder) is
// reported too, with why in the run log.
func (p *Protector) flag(ctx context.Context, flags, path string) bool {
	before := devIno(path)
	if before == "" {
		return true
	}
	err := p.Chflags(ctx, flags, path)
	if devIno(path) == before && p.realInDir(path) {
		if err != nil && flags == "uchg" {
			_, _ = io.WriteString(p.LogWriter(), "locking "+path+": "+err.Error()+"\n")
			p.Warn(msg.GitLockFailed(path))
		}
		return true
	}
	if flags == "uchg" {
		_ = p.Chflags(ctx, "nouchg", path)
	}
	p.Warn(msg.GitLockRaced(path))
	return false
}

// devIno is what's at path (not following a symlink) as dev:ino, or "".
func devIno(path string) string {
	fi, err := os.Lstat(path)
	if err != nil {
		return ""
	}
	return platform.DevIno(fi)
}

// folderID is what's at path as platform.FolderID has it, or "".
func folderID(path string) string {
	id, _ := platform.FolderID(path)
	return id
}
