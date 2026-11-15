package agentlogin

import (
	"net/netip"
	"net/url"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/proxy"
)

// legacyClaudeHost is claudeHost as it was before proxy.NormHost read its host, frozen for
// FuzzClaudeHostSite.
func legacyClaudeHost(s map[string]any) string {
	u, err := url.Parse(str(s, "url"))
	if err != nil || u.Scheme != "https" || (u.Port() != "" && u.Port() != "443") {
		return ""
	}
	h := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if _, err := netip.ParseAddr(h); err != nil && proxy.HostOK(h) && strings.Contains(h, ".") {
		return h
	}
	return ""
}

// FuzzClaudeHostSite: an MCP server's host, which goes into the VM's allowlist, reads as it did.
// Divergences: a host with anything but printable ASCII gets no binding, where strings.ToLower
// folded it into ASCII first (the Kelvin sign's kubernetes.io was bound as kubernetes.io), as the
// proxy refuses such a host before it folds it. A host that's an address with a stray bracket
// (https://1.2.3.4]/, whose hostname Go's URL parser gives as 1.2.3.4]) is an address, so it gets
// no binding, where HostOK trimmed the bracket and took it. Every host it binds, it bound.
func FuzzClaudeHostSite(f *testing.F) {
	for _, s := range []string{
		"https://mcp.example.com/mcp", "https://MCP.Example.com./mcp", "https://mcp.example.com:443/",
		"https://mcp.example.com:8443/", "http://mcp.example.com/", "https://Kubernetes.io/",
		"https://%4Bubernetes.io/", "https://[::1]/", "https://1.2.3.4/", "https://mcp/", "https://a..b/", "https://1.2.3.4]/",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		s := map[string]any{"url": raw}
		got, was := claudeHost(s), legacyClaudeHost(s)
		u, _ := url.Parse(raw)
		notASCII := u != nil && strings.ContainsFunc(u.Hostname(), func(r rune) bool { return r <= ' ' || r > '~' })
		_, strayAddr := netip.ParseAddr(strings.Trim(was, "[]"))
		if got != was && (got != "" || !notASCII && strayAddr != nil) {
			t.Fatalf("claudeHost(%q) = %q, was %q", raw, got, was)
		}
	})
}
