package gitprotect

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/devvmhq/devvm/internal/msg"
)

// OwnRepo checks that what git reads in dir, the top folder of a repo,
// is that repo's own, for --clone, whose push sends it into a VM. dir's
// .git is a folder, or the .git file of a linked worktree whose link git
// made both ways (linkedWorktree), or of a submodule whose git dir is in
// its parent repo's (submoduleGitDir); and nothing in its git dir or
// common dir points into another repo: no commondir in a .git folder or
// a submodule's git dir, no alternates, and no objects or refs that are
// symlinks. It returns the git dir and the common dir, resolved (the
// same for a .git folder or a submodule), or the message naming what
// points elsewhere.
func OwnRepo(dir string) (gitDir, commonDir string, err error) {
	g := filepath.Join(dir, ".git")
	fi, err := os.Lstat(g)
	switch {
	case err != nil:
		return "", "", msg.CloneNotTop(dir)
	case fi.IsDir():
		gitDir, _ = filepath.EvalSymlinks(g)
		if c, ok := commonOf(g); !ok || c != gitDir {
			return "", "", msg.CloneGitElsewhere(filepath.Join(g, "commondir"))
		}
		commonDir = gitDir
	case fi.Mode().IsRegular():
		if gitDir, commonDir = linkedWorktree(dir); gitDir != "" {
			break
		}
		if gitDir = submoduleGitDir(dir); gitDir == "" {
			return "", "", msg.CloneGitElsewhere(g)
		}
		commonDir = gitDir
	default:
		return "", "", msg.CloneGitElsewhere(g)
	}
	for _, d := range []string{gitDir, commonDir} {
		if err := ownObjects(d); err != nil {
			return "", "", err
		}
	}
	return gitDir, commonDir, nil
}

// linkedWorktree returns the git dir and common dir of the linked
// worktree dir, resolved, when git made its link both ways: dir's .git
// file names <repo>/.git/worktrees/<name>, whose gitdir file names dir's
// .git back, and whose commondir leads to that <repo>/.git, a folder with
// no commondir of its own. Else it returns "", "".
func linkedWorktree(dir string) (gitDir, commonDir string) {
	g, err := filepath.EvalSymlinks(filepath.Join(dir, ".git"))
	if err != nil {
		return "", ""
	}
	t := gitdirOf(g, dir)
	repo := filepath.Dir(filepath.Dir(t))
	if t == "" || filepath.Base(filepath.Dir(t)) != "worktrees" || filepath.Base(repo) != ".git" {
		return "", ""
	}
	back, err := gitFile(filepath.Join(t, "gitdir"))
	if err != nil {
		return "", ""
	}
	b := strings.TrimRight(string(back), "\n")
	if !filepath.IsAbs(b) {
		b = filepath.Join(t, b)
	}
	if r, err := filepath.EvalSymlinks(b); err != nil || r != g {
		return "", ""
	}
	if fi, err := os.Lstat(repo); err != nil || !fi.IsDir() {
		return "", ""
	}
	if c, ok := commonOf(t); !ok || c != repo {
		return "", ""
	}
	if c, ok := commonOf(repo); !ok || c != repo {
		return "", ""
	}
	return t, repo
}

// submoduleGitDir returns the git dir of the submodule dir, resolved, when
// it's one git made: dir's .git file names a folder below its parent
// repo's own git dir, modules/<name> (the parent is the nearest folder
// above dir with a .git, itself its own repo, OwnRepo, so a nested
// submodule's is <parent's git dir>/modules/<name> too), resolved, so
// no symbolic link leads it out of that git dir; that folder has no
// commondir, and
// its config's core.worktree, its only one and with no include in the
// file, leads back to dir. Else it returns "".
func submoduleGitDir(dir string) string {
	target := gitdirOf(filepath.Join(dir, ".git"), dir)
	parent := filepath.Dir(dir)
	for ; parent != filepath.Dir(parent); parent = filepath.Dir(parent) {
		if _, err := os.Lstat(filepath.Join(parent, ".git")); err == nil {
			break
		}
	}
	if target == "" || parent == filepath.Dir(parent) {
		return ""
	}
	parentGit, _, err := OwnRepo(parent)
	if err != nil {
		return ""
	}
	rel, err := filepath.Rel(parentGit, target)
	name, ok := strings.CutPrefix(filepath.ToSlash(rel), "modules/")
	if err != nil || !ok || name == "" || !filepath.IsLocal(rel) {
		return ""
	}
	// The guard is the Rel above: target is resolved, so a link anywhere
	// on its way can only have led inside the parent's git dir. Each part
	// below that git dir is checked to be a folder.
	at := parentGit
	for part := range strings.SplitSeq(filepath.ToSlash(rel), "/") {
		at = filepath.Join(at, part)
		if fi, err := os.Lstat(at); err != nil || !fi.IsDir() {
			return ""
		}
	}
	if c, ok := commonOf(target); !ok || c != target {
		return ""
	}
	wt, ok := coreWorktree(filepath.Join(target, "config"))
	if !ok {
		return ""
	}
	if !filepath.IsAbs(wt) {
		wt = filepath.Join(target, wt)
	}
	back, err := filepath.EvalSymlinks(wt)
	own, err2 := filepath.EvalSymlinks(dir)
	if err != nil || err2 != nil || back != own {
		return ""
	}
	return target
}

// coreWorktree is the core.worktree a git config file sets, read as
// plain text, and whether it sets exactly one and includes no other
// file, which this can't follow, so a submodule's git dir with one is
// refused rather than guessed at.
func coreWorktree(path string) (string, bool) {
	data, err := gitFile(path)
	if err != nil {
		return "", false
	}
	var values []string
	section := ""
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			section = strings.ToLower(strings.TrimSpace(strings.Trim(line, "[]")))
			if strings.HasPrefix(section, "include") {
				return "", false
			}
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if section == "core" && found && strings.EqualFold(strings.TrimSpace(key), "worktree") {
			values = append(values, strings.Trim(strings.TrimSpace(value), `"`))
		}
	}
	if len(values) != 1 || values[0] == "" {
		return "", false
	}
	return values[0], true
}

// ownObjects refuses a git dir d that reads objects or refs from
// elsewhere: alternates, or objects, packed refs, a reftable or anything
// in objects or refs that's a symlink (a pack folder, a loose object's
// folder or a ref).
func ownObjects(d string) error {
	alt := filepath.Join(d, "objects", "info", "alternates")
	if _, err := os.Lstat(alt); !errors.Is(err, fs.ErrNotExist) {
		return msg.CloneAlternates(alt)
	}
	for _, name := range []string{"objects", "packed-refs", "reftable", "refs"} {
		if p := filepath.Join(d, name); isLink(p) {
			return msg.CloneGitElsewhere(p)
		}
	}
	for _, name := range []string{"objects", "refs"} {
		var link string
		_ = filepath.WalkDir(filepath.Join(d, name), func(path string, e fs.DirEntry, err error) error {
			if err == nil && e.Type()&fs.ModeSymlink != 0 {
				link = path
				return filepath.SkipAll
			}
			return nil
		})
		if link != "" {
			return msg.CloneGitElsewhere(link)
		}
	}
	return nil
}
