package cli

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/devvmhq/devvm/internal/build"
	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/settings"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/tools"
	"github.com/devvmhq/devvm/internal/ui"
	"github.com/devvmhq/devvm/internal/vm"
)

// gcOpts are devvm gc's flags: what it's asked for besides its default
// run (vm.GCOpts), and what it does after.
type gcOpts struct {
	vm.GCOpts
	// olderThan is --older-than as written, which checkArgs parses into
	// Age.
	olderThan string
	// force is --force: gc deletes without asking, which it needs without
	// a terminal.
	force bool
	// caches is --caches: then devvm's share of the backends' own
	// caches, each backend asked for apart, only in a terminal.
	caches bool
	// retention is --retention: then the retention settings, at once.
	retention bool
}

// newGCCmd is devvm gc.
func newGCCmd(a *app) *cobra.Command {
	var o gcOpts
	c := &cobra.Command{
		Use: "gc [--tool-images [TOOL...]]",
		Annotations: about{
			docs: "using/images#cleaning-up", group: groupSpace,
			noJSON: "it deletes",
		}.annotations(),
		Short: "Delete stale images, unused tools, old files and the Nix cache over its cap",
		Long: "Delete the stale and unfinished images, the shared tools devvm images lists as unused, " +
			"the files devvm itself no longer needs, and the shared Nix cache's least recently used files while it's over its cap, " +
			"after listing them and asking in a terminal without --no-input. " +
			"Its flags delete more, such as --tool-images for the current shared tool images no VM has. " +
			"gc never deletes a VM.",
		Example: examples(
			"devvm gc", "delete stale images and unused tools, after asking",
			"devvm gc --tool-images", "also the current shared tool images no VM has",
			"devvm gc --history --older-than 30d", "also the history exports 30 days old or more", "devvm gc --force", "delete without asking, as a script must",
		),
		Args: func(_ *cobra.Command, args []string) error { return o.checkArgs(args) },
		RunE: func(cmd *cobra.Command, args []string) error {
			o.ToolNames = args
			return failed(gc(cmd.Context(), a, o), msg.GCFailed)
		},
	}
	f := c.Flags()
	f.BoolVar(&o.Tools, "tool-images", false, "also delete the current shared tool images no VM has, or only those of the TOOLs named")
	addForceFlag(f, &o.force)
	f.BoolVar(&o.History, "history", false, "also delete the saved exports of agents' history")
	f.StringVar(&o.olderThan, "older-than", "", "with --history, only those older than `DURATION`, such as 30d")
	f.BoolVar(&o.NixCache, "nix-cache", false, "also delete the whole shared Nix cache")
	f.BoolVar(&o.Orphans, "orphans", false, "also delete what devvm keeps for VMs their backend no longer has")
	f.BoolVar(&o.caches, "caches", false, "then free devvm's share of Lima's own caches, after asking")
	f.BoolVar(&o.retention, "retention", false, "then apply the retention settings")
	return c
}

// checkArgs takes tool names, by the names images have them by, only
// with --tool-images, and an age only with --history.
func (o *gcOpts) checkArgs(args []string) error {
	if o.olderThan != "" {
		if !o.History {
			return msg.GCOlderThanNeedsHistory()
		}
		age, err := state.ParseDuration(o.olderThan)
		if err != nil {
			return err
		}
		o.Age = age
	}
	if len(args) > 0 && !o.Tools {
		return msg.GCNamesNeedTools(args[0])
	}
	for _, a := range args {
		if !slices.Contains(image.Toolchains, a) && !slices.Contains(image.Agents, a) {
			return msg.UnknownTool(a, strings.Join(slices.Concat(image.Toolchains, image.Agents), " "))
		}
	}
	return nil
}

// gc lists what's stale or unused, asks in a terminal, and deletes it,
// then applies retention and frees the backends' caches when asked. One
// registry, the run's, serves it all.
func gc(ctx context.Context, a *app, o gcOpts) error {
	env, err := a.vmEnv()
	if err != nil {
		return err
	}
	b, err := a.builder()
	if err != nil {
		return err
	}
	b.From = fromRef("")
	h := vm.Housekeeping{Env: env}
	skipped, err := gcLocked(ctx, a, h, b, o)
	if err != nil {
		return err
	}
	if o.retention {
		if err := gcRetention(ctx, a, h, b); err != nil {
			return err
		}
	}
	if o.caches {
		return gcCaches(ctx, a, env, o, skipped)
	}
	return nil
}

// gcLocked is gcRun holding the share's lock throughout, and with
// --tool-images the lock VMs being made hold (vm.Housekeeping.LockGC).
// While one is being made, gc keeps every current tool no VM has, as that
// VM may not list the tools it picked yet.
func gcLocked(ctx context.Context, a *app, h vm.Housekeeping, b *build.Env, o gcOpts) (map[string]bool, error) {
	unlock, making, err := h.LockGC(ctx, b, o.Tools)
	if err != nil {
		return nil, err
	}
	defer unlock()
	o.Making = making
	return gcRun(ctx, a, h, b, o)
}

// gcRetention applies the retention settings now, the VMs' leftovers
// included, saying what it deleted in one line.
func gcRetention(ctx context.Context, a *app, h vm.Housekeeping, b *build.Env) error {
	u := a.u
	def, err := settings.Read(b.Dirs)
	if err != nil {
		return err
	}
	r := def.Retention
	if r.Off() {
		u.Info(msg.RetentionOff())
		return nil
	}
	unlock, ok := lockRetention(b.Dirs)
	if !ok {
		return msg.RetentionBusy()
	}
	defer unlock()
	done, err := h.Retain(ctx, b, r, true)
	touchRetention(b.Dirs)
	if err != nil {
		return err
	}
	u.Info(retainedReport(done))
	return nil
}

// gcRun is gc's run but the backends' caches and retention: it lists what
// it deletes, asks in a terminal, and deletes it. It returns the backends
// it skipped, as they couldn't list their images.
func gcRun(ctx context.Context, a *app, h vm.Housekeeping, b *build.Env, o gcOpts) (map[string]bool, error) {
	u := a.u
	done := u.Status(msg.ReadingImages())
	p, err := h.PlanGC(ctx, b, o.GCOpts)
	done()
	if err != nil {
		return nil, err
	}
	for _, w := range p.Warnings {
		u.Warn(w)
	}
	if p.Empty() {
		showOrphans(u, p.Orphans)
		if !showMaking(u, p) {
			u.Info(msg.NothingToClean())
		}
		return p.Skipped, nil
	}
	showGC(u, p)
	showMaking(u, p)
	if !o.force {
		yes, err := u.Confirm(msg.GCConfirm().AnsweredBy("--force"))
		if err != nil || !yes {
			if err == nil {
				u.Info(msg.NothingDeleted())
			}
			return p.Skipped, err
		}
	}
	deleted, err := p.Apply(ctx)
	showGCDone(u, deleted)
	return p.Skipped, err
}

// showMaking says which current tool images no VM has gc keeps because a
// VM is being made, reporting whether there were any.
func showMaking(u *ui.UI, p vm.GCPlan) bool {
	if len(p.Making) == 0 {
		return false
	}
	u.Info(msg.GCToolsMaking(len(p.Making), tools.Label(vm.ToolFiles(p.Making))))
	return true
}

// showOrphans lists the VMs gc --orphans forgets, then says which it
// leaves alone and why.
func showOrphans(u *ui.UI, r vm.LeftoverReport) {
	if orphans := r.Orphans; len(orphans) > 0 {
		u.Say(msg.GCOrphans(len(orphans)))
		for _, o := range orphans {
			u.Say(msg.GCOrphan(o.Backend, o.Dir, o.Name))
		}
	}
	if links := r.Links; len(links) > 0 {
		u.Say(msg.GCDeadLinks(len(links)))
		for _, l := range links {
			u.Say(msg.GCDeadLink(l.Name, strconv.Itoa(l.Port), l.Target.String(), l.Client))
		}
	}
	if n := len(r.Sockets); n > 0 {
		u.Say(msg.GCLinkSockets(n))
	}
	for _, b := range r.Unknown {
		u.Note(msg.GCOrphansUnknown(b))
	}
	for _, n := range r.Elsewhere {
		u.Note(msg.GCOrphanElsewhere(n))
	}
	for _, n := range r.Recent {
		u.Note(msg.GCOrphanRecent(n))
	}
}

// showGC lists what gc deletes.
func showGC(u *ui.UI, p vm.GCPlan) {
	showOrphans(u, p.Orphans)
	if len(p.Images) > 0 {
		u.Say(msg.GCImages())
		for _, l := range p.Images {
			if l.Name == "" {
				u.Say(msg.GCUnfinished(l.Key))
			} else {
				u.Say(msg.GCImage(l.Name, l.Key))
			}
		}
	}
	if len(p.Tools) > 0 {
		u.Say(msg.GCTools(len(p.Tools), humanSize(toolsSize(p.Tools))))
	}
	if len(p.Current) > 0 {
		u.Say(msg.GCToolsNoVM(len(p.Current), humanSize(toolsSize(p.Current)), tools.Label(vm.ToolFiles(p.Current))))
	}
	if len(p.Mise) > 0 {
		u.Say(msg.GCMise(len(p.Mise), humanSize(dirsSize(p.Mise))))
	}
	if n := len(p.Packages.Files); n > 0 || len(p.Packages.Sets) > 0 {
		u.Say(msg.GCPackages(n, humanSize(p.Packages.Size)))
	}
	showNixCache(u, p.Nix)
	if len(p.Exports) > 0 {
		u.Say(msg.GCHistory(len(p.Exports), humanSize(vm.ExportsSize(p.Exports))))
		for _, e := range p.Exports {
			u.Say(msg.GCHistoryExport(exportDate(e.At), humanSize(e.Size), e.VM))
		}
	}
}

// showNixCache says what gc deletes of the shared Nix cache.
func showNixCache(u *ui.UI, p vm.NixCachePlan) {
	switch {
	case !p.Any():
	case p.Limit == 0:
		u.Say(msg.GCNixCache(p.Files, humanSize(p.Size)))
	default:
		u.Say(msg.GCNixCacheOver(humanSize(p.Cap), humanSize(p.Size)))
	}
}

// showGCDone says what gc deleted, in the order it did; an image its
// backend refused stays, with the backend's message.
func showGCDone(u *ui.UI, d vm.GCDone) {
	for _, n := range d.Forgot {
		u.Info(msg.ForgotOrphan(n))
	}
	if d.LinkSockets > 0 {
		u.Info(msg.DeletedLinkSockets(d.LinkSockets))
	}
	for _, l := range d.Images {
		switch {
		case l.Refused:
			u.Warn(l.Why)
		case l.Name == "":
			u.Info(msg.DeletedUnfinishedBuild(l.Key))
		default:
			u.Info(msg.DeletedImage(l.Name, l.Key))
		}
	}
	if d.Mise > 0 {
		u.Info(msg.DeletedMise(d.Mise))
	}
	if d.PackagesDeleted {
		u.Info(msg.DeletedPackages(d.Packages))
	}
	for _, c := range []struct {
		n    int
		said func(int) msg.M
	}{{d.History, msg.DeletedHistory}, {d.Tools, msg.DeletedTools}, {d.ToolsNoVM, msg.DeletedToolsNoVM}} {
		if c.n > 0 {
			u.Info(c.said(c.n))
		}
	}
	if d.NixFiles > 0 {
		u.Info(msg.DeletedNixCache(d.NixFiles, humanSize(d.NixFreed)))
	}
}

// dirsSize is the disk these take.
func dirsSize(paths []string) int64 {
	var n int64
	for _, p := range paths {
		n += du(p)
	}
	return n
}
