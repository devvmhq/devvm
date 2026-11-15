package backend

import (
	"fmt"
	"strings"

	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/vmname"
)

// Feature is something a backend may have. Its name is the one the backends
// page lists.
type Feature string

// Features, in the backends page's order.
const (
	FeatureBuildImage       Feature = "build-image"       // build images from the layer and feature scripts
	FeatureBuildConfinement Feature = "build-confinement" // builds reach the internet but not host loopback (NetInternet), or nothing (NetNone)
	FeatureFrom             Feature = "from"              // base from a container image (--from)
	FeatureClone            Feature = "clone"             // a VM from an image
	FeatureProjectMount     Feature = "project-mount"     // the project folder, read-write
	FeatureReadOnlyPaths    Feature = "read-only-paths"   // paths in the project folder the guest can't change while it runs (Confinement.Pins)
	FeatureMountCache       Feature = "mount-cache"       // choosing how the guest caches the project folder's mount (Want.ProjectCache)
	FeatureAgentFolders     Feature = "agent-folders"     // agents' host folders, read-only
	FeatureToolsShare       Feature = "tools-share"       // the shared tools folder, read-only
	FeatureEgressProxy      Feature = "egress-proxy"      // egress only through devvm's proxy (NetProxy)
	FeatureSSH              Feature = "ssh"               // ssh for editors
	FeaturePorts            Feature = "ports"             // forwarding listed guest ports to host loopback
	FeaturePortsAll         Feature = "ports-all"         // forwarding every guest port, whatever address it listens on
	FeatureAgentForwarding  Feature = "agent-forwarding"  // ssh-agent forwarding, chosen when a VM is made
	FeatureAgentToggle      Feature = "agent-toggle"      // turning a VM's ssh-agent forwarding on or off later
	FeatureCPUs             Feature = "cpus"              // setting a VM's CPUs
	FeatureMemory           Feature = "memory"            // setting a VM's memory
	FeatureDisk             Feature = "disk"              // setting a VM's disk size
	FeatureDockerLayer      Feature = "docker-layer"      // the docker layer: Docker's engine running in the VM
	FeatureNixLayer         Feature = "nix-layer"         // the nix layer: Nix's daemon and store in the VM
	FeatureLink             Feature = "link"              // links to other VMs' ports, over ssh to both VMs
)

// Features is every feature, in the backends page's order.
var Features = []Feature{
	FeatureBuildImage, FeatureBuildConfinement, FeatureFrom, FeatureClone,
	FeatureProjectMount, FeatureReadOnlyPaths, FeatureMountCache, FeatureAgentFolders, FeatureToolsShare,
	FeatureEgressProxy, FeatureSSH, FeaturePorts, FeaturePortsAll,
	FeatureAgentForwarding, FeatureAgentToggle,
	FeatureCPUs, FeatureMemory, FeatureDisk, FeatureDockerLayer, FeatureNixLayer, FeatureLink,
}

// Support is how a backend has a feature: natively, emulated (usable only
// once the emulation's review has passed), or not at all.
type Support struct {
	kind     kind
	reviewed bool
}

type kind int

const (
	none kind = iota
	native
	emulated
)

// Native is a feature the backend has itself.
var Native = Support{kind: native}

// None is a feature the backend lacks.
var None = Support{}

// Emulated is a feature devvm builds on the backend from other parts.
// reviewed is whether its review passed: a design note, a threat check and
// e2e checks proving the guarantee. Until then it's refused.
func Emulated(reviewed bool) Support { return Support{kind: emulated, reviewed: reviewed} }

// IsNative reports whether the feature is the backend's own.
func (s Support) IsNative() bool { return s.kind == native }

// IsEmulated reports whether the feature is emulated, reviewed or not.
func (s Support) IsEmulated() bool { return s.kind == emulated }

// Reviewed reports whether an emulated feature's review passed.
func (s Support) Reviewed() bool { return s.reviewed }

// Usable reports whether devvm uses the feature: native, or emulated and
// reviewed.
func (s Support) Usable() bool { return s.kind == native || s.kind == emulated && s.reviewed }

// String is the support as devvm doctor lists it: native, emulated,
// unreviewed (emulated, refused until its review passes) or none.
func (s Support) String() string {
	switch {
	case s.IsNative():
		return "native"
	case s.Usable():
		return "emulated"
	case s.IsEmulated():
		return "unreviewed"
	}
	return "none"
}

// BuiltOn returns the feature f is built on, if any: a backend has f no
// better than its base, so it's emulated wherever its base is, and none
// wherever its base is none.
func (f Feature) BuiltOn() (Feature, bool) {
	switch f {
	case FeaturePortsAll: // every port, through the listed ports' forwarder
		return FeaturePorts, true
	case FeatureAgentToggle: // turning on or off what was chosen when the VM was made
		return FeatureAgentForwarding, true
	case FeatureLink: // a link session over ssh to both VMs
		return FeatureSSH, true
	case FeatureReadOnlyPaths: // pins in the project folder's mount
		return FeatureProjectMount, true
	case FeatureMountCache: // the project folder mount's own option
		return FeatureProjectMount, true
	case FeatureBuildImage, FeatureBuildConfinement, FeatureFrom, FeatureClone,
		FeatureProjectMount, FeatureAgentFolders, FeatureToolsShare,
		FeatureEgressProxy, FeatureSSH, FeaturePorts, FeatureAgentForwarding,
		FeatureCPUs, FeatureMemory, FeatureDisk, FeatureDockerLayer, FeatureNixLayer:
	}
	return "", false
}

// Caps is how a backend has each feature. Every backend lists every
// feature, None included (Check); a lookup of one unlisted is None.
type Caps map[Feature]Support

// Check returns an error naming the first feature c doesn't list, or
// lists better than the feature it's built on (Feature.BuiltOn): what the registry
// refuses to register.
func (c Caps) Check() error {
	for _, f := range Features {
		s, ok := c[f]
		if !ok {
			return fmt.Errorf("feature %s is not listed", f)
		}
		if base, on := f.BuiltOn(); on && s.rank() > c[base].rank() {
			return fmt.Errorf("feature %s is %s, better than %s, which it is built on (%s)", f, s, base, c[base])
		}
	}
	if len(c) != len(Features) {
		return fmt.Errorf("%d features listed, %d known", len(c), len(Features))
	}
	return nil
}

// rank orders support: none, unreviewed, emulated, native.
func (s Support) rank() int {
	switch {
	case s.IsNative():
		return 3
	case s.Usable():
		return 2
	case s.IsEmulated():
		return 1
	}
	return 0
}

// Review checks backends' features against their review. Its zero value
// refuses every unreviewed emulation.
type Review struct {
	// Unreviewed is the unreviewed emulations Require lets through anyway,
	// as backend:feature (lima:ports): for the end-to-end checks
	// that an emulation's review needs, which set DEVVM_UNREVIEWED.
	Unreviewed map[string]bool
	// Reporter warns each time one is let through; nil tells no one.
	msg.Reporter
}

// ParseUnreviewed reads DEVVM_UNREVIEWED's value: backend:feature pairs,
// comma-separated.
func ParseUnreviewed(v string) map[string]bool {
	out := map[string]bool{}
	for p := range strings.SplitSeq(v, ",") {
		if p = strings.TrimSpace(p); strings.Contains(p, ":") {
			out[p] = true
		}
	}
	return out
}

// Require returns nil when b has every feature usable or let through, else
// the refusal for the first one it lacks or only emulates unreviewed,
// naming vm.
func (r Review) Require(b Backend, vm vmname.VM, features ...Feature) error {
	return r.RequireFor(b, vm.String(), features...)
}

// RequireFor is Require for what isn't a VM, naming what: an image being
// built (image.Name), or the shared tools.
func (r Review) RequireFor(b Backend, what string, features ...Feature) error {
	var warn func(msg.M)
	if r.Reporter != nil {
		warn = r.Warn
	}
	return r.refusal(b, what, warn, features)
}

// refusal is Require's, telling warn of each unreviewed emulation let
// through, or no one when warn is nil (the guard: the caller's Require
// told), naming what needs them.
func (r Review) refusal(b Backend, what string, warn func(msg.M), features []Feature) error {
	caps := b.Caps()
	for _, f := range features {
		switch s := caps[f]; {
		case s.Usable():
		case s.IsEmulated() && r.Unreviewed[b.Name()+":"+string(f)]:
			if warn != nil {
				warn(msg.BackendUnreviewedUsed(b.Name(), string(f), what))
			}
		case s.IsEmulated():
			return msg.BackendUnreviewed(b.Name(), string(f), what)
		default:
			return msg.BackendLacks(b.Name(), string(f), what)
		}
	}
	return nil
}

// Allowed reports whether Require lets f through on b, without a warning:
// for what uses a feature a VM already got (Require told then).
func (r Review) Allowed(b Backend, f Feature) bool {
	s := b.Caps()[f]
	return s.Usable() || s.IsEmulated() && r.Unreviewed[b.Name()+":"+string(f)]
}

// Features returns what building the image needs.
func (s ImageSpec) Features() []Feature {
	fs := []Feature{FeatureBuildImage}
	if s.From != nil {
		fs = append(fs, FeatureFrom)
	}
	if s.MemoryGiB > 0 {
		fs = append(fs, FeatureMemory)
	}
	// A backend without nix-layer can't build the layer either: Nix
	// couldn't sandbox a build in its VMs.
	if image.Name(s.Image).Has(image.NixLayer) {
		fs = append(fs, FeatureNixLayer)
	}
	return append(fs, s.Net.Features()...)
}

// ImageFeatures returns what a VM of the image needs for its layers:
// docker-layer when it has the docker layer, nix-layer when it has the
// nix layer.
func ImageFeatures(img image.Name) []Feature {
	var fs []Feature
	if img.Has(image.DockerLayer) {
		fs = append(fs, FeatureDockerLayer)
	}
	if img.Has(image.NixLayer) {
		fs = append(fs, FeatureNixLayer)
	}
	return fs
}

// Features returns what the tools build needs.
func (s ToolsSpec) Features() []Feature {
	return append([]Feature{FeatureBuildImage}, s.Net.Features()...)
}

// Features returns what creating the VM needs: its Want's, and agent-toggle
// when it's made ready to change its forwarding.
func (s VMSpec) Features() []Feature {
	fs := []Feature{FeatureClone}
	if s.Project != "" {
		fs = append(fs, FeatureProjectMount)
	}
	if s.Tools != "" {
		fs = append(fs, FeatureToolsShare)
	}
	if s.AgentToggle {
		fs = append(fs, FeatureAgentToggle)
	}
	return append(fs, s.Want.Features()...)
}

// Features returns what a VM needs to be as w wants it: agent-toggle, to
// change its forwarding, is the caller's to ask for, as only it knows
// what the VM had.
func (w Want) Features() []Feature {
	var fs []Feature
	if len(w.AgentDirs) > 0 {
		fs = append(fs, FeatureAgentFolders)
	}
	if w.AgentForwarding {
		fs = append(fs, FeatureAgentForwarding)
	}
	if w.ProjectCache != "" && w.Project != "" {
		fs = append(fs, FeatureMountCache)
	}
	return append(fs, w.Size.Features()...)
}

// Features returns what setting the size needs.
func (s Size) Features() []Feature {
	var fs []Feature
	if s.CPUs > 0 {
		fs = append(fs, FeatureCPUs)
	}
	if s.MemoryGiB > 0 {
		fs = append(fs, FeatureMemory)
	}
	if s.DiskGiB > 0 {
		fs = append(fs, FeatureDisk)
	}
	return fs
}

// Features returns what forwarding the ports needs, with devvm's
// forwarder (vmssh.Forwards, over the VM's ssh): nothing for none.
func (p Ports) Features() []Feature {
	switch {
	case p.All:
		return []Feature{FeaturePorts, FeaturePortsAll}
	case len(p.Ranges) > 0:
		return []Feature{FeaturePorts}
	}
	return nil
}

// Features returns what starting with the network needs.
func (n Net) Features() []Feature {
	switch n {
	case NetProxy:
		return []Feature{FeatureEgressProxy}
	case NetInternet, NetNone:
		return []Feature{FeatureBuildConfinement}
	case NetDirect:
	}
	return nil
}

// Features returns what starting with the confinement needs.
func (c Confinement) Features() []Feature {
	fs := c.Net.Features()
	if !c.Pins.Empty() {
		fs = append(fs, FeatureReadOnlyPaths)
	}
	return fs
}
