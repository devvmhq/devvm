package from

import (
	"net"
	"net/netip"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/proxy"
)

// legacyPublicHost is publicHost as it was before proxy.NormHost read its host, frozen for
// FuzzPublicHostSite.
func legacyPublicHost(host string) bool {
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

// FuzzPublicHostSite: --from's registry host reads as it did, and is never more public than it
// was. Divergence: a name the proxy doesn't take (proxy.HostOK) isn't public, where it was unless
// strings.ToLower folded it into localhost's: one with anything but printable ASCII, a bracket a
// trailing dot hid from strings.Trim ([::1]., read as the name ::1]), an empty label or a shorthand
// address (10.1). No registry's name is one of those.
func FuzzPublicHostSite(f *testing.F) {
	for _, s := range []string{
		"ghcr.io", "GHCR.io.", "localhost", "LOCALHOST.:5000", "x.localhost", "127.0.0.1:5000", "[::1]:5000",
		"[::1].", "10.0.0.1.", "Kubernetes.io", "localhost..", "",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, host string) {
		got, was := publicHost(host), legacyPublicHost(host)
		h := host
		if hh, _, err := net.SplitHostPort(host); err == nil {
			h = hh
		}
		taken := proxy.NormHost(strings.Trim(h, "[]")).OK()
		if got != was && (got || taken) {
			t.Fatalf("publicHost(%q) = %v, was %v", host, got, was)
		}
	})
}
