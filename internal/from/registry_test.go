package from

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/fetch"
)

func TestIsDigest(t *testing.T) {
	hex64 := strings.Repeat("0123456789abcdef", 4)
	for s, want := range map[string]bool{
		"sha256:" + hex64:                  true,
		"sha256:" + hex64[:63]:             false,
		"sha256:" + hex64 + "0":            false,
		"sha256:" + strings.ToUpper(hex64): false,
		"sha512:" + hex64:                  false,
		hex64:                              false,
		"sha256:" + hex64 + "\n":           false,
		"":                                 false,
	} {
		if got := IsDigest(s); got != want {
			t.Errorf("IsDigest(%q) = %v", s, got)
		}
	}
}

func TestParseRef(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ in, host, repo, tag string }{
		{"node:24", DockerHub, "library/node", "24"},
		{"alpine", DockerHub, "library/alpine", "latest"},
		{"docker.io/library/debian:13", DockerHub, "library/debian", "13"},
		{"index.docker.io/library/debian", DockerHub, "library/debian", "latest"},
		{"bitnami/redis:7", DockerHub, "bitnami/redis", "7"},
		{"ghcr.io/astral-sh/uv:0.9", "ghcr.io", "astral-sh/uv", "0.9"},
		{"quay.io/org/sub/img", "quay.io", "org/sub/img", "latest"},
		{"localhost:5000/app:dev", "localhost:5000", "app", "dev"},
		{"localhost/app", "localhost", "app", "latest"},
		{"node:24@sha256:" + strings.Repeat("a", 64), DockerHub, "library/node", "24"},
		{"ghcr.io/o/r@sha256:" + strings.Repeat("a", 64), "ghcr.io", "o/r", "latest"},
	} {
		if got := ParseRef(c.in); got != (Ref{c.host, c.repo, c.tag}) {
			t.Errorf("ParseRef(%q) = %+v, want %s %s %s", c.in, got, c.host, c.repo, c.tag)
		}
	}
}

// FuzzParseRef: any reference parses, into a tag without / or :, and a
// valid one parses the same once written out in full.
func FuzzParseRef(f *testing.F) {
	for _, s := range []string{"node:24", "alpine", "ghcr.io/o/r:1", "localhost:5000/app:dev", "docker.io/x", "a@b", "node:", "/x", ":"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		r := ParseRef(s)
		if r.Host == "" || strings.ContainsAny(r.Tag, "/:") || strings.Contains(r.Repo, "@") {
			t.Fatalf("ParseRef(%q) = %+v", s, r)
		}
		if !Valid(s) {
			return
		}
		if again := ParseRef(r.Host + "/" + r.Repo + ":" + r.Tag); again != r {
			t.Fatalf("ParseRef(%q) = %+v, but its full form parses as %+v", s, r, again)
		}
	})
}

func TestName(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"node:24":              "node",
		"node":                 "node",
		"ghcr.io/o/r:1":        "ghcr.io/o/r",
		"localhost:5000/app":   "localhost:5000/app",
		"localhost:5000/a:dev": "localhost:5000/a",
		"node:24@sha256:aa":    "node",
	} {
		if got := Name(in); got != want {
			t.Errorf("Name(%q) = %q, want %q", in, got, want)
		}
	}
}

// fakeRegistry is a registry (and its token service) over TLS: every
// manifest needs a token first. Manifests maps repo:tag to a digest.
type fakeRegistry struct {
	srv       *httptest.Server
	mu        sync.Mutex
	manifests map[string]string
	realm     string // the token service's address; "" for its own /token
	calls     []string
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	t.Helper()
	f := &fakeRegistry{manifests: map[string]string{}}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRegistry) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := r.Method + " " + r.URL.Path
	if r.URL.Path == "/token" {
		call += "?" + r.URL.RawQuery
	}
	if a := r.Header.Get("Authorization"); a != "" {
		call += " (" + a + ")"
	}
	f.calls = append(f.calls, call)
	if r.URL.Path == "/token" {
		_, _ = w.Write([]byte(`{"token":"stub-token","expires_in":300}`))
		return
	}
	repoTag, ok := strings.CutPrefix(r.URL.Path, "/v2/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	repoTag = strings.Replace(repoTag, "/manifests/", ":", 1)
	if r.Header.Get("Authorization") != "Bearer stub-token" {
		realm := f.realm
		if realm == "" {
			realm = f.srv.URL + "/token"
		}
		repo, _, _ := strings.Cut(repoTag, ":")
		w.Header().Set("Www-Authenticate", `Bearer realm="`+realm+`",service="registry.test",scope="repository:`+repo+`:pull"`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if !strings.Contains(r.Header.Get("Accept"), "application/vnd.oci.image.index.v1+json") {
		w.WriteHeader(http.StatusNotAcceptable)
		return
	}
	d, ok := f.manifests[repoTag]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Docker-Content-Digest", d)
}

// NewPublicRegistry's never connects to an address that isn't the
// internet's, by address or by a name that resolves to one: the image
// isn't public.
func TestPublicRegistryReachesOnlyTheInternet(t *testing.T) {
	t.Parallel()
	f := newFakeRegistry(t)
	f.set("library/node:24", "sha256:"+strings.Repeat("a", 64))
	u, err := url.Parse(f.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	for _, base := range []string{f.srv.URL, "https://localhost:" + u.Port()} {
		r := NewPublicRegistry(nil)
		r.Base = func(string) string { return base }
		if _, err := r.Digest(t.Context(), "node:24"); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v", base, err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) != 0 {
		t.Errorf("reached: %q", f.calls)
	}
}

// A registry goes through the proxy its caller picks (the company
// proxy), which NewPublicRegistry's reaches even on this machine.
func TestRegistryThroughProxy(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var tunnels []string
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		tunnels = append(tunnels, r.Method+" "+r.Host)
		mu.Unlock()
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(proxySrv.Close)
	at, err := url.Parse(proxySrv.URL)
	if err != nil {
		t.Fatal(err)
	}
	pick := func(*http.Request) (*url.URL, error) { return at, nil }
	for _, r := range []*Registry{NewRegistry(pick), NewPublicRegistry(pick)} {
		r.Base = func(string) string { return "https://registry.example" }
		r.RetryDelay = time.Millisecond
		if _, err := r.Digest(t.Context(), "node:24"); err == nil {
			t.Error("a proxy that refuses gave a digest")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(tunnels) < 2 || slices.ContainsFunc(tunnels, func(s string) bool { return s != "CONNECT registry.example:443" }) {
		t.Errorf("the proxy got %q", tunnels)
	}
}

// A registry's address is judged as the proxy judges any (proxy.Public): a NAT64 or 6to4 form of
// a private address, or a range of IPv6's that isn't the internet, is no registry's either.
func TestPublicAddr(t *testing.T) {
	t.Parallel()
	for s, want := range map[string]bool{
		"1.1.1.1": true, "140.82.112.3": true, "2606:4700::1111": true,
		"127.0.0.1": false, "::1": false, "10.1.2.3": false, "172.16.0.1": false, "192.168.5.2": false,
		"169.254.169.254": false, "fe80::1": false, "fd00::1": false, "100.96.0.1": false, "0.0.0.0": false,
		"0.1.2.3": false, "::": false, "::ffff:127.0.0.1": false, "::ffff:10.0.0.1": false, "224.0.0.1": false,
		"198.18.0.1": false, "255.255.255.255": false,
		"64:ff9b::10.0.0.1": false, "2002:a00:1::1": false, "fec0::1": false, "2001:db8::1": false,
	} {
		if got := publicHost(s); got != want {
			t.Errorf("publicHost(%s) = %v", s, got)
		}
		if err := publicOnly("tcp", netip.AddrPortFrom(netip.MustParseAddr(s), 443).String(), nil); (err == nil) != want {
			t.Errorf("publicOnly(%s) = %v", s, err)
		}
	}
}

func TestProxyAddr(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"http://10.0.0.1": "10.0.0.1:80", "https://proxy.corp:8443": "proxy.corp:8443",
		"socks5://[::1]": "[::1]:1080", "http://u:p@127.0.0.1:3128": "127.0.0.1:3128",
	} {
		u, err := url.Parse(in)
		if err != nil {
			t.Fatal(err)
		}
		if got := proxyAddr(u); got != want {
			t.Errorf("proxyAddr(%s) = %s, want %s", in, got, want)
		}
	}
}

func (f *fakeRegistry) registry() *Registry { return registryAt(f.srv) }

// registryAt is a Registry reaching only srv, a TLS test server whose
// certificate its client trusts, trying again at once.
func registryAt(srv *httptest.Server) *Registry {
	return &Registry{Client: trusting(srv), Base: func(string) string { return srv.URL }}
}

// trusting is NewRegistry's client, trusting srv's certificate.
func trusting(srv *httptest.Server) *fetch.Client {
	c := NewRegistry(nil).Client
	if t, ok := srv.Client().Transport.(*http.Transport); ok {
		c.Transport().TLSClientConfig = t.TLSClientConfig
	}
	return c
}

func (f *fakeRegistry) set(repoTag, digest string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.manifests[repoTag] = digest
}

func digestOf(c string) string { return "sha256:" + strings.Repeat(c, 64) }

func TestDigestTokenThenManifest(t *testing.T) {
	t.Parallel()
	f := newFakeRegistry(t)
	f.set("library/node:24", digestOf("a"))
	d, err := f.registry().Digest(t.Context(), "node:24")
	if err != nil || d != digestOf("a") {
		t.Fatalf("Digest = %q, %v", d, err)
	}
	want := []string{
		"HEAD /v2/library/node/manifests/24",
		"GET /token?scope=repository%3Alibrary%2Fnode%3Apull&service=registry.test",
		"HEAD /v2/library/node/manifests/24 (Bearer stub-token)",
	}
	if strings.Join(f.calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(f.calls, "\n"), strings.Join(want, "\n"))
	}
}

func TestDigestNotFoundAndUnreachable(t *testing.T) {
	t.Parallel()
	f := newFakeRegistry(t)
	if _, err := f.registry().Digest(t.Context(), "ghcr.io/someone/private:1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a missing image: %v", err)
	}
	// A digest that isn't one is no digest.
	f.set("library/node:24", "sha256:short")
	if _, err := f.registry().Digest(t.Context(), "node:24"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a bad digest: %v", err)
	}
	// Nothing listening: unreachable, after retries.
	down := httptest.NewTLSServer(http.NotFoundHandler())
	r := registryAt(down)
	down.Close()
	if _, err := r.Digest(t.Context(), "node:24"); !errors.Is(err, ErrUnreachable) {
		t.Errorf("no registry: %v", err)
	}
	// One that keeps failing is unreachable too.
	failing := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer failing.Close()
	r = registryAt(failing)
	if _, err := r.Digest(t.Context(), "node:24"); !errors.Is(err, ErrUnreachable) {
		t.Errorf("a failing registry: %v", err)
	}
	// A cancelled lookup says so.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := f.registry().Digest(ctx, "node:24"); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
}

// A failure that may pass is tried again; a refusal (4xx) isn't, and
// neither is a redirect to plain http, which NewClient refuses.
func TestDigestRetries(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var heads, tokens int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if strings.HasPrefix(r.URL.Path, "/redirect/") {
			http.Redirect(w, r, strings.TrimPrefix(r.URL.Path, "/redirect"), http.StatusFound) //nolint:gosec // the test's own server
			return
		}
		heads++
		if heads < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Docker-Content-Digest", digestOf("b"))
	}))
	t.Cleanup(srv.Close)
	r := &Registry{Client: NewRegistry(nil).Client, Base: func(string) string { return srv.URL }}
	if d, err := r.Digest(t.Context(), "node:24"); err != nil || d != digestOf("b") || heads != 3 {
		t.Errorf("after two 503s: %q, %v, %d requests", d, err, heads)
	}
	r.Base = func(string) string { return srv.URL + "/redirect" }
	heads = 0
	if _, err := r.Digest(t.Context(), "node:24"); !errors.Is(err, ErrNotFound) || heads != 0 {
		t.Errorf("a redirect to http: %v, %d requests", err, heads)
	}
	// A token service's 403 is asked once.
	f := newFakeRegistry(t)
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		tokens++
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(tls.Close)
	f.realm = tls.URL + "/token"
	reg := f.registry()
	reg.Client = trusting(tls)
	if _, err := reg.Digest(t.Context(), "node:24"); !errors.Is(err, ErrNotFound) || tokens != 1 {
		t.Errorf("a 403 for the token: %v, %d requests", err, tokens)
	}
}

// A registry can't point devvm's token request at a host file or a plain
// http address: the realm must be https.
func TestDigestRealmMustBeHTTPS(t *testing.T) {
	t.Parallel()
	secret := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(secret, []byte(`{"access_token":"HOST-SECRET"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"token":"stub-token"}`))
	}))
	defer plain.Close()
	f := newFakeRegistry(t)
	f.set("library/node:24", digestOf("a"))
	for _, realm := range []string{(&url.URL{Scheme: "file", Path: secret}).String(), plain.URL + "/token", "/token"} {
		f.realm = realm
		f.calls = nil
		if _, err := f.registry().Digest(t.Context(), "node:24"); !errors.Is(err, ErrNotFound) {
			t.Errorf("realm %s: %v", realm, err)
		}
		if strings.Contains(strings.Join(f.calls, "\n"), "Bearer") {
			t.Errorf("realm %s: a token was used: %q", realm, f.calls)
		}
	}
}
