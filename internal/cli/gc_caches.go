package cli

import (
	"context"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/vm"
)

// gcCaches frees devvm's share of each backend's own caches (gc
// --caches): what each backend can tell only devvm's builds put there and
// no image or VM of devvm's needs. It lists each backend's apart and asks
// for each; it never runs a backend's own prune, which reaches the user's
// own entries too. Never one gc skipped, as it couldn't list its images.
func gcCaches(ctx context.Context, a *app, env *vm.Env, o gcOpts, skipped map[string]bool) error {
	u := a.u
	reg := env.Registry
	for _, be := range reg.All() {
		if skipped[be.Name()] {
			continue
		}
		stop := u.Status(msg.ReadingSpace())
		entries, ok := cacheEntries(ctx, reg.Host, be)
		stop()
		if !ok || len(entries) == 0 {
			continue
		}
		var size int64
		for _, e := range entries {
			size += e.Size
		}
		u.Say(msg.GCCache(be.Name(), len(entries), humanSize(size)))
		for _, e := range entries {
			u.Say(msg.GCCacheEntry(humanSize(e.Size), e.What))
		}
		if !o.force {
			yes, err := u.Confirm(msg.GCCacheConfirm(be.Name()).AnsweredBy("--force"))
			if err != nil {
				return err
			}
			if !yes {
				u.Info(msg.NothingDeleted())
				continue
			}
		}
		if err := be.DeleteCache(ctx, entries); err != nil {
			return err
		}
		u.Info(msg.DeletedCache(be.Name(), len(entries)))
	}
	return nil
}

// cacheEntries is devvm's share of a backend's caches; ok is false when
// the backend can't be asked.
func cacheEntries(ctx context.Context, host platform.Host, be backend.Backend) ([]backend.CacheEntry, bool) {
	ctx, cancel := context.WithTimeout(ctx, spaceTimeout(host, be.Name()))
	defer cancel()
	if backend.Check(ctx, be) != nil {
		return nil, false
	}
	entries, err := be.DevvmCache(ctx)
	return entries, err == nil
}
