package vm

import (
	"bytes"
	"context"
	"errors"
	"strings"

	"github.com/devvmhq/devvm/internal/hostgit"
	"github.com/devvmhq/devvm/internal/msg"
)

// Saving what only a --clone VM has into local branches of the repo on
// this machine, saved/<remote>/<name>, for rm, clone off and devvm clone
// fetch alike. A VM's stash entry is saved without its untracked files:
// git stash apply writes them, ignored ones too, such as a virtualenv's
// sitecustomize.py that Python runs on its own, and git stash show
// doesn't list them, so the VM could plant files on this machine that no
// one looked at. The entry is made again here from its tree and its first
// two parents (stashWithout), and its untracked files go to a branch of
// their own, named, to look at before using (sayUntracked).

// errStashShape is stashWithout's error for a stash entry that isn't
// what git stash makes.
var errStashShape = errors.New("the VM's stash entry isn't one git stash makes")

// hasCommit is whether the repo at dir has the commit sha.
func hasCommit(ctx context.Context, dir, sha string) bool {
	_, err := hostGit(ctx, dir, "cat-file", "-e", sha+"^{commit}")
	return err == nil
}

// fetchSaved fetches refs, which savedNames named, from the clone of the
// repo at dir in t's VM into local branches under prefix, as step,
// failing with fail: its branches and detached HEAD at once, by refspec,
// then each stash entry alone, by its commit, saved by saveStash. One
// that fails is named (clone_stash_fetch_failed) and left in the VM, or,
// strict, as for a VM about to go, fails the fetch with fail. It returns
// the branches it saved.
func (e clones) fetchSaved(ctx context.Context, t *target, dir, prefix string, refs []fetchRef, strict bool, step, fail msg.M) ([]string, error) {
	var saved, refspecs []string
	for _, r := range refs {
		if !r.stash {
			saved = append(saved, prefix+"/"+r.name)
			refspecs = append(refspecs, r.src+":refs/heads/"+prefix+"/"+r.name)
		}
	}
	if len(refspecs) > 0 {
		if err := e.fetchInto(ctx, t, dir, strict, step, fail, refspecs...); err != nil {
			return nil, err
		}
	}
	for _, r := range refs {
		if !r.stash {
			continue
		}
		branch := prefix + "/" + r.name
		err := e.fetchInto(ctx, t, dir, strict, step, fail, r.src)
		if err == nil {
			err = e.saveStash(ctx, dir, r.src, branch)
		}
		if ctx.Err() != nil {
			return saved, ctx.Err()
		}
		if err != nil {
			e.Warn(msg.CloneStashFetchFailed(r.name))
			if strict {
				return saved, fail
			}
			continue
		}
		saved = append(saved, branch)
	}
	return saved, nil
}

// saveStash saves the stash entry sha, fetched, as the new local branch
// branch: as it is when it has no untracked files, else made again
// without them (stashWithout), which go to branch-untracked, named
// (sayUntracked).
func (e clones) saveStash(ctx context.Context, dir, sha, branch string) error {
	untracked, err := hostGit(ctx, dir, "rev-parse", "-q", "--verify", sha+"^3")
	if err != nil || !objectName.MatchString(untracked) {
		_, err := hostGit(ctx, dir, "update-ref", "refs/heads/"+branch, sha, "")
		return err
	}
	without, err := stashWithout(ctx, dir, sha)
	if err != nil {
		return err
	}
	if _, err := hostGit(ctx, dir, "update-ref", "refs/heads/"+branch, without, ""); err != nil {
		return err
	}
	if _, err := hostGit(ctx, dir, "update-ref", "refs/heads/"+branch+"-untracked", untracked, ""); err != nil {
		return err
	}
	e.sayUntracked(ctx, dir, branch, branch+"-untracked")
	return nil
}

// stashWithout is the stash entry sha made again without its untracked
// files: a commit of its tree, its first parent (the commit it was made
// on) and its second (the index), with its own message, author and
// committer, so the same entry is always the same commit.
func stashWithout(ctx context.Context, dir, sha string) (string, error) {
	// sha is an object name (objectName), never an option.
	parents, err := hostGit(ctx, dir, "rev-parse", sha+"^1", sha+"^2")
	if err != nil {
		return "", err
	}
	p := strings.Fields(parents)
	who, err := hostGit(ctx, dir, "log", "-1", "--format=%an%x00%ae%x00%ad%x00%cn%x00%ce%x00%cd", "--date=raw", sha)
	if err != nil {
		return "", err
	}
	id := strings.Split(who, "\x00")
	message, err := hostgit.Output(ctx, "-C", dir, "log", "-1", "--format=%B", sha)
	if err != nil || len(p) != 2 || len(id) != 6 {
		return "", errStashShape
	}
	cmd := hostgit.Command(ctx, "-C", dir, "commit-tree", sha+"^{tree}", "-p", p[0], "-p", p[1])
	cmd.Env = append(cmd.Env, "GIT_AUTHOR_NAME="+id[0], "GIT_AUTHOR_EMAIL="+id[1], "GIT_AUTHOR_DATE="+id[2],
		"GIT_COMMITTER_NAME="+id[3], "GIT_COMMITTER_EMAIL="+id[4], "GIT_COMMITTER_DATE="+id[5])
	cmd.Stdin = bytes.NewReader(message)
	out, err := cmd.Output()
	c := strings.TrimSpace(string(out))
	if err != nil || !objectName.MatchString(c) {
		return "", errStashShape
	}
	return c, nil
}

// sayUntracked names the untracked files of the stash entry saved as
// branch, kept apart in untracked, and those of them git on this machine
// ignores.
func (e clones) sayUntracked(ctx context.Context, dir, branch, untracked string) {
	out, _ := hostgit.Output(ctx, "-C", dir, "ls-tree", "-r", "-z", "--name-only", untracked)
	paths := nulPaths(out)
	e.Warn(msg.CloneStashUntracked(branch, len(paths), shown(paths), untracked))
	if len(out) == 0 {
		return
	}
	cmd := hostgit.Command(ctx, "-C", dir, "check-ignore", "--no-index", "--stdin", "-z")
	cmd.Stdin = bytes.NewReader(out)
	ignoredOut, _ := cmd.Output() // exits 1 when it ignores none
	if ignored := nulPaths(ignoredOut); len(ignored) > 0 {
		e.Warn(msg.CloneStashUntrackedIgnored(len(ignored), shown(ignored), untracked))
	}
}

// nulPaths are the paths in b, each ending in a NUL.
func nulPaths(b []byte) []string {
	var out []string
	for p := range strings.SplitSeq(string(b), "\x00") {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
