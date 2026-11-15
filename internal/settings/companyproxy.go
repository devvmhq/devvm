package settings

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sync"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/fetch"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/proxy"
	"github.com/devvmhq/devvm/internal/redact"
)

// CompanyProxy is the company proxy (HostProxy) for what devvm does on
// this machine: the check before a build or a VM's proxy starts
// (Upstream), devvm's own requests (Route) and the programs it runs
// (Env). The environment's settings are the environment's to give: Route
// is then net/http's own, and Env adds nothing. config.toml's are read,
// with the password, once, at the first request or program that needs
// them. Make one with NewCompanyProxy.
type CompanyProxy struct {
	Dirs   paths.Dirs
	Getenv func(string) string
	// Password looks up the secret config.toml's proxy takes its
	// password from (secrets.Store.ProxyPassword): proxy_password_missing
	// while it isn't saved. nil finds none.
	Password func(ctx context.Context, name string) (redact.Secret, error)

	once  sync.Once
	route func(*http.Request) (*url.URL, error)
	env   []string
	err   error
}

// NewCompanyProxy returns the company proxy of devvm's folders d and its
// environment (getenv), with config.toml's password from password.
func NewCompanyProxy(d paths.Dirs, getenv func(string) string, password func(ctx context.Context, name string) (redact.Secret, error)) *CompanyProxy {
	return &CompanyProxy{Dirs: d, Getenv: getenv, Password: password}
}

// Upstream is the company proxy a VM's proxy or a build's goes through
// (proxy.Config's Upstream), checked (backend.HostProxy.Upstream): nil
// for none. One whose password is a secret (config.toml's) needs it
// saved, as a proxy started without it would fail: new VMs, builds and
// VMs' proxies check it before anything starts.
func (c *CompanyProxy) Upstream(ctx context.Context) (*proxy.Upstream, error) {
	up, _, err := c.upstream(ctx)
	return up, err
}

// upstream is Upstream, and the password of its secret (empty for none).
func (c *CompanyProxy) upstream(ctx context.Context) (*proxy.Upstream, redact.Secret, error) {
	var password redact.Secret
	host, err := HostProxy(c.Dirs, c.Getenv)
	if err != nil {
		return nil, password, err
	}
	if host.Secret != "" {
		if c.Password == nil {
			return nil, password, msg.ProxyPasswordMissing()
		}
		if password, err = c.Password(ctx, host.Secret); err != nil {
			return nil, redact.Secret{}, err
		}
	}
	up, err := host.Upstream()
	return up, password, err
}

// fromConfig reports whether the company proxy is config.toml's: the
// environment sets none.
func (c *CompanyProxy) fromConfig() bool {
	return c != nil && backend.ReadHostProxy(c.Getenv).None()
}

// resolve reads config.toml's company proxy, with its password, once.
func (c *CompanyProxy) resolve(ctx context.Context) {
	c.once.Do(func() {
		up, password, err := c.upstream(ctx)
		if err != nil || up == nil {
			c.err = err
			return
		}
		with, err := up.WithPassword(password)
		if err == nil {
			c.route, err = with.Route()
		}
		if err != nil {
			c.err = err
			return
		}
		c.env = backend.HostProxy{HTTPS: with.HTTPS, HTTP: with.HTTP, NoProxy: with.NoProxy}.Env()
	})
}

// Route is the proxy a request of devvm's own on this machine goes
// through, as net/http's Transport.Proxy: the environment's, as net/http
// reads it, else config.toml's, routed as a VM's proxy routes it
// (proxy.Upstream.Route). One that can't be gone through, such as one
// whose password isn't saved, fails the request with fetch.ErrProxy,
// which isn't tried again. A nil CompanyProxy is the environment's alone.
func (c *CompanyProxy) Route(req *http.Request) (*url.URL, error) {
	if !c.fromConfig() {
		return http.ProxyFromEnvironment(req)
	}
	c.resolve(req.Context())
	if c.err != nil {
		return nil, fmt.Errorf("%w: %w", fetch.ErrProxy, c.err)
	}
	if c.route == nil {
		return nil, nil
	}
	return c.route(req)
}

// Env is the variables a program devvm runs on this machine (host mise)
// gets besides devvm's own environment, KEY=value: config.toml's company
// proxy, with its password in each URL that names a user, as programs
// take no password apart; none when the environment sets the proxy, as
// the program inherits it. They go only into a program's environment,
// never its arguments or a file. A nil CompanyProxy adds none.
func (c *CompanyProxy) Env(ctx context.Context) ([]string, error) {
	if !c.fromConfig() {
		return nil, nil
	}
	c.resolve(ctx)
	return c.env, c.err
}
