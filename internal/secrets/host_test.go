package secrets

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/proxy"
)

// legacyCheckHost is CheckHost as it was before proxy.NormHost read its host, frozen for
// FuzzCheckHostSite: its result, and whether it took the host.
func legacyCheckHost(given string) (string, bool) {
	h := strings.ToLower(given)
	if rest, port, ok := strings.Cut(h, ":"); ok {
		if port != "443" {
			return "", false
		}
		h = rest
	}
	h = strings.TrimSuffix(h, ".")
	_, addrErr := netip.ParseAddr(strings.Trim(h, "[]"))
	if addrErr == nil || strings.Contains(h, "*") || !strings.Contains(h, ".") || !proxy.HostOK(h) || !proxy.ValidRule(h) || internalName(h) {
		return "", false
	}
	return h, true
}

// FuzzCheckHostSite: a secret's --host reads as it did. Divergence: a host with anything but
// printable ASCII is refused, where strings.ToLower folded it into ASCII first (the Kelvin sign's
// kubernetes.io was taken as kubernetes.io), as the proxy refuses such a host before it folds it.
// Every host CheckHost takes, it took, as the same host.
func FuzzCheckHostSite(f *testing.F) {
	for _, s := range []string{
		"api.example.com", "API.Example.com.", "api.example.com:443", "api.example.com.:443", "api.example.com..",
		"api.example.com:8443", "Kubernetes.io", "[::1]:443", "*.example.com", "example", "x.local", "",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, given string) {
		got, err := CheckHost(given)
		was, ok := legacyCheckHost(given)
		switch {
		case err == nil && (!ok || got != was):
			t.Fatalf("CheckHost(%q) = %q; was %q, %v", given, got, was, ok)
		case err != nil && ok && !strings.ContainsFunc(given, func(r rune) bool { return r <= ' ' || r > '~' }):
			t.Fatalf("CheckHost(%q) refused it; was %q", given, was)
		}
	})
}
