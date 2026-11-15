package settings

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"

	"github.com/devvmhq/devvm/internal/fetch"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/proxy"
	"github.com/devvmhq/devvm/internal/redact"
)

// config.toml's company proxy for devvm's own requests and programs on
// the host: its password from the secret, looked up once; none of its own
// when the environment sets the proxy.
func TestCompanyProxy(t *testing.T) {
	d := testDirs(t)
	write(t, d.ConfigFile(), "[proxy]\nhttps = \"http://alice@proxy.corp:3128\"\nno-proxy = \".corp\"\n")
	lookups := 0
	password := func(_ context.Context, name string) (redact.Secret, error) {
		lookups++
		if name != proxy.UpstreamSecret {
			t.Errorf("looked up %q", name)
		}
		return redact.NewSecret("pa55 word"), nil
	}
	env := map[string]string{}
	c := NewCompanyProxy(d, func(k string) string { return env[k] }, password)
	up, err := c.Upstream(t.Context())
	if err != nil || up == nil || *up != (proxy.Upstream{HTTPS: "http://alice@proxy.corp:3128", NoProxy: ".corp", Secret: proxy.UpstreamSecret}) {
		t.Fatalf("Upstream: %+v, %v", up, err)
	}
	withPassword := "http://" + url.UserPassword("alice", "pa55 word").String() + "@proxy.corp:3128"
	u, err := c.Route(request(t, "https://github.com/x"))
	if err != nil || u == nil || u.String() != withPassword {
		t.Errorf("Route: %v, %v", u, err)
	}
	if u, err := c.Route(request(t, "https://git.corp/x")); u != nil || err != nil {
		t.Errorf("Route for NO_PROXY's: %v, %v", u, err)
	}
	vars, err := c.Env(t.Context())
	want := []string{"HTTPS_PROXY=" + withPassword, "https_proxy=" + withPassword, "NO_PROXY=.corp", "no_proxy=.corp"}
	if err != nil || !slices.Equal(vars, want) {
		t.Errorf("Env: %q, %v", vars, err)
	}
	// Upstream's check, and one lookup for Route and Env together.
	if lookups != 2 {
		t.Errorf("%d lookups", lookups)
	}
	// The environment's is its own: nothing added, nothing looked up.
	env["HTTPS_PROXY"] = "http://env.corp:8080"
	e := NewCompanyProxy(d, func(k string) string { return env[k] }, password)
	if vars, err := e.Env(t.Context()); vars != nil || err != nil {
		t.Errorf("the environment's: %q, %v", vars, err)
	}
	if lookups != 2 {
		t.Errorf("the environment's looked up the password")
	}
	// nil is the environment's alone.
	var none *CompanyProxy
	if vars, err := none.Env(t.Context()); vars != nil || err != nil {
		t.Errorf("nil: %q, %v", vars, err)
	}
	if _, err := none.Route(request(t, "https://github.com/x")); err != nil {
		t.Errorf("nil Route: %v", err)
	}
}

// A password with the characters a URL gives meaning to is percent-encoded
// in Env's URLs, and parses back to itself. mise (reqwest) decodes it before
// it sends Proxy-Authorization: checked against squid with basic auth.
func TestCompanyProxyEnvEncodesPassword(t *testing.T) {
	d := testDirs(t)
	write(t, d.ConfigFile(), "[proxy]\nhttps = \"http://alice@proxy.corp:3128\"\n")
	const password = "p@ss:w/rd #1%?" //nolint:gosec // G101: a test's fake password
	c := NewCompanyProxy(d, func(string) string { return "" }, func(context.Context, string) (redact.Secret, error) { return redact.NewSecret(password), nil })
	vars, err := c.Env(t.Context())
	const want = "http://alice:p%40ss%3Aw%2Frd%20%231%25%3F@proxy.corp:3128" //nolint:gosec // G101: a fake company proxy's fake password
	if err != nil || !slices.Equal(vars, []string{"HTTPS_PROXY=" + want, "https_proxy=" + want}) {
		t.Fatalf("Env: %q, %v", vars, err)
	}
	u, err := url.Parse(want)
	if p, _ := u.User.Password(); err != nil || u.User.Username() != "alice" || p != password {
		t.Errorf("parsed back: %v, %q", err, p)
	}
}

// Without the password saved, the check fails with its message, and so
// do requests, which aren't tried again, and programs.
func TestCompanyProxyPasswordMissing(t *testing.T) {
	d := testDirs(t)
	write(t, d.ConfigFile(), "[proxy]\nhttp = \"http://alice@proxy.corp:3128\"\n")
	getenv := func(string) string { return "" }
	for name, password := range map[string]func(context.Context, string) (redact.Secret, error){
		"none saved": func(context.Context, string) (redact.Secret, error) {
			return redact.Secret{}, msg.ProxyPasswordMissing()
		},
		"no lookup": nil,
	} {
		c := NewCompanyProxy(d, getenv, password)
		if _, err := c.Upstream(t.Context()); msg.NamesOf(err) != "proxy_password_missing" {
			t.Errorf("%s: Upstream: %v", name, msg.NamesOf(err))
		}
		_, err := c.Route(request(t, "http://example.com/"))
		if m, ok := errors.AsType[msg.M](err); !errors.Is(err, fetch.ErrProxy) || !ok || m.Names() != "proxy_password_missing" || fetch.Transient(err) {
			t.Errorf("%s: Route: %v", name, err)
		}
		if _, err := c.Env(t.Context()); msg.NamesOf(err) != "proxy_password_missing" {
			t.Errorf("%s: Env: %v", name, msg.NamesOf(err))
		}
	}
	// A URL without a user needs none.
	write(t, d.ConfigFile(), "[proxy]\nhttp = \"proxy.corp:3128\"\n")
	c := NewCompanyProxy(d, getenv, nil)
	if vars, err := c.Env(t.Context()); err != nil || !slices.Contains(vars, "HTTP_PROXY=http://proxy.corp:3128") {
		t.Errorf("no user: %q, %v", vars, err)
	}
	// No [proxy]: none.
	write(t, d.ConfigFile(), "features = []\n")
	c = NewCompanyProxy(d, getenv, nil)
	if u, err := c.Route(request(t, "http://example.com/")); u != nil || err != nil {
		t.Errorf("none: %v, %v", u, err)
	}
}

// request is a GET of u, as a client sends it.
func request(t *testing.T, u string) *http.Request {
	t.Helper()
	return httptest.NewRequestWithContext(t.Context(), http.MethodGet, u, http.NoBody)
}
