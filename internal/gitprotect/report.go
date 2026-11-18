package gitprotect

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"slices"
	"strconv"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/vmname"
)

// why is what protect runs for.
type why int

const (
	whyNew   why = iota // a VM just made
	whyOn               // devvm git-protect on
	whyCheck            // a launch of an existing VM
)

// how protect treats the locks.
type how int

const (
	lockNow  how = iota // a stopped VM's: lock
	later               // a running VM's: report, and lock at its next stop
	watching            // a host without locks: report, record .git's identity, lock nothing
)

// run is one repo's protect: what it runs for, how it treats the locks,
// what the last one recorded of the repo, whether it had protected it
// (Made.Protected, or a nested repo's record), whether it's the folder's
// own repo, what the scan found, and the sum of its delegations.
type run struct {
	why       why
	how       how
	was       Repo
	protected bool
	top       bool
	s         snapshot
	sum       string
}

// report is something protect tells: when says whether a run tells it,
// and report tells it.
type report struct {
	when   func(r *run) bool
	report func(p *Protector, r *run)
}

// reports are what protect tells, in order, once .git is there and isn't
// one that replaced the one locked. A new VM's git dirs are recorded, not
// reported; a check reports what changed since the last one, and the
// hooks path and delegations only when they change. Watching, nothing is
// locked, so the hooks in folders a lock would cover aren't news: the
// watch names their changes.
var reports = []report{
	{ // submodules past what one check follows, on every run
		when:   func(r *run) bool { return r.s.emb.bounded },
		report: func(p *Protector, r *run) { p.Warn(msg.GitScanBounded(r.s.git)) },
	},
	{ // commondir points git elsewhere for config and hooks, on every run
		when:   func(r *run) bool { return r.s.commondir },
		report: func(p *Protector, r *run) { p.Warn(msg.GitCommondir(r.s.git, r.s.target)) },
	},
	{ // linked worktrees that read config and hooks from elsewhere, on every run
		when: func(*run) bool { return true },
		report: func(p *Protector, r *run) {
			for _, s := range r.s.strays {
				p.Warn(msg.GitWorktreeStray(s))
			}
		},
	},
	{ // the folder's own repo, made since the VM was (a nested one's
		// is git_nested_appeared, from protect)
		when:   func(r *run) bool { return r.top && r.why == whyCheck && r.was.GitID == "" },
		report: func(p *Protector, _ *run) { p.Warn(msg.GitAppeared()) },
	},
	{ // git dirs that appeared since the last time, once there was one
		when: func(r *run) bool { return r.why != whyNew && r.protected },
		report: func(p *Protector, r *run) {
			seen := p.abs(r.was.GitDirs)
			for _, d := range r.s.dirs {
				if !slices.Contains(seen, d) {
					p.Warn(msg.GitEmbeddedNew(d))
				}
			}
		},
	},
	{ // hooks in a hooks folder that isn't locked appeared since the last
		// lock (there was no folder to lock, or it's a new submodule's), so
		// the VM may have made them
		when:   func(r *run) bool { return r.why == whyCheck && r.how != watching },
		report: (*Protector).warnHooksUnlocked,
	},
	{ // core.hooksPath into the project folder, when it's set up
		when:   func(r *run) bool { return r.why != whyCheck && r.s.hooksPath != "" },
		report: func(p *Protector, r *run) { p.Warn(msg.GitHooksPath(r.s.hooksPath)) },
	},
	{ // what the locked files hand over, when it's set up or changed
		when: func(r *run) bool { return r.why != whyCheck || r.sum != r.was.Delegations },
		report: func(p *Protector, r *run) {
			for _, d := range r.s.ds {
				p.Warn(d.message())
			}
		},
	},
}

// protect locks each repo the scans in snaps found, as h says, and
// reports what a lock can't stop (protectRepo); it returns what it found
// (rec's Made, as it is now), for the caller to record with the mode it
// set. A nested repo without a record is scanned for the first time, as a
// new VM's (whyNew); at a check of a VM that protected its nested repos,
// it's one that appeared since (git_nested_appeared). A recorded one the
// walk found gone is named (git_nested_gone), and its record moves to
// Made.Left, as does that of one whose git dir another repo's protection
// now covers, so the locks it may have left are taken off with the rest.
// A running VM's check scans the folder's own repo alone, and keeps the
// nested repos' records as they are.
func (p *Protector) protect(ctx context.Context, vm vmname.VM, snaps []snapshot, rec InVM, w why, h how, c repoSet) Made {
	was := rec.Made
	made := Made{Protected: true, Nested: was.Nested || w != whyCheck, Left: maps.Clone(was.Left)}
	if h == later {
		made.Repos = maps.Clone(was.Repos)
	}
	for _, s := range snaps {
		if s.tree.Root == p.Dir {
			made.Repo = p.protectRepo(ctx, &run{why: w, how: h, was: was.Repo, protected: was.Protected, top: true, s: s})
			continue
		}
		rel := relTo(p.Dir, s.tree.Root)
		r, ok := was.Repos[rel]
		wi := w
		if !ok {
			wi = whyNew
			if w == whyCheck && was.Nested && s.exists {
				p.warnAppeared(ctx, s, rel, vm)
			}
		}
		if made.Repos == nil {
			made.Repos = map[string]Repo{}
		}
		made.Repos[rel] = p.protectRepo(ctx, &run{why: wi, how: h, was: r, protected: ok, s: s})
		delete(made.Left, rel)
	}
	for _, rel := range c.gone {
		p.Warn(msg.GitNestedGone(rel, vm))
	}
	if h != later {
		for rel, r := range was.Repos {
			if _, ok := made.Repos[rel]; !ok {
				if made.Left == nil {
					made.Left = map[string]Repo{}
				}
				made.Left[rel] = r
			}
		}
	}
	return made
}

// warnAppeared names a nested repo that appeared since the last check,
// scanned as s, at rel, with what in its config and hooks runs programs
// on this machine (programs), which devvm locks as it is.
func (p *Protector) warnAppeared(ctx context.Context, s snapshot, rel string, vm vmname.VM) {
	if runs := programs(ctx, s); len(runs) > 0 {
		l, _ := listed(runs)
		p.Warn(msg.GitNestedAppearedRuns(rel, l, vm))
		return
	}
	p.Warn(msg.GitNestedAppeared(rel, vm))
}

// protectRepo locks one repo's .git, as r.how says, and reports what a
// lock can't stop (reports); it returns what it found of the repo. Before
// anything else, a check whose .git replaced the one it locked reports
// that alone; the one it locked on a device a boot numbered anew is that
// .git (platform.SameFolder).
func (p *Protector) protectRepo(ctx context.Context, r *run) Repo {
	was, s, w, h := r.was, r.s, r.why, r.how
	if !s.exists {
		if !r.top {
			return was
		}
		return Repo{GitDirs: was.GitDirs, Delegations: was.Delegations}
	}
	if w == whyCheck && was.GitID != "" && !platform.SameFolder(s.git, was.GitID, s.git, s.id) {
		p.Warn(msg.GitReplaced(s.git))
		if h == watching {
			// Nothing to lock again: the next check compares with this one.
			was.GitID = s.id
		}
		return was
	}
	if platform.Renumbered(s.git, was.GitID, s.git, s.id) {
		// The .git locked, on the device a boot numbered anew: recorded as
		// it is now, whatever the check does with the locks.
		was.GitID = s.id
	}
	r.sum = delegationsSum(s.ds)
	for _, rep := range reports {
		if rep.when(r) {
			rep.report(p, r)
		}
	}
	made := Repo{GitID: was.GitID, GitDirs: p.rel(s.dirs), Delegations: r.sum}
	switch h {
	case lockNow:
		p.lock(ctx, s)
		made.GitID = s.id
	case watching:
		made.GitID = s.id
	case later:
	}
	return made
}

// warnHooksUnlocked names the hooks folders a lock covers that aren't
// locked and hold hooks.
func (p *Protector) warnHooksUnlocked(r *run) {
	for _, path := range r.s.paths {
		if isHooks(path) && !p.Locker.Locked(path) && hasHooks(path) {
			p.Warn(msg.GitHooksUnlocked(path))
		}
	}
}

// delegationKind is what a delegation hands over.
type delegationKind int

const (
	includeInside   delegationKind = iota // an include into the project folder
	hookLinkInside                        // a hook that's a symlink into it
	gitdirOutside                         // a submodule's git dir outside it
	gitdirNotGit                          // a folder a submodule names as its git dir that isn't one
	delegationKinds                       // how many kinds there are
)

// delegation is something the locked files hand over to a file the VM can
// edit, a git dir devvm can't lock, or a folder named as one that isn't.
type delegation struct {
	kind   delegationKind
	where  string // the config, the hook, or the submodule's folder
	target string // the include's path or the hook's link as written, or the git dir
	path   string // what an include leads to, absolute, for the watch
}

// message is the warning that names d.
func (d delegation) message() msg.M {
	switch d.kind {
	case includeInside:
		return msg.GitIncludeInside(d.where, d.target)
	case hookLinkInside:
		return msg.GitHookLinkInside(d.where, d.target)
	case gitdirOutside:
		return msg.GitGitdirOutside(d.target, d.where)
	case gitdirNotGit:
		return msg.GitGitdirNotGit(d.target, d.where)
	case delegationKinds:
	}
	return msg.M{}
}

// name is the kind as delegationsSum has it, which Made records: the
// names it had as strings.
func (k delegationKind) name() string {
	switch k {
	case includeInside:
		return "include"
	case hookLinkInside:
		return "hook"
	case gitdirOutside:
		return "outside"
	case gitdirNotGit:
		return "notgit"
	case delegationKinds:
	}
	return ""
}

func delegationsSum(ds []delegation) string {
	h := sha256.New()
	for _, d := range ds {
		for _, f := range []string{d.kind.name(), d.where, d.target} {
			h.Write([]byte(strconv.Itoa(len(f)) + ":" + f))
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
