package netpolicy

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/devvmhq/devvm/assets"
	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/backend/lima"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/vmname"
)

// TestE2ESelfCheck runs the self-check on a real VM with network policy,
// without a terminal: tests/e2e runs it in its sandbox, with the VM in
// DEVVM_E2E_VM and the result it expects in DEVVM_E2E_WANT (the message's
// names; empty for enforced). A failed check must leave the VM stopped.
func TestE2ESelfCheck(t *testing.T) {
	name := os.Getenv("DEVVM_E2E_VM")
	if name == "" {
		t.Skip("tests/e2e runs it, on a VM of its own")
	}
	vm, err := vmname.Parse(name)
	if err != nil {
		t.Fatal(err)
	}
	d, err := paths.FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	b := lima.New(d, assets.FS, testUI(t), platform.Host{}, "", nil)
	st, err := state.ReadVM(d, vm)
	if err != nil {
		t.Fatal(err)
	}
	want := os.Getenv("DEVVM_E2E_WANT")
	p := &Proxy{Reporter: testUI(t)}
	err = p.SelfCheck(t.Context(), b, st, func(context.Context) error { return errors.New("restarted without a terminal") })
	if msg.NamesOf(err) != want {
		t.Fatalf("SelfCheck = %v, want %q", msg.NamesOf(err), want)
	}
	in, err := b.Inspect(t.Context(), vm)
	if err != nil {
		t.Fatal(err)
	}
	if running := in.Status == backend.StatusRunning; running != (want == "") {
		t.Errorf("status %s after %q", in.Status, want)
	}
}
