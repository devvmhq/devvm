// Package build builds images through a backend and keeps their records:
// base, and layers on it (devvm's built-in ones and the user's). An image is built once per key and
// reused while its key matches; rebuilding one bumps its rebuild counter,
// which changes its key.
//
// Env is what images are built with, and what the groups share: keys,
// layers, the features there are, base's C library. Builds builds and
// rebuilds images; Images lists and deletes them, with their records;
// Choice chooses base's features.
package build

import (
	"cmp"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/from"
	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/settings"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/tools"
	"github.com/devvmhq/devvm/internal/trust"
)

// Env is what images on one backend are built with, shared by Builds,
// Images and Choice; its methods are what they share: image keys and
// chains, the user's layers, the features there are and those chosen,
// base's C library and deleting an image.
type Env struct {
	Dirs    paths.Dirs
	Assets  fs.FS // devvm's own files (assets.FS): layers/, features/, guest/ca.sh
	Backend backend.Backend
	Review  backend.Review // what Backend's features are checked against
	Tools   *tools.Env     // the shared tools image names hold
	// From is the container image base starts from (--from); "" for the
	// backend's own Ubuntu. Its images are apart from other bases'.
	From string
	// Version is devvm's version, which each image's record keeps.
	Version string
	// Lookup looks up From's digest when building; nil for the internet's
	// (from.NewLookup).
	Lookup *from.Lookup
	// Reporter is where it prints and asks, its caller's, and the run
	// log, for lines that aren't messages.
	msg.Reporter
	// Pick asks which features base gets, each as chosen so far.
	Pick func([]Feature) []Feature
	// Force is a rebuild's --force: it replaces the images built on the
	// one it rebuilds without asking, the answer where it can't ask.
	Force bool
	// FreeSpace returns the free space on the volume holding a path
	// (platform.Host's), checked before each build; nil checks none.
	FreeSpace func(path string) (uint64, error)
	// WaitNote is how long a build waits for another devvm building the
	// same image before it says so; 0 for buildWaitNote.
	WaitNote time.Duration

	// looked is set once lookUp has run, which holds lookMu: builds on
	// one Env may run at once.
	lookMu sync.Mutex
	looked bool
	// fresh is what the backend pinned for base in this run, "" for
	// nothing; fellBack is set once a build that didn't boot from it
	// pinned an older one (fallBack), which happens once a run. mu
	// guards both.
	mu       sync.Mutex
	fresh    string
	fellBack bool
}

// Builds builds images, and builds them again.
type Builds struct{ *Env }

// Images is the backend's images and their records: listing and deleting
// them.
type Images struct{ *Env }

// Choice is base's features: asking which it gets, or setting them.
type Choice struct{ *Env }

// Names reads and checks images' names against the layers there are.
type Names struct{ *Env }

// Key returns an image's key as it is now.
func (b *Env) Key(name image.Name) (string, error) {
	c, err := b.Chain(name)
	return c.Key(), err
}

// Chain returns an image's chain as it is now.
func (b *Env) Chain(name image.Name) (image.Chain, error) {
	in, err := b.inputs()
	if err != nil {
		return nil, err
	}
	return b.chain(in, name)
}

// lookUp looks up From's digest and has the backend pin what base starts
// from when nothing is pinned yet (PinOrigin), once a run, before any key
// that builds is worked out: every key holds base's.
func (b Builds) lookUp(ctx context.Context) error {
	b.lookMu.Lock()
	defer b.lookMu.Unlock()
	if b.looked {
		return nil
	}
	if b.From != "" {
		l := b.Lookup
		if l == nil {
			l = from.NewLookup(b.Dirs, b.Reporter, b.proxy())
		}
		if err := l.Refresh(ctx, b.From); err != nil {
			return err
		}
	}
	if err := b.pin(ctx, backend.PinFirst); err != nil {
		return err
	}
	b.looked = true
	return nil
}

// pin has the backend pin what base starts from, as how asks, and says
// what it pinned.
func (b Builds) pin(ctx context.Context, how backend.Pin) error {
	f, err := b.fromAsRecorded(b.From)
	if err != nil {
		return err
	}
	pinned, err := b.Backend.PinOrigin(ctx, f, how)
	if err != nil {
		return err
	}
	if pinned != "" {
		b.Info(msg.OriginPinned(pinned))
		b.mu.Lock()
		b.fresh = pinned
		b.mu.Unlock()
	}
	return nil
}

// fallBack is err, the end of a build of name: base's that didn't boot
// (backend.ErrBoot) from what the backend pinned in this run says so, has
// the backend pin the newest older one (PinOrigin's PinOlder) and builds
// base on that, once a run. Anything else is err as it is: what was
// pinned before this run booted then.
func (b Builds) fallBack(ctx context.Context, name image.Name, err error) error {
	if !errors.Is(err, backend.ErrBoot) || name != image.Base || b.From != "" {
		return err
	}
	b.mu.Lock()
	failed := b.fresh
	again := failed != "" && !b.fellBack
	b.fellBack = true
	b.mu.Unlock()
	if !again {
		return err
	}
	b.Warn(msg.OriginNoBoot(failed))
	if perr := b.pin(ctx, backend.PinOlder); perr != nil {
		return errors.Join(err, perr)
	}
	return b.buildOnce(ctx, name)
}

// Layers returns base, then the built-in layers and the user's, sorted:
// the scripts in the layers folder with names a layer can have (not a
// shared tool's or a built-in layer's).
func (b *Env) Layers() []string {
	names := slices.Concat([]string{image.Base}, image.Builtins)
	entries, _ := os.ReadDir(b.Dirs.Layers()) // no folder: no layers
	for _, e := range entries {
		n, ok := strings.CutSuffix(e.Name(), ".sh")
		if ok && image.LayerName(n) && !image.IsBuiltin(n) && b.isLayer(n) {
			names = append(names, n)
		}
	}
	slices.Sort(names[1:])
	return names
}

// LayerData is a layer's script as its build runs it: devvm's own for a
// built-in layer, else the user's, each with the prelude before it. A
// user's layer that's gone is an error matching fs.ErrNotExist.
func (b *Env) LayerData(layer string) ([]byte, error) {
	if image.IsBuiltin(layer) {
		return image.OwnScript(b.Assets, "layers/"+layer+".sh")
	}
	data, err := os.ReadFile(b.userLayer(layer))
	if err != nil {
		return nil, err
	}
	return image.WithPrelude(b.Assets, data)
}

func (b *Env) userLayer(layer string) string {
	return filepath.Join(b.Dirs.Layers(), layer+".sh")
}

func (b *Env) isLayer(layer string) bool {
	if image.IsBuiltin(layer) {
		return true
	}
	fi, err := os.Stat(b.userLayer(layer))
	return err == nil && fi.Mode().IsRegular()
}

// Parse reads an image's name as the user gives it (image.ParseName),
// refusing a part that isn't a built-in layer or one of the user's, as
// Check does: the one place a name the user gives is read.
func (b Names) Parse(s string) (image.Name, error) {
	return image.ParseName(s, b.checkLayer)
}

// Check refuses an image name with a part that isn't base, a built-in
// layer or one of the user's layers, bottom first: a shared tool's name
// says shared tools go in a VM, not an image. Names become file names:
// nothing else gets past it. An empty name is one empty part.
func (b Names) Check(name image.Name) error {
	if name == "" {
		return b.checkLayer("")
	}
	for _, p := range name.Layers() {
		if err := b.checkLayer(p); err != nil {
			return err
		}
	}
	return nil
}

// checkLayer refuses a part of an image's name that isn't base, a
// built-in layer or one of the user's layers.
func (b Names) checkLayer(p string) error {
	switch {
	case image.IsTool(p):
		return msg.ToolNotLayer(p)
	case p != image.Base && (!image.LayerName(p) || !b.isLayer(p)):
		return msg.UnknownLayer(p, strings.Join(b.Layers(), " "))
	}
	return nil
}

// ToolTarget is what the shared tools of a new VM on this backend are
// built for: base's C library as it is now, its record's (gnu when it
// isn't built), with the glibc version its record has, for musl its
// digest, and the backend's format.
func (b *Env) ToolTarget() (tools.Target, error) {
	tg := tools.Target{Libc: "gnu", Format: b.Backend.ToolFormat()}
	key, err := b.Key(image.Base)
	if err != nil {
		return tools.Target{}, err
	}
	rec, err := state.ReadImage(b.Dirs, key)
	if _, corrupt := errors.AsType[*state.CorruptError](err); corrupt || errors.Is(err, fs.ErrNotExist) {
		return tg, nil
	} else if err != nil {
		return tools.Target{}, err
	}
	if rec.Libc != "" {
		tg.Libc = rec.Libc
	}
	switch tg.Libc {
	case "gnu":
		tg.Glibc = rec.Glibc
	case "musl":
		tg.Base = rec.Digest
	}
	return tg, nil
}

// Build builds an image, and the images under it, unless they're built.
// Its layers are checked again (Names' Check): one may be gone since its
// name was read.
func (b Builds) Build(ctx context.Context, name image.Name) error {
	if err := Names(b).Check(name); err != nil {
		return err
	}
	if err := b.checkProxy(ctx); err != nil {
		return err
	}
	// Every key holds base's, so its features are chosen first.
	if err := Choice(b).Choose(); err != nil {
		return err
	}
	return b.build(ctx, name)
}

// checkProxy checks the company proxy builds go through, with
// config.toml's password saved (the shared tools' Proxy), before anything
// is asked or built, as a new VM's start does.
func (b Builds) checkProxy(ctx context.Context) error {
	if b.proxy() == nil {
		return nil
	}
	_, err := b.proxy().Upstream(ctx)
	return err
}

// proxy is the company proxy From's registry is asked through (the shared
// tools' Proxy); nil for the environment's.
func (b *Env) proxy() *settings.CompanyProxy {
	if b.Tools == nil {
		return nil
	}
	return b.Tools.Proxy
}

// build builds name and what it's built on, unless they're built, base
// falling back to an older origin when it doesn't boot (fallBack) once its
// first build has let go of its lock.
func (b Builds) build(ctx context.Context, name image.Name) error {
	return b.fallBack(ctx, name, b.buildOnce(ctx, name))
}

func (b Builds) buildOnce(ctx context.Context, name image.Name) error {
	if err := b.lookUp(ctx); err != nil {
		return err
	}
	in, err := b.inputs()
	if err != nil {
		return err
	}
	plans, err := b.plan(in, name)
	if err != nil {
		return err
	}
	chain, p := chainOf(plans), plans[len(plans)-1]
	if p.Missing != "" {
		// Deleted since Names' Check.
		return msg.UnknownLayer(strings.TrimSuffix(p.Missing, ".sh"), strings.Join(b.Layers(), " "))
	}
	digest := ""
	if in.from != nil {
		digest = in.from.Digest
	}
	key := chain.Key()
	// What its key hashes is what it's given.
	spec := backend.ImageSpec{Image: name.String(), Key: key, Parent: p.Parent, From: p.from, Recipe: p.Recipe, Net: backend.NetInternet}
	// One devvm at a time builds it: another waits, then finds it built,
	// or left over from a build that didn't finish.
	unlock, err := b.lockBuild(ctx, name, key)
	if err != nil {
		return err
	}
	defer unlock()
	// What building it needs, only when it's built: an image the backend
	// has, with its record, needs none of it.
	done, leftover, err := b.built(ctx, key)
	if err != nil || done {
		return err
	}
	if err := b.Review.Require(b.Backend, name.String(), spec.Features()...); err != nil {
		return err
	}
	if leftover {
		b.Info(msg.ImageLeftover(name.String(), key))
		if err := b.deleteLocked(ctx, key, false); err != nil {
			return err
		}
	}
	for _, f := range p.unknown {
		b.Warn(msg.UnknownFeature(f))
	}
	libc, glibc := "", ""
	if parent := name.Parent(); parent != "" {
		if err := b.build(ctx, parent); err != nil {
			return err
		}
		rec, err := state.ReadImage(b.Dirs, spec.Parent)
		if err != nil {
			return err
		}
		libc, glibc = rec.Libc, rec.Glibc
	}
	// The user's CAs, trusted while it builds: the image keeps none of
	// them, so its key holds none.
	cas, err := trust.Read(b.Dirs.TrustedCAs())
	if err != nil {
		return err
	}
	if spec.Trust, err = cas.Build(b.Assets); err != nil {
		return err
	}
	// Before anything of this image is made; its parent is built.
	need := func(f backend.Floors) uint64 { return f.Image }
	if name == image.Base {
		need = func(f backend.Floors) uint64 { return f.Base }
	}
	if err := (backend.Space{Free: b.FreeSpace}).Check(b.Backend, name.String(), need); err != nil {
		return err
	}

	switch {
	case b.From != "":
		b.Info(msg.BuildingImageFrom(b.From, name.String(), key))
	case name == image.Base:
		b.Info(msg.BuildingImage(name.String(), key))
		b.Say(msg.BaseBuildTime())
	default:
		b.Info(msg.BuildingImage(name.String(), key))
	}
	b.LogNote()
	out, err := b.buildWith(ctx, spec, name)
	if err != nil {
		return err
	}
	// The record is written last: an image without one didn't finish.
	// Every image on a --from base records it and its digest.
	rec := state.Image{Name: name.String(), Chain: chain.String(), From: b.From, Digest: digest, Libc: libc, Glibc: glibc, Built: time.Now().UTC().Truncate(time.Second), MadeBy: b.Version}
	if name == image.Base {
		rec.Libc, rec.Glibc = out.Libc, out.Glibc
		if b.From == "" {
			rec.Digest = out.Origin
		}
	}
	if rec.Libc == "" {
		rec.Libc = "gnu"
	}
	if err := state.WriteImage(b.Dirs, key, rec); err != nil {
		return err
	}
	b.sayFitted(out.Fitted)
	b.Info(msg.ImageReady(name.String()))
	return nil
}

// buildWith has the backend build spec, the image named name: base from
// the backend's own Ubuntu with its packages (basePackages), which a
// recording stores once the image is built, before its record.
func (b Builds) buildWith(ctx context.Context, spec backend.ImageSpec, name image.Name) (backend.Built, error) {
	if name != image.Base || b.From != "" {
		return b.Backend.BuildImage(ctx, spec)
	}
	pk, err := cache{b.Dirs, b.Assets}.basePackages(spec.Key)
	if err != nil {
		return backend.Built{}, err
	}
	spec.Packages = pk.spec
	out, err := b.Backend.BuildImage(ctx, spec)
	if pk.taken == nil {
		return out, err
	}
	if err != nil {
		pk.taken.discard()
		return out, err
	}
	return out, pk.taken.commit(ctx, spec.Key, b.LogWriter())
}

// sayFitted says what base got when the backend fitted it to the host
// (backend.Built's Fitted); nothing when it didn't.
func (b Builds) sayFitted(f backend.Size) {
	if f != (backend.Size{}) {
		b.Info(msg.BaseFitted(strconv.Itoa(f.CPUs), strconv.FormatFloat(f.MemoryGiB, 'g', -1, 64)))
	}
}

// buildWaitNote is how long a build waits for another devvm building the
// same image before it says so, when Env's WaitNote is 0.
const buildWaitNote = 3 * time.Second

// lockBuild holds the lock of the image with key, named name, while it's
// built, waiting for another devvm that holds it and saying so. The
// system lets go of it when its devvm ends, however it ends.
func (b Builds) lockBuild(ctx context.Context, name image.Name, key string) (unlock func(), err error) {
	path, err := state.ImageLock(b.Dirs, key)
	if err != nil {
		return nil, err
	}
	after := cmp.Or(b.WaitNote, buildWaitNote)
	return platform.LockFileWaiting(ctx, path, after, func() { b.Info(msg.ImageBuildWaiting(name.String())) })
}

// bumpRebuild changes the rebuild counter of the image named name on this
// backend and base, under the counters' lock: two devvms rebuilding at
// once each keep their change.
func (b Builds) bumpRebuild(ctx context.Context, name image.Name) error {
	if err := os.MkdirAll(b.Dirs.ImageLocks(), 0o700); err != nil {
		return err
	}
	unlock, err := platform.LockFile(ctx, b.Dirs.RebuildsLock())
	if err != nil {
		return err
	}
	defer unlock()
	return state.BumpRebuild(b.Dirs, b.Backend.Name(), b.From, name.String())
}

// lockFree holds the lock of the image with key, for deleting it, unless
// another devvm is building it (image_building).
func (b *Env) lockFree(key string) (unlock func(), err error) {
	path, err := state.ImageLock(b.Dirs, key)
	if err != nil {
		return nil, err
	}
	unlock, ok, err := platform.TryLockFile(path)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, msg.ImageBuilding(key)
	}
	return unlock, nil
}

// building reports whether another devvm is building the image with key.
func (b *Env) building(key string) (bool, error) {
	path, err := state.ImageLock(b.Dirs, key)
	if err != nil {
		return false, err
	}
	return platform.LockHeld(path)
}

// built reports whether the image with key is built: the backend has it,
// and its record. One without a record (leftover) is left over from a
// build that didn't finish, for the caller to delete; the caller holds
// its lock, so no other devvm is building it.
func (b Builds) built(ctx context.Context, key string) (done, leftover bool, err error) {
	keys, err := b.Backend.Images(ctx)
	if err != nil || !slices.Contains(keys, key) {
		return false, false, err
	}
	_, err = state.ReadImage(b.Dirs, key)
	if err == nil {
		return true, false, nil
	}
	if _, corrupt := errors.AsType[*state.CorruptError](err); !corrupt && !errors.Is(err, fs.ErrNotExist) {
		return false, false, err
	}
	return false, true, nil
}

// deleteImage deletes the image with key and its record, unless another
// devvm is building it (image_building), then its lock's file, which a
// build of it makes again.
func (b *Env) deleteImage(ctx context.Context, key string) error {
	unlock, err := b.lockFree(key)
	if err != nil {
		return err
	}
	defer unlock()
	return b.deleteLocked(ctx, key, true)
}

// deleteLocked deletes the image with key and its record, its lock held,
// and the lock's file when unlocking follows (a build that deletes a
// leftover keeps it).
func (b *Env) deleteLocked(ctx context.Context, key string, lockFile bool) error {
	if err := b.Backend.DeleteImage(ctx, key); err != nil {
		return err
	}
	if err := state.RemoveImage(b.Dirs, key); err != nil {
		return err
	}
	if !lockFile {
		return nil
	}
	path, err := state.ImageLock(b.Dirs, key)
	if err != nil {
		return err
	}
	// Safe while held: a devvm that then gets the lock on the removed file
	// tries again (platform.LockFile).
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Image is a built image as List finds it.
type Image struct {
	Key    string
	Name   string // "" for one without a record: a build that didn't finish
	From   string
	Digest string // what its base started from, as its record has it
	Chain  image.Chain
	// Current is whether its key is still what its inputs hash to. One
	// that isn't is stale: nothing new is built on it.
	Current bool
	// Building is whether another devvm is building it: it has no record
	// and its lock is held. One without a record that isn't is unfinished.
	Building bool
}

// List returns the backend's images, with their records.
func (b Images) List(ctx context.Context) ([]Image, error) {
	keys, err := b.Backend.Images(ctx)
	if err != nil {
		return nil, err
	}
	in, err := b.inputs()
	if err != nil {
		return nil, err
	}
	var out []Image
	for _, key := range keys {
		rec, err := state.ReadImage(b.Dirs, key)
		if _, corrupt := errors.AsType[*state.CorruptError](err); corrupt || errors.Is(err, fs.ErrNotExist) {
			building, err := b.building(key)
			if err != nil {
				return nil, err
			}
			out = append(out, Image{Key: key, Building: building})
			continue
		} else if err != nil {
			return nil, err
		}
		img := Image{Key: key, Name: rec.Name, From: rec.From, Digest: rec.Digest, Chain: image.ParseChain(rec.Chain)}
		// Each against its own base's inputs, as last recorded.
		if in.from, err = b.fromAsRecorded(rec.From); err != nil {
			return nil, err
		}
		if c, err := b.chain(in, image.Name(rec.Name)); err == nil && c.Key() == key {
			img.Current = true
		}
		out = append(out, img)
	}
	return out, nil
}

// Rebuild builds an image again, from its parent as it is: its rebuild
// counter changes, so it gets a new key, and base first has the backend
// pin the newest of what it starts from (PinOrigin's PinNewest). Then it
// deletes the stale images
// (the old one and those built on it among them) that no VM uses. When
// images are built on it, it asks first, and where it can't ask returns
// the question's refusal, unless Force.
func (b Builds) Rebuild(ctx context.Context, name image.Name) error {
	if err := Names(b).Check(name); err != nil {
		return err
	}
	if err := b.checkProxy(ctx); err != nil {
		return err
	}
	if err := Choice(b).Choose(); err != nil {
		return err
	}
	old, err := b.Key(name)
	if err != nil {
		return err
	}
	imgs, err := Images(b).List(ctx)
	if err != nil {
		return err
	}
	var above []string
	for _, img := range imgs {
		if img.Current && img.Name != name.String() && slices.Contains(img.Chain, image.Link{Name: name.String(), Key: old}) {
			above = append(above, img.Name)
		}
	}
	if len(above) > 0 {
		b.Say(msg.RebuildReplaces(strings.Join(above, " "), name.String()))
		if !b.Force {
			ok, err := b.Confirm(msg.Continue().AnsweredBy("--force"))
			if err != nil {
				return err
			}
			if !ok {
				b.Info(msg.NothingChanged())
				return nil
			}
		}
	}
	// Base starts again from the newest of what it starts from.
	if name == image.Base {
		if err := b.pin(ctx, backend.PinNewest); err != nil {
			return err
		}
	}
	if err := b.bumpRebuild(ctx, name); err != nil {
		return err
	}
	if err := b.build(ctx, name); err != nil {
		return err
	}
	if imgs, err = Images(b).List(ctx); err != nil {
		return err
	}
	for _, img := range imgs {
		// One another devvm is building isn't left over.
		if img.Current || img.Building {
			continue
		}
		if err := b.deleteImage(ctx, img.Key); err != nil {
			// One a VM uses, or one another devvm has started building
			// since, stays, saying so.
			if m, ok := errors.AsType[msg.M](err); ok {
				b.Warn(m)
				continue
			}
			return err
		}
		if img.Name == "" {
			b.Info(msg.DeletedUnfinishedBuild(img.Key))
		} else {
			b.Info(msg.DeletedOldImage(img.Name, img.Key))
		}
	}
	return nil
}

// Outdated returns the images a VM was made from that have changed since,
// base first: chain is the VM's, as its state records it. A --from base's
// digest is the one last recorded, so it looks nothing up.
func (b Builds) Outdated(chain string) ([]string, error) {
	in, err := b.inputs()
	if err != nil {
		return nil, err
	}
	return b.outdated(in, image.ParseChain(chain))
}

// WarnIfOutdated warns when an image a VM was made from has changed since
// (Outdated), naming devvm update. It never changes the VM.
func (b Builds) WarnIfOutdated(chain string) error {
	changed, err := b.Outdated(chain)
	if err != nil {
		return err
	}
	if len(changed) > 0 {
		b.Warn(msg.VMOutdated(strings.Join(changed, ", ")))
	}
	return nil
}
