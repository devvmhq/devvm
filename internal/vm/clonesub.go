package vm

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/devvmhq/devvm/internal/gitprotect"
	"github.com/devvmhq/devvm/internal/hostgit"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
)

// A clone's repo's initialised submodules, nested ones too, each get a
// clone of their own in the VM, at the same path, copied as any repo is
// (seed), with a remote for the VM in the submodule's own repo here, and
// recorded in the clone section (cloneRepo's Submodules) before each
// copy, so fetch, rm's check and the remotes' removal cover each one the
// VM has, whatever the host's repo holds since. Which they are comes
// from this machine's repo alone (hostSubmodules), never from the VM or
// .gitmodules. An uninitialised submodule stays an empty folder in the
// VM, as git leaves it here; one that can't be copied stays on this
// machine, named, and its repo's copy goes on without it.

// keptSubmodule is an initialised submodule that stays on this machine,
// by its path relative to its repo's folder, and why.
type keptSubmodule struct {
	path string
	why  error
}

var (
	// errLinkedSubmodule: a linked worktree's submodules, whose git dirs
	// are in the common dir's modules, stay on this machine.
	errLinkedSubmodule = errors.New("a linked worktree's submodules stay on this machine")
	// errNotSubmodule: an initialised submodule's git dir doesn't lead to
	// its folder, a submodule of the repo's index, its own repo.
	errNotSubmodule = errors.New("its git dir doesn't lead to a submodule of the repo that's its own")
	// errSubmodulePath: a submodule's path holds a character a terminal
	// can't show (plainPath).
	errSubmodulePath = errors.New("its path holds a character a terminal can't show")
)

// hostSubmodules are the initialised submodules of the repo at dir, each
// by its path relative to dir, sorted: those its own config gives a URL
// (submodule.<name>.url, as git submodule init sets it), whose git dir,
// modules/<name> in its common dir, is there; whose core.worktree there
// leads to a folder below dir that the repo's index has a submodule at
// (a gitlink); which is its own repo there (gitprotect.OwnRepo), that git
// dir; and whose path is a plain one (plainPath). One initialised but
// failing a check is kept, with why; so is each of a linked worktree's,
// whose own git dirs this doesn't read.
func hostSubmodules(ctx context.Context, dir string) (subs []string, kept []keptSubmodule, err error) {
	gitDir, common, err := gitprotect.OwnRepo(dir)
	if err != nil {
		return nil, nil, err
	}
	top, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, nil, err
	}
	links, err := gitlinks(ctx, dir)
	if err != nil {
		return nil, nil, err
	}
	if gitDir != common {
		for _, l := range slices.Sorted(maps.Keys(links)) {
			if _, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(l), ".git")); err == nil {
				kept = append(kept, keptSubmodule{filepath.FromSlash(l), errLinkedSubmodule})
			}
		}
		return nil, kept, nil
	}
	out, _ := hostgit.Output(ctx, "-C", dir, "config", "--local", "--null", "--name-only", "--get-regexp", `^submodule\..*\.url$`)
	for key := range strings.SplitSeq(string(out), "\x00") {
		_, name, _, ok := configKey(key)
		if !ok || name == "" || !filepath.IsLocal(name) {
			continue
		}
		mod := filepath.Join(common, "modules", name)
		if fi, err := os.Lstat(mod); err != nil || !fi.IsDir() {
			continue // not checked out here: uninitialised
		}
		rel, err := submodulePath(ctx, dir, top, mod, links)
		switch {
		case err != nil:
			kept = append(kept, keptSubmodule{name, err})
		case !plainPath(rel):
			kept = append(kept, keptSubmodule{rel, errSubmodulePath})
		default:
			subs = append(subs, rel)
		}
	}
	slices.Sort(subs)
	return slices.Compact(subs), kept, nil
}

// submodulePath is the folder, relative to the repo at dir (resolved,
// top), whose submodule's git dir is mod: its core.worktree, a folder
// below the repo where the index has a gitlink (links), which is its own
// repo there, mod; else errNotSubmodule.
func submodulePath(ctx context.Context, dir, top, mod string, links map[string]bool) (string, error) {
	wt, err := hostGit(ctx, dir, "config", "--file", filepath.Join(mod, "config"), "--get", "core.worktree")
	if err != nil || wt == "" {
		return "", errNotSubmodule
	}
	if !filepath.IsAbs(wt) {
		wt = filepath.Join(mod, wt)
	}
	at, err := filepath.EvalSymlinks(wt)
	if err != nil {
		return "", errNotSubmodule
	}
	rel, err := filepath.Rel(top, at)
	if err != nil || !filepath.IsLocal(rel) || !links[filepath.ToSlash(rel)] {
		return "", errNotSubmodule
	}
	own, _ := filepath.EvalSymlinks(mod)
	if gitDir, _, err := gitprotect.OwnRepo(at); err != nil || gitDir != own {
		return "", errNotSubmodule
	}
	return rel, nil
}

// gitlinks are the paths the index of the repo at dir has a submodule at
// (mode 160000, stage 0), as git ls-files writes them.
func gitlinks(ctx context.Context, dir string) (map[string]bool, error) {
	out, err := hostgit.Output(ctx, "-C", dir, "ls-files", "-s", "-z")
	if err != nil {
		return nil, err
	}
	links := map[string]bool{}
	for rec := range strings.SplitSeq(string(out), "\x00") {
		info, path, ok := strings.Cut(rec, "\t")
		if f := strings.Fields(info); ok && len(f) == 3 && f[0] == "160000" && f[2] == "0" {
			links[path] = true
		}
	}
	return links, nil
}

// isSubmodule reports whether the repo at at, below the folder of repos
// dir, is a submodule of a repo in dir, which its clone has: the nearest
// folder above it with a .git is in dir, and its hostSubmodules has at.
func isSubmodule(ctx context.Context, at, dir string) bool {
	fi, err := os.Lstat(filepath.Join(at, ".git"))
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	parent := filepath.Dir(at)
	for ; parent != filepath.Dir(parent); parent = filepath.Dir(parent) {
		if _, err := os.Lstat(filepath.Join(parent, ".git")); err == nil {
			break
		}
	}
	if !paths.Within(parent, dir) {
		return false
	}
	rel, err := filepath.Rel(parent, at)
	if err != nil {
		return false
	}
	subs, _, err := hostSubmodules(ctx, parent)
	return err == nil && slices.Contains(subs, rel)
}

// seedSubmodules copies into t's running VM the initialised submodules
// of the clone's repo at rel in its folder (hostSubmodules), nested ones
// too, each one not yet copied (seedSubmodule); one that isn't, or is
// kept, is named and stays on this machine, its own submodules with it.
func (e seeder) seedSubmodules(ctx context.Context, t *target, rel string) error {
	dir := repoDir(t.f.Dir, rel)
	subs, kept, err := hostSubmodules(ctx, dir)
	if err != nil {
		e.Warn(msg.CloneSubmodulesKept(rel).Because(err))
		return nil
	}
	for _, k := range kept {
		e.Warn(msg.CloneSubmoduleKept(filepath.Join(rel, k.path)).Because(k.why))
	}
	for _, sub := range subs {
		full := filepath.Join(rel, sub)
		copied := submoduleSeeded(sectionOf[cloneInVM](t.st, cloneSection), full)
		if !copied {
			if copied, err = e.seedSubmodule(ctx, t, rel, sub); err != nil {
				return err
			}
		}
		if copied {
			if err := e.seedSubmodules(ctx, t, full); err != nil {
				return err
			}
		}
	}
	return nil
}

// submoduleSeeded is whether c records the submodule at full, in the
// project folder, as copied.
func submoduleSeeded(c cloneInVM, full string) bool {
	return slices.ContainsFunc(c.allRepos(), func(r cloneRepo) bool { return r.Path == full && r.Seeded })
}

// seedSubmodule is seedSubmodules' copy of one submodule, at sub in the
// clone's repo at rel, and whether it went in: checked again as a
// workspace repo is (stillRepo: still its own repo's top, and no other
// VM mounts it), and as --clone checks a repo (planSeed); its remote
// chosen (freeRemote) and recorded with it (addSubmodule) before its repo
// is made (initRepos) and copied (seed); then its remote added in the
// submodule's repo here and the copy recorded. The parent's copy has
// left its folder empty in the VM, as a gitlink's, which its clone goes
// in. One that fails any of it, such as a copy the VM refuses, is named
// (clone_submodule_kept) and the parent's copy goes on without it, which
// is then recorded as done, so it's tried again only on a launch before
// that, when the parent's copy didn't finish. Its record is taken out
// only when nothing of it can be in the VM: when it fails before this
// launch recorded it (addSubmodule), with no record from an earlier
// launch. Otherwise the record stays, not copied, so the VM's work in
// whatever of it the VM has is still fetched and checked by rm (which
// checks a repo not copied that's there), and fetch adds its remote. Only
// an interrupt or a state that can't be written stops the copy.
func (e seeder) seedSubmodule(ctx context.Context, t *target, rel, sub string) (bool, error) {
	vm, full := t.f.Name, filepath.Join(rel, sub)
	dir := repoDir(t.f.Dir, full)
	recorded := slices.ContainsFunc(sectionOf[cloneInVM](t.st, cloneSection).allRepos(), func(r cloneRepo) bool { return r.Path == full })
	kept := func(err error) (bool, error) {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		e.Warn(msg.CloneSubmoduleKept(full).Because(err))
		if recorded {
			return false, nil
		}
		return false, update(ctx, t.st, cloneSection, func(c *cloneInVM) error { c.dropSubmodule(rel, sub); return nil })
	}
	if err := e.stillRepo(ctx, t, full, true); err != nil {
		return kept(err)
	}
	s, err := planSeedOf(ctx, full, dir, true)
	if err != nil {
		return kept(err)
	}
	remote, err := freeRemote(ctx, t.f, dir)
	if err != nil {
		return kept(err)
	}
	if err := update(ctx, t.st, cloneSection, func(c *cloneInVM) error { return c.addSubmodule(rel, sub, remote) }); err != nil {
		return false, err
	}
	recorded = true // from here, the VM may have it
	seeds := []repoSeed{s}
	if err := e.initRepos(ctx, t, seeds, nil); err != nil {
		return kept(err)
	}
	if !seeds[0].landed {
		e.Info(msg.CopyingSubmodule(full))
		if err := e.seed(ctx, t, seeds[0], msg.CopyingSubmodule(full)); err != nil {
			return kept(err)
		}
	}
	if _, ok := remoteURL(ctx, dir, remote); !ok {
		if _, err := hostGit(ctx, dir, "remote", "add", remote, cloneURL(vm, dir)); err != nil {
			return kept(msg.RemoteAddFailed(remote))
		}
	}
	return true, update(ctx, t.st, cloneSection, func(c *cloneInVM) error {
		c.setRepo(full, func(r *cloneRepo) { r.Seeded = true })
		return nil
	})
}
