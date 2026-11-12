package vm

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"errors"
	"io"
	"path"
	"strconv"
	"strings"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/msg"
)

// cloneCheck checks a --clone VM's clone before a command loses it:
// command, as typed, which its messages name, deletes the VM (devvm rm),
// or makes it again mounting the folder (remakes: devvm clone off).
type cloneCheck struct {
	clones
	command string
	remakes bool
}

// run offers to fetch the branches whose commits the host's repos lack,
// in every repo of the clone, and asks before uncommitted changes in a
// repo or its worktrees (changes) are lost, starting the VM if it's
// stopped. Where it can't ask, either refuses. It reports whether to keep
// the VM as it is. What the VM has of every repo is read at once
// (readClone). A repo whose copy didn't finish, the folder's own or a
// workspace clone's, may have the VM's work too, so it's checked when
// it's in the VM; one never made there has nothing to check.
func (e cloneCheck) run(ctx context.Context, t *target) (bool, error) {
	vm := t.f.Name
	c := sectionOf[cloneInVM](t.st, cloneSection)
	// Its recorded submodules too, each named by its path, as a workspace
	// clone's repos are.
	repos := c.allRepos()
	named := c.Workspace || len(repos) > 1
	if len(repos) == 0 {
		return false, nil
	}
	if err := t.ensureRunning(ctx); err != nil {
		return false, e.notStarted(ctx, vm, err)
	}
	got, err := e.readClone(ctx, t, repos)
	if err != nil {
		return false, err
	}
	if !c.Workspace && got[0].gone {
		if !c.Made.Seeded {
			return false, nil // never made in the VM
		}
		// The clone itself isn't there: nothing devvm can check.
		return false, msg.CloneCheckFailed(e.command, vm)
	}
	work, unchecked, err := e.unfetched(ctx, t, named, got)
	if err != nil {
		return false, err
	}
	if err := e.fetchUnfetched(ctx, t, named, work); err != nil {
		return false, err
	}
	if changed := e.changes(named, got); !changed && !unchecked {
		return false, nil
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

// unfetched is the branches of each of the clone's repos (got) whose
// commits the host's repo lacks, each named. A repo never made in the VM
// is passed over. The host's repo must still be that repo (stillOurs),
// else the check is refused. One that another VM mounts, which could
// swap it under host git, is read in the VM alone (its changes), never
// with host git, so its work can't be fetched: devvm names it, and
// unchecked asks before the VM goes, as for uncommitted changes, while
// every other repo is checked as ever. So do the LFS objects that only
// the VM's clone has of the commits the host has (lfsUnfetched).
func (e cloneCheck) unfetched(ctx context.Context, t *target, workspace bool, got []repoInVM) (work []unfetchedIn, unchecked bool, err error) {
	for _, g := range got {
		r := g.repo
		dir := repoDir(t.f.Dir, r.Path)
		if g.gone && !r.Seeded {
			continue // never made in the VM
		}
		mounted, err := e.stillOurs(ctx, t, r)
		switch {
		case err != nil && r.submodule && ctx.Err() == nil:
			// Deinitialised or gone here since its copy: named and asked
			// about, as a mounted one is, never passed over.
			e.Say(msg.CloneSubmoduleUnchecked(r.Path))
			unchecked = true
			continue
		case err != nil:
			return nil, false, err
		}
		if mounted != "" {
			e.Say(msg.CloneRepoUnchecked(r.Path, mounted))
			unchecked = true
			continue
		}
		if unfetched := unfetchedRefs(ctx, dir, g.refs); len(unfetched) > 0 {
			if workspace {
				e.Say(msg.UnfetchedCommitsIn(quoted(names(unfetched)), r.Path))
			} else {
				e.Say(msg.UnfetchedCommits(quoted(names(unfetched))))
			}
			work = append(work, unfetchedIn{repo: r, dir: dir, branches: unfetched})
		}
		if e.lfsUnfetched(ctx, t, dir, r.Path, g.refs) {
			unchecked = true
		}
	}
	return work, unchecked, nil
}

// stillOurs refuses the check of the clone's repo r when the host's isn't
// that repo any more (checkRepoTopOf, a submodule as one:
// clone_check_repo_changed), which host git reads and fetches into, and
// returns the VM other than this one that mounts it, or its common dir
// (mountingVM), "" for none.
func (e cloneCheck) stillOurs(ctx context.Context, t *target, r cloneRepo) (mounted string, err error) {
	dir := repoDir(t.f.Dir, r.Path)
	if err := checkRepoTopOf(ctx, dir, r.submodule); err != nil {
		return "", msg.CloneCheckRepoChanged(e.command, r.Path, t.f.Name).Because(err)
	}
	return e.mountingVM(dir, t.f.Name), nil
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

// changes shows the uncommitted changes in each of the clone's repos and
// in their linked worktrees inside the project folder, each under its
// heading (a workspace clone's naming its repo), at most cloneShown in
// all, and reports whether there are any. It stops showing once it has
// shown cloneShown, but goes on counting. A repo with more worktrees than
// devvm checks counts as changed, and devvm says so, and so does a
// worktree whose detached HEAD has commits nothing else in the VM has.
func (e cloneCheck) changes(workspace bool, got []repoInVM) bool {
	shown, dirty := 0, false
	show := func(heading msg.M, status []byte) {
		if len(bytes.TrimSpace(status)) == 0 {
			return
		}
		dirty = true
		if shown < cloneShown {
			e.Say(heading)
			shown += e.showChanges(status, cloneShown-shown)
		}
	}
	for _, g := range got {
		if g.gone {
			// Moved in the VM, maybe with its work: asked about. One
			// never copied was never made.
			if g.repo.Seeded {
				e.Say(msg.CloneRepoGone(g.repo.Path))
				dirty = true
			}
			continue
		}
		status := notSeeded(g.status, g.same)
		if workspace {
			show(msg.CloneRepoDirty(g.repo.Path), status)
		} else {
			show(msg.CloneDirty(), status)
		}
		for _, wt := range g.trees {
			show(msg.CloneWorktreeDirty(wt.path), wt.status)
			if wt.head != "" {
				// Commits only its detached HEAD has: lost with the VM.
				e.Say(msg.CloneWorktreeDetached(wt.path))
				dirty = true
			}
		}
		if g.more {
			e.Say(msg.CloneWorktreesUnchecked(strconv.Itoa(cloneWorktreesMax)))
			dirty = true
		}
	}
	return dirty
}

// notSeeded is status, git status --porcelain's lines, less those whose
// every path is one of same, which the VM has as its seed put them: the
// repo's own uncommitted changes, which are on this machine still, not
// the VM's work. Untracked files' lines stay, as the seed sends none, and
// so does any line it can't read.
func notSeeded(status []byte, same map[string]bool) []byte {
	if len(same) == 0 {
		return status
	}
	var out bytes.Buffer
	for line := range bytes.Lines(status) {
		if !seededLine(strings.TrimSuffix(string(line), "\n"), same) {
			out.Write(line)
		}
	}
	return out.Bytes()
}

// seededLine is whether the status line l ("XY PATH", or "XY FROM -> TO"
// for a rename or copy, each path quoted when git quotes it) names only
// paths of same.
func seededLine(l string, same map[string]bool) bool {
	if len(l) < 4 || l[2] != ' ' || l[:2] == "??" || l[:2] == "!!" {
		return false
	}
	rest := []string{l[3:]}
	if l[0] == 'R' || l[0] == 'C' {
		from, to, ok := strings.Cut(l[3:], " -> ")
		if !ok {
			return false
		}
		rest = []string{from, to}
	}
	for _, p := range rest {
		if strings.HasPrefix(p, `"`) {
			u, err := strconv.Unquote(p)
			if err != nil {
				return false
			}
			p = u
		}
		if !same[p] {
			return false
		}
	}
	return true
}

// cloneWorktreesMax is how many linked worktrees of each repo of a clone
// rm checks.
const cloneWorktreesMax = 100

// cloneRefsMax is the most branches the clone check takes from the VM, in
// all its repos together, as each costs host git a lookup: past it, the
// check fails.
const cloneRefsMax = 10_000

// cloneStateMax is the most of the VM's answer readClone reads, for all
// of a clone's repos: past it, the check fails rather than miss a repo.
const cloneStateMax = 8 * cloneOutMax

// repoInVM is what the VM has of one repo of its clone (readClone), that
// repo's: its branches, as for-each-ref gives them ("SHA refs/heads/BRANCH" lines),
// its git status, and its linked worktrees inside the project folder,
// each with its status, at most cloneWorktreesMax (more: there were
// more). A repo whose folder is gone in the VM has none of them (gone),
// and changes asks about it, as the VM may have moved it, work and all.
type repoInVM struct {
	repo         cloneRepo
	refs, status []byte
	trees        []treeInVM
	more         bool
	gone         bool // its folder isn't there in the VM
	// same are the paths the VM says are as its seed put them
	// (seedTreeRecord): the repo's own changes, not the VM's.
	same map[string]bool
}

// treeInVM is a linked worktree of a clone's repo: its path in the VM,
// its git status, and when its HEAD is detached at commits no branch,
// tag or remote-tracking branch in the VM has, that commit (head).
type treeInVM struct {
	path   string
	status []byte
	head   string
}

// cloneState is the script readClone runs in the VM: the project folder,
// how many worktrees of each repo to look at (cloneWorktreesMax+1), then
// each repo's path relative to it. For each repo, in order, it prints
// records, each a line "KIND INDEX LENGTH" and that many bytes: gone, for
// a repo whose folder isn't there (every repo, when the project folder
// isn't, as when its copy never began); else refs (its branches, a
// detached HEAD's commit as "SHA HEAD" when no branch has it, and each
// stash entry its seed didn't store, seedStashRecord, as "SHA
// stash@{N}"), status,
// same (when its seed left a record, seedTreeRecord, and its git has
// ls-files --format: each path, ending in a NUL, whose working copy and
// index entries are still those the record names), and for each linked
// worktree below the project folder (git lists its
// own first), path (relative to the project folder), whead (the commit
// its HEAD is detached at, when nothing the VM has besides has it) and
// then wstatus (none for a folder that's gone) or wfail. It counts only the worktrees below
// the folder, so none outside it hides one in it. A git that
// fails on a repo ends it with an error. A worktree whose path holds a
// line break isn't found: the VM can hide anything from what it's asked,
// so readClone checks what it says, not that it says everything.
const cloneState = `if [ ! -d "$1" ]; then
  shift 2; i=0
  for r; do printf 'gone %s 0\n' "$i"; i=$((i+1)); done
  exit 0
fi
ws=$(cd -- "$1" && pwd -P) || exit 1
lim=$2
shift 2
t=$(mktemp -d) || exit 1
trap 'rm -rf -- "$t"' EXIT
i=0
put() { printf '%s %s %s\n' "$1" "$i" "$(wc -c < "$t/o" | tr -d ' ')" && cat -- "$t/o"; }
for r; do
  d=$ws/$r
  if [ ! -d "$d" ]; then
    : > "$t/o"; put gone; i=$((i+1)); continue
  fi
  git -C "$d" for-each-ref --format='%(objectname) %(refname)' refs/heads > "$t/o" || exit 1
  if ! git -C "$d" symbolic-ref -q HEAD >/dev/null 2>&1 && [ -n "$(git -C "$d" rev-list -n1 HEAD --not --branches 2>/dev/null)" ]; then
    printf '%s HEAD\n' "$(git -C "$d" rev-parse -q --verify HEAD)" >> "$t/o"
  fi
  if git -C "$d" rev-parse -q --verify refs/stash >/dev/null 2>&1; then
    git -C "$d" reflog show --format=%H refs/stash -- > "$t/s" || exit 1
    n=0
    while read -r h; do
      grep -qxF -- "$h" "$d/.git/` + seedStashRecord + `" 2>/dev/null || printf '%s stash@{%s}\n' "$h" "$n" >> "$t/o"
      n=$((n+1))
    done < "$t/s"
  fi
  put refs
  git -C "$d" status --porcelain > "$t/o" || exit 1
  put status
  if [ -f "$d/.git/` + seedTreeRecord + `" ] && git -C "$d" ls-files --format=x -- . >/dev/null 2>&1; then
    git -C "$d" update-index -q --refresh >/dev/null 2>&1
    (cd -- "$d" && GIT_LITERAL_PATHSPECS=1 xargs -0 sh -c 'for e; do
      w=${e%% *} r=${e#* }; p=${r#* }
      if [ "$w" = = ]; then if git diff-files --quiet -- "$p"; then c==; else c=changed; fi
      elif [ -L "$p" ]; then c=$(printf %s "$(readlink -- "$p")" | git hash-object --stdin) || continue
      elif [ -f "$p" ]; then c=$(git hash-object --no-filters -- "$p") || continue; if [ -x "$p" ]; then c=$c:x; fi
      elif [ -e "$p" ]; then c=dir
      else c=-; fi
      i=$(git ls-files --format="%(objectmode):%(objectname):%(stage)" -- "$p" | paste -s -d , -) || continue
      if [ "$c ${i:--} $p" = "$e" ]; then printf "%s\000" "$p"; fi
    done' sh < "$d/.git/` + seedTreeRecord + `") > "$t/o" 2>/dev/null || : > "$t/o"
    put same
  fi
  git -C "$d" worktree list --porcelain > "$t/l" || exit 1
  first=1 w='' skip=''
  while IFS= read -r l; do
    case $l in
      "worktree "*) w=${l#worktree }; skip= ;;
      bare|prunable|"prunable "*) skip=1 ;;
      "")
        if [ -n "$first" ]; then first=
        elif [ -n "$w" ] && [ -z "$skip" ]; then printf '%s\n' "$w"; fi
        w= ;;
    esac
  done < "$t/l" > "$t/w"
  k=0
  while IFS= read -r w; do
    case $w in "$ws"/*) ;; *) continue ;; esac
    k=$((k+1)); [ "$k" -le "$lim" ] || break
    printf '%s' "${w#"$ws"/}" > "$t/o"; put path
    if [ -d "$w" ] && ! git -C "$w" symbolic-ref -q HEAD >/dev/null 2>&1 &&
      [ -n "$(git -C "$w" rev-list -n1 HEAD --not --branches --tags --remotes 2>/dev/null)" ]; then
      git -C "$w" rev-parse -q --verify HEAD > "$t/o" && put whead
    fi
    if [ ! -d "$w" ]; then : > "$t/o"; put wstatus
    elif git -C "$w" status --porcelain > "$t/o"; then put wstatus
    else : > "$t/o"; put wfail; fi
  done < "$t/w"
  i=$((i+1))
done`

// readClone reads what t's running VM has of each of repos (cloneState),
// in one command, at most cloneStateMax bytes, and checks it
// (parseCloneState): failing, or anything devvm can't take, is the
// check's failure (clone_check_failed).
func (e cloneCheck) readClone(ctx context.Context, t *target, repos []cloneRepo) ([]repoInVM, error) {
	vm := t.f.Name
	argv := []string{"sh", "-c", cloneState, "sh", t.f.Dir, strconv.Itoa(cloneWorktreesMax + 1)}
	for _, r := range repos {
		argv = append(argv, r.Path)
	}
	out := &bounded{max: cloneStateMax}
	err := t.b.Exec(ctx, vm, argv, backend.ExecOpts{Stdout: out, Stderr: e.vmLog()})
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	var got []repoInVM
	if err == nil && !out.over {
		got, err = parseCloneState(out.Bytes(), t.f.Dir, len(repos))
	}
	if err != nil || out.over {
		return nil, msg.CloneCheckFailed(e.command, vm)
	}
	for i := range got {
		got[i].repo = repos[i]
	}
	return got, nil
}

// errCloneState is parseCloneState's error: the VM's answer isn't what
// cloneState prints.
var errCloneState = errors.New("the clone's answer isn't cloneState's")

// parseCloneState reads cloneState's records for n repos of the clone in
// the project folder ws. The VM's answer is untrusted: each record must
// be whole and of a repo asked for, each repo must have its branches and
// status (or be gone), and a worktree's path must be a clean relative
// path below ws, each once, followed by its status; a worktree's paths
// are its path in the VM. Past cloneWorktreesMax a repo's worktrees are
// counted (more), not kept.
func parseCloneState(out []byte, ws string, n int) ([]repoInVM, error) {
	got := make([]repoInVM, n)
	had := make([]map[string]bool, n)
	seen := map[string]bool{}
	pending, head, refs := "", "", 0
	r := bufio.NewReader(bytes.NewReader(out))
	for {
		kind, i, body, err := nextRecord(r, n)
		if errors.Is(err, errNoRecord) {
			break
		}
		if err != nil {
			return nil, err
		}
		if had[i] == nil {
			had[i] = map[string]bool{}
		}
		if pending != "" && kind != "wstatus" && kind != "wfail" && kind != "whead" {
			return nil, errCloneState
		}
		g := &got[i]
		switch kind {
		case "same":
			// The VM's word, which can only hide its own work.
			if had[i][kind] || had[i]["gone"] {
				return nil, errCloneState
			}
			had[i][kind] = true
			g.same = map[string]bool{}
			for p := range strings.SplitSeq(string(body), "\x00") {
				if p != "" {
					g.same[p] = true
				}
			}
		case "gone", "refs", "status":
			if had[i][kind] {
				return nil, errCloneState
			}
			had[i][kind] = true
			switch kind {
			case "refs":
				// Each is looked up with host git (unfetchedRefs).
				if refs += bytes.Count(body, []byte("\n")); refs > cloneRefsMax {
					return nil, errCloneState
				}
				g.refs = body
			case "status":
				g.status = body
			default:
				g.gone = true
			}
		case "path":
			p, ok := worktreePath(ws, string(body))
			if !ok || seen[p] {
				return nil, errCloneState
			}
			seen[p], pending = true, p
		case "whead":
			sha := strings.TrimSuffix(string(body), "\n")
			if pending == "" || head != "" || !objectName.MatchString(sha) {
				return nil, errCloneState
			}
			head = sha
		case "wstatus":
			if pending == "" {
				return nil, errCloneState
			}
			if len(g.trees) < cloneWorktreesMax {
				g.trees = append(g.trees, treeInVM{path: pending, status: body, head: head})
			} else {
				g.more = true
			}
			pending, head = "", ""
		default: // wfail, or anything else
			return nil, errCloneState
		}
	}
	for i := range got {
		if pending != "" || !had[i]["gone"] && (!had[i]["refs"] || !had[i]["status"]) {
			return nil, errCloneState
		}
	}
	return got, nil
}

// worktreePath is the path in the VM of the worktree the VM named by rel,
// relative to the project folder ws, when rel is a clean relative path
// below it, never ws itself nor out of it.
func worktreePath(ws, rel string) (string, bool) {
	if rel == "" || rel == "." || path.IsAbs(rel) || path.Clean(rel) != rel || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	return path.Join(ws, rel), true
}

// errNoRecord is nextRecord's at the end of the answer.
var errNoRecord = errors.New("no record")

// nextRecord reads one of cloneState's records from r: its kind, its
// repo's index (below n) and its body.
func nextRecord(r *bufio.Reader, n int) (kind string, i int, body []byte, err error) {
	line, err := r.ReadString('\n')
	if line == "" && err != nil {
		return "", 0, nil, errNoRecord
	}
	f := strings.Fields(line)
	if err != nil || len(f) != 3 {
		return "", 0, nil, errCloneState
	}
	i, err1 := strconv.Atoi(f[1])
	size, err2 := strconv.Atoi(f[2])
	if err1 != nil || err2 != nil || i < 0 || i >= n || size < 0 || size > cloneStateMax {
		return "", 0, nil, errCloneState
	}
	body = make([]byte, size)
	if _, err := io.ReadFull(r, body); err != nil {
		return "", 0, nil, errCloneState
	}
	return f[0], i, body, nil
}

// unfetchedIn is a repo of the clone with branches, a detached HEAD or
// stash entries whose commits the host's repo at dir hasn't fetched.
type unfetchedIn struct {
	repo     cloneRepo
	dir      string
	branches []fetchRef
}

// fetchUnfetched offers, once for every repo, to fetch the VM's
// unfetched branches into new local branches of each repo,
// saved/<remote>/<branch> (savedPrefix), never over one that exists, a
// detached HEAD's commits as saved/<remote>/HEAD and each stash entry as
// saved/<remote>/stash-N (stash-N-2 and on when a branch of the VM has
// that name). Branches whose names git refuses are skipped. A workspace
// clone's refusal names devvm clone fetch, which fetches every repo.
func (e cloneCheck) fetchUnfetched(ctx context.Context, t *target, workspace bool, work []unfetchedIn) error {
	vm := t.f.Name
	var fetches []savedFetch
	for _, w := range work {
		// A repo whose copy didn't finish may have no remote recorded.
		w.repo.Remote = cmp.Or(w.repo.Remote, "devvm")
		good, bad := savedNames(ctx, w.dir, w.branches)
		if len(bad) > 0 {
			e.Warn(msg.BranchNamesInvalid(quoted(bad)))
		}
		if len(good) == 0 {
			continue
		}
		prefix, ok := savedPrefix(ctx, w.dir, w.repo.Remote)
		if !ok {
			return msg.SavedNamesTaken(e.command, w.repo.Remote, vm)
		}
		fetches = append(fetches, savedFetch{unfetchedIn: w, prefix: prefix, branches: good})
	}
	if len(fetches) == 0 {
		return nil
	}
	q := msg.FetchBeforeDeleteRepos(len(fetches)).RefusedWith(msg.NoTerminalFetchRepos(e.command))
	if len(fetches) == 1 && !workspace {
		q = msg.FetchBeforeDelete(fetches[0].prefix).RefusedWith(msg.NoTerminalFetch(e.command))
	}
	fetch, err := e.Confirm(q)
	if err != nil || !fetch {
		return err
	}
	if err := e.writeSSHConfig(ctx); err != nil {
		return err
	}
	for _, f := range fetches {
		// Again, as the question may have waited: written into only
		// while it's that repo, and no other VM mounts it.
		if mounted, err := e.stillOurs(ctx, t, f.repo); err != nil {
			return err
		} else if mounted != "" {
			return msg.CloneRepoMounted(f.repo.Path, mounted)
		}
		// The VM is about to go: a stash entry not saved keeps it.
		saved, err := e.fetchSaved(ctx, t, f.dir, f.prefix, f.branches, true, msg.Fetching(f.prefix), msg.FetchFailed(e.command, vm))
		if err != nil {
			return err
		}
		if len(saved) > 0 {
			e.Info(msg.SavedBranches(quoted(saved)))
		}
	}
	return nil
}

// savedNames are refs as the saved branches they're fetched into name
// them below saved/<remote>/: each name once, a stash entry's with its
// NAME-untracked too (saveStash), so a stash entry's stash-N that a VM
// branch has too gets -2 and on; those git takes as a branch's (good) and
// the names of those it refuses (bad).
func savedNames(ctx context.Context, dir string, refs []fetchRef) (good []fetchRef, bad []string) {
	taken := map[string]bool{}
	for _, r := range refs {
		if !r.stash {
			taken[r.name] = true
		}
	}
	for _, r := range refs {
		if !r.stash {
			if validBranch(ctx, dir, r.name) {
				good = append(good, r)
			} else {
				bad = append(bad, r.name)
			}
			continue
		}
		base := r.name
		for n := 2; taken[r.name] || taken[r.name+"-untracked"]; n++ {
			r.name = base + "-" + strconv.Itoa(n)
		}
		taken[r.name], taken[r.name+"-untracked"] = true, true
		if validBranch(ctx, dir, r.name) {
			good = append(good, r)
		} else {
			bad = append(bad, r.name)
		}
	}
	return good, bad
}

// savedFetch is one repo's fetch of fetchUnfetched: its branches whose
// names git takes, into local branches under prefix.
type savedFetch struct {
	unfetchedIn
	prefix   string
	branches []fetchRef
}
