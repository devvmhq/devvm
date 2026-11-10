package vm

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/gitprotect"
	"github.com/devvmhq/devvm/internal/hostgit"
	"github.com/devvmhq/devvm/internal/hostsafety"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/safefile"
	"github.com/devvmhq/devvm/internal/state"
)

// A workspace clone: devvm --clone in a folder of repos gives the VM a
// clone of each repo the user chooses, each at its own path, so paths
// between them (file:../lib, ../Lib/Lib.csproj) work as on the host, and
// a copy of the folder's files outside them, made once. Each repo gets a
// remote of its own for the VM, as one repo's clone does.

// The values of --repos besides a list of paths: every repo --clone can
// copy, or none of them.
const (
	ReposAll  = "all"
	ReposNone = "none"
)

// wsRepo is a repo found in a folder of repos (findRepos): its folder,
// relative to the project folder ("." for its own), and why --clone
// can't copy it, the zero message when it can.
type wsRepo struct {
	Path   string
	Reason msg.M
}

// ok reports whether --clone can copy the repo.
func (r wsRepo) ok() bool { return r.Reason.ID() == "" }

// findRepos finds the repos of the folder dir for a workspace clone of
// self, the VM: its own (".") when it has a .git, then those the walk
// finds below it (starter.walk: the walk .git protection makes, so the
// same depth, skipped folders and bound), each checked as --clone checks
// a repo (repoReason), and refused when another VM mounts it, or its
// common dir (mountingVM), or its path holds a character a terminal can't
// show, such as a line break (plainPath). A linked worktree of a repo in the folder isn't a repo of
// its own: its repo's clone gets that repo's current and default
// branches, not the worktree's own branch, which the user pushes with
// git push devvm BRANCH. more is how many more the walk found past its
// bound.
func (e clones) findRepos(ctx context.Context, dir, self string) (repos []wsRepo, more int) {
	found := e.starter.walk(dir, false).Repos
	var rels []string
	if hasGit(dir) {
		rels = append(rels, ".")
	}
	for _, rel := range append(rels, found.Dirs...) {
		at := repoDir(dir, rel)
		if worktreeIn(at, dir) {
			continue
		}
		r := wsRepo{Path: rel, Reason: repoReason(ctx, at)}
		if !plainPath(rel) {
			r.Reason = msg.WorkspaceReasonName()
		} else if vm := e.mountingVM(at, self); r.ok() && vm != "" {
			r.Reason = msg.WorkspaceReasonMounted(vm)
		}
		repos = append(repos, r)
	}
	return repos, found.More
}

// worktreeIn reports whether the repo at at is a linked worktree of a repo
// in the folder dir, its common dir there (gitprotect.OwnRepo's).
func worktreeIn(at, dir string) bool {
	gitDir, common, err := gitprotect.OwnRepo(at)
	return err == nil && gitDir != common && paths.Within(filepath.Dir(common), dir)
}

// repoReason is why --clone can't copy the repo at dir, as the picker
// says it: not a repo's top folder, a .git that reads another repo's
// objects or refs (cloneSource), no commits, or no branch to check out
// (branchPlan); the zero message when it can.
func repoReason(ctx context.Context, dir string) msg.M {
	err := checkRepoTop(ctx, dir)
	m, _ := errors.AsType[msg.M](err)
	switch {
	case err == nil:
		if _, refs := branchPlan(currentBranch(ctx, dir)); len(refs) == 0 {
			return msg.WorkspaceReasonNoBranch()
		}
		return msg.M{}
	case m.ID() == msg.CloneNoCommits().ID():
		return msg.WorkspaceReasonNoCommits()
	case m.ID() == msg.CloneNotTop("").ID():
		return msg.WorkspaceReasonNotRepo()
	}
	return msg.WorkspaceReasonElsewhere()
}

// isWorkspace reports whether repos make a folder a workspace clone's: a
// repo below it that --clone can copy. Without one, --clone copies the
// folder's own repo, as for a folder that holds no other.
func isWorkspace(repos []wsRepo) bool {
	return slices.ContainsFunc(repos, func(r wsRepo) bool { return r.Path != "." && r.ok() })
}

// planClone is the clone section of a --clone VM, self, for the folder
// dir, as a new VM (creation.plan) or devvm clone on (canSwitch) makes it:
// in a folder of repos (isWorkspace), a workspace clone of the repos
// repos chooses (--repos: chooseRepos), else, once the folder is a repo
// --clone takes (checkCloneRepo), a clone of the folder's own. Only all
// is a value of --repos for that one. A folder that's a repo itself gets
// a clone of its own repo unless --repos is given, as devvm 0.1.0 made
// it, whatever repos are below it; one that isn't, but is in a repo's
// working tree, is refused (workspace_inside_repo), as the copy of its
// files would take that repo's.
func (e clones) planClone(ctx context.Context, dir, repos, self string) (cloneInVM, error) {
	if err := needGit(); err != nil {
		return cloneInVM{}, err
	}
	if hasGit(dir) && repos == "" {
		return e.ownClone(ctx, dir, self)
	}
	found, more := e.findRepos(ctx, dir, self)
	if !isWorkspace(found) {
		if repos != "" && repos != ReposAll {
			return cloneInVM{}, msg.ReposNotWorkspace(dir)
		}
		return e.ownClone(ctx, dir, self)
	}
	if top, err := hostGit(ctx, dir, "rev-parse", "--show-toplevel"); !hasGit(dir) && err == nil && top != dir {
		return cloneInVM{}, msg.WorkspaceInsideRepo(dir, top)
	}
	if more > 0 {
		e.Warn(msg.WorkspaceReposPast(more, strconv.Itoa(hostsafety.MaxRepos)))
	}
	chosen, err := e.chooseRepos(dir, found, repos)
	if err != nil {
		return cloneInVM{}, err
	}
	c := cloneInVM{On: true, Workspace: true}
	for _, p := range chosen {
		c.Repos = append(c.Repos, cloneRepo{Path: p})
	}
	return c, nil
}

// ownClone is the clone section of self, a --clone VM of the folder dir's
// own repo, once --clone takes the repo (checkCloneRepo) and no other VM
// mounts it (mountingVM: clone_folder_mounted), as its seed's push runs
// host git there.
func (e clones) ownClone(ctx context.Context, dir, self string) (cloneInVM, error) {
	if err := checkCloneRepo(ctx, dir); err != nil {
		return cloneInVM{}, err
	}
	if vm := e.mountingVM(dir, self); vm != "" {
		return cloneInVM{}, msg.CloneFolderMounted(dir, vm)
	}
	return cloneInVM{On: true}, nil
}

// chooseRepos returns the paths of the repos of found that a workspace
// clone of dir gets, in found's order: as flag (--repos) says, all, none
// or the paths it lists (pickedRepos), else in the picker, every repo
// --clone can copy ticked first and the others shown as they are, with
// why. Where the picker can't show, the question is refused, naming
// --repos. The repos asked for that --clone can't copy are left out and
// named.
func (e clones) chooseRepos(dir string, found []wsRepo, flag string) ([]string, error) {
	var chosen, cannot []string
	switch flag {
	case "":
		return e.pickRepos(dir, found)
	case ReposNone:
		return nil, nil
	case ReposAll:
		for _, r := range found {
			if r.ok() {
				chosen = append(chosen, r.Path)
			} else {
				cannot = append(cannot, r.Path)
			}
		}
	default:
		var err error
		if chosen, cannot, err = pickedRepos(dir, found, flag); err != nil {
			return nil, err
		}
	}
	if len(cannot) > 0 {
		e.Warn(msg.WorkspaceSkipped(quoted(cannot)))
	}
	return chosen, nil
}

// pickedRepos are the repos of found that list, --repos's comma-separated
// paths relative to dir, names, in found's order: those --clone can copy
// (chosen), and those it can't (cannot). A path that's none of found's
// is refused.
func pickedRepos(dir string, found []wsRepo, list string) (chosen, cannot []string, err error) {
	asked := map[string]bool{}
	for p := range strings.SplitSeq(list, ",") {
		p = path.Clean(strings.TrimSpace(p))
		if !slices.ContainsFunc(found, func(r wsRepo) bool { return r.Path == p }) {
			return nil, nil, msg.WorkspaceRepoUnknown(dir, p)
		}
		asked[p] = true
	}
	for _, r := range found {
		switch {
		case !asked[r.Path]:
		case r.ok():
			chosen = append(chosen, r.Path)
		default:
			cannot = append(cannot, r.Path)
		}
	}
	return chosen, cannot, nil
}

// pickRepos shows the picker of a workspace clone's repos: those --clone
// can copy, each ticked, then the others under a heading of their own,
// each with why, which can't be ticked. Where it can't show, the
// question is refused, naming --repos.
func (e clones) pickRepos(dir string, found []wsRepo) ([]string, error) {
	q := msg.WorkspacePickHelp(dir).AnsweredBy("--repos")
	if !e.CanPick() || e.PickMany == nil {
		return nil, q.Refused(false)
	}
	parts := []PickPart{{}, {Heading: msg.WorkspacePickCannot()}}
	for _, r := range found {
		if r.ok() {
			parts[0].Rows = append(parts[0].Rows, PickRow{Name: r.Path, On: true})
		} else {
			parts[1].Rows = append(parts[1].Rows, PickRow{Name: r.Path, Desc: r.Reason, Fixed: true})
		}
	}
	parts = slices.DeleteFunc(parts, func(p PickPart) bool { return len(p.Rows) == 0 })
	picked, err := e.PickMany(q.M(), parts)
	if err != nil {
		return nil, err
	}
	var chosen []string
	for _, r := range picked[0].Rows {
		if r.On && !r.Fixed {
			chosen = append(chosen, r.Name)
		}
	}
	return chosen, nil
}

// currentBranch is the branch checked out in the repo at dir, and its
// default branch's name and the ref it's pushed from (defaultBranch);
// cur is "" for a detached HEAD.
func currentBranch(ctx context.Context, dir string) (cur, def, from string) {
	if ref, err := hostGit(ctx, dir, "symbolic-ref", "-q", "HEAD"); err == nil {
		cur, _ = strings.CutPrefix(ref, "refs/heads/")
	}
	head, _ := hostGit(ctx, dir, "symbolic-ref", "-q", "refs/remotes/origin/HEAD")
	list, _ := hostGit(ctx, dir, "for-each-ref", "--format=%(refname)", "refs/heads", "refs/remotes/origin")
	def, from = defaultBranch(head, strings.Fields(list))
	if def != "" && !validBranch(ctx, dir, def) {
		def, from = "", ""
	}
	return cur, def, from
}

// defaultBranch is a repo's default branch, from origin/HEAD (originHead,
// the ref it points to: "" for none) and the repo's branches and origin's
// remote-tracking branches (refs): origin/HEAD's branch, else main, else
// master, the first of them it has, as a local branch, else as origin's,
// with the ref it's pushed from. "" for none.
func defaultBranch(originHead string, refs []string) (name, from string) {
	var names []string
	if n, ok := strings.CutPrefix(originHead, "refs/remotes/origin/"); ok && n != "HEAD" {
		names = append(names, n)
	}
	for _, n := range append(names, "main", "master") {
		for _, ref := range []string{"refs/heads/" + n, "refs/remotes/origin/" + n} {
			if slices.Contains(refs, ref) {
				return n, ref
			}
		}
	}
	return "", ""
}

// branchPlan is what a workspace clone's seed copies of a repo: the
// branch its clone checks out, the current one (cur), else, for a
// detached HEAD, its default branch (def, pushed from the ref from), and
// the refspecs its push sends, the current branch and the default one.
// No refspecs: neither, so nothing to copy.
func branchPlan(cur, def, from string) (checkout string, refs []string) {
	checkout = cur
	if cur != "" {
		refs = append(refs, "refs/heads/"+cur+":refs/heads/"+cur)
	} else {
		checkout = def
	}
	if def != "" && def != cur {
		refs = append(refs, from+":refs/heads/"+def)
	}
	return checkout, refs
}

// repoSeed is how a workspace clone's seed copies one repo into the VM:
// its path, relative to the project folder, and folder, the branch its
// clone checks out and the refspecs its push sends (branchPlan), and
// whether its HEAD was detached.
type repoSeed struct {
	path, dir, checkout string
	refs                []string
	detached            bool
	// landed: the VM has the repo with a commit already, an earlier
	// copy's push (initRepos), so only its remote is left to add.
	landed bool
}

// prepareSeed checks the repo at path in t's folder as --clone checks a
// repo it copies (stillRepo), again, as the folder may have changed since
// the VM was made, and returns how it's copied: a repo with no branch to
// copy, or whose remote names are all taken, is refused.
func (e clones) prepareSeed(ctx context.Context, t *target, rel string) (repoSeed, error) {
	dir := repoDir(t.f.Dir, rel)
	if err := e.stillRepo(ctx, t, rel); err != nil {
		return repoSeed{}, err
	}
	cur, def, from := currentBranch(ctx, dir)
	checkout, refs := branchPlan(cur, def, from)
	if len(refs) == 0 {
		return repoSeed{}, msg.CloneDetached()
	}
	if _, err := freeRemote(ctx, t.f, dir); err != nil {
		return repoSeed{}, err
	}
	return repoSeed{path: rel, dir: dir, checkout: checkout, refs: refs, detached: cur == ""}, nil
}

// stillRepo checks, before host git reads or writes the repo at rel in
// t's workspace clone's folder, that it's still the top of a repo of its
// own (checkRepoTop: its error), and that no other VM mounts it, which
// could swap it (finder.mountsRepo: clone_repo_mounted).
func (e clones) stillRepo(ctx context.Context, t *target, rel string) error {
	dir := repoDir(t.f.Dir, rel)
	if err := checkRepoTop(ctx, dir); err != nil {
		return err
	}
	if vm := e.mountingVM(dir, t.f.Name); vm != "" {
		return msg.CloneRepoMounted(rel, vm)
	}
	return nil
}

// mountingVM is the VM other than self that mounts the repo at dir (its
// own, or a parent folder's: finder.mountsRepo, unlike finder.mountedBy,
// a launch's warning, which looks at the parents alone), or,
// for a linked worktree, the repo its common dir is in, whose objects and
// refs its git reads and writes (finder.mountsRepo); "" for none.
func (e clones) mountingVM(dir, self string) string {
	if vm := e.finder.mountsRepo(dir, self); vm != "" {
		return vm
	}
	if gitDir, common, err := gitprotect.OwnRepo(dir); err == nil && gitDir != common {
		return e.finder.mountsRepo(filepath.Dir(common), self)
	}
	return ""
}

// initRepos is the script that makes a workspace clone's repos in the VM,
// all in one command: the project folder ($1), the user's; whether ($2)
// the folder's own repo's info/exclude gets what's on stdin; then for
// each repo its path relative to it and the branch it checks out, a new
// repo there (the folders between the user's too) that takes pushes to
// its checked-out branch, a shallow repo's included (receive.shallowUpdate).
// A repo there already with a commit, an earlier copy's push landed, is
// left as it is (its HEAD may be the VM's work now) and its path printed,
// a line each.
const initRepos = `ws=$1; ex=$2; shift 2
sudo mkdir -p -- "$ws" && sudo chown -- "$(id -u):$(id -g)" "$ws" || exit 1
while [ "$#" -ge 2 ]; do
  d=$ws/$1
  if [ -e "$d/.git" ] && git -C "$d" rev-parse -q --verify HEAD >/dev/null 2>&1; then
    printf '%s\n' "$1"; shift 2; continue
  fi
  mkdir -p -- "$d" && git -C "$d" init -q && git -C "$d" config receive.denyCurrentBranch updateInstead &&
    git -C "$d" config receive.shallowUpdate true && git -C "$d" symbolic-ref HEAD "refs/heads/$2" || exit 1
  shift 2
done
if [ "$ex" = exclude ]; then
  mkdir -p -- "$ws/.git/info" && cat >> "$ws/.git/info/exclude" || exit 1
fi`

// initRepos makes the project folder and the repos of seeds, if any, in
// t's running VM (initRepos), its command carrying the VM's agent
// (target.exec), and, when the clone has the folder's own repo, adds
// exclude (repoExcludes) to that repo's info/exclude in the VM. Each
// seed the VM has with a commit already is marked landed, so its push
// isn't sent again (seedRepo); the VM's answer names only seeds' paths.
func (e clones) initRepos(ctx context.Context, t *target, seeds []repoSeed, exclude []byte) error {
	ex := "-"
	if exclude != nil {
		ex = "exclude"
	}
	argv := []string{"sh", "-c", initRepos, "sh", t.f.Dir, ex}
	for _, s := range seeds {
		argv = append(argv, s.path, s.checkout)
	}
	log := e.vmLog()
	out := &bounded{max: cloneOutMax}
	if err := t.exec(ctx, argv, backend.ExecOpts{Stdin: bytes.NewReader(exclude), Stdout: out, Stderr: log}); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return backend.Logged(msg.CloneSeedFailed(t.f.Name))
	}
	had := strings.Split(out.String(), "\n")
	for i := range seeds {
		seeds[i].landed = slices.Contains(had, seeds[i].path)
	}
	return nil
}

// excludeMax is the most of the host repo's info/exclude repoExcludes
// takes.
const excludeMax = 64 << 10

// repoExcludes is what the VM's clone of the folder's own repo, when the
// workspace clone c has it, gets in its info/exclude, so its git status
// leaves out what's the host's to leave out: each of the clone's other
// repos, of paths (all of them, or those added), which are its own
// clones, then, withHost, the host repo's own info/exclude, such as a
// .worktrees/ line. nil when c hasn't the folder's own repo.
func repoExcludes(ws string, c cloneInVM, rels []string, withHost bool) []byte {
	if !slices.ContainsFunc(c.repos(), func(r cloneRepo) bool { return r.Path == "." }) {
		return nil
	}
	var b bytes.Buffer
	for _, p := range rels {
		if p != "." && plainPath(p) {
			b.WriteString(excludeLine(p))
		}
	}
	if withHost {
		if host, err := safefile.Read(filepath.Join(ws, ".git", "info", "exclude"), excludeMax, safefile.Opts{}); err == nil {
			b.Write(host)
			b.WriteString("\n")
		}
	}
	return b.Bytes()
}

// excludeLine is the info/exclude line that leaves out the folder p,
// relative to the repo's top, and nothing else: anchored with a / at
// each end, so a ! or # can't start it and no space ends it, and git's
// pattern characters in it escaped, so lib[2] is lib[2] alone.
func excludeLine(p string) string {
	var b strings.Builder
	b.WriteString("/")
	for _, r := range p {
		if strings.ContainsRune(`\*?[`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteString("/\n")
	return b.String()
}

// plainPath reports whether p, a repo's path relative to the project
// folder, has only characters a terminal shows as they are (msg.Safe):
// no control character, such as a line break, which no line of a git
// exclude, nor of a message, can hold.
func plainPath(p string) bool {
	return !strings.ContainsFunc(p, func(r rune) bool { return !msg.Safe(r) })
}

// seedRepo copies one repo into t's running VM, whose repo initRepos
// made: checked again just before (stillRepo), its remote chosen once the
// earlier repos' are added (freeRemote: two worktrees of one repo share a
// config), its branches pushed (s.refs) unless an earlier copy's push
// landed (s.landed: a push again would be refused once the VM's branch
// moved on), then its remote for the VM added, each recorded in the clone
// section as it's done, so a retry, rm and an interrupt find it. It
// returns the remote.
func (e clones) seedRepo(ctx context.Context, t *target, s repoSeed) (string, error) {
	vm, st := t.f.Name, t.st
	if err := e.stillRepo(ctx, t, s.path); err != nil {
		return "", err
	}
	remote, err := freeRemote(ctx, t.f, s.dir)
	if err != nil {
		return "", err
	}
	if err := update(ctx, st, cloneSection, func(c *cloneInVM) error {
		c.setRepo(s.path, func(r *cloneRepo) { r.Remote = remote })
		return nil
	}); err != nil {
		return "", err
	}
	if !s.landed {
		if err := e.pushSeed(ctx, t, s); err != nil {
			return "", err
		}
	}
	if _, ok := remoteURL(ctx, s.dir, remote); !ok {
		if _, err := hostGit(ctx, s.dir, "remote", "add", remote, cloneURL(vm, s.dir)); err != nil {
			return "", msg.RemoteAddFailed(remote)
		}
	}
	return remote, update(ctx, st, cloneSection, func(c *cloneInVM) error {
		c.setRepo(s.path, func(r *cloneRepo) { r.Seeded = true })
		return nil
	})
}

// pushSeed pushes s's branches (s.refs) into its repo in t's running VM,
// over devvm's own ssh to the VM alone (gitSSH).
func (e clones) pushSeed(ctx context.Context, t *target, s repoSeed) error {
	vm := t.f.Name
	push := append([]string{"push", "-q", "--no-verify", "--no-recurse-submodules", "--no-signed", cloneURL(vm, s.dir)}, s.refs...)
	cmd, err := e.gitSSH(ctx, t, s.dir, push...)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return msg.CloneSeedFailed(vm).Because(err)
	}
	if err := e.LoggedCmd(ctx, msg.CopyingRepo(s.path), cmd); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return backend.Logged(msg.CloneSeedFailed(vm))
	}
	return nil
}

// seedWorkspace makes a workspace clone in t's running VM, as seedClone
// makes one repo's: each repo not yet copied (prepareSeed: one that no
// longer passes is left out, named, and dropped from the clone section),
// made at once (initRepos), then copied one by one (seedRepo), the
// folder's files outside them copied once (copyFiles), and the clone
// recorded as made. Then it offers the ~/.ssh/config line that the
// remotes need, and names the repos with uncommitted changes, which
// aren't copied.
func (e clones) seedWorkspace(ctx context.Context, t *target) error {
	st := t.st
	c := sectionOf[cloneInVM](st, cloneSection)
	var seeds []repoSeed
	var dropped, all []string
	for _, r := range c.repos() {
		all = append(all, r.Path)
		if r.Seeded {
			continue
		}
		s, err := e.prepareSeed(ctx, t, r.Path)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			e.Warn(msg.WorkspaceRepoDropped(r.Path))
			dropped = append(dropped, r.Path)
			continue
		}
		seeds = append(seeds, s)
	}
	if err := update(ctx, st, cloneSection, func(c *cloneInVM) error {
		c.Repos = slices.DeleteFunc(c.Repos, func(r cloneRepo) bool { return slices.Contains(dropped, r.Path) })
		return nil
	}); err != nil {
		return err
	}
	c = sectionOf[cloneInVM](st, cloneSection)
	if err := e.initRepos(ctx, t, seeds, repoExcludes(t.f.Dir, c, all, true)); err != nil {
		return err
	}
	remote := ""
	for _, s := range seeds {
		if s.detached {
			e.Info(msg.WorkspaceDetached(s.checkout, s.path))
		}
		r, err := e.seedRepo(ctx, t, s)
		if err != nil {
			return err
		}
		remote = cmp.Or(remote, r)
	}
	if !c.Made.Files {
		if err := e.copyFiles(ctx, t); err != nil {
			return err
		}
	}
	if err := update(ctx, st, cloneSection, func(c *cloneInVM) error { c.Made.Files, c.Made.Seeded = true, true; return nil }); err != nil {
		return err
	}
	// git fetch and git push to a repo's remote need the Include line;
	// devvm clone fetch doesn't.
	if ok, err := e.sshConfig().EnsureInclude(e.Dirs.Home); err != nil {
		return err
	} else if !ok && remote != "" {
		e.Warn(msg.FetchUntil(e.Dirs.SSHConfig(), remote))
	}
	var dirty []string
	for _, s := range seeds {
		if out, _ := hostgit.Status(ctx, s.dir, "--porcelain", "--untracked-files=no").Output(); len(bytes.TrimSpace(out)) > 0 {
			dirty = append(dirty, s.path)
		}
	}
	if len(dirty) > 0 {
		e.Warn(msg.UncommittedNotCopiedRepos(quoted(dirty)))
	}
	e.Info(msg.WorkspaceReady(len(sectionOf[cloneInVM](st, cloneSection).Repos)))
	return nil
}

// copyFiles copies the files of t's project folder outside every repo in
// it (workspaceFiles) into its running VM, at the same paths, once, and
// says how many it left out. A folder that's a repo itself has no files
// of its own outside it: its files are its repo's, chosen or not.
func (e clones) copyFiles(ctx context.Context, t *target) error {
	if hasGit(t.f.Dir) {
		return nil
	}
	tarball, skipped, err := workspaceFiles(t.f.Dir)
	if err != nil {
		return msg.WorkspaceFilesFailed(t.f.Dir).Because(err)
	}
	if skipped > 0 {
		e.Warn(msg.WorkspaceFilesSkipped(skipped))
	}
	if tarball == nil {
		return nil
	}
	log := e.vmLog()
	argv := []string{"sh", "-c", `exec tar -x -f - --no-same-owner -C "$1"`, "sh", t.f.Dir}
	if err := t.exec(ctx, argv, backend.ExecOpts{Stdin: bytes.NewReader(tarball), Stdout: log, Stderr: log}); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return backend.Logged(msg.CloneSeedFailed(t.f.Name))
	}
	return nil
}

// Clone is devvm clone fetch and devvm clone add: the work of a --clone
// VM's repos fetched into this machine's, and a repo added to a
// workspace clone.
type Clone struct{ *Env }

// Fetched is what devvm clone fetch brought into each repo of the clone:
// the VM's branches that are new or moved in its remote's
// remote-tracking branches, by repo.
type Fetched struct {
	Repos []FetchedRepo
	// Checked is how many repos it fetched from, those with nothing new
	// too: none when it left every one alone (each named).
	Checked int
}

// FetchedRepo is one repo's fetch: its path relative to the project
// folder ("." for the folder's own), its remote, and the branches new or
// moved, as the VM named them.
type FetchedRepo struct {
	Path, Remote string
	Branches     []string
}

// FetchedBranch is a branch name and the repos it's new or moved in.
type FetchedBranch struct {
	Name  string
	Repos []string
}

// ByBranch is f by branch name, each with its repos in the clone's order,
// sorted by name: one name across repos is one piece of work.
func (f Fetched) ByBranch() []FetchedBranch {
	at := map[string]int{}
	var out []FetchedBranch
	for _, r := range f.Repos {
		for _, b := range r.Branches {
			i, ok := at[b]
			if !ok {
				i = len(out)
				at[b] = i
				out = append(out, FetchedBranch{Name: b})
			}
			out[i].Repos = append(out[i].Repos, r.Path)
		}
	}
	slices.SortFunc(out, func(a, b FetchedBranch) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// Fetch fetches the VM's branches into each repo of the folder's --clone
// VM, starting it if it's stopped: every branch into its remote's
// remote-tracking branches (fetchInto, as rm's fetch does), and returns
// those new or moved. A repo whose remote was removed gets it again; one
// whose remote points elsewhere now is left alone and named.
func (e Clone) Fetch(ctx context.Context) (Fetched, error) {
	t, err := e.finder.target(ctx, need{has: hasMade})
	if err != nil {
		return Fetched{}, err
	}
	vm := t.f.Name
	if !isClone(t.st) {
		return Fetched{}, msg.CloneFetchMounted(vm)
	}
	if err := t.ensureRunning(ctx); err != nil {
		return Fetched{}, err
	}
	var out Fetched
	for _, r := range sectionOf[cloneInVM](t.st, cloneSection).repos() {
		// A remote is recorded before its push: one whose copy didn't
		// finish may have the VM's work too.
		if r.Remote == "" {
			continue
		}
		got, err := e.fetchRepo(ctx, t, r)
		if err != nil {
			return out, err
		}
		if got.Path != "" { // fetched from, not left alone
			out.Checked++
		}
		if len(got.Branches) > 0 {
			out.Repos = append(out.Repos, got)
		}
	}
	return out, nil
}

// fetchRepo is Fetch's of the repo r.
func (e Clone) fetchRepo(ctx context.Context, t *target, r cloneRepo) (FetchedRepo, error) {
	vm, dir := t.f.Name, repoDir(t.f.Dir, r.Path)
	url := cloneURL(vm, dir)
	// Fetched into only while it's still that repo, not one above it or
	// put in its place, and no other VM can swap it.
	if err := e.clones.stillRepo(ctx, t, r.Path); err != nil {
		if m, ok := errors.AsType[msg.M](err); ok && m.ID() == msg.CloneRepoMounted("", "").ID() {
			e.Warn(m)
		} else {
			e.Warn(msg.CloneRepoChanged(r.Path))
		}
		return FetchedRepo{}, nil
	}
	switch got, ok := remoteURL(ctx, dir, r.Remote); {
	case !ok:
		if _, err := hostGit(ctx, dir, "remote", "add", r.Remote, url); err != nil {
			return FetchedRepo{}, msg.RemoteAddFailed(r.Remote)
		}
	case got != url:
		e.Warn(msg.CloneRemoteElsewhere(r.Path, r.Remote))
		return FetchedRepo{}, nil
	}
	before := trackingRefs(ctx, dir, r.Remote)
	fail := msg.CloneFetchFailed(r.Path, vm)
	if err := e.clones.fetchInto(ctx, t, dir, msg.FetchingRepo(r.Path), fail, "+refs/heads/*:refs/remotes/"+r.Remote+"/*"); err != nil {
		return FetchedRepo{}, err
	}
	out := FetchedRepo{Path: r.Path, Remote: r.Remote}
	for name, sha := range trackingRefs(ctx, dir, r.Remote) {
		if before[name] != sha {
			out.Branches = append(out.Branches, name)
		}
	}
	slices.Sort(out.Branches)
	return out, nil
}

// trackingRefs are remote's remote-tracking branches in the repo at dir,
// by branch name, each with its commit.
func trackingRefs(ctx context.Context, dir, remote string) map[string]string {
	prefix := "refs/remotes/" + remote + "/"
	out, _ := hostGit(ctx, dir, "for-each-ref", "--format=%(objectname) %(refname)", prefix)
	refs := map[string]string{}
	for line := range strings.Lines(out) {
		sha, ref, _ := strings.Cut(strings.TrimSpace(line), " ")
		if name, ok := strings.CutPrefix(ref, prefix); ok && name != "HEAD" {
			refs[name] = sha
		}
	}
	return refs
}

// Add copies the repo at dir, a folder in the --clone VM's project folder
// (or below the folder devvm runs in, when relative), into the VM, at its
// path, starting the VM if it's stopped, as a workspace clone's seed
// copies each (prepareSeed, then seedRepo), and returns its path relative
// to the project folder. A clone of the folder's own repo becomes a
// workspace clone of it. A repo the clone has already is said to be.
func (e Clone) Add(ctx context.Context, dir string) error {
	t, err := e.finder.target(ctx, need{has: hasMade})
	if err != nil {
		return err
	}
	vm, ws := t.f.Name, t.f.Dir
	if !isClone(t.st) {
		return msg.CloneAddMounted(vm)
	}
	rel, err := e.addPath(ws, dir)
	if err != nil {
		return err
	}
	c := sectionOf[cloneInVM](t.st, cloneSection)
	i := slices.IndexFunc(c.repos(), func(r cloneRepo) bool { return r.Path == rel })
	if i >= 0 && c.repos()[i].Seeded {
		e.Info(msg.CloneAddAlready(rel, vm))
		return nil
	}
	if err := needGit(); err != nil {
		return err
	}
	s, err := e.clones.prepareSeed(ctx, t, rel)
	if err != nil {
		return err
	}
	if err := t.ensureRunning(ctx); err != nil {
		return err
	}
	// Recorded first, so rm checks it however far the copy gets; one
	// recorded whose copy didn't finish is copied again.
	if i < 0 {
		if err := update(ctx, t.st, cloneSection, func(c *cloneInVM) error { addRepo(c, rel); return nil }); err != nil {
			return err
		}
	}
	c = sectionOf[cloneInVM](t.st, cloneSection)
	// The folder's own repo added: its clone leaves out every other one,
	// and what the host's leaves out; another added: that one.
	exclude := repoExcludes(ws, c, []string{rel}, false)
	if rel == "." {
		var all []string
		for _, r := range c.repos() {
			all = append(all, r.Path)
		}
		exclude = repoExcludes(ws, c, all, true)
	}
	seeds := []repoSeed{s}
	if err := e.clones.initRepos(ctx, t, seeds, exclude); err != nil {
		return err
	}
	s = seeds[0]
	if s.detached {
		e.Info(msg.WorkspaceDetached(s.checkout, s.path))
	}
	remote, err := e.clones.seedRepo(ctx, t, s)
	if err != nil {
		return err
	}
	e.Info(msg.CloneAdded(rel, remote))
	return nil
}

// addRepo adds the repo at rel to the clone section c, unseeded: a clone
// of the folder's own repo becomes a workspace clone of it first, with
// its remote and seed, and its files made (it had none to copy).
func addRepo(c *cloneInVM, rel string) {
	if !c.Workspace {
		*c = cloneInVM{
			On: c.On, Workspace: true, Repos: c.repos(),
			Made: cloneMade{Seeded: c.Made.Seeded, Files: true},
		}
	}
	c.Repos = append(c.Repos, cloneRepo{Path: rel})
}

// addPath is devvm clone add's folder, dir, relative to the project
// folder ws: resolved (from the folder devvm runs in, when relative), and
// in ws.
func (e Clone) addPath(ws, dir string) (string, error) {
	if !filepath.IsAbs(dir) {
		here, err := e.finder.dir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(here, dir)
	}
	abs, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", msg.CloneAddMissing(dir)
	}
	if !paths.Within(abs, ws) {
		return "", msg.CloneAddOutside(abs, ws)
	}
	rel, err := filepath.Rel(ws, abs)
	if err != nil {
		return "", msg.CloneAddOutside(abs, ws)
	}
	if !plainPath(rel) {
		return "", msg.CloneAddName()
	}
	return filepath.ToSlash(rel), nil
}

// coversRepo reports whether st is a workspace clone's state, of the
// project folder ws, whose repos include the one at repo: the finder
// uses that VM in the repo's folders, as the clone has the repo at the
// same path.
func coversRepo(st *state.VM, ws, repo string) bool {
	c := sectionOf[cloneInVM](st, cloneSection)
	rel, err := filepath.Rel(ws, repo)
	if !c.On || !c.Workspace || err != nil {
		return false
	}
	return slices.ContainsFunc(c.Repos, func(r cloneRepo) bool { return r.Path == filepath.ToSlash(rel) })
}
