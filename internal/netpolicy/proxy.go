package netpolicy

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"syscall"
	"time"

	"github.com/devvmhq/devvm/internal/agentlogin"
	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/daemon"
	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/ports"
	"github.com/devvmhq/devvm/internal/proxy"
	"github.com/devvmhq/devvm/internal/redact"
	"github.com/devvmhq/devvm/internal/secrets"
	"github.com/devvmhq/devvm/internal/settings"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/tools"
	"github.com/devvmhq/devvm/internal/trust"
	"github.com/devvmhq/devvm/internal/vmname"
)

// Proxy starts and stops VMs' proxies: devvm _proxy, one per VM, in the
// background, following the host process that runs the VM. It also
// chooses a new VM's network tier (Choose) and checks it holds
// (SelfCheck). NewProxy sets every field; tests change them.
type Proxy struct {
	Dirs    paths.Dirs
	Assets  fs.FS  // devvm's own files (assets.FS), whose built-in policies devvm network shows
	Self    string // devvm's binary, which runs as the proxy
	OS      string // the host's, as Go names it
	Secrets *secrets.Store
	// Start starts a process in the background and returns its pid
	// (daemon.Detach).
	Start daemon.Starter
	// Args returns a running process's arguments (platform.ProcessArgs).
	Args func(pid int) ([]string, error)
	// ExcludeFromBackup keeps a folder out of backups (macOS).
	ExcludeFromBackup func(ctx context.Context, dir string) error
	// InitCA readies a VM's CA and its next one (trust.InitCA).
	InitCA func(context.Context, paths.CA) error
	// Listening reports whether a program has a host loopback port.
	Listening func(ctx context.Context, port int) bool
	// PortRange is the range new ports come from (platform.ChoosablePorts,
	// which the zero value means too, or a test build's own,
	// DEVVM_PORT_RANGE), which its allocator (Ports) has.
	PortRange platform.PortRange
	// SSHList is the ports VMs' ssh comes from in place of
	// ports.SSHPorts, which its allocator (Ports) has: nil but in a test
	// build (DEVVM_SSH_PORTS).
	SSHList []int
	// Tick is how often a start looks for the proxy to listen, ten times;
	// a stop waits four Ticks for each signal to end it.
	Tick time.Duration
	// Signal sends a background process a signal, and its group with it
	// (Stop's: daemon.SignalGroup).
	Signal func(pid int, sig syscall.Signal) error
	// Getenv reads devvm's environment: the host's proxy settings, which a
	// VM's proxy goes through over config.toml's (settings.HostProxy).
	Getenv func(string) string
	// Reporter is where it prints, and asks ([Y/n]) when its input is a
	// terminal.
	msg.Reporter
}

// NewProxy returns the proxies of devvm's folders, run by self (devvm's
// binary), with its own files (assets), printing and asking through r.
func NewProxy(d paths.Dirs, assets fs.FS, self string, store *secrets.Store, r msg.Reporter) *Proxy {
	return &Proxy{
		Dirs: d, Assets: assets, Self: self, OS: runtime.GOOS, Secrets: store,
		Start: daemon.Detach, Args: platform.ProcessArgs, ExcludeFromBackup: platform.ExcludeFromBackup,
		InitCA:    trust.InitCA,
		Listening: ports.Listening, PortRange: platform.ChoosablePorts(), Tick: 500 * time.Millisecond, Signal: daemon.SignalGroup, Getenv: os.Getenv,
		Reporter: r,
	}
}

// proc is the VM's proxy as a background process: its pid file in the
// VM's run folder, and devvm _proxy -config with the VM's config in its
// arguments.
func (p *Proxy) proc(vm vmname.VM) daemon.Proc {
	files := p.Dirs.VM(vm)
	return daemon.Proc{
		PIDFile: files.Proxy(".pid"), Args: p.Args, Signal: p.Signal,
		Is: func(args []string) bool { return daemon.HasFlag(args, "_proxy", "-config", files.ProxyConfig()) },
	}
}

// PID returns the pid of the VM's running proxy: the process its pid file
// names, if that still runs as the VM's proxy.
func (p *Proxy) PID(vm vmname.VM) (int, bool) { return p.proc(vm).PID() }

// otherBinary reports whether p's VM vm has a proxy running, started from
// another devvm binary than p's (the config it started with, its
// DevvmSHA256), so it may read a rule or a bindings file this devvm writes
// otherwise, such as one by method and path, until the VM's next launch
// starts it again. A running proxy whose config, or this binary, can't be
// read counts as another's: devvm can't tell.
func otherBinary(p *Proxy, vm vmname.VM) bool {
	if _, ok := p.PID(vm); !ok {
		return false
	}
	c, err := proxy.ReadConfig(p.Dirs.VM(vm).ProxyConfig())
	if err != nil {
		return true
	}
	self, err := image.SHA256File(p.Self)
	return err != nil || c.DevvmSHA256 != self
}

// childProc is p's VM vm's proxy's child as a background process: its
// pid file, which its broker writes once it's ready, and devvm
// _proxy-child -for with the VM's config in its arguments.
func childProc(p *Proxy, vm vmname.VM) daemon.Proc {
	config := p.Dirs.VM(vm).ProxyConfig()
	return daemon.Proc{
		PIDFile: proxy.ChildPIDFile(config), Args: p.Args, Signal: p.Signal,
		Is: func(args []string) bool { return proxy.IsChild(args, config) },
	}
}

// Stop stops the VM's proxy, if it runs: its broker, SIGTERM, then after
// two seconds SIGKILL, waiting up to two more for it to end, which stops
// its child; then a child that outlived its broker, the same way. It
// reports whether neither runs then, and then deletes the socket the
// proxy relayed the VM's SSH agent on, which a signal leaves behind, and
// what the broker said of its child.
func (p *Proxy) Stop(vm vmname.VM) bool {
	if !p.proc(vm).Stop(4*p.Tick) || !childProc(p, vm).Stop(4*p.Tick) {
		return false
	}
	_ = os.Remove(p.Dirs.AgentRelay(vm))
	config := p.Dirs.VM(vm).ProxyConfig()
	_ = os.Remove(proxy.ChildPIDFile(config))
	_ = os.Remove(proxy.ConfinedFile(config))
	return true
}

// Unconfined are devvm doctor's warnings for the VMs among vms whose
// proxy runs with a child that isn't confined to only what it needs, as
// its broker wrote once the child was ready (proxy.ReadConfined), one for
// each, saying why. A proxy that hasn't said, such as one still starting
// or one an older devvm started, gets none.
func Unconfined(p *Proxy, vms []*state.VM) []msg.M {
	var ms []msg.M
	for _, st := range vms {
		vm := st.Name()
		if _, ok := p.PID(vm); !ok {
			continue
		}
		c, err := proxy.ReadConfined(p.Dirs.VM(vm).ProxyConfig())
		if err != nil || c.Level == proxy.ConfinedFull {
			continue
		}
		ms = append(ms, unconfinedWarning(vm, c, p.Dirs.VM(vm).ProxyLog()))
	}
	return ms
}

// unconfinedWarning is the warning for VM vm's proxy's child confined as
// c, whose log is log: one for each reason devvm knows.
func unconfinedWarning(vm vmname.VM, c proxy.Confinement, log string) msg.M {
	switch {
	case c.Level != proxy.ConfinedNone:
		return linuxWarning(vm, c, log)
	case c.Why == proxy.WhyUnsupported:
		return msg.ProxyUnconfinedUnsupported(vm)
	case c.Why == proxy.WhySandboxMissing:
		return msg.ProxyUnconfinedNoSandbox(vm)
	case c.Why == proxy.WhySandboxFailed:
		return msg.ProxyUnconfinedSandboxFailed(log, vm)
	case c.Why == proxy.WhyCheckFailed:
		return msg.ProxyUnconfinedCheckFailed(log, vm)
	case c.Why == proxy.WhyNoKernel:
		return msg.ProxyUnconfinedNoKernel(vm)
	}
	return msg.ProxyUnconfined(c.String(), log, vm)
}

// linuxWarning is the warning for VM vm's proxy's child that Landlock or
// the seccomp filter confines only in part on this kernel, as c says,
// whose log is log: one for what the kernel lacks first, or the line
// itself for a reason devvm doesn't know.
func linuxWarning(vm vmname.VM, c proxy.Confinement, log string) msg.M {
	switch c.Why {
	case proxy.WhyNoLandlock:
		return msg.ProxyUnconfinedNoLandlock(vm)
	case proxy.WhyNoSeccomp:
		return msg.ProxyUnconfinedNoSeccomp(vm)
	case proxy.WhyLandlockPorts:
		return msg.ProxyUnconfinedOldPorts(vm)
	case proxy.WhyLandlockSignals:
		return msg.ProxyUnconfinedOldSignals(vm)
	}
	return msg.ProxyUnconfined(c.String(), log, vm)
}

// relayedAgent is the SSH agent a VM's proxy relays and the socket it
// relays it on: a start's, agent ("" none), on the VM's own socket
// (paths.Dirs.AgentRelay); a running VM's, the one its proxy's start
// relayed, as its backend forwards that socket until its next start.
func relayedAgent(d paths.Dirs, vm vmname.VM, running bool, agent string) (upstream, sock string) {
	if running {
		old, err := proxy.ReadConfig(d.VM(vm).ProxyConfig())
		if err != nil {
			return "", ""
		}
		return old.Agent, old.AgentSocket
	}
	if agent == "" {
		return "", ""
	}
	// A relative SSH_AUTH_SOCK is devvm's folder's, where the proxy isn't.
	if abs, err := filepath.Abs(agent); err == nil {
		agent = abs
	}
	return agent, d.AgentRelay(vm)
}

// Prepare readies a VM to start on b, or a running one to go on, on its
// tier: the VM's own CA and its secret files, whatever its tier; its
// ports, and its proxy running. It returns the confinement its backend
// starts it with (Tier.Net): a running VM keeps the one it started with
// until its next start, on its proxy alone when it started so or its tier
// is confined. The proxy follows the host process that runs the VM (its
// Run.Lifeline): a VM started again gets a new proxy. A start forwards
// the SSH agent at agent ("" none): its proxy relays it, and the
// confinement's AgentSocket is the relay's socket (proxy.Config.Agent). A
// running VM's proxy relays the agent its start forwarded, whatever agent
// says (relayedAgent). A running VM's goes through PrepareRunning.
func (p *Proxy) Prepare(ctx context.Context, b backend.Backend, st *state.VM, running bool, agent string) (backend.Confinement, error) {
	c, _, err := prepare(ctx, p, b, st, running, agent)
	return c, err
}

// PrepareRunning readies a running VM to go on (Prepare). The VM never
// runs on without its proxy, so a failure once its proxy was stopped, or
// while none of its runs, stops the VM (unproxied). Whatever refuses the
// proxy's start before its proxy is touched, such as a company proxy
// whose password isn't saved, leaves the VM and its proxy as they were,
// and says so (net_proxy_kept), but only while that proxy enforces what
// the VM's state asks for now (enforcesNow): one left with a looser tier
// than the state's would fail open, so the VM is stopped then too.
func (p *Proxy) PrepareRunning(ctx context.Context, b backend.Backend, st *state.VM) (backend.Confinement, error) {
	vm := st.Name()
	c, touched, err := prepare(ctx, p, b, st, true, "")
	if err == nil {
		return c, nil
	}
	// Kept only while both of its proxy's processes run: a broker whose child
	// has ended is ending too.
	if _, up := p.PID(vm); up && childProc(p, vm).Running() && !touched && enforcesNow(p, st) {
		p.Warn(msg.NetProxyKept(vm))
		return c, err
	}
	return c, unproxied(ctx, p, b, vm, err)
}

// tierNow is st's VM's tier as its state has it now: a command that read
// the state before devvm network changed it never starts the proxy with
// the tier before, nor keeps one that has it.
func tierNow(p *Proxy, st *state.VM) Tier {
	if now, err := state.ReadVM(p.Dirs, st.Name()); err == nil && now.Exists() && now.ID == st.ID {
		return TierOf(now)
	}
	return TierOf(st)
}

// enforcesNow reports whether st's VM's running proxy (p's) enforces what
// its state asks for now: it started with the policies its tier gives
// (Policies, tierNow), built-in, files and denies, open or not, from this
// devvm binary, whose built-in policies' rules it has. A config that
// can't be read, or a binary that can't, counts as another's.
func enforcesNow(p *Proxy, st *state.VM) bool {
	vm, tier := st.Name(), tierNow(p, st)
	old, err := proxy.ReadConfig(p.Dirs.VM(vm).ProxyConfig())
	if err != nil {
		return false
	}
	self, err := image.SHA256File(p.Self)
	if err != nil || old.DevvmSHA256 != self {
		return false
	}
	set := Policies(p.Dirs, vm, tier, image.Name(st.Image), tools.Names(st))
	return slices.Equal(old.BuiltinPolicies, set.Builtin) && slices.Equal(old.Policies, set.Files) &&
		slices.Equal(old.DenyPolicies, set.Denies) && old.Open == (tier == TierOpen)
}

// Refusal is what refuses any VM's proxy start, whatever the VM, known
// before anything stops (shared): devvm checks it before it stops a
// running VM to start it again, and before it makes one, so a refused
// restart leaves the VM running.
func (p *Proxy) Refusal(ctx context.Context) error {
	_, err := shared(ctx, p)
	return err
}

// common is what every VM's proxy starts with, whatever the VM.
type common struct {
	cas      trust.CAs       // the user's CAs
	upstream *proxy.Upstream // the company proxy; nil: none
}

// shared is what every VM's proxy (p's) starts with now: the user's CAs
// (trust.Read) and the company proxy, the environment's, else
// config.toml's, checked, with config.toml's password saved
// (settings.CompanyProxy.Upstream).
func shared(ctx context.Context, p *Proxy) (common, error) {
	cas, err := trust.Read(p.Dirs.TrustedCAs())
	if err != nil {
		return common{}, err
	}
	upstream, err := p.Company().Upstream(ctx)
	if err != nil {
		return common{}, err
	}
	return common{cas: cas, upstream: upstream}, nil
}

// prepare is p's Prepare, and reports whether it stopped the VM's proxy,
// or tried to, or started one: what fails before leaves the proxy as it
// was.
func prepare(ctx context.Context, p *Proxy, b backend.Backend, st *state.VM, running bool, agent string) (_ backend.Confinement, touched bool, _ error) {
	vm, tier := st.Name(), tierNow(p, st)
	none := backend.Confinement{}
	// What refuses every VM's proxy, first: nothing is written before.
	sh, err := shared(ctx, p)
	if err != nil {
		return none, false, err
	}
	run, err := b.Running(ctx, vm)
	if err != nil {
		return none, false, err
	}
	net := tier.Net()
	if running && run.Confined {
		net = backend.NetProxy
	}
	key, err := Key(ctx, st)
	if err != nil {
		return none, false, err
	}
	if err := p.secretsReady(ctx, st); err != nil {
		return none, false, err
	}
	// A stopped VM's proxy follows the hostagent it started with: a new
	// start gets a new one. A running VM's proxy starts again, on the same
	// port, when its config would differ from the one it started with
	// (stale).
	cfg, err := p.config(st, tier, key, run, sh)
	if err != nil {
		return none, false, err
	}
	cfg.Agent, cfg.AgentSocket = relayedAgent(p.Dirs, vm, running, agent)
	if stale := p.stale(vm, cfg); !running || stale {
		// Never the old one kept: it would go on with what it started with.
		touched = true
		if !p.Stop(vm) {
			return none, touched, msg.ProxyStopFailed(vm)
		}
	}
	sp, err := p.Ports().SSH(ctx, st, running)
	if err != nil {
		return none, touched, err
	}
	_, up := p.PID(vm)
	pp, err := p.proxyPort(ctx, st, up, running)
	if err != nil {
		return none, touched, err
	}
	conf := backend.Confinement{
		Net: net, ProxyPort: pp, ProxyPassword: key, Env: Env, SSHPort: sp, AgentSocket: cfg.AgentSocket,
	}
	// The logs hold each VM's requests: the user's alone.
	for _, d := range []string{p.Dirs.Run(), p.Dirs.VM(vm).Run(), p.Dirs.VM(vm).Logs()} {
		if err := state.PrivateDir(d); err != nil {
			return none, touched, err
		}
	}
	if up {
		return conf, touched, nil
	}
	cfg.Listen = listenOn(pp)
	return conf, true, p.start(vm, cfg)
}

// secretsReady readies what a VM's proxy puts its secrets in
// with: the VM's own CA, and the next one, which it trusts too
// (trust.InitCA), kept out of backups, as a copy of a key elsewhere could
// impersonate any site to the VM; and its secret files, with its salt.
func (p *Proxy) secretsReady(ctx context.Context, st *state.VM) error {
	ca := p.Dirs.VM(st.Name()).CA()
	if err := p.InitCA(ctx, ca); err != nil {
		return msg.ProxyCAFailed(ca.Dir).Because(err)
	}
	if p.OS == "darwin" {
		_ = p.ExcludeFromBackup(ctx, ca.Dir)
	}
	salt, err := Salt(ctx, st)
	if err != nil {
		return err
	}
	return p.Secrets.WriteFiles(ctx, st, salt)
}

// config is the config the VM's proxy would start with now, on tier t,
// listening on the proxy port the VM has (none: port 0), with its key
// (the password), following the lifeline of its run, and holding the
// logs its backend writes to their cap (Run.Logs). It holds everything that,
// changed, starts a running VM's proxy again: its tier and policies
// (tools added or removed, a layer of the user's), devvm's binary (an
// upgrade, which may change the built-in policies' rules), the VM's CAs
// (replaced, so it would mint certificates the VM no longer trusts after
// its next setup), the user's CAs, which it trusts for its own
// connections, the company proxy it goes through (upstream), both sh's
// (shared), and the shared Nix cache's cap. Every tier's proxy has the
// VM's CAs and its secrets: an open VM's applies no rules, but still puts
// its secrets in.
func (p *Proxy) config(st *state.VM, t Tier, key redact.Secret, run backend.Run, sh common) (proxy.Config, error) {
	vm, files := st.Name(), p.Dirs.VM(st.Name())
	ca := files.CA()
	set := Policies(p.Dirs, vm, t, image.Name(st.Image), tools.Names(st))
	for _, m := range PolicyWarnings(append(slices.Clone(set.Files), set.Denies...)) {
		p.Warn(m)
	}
	cas, upstream := sh.cas, sh.upstream
	pp, err := ports.Of(st)
	if err != nil {
		return proxy.Config{}, err
	}
	bin, _ := image.SHA256File(p.Self)
	c := proxy.Config{
		Listen: listenOn(pp.Host.Proxy), VM: vm, Password: key,
		BuiltinPolicies: set.Builtin, Policies: set.Files, DenyPolicies: set.Denies, Open: t == TierOpen,
		Log: files.EventLog(), ProxyLog: files.ProxyLog(), Lifeline: run.Lifeline, VMLogs: run.Logs,
		UpstreamCAs: string(cas.Bundle()), Upstream: upstream,
		Links: files.LinkList(), LinkSockets: p.Dirs.Run(),
		DevvmSHA256: bin,
		CA:          ca.Cert(), CAKey: ca.Key(), CANext: ca.NextCert(), CANextKey: ca.NextKey(),
		Secrets: files.Secrets(), Captures: agentlogin.Endpoints(),
	}
	c.CASHA256, _ = image.SHA256File(ca.Cert())
	c.CANextSHA256, _ = image.SHA256File(ca.NextCert())
	c.NixCache, c.NixCacheMax = p.nixCache(st)
	return c, nil
}

// Company is the company proxy as devvm's environment and config.toml
// have it now, with config.toml's password from p's secrets.
func (p *Proxy) Company() *settings.CompanyProxy {
	return settings.NewCompanyProxy(p.Dirs, p.Getenv, p.Secrets.ProxyPassword)
}

// stale reports whether the VM's proxy last started with another config
// than cfg (its Sum): one that's gone or can't be read counts as another.
// It warns when that proxy went through a company proxy and cfg goes
// through none, as a shell without the proxy settings would start it.
func (p *Proxy) stale(vm vmname.VM, cfg proxy.Config) bool {
	old, err := proxy.ReadConfig(p.Dirs.VM(vm).ProxyConfig())
	if err != nil {
		return true
	}
	if old.Upstream != nil && cfg.Upstream == nil {
		p.Warn(msg.UpstreamProxyGone(p.Dirs.ConfigFile(), vm))
	}
	return old.Sum() != cfg.Sum()
}

// listenOn is the proxy's address for port, on the host's loopback.
func listenOn(port int) string { return "127.0.0.1:" + strconv.Itoa(port) }

// Ports is the allocator of VMs' host ports, which asks Listening
// whether a program has one.
func (p *Proxy) Ports() ports.Allocator {
	return ports.Allocator{Dirs: p.Dirs, Listening: p.Listening, Range: p.PortRange, SSHList: p.SSHList}
}

// proxyPort returns the port of the VM's proxy: a new free one (Ports)
// when it has none, or when its proxy doesn't run and another program
// took it, or when it's one only VMs' ssh may have (ports.Allocator's
// SSHOnly), which an older devvm chose, unless the VM runs: its backend
// holds the port it started with.
func (p *Proxy) proxyPort(ctx context.Context, st *state.VM, up, running bool) (int, error) {
	pp, err := ports.Of(st)
	if err != nil {
		return 0, err
	}
	n := pp.Host.Proxy
	if up || n != 0 && !p.Listening(ctx, n) && (running || !p.Ports().SSHOnly(n)) {
		return n, nil
	}
	if running && n != 0 {
		return 0, msg.ProxyPortTaken(strconv.Itoa(n), st.Name())
	}
	// One another devvm chose meanwhile is kept while it's free.
	return p.Ports().Choose(ctx, st, ports.Proxy, func(cur int, _ ports.Ports) bool { return cur != n && !p.Listening(ctx, cur) })
}

// RefreshSecrets writes again the secret files of the VMs named (none:
// every VM) whose proxy has them, so a token saved, deleted, bound or
// unbound applies at once: a proxy reads its files again when they change,
// and looks its values up afresh. Its logins' bindings stay as its start
// wrote them. A VM whose files can't be written doesn't stop the others',
// so a secret deleted or unbound leaves every other VM's proxy at once,
// and the errors come back together.
func (p *Proxy) RefreshSecrets(ctx context.Context, vms ...vmname.VM) error {
	if len(vms) == 0 {
		all, err := state.VMNames(p.Dirs)
		if err != nil {
			return err
		}
		vms = all
	}
	var errs []error
	for _, vm := range vms {
		if _, err := os.Stat(p.Dirs.VM(vm).Secrets()); err != nil {
			continue
		}
		st, err := state.ReadVM(p.Dirs, vm)
		if err != nil || !st.Exists() {
			continue // not a VM devvm still has
		}
		n, err := Of(st)
		if err != nil || n.Made.ProxySalt == "" {
			continue
		}
		if err := p.Secrets.RefreshFiles(ctx, st, n.Made.ProxySalt); err != nil {
			errs = append(errs, err)
			continue
		}
		p.SecretsWritten(vm)
	}
	return errors.Join(errs...)
}

// SecretsWritten is what follows every write of vm's bindings file
// (secrets.Store's WriteFiles and RefreshFiles): a warning when the file is
// of a version after 1, for a binding by method or pattern, which a proxy
// from another devvm binary (otherBinary) may refuse whole, so it puts none
// of the VM's secrets in until it starts again.
func (p *Proxy) SecretsWritten(vm vmname.VM) {
	if l, err := proxy.ReadBindings(p.Dirs.VM(vm).Secrets()); err == nil && l.Version != 1 && otherBinary(p, vm) {
		p.Warn(msg.SecretsOtherBinary(vm))
	}
}

// start writes cfg, the VM's proxy's config, and starts the proxy with
// it, devvm _proxy -config FILE, waiting for it to listen. The config holds
// the password and the company proxy's, so it's the user's alone, and in
// no process's arguments, which other users can see.
func (p *Proxy) start(vm vmname.VM, cfg proxy.Config) error {
	files := p.Dirs.VM(vm)
	if err := p.writeConfig(vm, cfg); err != nil {
		return err
	}
	// The child writes the logs' folder: the log is opened never through a
	// link, and to read too, as the proxy rotates it through this file.
	out, err := proxy.OpenLog(cfg.ProxyLog)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	status := files.Proxy(".status")
	args := []string{"_proxy", "-config", files.ProxyConfig()}
	pid, err := p.proc(vm).Start(p.Start, daemon.Cmd{Path: p.Self, Args: args, Out: out, Status: status})
	if err != nil {
		return err
	}
	// Started means this proxy says it listens on the port, and still runs:
	// another program that took the port first would otherwise get the
	// VM's traffic.
	if said, ok := p.proc(vm).Ready(status, pid, p.Tick, 10); ok && said == cfg.Listen {
		return nil
	}
	return msg.ProxyStartFailed(cfg.ProxyLog, vm)
}

// writeConfig writes the VM's proxy's config whole, mode 600, so a proxy
// never reads half of it, and two devvms writing it at once leave one of
// theirs.
func (p *Proxy) writeConfig(vm vmname.VM, cfg proxy.Config) error {
	return state.WriteFile(p.Dirs.VM(vm).ProxyConfig(), cfg.Encode())
}

// nixCache is the proxy's shared Nix cache and its cap: only for a VM
// whose image has the nix layer, with the cap nix-cache.size gives (its
// default when the retention settings can't be read, with a warning).
func (p *Proxy) nixCache(st *state.VM) (string, int64) {
	if !image.Name(st.Image).Has(image.NixLayer) {
		return "", 0
	}
	def, err := settings.Read(p.Dirs)
	if err != nil {
		p.Warn(msg.RetentionUnreadable().Because(err))
	}
	return p.Dirs.NixCache(), def.Retention.NixCacheCap()
}
