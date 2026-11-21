package vm

import (
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/proxy"
)

// legacyURLHostPort and legacyPrivateHost are urlHostPort and privateHost as they were before
// proxy.NormHost read their hosts, frozen for FuzzPromisorHostSite.
func legacyURLHostPort(u string) (host string, port int, local bool) {
	scheme, _, hasScheme := strings.Cut(u, "://")
	if !hasScheme {
		if h, _, ok := scpHost(u); ok {
			return strings.ToLower(h), 22, false
		}
		return "", 0, true
	}
	if strings.EqualFold(scheme, "file") {
		return "", 0, true
	}
	p, err := url.Parse(u)
	if err != nil || p.Hostname() == "" {
		return "", 0, false
	}
	port = map[string]int{"http": 80, "https": 443, "ssh": 22, "git+ssh": 22, "ssh+git": 22}[strings.ToLower(scheme)]
	if p.Port() != "" {
		n, err := strconv.Atoi(p.Port())
		if err != nil {
			return "", 0, false
		}
		port = n
	}
	if port == 0 {
		return "", 0, false
	}
	return strings.ToLower(p.Hostname()), port, false
}

func legacyPrivateHost(host string) bool {
	if a, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		return !proxy.Public(a)
	}
	return host == "localhost" || strings.HasSuffix(host, ".localhost")
}

// FuzzPromisorHostSite: a partial clone's promisor remote's host, which its warnings judge by the
// VM's policies, reads as it did, but as the proxy reads it (proxy.NormHost), which the policies
// did already. Divergences: its one trailing dot is dropped, so a warning names example.com, not
// example.com., and localhost. or 127.0.0.1. counts as private, which the proxy refuses as it
// refuses localhost; and a host with anything but printable ASCII isn't folded in Unicode (the
// Kelvin sign into k), as the proxy refuses such a host. A host private before is private still,
// but one with a stray bracket ([::, from the scp-like [[::]:path), which strings.Trim read as an
// address and is none: git reaches no such host, and the proxy refuses it. An IPv6 address with a
// zone is private, as proxy.Public calls none the internet's, where a zoned public address wasn't;
// and one whose zone is "." keeps its dot (::1%.), so it's the address it is, private as it was.
func FuzzPromisorHostSite(f *testing.F) {
	for _, s := range []string{
		"https://github.com/x/y.git", "https://GitHub.com./x", "git@Host.example.:x", "git@localhost.:x",
		"https://127.0.0.1./x", "ssh://[::1]/x", "[::1]:x", "https://Kubernetes.io/x", "https://a..b/x",
		"https://x.localhost../x", "file:///x", "/x", "https://h:bad/x", "[[::]:x",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, u string) {
		host, port, local := urlHostPort(u)
		lhost, lport, llocal := legacyURLHostPort(u)
		if port != lport || local != llocal {
			t.Fatalf("urlHostPort(%q) = %q, %d, %v; was %q, %d, %v", u, host, port, local, lhost, lport, llocal)
		}
		ascii := !strings.ContainsFunc(host, func(r rune) bool { return r <= ' ' || r > '~' })
		if ascii && host != proxy.NormHost(lhost).String() {
			t.Fatalf("urlHostPort(%q) = %q; was %q", u, host, lhost)
		}
		if legacyPrivateHost(lhost) && !privateHost(host) && !strings.ContainsAny(lhost, "[]") {
			t.Fatalf("%q: %q was private, %q isn't", u, lhost, host)
		}
		a, isAddr := proxy.NormHost(host).Addr()
		if privateHost(host) && !legacyPrivateHost(lhost) && !strings.HasSuffix(lhost, ".") && (!isAddr || a.Zone() == "") {
			t.Fatalf("%q: %q is private, %q wasn't", u, host, lhost)
		}
	})
}
