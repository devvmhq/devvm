package vmssh

import (
	"context"
	"fmt"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/vmname"
)

// HostKeys is how Checked gets a VM's host keys for its current start.
type HostKeys int

const (
	// ReadKeys takes the record devvm pinned already (KnownHostsFor).
	ReadKeys HostKeys = iota
	// PinKeys reads them from the VM first when there's no record for its
	// start (PinHostKeys).
	PinKeys
)

// UnusableError is a VM whose backend's ssh devvm can't use: the review
// doesn't let its ssh through, the backend can't say how ssh reaches it,
// or what it says isn't a user and a key on a host loopback port
// (FromTarget). Its text is its cause's.
type UnusableError struct {
	VM  vmname.VM
	Err error
}

func (e *UnusableError) Error() string { return e.Err.Error() }
func (e *UnusableError) Unwrap() error { return e.Err }

// Reach is how Checked found a running VM's ssh: devvm's own (SSH, its
// KnownHosts the VM's record), the backend's other ssh options, and the
// start the record is of (backend.Run.ID).
type Reach struct {
	SSH     SSH
	Options []backend.SSHOption
	Start   string
}

// Checked is the one way devvm's own ssh, and its ssh config, reach a
// running VM: its backend's ssh, when r lets it through, checked by
// FromTarget, with the VM's host keys for its current start, read or
// pinned as keys says. A failure before the host keys is an
// *UnusableError; the host keys' own wrap ErrNoHostKeys, unless reading
// or writing the record failed.
func Checked(ctx context.Context, d paths.Dirs, r backend.Review, b backend.Backend, vm vmname.VM, keys HostKeys) (Reach, error) {
	if !r.Allowed(b, backend.FeatureSSH) {
		return Reach{}, &UnusableError{vm, fmt.Errorf("%s: the %s backend's ssh isn't usable", vm, b.Name())}
	}
	t, err := b.SSH(ctx, vm)
	if err != nil {
		return Reach{}, &UnusableError{vm, err}
	}
	s, err := FromTarget(vm, t)
	if err != nil {
		return Reach{}, &UnusableError{vm, err}
	}
	if keys == PinKeys {
		s.KnownHosts, err = PinHostKeys(ctx, d, b, vm)
	} else {
		s.KnownHosts, err = KnownHostsFor(ctx, d, b, vm)
	}
	if err != nil {
		return Reach{}, err
	}
	// The record is of the current start: both just checked it.
	start, err := recordedStart(s.KnownHosts)
	if err != nil {
		return Reach{}, err
	}
	return Reach{SSH: s, Options: t.Options, Start: start}, nil
}
