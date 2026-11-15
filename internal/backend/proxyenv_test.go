package backend

import (
	"regexp"
	"slices"
	"testing"

	"github.com/devvmhq/devvm/internal/redact"
)

// A proxy's address is devvm's user and the password at the host and port
// the guest reaches, an IPv6 host in brackets, a password of anything but
// URL-safe characters escaped.
func TestProxyURL(t *testing.T) {
	for _, tc := range []struct {
		host     string
		port     int
		password string
		want     string
	}{
		{"192.168.5.2", 40123, "0123abcd", "http://devvm:0123abcd@192.168.5.2:40123"},
		{"devvm-proxy", 3128, "k3y", "http://devvm:k3y@devvm-proxy:3128"},
		{"100.96.0.1", 1, "pw", "http://devvm:pw@100.96.0.1:1"},
		{"::1", 8080, "pw", "http://devvm:pw@[::1]:8080"},
		{"h", 2, "a b@c:d/e", "http://devvm:a%20b%40c%3Ad%2Fe@h:2"},
	} {
		if got := ProxyURL(tc.host, tc.port, redact.NewSecret(tc.password)); got != tc.want {
			t.Errorf("ProxyURL(%q, %d, %q) = %q, want %q", tc.host, tc.port, tc.password, got, tc.want)
		}
	}
}

// Each password is new: 64 hex digits.
func TestNewProxyPassword(t *testing.T) {
	hex64 := regexp.MustCompile(`^[0-9a-f]{64}$`)
	seen := map[string]bool{}
	for range 20 {
		s, err := NewProxyPassword()
		p := s.Reveal()
		if err != nil || !hex64.MatchString(p) || seen[p] {
			t.Fatalf("NewProxyPassword = %q, %v (seen: %v)", p, err, seen[p])
		}
		seen[p] = true
	}
}

// The guest's variables name the proxy for HTTP and HTTPS; the clients'
// are every proxy variable, in capitals and lowercase, and each list is
// the caller's own to change.
func TestProxyVars(t *testing.T) {
	if got, want := GuestProxyVars(), []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"}; !slices.Equal(got, want) {
		t.Errorf("GuestProxyVars = %q", got)
	}
	want := []string{
		"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "FTP_PROXY", "ftp_proxy",
		"NO_PROXY", "no_proxy", "ALL_PROXY", "all_proxy",
	}
	if got := ClientProxyVars(); !slices.Equal(got, want) {
		t.Errorf("ClientProxyVars = %q", got)
	}
	for _, k := range GuestProxyVars() {
		if !slices.Contains(ClientProxyVars(), k) {
			t.Errorf("%s names the proxy in a guest, and isn't a client's", k)
		}
	}
	g := GuestProxyVars()
	g[0] = "X"
	if GuestProxyVars()[0] != "HTTP_PROXY" {
		t.Error("GuestProxyVars is shared")
	}
}
