//go:build e2e

package e2e

import (
	"errors"
	"strings"

	"github.com/devvmhq/devvm/internal/shell"
)

// partialRemote is a small public repo with more than one commit, whose
// old blobs a blob:none clone lacks.
const partialRemote = "https://github.com/octocat/Spoon-Knife.git"

// partialTitles are clonePartial's checks.
func partialTitles() []string {
	return []string{
		"a partial clone's VM fetches a blob it lacks from the repo's origin when it needs it",
		"fetching back work that needs a blob the VM's partial clone lacks says what to do, and works once the VM has it",
	}
}

// clonePartial: a blob:none clone of a public repo, on the internet tier:
// the VM's clone is a promisor clone, and its git fetches a blob the
// clone lacks from the repo's origin when a command needs it. Work in the
// VM that names another such blob doesn't come back, as the VM's
// upload-pack fetches nothing to send it, and devvm says what to do; once
// git in the VM has read the blob, it does. Skipped when github.com
// can't be reached.
func clonePartial(s *S, g cloneGit) {
	d, err := s.NewProject("cloned-partial")
	if err == nil {
		if _, cerr := g.git(d, "clone", "-q", "--filter=blob:none", partialRemote, "."); cerr != nil {
			for _, title := range partialTitles() {
				s.Skip(title, "github.com can't be reached: "+cerr.Error())
			}
			return
		}
	}
	var missing []string
	if err == nil {
		// Blobs of the history the clone hasn't fetched (?SHA).
		var r Res
		if r, err = g.git(d, "rev-list", "--objects", "--missing=print", "--all"); err == nil {
			for line := range strings.Lines(r.Stdout) {
				if sha, ok := strings.CutPrefix(strings.TrimSpace(line), "?"); ok {
					missing = append(missing, sha)
				}
			}
		}
	}
	var vm string
	if err == nil {
		_, _ = g.devvm(d, "pwd\n", "--clone", "--network", "internet")
		vm = g.made(d)
	}
	in := "cd " + shell.Quote(d) + " && "
	s.Check(partialTitles()[0], func() error {
		switch {
		case err != nil:
			return err
		case len(missing) < 2:
			return errors.New("the partial clone lacks fewer than two blobs")
		case vm == "":
			return errors.New("no VM was made")
		}
		r, err := s.InVM(vm, in+"git config remote.origin.promisor && git cat-file -t "+missing[0])
		if err != nil || !HasLine(r.Stdout, "true") || !HasLine(r.Stdout, "blob") {
			return errors.New("the VM's clone didn't fetch the missing blob from origin:\n" + r.Out)
		}
		return nil
	})
	s.Check(partialTitles()[1], func() error {
		if err != nil || len(missing) < 2 || vm == "" {
			return errors.New("no partial clone in a VM")
		}
		// A commit whose tree names a blob neither side has.
		mk := in + "t=$(printf '100644 blob %s\\tcopy\\n' " + missing[1] + " | git mktree --missing) && " +
			"c=$(git -c user.name=e -c user.email=e@e commit-tree \"$t\" -p HEAD -m needs-blob) && git update-ref refs/heads/needs-blob \"$c\""
		if r, err := s.InVM(vm, mk); err != nil {
			return errors.New("making the commit: " + r.Out)
		}
		r, _ := g.devvm(d, "", "clone", "fetch")
		if !g.said(r, "clone_fetch_partial", "path=.") {
			return errors.New("devvm clone fetch didn't say the VM lacked an object:\n" + r.Out)
		}
		if r, err := s.InVM(vm, in+"git cat-file -t "+missing[1]); err != nil || !HasLine(r.Stdout, "blob") {
			return errors.New("the VM's git didn't fetch the blob:\n" + r.Out)
		}
		if r, err := g.devvm(d, "", "clone", "fetch"); err != nil {
			return errors.New("devvm clone fetch failed once the VM had the blob:\n" + r.Out)
		}
		if _, err := g.git(d, "rev-parse", "--verify", "devvm/needs-blob"); err != nil {
			return errors.New("the VM's work didn't come back")
		}
		return nil
	})
	_, _ = g.devvm(d, "", "rm", "--force", "--no-export", "--no-clone-check")
}
