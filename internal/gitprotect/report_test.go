package gitprotect

import (
	"slices"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/stubtest"
)

// Every delegation kind has a message of its own, and its name as Made's
// sum has always had it, so a VM's recorded sum still matches.
func TestDelegationKinds(t *testing.T) {
	ids, names := map[string]bool{}, map[string]bool{}
	for k := range delegationKinds {
		d := delegation{kind: k, where: "/w", target: "/t"}
		id, name := d.message().ID(), k.name()
		if id == "" || ids[id] || name == "" || names[name] {
			t.Errorf("kind %d: message %q, name %q (taken: %t, %t)", k, id, name, ids[id], names[name])
		}
		ids[id], names[name] = true, true
	}
	if got := (delegation{kind: delegationKinds}).message().ID(); got != "" {
		t.Errorf("past the kinds: %q", got)
	}
	// A VM's recorded sum (Made.Delegations) is of the names as strings.
	ds := []delegation{{includeInside, "/c", "x", ""}, {hookLinkInside, "/h", "y", ""}, {gitdirOutside, "/s", "/o", ""}, {gitdirNotGit, "/n", "/g", ""}}
	// sha256 of "7:include2:/c1:x4:hook2:/h1:y7:outside2:/s2:/o6:notgit2:/n2:/g".
	if got := delegationsSum(ds); got != "5a83e04077a77954" {
		t.Errorf("sum %s: the kinds' names changed", got)
	}
}

// reportsFixture is a snapshot with something for every report: a
// commondir, a stray worktree, a git dir besides .git, a hooks folder
// with a hook, unlocked, every kind of delegation and a hooks path.
func reportsFixture(t *testing.T) (*fixture, snapshot) {
	t.Helper()
	f := newFixture(t, "p")
	mkdirs(t, f.path(".git/hooks"))
	write(t, f.path(".git/hooks/pre-commit"), "#!/bin/sh\n")
	s := snapshot{
		tree: f.p.tree,
		git:  f.path(".git"), exists: true, id: folderID(f.path(".git")),
		commondir: true, target: "../x",
		strays: []string{f.path(".git/worktrees/w")},
		dirs:   []string{f.path("sub/.git")},
		paths:  []string{f.path(".git/hooks")},
		ds: []delegation{
			{includeInside, f.path(".git/config"), "../i", f.path("i")},
			{hookLinkInside, f.path(".git/hooks/post-merge"), "../../h", ""},
			{gitdirOutside, f.path("o"), "/elsewhere", ""},
			{gitdirNotGit, f.path("n"), f.path("src"), ""},
		},
		hooksPath: ".husky",
	}
	return f, s
}

// What protect reports, for each thing it runs for, each way it treats
// the locks, and each record: never set up, or set up as the snapshot
// is but for the git dir besides .git.
func TestReports(t *testing.T) {
	for _, tc := range []struct {
		name string
		why  why
		how  how
		set  bool // the record is the snapshot's; else never set up
		want string
	}{
		{"new", whyNew, lockNow, false, "always hooksPath delegations"},
		{"new, watching", whyNew, watching, false, "always hooksPath delegations"},
		{"on, never set up", whyOn, lockNow, false, "always hooksPath delegations"},
		{"on", whyOn, lockNow, true, "always embedded hooksPath delegations"},
		{"check, never set up", whyCheck, lockNow, false, "always appeared unlocked delegations"},
		{"check, never set up, running", whyCheck, later, false, "always appeared unlocked delegations"},
		{"check, never set up, watching", whyCheck, watching, false, "always appeared delegations"},
		{"check", whyCheck, lockNow, true, "always embedded unlocked"},
		{"check, running", whyCheck, later, true, "always embedded unlocked"},
		{"check, watching", whyCheck, watching, true, "always embedded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, s := reportsFixture(t)
			if tc.how == watching {
				f.canLock(false)
			}
			groups := map[string][]msg.M{
				"always":    {msg.GitCommondir(s.git, "../x"), msg.GitWorktreeStray(s.strays[0])},
				"appeared":  {msg.GitAppeared()},
				"embedded":  {msg.GitEmbeddedNew(s.dirs[0])},
				"unlocked":  {msg.GitHooksUnlocked(s.paths[0])},
				"hooksPath": {msg.GitHooksPath(".husky")},
			}
			for _, d := range s.ds {
				groups["delegations"] = append(groups["delegations"], d.message())
			}
			was := Made{}
			if tc.set {
				was = Made{Protected: true, Repo: Repo{GitID: s.id, Delegations: delegationsSum(s.ds)}}
			}
			made := f.protectOne(s, was, tc.why, tc.how)
			var want []string
			for _, g := range strings.Fields(tc.want) {
				for _, m := range groups[g] {
					want = append(want, m.Names())
				}
			}
			if got := f.warns.Names(); !slices.Equal(got, want) {
				t.Errorf("warned\n%q\nwant\n%q", got, want)
			}
			gitID := s.id
			if tc.how == later {
				gitID = was.GitID
			}
			if want := (Made{Protected: true, Nested: tc.why != whyCheck, Repo: Repo{GitID: gitID, GitDirs: []string{"sub/.git"}, Delegations: delegationsSum(s.ds)}}); !madeEqual(made, want) {
				t.Errorf("made %+v, want %+v", made, want)
			}
			if locked := f.locked(s.paths[0]); locked != (tc.how == lockNow) {
				t.Errorf("locks %q", f.flags)
			}
		})
	}
}

// A check of a .git that replaced the one locked reports that alone, and
// returns the record as it was (watching: with the new .git's identity,
// to compare the next check with); without a .git, nothing is reported.
func TestReportsReplacedAndMissing(t *testing.T) {
	f, s := reportsFixture(t)
	was := Made{Protected: true, Repo: Repo{GitID: "1:2", GitDirs: []string{"x"}, Delegations: "d"}}
	if made := f.protectOne(s, was, whyCheck, lockNow); !madeEqual(made, was) ||
		!slices.Equal(f.warns.Names(), []string{msg.GitReplaced(s.git).Names()}) || len(f.flags) > 0 {
		t.Errorf("replaced: made %+v, warned %q, locks %q", made, f.warns.Names(), f.flags)
	}
	f.canLock(false)
	if made := f.protectOne(s, was, whyCheck, watching); made.GitID != s.id {
		t.Errorf("replaced, watching: made %+v", made)
	}
	f.warns.Reset()
	s.exists = false
	if made := f.protectOne(s, was, whyNew, lockNow); !madeEqual(made, Made{Protected: true, Nested: true, Repo: Repo{GitDirs: was.GitDirs, Delegations: "d"}}) ||
		len(f.warns.Names()) > 0 {
		t.Errorf("no .git: made %+v, warned %q", made, f.warns.Names())
	}
}

// A .git whose device a boot numbered anew, as macOS does, is the .git
// locked: a check reports no git_replaced, and records its identity on
// the device it has now, however it treats the locks. A strict
// conversion takes it as that repo's too. Another inode is replaced.
func TestReportsRenumbered(t *testing.T) {
	f, s := reportsFixture(t)
	renumbered := stubtest.Renumbered(s.id)
	for _, h := range []how{lockNow, watching, later} {
		f.warns.Reset()
		was := Made{Protected: true, Repo: Repo{GitID: renumbered, GitDirs: []string{"sub/.git"}, Delegations: delegationsSum(s.ds)}}
		made := f.protectOne(s, was, whyCheck, h)
		if slices.Contains(f.warns.Names(), msg.GitReplaced(s.git).Names()) || made.GitID != s.id {
			t.Errorf("how %d: made %+v, warned %q, want its identity now %q", h, made, f.warns.Names(), s.id)
		}
	}
	if r := (strictRepo{tree: f.p.tree, rel: ".", id: renumbered}); !r.recorded() {
		t.Error("a strict conversion: not its .git")
	}
	f.warns.Reset()
	dev, rest, _ := strings.Cut(s.id, ":")
	ino, birth, _ := strings.Cut(rest, ":")
	was := Made{Protected: true, Repo: Repo{GitID: stubtest.Renumbered(dev + ":" + ino + "0:" + birth)}}
	if f.protectOne(s, was, whyCheck, lockNow); !slices.Equal(f.warns.Names(), []string{msg.GitReplaced(s.git).Names()}) {
		t.Errorf("another inode: warned %q", f.warns.Names())
	}
}

// protectOne is protect of the folder's own repo alone, its scan s,
// recorded as was.
func (f *fixture) protectOne(s snapshot, was Made, w why, h how) Made {
	return f.p.protect(f.t.Context(), f.vm.Name(), []snapshot{s}, InVM{Made: was}, w, h, repoSet{trees: []tree{f.p.tree}})
}

func madeEqual(a, b Made) bool {
	return a.Protected == b.Protected && a.Nested == b.Nested && a.GitID == b.GitID && slices.Equal(a.GitDirs, b.GitDirs) && a.Delegations == b.Delegations
}
