// Package netpolicy runs VMs' network tiers: the choice for a new VM,
// the VM's allowlist files, its proxy on the host (devvm _proxy), the
// confinement its backend starts it with, and the self-check after each
// start that it can't reach around the proxy.
package netpolicy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"path/filepath"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/vmname"
)

// Section is the name of the network policy section in a VM's state.
const Section = "net"

// Net is a VM's network policy section: its tier, as asked, and what
// devvm made for it.
type Net struct {
	Tier Tier `json:"tier"`
	// Learning is whether the VM learns the hosts it reaches, on the
	// internet tier, and since when; the zero value while it doesn't.
	Learning Learning `json:"learning,omitzero"`
	// Transparent is the VM's own transparent mode, TransparentOn or
	// TransparentOff (devvm network transparent); "" follows config.toml's
	// (TransparentOf).
	Transparent string  `json:"transparent,omitempty"`
	Made        NetMade `json:"made,omitzero"`
}

// Learning is a learning VM's: when it started learning the hosts it
// reaches, which devvm network lock reads from its network log from then
// on. A VM that learns is on the internet tier.
type Learning struct {
	Since time.Time `json:"since,omitzero"`
}

// On reports whether the VM learns.
func (l Learning) On() bool { return !l.Since.IsZero() }

// NetMade is what devvm made for a VM's network tier.
type NetMade struct {
	Check     string `json:"check,omitempty"`      // the last self-check's result: CheckOK or CheckUnenforced
	ProxyKey  string `json:"proxy_key,omitempty"`  // the VM's proxy password (Key)
	ProxySalt string `json:"proxy_salt,omitempty"` // what ends the VM's placeholders (Salt)
}

// Of returns a VM's network policy section. A section without a tier
// has the default one (TierDefault); one with a tier devvm doesn't know
// can't be read (Tier's UnmarshalJSON).
func Of(st *state.VM) (Net, error) {
	n, err := state.Section[Net](st, Section)
	if err == nil && n.Tier == "" {
		n.Tier = TierDefault
	}
	return n, err
}

// Set sets a VM's network policy section, in st alone: inside st.Update.
func Set(st *state.VM, n Net) error { return state.SetSection(st, Section, n) }

// SetTier sets a VM's tier, in st alone: inside st.Update. A VM that
// learned stops learning, unless learn starts it again since now, on the
// internet tier (t must be TierInternet then).
func SetTier(st *state.VM, t Tier, learn Learning) error {
	n, err := Of(st)
	if err != nil {
		return err
	}
	n.Tier, n.Learning = t, learn
	return Set(st, n)
}

// LearningOf returns a VM's learning: the zero value for a VM that doesn't
// learn, or whose section can't be read.
func LearningOf(st *state.VM) Learning {
	n, err := Of(st)
	if err != nil || n.Tier != TierInternet {
		return Learning{}
	}
	return n.Learning
}

// TierOf returns a VM's tier. A section that can't be read (the folder's
// VM's is checked as it's found) reads as the strictest, never as open.
func TierOf(st *state.VM) Tier {
	n, err := Of(st)
	if err != nil {
		return TierNone
	}
	return n.Tier
}

// Key returns the VM's own random key, made the first time it's needed
// (backend.NewProxyPassword). It's the password for the VM's proxy, so
// other VMs and programs on the host that reach its port can't use it.
func Key(ctx context.Context, st *state.VM) (string, error) {
	return made(ctx, st, func(m *NetMade) *string { return &m.ProxyKey }, backend.NewProxyPassword)
}

// Salt returns what ends the VM's placeholders: random, like the key, and
// not the key itself, so a placeholder the VM sends elsewhere doesn't give
// its proxy's password away.
func Salt(ctx context.Context, st *state.VM) (string, error) {
	return made(ctx, st, func(m *NetMade) *string { return &m.ProxySalt }, func() (string, error) {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		return hex.EncodeToString(b), nil
	})
}

// made returns the value of what devvm made for st that field names,
// made by mk and recorded the first time, unless another devvm recorded
// one meanwhile.
func made(ctx context.Context, st *state.VM, field func(*NetMade) *string, mk func() (string, error)) (string, error) {
	if n, err := Of(st); err != nil || *field(&n.Made) != "" {
		return *field(&n.Made), err
	}
	v, err := mk()
	if err != nil {
		return "", err
	}
	err = st.Update(ctx, func(st *state.VM) error {
		n, err := Of(st)
		if err != nil || *field(&n.Made) != "" {
			v = *field(&n.Made)
			return err
		}
		*field(&n.Made) = v
		return Set(st, n)
	})
	return v, err
}

// PolicySet is the policies a VM's proxy reads, each list in its order.
// Builtin is devvm's own, as paths in its files (assets.FS), which the
// proxy reads from the binary. Files are the user's files and devvm's own
// on disk whose every rule applies, and Denies the user's files whose
// denies alone apply. A missing file has no rules.
type PolicySet struct{ Builtin, Files, Denies []string }

// InternetPolicy is the internet tier's built-in policy: every host, on
// any port.
const InternetPolicy = "network/internet.policy"

// BasePolicy is devvm's built-in policy for every VM on the allowlist
// tier.
const BasePolicy = "layers/base.policy"

// Policies returns the policies of the VM vm on tier t, of image img with
// these shared tools. The user's files are each of the image's own
// layers' policy file, next to its script, the user's own for every VM
// and the VM's own; devvm's on disk is the hosts its secrets imply.
//
// On allowlist, devvm's built-in policies are base's, then each shared
// tool's in shared-tools/, then each built-in layer's in layers/, and
// every rule of the user's files applies. On internet, the one built-in
// policy is InternetPolicy; on none, the VM's agents' policies alone. On
// either, only the denies of the user's files apply. Open has none.
func Policies(d paths.Dirs, vm vmname.VM, t Tier, img image.Name, tools []string) PolicySet {
	var builtin, files []string
	for _, p := range img.Layers() {
		switch {
		case p == "" || p == "base" || image.IsTool(p):
		case image.IsBuiltin(p):
			builtin = append(builtin, "layers/"+p+".policy")
		case image.LayerName(p):
			files = append(files, filepath.Join(d.Layers(), p+".policy"))
		}
		// A part that's no layer's name (../x) names no file: its path
		// would leave the user's layers, so it adds no rules.
	}
	files = append(files, d.Policy(), d.VM(vm).Policy())
	secrets := d.VM(vm).SecretsPolicy()
	switch t {
	case TierOpen:
		return PolicySet{}
	case TierInternet:
		return PolicySet{Builtin: []string{InternetPolicy}, Files: []string{secrets}, Denies: files}
	case TierNone:
		return PolicySet{Builtin: toolPolicies(image.AgentsOf(tools)), Files: []string{secrets}, Denies: files}
	case TierAllowlist:
	}
	builtin = append(append([]string{BasePolicy}, toolPolicies(tools)...), builtin...)
	return PolicySet{Builtin: builtin, Files: append(files, secrets)}
}

// toolPolicies are the built-in policies of these shared tools, in order:
// only a shared tool's name names a file of devvm's.
func toolPolicies(tools []string) []string {
	var out []string
	for _, t := range tools {
		if image.IsTool(t) {
			out = append(out, "shared-tools/"+t+".policy")
		}
	}
	return out
}

// Env is the guest environment a VM gets besides its
// proxy's address: no proxy for the VM itself, and Node told to use the
// proxy variables. Tools find the proxy's CA as every VM finds the user's
// (trust.Env).
var Env = map[string]string{
	"NO_PROXY":           NoProxy,
	"no_proxy":           NoProxy,
	"NODE_USE_ENV_PROXY": "1",
}

// NoProxy is what a VM reaches without its proxy: the
// VM itself. Its links aren't in it: an HTTP client that sends one to the
// proxy reaches it there (proxy.Config's Links), so links change no VM's
// environment.
const NoProxy = "localhost,127.0.0.1,::1"
