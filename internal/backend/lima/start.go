package lima

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/daemon"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/redact"
	"github.com/devvmhq/devvm/internal/state"
)

// limaHost is the host's address on Lima's user-mode network
// (host.lima.internal), with vz and with QEMU: host loopback as a guest
// reaches it. An address, not the name, so a chroot with its own
// /etc/hosts reaches it too.
const limaHost = "192.168.5.2"

// limaHostName is limaHost's name in every Lima guest.
const limaHostName = "host.lima.internal"

// haPIDFile is the pid file of the VM's hostagent, in Lima's own folder
// for it.
func (l *Lima) haPIDFile(vm string) string { return filepath.Join(l.Dirs.LimaVM(vm), "ha.pid") }

// Start starts a stopped VM as w wants it, confined as c asks, and
// returns once it answers over ssh. A running VM is left as it is.
//
// What differs of w and c is changed in one limactl edit, none when
// nothing does: any edit gives the VM new ssh host keys at its next boot.
// The VM's guest environment (Lima's env:, written to /etc/environment on
// every boot) is c.Env, plus, for NetProxy, the proxy's address in
// HTTP_PROXY and HTTPS_PROXY (and lowercase), its password written into
// lima.yaml by devvm, never in limactl's arguments.
func (l *machines) Start(ctx context.Context, vm string, w backend.Want, c backend.Confinement) error {
	return l.start(ctx, vm, &w, c)
}

// start is Start, for a project VM (w set) or a build's VM (w nil: its
// config is left as it was made, but for c's).
func (l *machines) start(ctx context.Context, vm string, w *backend.Want, c backend.Confinement) error {
	if err := l.checkGlobalConfigs(); err != nil {
		return err
	}
	in, err := l.inspect(ctx, vm)
	if err != nil {
		return err
	}
	// devvm never gives a VM a network of its own: one would reach
	// around the proxy.
	if len(in.Config.Networks) > 0 {
		return msg.VMHasNetworks(filepath.Join(l.Dirs.LimaVM(vm), "lima.yaml"), vm)
	}
	if backend.Status(in.Status) == backend.StatusRunning {
		return nil
	}
	// A driver left running without its hostagent (a start cut short
	// on a slow host) holds the VM's disk, so a new one can't start:
	// it's stopped with --force first.
	if backend.Status(in.Status) == statusBroken && in.DriverPID != 0 && in.HostAgentPID == 0 {
		if err := l.stop(ctx, vm, "--force"); err != nil {
			return err
		}
	}
	sshPort := c.SSHPort
	if sshPort == 0 && c.Net != backend.NetDirect {
		// A build VM's, chosen as a VM's are and held until its hostagent
		// has it, so no VM's forwarder takes it first (on macOS its
		// profile names it too). Never the port its clone kept from its
		// image, so configure always edits the new one in.
		var release func()
		if sshPort, release, err = l.Ports.Build(ctx, vm, in.Config.SSH.LocalPort); err != nil {
			return err
		}
		defer release()
	}
	if err := configure(ctx, l.Lima, in, w, c, sshPort); err != nil {
		return err
	}
	// Its profile, wrapper and records: the user's alone.
	for _, d := range []string{l.Dirs.Run(), l.Dirs.VM(vm).Run()} {
		if err := state.PrivateDir(d); err != nil {
			return err
		}
	}
	// It forwards the agent its config now says.
	forwarding := in.backend().AgentForwarding
	if w != nil {
		forwarding = w.AgentForwarding
	}
	wrap, env, err := l.confine(vm, c, sshPort, forwarding)
	if err != nil {
		return err
	}
	// The record of its last start: "<hostagent's pid> <net>".
	if err := os.Remove(l.Dirs.VM(vm).Started()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	cmd := l.limactl(ctx, "start", "--tty=false", vm)
	if len(wrap) > 0 {
		// sandbox-exec runs limactl by its path, in the same folder.
		cmd.Args = append(slices.Clone(wrap), append([]string{cmd.Path}, cmd.Args[1:]...)...)
		cmd.Path = wrap[0]
	}
	cmd.Env = setEnv(cmd.Env, env...)
	defer l.redactLog(vm)
	if err := l.Runner.Run(ctx, msg.Starting(shown(ctx, vm)), cmd); err != nil {
		if err := l.degraded(ctx, vm, c.AgentSocket); err != nil {
			return err
		}
	}
	return l.recordStart(vm, c.Net)
}

// redactLog takes proxy passwords out of the VM's hostagent log, once
// limactl start has ended, started or not. The hostagent logs a proxy
// variable's value, password and all, as it reads it at its start (the
// limactl comment says when), and that log is Lima's, outside devvm's run
// log. Each password becomes as many "*" in place, so the hostagent, which
// keeps writing to the file at its own offset, goes on where it was.
func (l *machines) redactLog(vm string) {
	path := filepath.Join(l.Dirs.LimaVM(vm), "ha.stderr.log")
	if err := redact.File(path); err != nil {
		l.Warn(msg.LimaLogUnredacted(path, err.Error()).Because(err))
	}
}

// degraded decides on a start Lima reported as failed. Lima reports that
// for a VM that's running but degraded (one of its own boot scripts
// failed), which is usable: carry on with a warning. Unless it doesn't
// answer over ssh: then it never booted (QEMU can keep running a guest
// stuck in its firmware), and it's stopped. Its check carries agent, the
// start's.
func (l *machines) degraded(ctx context.Context, vm, agent string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	in, err := l.inspect(ctx, vm)
	if err != nil || backend.Status(in.Status) != backend.StatusRunning {
		return backend.Logged(msg.StartFailed(vm))
	}
	sctx, cancel := context.WithTimeout(ctx, l.SSHTimeout)
	defer cancel()
	if err := l.Exec(sctx, vm, []string{"true"}, backend.ExecOpts{Agent: agent}); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		_ = l.Runner.Run(ctx, msg.Stopping(vm), l.limactl(ctx, "stop", "--force", vm))
		return backend.Logged(msg.StartNoSSH(filepath.Join(l.Dirs.LimaVM(vm), "serial*.log"), vm))
	}
	l.Warn(msg.StartedWithWarnings(vm))
	return nil
}

// confine returns how limactl start runs confined: the command it runs
// under (sandbox-exec and its profile; none), and the environment it adds
// (KEY=value, or a bare KEY to remove).
func (l *machines) confine(vm string, c backend.Confinement, sshPort int, forwarding bool) (wrap, env []string, err error) {
	// Lima's processes forward the agent they find at start: the VM's
	// own, or none (limactl gives it none).
	agent := c.AgentSocket
	if agent != "" {
		env = append(env, "SSH_AUTH_SOCK="+agent)
	}
	// A VM on Lima's own network without devvm's proxy gets the host's
	// proxy settings from Lima (propagateProxyEnv), which limactl
	// otherwise runs without.
	if c.Net == backend.NetDirect && !c.GuestProxy() {
		for _, k := range backend.ClientProxyVars() {
			if v := l.Getenv(k); v != "" {
				env = append(env, k+"="+v)
			}
		}
	}
	switch l.OS {
	case "darwin":
		p := profile{LimaDir: l.Dirs.LimaVM(vm), SSHPort: sshPort}
		switch c.Net {
		case backend.NetDirect:
			// Unconfined, an open VM's included: no profile. Its guest
			// reaches devvm's proxy on host loopback as Lima's network has
			// it (proxyURL).
			return nil, env, nil
		case backend.NetProxy:
			p.ProxyPort, p.NoBinds = c.ProxyPort, true
			if !forwarding {
				agent = ""
			}
			p.AgentSocket = agent
		case backend.NetNone:
			p.AgentSocket = agent
		case backend.NetInternet:
			// A build's own proxy, behind a company proxy (buildproxy.go).
			p.Internet, p.ProxyPort = true, c.ProxyPort
		}
		if err := writeProfile(l.Dirs.VM(vm).Profile(), p); err != nil {
			return nil, nil, err
		}
		return []string{sandboxExec, "-f", l.Dirs.VM(vm).Profile()}, env, nil
	case "linux":
		switch c.Net {
		case backend.NetDirect, backend.NetInternet:
			// QEMU can't keep the internet and drop host loopback. An open
			// VM's guest reaches devvm's proxy as host loopback, without
			// the wrapper (proxyURL).
			return nil, env, nil
		case backend.NetProxy, backend.NetNone:
		}
		w, err := writeWrapper(l.Dirs.QEMUWrapper(), l.Self, l.Arch)
		if err != nil {
			return nil, nil, err
		}
		env = append(env, qemuEnvVar(l.Arch)+"="+w)
		if c.Net == backend.NetProxy {
			env = append(env, GuestFwdEnv+"="+guestFwdCmd(l.Self, "127.0.0.1:"+strconv.Itoa(c.ProxyPort)))
		} else {
			env = append(env, GuestFwdEnv) // nothing forwarded
		}
		return nil, env, nil
	}
	return nil, nil, msg.LimaHostUnsupported(l.OS)
}

// configure edits a stopped VM's config (in, as limactl list has it) to
// match what w wants of it (wantEdits; nothing for nil) and how it starts
// with c and sshPort (startSets), in one edit and only where it doesn't
// already, then fills in the proxy's password.
func configure(ctx context.Context, l *Lima, in instance, w *backend.Want, c backend.Confinement, sshPort int) error {
	var opts []string
	if w != nil {
		var err error
		if opts, err = wantEdits(in, *w); err != nil {
			return err
		}
	}
	sets, fill := l.startSets(in.Config, c, sshPort)
	opts = append(opts, sets...)
	if len(opts) == 0 {
		return nil
	}
	vm := in.Name
	if err := l.edit(ctx, vm, opts...); err != nil {
		return err
	}
	if fill {
		return l.fillPassword(vm, c)
	}
	return nil
}

// passwordStandIn stands for the proxy's password in the edit that sets
// the VM's environment: limactl's arguments are visible to other users, so
// devvm writes the password into the VM's lima.yaml itself (fillPassword).
const passwordStandIn = "devvm-proxy-password" //nolint:gosec // a stand-in, no password

// startSets returns the --set edits that make a VM's config match how it
// starts, none when it already does: an edit gives the VM new ssh host keys
// at its next boot (cloud-init sees a new instance). fill reports that the
// edits hold passwordStandIn for fillPassword to replace.
func (l *machines) startSets(cfg *config, c backend.Confinement, sshPort int) (opts []string, fill bool) {
	var sets []string
	if sshPort != 0 && cfg.SSH.LocalPort != sshPort {
		sets = append(sets, ".ssh.localPort="+strconv.Itoa(sshPort))
	}
	proxy := c.Net == backend.NetProxy
	if env := l.guestEnv(c); !maps.Equal(env, cfg.Env) && (len(env) > 0 || len(cfg.Env) > 0) {
		if len(env) == 0 {
			sets = append(sets, "del(.env)")
		} else {
			stand := c
			stand.ProxyPassword = passwordStandIn
			sets = append(sets, ".env = "+yqJSON(l.guestEnv(stand)))
			fill = c.GuestProxy()
		}
	}
	// The host's own proxy settings only for a VM on Lima's own network
	// without devvm's proxy: Lima writes them into the guest's
	// /etc/environment, which a build VM's image would keep, password and
	// all (its scripts get them on their input instead: buildproxy.go),
	// and a project VM goes through devvm's proxy, which goes through the
	// company proxy itself.
	direct := c.Net == backend.NetDirect && !c.GuestProxy()
	propagates := cfg.PropagateProxyEnv == nil || *cfg.PropagateProxyEnv
	if !direct && propagates {
		sets = append(sets, ".propagateProxyEnv=false")
	} else if direct && !propagates {
		sets = append(sets, ".propagateProxyEnv=true")
	}
	// Names resolve in the proxy. On Linux restrict=on drops Lima's DNS
	// too, so lookups would time out; on macOS Lima's host resolver binds
	// loopback ports, which the sandbox denies (seatbelt.go). A resolver
	// nothing listens on fails at once.
	noDNS := proxy
	resolver := cfg.HostResolver.Enabled == nil || *cfg.HostResolver.Enabled
	switch {
	case noDNS && resolver:
		sets = append(sets, ".hostResolver.enabled=false")
	case !noDNS && !resolver:
		sets = append(sets, "del(.hostResolver)")
	}
	switch {
	case noDNS && !slices.Equal(cfg.DNS, []string{"127.0.0.1"}):
		sets = append(sets, `.dns=["127.0.0.1"]`)
	case !noDNS && len(cfg.DNS) > 0:
		sets = append(sets, "del(.dns)")
	}
	// Lima forwards no port: devvm's forwarder does (ports.go).
	if !forwardsNone(cfg.PortForwards) {
		sets = append(sets, noForwardsSet())
	}
	for _, s := range sets {
		opts = append(opts, "--set", s)
	}
	return opts, fill
}

// guestEnv is the VM's guest environment as c has it: c.Env, and where
// the guest's proxy settings name devvm's proxy (GuestProxy), its
// address. An open VM reaches this machine as Lima's network has it
// (host.lima.internal) without the proxy, which can't resolve that name:
// its NO_PROXY names it.
func (l *machines) guestEnv(c backend.Confinement) map[string]string {
	env := maps.Clone(c.Env)
	if env == nil {
		env = map[string]string{}
	}
	if !c.GuestProxy() {
		return env
	}
	u := l.proxyURL(c)
	for _, k := range backend.GuestProxyVars() {
		env[k] = u
	}
	if c.Net == backend.NetDirect {
		for _, k := range []string{"NO_PROXY", "no_proxy"} {
			if v, ok := env[k]; ok {
				env[k] = v + "," + limaHostName + "," + limaHost
			}
		}
	}
	return env
}

// fillPassword replaces the proxy URL with passwordStandIn in the VM's
// lima.yaml, as limactl edit wrote it, with the real one: the file, unlike
// a process's arguments, is the user's alone (devvm writes it 0600).
func (l *machines) fillPassword(vm string, c backend.Confinement) error {
	path := filepath.Join(l.Dirs.LimaVM(vm), "lima.yaml")
	data, err := os.ReadFile(path) //nolint:gosec // the VM's own config, in devvm's Lima home
	if err != nil {
		return err
	}
	stand := c
	stand.ProxyPassword = passwordStandIn
	from, to := l.proxyURL(stand), l.proxyURL(c)
	// The URL goes into YAML as it is: only characters no YAML scalar
	// treats specially.
	if strings.Trim(to, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._~%:/@") != "" {
		return fmt.Errorf("lima: %s: the proxy's URL can't go into lima.yaml", vm)
	}
	if !bytes.Contains(data, []byte(from)) {
		return fmt.Errorf("lima: %s: lima.yaml has no proxy settings to fill in", vm)
	}
	return state.WriteFile(path, bytes.ReplaceAll(data, []byte(from), []byte(to)))
}

// proxyURL is devvm's proxy as the guest reaches it, with the VM's
// password: the host's address on Lima's network and the proxy's own port
// on macOS, and for an open VM (NetDirect), whose user-mode network
// reaches host loopback there on Linux too; else on Linux QEMU's guestfwd
// address.
func (l *machines) proxyURL(c backend.Confinement) string {
	if l.OS == "darwin" || c.Net == backend.NetDirect {
		return backend.ProxyURL(limaHost, c.ProxyPort, c.ProxyPassword)
	}
	return backend.ProxyURL(guestFwdHost, guestFwdPort, c.ProxyPassword)
}

// recordStart notes the hostagent Lima started for the VM, which a
// confined start started confined (a process can't leave its sandbox).
func (l *machines) recordStart(vm string, n backend.Net) error {
	pid, err := os.ReadFile(l.haPIDFile(vm))
	if errors.Is(err, os.ErrNotExist) {
		return nil // no hostagent to record: Running says it's not confined
	} else if err != nil {
		return err
	}
	return state.WriteFile(l.Dirs.VM(vm).Started(), fmt.Appendf(nil, "%s %d\n", strings.TrimSpace(string(pid)), n))
}

// Running reads the VM's hostagent's pid file, its lifeline: the process
// that runs the VM and its network, which Lima removes when the VM stops.
// Each start writes it anew, so the pid it holds and when it was written
// are the start's ID. The VM is confined when it runs on the hostagent
// Start started with NetProxy (recordStart): anything else (limactl
// start) is a new hostagent, unconfined.
func (l *machines) Running(_ context.Context, vm string) (backend.Run, error) {
	r := backend.Run{Lifeline: l.haPIDFile(vm)}
	fi, err := os.Stat(r.Lifeline)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	} else if err != nil {
		return r, err
	}
	ha, err := os.ReadFile(r.Lifeline)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil // stopped since
	} else if err != nil {
		return r, err
	}
	pid := strings.TrimSpace(string(ha))
	r.ID = pid + " " + fi.ModTime().UTC().Format(time.RFC3339Nano)
	rec, err := os.ReadFile(l.Dirs.VM(vm).Started())
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	} else if err != nil {
		return r, err
	}
	want, n, _ := strings.Cut(strings.TrimSpace(string(rec)), " ")
	p, err := strconv.Atoi(pid)
	if err != nil || strconv.Itoa(p) != want || n != strconv.Itoa(int(backend.NetProxy)) {
		return r, nil //nolint:nilerr // an unreadable pid is no hostagent of devvm's
	}
	r.Confined = daemon.Alive(p)
	return r, nil
}

// ProxyURL is devvm's proxy as a VM started with c reaches it.
func (l *machines) ProxyURL(c backend.Confinement) string {
	if !c.GuestProxy() {
		return ""
	}
	return l.proxyURL(c)
}
