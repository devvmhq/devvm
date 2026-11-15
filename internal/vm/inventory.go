package vm

import (
	"cmp"
	"context"
	"maps"
	"path/filepath"
	"slices"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/gitprotect"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/project"
	"github.com/devvmhq/devvm/internal/sshagent"
	"github.com/devvmhq/devvm/internal/sshconfig"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/tools"
	"github.com/devvmhq/devvm/internal/vmname"
)

// Inventory is the project VMs as one reading finds them: each VM's
// state, the backend it's on, and what that backend's list has of it.
// ReadStates reads the states alone; ReadInventory asks the backends too.
//
// Its one failure policy: a vm.json that can't be read is a row of its
// own (Known.Err), never a failure for every VM. A VM whose state can't be
// read never starts, so what only reads or shows VMs leaves it out; what
// deletes by the states (the shared tools in use) or keeps a key from a
// VM by them (Mounts) fails on it, naming the file. A backend that can't
// list fails what lists VMs (Err) only when some VM's state names it; one
// no state names holds none of devvm's VMs (not installed, say).
type Inventory struct {
	// VMs are sorted by name, then backend; a state that can't be read,
	// and that no backend's list names, sorts by its folder, first.
	VMs []Known
	// Unlisted are the backends asked that couldn't list, by name, with
	// why: their own check's failure (backend.Check) when it has one.
	Unlisted map[string]error
	// Lists are the lists of the backends that listed, by name, whole:
	// their build VMs too.
	Lists map[string][]backend.Instance
	// named are the backends some VM's state names.
	named map[string]bool
}

// Known is one project VM the inventory found: by its state, its
// backend's list, or both.
type Known struct {
	// Name is the VM's name: "" for a state that can't be read and that
	// no backend's list names.
	Name vmname.VM
	// Hash is its folder's (paths.Dirs.VMDir), Path its state file's.
	Hash, Path string
	// State is its state: empty (Exists false) for a backend's VM with
	// none, whose creation didn't finish; nil when Err is set.
	State *state.VM
	// Err is why its vm.json can't be read: what the VM is, devvm
	// doesn't know.
	Err error
	// Backend is the backend whose list has it, else the one its state
	// names: nil for one that isn't registered.
	Backend backend.Backend
	// Instance is what Backend's list has of it, when Listed.
	Instance backend.Instance
	Listed   bool
}

// Asked is which backends ReadInventory asks for their VMs.
type Asked int

const (
	// UsedBackends asks the backends some VM's state names, so a launch
	// never waits for one nobody uses (not running, not installed).
	UsedBackends Asked = iota
	// AllBackends asks every backend, so devvm ls and space see a VM
	// whose state is gone too.
	AllBackends
)

// listWait is how long a backend gets to list its VMs, and then to say
// why it couldn't (the host's ListTimeout can change it), so one that
// hangs fails alone.
const listWait = 30 * time.Second

// ReadStates reads every project VM's state, each on its own: the
// inventory without the backends' lists. Only the VMs' folder failing to
// be read fails it.
func ReadStates(d paths.Dirs) (*Inventory, error) {
	all, err := state.ReadAll(d)
	if err != nil {
		return nil, err
	}
	inv := &Inventory{Unlisted: map[string]error{}, Lists: map[string][]backend.Instance{}, named: map[string]bool{}}
	for _, r := range all {
		k := Known{Hash: r.Hash, Path: paths.VM{Dir: d.VMDir(r.Hash)}.State(), State: r.VM, Err: r.Err}
		if r.Err != nil {
			k.State = nil
		} else {
			k.Name = r.VM.Name()
			inv.named[r.VM.Backend] = true
		}
		inv.VMs = append(inv.VMs, k)
	}
	inv.sort()
	return inv, nil
}

// ReadInventory reads every project VM's state, then asks the backends
// asked for their VMs, each within listWait.
func ReadInventory(ctx context.Context, d paths.Dirs, reg *backend.Registry, asked Asked) (*Inventory, error) {
	inv, err := ReadStates(d)
	if err != nil {
		return nil, err
	}
	for i, k := range inv.VMs {
		if k.State != nil {
			inv.VMs[i].Backend, _ = reg.Lookup(k.State.Backend)
		}
	}
	for _, b := range reg.All() {
		if asked == UsedBackends && !inv.named[b.Name()] {
			continue
		}
		ins, err := listVMs(ctx, reg.Host, b)
		if err != nil {
			inv.Unlisted[b.Name()] = err
			continue
		}
		inv.Lists[b.Name()] = ins
		for _, in := range ins {
			if in.Name.IsProject() {
				inv.add(d, b, in)
			}
		}
	}
	inv.sort()
	return inv, nil
}

// listVMs is b's list, within listWait; when it fails, its own check's
// failure, within listWait again, says why when it has one.
func listVMs(ctx context.Context, host platform.Host, b backend.Backend) ([]backend.Instance, error) {
	wait := host.ListTimeout(b.Name(), listWait)
	lctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	ins, err := b.List(lctx)
	if err == nil {
		return ins, nil
	}
	cctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	if cerr := backend.Check(cctx, b); cerr != nil {
		return nil, cerr
	}
	return nil, err
}

// add is in, on b's list: its state's row, when its state names b, or
// the row of a state by its hash that can't be read; else a row of its
// own, with the state its name has (empty for none).
func (inv *Inventory) add(d paths.Dirs, b backend.Backend, in backend.Instance) {
	h, _ := in.Name.Hash()
	for i := range inv.VMs {
		k := &inv.VMs[i]
		if k.Listed || k.Hash != h {
			continue
		}
		mine := k.State != nil && k.Name == in.Name && k.State.Backend == b.Name()
		if mine || k.Err != nil && k.Name == "" {
			k.Name, k.Backend, k.Instance, k.Listed = in.Name, b, in, true
			return
		}
	}
	k := Known{Name: in.Name, Hash: h, Path: d.VM(in.Name).State(), Backend: b, Instance: in, Listed: true}
	k.State, k.Err = state.ReadVM(d, in.Name)
	inv.VMs = append(inv.VMs, k)
}

func (inv *Inventory) sort() {
	slices.SortStableFunc(inv.VMs, func(a, b Known) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(backendName(a.Backend), backendName(b.Backend)), cmp.Compare(a.Hash, b.Hash))
	})
}

func backendName(b backend.Backend) string {
	if b == nil {
		return ""
	}
	return b.Name()
}

// Err is the failure of the first backend, by name, that some VM's state
// names and that couldn't list: what lists VMs fails with it.
func (inv *Inventory) Err() error {
	for _, n := range slices.Sorted(maps.Keys(inv.Unlisted)) {
		if inv.named[n] {
			return inv.Unlisted[n]
		}
	}
	return nil
}

// Used reports whether some VM's state names the backend by that name:
// the only backends that hold devvm's VMs.
func (inv *Inventory) Used(name string) bool { return inv.named[name] }

// Asked reports whether the backend by that name listed its VMs: false
// for one not asked, or that couldn't list.
func (inv *Inventory) Asked(name string) bool {
	_, ok := inv.Lists[name]
	return ok
}

// States are the states of the VMs that have one that can be read, once
// each, sorted by name.
func (inv *Inventory) States() []*state.VM {
	var out []*state.VM
	for _, k := range inv.VMs {
		if k.Err == nil && k.State.Exists() && !slices.ContainsFunc(out, func(st *state.VM) bool { return st.Name() == k.Name }) {
			out = append(out, k.State)
		}
	}
	return out
}

// GitKeeper is, for the VM self, whether another VM's .git protection
// keeps a path locked too (gitprotect.Keeps), as the warning that says
// so, or a zero msg.M, so self's unlock leaves the path locked
// (gitprotect.Protector.Kept). A parent folder's VM protects the repos
// nested in its folder, and a nested repo's own VM its own. A state that
// can't be read keeps every path in the folder whose hash names its
// folder (project.Hash), as it may protect them: what it protects, devvm
// can't tell.
func (inv *Inventory) GitKeeper(self vmname.VM) func(path string) msg.M {
	states := inv.States()
	unread := inv.Unreadable()
	return func(path string) msg.M {
		for _, st := range states {
			if st.Name() != self && gitprotect.Keeps(st, path) {
				return msg.GitKeptLocked(st.Name())
			}
		}
		for _, k := range unread {
			for d := path; d != "/" && d != "."; d = filepath.Dir(d) {
				if project.Hash(d) == k.Hash {
					return msg.GitKeptUnread(k.Path)
				}
			}
		}
		return msg.M{}
	}
}

// Unreadable are the VMs whose vm.json can't be read.
func (inv *Inventory) Unreadable() []Known {
	var out []Known
	for _, k := range inv.VMs {
		if k.Err != nil {
			out = append(out, k)
		}
	}
	return out
}

// unreadable is the first state that can't be read, which what deletes
// or refuses by the states fails on.
func (inv *Inventory) unreadable() error {
	if u := inv.Unreadable(); len(u) > 0 {
		return u[0].Err
	}
	return nil
}

// Running are the project VMs the backends' lists have running, for the
// ssh config.
func (inv *Inventory) Running() []sshconfig.Running {
	var out []sshconfig.Running
	for _, k := range inv.VMs {
		if k.Listed && k.Instance.Status == backend.StatusRunning {
			out = append(out, sshconfig.Running{Backend: k.Backend, Instance: k.Instance})
		}
	}
	return out
}

// Recorded are the VMs' names, aliases and folders, as their states
// record them, for finding a VM by its name or alias.
func (inv *Inventory) Recorded() []project.Recorded {
	var out []project.Recorded
	for _, st := range inv.States() {
		out = append(out, project.Recorded{Name: st.Name(), Alias: st.Alias, Dir: st.Dir})
	}
	return out
}

// ToolsInUse counts the VMs that list each shared tool image in their
// state. A state that can't be read fails it: its VM may use any.
func (inv *Inventory) ToolsInUse() (map[string]int, error) {
	if err := inv.unreadable(); err != nil {
		return nil, err
	}
	n := map[string]int{}
	for _, st := range inv.States() {
		for _, f := range tools.Files(st) {
			n[f]++
		}
	}
	return n, nil
}

// Mounts are the host folders project VMs mount, as their states have
// them, the VM running or not: its project folder (not a --clone VM's),
// its agents' folders in devvm's store, and the tools share. A state
// that can't be read fails it: its VM may mount any folder.
func (inv *Inventory) Mounts(d paths.Dirs) ([]sshagent.Mount, error) {
	if err := inv.unreadable(); err != nil {
		return nil, err
	}
	var ms []sshagent.Mount
	for _, st := range inv.States() {
		vm := st.Name()
		if st.Dir != "" && !isClone(st) {
			ms = append(ms, sshagent.Mount{VM: vm, Dir: st.Dir})
		}
		dirs, _ := agentMounts(d, tools.Names(st))
		for _, dir := range append(dirs, d.Tools()) {
			ms = append(ms, sshagent.Mount{VM: vm, Dir: dir})
		}
	}
	return ms, nil
}
