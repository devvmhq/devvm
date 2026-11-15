package gitprotect

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/devvmhq/devvm/internal/hostsafety"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/vmname"
)

// A folder can hold git repos of its own below its own repo, or without
// one: a folder of repos. The walk of a stopped VM's folder finds them
// (Protector.Nested), and a VM whose protection covers them (Made.Nested)
// protects each as it does the folder's own (repos.go). Those it doesn't
// cover are named, as the VM can change their hooks and config, and git
// on this machine runs them when the user runs git there: a VM's nested
// repos past the walk's bound, those whose .git leads to a git dir in the
// folder that nothing locks, and all of them for a VM an older devvm made,
// whose protection covers its own repo alone until devvm git-protect on.

// nested records the repos nested in the folder that the walk found
// (p.Nested) and the protection, as of the scans snaps, doesn't cover
// (uncovered), and names them where the host locks: all of them when the
// protection was just set up (a new VM, devvm git-protect), those not
// named before at a check. A VM whose protection doesn't cover its nested
// repos is told once that devvm git-protect on protects them
// (git_nested_protectable). On a host without locks, WarnUnprotected
// names them at each launch instead. Without a walk, nothing changes.
func (p *Protector) nested(ctx context.Context, vm *state.VM, snaps []snapshot, w why) error {
	if p.Nested == nil {
		return nil
	}
	n := p.Nested
	inside, outside := uncovered(p.Dir, snaps, n.Dirs)
	un := slices.Clone(inside)
	for _, o := range outside {
		un = append(un, o[0])
	}
	was, _ := Of(vm)
	// Named once each: at a check, those not named before.
	fresh := func(now, before []string) []string {
		if w != whyCheck {
			return now
		}
		return slices.DeleteFunc(slices.Clone(now), func(d string) bool { return slices.Contains(before, d) })
	}
	notice := p.Locker.Locks() && w == whyCheck && !was.Made.Nested && !was.NestedNoticed && len(un) > 0
	switch {
	case notice:
		p.warnNested(un, func(l string, c int) msg.M {
			if c == 0 {
				return msg.GitNestedProtectable(l, vm.Name())
			}
			return msg.GitNestedProtectableMore(c, l, vm.Name())
		})
	case p.Locker.Locks():
		p.warnNested(fresh(inside, was.Nested), unprotected(vm.Name()))
		for _, o := range outside {
			if w != whyCheck || !slices.Contains(was.Nested, o[0]) {
				p.Warn(msg.GitNestedGitdirOutside(o[1], o[0]))
			}
		}
	}
	bare := p.bare(snaps, n)
	p.warnFound(vm.Name(), was, w, fresh(bare, was.Bare))
	if p.Locker.Locks() {
		p.warnPast(vm.Name(), n.More, n.Past)
	}
	return record(ctx, vm, func(g *InVM) {
		g.Nested, g.NestedMore, g.NestedPast = un, n.More, n.Past
		g.NestedNoticed = g.NestedNoticed || notice
		g.Bare, g.Skipped, g.Devices = bare, n.Skipped, n.Devices
	})
}

// bare is the folders shaped like git dirs that the walk found (n.Bare)
// but a repo's: not a git dir a scan in snaps locks, as a repo's own or a
// submodule's, or one a nested repo's .git leads to, such as a separate
// git dir, which the repo's own warnings name.
func (t tree) bare(snaps []snapshot, n *hostsafety.Repos) []string {
	var known []string
	for _, s := range snaps {
		known = append(known, s.covered...)
		for _, d := range s.emb.inside {
			known = append(known, resolved(d))
		}
	}
	for _, rel := range n.Dirs {
		known = append(known, resolved(repoGitDir(filepath.Join(t.Dir, rel))))
	}
	return slices.DeleteFunc(slices.Clone(n.Bare), func(rel string) bool {
		r := resolved(filepath.Join(t.Dir, rel))
		return r != "" && slices.Contains(known, r)
	})
}

// warnFound names, on any host, what the walk found that devvm doesn't
// protect: the folders shaped like git dirs in bare, and the repos it
// counted where it doesn't look for them and the folders on other file
// systems, all of them when the protection was just set up (w), and at a
// check when the count changed from was's.
func (p *Protector) warnFound(vm vmname.VM, was InVM, w why, bare []string) {
	n := p.Nested
	if len(bare) > 0 {
		l, _ := listed(bare)
		p.Warn(msg.GitNestedBare(l, vm))
	}
	if n.Skipped > 0 && (w != whyCheck || n.Skipped != was.Skipped) {
		p.Warn(msg.GitNestedSkipped(n.Skipped, vm))
	}
	if n.Devices > 0 && (w != whyCheck || n.Devices != was.Devices) {
		p.Warn(msg.GitNestedDevices(n.Devices, vm))
	}
}

// warnPast names the nested repos past the walk's bound, more of them,
// the first of them past: at every check, as the VM could keep their
// count and change which they are.
func (p *Protector) warnPast(vm vmname.VM, more int, past []string) {
	if more > 0 {
		l, _ := listed(past)
		p.Warn(msg.GitNestedPastBound(more, l, vm))
	}
}

// unprotected is the warning that names the nested repos l, and n more,
// that the VM vm's protection doesn't cover.
func unprotected(vm vmname.VM) func(l string, n int) msg.M {
	return func(l string, n int) msg.M {
		if n == 0 {
			return msg.GitNestedUnprotected(l, vm)
		}
		return msg.GitNestedUnprotectedMore(n, l, vm)
	}
}

// warnNested names nested repos, the folders rel to the project folder,
// at most maxListed of them, in the warning m makes of them and how many
// more there are; none, nothing.
func (p *Protector) warnNested(rel []string, m func(l string, n int) msg.M) {
	if len(rel) == 0 {
		return
	}
	l, more := listed(rel)
	p.Warn(m(l, more))
}

// programs is what in the .git the scan s found runs programs on this
// machine when git runs there: each key of its config that names a
// program, an include, or an alias that runs a shell command (! first),
// as git lists it, and each hook in its hooks folder that isn't one of
// git's samples, as hooks/NAME.
func programs(ctx context.Context, s snapshot) []string {
	var out []string
	g, common := s.git, s.git
	if d := repoGitDir(s.tree.Root); d != "" {
		g, common = d, d // where a .git file or symlink leads
	}
	if c, ok := commonOf(g); ok {
		common = c // where git reads the config, through a commondir
	}
	list := git(ctx, "config", "--file", filepath.Join(common, "config"), "--list", "-z")
	for rec := range bytes.SplitSeq(list, []byte{0}) {
		k, v, _ := bytes.Cut(rec, []byte{'\n'})
		if key := string(k); runsProgram(key, string(v)) && !slices.Contains(out, key) {
			out = append(out, key)
		}
	}
	var entries []os.DirEntry
	if f, err := os.Open(filepath.Join(common, "hooks")); err == nil { //nolint:gosec,forbidigo // a folder, for its first names alone: no file in it is read
		entries, _ = f.ReadDir(maxWatchEntries)
		_ = f.Close()
	}
	slices.SortFunc(entries, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	for _, e := range entries {
		if !e.IsDir() && !strings.HasSuffix(e.Name(), ".sample") {
			out = append(out, "hooks/"+e.Name())
		}
	}
	return out
}

// runsProgram reports whether the config key, as git lists it (its
// section and name in lowercase), with value v, makes git run a program,
// or read another config: what git on this machine could run of a repo
// the VM made.
func runsProgram(key, v string) bool {
	section, name, _ := strings.Cut(key, ".")
	sub, last := "", name
	if i := strings.LastIndex(name, "."); i >= 0 {
		sub, last = name[:i], name[i+1:]
	}
	switch {
	case section == "alias":
		return strings.HasPrefix(strings.TrimSpace(v), "!")
	case section == "include" || section == "includeif":
		return last == "path"
	case section == "filter":
		return slices.Contains([]string{"clean", "smudge", "process"}, last)
	case section == "diff" && sub != "":
		return last == "textconv" || last == "command"
	case section == "merge" && sub != "":
		return last == "driver"
	case section == "gpg":
		return last == "program"
	}
	return slices.Contains([]string{
		"core.fsmonitor", "core.hookspath", "core.sshcommand", "core.pager", "core.editor", "core.askpass",
		"sequence.editor", "diff.external", "credential.helper", "uploadpack.packobjectshook",
	}, key) || section == "credential" && last == "helper"
}

// repoGitDir returns the git dir of the repo whose top folder is dir,
// resolved: its .git folder, the git dir its .git file names (gitdirOf),
// or where its .git symlink leads; "" when there's none.
func repoGitDir(dir string) string {
	g := filepath.Join(dir, ".git")
	fi, err := os.Lstat(g)
	switch {
	case err != nil:
		return ""
	case fi.IsDir():
		return g
	case fi.Mode().IsRegular():
		return gitdirOf(g, dir)
	}
	r, err := filepath.EvalSymlinks(g)
	if err != nil || !isDir(r) {
		return ""
	}
	return r
}
