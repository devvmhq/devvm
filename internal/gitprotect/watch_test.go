package gitprotect

import (
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// linuxFixture is a fixture on a host without locks, its VM's mode off as
// such a host gives a new VM, not chosen.
func linuxFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t, "p")
	f.p.CanLock = false
	if err := f.newVM(false); err != nil {
		t.Fatal(err)
	}
	return f
}

// check runs a check of the stopped VM, and returns what it warned.
func (f *fixture) check() []string {
	f.t.Helper()
	f.warns.Reset()
	if err := f.p.Check(f.t.Context(), f.vm, false); err != nil {
		f.t.Fatal(err)
	}
	return f.warns.Names()
}

// Each launch on a host without locks warns that the VM can change .git,
// unless there's no .git, the user chose off, or it's a --clone VM; a
// host with locks never does.
func TestWarnUnprotected(t *testing.T) {
	f := linuxFixture(t)
	mkdirs(t, f.path(".git/hooks"))
	want := []string{"git_unprotected vm=" + f.vm.Name()}
	f.p.WarnUnprotected(f.vm)
	if !slices.Equal(f.warns.Names(), want) {
		t.Errorf("off by the host: %q", f.warns.Names())
	}
	f.warns.Reset()
	if err := os.RemoveAll(f.path(".git")); err != nil {
		t.Fatal(err)
	}
	f.p.WarnUnprotected(f.vm)
	if len(f.warns.Names()) > 0 {
		t.Errorf("no .git: %q", f.warns.Names())
	}
	mkdirs(t, f.path(".git/hooks"))
	for _, g := range []InVM{{Mode: Off, Chosen: true}, {Mode: Clone}} {
		if err := record(t.Context(), f.vm, func(s *InVM) { *s = g }); err != nil {
			t.Fatal(err)
		}
		f.p.WarnUnprotected(f.vm)
		if len(f.warns.Names()) > 0 {
			t.Errorf("%+v: %q", g, f.warns.Names())
		}
	}
	// devvm git-protect off, on such a host, is the user's choice.
	if err := record(t.Context(), f.vm, func(s *InVM) { *s = InVM{Mode: Off} }); err != nil {
		t.Fatal(err)
	}
	if err := f.p.Set(t.Context(), f.vm, Off); err != nil || !f.section().Chosen {
		t.Errorf("devvm git-protect off: %v, chosen %t", err, f.section().Chosen)
	}
}

// On a host that can lock, a VM made where devvm couldn't (off, not
// chosen) keeps its mode and its watch, and each launch says it can be
// locked now; one the user turned off or on is quiet.
func TestWarnProtectAvailable(t *testing.T) {
	needGit(t)
	f := linuxFixture(t)
	f.git(f.dir, "init", "-q")
	_ = f.check() // the repo, made since the VM, recorded
	f.warns.Reset()
	f.p.CanLock = true
	f.p.WarnUnprotected(f.vm)
	if want := []string{"git_protect_available vm=" + f.vm.Name()}; !slices.Equal(f.warns.Names(), want) {
		t.Errorf("off by the host: %q", f.warns.Names())
	}
	if err := os.WriteFile(f.path(".git/hooks/pre-commit"), []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := f.check(); !slices.Equal(got, []string{"git_changed paths=.git/hooks/pre-commit vm=" + f.vm.Name()}) || len(f.flags) > 0 {
		t.Errorf("its check: %q, locks %q", got, f.flags)
	}
	for _, g := range []InVM{{Mode: Off, Chosen: true}, {Mode: On, Made: Made{Protected: true}}} {
		if err := record(t.Context(), f.vm, func(s *InVM) { *s = g }); err != nil {
			t.Fatal(err)
		}
		f.warns.Reset()
		f.p.WarnUnprotected(f.vm)
		if len(f.warns.Names()) > 0 {
			t.Errorf("%+v: %q", g, f.warns.Names())
		}
	}
}

// A new VM's first sums are recorded as it's made, without reporting;
// each later check names what changed since, added and gone included,
// and nothing when nothing did. A running VM's launch checks nothing.
func TestWatch(t *testing.T) {
	needGit(t)
	f := newFixture(t, "p")
	f.git(f.dir, "init", "-q")
	write(t, f.path(".git/hooks/post-merge"), "#!/bin/sh\n")
	f.p.CanLock = false
	if err := f.newVM(false); err != nil || len(f.warns.Names()) > 0 {
		t.Errorf("new VM: %v, %q", err, f.warns.Names())
	}
	w := f.section().Watched
	if !w.Scanned || w.Sums[".git/config"] == "" || w.Sums[".git/hooks/post-merge"] == "" {
		t.Errorf("first check recorded %+v", w)
	}
	for path := range w.Sums {
		if strings.HasSuffix(path, ".sample") {
			t.Errorf("a sample is watched: %s", path)
		}
	}
	if w := f.check(); len(w) > 0 {
		t.Errorf("nothing changed: %q", w)
	}
	f.git(f.dir, "config", "core.fsmonitor", "./evil")
	write(t, f.path(".git/hooks/pre-commit"), "#!/bin/sh\n")
	if err := os.Remove(f.path(".git/hooks/post-merge")); err != nil {
		t.Fatal(err)
	}
	f.warns.Reset()
	if err := f.p.Check(t.Context(), f.vm, true); err != nil || len(f.warns.Names()) > 0 {
		t.Errorf("running: %v, %q", err, f.warns.Names())
	}
	want := []string{"git_changed paths=.git/config, .git/hooks/post-merge, .git/hooks/pre-commit vm=" + f.vm.Name()}
	if w := f.check(); !slices.Equal(w, want) {
		t.Errorf("changed: %q, want %q", w, want)
	}
	if w := f.check(); len(w) > 0 {
		t.Errorf("named once: %q", w)
	}
}

// A VM that an older devvm made, or that a host's lock protected, has
// nothing watched: its first check records without reporting, and so
// does the first after its record was dropped (devvm adopt).
func TestWatchFirstOfExisting(t *testing.T) {
	f := linuxFixture(t)
	mkdirs(t, f.path(".git/hooks"))
	write(t, f.path(".git/config"), "[core]\n")
	if err := record(t.Context(), f.vm, func(g *InVM) { g.Watched, g.Made = Watched{}, Made{} }); err != nil {
		t.Fatal(err)
	}
	if w := f.check(); len(w) > 0 || !f.section().Watched.Scanned || !f.section().Made.Protected {
		t.Errorf("first: %q, %+v", w, f.section())
	}
	// A folder with no .git yet: one that appears is named.
	g := newFixture(t, "q")
	g.p.CanLock = false
	if err := g.newVM(false); err != nil {
		t.Fatal(err)
	}
	if w := g.check(); len(w) > 0 || !g.section().Watched.Scanned {
		t.Errorf("no .git: %q, %+v", w, g.section().Watched)
	}
	mkdirs(t, g.path(".git/hooks"))
	write(t, g.path(".git/config"), "[core]\n")
	if w := g.check(); !slices.Equal(w, []string{"git_appeared", "git_changed paths=.git/config vm=" + g.vm.Name()}) {
		t.Errorf("appeared: %q", w)
	}
}

// blind is the standing warning a check gives for paths it can't fully
// watch.
func blind(paths string) string { return "git_watch_blind paths=" + paths }

// No symlink out of the project folder is followed: such a symlink is
// summed by where it points, so changing what it points to names nothing,
// and changing the link names it. One that leads into the project folder
// is summed by what it holds too, so the VM changing that is named. Every
// check names them all as paths it can't fully watch.
func TestWatchSymlinks(t *testing.T) {
	f := linuxFixture(t)
	mkdirs(t, f.path(".git/hooks"))
	outside := f.root + "/outside"
	write(t, outside, "one")
	write(t, f.path("tracked.sh"), "one")
	symlink(t, outside, f.path(".git/config"))
	symlink(t, outside, f.path(".git/hooks/pre-commit"))
	symlink(t, "../../tracked.sh", f.path(".git/hooks/post-merge"))
	f.check()
	s := f.section().Watched.Sums
	if s[".git/config"] != "symlink "+outside || s[".git/hooks/pre-commit"] != "symlink "+outside ||
		!strings.HasPrefix(s[".git/hooks/post-merge"], "symlink ../../tracked.sh, sha256 ") {
		t.Errorf("sums %q", s)
	}
	stand := blind(".git/config, .git/hooks/post-merge, .git/hooks/pre-commit")
	write(t, outside, "two")
	if w := f.check(); !slices.Equal(w, []string{stand}) {
		t.Errorf("what a link out points to changed: %q", w)
	}
	write(t, f.path("tracked.sh"), "two")
	if w := f.check(); !slices.Equal(w, []string{"git_changed paths=.git/hooks/post-merge vm=" + f.vm.Name(), stand}) {
		t.Errorf("what a link in points to changed: %q", w)
	}
	if err := os.Remove(f.path(".git/hooks/pre-commit")); err != nil {
		t.Fatal(err)
	}
	symlink(t, f.root+"/elsewhere", f.path(".git/hooks/pre-commit"))
	if w := f.check(); !slices.Equal(w, []string{"git_changed paths=.git/hooks/pre-commit vm=" + f.vm.Name(), stand}) {
		t.Errorf("the link changed: %q", w)
	}

	// .git itself a symlink: to a repo elsewhere, never read; to one in
	// the project folder, read.
	g := newFixture(t, "q")
	g.p.CanLock = false
	if err := g.newVM(false); err != nil {
		t.Fatal(err)
	}
	repo := g.root + "/repo"
	mkdirs(t, repo+"/hooks")
	write(t, repo+"/config", "")
	write(t, repo+"/hooks/pre-commit", "one")
	symlink(t, repo, g.path(".git"))
	g.check()
	if s := g.section().Watched.Sums; s[".git/hooks"] != "under a symlink" || s[".git/config"] != "under a symlink" {
		t.Errorf("sums %q", s)
	}
	write(t, repo+"/hooks/pre-commit", "two")
	if w := g.check(); !slices.Equal(w, []string{blind(".git/config, .git/hooks")}) {
		t.Errorf("read through .git's link out: %q", w)
	}
	if err := os.Remove(g.path(".git")); err != nil {
		t.Fatal(err)
	}
	inner := g.path("inner")
	mkdirs(t, inner+"/hooks")
	write(t, inner+"/config", "")
	symlink(t, "inner", g.path(".git"))
	g.check()
	write(t, inner+"/hooks/pre-commit", "evil")
	if w := g.check(); len(w) != 2 || w[0] != "git_changed paths=.git/hooks vm="+g.vm.Name() {
		t.Errorf("through .git's link in: %q", w)
	}
}

// The watch is bounded, and names what it reads only in part at each
// check: a hooks folder past maxWatchEntries, summed by that and its
// change time, though the hooks git runs at its top are always summed, and
// a file past maxWatchBytes, summed by its size, inode and change time, so
// a change that keeps its size is named.
func TestWatchBounds(t *testing.T) {
	f := linuxFixture(t)
	mkdirs(t, f.path(".git/hooks/a"))
	for i := range maxWatchEntries + 1 {
		write(t, f.path(".git/hooks/a/h"+strconv.Itoa(i)), "")
	}
	write(t, f.path(".git/hooks/pre-commit"), "one")
	write(t, f.path(".git/config"), "")
	if err := os.Truncate(f.path(".git/config"), maxWatchBytes+1); err != nil {
		t.Fatal(err)
	}
	_ = f.check() // the repo, made since the VM, recorded
	if w := f.check(); !slices.Equal(w, []string{blind(".git/config, .git/hooks")}) {
		t.Errorf("unchanged: %q", w)
	}
	s := f.section().Watched.Sums
	if !strings.HasPrefix(s[".git/hooks"], "more than 1000 entries, ino ") || len(s) > maxWatchEntries+4 {
		t.Errorf("hooks: %q, %d sums", s[".git/hooks"], len(s))
	}
	if !strings.HasPrefix(s[".git/config"], "size "+strconv.Itoa(maxWatchBytes+1)+", ino ") {
		t.Errorf("config: %q", s[".git/config"])
	}
	if !strings.HasPrefix(s[".git/hooks/pre-commit"], "sha256 ") {
		t.Errorf("pre-commit, hidden by a/: %q", s[".git/hooks/pre-commit"])
	}
	write(t, f.path(".git/hooks/pre-commit"), "two")
	cfg, err := os.OpenFile(f.path(".git/config"), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.WriteAt([]byte("[core]"), 0); err != nil {
		t.Fatal(err)
	}
	_ = cfg.Close()
	if err := os.Chtimes(f.path(".git/config"), time.Unix(1, 0), time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	w := f.check()
	if len(w) != 2 || w[0] != "git_changed paths=.git/config, .git/hooks/pre-commit vm="+f.vm.Name() {
		t.Errorf("changed, its size and times kept: %q", w)
	}
}

// A watch keeps at most maxWatchTotal sums, and a warning names at most
// maxListed paths, then how many more.
func TestWatchTotal(t *testing.T) {
	f := linuxFixture(t)
	mkdirs(t, f.path(".git/hooks"))
	// Submodules' git dirs, each with a hooks folder within its own bound.
	for i := range 3 {
		dir := f.path(".git/modules/m" + strconv.Itoa(i))
		mkdirs(t, dir+"/objects", dir+"/refs", dir+"/hooks")
		write(t, dir+"/HEAD", "")
		for j := range maxWatchEntries - 10 {
			write(t, dir+"/hooks/h"+strconv.Itoa(j), "")
		}
	}
	_ = f.check()
	if s := f.section().Watched.Sums; len(s) > maxWatchTotal {
		t.Errorf("%d sums", len(s))
	}
	for i := range maxListed + 3 {
		write(t, f.path(".git/hooks/n"+strconv.Itoa(i)), "")
	}
	w := f.check()
	if len(w) != 2 || !strings.HasPrefix(w[0], "git_changed_more count=") || !strings.HasPrefix(w[1], "git_watch_blind_more count=") {
		t.Errorf("changed: %q", w)
	}
	if l, more := listed([]string{"a", "b"}); l != "a, b" || more != 0 {
		t.Errorf("listed: %q, %d", l, more)
	}
}

// The VM pointing git at another config through .git/commondir, as a
// proven exploit did, is reported at the next check, as a lock's check
// reports it, and the watch names it; so are core.hooksPath into the
// project folder (husky's .husky) and an include into it, and changes to
// what they lead to.
func TestWatchReportsWhatALockCantStop(t *testing.T) {
	needGit(t)
	f := newFixture(t, "p")
	f.git(f.dir, "init", "-q")
	f.p.CanLock = false
	if err := f.newVM(false); err != nil || len(f.warns.Names()) > 0 {
		t.Fatalf("new VM: %v, %q", err, f.warns.Names())
	}
	mkdirs(t, f.path("evil"))
	symlink(t, "../.git/objects", f.path("evil/objects"))
	symlink(t, "../.git/refs", f.path("evil/refs"))
	write(t, f.path("evil/config"), "[core]\n\tfsmonitor = ./pwn\n")
	write(t, f.path(".git/commondir"), "../evil\n")
	w := f.check()
	if !slices.Contains(w, "git_commondir git="+f.path(".git")+" target=../evil") ||
		!slices.Contains(w, "git_changed paths=.git/commondir vm="+f.vm.Name()) {
		t.Errorf("commondir: %q", w)
	}
	if err := os.Remove(f.path(".git/commondir")); err != nil {
		t.Fatal(err)
	}
	_ = f.check()

	// A .git replaced: named once, then compared with the new one.
	if err := os.Rename(f.path(".git"), f.path(".git-old")); err != nil {
		t.Fatal(err)
	}
	f.git(f.dir, "init", "-q")
	if w := f.check(); !slices.Contains(w, "git_replaced git="+f.path(".git")) {
		t.Errorf("replaced: %q", w)
	}
	if w := f.check(); slices.Contains(w, "git_replaced git="+f.path(".git")) {
		t.Errorf("replaced, again: %q", w)
	}

	// Delegations into the project folder.
	mkdirs(t, f.path(".husky"))
	write(t, f.path(".husky/pre-commit"), "one")
	write(t, f.path("more.cfg"), "")
	f.git(f.dir, "config", "core.hooksPath", ".husky")
	f.git(f.dir, "config", "include.path", "../more.cfg")
	w = f.check()
	if !slices.Contains(w, "git_include_inside config="+f.path(".git/config")+" path=../more.cfg") {
		t.Errorf("include: %q", w)
	}
	write(t, f.path(".husky/pre-commit"), "two")
	write(t, f.path("more.cfg"), "[core]\n\tpager = ./pwn\n")
	if w := f.check(); !slices.Contains(w, "git_changed paths=.husky/pre-commit, more.cfg vm="+f.vm.Name()) {
		t.Errorf("what they lead to: %q", w)
	}
}

// What changed is sorted, gone and added included.
func TestChangedPaths(t *testing.T) {
	for _, c := range []struct {
		was, now map[string]string
		want     []string
	}{
		{nil, nil, nil},
		{map[string]string{"a": "1"}, map[string]string{"a": "1"}, nil},
		{map[string]string{"b": "1", "c": "1"}, map[string]string{"a": "1", "b": "2"}, []string{"a", "b", "c"}},
	} {
		if got := changedPaths(c.was, c.now); !slices.Equal(got, c.want) {
			t.Errorf("changedPaths(%v, %v) = %q, want %q", c.was, c.now, got, c.want)
		}
	}
}
