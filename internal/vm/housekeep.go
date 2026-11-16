package vm

import (
	"context"
	"errors"
	"os"
	"slices"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/build"
	"github.com/devvmhq/devvm/internal/from"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/proxy"
	"github.com/devvmhq/devvm/internal/settings"
	"github.com/devvmhq/devvm/internal/tools"
	"github.com/devvmhq/devvm/internal/vmname"
)

// Housekeeping is what devvm keeps besides its VMs, as devvm images,
// devvm gc, devvm space and retention read and delete it: the images on
// every backend, the shared tools, the exports, the VMs' leftovers, the
// old mise versions, the package cache and the shared Nix cache. Its
// flow is the inventory, then PlanGC, then the plan's Apply, and Retain;
// each returns what it found or did, which its caller shows. It never
// deletes a VM.
type Housekeeping struct{ *Env }

// ToolTargets are the targets the share's tool images are current for:
// the base new VMs get on b (its backend and From), then that base on
// every backend.
func (h Housekeeping) ToolTargets(b *build.Env) ([]tools.Target, error) {
	tg, err := b.ToolTarget()
	if err != nil {
		return nil, err
	}
	tgs := []tools.Target{tg}
	for _, be := range h.Registry.All() {
		t, err := h.Builder(be, b.From).ToolTarget()
		if err != nil {
			return nil, err
		}
		tgs = append(tgs, t)
	}
	return tgs, nil
}

// ShareListing lists the share as devvm images, devvm gc, devvm space
// and retention do, without asking any backend: current for ToolTargets,
// each with how many of the VMs' states (inv) list it.
func (h Housekeeping) ShareListing(b *build.Env, inv *Inventory) ([]tools.Listed, error) {
	tgs, err := h.ToolTargets(b)
	if err != nil {
		return nil, err
	}
	inUse, err := inv.ToolsInUse()
	if err != nil {
		return nil, err
	}
	return tools.Share{Env: b.Tools}.List(tgs, inUse)
}

// ListedImage is an image, its backend's name and the images on it.
type ListedImage struct {
	build.Listed
	Backend string
	images  build.Images
}

// Unlisted is a backend a VM's state names that couldn't list its
// images, and why: its Check's error, else its listing's.
type Unlisted struct {
	Backend string
	Err     error
}

// ImageListing is what ListImages found: the images on every backend
// that listed them, the shared tools, and the backends a VM uses that
// couldn't list.
type ImageListing struct {
	Images   []ListedImage
	Tools    []tools.Listed
	Unlisted []Unlisted
}

// FailUnlisted is the error of the first backend a VM uses that couldn't
// list its images, for devvm images, which lists every backend's or none.
func (l ImageListing) FailUnlisted() error {
	if len(l.Unlisted) > 0 {
		return l.Unlisted[0].Err
	}
	return nil
}

// ListImages lists the images on every backend, and the shared tools for
// the base new VMs get on b (its backend and From). A backend that can't
// list is left out, and named in Unlisted when a VM's state names it.
func (h Housekeeping) ListImages(ctx context.Context, b *build.Env) (ImageListing, error) {
	inv, err := ReadStates(h.Dirs)
	if err != nil {
		return ImageListing{}, err
	}
	return h.listImages(ctx, b, inv)
}

func (h Housekeeping) listImages(ctx context.Context, b *build.Env, inv *Inventory) (ImageListing, error) {
	var out ImageListing
	if err := from.Check(b.From); err != nil {
		return out, err
	}
	for _, be := range h.Registry.All() {
		on := build.Images{Env: h.Builder(be, "")}
		ls, err := on.Listing(ctx)
		if err != nil {
			if !inv.Used(be.Name()) {
				continue
			}
			if cerr := backend.Check(ctx, be); cerr != nil {
				err = cerr
			}
			out.Unlisted = append(out.Unlisted, Unlisted{Backend: be.Name(), Err: err})
			continue
		}
		for _, l := range ls {
			out.Images = append(out.Images, ListedImage{Listed: l, Backend: be.Name(), images: on})
		}
	}
	var err error
	out.Tools, err = h.ShareListing(b, inv)
	return out, err
}

// StaleImages returns the images that are stale or unfinished, never one
// another devvm is building.
func StaleImages(imgs []ListedImage) []ListedImage {
	var out []ListedImage
	for _, l := range imgs {
		if l.State == build.StateStale || l.State == build.StateUnfinished {
			out = append(out, l)
		}
	}
	return out
}

// NixCacheCap is the shared Nix cache's cap as the retention settings
// give it, and why they can't be read: its default then.
func NixCacheCap(d paths.Dirs) (int64, error) {
	def, err := settings.Read(d)
	return def.Retention.NixCacheCap(), err
}

// NixCachePlan is what devvm gc deletes of the shared Nix cache: its
// least recently used files while it's over its cap, or with --nix-cache
// all of them.
type NixCachePlan struct {
	Dir   string
	Cap   int64 // nix-cache.size, or its default
	Limit int64 // what it keeps: its cap, or 0 for all of it
	Files int
	Size  int64
}

// Any reports whether gc deletes anything of the cache.
func (p NixCachePlan) Any() bool { return p.Files > 0 && (p.Limit == 0 || p.Size > p.Limit) }

// GCOpts are what devvm gc is asked for besides its default run, which
// each adds to.
type GCOpts struct {
	// Tools is --tool-images: also the current tool images no VM has, of
	// ToolNames when there are any. Making is set with it while a VM is
	// being made (LockGC): gc keeps them, and says so.
	Tools     bool
	ToolNames []string
	Making    bool
	// History is --history: also the exports, only those at least Age
	// old when it isn't 0.
	History bool
	Age     time.Duration
	// Orphans is --orphans: also what devvm keeps for VMs their backend
	// no longer has.
	Orphans bool
	// NixCache is --nix-cache: the whole shared Nix cache, not only what's
	// over its cap.
	NixCache bool
}

// GCPlan is what devvm gc deletes, gathered before it asks.
type GCPlan struct {
	Images  []ListedImage  // stale and unfinished images
	Tools   []tools.Listed // unused tool images
	Current []tools.Listed // with --tool-images, current tool images no VM has
	// Making are, with --tool-images while a VM is being made, the current
	// tool images no VM has, which gc keeps.
	Making  []tools.Listed
	Exports []Export // with --history, those old enough
	// Orphans are, with --orphans, the VMs their backend no longer has,
	// the dead links and the link sockets nothing listens on.
	Orphans  LeftoverReport
	Mise     []string            // older versions of devvm's own mise
	Packages build.StalePackages // the package cache's: sets and files nothing needs
	Nix      NixCachePlan
	// Skipped are the backends a VM uses that couldn't list their images:
	// gc leaves everything of theirs alone.
	Skipped map[string]bool
	// Warnings are what the caller says before the plan: each backend
	// skipped, and retention settings that can't be read.
	Warnings []msg.M

	leftovers Leftovers
	cache     build.Images
	share     tools.Share
	exports   Exports
}

// LockGC holds the share's lock, so no build moves a tool image in and no
// update swaps the lock while gc runs. With withTools (--tool-images) it
// then tries the lock VMs being made hold (tools.Making), and holds it so
// no VM picks its tools meanwhile; making is true when a VM is being
// made, as that VM may not list the tools it picked yet.
func (h Housekeeping) LockGC(ctx context.Context, b *build.Env, withTools bool) (unlock func(), making bool, err error) {
	unshare, err := b.Tools.LockShare(ctx)
	if err != nil {
		return nil, false, err
	}
	if !withTools {
		return unshare, false, nil
	}
	unmaking, ok, err := (tools.Making{Dirs: b.Dirs}).Exclude()
	if err != nil {
		unshare()
		return nil, false, err
	}
	if !ok {
		return unshare, true, nil
	}
	return func() { unmaking(); unshare() }, false, nil
}

// PlanGC gathers what gc deletes, by one inventory: the VMs' states,
// and with --orphans the lists of the backends they name.
func (h Housekeeping) PlanGC(ctx context.Context, b *build.Env, o GCOpts) (GCPlan, error) {
	p := GCPlan{share: tools.Share{Env: b.Tools}, exports: Exports{Dirs: b.Dirs}, leftovers: h.leftovers, cache: build.Images{Env: b}}
	inv, err := h.gcInventory(ctx, o.Orphans)
	if err != nil {
		return p, err
	}
	l, err := h.listImages(ctx, b, inv)
	if err != nil {
		return p, err
	}
	p.Images, p.Tools = StaleImages(l.Images), tools.Unused(l.Tools)
	if o.Tools {
		p.Current = p.share.NoVM(l.Tools, o.ToolNames)
		if o.Making {
			p.Current, p.Making = nil, p.Current
		}
	}
	p.skip(l.Unlisted)
	if o.History {
		es, err := p.exports.List()
		if err != nil {
			return p, err
		}
		p.Exports = ExportsOlderThan(es, o.Age, time.Now())
	}
	if o.Orphans {
		if p.Orphans, err = p.leftovers.FindIn(ctx, inv); err != nil {
			return p, err
		}
		p.Orphans = withoutSkipped(p.Orphans, p.Skipped)
	}
	if p.Mise, err = (tools.Versions{Env: b.Tools}).StaleMise(); err != nil {
		return p, err
	}
	if p.Packages, err = p.cache.Stale(ctx); err != nil {
		return p, err
	}
	p.Nix, err = p.planNixCache(b.Dirs, o.NixCache)
	return p, err
}

// gcInventory is the inventory gc plans by: the states alone, or with
// orphans the lists of the backends they name too.
func (h Housekeeping) gcInventory(ctx context.Context, orphans bool) (*Inventory, error) {
	if orphans {
		return ReadInventory(ctx, h.Dirs, h.Registry, UsedBackends)
	}
	return ReadStates(h.Dirs)
}

// skip warns for each backend a VM uses that couldn't list its images and
// leaves out its shared tool images, and those without a record naming
// their backend, which may be its: gc carries on with the other backends.
func (p *GCPlan) skip(unlisted []Unlisted) {
	if len(unlisted) == 0 {
		return
	}
	p.Skipped = map[string]bool{}
	for _, s := range unlisted {
		p.Warnings = append(p.Warnings, msg.GCBackendSkipped(s.Backend).Because(s.Err))
		p.Skipped[s.Backend] = true
	}
	keep := func(ts []tools.Listed) []tools.Listed {
		var out []tools.Listed
		for _, t := range ts {
			if t.Backend != "" && !p.Skipped[t.Backend] {
				out = append(out, t)
			}
		}
		return out
	}
	p.Tools, p.Current, p.Making = keep(p.Tools), keep(p.Current), keep(p.Making)
}

// withoutSkipped moves the orphans of a skipped backend to the backends
// that couldn't be asked, so their state stays.
func withoutSkipped(r LeftoverReport, skipped map[string]bool) LeftoverReport {
	var orphans []Orphan
	for _, o := range r.Orphans {
		if !skipped[o.Backend] {
			orphans = append(orphans, o)
		} else if !slices.Contains(r.Unknown, o.Backend) {
			r.Unknown = append(r.Unknown, o.Backend)
		}
	}
	r.Orphans = orphans
	return r
}

// planNixCache reads what the shared Nix cache holds; all is --nix-cache.
func (p *GCPlan) planNixCache(d paths.Dirs, all bool) (NixCachePlan, error) {
	capped, err := NixCacheCap(d)
	if err != nil {
		p.Warnings = append(p.Warnings, msg.RetentionUnreadable().Because(err))
	}
	n := NixCachePlan{Dir: d.NixCache(), Cap: capped, Limit: capped}
	if all {
		n.Limit = 0
	}
	n.Files, n.Size, err = proxy.NixCacheUsage(n.Dir)
	return n, err
}

// Empty reports whether gc has nothing to delete; the tool images it
// keeps while a VM is being made aren't any.
func (p GCPlan) Empty() bool {
	return len(p.Images)+len(p.Tools)+len(p.Current)+len(p.Exports)+len(p.Orphans.Orphans)+len(p.Mise) == 0 &&
		len(p.Orphans.Links)+len(p.Orphans.Sockets)+len(p.Packages.Sets)+len(p.Packages.Files) == 0 && !p.Nix.Any()
}

// GCDone is what a plan's Apply deleted, in the order it did, for its
// caller to say.
type GCDone struct {
	Forgot  []vmname.VM // the orphans forgotten, by name
	Sockets int         // the leftover sockets deleted (LeftoverSockets)
	// Images are the stale images, each deleted or, Refused, kept.
	Images []DeletedImage
	Mise   int // old mise versions
	// Packages are the package cache's files deleted, when PackagesDeleted:
	// its sets nothing needs went too.
	Packages        int
	PackagesDeleted bool
	History         int // exports
	Tools           int // unused tool images
	ToolsNoVM       int // current tool images no VM has
	NixFiles        int // the shared Nix cache's files, and what they took
	NixFreed        int64
}

// DeletedImage is a stale image Apply deleted, or one its backend refused
// with a message (on Linux hosts, an image whose disk VMs use), which
// stays.
type DeletedImage struct {
	ListedImage
	Refused bool
	Why     msg.M
}

// Apply deletes what's planned: the orphans' state, the dead links and
// sockets, the stale images, old mise versions, the package cache's
// files, the exports, the tool images and the Nix cache's files. It
// returns what it deleted up to a failure too.
func (p GCPlan) Apply(ctx context.Context) (GCDone, error) {
	var done GCDone
	if err := p.applyOrphans(ctx, &done); err != nil {
		return done, err
	}
	if err := p.applyImages(ctx, &done); err != nil {
		return done, err
	}
	if err := removeAll(p.Mise); err != nil {
		return done, err
	}
	done.Mise = len(p.Mise)
	if len(p.Packages.Sets)+len(p.Packages.Files) > 0 {
		if err := p.cache.DeletePackages(ctx, p.Packages); err != nil {
			return done, err
		}
		done.Packages, done.PackagesDeleted = len(p.Packages.Files), true
	}
	if err := p.exports.Delete(p.Exports); err != nil {
		return done, err
	}
	done.History = len(p.Exports)
	if err := p.applyTools(&done); err != nil {
		return done, err
	}
	if !p.Nix.Any() {
		return done, nil
	}
	var err error
	done.NixFiles, done.NixFreed, err = proxy.TrimNixCache(p.Nix.Dir, p.Nix.Limit)
	return done, nixLocked(err)
}

// nixLocked is err, a trim of the shared Nix cache's, as devvm says it: nix_cache_locked when a
// VM's proxy held its lock past its wait, so the trim was skipped.
func nixLocked(err error) error {
	if errors.Is(err, proxy.ErrNixLocked) {
		return msg.NixCacheLocked().Because(err)
	}
	return err
}

func (p GCPlan) applyOrphans(ctx context.Context, done *GCDone) error {
	for _, o := range p.Orphans.Orphans {
		if err := p.leftovers.Forget(ctx, o); err != nil {
			return err
		}
		done.Forgot = append(done.Forgot, o.Name)
	}
	if len(p.Orphans.Links)+len(p.Orphans.Sockets) > 0 {
		if err := p.leftovers.ForgetLinks(ctx); err != nil {
			return err
		}
		done.Sockets = len(p.Orphans.Sockets)
	}
	return nil
}

// applyImages deletes the stale images, each on its backend. One the
// backend refuses with a message stays, and gc goes on; any other failure
// ends it.
func (p GCPlan) applyImages(ctx context.Context, done *GCDone) error {
	for _, l := range p.Images {
		if err := l.images.DeleteKey(ctx, l.Key); err != nil {
			m, ok := errors.AsType[msg.M](err)
			if !ok {
				return err
			}
			done.Images = append(done.Images, DeletedImage{ListedImage: l, Refused: true, Why: m})
			continue
		}
		done.Images = append(done.Images, DeletedImage{ListedImage: l})
	}
	return nil
}

func (p GCPlan) applyTools(done *GCDone) error {
	if len(p.Tools) > 0 {
		if err := p.share.Delete(ToolFiles(p.Tools)); err != nil {
			return err
		}
		done.Tools = len(p.Tools)
	}
	if len(p.Current) > 0 {
		if err := p.share.Delete(ToolFiles(p.Current)); err != nil {
			return err
		}
		done.ToolsNoVM = len(p.Current)
	}
	return nil
}

// ToolFiles are these tool images' files in the share.
func ToolFiles(ts []tools.Listed) []string {
	files := make([]string, 0, len(ts))
	for _, t := range ts {
		files = append(files, t.File)
	}
	return files
}

// removeAll deletes these files and folders.
func removeAll(paths []string) error {
	for _, p := range paths {
		if err := os.RemoveAll(p); err != nil {
			return err
		}
	}
	return nil
}

// Retained is what Retain deleted.
type Retained struct{ Exports, Tools, Orphans, NixFiles int }

// Any reports whether it deleted anything.
func (r Retained) Any() bool { return r.Exports+r.Tools+r.Orphans+r.NixFiles > 0 }

// Retain applies the retention settings r, each to what it names and
// nothing else: exports older than history.age, then all but the newest
// history.count; current tool images no VM has had for tools.unused-age;
// with orphans, what devvm keeps for VMs their backend hasn't had for
// orphans.age (it asks the backends); the shared Nix cache's least
// recently used files while it's over nix-cache.size (or its default).
// Never a VM, an image, or a tool image any VM's state lists. logs.runs
// is the run log's own.
func (h Housekeeping) Retain(ctx context.Context, b *build.Env, r settings.Retention, orphans bool) (Retained, error) {
	var done Retained
	now := time.Now()
	n, err := retainExports(b.Dirs, r, now)
	if err != nil {
		return done, err
	}
	done.Exports = n
	if done.NixFiles, _, err = proxy.TrimNixCache(b.Dirs.NixCache(), r.NixCacheCap()); err != nil {
		return done, nixLocked(err)
	}
	if r.ToolsAge > 0 {
		if done.Tools, err = h.retainTools(ctx, b, r.ToolsAge, now); err != nil {
			return done, err
		}
	}
	if orphans && r.OrphansAge > 0 {
		if done.Orphans, err = h.retainOrphans(ctx, r.OrphansAge, now); err != nil {
			return done, err
		}
	}
	return done, nil
}

func retainExports(d paths.Dirs, r settings.Retention, now time.Time) (int, error) {
	if r.ExportsAge == 0 && r.ExportsCount == 0 {
		return 0, nil
	}
	x := Exports{Dirs: d}
	es, err := x.List()
	if err != nil {
		return 0, err
	}
	gone := ExportsToRetire(es, r, now)
	return len(gone), x.Delete(gone)
}

// retainTools deletes the current tool images no VM has had for age,
// holding the share's lock throughout, then the lock VMs being made hold,
// as gc --tool-images does. While a VM is being made it deletes none, and
// leaves their idle records as they are.
func (h Housekeeping) retainTools(ctx context.Context, b *build.Env, age time.Duration, now time.Time) (int, error) {
	unlock, making, err := h.LockGC(ctx, b, true)
	if err != nil {
		return 0, err
	}
	defer unlock()
	if making {
		return 0, nil
	}
	inv, err := ReadStates(b.Dirs)
	if err != nil {
		return 0, err
	}
	ts, err := h.ShareListing(b, inv)
	if err != nil {
		return 0, err
	}
	share := tools.Share{Env: b.Tools}
	idle, err := share.Idle(ts, age, now)
	if err != nil || len(idle) == 0 {
		return 0, err
	}
	return len(idle), share.Delete(ToolFiles(idle))
}

// retainOrphans forgets the VMs their backend no longer has whose state
// hasn't changed for age.
func (h Housekeeping) retainOrphans(ctx context.Context, age time.Duration, now time.Time) (int, error) {
	l := h.leftovers
	rep, err := l.Find(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, o := range rep.Orphans {
		if since := now.Sub(o.Changed); since < age || since < 0 {
			continue
		}
		if err := l.Forget(ctx, o); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
