package vm

import (
	"archive/tar"
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/gitprotect"
	"github.com/devvmhq/devvm/internal/hostgit"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/safefile"
)

// Git LFS, both ways, with no git-lfs on this machine. In: the seed sends
// the LFS objects that the index's pointers name and the repo has, each
// hashed before it's sent (readLFS) and again as it's sent (writeLFS),
// which the VM puts in its repo's .git/lfs/objects before the checkout;
// the VM's git-lfs (base's image), set up in that repo alone, then checks
// the files out (seedApply). Back: once a fetch from the VM is in, the LFS
// objects that the fetched commits' history names past this repo's own
// refs, and that this repo lacks, are read from the VM in one stream, each
// exactly its pointer's size, and written into this repo's
// .git/lfs/objects only when its SHA-256 is its oid (fetchLFS). Every
// file there is opened beneath the repo's common dir, each folder on the
// way checked to be one, never a link (lfsCheckDirs), so a link planted in
// .git/lfs can't lead a read or a write anywhere else.

const (
	// lfsPointerMax is the most a Git LFS pointer file can be (git-lfs's
	// spec), so a bigger blob is never read as one.
	lfsPointerMax = 1024
	// lfsPointersMax is the most blobs read as maybe pointers in one go,
	// which bounds what lfsPointers holds.
	lfsPointersMax = 200_000
	// lfsAskOver is the size of the LFS objects to bring back from a VM
	// past which devvm asks first (clone_lfs_large).
	lfsAskOver int64 = 2 << 30
)

// lfsObject is a Git LFS object: its oid (its SHA-256, hex) and size, as
// a pointer names them.
type lfsObject struct {
	oid  string
	size int64
}

// short is o's oid as messages name it.
func (o lfsObject) short() string { return o.oid[:12] }

// shorts are objs' short oids.
func shorts(objs []lfsObject) []string {
	s := make([]string, len(objs))
	for i, o := range objs {
		s[i] = o.short()
	}
	return s
}

// lfsRel is where a repo keeps the object oid, below its common dir:
// lfs/objects/<oid[0:2]>/<oid[2:4]>/<oid>, as git-lfs does.
func lfsRel(oid string) string {
	return filepath.Join("lfs", "objects", oid[:2], oid[2:4], oid)
}

// lfsFolders are the folders on the way to oid's file, below the common
// dir, outermost first.
func lfsFolders(oid string) []string {
	objects := filepath.Join("lfs", "objects")
	return []string{"lfs", objects, filepath.Join(objects, oid[:2]), filepath.Join(objects, oid[:2], oid[2:4])}
}

// errLFSLink is lfsCheckDirs' error for a folder of .git/lfs that's a
// link or isn't a folder.
var errLFSLink = errors.New("a folder of .git/lfs is a link or isn't a folder")

// lfsCheckDirs checks that each of dirs, in a root (its lstat), is a
// folder, never a link (errLFSLink), outermost first, so a path through
// them stays in the folders git-lfs made; with mkdir, each is made first
// where it isn't there.
func lfsCheckDirs(lstat func(string) (fs.FileInfo, error), mkdir func(string) error, dirs ...string) error {
	for _, d := range dirs {
		if mkdir != nil {
			if err := mkdir(d); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
		}
		fi, err := lstat(d)
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			return fmt.Errorf("%s: %w", d, errLFSLink)
		}
	}
	return nil
}

// parseLFSPointer reads a Git LFS pointer file (git-lfs's spec): its
// first line the version, then keys, each "key value" on a line of its
// own, with an oid sha256:<hex> and a size, both once. Anything else
// isn't one.
func parseLFSPointer(b []byte) (lfsObject, bool) {
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if len(lines) < 3 || lines[0] != "version https://git-lfs.github.com/spec/v1" || !strings.HasSuffix(string(b), "\n") {
		return lfsObject{}, false
	}
	var o lfsObject
	sized := false
	for _, l := range lines[1:] {
		key, value, ok := strings.Cut(l, " ")
		if !ok {
			return lfsObject{}, false
		}
		switch key {
		case "oid":
			hexOid, ok := strings.CutPrefix(value, "sha256:")
			if !ok || o.oid != "" || len(hexOid) != 64 || strings.Trim(hexOid, "0123456789abcdef") != "" {
				return lfsObject{}, false
			}
			o.oid = hexOid
		case "size":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil || n < 0 || sized {
				return lfsObject{}, false
			}
			o.size, sized = n, true
		}
	}
	return o, o.oid != "" && sized
}

// errLFSTooMany is lfsPointers' error past lfsPointersMax blobs.
var errLFSTooMany = errors.New("too many blobs to read as Git LFS pointers")

// lfsPointers are the LFS objects that the blobs among ids (object names,
// one a line) of the repo at dir name, each once, sorted by oid: those of
// at most lfsPointerMax bytes (git cat-file --batch-check) that are a
// pointer (git cat-file --batch, parseLFSPointer). Both outputs are read
// as they come, never held whole.
func lfsPointers(ctx context.Context, dir string, ids io.Reader) ([]lfsObject, error) {
	check := hostgit.Command(ctx, "-C", dir, "cat-file", "--batch-check=%(objectname) %(objecttype) %(objectsize)")
	check.Stdin = ids
	var small []string
	err := streamOut(check, func(r *bufio.Reader) error {
		for line, err := r.ReadString('\n'); err == nil; line, err = r.ReadString('\n') {
			f := strings.Fields(line)
			if len(f) != 3 || f[1] != "blob" || !objectName.MatchString(f[0]) {
				continue
			}
			if n, err := strconv.Atoi(f[2]); err != nil || n > lfsPointerMax {
				continue
			}
			if len(small) == lfsPointersMax {
				return errLFSTooMany
			}
			small = append(small, f[0])
		}
		return nil
	})
	if err != nil || len(small) == 0 {
		return nil, err
	}
	cat := hostgit.Command(ctx, "-C", dir, "cat-file", "--batch")
	cat.Stdin = strings.NewReader(strings.Join(small, "\n") + "\n")
	found := map[string]lfsObject{}
	err = streamOut(cat, func(r *bufio.Reader) error {
		for {
			o, ok, more := readBatchPointer(r)
			if !more {
				return nil
			}
			if ok {
				found[o.oid] = o
			}
		}
	})
	if err != nil {
		return nil, err
	}
	objs := make([]lfsObject, 0, len(found))
	for _, o := range found {
		objs = append(objs, o)
	}
	slices.SortFunc(objs, func(a, b lfsObject) int { return strings.Compare(a.oid, b.oid) })
	return objs, nil
}

// readBatchPointer reads one blob of git cat-file --batch's output from
// r, at most lfsPointerMax bytes, and the pointer it is, if it's one (ok);
// more is false at the end or at anything else.
func readBatchPointer(r *bufio.Reader) (o lfsObject, ok, more bool) {
	head, err := r.ReadString('\n')
	if err != nil {
		return lfsObject{}, false, false
	}
	f := strings.Fields(head)
	if len(f) != 3 {
		return lfsObject{}, false, false
	}
	n, err := strconv.Atoi(f[2])
	if err != nil || n < 0 || n > lfsPointerMax {
		return lfsObject{}, false, false
	}
	body := make([]byte, n+1) // the content, then git's newline
	if _, err := io.ReadFull(r, body); err != nil {
		return lfsObject{}, false, false
	}
	o, ok = parseLFSPointer(body[:n])
	return o, ok, true
}

// streamOut runs cmd, giving read its standard output as it comes; what
// read leaves is read to the end, so cmd never waits on it. read's error
// comes first, then cmd's.
func streamOut(cmd *exec.Cmd, read func(*bufio.Reader) error) error {
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	rerr := read(bufio.NewReader(out))
	_, _ = io.Copy(io.Discard, out)
	if err := cmd.Wait(); rerr == nil {
		rerr = err
	}
	return rerr
}

// lfsStorage is the repo at dir's lfs.storage, the folder git-lfs keeps
// its objects in instead of .git/lfs: "" when it isn't set.
func lfsStorage(ctx context.Context, dir string) string {
	v, _ := hostGit(ctx, dir, "config", "--get", "lfs.storage")
	return v
}

// lfsSeed is what readLFS found: the objects the seed sends, read from
// common, the repo's common dir; bad, the short oids of those whose file
// isn't what its pointer names, which stay; elsewhere, whether the repo
// keeps its objects elsewhere (lfsStorage), so none go.
type lfsSeed struct {
	objs      []lfsObject
	common    string
	bad       []string
	elsewhere bool
}

// readLFS is the LFS objects the seed of the repo at dir sends: those the
// pointers among its index's blobs (ids) name (lfsPointers) that its
// common dir has, each a regular file of its pointer's size whose SHA-256
// is its oid, hashed here, before anything is sent. One the repo lacks,
// or that a link on the way would reach, isn't sent; one whose content
// isn't its oid's is named (bad).
func readLFS(ctx context.Context, dir string, ids []string) (lfsSeed, error) {
	objs, err := lfsPointers(ctx, dir, strings.NewReader(strings.Join(ids, "\n")+"\n"))
	if err != nil || len(objs) == 0 {
		return lfsSeed{}, err
	}
	if lfsStorage(ctx, dir) != "" {
		return lfsSeed{elsewhere: true}, nil
	}
	_, common, err := gitprotect.OwnRepo(dir)
	if err != nil {
		return lfsSeed{}, err
	}
	r, err := safefile.OpenRoot(common)
	if err != nil {
		return lfsSeed{}, err
	}
	defer func() { _ = r.Close() }()
	s := lfsSeed{common: common}
	for _, o := range objs {
		switch err := lfsHash(r, o, io.Discard); {
		case err == nil:
			s.objs = append(s.objs, o)
		case errors.Is(err, errLFSObject):
			s.bad = append(s.bad, o.short())
		}
	}
	return s, nil
}

// errLFSObject is the error for an LFS object that isn't what its
// pointer names.
var errLFSObject = errors.New("an LFS object isn't what its pointer names")

// lfsHash copies the object o, in the repo whose common dir is r, to w,
// failing with errLFSObject when it isn't o's size or its SHA-256 isn't
// o's oid: read as a regular file, every folder on the way one of its own
// (lfsCheckDirs), never through a link.
func lfsHash(r *safefile.Root, o lfsObject, w io.Writer) error {
	if err := lfsCheckDirs(r.Lstat, nil, lfsFolders(o.oid)...); err != nil {
		return err
	}
	f, err := r.Open(lfsRel(o.oid), safefile.Opts{})
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if f.Info.Size() != o.size {
		return errLFSObject
	}
	h := sha256.New()
	if _, err := io.CopyN(io.MultiWriter(w, h), f, o.size); err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != o.oid {
		return errLFSObject
	}
	return nil
}

// writeLFS writes the LFS objects s.objs to tw, each as lfs/<oid>, hashed
// again as it's sent (lfsHash): one that changed since readLFS fails the
// seed, which the VM's tar then takes as cut short.
func writeLFS(tw *tar.Writer, s lfsSeed) error {
	if len(s.objs) == 0 {
		return nil
	}
	r, err := safefile.OpenRoot(s.common)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	for _, o := range s.objs {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "lfs/" + o.oid, Size: o.size, Mode: 0o600}); err != nil {
			return err
		}
		if err := lfsHash(r, o, tw); err != nil {
			return err
		}
	}
	return nil
}

// fetchHead is what the last fetch into the repo at dir brought, as its
// FETCH_HEAD names it: each object, once.
func fetchHead(ctx context.Context, dir string) []string {
	p, err := hostGit(ctx, dir, "rev-parse", "--path-format=absolute", "--git-path", "FETCH_HEAD")
	if err != nil {
		return nil
	}
	b, err := safefile.Read(p, 16<<20, safefile.Opts{})
	if err != nil {
		return nil
	}
	var ids []string
	for line := range strings.Lines(string(b)) {
		if id, _, _ := strings.Cut(line, "\t"); objectName.MatchString(id) && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids
}

// lfsBase are the objects of the repo at dir's own refs, which bound the
// history whose LFS objects come back: every ref but the saved branches
// (refs/heads/saved/) and a devvm VM's remote-tracking branches
// (vmRemotes), which hold a VM's work, which git fetch devvm fetches
// without its LFS objects.
func lfsBase(ctx context.Context, dir string) []string {
	out, _ := hostGit(ctx, dir, "for-each-ref", "--format=%(objectname) %(refname)")
	vms := vmRemotes(ctx, dir)
	var base []string
	for line := range strings.Lines(out) {
		id, ref, _ := strings.Cut(strings.TrimSpace(line), " ")
		if !objectName.MatchString(id) || strings.HasPrefix(ref, "refs/heads/saved/") || slices.ContainsFunc(vms, func(p string) bool { return strings.HasPrefix(ref, p) }) {
			continue
		}
		if !slices.Contains(base, id) {
			base = append(base, id)
		}
	}
	return base
}

// hasObject reports whether the repo at dir has the object id.
func hasObject(ctx context.Context, dir, id string) bool {
	_, err := hostGit(ctx, dir, "cat-file", "-e", id)
	return err == nil
}

// lfsWanted are the LFS objects that the history of tips, past the
// repo at dir's own refs (lfsBase), names and the repo lacks (lfsHas),
// and its common dir. tips the repo hasn't got are left out, and an
// object a partial clone lacks is passed over (--missing=allow-promisor).
func lfsWanted(ctx context.Context, dir string, tips []string) ([]lfsObject, string, error) {
	objs, err := lfsHistory(ctx, dir, tips, lfsBase(ctx, dir))
	if err != nil || len(objs) == 0 {
		return nil, "", err
	}
	return lfsLacks(dir, objs)
}

// lfsLacks are those of objs that the repo at dir lacks (lfsHas), and its
// common dir.
func lfsLacks(dir string, objs []lfsObject) ([]lfsObject, string, error) {
	_, common, err := gitprotect.OwnRepo(dir)
	if err != nil {
		return nil, "", err
	}
	r, err := safefile.OpenRoot(common)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = r.Close() }()
	return slices.DeleteFunc(objs, func(o lfsObject) bool { return lfsHas(r, o) }), common, nil
}

// lfsHistory are the LFS objects that the history of tips names in the
// repo at dir, past the objects not (lfsPointers of git rev-list
// --objects): tips the repo hasn't got are left out, and an object a
// partial clone lacks is passed over (--missing=allow-promisor).
func lfsHistory(ctx context.Context, dir string, tips, not []string) ([]lfsObject, error) {
	var in strings.Builder
	for _, id := range tips {
		if objectName.MatchString(id) && hasObject(ctx, dir, id) {
			in.WriteString(id + "\n")
		}
	}
	if in.Len() == 0 {
		return nil, nil
	}
	for _, id := range not {
		in.WriteString("^" + id + "\n")
	}
	list := hostgit.Command(ctx, "-C", dir, "rev-list", "--objects", "--no-object-names", "--missing=allow-promisor", "--stdin")
	list.Stdin = strings.NewReader(in.String())
	var objs []lfsObject
	err := streamOut(list, func(r *bufio.Reader) error {
		var err error
		objs, err = lfsPointers(ctx, dir, r)
		return err
	})
	return objs, err
}

// vmLFS is the LFS objects of the clone at dir in t's VM, which devvm
// asks the VM for (lfsSend), and of the repo at dir here.
type vmLFS struct {
	e   clones
	t   *target
	dir string
}

// only are the LFS objects that the VM's clone has (list), that the repo
// here lacks, and that the history of tips since the copy names, whatever
// refs of the repo's own reach it: such as the VM's work merged into a
// branch here after a plain git fetch devvm, which brought its commits
// but none of its LFS objects (lfsWanted's past lfsBase leaves those
// out). The walk stops at what the copy set the clone's refs to (list's
// seeded) that this repo has, so neither an object the VM fetched for
// history it was given nor a large history makes it read every blob
// (lfsPointersMax); a clone without that record, an older devvm's, stops
// at the repo's own refs (lfsBase), as lfsWanted does.
func (v vmLFS) only(ctx context.Context, tips []string) ([]lfsObject, error) {
	listed, seeded, err := v.list(ctx)
	if err != nil || len(listed) == 0 {
		return nil, err
	}
	missing, _, err := lfsLacks(v.dir, listed)
	if err != nil || len(missing) == 0 {
		return nil, err
	}
	not := slices.DeleteFunc(seeded, func(id string) bool { return !hasObject(ctx, v.dir, id) })
	if len(not) == 0 {
		not = lfsBase(ctx, v.dir)
	}
	named, err := lfsHistory(ctx, v.dir, tips, not)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(missing, func(o lfsObject) bool { return !slices.Contains(named, o) }), nil
}

// lfsUnion is a and those of b it hasn't, sorted by oid.
func lfsUnion(a, b []lfsObject) []lfsObject {
	out := slices.Clone(a)
	for _, o := range b {
		if !slices.Contains(out, o) {
			out = append(out, o)
		}
	}
	slices.SortFunc(out, func(x, y lfsObject) int { return strings.Compare(x.oid, y.oid) })
	return out
}

// lfsTotal is the size of objs in all, at most math.MaxInt64.
func lfsTotal(objs []lfsObject) int64 {
	var total int64
	for _, o := range objs {
		if o.size > math.MaxInt64-total {
			return math.MaxInt64
		}
		total += o.size
	}
	return total
}

// lfsHas reports whether the repo whose common dir is r has o: a regular
// file of o's size where git-lfs keeps it, every folder on the way one of
// its own.
func lfsHas(r *safefile.Root, o lfsObject) bool {
	if lfsCheckDirs(r.Lstat, nil, lfsFolders(o.oid)...) != nil {
		return false
	}
	fi, err := r.Lstat(lfsRel(o.oid))
	return err == nil && fi.Mode().IsRegular() && fi.Size() == o.size
}

// repoRel is the repo at dir's path as messages name it: relative to t's
// project folder.
func repoRel(t *target, dir string) string {
	rel, err := filepath.Rel(t.f.Dir, dir)
	if err != nil {
		return dir
	}
	return rel
}

// logLFS puts err, a failure to bring the LFS objects of the repo at dir,
// into the run log.
func (e clones) logLFS(dir string, err error) {
	_, _ = fmt.Fprintf(e.LogWriter(), "devvm: git lfs objects of %s: %v\n", dir, err)
}

// fetchLFS brings into the repo at dir the LFS objects that the history
// of tips, what a fetch from its clone in t's VM brought, names past the
// repo's own refs, and that it lacks (lfsWanted): all at once from the
// VM's clone at the same path (vmLFS.bring), asking first when they come to
// more than lfsAskOver. Those that didn't come are named, as is a
// failure, and lost reports whether the VM's clone may have any of them,
// so a VM about to go is kept. It fails only when ctx ends, after a
// warning, or when the question is refused.
func (e clones) fetchLFS(ctx context.Context, t *target, dir string, tips []string) (lost bool, err error) {
	rel, v := repoRel(t, dir), vmLFS{e, t, dir}
	wanted, common, err := lfsWanted(ctx, dir, tips)
	if err == nil {
		var only []lfsObject
		only, err = v.only(ctx, tips)
		wanted = lfsUnion(wanted, only)
	}
	switch {
	case err != nil:
		e.logLFS(dir, err)
		e.Warn(msg.CloneLfsFailed(rel))
		return true, ctx.Err()
	case len(wanted) == 0:
		return false, nil
	case lfsStorage(ctx, dir) != "":
		e.Warn(msg.CloneLfsStorage(rel))
		return true, nil
	}
	if yes, err := e.askLarge(rel, wanted); err != nil || !yes {
		return true, err
	}
	if common == "" {
		if _, common, err = gitprotect.OwnRepo(dir); err != nil {
			e.logLFS(dir, err)
			e.Warn(msg.CloneLfsFailed(rel))
			return true, nil
		}
	}
	invalid, failed, err := v.bring(ctx, common, wanted)
	if err != nil {
		e.logLFS(dir, err)
	}
	if left := slices.Concat(invalid, failed); len(left) > 0 {
		slices.Sort(left)
		e.Warn(msg.CloneLfsKept(len(left), shown(left), rel))
	}
	if ctx.Err() != nil {
		e.Warn(msg.CloneLfsFailed(rel))
		return true, ctx.Err()
	}
	return len(failed) > 0, nil
}

// askLarge asks before objs come back from the VM, when they come to more
// than lfsAskOver; without a terminal, the question's refusal. A no is
// named, as objects that didn't come.
func (e clones) askLarge(rel string, objs []lfsObject) (bool, error) {
	total := lfsTotal(objs)
	if total <= lfsAskOver {
		return true, nil
	}
	gib := strconv.FormatFloat(float64(total)/(1<<30), 'f', 1, 64)
	yes, err := e.Confirm(msg.CloneLfsLarge(rel, gib).RefusedWith(msg.NoTerminalLfsLarge(rel, gib)))
	if err == nil && !yes {
		e.Warn(msg.CloneLfsKept(len(objs), shown(shorts(objs)), rel))
	}
	return yes, err
}

// lfsUnfetched names the LFS objects that only the clone at dir in t's VM
// has, path in the project folder, of the history of its refs ("SHA REF"
// lines) that the repo at dir has: those lfsWanted finds that the VM's
// clone has (vmLFS.have), and those of its lfs/objects that the history
// names wherever it's merged here (vmLFS.only). git fetch devvm brings
// their commits, never them.
// It reports whether there are any, or whether it couldn't tell, which is
// named too, so the VM isn't deleted without a question.
func (e clones) lfsUnfetched(ctx context.Context, t *target, dir, path string, refs []byte) bool {
	var tips []string
	for line := range strings.Lines(string(refs)) {
		sha, _, _ := strings.Cut(strings.TrimSpace(line), " ")
		tips = append(tips, sha)
	}
	v := vmLFS{e, t, dir}
	wanted, _, err := lfsWanted(ctx, dir, tips)
	var only []lfsObject
	if err == nil {
		only, err = v.only(ctx, tips)
	}
	switch {
	case err == nil && len(wanted)+len(only) == 0:
		return false
	case err == nil && lfsStorage(ctx, dir) != "":
		e.Warn(msg.CloneLfsStorage(path))
		return true
	case err == nil && len(wanted) > 0:
		wanted, err = v.have(ctx, wanted)
	}
	wanted = lfsUnion(wanted, only)
	switch {
	case err != nil:
		e.logLFS(dir, err)
		e.Warn(msg.CloneLfsFailed(path))
		return true
	case len(wanted) == 0:
		return false
	}
	e.Say(msg.CloneLfsUnfetched(len(wanted), shown(shorts(wanted)), path))
	return true
}

// lfsSend is the script that writes, for each "oid size" line on its
// stdin, the LFS object of the clone at $1: a line with the oid and its
// size, then its content, exactly that size; the oid and ! for one of
// another size; or the oid and - for one the clone lacks, or that's a link
// or not a regular file. With $2, the lines alone; with $2 list, a line
// with the name and size of each regular file below the clone's
// lfs/objects whose name is an oid's length, none followed through a
// link, and nothing read, after a "seeded SHA" line for each object of
// the clone's devvm-seed-tips, what the copy set its refs to.
const lfsSend = `: devvm-lfs-send; d=$1/.git/lfs/objects
if [ "$2" = list ]; then
  s=$1/.git/devvm-seed-tips
  if [ -f "$s" ] && [ ! -L "$s" ]; then awk '{ print "seeded", $1 }' "$s"; fi
  [ -d "$d" ] && [ ! -L "$d" ] || exit 0
  find "$d" -type f -name '????????????????????????????????????????????????????????????????' | while IFS= read -r p; do
    n=$(wc -c < "$p") && printf '%s %s\n' "${p##*/}" "$((n))"
  done
  exit 0
fi
while read -r oid want; do
  r=${oid#??}; p=$d/${oid%"$r"}/${r%"${r#??}"}/$oid
  if [ -f "$p" ] && [ ! -L "$p" ] && n=$(wc -c < "$p"); then
    n=$((n))
    if [ "$n" != "$want" ]; then printf '%s !\n' "$oid"; continue; fi
    printf '%s %s\n' "$oid" "$n"
    if [ -z "$2" ]; then head -c "$n" -- "$p" || exit 1; fi
  else
    printf '%s -\n' "$oid"
  fi
done`

// errLFSStream is the error for a stream from the VM that isn't what
// lfsSend writes.
var errLFSStream = errors.New("the VM's stream of Git LFS objects isn't what devvm asked for")

// send runs lfsSend in the VM on its clone for objs, its output to out,
// its connection carrying no agent; mode, lfsSend's $2: "" for the
// objects, "sizes" or "list".
func (v vmLFS) send(ctx context.Context, objs []lfsObject, mode string, out io.Writer) error {
	var in strings.Builder
	for _, o := range objs {
		in.WriteString(o.oid + " " + strconv.FormatInt(o.size, 10) + "\n")
	}
	argv := []string{"sh", "-c", lfsSend, "sh", v.dir}
	if mode != "" {
		argv = append(argv, mode)
	}
	return v.t.b.Exec(ctx, v.t.f.Name, argv, backend.ExecOpts{Stdin: strings.NewReader(in.String()), Stdout: out, Stderr: v.e.vmLog()})
}

// lfsLinesMax is the most of lfsSend's lines, sizes or a list, devvm
// reads: past it, the VM's command is stopped (errBounded).
const lfsLinesMax = 64 << 20

// lines is lfsSend's output in mode "sizes" or "list" for objs, at most
// limit bytes of it, the VM's command stopped past that (errBounded).
func (v vmLFS) lines(ctx context.Context, objs []lfsObject, mode string, limit int) (*bufio.Reader, error) {
	run, cancel := context.WithCancel(ctx)
	defer cancel()
	out := &bounded{max: limit, cancel: cancel}
	err := v.send(run, objs, mode, out)
	if out.over {
		return nil, errBounded
	}
	if err != nil {
		return nil, err
	}
	return bufio.NewReader(bytes.NewReader(out.Bytes())), nil
}

// list are the LFS objects the VM's clone has, by the names and sizes of
// the files in its lfs/objects (lfsSend, list): those whose name is an
// oid; and seeded, the objects the copy set the clone's refs to
// (devvm-seed-tips), each once.
func (v vmLFS) list(ctx context.Context) (objs []lfsObject, seeded []string, err error) {
	r, err := v.lines(ctx, nil, "list", lfsLinesMax)
	if err != nil {
		return nil, nil, err
	}
	for line, err := r.ReadString('\n'); err == nil; line, err = r.ReadString('\n') {
		oid, size, _ := strings.Cut(strings.TrimSuffix(line, "\n"), " ")
		if oid == "seeded" {
			if objectName.MatchString(size) && !slices.Contains(seeded, size) {
				seeded = append(seeded, size)
			}
			continue
		}
		n, perr := strconv.ParseInt(size, 10, 64)
		if perr != nil || n < 0 || len(oid) != 64 || strings.Trim(oid, "0123456789abcdef") != "" {
			continue
		}
		if o := (lfsObject{oid: oid, size: n}); !slices.Contains(objs, o) {
			objs = append(objs, o)
		}
	}
	return objs, seeded, nil
}

// have are those of objs that the VM's clone has, of their size
// (lfsSend, sizes only).
func (v vmLFS) have(ctx context.Context, objs []lfsObject) ([]lfsObject, error) {
	r, err := v.lines(ctx, objs, "sizes", len(objs)*100)
	if err != nil {
		return nil, err
	}
	var have []lfsObject
	for _, o := range objs {
		got, err := lfsHeader(r, o)
		if err != nil {
			return nil, err
		}
		if got == lfsThere {
			have = append(have, o)
		}
	}
	return have, nil
}

// bring brings objs from the VM's clone into the repo here, whose common
// dir is common, in one stream (send): each read
// into a new file in lfs/tmp, exactly its size, and moved into place only
// when its SHA-256 is its oid, every folder on the way made or checked to
// be one, never a link (lfsCheckDirs). It returns the short oids of those
// the VM's clone has nothing valid of (invalid: it lacks them, or what it
// has is of another size or not what their pointers name), and of those
// that didn't come otherwise (failed), such as every one past the point
// where the stream isn't what devvm asked for.
func (v vmLFS) bring(ctx context.Context, common string, objs []lfsObject) (invalid, failed []string, err error) {
	root, err := os.OpenRoot(common)
	if err != nil {
		return nil, shorts(objs), err
	}
	defer func() { _ = root.Close() }()
	if err := lfsCheckDirs(root.Lstat, lfsMkdir(root), "lfs", filepath.Join("lfs", "tmp")); err != nil {
		return nil, shorts(objs), err
	}
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := v.send(ctx, objs, "", pw)
		_ = pw.CloseWithError(err)
		done <- err
	}()
	invalid, failed, err = lfsReadStream(root, bufio.NewReader(pr), objs)
	_ = pr.CloseWithError(errLFSStream) // the VM's command stops, if it's still writing
	if xerr := <-done; err == nil {
		err = xerr
	}
	return invalid, failed, err
}

// lfsMkdir makes a folder in root, as git-lfs makes its own.
func lfsMkdir(root *os.Root) func(string) error {
	return func(d string) error { return root.Mkdir(d, 0o755) }
}

// lfsReadStream reads objs from r, lfsSend's output, into the common dir
// root (lfsReceive), as vmLFS.bring says, with the first error past an
// object's.
func lfsReadStream(root *os.Root, r *bufio.Reader, objs []lfsObject) (invalid, failed []string, err error) {
	for i, o := range objs {
		got, herr := lfsHeader(r, o)
		if herr != nil {
			return invalid, append(failed, shorts(objs[i:])...), herr
		}
		if got != lfsThere {
			invalid = append(invalid, o.short())
			continue
		}
		synced, rerr := lfsReceive(root, r, lfsMkdir(root), o)
		switch {
		case !synced:
			return invalid, append(failed, shorts(objs[i:])...), rerr
		case errors.Is(rerr, errLFSObject):
			invalid = append(invalid, o.short())
		case rerr != nil:
			failed, err = append(failed, o.short()), cmp.Or(err, rerr)
		}
	}
	return invalid, failed, err
}

// lfsState is what lfsSend's line says of an object in the VM's clone.
type lfsState int

const (
	lfsThere   lfsState = iota // there, of its size: its content follows
	lfsAbsent                  // not there
	lfsDiffers                 // there, of another size
)

// lfsHeader reads lfsSend's line for o: what the VM's clone has of it.
// Any line but o's, or one that names another size, fails (errLFSStream).
func lfsHeader(r *bufio.Reader, o lfsObject) (lfsState, error) {
	line, err := r.ReadSlice('\n')
	if err != nil {
		return 0, err
	}
	got, rest, ok := strings.Cut(strings.TrimSuffix(string(line), "\n"), " ")
	switch {
	case !ok || got != o.oid:
		return 0, errLFSStream
	case rest == "-":
		return lfsAbsent, nil
	case rest == "!":
		return lfsDiffers, nil
	case rest != strconv.FormatInt(o.size, 10):
		return 0, errLFSStream
	}
	return lfsThere, nil
}

// lfsReceive reads o, exactly its size, from r into a new file in the
// common dir root's lfs/tmp, then moves it into place, the folders on the
// way made where they aren't (mkdir) and checked (lfsCheckDirs), when its
// SHA-256 is o's oid (else errLFSObject); the file goes otherwise. synced
// reports whether r is past o, so the next object can be read.
func lfsReceive(root *os.Root, r io.Reader, mkdir func(string) error, o lfsObject) (synced bool, err error) {
	tmp := filepath.Join("lfs", "tmp", "devvm-"+rand.Text())
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false, err
	}
	defer func() { _ = root.Remove(tmp) }()
	h := sha256.New()
	_, err = io.CopyN(io.MultiWriter(f, h), r, o.size)
	cerr := f.Close()
	switch {
	case err != nil:
		return false, err
	case cerr != nil:
		return true, cerr
	case hex.EncodeToString(h.Sum(nil)) != o.oid:
		return true, errLFSObject
	}
	if err := lfsCheckDirs(root.Lstat, mkdir, lfsFolders(o.oid)...); err != nil {
		return true, err
	}
	return true, root.Rename(tmp, lfsRel(o.oid))
}
