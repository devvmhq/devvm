package vm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/backend/backendtest"
	"github.com/devvmhq/devvm/internal/gitprotect"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/project"
	"github.com/devvmhq/devvm/internal/sshagent"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/tools"
	"github.com/devvmhq/devvm/internal/vmname"
)

// hangs is a backend whose list waits until it's given up on.
type hangs struct{ *backendtest.Fake }

func (h hangs) List(ctx context.Context) ([]backend.Instance, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// The one failure policy for backends: one that can't list fails what
// lists VMs (Err) only when some VM's state names it, with its own
// check's failure when it has one; one that hangs is given up on after
// its wait. UsedBackends asks no backend that no state names.
func TestInventoryBackends(t *testing.T) {
	const web, oth = "devvm-web-0123456789abc1", "devvm-oth-0123456789abc2"
	for _, tc := range []struct {
		name        string
		listErr     error // other's list's failure
		missing     bool  // other's check fails too
		hang        bool  // other's list hangs
		named       bool  // a VM's state names other
		asked       Asked
		wantErr     string // Err: its text, or message ID
		wantUnasked bool   // other isn't asked at all
	}{
		{name: "every backend lists", named: true, asked: AllBackends},
		{name: "one no state names can't list", listErr: errors.New("list failed"), asked: AllBackends},
		{name: "one a state names can't list", listErr: errors.New("list failed"), named: true, asked: AllBackends, wantErr: "list failed"},
		{name: "its check says why", listErr: errors.New("list failed"), missing: true, named: true, asked: AllBackends, wantErr: "program_missing"},
		{name: "one a state names hangs", hang: true, named: true, asked: AllBackends, wantErr: context.DeadlineExceeded.Error()},
		{name: "one no state names hangs", hang: true, asked: AllBackends},
		{name: "used backends: one no state names isn't asked", listErr: errors.New("list failed"), asked: UsedBackends, wantUnasked: true},
		{name: "used backends: one a state names is", listErr: errors.New("list failed"), named: true, asked: UsedBackends, wantErr: "list failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := paths.Dirs{Data: t.TempDir()}
			vmState(t, d, web)
			if tc.named {
				vmState(t, d, oth, func(st *state.VM) error { st.Backend = "other"; return nil })
			}
			fake := backendtest.New("fake", nil)
			fake.Instances[web] = backend.Instance{Name: web, Status: backend.StatusRunning}
			other := backendtest.New("other", nil)
			other.Instances[oth] = backend.Instance{Name: oth, Status: backend.StatusStopped}
			if tc.listErr != nil {
				other.Errs = map[string]error{"List": tc.listErr}
			}
			if tc.missing {
				other.Missing = []backend.Problem{{Key: "lima", Msg: msg.ProgramMissing("Lima", "limactl")}}
			}
			var ob backend.Backend = other
			if tc.hang {
				ob = hangs{other}
			}
			reg, err := backend.NewRegistry(fake, ob)
			if err != nil {
				t.Fatal(err)
			}
			reg.Host = platform.NewHost(platform.Overrides{ListTimeout: "other:20ms"})
			inv, err := ReadInventory(t.Context(), d, reg, tc.asked)
			if err != nil {
				t.Fatal(err)
			}
			err = inv.Err()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("Err = %v", err)
			case tc.wantErr != "" && (err == nil || err.Error() != tc.wantErr && msg.IDOf(err) != tc.wantErr):
				t.Errorf("Err = %v, want %s", err, tc.wantErr)
			}
			failed := tc.listErr != nil || tc.hang
			if _, unlisted := inv.Unlisted["other"]; unlisted != (failed && !tc.wantUnasked) || inv.Asked("other") == failed {
				t.Errorf("other: unlisted %v, asked %v", inv.Unlisted, inv.Asked("other"))
			}
			if called := slices.Contains(other.Methods(), "List"); tc.wantUnasked && called {
				t.Error("other asked for its list")
			}
			// fake's VMs are there, whatever other does.
			if !inv.Asked("fake") || len(inv.Running()) != 1 || inv.Running()[0].Instance.Name != web {
				t.Errorf("fake's: running %+v", inv.Running())
			}
		})
	}
}

// The one failure policy for states: a vm.json that can't be read is a
// row of its own, by its backend's name for it when its backend has it,
// and the rest are read. What only reads leaves it out; the shared tools
// in use and the mounts fail on it.
func TestInventoryRows(t *testing.T) {
	d := paths.Dirs{Data: t.TempDir()}
	const (
		web  = "devvm-web-0123456789abc1" // running
		api  = "devvm-api-0123456789abc2" // its machine is gone
		half = "devvm-half-0123456789abc3"
		bad  = "devvm-bad-0123456789abc4" // its backend has it
		lost = "0123456789abc5"           // its backend doesn't
	)
	files := func(fs ...string) func(*state.VM) error {
		return func(st *state.VM) error { return tools.Set(st, tools.InVM{Made: tools.Made{Files: fs}}) }
	}
	vmState(t, d, web, files("node-24.tar"))
	vmState(t, d, api, files("node-24.tar", "go-1.26.tar"))
	for _, f := range []string{d.VM(bad).State(), paths.VM{Dir: d.VMDir(lost)}.State()} {
		if err := state.WriteFile(f, []byte("[]")); err != nil {
			t.Fatal(err)
		}
	}
	fake := backendtest.New("fake", nil)
	fake.Unconfined = true
	for _, in := range []backend.Instance{
		{Name: web, Status: backend.StatusRunning},
		{Name: half, Status: backend.StatusStopped},
		{Name: bad, Status: backend.StatusRunning},
		{Name: "devvm-tools-build", Status: backend.StatusRunning},
	} {
		fake.Instances[in.Name] = in
	}
	reg, err := backend.NewRegistry(fake)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := ReadInventory(t.Context(), d, reg, AllBackends)
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		Name            vmname.VM
		Hash            string
		State, Err      bool
		Backend, Listed bool
	}
	var got []row
	for _, k := range inv.VMs {
		got = append(got, row{k.Name, k.Hash, k.State != nil && k.State.Exists(), k.Err != nil, k.Backend != nil, k.Listed})
	}
	want := []row{
		{"", lost, false, true, false, false},
		{api, "0123456789abc2", true, false, true, false},
		{bad, "0123456789abc4", false, true, true, true},
		{half, "0123456789abc3", false, false, true, true},
		{web, "0123456789abc1", true, false, true, true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rows =\n%+v\nwant\n%+v", got, want)
	}
	if n := len(inv.Lists["fake"]); n != 4 {
		t.Errorf("fake's whole list: %d", n)
	}
	var names []vmname.VM
	for _, r := range inv.Recorded() {
		names = append(names, r.Name)
	}
	if !slices.Equal(names, []vmname.VM{api, web}) {
		t.Errorf("Recorded = %v", names)
	}
	lostFile := paths.VM{Dir: d.VMDir(lost)}.State()
	if u := inv.Unreadable(); len(u) != 2 || u[0].Path != lostFile || u[1].Path != d.VM(bad).State() {
		t.Errorf("Unreadable = %+v", u)
	}
	if _, err := inv.ToolsInUse(); err == nil {
		t.Error("ToolsInUse past a state it can't read")
	}
	if _, err := inv.Mounts(d); err == nil {
		t.Error("Mounts past a state it can't read")
	}
	// devvm ls: what the backend has, a state it can't read with its
	// status alone.
	ls, err := inv.List(t.Context(), &Env{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ls) != 3 || !reflect.DeepEqual(ls[0], Listed{Name: bad, Status: "Running", Backend: "fake", FolderState: FolderThere}) || ls[1].Name != half || ls[2].Name != web {
		t.Errorf("List = %+v", ls)
	}

	// Once it can be read: the tools each state lists.
	for _, f := range []string{d.VM(bad).State(), paths.VM{Dir: d.VMDir(lost)}.State()} {
		if err := os.Remove(f); err != nil {
			t.Fatal(err)
		}
	}
	inv, err = ReadStates(d)
	if err != nil {
		t.Fatal(err)
	}
	n, err := inv.ToolsInUse()
	if err != nil || !reflect.DeepEqual(n, map[string]int{"node-24.tar": 2, "go-1.26.tar": 1}) {
		t.Errorf("ToolsInUse = %v, %v", n, err)
	}
	if !inv.Used("fake") || inv.Used("other") || inv.Asked("fake") {
		t.Error("ReadStates: used fake alone, and asks no backend")
	}
}

// A VM another backend lists too by its name is a row of that backend's
// with the same state, and its state counts once.
func TestInventoryTwoBackends(t *testing.T) {
	d := paths.Dirs{Data: t.TempDir()}
	const web = "devvm-web-0123456789abc1"
	vmState(t, d, web)
	fake, other := backendtest.New("fake", nil), backendtest.New("other", nil)
	fake.Instances[web] = backend.Instance{Name: web, Status: backend.StatusRunning}
	other.Instances[web] = backend.Instance{Name: web, Status: backend.StatusStopped}
	reg, err := backend.NewRegistry(fake, other)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := ReadInventory(t.Context(), d, reg, AllBackends)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.VMs) != 2 || inv.VMs[0].Backend.Name() != "fake" || inv.VMs[1].Backend.Name() != "other" || !inv.VMs[1].State.Exists() {
		t.Errorf("rows = %+v", inv.VMs)
	}
	if len(inv.States()) != 1 || len(inv.Running()) != 1 {
		t.Errorf("states %d, running %d", len(inv.States()), len(inv.Running()))
	}
}

// Mounts are what the states have, whether the VM runs or not, and no
// backend is asked: each VM's project folder (not a --clone VM's), its
// agents' folders that exist in devvm's store, and the share.
func TestInventoryMounts(t *testing.T) {
	d := paths.Dirs{Data: t.TempDir(), Config: t.TempDir()}
	if err := os.MkdirAll(filepath.Join(agentStore(d, "claude"), ".claude", "skills"), 0o700); err != nil {
		t.Fatal(err)
	}
	const web, cl = "devvm-web-0123456789abc1", "devvm-cl-0123456789abc2"
	vmState(t, d, web, withTools("node", "claude"))
	vmState(t, d, cl, func(st *state.VM) error { return setField(st, KeyMode, ModeClone) })
	inv, err := ReadStates(d)
	if err != nil {
		t.Fatal(err)
	}
	got, err := inv.Mounts(d)
	if err != nil {
		t.Fatal(err)
	}
	agents, _ := agentMounts(d, []string{"claude"})
	if len(agents) != 1 || agents[0] != filepath.Join(d.Config, "agents", "claude", ".claude", "skills") {
		t.Fatalf("agent folders %q", agents)
	}
	share := d.Tools()
	want := []sshagent.Mount{{VM: cl, Dir: share}, {VM: web, Dir: "/p"}}
	for _, a := range agents {
		want = append(want, sshagent.Mount{VM: web, Dir: a})
	}
	want = append(want, sshagent.Mount{VM: web, Dir: share})
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Mounts =\n%+v\nwant\n%+v", got, want)
	}
}

// gc --orphans by the inventory: a VM whose state can't be read is never
// an orphan, and fails nothing; a backend that can't list, or that isn't
// registered, makes its VMs unknown; the rest are found.
func TestLeftoversFind(t *testing.T) {
	d := paths.Dirs{Data: t.TempDir()}
	const orphan, kept, unlisted, unknown = "devvm-o-0123456789abc1", "devvm-k-0123456789abc2", "devvm-u-0123456789abc3", "devvm-x-0123456789abc4"
	vmState(t, d, orphan)
	vmState(t, d, kept)
	vmState(t, d, unlisted, func(st *state.VM) error { st.Backend = "other"; return nil })
	vmState(t, d, unknown, func(st *state.VM) error { st.Backend = "gone"; return nil })
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(d.VM(orphan).State(), old, old); err != nil {
		t.Fatal(err)
	}
	if err := state.WriteFile(paths.VM{Dir: d.VMDir("0123456789abc5")}.State(), []byte("[]")); err != nil {
		t.Fatal(err)
	}
	fake := backendtest.New("fake", nil)
	fake.Instances[kept] = backend.Instance{Name: kept, Status: backend.StatusStopped}
	other := backendtest.New("other", nil)
	other.Errs = map[string]error{"List": errors.New("no")}
	reg, err := backend.NewRegistry(fake, other)
	if err != nil {
		t.Fatal(err)
	}
	r, err := (Leftovers{&Env{Dirs: d, Registry: reg}}).Find(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Orphans) != 1 || r.Orphans[0].Name != orphan || !reflect.DeepEqual(r.Unknown, []string{"other", "gone"}) {
		t.Errorf("Find = %+v", r)
	}
}

// The ssh config is written with the running VMs of the backends the
// states name; one of them that can't list fails it, and the config
// stays as it was.
func TestWriteSSHConfig(t *testing.T) {
	d := paths.Dirs{Data: t.TempDir(), Config: t.TempDir()}
	vmState(t, d, "devvm-web-0123456789abc1")
	fake := backendtest.New("fake", nil)
	fake.Errs = map[string]error{"List": errors.New("no")}
	reg, err := backend.NewRegistry(fake)
	if err != nil {
		t.Fatal(err)
	}
	e := &Env{Dirs: d, Registry: reg}
	if err := state.WriteFile(d.SSHConfig(), []byte("before\n")); err != nil {
		t.Fatal(err)
	}
	if err := e.writeSSHConfig(t.Context()); err == nil || err.Error() != "no" {
		t.Errorf("writeSSHConfig = %v", err)
	}
	if data, err := os.ReadFile(d.SSHConfig()); err != nil || string(data) != "before\n" {
		t.Errorf("ssh config = %q, %v", data, err)
	}
	fake.Errs = nil
	if err := e.writeSSHConfig(t.Context()); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(d.SSHConfig()); err != nil || string(data) == "before\n" {
		t.Errorf("ssh config = %q, %v", data, err)
	}
}

// A folder whose disk doesn't answer in time is there: never gone.
func TestFolderWithin(t *testing.T) {
	st := &state.VM{Dir: "/nfs/p"}
	hung := make(chan struct{})
	defer close(hung)
	start := time.Now()
	got := folderWithin(st, func(*state.VM) FolderState { <-hung; return FolderGone }, 20*time.Millisecond)
	if got != FolderThere || time.Since(start) > 5*time.Second {
		t.Errorf("a hung look = %v after %v", got, time.Since(start))
	}
	if got := folderWithin(st, func(*state.VM) FolderState { return FolderGone }, time.Minute); got != FolderGone {
		t.Errorf("an answer in time = %v", got)
	}
}

// A VM's unlock leaves what another VM's .git protection covers, and what
// a VM whose state can't be read may: every path in the folder its state's
// folder is named after.
func TestGitKeeper(t *testing.T) {
	d := paths.Dirs{Data: t.TempDir()}
	parent, err := state.New(d, "devvm-ws-0123456789abcd")
	if err != nil {
		t.Fatal(err)
	}
	parent.Image, parent.Dir, parent.Backend = "base", "/p/ws", "fake"
	if err := parent.Create(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	g := gitprotect.InVM{Mode: gitprotect.On, Made: gitprotect.Made{Protected: true, Nested: true, Repos: map[string]gitprotect.Repo{"a": {}}}}
	if err := parent.Update(t.Context(), func(st *state.VM) error { return gitprotect.Set(st, g) }); err != nil {
		t.Fatal(err)
	}
	broken := d.VMDir(project.Hash("/q/other"))
	if err := os.MkdirAll(broken, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.VM{Dir: broken}.State(), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	inv, err := ReadStates(d)
	if err != nil {
		t.Fatal(err)
	}
	k := inv.GitKeeper("devvm-a-0123456789abce")
	for path, want := range map[string]string{
		"/p/ws/a/.git/config":    "git_kept_locked vm=devvm-ws-0123456789abcd",
		"/p/ws/b/.git/config":    "",
		"/q/other/x/.git/config": "git_kept_unread state=" + paths.VM{Dir: broken}.State(),
	} {
		if got := k(path); got.ID() != "" && got.Names() != want || got.ID() == "" && want != "" {
			t.Errorf("%s: %q, want %q", path, got.Names(), want)
		}
	}
	if inv.GitKeeper("devvm-ws-0123456789abcd")("/p/ws/a/.git/config").ID() != "" {
		t.Error("its own protection keeps it")
	}
}
