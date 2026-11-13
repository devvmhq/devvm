package proxy

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/devvmhq/devvm/internal/safefile"
)

// A rule from a policy file. One per line, '#' starts a comment:
//
//	example.com       exactly that host
//	*.example.com     any subdomain, at any depth (not example.com itself)
//	**                any host, or address (the floor still refuses internal ones)
//	10.0.0.5, 10.0.0.0/8, fd00::/8   addresses (also needed to reach private ranges, see floor)
//	…:22              only that port; without a port, 443 and 80
//	…:*               any port
//	!rule             deny; a deny beats any allow
//	… GET,HEAD /repos/**   only those methods, or that path, or both, of a host name's requests
//
// Matching hosts is case-insensitive. A rule with methods or a path narrows its host to some
// requests (match), which the proxy judges one by one (policy.judge), decrypting the host's HTTPS
// to see them.
type rule struct {
	text   string
	deny   bool
	host   string       // "", "**", "*.example.com" or "example.com"
	prefix netip.Prefix // for address rules
	port   int          // 0: 443 and 80; anyPort: every port
	req    match        // the requests it covers: every one unless it narrows
	// quiet is a built-in policy's `quiet HOST[:PORT]` line: a host it leaves out on purpose, such
	// as a tool's telemetry, whose blocks give the VM no notice and no command to allow it
	// (policy.quiet). It allows and denies nothing, and only devvm's own policies have one.
	quiet bool
}

// narrows reports whether r covers some of its host's requests alone.
func (r rule) narrows() bool { return r.req.narrows() }

// parseRule reads one rule, strictly: a host rule (parseHostRule), then, for a host name or
// *.name, optionally its methods (ParseMethods) and then its path (PathOK), each after spaces or
// tabs. Its text is the host rule as parseHostRule has it, then the methods in their canonical
// form and the path as written, joined by one space.
func parseRule(s string) (rule, bool) {
	f := strings.FieldsFunc(s, func(c rune) bool { return c == ' ' || c == '\t' })
	if len(f) == 0 || len(f) > 3 {
		return rule{}, false
	}
	r, ok := parseHostRule(f[0])
	if !ok || len(f) == 1 {
		return r, ok
	}
	var methods, p string
	for _, x := range f[1:] {
		switch {
		case strings.HasPrefix(x, "/") && p == "":
			if !PathOK(x) {
				return rule{}, false
			}
			p = x
		case methods == "" && p == "":
			if methods, ok = ParseMethods(x); !ok {
				return rule{}, false
			}
		default:
			return rule{}, false
		}
	}
	// A host name's alone: an address has no name to decrypt by, and ** would decrypt everything.
	if r.host == "" || r.host == "**" {
		return rule{}, false
	}
	r.req = newMatch(methods, p)
	r.text = strings.Join(slices.DeleteFunc([]string{r.text, methods, p}, func(x string) bool { return x == "" }), " ")
	return r, true
}

// anyPort is a rule's port for :*, every port.
const anyPort = -1

// parseHostRule reads a rule's host, its port and its !, strictly: s is the whole of it, without a
// comment or spaces. It refuses anything that wouldn't match as written: a port that isn't 1 to
// 65535 in digits, a host that isn't a plain name (HostOK, with no empty label), a zone, a
// wildcard other than a leading "*." or "**", an address under "*.", an IPv4-mapped prefix shorter
// than the mapping, anything but printable ASCII. A host's one trailing dot is dropped, as
// requests' are, and a mapped prefix is its IPv4 one.
func parseHostRule(s string) (rule, bool) {
	// Printable ASCII alone: no spaces, and nothing strings.ToLower would fold into ASCII (the
	// Kelvin sign into k).
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
		// An IPv4-mapped prefix is its IPv4 one, as matchAddr unmaps what it judges; one
		// shorter than the mapping's 96 bits isn't a rule.
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

// cutPort takes a rule's port off: host:port, 1.2.3.4:port, [v6]:port or cidr:port, the port 1 to
// 65535 in digits, or * for every port (anyPort). Without one, the rule is as it was and the port
// 0. false: a port that isn't one.
func cutPort(s string) (string, int, bool) {
	h, p, err := net.SplitHostPort(s)
	if err == nil && strings.HasPrefix(s, "[") {
		if a, err := netip.ParseAddr(h); err != nil || !a.Is6() {
			return "", 0, false // brackets hold an IPv6 address alone
		}
	}
	if err != nil {
		i := strings.LastIndex(s, ":")
		if i <= 0 || !strings.Contains(s, "/") || strings.Contains(s[:i], ":") {
			return s, 0, true // no port, or an IPv6 address
		}
		h, p = s[:i], s[i+1:]
	}
	if p == "*" {
		return h, anyPort, true
	}
	n, err := strconv.Atoi(p)
	if err != nil || strings.Trim(p, "0123456789") != "" || n < 1 || n > 65535 {
		return "", 0, false
	}
	return h, n, true
}

// ValidRule reports whether s is one rule, as devvm network allow writes it
// and the proxy takes it from a policy file's line: a host name, *.name, **
// or an address or CIDR (no zone), optionally with a port (or :* for any)
// and a leading !, and for a host name or *.name, its methods and its path
// after spaces, and no comment. AllowRule's rules are valid.
func ValidRule(s string) bool {
	_, ok := parseRule(s)
	return ok
}

// CanonicalRule is s, a rule (ValidRule), as the proxy reads it: its host
// in lower case, its methods in their canonical form, and one space between
// the parts. false for what isn't a rule.
func CanonicalRule(s string) (string, bool) {
	r, ok := parseRule(s)
	return r.text, ok
}

// NarrowRule is the rule host (a rule without spaces, ValidRule's) narrowed
// to methods (ParseMethods' form, or "") and the path pattern p (PathOK, or
// ""), as devvm network allow, deny and rm write it from their flags, in
// its canonical form. false when it isn't a rule: methods or a path on an
// address, a CIDR or **.
func NarrowRule(host, methods, p string) (string, bool) {
	return CanonicalRule(strings.Join(slices.DeleteFunc([]string{host, methods, p}, func(x string) bool { return x == "" }), " "))
}

// Narrowable reports whether the rule host (without spaces) may take
// methods or a path: a host name or *.name, never an address, a CIDR or **,
// which have no name the proxy could decrypt their HTTPS by.
func Narrowable(host string) bool {
	r, ok := parseHostRule(host)
	return ok && r.host != "" && r.host != "**"
}

// A PolicyLine is a line of a policy file that holds more than a comment.
type PolicyLine struct {
	N    int    // its number, from 1
	Text string // as written, without its comment or the spaces around it
	OK   bool   // whether it's a rule (ValidRule): the proxy skips one that isn't
}

// Quiet reports whether l is a `quiet HOST[:PORT]` line, which isn't a rule: a built-in policy's
// marks a host it leaves out quietly (rule.quiet), and the proxy skips one in any other file.
func (l PolicyLine) Quiet() bool {
	_, ok := parseQuiet(l.Text)
	return ok
}

// ParsePolicy reads a policy file: one rule a line, '#' starting a comment. It returns each line
// that holds more than a comment, in order, each marked whether it's a rule. The proxy, devvm
// network and devvm doctor all read policy files with it.
func ParsePolicy(text string) []PolicyLine {
	lines, _ := parsePolicy(text)
	return lines
}

// parsePolicy is ParsePolicy, with the rules of the lines that are rules.
func parsePolicy(text string) ([]PolicyLine, []rule) {
	var (
		lines []PolicyLine
		rules []rule
	)
	for i, l := range strings.Split(strings.TrimPrefix(text, "\xef\xbb\xbf"), "\n") { // a byte-order mark isn't text
		s, _, _ := strings.Cut(l, "#")
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		if q, ok := parseQuiet(s); ok {
			lines = append(lines, PolicyLine{N: i + 1, Text: s})
			rules = append(rules, q)
			continue
		}
		r, ok := parseRule(s)
		lines = append(lines, PolicyLine{N: i + 1, Text: s, OK: ok})
		if ok {
			rules = append(rules, r)
		}
	}
	return lines, rules
}

// quietWord starts a built-in policy's quiet line (rule.quiet).
const quietWord = "quiet"

// parseQuiet reads `quiet HOST[:PORT]`, one exact host name's rule (parseHostRule) after the word,
// as a quiet rule: never a wildcard, so a quiet line can't hide a whole domain's blocks.
func parseQuiet(s string) (rule, bool) {
	f := strings.FieldsFunc(s, func(c rune) bool { return c == ' ' || c == '\t' })
	if len(f) != 2 || f[0] != quietWord {
		return rule{}, false
	}
	r, ok := parseHostRule(f[1])
	if !ok || r.deny || r.host == "" || strings.Contains(r.host, "*") {
		return rule{}, false
	}
	r.quiet, r.text = true, quietWord+" "+r.text
	return r, true
}

func (r rule) portOK(port int) bool {
	switch r.port {
	case 0:
		return port == 443 || port == 80
	case anyPort:
		return port >= 1 && port <= 65535
	}
	return r.port == port
}

// matchHost: does a host-name rule cover host (lower-case, no port)?
func (r rule) matchHost(host string) bool {
	switch {
	case r.host == "":
		return false
	case r.host == "**":
		return true
	case strings.HasPrefix(r.host, "*."):
		return strings.HasSuffix(host, r.host[1:])
	default:
		return r.host == host
	}
}

// matchAddr: does an address rule cover a? A NAT64 address also matches as the IPv4 address in it,
// as internal() judges it, so a rule for an IPv4 address holds on IPv6-only networks too. A 6to4
// address matches as its IPv4 address for a deny alone: its site is reached through relays anyone
// may run, so an allow for the IPv4 address doesn't open it (withIPv4's site).
func (r rule) matchAddr(a netip.Addr) bool {
	if !r.prefix.IsValid() {
		return false
	}
	a = a.Unmap()
	if r.prefix.Contains(a) {
		return true
	}
	v4, site, ok := embeddedIPv4(a)
	return ok && (site == 0 || r.deny) && r.prefix.Contains(v4)
}

// policy is the union of devvm's built-in policies, read once, and several files, each re-read when
// it changes (watched). A file that's gone has no rules; one that can't be read keeps its last
// rules and broken deny, logged once (a deny dropped for a moment would let through).
type policy struct {
	// builtin is the built-in policies' rules (readBuiltins), ahead of the files'.
	builtin []rule
	files   []*watched[policyFile]
	// open allows every host and address on every port, the floor's included: no rule applies
	// (Config.Open).
	open bool
}

// policyFile is what one policy file holds.
type policyFile struct {
	rules []rule
	// broken is the first of its lines that starts with ! and isn't a rule (BrokenDeny): while any
	// file has one, the policy denies everything.
	broken *PolicyLine
}

// PolicyMax bounds a policy file that the proxy, devvm network and devvm doctor read.
const PolicyMax = 1 << 20

// ReadPolicy reads a policy file, through a symbolic link if it's one, as the proxy, devvm network
// and devvm doctor read it: a regular file of at most PolicyMax, opened without blocking, so a
// FIFO or a device there (a link to /dev/zero) can't hold or flood its reader
// (safefile.ErrNotRegular, safefile.ErrTooLong).
func ReadPolicy(path string) ([]byte, error) {
	return safefile.Read(path, PolicyMax, safefile.Opts{Follow: true})
}

// newPolicy is the policy of builtin, files and denies, whose denies alone apply, logging to l,
// the proxy's own log, what was read and what was skipped.
func newPolicy(builtin []rule, files, denies []string, l *log.Logger) *policy {
	p := &policy{builtin: builtin}
	add := func(f string, deniesOnly bool) {
		p.files = append(p.files, &watched[policyFile]{
			// A FIFO or a device there (a link to /dev/zero) is refused, not read (ReadPolicy).
			path: f, follow: true, keep: true,
			read:   ReadPolicy,
			parse:  func(b []byte) (policyFile, error) { return loadPolicy(f, string(b), deniesOnly, l), nil },
			onFail: func(err error) { l.Printf("policy %v; its last rules kept", err) },
		})
	}
	for _, f := range files {
		add(f, false)
	}
	for _, f := range denies {
		add(f, true)
	}
	return p
}

// readBuiltins reads the built-in policies names, paths in devvm's own files own, once, in order,
// logging each to l. Each is devvm's own, so one that's missing or holds a line that isn't a rule
// is devvm's bug: an error, and the proxy refuses to start, rather than run with fewer rules than
// devvm meant.
func readBuiltins(own fs.FS, names []string, l *log.Logger) ([]rule, error) {
	if len(names) > 0 && own == nil {
		return nil, errors.New("built-in policies need devvm's own files")
	}
	var out []rule
	for _, n := range names {
		b, err := fs.ReadFile(own, n)
		if err != nil {
			return nil, fmt.Errorf("built-in policy %s: %w", n, err)
		}
		lines, rs := parsePolicy(string(b))
		for _, l := range lines {
			if !l.OK && !l.Quiet() {
				return nil, fmt.Errorf("built-in policy %s:%d isn't a rule: %q", n, l.N, l.Text)
			}
		}
		l.Printf("built-in policy %s: %d rules", n, len(rs))
		out = append(out, rs...)
	}
	return out, nil
}

// BrokenDeny reports whether l is a line that reads as a deny, starting with !, but isn't a
// rule. Its proxy can't know what it would deny, so it denies everything while the line is there.
func BrokenDeny(l PolicyLine) bool { return !l.OK && strings.HasPrefix(l.Text, "!") }

// all returns the policy's rules, the built-in policies' first, and while a file has a broken deny
// (BrokenDeny), why the policy denies everything.
func (p *policy) all() ([]rule, string) {
	out := slices.DeleteFunc(slices.Clone(p.builtin), func(r rule) bool { return r.quiet })
	why := ""
	for _, w := range p.files {
		pf := w.get()
		out = append(out, pf.rules...)
		if l := pf.broken; l != nil && why == "" {
			why = fmt.Sprintf("%s:%d isn't a rule but reads as a deny (%q), so this VM's proxy denies everything until it's fixed", w.path, l.N, l.Text)
		}
	}
	return out, why
}

// quiet reports whether a built-in policy marks host:port quiet (rule.quiet) and no rule covers
// it, an allow, a deny or one that narrows, so it's blocked only because no rule allows it and its
// block gives the VM no notice: never when the policy is open or a file has a broken deny.
func (p *policy) quiet(host string, port int) bool {
	if h := p.rulesFor(host, port); h.open || h.broken != "" || len(h.rs) > 0 {
		return false
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	return slices.ContainsFunc(p.builtin, func(r rule) bool { return r.quiet && r.portOK(port) && r.matchHost(host) })
}

// loadPolicy is file f's rules from its text b, its denies alone with deniesOnly, logging to l
// each line that isn't a rule and noting the first broken deny.
func loadPolicy(f, b string, deniesOnly bool, l *log.Logger) policyFile {
	lines, rs := parsePolicy(b)
	// A quiet line is devvm's own policies' alone.
	rs = slices.DeleteFunc(rs, func(r rule) bool { return r.quiet || deniesOnly && !r.deny })
	pf := policyFile{rules: rs}
	for _, line := range lines {
		switch {
		case line.Quiet():
			l.Printf("%s:%d: only devvm's own policies mark a host quiet, skipped: %q", f, line.N, line.Text)
		case BrokenDeny(line):
			l.Printf("%s:%d: not a rule but reads as a deny, so everything is denied: %q", f, line.N, line.Text)
			if pf.broken == nil {
				pf.broken = &line
			}
		case !line.OK:
			l.Printf("%s:%d: not a rule, skipped: %q", f, line.N, line.Text)
		}
	}
	l.Printf("policy %s: %d rules", f, len(rs))
	return pf
}

// openRule is what an open policy logs as the rule that allowed a request.
const openRule = "open"

// deniedBy starts the reason of a refusal by a deny rule, which its text follows.
const deniedBy = "denied by "

// verdict is whether host:port may be reached, host a name or an address literal: the matching
// rule's text, or why not, and whether a refusal is one no allow rule would lift: a broken deny's.
// It judges host:port as a whole: a deny that narrows doesn't refuse it, as some of its requests
// may pass, and an allow that narrows allows it, for judge to look at each request. The proxy's
// own connections take the verdict and byRequest from one rulesFor (egress.checkHost).
func (p *policy) verdict(host string, port int) (bool, string, bool) {
	return p.rulesFor(host, port).verdict()
}

// judge is rulesFor(host, port).judge(r).
func (p *policy) judge(host string, port int, r *http.Request) (ok bool, why string, refused, narrowed bool) {
	return p.rulesFor(host, port).judge(r)
}

// hostRules are the rules that cover one host and port, from one read of the policy's files, so a
// connection's verdict and whether its requests are judged one by one come from the same rules.
type hostRules struct {
	open   bool   // the policy is open: no rule applies
	broken string // while a file has a broken deny, why the policy denies everything
	rs     []rule
}

// rulesFor is the rules that cover host:port, as verdict matches them, read from the files once.
func (p *policy) rulesFor(host string, port int) hostRules {
	if p.open {
		return hostRules{open: true}
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	rs, broken := p.all()
	if broken != "" {
		return hostRules{broken: broken}
	}
	addr, err := netip.ParseAddr(strings.Trim(host, "[]"))
	isAddr := err == nil
	return hostRules{rs: slices.DeleteFunc(rs, func(r rule) bool {
		if !r.portOK(port) {
			return true
		}
		if isAddr {
			// ** is any host, an address too; the floor still refuses an
			// internal one that no address rule names (addrAllowed).
			return !r.matchAddr(addr) && r.host != "**"
		}
		return !r.matchHost(host)
	})}
}

// verdict judges the host as a whole, as policy.verdict says.
func (h hostRules) verdict() (bool, string, bool) {
	switch {
	case h.open:
		return true, openRule, false
	case h.broken != "":
		return false, h.broken, true
	}
	for _, r := range h.rs {
		if r.deny && !r.narrows() {
			return false, deniedBy + r.text, false
		}
	}
	for _, r := range h.rs {
		if !r.deny {
			return true, r.text, false
		}
	}
	return false, notAllowed, false
}

// notAllowed is the reason for a block because no rule allows the host: the one a quiet host's
// block can have (server.blocked).
const notAllowed = "not on the allowlist"

// byRequest reports whether the host's requests are judged one by one (judge), as its verdict
// alone can't decide them: a rule that narrows covers it, and either it's a deny or no allow
// covers all of its requests. The proxy decrypts such a host's HTTPS to see them.
func (h hostRules) byRequest() bool {
	if h.open || h.broken != "" {
		return false
	}
	whole, narrowed := false, false
	for _, r := range h.rs {
		switch {
		case r.deny && r.narrows():
			return true
		case !r.deny && r.narrows():
			narrowed = true
		case !r.deny:
			whole = true
		}
	}
	return narrowed && !whole
}

// Why a request is refused by the rules that narrow its host (judge).
const (
	reasonPathNotPlain = "its path isn't in a plain form, which rules with a path need: no %, \\, ; or . or .. segment, and no doubled slash"
	reasonConnectIn    = "CONNECT inside a decrypted tunnel"
	reasonUpgrade      = "an upgrade to a protocol other than WebSocket, whose requests the proxy couldn't judge"
	reasonOverride     = "a method-override header or _method parameter that isn't exactly one method, which rules by method need"
	reasonNoRequest    = "no rule allows this method and path"
)

// judge decides r, a request to the host the proxy forwards, by the rules that cover it: a deny
// beats any allow, a deny that narrows refuses the requests it matches, and an allow that narrows
// lets through only those it matches. A path that isn't plain (plainPath) matches every deny with
// a path and no allow with one, so no server's reading of it slips past a deny. It returns whether
// r may go, the allowing rule's text or why not, whether no allow would lift the refusal, and
// whether a rule that narrows covered the host (narrowed). A host no rule narrows gets its
// verdict.
func (h hostRules) judge(r *http.Request) (ok bool, why string, refused, narrowed bool) {
	switch {
	case h.open:
		return true, openRule, false, false
	case h.broken != "":
		return false, h.broken, true, false
	}
	narrowed = slices.ContainsFunc(h.rs, rule.narrows)
	for _, x := range h.rs {
		if x.deny && !x.narrows() {
			return false, deniedBy + x.text, false, narrowed
		}
	}
	if why := h.unjudgeable(r); why != "" {
		return false, why, true, narrowed
	}
	plain := plainPath(r.URL)
	rs := h.rs
	for _, x := range rs {
		if x.deny && x.req.denies(r) {
			if x.req.path != "" && !plain {
				return false, reasonPathNotPlain, true, narrowed
			}
			return false, deniedBy + x.text, true, narrowed
		}
	}
	for _, x := range rs {
		if !x.deny && x.req.allows(r) {
			return true, x.text, false, narrowed
		}
	}
	switch {
	case !narrowed:
		return false, notAllowed, false, false
	case !plain && slices.ContainsFunc(rs, func(x rule) bool { return x.req.path != "" }):
		return false, reasonPathNotPlain, false, narrowed
	}
	return false, reasonNoRequest, false, narrowed
}

// unjudgeable is why r can't be judged by the rules that narrow its host, which refuses it
// whatever they say, or "": a CONNECT inside the tunnel, or a switch to another protocol than
// WebSocket, after which the proxy would pass the bytes unread (an h2c upgrade would carry requests
// unjudged); and while a rule narrows the host by method, a method-override header or _method
// parameter that isn't exactly one method (overrides), which a server might read either way.
func (h hostRules) unjudgeable(r *http.Request) string {
	if !slices.ContainsFunc(h.rs, rule.narrows) {
		return ""
	}
	if r.Method == http.MethodConnect {
		return reasonConnectIn
	}
	if up := r.Header.Get("Upgrade"); up != "" && !strings.EqualFold(up, "websocket") {
		return reasonUpgrade
	}
	if slices.ContainsFunc(h.rs, func(x rule) bool { return x.req.methods != "" }) {
		if _, odd := overrides(r); odd {
			return reasonOverride
		}
	}
	return ""
}

// Allowlist is a set of policies as a VM's proxy reads them, read once: what devvm network lock
// judges the hosts a learning VM reached with, so it leaves out what the VM's allowlist tier
// already allows, by the proxy's own rules.
type Allowlist struct{ p *policy }

// ReadAllowlist reads builtin, paths in devvm's own files own, then files, whose every rule
// applies, and denies, whose denies alone apply, as Config's BuiltinPolicies, Policies and
// DenyPolicies have them. A built-in policy that's missing or holds a line that isn't a rule is
// an error, as it is for the proxy.
func ReadAllowlist(own fs.FS, builtin, files, denies []string) (Allowlist, error) {
	l := log.New(io.Discard, "", 0)
	rs, err := readBuiltins(own, builtin, l)
	if err != nil {
		return Allowlist{}, err
	}
	return Allowlist{newPolicy(rs, files, denies, l)}, nil
}

// Allows reports whether the policies allow host:port, as the proxy decides it by its rules,
// before the floor of internal addresses: a deny beats any allow, and while a file has a broken
// deny nothing is allowed.
func (p Allowlist) Allows(host string, port int) bool {
	ok, _, _ := p.p.verdict(host, port)
	return ok
}

// CoversWhole reports, for rule, an allow that narrows (NarrowRule's), whether an allow that
// doesn't narrow already covers every host it covers on each of HTTPS's port 443 and plain HTTP's
// 80 that it covers, so it narrows nothing (nothing), as allows add up and that one lets every
// request through; and whether that's so on 443 alone, so it narrows only port 80's plain HTTP
// (httpOnly). A host name's is covered by a rule that matches it, and *.name's by ** or by *.name
// or *.parent.
func (p Allowlist) CoversWhole(rule string) (nothing, httpOnly bool) {
	n, ok := parseRule(rule)
	on443, on80 := n.portOK(443), n.portOK(80)
	if !ok || n.deny || !n.narrows() || !on443 && !on80 {
		return false, false
	}
	w443, w80 := on443 && p.coversOn(n, 443), on80 && p.coversOn(n, 80)
	return (!on443 || w443) && (!on80 || w80), w443 && on80 && !w80
}

// coversOn is CoversWhole for one port.
func (p Allowlist) coversOn(n rule, port int) bool {
	if p.p.open {
		return true
	}
	rs, broken := p.p.all()
	if broken != "" {
		return false
	}
	for _, r := range rs {
		if r.deny || r.narrows() || !r.portOK(port) || r.host == "" {
			continue
		}
		if d, wild := strings.CutPrefix(n.host, "*."); !wild && r.matchHost(n.host) ||
			wild && (r.host == "**" || strings.HasPrefix(r.host, "*.") && (d == r.host[2:] || strings.HasSuffix(d, r.host[1:]))) {
			return true
		}
	}
	return false
}

// Quiet reports whether a built-in policy marks host:port quiet (rule.quiet): a host devvm leaves
// out on purpose, which devvm network lock never offers.
func (p Allowlist) Quiet(host string, port int) bool { return p.p.quiet(host, port) }

// Denies reports whether the policies deny host:port, so that no allow would lift it: a deny rule
// matches it, or a file has a broken deny.
func (p Allowlist) Denies(host string, port int) bool {
	ok, why, refused := p.p.verdict(host, port)
	return !ok && (refused || strings.HasPrefix(why, deniedBy))
}

// ExactRule is the rule that allows host on port alone, as devvm network lock adds it: host:port, or
// [address]:port for an IPv6 address, the port always written, so 443 doesn't allow 80 too. false
// for a host the policy doesn't take (HostOK) or a port that isn't one.
func ExactRule(host string, port int) (string, bool) {
	if !HostOK(host) || port < 1 || port > 65535 {
		return "", false
	}
	return net.JoinHostPort(strings.Trim(host, "[]"), strconv.Itoa(port)), true
}

// transparentPorts are the ports a VM's transparent mode redirects (/transparent/ports): 80 and
// 443, then each port an allow rule names for hosts by name (** included), ascending, at most
// transparentPortsMax of them, and those past it, dropped. Not :*, every port, nor an address
// rule's, which no name reaches, nor 22, whose SSH names no host in its first bytes, nor 53, the
// VM's own names'. None besides 80 and 443 on an open policy, or while a deny is broken.
func (p *policy) transparentPorts() (ports, dropped []int) {
	ports = []int{80, 443}
	if p.open {
		return ports, nil
	}
	rs, broken := p.all()
	if broken != "" {
		return ports, nil
	}
	var named []int
	for _, r := range rs {
		if r.deny || r.host == "" || r.port <= 0 || slices.Contains([]int{22, 53, 80, 443}, r.port) {
			continue
		}
		if !slices.Contains(named, r.port) {
			named = append(named, r.port)
		}
	}
	slices.Sort(named)
	if len(named) > transparentPortsMax {
		named, dropped = named[:transparentPortsMax], named[transparentPortsMax:]
	}
	return append(ports, named...), dropped
}

// byName: an allow rule that names host, as itself or under *.domain, for port: one that's more
// than **, which allows any host. None while a deny is broken.
func (p *policy) byName(host string, port int) bool {
	if p.open {
		return true
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	rs, broken := p.all()
	if broken != "" {
		return false
	}
	for _, r := range rs {
		if !r.deny && r.host != "**" && r.portOK(port) && r.matchHost(host) {
			return true
		}
	}
	return false
}

// addrAllowed: an explicit address rule for a (needed to pass the floor), and no deny for it.
func (p *policy) addrAllowed(a netip.Addr, port int) bool {
	if p.open {
		return true
	}
	if p.addrDenied(a, port) {
		return false
	}
	rs, broken := p.all()
	if broken != "" {
		return false
	}
	for _, r := range rs {
		if !r.deny && r.portOK(port) && r.matchAddr(a) {
			return true
		}
	}
	return false
}

// addrDenied: a deny rule for address a, or a broken deny (every address). A deny beats any
// allow, including a host rule whose name resolves to a, so every address the proxy dials is
// checked against it.
func (p *policy) addrDenied(a netip.Addr, port int) bool {
	if p.open {
		return false
	}
	rs, broken := p.all()
	if broken != "" {
		return true
	}
	for _, r := range rs {
		if r.deny && r.portOK(port) && r.matchAddr(a) {
			return true
		}
	}
	return false
}
