package gitprotect

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/devvmhq/devvm/internal/hostsafety"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/vmname"
)

// A VM's protection covers each repo in its folder: the folder's own, and,
// once Made.Nested is set, each repo nested in it, a tree of its own whose
// top folder is below the folder. A check of a stopped VM takes the
// nested repos the walk found (Protector.Nested), the ones it recorded
// first; any other check takes those it recorded, so a launch into a
// running VM never walks. Each repo is scanned (scanAll), at most scanners
// at once, and a nested repo whose git dir another repo's protection
// covers, such as a submodule's, is left to that one.

// scanners is how many repos scanAll scans at once: each scan runs a few
// host git commands.
const scanners = 8

// repoSet is the repos a check protects: the folder's own first, then
// the nested ones; and the nested repos recorded that the walk found
// gone.
type repoSet struct {
	trees []tree
	gone  []string
}

// repoAt is the tree of the repo nested at rel, relative to the folder t
// is the own repo of.
func (t tree) repoAt(rel string) tree {
	return tree{Dir: t.Dir, Root: filepath.Join(t.Dir, rel), Home: t.Home}
}

// recorded is the folder's own repo t and the nested repos made records.
func (t tree) recorded(made Made) []tree {
	out := []tree{t}
	for _, rel := range slices.Sorted(maps.Keys(made.Repos)) {
		if filepath.IsLocal(rel) && rel != "." {
			out = append(out, t.repoAt(rel))
		}
	}
	return out
}

// repos is what a check protects, with the nested repos when nested:
// those made records and the walk found (walked; nil when it didn't walk,
// which takes the recorded ones), then the ones it found besides, at most
// hostsafety.MaxRepos in all. A recorded repo is kept, whether the walk
// found it or not, while its folder is a real folder of the project with
// anything at its .git: the VM can make a repo's .git one the walk passes
// over, and git still takes it (renamed objects and refs with a
// commondir, a .GIT, a folder it can't read). Only one with nothing at
// its .git, or whose folder isn't real, is gone.
func (t tree) repos(made Made, nested bool, walked *hostsafety.Repos) repoSet {
	switch {
	case !nested:
		return repoSet{trees: []tree{t}}
	case walked == nil:
		return repoSet{trees: t.recorded(made)}
	}
	found := map[string]bool{}
	for _, d := range walked.Dirs {
		found[d] = true
	}
	set := repoSet{trees: []tree{t}}
	for _, r := range t.recorded(made)[1:] {
		rel := relTo(t.Dir, r.Root)
		if _, err := os.Lstat(r.git()); err == nil && t.realInDir(r.Root) {
			set.trees = append(set.trees, r)
			found[rel] = false
			continue
		}
		set.gone = append(set.gone, rel)
	}
	for _, d := range walked.Dirs {
		if len(set.trees)-1 >= hostsafety.MaxRepos {
			break
		}
		if found[d] {
			set.trees = append(set.trees, t.repoAt(d))
		}
	}
	return set
}

// scanAll scans each of trees, the folder's own repo first, at most
// scanners at once and within one budget of submodule git dirs, and
// leaves out a nested repo whose git dir another one's protection covers
// (covers).
func scanAll(ctx context.Context, trees []tree) []snapshot {
	snaps := make([]snapshot, len(trees))
	sem := make(chan struct{}, scanners)
	b := newBudget()
	var wg sync.WaitGroup
	for i, t := range trees {
		t.budget = b
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			snaps[i] = t.scan(ctx)
		})
	}
	wg.Wait()
	out := snaps[:min(1, len(snaps))]
	for i := 1; i < len(snaps); i++ {
		d := repoGitDir(snaps[i].tree.Root)
		other := slices.ContainsFunc(snaps, func(s snapshot) bool { return s.tree.Root != snaps[i].tree.Root && covers(s, d) })
		if !other {
			out = append(out, snaps[i])
		}
	}
	return out
}

// covers reports whether the scan s's protection covers the git dir d,
// which is one the scan locks whole (snapshot.covered): s's .git, a git
// dir under its .git/modules that reads its own config, or one of its
// linked worktrees whose commondir leads back. Anything else below .git,
// such as a .git/evil the VM made, isn't, and neither is a submodule's
// git dir in the working tree: the index that names it is the VM's to
// write, so a nested repo it names is scanned as a repo of its own.
func covers(s snapshot, d string) bool {
	r := resolved(d)
	return r != "" && s.exists && slices.Contains(s.covered, r)
}

// uncovered is the nested repos rel, relative to the folder dir, whose git
// dir no scan in snaps covers (covers), or locks as a submodule's (one
// that reads its own config): a repo the protection doesn't protect, or
// whose .git file or symlink leads to a git dir in the folder that
// nothing locks, in inside; and in outside, each whose .git leads out of
// the folder, by its folder and the git dir, which devvm can't lock and
// another VM may write, as a submodule's (git_gitdir_outside).
func uncovered(dir string, snaps []snapshot, rel []string) (inside []string, outside [][2]string) {
	for _, r := range rel {
		d := repoGitDir(filepath.Join(dir, r))
		ok := slices.ContainsFunc(snaps, func(s snapshot) bool {
			if covers(s, d) {
				return true
			}
			c, own := commonOf(d)
			return d != "" && own && c == resolved(d) && slices.Contains(s.emb.inside, d)
		})
		switch {
		case ok:
		case d != "" && !paths.Below(d, dir):
			outside = append(outside, [2]string{r, d})
		default:
			inside = append(inside, r)
		}
	}
	return inside, outside
}

// unwatched is the nested repos whose sums the watch records without
// naming them: those now records that was didn't, scanned for the first
// time, and those gone, which git_nested_gone names.
func unwatched(was, now Made, gone []string) []string {
	out := slices.Clone(gone)
	for rel := range now.Repos {
		if _, ok := was.Repos[rel]; !ok {
			out = append(out, rel)
		}
	}
	return out
}

// keeper is kept (Protector.Kept) for the VM vm, or one that keeps
// nothing.
func keeper(kept func(vm vmname.VM) func(path string) msg.M, vm vmname.VM) func(path string) msg.M {
	if kept != nil {
		if k := kept(vm); k != nil {
			return k
		}
	}
	return func(string) msg.M { return msg.M{} }
}

// Keeps reports whether the VM st's .git protection keeps path locked:
// it locks (InVM.locks), it was set up, and path is a git dir it locked, or
// in one: its folder's .git, the git dirs it recorded locking besides,
// and those of the nested repos it protects. Another VM's Unlock leaves
// such a path locked (Protector.Kept).
func Keeps(st *state.VM, path string) bool {
	g, err := Of(st)
	if err != nil || !g.locks() || !g.Made.Protected || st.Dir == "" {
		return false
	}
	t := tree{Dir: st.Dir, Root: st.Dir}
	dirs := []string{t.git()}
	dirs = append(dirs, t.abs(g.Made.GitDirs)...)
	for rel, r := range g.Made.Repos {
		dirs = append(dirs, t.repoAt(rel).git())
		dirs = append(dirs, t.abs(r.GitDirs)...)
	}
	return slices.ContainsFunc(dirs, func(d string) bool { return paths.Within(path, d) })
}
