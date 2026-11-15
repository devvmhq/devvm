package proxy

import (
	"encoding/json"
	"net"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestNormHost(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]Host{
		"example.com":    "example.com",
		"Example.COM":    "example.com",
		"example.com.":   "example.com",
		"[::1].":         "[::1].", // a dot after a bracket stays: no address
		"Example.COM.":   "example.com",
		"example.com..":  "example.com..", // two dots: no host, and NormHost stays idempotent
		".":              "",
		"..":             "..",
		"1.2.3.4.":       "1.2.3.4",
		"[::1]":          "[::1]", // brackets are the caller's (splitHostPort's)
		"::FFFF:1.2.3.4": "::ffff:1.2.3.4",
		"*.Example.com.": "*.example.com",
		"Kubernetes.io":  "Kubernetes.io", // the Kelvin sign is never folded into k
		"Ké.example.":    "Ké.example.",   // nor anything else in a host that isn't ASCII
		"a b.example":    "a b.example",
		"":               "",
	} {
		if got := NormHost(in); got != want {
			t.Errorf("NormHost(%q) = %q, want %q", in, got, want)
		}
		if again := NormHost(NormHost(in).String()); again != NormHost(in) {
			t.Errorf("NormHost(NormHost(%q)) = %q, not idempotent", in, again)
		}
	}
	if !NormHost("API.github.com.").Under("github.com") || NormHost("evilgithub.com").Under("github.com") || !NormHost("github.com").Under("github.com") {
		t.Error("Under")
	}
	if a, ok := NormHost("[::1]").Addr(); !ok || a != netip.IPv6Loopback() {
		t.Errorf("Addr of [::1] = %v, %v", a, ok)
	}
	if _, ok := NormHost("example.com").Addr(); ok {
		t.Error("example.com is an address")
	}
	// An address is bare, or IPv6 in one pair of brackets: never with a stray bracket.
	for s, want := range map[string]bool{
		"1.2.3.4": true, "::1": true, "[::1]": true, "[::ffff:1.2.3.4]": true,
		"1.2.3.4]": false, "[1.2.3.4]": false, "[[::1]]": false, "[::1": false, "::1]": false, "[]": false,
	} {
		if _, ok := NormHost(s).Addr(); ok != want || HostOK(s) != want {
			t.Errorf("%q: an address %v, HostOK %v; want %v", s, ok, HostOK(s), want)
		}
	}
	for hp, want := range map[string]string{
		"[::1]:443": "::1", "[::1]": "::1", "1.2.3.4:443": "1.2.3.4",
		"[evil.com]:443": "[evil.com]", "[1.2.3.4]:443": "[1.2.3.4]", "1.2.3.4]": "1.2.3.4]", "[[::1]]": "[[::1]]",
	} {
		if h, _ := splitHostPort(hp, 443); h != want {
			t.Errorf("splitHostPort(%q) = %q, want %q", hp, h, want)
		}
	}
}

// The normalization each site of the proxy did before NormHost, frozen as it was, so
// FuzzHostSites checks that every site reads a host as it did, but where the divergences listed
// with each case below say otherwise. Every other package's site has its own frozen copy and
// differential test next to it: netpolicy's risks and ticks, secrets' CheckHost, from's
// publicHost, agentlogin's claudeHost and vm's urlHostPort and privateHost.

// legacySplitHostPort is splitHostPort as it was.
func legacySplitHostPort(hp string, def int) (string, int) {
	norm := func(h string) string {
		if !hostASCII(h) {
			return h
		}
		return strings.TrimSuffix(strings.ToLower(h), ".")
	}
	h, p, err := net.SplitHostPort(hp)
	if err != nil {
		return norm(strings.Trim(hp, "[]")), def
	}
	n, err := strconv.Atoi(p)
	if err != nil {
		return norm(h), def
	}
	return norm(h), n
}

// legacyPolicyHost is how policy.rulesFor, quiet and byName read a host before they matched it.
func legacyPolicyHost(host string) string { return strings.ToLower(strings.TrimSuffix(host, ".")) }

// legacySNIMatches is sniMatches as it was.
func legacySNIMatches(sni, host string) bool {
	if _, err := netip.ParseAddr(host); err == nil {
		return sni == "" || asciiEqualFold(sni, host)
	}
	return asciiEqualFold(strings.TrimSuffix(sni, "."), host)
}

// legacySNIHost is sniHost as it was.
func legacySNIHost(sni string) (string, string) {
	switch {
	case sni == "":
		return "", reasonNoSNI
	case !hostASCII(sni):
		return "", "the TLS server name " + quoteCapped(sni) + " isn't ASCII"
	}
	h := strings.ToLower(sni)
	if _, err := netip.ParseAddr(strings.Trim(h, "[]")); err == nil {
		return "", reasonSNIAddr
	}
	if !HostOK(h) {
		return "", "the TLS server name " + quoteCapped(sni) + " isn't a host name"
	}
	if ownDomain(h) {
		return "", reasonOwnDomain
	}
	return h, ""
}

// legacyParseHostRule is parseHostRule as it was.
func legacyParseHostRule(s string) (rule, bool) {
	if s == "" || strings.ContainsAny(s, "#%") || strings.ContainsFunc(s, func(c rune) bool { return c <= ' ' || c > '~' }) {
		return rule{}, false
	}
	line := strings.ToLower(s)
	r := rule{text: line}
	if rest, ok := strings.CutPrefix(line, "!"); ok {
		r.deny, line = true, rest
	}
	line, port, ok := cutPort(line)
	if !ok {
		return rule{}, false
	}
	r.port = port
	if p, err := netip.ParsePrefix(line); err == nil {
		if p.Addr().Is4In6() {
			if p.Bits() < 96 {
				return rule{}, false
			}
			p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
		}
		r.prefix = p.Masked()
		return r, true
	}
	if a, err := netip.ParseAddr(line); err == nil {
		r.prefix = netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen())
		return r, true
	}
	if line == "**" {
		r.host = line
		return r, true
	}
	h := strings.TrimSuffix(line, ".")
	name := strings.TrimPrefix(h, "*.")
	if _, err := netip.ParseAddr(name); err == nil || strings.ContainsAny(name, "[]*") || !HostOK(name) {
		return rule{}, false
	}
	r.host = h
	return r, true
}

// legacyParseNoProxy is parseNoProxy as it was.
func legacyParseNoProxy(s string) []noProxyRule {
	var out []noProxyRule
	for e := range strings.SplitSeq(s, ",") {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if e == "*" {
			out = append(out, noProxyRule{all: true})
			continue
		}
		r := noProxyRule{}
		if p, err := netip.ParsePrefix(e); err == nil {
			r.prefix = p.Masked()
			out = append(out, r)
			continue
		}
		if h, p, err := net.SplitHostPort(e); err == nil {
			if n, err := strconv.Atoi(p); err == nil {
				e, r.port = h, n
			}
		}
		e = strings.Trim(e, "[]")
		if a, err := netip.ParseAddr(e); err == nil {
			r.prefix = netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen())
			out = append(out, r)
			continue
		}
		if strings.HasPrefix(e, "*.") {
			e = e[1:]
		}
		r.name = strings.TrimSuffix(e, ".")
		if r.name != "" && r.name != "." {
			out = append(out, r)
		}
	}
	return out
}

// legacyNoProxyMatches is noProxyRule.matches as it was, and legacyLocal local.
func legacyNoProxyMatches(r noProxyRule, host string, port int) bool {
	if r.all {
		return true
	}
	if r.port != 0 && r.port != port {
		return false
	}
	if r.prefix.IsValid() {
		a, err := netip.ParseAddr(strings.Trim(host, "[]"))
		return err == nil && r.prefix.Contains(a.Unmap())
	}
	if strings.HasPrefix(r.name, ".") {
		return strings.HasSuffix(host, r.name)
	}
	return host == r.name || strings.HasSuffix(host, "."+r.name)
}

func legacyLocal(host string) bool {
	if a, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		return a.Unmap().IsLoopback() || a.IsUnspecified()
	}
	return host == "localhost" || strings.HasSuffix(host, ".localhost")
}

// twoDots reports whether s ends in two dots or more, which NormHost leaves as they are, where
// the sites dropped one: either way it's no host (HostOK), and no rule matches it but **.
func twoDots(s string) bool { return strings.HasSuffix(s, "..") }

// hostSeeds are the shapes the sites disagreed on: case, trailing dots, ports, brackets, IPv6,
// zones, non-ASCII letters (the Kelvin sign), punycode, empty labels, wildcards and denies.
func hostSeeds() []string {
	return []string{
		"example.com", "Example.COM", "example.com.", "example.com..", "example.com:443", "Example.com.:8080",
		"[::1]", "[::1]:443", "::1", "[::FFFF:10.0.0.1]:80", "fe80::1%eth0", "1.2.3.4.", "[1.2.3.4]", "[evil.com]:443",
		"Kubernetes.io", "Kubernetes.io:443", "xn--80ak6aa92e.com", "a..b", ".evil.test", "evil.test..",
		"*.example.com", "*.example.com.", "**", "**.", "!evil.test", "!evil.test..", "!Evil.TEST.", "!Kubernetes.io",
		"localhost", "LOCALHOST.", "x.localhost", "devvm.proxy", "Db.devvm.internal.", ".", "..", "", " example.com ",
		"example.com:x", "*", ".example.com", "10.0.0.0/8", "127.1", "0x7f000001",
		"1.2.3.4]", "[[::1]]", "[1.2.3.4]:443", "[::1", "[::ffff:1.2.3.4]", "[::].", "[::1].:443",
	}
}

// FuzzHostSites is the differential check of every site of the proxy that read a host, against
// its frozen copy: each reads it as it did, but where its case says.
func FuzzHostSites(f *testing.F) {
	for _, s := range hostSeeds() {
		f.Add(s, "api.example.com")
	}
	f.Fuzz(func(t *testing.T, s, other string) {
		splitSite(t, s)
		policySite(t, s)
		ruleSite(t, s)
		sniSites(t, s, other)
		noProxySite(t, s, other)
	})
}

// splitSite: the requests' hosts (CONNECT's, absolute-form, the transparent HTTP Host, an
// interception's requests). Divergences: a host with two trailing dots or more keeps them all,
// where one went before; HostOK refuses both, so the request gets 400 either way. Brackets around
// anything but an IPv6 address, or stray ones ([evil.com]:443, [1.2.3.4], 1.2.3.4], [[::1]]), stay,
// so HostOK refuses the host and the request gets 400, where strings.Trim or net.SplitHostPort took
// them off and the name or address in them was judged.
func splitSite(t *testing.T, s string) {
	for _, def := range []int{80, 443} {
		h, p := splitHostPort(s, def)
		lh, lp := legacySplitHostPort(s, def)
		brackets := strings.ContainsAny(s, "[]") && !HostOK(h)
		if p != lp || h != lh && !brackets && (HostOK(h) || HostOK(lh) || !twoDots(h)) {
			t.Fatalf("splitHostPort(%q) = %q, %d; was %q, %d", s, h, p, lh, lp)
		}
	}
}

// policySite: rulesFor, quiet and byName, which every verdict, Allows, Denies and Quiet goes
// through. Divergences: a host with anything but printable ASCII isn't folded, where
// strings.ToLower folded it in Unicode (the Kelvin sign into k): the proxy never judges one, as
// splitHostPort kept it and HostOK refused it first, and other callers' hosts are NormHost's. Two
// trailing dots: no rule matches either form but **, which matches both. A stray bracket, an IPv4
// address in brackets, or a dot after a bracket ([1.2.3.4], ::1], [::].) is no address, where
// strings.Trim made it one that address rules matched: HostOK refuses each, so the proxy never
// judges one, and no other caller's host has brackets.
func policySite(t *testing.T, s string) {
	h, lh := NormHost(s), legacyPolicyHost(s)
	_, isAddr := h.Addr()
	_, err := netip.ParseAddr(strings.Trim(lh, "[]"))
	if isAddr != (err == nil) && (h.OK() || !strings.ContainsAny(s, "[]")) {
		t.Fatalf("%q: an address %v; the policy read %q, an address %v", s, isAddr, lh, err == nil)
	}
	switch {
	case h.String() == lh:
		return
	case strings.HasSuffix(lh, "]") && strings.HasSuffix(h.String(), "].") && !h.OK():
		return
	case !hostASCII(s):
		if h.String() != s || h.OK() {
			t.Fatalf("NormHost(%q) = %q: a host that isn't ASCII stays as it is, and isn't one", s, h)
		}
	case twoDots(s):
		for _, rs := range []string{"**", lh, strings.TrimSuffix(lh, "."), "*." + strings.TrimSuffix(lh, ".")} {
			r, ok := parseRule(rs)
			if ok && r.matchHost(h) != r.matchHost(Host(lh)) {
				t.Fatalf("rule %q matches %q and %q apart", rs, h, lh)
			}
		}
	default:
		t.Fatalf("NormHost(%q) = %q; the policy read %q", s, h, lh)
	}
}

// ruleSite: a rule's host, in a policy file, a binding's hosts and devvm network's commands. No
// divergence at all: a line is a rule exactly when it was, the same rule, so a line that was a
// broken deny (one that makes the policy deny everything) is one still, and none became a deny.
func ruleSite(t *testing.T, s string) {
	r, ok := parseHostRule(s)
	lr, lok := legacyParseHostRule(s)
	if ok != lok || r != lr {
		t.Fatalf("parseHostRule(%q) = %+v, %v; was %+v, %v", s, r, ok, lr, lok)
	}
	line := PolicyLine{N: 1, Text: s}
	_, line.OK = parseRule(s)
	if strings.TrimSpace(s) == s && !strings.ContainsAny(s, " \t#") && BrokenDeny(line) != (!lok && strings.HasPrefix(s, "!")) {
		t.Fatalf("%q: a broken deny %v, was %v", s, BrokenDeny(line), !lok && strings.HasPrefix(s, "!"))
	}
}

// sniSites: the SNI check on every tunnel, against a host splitHostPort took, and the transparent
// path's SNI host. Divergence: sniHost drops a trailing dot, as every host's is dropped, where it
// kept it and refused the name; crypto/tls refuses a server name with a trailing dot before the
// proxy sees it, so no ClientHello gives one. A name with brackets ([1.2.3.4]) is refused as no
// host name, where it was refused as an address: refused either way, for another reason.
func sniSites(t *testing.T, sni, other string) {
	for _, hp := range []string{other, sni} {
		host, _ := splitHostPort(hp, 443)
		if !HostOK(host) {
			continue
		}
		if got, was := sniMatches(sni, NormHost(host)), legacySNIMatches(sni, host); got != was {
			t.Fatalf("sniMatches(%q, %q) = %v, was %v", sni, host, got, was)
		}
	}
	h, why := sniHost(sni)
	lh, lwhy := legacySNIHost(sni)
	if (h != lh || why != lwhy) && !strings.HasSuffix(sni, ".") && (h != "" || lh != "" || !strings.ContainsAny(sni, "[]")) {
		t.Fatalf("sniHost(%q) = %q, %q; was %q, %q", sni, h, why, lh, lwhy)
	}
}

// noProxySite: NO_PROXY's entries and the hosts they send directly. Divergence: an entry with
// anything but printable ASCII isn't folded, where strings.ToLower folded it in Unicode, so it
// matches no host the proxy takes (the Kelvin sign's kubernetes.io no longer matches
// kubernetes.io). An entry of dots alone is no entry, as "" and "." weren't. An entry with a port
// no request has (0, or past 65535), or one not in plain decimal (080, +80), is no entry, where
// port 0 was read as every port and 080 as 80.
func noProxySite(t *testing.T, entry, other string) {
	entry = strings.ReplaceAll(entry, ",", "") // one entry
	list := entry + ",*." + entry + ",." + entry
	var kept []string
	for e := range strings.SplitSeq(list, ",") {
		e = strings.ToLower(strings.TrimSpace(e))
		_, perr := netip.ParsePrefix(e)
		_, p, err := net.SplitHostPort(e)
		n, nerr := strconv.Atoi(p)
		if perr == nil || err != nil || nerr != nil || n >= 1 && n <= 65535 && strconv.Itoa(n) == p {
			kept = append(kept, e)
		}
	}
	rs, lrs := parseNoProxy(list), slices.DeleteFunc(legacyParseNoProxy(strings.Join(kept, ",")), func(r noProxyRule) bool {
		return !r.all && !r.prefix.IsValid() && strings.Trim(r.name, ".") == ""
	})
	var hosts []string
	for _, hp := range []string{other, entry, "a." + entry, "localhost", "127.0.0.1"} {
		if h, _ := splitHostPort(hp, 443); HostOK(h) {
			hosts = append(hosts, h)
		}
	}
	for _, h := range hosts {
		if local(NormHost(h)) != legacyLocal(h) {
			t.Fatalf("local(%q) = %v, was %v", h, local(NormHost(h)), legacyLocal(h))
		}
	}
	if !hostASCII(strings.TrimSpace(entry)) {
		for _, r := range rs {
			for _, h := range hosts {
				if r.name != "" && r.matches(NormHost(h), 443) {
					t.Fatalf("NO_PROXY %q: %+v matches %q", entry, r, h)
				}
			}
		}
		return
	}
	if len(rs) != len(lrs) {
		t.Fatalf("parseNoProxy(%q) = %+v, was %+v", list, rs, lrs)
	}
	for i := range rs {
		for _, h := range hosts {
			for _, port := range []int{443, 8080} {
				if got, was := rs[i].matches(NormHost(h), port), legacyNoProxyMatches(lrs[i], h, port); got != was {
					t.Fatalf("NO_PROXY %q's %+v on %s:%d = %v, was %v", list, rs[i], h, port, got, was)
				}
			}
		}
	}
}

// Every policy line, binding host and link host in a release's frozen state reads as it did: each
// line is the rule it was, or no rule as it was, and each link's names are already in NormHost's
// form, so a request for one finds it.
func TestFixtureHosts(t *testing.T) {
	t.Parallel()
	for _, f := range fixtureFiles(t, "secrets.policy") {
		b, err := os.ReadFile(f.path)
		must(t, err)
		for _, l := range ParsePolicy(string(b)) {
			ruleSite(t, l.Text)
			if !l.OK {
				t.Errorf("%s:%d: %q isn't a rule", f.path, l.N, l.Text)
			}
		}
	}
	for _, f := range fixtureFiles(t, "secrets.json") {
		b, err := os.ReadFile(f.path)
		must(t, err)
		l, err := parseBindings(f.path, b)
		must(t, err)
		for _, bd := range l.Bindings {
			for _, h := range bd.Hosts {
				ruleSite(t, h)
				policySite(t, h)
			}
		}
	}
	for _, f := range fixtureFiles(t, "links.json") {
		b, err := os.ReadFile(f.path)
		must(t, err)
		var l LinkList
		must(t, json.Unmarshal(b, &l))
		for _, e := range l.Links {
			for _, h := range e.Hosts {
				if got, _ := splitHostPort(h+":5432", 0); got != h || !HostOK(h) {
					t.Errorf("%s: link host %q reads as %q", f.path, h, got)
				}
			}
		}
	}
}
