package backendtest

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/vmname"
)

// fakeSubject is a fake as Conformance checks it: holding an image, with
// a lifeline and what reaches a running VM set.
func fakeSubject(t *testing.T) (Subject, *Fake) {
	t.Helper()
	dir := t.TempDir()
	f := New("fake", nil)
	f.ImageKeys = []string{"0123456789ab"}
	f.LifelineFile = filepath.Join(dir, "ha.pid")
	f.Target = backend.SSHTarget{Host: "127.0.0.1", Port: 60022, User: "u", Key: filepath.Join(dir, "key")}
	f.ShellCmd = backend.Command{Path: "/usr/bin/ssh", Args: []string{"ssh", "vm"}}
	return Subject{
		Backend: f, Image: "0123456789ab", Dir: dir, Boots: true,
		Restart: func(_ context.Context, vm vmname.VM) error { f.Restart(vm); return nil },
	}, f
}

// The fake keeps the contract.
func TestConformance(t *testing.T) {
	Conformance(t, func(t *testing.T) Subject { s, _ := fakeSubject(t); return s })
}

// Fakes that break the contract, each in one method.
type (
	staleRun        struct{ *Fake } // a start's ID on a stopped VM, and on every start
	alwaysConfined  struct{ *Fake } // confined however it started
	noLifeline      struct{ *Fake } // no lifeline
	fewCaps         struct{ *Fake } // Caps that leave features out
	looseInspect    struct{ *Fake } // a missing VM isn't ErrNotFound
	restartsRunning struct{ *Fake } // a start of a running VM starts it again
	keepsImages     struct{ *Fake } // DeleteImage deletes nothing
	reachesStopped  struct{ *Fake } // SSH reaches a stopped VM
	sizeless        struct{ *Fake } // a VM without its size
	wrongProxy      struct{ *Fake } // a proxy without the VM's password
)

func (m staleRun) Running(ctx context.Context, vm vmname.VM) (backend.Run, error) {
	r, err := m.Fake.Running(ctx, vm)
	r.ID = "start-1"
	return r, err
}

func (m alwaysConfined) Running(ctx context.Context, vm vmname.VM) (backend.Run, error) {
	r, err := m.Fake.Running(ctx, vm)
	r.Confined = true
	return r, err
}

func (m noLifeline) Running(ctx context.Context, vm vmname.VM) (backend.Run, error) {
	r, err := m.Fake.Running(ctx, vm)
	r.Lifeline = ""
	return r, err
}

func (fewCaps) Caps() backend.Caps { return backend.Caps{backend.FeatureClone: backend.Native} }

func (m looseInspect) Inspect(ctx context.Context, vm vmname.VM) (backend.Instance, error) {
	in, err := m.Fake.Inspect(ctx, vm)
	if errors.Is(err, backend.ErrNotFound) {
		err = errors.New("no " + vm.String())
	}
	return in, err
}

func (m restartsRunning) Start(ctx context.Context, vm vmname.VM, w backend.Want, c backend.Confinement) error {
	if in, err := m.Inspect(ctx, vm); err == nil && in.Status == backend.StatusRunning {
		m.Restart(vm)
		return nil
	}
	return m.Fake.Start(ctx, vm, w, c)
}

func (keepsImages) DeleteImage(context.Context, string) error { return nil }

func (m reachesStopped) SSH(context.Context, vmname.VM) (backend.SSHTarget, error) {
	return m.Target, nil
}

func (m sizeless) Inspect(ctx context.Context, vm vmname.VM) (backend.Instance, error) {
	in, err := m.Fake.Inspect(ctx, vm)
	in.Size = backend.Size{}
	return in, err
}

func (wrongProxy) ProxyURL(c backend.Confinement) string {
	if c.Net != backend.NetProxy {
		return ""
	}
	return backend.ProxyURL("proxy.test", c.ProxyPort, "another")
}

// The suite finds each of these fakes' breaks, in the check named.
func TestConformanceFinds(t *testing.T) {
	for _, tc := range []struct {
		check string
		wrap  func(*Fake) backend.Backend
	}{
		{"stopped", func(f *Fake) backend.Backend { return staleRun{f} }},
		{"confined", func(f *Fake) backend.Backend { return staleRun{f} }},
		{"stopped", func(f *Fake) backend.Backend { return alwaysConfined{f} }},
		{"confined", func(f *Fake) backend.Backend { return alwaysConfined{f} }},
		{"restart", func(f *Fake) backend.Backend { return alwaysConfined{f} }},
		{"stopped", func(f *Fake) backend.Backend { return noLifeline{f} }},
		{"caps", func(f *Fake) backend.Backend { return fewCaps{f} }},
		{"inspect-missing", func(f *Fake) backend.Backend { return looseInspect{f} }},
		{"rename", func(f *Fake) backend.Backend { return looseInspect{f} }},
		{"delete", func(f *Fake) backend.Backend { return looseInspect{f} }},
		{"start", func(f *Fake) backend.Backend { return restartsRunning{f} }},
		{"delete-image", func(f *Fake) backend.Backend { return keepsImages{f} }},
		{"stopped", func(f *Fake) backend.Backend { return reachesStopped{f} }},
		{"create", func(f *Fake) backend.Backend { return sizeless{f} }},
		{"proxy-url", func(f *Fake) backend.Backend { return wrongProxy{f} }},
	} {
		s, f := fakeSubject(t)
		s.Backend = tc.wrap(f)
		i := slices.IndexFunc(checks(), func(c check) bool { return c.name == tc.check })
		if i < 0 {
			t.Fatalf("no check %s", tc.check)
		}
		if err := checks()[i].run(t.Context(), s); err == nil {
			t.Errorf("%s passes %T", tc.check, s.Backend)
		}
	}
}

// A backend whose VMs don't start skips the checks that need one.
func TestConformanceWithoutBooting(t *testing.T) {
	Conformance(t, func(t *testing.T) Subject {
		s, _ := fakeSubject(t)
		s.Boots, s.Restart = false, nil
		return s
	})
}
