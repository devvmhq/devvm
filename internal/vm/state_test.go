package vm

import (
	"context"
	"errors"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/netpolicy"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/ports"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/tools"
	"github.com/devvmhq/devvm/internal/vmname"
)

// vmState writes the state of a VM named name as up does: made from base
// on the fake backend, for the folder /p, finished, with each change of
// set's applied before it's written.
func vmState(t *testing.T, d paths.Dirs, name vmname.VM, set ...func(*state.VM) error) *state.VM {
	t.Helper()
	st, err := state.New(d, name)
	if err != nil {
		t.Fatal(err)
	}
	st.Image, st.Dir, st.Backend, st.Created = "base", "/p", "fake", time.Now()
	// Open unless set says otherwise.
	for _, f := range append([]func(*state.VM) error{withNet(false)}, set...) {
		if err := f(st); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Create(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	return st
}

// change changes a VM's state as f says, as devvm does (state.VM.Update).
func change(t *testing.T, st *state.VM, f func(*state.VM) error) {
	t.Helper()
	if err := st.Update(t.Context(), f); err != nil {
		t.Fatal(err)
	}
}

// withTools gives a VM these shared tools, by name.
func withTools(names ...string) func(*state.VM) error {
	return func(st *state.VM) error { return tools.Set(st, tools.InVM{Names: names}) }
}

// withNet gives a VM the allowlist tier, or the open one.
func withNet(on bool) func(*state.VM) error {
	t := netpolicy.TierOpen
	if on {
		t = netpolicy.TierAllowlist
	}
	return withTier(t)
}

// withTier gives a VM a tier.
func withTier(t netpolicy.Tier) func(*state.VM) error {
	return func(st *state.VM) error { return netpolicy.SetTier(st, t, netpolicy.Learning{}) }
}

// withLearning makes a VM learn, on the internet tier, since since.
func withLearning(since time.Time) func(*state.VM) error {
	return func(st *state.VM) error {
		return netpolicy.SetTier(st, netpolicy.TierInternet, netpolicy.Learning{Since: since})
	}
}

// withPublish has a VM publish ports (linksInVM.Publish).
func withPublish(p string) func(*state.VM) error {
	return func(st *state.VM) error {
		return edit(st, linksSection, func(l *linksInVM) error { l.Publish = p; return nil })
	}
}

// withForward has a VM forward these ports (ports.Ports.Forward).
func withForward(p string) func(*state.VM) error {
	return func(st *state.VM) error { return ports.SetForward(st, p) }
}

// reread reads a VM's state again, as another devvm would.
func readState(t *testing.T, d paths.Dirs, name vmname.VM) *state.VM {
	t.Helper()
	st, err := state.ReadVM(d, name)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// A section of the folder's VM's state that its package can't read is
// refused as the VM is found, never read as empty: network policy that
// reads as off would start the VM without it.
func TestCheckState(t *testing.T) {
	d := paths.Dirs{Data: t.TempDir()}
	st := vmState(t, d, "devvm-p-00000000000000", withNet(true))
	if err := checkState(st); err != nil {
		t.Fatalf("a VM made as up makes it: %v", err)
	}
	for _, section := range []string{
		netpolicy.Section, "git", ports.Section, "agent", "secrets", tools.Section,
		sizeSection, cloneSection, devcontainerSection, desktopSection, linksSection,
	} {
		change(t, st, func(st *state.VM) error { return state.SetSection(st, section, []int{1}) })
		var ce *state.CorruptError
		if err := checkState(readState(t, d, st.Name())); !errors.As(err, &ce) {
			t.Errorf("%s isn't its type: %v, want a CorruptError", section, err)
		}
		change(t, st, func(st *state.VM) error { return state.SetSection(st, section, struct{}{}) })
	}
	// A link that isn't one, too.
	change(t, st, func(st *state.VM) error {
		return edit(st, linksSection, func(l *linksInVM) error { l.Links = []Link{{Name: "-bad", Port: 1}}; return nil })
	})
	if err := checkState(readState(t, d, st.Name())); err == nil {
		t.Error("a bad link: want an error")
	}
}

// Two devvms changing one VM at once, each through its own command, lose
// neither's change: each reads the state again under the VM's lock.
func TestChangesTogether(t *testing.T) {
	d := paths.Dirs{Data: t.TempDir()}
	const vm vmname.VM = "devvm-p-00000000000000"
	vmState(t, d, vm)
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	// One publishes ports, one ports forwards, one binds secrets, each
	// from the state it read before the others wrote.
	for _, f := range []func(*state.VM) error{
		func(st *state.VM) error { return withPublish("5432")(st) },
		func(st *state.VM) error { return withForward("3000")(st) },
		func(st *state.VM) error { return state.SetSection(st, sizeSection, Size{CPUs: "4"}) },
	} {
		st := readState(t, d, vm)
		wg.Go(func() { errs <- st.Update(context.WithoutCancel(t.Context()), f) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	st := readState(t, d, vm)
	if linksOf(st).Publish != "5432" || sectionOf[ports.Ports](st, ports.Section).Forward != "3000" || sectionOf[Size](st, sizeSection).CPUs != "4" {
		t.Errorf("a change was lost: publish %q, forward %q, size %+v", linksOf(st).Publish,
			sectionOf[ports.Ports](st, ports.Section).Forward, sectionOf[Size](st, sizeSection))
	}
}

// A command's changes land together or not at all: a change that fails
// part way (dropping a port the VM doesn't publish, after setting
// another) writes nothing.
func TestChangesAtOnce(t *testing.T) {
	d := paths.Dirs{Data: t.TempDir()}
	st := vmState(t, d, "devvm-p-00000000000000", withPublish("5432"))
	before, err := os.ReadFile(d.VM(st.Name()).State())
	if err != nil {
		t.Fatal(err)
	}
	err = update(t.Context(), st, linksSection, func(l *linksInVM) error {
		l.Group = "db"
		var err error
		l.Publish, err = dropPorts(l.Publish, "8080", msg.PortNotPublished)
		return err
	})
	if err == nil {
		t.Fatal("dropping a port it doesn't publish: want an error")
	}
	if after, _ := os.ReadFile(d.VM(st.Name()).State()); !slices.Equal(after, before) || linksOf(st).Publish != "5432" || groupOf(st) != "" {
		t.Errorf("a failed change wrote:\n%s", after)
	}
}
