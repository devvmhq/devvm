package proxy

import (
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

// --- hosts ---
//
// Every package that judges or compares a host reads it in one form, Host, which NormHost makes:
// the proxy's requests, SNI check, rules, bindings and NO_PROXY, netpolicy's risks and ticks, a
// secret's --host, an MCP server's host, --from's registry and a partial clone's promisor remotes.
// The structure test refuses a copy of the normalization outside this file.

// Host is a host in the form every judge of hosts reads it in (NormHost): a name in lower case
// with its one trailing dot dropped, or an address literal, maybe in brackets. Only NormHost makes
// one from a string, so a matcher that takes a Host never sees a host in another form.
type Host string

// NormHost is s, a host as it was given (a request's, a rule's, a URL's, a setting's), as every
// judge reads it. A host of printable ASCII alone, without spaces, is lowercased, in ASCII alone,
// and its trailing dot is dropped when it has one alone: s with two or more stays as it is, so
// NormHost gives the same Host however many times it runs, and HostOK refuses both forms. A dot
// after a closing bracket ([::1].) stays too, so a bracketed address with a dot after it is no
// address. An address keeps its dot too, which can only be its IPv6 zone's (::1%.): dropping it
// would leave an empty zone, ::1%, which is no address to netip, so not to the judges, but is ::1
// to Go's resolver. A host with any other character stays as it is, its case and its dots too, so
// HostOK refuses it: strings.ToLower folds some letters into ASCII (the Kelvin sign into k), and
// the host judged would then not be the one that was named.
func NormHost(s string) Host {
	if !hostASCII(s) {
		return Host(s)
	}
	s = strings.ToLower(s)
	if _, ok := hostAddr(s); ok {
		return Host(s)
	}
	if t, ok := strings.CutSuffix(s, "."); ok && !strings.HasSuffix(t, ".") && !strings.HasSuffix(t, "]") {
		s = t
	}
	return Host(s)
}

// String is the host as text.
func (h Host) String() string { return string(h) }

// OK reports whether h is a host the proxy takes (HostOK).
func (h Host) OK() bool { return HostOK(string(h)) }

// Addr is the address h is, and whether it's one (hostAddr).
func (h Host) Addr() (netip.Addr, bool) { return hostAddr(string(h)) }

// hostAddr is the address s is: bare, or an IPv6 address in one pair of brackets, and never one
// with a stray bracket ("1.2.3.4]", "[[::1]]") or an IPv4 address in brackets, which no URL or
// request writes, so a host of another shape is no address.
func hostAddr(s string) (netip.Addr, bool) {
	inner, bracketed := strings.CutPrefix(s, "[")
	if bracketed {
		if inner, bracketed = strings.CutSuffix(inner, "]"); !bracketed {
			return netip.Addr{}, false
		}
	}
	a, err := netip.ParseAddr(inner)
	if err != nil || bracketed && !a.Is6() {
		return netip.Addr{}, false
	}
	return a, true
}

// Under reports whether h is domain, a name in Host's form, or a name under it.
func (h Host) Under(domain string) bool {
	return string(h) == domain || strings.HasSuffix(string(h), "."+domain)
}

// HostOK reports whether h is a host name (letters, digits, '-', '_', '.') or an address, bare or
// an IPv6 one in one pair of brackets (hostAddr). The VM
// chooses the host, and it ends up in the log, in the policy and in the allow command devvm
// suggests for the host's shell; agentlogin checks an MCP server's host with it before it goes into
// a VM's allowlist.
// A name is NormHost's form, with no trailing dot, and has no empty label: a company proxy that
// drops trailing dots (evil.test.., which NormHost leaves as it is) would reach a host that a deny
// of evil.test never matched.
func HostOK(h string) bool {
	if a, ok := hostAddr(h); ok {
		return a.Zone() == "" // a zone (fe80::1%eth0) is any text
	}
	if h == "" || len(h) > 253 || slices.Contains(strings.Split(h, "."), "") {
		return false
	}
	// An address in a shorthand netip doesn't read (127.1, 2130706433, 0x7f000001, 10.1): no name
	// ends in such a label, and a company proxy or resolver that reads it as inet_aton does would
	// reach an address the floor never judged.
	if last := h[strings.LastIndexByte(h, '.')+1:]; strings.HasPrefix(last, "0x") || strings.Trim(last, "0123456789") == "" {
		return false
	}
	for _, c := range h {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' && c != '.' {
			return false
		}
	}
	return true
}

// splitHostPort: the host, in NormHost's form without an IPv6 address's one pair of brackets (so
// the policy, bindings, SNI check and log all see one form), and the port (def when there's none).
// Brackets around anything but an IPv6 address stay, so HostOK refuses the host: net.SplitHostPort
// takes them off any host ([evil.com]:443), and a stray one ("1.2.3.4]") is no address's.
func splitHostPort(hp string, def int) (string, int) {
	h, p, err := net.SplitHostPort(hp)
	if err != nil {
		h, p = hp, ""
	} else if strings.HasPrefix(hp, "[") {
		h = "[" + h + "]"
	}
	if _, ok := hostAddr(h); ok && strings.HasPrefix(h, "[") {
		h = h[1 : len(h)-1]
	}
	n, err := strconv.Atoi(p)
	if err != nil {
		n = def
	}
	return NormHost(h).String(), n
}

// hostASCII reports whether s is printable ASCII alone, without spaces: the only bytes a host the
// proxy takes can have, checked before anything folds its case.
func hostASCII(s string) bool {
	return !strings.ContainsFunc(s, func(r rune) bool { return r <= ' ' || r > '~' })
}

// asciiEqualFold reports whether a and b are the same in ASCII, letters' case aside: never when
// either has anything but printable ASCII, which a Unicode fold could make equal (U+212A and k).
func asciiEqualFold(a, b string) bool { return hostASCII(a) && hostASCII(b) && strings.EqualFold(a, b) }
