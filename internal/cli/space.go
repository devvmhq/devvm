package cli

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
	"time"

	"github.com/spf13/cobra"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/build"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/proxy"
	"github.com/devvmhq/devvm/internal/settings"
	"github.com/devvmhq/devvm/internal/tools"
	"github.com/devvmhq/devvm/internal/ui"
	"github.com/devvmhq/devvm/internal/vm"
	"github.com/devvmhq/devvm/internal/vmname"
)

// spaceTimeout bounds what devvm space asks each backend, so a backend
// that hangs never holds it up; a test build's host (the registry's)
// can set it (platform.TestListWait).
func spaceTimeout(host platform.Host, backend string) time.Duration {
	return host.ListTimeout(backend, 30*time.Second)
}

// What devvm space's KEPT column says of each kind of thing.
const (
	keptRemade   = "remade"    // devvm makes it again when it's needed
	keptOnlyCopy = "only copy" // nothing else has it: a VM's work, an export
	keptLog      = "log"       // a record nothing needs
	keptState    = "state"     // what devvm knows of its VMs and images
	keptLeftover = "leftover"  // what devvm keeps for a VM its backend no longer has
)

// newSpaceCmd is devvm space.
func newSpaceCmd(a *app) *cobra.Command {
	u := a.u
	var asJSON bool
	c := &cobra.Command{
		Use:         "space",
		Annotations: about{docs: "using/space#devvm-space", group: groupSpace}.annotations(),
		Short:       "Show what devvm keeps on this machine and the space it takes",
		Long: "Show each kind of thing devvm keeps on this machine, with the disk it takes, " +
			"what devvm gc frees of it now and what frees the rest, then Lima's own caches apart. " +
			"It changes nothing and starts no VM.",
		Example: examples(
			"devvm space", "show what devvm keeps and what frees it", "devvm space --json", "the same as JSON, for scripts",
		),
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, err := space(cmd.Context(), a)
			if err != nil {
				return failed(err, msg.SpaceFailed)
			}
			for _, b := range r.Unknown {
				u.Note(msg.SpaceBackendUnknown(b))
			}
			for _, s := range r.Strays {
				u.Note(msg.SpaceStray(s.Backend, s.Data, s.Name))
			}
			for _, g := range r.FolderGone {
				u.Note(msg.SpaceFolderGone(g.Folder, g.Name))
			}
			for _, g := range r.NotConnected {
				u.Note(msg.SpaceFolderNotConnected(g.Folder, g.Name))
			}
			if asJSON {
				// Lists are [] when empty, never null.
				if r.Rows == nil {
					r.Rows = []spaceRow{}
				}
				if r.Caches == nil {
					r.Caches = []cacheRow{}
				}
				err = printJSON(u, r)
			} else {
				err = writeSpace(u.Table(), r)
			}
			if err != nil {
				return err
			}
			if len(r.Caches) > 0 {
				u.Note(msg.SpaceCachesNote())
			}
			return nil
		},
	}
	jsonFlag(c, &asJSON)
	return c
}

// spaceRow is one kind of thing devvm keeps, as devvm space shows it.
type spaceRow struct {
	What    string `json:"what"`
	Backend string `json:"backend,omitempty"`
	Count   int    `json:"count"`
	Size    int64  `json:"size"`
	Sized   bool   `json:"sized"` // Size is known
	Kept    string `json:"kept"`  // one of the kept constants
	GC      int64  `json:"gc"`    // what devvm gc frees of it now
	Rest    string `json:"rest"`  // what frees the rest
	// Retain is the retention setting that applies to it, as written, ""
	// when there's none or it's off; RetainFrees what it would free now.
	Retain      string `json:"retain,omitempty"`
	RetainFrees int64  `json:"retain_frees,omitempty"`
}

// retainCell is how the RETAIN column shows a row's setting: off, or its
// value and what it would free now.
func (s spaceRow) retainCell() string {
	if s.Retain == "" {
		return ""
	}
	return s.Retain + " (" + humanSize(s.RetainFrees) + ")"
}

// cacheRow is one of a backend's own caches.
type cacheRow struct {
	Backend string `json:"backend"`
	Name    string `json:"name"`
	Path    string `json:"path,omitempty"`
	Count   int    `json:"count"`
	Size    int64  `json:"size"`
	Sized   bool   `json:"sized"`
}

// spaceReport is what devvm space shows.
type spaceReport struct {
	Rows    []spaceRow `json:"devvm"`
	Total   int64      `json:"total"`
	GC      int64      `json:"gc"`
	Caches  []cacheRow `json:"backends"`
	Unknown []string   `json:"unknown,omitempty"` // backends that didn't answer
	// Strays are VMs with devvm's label it has no state for: another
	// devvm's, or this one's whose state is gone. devvm never deletes them.
	Strays []strayVM `json:"strays,omitempty"`
	// FolderGone are the VMs whose backend has them and whose project
	// folder is gone (vm.FolderGone), by name: devvm rm VM deletes one,
	// and devvm gc never does.
	FolderGone []goneVM `json:"folder_gone,omitempty"`
	// NotConnected are those whose folder is on a disk that isn't
	// connected (vm.FolderNotConnected).
	NotConnected []goneVM `json:"folder_not_connected,omitempty"`
}

// goneVM is a VM whose project folder is gone or not connected.
type goneVM struct {
	Backend string    `json:"backend"`
	Name    vmname.VM `json:"name"`
	Folder  string    `json:"folder"`
}

// strayVM is a VM with devvm's label that this devvm has no state for.
type strayVM struct {
	Backend string    `json:"backend"`
	Name    vmname.VM `json:"name"`
	Data    string    `json:"data"` // the data folder its label names
}

// spaceEnv is what devvm space reads: devvm's folders, its backends (the
// VM environment's registry, whose Builder builds on each) and the shared
// tools.
type spaceEnv struct {
	b   *build.Env
	env *vm.Env
	// inv is the VMs, read once (vm.ReadInventory); answered the
	// backends that listed their VMs and images.
	inv      *vm.Inventory
	answered map[string]bool
	// left is what devvm gc --orphans would forget, by inv
	// (vm.Leftovers.FindIn): its orphans and link sockets are leftover
	// state.
	left vm.LeftoverReport
	ret  settings.Retention // the retention settings
	now  time.Time
}

// space reads what devvm keeps. Nothing it runs changes anything, and
// each backend gets spaceTimeout.
func space(ctx context.Context, a *app) (spaceReport, error) {
	u := a.u
	defer u.Status(msg.ReadingSpace())()
	var r spaceReport
	b, err := a.builder()
	if err != nil {
		return r, err
	}
	env, err := a.vmEnv()
	if err != nil {
		return r, err
	}
	reg := env.Registry
	inv, err := vm.ReadInventory(ctx, b.Dirs, reg, vm.AllBackends)
	if err != nil {
		return r, err
	}
	def, err := settings.Read(b.Dirs)
	if err != nil {
		return r, err
	}
	left, err := vm.Leftovers{Env: env}.FindIn(ctx, inv)
	if err != nil {
		return r, err
	}
	e := spaceEnv{b: b, env: env, inv: inv, answered: map[string]bool{}, left: left, ret: def.Retention, now: time.Now()}
	for _, be := range reg.All() {
		rows, caches, ok := e.backendRows(ctx, be)
		if !ok {
			if inv.Used(be.Name()) {
				r.Unknown = append(r.Unknown, be.Name())
			}
			continue
		}
		r.Rows = append(r.Rows, rows...)
		r.Caches = append(r.Caches, caches...)
		r.Strays = append(r.Strays, strays(ctx, reg.Host, be)...)
	}
	toolRows, err := e.toolRows()
	if err != nil {
		return r, err
	}
	r.Rows = append(r.Rows, toolRows...)
	dataRows, err := e.dataRows(ctx)
	if err != nil {
		return r, err
	}
	r.Rows = append(r.Rows, dataRows...)
	r.FolderGone, r.NotConnected = e.folderGone()
	for _, row := range r.Rows {
		r.Total += row.Size
		r.GC += row.GC
	}
	return r, nil
}

// backendRows are a backend's VMs and images, and its caches; ok is false
// when it can't be asked (not installed, not running, too slow).
func (e spaceEnv) backendRows(ctx context.Context, be backend.Backend) (rows []spaceRow, caches []cacheRow, ok bool) {
	ctx, cancel := context.WithTimeout(ctx, spaceTimeout(e.env.Registry.Host, be.Name()))
	defer cancel()
	if backend.Check(ctx, be) != nil {
		return nil, nil, false
	}
	vms, listed := e.inv.Lists[be.Name()]
	if !listed {
		return nil, nil, false
	}
	ls, err := build.Images{Env: e.env.Builder(be, "")}.Listing(ctx)
	if err != nil {
		return nil, nil, false
	}
	e.answered[be.Name()] = true
	usage, _ := be.Usage(ctx)
	vmRow := spaceRow{What: "VMs", Backend: be.Name(), Count: len(vms), Sized: true, Kept: keptOnlyCopy, Rest: "devvm rm"}
	for _, in := range vms {
		n, sized := usage.VMs[in.Name]
		vmRow.Size += n
		vmRow.Sized = vmRow.Sized && sized
	}
	imgRow := spaceRow{What: "images", Backend: be.Name(), Count: len(ls), Sized: true, Kept: keptRemade, Rest: "images rm"}
	for _, l := range ls {
		imgRow.Size += l.Size
		imgRow.Sized = imgRow.Sized && l.Sized
		if l.State != build.StateCurrent {
			imgRow.GC += l.Size
		}
	}
	for _, c := range usage.Caches {
		if c.Count == 0 {
			continue
		}
		caches = append(caches, cacheRow{Backend: be.Name(), Name: c.Name, Path: c.Path, Count: c.Count, Size: c.Size, Sized: c.Sized})
	}
	return nonEmpty(vmRow, imgRow), caches, true
}

// strays are a backend's VMs with devvm's label that this devvm has no
// state for; none when it can't say.
func strays(ctx context.Context, host platform.Host, be backend.Backend) []strayVM {
	ctx, cancel := context.WithTimeout(ctx, spaceTimeout(host, be.Name()))
	defer cancel()
	ss, err := be.Strays(ctx)
	if err != nil {
		return nil
	}
	out := make([]strayVM, 0, len(ss))
	for _, s := range ss {
		out = append(out, strayVM{Backend: be.Name(), Name: s.Name, Data: s.Data})
	}
	return out
}

// nonEmpty returns the rows that have something.
func nonEmpty(rows ...spaceRow) []spaceRow {
	return slices.DeleteFunc(rows, func(r spaceRow) bool { return r.Count == 0 })
}

// toolRows are the shared tools' builds, by backend and C library: what
// devvm gc frees of them is what devvm images lists as unused.
func (e spaceEnv) toolRows() ([]spaceRow, error) {
	ts, err := vm.Housekeeping{Env: e.env}.ShareListing(e.b, e.inv)
	if err != nil {
		return nil, err
	}
	byKey := map[string]*spaceRow{}
	var keys []string
	for _, t := range ts {
		if !t.Built {
			continue
		}
		be, libc := t.Backend, t.Target.Libc
		key := be + " " + libc
		row, ok := byKey[key]
		if !ok {
			row = &spaceRow{What: "tools " + cmp.Or(libc, "-"), Backend: be, Sized: true, Kept: keptRemade, Rest: "gc --tool-images", Retain: e.ret.Raw[settings.RetainToolsAge]}
			byKey[key] = row
			keys = append(keys, key)
		}
		row.Count++
		row.Size += t.Size
		if t.State == tools.StateUnused {
			row.GC += t.Size
		}
		if e.ret.ToolsAge > 0 && e.idleFor(t) >= e.ret.ToolsAge {
			row.RetainFrees += t.Size
		}
	}
	slices.Sort(keys)
	rows := make([]spaceRow, 0, len(keys))
	for _, k := range keys {
		rows = append(rows, *byKey[k])
	}
	return rows, nil
}

// idleFor is how long a current tool image has had no VM, as retention
// recorded it (Share.Idle); 0 when it hasn't, or a VM has it. It changes
// nothing.
func (e spaceEnv) idleFor(t tools.Listed) time.Duration {
	return tools.Share{Env: e.b.Tools}.IdleFor(t, e.now)
}

// dataRows are the rest of devvm's data folder: exports, logs, devvm's
// own mise, what's left of VMs gone from their
// backends, and its other state. Each file is counted once.
func (e spaceEnv) dataRows(ctx context.Context) ([]spaceRow, error) {
	d := e.b.Dirs
	es, err := vm.Exports{Dirs: d}.List()
	if err != nil {
		return nil, err
	}
	// The VMs' logs, and the daily tools check's: not the runs'.
	logs, err := filesIn(d.Logs(), func(string) bool { return true })
	if err != nil {
		return nil, err
	}
	vmLogs, err := e.vmLogs()
	if err != nil {
		return nil, err
	}
	logs.count, logs.size = logs.count+vmLogs.count, logs.size+vmLogs.size
	runs, err := filesIn(d.RunLogs(), func(n string) bool { return strings.HasSuffix(n, ".log") })
	if err != nil {
		return nil, err
	}
	mise, err := dirsIn(d.Mise())
	if err != nil {
		return nil, err
	}
	oldMise, err := tools.Versions{Env: e.b.Tools}.StaleMise()
	if err != nil {
		return nil, err
	}
	debs, err := filesIn(d.Packages(), func(n string) bool { return strings.HasSuffix(n, ".deb") })
	if err != nil {
		return nil, err
	}
	stalePackages, err := build.Images{Env: e.b}.Stale(ctx)
	if err != nil {
		return nil, err
	}
	left := e.leftovers()
	nix, err := e.nixCacheRow()
	if err != nil {
		return nil, err
	}
	other := otherState(d) - left.Size - vmLogs.size
	exportsRow := spaceRow{
		What: "history", Count: len(es), Size: vm.ExportsSize(es), Sized: true, Kept: keptOnlyCopy, Rest: "gc --history",
		Retain: e.retainOf(settings.RetainHistoryAge, settings.RetainHistoryCount), RetainFrees: vm.ExportsSize(vm.ExportsToRetire(es, e.ret, e.now)),
	}
	runsRow := spaceRow{
		What: "run logs", Count: runs.count, Size: runs.size, Sized: true, Kept: keptLog,
		Rest: "the newest " + strconv.Itoa(cmp.Or(e.ret.RunLogs, 20)) + " stay", Retain: e.retainOf(settings.RetainRunLogs),
		RetainFrees: oldestBeyond(d.RunLogs(), e.ret.RunLogs),
	}
	return nonEmpty(
		exportsRow,
		spaceRow{What: "network logs", Count: logs.count, Size: logs.size, Sized: true, Kept: keptLog, Rest: "devvm rm"},
		runsRow,
		spaceRow{What: "mise", Count: mise.count, Size: mise.size, Sized: true, Kept: keptRemade, GC: dirsSize(oldMise), Rest: "-"},
		spaceRow{What: "packages", Count: debs.count, Size: debs.size, Sized: true, Kept: keptRemade, GC: stalePackages.Size, Rest: "rebuild base"},
		nix,
		left,
		spaceRow{What: "state", Count: 1, Size: max(other, 0), Sized: true, Kept: keptState, Rest: "-"},
	), nil
}

// nixCacheRow is the shared Nix cache: what devvm gc and retention free of
// it now is what it takes over its cap. Its size is its files', as its cap
// counts them.
func (e spaceEnv) nixCacheRow() (spaceRow, error) {
	n, size, err := proxy.NixCacheUsage(e.b.Dirs.NixCache())
	over := max(size-e.ret.NixCacheCap(), 0)
	return spaceRow{
		What: "Nix cache", Count: n, Size: size, Sized: true, Kept: keptRemade, GC: over, Rest: "gc --nix-cache",
		Retain: e.retainOf(settings.RetainNixCacheSize), RetainFrees: over,
	}, err
}

// leftovers is what devvm gc --orphans would forget (vm.Leftovers.FindIn):
// the whole folder of each orphan, its state, run and backend files, and
// the link sessions' sockets that nothing listens on, one each. Its
// retention frees the orphans orphans.age reaches, as retention would.
func (e spaceEnv) leftovers() spaceRow {
	row := spaceRow{What: "leftover state", Sized: true, Kept: keptLeftover, Rest: "gc --orphans", Retain: e.retainOf(settings.RetainOrphansAge)}
	row.Count = len(e.left.Sockets)
	for _, o := range e.left.Orphans {
		row.Count++
		n64 := du(e.b.Dirs.VM(o.Name).Dir)
		row.Size += n64
		if since := e.now.Sub(o.Changed); e.ret.OrphansAge > 0 && since >= e.ret.OrphansAge {
			row.RetainFrees += n64
		}
	}
	return row
}

// isOrphan reports whether k is a state's row of one of the orphans.
func (e spaceEnv) isOrphan(k vm.Known) bool {
	return !k.Listed && k.Err == nil && slices.ContainsFunc(e.left.Orphans, func(o vm.Orphan) bool {
		return o.Name == k.Name && o.Backend == k.State.Backend
	})
}

// vmFolders are the VMs' folders that have a state file but the orphans'
// (whose whole folder is leftover state), a state that can't be read
// among them.
func (e spaceEnv) vmFolders() []vm.Known {
	var rest []vm.Known
	for _, k := range e.inv.VMs {
		switch {
		case k.Err == nil && !k.State.Exists():
			// A backend's VM without state: no folder of devvm's.
		case e.isOrphan(k):
		case !k.Listed || k.Err != nil || k.State.Backend == k.Backend.Name():
			rest = append(rest, k)
		}
	}
	return rest
}

// folderGone are the VMs whose backend answered with them and whose
// project folder is gone, then those whose folder is on a disk that isn't
// connected, by name.
func (e spaceEnv) folderGone() (gone, unconnected []goneVM) {
	for _, k := range e.vmFolders() {
		st := k.State
		if k.Err != nil || !k.Listed || !e.answered[st.Backend] {
			continue
		}
		g := goneVM{Backend: st.Backend, Name: k.Name, Folder: st.Dir}
		switch vm.FolderOf(st) {
		case vm.FolderGone:
			gone = append(gone, g)
		case vm.FolderNotConnected:
			unconnected = append(unconnected, g)
		case vm.FolderThere:
		}
	}
	return gone, unconnected
}

// vmLogs are the logs of the VMs that aren't orphans: their network logs,
// their proxies' and link sessions' output.
func (e spaceEnv) vmLogs() (counted, error) {
	var c counted
	for _, k := range e.vmFolders() {
		l, err := filesIn(paths.VM{Dir: e.b.Dirs.VMDir(k.Hash)}.Logs(), func(string) bool { return true })
		if err != nil {
			return c, err
		}
		c.count, c.size = c.count+l.count, c.size+l.size
	}
	return c, nil
}

// oldestBeyond is the disk the run logs in dir take beyond the newest
// keep, which the next run deletes; 0 when keep is 0 (off). Their names
// sort by when they were written.
func oldestBeyond(dir string, keep int) int64 {
	if keep == 0 {
		return 0
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	var logs []string
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".log") {
			logs = append(logs, e.Name())
		}
	}
	var n int64
	for i := 0; i < len(logs)-keep; i++ {
		n += du(filepath.Join(dir, logs[i]))
	}
	return n
}

// retainOf is the retention settings of these keys that are on, as
// written: 30d, or 30d, 5 for two.
func (e spaceEnv) retainOf(keys ...string) string {
	var on []string
	for _, k := range keys {
		if v := e.ret.Raw[k]; v != "" {
			on = append(on, v)
		}
	}
	return strings.Join(on, ", ")
}

// otherState is the disk devvm's other state takes: the VMs' folders (their
// logs and the orphans' are counted apart, by the caller), images'
// records, run files, the tools lock and the --from digests in the
// cache.
func otherState(d paths.Dirs) int64 {
	var n int64
	for _, p := range []string{
		d.VMs(), d.Images(), d.FromDigests(), d.Run(), d.PackageSets(),
		d.ToolLock(), d.ToolLockConfig(), d.ToolLockChecked(),
	} {
		n += du(p)
	}
	return n
}

// counted is how many and how much.
type counted struct {
	count int
	size  int64
}

// filesIn counts the plain files in dir that keep picks.
func filesIn(dir string, keep func(string) bool) (counted, error) {
	return entriesIn(dir, func(e fs.DirEntry) bool { return e.Type().IsRegular() && keep(e.Name()) })
}

// dirsIn counts the folders in dir, but those of work in progress
// (.tmp-…).
func dirsIn(dir string) (counted, error) {
	return entriesIn(dir, func(e fs.DirEntry) bool { return e.IsDir() && !strings.HasPrefix(e.Name(), ".") })
}

func entriesIn(dir string, keep func(fs.DirEntry) bool) (counted, error) {
	var c counted
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	} else if err != nil {
		return c, err
	}
	for _, e := range entries {
		if keep(e) {
			c.count++
			c.size += du(filepath.Join(dir, e.Name()))
		}
	}
	return c, nil
}

// du is the disk a path takes, as platform.DiskUsage counts it: 0 for
// one that's gone or can't be read.
func du(path string) int64 {
	n, err := platform.DiskUsage(path)
	if err != nil {
		return 0
	}
	return n
}

// writeSpace writes devvm space's tables: devvm's own with a total, bold,
// then the backends' caches; a value one lacks is -. What devvm keeps only
// copy of, and leftovers, are yellow.
func writeSpace(tab *ui.Table, r spaceReport) error {
	size := func(n int64, sized bool) string {
		if !sized {
			return ""
		}
		return humanSize(n)
	}
	gc := func(n int64) string {
		if n == 0 {
			return ""
		}
		return humanSize(n)
	}
	tab.Header("WHAT", "COUNT", "SIZE", "KEPT", "GC", "FREED BY", "RETAIN")
	for _, s := range r.Rows {
		tab.Row(cell(s.What), cell(strconv.Itoa(s.Count)), cell(size(s.Size, s.Sized)),
			cellAs(s.Kept, keptRoles), cell(gc(s.GC)), cell(s.Rest), cell(s.retainCell()))
	}
	tab.Total(cell("total"), cell(""), cell(humanSize(r.Total)), cell(""), cell(gc(r.GC)), cell(""), cell(""))
	if len(r.Caches) == 0 {
		return tab.Flush()
	}
	if err := tab.Break(); err != nil {
		return err
	}
	tab.Header("CACHE", "COUNT", "SIZE", "PATH")
	for _, c := range r.Caches {
		tab.Row(cell(c.Name), cell(strconv.Itoa(c.Count)), cell(size(c.Size, c.Sized)), cell(c.Path))
	}
	return tab.Flush()
}

// keptRoles colors devvm space's KEPT column.
func keptRoles(s string) ui.Role {
	if s == keptOnlyCopy || s == keptLeftover {
		return ui.Attention
	}
	return ui.Plain
}
