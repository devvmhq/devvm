package vm

import (
	"archive/tar"
	"bytes"
	"cmp"
	"context"
	"crypto/sha1" //nolint:gosec // git's SHA-1 object names
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"

	"github.com/devvmhq/devvm/internal/hostgit"
	"github.com/devvmhq/devvm/internal/safefile"
)

// The seed's working tree: what the VM's clone gets of the repo's
// uncommitted changes. Its index is the repo's own, refreshed and
// normalised by host git (a v3 index with no split index, no untracked
// cache, which names untracked files and this machine's path, no
// fsmonitor data, no resolve-undo, whose blobs the pack doesn't carry,
// and a real hash), so what's staged is staged in the VM too, conflicts,
// intent-to-add and skip-worktree included. The tracked files whose
// working copy isn't the index's go as they are, but only a regular file
// or a symbolic link where the index has one of those: never a folder (a
// tracked file turned into one, a submodule, an embedded repo, with
// whatever is in it), never a gitlink, and never through a folder that's
// a symbolic link here, which git takes for a deletion: each folder on
// the way is opened from its parent, never through a link (walk). A file
// a filter changes (git-crypt's, LFS's) goes as git stores it, as host
// git runs no filter and can't tell whether its working copy differs.

// treeFile is a tracked file whose working copy the seed sends: its path,
// and whether it's a symbolic link.
type treeFile struct {
	path string
	link bool
}

// seedTree is what the seed sends of a repo's working tree: the
// normalised index's file, the paths the VM checks out from it (each
// ending in a NUL), the files whose working copies go as they are, the
// paths of the index the VM gets neither for (gone), the index's blobs,
// which the pack carries, and what it names: every path whose working
// copy isn't the index's here (dirty), the filtered ones among them,
// which may differ, and the tracked files that are no longer a file or
// a link here, such as one that's a folder now, which the VM doesn't
// get.
type seedTree struct {
	root                  *safefile.Root
	index                 string
	checkout              []byte
	changed               []treeFile
	gone                  []string
	blobs                 []string
	dirty                 map[string]bool
	filtered, typechanged []string
	// skipped are the skip-worktree paths the working tree has, which go
	// as the index has them (skip.list), named.
	skipped []string
	// The record (record): the repo's object format, the paths it covers,
	// in order, each path's index entries, and the paths not checked out.
	format  string
	covered []string
	stages  map[string][]indexEntry
	away    map[string]bool
}

// indexEntry is a line of git ls-files -s: a path's mode, blob, and its
// stage (0, or 1 to 3 for a conflict).
type indexEntry struct {
	mode, sha, stage, path string
}

// errUnsafePath is load's error for an index with a path git never checks
// out, such as one with a .git or .. part.
var errUnsafePath = errors.New("the index has a path git never checks out")

// newSeedTree makes a seedTree, with the file its index goes in, which
// close removes: its caller registers that before anything else runs.
func newSeedTree() (*seedTree, error) {
	tmp, err := os.CreateTemp("", "devvm-seed-index-*")
	if err != nil {
		return nil, err
	}
	_ = tmp.Close()
	return &seedTree{index: tmp.Name(), dirty: map[string]bool{}}, nil
}

// load reads what the seed sends of the working tree of the repo at dir:
// its index, read from its git dir through safefile into t's file, with
// its modification time, so that git tells a file changed in the second
// it was written as git would with the repo's own (racy git), then
// refreshed and normalised (normalise), and the working tree read
// through a root at dir (read). It's false when the repo has no index.
func (t *seedTree) load(ctx context.Context, dir string) (bool, error) {
	gitIndex, err := hostGit(ctx, dir, "rev-parse", "--path-format=absolute", "--git-path", "index")
	if err != nil {
		return false, err
	}
	src, err := safefile.Open(gitIndex, safefile.Opts{})
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	defer func() { _ = src.Close() }()
	// newSeedTree made it empty, and only load writes it.
	dst, err := os.OpenFile(t.index, os.O_WRONLY, 0o600)
	if err != nil {
		return false, err
	}
	_, err = io.Copy(dst, src)
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chtimes(t.index, src.Info.ModTime(), src.Info.ModTime())
	}
	if err == nil {
		err = t.normalise(ctx, dir)
	}
	if err == nil {
		t.root, err = safefile.OpenRoot(dir)
	}
	if err == nil {
		err = t.read(ctx, dir)
	}
	return err == nil, err
}

// close closes t's root and removes its index's file; again, it does
// nothing.
func (t *seedTree) close() {
	if t.root != nil {
		_ = t.root.Close()
		t.root = nil
	}
	_ = os.Remove(t.index)
}

// git is host git in the repo at dir, reading t's index.
func (t *seedTree) git(ctx context.Context, dir string, stdin []byte, args ...string) ([]byte, error) {
	return t.run(hostgit.Command(ctx, append([]string{"-C", dir}, args...)...), stdin)
}

// run runs c, a host git command, with t's index and stdin.
func (t *seedTree) run(c *exec.Cmd, stdin []byte) ([]byte, error) {
	c.Env = append(c.Env, "GIT_INDEX_FILE="+t.index)
	if stdin != nil {
		c.Stdin = bytes.NewReader(stdin)
	}
	return c.Output()
}

// normalise refreshes t's index, so a file whose stat alone changed
// isn't sent, never entering a submodule, and makes it one any git since
// 1.7 reads, with nothing of this machine's but what's staged: version
// 3, written even if it's that already, no split index, no sparse
// directories, no untracked cache (which names untracked files and this
// machine's path), no fsmonitor data, no resolve-undo, whose blobs no ref
// may reach, and a real trailing hash.
func (t *seedTree) normalise(ctx context.Context, dir string) error {
	_, err := t.git(ctx, dir, nil, "-c", "index.sparse=false", "-c", "core.splitIndex=false", "-c", "core.untrackedCache=false",
		"-c", "core.fsmonitor=false", "-c", "index.skipHash=false", "-c", "feature.manyFiles=false",
		"update-index", "-q", "--unmerged", "--ignore-missing", "--ignore-submodules", "--refresh",
		"--index-version", "3", "--no-split-index", "--no-untracked-cache", "--clear-resolve-undo", "--force-write-index")
	return err
}

// read reads, with t's index, what of the working tree t sends and names.
func (t *seedTree) read(ctx context.Context, dir string) error {
	entries, paths, err := t.entries(ctx, dir)
	if err != nil {
		return err
	}
	diff, err := t.run(hostgit.DiffFiles(ctx, dir, "--name-only", "-z"), nil)
	if err != nil {
		return err
	}
	var dirty []string
	for p := range strings.SplitSeq(string(diff), "\x00") {
		if p != "" && !t.dirty[p] {
			t.dirty[p] = true
			dirty = append(dirty, p)
		}
	}
	filtered, err := t.filteredPaths(ctx, dir, dirty)
	if err != nil {
		return err
	}
	t.away = map[string]bool{}
	for _, p := range dirty {
		e, ok := entries[p]
		switch {
		case !ok, e.mode == "160000":
			// A gitlink: its folder goes empty, as the index has it.
		case filtered[p] && t.exists(p):
			t.filtered = append(t.filtered, p)
		default:
			t.away[p] = true
			t.overlay(p, e)
		}
	}
	sent := map[string]bool{}
	for _, c := range t.changed {
		sent[c.path] = true
	}
	// A conflict's path has no entry to check out (git checkout-index
	// fails on it): its working copy goes as it is, or nothing does. Nor
	// does a sparse checkout's path outside it (skip-worktree), which git
	// leaves out of the working tree, there as here. A skip-worktree path
	// the working tree has, marked by hand, or left by an older git's
	// sparse checkout, is checked out as the index has it, never sent as
	// it is, as its edits, which git hides, are often local settings or
	// secrets, and named.
	skip, err := t.skipWorktree(ctx, dir)
	if err != nil {
		return err
	}
	var checkout bytes.Buffer
	for _, p := range paths {
		switch {
		case skip[p] && !t.exists(p):
		case skip[p]:
			checkout.WriteString(p + "\x00")
			t.skipped = append(t.skipped, p)
		case !t.away[p] && entries[p].stage == "0":
			checkout.WriteString(p + "\x00")
		case !sent[p]:
			t.gone = append(t.gone, p)
		}
	}
	t.checkout = checkout.Bytes()
	return t.cover(ctx, dir, dirty)
}

// entries reads t's index (git ls-files -s): each path's entry, a
// conflict's last stage, theirs, which says what the path is, its paths
// in order, each once, and into t, each path's stages and its blobs. A
// path git never checks out (safeIndexPath) fails it.
func (t *seedTree) entries(ctx context.Context, dir string) (map[string]indexEntry, []string, error) {
	out, err := t.git(ctx, dir, nil, "ls-files", "-s", "-z")
	if err != nil {
		return nil, nil, err
	}
	entries := map[string]indexEntry{}
	t.stages = map[string][]indexEntry{}
	var paths []string
	for rec := range strings.SplitSeq(string(out), "\x00") {
		info, path, ok := strings.Cut(rec, "\t")
		f := strings.Fields(info)
		if !ok || len(f) != 3 || !objectName.MatchString(f[1]) {
			continue
		}
		if !safeIndexPath(path) {
			return nil, nil, errUnsafePath
		}
		e := indexEntry{mode: f[0], sha: f[1], stage: f[2], path: path}
		if _, seen := entries[path]; !seen {
			paths = append(paths, path)
		}
		entries[path] = e
		t.stages[path] = append(t.stages[path], e)
		if e.mode != "160000" {
			t.blobs = append(t.blobs, e.sha)
		}
	}
	return entries, paths, nil
}

// skipWorktree are the paths of t's index that have the skip-worktree bit
// (ls-files -t's S), which a sparse checkout leaves out.
func (t *seedTree) skipWorktree(ctx context.Context, dir string) (map[string]bool, error) {
	out, err := t.git(ctx, dir, nil, "ls-files", "-t", "-z")
	if err != nil {
		return nil, err
	}
	skip := map[string]bool{}
	for rec := range strings.SplitSeq(string(out), "\x00") {
		if p, ok := strings.CutPrefix(rec, "S "); ok {
			skip[p] = true
		}
	}
	return skip, nil
}

// safeIndexPath is whether p is a path git would check out: relative, of
// parts that are neither empty, . nor .., nor .git in any case, also as
// macOS (HFS+) and Windows see one, with characters they ignore or a
// trailing dot or space.
func safeIndexPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") {
		return false
	}
	for part := range strings.SplitSeq(p, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
		bare := strings.TrimRight(strings.Map(func(r rune) rune {
			if r >= 0x200c && r <= 0x200f || r >= 0x202a && r <= 0x202e || r >= 0x206a && r <= 0x206f || r == 0xfeff {
				return -1
			}
			return r
		}, part), ". ")
		if strings.EqualFold(bare, ".git") || strings.EqualFold(bare, "git~1") {
			return false
		}
	}
	return true
}

// walk opens the folder p is in, from t's root, each folder on the way
// from its parent, never through a symbolic link (safefile's OpenDir),
// and returns it, with p's last part; its caller closes the folder when
// it isn't t's root. It's false when a folder on the way is gone, isn't
// one, or is a link, where git sees no file at p.
func (t *seedTree) walk(p string) (*safefile.Root, string, bool) {
	dir, base := "", p
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		dir, base = p[:i], p[i+1:]
	}
	at := t.root
	if dir == "" {
		return at, base, true
	}
	for part := range strings.SplitSeq(dir, "/") {
		next, err := at.OpenDir(part)
		if at != t.root {
			_ = at.Close()
		}
		if err != nil {
			return nil, "", false
		}
		at = next
	}
	return at, base, true
}

// lstat is what's at p in the working tree (walk), never followed.
func (t *seedTree) lstat(p string) (fs.FileInfo, error) {
	at, base, ok := t.walk(p)
	if !ok {
		return nil, fs.ErrNotExist
	}
	if at != t.root {
		defer func() { _ = at.Close() }()
	}
	return at.Lstat(base)
}

// exists is whether the working tree has anything at p (walk).
func (t *seedTree) exists(p string) bool {
	_, err := t.lstat(p)
	return err == nil
}

// overlay sorts p, a tracked path whose working copy isn't the index's
// entry e, out: a regular file or a symbolic link, where e is one of
// those, goes as it is (changed); anything else (a folder, a gitlink's
// folder) goes as gone, and is named (typechanged); a path that's gone,
// or whose folder is a link here, goes as gone.
func (t *seedTree) overlay(p string, e indexEntry) {
	fi, err := t.lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	file := e.mode == "100644" || e.mode == "100755" || e.mode == "120000"
	switch {
	case err == nil && file && fi.Mode().IsRegular():
		t.changed = append(t.changed, treeFile{path: p})
	case err == nil && file && fi.Mode()&fs.ModeSymlink != 0:
		t.changed = append(t.changed, treeFile{path: p, link: true})
	default:
		t.typechanged = append(t.typechanged, p)
	}
}

// filteredPaths are those of paths that a filter attribute names a filter
// for (check-attr), by path, in the working tree's attributes or the
// index's.
func (t *seedTree) filteredPaths(ctx context.Context, dir string, paths []string) (map[string]bool, error) {
	filtered := map[string]bool{}
	if len(paths) == 0 {
		return filtered, nil
	}
	in := strings.Join(paths, "\x00") + "\x00"
	for _, cached := range [][]string{nil, {"--cached"}} {
		out, err := t.git(ctx, dir, []byte(in), append([]string{"check-attr", "--stdin", "-z"}, append(cached, "filter")...)...)
		if err != nil {
			return nil, err
		}
		f := strings.Split(string(out), "\x00")
		for i := 0; i+2 < len(f); i += 3 {
			if v := f[i+2]; v != "unspecified" && v != "unset" {
				filtered[f[i]] = true
			}
		}
	}
	return filtered, nil
}

// cover finds the paths t's record covers: those git status in the VM's
// clone would list as changed once the seed is in, each once, which are
// those whose staged entry isn't HEAD's (every path, when HEAD is
// unborn) and dirty, those whose working copy isn't the index's.
func (t *seedTree) cover(ctx context.Context, dir string, dirty []string) error {
	format, err := objectFormat(ctx, dir)
	if err != nil {
		return err
	}
	t.format = format
	var staged []byte
	if _, err := hostGit(ctx, dir, "rev-parse", "-q", "--verify", "HEAD^{commit}"); err == nil {
		staged, err = t.git(ctx, dir, nil, "diff-index", "--cached", "--no-renames", "--name-only", "-z", "HEAD")
		if err != nil {
			return err
		}
	} else {
		staged, err = t.git(ctx, dir, nil, "ls-files", "-z")
		if err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	for _, p := range append(strings.Split(string(staged), "\x00"), dirty...) {
		if p != "" && !seen[p] {
			seen[p] = true
			t.covered = append(t.covered, p)
		}
	}
	return nil
}

// record is the seed's record of what it put in the VM's clone at each
// path it covers, which rm and devvm clone off read back (cloneState) to
// tell the repo's own changes, unchanged in the VM since, from the VM's
// work: an entry "WORKTREE INDEX PATH", each ending in a NUL. WORKTREE is
// the blob of the working copy the VM got (sent: those sent as they are,
// by path), with ":x" after it for an executable file, "=" for one
// checked out from the index, which the VM's git tells is still the
// index's (diff-files), as git converts it (info/attributes' eol, say),
// "dir" for a gitlink's empty folder, or "-" for none; INDEX is the
// path's index entries, "MODE:BLOB:STAGE" each, joined by commas, or "-"
// for none.
func (t *seedTree) record(sent map[string]string) []byte {
	var b bytes.Buffer
	for _, p := range t.covered {
		idx := make([]string, 0, len(t.stages[p]))
		for _, e := range t.stages[p] {
			idx = append(idx, e.mode+":"+e.sha+":"+e.stage)
		}
		b.WriteString(t.worktreeBlob(p, sent) + " " + cmp.Or(strings.Join(idx, ","), "-") + " " + p + "\x00")
	}
	return b.Bytes()
}

// worktreeBlob is the blob of what the VM's clone has at p once the seed
// is in (record's WORKTREE).
func (t *seedTree) worktreeBlob(p string, sent map[string]string) string {
	if blob, ok := sent[p]; ok {
		return blob
	}
	st := t.stages[p]
	switch {
	case t.away[p] || len(st) != 1 || st[0].stage != "0":
		return "-"
	case st[0].mode == "160000":
		return "dir"
	}
	return "="
}

// blobHasher hashes a blob of size bytes as git does, in t's object
// format.
func (t *seedTree) blobHasher(size int64) hash.Hash {
	h := sha1.New() //nolint:gosec // git's object names, not a security check
	if t.format == "sha256" {
		h = sha256.New()
	}
	_, _ = h.Write([]byte("blob " + strconv.FormatInt(size, 10) + "\x00"))
	return h
}

// write writes t's tar entries to tw: index, checkout (the paths to check
// out, each ending in a NUL), changed.list (the paths of the working
// copies sent, each ending in a NUL), gone.list (gone's, the same),
// skip.list (skipped, the same), and
// each copy sent under changed/, read through t's root (walk): a regular
// file, never through a link, with its mode, or a link, as it is; last,
// tree.rec, the record (record).
func (t *seedTree) write(tw *tar.Writer) error {
	idx, err := os.Open(t.index)
	if err != nil {
		return err
	}
	defer func() { _ = idx.Close() }()
	fi, err := idx.Stat()
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "index", Size: fi.Size(), Mode: 0o600}); err != nil {
		return err
	}
	if _, err := io.Copy(tw, idx); err != nil {
		return err
	}
	changed := make([]string, 0, len(t.changed))
	for _, c := range t.changed {
		changed = append(changed, c.path)
	}
	for _, e := range []tarEntry{{"checkout", t.checkout}, {"changed.list", nulList(changed)}, {"gone.list", nulList(t.gone)}, {"skip.list", nulList(t.skipped)}} {
		if err := writeEntry(tw, e); err != nil {
			return err
		}
	}
	sent := map[string]string{}
	for _, c := range t.changed {
		blob, err := t.writeFile(tw, c)
		if err != nil {
			return err
		}
		sent[c.path] = blob
	}
	return writeEntry(tw, tarEntry{"tree.rec", t.record(sent)})
}

// nulList is paths, each ending in a NUL.
func nulList(paths []string) []byte {
	var b bytes.Buffer
	for _, p := range paths {
		b.WriteString(p + "\x00")
	}
	return b.Bytes()
}

// writeFile writes the working copy of c to tw, as changed/<path>, with
// its modification time, and returns its blob, as git names it.
func (t *seedTree) writeFile(tw *tar.Writer, c treeFile) (string, error) {
	at, base, ok := t.walk(c.path)
	if !ok {
		return "", fs.ErrNotExist
	}
	if at != t.root {
		defer func() { _ = at.Close() }()
	}
	name := "changed/" + c.path
	if c.link {
		fi, err := at.Lstat(base)
		if err != nil {
			return "", err
		}
		target, err := at.Readlink(base)
		if err != nil {
			return "", err
		}
		h := t.blobHasher(int64(len(target)))
		_, _ = h.Write([]byte(target))
		return hex.EncodeToString(h.Sum(nil)), tw.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: name, Linkname: target, Mode: 0o777, ModTime: fi.ModTime()})
	}
	f, err := at.Open(base, safefile.Opts{})
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	size := f.Info.Size()
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Size: size, Mode: int64(f.Info.Mode().Perm()), ModTime: f.Info.ModTime()}); err != nil {
		return "", err
	}
	h := t.blobHasher(size)
	if _, err := io.CopyN(io.MultiWriter(tw, h), f, size); err != nil {
		return "", err
	}
	blob := hex.EncodeToString(h.Sum(nil))
	if f.Info.Mode().Perm()&0o100 != 0 {
		blob += ":x"
	}
	return blob, nil
}

// writeEntry writes e to tw as a regular file.
func writeEntry(tw *tar.Writer, e tarEntry) error {
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: e.name, Size: int64(len(e.data)), Mode: 0o600}); err != nil {
		return err
	}
	_, err := tw.Write(e.data)
	return err
}

// ownFiles are the VM's own files that a seed's apply kept (seedApply's
// kept-files, and pre, the paths of changed.list and gone.list that the
// VM had before the apply): those whose content isn't the index's, but
// for the copies sent and the paths gone, which the seed made so, and
// every path it would have sent or left gone that the VM had already.
func (t *seedTree) ownFiles(files, pre []string) []string {
	mine := map[string]bool{}
	for _, p := range pre {
		mine[p] = true
	}
	ours := map[string]bool{}
	for _, c := range t.changed {
		ours[c.path] = true
	}
	for _, p := range t.gone {
		ours[p] = true
	}
	var out []string
	seen := map[string]bool{}
	for _, f := range append(slices.Clone(files), pre...) {
		if !seen[f] && (!ours[f] || mine[f]) {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}
