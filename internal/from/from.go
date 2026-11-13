// Package from looks up the container images base can start from
// (--from): references as Docker reads them, their digests from their
// registries or from Docker, and the digest devvm last recorded for each.
//
// base is keyed by an image's digest, not its tag, so a tag that moves (a
// new node:24) makes base stale. Digests are looked up when building, and
// recorded per reference, so listing images and opening VMs never look
// anything up. A registry image's digest is a reference
// (node@sha256:…); an image only Docker has (built and never pushed, or
// private) is keyed by Docker's ID (sha256:…), and only Docker can hand
// it over.
package from

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os/exec"
	"regexp"
	"strings"

	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/proxy"
	"github.com/devvmhq/devvm/internal/settings"
	"github.com/devvmhq/devvm/internal/state"
)

// Env is the environment variable that sets --from's default.
const Env = "DEVVM_FROM"

var refPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@+-]*$`)

// Valid reports whether s can be an image reference. References go into
// URLs, docker's arguments, the conversion's arguments and state files.
func Valid(s string) bool { return refPattern.MatchString(s) }

// Check refuses a --from that isn't an image reference; "" is none.
func Check(s string) error {
	if s != "" && !Valid(s) {
		return msg.BadFrom(s)
	}
	return nil
}

// Pinned reports whether a reference names its digest (…@sha256:…): it
// needs no lookup.
func Pinned(ref string) bool { return strings.Contains(ref, "@sha256:") }

// Name is a reference without its tag or digest: ghcr.io/o/r for
// ghcr.io/o/r:1.
func Name(ref string) string {
	if i := strings.LastIndex(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	last := ref[strings.LastIndex(ref, "/")+1:]
	if i := strings.LastIndex(last, ":"); i >= 0 {
		ref = ref[:len(ref)-len(last)+i]
	}
	return ref
}

// Recorded returns the digest base's key has for ref: ref itself when
// it's pinned, else the one last recorded in the cache, "" when there is
// none. With the cache gone, it's the digest the newest base built from
// ref records, so a deleted cache changes no key: the next lookup records
// it again.
func Recorded(d paths.Dirs, ref string) (string, error) {
	if ref == "" || Pinned(ref) {
		return ref, nil
	}
	if digest, err := state.FromDigest(d, ref); err != nil || digest != "" {
		return digest, err
	}
	imgs, err := state.Images(d)
	if err != nil {
		return "", err
	}
	var newest state.Image
	for _, img := range imgs {
		if img.Name == image.Base && img.From == ref && img.Digest != "" && !img.Built.Before(newest.Built) {
			newest = img
		}
	}
	return newest.Digest, nil
}

// Lookup looks up digests and records them. NewLookup sets every field;
// tests change them.
type Lookup struct {
	Dirs     paths.Dirs
	Registry *Registry
	// PublicRegistry is Public's: only the internet's addresses
	// (NewPublicRegistry).
	PublicRegistry *Registry
	// DockerID returns Docker's ID for an image, when Docker runs and has
	// it.
	DockerID func(ctx context.Context, ref string) (string, bool)
	// Reporter is where it warns.
	msg.Reporter
}

// NewLookup returns a Lookup on the internet, through the company proxy
// cp (nil for the environment's), and this machine's Docker, warning
// through r.
func NewLookup(d paths.Dirs, r msg.Reporter, cp *settings.CompanyProxy) *Lookup {
	return &Lookup{Dirs: d, Registry: NewRegistry(cp.Route), PublicRegistry: NewPublicRegistry(cp.Route), DockerID: DockerID, Reporter: r}
}

// Refresh looks up ref's digest and records it: its registry's, else
// Docker's ID for an image the registry doesn't have and Docker does.
// Registry digests can't tell: Docker's containerd store gives its own
// builds one too. When neither has it, the digest recorded before is kept,
// with a warning, before Docker's ID, so an offline build doesn't rebuild
// base. A pinned reference needs nothing.
func (l *Lookup) Refresh(ctx context.Context, ref string) error {
	if ref == "" || Pinned(ref) {
		return nil
	}
	host := ParseRef(ref).Host
	d, err := l.Registry.Digest(ctx, ref)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err == nil {
		return state.SetFromDigest(l.Dirs, ref, Name(ref)+"@"+d)
	}
	notFound := !errors.Is(err, ErrUnreachable)
	if notFound {
		if id, ok := l.DockerID(ctx, ref); ok {
			return state.SetFromDigest(l.Dirs, ref, id)
		}
	}
	cached, err := Recorded(l.Dirs, ref)
	if err != nil {
		return err
	}
	switch {
	case cached != "" && notFound:
		l.Warn(msg.FromCachedNotFound(ref, host))
		return nil
	case cached != "":
		l.Warn(msg.FromCachedUnreachable(ref, host))
		return nil
	case notFound:
		return msg.FromNotFound(ref, host)
	}
	if id, ok := l.DockerID(ctx, ref); ok {
		return state.SetFromDigest(l.Dirs, ref, id)
	}
	return msg.FromUnreachable(ref, host)
}

// Public returns ref pinned to the digest its registry gives anyone
// (name@sha256:…), for an image a project's devcontainer.json names:
// never one only Docker has, one only a registry login reaches, or one on
// this machine or its network (publicHost, PublicRegistry), which the
// project would get into its VM. A pinned reference is checked the
// same way. Offline, a digest the registry gave before is kept, with a
// warning. It fails with ErrNotFound (wrapped) for an image that isn't
// public, else a message.
func (l *Lookup) Public(ctx context.Context, ref string) (string, error) {
	host := ParseRef(ref).Host
	if !publicHost(host) {
		return "", fmt.Errorf("%w: %s isn't on the internet", ErrNotFound, host)
	}
	d, err := l.PublicRegistry.Digest(ctx, ref)
	switch {
	case ctx.Err() != nil:
		return "", ctx.Err()
	case err == nil && Pinned(ref) && !strings.HasSuffix(ref, "@"+d):
		return "", fmt.Errorf("%w: %s answered %s", ErrNotFound, host, d)
	case err == nil:
		pinned := Name(ref) + "@" + d
		if Pinned(ref) {
			return ref, nil
		}
		return pinned, state.SetFromDigest(l.Dirs, ref, pinned)
	case errors.Is(err, ErrUnreachable) && !Pinned(ref):
		cached, rerr := Recorded(l.Dirs, ref)
		if rerr != nil {
			return "", rerr
		}
		if strings.HasPrefix(cached, Name(ref)+"@sha256:") {
			l.Warn(msg.FromCachedUnreachable(ref, host))
			return cached, nil
		}
		return "", msg.FromUnreachable(ref, host)
	case errors.Is(err, ErrUnreachable):
		return "", msg.FromUnreachable(ref, host)
	}
	return "", err
}

// publicHost reports whether a registry's host (with any port) may be the
// internet's by its name: not localhost, nor an address that isn't the
// internet's. What a name resolves to is checked when it's reached
// (NewPublicRegistry).
func publicHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return false
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return proxy.Public(ip)
	}
	return true
}

// DockerID returns Docker's ID for an image (sha256:…), when Docker is
// installed and running and has it. It only asks.
func DockerID(ctx context.Context, ref string) (string, bool) {
	if !Valid(ref) {
		return "", false
	}
	if _, err := exec.LookPath("docker"); err != nil {
		return "", false
	}
	out, err := exec.CommandContext(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", ref).Output() //nolint:gosec // a checked reference
	id := strings.TrimSpace(string(out))
	return id, err == nil && IsDigest(id)
}

// DockerRunning reports whether Docker is installed and running.
func DockerRunning(ctx context.Context) bool {
	if _, err := exec.LookPath("docker"); err != nil {
		return false
	}
	return exec.CommandContext(ctx, "docker", "info").Run() == nil
}
