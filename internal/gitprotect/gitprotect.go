// Package gitprotect keeps a VM from changing the parts of its project's
// .git that run code on the host. The project folder is mounted read-write,
// so without it the VM could add a hook, or set core.fsmonitor, hooksPath or
// sshCommand in .git/config, and the host's next git command would run it.
// A read-only mount wouldn't hold (root in the VM can unmount it), so on
// macOS devvm locks those paths with the user-immutable flag (chflags uchg),
// which the VM can't clear. Linux hosts have no unprivileged flag like it,
// so there the VM's backend pins those paths read-only at each start, in
// the namespace its QEMU runs in (Binds, Pins), where the kernel lets it,
// and devvm watches .git where it doesn't (watcher).
//
// A lock can't stop the VM from swapping the whole .git, or from pointing
// git elsewhere (commondir), so devvm reports what it finds and never
// repairs .git itself: the user decides.
//
// Each check scans .git once (scan: a snapshot), which locking, unlocking,
// the watch and the reports (a table, reports) go by; the host's Locker
// does the locking.
package gitprotect

import (
	"context"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/devvmhq/devvm/internal/hostgit"
	"github.com/devvmhq/devvm/internal/hostsafety"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/vmname"
)

// Section is the name of the .git protection section in a VM's state.
const Section = "git"

// InVM is a VM's .git protection section: its mode, as asked (recorded
// before its backend makes it, so it's never unset), whether the user
// chose it, the mode chosen while it ran, and what protecting it found;
// on hosts without locks, what watching it found.
type InVM struct {
	Mode string `json:"mode"` // On, Off, Clone for a --clone VM, or an earlier devvm's Strict
	// Chosen is set once the user chose the mode (--no-git-protect,
	// devvm git-protect), rather than the host: off chosen on a host
	// without locks quiets the launches' warning (WarnUnprotected).
	Chosen  bool    `json:"chosen,omitempty"`
	Next    string  `json:"next,omitempty"` // the mode chosen while the VM ran, set when it's next stopped
	Made    Made    `json:"made,omitzero"`
	Watched Watched `json:"watched,omitzero"`
	// Nested is the git repos nested in the folder that the last walk
	// found (Protector.Nested) and the protection doesn't cover
	// (uncovered), relative to the folder, and NestedMore how many more
	// the walk counted past its bound: launches name them, as the VM can
	// change their .git.
	Nested     []string `json:"nested,omitempty"`
	NestedMore int      `json:"nested_more,omitempty"`
	NestedPast []string `json:"nested_past,omitempty"` // the first of those NestedMore counts
	// Bare, Skipped and Devices are what else the last walk found
	// (hostsafety.Repos), which checks name when it's new.
	Bare    []string `json:"bare,omitempty"`
	Skipped int      `json:"skipped,omitempty"`
	Devices int      `json:"devices,omitempty"`
	// NestedNoticed is set once a VM whose protection doesn't cover its
	// nested repos (Made.Nested unset) was told it can (git_nested_protectable).
	NestedNoticed bool `json:"nested_noticed,omitempty"`
	// StrictLeft is the repos, by their top folder relative to the project
	// folder ("." for its own), whose config keeps the
	// diff.ignoreSubmodules = all that an earlier devvm's strict set, as
	// another VM's protection keeps it locked too, or it changed after the
	// check: each stopped check tries again (unstrict).
	StrictLeft []string `json:"strict_left,omitempty"`
}

// Made is what protecting a VM's .git found, and checks compare with:
// of the folder's own repo, and of each repo nested in it that it
// protects.
type Made struct {
	// Protected is set once its protection was set up as a new VM's
	// (NewVM) or turned on: until then, there's nothing to compare with.
	Protected bool `json:"protected,omitempty"`
	Repo           // the folder's own repo's, as its fields
	// Nested is set once the repos nested in the folder are protected
	// too: by a new VM, devvm git-protect on, or the watch. A
	// VM an older devvm made protects only its own until then.
	Nested bool `json:"nested,omitempty"`
	// Repos is each nested repo's, by its top folder relative to the
	// project folder. A repo without one is scanned the first time, which
	// records it without reporting.
	Repos map[string]Repo `json:"repos,omitempty"`
	// Left is the nested repos it stopped protecting, as they're gone or
	// another repo's protection covers them, whose locks may still be on:
	// Unlock takes them off too, and names those not in the folder.
	Left map[string]Repo `json:"left,omitempty"`
}

// Repo is what protecting one repo's .git found.
type Repo struct {
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

// Mode returns a VM's .git protection: on, off, or clone for a --clone
// VM, which mounts no .git; "" when it can't be read. An earlier devvm's
// strict is on, which the VM's next check that finds it stopped records
// (unstrict).
func Mode(st *state.VM) string {
	g, _ := Of(st)
	return asOn(g.Mode)
}

// locks reports whether the section's mode locks .git: on, or an earlier
// devvm's strict, until a check converts it (unstrict).
func (g InVM) locks() bool { return g.Mode == On || g.Mode == Strict }

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
	tree // the project folder (Dir) and the home folder for ~ (Home)
	// Locker locks, the host's way (HostLocker): on needs one that does
	// (Locker.Locks).
	Locker Locker
	// Reporter is where it warns.
	msg.Reporter
	// Nested is the git repos nested in the folder, as the walk of a
	// stopped VM's folder found them (hostsafety.Walk), which the caller
	// sets; nil when it didn't walk, as for a running VM, whose recorded
	// repos are protected.
	Nested *hostsafety.Repos
	// Kept says, for the VM vm, whether another VM keeps a path locked
	// too, as its own protection covers it (Keeps), or may (its state
	// can't be read): the warning that names it (git_kept_locked,
	// git_kept_unread), or a zero msg.M for none. Unlock, and taking out
	// what strict set (dropIgnore), leave such a path as it is. nil keeps
	// none.
	Kept func(vm vmname.VM) func(path string) msg.M
	// Serial runs f holding the host's lock of .git protection
	// (paths.Dirs.GitLock), so one VM's unlock never runs between
	// another's locks and their record; nil runs f alone.
	Serial func(ctx context.Context, f func() error) error
}

// New returns the protector for the project folder dir, ~ in git's paths
// being home (paths.Dirs.Home), locking with l (HostLocker), warning
// through r.
func New(dir, home string, l Locker, r msg.Reporter) *Protector {
	return &Protector{tree: tree{Dir: dir, Root: dir, Home: home}, Locker: l, Reporter: r}
}

// NewSection is the protection a new VM gets, recorded before its
// backend makes it: off when asked (chosen), or on hosts without locks,
// whose launches warn (WarnUnprotected); otherwise on, which NewVM sets
// up once it's made.
func (p *Protector) NewSection(off bool) InVM {
	switch {
	case off:
		return InVM{Mode: Off, Chosen: true}
	case !p.Locker.Locks():
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
	case !p.Locker.Locks() && g.Mode != Clone:
		return watcher{p}.check(ctx, vm, g)
	case !g.locks() || !p.Locker.Locks():
		return nil
	}
	return p.lockAndWatch(ctx, vm, g, whyNew, lockNow, Watched{}, func(*InVM) {})
}

// lockAndWatch protects a VM's .git as w and h say (protect), recording
// what it found and what set changes (as Set and Check do), and, where
// the locker leaves a window the guest could use (Locker.Watches), sums
// what a lock covers once the VM is stopped, naming what changed since
// was, as a host without locks does (watcher.watch). A zero was records
// the sums without naming anything: .git as it is now, as trusted. The
// repos nested in the folder are protected too once the VM's protection
// covers them (Made.Nested), which a new VM's and devvm git-protect's
// does.
func (p *Protector) lockAndWatch(ctx context.Context, vm *state.VM, g InVM, w why, h how, was Watched, set func(*InVM)) error {
	// A running VM's check is the folder's own repo's alone: it locks
	// nothing, so its nested repos wait for the next stopped one, which a
	// launch into a running VM never pays for.
	nested := g.Made.Nested || w != whyCheck
	c := p.repos(g.Made, nested && h != later, p.Nested)
	var snaps []snapshot
	var made Made
	// The locks and their record, under the host's lock: another VM's
	// unlock goes by the record.
	err := serial(ctx, p.Serial, func() error {
		snaps = scanAll(ctx, c.trees)
		made = p.protect(ctx, vm.Name(), snaps, g, w, h, c)
		return record(ctx, vm, func(g *InVM) { set(g); g.Made = made })
	})
	if err != nil {
		return err
	}
	if err := p.nested(ctx, vm, snaps, w); err != nil {
		return err
	}
	if h != lockNow || !p.Locker.Watches() {
		return nil
	}
	return watcher{p}.watch(ctx, vm, snaps, was, unwatched(g.Made, made, c.gone))
}

// Modes of .git protection, as devvm git-protect takes them.
const (
	On  = "on"
	Off = "off"
)

// Clone is the mode of a --clone VM, which mounts no .git to protect.
const Clone = "clone"

// Modes are the modes devvm git-protect takes.
var Modes = []string{On, Off}

// Next returns the mode chosen for a VM while it ran (SetNext), which
// the caller sets with Set once it's stopped; "" for none. An earlier
// devvm's strict is on.
func Next(vm *state.VM) string {
	g, _ := Of(vm)
	return asOn(g.Next)
}

// SetNext saves mode for a running VM, whose locks can't change until it
// stops: the caller sets it with Set then, and a later choice replaces it.
func SetNext(ctx context.Context, vm *state.VM, mode string) error {
	return record(ctx, vm, func(g *InVM) { g.Next = mode })
}

// Set changes the protection of a stopped VM: on locks .git as it is
// now, as trusted, the repos nested in the folder's included, and
// reports what a lock can't cover; off unlocks. On needs a host that
// locks. A VM an earlier devvm made strict loses what strict set in its
// repos' configs either way, and so do those a conversion left
// (strictLeft), whose repos it leaves are recorded. Once it's set, a mode saved for
// the VM's next stop (SetNext) is dropped: both are recorded at once,
// with the mode as the user's choice (Chosen).
func (p *Protector) Set(ctx context.Context, vm *state.VM, mode string) error {
	g, err := Of(vm)
	if err != nil {
		return err
	}
	if mode == Off {
		left := p.unlockAll(ctx, vm)
		return record(ctx, vm, func(g *InVM) { g.Mode, g.Chosen, g.Next, g.Made.Left, g.StrictLeft = Off, true, "", nil, left })
	}
	if !p.Locker.Locks() {
		return msg.GitProtectOnNeedsLocks()
	}
	var left []string
	_ = serial(ctx, p.Serial, func() error {
		left = converter{p}.strictLeft(ctx, vm.Name(), g, true)
		return nil
	})
	g.Mode = mode
	return p.lockAndWatch(ctx, vm, g, whyOn, lockNow, Watched{}, func(g *InVM) { g.Mode, g.Chosen, g.Next, g.StrictLeft = mode, true, "", left })
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
// (watcher.check). A stopped VM an earlier devvm made strict is on once
// it's checked, in the repos its state recorded before the check
// (unstrict): one the check found is new, so strict never set anything
// there.
func (p *Protector) Check(ctx context.Context, vm *state.VM, running bool) error {
	was, err := Of(vm)
	if err != nil {
		return err
	}
	if err := p.check(ctx, vm, running); err != nil || running {
		return err
	}
	return converter{p}.unstrict(ctx, vm, was.Made)
}

// check is Check's, before a strict VM is converted.
func (p *Protector) check(ctx context.Context, vm *state.VM, running bool) error {
	g, err := Of(vm)
	switch {
	case err != nil:
		return err
	case !p.Locker.Locks():
		if running || g.Mode == Clone {
			return nil
		}
		return watcher{p}.check(ctx, vm, g)
	case !g.locks():
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
	return p.lockAndWatch(ctx, vm, g, whyCheck, h, g.Watched, func(*InVM) {})
}

// Unlock takes the locks off, for a VM that's being deleted or turned
// off: of the folder's own repo and of each nested repo its state records
// protecting (vm; nil for none), what's to lock now, and the hooks and
// config of every git dir its state records locking, which the VM may
// since have dropped from its index. A path another VM's protection
// keeps locked too (Kept) stays locked, and that VM is named. Like
// locking, it never acts through a symlink. A VM an earlier devvm made
// strict loses what strict set in its repos' configs too (strictLeft).
func (p *Protector) Unlock(ctx context.Context, vm *state.VM) { p.unlockAll(ctx, vm) }

// unlockAll is Unlock's, and returns the repos whose config keeps what
// strict set (InVM.StrictLeft).
func (p *Protector) unlockAll(ctx context.Context, vm *state.VM) []string {
	var g InVM
	var name vmname.VM
	if vm != nil {
		g, _ = Of(vm)
		name = vm.Name()
	}
	var left []string
	_ = serial(ctx, p.Serial, func() error {
		if p.Locker.Locks() {
			p.unlock(ctx, name, g.Made)
		}
		left = converter{p}.strictLeft(ctx, name, g, false)
		return nil
	})
	return left
}

// unlock is Unlock's, under the host's lock: of the repos made records,
// those it protects and those it stopped protecting but may have left
// locked (Made.Left), the git dirs it recorded locking too. Those of the
// latter that aren't in the folder any more are named.
func (p *Protector) unlock(ctx context.Context, vm vmname.VM, made Made) {
	trees := p.recorded(made)
	repos := append([]Repo{made.Repo}, slices.Collect(maps.Values(made.Repos))...)
	var lost []string
	for _, rel := range slices.Sorted(maps.Keys(made.Left)) {
		repos = append(repos, made.Left[rel])
		if r := p.repoAt(rel); filepath.IsLocal(rel) && exists(r.Root) {
			trees = append(trees, r)
		} else {
			lost = append(lost, rel)
		}
	}
	var todo []string
	for _, s := range scanAll(ctx, trees) {
		todo = append(todo, s.paths...)
	}
	for _, r := range repos {
		for _, d := range p.abs(r.GitDirs) {
			todo = append(todo, filepath.Join(d, "hooks"), filepath.Join(d, "config"))
		}
	}
	kept := keeper(p.Kept, vm)
	keepers := map[string]msg.M{}
	done := map[string]bool{}
	for _, path := range todo {
		if done[path] {
			continue
		}
		done[path] = true
		if k := kept(path); k.ID() != "" {
			keepers[k.String()] = k
			continue
		}
		if _, err := os.Lstat(path); err == nil && p.realInDir(path) {
			p.flag(ctx, false, path)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(keepers)) {
		p.Warn(keepers[k])
	}
	if _, flags := p.Locker.(Flags); flags && len(lost) > 0 {
		l, _ := listed(lost)
		p.Warn(msg.GitLocksLeft(l))
	}
}

// serial runs f under the host's lock of .git protection, with s
// (Protector.Serial), or alone when it's nil.
func serial(ctx context.Context, s func(context.Context, func() error) error, f func() error) error {
	if s == nil {
		return f()
	}
	return s(ctx, f)
}

// lock locks the paths to protect (s.paths), which only happens while
// the VM is stopped, so nothing in it can race the checks.
func (p *Protector) lock(ctx context.Context, s snapshot) {
	for _, path := range s.paths {
		if !p.realInDir(path) {
			p.Warn(msg.GitNotLocked(path))
			continue
		}
		if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
			// So the VM can't create it. O_EXCL and mkdir fail on any
			// existing name, and neither follows a symlink.
			switch filepath.Base(path) {
			case "config.worktree":
				if f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err == nil { //nolint:gosec,forbidigo // a new empty file, checked by realInDir, never read
					_ = f.Close()
				}
			case "hooks":
				_ = os.Mkdir(path, 0o700)
			}
		}
		switch _, err := os.Lstat(path); {
		case err == nil:
			p.flag(ctx, true, path)
		case !errors.Is(err, fs.ErrNotExist):
			// One the VM made unreadable can't be locked, nor known gone.
			p.Warn(msg.GitNotLocked(path))
		}
	}
}

// flag locks a path realInDir passed (lock), or unlocks it, then checks
// it again. Between the check and the locker, a running VM could swap a
// folder above the path for a symlink to somewhere else: a path that
// moved meanwhile is unlocked again where it landed, and reported.
func (p *Protector) flag(ctx context.Context, lock bool, path string) bool {
	before := devIno(path)
	if before == "" {
		return true
	}
	if lock {
		_ = p.Locker.Lock(ctx, path)
	} else {
		_ = p.Locker.Unlock(ctx, path)
	}
	if devIno(path) == before && p.realInDir(path) {
		return true
	}
	if lock {
		_ = p.Locker.Unlock(ctx, path)
	}
	p.Warn(msg.GitLockRaced(path))
	return false
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

func isDir(path string) bool { fi, err := os.Stat(path); return err == nil && fi.IsDir() }

func isLink(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.Mode()&fs.ModeSymlink != 0
}

func isFile(path string) bool { fi, err := os.Stat(path); return err == nil && fi.Mode().IsRegular() }

// isHooks reports whether path is a hooks folder, itself, not a symlink.
func isHooks(path string) bool {
	return filepath.Base(path) == "hooks" && isDir(path) && !isLink(path)
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
