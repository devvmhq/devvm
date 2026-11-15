package vm

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/project"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/vmname"
)

// newOpts is a new VM that asks nothing.
func newOpts() Options { return Options{Network: "open", Ports: "none"} }

// countOf is how many times ms has method.
func countOf(ms []string, method string) int {
	n := 0
	for _, m := range ms {
		if m == method {
			n++
		}
	}
	return n
}

// Two creations at once in one folder make one VM and one state: the
// second, which finds no VM while the first is asking its questions,
// refuses at once (create_running) instead of waiting, and the first
// makes the VM, which the next devvm opens. The lock is the folder's,
// outside the VM's folder, and gone once it's let go of.
func TestUpCreateLock(t *testing.T) {
	e, f, execs := upEnv(t, allNative())
	second := *e
	wire(&second)
	asking, answered := make(chan struct{}), make(chan struct{})
	var once sync.Once
	host := e.Host
	e.Host = func() (Host, error) {
		once.Do(func() { close(asking); <-answered })
		return host()
	}
	first := make(chan error, 1)
	go func() { first <- Launch{e}.Up(t.Context(), newOpts()) }()
	<-asking
	err := Launch{&second}.Up(t.Context(), newOpts())
	close(answered)
	if err1 := <-first; err1 != nil {
		t.Fatal(msg.NamesOf(err1))
	}
	vm := vmName(t, e)
	if msg.NamesOf(err) != "create_running vm="+vm.String() {
		t.Errorf("second creation: %v", msg.NamesOf(err))
	}
	if n := countOf(f.Methods(), "CreateVM"); n != 1 || len(f.Instances) != 1 {
		t.Errorf("CreateVM called %d times, %d VMs", n, len(f.Instances))
	}
	if names, err := state.VMNames(e.Dirs); err != nil || !slices.Equal(names, []vmname.VM{vm}) {
		t.Errorf("states = %q (%v)", names, err)
	}
	if st, err := state.ReadVM(e.Dirs, vm); err != nil || !st.Finished() {
		t.Errorf("state finished: %v (%v)", st.Finished(), err)
	}
	if err := (Launch{&second}).Up(t.Context(), newOpts()); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if n := countOf(f.Methods(), "CreateVM"); n != 1 || len(*execs) != 2 {
		t.Errorf("after: CreateVM called %d times, %d shells", n, len(*execs))
	}
	h, _ := vm.Hash()
	if _, err := os.Lstat(e.Dirs.CreateLock(h)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the lock file stays: %v", err)
	}
}

// A VM's teardown deletes its folder's creation lock that a killed devvm
// left, and keeps one a devvm holds.
func TestTeardownDropsCreateLock(t *testing.T) {
	e, _, _ := upEnv(t, allNative())
	vm := existingVM(t, e, newOpts()).Name()
	h, _ := vm.Hash()
	lock := e.Dirs.CreateLock(h)
	if err := state.WriteFile(lock, nil); err != nil {
		t.Fatal(err)
	}
	dropCreateLock(e, vm)
	if _, err := os.Lstat(lock); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("left one stays: %v", err)
	}
	release, err := holdCreate(e, vm)
	if err != nil {
		t.Fatal(err)
	}
	dropCreateLock(e, vm)
	if _, err := os.Lstat(lock); err != nil {
		t.Errorf("held one: %v", err)
	}
	release()
	dropCreateLock(e, "not-a-vm")
}

// An interrupted creation holds the lock until its cleanups have run,
// its own first: no other devvm takes the folder meanwhile.
func TestUpInterruptedHoldsCreateLock(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	var cleanups []func(context.Context)
	fake(e).OnCleanup = func(c func(context.Context)) func() {
		cleanups = append(cleanups, c)
		return func() {}
	}
	fake(e).Interrupt = true
	f.Errs = map[string]error{"CreateVM": context.Canceled}
	if err := (Launch{e}).Up(t.Context(), newOpts()); err == nil {
		t.Fatal("made")
	}
	vm := vmName(t, e)
	if _, err := holdCreate(e, vm); msg.NamesOf(err) != "create_running vm="+vm.String() {
		t.Errorf("before the cleanups: %v", msg.NamesOf(err))
	}
	for i := len(cleanups) - 1; i >= 0; i-- {
		cleanups[i](t.Context())
	}
	release, err := holdCreate(e, vm)
	if err != nil {
		t.Fatalf("after the cleanups: %v", msg.NamesOf(err))
	}
	release()
}

// A devvm that found no VM, and gets the lock once another devvm has
// made it, opens that one instead of making it again.
func TestUpMadeMeanwhile(t *testing.T) {
	e, f, execs := upEnv(t, allNative())
	stale, err := (finder{e}).target(t.Context(), need{folder: true})
	if err != nil || stale.exists || stale.st.Exists() {
		t.Fatalf("found %+v (%v)", stale, err)
	}
	existingVM(t, e, newOpts())
	if err := (Launch{e}).makeHeld(t.Context(), newOpts(), stale); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if n := countOf(f.Methods(), "CreateVM"); n != 1 || len(*execs) != 2 {
		t.Errorf("CreateVM called %d times, %d shells", n, len(*execs))
	}
}

// While another devvm makes the folder's VM, up doesn't offer to delete
// the unfinished VM and make it again, and devvm rm deletes it neither by
// the folder nor by its name: each refuses (create_running). Once the
// lock is let go of, rm deletes it.
func TestCreateLockRefuses(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	st := existingVM(t, e, newOpts())
	vm := st.Name()
	change(t, st, func(st *state.VM) error { st.Created = time.Time{}; return nil })
	release, err := holdCreate(e, vm)
	if err != nil {
		t.Fatal(err)
	}
	fake(e).Terminal = true
	if err := (Launch{e}).Up(t.Context(), newOpts()); msg.NamesOf(err) != "create_running vm="+vm.String() {
		t.Errorf("up: %v", msg.NamesOf(err))
	}
	lc := Lifecycle{e}
	if err := lc.Rm(t.Context(), RmOptions{Force: true}); msg.NamesOf(err) != "create_running vm="+vm.String() {
		t.Errorf("rm: %v", msg.NamesOf(err))
	}
	gone := &target{f: project.Folder{Dir: st.Dir, Name: vm}, st: st, b: f, exists: true}
	if err := lc.rmGone(t.Context(), gone, FolderGone, RmOptions{Force: true}); msg.NamesOf(err) != "create_running vm="+vm.String() {
		t.Errorf("rm VM: %v", msg.NamesOf(err))
	}
	if _, ok := f.Instances[vm]; !ok {
		t.Fatal("deleted while it was being made")
	}
	release()
	if err := lc.Rm(t.Context(), RmOptions{Force: true}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if _, ok := f.Instances[vm]; ok {
		t.Error("kept")
	}
}

// rm reads the VM's state again under the lock: an unfinished VM whose
// creation another devvm recorded since rm found it is refused too, and
// so is a finished one made again since.
func TestRmChangedMeanwhile(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	st := existingVM(t, e, newOpts())
	vm, created := st.Name(), st.Created
	change(t, st, func(st *state.VM) error { st.Created = time.Time{}; return nil })
	unfinished, err := state.ReadVM(e.Dirs, vm)
	if err != nil {
		t.Fatal(err)
	}
	change(t, st, func(st *state.VM) error { st.Created = created; return nil })
	t1 := &target{f: project.Folder{Dir: st.Dir, Name: vm}, st: unfinished, b: f, exists: true}
	if _, err := holdForRm(e, t1); msg.NamesOf(err) != "create_running vm="+vm.String() {
		t.Errorf("finished since: %v", msg.NamesOf(err))
	}
	finished, err := state.ReadVM(e.Dirs, vm)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(e.Dirs.VM(vm).State()); err != nil {
		t.Fatal(err)
	}
	t2 := &target{f: t1.f, st: finished, b: f, exists: true}
	if _, err := holdForRm(e, t2); msg.NamesOf(err) != "create_running vm="+vm.String() {
		t.Errorf("deleted since: %v", msg.NamesOf(err))
	}
	// Each refusal let go of the lock.
	release, err := holdCreate(e, vm)
	if err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	release()
}

// devvm gc --orphans forgets an orphan under its folder's creation lock:
// one that a devvm is making again, its state written before its backend
// has it, is refused and kept, however old its state.
func TestForgetHoldsCreateLock(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	st := existingVM(t, e, newOpts())
	vm := st.Name()
	delete(f.Instances, vm)
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(e.Dirs.VM(vm).State(), old, old); err != nil {
		t.Fatal(err)
	}
	r, err := (Leftovers{e}).Find(t.Context())
	if err != nil || len(r.Orphans) != 1 {
		t.Fatalf("Find = %+v, %v", r, err)
	}
	release, err := holdCreate(e, vm)
	if err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if err := (Leftovers{e}).Forget(t.Context(), r.Orphans[0]); msg.NamesOf(err) != "create_running vm="+vm.String() {
		t.Errorf("forget while held: %v", msg.NamesOf(err))
	}
	if _, err := os.Stat(e.Dirs.VM(vm).State()); err != nil {
		t.Fatal("its state went while it was being made")
	}
	release()
	if err := (Leftovers{e}).Forget(t.Context(), r.Orphans[0]); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if left := vmsLeft(t, e); len(left) > 0 {
		t.Errorf("left %q", left)
	}
}
