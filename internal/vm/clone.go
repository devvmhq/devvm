package vm

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"errors"
	"os"
	"os/exec"
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
// The host copies its branches into the VM once (the seed, cloneseed.go),
// and the repo gets a git
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
// repo of its own (cloneSource) with a commit (checkRepoTop). A detached
// HEAD is copied as it is.
func checkCloneRepo(ctx context.Context, dir string) error {
	if err := needGit(); err != nil {
		return err
	}
	return checkRepoTop(ctx, dir)
}

// needGit refuses --clone on a machine without git.
func needGit() error {
	if _, err := exec.LookPath("git"); err != nil {
		return msg.CloneNeedsGit()
	}
	return nil
}

// checkRepoTop refuses a folder that isn't the top of a git repo of its
// own (cloneSource) that the seed can copy (checkShape) with HEAD at a
// commit, which the seed checks out, on a branch or detached: what
// --clone checks of every repo it copies, a workspace's too.
func checkRepoTop(ctx context.Context, dir string) error { return checkRepoTopOf(ctx, dir, false) }

// checkRepoTopOf is checkRepoTop, of a submodule of a --clone repo when
// sub (repoSource).
func checkRepoTopOf(ctx context.Context, dir string, sub bool) error {
	if top, err := hostGit(ctx, dir, "rev-parse", "--show-toplevel"); err != nil || top != dir {
		return msg.CloneNotTop(dir)
	}
	if _, err := repoSource(ctx, dir, sub); err != nil {
		return err
	}
	if err := checkShape(ctx, dir); err != nil {
		return err
	}
	if _, err := hostGit(ctx, dir, "rev-parse", "-q", "--verify", "HEAD^{commit}"); err != nil {
		return msg.CloneNoCommits()
	}
	return nil
}

// checkShape refuses a repo, the top of one of its own at dir, that the
// seed can't copy: one with a grafts file, which gives its commits other
// parents than their own.
func checkShape(_ context.Context, dir string) error {
	_, common, err := gitprotect.OwnRepo(dir)
	if err != nil {
		return err
	}
	if grafts := filepath.Join(common, "info", "grafts"); exists(grafts) {
		return msg.CloneGrafts(grafts)
	}
	return nil
}

// exists reports whether there's anything at p, a link included.
func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// partialClone reports whether the repo at dir is a partial clone: its
// config names a promisor remote, as git 2.54's partial clone does
// (remote.NAME.promisor), or as older ones did (extensions.partialClone)
// (hostPromisors).
func partialClone(ctx context.Context, dir string) bool {
	return len(hostPromisors(ctx, dir)) > 0
}

// cloneSource checks that what git reads in dir, a repo's top folder, is
// that repo's own (gitprotect.OwnRepo), and that git run there reads the
// same git dir and common dir. It reports whether dir is a linked
// worktree of a repo elsewhere. A submodule's folder, whose .git file
// leads into its parent's .git/modules, isn't a repo of its own here
// (clone_git_elsewhere): a VM that mounts the parent's folder, or the
// main repo's of a linked worktree parent, could swap that git dir under
// host git, which mountingVM, looking at dir and a worktree's common dir,
// doesn't see. Only as a submodule of a --clone repo is it copied
// (repoSource with sub).
func cloneSource(ctx context.Context, dir string) (worktree bool, err error) {
	return repoSource(ctx, dir, false)
}

// repoSource is cloneSource, taking a submodule's folder when sub.
func repoSource(ctx context.Context, dir string, sub bool) (worktree bool, err error) {
	gitDir, common, err := gitprotect.OwnRepo(dir)
	if err != nil {
		return false, err
	}
	if fi, lerr := os.Lstat(filepath.Join(dir, ".git")); !sub && gitDir == common && (lerr != nil || !fi.IsDir()) {
		return false, msg.CloneGitElsewhere(filepath.Join(dir, ".git"))
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

// gitSSH is host git (hostgit) run in dir, the repo of t's clone at that
// path (t's project folder, or a workspace clone's repo), with its ssh
// (gitSSHCommand) reaching t's running VM alone (target.ownSSH), and the
// repo at the same path there.
func (e clones) gitSSH(ctx context.Context, t *target, dir string, args ...string) (*exec.Cmd, error) {
	s, err := t.ownSSH(ctx)
	if err != nil {
		return nil, err
	}
	cmd := hostgit.Command(ctx, append([]string{"-C", dir}, args...)...)
	cmd.Env = append(cmd.Env, "GIT_SSH_COMMAND="+gitSSHCommand(s, dir))
	return cmd, nil
}

// gitSSHCommand is git's ssh for a --clone VM's fetch: a shell
// function, as git runs GIT_SSH_COMMAND through sh -c with its own
// arguments after it. It ignores the host, port and options git passes,
// so no url.*.insteadOf or remote URL in the repo's config can send it
// anywhere else, and connects to the VM alone (vmssh.SSH's CommandArgs:
// its loopback port and key, the host keys devvm pinned for its start
// under HostKeyAlias, BatchMode, nothing forwarded). There it runs
// git-receive-pack or git-upload-pack, whichever git's last argument
// asks for, on the clone at dir, whatever path git asked for; anything
// else fails. Its upload-pack serves any commit asked for by name
// (uploadPack), so a stash entry, which no ref names, can be fetched.
func gitSSHCommand(s vmssh.SSH, dir string) string {
	run := func(verb string) string {
		return "exec " + shell.QuoteJoin(append([]string{"ssh"}, s.CommandArgs(verb+" "+shell.Quote(dir))...))
	}
	return `f() { for c; do :; done; case $c in "git-receive-pack "*) ` + run("git-receive-pack") +
		` ;; "git-upload-pack "*) ` + run(uploadPack) + ` ;; esac; exit 1; }; f`
}

// seedClone makes the running VM's clone: a repo at the folder's path
// (initRepos) with the host's refs and uncommitted changes copied into it
// (seed), checked out as the host's, and a remote for it in the host's
// repo. Then it offers the ~/.ssh/config line that remote needs. Its
// commands in the VM carry the VM's
// agent (target.exec). A clone an earlier launch copied already (an
// earlier devvm's push too) gets only its remote.
func (e seeder) seedClone(ctx context.Context, t *target) error {
	vm, st, f, dir := t.f.Name, t.st, t.f, t.f.Dir
	// Checked again, as the folder may have changed since the VM was made.
	if _, err := cloneSource(ctx, dir); err != nil {
		return err
	}
	if err := checkShape(ctx, dir); err != nil {
		return err
	}
	if other := e.mountingVM(dir, vm); other != "" {
		return msg.CloneFolderMounted(dir, other)
	}
	remote, err := freeRemote(ctx, f, dir)
	if err != nil {
		return err
	}
	s, err := planSeed(ctx, ".", dir)
	if err != nil {
		return msg.CloneSeedFailed(vm).Because(err)
	}
	branch := strings.TrimPrefix(s.head, "refs/heads/")
	// Recorded before the copy, as a workspace repo's is (seedRepo): rm
	// checks the clone, and removes the remote once it's the VM's.
	if err := update(ctx, st, cloneSection, func(c *cloneInVM) error { c.Made.Remote = remote; return nil }); err != nil {
		return err
	}
	seeds := []repoSeed{s}
	if err := e.initRepos(ctx, t, seeds, nil); err != nil {
		return err
	}
	if !seeds[0].landed {
		sayHow(e.Info, s, false)
		e.Info(msg.CopyingBranches())
		if err := e.seed(ctx, t, s, msg.CopyingBranches()); err != nil {
			return err
		}
	}
	if _, ok := remoteURL(ctx, dir, remote); !ok {
		if _, err := hostGit(ctx, dir, "remote", "add", remote, cloneURL(vm, dir)); err != nil {
			return msg.RemoteAddFailed(remote)
		}
	}
	// Before the clone is recorded as made, so a try again copies those
	// it didn't.
	if err := e.seedSubmodules(ctx, t, "."); err != nil {
		return err
	}
	if err := update(ctx, st, cloneSection, func(c *cloneInVM) error { c.Made.Seeded = true; return nil }); err != nil {
		return err
	}
	if ok, err := e.sshConfig().EnsureInclude(e.Dirs.Home); err != nil {
		return err
	} else if !ok {
		e.Warn(msg.FetchUntil(e.Dirs.SSHConfig(), remote))
	}
	if s.head == "" {
		e.Info(msg.CloneReadyDetached(remote))
		return nil
	}
	e.Info(msg.CloneReady(branch, remote))
	return nil
}

// freeRemote is the name of the remote for the folder's VM of the repo
// at dir, the folder's own or a workspace clone's: devvm, else
// devvm-<folder>, else the VM's name, whichever is free or already the
// VM's, each repo's chosen on its own.
func freeRemote(ctx context.Context, f project.Folder, dir string) (string, error) {
	url := cloneURL(f.Name, dir)
	for _, r := range []string{"devvm", vmname.Prefix + f.Slug(), f.Name} {
		if got, ok := remoteURL(ctx, dir, r); !ok || got == url {
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

// uploadPack is git upload-pack as a --clone VM's fetch runs it in the
// VM: allowing any commit asked for by name, which only the VM's own
// repo serves, so the host can fetch a stash entry by its commit, which
// no ref names. The fetch speaks git's protocol v0, as devvm's ssh
// command passes no GIT_PROTOCOL to the VM, and v0 refuses a commit no
// ref names without this.
const uploadPack = "git -c uploadpack.allowAnySHA1InWant=true upload-pack"

// fetchRef is what the VM has that the host lacks, as unfetchedRefs
// gives it: its name, shown and saved by (a branch's, HEAD, or stash-N
// for a stash entry), what git fetches it from (src: the branch's full
// name, HEAD, or a stash entry's commit), and whether it's a stash entry
// (saveStash).
type fetchRef struct {
	name, src string
	stash     bool
}

// unfetchedRefs returns, from the VM's lines ("SHA refs/heads/BRANCH",
// "SHA HEAD" for a detached HEAD, "SHA stash@{N}" for a stash entry), the
// branches with commits no ref in dir's repo has, as the VM named them,
// HEAD for a detached HEAD's, and stash-N for a stash entry: untrusted,
// so checked (validBranch) before use and quoted when shown. A stash
// entry is fetched by its commit, so one whose name or commit isn't
// git's is left out, and so is one the repo has as an object already: a
// stash entry is saved without its untracked files (saveStash), so no
// ref reaches the entry itself.
func unfetchedRefs(ctx context.Context, dir string, refs []byte) []fetchRef {
	var out []fetchRef
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
		if n, ok := stashIndex(ref); ok {
			if objectName.MatchString(sha) && !hasCommit(ctx, dir, sha) {
				out = append(out, fetchRef{name: "stash-" + n, src: sha, stash: true})
			}
			continue
		}
		if ref == "HEAD" {
			out = append(out, fetchRef{name: "HEAD", src: "HEAD"})
			continue
		}
		name := strings.TrimPrefix(ref, "refs/heads/")
		out = append(out, fetchRef{name: name, src: "refs/heads/" + name})
	}
	return out
}

// stashIndex is N of a stash entry's name, stash@{N}.
func stashIndex(ref string) (string, bool) {
	n, ok := strings.CutPrefix(ref, "stash@{")
	n, ok2 := strings.CutSuffix(n, "}")
	digits := n != "" && len(n) <= 9 && strings.Trim(n, "0123456789") == ""
	return n, ok && ok2 && digits
}

// names are refs' names.
func names(refs []fetchRef) []string {
	out := make([]string, len(refs))
	for i, r := range refs {
		out[i] = r.name
	}
	return out
}

// fetchArgs is git fetch of refspecs from the clone of the repo at dir in
// vm: no tags, no submodules, and --refmap= , so only the refspecs given
// update refs, never a remote's of the same URL, whose own could update
// other branches, and every object checked as it comes
// (transfer.fsckObjects). Every fetch from a clone VM is one.
func fetchArgs(vm, dir string, refspecs ...string) []string {
	return append([]string{"-c", "transfer.fsckObjects=true", "fetch", "--no-tags", "--no-recurse-submodules", "--refmap=", cloneURL(vm, dir)}, refspecs...)
}

// fetchInto runs git fetch (fetchArgs) of refspecs into the repo at dir
// from its clone in t's VM, as step, failing with fail; then brings the
// Git LFS objects what it fetched names, which git's fetch doesn't
// (fetchLFS). strict, as for a VM about to go, fails with fail too when
// an LFS object the VM's clone may have didn't come.
func (e clones) fetchInto(ctx context.Context, t *target, dir string, strict bool, step, fail msg.M, refspecs ...string) error {
	cmd, err := e.gitSSH(ctx, t, dir, fetchArgs(t.f.Name, dir, refspecs...)...)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fail.Because(err)
	}
	if err := e.LoggedCmd(ctx, step, cmd); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// The VM's upload-pack may have needed an object its partial
		// clone lacks, which it never fetches to send it.
		if partialClone(ctx, dir) {
			rel, err := filepath.Rel(t.f.Dir, dir)
			if err != nil {
				rel = dir
			}
			e.Warn(msg.CloneFetchPartial(rel))
		}
		return backend.Logged(fail)
	}
	lost, err := e.fetchLFS(ctx, t, dir, fetchHead(ctx, dir))
	switch {
	case err != nil:
		return err
	case lost && strict:
		return fail
	}
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

// removeRemotes removes each repo's remote for the deleted VM, whose
// clone section is c and project folder ws (removeRemote), its recorded
// submodules' too (allRepos).
func (e clones) removeRemotes(ctx context.Context, ws, vm string, c cloneInVM) {
	for _, r := range c.allRepos() {
		e.removeRemote(ctx, repoDir(ws, r.Path), vm, r.Remote)
	}
}

// repoDir is the folder of a clone's repo at path, relative to the
// project folder ws ("." for its own), on the host, and at the same path
// in the VM.
func repoDir(ws, path string) string { return filepath.Join(ws, path) }

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
	if err := e.canSwitch(ctx, t, &p, o); err != nil {
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
// repo it can clone, nor a folder of repos (planClone), whose repos it
// then chooses, as --repos says or the user picks, into p. Then it says
// what changes, and for the folder mounted with --no-clone-check, what's
// lost.
func (e Lifecycle) canSwitch(ctx context.Context, t *target, p *remakeInVM, o RemakeOptions) error {
	vm := t.f.Name
	if p.Clone == CloneOn {
		to, err := e.clones.planClone(ctx, t.f.Dir, o.Repos, t.f.Name)
		if err != nil {
			return err
		}
		if to.Workspace {
			p.CloneTo = &to
		}
	}
	if err := e.remaking.check(t, *p); err != nil {
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
// .git protection (gitprotect.Protector.NewSection). Its own clone is of
// the repos to says, a workspace clone's (nil: the folder's own repo).
// What the old machine's mode left on the host stays recorded until the
// old machine is gone (leftBehind): .git's locks, or the clone's remotes.
// The folder mounted gets cache, its mount's cache mode
// (remaking.switchCache), "" for none to choose.
func switchedTo(st *state.VM, clone string, to *cloneInVM, git gitprotect.InVM, cache string) error {
	switch clone {
	case CloneOn:
		c := cloneInVM{On: true}
		if to != nil {
			c = cloneInVM{On: true, Workspace: to.Workspace, Repos: to.Repos}
		}
		return state.SetSection(st, cloneSection, c)
	case CloneOff:
		if err := gitprotect.Set(st, git); err != nil {
			return err
		}
		// The mount is new: a stored mmap, none chosen (cacheOf), gets cache too.
		if err := edit(st, mountSection, func(m *mountInVM) error { m.Cache = cmp.Or(cacheOf(st), cache); return nil }); err != nil {
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
		if gitprotect.Mode(st) == gitprotect.On {
			p := e.Protector(t.f.Dir)
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
	e.clones.removeRemotes(ctx, t.f.Dir, t.f.Name, sectionOf[cloneInVM](t.st, cloneSection))
	p := e.Protector(t.f.Dir)
	found := e.starter.walk(t.f.Dir, false)
	p.Nested = &found.Repos
	return p.NewVM(ctx, t.st)
}

// unmountFolder undoes mountFolder, once putBack has deleted the new
// machine that mounted the folder: .git's locks come off, once what that
// machine did to .git is named as a launch names it (Check), as a clone
// VM's .git is never checked, and the repo's remote for the clone, which
// the old machine still has, is added again when it's gone.
func (e remaking) unmountFolder(ctx context.Context, t *target) {
	dir, vm := t.f.Dir, t.f.Name
	if gitprotect.Mode(t.st) == gitprotect.On {
		p := e.Protector(dir)
		// An undo goes on whatever Check finds: it has named it.
		_ = p.Check(ctx, t.st, false)
		p.Unlock(ctx, t.st)
	}
	for _, r := range sectionOf[cloneInVM](t.st, cloneSection).repos() {
		at := repoDir(dir, r.Path)
		if r.Remote == "" {
			continue
		}
		if _, ok := remoteURL(ctx, at, r.Remote); ok {
			continue
		}
		if err := e.clones.stillRepo(ctx, t, r.Path, r.submodule); err != nil {
			e.Warn(msg.CloneRepoChanged(r.Path).Because(err))
			continue
		}
		if _, err := hostGit(ctx, at, "remote", "add", r.Remote, cloneURL(vm, at)); err != nil {
			e.Warn(msg.RemoteAddFailed(r.Remote))
		}
	}
}
