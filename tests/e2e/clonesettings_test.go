//go:build e2e

package e2e

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/devvmhq/devvm/internal/shell"
)

// settingsRepo gives the repo at d settings the copy sends and some it
// never does: its own identity, a remote whose https URL has a user and
// token, a branch ahead at d's detached HEAD, its upstream origin/main,
// which it's ahead of (by the commits d's setup made), a credential helper, an extra header with a
// token, and a pre-commit hook that leaves a file behind when it runs.
func settingsRepo(g cloneGit, d string) error {
	for _, args := range [][]string{
		{"config", "credential.helper", "store"},
		{"config", "http.extraHeader", "Authorization: Bearer ghp_E2E"},
		{"config", "user.name", "Repo Person"},
		{"config", "user.email", "repo@example.com"},
		{"remote", "add", "upstream", "https://u:ghp_E2EURL@example.com/x/y.git"},
		{"branch", "ahead", "HEAD"},
		{"branch", "-q", "--set-upstream-to=origin/main", "ahead"},
	} {
		if _, err := g.git(d, args...); err != nil {
			return err
		}
	}
	hook := filepath.Join(d, ".git", "hooks", "pre-commit")
	return os.WriteFile(hook, []byte("#!/bin/sh\ntouch \"$(git rev-parse --git-dir)/hook-ran\"\n"), 0o700) //nolint:gosec // a hook runs
}

// checkSettings checks the VM's clone of d, a settingsRepo, which vm
// has: origin's URL as the repo has it, upstream's without its user and
// token, ahead's upstream, as far behind it as here, the repo's identity,
// which a commit with no other takes, none of the credential settings,
// and the pre-commit hook, which runs.
func checkSettings(s *S, g cloneGit, d, vm string) {
	s.Check("the repo's remotes, upstreams, identity and hooks go to the VM's clone, its credentials don't", func() error {
		if vm == "" {
			return errors.New("no VM was made")
		}
		host, err := g.git(d, "remote", "get-url", "origin")
		if err != nil {
			return err
		}
		track, err := g.git(d, "for-each-ref", "--format=track %(upstream:track)", "refs/heads/ahead")
		if err != nil || !strings.HasPrefix(track.Stdout, "track [ahead ") {
			return errors.New("ahead isn't ahead of origin/main here: " + track.Stdout)
		}
		in := "cd " + shell.Quote(d) + " && "
		r, err := s.InVM(vm, in+"git remote get-url origin && git remote get-url upstream && git config user.name && git for-each-ref --format='track %(upstream:track)' refs/heads/ahead && git config --local --list")
		switch {
		case err != nil:
			return err
		case !HasLine(r.Stdout, strings.TrimSpace(host.Stdout)):
			return errors.New("the VM's origin isn't the repo's:\n" + r.Stdout)
		case !HasLine(r.Stdout, "https://example.com/x/y.git"):
			return errors.New("the VM's upstream isn't the repo's, cleaned:\n" + r.Stdout)
		case !HasLine(r.Stdout, strings.TrimSpace(track.Stdout)):
			return errors.New("the VM's ahead isn't as far ahead of its upstream as here (" + strings.TrimSpace(track.Stdout) + "):\n" + r.Stdout)
		case !HasLine(r.Stdout, "Repo Person"):
			return errors.New("the VM's clone hasn't the repo's identity")
		case strings.Contains(strings.ToLower(r.Stdout), "credential") || strings.Contains(r.Stdout, "ghp_"):
			return errors.New("a credential setting is in the VM's config:\n" + r.Stdout)
		}
		h, err := s.InVM(vm, in+"git commit -q --allow-empty -m hooked && test -e .git/hook-ran && git log -1 --format='author %an <%ae>' && git reset -q --soft HEAD~ && echo ran")
		switch {
		case err != nil || !HasLine(h.Stdout, "ran"):
			return errors.New("the pre-commit hook didn't run in the VM")
		case !HasLine(h.Stdout, "author Repo Person <repo@example.com>"):
			return errors.New("a commit in the VM didn't take the repo's identity:\n" + h.Stdout)
		}
		return nil
	})
}
