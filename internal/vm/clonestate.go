package vm

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/devvmhq/devvm/internal/hostgit"
	"github.com/devvmhq/devvm/internal/safefile"
	"github.com/devvmhq/devvm/internal/state"
)

// The seed's state: what the VM's clone gets of the repo's git dir besides
// its refs and index. Always the info files git reads (sparse-checkout,
// attributes, and exclude, added to the VM's own). While an operation is in progress, and
// only then, that operation's files and pseudo-refs, so the VM can go on
// with it: a rebase (by its folder, rebase-merge or rebase-apply), a
// merge (by its MERGE_HEAD file), a cherry-pick or a revert (by its
// pseudo-ref or the sequencer folder) and bisect (by its BISECT_START
// file). An operation is never told by a pseudo-ref's file alone, as a
// reftable repo keeps pseudo-refs as refs (rev-parse finds them); they go
// as refs, set with update-ref, never as files. ORIG_HEAD from a reset
// long done never goes, as it may be all that still reaches a commit you
// took back. The commits the operation names go too, but only those its
// files hold as object ids (stateIDs): never one quoted in free text, such
// as a merge message's. And the stash (stashEntries), which a linked
// worktree's seed leaves out, as the stash is the whole repo's.

// stateFileMax is the most of one state file the seed sends, and
// stateMax the most of them all.
const (
	stateFileMax = 16 << 20
	stateMax     = 64 << 20
)

// stashMax is the most stash entries the seed sends.
const stashMax = 1000

// errStateTooBig is readRepoState's error for state files past stateFileMax
// or, all together, stateMax.
var errStateTooBig = errors.New("the repo's operation in progress has more files than devvm copies")

// stashEntry is a stash entry the seed sends: its commit, and its reflog
// message (git stash list's).
type stashEntry struct{ sha, msg string }

// seedState is what the seed sends of the repo's state (see above): its
// files, each by its path in the git dir, the pseudo-refs to set, the
// refs of an operation (refs/rewritten/, refs/bisect/), the objects its
// files name, the commits its todo lists name (todoCommits), which the
// pack doesn't carry, the stash, oldest first, the stash entries left
// out as git stash --all ones, and those left out past stashMax
// (stash@{N}).
type seedState struct {
	exclude   []byte
	files     []tarEntry
	pseudo    []seedRef
	refs      []seedRef
	tips      []string
	todo      []string
	stash     []stashEntry
	stashAll  []string
	stashOver []string
	size      int
}

// readRepoState reads the state of the repo at dir that its seed sends; a
// linked worktree's (worktree) has no stash.
func readRepoState(ctx context.Context, dir string, worktree bool) (seedState, error) {
	var s seedState
	r := stateReader{ctx: ctx, dir: dir, s: &s}
	// info/exclude goes on its own (exclude), as the VM's has devvm's own
	// lines, which keep a workspace's other repos out of each one's status.
	if p, ok := r.gitPath("info/exclude"); ok {
		if b, err := safefile.Read(p, stateFileMax, safefile.Opts{}); err == nil {
			s.exclude = b
		}
	}
	for _, f := range []string{"info/sparse-checkout", "info/attributes"} {
		r.file(f)
	}
	r.operations()
	if r.err != nil {
		return seedState{}, r.err
	}
	s.files = dedupFiles(s.files)
	ids, err := haveObjects(ctx, dir, stateIDs(s.files, bisectTerms(s.files)))
	if err != nil {
		return seedState{}, err
	}
	s.tips = ids
	s.todo = todoCommits(s.files)
	if !worktree {
		if s.stash, s.stashAll, s.stashOver, err = stashEntries(ctx, dir, stashMax); err != nil {
			return seedState{}, err
		}
	}
	return s, nil
}

// operations reads the files, pseudo-refs and refs of each operation in
// progress. ORIG_HEAD never goes, as a reset made meanwhile points it at
// what was taken back, but for git am's (rebase-apply/applying), whose
// --abort needs it.
func (r *stateReader) operations() {
	rebase := r.isDir("rebase-merge") || r.isDir("rebase-apply")
	pick := r.pseudoRef("CHERRY_PICK_HEAD") != "" || r.pseudoRef("REVERT_HEAD") != "" || r.isDir("sequencer")
	if rebase {
		r.folder("rebase-merge")
		r.folder("rebase-apply")
		r.pseudo("REBASE_HEAD", "AUTO_MERGE")
		if r.isFile("rebase-apply/applying") {
			r.pseudo("ORIG_HEAD")
		}
		r.namespace("refs/rewritten")
	}
	if r.isFile("MERGE_HEAD") {
		for _, f := range []string{"MERGE_HEAD", "MERGE_MSG", "MERGE_MODE", "MERGE_AUTOSTASH", "SQUASH_MSG"} {
			r.file(f)
		}
		r.pseudo("AUTO_MERGE")
	}
	if pick {
		r.file("MERGE_MSG")
		r.folder("sequencer")
		r.pseudo("CHERRY_PICK_HEAD", "REVERT_HEAD", "AUTO_MERGE")
	}
	if r.isFile("BISECT_START") {
		for _, f := range []string{"BISECT_START", "BISECT_LOG", "BISECT_NAMES", "BISECT_EXPECTED_REV", "BISECT_ANCESTORS_OK", "BISECT_TERMS", "BISECT_RUN", "BISECT_FIRST_PARENT"} {
			r.file(f)
		}
		r.pseudo("BISECT_HEAD")
		r.namespace("refs/bisect")
	}
}

// stateReader reads state files and refs of the repo at dir into s,
// keeping the first error.
type stateReader struct {
	ctx context.Context
	dir string
	s   *seedState
	err error
}

// gitPath is where the repo's git dir (a linked worktree's own, for what
// git keeps for each worktree) has name.
func (r *stateReader) gitPath(name string) (string, bool) {
	p, err := hostGit(r.ctx, r.dir, "rev-parse", "--path-format=absolute", "--git-path", name)
	if err != nil || !filepath.IsAbs(p) {
		r.fail(err)
		return "", false
	}
	return p, true
}

// fail keeps err, the first.
func (r *stateReader) fail(err error) {
	if r.err == nil && err != nil {
		r.err = err
	}
}

// isDir is whether name in the git dir is a folder, never a link.
func (r *stateReader) isDir(name string) bool {
	p, ok := r.gitPath(name)
	if !ok {
		return false
	}
	d, err := openStateDir(p)
	if err != nil {
		return false
	}
	_ = d.Close()
	return true
}

// isFile is whether name in the git dir is a regular file.
func (r *stateReader) isFile(name string) bool {
	p, ok := r.gitPath(name)
	if !ok {
		return false
	}
	f, err := safefile.Open(p, safefile.Opts{})
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// file adds name in the git dir to the state, when it's a regular file,
// read through safefile.
func (r *stateReader) file(name string) {
	p, ok := r.gitPath(name)
	if !ok {
		return
	}
	b, err := safefile.Read(p, stateFileMax, safefile.Opts{})
	switch {
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, safefile.ErrNotRegular):
	case err != nil:
		r.fail(err)
	default:
		r.add(name, b)
	}
}

// folder adds the regular files in the folder name in the git dir,
// opened from its parent, never through a link.
func (r *stateReader) folder(name string) {
	p, ok := r.gitPath(name)
	if !ok {
		return
	}
	d, err := openStateDir(p)
	if err != nil {
		return
	}
	defer func() { _ = d.Close() }()
	entries, err := d.ReadDir()
	if err != nil {
		r.fail(err)
		return
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		b, err := d.Read(e.Name(), stateFileMax, safefile.Opts{})
		if err != nil {
			r.fail(err)
			return
		}
		r.add(name+"/"+e.Name(), b)
	}
}

// add adds the file name, its contents b, within stateMax.
func (r *stateReader) add(name string, b []byte) {
	if r.s.size += len(b); r.s.size > stateMax {
		r.fail(errStateTooBig)
		return
	}
	r.s.files = append(r.s.files, tarEntry{"state/" + name, b})
}

// pseudoRef is the object the pseudo-ref name resolves to, "" for none:
// never a branch or tag of that name, which git resolves too.
func (r *stateReader) pseudoRef(name string) string {
	full, err := hostGit(r.ctx, r.dir, "rev-parse", "-q", "--verify", "--symbolic-full-name", "--end-of-options", name)
	if err != nil || full != name {
		return ""
	}
	sha, err := hostGit(r.ctx, r.dir, "rev-parse", "-q", "--verify", "--end-of-options", name)
	if err != nil || !objectName.MatchString(sha) {
		return ""
	}
	return sha
}

// pseudo adds the pseudo-refs names that resolve, each once.
func (r *stateReader) pseudo(names ...string) {
	for _, n := range names {
		if sha := r.pseudoRef(n); sha != "" && !slices.ContainsFunc(r.s.pseudo, func(p seedRef) bool { return p.name == n }) {
			r.s.pseudo = append(r.s.pseudo, seedRef{sha: sha, name: n})
		}
	}
}

// namespace adds the refs below ns, as the repo's worktree sees them.
func (r *stateReader) namespace(ns string) {
	out, err := hostGit(r.ctx, r.dir, "for-each-ref", "--format=%(objectname) %(refname)", ns)
	if err != nil {
		r.fail(err)
		return
	}
	for line := range strings.Lines(out) {
		sha, ref, _ := strings.Cut(strings.TrimSpace(line), " ")
		if objectName.MatchString(sha) && strings.HasPrefix(ref, ns+"/") {
			r.s.refs = append(r.s.refs, seedRef{sha: sha, name: ref})
		}
	}
}

// openStateDir opens the folder at p, opened from its parent, so a link
// at p is refused (safefile's OpenDir).
func openStateDir(p string) (*safefile.Root, error) {
	parent, err := safefile.OpenRoot(filepath.Dir(p))
	if err != nil {
		return nil, err
	}
	defer func() { _ = parent.Close() }()
	return parent.OpenDir(filepath.Base(p))
}

// dedupFiles is files, each name once (MERGE_MSG can be named twice).
func dedupFiles(files []tarEntry) []tarEntry {
	seen := map[string]bool{}
	return slices.DeleteFunc(files, func(e tarEntry) bool {
		dup := seen[e.name]
		seen[e.name] = true
		return dup
	})
}

// stateIDs are the object ids in the state files known to hold them, and
// only there (never a message's free text): each line's first word of
// rebase-merge's onto, orig-head, stopped-sha, amend, autostash and
// squash-onto, the first two of each line of its rewritten-list and
// rewritten-pending, every id in its update-refs (a ref's name, then its
// two ids, each on a line), rebase-apply's onto, orig-head and
// abort-safety, each line of MERGE_HEAD, MERGE_AUTOSTASH, sequencer's
// head and abort-safety, BISECT_EXPECTED_REV, and the ids of BISECT_LOG's
// git bisect lines for good, bad, skip, new, old and the terms (terms)
// BISECT_TERMS names.
func stateIDs(files []tarEntry, terms []string) []string {
	first := []string{
		"rebase-merge/onto", "rebase-merge/orig-head", "rebase-merge/stopped-sha", "rebase-merge/amend", "rebase-merge/autostash", "rebase-merge/squash-onto", "rebase-merge/update-refs",
		"rebase-apply/onto", "rebase-apply/orig-head", "rebase-apply/abort-safety",
		"MERGE_HEAD", "MERGE_AUTOSTASH", "sequencer/head", "sequencer/abort-safety", "BISECT_EXPECTED_REV",
	}
	words := slices.Concat([]string{"good", "bad", "skip", "new", "old"}, terms)
	var ids []string
	for _, f := range files {
		name := strings.TrimPrefix(f.name, "state/")
		for line := range strings.Lines(string(f.data)) {
			w := strings.Fields(line)
			switch {
			case len(w) == 0:
			case slices.Contains(first, name):
				ids = append(ids, w[0])
			case name == "rebase-merge/rewritten-list" || name == "rebase-merge/rewritten-pending":
				ids = append(ids, w[:min(2, len(w))]...)
			case name == "BISECT_LOG" && len(w) > 2 && w[0] == "git" && w[1] == "bisect" && slices.Contains(words, w[2]):
				ids = append(ids, w[3:]...)
			}
		}
	}
	return slices.DeleteFunc(ids, func(id string) bool { return !objectName.MatchString(id) })
}

// bisectTerms are the bisect terms BISECT_TERMS names, among files.
func bisectTerms(files []tarEntry) []string {
	for _, f := range files {
		if f.name == "state/BISECT_TERMS" {
			return strings.Fields(string(f.data))
		}
	}
	return nil
}

// todoCommits are the commits a rebase's todo list (rebase-merge's
// git-rebase-todo) or a sequencer's (sequencer/todo) names, as written
// there, often abbreviated: each line's commit after its command (and a
// fixup's -C or -c), comments and commands that name none aside. They're
// only checked for, never packed: the VM may lack one (missingCommits).
func todoCommits(files []tarEntry) []string {
	var out []string
	for _, f := range files {
		if f.name != "state/rebase-merge/git-rebase-todo" && f.name != "state/sequencer/todo" {
			continue
		}
		for line := range strings.Lines(string(f.data)) {
			w := strings.Fields(line)
			if len(w) > 2 && (w[1] == "-C" || w[1] == "-c") {
				w = append(w[:1], w[2:]...)
			}
			if len(w) > 1 && slices.Contains(todoCommands(), w[0]) && abbrevName.MatchString(w[1]) {
				out = append(out, w[1])
			}
		}
	}
	return out
}

// todoCommands are the todo list commands that name a commit after them.
func todoCommands() []string {
	return []string{"pick", "p", "reword", "r", "edit", "e", "squash", "s", "fixup", "f", "drop", "d", "revert", "merge", "m"}
}

// abbrevName is an object name as git abbreviates it: 4 hex digits or
// more.
var abbrevName = regexp.MustCompile(`^[0-9a-f]{4,64}$`)

// missingCommits are those of commits (todoCommits) that the pack of
// tips won't carry: each one host git resolves, which no tip reaches,
// and each one it can't, as written.
func missingCommits(ctx context.Context, dir string, commits []string, tips string) []string {
	var out []string
	for _, c := range commits {
		sha, err := hostGit(ctx, dir, "rev-parse", "-q", "--verify", "--end-of-options", c+"^{commit}")
		if err != nil || !objectName.MatchString(sha) {
			out = append(out, c)
			continue
		}
		var in strings.Builder
		in.WriteString(sha + "\n")
		for t := range strings.FieldsSeq(tips) {
			in.WriteString("^" + t + "\n")
		}
		cmd := hostgit.Command(ctx, "-C", dir, "rev-list", "-n1", "--stdin")
		cmd.Stdin = strings.NewReader(in.String())
		if got, err := cmd.Output(); err != nil || len(bytes.TrimSpace(got)) > 0 {
			out = append(out, c)
		}
	}
	return out
}

// haveObjects are those of ids the repo at dir has (cat-file
// --batch-check), each once, in order: each line of the answer is looked
// up in the sorted ids by binary search, so a partial monorepo's many
// tips take O(n log n), not O(n²). ids is left as it was.
func haveObjects(ctx context.Context, dir string, ids []string) ([]string, error) {
	ids = slices.Compact(slices.Sorted(slices.Values(ids)))
	if len(ids) == 0 {
		return nil, nil
	}
	cmd := hostgit.Command(ctx, "-C", dir, "cat-file", "--batch-check=%(objectname)")
	cmd.Stdin = strings.NewReader(strings.Join(ids, "\n") + "\n")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var have []string
	for line := range strings.Lines(string(out)) {
		id := strings.TrimSpace(line)
		if _, found := slices.BinarySearch(ids, id); found && objectName.MatchString(id) {
			have = append(have, id)
		}
	}
	return have, nil
}

// stashEntries are the repo at dir's stash entries, oldest first, the
// newest max of them: those past it are named (over), and so are those
// git stash --all made (all), which hold ignored files (holdsIgnored),
// each as stash@{N}, as git stash list names it.
func stashEntries(ctx context.Context, dir string, maxEntries int) (sent []stashEntry, all, over []string, err error) {
	if _, err := hostGit(ctx, dir, "rev-parse", "-q", "--verify", "refs/stash"); err != nil {
		return nil, nil, nil, nil //nolint:nilerr // no stash: nothing to send
	}
	out, err := hostGit(ctx, dir, "reflog", "show", "--format=%H %gs", "refs/stash", "--")
	if err != nil {
		return nil, nil, nil, err
	}
	lines := strings.Split(out, "\n")
	for n := len(lines) - 1; n >= 0; n-- {
		sha, m, _ := strings.Cut(lines[n], " ")
		switch {
		case !objectName.MatchString(sha):
			continue
		case n >= maxEntries:
			over = append(over, "stash@{"+strconv.Itoa(n)+"}")
			continue
		}
		if ignored, err := holdsIgnored(ctx, dir, sha); err != nil {
			return nil, nil, nil, err
		} else if ignored {
			all = append(all, "stash@{"+strconv.Itoa(n)+"}")
			continue
		}
		sent = append(sent, stashEntry{sha: sha, msg: m})
	}
	return sent, all, over, nil
}

// holdsIgnored is whether the stash entry sha may hold ignored files, as
// git stash --all's do, erring toward yes: when it has untracked files
// (a third parent) and any of them is a .gitignore, or the entry changes
// a .gitignore (stash --all takes back an edit of one too), or the rules
// ignore any of its untracked files, as they are now or as the entry's
// .gitignore files had them when it was made (ignoredThen).
func holdsIgnored(ctx context.Context, dir, sha string) (bool, error) {
	if _, err := hostGit(ctx, dir, "rev-parse", "-q", "--verify", sha+"^3"); err != nil {
		return false, nil //nolint:nilerr // no untracked files: not git stash --all's
	}
	paths, err := hostgit.Output(ctx, "-C", dir, "ls-tree", "-r", "-z", "--name-only", sha+"^3")
	if err != nil {
		return false, err
	}
	changed, err := hostgit.Output(ctx, "-C", dir, "diff-tree", "-r", "-z", "--name-only", "--no-renames", sha+"^1", sha)
	if err != nil {
		return false, err
	}
	for _, p := range slices.Concat(bytes.Split(paths, []byte{0}), bytes.Split(changed, []byte{0})) {
		if filepath.Base(string(p)) == ".gitignore" {
			return true, nil
		}
	}
	if len(paths) == 0 {
		return false, nil
	}
	if ignored, err := checkIgnored(ctx, dir, "", paths); err != nil || ignored {
		return ignored, err
	}
	return ignoredThen(ctx, dir, sha, paths)
}

// ignoredThen is whether the rules of the .gitignore files in the stash
// entry sha's tree, as they were when it was made, with the repo's own
// info/exclude and excludes file, ignore any of paths: checked in a
// folder of its own holding those files alone.
func ignoredThen(ctx context.Context, dir, sha string, paths []byte) (bool, error) {
	list, err := hostgit.Output(ctx, "-C", dir, "ls-tree", "-r", "-z", "--format=%(objectname) %(path)", sha)
	if err != nil {
		return false, err
	}
	tmp, err := os.MkdirTemp("", "devvm-stash-ignore-*")
	if err != nil {
		return false, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	for rec := range bytes.SplitSeq(list, []byte{0}) {
		blob, p, ok := strings.Cut(string(rec), " ")
		if !ok || filepath.Base(p) != ".gitignore" || !safeIndexPath(p) || !objectName.MatchString(blob) {
			continue
		}
		b, err := hostgit.Output(ctx, "-C", dir, "cat-file", "blob", blob)
		if err != nil {
			return false, err
		}
		if err := os.MkdirAll(filepath.Join(tmp, filepath.Dir(p)), 0o700); err != nil {
			return false, err
		}
		if err := state.WriteFile(filepath.Join(tmp, p), b); err != nil {
			return false, err
		}
	}
	return checkIgnored(ctx, dir, tmp, paths)
}

// checkIgnored is whether the repo at dir's ignore rules ignore any of
// paths (NUL-separated): its working tree's, or, given worktree, the
// .gitignore files there instead.
func checkIgnored(ctx context.Context, dir, worktree string, paths []byte) (bool, error) {
	args := []string{"-C", dir}
	if worktree != "" {
		args = append(args, "--work-tree", worktree)
	}
	cmd := hostgit.Command(ctx, append(args, "check-ignore", "--no-index", "--stdin", "-z")...)
	cmd.Stdin = bytes.NewReader(paths)
	out, err := cmd.Output()
	// check-ignore exits 1 when it ignores none.
	if exit, ok := errors.AsType[*exec.ExitError](err); ok && exit.ExitCode() == 1 {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return len(out) > 0, nil
}

// entries are s's tar entries: state/... (its files), exclude (the
// repo's info/exclude), pseudo ("SHA NAME" lines: the pseudo-refs, then
// the operation's refs) and stash ("SHA MESSAGE" lines, oldest first),
// each when there's any.
func (s seedState) entries() []tarEntry {
	out := slices.Clone(s.files)
	if len(s.exclude) > 0 {
		out = append(out, tarEntry{"exclude", s.exclude})
	}
	// The operation's refs go with its pseudo-refs, set only with its state.
	if len(s.pseudo)+len(s.refs) > 0 {
		var b strings.Builder
		for _, p := range slices.Concat(s.pseudo, s.refs) {
			b.WriteString(p.sha + " " + p.name + "\n")
		}
		out = append(out, tarEntry{"pseudo", []byte(b.String())})
	}
	if len(s.stash) > 0 {
		var b strings.Builder
		for _, e := range s.stash {
			b.WriteString(e.sha + " " + strings.ReplaceAll(e.msg, "\n", " ") + "\n")
		}
		out = append(out, tarEntry{"stash", []byte(b.String())})
	}
	return out
}

// objects are the objects s's pack carries: the pseudo-refs' and the
// operation's refs', the ids its files name and the stash entries'.
func (s seedState) objects() []string {
	var out []string
	for _, p := range slices.Concat(s.pseudo, s.refs) {
		out = append(out, p.sha)
	}
	for _, e := range s.stash {
		out = append(out, e.sha)
	}
	return append(out, s.tips...)
}
