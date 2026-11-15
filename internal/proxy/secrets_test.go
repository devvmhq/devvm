package proxy

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/safefile"
)

// writeAtomic replaces a file whole with the mode asked for, a file there
// with another mode included, and leaves nothing else; one it can't write
// leaves nothing either.
func TestWriteAtomic(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "ca.crt")
	for _, tc := range []struct {
		data string
		mode os.FileMode
	}{{"one\n", 0o644}, {"two\n", 0o600}, {"three\n", 0o644}} {
		if err := writeAtomic(path, []byte(tc.data), tc.mode); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(path) //nolint:gosec // the test's own file
		fi, serr := os.Stat(path)
		if err != nil || serr != nil || string(b) != tc.data || fi.Mode().Perm() != tc.mode {
			t.Errorf("%q, %v: %v, %v", b, fi.Mode(), err, serr)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "f"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(filepath.Join(dir, "sub"), []byte("x"), 0o600); err == nil {
		t.Error("a folder replaced")
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 2 {
		t.Errorf("left %v", ents)
	}
}

// writeBindings writes the bindings file at path as devvm does.
func writeBindings(t *testing.T, path string, bs ...Binding) {
	t.Helper()
	must(t, os.WriteFile(path, BindingList{Bindings: bs}.Encode(), 0o600))
}

// bound is the secret t bound to hosts, in Authorization, for the placeholder PH.
func bound(hosts ...string) Binding {
	return Binding{Name: "t", Kind: KindSecret, Hosts: hosts, Header: "Authorization", Placeholder: "PH"}
}

// fakeSecret makes every lookup of s give value, with nothing cached yet, and returns the bindings
// looked up, in order.
func fakeSecret(s *secrets, value string) *[]Binding {
	var asked []Binding
	s.valueMu.Lock()
	defer s.valueMu.Unlock()
	s.lookup = func(_ context.Context, b Binding) (string, error) { asked = append(asked, b); return value, nil }
	s.values = map[valueKey]cached{}
	return &asked
}

func TestInject(t *testing.T) {
	t.Parallel()
	s := newSecrets("", nil, quiet())
	fakeSecret(s, "real")
	bs := []Binding{{Name: "github", Kind: KindSecret, Header: "Authorization", Placeholder: "PH"}}
	h := http.Header{}
	h.Set("Authorization", "token PH")
	h.Set("X-Echo", "PH")
	if used := s.inject(t.Context(), h, bs); len(used) != 1 || used[0] != "github" {
		t.Errorf("used = %v", used)
	}
	if h.Get("Authorization") != "token real" || h.Get("X-Echo") != "PH" {
		t.Errorf("headers = %v (only the binding's header may change)", h)
	}
	h = http.Header{}
	h.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("x-access-token:PH")))
	s.inject(t.Context(), h, bs)
	if raw, _ := basic(h.Get("Authorization")); raw != "x-access-token:real" {
		t.Errorf("basic = %q", raw)
	}
	h = http.Header{}
	h.Set("Authorization", "Bearer something-else")
	if used := s.inject(t.Context(), h, bs); len(used) != 0 || h.Get("Authorization") != "Bearer something-else" {
		t.Error("a header without the placeholder changed")
	}
	// Two values that hold the placeholder, which a server may join into one: neither changes.
	h = http.Header{}
	h.Add("Authorization", "xxxx PH yyyy")
	h.Add("Authorization", "token PH")
	if used := s.inject(t.Context(), h, bs); len(used) != 0 || !reflect.DeepEqual(h.Values("Authorization"), []string{"xxxx PH yyyy", "token PH"}) {
		t.Errorf("a secret's values = %q, used %v", h.Values("Authorization"), used)
	}
}

// A secret's value goes in only in the shapes that put it where a server reads a credential:
// the placeholder alone, Bearer, Token or the binding's own scheme before it in Authorization,
// the password of Basic credentials, and the username only with BasicUser. Every other shape,
// each of which a server could echo (a username it doesn't know, a framed or doubled value, a
// Digest field), keeps the placeholder.
func TestInjectSecretShapes(t *testing.T) {
	t.Parallel()
	enc := func(s string) string { return "Basic " + base64.StdEncoding.EncodeToString([]byte(s)) }
	auth := Binding{Name: "s", Kind: KindSecret, Header: "Authorization", Placeholder: "PH"}
	apiKey := auth
	apiKey.Scheme = "ApiKey"
	user := auth
	user.BasicUser = true
	plain := Binding{Name: "s", Kind: KindSecret, Header: "X-Api-Key", Placeholder: "PH"}
	long := strings.Repeat("u", basicPartMax+1)
	for _, tc := range []struct {
		b         Binding
		vals      []string
		want      []string // nil: unchanged
		basicWant string   // the Basic credentials put in, decoded
	}{
		// Allowed, and swapped.
		{auth, []string{"PH"}, []string{"real"}, ""},
		{auth, []string{"Bearer PH"}, []string{"Bearer real"}, ""},
		{auth, []string{"bearer PH"}, []string{"bearer real"}, ""},
		{auth, []string{"token PH"}, []string{"token real"}, ""},
		{auth, []string{"Token PH"}, []string{"Token real"}, ""},
		{apiKey, []string{"ApiKey PH"}, []string{"ApiKey real"}, ""},
		{apiKey, []string{"apikey PH"}, []string{"apikey real"}, ""},
		{auth, []string{enc("x-access-token:PH")}, nil, "x-access-token:real"},
		{auth, []string{enc("api:PH")}, nil, "api:real"},
		{auth, []string{enc(":PH")}, nil, ":real"},
		{user, []string{enc("PH:")}, nil, "real:"},
		{user, []string{enc("PH:x")}, nil, "real:x"},
		{user, []string{enc("PH:a:b")}, nil, "real:a:b"},
		{user, []string{enc("api:PH")}, nil, "api:real"},
		{plain, []string{"PH"}, []string{"real"}, ""},
		{auth, []string{"PH", "Other x"}, []string{"real", "Other x"}, ""},
		// Refused: the placeholder stays.
		{auth, []string{enc("PH:x")}, nil, ""},                 // the username, which Elasticsearch echoes
		{auth, []string{enc("PH:")}, nil, ""},                  // the username alone
		{auth, []string{"Basic PH"}, nil, ""},                  // raw Basic
		{auth, []string{`Digest username="PH"`}, nil, ""},      // a Digest field
		{auth, []string{"Bearer aaaaPHbbbb"}, nil, ""},         // framed
		{auth, []string{"Bearer  PH"}, nil, ""},                // two spaces
		{auth, []string{"Bearer PH "}, nil, ""},                // a byte after it
		{auth, []string{"PH,PH"}, nil, ""},                     // doubled in one value
		{auth, []string{"PH PH"}, nil, ""},                     // doubled, a space between
		{auth, []string{"PH", "PH"}, nil, ""},                  // in two values
		{auth, []string{"PH", "Bearer PH"}, nil, ""},           // in two values, both fine alone
		{auth, []string{"ApiKey PH"}, nil, ""},                 // a scheme the binding doesn't have
		{auth, []string{"Negotiate PH"}, nil, ""},              // nor this one
		{apiKey, []string{"Basic PH"}, nil, ""},                // Basic is never a scheme of its own
		{auth, []string{enc("PH" + "x:PH")}, nil, ""},          // the username holds it too
		{auth, []string{enc(long + ":PH")}, nil, ""},           // a username past basicPartMax
		{auth, []string{enc("a\x01b:PH")}, nil, ""},            // a control character in it
		{auth, []string{enc("é:PH")}, nil, ""},                 // not ASCII
		{user, []string{enc("PH:" + long)}, nil, ""},           // a password past basicPartMax
		{user, []string{enc("PH:PH")}, nil, ""},                // both
		{auth, []string{enc("x:PH") + "="}, nil, ""},           // not canonical base64
		{auth, []string{"Basic  " + enc("x:PH")[6:]}, nil, ""}, // two spaces
		{plain, []string{"Bearer PH"}, nil, ""},                // a scheme outside Authorization
		{plain, []string{enc("x:PH")}, nil, ""},                // Basic outside Authorization
		{plain, []string{"xPH"}, nil, ""},
	} {
		s := newSecrets("", nil, quiet())
		fakeSecret(s, "real")
		h := http.Header{}
		for _, v := range tc.vals {
			h.Add(tc.b.Header, v)
		}
		used := s.inject(t.Context(), h, []Binding{tc.b})
		got := h.Values(tc.b.Header)
		switch {
		case tc.basicWant != "":
			raw, _ := basic(got[0])
			if len(used) != 1 || raw != tc.basicWant {
				t.Errorf("%+v %q: Basic %q, used %v, want %q", tc.b, tc.vals, raw, used, tc.basicWant)
			}
		case tc.want != nil:
			if len(used) != 1 || !reflect.DeepEqual(got, tc.want) {
				t.Errorf("%+v %q: %q, used %v, want %q", tc.b, tc.vals, got, used, tc.want)
			}
		case len(used) != 0 || !reflect.DeepEqual(got, tc.vals):
			t.Errorf("%+v %q: %q, used %v: want it refused, unchanged", tc.b, tc.vals, got, used)
		}
	}
}

// A secret's own scheme: a token of its own, never Basic, Digest, Bearer or Token.
func TestSchemeOK(t *testing.T) {
	t.Parallel()
	for s, ok := range map[string]bool{
		"ApiKey": true, "SSWS": true, "Klaviyo-API-Key": true, "GenieKey": true,
		"": false, "Basic": false, "basic": false, "Digest": false, "Bearer": false, "token": false,
		"Api Key": false, "Api:Key": false, strings.Repeat("a", 33): false,
	} {
		if SchemeOK(s) != ok {
			t.Errorf("SchemeOK(%q) = %v", s, !ok)
		}
	}
}

// A login's value goes in only when its header's one value is the placeholder alone, or in
// Authorization, Bearer (in any case), one space and the placeholder: any other value, with
// bytes of the VM's around the placeholder, Basic credentials or a second value, keeps the
// placeholder, and so does every other header.
func TestInjectLoginExact(t *testing.T) {
	t.Parallel()
	enc := func(s string) string { return "Basic " + base64.StdEncoding.EncodeToString([]byte(s)) }
	for _, tc := range []struct {
		header string
		values []string
		want   []string // nil: unchanged
	}{
		{"x-api-key", []string{"PH"}, []string{"real"}},
		{"Authorization", []string{"Bearer PH"}, []string{"Bearer real"}},
		{"Authorization", []string{"bearer PH"}, []string{"bearer real"}},
		{"Authorization", []string{"BEARER PH"}, []string{"BEARER real"}},
		{"Authorization", []string{"PH"}, []string{"real"}}, // an MCP server's header, its whole value bound
		{"x-api-key", []string{"Bearer PH"}, nil},
		{"x-api-key", []string{"xxxx PH"}, nil},
		{"x-api-key", []string{"PH "}, nil},
		{"Authorization", []string{"xxxx PH yyyy"}, nil},
		{"Authorization", []string{"Bearer PH yyyy"}, nil},
		{"Authorization", []string{"Bearer xxxxPH"}, nil},
		{"Authorization", []string{"Bearer  PH"}, nil},
		{"Authorization", []string{"Bearer\tPH"}, nil},
		{"Authorization", []string{"Token PH"}, nil},
		{"Authorization", []string{"Bearer PH,Bearer PH"}, nil},
		{"Authorization", []string{enc("x-access-token:PH")}, nil},
		{"Authorization", []string{"Bearer PH", "Bearer PH"}, nil},
		{"Authorization", []string{"Bearer PH", "yyyy"}, nil},
		{"Authorization", []string{"yyyy", "Bearer PH"}, nil},
	} {
		s := newSecrets("", nil, quiet())
		asked := fakeSecret(s, "real")
		b := Binding{Name: "pi-groq", Kind: KindLogin, Agent: "pi", Header: tc.header, Placeholder: "PH"}
		h := http.Header{}
		for _, v := range tc.values {
			h.Add(tc.header, v)
		}
		h.Set("X-Echo", "PH")
		used := s.inject(t.Context(), h, []Binding{b})
		want := tc.want
		if want == nil {
			want = tc.values
		}
		if got := h.Values(tc.header); !reflect.DeepEqual(got, want) || h.Get("X-Echo") != "PH" {
			t.Errorf("%s %q: got %q, X-Echo %q; want %q", tc.header, tc.values, got, h.Get("X-Echo"), want)
		}
		if put := tc.want != nil; (len(used) == 1) != put || (len(*asked) == 1) != put {
			t.Errorf("%s %q: used %v, looked up %d times", tc.header, tc.values, used, len(*asked))
		}
	}
}

// A login bound in two headers, as OpenRouter's key is (Authorization and x-api-key, one name and
// placeholder), gets its value in either, each by the exact rule, and is named once when both
// take it; another login's placeholder in either header stays as it is.
func TestInjectLoginTwoHeaders(t *testing.T) {
	t.Parallel()
	or := func(header string) Binding {
		return Binding{Name: "pi-openrouter", Kind: KindLogin, Agent: "pi", Hosts: []string{"openrouter.ai"}, Path: "/api/", Header: header, Placeholder: "PH"}
	}
	bs := []Binding{or("Authorization"), or("x-api-key")}
	for _, tc := range []struct {
		auth, key         string
		wantAuth, wantKey string
		used              int
	}{
		{"Bearer PH", "", "Bearer real", "", 1},
		{"", "PH", "", "real", 1},
		{"Bearer PH", "PH", "Bearer real", "real", 1},
		{"xxxx PH", "PH", "xxxx PH", "real", 1},
		{"Bearer PH", "xxxx PH", "Bearer real", "xxxx PH", 1},
		{"", "Bearer PH", "", "Bearer PH", 0},
		{"Bearer OTHER", "OTHER", "Bearer OTHER", "OTHER", 0},
		{"", "devvm-proxy-managed-pi-anthropic-s1", "", "devvm-proxy-managed-pi-anthropic-s1", 0},
	} {
		s := newSecrets("", nil, quiet())
		fakeSecret(s, "real")
		h := http.Header{}
		if tc.auth != "" {
			h.Set("Authorization", tc.auth)
		}
		if tc.key != "" {
			h.Set("X-Api-Key", tc.key)
		}
		used := s.inject(t.Context(), h, bs)
		if h.Get("Authorization") != tc.wantAuth || h.Get("X-Api-Key") != tc.wantKey || len(used) != tc.used {
			t.Errorf("%q, %q: got %q, %q, used %v", tc.auth, tc.key, h.Get("Authorization"), h.Get("X-Api-Key"), used)
		}
	}
}

// A path's match takes a request's path only in its clean form, unencoded, below /api/: no dot
// segment, doubled slash, encoded slash, dot or backslash, backslash or ; can take it elsewhere.
// A rule's allow and a secret's binding are judged by this one matcher (match.allows).
func TestPathWithin(t *testing.T) {
	t.Parallel()
	within := func(u *url.URL, p string) bool {
		return newMatch("", p).allows(&http.Request{Method: http.MethodGet, URL: u, Header: http.Header{}})
	}
	for uri, want := range map[string]bool{
		"/api/v1/chat/completions":  true,
		"/api/v1/models?x=/../":     true,
		"/api/":                     true,
		"/api":                      false,
		"/apix/v1":                  false,
		"/API/v1":                   false,
		"/v1/keys":                  false,
		"/":                         false,
		"":                          false,
		"/api/../v1/keys":           false,
		"/api/./v1":                 false,
		"/api/v1/..":                false,
		"/api//v1":                  false,
		"//api/v1":                  false,
		"/x/../api/v1":              false,
		"/api%2F..%2Fsettings":      false,
		"/api/%2e%2e/settings":      false,
		"/api/..%2fsettings":        false,
		"/api/v1%2Fx":               false,
		"/%61pi/v1":                 false,
		"/api/%252e%252e/settings":  false,
		`/api/..\settings`:          false,
		"/api/%5C..%5Csettings":     false,
		"/api/..;/settings":         false,
		"/api/v1;x":                 false,
		"https://openrouter.ai/api": false,
	} {
		u, err := url.ParseRequestURI(uri)
		if uri == "" {
			u, err = &url.URL{}, nil
		}
		if err != nil {
			t.Errorf("%q: %v", uri, err)
			continue
		}
		if got := within(u, "/api/"); got != want {
			t.Errorf("within(%q) = %v, want %v", uri, got, want)
		}
		if got := within(u, "/api/**"); got != want {
			t.Errorf("within(%q, /api/**) = %v, want %v", uri, got, want)
		}
	}
	if within(&url.URL{Opaque: "/api/v1"}, "/api/") {
		t.Error("an opaque URL was taken")
	}
	if !within(&url.URL{Scheme: "https", Host: "openrouter.ai", Path: "/api/v1/x"}, "/api/") {
		t.Error("an absolute-form request's path was refused")
	}
}

// A binding with a Path applies to a host's requests below it alone; one without, to all of them.
func TestForRequestPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "bindings.json")
	writeBindings(t, file,
		Binding{Name: "or", Kind: KindLogin, Agent: "pi", Hosts: []string{"openrouter.ai"}, Path: "/api/", Header: "Authorization", Placeholder: "PH"},
		Binding{Name: "any", Kind: KindLogin, Agent: "pi", Hosts: []string{"openrouter.ai"}, Header: "x-api-key", Placeholder: "PH2"},
	)
	s := newSecrets(file, nil, quiet())
	names := func(p string) []string {
		var out []string
		for _, b := range s.forRequest("openrouter.ai", &http.Request{Method: http.MethodPost, URL: &url.URL{Path: p}, Header: http.Header{}}) {
			out = append(out, b.Name)
		}
		return out
	}
	if got := names("/api/v1/chat/completions"); !reflect.DeepEqual(got, []string{"or", "any"}) {
		t.Errorf("below the path: %v", got)
	}
	for _, p := range []string{"/settings/keys", "/api/../settings", "/api"} {
		if got := names(p); !reflect.DeepEqual(got, []string{"any"}) {
			t.Errorf("%s: %v", p, got)
		}
	}
	if got := s.forRequest("example.com", &http.Request{Method: http.MethodGet, URL: &url.URL{Path: "/api/v1"}}); len(got) != 0 {
		t.Errorf("another host: %v", got)
	}
}

// TestLookupRoots: each lookup's context carries the proxy's own roots (WithRoots), the user's CAs
// (Config.UpstreamCAs) among them, so a login's refresh trusts what the VM's requests do; a proxy
// without them gives none, the system's.
func TestLookupRoots(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeCA(t, dir)
	data, err := os.ReadFile(filepath.Join(dir, "ca.pem")) //nolint:gosec // G304: the test's own temp folder
	must(t, err)
	block, _ := pem.Decode(data)
	cert, err := x509.ParseCertificate(block.Bytes)
	must(t, err)
	rootsOf := func(c Config) *x509.CertPool {
		var roots *x509.CertPool
		lookup := func(ctx context.Context, _ Binding) (string, error) { roots = RootsOf(ctx); return "v", nil }
		s, err := newServer(c, IO{Secret: lookup}, quiet(), io.Discard)
		must(t, err)
		_, err = s.secrets.value(t.Context(), Binding{Name: "codex", Kind: KindLogin, Agent: "codex"})
		must(t, err)
		return roots
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: rootsOf(Config{VM: "vm", UpstreamCAs: string(data)})}); err != nil {
		t.Errorf("the lookup's roots don't trust the user's CA: %v", err)
	}
	if r := rootsOf(Config{VM: "vm"}); r != nil {
		t.Errorf("no CAs: roots %v", r)
	}
}

// interceptSetup: a proxy with devvm's CA, which a temp dir holds, an upstream HTTPS server for
// "localhost" (handler h) with a certificate from it, a policy allowing it, a secret bound to it
// (placeholder PH, value "real"). It returns the proxy, the upstream's port, the bindings file, and
// a client that goes through the proxy and trusts the CA, keeping one connection open.
func interceptSetup(t *testing.T, h http.HandlerFunc) (*server, int, string, *http.Client) {
	t.Helper()
	dir := t.TempDir()
	writeCA(t, dir)
	file := filepath.Join(dir, "bindings.json")
	s := testServer(t, Config{CA: filepath.Join(dir, "ca.pem"), CAKey: filepath.Join(dir, "ca.key"), Secrets: file}, "")
	pool := x509.NewCertPool()
	pool.AddCert(s.ca.cert)
	s.upstreamCAs = pool
	up := httptest.NewUnstartedServer(h)
	cert, _ := s.ca.leafFor("localhost")
	up.TLS = &tls.Config{Certificates: []tls.Certificate{*cert}}
	up.StartTLS()
	t.Cleanup(up.Close)
	_, portStr, _ := net.SplitHostPort(up.Listener.Addr().String())
	port, err := strconv.Atoi(portStr)
	must(t, err)
	s.interceptPorts[port] = true
	s.egress.pol = writePolicy(t, fmt.Sprintf("localhost:%d\n127.0.0.1:%d\n", port, port))
	writeBindings(t, file, bound("localhost"))
	fakeSecret(s.secrets, "real")
	pu, _ := url.Parse("http://" + listen(t, s))
	return s, port, file, &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyURL(pu), TLSClientConfig: &tls.Config{RootCAs: pool}, MaxConnsPerHost: 1,
	}}
}

func get(t *testing.T, c *http.Client, url, host string) (int, string) {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer PH")
	if host != "" {
		req.Host = host
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// A secret goes only to the host it's bound to: inside an intercepted tunnel, a request whose Host
// names another site (one sharing the bound host's CDN, say) is refused, not forwarded with the token.
func TestInterceptRefusesAnotherHost(t *testing.T) {
	t.Parallel()
	_, port, _, c := interceptSetup(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s|%s", r.Host, r.Header.Get("Authorization")) //nolint:gosec // a test echo server
	})
	u := fmt.Sprintf("https://localhost:%d/x", port)
	if code, body := get(t, c, u, ""); code != 200 || body != fmt.Sprintf("localhost:%d|Bearer real", port) {
		t.Fatalf("same host: %d %q", code, body)
	}
	code, body := get(t, c, u, "other.example")
	if code != http.StatusMisdirectedRequest || strings.Contains(body, "real") {
		t.Errorf("another Host: %d %q (want 421 and no secret)", code, body)
	}
}

// Through the proxy, a login bound below a path gets its value only for a request whose path, as
// it goes upstream, is below it in its clean form, and only for a header that's Bearer and the
// placeholder alone: the upstream gets the placeholder for any other.
func TestInterceptLoginPathAndExact(t *testing.T) {
	t.Parallel()
	_, port, file, c := interceptSetup(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s|%s", r.URL.EscapedPath(), r.Header.Get("Authorization")) //nolint:gosec // a test echo server
	})
	writeBindings(t, file, Binding{
		Name: "pi-openrouter", Kind: KindLogin, Agent: "pi", Hosts: []string{"localhost"}, Path: "/api/", Header: "Authorization", Placeholder: "PH",
	})
	send := func(p, auth string) string {
		t.Helper()
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("https://localhost:%d", port), nil)
		req.URL.Opaque = p // sent as it is, unparsed
		req.Header.Set("Authorization", auth)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	for _, tc := range []struct{ path, auth, want string }{
		{"/api/v1/chat/completions", "Bearer PH", "/api/v1/chat/completions|Bearer real"},
		{"/settings/keys", "Bearer PH", "/settings/keys|Bearer PH"},
		{"/api/../settings/keys", "Bearer PH", "/api/../settings/keys|Bearer PH"},
		{"/api/%2e%2e/settings/keys", "Bearer PH", "/api/%2e%2e/settings/keys|Bearer PH"},
		{"/api%2F..%2Fsettings", "Bearer PH", "/api%2F..%2Fsettings|Bearer PH"},
		{"/api/v1/chat/completions", "xxxx PH yyyy", "/api/v1/chat/completions|xxxx PH yyyy"},
	} {
		if got := send(tc.path, tc.auth); got != tc.want {
			t.Errorf("%s %q: upstream saw %q, want %q", tc.path, tc.auth, got, tc.want)
		}
	}
}

// An open VM's proxy (Config.Open) applies no rules but puts secrets in as every tier's does: the
// bound host gets the real value, and a request in its tunnel that names another host gets 421.
func TestInterceptOpen(t *testing.T) {
	t.Parallel()
	s, port, _, c := interceptSetup(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s|%s", r.Host, r.Header.Get("Authorization")) //nolint:gosec // a test echo server
	})
	s.egress.pol = newPolicy(nil, nil, nil, quiet())
	s.egress.pol.open = true
	u := fmt.Sprintf("https://localhost:%d/x", port)
	if code, body := get(t, c, u, ""); code != 200 || body != fmt.Sprintf("localhost:%d|Bearer real", port) {
		t.Fatalf("open, the bound host: %d %q", code, body)
	}
	if code, body := get(t, c, u, "other.example"); code != http.StatusMisdirectedRequest || strings.Contains(body, "real") {
		t.Errorf("open, another Host: %d %q (want 421 and no secret)", code, body)
	}
}

// Unbinding a secret applies to tunnels already open: the next request in a kept-alive tunnel gets
// no secret.
func TestUnbindAppliesToOpenTunnels(t *testing.T) {
	t.Parallel()
	_, port, file, c := interceptSetup(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, r.Header.Get("Authorization")) //nolint:gosec // a test echo server
	})
	u := fmt.Sprintf("https://localhost:%d/x", port)
	if _, body := get(t, c, u, ""); body != "Bearer real" {
		t.Fatalf("bound: %q", body)
	}
	writeBindings(t, file)
	later := time.Now().Add(2 * time.Second)
	must(t, os.Chtimes(file, later, later))
	if _, body := get(t, c, u, ""); body != "Bearer PH" {
		t.Errorf("after unbinding, the open tunnel still got %q", body)
	}
}

// writeCA writes a CA into dir as devvm's (trust.InitCA) is: ca.pem, and its ECDSA key, ca.key,
// "EC PRIVATE KEY", mode 600. The proxy's tests can't import trust, which isn't the standard library.
func writeCA(t *testing.T, dir string) {
	t.Helper()
	writeCAFiles(t, filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca.key"), time.Now().Add(-time.Hour), time.Now().Add(30*24*time.Hour))
}

// writeCAFiles writes a CA valid from notBefore to notAfter to certPath, mode 644, and its key to
// keyPath, mode 600.
func writeCAFiles(t *testing.T, certPath, keyPath string, notBefore, notAfter time.Time) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "devvm proxy CA"},
		NotBefore: notBefore, NotAfter: notAfter,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign, BasicConstraintsValid: true, IsCA: true, MaxPathLenZero: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	must(t, err)
	kder, err := x509.MarshalECPrivateKey(key)
	must(t, err)
	must(t, os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600))
	must(t, os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)) //nolint:gosec // a certificate
}

// Values from agents' login files are cached briefly: a logout on the host stops them soon. The
// binding's kind decides, never its name.
func TestLoginValuesCacheBriefly(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		b    Binding
		want time.Duration
	}{
		{Binding{Name: "claude", Kind: KindSecret}, valueTTL},
		{Binding{Name: "github", Kind: KindSecret}, valueTTL},
		{Binding{Name: "codex", Kind: KindLogin, Agent: "codex"}, loginTTL},
		{Binding{Name: "opencode-anthropic", Kind: KindLogin, Agent: "opencode"}, loginTTL},
		{Binding{Name: "claude-mcp-abc", Kind: KindLogin, Agent: "claude"}, loginTTL},
		{Binding{Name: "codex-key", Kind: KindSecret}, valueTTL},
	} {
		if got := ttlOf(tc.b); got != tc.want {
			t.Errorf("ttlOf(%+v) = %v, want %v", tc.b, got, tc.want)
		}
	}
}

// A value is looked up in this process, with the binding the file has, once, then cached until its
// time is up; a lookup that fails, or gives nothing, isn't cached, and puts nothing in.
func TestSecretValueLooksUpInProcess(t *testing.T) {
	t.Parallel()
	s := newSecrets("", nil, quiet())
	asked := fakeSecret(s, " real\n")
	b := Binding{Name: "codex", Kind: KindLogin, Agent: "codex"}
	for range 2 {
		if v, err := s.value(t.Context(), b); v != "real" || err != nil {
			t.Fatalf("value = %q, %v", v, err)
		}
	}
	if len(*asked) != 1 || !reflect.DeepEqual((*asked)[0], b) {
		t.Errorf("looked up %+v, want %+v once", *asked, b)
	}
	s.values[valueKeyOf(b)] = cached{"real", time.Now().Add(-loginTTL)}
	if _, err := s.value(t.Context(), b); err != nil || len(*asked) != 2 {
		t.Errorf("an expired value: %v, %d lookups", err, len(*asked))
	}
	s.values = map[valueKey]cached{}
	s.lookup = func(context.Context, Binding) (string, error) { return "", errors.New("locked") }
	if _, err := s.value(t.Context(), b); err == nil || !strings.Contains(err.Error(), "codex: locked") {
		t.Errorf("a failed lookup: %v", err)
	}
	s.lookup = func(context.Context, Binding) (string, error) { return " ", nil }
	if _, err := s.value(t.Context(), b); err == nil {
		t.Error("an empty value was taken")
	}
	if _, ok := s.values[valueKeyOf(b)]; ok {
		t.Error("a failed lookup was cached")
	}
	s.lookup = nil
	if _, err := s.value(t.Context(), b); err == nil {
		t.Error("a value without a lookup")
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer PH")
	if used := s.inject(t.Context(), h, []Binding{{Name: "codex", Kind: KindLogin, Header: "Authorization", Placeholder: "PH"}}); used != nil || h.Get("Authorization") != "Bearer PH" {
		t.Errorf("without a value: used %v, %q", used, h.Get("Authorization"))
	}
}

// A secret of yours and an agent's login that share a name are cached apart: neither binding's
// host ever gets the other's value.
func TestValuesCachedByBinding(t *testing.T) {
	t.Parallel()
	s := newSecrets("", nil, quiet())
	s.lookup = func(_ context.Context, b Binding) (string, error) { return b.Kind + "-value", nil }
	login := Binding{Name: "pi-openai", Kind: KindLogin, Agent: "pi"}
	secret := Binding{Name: "pi-openai", Kind: KindSecret}
	for _, tc := range []struct {
		b    Binding
		want string
	}{{login, "login-value"}, {secret, "secret-value"}, {login, "login-value"}} {
		if v, err := s.value(t.Context(), tc.b); v != tc.want || err != nil {
			t.Errorf("value(%+v) = %q, %v, want %q", tc.b, v, err, tc.want)
		}
	}
}

// The bindings file: versioned, private, of known kinds; InterceptLines gives its hosts on 443.
func TestReadBindings(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.json")
	l := BindingList{Bindings: []Binding{
		{Name: "github", Kind: KindSecret, Hosts: []string{"api.github.com"}, Header: "Authorization", Placeholder: "PH"},
		{Name: "codex", Kind: KindLogin, Agent: "codex", Hosts: []string{"chatgpt.com"}, Header: "Authorization", Placeholder: "PH2"},
		{Name: "pi-openrouter", Kind: KindLogin, Agent: "pi", Hosts: []string{"openrouter.ai"}, Path: "/api/v1/", Header: "Authorization", Placeholder: "PH3"},
		{Name: "mine", Kind: KindSecret, Hosts: []string{"api.example.com"}, Path: "/v1/*/charges", Methods: "GET,HEAD", Header: "Authorization", Placeholder: "PH4"},
	}}
	must(t, os.WriteFile(path, l.Encode(), 0o600))
	got, err := ReadBindings(path)
	l.Version = BindingListVersion
	if err != nil || !reflect.DeepEqual(got, l) {
		t.Errorf("ReadBindings = %+v, %v; want %+v", got, err, l)
	}
	if s := string(BindingList{}.Encode()); s != `{"version":1,"bindings":[]}`+"\n" {
		t.Errorf("no bindings = %q", s)
	}
	// Version 1 while every binding reads alike to a proxy that knows only it; 2 for methods, a
	// pattern with a *, or a path without its last /, which such a proxy reads otherwise.
	for _, tc := range []struct {
		b    Binding
		want int
	}{
		{Binding{Name: "a", Kind: KindLogin, Path: "/api/"}, 1},
		{Binding{Name: "a", Kind: KindLogin}, 1},
		{Binding{Name: "a", Kind: KindSecret, Methods: "GET"}, 2},
		{Binding{Name: "a", Kind: KindSecret, Path: "/v1/*/x"}, 2},
		{Binding{Name: "a", Kind: KindSecret, Path: "/v1"}, 2},
		{Binding{Name: "a", Kind: KindSecret, Path: "/"}, 2}, // which PathPrefixOK refused
		{Binding{Name: "a", Kind: KindSecret, Path: "/v1/", Methods: "GET,HEAD"}, 2},
	} {
		b, want := tc.b, tc.want
		var got BindingList
		must(t, json.Unmarshal(BindingList{Bindings: []Binding{b}}.Encode(), &got))
		if got.Version != want {
			t.Errorf("%+v: version %d, want %d", b, got.Version, want)
		}
		if _, err := parseBindings("x", BindingList{Bindings: []Binding{b}}.Encode()); err != nil {
			t.Errorf("%+v: not read back: %v", b, err)
		}
	}
	// Version 1, as an earlier devvm wrote it: read, as it can hold no methods and its paths, which
	// were prefixes then, no *; one that has either isn't.
	v1 := `{"version":1,"bindings":[{"name":"or","kind":"login","agent":"pi","hosts":["openrouter.ai"],"path":"/api/","header":"Authorization","placeholder":"PH"}]}`
	if got, err := parseBindings("v1", []byte(v1)); err != nil || len(got.Bindings) != 1 || got.Bindings[0].Path != "/api/" {
		t.Errorf("version 1: %+v, %v", got, err)
	}
	for name, tc := range map[string]struct {
		data string
		mode os.FileMode
	}{
		"another version":             {`{"version":3,"bindings":[]}`, 0o600},
		"version 1 with methods":      {`{"version":1,"bindings":[{"name":"x","kind":"login","methods":"GET"}]}`, 0o600},
		"version 1 with a * path":     {`{"version":1,"bindings":[{"name":"x","kind":"login","path":"/a*/"}]}`, 0o600},
		"a bare list":                 {`[]`, 0o600},
		"an unknown kind":             {`{"version":1,"bindings":[{"name":"x","kind":"env"}]}`, 0o600},
		"no kind":                     {`{"version":1,"bindings":[{"name":"x"}]}`, 0o600},
		"others may read":             {`{"version":1,"bindings":[]}`, 0o644},
		"a relative path":             {`{"version":1,"bindings":[{"name":"x","kind":"login","path":"api/"}]}`, 0o600},
		"a ** within a segment":       {`{"version":1,"bindings":[{"name":"x","kind":"login","path":"/a**/"}]}`, 0o600},
		"a path with a ?":             {`{"version":1,"bindings":[{"name":"x","kind":"login","path":"/api?x"}]}`, 0o600},
		"methods it doesn't know":     {`{"version":1,"bindings":[{"name":"x","kind":"login","methods":"GTE"}]}`, 0o600},
		"methods not canonical":       {`{"version":1,"bindings":[{"name":"x","kind":"login","methods":"head,get"}]}`, 0o600},
		"a path with a dot segment":   {`{"version":1,"bindings":[{"name":"x","kind":"login","path":"/api/../"}]}`, 0o600},
		"a path with a doubled slash": {`{"version":1,"bindings":[{"name":"x","kind":"login","path":"/api//"}]}`, 0o600},
		"an encoded path":             {`{"version":1,"bindings":[{"name":"x","kind":"login","path":"/%61pi/"}]}`, 0o600},
		"a path with a ;":             {`{"version":1,"bindings":[{"name":"x","kind":"login","path":"/api;x/"}]}`, 0o600},
	} {
		p := filepath.Join(dir, strings.ReplaceAll(name, " ", "-"))
		must(t, os.WriteFile(p, []byte(tc.data), 0o600))
		must(t, os.Chmod(p, tc.mode))
		if _, err := ReadBindings(p); err == nil {
			t.Errorf("%s: read", name)
		}
	}
	link := filepath.Join(dir, "link")
	must(t, os.Symlink(path, link))
	if _, err := ReadBindings(link); !errors.Is(err, safefile.ErrNotRegular) {
		t.Errorf("a link: %v", err)
	}
	hosts := InterceptLines([]string{"api.github.com", "github.com", "api.github.com"})
	if want := []string{"api.github.com:443", "github.com:443"}; !reflect.DeepEqual(hosts, want) {
		t.Errorf("InterceptLines = %q, want %q", hosts, want)
	}
	if InterceptLines(nil) != nil {
		t.Error("InterceptLines(nil) isn't empty")
	}
}

// A bindings file the proxy can't read, or that's gone, has no bindings: nothing goes in.
func TestBindingsUnreadableHaveNone(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "bindings.json")
	s := newSecrets(path, nil, quiet())
	must(t, os.WriteFile(path, BindingList{Bindings: []Binding{{Name: "t", Kind: KindSecret, Hosts: []string{"localhost"}}}}.Encode(), 0o600))
	if bs := s.forHost("localhost"); len(bs) != 1 {
		t.Fatalf("bindings = %+v", bs)
	}
	must(t, os.Chmod(path, 0o644)) //nolint:gosec // G302: the file others may read is what's tested
	later := time.Now().Add(2 * time.Second)
	must(t, os.Chtimes(path, later, later))
	if bs := s.forHost("localhost"); bs != nil {
		t.Errorf("a file others may read gave %+v", bs)
	}
	must(t, os.Remove(path))
	if bs := s.forHost("localhost"); bs != nil {
		t.Errorf("a file that's gone gave %+v", bs)
	}
}

// testCA is a CA valid from notBefore to notAfter, with no leaves minted yet.
func testCA(t *testing.T, notBefore, notAfter time.Time) *ca {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(9), Subject: pkix.Name{CommonName: "test CA"}, NotBefore: notBefore, NotAfter: notAfter,
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	must(t, err)
	cert, err := x509.ParseCertificate(der)
	must(t, err)
	c, err := newCA(cert, key)
	must(t, err)
	return c
}

// A leaf never outlives the CA that signed it, or starts before it: a VM left running near the CA's
// end gets leaves that end with it, kept until then, and none once it has ended.
func TestLeafWithinCA(t *testing.T) {
	t.Parallel()
	start, end := time.Now().Add(-time.Minute).Truncate(time.Second), time.Now().Add(30*time.Minute).Truncate(time.Second)
	authority := testCA(t, start, end)
	c, err := authority.leafFor("api.example.com")
	must(t, err)
	if c.Leaf.NotAfter.After(end) || c.Leaf.NotBefore.Before(start) {
		t.Errorf("a leaf valid %v to %v, from a CA valid %v to %v", c.Leaf.NotBefore, c.Leaf.NotAfter, start, end)
	}
	if again, err := authority.leafFor("api.example.com"); err != nil || again != c {
		t.Errorf("a leaf that ends with its CA was minted again (%v)", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(authority.cert)
	if _, err := c.Leaf.Verify(x509.VerifyOptions{DNSName: "api.example.com", Roots: pool}); err != nil {
		t.Errorf("the leaf doesn't verify: %v", err)
	}

	authority = testCA(t, time.Now().Add(-30*24*time.Hour), time.Now().Add(-time.Minute))
	if c, err := authority.leafFor("api.example.com"); !errors.Is(err, errCAEnded) || c != nil {
		t.Errorf("a leaf from a CA that has ended: %v, %v", c != nil, err)
	}
}

// The leaves a CA keeps are bounded (leafMax, leafCacheMax for a CA the proxy loads): past it, the
// one used least recently goes, so a binding with a wildcard host can't grow the cache without end.
func TestLeafCacheBounded(t *testing.T) {
	t.Parallel()
	if c := testCA(t, time.Now().Add(-time.Hour), time.Now().Add(48*time.Hour)); c.leafMax != leafCacheMax {
		t.Errorf("a CA keeps %d leaves, want %d", c.leafMax, leafCacheMax)
	}
	authority := testCA(t, time.Now().Add(-time.Hour), time.Now().Add(48*time.Hour))
	authority.leafMax = 3
	leaf := func(host string) *tls.Certificate {
		t.Helper()
		l, err := authority.leafFor(host)
		must(t, err)
		return l
	}
	a := leaf("a.example.com")
	b := leaf("b.example.com")
	leaf("c.example.com")
	if leaf("a.example.com") != a { // a is the most recently used now, b the least
		t.Error("a cached leaf was minted again")
	}
	leaf("d.example.com")
	if n := len(authority.leaves); n != 3 || authority.order.Len() != 3 {
		t.Errorf("%d leaves kept (%d in order), want 3", n, authority.order.Len())
	}
	if leaf("a.example.com") != a {
		t.Error("the most recently used leaf went")
	}
	if leaf("b.example.com") == b {
		t.Error("the least recently used leaf stayed")
	}
}

// A tunnel to a host with a bound secret, once the proxy's CA has ended, is refused before it
// opens: 403 with the reason and no allow command, logged as a refused block.
func TestInterceptCAEnded(t *testing.T) {
	t.Parallel()
	s, port, _, _ := interceptSetup(t, func(http.ResponseWriter, *http.Request) {})
	var events strings.Builder
	s.events.out = &events // the handler runs here, alone
	s.ca = testCA(t, time.Now().Add(-30*24*time.Hour), time.Now().Add(-time.Minute))
	target := net.JoinHostPort("localhost", strconv.Itoa(port))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodConnect, "http://"+target, nil)
	req.Host = target
	w := httptest.NewRecorder()
	s.handler(w, req)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), caEndedReason) ||
		strings.Contains(w.Body.String(), "network allow") || w.Header().Get("X-Devvm-Blocked") != "localhost" {
		t.Errorf("answered %d %q", w.Code, w.Body.String())
	}
	var e Event
	must(t, json.Unmarshal([]byte(events.String()), &e))
	if e.Event != "blocked" || e.Reason != caEndedReason || !e.Refused || e.Host != "localhost" || e.Port != port {
		t.Errorf("logged %+v", e)
	}
}

// The next CA signs from an hour after its start, a week before the current one ends: a VM that
// trusts both, as each launch installs them, verifies the proxy's certificates before the switch
// and after it, with the same proxy, until the next CA ends.
func TestNextCASigns(t *testing.T) {
	t.Parallel()
	now := time.Now().Truncate(time.Second)
	cur := testCA(t, now.Add(-23*24*time.Hour), now.Add(7*24*time.Hour+30*time.Minute))
	next := testCA(t, now.Add(-30*time.Minute), now.Add(30*24*time.Hour))
	cur.next = next
	pool := x509.NewCertPool()
	pool.AddCert(cur.cert)
	pool.AddCert(next.cert)
	for _, tc := range []struct {
		name  string
		at    time.Time
		want  *ca
		ended bool
	}{
		{"before the switch", now, cur, false},
		{"the switch", now.Add(30 * time.Minute), next, false},
		{"after the current one ends", now.Add(8 * 24 * time.Hour), next, false},
		{"after the next one ends", now.Add(31 * 24 * time.Hour), next, true},
	} {
		if got := cur.signer(tc.at); got != tc.want {
			t.Errorf("%s: signed by the %s CA", tc.name, map[bool]string{true: "current", false: "next"}[got == cur])
		}
		if got := cur.ended(tc.at); got != tc.ended {
			t.Errorf("%s: ended = %v", tc.name, got)
		}
		if tc.ended {
			continue
		}
		leaf, err := cur.signer(tc.at).mint("api.example.com", tc.at)
		must(t, err)
		if _, err := leaf.Leaf.Verify(x509.VerifyOptions{DNSName: "api.example.com", Roots: pool, CurrentTime: tc.at}); err != nil {
			t.Errorf("%s: the leaf doesn't verify: %v", tc.name, err)
		}
	}
	if got := cur.pem(); strings.Count(string(got), "BEGIN CERTIFICATE") != 2 || strings.Contains(string(got), "PRIVATE") {
		t.Errorf("/ca.pem = %q", got)
	}
}

// A proxy loads the next CA its config names beside the current one, and refuses a key another user
// may read, or a symbolic link where a CA file should be.
func TestLoadCA(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cert, key, ncert, nkey := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca.key"), filepath.Join(dir, "next.pem"), filepath.Join(dir, "next.key")
	writeCAFiles(t, cert, key, time.Now().Add(-time.Hour), time.Now().Add(30*24*time.Hour))
	writeCAFiles(t, ncert, nkey, time.Now().Add(22*24*time.Hour), time.Now().Add(52*24*time.Hour))
	s := testServer(t, Config{CA: cert, CAKey: key, CANext: ncert, CANextKey: nkey}, "")
	if s.ca == nil || s.ca.next == nil || s.ca.signer(time.Now()) != s.ca {
		t.Fatal("the proxy didn't load its next CA, or signs with it before it starts")
	}
	if l, err := s.ca.leafFor("api.example.com"); err != nil || l.Leaf.CheckSignatureFrom(s.ca.cert) != nil {
		t.Errorf("a leaf before the switch: %v", err)
	}
	must(t, os.Chmod(nkey, 0o640)) //nolint:gosec // G302: a key others may read, which the proxy refuses
	if _, err := loadCA(ncert, nkey); !errors.Is(err, ErrNotPrivate) {
		t.Errorf("a key another user may read: %v", err)
	}
	must(t, os.Chmod(nkey, 0o600))
	link := filepath.Join(dir, "link.key")
	must(t, os.Symlink(nkey, link))
	if _, err := loadCA(ncert, link); !errors.Is(err, safefile.ErrNotRegular) {
		t.Errorf("a key that's a link: %v", err)
	}
	if _, err := loadCA(ncert, key); err == nil {
		t.Error("a certificate with another CA's key")
	}
}

// devvm touches the bindings file when a value is set or removed: cached values are dropped.
func TestBindingsChangeDropsCachedValues(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "bindings.json")
	s := newSecrets(path, nil, quiet())
	writeBindings(t, path)
	s.all()
	s.valueMu.Lock()
	s.values[valueKey{name: "t"}] = cached{"old", time.Now()}
	s.valueMu.Unlock()
	later := time.Now().Add(2 * time.Second)
	must(t, os.Chtimes(path, later, later))
	s.all()
	s.valueMu.Lock()
	defer s.valueMu.Unlock()
	if _, ok := s.values[valueKey{name: "t"}]; ok {
		t.Error("a cached value survived the bindings file changing")
	}
}

// An intercepted HTTPS request through the proxy: the client trusts only devvm's CA and sends the
// placeholder; the upstream server sees the real secret.
func TestIntercept(t *testing.T) {
	t.Parallel()
	// Upstream: echoes what it received, with a certificate for "localhost" from the same CA.
	s, port, _, client := interceptSetup(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s|%s|%s", r.Header.Get("Authorization"), r.Header.Get("X-Echo"), r.Host) //nolint:gosec // a test echo server
	})
	fakeSecret(s.secrets, "real-secret")
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("https://localhost:%d/x", port), nil)
	req.Header.Set("Authorization", "Bearer PH")
	req.Header.Set("X-Echo", "PH")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if got, want := string(body), fmt.Sprintf("Bearer real-secret|PH|localhost:%d", port); got != want {
		t.Errorf("upstream saw %q, want %q", got, want)
	}
	// The Host upstream gets is the one checked, canonical: not the VM's spelling (a trailing dot).
	req, _ = http.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("https://localhost.:%d/x", port), nil)
	req.Header.Set("Authorization", "Bearer PH")
	if resp, err := client.Do(req); err != nil {
		t.Error(err)
	} else {
		body, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if got := string(body); !strings.HasSuffix(got, fmt.Sprintf("|localhost:%d", port)) {
			t.Errorf("upstream saw Host %q", got)
		}
	}

	// The CA is served to the VM; the private key never is.
	req, _ = http.NewRequestWithContext(t.Context(), http.MethodGet, "http://devvm.proxy/ca.pem", nil)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "BEGIN CERTIFICATE") || strings.Contains(string(body), "PRIVATE") {
		t.Errorf("/ca.pem = %q", body)
	}
}

// A WebSocket's upgrade in an intercepted tunnel, as opencode opens OpenAI's and xAI's Responses
// API (wss://…/v1/responses), gets the secret put in its header as any request does, and once the
// upstream switches protocols, the proxy carries the connection's bytes both ways.
func TestInterceptWebSocket(t *testing.T) {
	t.Parallel()
	_, port, _, client := interceptSetup(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			http.Error(w, "no upgrade", http.StatusBadRequest)
			return
		}
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nX-Auth: %s\r\n\r\n", r.Header.Get("Authorization"))
		_ = rw.Flush()
		b := make([]byte, 4)
		if _, err := io.ReadFull(rw, b); err == nil {
			_, _ = conn.Write(b)
		}
	})
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("https://localhost:%d/v1/responses", port), nil)
	req.Header.Set("Authorization", "Bearer PH")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols || resp.Header.Get("X-Auth") != "Bearer real" {
		t.Fatalf("the upgrade: %d, the upstream saw %q", resp.StatusCode, resp.Header.Get("X-Auth"))
	}
	conn, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		t.Fatal("the switched connection can't be written to")
	}
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 4)
	if _, err := io.ReadFull(conn, b); err != nil || string(b) != "ping" {
		t.Errorf("the switched connection gave %q, %v", b, err)
	}
}

// Every HTTPS request the proxy forwards, in an intercepted tunnel or in absolute form
// (handleHTTP), is refused before it goes anywhere when it would make a lasting GitHub
// credential, logged as a refused block; plain HTTP carries no secret.
func TestForwarderRefusesMinting(t *testing.T) {
	t.Parallel()
	s := testServer(t, Config{}, "")
	var events strings.Builder
	s.events.out = &events // the handler runs here, alone
	// Ended: one forwarded instead would fail at once, never reaching GitHub.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, path := range []string{"/user/keys", "/repositories/1/keys"} {
		events.Reset()
		r := httptest.NewRequestWithContext(ctx, http.MethodPost, "https://api.github.com"+path, nil)
		r.Header.Set("Authorization", "Bearer PH")
		w := httptest.NewRecorder()
		fwd, done := s.forwarder("https", "api.github.com", "api.github.com", 443)
		fwd.ServeHTTP(w, r)
		done()
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "refused") {
			t.Errorf("POST %s: %d %q", path, w.Code, w.Body.String())
		}
		var e Event
		must(t, json.Unmarshal([]byte(events.String()), &e))
		if e.Event != "blocked" || !e.Refused || e.Host != "api.github.com" || e.Path != path {
			t.Errorf("POST %s: logged %+v", path, e)
		}
	}
}

// Calls that make lasting GitHub credentials with the VM's token are refused; ordinary ones aren't.
func TestMintsCredential(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ host, method, path, want string }{
		{"api.github.com", "POST", "/user/keys", "a key on your GitHub account"},
		{"api.github.com", "post", "/User/SSH_Signing_Keys/", "a key on your GitHub account"},
		{"api.github.com", "POST", "/repos/o/r/keys", "a deploy key"},
		{"api.github.com", "POST", "//repos/o/r/./keys", "a deploy key"},
		{"api.github.com", "POST", "/repos/o/r/x/../keys", "a deploy key"},
		{"api.github.com", "POST", "/orgs/o/actions/runners/registration-token", "a self-hosted runner"},
		{"api.github.com", "POST", "/repos/o/r/actions/runners/generate-jitconfig", "a self-hosted runner"},
		{"api.github.com", "PATCH", "/repos/o/r/hooks/1/config", "a webhook"},
		{"api.github.com", "POST", "/orgs/o/hooks", "a webhook"},
		{"api.github.com", "POST", "/repositories/123/keys", "a deploy key"},
		{"api.github.com", "POST", "/repositories/123/hooks", "a webhook"},
		{"api.github.com", "POST", "/repositories/123/actions/runners/registration-token", "a self-hosted runner"},
		{"api.github.com", "POST", "/organizations/456/hooks", "a webhook"},
		{"api.github.com", "POST", "/organizations/456/actions/runners/generate-jitconfig", "a self-hosted runner"},
		{"api.github.com", "GET", "/repositories/123", ""},
		{"api.github.com", "GET", "/user/keys", ""},
		{"api.github.com", "DELETE", "/repos/o/r/keys/1", ""},
		{"api.github.com", "POST", "/repos/o/r/pulls", ""},
		{"api.github.com", "POST", "/repos/o/r/issues/1/comments", ""},
		{"github.com", "POST", "/user/keys", ""},
	} {
		if got := mintsCredential(c.host, c.method, c.path); got != c.want {
			t.Errorf("%s %s%s = %q, want %q", c.method, c.host, c.path, got, c.want)
		}
	}
}
