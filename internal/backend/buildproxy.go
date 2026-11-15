package backend

import (
	"net/netip"
	"os"
	"path/filepath"
	"time"

	"github.com/devvmhq/devvm/internal/daemon"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/proxy"
	"github.com/devvmhq/devvm/internal/redact"
	"github.com/devvmhq/devvm/internal/state"
)

// A build that reaches the internet through a company proxy goes through
// a proxy of devvm's own on the host, one per build: devvm _proxy with any host on ports 80 and 443 but
// the host's own and internal addresses, through the company proxy (its
// config's Upstream), and a password made for the build. The build gets only
// that proxy's address with that password, so the company proxy's
// credentials stay on the host, out of reach of the code a build runs,
// which is an image's own.

// ProxyStarter starts build proxies: devvm's own binary (Self), run by
// Spawn (daemon.Spawn: a process that can outlive devvm, stopped through
// the process itself), looked at every Tick while it starts, ten times at
// most.
type ProxyStarter struct {
	Self  string
	Spawn func(daemon.Cmd) (*daemon.Handle, error)
	Tick  time.Duration
}

// BuildProxy is one build's proxy: its port on the host's loopback and its
// password, which a build reaches it with (ProxyURL).
type BuildProxy struct {
	Port     int
	Password redact.Secret
	proc     *daemon.Handle
	dir      string
}

// Start starts the proxy of a build (c.VM), its files in dir, made again
// and the user's alone: its config (proxy.json), its policy, its log
// (proxy.log) and its event log (events.jsonl). c has what the build
// chose: its lifeline, the company proxy it goes through and whether names
// the host can't resolve go there; Start sets the rest. It listens on a
// loopback port of its own and ends when the process its lifeline's pid
// file names does, or at Stop. It returns once the proxy says it listens,
// in its status file (status), and still runs.
func (s ProxyStarter) Start(dir string, c proxy.Config) (*BuildProxy, error) {
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	pw, err := NewProxyPassword()
	if err != nil {
		return nil, err
	}
	p := &BuildProxy{Password: pw, dir: dir}
	policy, logPath := filepath.Join(dir, "policy"), filepath.Join(dir, "proxy.log")
	if err := state.WriteFile(policy, []byte("# a build: any host, not this machine's own or internal addresses\n**\n")); err != nil {
		return nil, err
	}
	c.Listen, c.Password, c.Policies, c.Log = "127.0.0.1:0", pw, []string{policy}, filepath.Join(dir, "events.jsonl")
	// The password and the company proxy's credentials go in the config,
	// the user's alone: a process's arguments are visible to other users.
	config := filepath.Join(dir, configFile)
	if err := state.WriteFile(config, c.Encode()); err != nil {
		return nil, err
	}
	out, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // devvm's own log
	if err != nil {
		return nil, err
	}
	defer func() { _ = out.Close() }()
	status := filepath.Join(dir, "status")
	if p.proc, err = s.Spawn(daemon.Cmd{Path: s.Self, Args: []string{"_proxy", "-config", config}, Out: out, Status: status}); err != nil {
		_ = os.Remove(config) // its passwords, for a proxy that never ran
		return nil, err
	}
	said, ok := daemon.WaitReady(status, p.proc.PID(), p.proc.Running, s.Tick, 10)
	if ap, err := netip.ParseAddrPort(said); ok && err == nil && ap.Addr().IsLoopback() && ap.Port() != 0 {
		p.Port = int(ap.Port())
		return p, nil
	}
	p.Stop()
	return nil, msg.ProxyStartFailed(logPath, c.VM)
}

// Stop stops the proxy, through the process Start started (daemon.Handle),
// so the signal reaches it alone, never another process given its pid
// since: SIGTERM, then after two seconds SIGKILL. It deletes its config,
// which holds its password and the company proxy's, leaving its logs. A
// nil one is none.
func (p *BuildProxy) Stop() {
	if p == nil {
		return
	}
	p.proc.Stop(2 * time.Second)
	_ = os.Remove(filepath.Join(p.dir, configFile))
}

// configFile is a build proxy's config in its folder.
const configFile = "proxy.json"
