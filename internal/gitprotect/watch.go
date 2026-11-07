package gitprotect

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/devvmhq/devvm/internal/hostgit"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/state"
)

// On hosts without locks (Linux without devvm-immutable, or a file system
// without the immutable attribute), devvm can't stop the VM changing what a
// lock would cover (paths), so it watches those paths instead: each check
// reports what a lock can't stop, as protect does without locking, sums
// them, compares with the sums the last one recorded, and names what
// changed. That's after the fact: git on the host may have run it
// already, and the VM may have put it back before it stopped.

// Watched is what the last watch found: a sum of each path a lock would
// cover, by its path relative to the project folder, of each file in its
// hooks folders, of .git/commondir, and of what the config hands over to
// the project folder (includes, core.hooksPath). Scanned is set once a
// check has recorded it, so the first check of a VM, or of one an older
// devvm made, records without reporting.
type Watched struct {
	Scanned bool              `json:"scanned,omitempty"`
	Sums    map[string]string `json:"sums,omitempty"`
}

// The watch's bounds: hooks folders and configs are small, so a folder
// with more entries, or a file larger, than these is the VM's doing, and
// is summed by what's cheap (its entries' count, its size, its inode and
// change time) past them, and named at each check. A warning names at most
// maxListed paths, and the rest by their count.
const (
	maxWatchEntries = 1000     // entries of one hooks folder
	maxWatchBytes   = 16 << 20 // bytes of one file
	maxWatchTotal   = 2000     // sums of one watch
	maxListed       = 10       // paths one warning names
)

// WarnUnprotected warns, at each launch of the VM vm on a host without
// locks, that it can change the hooks and config of the folder's .git,
// which git on this machine runs, and why the host can't lock them
// (unprotected); on a host with locks, that a VM made where devvm
// couldn't lock (off, not chosen) can have them now. It doesn't when the
// folder has no .git, or the user chose off (Chosen), which says they
// know.
func (p *Protector) WarnUnprotected(vm *state.VM) {
	g, _ := Of(vm)
	switch {
	case g.Mode == Clone || g.Mode == Off && g.Chosen || !exists(p.git()):
	case !p.CanLock:
		p.Warn(unprotected(p.Unable, vm.Name()))
	case g.Mode == Off:
		p.Warn(msg.GitProtectAvailable(vm.Name()))
	}
}

// watcher is a protector's watch, on a host without locks.
type watcher struct{ *Protector }

// check is a check of a stopped VM's .git on a host without locks: what a
// lock can't stop, reported as protect reports it, without locking (a VM
// whose protection was never set up reports as a new VM does), then the
// watch.
func (w watcher) check(ctx context.Context, vm *state.VM, g InVM) error {
	why := whyCheck
	if !g.Made.Protected {
		why = whyNew
	}
	made, err := w.protect(ctx, g, why, watching)
	if err != nil {
		return err
	}
	if err := record(ctx, vm, func(g *InVM) { g.Made = made }); err != nil {
		return err
	}
	return w.watch(ctx, vm, g.Watched)
}

// watch sums what a lock would cover, names what changed since the last
// check, once there was one, and what it can't fully watch, at every
// check, and records the sums.
func (w watcher) watch(ctx context.Context, vm *state.VM, was Watched) error {
	s := w.sums(ctx)
	now := Watched{Scanned: true, Sums: s.sums}
	if was.Scanned {
		if changed := changedPaths(was.Sums, now.Sums); len(changed) > 0 {
			l, more := listed(changed)
			if more == 0 {
				w.Warn(msg.GitChanged(l, vm.Name()))
			} else {
				w.Warn(msg.GitChangedMore(more, l, vm.Name()))
			}
		}
	}
	if len(s.blind) > 0 {
		if l, more := listed(s.blind); more == 0 {
			w.Warn(msg.GitWatchBlind(l))
		} else {
			w.Warn(msg.GitWatchBlindMore(more, l))
		}
	}
	return record(ctx, vm, func(g *InVM) { g.Watched = now })
}

// listed is paths as a warning names them: at most maxListed, joined, and
// how many more there are.
func listed(paths []string) (string, int) {
	if len(paths) <= maxListed {
		return strings.Join(paths, ", "), 0
	}
	return strings.Join(paths[:maxListed], ", "), len(paths) - maxListed
}

// changedPaths returns the paths whose sums differ between was and now,
// those added or gone included, sorted.
func changedPaths(was, now map[string]string) []string {
	var out []string
	for path, sum := range now {
		if was[path] != sum {
			out = append(out, path)
		}
	}
	for path := range was {
		if _, ok := now[path]; !ok {
			out = append(out, path)
		}
	}
	slices.Sort(out)
	return out
}

// sums sums each path a lock would cover (paths), each file in its hooks
// folders, .git/commondir, there or not, and what the config hands over
// to the project folder (watchedDelegations), by path relative to the
// project folder. It never reads through a symlink that leads out of the
// project folder.
func (w watcher) sums(ctx context.Context) summer {
	s := summer{top: w.Dir, real: w.realInDir, sums: map[string]string{}}
	emb := w.embedded(ctx)
	todo := w.paths(ctx, emb)
	if g := w.git(); !isFile(g) {
		todo = append(todo, filepath.Join(g, "commondir"))
	}
	todo = append(todo, w.watchedDelegations(ctx, emb)...)
	done := map[string]bool{}
	for _, path := range todo {
		if !done[path] {
			done[path] = true
			s.add(path)
		}
	}
	slices.Sort(s.blind)
	s.blind = slices.Compact(s.blind)
	return s
}

// watchedDelegations are the files and folders in the project folder
// that the config hands over to git: include.path's and includeIf's
// files (delegations), and the folder core.hooksPath names, whose hooks
// git runs in place of .git/hooks.
func (w watcher) watchedDelegations(ctx context.Context, emb embeds) []string {
	var out []string
	for _, d := range w.delegations(ctx, emb) {
		if d.kind == "include" && d.path != "" {
			out = append(out, d.path)
		}
	}
	if hp := gitConfig(ctx, filepath.Join(w.git(), "config"), "core.hooksPath"); hp != "" && w.hooksPathInside(hp) {
		hp = expandHome(w.Home, hp)
		if !filepath.IsAbs(hp) {
			hp = filepath.Join(w.Dir, hp)
		}
		out = append(out, filepath.Clean(hp))
	}
	return out
}

// summer sums a watch's paths, bounded, and keeps those it can't watch
// fully (blind): a symlink or a path under one, a file past maxWatchBytes,
// a folder past maxWatchEntries, a path past maxWatchTotal.
type summer struct {
	top   string // the project folder
	real  func(path string) bool
	sums  map[string]string
	blind []string
}

// add sums what's at path, if anything: a missing path is absent from the
// sums, so one that appears is named.
func (s *summer) add(path string) {
	rel := relTo(s.top, path)
	fi, err := os.Lstat(path)
	switch {
	case err != nil:
	case fi.Mode()&fs.ModeSymlink != 0:
		s.put(rel, sumOf(path, fi)+s.within(path))
		s.blind = append(s.blind, rel)
	case !s.real(path):
		s.put(rel, "under a symlink"+s.within(path))
		s.blind = append(s.blind, rel)
	case fi.IsDir():
		s.hooks(path)
	default:
		s.file(rel, path, fi)
	}
}

// put records a sum, or past maxWatchTotal, the path as one it can't
// watch.
func (s *summer) put(rel, sum string) {
	if len(s.sums) >= maxWatchTotal {
		s.blind = append(s.blind, rel)
		return
	}
	s.sums[rel] = sum
}

// file sums a regular file, or anything else that isn't a folder or a
// symlink, by its type.
func (s *summer) file(rel, path string, fi fs.FileInfo) {
	if fi.Mode().IsRegular() && fi.Size() > maxWatchBytes {
		s.blind = append(s.blind, rel)
	}
	s.put(rel, sumOf(path, fi))
}

// within is what a symlink, or a path under one, holds, when it leads to
// a file or folder in the project folder, which the VM can change: its
// sum after ", ". A link out of the project folder isn't followed.
func (s *summer) within(path string) string {
	r, err := filepath.EvalSymlinks(path)
	if err != nil || !paths.Below(r, s.top) {
		return ""
	}
	fi, err := os.Lstat(r)
	switch {
	case err != nil:
		return ""
	case fi.IsDir():
		inner := summer{top: s.top, real: s.real, sums: map[string]string{}}
		inner.hooks(r)
		h := sha256.New()
		for _, k := range slices.Sorted(maps.Keys(inner.sums)) {
			h.Write([]byte(strconv.Itoa(len(k)) + ":" + k + "=" + inner.sums[k] + "\n"))
		}
		return ", folder " + hex.EncodeToString(h.Sum(nil))
	}
	return ", " + sumOf(r, fi)
}

// hooks sums each entry of a hooks folder below it, git's samples apart
// (git never runs them): first the entries at its top named as git's
// hooks (hostgit.HookEvents), which git runs, then the rest, at most
// maxWatchEntries in all. Past those, the folder is summed by its count,
// inode and change time, and named as one it can't watch.
func (s *summer) hooks(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	events := hostgit.HookEvents()
	slices.SortStableFunc(entries, func(a, b fs.DirEntry) int {
		return boolOrder(!slices.Contains(events, a.Name())) - boolOrder(!slices.Contains(events, b.Name()))
	})
	n := 0
	var sub []string
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		switch {
		case e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".sample"):
			continue
		case e.IsDir():
			sub = append(sub, path)
			continue
		}
		if n++; n > maxWatchEntries {
			s.over(dir)
			return
		}
		s.entry(path)
	}
	for _, d := range sub {
		err := filepath.WalkDir(d, func(path string, e fs.DirEntry, err error) error {
			if err != nil || e.IsDir() || e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".sample") {
				return nil //nolint:nilerr // unreadable: skipped, like find
			}
			if n++; n > maxWatchEntries {
				return fs.SkipAll
			}
			s.entry(path)
			return nil
		})
		if err != nil || n > maxWatchEntries {
			s.over(dir)
			return
		}
	}
}

// entry sums one entry of a hooks folder.
func (s *summer) entry(path string) {
	fi, err := os.Lstat(path)
	if err != nil {
		return
	}
	rel := relTo(s.top, path)
	if fi.Mode()&fs.ModeSymlink != 0 {
		s.put(rel, sumOf(path, fi)+s.within(path))
		s.blind = append(s.blind, rel)
		return
	}
	s.file(rel, path, fi)
}

// over sums a hooks folder past maxWatchEntries by its count, inode and
// change time, and names it as one it can't watch.
func (s *summer) over(dir string) {
	rel := relTo(s.top, dir)
	sum := "more than " + strconv.Itoa(maxWatchEntries) + " entries"
	if fi, err := os.Lstat(dir); err == nil {
		sum += ", " + platform.ChangeID(fi)
	}
	s.put(rel, sum)
	s.blind = append(s.blind, rel)
}

func boolOrder(b bool) int {
	if b {
		return 1
	}
	return 0
}

// relTo is a path in the project folder top relative to it.
func relTo(top, path string) string { return strings.TrimPrefix(path, top+"/") }

// sumOf sums what's at path, which fi (from Lstat) describes: a regular
// file by its contents, read without following a symlink, past
// maxWatchBytes by its size, inode and change time (which the VM can't set
// back), a symlink by where it points, and anything else by its type.
func sumOf(path string, fi fs.FileInfo) string {
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		link, _ := os.Readlink(path)
		return "symlink " + link
	case !fi.Mode().IsRegular():
		return "type " + fi.Mode().Type().String()
	case fi.Size() > maxWatchBytes:
		return "size " + strconv.FormatInt(fi.Size(), 10) + ", " + platform.ChangeID(fi)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|platform.NoBlock|platform.NoFollow, 0) //nolint:gosec // the project's own, not through a symlink
	if err != nil {
		return "unreadable"
	}
	defer func() { _ = f.Close() }()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return "unreadable"
	}
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, maxWatchBytes)); err != nil {
		return "unreadable"
	}
	return "sha256 " + hex.EncodeToString(h.Sum(nil))
}
