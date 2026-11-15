package backend_test

import (
	"slices"
	"testing"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/backend/backendtest"
	"github.com/devvmhq/devvm/internal/msg"
)

func TestSupport(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		s                            backend.Support
		native, emulated, ok, review bool
		shown                        string
	}{
		{"native", backend.Native, true, false, true, false, "native"},
		{"none", backend.None, false, false, false, false, "none"},
		{"emulated, reviewed", backend.Emulated(true), false, true, true, true, "emulated"},
		{"emulated, unreviewed", backend.Emulated(false), false, true, false, false, "unreviewed"},
	} {
		s := tc.s
		if s.IsNative() != tc.native || s.IsEmulated() != tc.emulated || s.Usable() != tc.ok || s.Reviewed() != tc.review {
			t.Errorf("%s: native %v, emulated %v, usable %v, reviewed %v", tc.name, s.IsNative(), s.IsEmulated(), s.Usable(), s.Reviewed())
		}
		if s.String() != tc.shown {
			t.Errorf("%s: shown as %q, want %q", tc.name, s.String(), tc.shown)
		}
	}
}

func TestRequire(t *testing.T) {
	t.Parallel()
	b := backendtest.New("fake", backend.Caps{
		backend.FeatureClone:           backend.Native,
		backend.FeatureSSH:             backend.Emulated(true),
		backend.FeatureEgressProxy:     backend.Emulated(false),
		backend.FeatureDisk:            backend.None,
		backend.FeatureAgentForwarding: backend.Native,
	})
	for _, tc := range []struct {
		name     string
		features []backend.Feature
		want     string
	}{
		{"nothing", nil, ""},
		{"native", []backend.Feature{backend.FeatureClone}, ""},
		{"reviewed emulation", []backend.Feature{backend.FeatureSSH}, ""},
		{"unreviewed emulation", []backend.Feature{backend.FeatureEgressProxy}, "backend_unreviewed backend=fake feature=egress-proxy what=devvm-x"},
		{"none", []backend.Feature{backend.FeatureDisk}, "backend_lacks backend=fake feature=disk what=devvm-x"},
		{"not listed", []backend.Feature{backend.FeaturePorts}, "backend_lacks backend=fake feature=ports what=devvm-x"},
		{"the first refused", []backend.Feature{backend.FeatureClone, backend.FeaturePorts, backend.FeatureEgressProxy}, "backend_lacks backend=fake feature=ports what=devvm-x"},
	} {
		if got := msg.NamesOf(backend.Review{}.Require(b, "devvm-x", tc.features...)); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestEveryFeatureIsListed(t *testing.T) {
	seen := map[backend.Feature]bool{}
	for _, f := range backend.Features {
		if seen[f] {
			t.Errorf("%s is listed twice", f)
		}
		seen[f] = true
	}
	if len(seen) != 21 {
		t.Errorf("%d features; a new one goes in Features and on the backends page", len(seen))
	}
}

// Caps list every feature, none better than what it's built on.
func TestCapsCheck(t *testing.T) {
	all := func(s backend.Support) backend.Caps {
		c := backend.Caps{}
		for _, f := range backend.Features {
			c[f] = s
		}
		return c
	}
	with := func(c backend.Caps, f backend.Feature, s backend.Support) backend.Caps { c[f] = s; return c }
	without := func(c backend.Caps, f backend.Feature) backend.Caps { delete(c, f); return c }
	for _, tc := range []struct {
		name string
		caps backend.Caps
		ok   bool
	}{
		{"all native", all(backend.Native), true},
		{"all none", all(backend.None), true},
		{"one unlisted", without(all(backend.Native), backend.FeatureLink), false},
		{"an unknown one", with(all(backend.Native), "teleport", backend.Native), false},
		{"ports-all native on emulated ports", with(all(backend.Native), backend.FeaturePorts, backend.Emulated(true)), false},
		{"ports-all emulated on emulated ports", with(with(all(backend.Native), backend.FeaturePorts, backend.Emulated(true)), backend.FeaturePortsAll, backend.Emulated(true)), true},
		{"reviewed on unreviewed", with(with(all(backend.Native), backend.FeaturePorts, backend.Emulated(false)), backend.FeaturePortsAll, backend.Emulated(true)), false},
		{"agent-toggle without forwarding", with(all(backend.Native), backend.FeatureAgentForwarding, backend.None), false},
		{"link without ssh", with(all(backend.Native), backend.FeatureSSH, backend.None), false},
	} {
		if err := tc.caps.Check(); (err == nil) != tc.ok {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}

// The registry refuses a backend whose Caps leave a feature out.
func TestRegisterChecksCaps(t *testing.T) {
	f := backendtest.New("fake", nil)
	delete(f.Capabilities, backend.FeatureDisk)
	if _, err := backend.NewRegistry(f); err == nil {
		t.Error("registered a backend without disk listed")
	}
	if _, err := backend.NewRegistry(backendtest.New("fake", nil)); err != nil {
		t.Errorf("the fake lists every feature: %v", err)
	}
}

func TestFeaturesNeeded(t *testing.T) {
	f := func(fs ...backend.Feature) []backend.Feature { return fs }
	for _, tc := range []struct {
		name string
		got  []backend.Feature
		want []backend.Feature
	}{
		{"a layer", backend.ImageSpec{Net: backend.NetInternet}.Features(), f(backend.FeatureBuildImage, backend.FeatureBuildConfinement)},
		{
			"base --from, offline, sized", backend.ImageSpec{From: &backend.From{Ref: "x"}, MemoryGiB: 4, Net: backend.NetNone}.Features(),
			f(backend.FeatureBuildImage, backend.FeatureFrom, backend.FeatureMemory, backend.FeatureBuildConfinement),
		},
		{
			"the nix layer", backend.ImageSpec{Image: "node+nix", Net: backend.NetInternet}.Features(),
			f(backend.FeatureBuildImage, backend.FeatureNixLayer, backend.FeatureBuildConfinement),
		},
		{"a VM of docker and nix", backend.ImageFeatures("docker+nix+foo"), f(backend.FeatureDockerLayer, backend.FeatureNixLayer)},
		{"a VM of neither", backend.ImageFeatures("node+nixos"), nil},
		{"a --clone VM forwarding nothing", backend.VMSpec{Name: "v"}.Features(), f(backend.FeatureClone)},
		{"a VM with everything", backend.VMSpec{
			Name: "v", Project: "/p", AgentDirs: []string{"/h/.claude"}, Tools: "/t", ToolsAt: "/opt/devvm",
			AgentForwarding: true, AgentToggle: true, Size: backend.Size{CPUs: 2, MemoryGiB: 4, DiskGiB: 60},
		}.Features(), f(backend.FeatureClone, backend.FeatureProjectMount, backend.FeatureToolsShare, backend.FeatureAgentToggle,
			backend.FeatureAgentFolders, backend.FeatureAgentForwarding, backend.FeatureCPUs, backend.FeatureMemory, backend.FeatureDisk)},
		{"a start wanting nothing", backend.Want{}.Features(), nil},
		{
			"a start wanting agents and memory", backend.Want{AgentDirs: []string{"/h/.claude"}, AgentForwarding: true, Size: backend.Size{MemoryGiB: 8}}.Features(),
			f(backend.FeatureAgentFolders, backend.FeatureAgentForwarding, backend.FeatureMemory),
		},
		{"a start choosing its folder's cache", backend.Want{Project: "/p", ProjectCache: backend.CacheNone}.Features(), f(backend.FeatureMountCache)},
		{"a --clone VM's cache mode, with no folder", backend.Want{ProjectCache: backend.CacheNone}.Features(), nil},
		{"every port", backend.Ports{All: true}.Features(), f(backend.FeaturePorts, backend.FeaturePortsAll)},
		{"some ports", backend.Ports{Ranges: []backend.PortRange{{5173, 5173}}}.Features(), f(backend.FeaturePorts)},
		{"no ports", backend.Ports{}.Features(), nil},
		{"direct", backend.Confinement{Net: backend.NetDirect}.Features(), nil},
		{"through the proxy", backend.Confinement{Net: backend.NetProxy, ProxyPort: 3128}.Features(), f(backend.FeatureEgressProxy)},
	} {
		if !slices.Equal(tc.got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}

// A backend that only emulates network policy, unreviewed, can't start a
// VM with it, but can start one without.
func TestUnreviewedEgressRefused(t *testing.T) {
	t.Parallel()
	b := backendtest.New("fake", backend.Caps{backend.FeatureEgressProxy: backend.Emulated(false)})
	c := backend.Confinement{Net: backend.NetProxy}
	want := "backend_unreviewed backend=fake feature=egress-proxy what=devvm-x"
	if got := msg.NamesOf(backend.Review{}.Require(b, "devvm-x", c.Features()...)); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if err := (backend.Review{}).Require(b, "devvm-x", backend.Confinement{}.Features()...); err != nil {
		t.Errorf("direct: %v", err)
	}
}

// DEVVM_UNREVIEWED lets through only the unreviewed emulations it names,
// on their own backend, warning each time; never a feature a backend lacks.
func TestUnreviewedLetThrough(t *testing.T) {
	t.Parallel()
	b := backendtest.New("fake", backend.Caps{backend.FeatureEgressProxy: backend.Emulated(false)})
	var warned msg.Recorder
	r := backend.Review{
		Unreviewed: backend.ParseUnreviewed("lima:egress-proxy, fake:ssh"),
		Reporter:   &msg.Fake{OnWarn: warned.Add},
	}
	if got := msg.NamesOf(r.Require(b, "devvm-x", backend.FeatureEgressProxy)); got != "backend_unreviewed backend=fake feature=egress-proxy what=devvm-x" {
		t.Errorf("another backend's: %s", got)
	}
	if got := msg.NamesOf(r.Require(b, "devvm-x", backend.FeatureSSH)); got != "backend_lacks backend=fake feature=ssh what=devvm-x" {
		t.Errorf("a feature it lacks: %s", got)
	}
	if r.Allowed(b, backend.FeatureEgressProxy) || r.Allowed(b, backend.FeatureSSH) {
		t.Error("Allowed: not named, or lacking")
	}
	r.Unreviewed = backend.ParseUnreviewed("fake:egress-proxy")
	if err := r.Require(b, "devvm-x", backend.FeatureEgressProxy); err != nil ||
		!slices.Equal(warned.Names(), []string{"backend_unreviewed_used backend=fake feature=egress-proxy what=devvm-x"}) {
		t.Errorf("named: %v, warned %q", err, warned.Names())
	}
	// Allowed says the same, without a warning.
	if !r.Allowed(b, backend.FeatureEgressProxy) || len(warned.Names()) != 1 {
		t.Errorf("Allowed: warned %q", warned.Names())
	}
}
