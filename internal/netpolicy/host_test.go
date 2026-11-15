package netpolicy

import (
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/proxy"
)

// legacyRiskOf and legacyTickable are RiskOf and Tickable as they were before proxy.NormHost read
// their hosts, frozen for FuzzRiskSites.
func legacyRiskOf(host string) Risk {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if _, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		return RiskAddress
	}
	labels := strings.Split(host, ".")
	if slices.ContainsFunc(labels, func(l string) bool { return strings.HasPrefix(l, "xn--") }) {
		return RiskLookalike
	}
	for why, hosts := range riskyHosts() {
		if slices.ContainsFunc(hosts, func(h string) bool { return host == h || strings.HasSuffix(host, "."+h) }) {
			return why
		}
	}
	n := len(labels)
	aws := strings.HasSuffix(host, ".amazonaws.com") || strings.HasSuffix(host, ".amazonaws.com.cn")
	switch {
	case n >= 2 && labels[n-2] == "oast":
		return RiskWebhook
	case aws && slices.ContainsFunc(labels, func(l string) bool { return l == "s3" || strings.HasPrefix(l, "s3-") }):
		return RiskStorage
	case aws && slices.Contains(labels, "execute-api"):
		return RiskHosting
	}
	return ""
}

func legacyTickable(host string, port int) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if port != 443 {
		return false
	}
	for name := range tickable() {
		if host == name || strings.HasSuffix(host, "."+name) {
			return true
		}
	}
	return false
}

// A line that isn't a rule is the same as another in ASCII's case alone: a letter whose Unicode
// fold is ASCII (the Kelvin sign) names no rule, so devvm network rm with it never takes out the
// rule with the k.
func TestSameRuleFold(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		text, rule string
		want       bool
	}{
		{"!kubernetes.io", "!Kubernetes.io", false},
		{"kubernetes.io", "Kubernetes.io", false},
		{"example.com:x", "EXAMPLE.com:X", true},
		{"éx", "éx", true},
		{"éx", "Éx", false},
		{"Example.com:x.", "example.com:x", true},
	} {
		if got := sameRule(c.text, c.rule); got != c.want {
			t.Errorf("sameRule(%q, %q) = %v, want %v", c.text, c.rule, got, c.want)
		}
	}
}

// FuzzRiskSites: devvm network lock's risks and ticks read a host as they did. Divergences, for
// hosts the proxy never takes (proxy.HostOK), and devvm network lock only ever judges one it took
// (learning.add): a host with anything but printable ASCII isn't folded in Unicode (the Kelvin sign
// into k), so it's no risky or tickable name's; one with two trailing dots or more keeps them all
// (oast.. isn't oast.'s webhook); and one with a stray bracket (::[, [1.2.3.4]) is no address.
// Every host the proxy takes reads as it did.
func FuzzRiskSites(f *testing.F) {
	for _, s := range []string{
		"ngrok.io", "NGROK.io.", "x.ngrok.io..", "registry.npmjs.org.", "Registry.NPMJS.org", "Kubernetes.io",
		"[::1]", "1.2.3.4.", "xn--80ak6aa92e.com", "a.oast.fun.", "b.s3.us-east-1.amazonaws.com.", "", "::[", "[1.2.3.4]",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if !proxy.NormHost(s).OK() {
			return
		}
		if got, was := RiskOf(s), legacyRiskOf(s); got != was {
			t.Fatalf("RiskOf(%q) = %q, was %q", s, got, was)
		}
		if got, was := Tickable(s, 443), legacyTickable(s, 443); got != was {
			t.Fatalf("Tickable(%q) = %v, was %v", s, got, was)
		}
	})
}
