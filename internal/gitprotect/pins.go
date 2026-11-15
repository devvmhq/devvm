package gitprotect

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/safefile"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/vmname"
)

// On a Linux host, a start pins what a lock covers in the namespace the
// VM's QEMU runs in (Binds). A pin covers one name of a file, so a file
// with another name (a hard link) can't be pinned, and the start is
// refused; a file that git on the host renames over a pinned one is the
// guest's to change until it's pinned again, which the namespace records
// (backend.PinEvent) and ReportPinEvents names, as the watch names what
// changed. A running VM's pins are checked as it runs (PinsMissing).

// Pins are what the start of the VM vm pins for its locks (Binds), in the
// folder's own repo and each nested repo its state records protecting,
// none unless its mode locks (InVM.locks), each relative to the project folder and
// a real path in it (realInDir), as lock takes them: .git, when it's a
// folder, and the git dirs a lock covers besides (fixed), which the guest
// can write in but not rename or remove, and each path a lock covers that
// is there (readOnly), which it can't change. lock made a missing hooks
// folder and config.worktree while the VM was stopped. A file to pin, or
// a hook in a hooks folder to pin, with another name is refused
// (git_pin_linked): its other name would stay the guest's to change.
func (p *Protector) Pins(ctx context.Context, vm *state.VM) (fixed, readOnly []string, err error) {
	g, _ := Of(vm)
	if !g.locks() {
		return nil, nil, nil
	}
	return p.pinsOf(vm.Name(), scanAll(ctx, p.recorded(g.Made)))
}

// pinsOf is what to pin of the repos snaps scanned, as Pins has it, for
// the VM vm.
func (t tree) pinsOf(vm vmname.VM, snaps []snapshot) (fixed, readOnly []string, err error) {
	for _, s := range snaps {
		for _, d := range append([]string{s.git}, s.dirs...) {
			if isDir(d) && t.realInDir(d) {
				fixed = append(fixed, relTo(t.Dir, d))
			}
		}
		for _, path := range s.paths {
			if _, err := os.Lstat(path); err != nil || !t.realInDir(path) {
				continue
			}
			if l, ok := linkedFile(path); ok {
				return nil, nil, msg.GitPinLinked(l, vm)
			}
			readOnly = append(readOnly, relTo(t.Dir, path))
		}
	}
	return fixed, readOnly, nil
}

// recordedPins is what a running VM's start pinned of the nested repos
// made records, as far as their records tell without scanning them: each
// git dir, fixed, and its hooks and config, read-only.
func (t tree) recordedPins(made Made) (fixed, readOnly []string) {
	for _, rel := range slices.Sorted(maps.Keys(made.Repos)) {
		if !filepath.IsLocal(rel) {
			continue
		}
		for _, d := range append([]string{t.repoAt(rel).git()}, t.abs(made.Repos[rel].GitDirs)...) {
			if !isDir(d) || !t.realInDir(d) {
				continue
			}
			fixed = append(fixed, relTo(t.Dir, d))
			for _, f := range []string{"hooks", "config"} {
				if _, err := os.Lstat(filepath.Join(d, f)); err == nil {
					readOnly = append(readOnly, relTo(t.Dir, filepath.Join(d, f)))
				}
			}
		}
	}
	return fixed, readOnly
}

// linkedFile is path, or the file in it, a folder, that has another name
// (platform.Links), and whether there's one.
func linkedFile(path string) (string, bool) {
	fi, err := os.Lstat(path)
	switch {
	case err != nil:
		return "", false
	case fi.Mode().IsRegular():
		return path, platform.Links(fi) > 1
	case !fi.IsDir():
		return "", false
	}
	entries, _ := os.ReadDir(path)
	for _, e := range entries {
		if fi, err := e.Info(); err == nil && fi.Mode().IsRegular() && platform.Links(fi) > 1 {
			return filepath.Join(path, e.Name()), true
		}
	}
	return "", false
}

// ReportPinEvents names, once the VM vm has stopped, what its runs since
// the last check recorded in file of its pins (backend.Pins.Events): each
// path that a git command on this machine replaced and the namespace
// pinned again, which the guest could change in between, each that got
// another name, and each that couldn't be pinned again, one line for a
// path however often it happened. A record it can't trust (unreadable, a
// line that isn't an event, an event the run couldn't add, or a file
// that doesn't start as a run does) is named too (git_pins_unrecorded),
// as the guest may have changed what isn't in it. Then it removes the
// file, unless it couldn't read it.
func (p *Protector) ReportPinEvents(file string, vm vmname.VM) {
	f, err := safefile.Open(file, safefile.Opts{})
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		p.Warn(msg.GitPinsUnrecorded(vm).Because(err))
		return
	}
	events, whole := readPinEvents(f.File)
	_ = f.Close()
	for _, e := range events {
		path := filepath.Join(p.Dir, e.Path)
		switch e.What {
		case platform.PinAgain:
			p.Warn(msg.GitPinAgain(path, vm))
		case platform.PinLinked:
			p.Warn(msg.GitPinLinkedRun(path, vm))
		default:
			p.Warn(msg.GitPinLost(path, vm))
		}
	}
	if !whole {
		p.Warn(msg.GitPinsUnrecorded(vm))
	}
	if err := os.Remove(file); err != nil && !errors.Is(err, fs.ErrNotExist) {
		p.Warn(msg.GitPinEventsKept(file).Because(err))
	}
}

// readPinEvents is the events file f's pin changes, each path's and kind's
// once, in the order first seen, and whether f records them all: it starts
// with a run's backend.PinKeeping line, and has only events, none of them
// backend.PinUnrecorded.
func readPinEvents(f *os.File) (events []backend.PinEvent, whole bool) {
	seen := map[backend.PinEvent]bool{}
	whole = true
	sc := bufio.NewScanner(f)
	for n := 0; sc.Scan(); n++ {
		var e backend.PinEvent
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.What == backend.PinUnrecorded {
			whole = false
			continue
		}
		if e.What == backend.PinKeeping {
			continue
		}
		if n == 0 {
			whole = false
		}
		if !seen[e] {
			seen[e] = true
			events = append(events, e)
		}
	}
	return events, whole && sc.Err() == nil
}

// PinsMissing is what of the running VM vm's pins its backend doesn't
// have, readOnly and mounted being the paths its process sees through a
// read-only mount, and through any mount, now (backend.Run's; nil when the
// backend can't tell, which misses nothing): each path of .git's and of
// the git dirs it locked at its last check that's there, the files and
// hooks folders not read-only, and .git and the git dirs not mounted, by
// its path on this machine. A VM started without devvm's pins (limactl
// start) misses all of them.
func (p *Protector) PinsMissing(ctx context.Context, vm *state.VM, readOnly, mounted []string) []string {
	if readOnly == nil || mounted == nil {
		return nil
	}
	g, _ := Of(vm)
	if !g.locks() {
		return nil
	}
	// The folder's own repo scanned, the nested ones as recorded: a
	// launch into a running VM scans no nested repo.
	fixed, want, err := p.pinsOf(vm.Name(), scanAll(ctx, []tree{p.tree}))
	if err != nil {
		return nil
	}
	nf, nr := p.recordedPins(g.Made)
	fixed, want = append(fixed, nf...), append(want, nr...)
	known := append([]string{".git"}, g.Made.GitDirs...)
	for rel, r := range g.Made.Repos {
		known = append(known, filepath.Join(rel, ".git"))
		known = append(known, r.GitDirs...)
	}
	mine := func(rel string) bool {
		return slices.ContainsFunc(known, func(d string) bool { return rel == d || strings.HasPrefix(rel, d+"/") })
	}
	var out []string
	missing := func(rels, have []string) {
		for _, rel := range rels {
			if abs := filepath.Join(p.Dir, rel); mine(rel) && !slices.Contains(have, abs) {
				out = append(out, abs)
			}
		}
	}
	missing(fixed, mounted)
	missing(want, readOnly)
	return out
}
