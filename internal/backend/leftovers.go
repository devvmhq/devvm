package backend

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"github.com/devvmhq/devvm/internal/vmname"
)

// LocationOf is where h's VMs live in this run (Host.Location), as a VM's
// state records it (a hash, so any path fits), and whether that's h's
// default; "" and true for a backend whose VMs can't move.
func LocationOf(h Host) (string, bool) {
	where, isDefault := h.Location()
	if where == "" {
		return "", true
	}
	sum := sha256.Sum256([]byte(where))
	return hex.EncodeToString(sum[:8]), isDefault
}

// Forgetter is a backend that keeps files or resources of its own for a
// VM besides the VM (an ssh key, say: Lima keeps none), which devvm gc
// --orphans deletes once the backend no longer has the VM, as Delete does
// after deleting it. It's the one role a backend may leave out, found
// with As.
type Forgetter interface {
	// Forget deletes what the backend keeps for vm, which it must not
	// have: a VM it has is refused, and nothing is deleted.
	Forget(ctx context.Context, vm vmname.VM) error
}

// Stray is a VM with devvm's label that this devvm has no state for: one
// made by a devvm with another data folder (a sandbox's, a test's), or
// one whose state is gone (Housekeeping.Strays).
type Stray struct {
	Name vmname.VM
	Data string // the data folder its label names
}
