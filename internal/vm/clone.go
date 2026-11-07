package vm

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/gitprotect"
	"github.com/devvmhq/devvm/internal/hostgit"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/project"
	"github.com/devvmhq/devvm/internal/shell"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/vmname"
	"github.com/devvmhq/devvm/internal/vmssh"
)

// --clone: the VM works on its own clone of the repo instead of mounting
// the folder, so nothing in the VM can reach the working tree or .git.
// The host pushes its branches into the VM once, and the repo gets a git
// remote to fetch the VM's work from, by the VM's full name in devvm's ssh
// config. The clone is at the folder's path in the VM, so shells and
// editors open in the same place.

// cloneShown is how many of the clone's uncommitted changes rm shows, in
// all its worktrees.
const cloneShown = 20

// cloneOutMax is the most of the VM's answer read for its branches, its
// worktrees or a worktree's changes.
const cloneOutMax = 1 << 20

// objectName matches a git object name: what a VM answers is only looked
// up when it's one.
var objectName = regexp.MustCompile(`^[0-9a-f]{40,64}$`)

// clones makes a --clone VM's clone, and checks and removes what the repo
// has of it when the VM goes.
type clones struct{ *Env }

// isClone reports whether the VM has its own clone of the repo.
func isClone(st *state.VM) bool { return sectionOf[cloneInVM](st, cloneSection).On }

// cloneURL is the remote URL of vm's clone of dir: the VM's full name in
// devvm's ssh config, which never changes, and the clone's path.
func cloneURL(vm, dir string) string { return vm + ":" + dir }

// hostGit runs git in dir on the host (hostgit) and returns its output,
// trimmed.
func hostGit(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := hostgit.Output(ctx, append([]string{"-C", dir}, args...)...)
	return strings.TrimSpace(string(out)), err
}

// checkCloneRepo refuses --clone for a folder that isn't the top of a git
// repo of its own (cloneSource) with a commit and a branch checked out.
func checkCloneRepo(ctx context.Context, dir string) error {
	if _, err := exec.LookPath("git"); err != nil {
		return msg.CloneNeedsGit()
	}
	if top, err := hostGit(ctx, dir, "rev-parse", "--show-toplevel"); err != nil || top != dir {
		return msg.CloneNotTop(dir)
	}
	if _, err := cloneSource(ctx, dir); err != nil {
		return err
	}
	if _, err := hostGit(ctx, dir, "rev-parse", "-q", "--verify", "HEAD"); err != nil {
		return msg.CloneNoCommits()
	}
	if _, err := hostGit(ctx, dir, "symbolic-ref", "-q", "HEAD"); err != nil {
		return msg.CloneDetached()
	}
	return nil
}

// cloneSource checks that what git reads in dir, a repo's top folder, is
// that repo's own (gitprotect.OwnRepo), and that git run there reads the
// same git dir and common dir. It reports whether dir is a linked
// worktree of a repo elsewhere.
func cloneSource(ctx context.Context, dir string) (worktree bool, err error) {
	gitDir, common, err := gitprotect.OwnRepo(dir)
	if err != nil {
		return false, err
	}
	out, err := hostGit(ctx, dir, "rev-parse", "--path-format=absolute", "--absolute-git-dir", "--git-common-dir")
	got := strings.Split(out, "\n")
	if err != nil || len(got) != 2 || resolved(got[0]) != gitDir || resolved(got[1]) != common {
		return false, msg.CloneGitElsewhere(filepath.Join(dir, ".git"))
	}
	return gitDir != common, nil
}

// resolved is p with its symlinks resolved, or "" when it can't be.
func resolved(p string) string {
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		return ""
	}
	return r
}

// seedRefs are what the seed push sends: every branch of a repo; of a
// linked worktree, whose repo is elsewhere, only its branch and that
// branch's upstream, if it has one and it's one ref (the repo's config
// can make it a pattern, which would push every branch).
func seedRefs(ctx context.Context, dir, branch string, worktree bool) []string {
	if !worktree {
		return []string{"--all"}
	}
	refs := []string{"refs/heads/" + branch + ":refs/heads/" + branch}
	if up, err := hostGit(ctx, dir, "for-each-ref", "--format=%(upstream)", "refs/heads/"+branch); err == nil && strings.HasPrefix(up, "refs/") && up != "refs/heads/"+branch && validRef(ctx, dir, up) {
		refs = append(refs, up+":"+up)
	}
	return refs
}

// gitSSH is host git (hostgit) run in t's project folder with its ssh
// (gitSSHCommand) reaching t's running VM alone (target.ownSSH).
func (e clones) gitSSH(ctx context.Context, t *target, args ...string) (*exec.Cmd, error) {
	dir := t.f.Dir
	s, err := t.ownSSH(ctx)
	if err != nil {
		return nil, err
	}
	cmd := hostgit.Command(ctx, append([]string{"-C", dir}, args...)...)
	cmd.Env = append(cmd.Env, "GIT_SSH_COMMAND="+gitSSHCommand(s, dir))
	return cmd, nil
}

// gitSSHCommand is git's ssh for a --clone VM's push and fetch: a shell
// function, as git runs GIT_SSH_COMMAND through sh -c with its own
// arguments after it. It ignores the host, port and options git passes,
// so no url.*.insteadOf or remote URL in the repo's config can send it
// anywhere else, and connects to the VM alone (vmssh.SSH's CommandArgs:
// its loopback port and key, the host keys devvm pinned for its start
// under HostKeyAlias, BatchMode, nothing forwarded). There it runs
// git-receive-pack or git-upload-pack, whichever git's last argument
// asks for, on the clone at dir, whatever path git asked for; anything
// else fails.
func gitSSHCommand(s vmssh.SSH, dir string) string {
	run := func(verb string) string {
		return "exec " + shell.QuoteJoin(append([]string{"ssh"}, s.CommandArgs(verb+" "+shell.Quote(dir))...))
	}
	return `f() { for c; do :; done; case $c in "git-receive-pack "*) ` + run("git-receive-pack") +
		` ;; "git-upload-pack "*) ` + run("git-upload-pack") + ` ;; esac; exit 1; }; f`
}

// seedClone makes the running VM's clone: a new repo at the folder's path
// that takes pushes to its checked-out branch, every branch of the
// host's pushed into it, the host's checked out, and a remote for it in
// the host's repo. Then it offers the ~/.ssh/config line that remote
// needs, and warns about uncommitted changes, which aren't copied. It
// needs the VM's ssh config entry. Its command in the VM carries the
// VM's agent (target.exec).
func (e clones) seedClone(ctx context.Context, t *target) error {
	vm, st, f, dir := t.f.Name, t.st, t.f, t.f.Dir
	branch, err := hostGit(ctx, dir, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return msg.CloneDetached()
	}
	// Checked again, as the folder may have changed since the VM was made.
	worktree, err := cloneSource(ctx, dir)
	if err != nil {
		return err
	}
	remote, err := freeRemote(ctx, f)
	if err != nil {
		return err
	}
	const initRepo = `sudo mkdir -p -- "$1" && sudo chown -- "$(id -u):$(id -g)" "$1" && cd -- "$1" &&
git init -q && git config receive.denyCurrentBranch updateInstead && git symbolic-ref HEAD "refs/heads/$2"`
	log := e.vmLog()
	if err := t.exec(ctx, []string{"sh", "-c", initRepo, "sh", dir, branch}, backend.ExecOpts{Stdout: log, Stderr: log}); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return backend.Logged(msg.CloneSeedFailed(vm))
	}
	e.Info(msg.CopyingBranches())
	push := append([]string{"push", "-q", "--no-verify", "--no-recurse-submodules", "--no-signed", cloneURL(vm, dir)}, seedRefs(ctx, dir, branch, worktree)...)
	cmd, err := e.gitSSH(ctx, t, push...)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return msg.CloneSeedFailed(vm).Because(err)
	}
	if err := e.LoggedCmd(ctx, msg.CopyingBranches(), cmd); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return backend.Logged(msg.CloneSeedFailed(vm))
	}
	// Recorded first: rm and an interrupt remove it, if it's the VM's.
	if err := update(ctx, st, cloneSection, func(c *cloneInVM) error { c.Made.Remote = remote; return nil }); err != nil {
		return err
	}
	if _, ok := remoteURL(ctx, dir, remote); !ok {
		if _, err := hostGit(ctx, dir, "remote", "add", remote, cloneURL(vm, dir)); err != nil {
			return msg.RemoteAddFailed(remote)
		}
	}
	if err := update(ctx, st, cloneSection, func(c *cloneInVM) error { c.Made.Seeded = true; return nil }); err != nil {
		return err
	}
	if ok, err := e.sshConfig().EnsureInclude(e.Dirs.Home); err != nil {
		return err
	} else if !ok {
		e.Warn(msg.FetchUntil(e.Dirs.SSHConfig(), remote))
	}
	if dirty, _ := hostgit.Status(ctx, dir, "--porcelain", "--untracked-files=no").Output(); len(bytes.TrimSpace(dirty)) > 0 {
		e.Warn(msg.UncommittedNotCopied(branch, dir, remote))
	}
	e.Info(msg.CloneReady(branch, remote))
	return nil
}

// freeRemote is the name of the repo's remote for the folder's VM: devvm,
// else devvm-<folder>, else the VM's name, whichever is free or already
// the VM's.
func freeRemote(ctx context.Context, f project.Folder) (string, error) {
	url := cloneURL(f.Name, f.Dir)
	for _, r := range []string{"devvm", vmname.Prefix + f.Slug(), f.Name} {
		if got, ok := remoteURL(ctx, f.Dir, r); !ok || got == url {
			return r, nil
		}
	}
	return "", msg.RemotesTaken(f.Slug(), f.Name)
}

// remoteURL returns the URL of dir's remote by that name, if it has one.
func remoteURL(ctx context.Context, dir, name string) (string, bool) {
	url, err := hostGit(ctx, dir, "remote", "get-url", name)
	return url, err == nil
}

// cloneCheck checks a --clone VM's clone before a command loses it:
// command, as typed, which its messages name, deletes the VM (devvm rm),
// or makes it again mounting the folder (remakes: devvm clone off).
type cloneCheck struct {
	clones
	command string
	remakes bool
}

// run offers to fetch the branches whose commits the host's repo lacks,
// and asks before uncommitted changes in the clone or its worktrees
// (changes) are lost, starting the VM if it's stopped. Where it can't
// ask, either refuses. It reports whether to keep the VM as it is.
// A VM never seeded has nothing of its own.
func (e cloneCheck) run(ctx context.Context, t *target) (bool, error) {
	vm, st, b, dir := t.f.Name, t.st, t.b, t.f.Dir
	c := sectionOf[cloneInVM](st, cloneSection)
	if !c.Made.Seeded {
		return false, nil
	}
	remote := c.Made.Remote
	if err := t.ensureRunning(ctx); err != nil {
		return false, e.notStarted(ctx, vm, err)
	}
	refs, err := e.git(ctx, b, vm, dir, "for-each-ref", "--format=%(objectname) %(refname)", "refs/heads")
	if err != nil {
		return false, err
	}
	if unfetched := unfetchedRefs(ctx, dir, refs); len(unfetched) > 0 {
		e.Say(msg.UnfetchedCommits(quoted(unfetched)))
		if err := e.fetchUnfetched(ctx, t, remote, unfetched); err != nil {
			return false, err
		}
	}
	if dirty, err := e.changes(ctx, b, vm, dir); err != nil || !dirty {
		return false, err
	}
	q := msg.DeleteAnyway()
	if e.remakes {
		q = msg.RemakeAnyway()
	}
	yes, err := e.Ask(q.RefusedWith(msg.NoTerminalDelete(e.command)))
	if err != nil {
		return false, err
	}
	if !yes {
		e.Info(msg.Kept(vm))
		return true, nil
	}
	return false, nil
}

// notStarted is the error when the VM didn't start for its clone check
// (err): what kept it from starting, said, then the way out, the command
// with --no-clone-check, carrying err. An interrupt, or an error that
// isn't a message, is err as it is.
func (e cloneCheck) notStarted(ctx context.Context, vm string, err error) error {
	m, ok := errors.AsType[msg.M](err)
	if ctx.Err() != nil || !ok {
		return err
	}
	e.Warn(m)
	out := msg.CloneCheckNotStarted(e.command, vm).Because(err)
	if _, logged := errors.AsType[*backend.LoggedError](err); logged {
		return backend.Logged(out)
	}
	return out
}

// changes shows the uncommitted changes in the VM's clone and in its
// linked worktrees inside it (cloneWorktrees), each under its heading,
// at most cloneShown in all, and reports whether there are any. It stops
// checking once it has shown cloneShown. A clone with more worktrees
// than devvm checks counts as changed, and devvm says so.
func (e cloneCheck) changes(ctx context.Context, b backend.Backend, vm, dir string) (bool, error) {
	list, err := e.git(ctx, b, vm, dir, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return false, err
	}
	trees, more := cloneWorktrees(dir, list)
	shown, dirty := 0, false
	for _, wt := range append([]string{dir}, trees...) {
		if shown >= cloneShown {
			break
		}
		var out []byte
		if wt == dir {
			out, err = e.git(ctx, b, vm, dir, "status", "--porcelain")
		} else {
			out, err = e.exec(ctx, b, vm, []string{"sh", "-c", worktreeStatus, "sh", wt})
		}
		if err != nil {
			return false, err
		}
		if len(bytes.TrimSpace(out)) == 0 {
			continue
		}
		dirty = true
		if wt == dir {
			e.Say(msg.CloneDirty())
		} else {
			e.Say(msg.CloneWorktreeDirty(wt))
		}
		shown += e.showChanges(out, cloneShown-shown)
	}
	if more > 0 {
		e.Say(msg.CloneWorktreesUnchecked(strconv.Itoa(cloneWorktreesMax)))
		dirty = true
	}
	return dirty, nil
}

// worktreeStatus is git status in a linked worktree, $1, in the VM: one
// whose folder is gone has nothing to lose.
const worktreeStatus = `[ -d "$1" ] || exit 0; exec git -C "$1" status --porcelain`

// showChanges prints the first n lines of git status's output, each
// through msg.OneLine, and returns how many it printed.
func (e clones) showChanges(status []byte, n int) int {
	var shown strings.Builder
	s := bufio.NewScanner(bytes.NewReader(status))
	i := 0
	for ; i < n && s.Scan(); i++ {
		shown.WriteString(msg.OneLine(s.Text()) + "\n")
	}
	_, _ = e.Stdout().Write([]byte(shown.String()))
	return i
}

// cloneWorktreesMax is how many of the clone's linked worktrees rm checks.
const cloneWorktreesMax = 100

// cloneWorktrees returns, from the VM's git worktree list --porcelain -z
// (records of NUL-ended "key value" lines, each record ended by an empty
// line), the linked worktrees rm checks, at most cloneWorktreesMax, and
// how many more there are. The VM's answer is untrusted: only a clean
// absolute path inside dir is kept, each once; bare and prunable ones are
// skipped.
func cloneWorktrees(dir string, list []byte) (trees []string, more int) {
	seen := map[string]bool{}
	var wt string
	skip := false
	for line := range strings.SplitSeq(string(list), "\x00") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			wt, skip = strings.TrimPrefix(line, "worktree "), false
		case line == "bare" || line == "prunable" || strings.HasPrefix(line, "prunable "):
			skip = true
		case line == "":
			if !skip && insideClone(dir, wt) && !seen[wt] {
				seen[wt] = true
				if len(trees) < cloneWorktreesMax {
					trees = append(trees, wt)
				} else {
					more++
				}
			}
			wt, skip = "", false
		}
	}
	return trees, more
}

// insideClone reports whether p, a path the VM gave, is a clean absolute
// path below dir.
func insideClone(dir, p string) bool {
	return path.IsAbs(p) && path.Clean(p) == p && paths.Below(p, dir)
}

// git runs git on the VM's clone and returns what it printed, at most
// cloneOutMax bytes.
func (e cloneCheck) git(ctx context.Context, b backend.Backend, vm, dir string, args ...string) ([]byte, error) {
	return e.exec(ctx, b, vm, append([]string{"git", "-C", dir}, args...))
}

// exec runs argv in the VM and returns what it printed, at most
// cloneOutMax bytes.
func (e cloneCheck) exec(ctx context.Context, b backend.Backend, vm string, argv []string) ([]byte, error) {
	out := &bounded{max: cloneOutMax}
	if err := b.Exec(ctx, vm, argv, backend.ExecOpts{Stdout: out, Stderr: e.vmLog()}); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, msg.CloneCheckFailed(e.command, vm)
	}
	return out.Bytes(), nil
}

// unfetchedRefs returns, from the VM's for-each-ref lines ("SHA
// refs/heads/BRANCH"), the branches with commits no ref in dir's repo
// has, as the VM named them: untrusted, so checked (validBranch) before
// use and quoted when shown.
func unfetchedRefs(ctx context.Context, dir string, refs []byte) []string {
	var out []string
	s := bufio.NewScanner(bytes.NewReader(refs))
	for s.Scan() {
		sha, ref, _ := strings.Cut(strings.TrimSpace(s.Text()), " ")
		if sha == "" {
			continue
		}
		if objectName.MatchString(sha) {
			if got, err := hostGit(ctx, dir, "rev-list", "-n1", sha, "--not", "--all"); err == nil && got == "" {
				continue
			}
		}
		out = append(out, strings.TrimPrefix(ref, "refs/heads/"))
	}
	return out
}

// fetchUnfetched offers to fetch the VM's unfetched branches into new
// local branches, saved/<remote>/<branch> (savedPrefix), never over one
// that exists. Branches whose names git refuses are skipped.
func (e cloneCheck) fetchUnfetched(ctx context.Context, t *target, remote string, unfetched []string) error {
	vm, dir := t.f.Name, t.f.Dir
	var good, bad []string
	for _, br := range unfetched {
		if validBranch(ctx, dir, br) {
			good = append(good, br)
		} else {
			bad = append(bad, br)
		}
	}
	if len(bad) > 0 {
		e.Warn(msg.BranchNamesInvalid(quoted(bad)))
	}
	if len(good) == 0 {
		return nil
	}
	prefix, ok := savedPrefix(ctx, dir, remote)
	if !ok {
		return msg.SavedNamesTaken(e.command, remote, vm)
	}
	fetch, err := e.Confirm(msg.FetchBeforeDelete(prefix).RefusedWith(msg.NoTerminalFetch(e.command, remote)))
	if err != nil || !fetch {
		return err
	}
	if err := e.writeSSHConfig(ctx); err != nil {
		return err
	}
	// --refmap= : only the refspecs given, never a remote's of the same
	// URL, whose own could update other branches.
	args := []string{"fetch", "--no-tags", "--no-recurse-submodules", "--refmap=", cloneURL(vm, dir)}
	saved := make([]string, len(good))
	for i, br := range good {
		saved[i] = prefix + "/" + br
		args = append(args, "refs/heads/"+br+":refs/heads/"+saved[i])
	}
	cmd, err := e.gitSSH(ctx, t, args...)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return msg.FetchFailed(e.command, vm).Because(err)
	}
	if err := e.LoggedCmd(ctx, msg.Fetching(prefix), cmd); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return backend.Logged(msg.FetchFailed(e.command, vm))
	}
	e.Info(msg.SavedBranches(quoted(saved)))
	return nil
}

// validBranch reports whether git takes name as a branch name.
func validBranch(ctx context.Context, dir, name string) bool {
	return validRef(ctx, dir, "refs/heads/"+name)
}

// validRef reports whether git takes ref as one ref's full name: no
// pattern (*) among them.
func validRef(ctx context.Context, dir, ref string) bool {
	_, err := hostGit(ctx, dir, "check-ref-format", ref)
	return err == nil
}

// savedPrefixMax is how many names savedPrefix tries.
const savedPrefixMax = 99

// savedPrefix is where a deleted VM's branches are kept, as local
// branches: saved/<remote>, else saved/<remote>-2 and on to -99, the first
// that no local branch is, is in or holds. A remote devvm picks starts
// with devvm, so these never look like a later VM's remote-tracking
// branches.
func savedPrefix(ctx context.Context, dir, remote string) (string, bool) {
	out, err := hostGit(ctx, dir, "for-each-ref", "--format=%(refname)", "refs/heads")
	if err != nil {
		return "", false
	}
	heads := strings.Fields(out)
	for n := 1; n <= savedPrefixMax; n++ {
		p := "refs/heads/saved/" + remote
		if n > 1 {
			p += "-" + strconv.Itoa(n)
		}
		free := true
		for _, h := range heads {
			if paths.Within(h, p) || paths.Below(p, h) {
				free = false
				break
			}
		}
		if free {
			return strings.TrimPrefix(p, "refs/heads/"), true
		}
	}
	return "", false
}

// quoted is names, each quoted, space-separated.
func quoted(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = strconv.Quote(n)
	}
	return strings.Join(q, " ")
}

// removeRemote removes the repo's remote for the deleted VM, if it still
// is the VM's, with its remote-tracking branches and upstream settings.
// Fetched branches holding commits no other ref has are kept as local
// branches first (saveFetched); if that fails, the remote stays.
func (e clones) removeRemote(ctx context.Context, dir, vm, remote string) {
	if remote == "" {
		return
	}
	if url, ok := remoteURL(ctx, dir, remote); !ok || url != cloneURL(vm, dir) {
		return
	}
	if err := e.saveFetched(ctx, dir, remote); err != nil {
		e.Warn(msg.RemoteKept(remote))
		return
	}
	if _, err := hostGit(ctx, dir, "remote", "remove", remote); err == nil {
		e.Info(msg.RemovedRemote(remote))
	}
}

// saveFetched makes a new local branch, under savedPrefix, of each of
// remote's remote-tracking branches with commits no other ref has, all at
// once and never over a branch that exists.
func (e clones) saveFetched(ctx context.Context, dir, remote string) error {
	tracking := "refs/remotes/" + remote + "/"
	out, err := hostGit(ctx, dir, "for-each-ref", "--format=%(objectname) %(refname)", tracking)
	if err != nil {
		return err
	}
	var input strings.Builder
	var saved []string
	prefix := ""
	for line := range strings.Lines(out) {
		sha, ref, _ := strings.Cut(strings.TrimSpace(line), " ")
		br := strings.TrimPrefix(ref, tracking)
		if sha == "" || br == "HEAD" || !validBranch(ctx, dir, br) {
			continue
		}
		if only, err := hostGit(ctx, dir, "rev-list", "-n1", sha, "--not", "--exclude="+tracking+"*", "--all"); err != nil || only == "" {
			continue
		}
		if prefix == "" {
			var ok bool
			if prefix, ok = savedPrefix(ctx, dir, remote); !ok {
				return errors.New("no free saved/ branch names")
			}
		}
		saved = append(saved, prefix+"/"+br)
		input.WriteString("create refs/heads/" + prefix + "/" + br + " " + sha + "\n")
	}
	if len(saved) == 0 {
		return nil
	}
	cmd := hostgit.Command(ctx, "-C", dir, "update-ref", "--stdin")
	cmd.Stdin = strings.NewReader(input.String())
	if err := cmd.Run(); err != nil {
		return err
	}
	e.Info(msg.SavedBranches(quoted(saved)))
	return nil
}

// SetClone makes the folder's VM again from its image in mode, CloneOn
// (its own clone of the repo) or CloneOff (the folder mounted), after
// the user agrees (remakeTo), then opens a shell in it; a VM in that
// mode already is said to be, and nothing changes. On needs a folder
// that's a repo's top with a commit and a branch (checkCloneRepo), and
// the clone is made as its shell opens (seedClone), as a new VM's is.
// Off checks the clone first as devvm rm does (cloneCheck), unless
// --no-clone-check, which warns that its work is lost instead. command
// is the command as typed. A remake of it that didn't finish is finished
// first.
func (e Lifecycle) SetClone(ctx context.Context, command, mode string, o RemakeOptions) error {
	t, resumed, release, err := e.toRemake(ctx, command, true)
	if err != nil || t == nil {
		return err
	}
	defer release()
	vm, on := t.f.Name, mode == CloneOn
	if isClone(t.st) == on {
		e.Info(msg.SettingAlready(command, vm))
		if resumed {
			return e.launcher.open(ctx, Options{}, t)
		}
		return nil
	}
	p := plan(t.st.Image, command, o)
	p.Clone = mode
	if err := e.canSwitch(ctx, t, p, o); err != nil {
		return err
	}
	if ok, err := e.agreed(t, p, o); err != nil || !ok {
		return err
	}
	if !on && !o.NoCloneCheck {
		keep, err := cloneCheck{clones: e.clones, command: command, remakes: true}.run(ctx, t)
		if err != nil || keep {
			return err
		}
	}
	return e.remakeNow(ctx, t, p)
}

// canSwitch refuses to make t's VM again in p's clone mode when
// remaking.check does, or, for its own clone, when the folder isn't a
// repo it can clone (checkCloneRepo). Then it says what changes, and for
// the folder mounted with --no-clone-check, what's lost.
func (e Lifecycle) canSwitch(ctx context.Context, t *target, p remakeInVM, o RemakeOptions) error {
	vm := t.f.Name
	if p.Clone == CloneOn {
		if err := checkCloneRepo(ctx, t.f.Dir); err != nil {
			return err
		}
	}
	if err := e.remaking.check(t, p); err != nil {
		return err
	}
	switch {
	case p.Clone == CloneOn:
		e.Info(msg.CloneSwitchOn(vm))
	case o.NoCloneCheck:
		e.Info(msg.CloneSwitchOff(vm))
		e.Warn(msg.CloneCheckSkipped(p.Command, vm))
	default:
		e.Info(msg.CloneSwitchOff(vm))
	}
	return nil
}

// switchedTo sets st's sections for the clone mode a remake made its new
// machine in (clone: CloneOn, CloneOff, or "" for the VM's): its own
// clone, made as its shell opens, or the folder mounted, with git its
// .git protection (gitprotect.Protector.NewSection). What the old
// machine's mode left on the host stays recorded until the old machine is
// gone (leftBehind): .git's locks, or the clone's remote.
func switchedTo(st *state.VM, clone string, git gitprotect.InVM) error {
	switch clone {
	case CloneOn:
		return state.SetSection(st, cloneSection, cloneInVM{On: true})
	case CloneOff:
		if err := gitprotect.Set(st, git); err != nil {
			return err
		}
		return edit(st, cloneSection, func(c *cloneInVM) error { c.On = false; return nil })
	}
	return nil
}

// switchedBack sets st's sections back to its old machine's clone mode,
// once a remake to the other (clone) is undone (putBack): the folder
// mounted, or its own clone, as it was, with no .git to protect.
func switchedBack(st *state.VM, clone string) error {
	switch clone {
	case CloneOn:
		return state.SetSection(st, cloneSection, cloneInVM{})
	case CloneOff:
		if err := gitprotect.Set(st, gitprotect.InVM{Mode: gitprotect.Clone}); err != nil {
			return err
		}
		return edit(st, cloneSection, func(c *cloneInVM) error { c.On = true; return nil })
	}
	return nil
}

// leftBehind takes what t's VM's old machine had of its clone mode off
// the host, once a remake to the other (clone) is done and the old
// machine is gone: .git's locks, for its own clone, once what the old
// machine did to .git is named as a launch names it (Check), or, for the
// folder mounted, the repo's remote for the clone, its fetched work kept
// (removeRemote). Run again, it finds nothing left.
func (e remaking) leftBehind(ctx context.Context, t *target, clone string) error {
	st := t.st
	switch clone {
	case CloneOn:
		if m := gitprotect.Mode(st); m == gitprotect.On || m == gitprotect.Strict {
			p := e.Protector(ctx, t.f.Dir)
			if err := p.Check(ctx, st, false); err != nil {
				return err
			}
			if err := p.Set(ctx, st, gitprotect.Off); err != nil {
				return err
			}
		}
		return st.Update(ctx, func(st *state.VM) error { return gitprotect.Set(st, gitprotect.InVM{Mode: gitprotect.Clone}) })
	case CloneOff:
		// Its remote went before the new machine started (mountFolder).
		return update(ctx, st, cloneSection, func(c *cloneInVM) error { *c = cloneInVM{}; return nil })
	}
	return nil
}

// mountFolder readies the folder for t's VM's new machine, made by devvm
// clone off and not yet started: the repo's remote for the old clone goes
// first, its fetched work kept (removeRemote), as .git/config is about to
// be locked; then .git is protected as a new VM's is (NewVM). The clone
// section keeps the remote's name, for putBack (unmountFolder).
func (e remaking) mountFolder(ctx context.Context, t *target) error {
	e.clones.removeRemote(ctx, t.f.Dir, t.f.Name, sectionOf[cloneInVM](t.st, cloneSection).Made.Remote)
	return e.Protector(ctx, t.f.Dir).NewVM(ctx, t.st)
}

// unmountFolder undoes mountFolder, once putBack has deleted the new
// machine that mounted the folder: .git's locks come off, once what that
// machine did to .git is named as a launch names it (Check), as a clone
// VM's .git is never checked, and the repo's remote for the clone, which
// the old machine still has, is added again when it's gone.
func (e remaking) unmountFolder(ctx context.Context, t *target) {
	dir, vm := t.f.Dir, t.f.Name
	if m := gitprotect.Mode(t.st); m == gitprotect.On || m == gitprotect.Strict {
		p := e.Protector(ctx, dir)
		// An undo goes on whatever Check finds: it has named it.
		_ = p.Check(ctx, t.st, false)
		p.Unlock(ctx, t.st)
	}
	remote := sectionOf[cloneInVM](t.st, cloneSection).Made.Remote
	if remote == "" {
		return
	}
	if _, ok := remoteURL(ctx, dir, remote); ok {
		return
	}
	if _, err := hostGit(ctx, dir, "remote", "add", remote, cloneURL(vm, dir)); err != nil {
		e.Warn(msg.RemoteAddFailed(remote))
	}
}
