package backend_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/backend/backendtest"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/vmname"
)

const (
	baseKey  = "0123456789ab"
	layerKey = "ba9876543210"
)

// Check is the first of a backend's problems, or nil.
func TestCheck(t *testing.T) {
	f := backendtest.New("fake", nil)
	if err := backend.Check(t.Context(), f); err != nil {
		t.Errorf("no problems: %v", err)
	}
	f.Missing = []backend.Problem{{Key: "lima", Msg: msg.ProgramMissing("Lima", "limactl")}, {Key: "qemu", Msg: msg.NoKVM()}}
	if got := msg.IDOf(backend.Check(t.Context(), f)); got != "program_missing" {
		t.Errorf("two problems: %s, want the first", got)
	}
}

func TestImageSpecValidate(t *testing.T) {
	base := backend.ImageSpec{Image: "base", Key: baseKey, Net: backend.NetInternet}
	for _, tc := range []struct {
		name   string
		change func(*backend.ImageSpec)
		err    string // in the error; "" for none
	}{
		{"base", func(*backend.ImageSpec) {}, ""},
		{"offline", func(s *backend.ImageSpec) { s.Net = backend.NetNone }, ""},
		{"a layer", func(s *backend.ImageSpec) { s.Image, s.Key, s.Parent = "base+foo", layerKey, baseKey }, ""},
		{"base --from", func(s *backend.ImageSpec) { s.From = &backend.From{Ref: "node:24"} }, ""},
		{"a layer --from", func(s *backend.ImageSpec) { s.Parent, s.From = layerKey, &backend.From{Ref: "node:24"} }, "only base"},
		{"an unconfined net", func(s *backend.ImageSpec) { s.Net = backend.NetDirect }, "confined"},
		{"the proxy's net", func(s *backend.ImageSpec) { s.Net = backend.NetProxy }, "confined"},
		{"a bad key", func(s *backend.ImageSpec) { s.Key = "../x" }, "invalid image key"},
		{"a bad parent key", func(s *backend.ImageSpec) { s.Parent = "x y" }, "invalid image key"},
		{"no key", func(s *backend.ImageSpec) { s.Key = "" }, "without a key"},
	} {
		spec := base
		tc.change(&spec)
		if err := spec.Validate(); (err == nil) != (tc.err == "") || err != nil && !strings.Contains(err.Error(), tc.err) {
			t.Errorf("%s: %v, want %q", tc.name, err, tc.err)
		}
	}
}

func toolsSpec() backend.ToolsSpec {
	return backend.ToolsSpec{
		Name: "node", Base: baseKey, Out: "/d/tools-build", OutAt: "/mnt/devvm-tools", Net: backend.NetInternet,
		Script: backend.Script{Name: "build.sh", Data: []byte("#!/bin/bash\n")},
	}
}

func TestToolsSpecValidate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*backend.ToolsSpec)
		format string // the backend's
		err    string // in the error; "" for none
	}{
		{"build.sh's format", func(*backend.ToolsSpec) {}, "", ""},
		{"the backend's own format", func(s *backend.ToolsSpec) { s.Format = "ext4" }, "ext4", ""},
		{"offline", func(s *backend.ToolsSpec) { s.Net = backend.NetNone }, "", ""},
		{"another format", func(s *backend.ToolsSpec) { s.Format = "ext4" }, "", "format"},
		{"an unconfined net", func(s *backend.ToolsSpec) { s.Net = backend.NetDirect }, "", "confined"},
		{"a bad key", func(s *backend.ToolsSpec) { s.Base = "../x" }, "", "invalid image key"},
		{"a relative folder", func(s *backend.ToolsSpec) { s.Out = "out" }, "", "absolute"},
		{"a relative mount", func(s *backend.ToolsSpec) { s.OutAt = "mnt" }, "", "absolute"},
	} {
		spec := toolsSpec()
		tc.change(&spec)
		if err := spec.Validate(tc.format); (err == nil) != (tc.err == "") || err != nil && !strings.Contains(err.Error(), tc.err) {
			t.Errorf("%s: %v, want %q", tc.name, err, tc.err)
		}
	}
}

// guardedFake is a registry holding a fake with these capabilities, and
// the fake behind its guard.
func guardedFake(t *testing.T, caps backend.Caps) (*backend.Registry, *backendtest.Fake, backend.Backend) {
	t.Helper()
	f := backendtest.New("fake", caps)
	r, err := backend.NewRegistry(f)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := r.Lookup("fake")
	return r, f, b
}

// A registry's backend refuses, before the backend sees it, a create,
// start or build that needs a feature it lacks or only emulates
// unreviewed, or a build spec that isn't valid: whatever its caller
// checked first.
func TestGuardRefuses(t *testing.T) {
	ctx := t.Context()
	vmSpec := backend.VMSpec{Name: "devvm-x", Want: backend.Want{Project: "/p", Size: backend.Size{CPUs: 2}}}
	image := backend.ImageSpec{Image: "base+nix", Key: layerKey, Parent: baseKey, Net: backend.NetInternet}
	for _, tc := range []struct {
		name string
		call func(backend.Backend) error
		want string // the refusal's names, or a text in its error
	}{
		{
			"a create without the project mount", func(b backend.Backend) error { return b.CreateVM(ctx, baseKey, vmSpec) },
			"backend_lacks backend=fake feature=project-mount what=devvm-x",
		},
		{"a start with network policy", func(b backend.Backend) error {
			return b.Start(ctx, "devvm-x", backend.Want{}, backend.Confinement{Net: backend.NetProxy})
		}, "backend_unreviewed backend=fake feature=egress-proxy what=devvm-x"},
		{"a start with a size", func(b backend.Backend) error {
			return b.Start(ctx, "devvm-x", backend.Want{Size: backend.Size{DiskGiB: 80}}, backend.Confinement{})
		}, "backend_lacks backend=fake feature=disk what=devvm-x"},
		{
			"a build of the nix layer", func(b backend.Backend) error { _, err := b.BuildImage(ctx, image); return err },
			"backend_lacks backend=fake feature=nix-layer what=base+nix",
		},
		{
			"a build on the internet", func(b backend.Backend) error { return b.BuildTools(ctx, toolsSpec()) },
			"backend_lacks backend=fake feature=build-confinement what=node",
		},
		{"an image build with no key", func(b backend.Backend) error {
			_, err := b.BuildImage(ctx, backend.ImageSpec{Image: "base", Net: backend.NetInternet})
			return err
		}, "without a key"},
		{"a tools build in another format", func(b backend.Backend) error {
			s := toolsSpec()
			s.Format = "ext4"
			return b.BuildTools(ctx, s)
		}, "format"},
	} {
		_, f, b := guardedFake(t, backend.Caps{
			backend.FeatureClone: backend.Native, backend.FeatureCPUs: backend.Native,
			backend.FeatureEgressProxy: backend.Emulated(false), backend.FeatureBuildImage: backend.Native,
		})
		err := tc.call(b)
		if got := msg.NamesOf(err); got != tc.want && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: %v, want %q", tc.name, got, tc.want)
		}
		if ms := f.Methods(); len(ms) != 0 {
			t.Errorf("%s: the backend was asked %q", tc.name, ms)
		}
	}
}

// What the backend has, or an unreviewed emulation let through, reaches
// it, without a second warning: the caller's own Require gave one.
func TestGuardLetsThrough(t *testing.T) {
	ctx := t.Context()
	r, f, b := guardedFake(t, backend.Caps{
		backend.FeatureClone: backend.Native, backend.FeatureProjectMount: backend.Native,
		backend.FeatureEgressProxy: backend.Emulated(false), backend.FeatureBuildImage: backend.Native,
		backend.FeatureBuildConfinement: backend.Native,
	})
	f.ImageKeys = []string{baseKey}
	var warned msg.Recorder
	r.Unreviewed, r.Reporter = backend.ParseUnreviewed("fake:egress-proxy"), &msg.Fake{OnWarn: warned.Add}
	if err := b.CreateVM(ctx, baseKey, backend.VMSpec{Name: "devvm-x", Project: "/p"}); err != nil {
		t.Errorf("CreateVM: %v", err)
	}
	if err := b.Start(ctx, "devvm-x", backend.Want{}, backend.Confinement{Net: backend.NetProxy}); err != nil {
		t.Errorf("Start: %v", err)
	}
	if _, err := b.BuildImage(ctx, backend.ImageSpec{Image: "base", Key: baseKey, Net: backend.NetNone}); err != nil {
		t.Errorf("BuildImage: %v", err)
	}
	if err := b.BuildTools(ctx, toolsSpec()); err != nil {
		t.Errorf("BuildTools: %v", err)
	}
	if ms := f.Methods(); !slices.Equal(ms, []string{"CreateVM", "Start", "BuildImage", "BuildTools"}) {
		t.Errorf("the backend was asked %q", ms)
	}
	if len(warned.Names()) != 0 {
		t.Errorf("warned %q", warned.Names())
	}
}

// forgetful is a backend with the optional Forgetter role.
type forgetful struct {
	*backendtest.Fake
	forgot *[]vmname.VM
}

func (f forgetful) Forget(_ context.Context, vm vmname.VM) error {
	*f.forgot = append(*f.forgot, vm)
	return nil
}

// As finds a registry's backend's optional role behind its guard, which
// a type assertion doesn't.
func TestAs(t *testing.T) {
	var forgot []vmname.VM
	k := forgetful{backendtest.New("forgetful", nil), &forgot}
	r, err := backend.NewRegistry(k, backendtest.New("plain", nil))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := r.Lookup("forgetful")
	if _, ok := b.(backend.Forgetter); ok {
		t.Error("the guard is a Forgetter: As isn't needed")
	}
	if f, ok := backend.As[backend.Forgetter](b); !ok || f.Forget(t.Context(), "devvm-x") != nil || !slices.Equal(forgot, []vmname.VM{"devvm-x"}) {
		t.Errorf("As: no Forgetter behind the guard (forgot %q)", forgot)
	}
	if _, ok := backend.As[backend.Forgetter](k); !ok {
		t.Error("As: no Forgetter on the backend itself")
	}
	plain, _ := r.Lookup("plain")
	if _, ok := backend.As[backend.Forgetter](plain); ok {
		t.Error("As: a Forgetter of a backend without")
	}
	// A guarded backend registered again is guarded once, by its new
	// registry.
	again, err := backend.NewRegistry(b)
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := again.Lookup("forgetful")
	if _, ok := backend.As[backend.Forgetter](b2); !ok {
		t.Error("As: no Forgetter behind a registry's guard of a guarded backend")
	}
}

// The guard has every role, guarded: a guarded backend handed on as
// Images or Machines alone still refuses what its backend lacks.
func TestGuardRoles(t *testing.T) {
	_, f, b := guardedFake(t, nil)
	var (
		_ backend.Host         = b
		_ backend.Housekeeping = b
	)
	var im backend.Images = b
	var ms backend.Machines = b
	ctx := t.Context()
	if _, err := im.BuildImage(ctx, backend.ImageSpec{Image: "base", Key: baseKey, Net: backend.NetNone}); msg.IDOf(err) != "backend_lacks" {
		t.Errorf("Images.BuildImage: %v", err)
	}
	if err := ms.CreateVM(ctx, baseKey, backend.VMSpec{Name: "devvm-x", Project: "/p"}); msg.IDOf(err) != "backend_lacks" {
		t.Errorf("Machines.CreateVM: %v", err)
	}
	if got := f.Methods(); len(got) != 0 {
		t.Errorf("the backend was asked %q", got)
	}
}
