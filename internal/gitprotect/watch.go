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

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/state"
)

// On hosts without locks (Linux without devvm-immutable, or a file system
// without the immutable attribute), devvm can't stop the VM changing what a
// lock would cover (paths), so it watches those paths instead: each check
// sums them, compares with the sums the last one recorded, and names what
// changed. That's after the fact: git on the host may have run it already.

// Watched is what the last watch found: a sum of each path a lock would
// cover, by its path relative to the project folder, and of each file in
// its hooks folders. Scanned is set once a check has recorded it, so the
// first check of a VM, or of one an older devvm made, records without
// reporting.
type Watched struct {
	Scanned bool              `json:"scanned,omitempty"`
	Sums    map[string]string `json:"sums,omitempty"`
}

// The watch's bounds: hooks folders and configs are small, so a folder
// with more entries, or a file larger, than these is the VM's doing, and
// is summed by what's cheap (its entries' count, its size) past them.
const (
	maxWatchEntries = 1000     // entries of one hooks folder
	maxWatchBytes   = 16 << 20 // bytes of one file
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

// watch sums what a lock would cover, names what changed since the last
// check, once there was one, and records the sums.
func (w watcher) watch(ctx context.Context, vm *state.VM, was Watched) error {
	now := Watched{Scanned: true, Sums: w.sums(ctx)}
	if was.Scanned {
		if changed := changedPaths(was.Sums, now.Sums); len(changed) > 0 {
			w.Warn(msg.GitChanged(strings.Join(changed, ", "), vm.Name()))
		}
	}
	return record(ctx, vm, func(g *InVM) { g.Watched = now })
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

// sums sums each path a lock would cover (paths), and each file in its
// hooks folders, by path relative to the project folder. It never reads
// through a symlink: a symlink is summed by where it points, and a path
// under one by that alone.
func (w watcher) sums(ctx context.Context) map[string]string {
	out := map[string]string{}
	for _, path := range w.paths(ctx, w.embedded(ctx)) {
		fi, err := os.Lstat(path)
		switch {
		case err != nil:
			// Missing: absent from the sums, so one that appears is named.
		case fi.Mode()&fs.ModeSymlink == 0 && !w.realInDir(path):
			out[relTo(w.Dir, path)] = "under a symlink"
		case fi.IsDir():
			maps.Copy(out, sumHooks(w.Dir, path))
		default:
			out[relTo(w.Dir, path)] = sumOf(path, fi)
		}
	}
	return out
}

// sumHooks sums each entry of a hooks folder below it, by its path
// relative to the project folder top, git's samples apart (git never
// runs them), and at most maxWatchEntries: past those, the folder is
// summed by how many it has.
func sumHooks(top, dir string) map[string]string {
	out := map[string]string{}
	n := 0
	_ = filepath.WalkDir(dir, func(path string, e fs.DirEntry, err error) error {
		if err != nil || path == dir || e.IsDir() {
			return nil //nolint:nilerr // unreadable: skipped, like find
		}
		if n++; n > maxWatchEntries {
			return fs.SkipAll
		}
		if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".sample") {
			return nil
		}
		if fi, err := e.Info(); err == nil {
			out[relTo(top, path)] = sumOf(path, fi)
		}
		return nil
	})
	if n > maxWatchEntries {
		out[relTo(top, dir)] = "more than " + strconv.Itoa(maxWatchEntries) + " entries"
	}
	return out
}

// relTo is a path in the project folder top relative to it.
func relTo(top, path string) string { return strings.TrimPrefix(path, top+"/") }

// sumOf sums what's at path, which fi (from Lstat) describes: a regular
// file by its contents, read without following a symlink, a symlink by
// where it points, and anything else by its type.
func sumOf(path string, fi fs.FileInfo) string {
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		link, _ := os.Readlink(path)
		return "symlink " + link
	case !fi.Mode().IsRegular():
		return "type " + fi.Mode().Type().String()
	case fi.Size() > maxWatchBytes:
		return "size " + strconv.FormatInt(fi.Size(), 10)
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
