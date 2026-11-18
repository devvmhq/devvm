package gitprotect

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/devvmhq/devvm/internal/hostgit"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/safefile"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/vmname"
)

// Strict is the mode that devvm git-protect strict recorded in devvm
// 0.1.0 and 0.1.1-rc.1: on, with diff.ignoreSubmodules = all set in the
// locked .git/config of each repo its protection covered then. That can't
// keep git on this machine out of a submodule the VM adds (a .gitmodules
// ignore line, git add -A, the plumbing and describe --dirty enter it
// anyway), so devvm git-protect refuses it, Mode and Next read it as on,
// and the next check that finds the VM stopped converts it (unstrict).
const Strict = "strict"

// ignoreKey is the setting strict set, and ignoreAll its value.
const (
	ignoreKey = "diff.ignoreSubmodules"
	ignoreAll = "all"
)

// asOn is mode, with an earlier devvm's strict as on.
func asOn(mode string) string {
	if mode == Strict {
		return On
	}
	return mode
}

// converter is a protector's conversion of a VM an earlier devvm made
// strict, and what strict set in its repos' configs.
type converter struct{ *Protector }

// strictRepo is a repo whose config strict may have set: its tree, its
// top folder relative to the project folder ("." for the folder's own),
// and its .git's identity as the VM's state recorded it (Repo.GitID).
type strictRepo struct {
	tree
	rel, id string
}

// recorded reports whether r's .git is the one the VM's state recorded,
// on whichever device a boot numbered its disk (platform.SameFolder).
func (r strictRepo) recorded() bool {
	return platform.SameFolder(r.git(), r.id, r.git(), folderID(r.git()))
}

// strictRepos are the repos made records, the folder's own and the nested
// ones, those named in only when it's set (InVM.StrictLeft).
func (p converter) strictRepos(made Made, only []string) []strictRepo {
	all := []strictRepo{{tree: p.tree, rel: ".", id: made.GitID}}
	for _, rel := range slices.Sorted(maps.Keys(made.Repos)) {
		if filepath.IsLocal(rel) && rel != "." {
			all = append(all, strictRepo{tree: p.repoAt(rel), rel: rel, id: made.Repos[rel].GitID})
		}
	}
	if only == nil {
		return all
	}
	return slices.DeleteFunc(all, func(r strictRepo) bool { return !slices.Contains(only, r.rel) })
}

// dropped is what dropIgnore did: the new sum of each config it changed,
// by its path relative to the project folder, and the repos whose config
// it left for a later check (InVM.StrictLeft).
type dropped struct {
	sums map[string]string
	left []string
}

// unstrict converts a stopped VM an earlier devvm made strict, once its
// check is done, and takes out what a conversion left (InVM.StrictLeft):
// what strict set comes out of the configs of the repos made records
// protecting (dropIgnore), the configs' new sums go into the watch's, so
// the next check doesn't name devvm's own change, and its mode, and a mode
// saved for its next stop, read on, all under the host's lock. An
// interrupt records nothing, so the next check goes again. It warns once,
// unless the mode saved for its next stop is off, which it's set to next.
func (p converter) unstrict(ctx context.Context, vm *state.VM, made Made) error {
	g, err := Of(vm)
	if err != nil || g.Mode != Strict && g.Next != Strict && len(g.StrictLeft) == 0 {
		return err
	}
	var d dropped
	err = serial(ctx, p.Serial, func() error {
		d = p.dropStrict(ctx, vm.Name(), g, made, g.locks(), g.Watched)
		if err := ctx.Err(); err != nil {
			return err
		}
		return record(ctx, vm, func(g *InVM) {
			g.Mode, g.Next, g.StrictLeft = asOn(g.Mode), asOn(g.Next), d.left
			for rel, sum := range d.sums {
				if _, ok := g.Watched.Sums[rel]; ok {
					g.Watched.Sums[rel] = sum
				}
			}
		})
	})
	if err != nil {
		return err
	}
	if (g.Mode == Strict || g.Next == Strict) && g.Next != Off {
		p.Warn(msg.GitStrictConverted(vm.Name()))
	}
	return nil
}

// strictLeft takes what strict set out of the VM vm's configs, as its
// section g has them (dropStrict), for devvm git-protect and Unlock, which
// take .git as it is, and returns the repos it left.
func (p converter) strictLeft(ctx context.Context, vm vmname.VM, g InVM, locks bool) []string {
	return p.dropStrict(ctx, vm, g, g.Made, locks, Watched{}).left
}

// dropStrict takes what strict set out of the configs of the repos made
// records (dropIgnore): all of them while g, the VM's section, is strict,
// those a conversion left (InVM.StrictLeft) once it isn't, else none.
func (p converter) dropStrict(ctx context.Context, vm vmname.VM, g InVM, made Made, locks bool, watched Watched) dropped {
	switch {
	case g.Mode == Strict:
		return p.dropIgnore(ctx, vm, p.strictRepos(made, nil), locks, watched)
	case len(g.StrictLeft) > 0:
		return p.dropIgnore(ctx, vm, p.strictRepos(made, g.StrictLeft), locks, watched)
	}
	return dropped{}
}

// dropIgnore takes diff.ignoreSubmodules = all, which strict set, out of
// the .git/config of each of repos, for the VM vm, under the host's lock
// (the caller's serial). strict recorded nothing of a value the config
// had before, and set its own over it, so only all comes out: any other
// value is one set since, which stays. It acts only on a repo whose .git
// is the one the VM's state recorded: one replaced since, or one it has
// no record of (a VM devvm adopt moved), isn't strict's, so its all is
// named, with what to do, and left. On a host that
// watches, a config whose sum isn't the one watch recorded changed since
// the check, which the next check names, so it's left for then. A config
// another VM's protection keeps locked too (Kept) is left as it is, which
// unlocking would leave that VM's to race, and named with what to do,
// and a later check tries again. Otherwise each config is unlocked for
// the moment, when it's locked (locks, the VM's mode, or the flag), and
// locked again, even after an interrupt, unless unlocked says the caller
// unlocked them (Unlock).
func (p converter) dropIgnore(ctx context.Context, vm vmname.VM, repos []strictRepo, locks bool, watched Watched) dropped {
	kept := keeper(p.Kept, vm)
	d := dropped{sums: map[string]string{}}
	for _, r := range repos {
		if ctx.Err() != nil {
			break
		}
		cfg := filepath.Join(r.git(), "config")
		if !p.setsAll(ctx, r, cfg) {
			continue
		}
		if !r.recorded() {
			// Not the .git strict set it in: the user's to decide.
			p.Warn(msg.GitStrictUnknown(cfg))
			continue
		}
		if k := kept(cfg); k.ID() != "" {
			d.left = append(d.left, r.rel)
			p.warnKept(k, cfg)
			continue
		}
		if sum, ok := watched.Sums[relTo(p.Dir, cfg)]; ok && watched.Scanned && sum != sumNow(cfg) {
			d.left = append(d.left, r.rel)
			continue
		}
		relock := locks || p.Locker.Locked(cfg)
		if relock && !p.flag(ctx, false, cfg) {
			continue // moved meanwhile: flag named it
		}
		err := hostgit.Command(ctx, "config", "--file", cfg, "--unset-all", ignoreKey, "^"+ignoreAll+"$").Run()
		sum, ok := sumNow(cfg), p.sameRepo(r, cfg)
		if relock && ok {
			// Locked again even when the run was interrupted.
			p.flag(context.WithoutCancel(ctx), true, cfg)
		}
		switch {
		case ctx.Err() != nil:
		case !ok:
			p.Warn(msg.GitLockRaced(cfg))
		case err != nil:
			p.Warn(msg.GitStrictLeft(cfg))
		default:
			d.sums[relTo(p.Dir, cfg)] = sum
		}
	}
	return d
}

// setsAll reports whether cfg, the config of r, sets
// diff.ignoreSubmodules to all: r's .git is a folder, itself, and cfg a
// regular file in the project folder, read without following a symlink or
// waiting on a FIFO. Whether it's the .git strict set it in is the
// caller's to check, against the identity the VM's state recorded.
func (p converter) setsAll(ctx context.Context, r strictRepo, cfg string) bool {
	if !isDir(r.git()) || isLink(r.git()) || !p.realInDir(cfg) {
		return false
	}
	if _, err := safefile.Read(cfg, maxWatchBytes, safefile.Opts{}); err != nil {
		return false
	}
	out := git(ctx, "config", "--file", cfg, "--get-all", ignoreKey)
	return slices.Contains(strings.Split(strings.TrimSpace(string(out)), "\n"), ignoreAll)
}

// sameRepo reports whether, once git wrote it, cfg is still a regular
// file in the project folder, in r's .git as recorded: git renames its
// new file over the old one, so it's the folder that's compared.
func (p converter) sameRepo(r strictRepo, cfg string) bool {
	fi, err := os.Lstat(cfg)
	return err == nil && fi.Mode().IsRegular() && p.realInDir(cfg) && r.recorded()
}

// warnKept names a config another VM's protection keeps locked, k, that
// keeps strict's all: with what to do when that VM's named
// (git_kept_locked), else as k says (its state can't be read).
func (p converter) warnKept(k msg.M, cfg string) {
	if k.ID() == "git_kept_locked" {
		p.Warn(msg.GitStrictKept(cfg, k.VM()))
		return
	}
	p.Warn(k)
}

// sumNow is what's at path summed as the watch sums it, or "" when
// nothing is.
func sumNow(path string) string {
	fi, err := os.Lstat(path)
	if err != nil {
		return ""
	}
	return sumOf(path, fi)
}
