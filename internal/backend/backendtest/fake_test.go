package backendtest

import (
	"bytes"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/vmname"
)

// A check that the project folder is mounted is answered as a guest whose
// folder is: without ExecFunc, or when ExecFunc says nothing; an ExecFunc
// that answers, or fails, has its own way. Other commands get no answer.
func TestFakeMountCheck(t *testing.T) {
	check := []string{"sh", "-c", "if mountpoint -q '/p'; then echo devvm-mounted; else echo devvm-not-mounted; fi"}
	for _, tc := range []struct {
		name string
		argv []string
		exec func(vmname.VM, []string, backend.ExecOpts) error
		want string
	}{
		{"no ExecFunc", check, nil, "devvm-mounted\n"},
		{"ExecFunc says nothing", check, func(vmname.VM, []string, backend.ExecOpts) error { return nil }, "devvm-mounted\n"},
		{"ExecFunc answers", check, func(_ vmname.VM, _ []string, o backend.ExecOpts) error {
			_, err := o.Stdout.Write([]byte("devvm-not-mounted\n"))
			return err
		}, "devvm-not-mounted\n"},
		{"ExecFunc fails", check, func(vmname.VM, []string, backend.ExecOpts) error { return errors.New("no ssh") }, ""},
		{"another command", []string{"true"}, nil, ""},
	} {
		f := New("fake", backend.Caps{})
		f.ExecFunc = tc.exec
		var out bytes.Buffer
		_ = f.Exec(t.Context(), "v", tc.argv, backend.ExecOpts{Stdout: &out})
		if out.String() != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, out.String(), tc.want)
		}
	}
}

func TestFakeRecordsAndAnswers(t *testing.T) {
	ctx := t.Context()
	f := New("fake", backend.Caps{backend.FeatureClone: backend.Native})
	f.ShellCmd = backend.Command{Path: "/bin/true", Args: []string{"true"}}

	if _, err := f.BuildImage(ctx, backend.ImageSpec{Image: "base", Key: "k1"}); err != nil {
		t.Fatal(err)
	}
	if err := f.CreateVM(ctx, "k1", backend.VMSpec{Name: "v", Want: backend.Want{Size: backend.Size{CPUs: 2}}}); err != nil {
		t.Fatal(err)
	}
	// A start makes a stopped VM what it wants; a running one stays.
	w := backend.Want{Size: backend.Size{MemoryGiB: 4}, AgentDirs: []string{"/h/.claude"}}
	if err := f.Start(ctx, "v", w, backend.Confinement{Net: backend.NetProxy}); err != nil {
		t.Fatal(err)
	}
	if err := f.Start(ctx, "v", backend.Want{}, backend.Confinement{Net: backend.NetProxy}); err != nil {
		t.Fatal(err)
	}
	in, err := f.Inspect(ctx, "v")
	if err != nil {
		t.Fatal(err)
	}
	if in.Status != backend.StatusRunning || in.Size != (backend.Size{CPUs: 2, MemoryGiB: 4, DiskGiB: 100}) || !slices.Equal(in.AgentDirs, []string{"/h/.claude"}) {
		t.Errorf("instance: %+v", in)
	}
	if c, err := f.Shell(ctx, "v", backend.ShellOpts{Dir: "/p"}); err != nil || c.Path != "/bin/true" {
		t.Errorf("Shell: %+v, %v", c, err)
	}
	if err := f.Delete(ctx, "v"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Inspect(ctx, "v"); !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("Inspect after Delete: %v", err)
	}
	if err := f.Stop(ctx, "v"); !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("Stop after Delete: %v", err)
	}
	want := []string{"BuildImage", "CreateVM", "Start", "Start", "Inspect", "Shell", "Delete", "Inspect", "Stop"}
	if got := f.Methods(); !slices.Equal(got, want) {
		t.Errorf("calls: %q, want %q", got, want)
	}
	if c := f.Calls()[2]; c.Args[0] != vmname.VM("v") || c.Args[2].(backend.Confinement).Net != backend.NetProxy || c.Args[1].(backend.Want).Size.MemoryGiB != 4 {
		t.Errorf("Start's arguments: %v", c.Args)
	}
}

// Running gives the lifeline, the start's ID and its confinement: a VM
// preset running is confined, a stopped one isn't, and one the fake made
// is only once Start started it with NetProxy; the roles Lima has and the
// fake doesn't answer none.
func TestFakeRunningAndNone(t *testing.T) {
	ctx := t.Context()
	f := New("fake", nil)
	f.LifelineFile = "/ha.pid"
	f.Instances["v"] = backend.Instance{Name: "v", Status: backend.StatusRunning}
	if r, err := f.Running(ctx, "v"); !reflect.DeepEqual(r, backend.Run{ID: "start-0", Lifeline: "/ha.pid", Confined: true}) || err != nil {
		t.Errorf("preset running: %+v, %v", r, err)
	}
	f.Instances["v"] = backend.Instance{Name: "v", Status: backend.StatusStopped}
	if r, err := f.Running(ctx, "v"); !reflect.DeepEqual(r, backend.Run{Lifeline: "/ha.pid"}) || err != nil {
		t.Errorf("preset stopped: %+v, %v", r, err)
	}
	if r, err := f.Running(ctx, "none"); !reflect.DeepEqual(r, backend.Run{Lifeline: "/ha.pid"}) || err != nil {
		t.Errorf("not held: %+v, %v", r, err)
	}
	if err := f.Start(ctx, "v", backend.Want{}, backend.Confinement{Net: backend.NetDirect}); err != nil {
		t.Fatal(err)
	}
	if r, err := f.Running(ctx, "v"); !reflect.DeepEqual(r, backend.Run{ID: "start-1", Lifeline: "/ha.pid"}) || err != nil {
		t.Errorf("started direct: %+v, %v", r, err)
	}
	f.Restart("v")
	if r, _ := f.Running(ctx, "v"); r.ID != "start-2" {
		t.Errorf("restarted: %+v", r)
	}
	where, isDefault := f.Location()
	u, uerr := f.Usage(ctx)
	ss, serr := f.Strays(ctx)
	cs, cerr := f.DevvmCache(ctx)
	os, oerr := f.Origin(nil)
	if where != "" || !isDefault || f.DiskDirs() != nil || f.Floors() != (backend.Floors{}) || os != nil || oerr != nil ||
		u.VMs != nil || uerr != nil || ss != nil || serr != nil || cs != nil || cerr != nil {
		t.Error("the fake has something of a role it has none of")
	}
	if err := f.DeleteCache(ctx, nil); err != nil || !slices.Contains(f.Methods(), "DeleteCache") {
		t.Errorf("DeleteCache: %v, %q", err, f.Methods())
	}
}

// A stopped VM's start refuses a disk smaller than its own, changing
// nothing.
func TestFakeStartValidates(t *testing.T) {
	ctx := t.Context()
	f := New("fake", nil)
	f.Instances["v"] = backend.Instance{Name: "v", Status: backend.StatusStopped, Size: backend.Size{DiskGiB: 100}}
	err := f.Start(ctx, "v", backend.Want{Size: backend.Size{DiskGiB: 50}}, backend.Confinement{})
	if msg.NamesOf(err) != "disk_shrink have=100 vm=v want=50" || f.Instances["v"].Status != backend.StatusStopped {
		t.Errorf("Start: %v, %+v", err, f.Instances["v"])
	}
}

func TestFakePresetErrors(t *testing.T) {
	ctx := t.Context()
	boom := errors.New("boom")
	f := New("fake", nil)
	f.Errs = map[string]error{"CreateVM": boom}
	if err := f.CreateVM(ctx, "k1", backend.VMSpec{Name: "v"}); !errors.Is(err, boom) {
		t.Errorf("CreateVM: %v", err)
	}
	if ins, err := f.List(ctx); err != nil || len(ins) != 0 {
		t.Errorf("a failed CreateVM made a VM: %v, %v", ins, err)
	}
}
