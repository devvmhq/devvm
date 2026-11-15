package vm

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/devvmhq/devvm/internal/netpolicy"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/settings"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/vmname"
)

// Exports are the exports of agents' history in devvm's exports folder
// (paths.Dirs.Exports), as devvm history export and devvm rm write them:
// their names, their list, the file beside each that names its VM's
// folder (paths.ExportFolder), and their deletion.
type Exports struct{ Dirs paths.Dirs }

// Export is one of the exports in devvm's exports folder.
type Export struct {
	Path   string
	VM     vmname.VM // the VM it's of
	Folder string    // its project folder, from the file beside it; "" without one
	// Tier is its VM's network tier when it was written, from the file
	// beside it; "" without one.
	Tier netpolicy.Tier
	At   time.Time // when it was written, from its name
	Size int64
}

// exportStamp is the date and time in an export's name, the host's wall
// clock when it was written.
const exportStamp = "20060102-150405"

// exportSuffix ends every export's name.
const exportSuffix = ".tar.gz"

// newFile is a new export's file in the exports folder, written at now:
// <vm>-<date>-<time>.tar.gz.
func (x Exports) newFile(vm vmname.VM, now time.Time) string {
	return filepath.Join(x.Dirs.Exports(), vm.String()+"-"+now.Format(exportStamp)+exportSuffix)
}

// parseExportName returns the VM and the date and time of an export's
// name (newFile's), and whether it is one: its VM a project VM's
// (vmname.IsVM), its date and time digits. Nothing else in the exports
// folder is one: an export being written (.devvm-export-…), the file
// beside one that names its folder or the user's own.
func parseExportName(name string) (vmname.VM, string, bool) {
	base, found := strings.CutSuffix(name, exportSuffix)
	n := len(exportStamp)
	if !found || len(base) <= n+1 || base[len(base)-n-1] != '-' {
		return "", "", false
	}
	stamp := base[len(base)-n:]
	for i, c := range stamp {
		if (i == 8) != (c == '-') || i != 8 && (c < '0' || c > '9') {
			return "", "", false
		}
	}
	vm, err := vmname.Parse(base[:len(base)-n-1])
	if err != nil {
		return "", "", false
	}
	return vm, stamp, true
}

// writeFolder writes the file beside an export that names its VM's
// project folder, for devvm history to list once the VM is gone, and on
// its last line the VM's network tier as it was exported, for an import
// to compare (Lifecycle's warnTier).
func (Exports) writeFolder(export, dir string, tier netpolicy.Tier) error {
	return state.WriteFile(paths.ExportFolder(export), []byte(dir+"\n"+string(tier)+"\n"))
}

// record reads the file beside an export: the project folder it names,
// "" without one, and the network tier its VM had, "" without one.
func (Exports) record(export string) (string, netpolicy.Tier) {
	data, err := os.ReadFile(paths.ExportFolder(export))
	if err != nil {
		return "", ""
	}
	s := strings.TrimSuffix(string(data), "\n")
	i := strings.LastIndexByte(s, '\n')
	if i < 0 {
		return strings.TrimSpace(s), ""
	}
	tier := netpolicy.Tier(s[i+1:])
	if !tier.Valid() {
		tier = ""
	}
	return strings.TrimSpace(s[:i]), tier
}

// List lists the exports in the exports folder, oldest first: only plain
// files named as devvm names them.
func (x Exports) List() ([]Export, error) {
	entries, err := os.ReadDir(x.Dirs.Exports())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var out []Export
	for _, e := range entries {
		vm, stamp, ok := parseExportName(e.Name())
		if !ok || !e.Type().IsRegular() {
			continue
		}
		// The name has no zone: it's the host's wall clock when the export
		// was written (newFile names it in local time), so it's read in
		// local time too and shown as written. An age is elapsed time from
		// that instant (30d is 720 hours), so across a DST change an export
		// reaches it an hour before or after the same wall-clock time 30
		// days on, and on a host moved to another zone it's off by the
		// zones' difference.
		at, err := time.ParseInLocation(exportStamp, stamp, time.Local)
		if err != nil {
			continue
		}
		p := filepath.Join(x.Dirs.Exports(), e.Name())
		size, err := platform.DiskUsage(p)
		if err != nil {
			size = 0
		}
		folder, tier := x.record(p)
		out = append(out, Export{Path: p, VM: vm, Folder: folder, Tier: tier, At: at, Size: size})
	}
	slices.SortFunc(out, func(a, b Export) int { return a.At.Compare(b.At) })
	return out, nil
}

// Newest is the newest export made of a VM of the project folder dir, by
// the file beside it; ok is false when there's none.
func (x Exports) Newest(dir string) (e Export, ok bool, err error) {
	es, err := x.List()
	if err != nil {
		return e, false, err
	}
	for _, c := range slices.Backward(es) {
		if c.Folder == dir {
			return c, true, nil
		}
	}
	return e, false, nil
}

// Delete deletes these exports, each with the file beside it that names
// its folder.
func (Exports) Delete(es []Export) error {
	for _, e := range es {
		for _, p := range []string{e.Path, paths.ExportFolder(e.Path)} {
			if err := os.RemoveAll(p); err != nil {
				return err
			}
		}
	}
	return nil
}

// ExportsOlderThan returns the exports written at least age before now;
// all of them when age is 0.
func ExportsOlderThan(es []Export, age time.Duration, now time.Time) []Export {
	if age == 0 {
		return es
	}
	var out []Export
	for _, e := range es {
		if now.Sub(e.At) >= age {
			out = append(out, e)
		}
	}
	return out
}

// ExportsToRetire are the exports (oldest first) history.age and
// history.count say to delete: those that old, then all but the newest
// count.
func ExportsToRetire(es []Export, r settings.Retention, now time.Time) []Export {
	var gone []Export
	keep := es
	if r.ExportsAge > 0 {
		keep = nil
		for _, e := range es {
			if age := now.Sub(e.At); age >= r.ExportsAge {
				gone = append(gone, e)
			} else {
				keep = append(keep, e)
			}
		}
	}
	if r.ExportsCount > 0 && len(keep) > r.ExportsCount {
		gone = append(gone, keep[:len(keep)-r.ExportsCount]...) // oldest first
	}
	return gone
}

// ExportsSize is the disk these exports take.
func ExportsSize(es []Export) int64 {
	var n int64
	for _, e := range es {
		n += e.Size
	}
	return n
}
