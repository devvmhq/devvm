// Package vmssh is what devvm runs over a VM's ssh on the host, on every
// backend, none of it a backend's: the port forwarder (Forwarder, run as
// devvm _ports by its one owner, Forwards, which reads the ports a VM
// forwards from its state and how to reach it from its backend), the
// sessions that carry links between VMs (Links), and the one record of
// each VM's host keys for its start that every ssh of devvm's to the VM
// checks (PinHostKeys).
package vmssh

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/daemon"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/proxy"
)

// The forwarder forwards a VM's ports to the host's loopback, by the same
// rules on every backend, where Lima's own forwards would share a port a
// host program has (SO_REUSEADDR). It runs on the host, detached (devvm
// _ports; Forwards), one per VM:
//
//   - it reaches the VM only through the VM's sshd, as its backend says
//     (its SSH, as FromTarget checks it: a user, a key, a host loopback port), the
//     host key devvm pins for the VM pinned too: one ssh connection (a master), whose session runs ports.sh,
//     which reports what listens in the VM; each forwarded connection is a
//     channel of it (ssh -W through the master), which the VM's sshd opens
//     to the VM's own loopback;
//   - it listens on the host's loopback alone (127.0.0.1 and ::1), on the
//     port the VM listens on, for servers on the VM's loopback or all its
//     addresses, but never a link's listener, in 127.77.0.0/16
//     (proxy.IsLinkAddr), nor the VM's local proxy, which adds the VM's
//     password (proxy.IsLocalProxyAddr): every port from 1024 up, or the VM's listed ports alone
//     (Only); never on a port the host has on any address (no SO_REUSEADDR:
//     platform.ListenExclusive), nor on a port devvm records for any VM
//     (ssh, proxy, desktop), nor one another running VM forwards or
//     lists but the VM's own listed ones (ports.Reserved), which it
//     forwards once they're free;
//   - every connection starts on the host: the VM opens none to the host
//     through it, and the master asks for no forwarding into the host;
//   - it ends once the VM no longer runs as it did when it started (Alive),
//     and devvm stops it when it stops the VM.

// Limits on what the VM's report makes the forwarder do.
const (
	minForwardPort = 1024 // privileged ports aren't forwarded unless listed
	maxListeners   = 4096 // lines in one report
	maxRelays      = 256  // connections carried at once
)

// MaxForwards is the most ports a VM's forwarder forwards at once
// (ports_limit names it).
const MaxForwards = 256

// Forwarder forwards a running VM's ports to the host's loopback, as the
// VM reports them (Watch).
type Forwarder struct {
	VM string
	// Alive reports whether the VM still runs as it did when the forwarder
	// started (the same process, the same start): asked at the start,
	// every Every, and whenever its connection ends. Once it doesn't, the
	// forwarder ends.
	Alive func(ctx context.Context) bool
	Every time.Duration
	// Only is the ports it forwards: nil for every one from 1024 up, else
	// these alone (the VM's listed ports), whatever their number.
	Only []backend.PortRange
	// Reserved returns the host ports never forwarded, asked with each
	// report: a forward on one goes, and a port the VM listens on waits
	// until it's no longer reserved; while it fails, no new port is
	// forwarded.
	Reserved func() (map[int]bool, error)
	// Recheck is how often, besides each report, a forward whose port is
	// reserved now goes (release), whatever the VM reports: Every when
	// it's 0.
	Recheck time.Duration
	// Note, when set, records the host ports it forwards and those it
	// tried and couldn't take, by why, after each change (Forwards' record,
	// which other VMs' forwarders and devvm ports read).
	Note func(now []int, failed failedPorts)
	// Watch connects to the VM and runs ports.sh there, returning its
	// output and how to wait for it; it ends with ctx or the connection.
	Watch func(ctx context.Context) (io.ReadCloser, func() error, error)
	// Relay carries conn to host:port in the VM until either side ends,
	// or ctx does, which its forward's close ends, then closes conn.
	Relay func(ctx context.Context, conn net.Conn, host string, port int)
	// Listen listens on a host loopback address (HOST:PORT).
	Listen func(ctx context.Context, addr string) (net.Listener, error)
	// Log gets its lines.
	Log io.Writer
	// Tick is how long a lost connection waits to be made again.
	Tick time.Duration
	// Gap is the least time between two reports applied: one that comes
	// sooner waits, and the VM's ssh channel with it.
	Gap time.Duration

	mu      sync.Mutex
	ports   map[int]*forward
	skipped map[int]failure // not forwarded, and why: tried again only once the VM's listener goes and comes back
	relays  chan struct{}
}

// failure is why the forwarder couldn't take a port.
type failure int

const (
	failedInUse   failure = iota + 1 // a program on the host has it (EADDRINUSE)
	failedLimit                      // MaxForwards are forwarded already
	failedRefused                    // the host refused it (EACCES, EPERM), such as below 1024 on Linux
)

// failureOf is why a listen on a port failed, with ok when that's one
// the port keeps: in use, or refused. Any other error (EMFILE, ENOBUFS,
// EADDRNOTAVAIL...) says nothing of the port, so it's tried again at the
// next report.
func failureOf(err error) (failure, bool) {
	switch {
	case errors.Is(err, syscall.EADDRINUSE):
		return failedInUse, true
	case errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
		return failedRefused, true
	}
	return 0, false
}

// noIPv6 is whether a listen on ::1 failed because the host has no IPv6
// loopback: none on lo (EADDRNOTAVAIL), or no IPv6 at all (EAFNOSUPPORT,
// EPROTONOSUPPORT).
func noIPv6(err error) bool {
	return errors.Is(err, syscall.EADDRNOTAVAIL) || errors.Is(err, syscall.EAFNOSUPPORT) || errors.Is(err, syscall.EPROTONOSUPPORT)
}

// failedPorts is the ports the forwarder tried and couldn't take, by
// why, each sorted.
type failedPorts struct{ inUse, limit, refused []int }

// forward is a port being forwarded: its host listeners, and the address
// in the VM its connections go to.
type forward struct {
	lns  []net.Listener
	host string
	// conns is the connections it accepted that are open, which close
	// cuts, under the forwarder's mu.
	conns relayConns
}

func (f *Forwarder) logf(format string, args ...any) {
	_, _ = fmt.Fprintf(f.Log, "devvm ports: "+format+"\n", args...)
}

// Run forwards the VM's ports until ctx ends or the VM no longer runs as
// it did, making the connection again whenever it's lost. Its ssh master
// gets devvm's limit on open files: carrying maxRelays connections, it
// holds two descriptors for each.
func (f *Forwarder) Run(ctx context.Context) error {
	if !f.Alive(ctx) {
		return fmt.Errorf("%s isn't running", f.VM)
	}
	if err := platform.ShareFileLimit(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		defer cancel()
		for f.Alive(ctx) {
			select {
			case <-ctx.Done():
				return
			case <-time.After(f.Every):
			}
		}
		if ctx.Err() == nil {
			f.logf("%s stopped", f.VM)
		}
	}()
	f.ports, f.skipped, f.relays = map[int]*forward{}, map[int]failure{}, make(chan struct{}, maxRelays)
	if f.Note != nil {
		f.Note(nil, failedPorts{}) // an older record of the VM's names nothing now
	}
	defer f.closeAll()
	go func() {
		every := cmp.Or(f.Recheck, f.Every)
		for every > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(every):
				f.release()
			}
		}
	}()
	for ctx.Err() == nil {
		if err := f.session(ctx); err != nil && ctx.Err() == nil {
			f.logf("%s: %v", f.VM, err)
		}
		f.closeAll()
		if ctx.Err() == nil && !f.Alive(ctx) {
			f.logf("%s stopped", f.VM)
			return nil
		}
		select {
		case <-ctx.Done():
		case <-time.After(f.Tick):
		}
	}
	return nil
}

// session makes one connection to the VM and forwards what it reports
// until it ends.
func (f *Forwarder) session(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	out, wait, err := f.Watch(ctx)
	if err != nil {
		return err
	}
	defer func() { cancel(); _ = wait() }()
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 0, 128), 128)
	var report []string
	var last time.Time
	ready := false
	for sc.Scan() {
		if line := sc.Text(); line != "." {
			if len(report) >= maxListeners {
				return errors.New("its report is too long")
			}
			report = append(report, line)
			continue
		}
		if err := waitUntil(ctx, last.Add(f.Gap)); err != nil {
			return err
		}
		last = time.Now()
		f.apply(ctx, listeners(report))
		report = report[:0]
		if !ready {
			f.logf("forwarding %s's ports", f.VM)
			// What its start waits for (Forwards.Start).
			if err := daemon.Ready(""); err != nil {
				f.logf("%v", err)
			}
			ready = true
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return errors.New("the connection ended")
}

// waitUntil waits until t, or ctx ends.
func waitUntil(ctx context.Context, t time.Time) error {
	d := time.Until(t)
	if d <= 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// What the VM can make the forwarder log (logLimit): logBurst writes at
// once, then one per logEvery, each at most logWriteMax bytes.
const (
	logBurst    = 200
	logEvery    = 10 * time.Second
	logWriteMax = 512
)

// logLimit is the forwarder's log, which the VM can make it write to (its
// ports, its ssh's errors, and its sshd's stderr): limited, so it can't
// fill the host's disk, and without control characters but newlines and
// tabs (msg.Printable).
type logLimit struct {
	w   io.Writer
	now func() time.Time

	mu      sync.Mutex
	tokens  int
	at      time.Time
	dropped int
}

func newLogLimit(w io.Writer, now func() time.Time) *logLimit {
	return &logLimit{w: w, now: now, tokens: logBurst, at: now()}
}

func (l *logLimit) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if n := int(l.now().Sub(l.at) / logEvery); n > 0 {
		l.tokens, l.at = min(logBurst, l.tokens+n), l.at.Add(time.Duration(n)*logEvery)
	}
	if l.tokens == 0 {
		l.dropped++
		return len(p), nil
	}
	l.tokens--
	var b []byte
	if l.dropped > 0 {
		b = fmt.Appendf(b, "devvm ports: %d writes not logged\n", l.dropped)
		l.dropped = 0
	}
	b = append(b, msg.Printable(string(p[:min(len(p), logWriteMax)]))...)
	if len(p) > logWriteMax {
		b = append(b, "…\n"...)
	}
	_, err := l.w.Write(b)
	return len(p), err
}

// wanted reports whether the forwarder forwards a port the VM listens on:
// one of Only's, or any from 1024 up without it.
func (f *Forwarder) wanted(port int) bool {
	if f.Only == nil {
		return port >= minForwardPort
	}
	return backend.Ports{Ranges: f.Only}.Has(port)
}

// apply makes the forwards what the VM reports: port to address in the
// VM. A forward goes once the VM no longer listens there, or once devvm
// reserves its port; while the reserved ports can't be read, those it has
// stay. A reserved port the VM listens on is tried again at each report
// until it's free, as another VM gives it up, even one whose forward
// failed before. What it forwards then is noted (Note) when it changed.
func (f *Forwarder) apply(ctx context.Context, want map[int]string) {
	reserved, rerr := f.Reserved()
	if rerr != nil {
		f.logf("no new ports forwarded: devvm's ports unknown: %v", rerr)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	changed := false
	failed := maps.Clone(f.skipped)
	defer func() {
		if changed || !maps.Equal(failed, f.skipped) {
			f.note()
		}
	}()
	for port, fw := range f.ports {
		if _, ok := want[port]; !ok || reserved[port] {
			fw.close()
			delete(f.ports, port)
			changed = true
			f.logf("port %d: no longer forwarded", port)
		}
	}
	// A port past the limit is tried again once the forwarder is below
	// it; any other failure stays until the VM's listener goes.
	below := len(f.ports) < MaxForwards
	for port, why := range f.skipped {
		if _, ok := want[port]; !ok || why == failedLimit && below {
			delete(f.skipped, port)
		}
	}
	for _, port := range slices.Sorted(maps.Keys(want)) {
		host := want[port]
		if fw, ok := f.ports[port]; ok {
			fw.host = host
			continue
		}
		if rerr != nil || !f.wanted(port) {
			continue
		}
		if reserved[port] {
			// Tried again once it's free, whatever an earlier try found.
			delete(f.skipped, port)
			continue
		}
		if _, ok := f.skipped[port]; ok {
			continue
		}
		if f.try(ctx, port, host) {
			changed = true
		}
	}
}

// try forwards a port the VM listens on, or records why it can't
// (skipped): its limit of forwards reached, or the host's listen failing
// with an error that's the port's (failureOf). Any other error is logged
// alone, and the port is tried again at the next report. Its caller holds
// mu.
func (f *Forwarder) try(ctx context.Context, port int, host string) bool {
	if len(f.ports) >= MaxForwards {
		f.skipped[port] = failedLimit
		f.logf("port %d: not forwarded: %d forwarded already", port, MaxForwards)
		return false
	}
	fw, err := f.open(ctx, port)
	if err != nil {
		if why, ok := failureOf(err); ok {
			f.skipped[port] = why
		}
		f.logf("port %d: not forwarded: %v", port, err)
		return false
	}
	fw.host = host
	f.ports[port] = fw
	f.logf("port %d: forwarded", port)
	return true
}

// open listens on the port on the host's loopback, IPv4 and IPv6 (a host
// without IPv6 gets IPv4 alone), and serves it. A host that refuses ::1
// alone (EACCES, EPERM), with IPv4 fine, gets IPv4 alone too, logged: a
// security module denying IPv6 sockets would otherwise stop every
// forward. An IPv6 listen failing otherwise fails the port as an IPv4 one
// would (try): a program on ::1 (EADDRINUSE) makes it in use, as
// localhost would reach either, and an error that says nothing of the
// port (EMFILE, ENOBUFS...) has it tried again at the next report, rather
// than forwarded on IPv4 alone.
func (f *Forwarder) open(ctx context.Context, port int) (*forward, error) {
	p := strconv.Itoa(port)
	l4, err := f.Listen(ctx, net.JoinHostPort("127.0.0.1", p))
	if err != nil {
		return nil, err
	}
	fw := &forward{lns: []net.Listener{l4}}
	switch l6, err := f.Listen(ctx, net.JoinHostPort("::1", p)); {
	case err == nil:
		fw.lns = append(fw.lns, l6)
	case noIPv6(err):
	case errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
		f.logf("port %d: forwarded on IPv4 alone: ::1 refused: %v", port, err)
	default:
		fw.close()
		return nil, err
	}
	for _, l := range fw.lns {
		go f.serve(ctx, l, port, fw)
	}
	return fw, nil
}

// serve relays each connection to a listener to the port in the VM.
func (f *Forwarder) serve(ctx context.Context, l net.Listener, port int, fw *forward) {
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		select {
		case f.relays <- struct{}{}:
		default:
			_ = conn.Close() // too many at once
			continue
		}
		// Each relay's own context, which the forward's close ends, so its
		// ssh is killed even when the VM never ends its channel.
		rctx, cancel := context.WithCancel(ctx)
		f.mu.Lock()
		host, open := fw.host, fw.conns.add(conn, cancel)
		f.mu.Unlock()
		if !open {
			cancel()
			_ = conn.Close() // accepted as the forward went
			<-f.relays
			continue
		}
		go func() {
			defer func() {
				f.mu.Lock()
				fw.conns.done(conn)
				f.mu.Unlock()
				cancel()
				<-f.relays
			}()
			f.Relay(rctx, conn, host, port)
		}()
	}
}

// close ends the forward: its listeners, and the connections they
// accepted that are open, with their relays' contexts, so a client's
// keep-alive connection, such as a browser's, never reaches the VM once
// its port is given up, and a relay whose channel the VM never ends
// stops with it. Its caller holds the forwarder's mu.
func (fw *forward) close() {
	for _, l := range fw.lns {
		_ = l.Close()
	}
	fw.conns.cutAll()
}

func (f *Forwarder) closeAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.ports) > 0 || len(f.skipped) > 0 {
		defer f.note()
	}
	for port, fw := range f.ports {
		fw.close()
		delete(f.ports, port)
	}
	clear(f.skipped)
}

// note records what it forwards and the ports it couldn't take, by why
// (Note). Its caller holds mu.
func (f *Forwarder) note() {
	if f.Note == nil {
		return
	}
	var failed failedPorts
	for _, port := range slices.Sorted(maps.Keys(f.skipped)) {
		switch f.skipped[port] {
		case failedInUse:
			failed.inUse = append(failed.inUse, port)
		case failedLimit:
			failed.limit = append(failed.limit, port)
		case failedRefused:
			failed.refused = append(failed.refused, port)
		}
	}
	f.Note(slices.Sorted(maps.Keys(f.ports)), failed)
}

// release closes the forwards whose ports devvm reserves now, noting what
// it forwards then: on the host's own clock (Recheck), so a VM that holds
// its connection open and never reports again can't keep a port another
// VM has a claim on. While the reserved ports can't be read, the forwards
// stay.
func (f *Forwarder) release() {
	reserved, err := f.Reserved()
	if err != nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	changed := false
	for port, fw := range f.ports {
		if reserved[port] {
			fw.close()
			delete(f.ports, port)
			changed = true
			f.logf("port %d: no longer forwarded", port)
		}
	}
	if changed {
		f.note()
	}
}

// listeners reads ports.sh's report, /proc/net/tcp's local addresses
// (ADDRESS:PORT in hex), as the address in the VM each port's
// connections go to: 127.0.0.1 for a server on all IPv4 addresses or
// that one, ::1 for one on all IPv6 addresses or that one, another
// loopback address for one there alone. Servers on other addresses
// alone aren't forwarded, nor are lines that aren't addresses.
func listeners(report []string) map[int]string {
	found := map[int][]netip.Addr{}
	for _, line := range report {
		a, p, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok || len(p) != 4 {
			continue
		}
		port, err := strconv.ParseUint(p, 16, 16)
		ip, iok := procAddr(a)
		if err != nil || port == 0 || !iok {
			continue
		}
		if to, ok := dialAddr(ip); ok {
			found[int(port)] = append(found[int(port)], to)
		}
	}
	out := map[int]string{}
	v4, v6 := netip.MustParseAddr("127.0.0.1"), netip.IPv6Loopback()
	for port, addrs := range found {
		switch {
		case slices.Contains(addrs, v4):
			out[port] = v4.String()
		case slices.Contains(addrs, v6):
			out[port] = v6.String()
		default:
			slices.SortFunc(addrs, func(a, b netip.Addr) int { return a.Compare(b) })
			out[port] = addrs[0].String()
		}
	}
	return out
}

// procAddr reads an address as /proc/net/tcp has it: 4 or 16 bytes in
// hex, each 32-bit word in the host's order (little-endian on devvm's).
func procAddr(h string) (netip.Addr, bool) {
	b, err := hex.DecodeString(h)
	if err != nil || len(b) != 4 && len(b) != 16 {
		return netip.Addr{}, false
	}
	for i := 0; i < len(b); i += 4 {
		b[i], b[i+1], b[i+2], b[i+3] = b[i+3], b[i+2], b[i+1], b[i]
	}
	a, ok := netip.AddrFromSlice(b)
	return a.Unmap(), ok
}

// dialAddr is where in the VM a forwarded connection to a server on a goes:
// its loopback alone. A link's listener (proxy.IsLinkAddr) is never forwarded,
// so a link isn't put on the host's loopback, and neither is the VM's local
// proxy (proxy.IsLocalProxyAddr), which would let the host's programs use the
// VM's proxy without its password, nor transparent mode's listeners on the
// stand-in address (proxy.IsSentinelAddr), which add it too: that one isn't
// loopback, and it's named here so that stays so whatever this switch takes.
func dialAddr(a netip.Addr) (netip.Addr, bool) {
	switch {
	case proxy.IsSentinelAddr(a):
		return netip.Addr{}, false
	case a == netip.IPv4Unspecified():
		return netip.MustParseAddr("127.0.0.1"), true
	case a == netip.IPv6Unspecified():
		return netip.IPv6Loopback(), true
	case a.IsLoopback() && !proxy.IsLinkAddr(a) && !proxy.IsLocalProxyAddr(a):
		return a, true
	}
	return netip.Addr{}, false
}

// SSH is how the forwarder reaches a VM's sshd, as the VM's user with its
// key, its pinned host key, and the master's control socket, on a host
// loopback port (Port).
type SSH struct {
	VM, User, Key, KnownHosts, Control string
	Port                               int
}

// Loopback is the one address devvm's own ssh, and the ssh config's
// entries, reach a VM's sshd at: the host's loopback.
const Loopback = "127.0.0.1"

// FromTarget is how devvm's own ssh reaches vm as its backend says (t),
// once t is checked: a user, an absolute key file, and a port on the
// host's loopback (Loopback), never another address, so ssh connects
// to that VM's forward alone. The user and key go into ssh config lines
// too (sshconfig), so neither may break one. KnownHosts and Control are
// the caller's.
func FromTarget(vm string, t backend.SSHTarget) (SSH, error) {
	switch {
	case t.User == "" || t.Key == "":
		return SSH{}, fmt.Errorf("%s: no user or key for its ssh", vm)
	case strings.ContainsAny(t.User, " \t\"#\r\n"):
		return SSH{}, fmt.Errorf("%s: its ssh user %q isn't one word", vm, t.User)
	case !strings.HasPrefix(t.Key, "/") || strings.ContainsAny(t.Key, "\"\r\n"):
		return SSH{}, fmt.Errorf("%s: its ssh key %q isn't a file's absolute path", vm, t.Key)
	case t.Host != Loopback || t.Port <= 0 || t.Port > 65535:
		return SSH{}, fmt.Errorf("%s: its ssh isn't on a host loopback port", vm)
	}
	return SSH{VM: vm, User: t.User, Key: t.Key, Port: t.Port}, nil
}

// connArgs are ssh's options for reaching the VM: no configuration of the
// user's, the VM's key alone and no agent, its host key pinned, and
// nothing forwarded either way.
func (o SSH) connArgs() []string {
	return []string{
		"-o", "BatchMode=yes",
		"-o", "IdentitiesOnly=yes", "-o", "IdentityAgent=none", "-o", "IdentityFile=" + o.Key,
		"-o", "PreferredAuthentications=publickey", "-o", "AddKeysToAgent=no",
		"-o", "UserKnownHostsFile=" + o.KnownHosts, "-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "HostKeyAlias=" + o.VM, "-o", "StrictHostKeyChecking=yes", "-o", "UpdateHostKeys=no",
		"-o", "ForwardAgent=no", "-o", "ForwardX11=no", "-o", "Tunnel=no", "-o", "PermitRemoteOpen=none",
		"-o", "ClearAllForwardings=yes", "-o", "PermitLocalCommand=no", "-o", "ProxyJump=none",
		"-o", "ConnectTimeout=10", "-o", "LogLevel=ERROR",
		"-o", "ProxyCommand=none", "-p", strconv.Itoa(o.Port), "-l", o.User, Loopback,
	}
}

// CommandArgs are ssh's arguments for running command in the VM, without
// a terminal: no ssh config (-F none), then connArgs, so ssh reaches the
// VM alone, whatever host its caller was asked for.
func (o SSH) CommandArgs(command string) []string {
	return slices.Concat([]string{"-F", "none", "-T"}, o.connArgs(), []string{command})
}

// masterArgs are ssh's arguments for the master connection, ports.sh run
// in its session.
func (o SSH) masterArgs() []string {
	return slices.Concat(
		[]string{"-F", "none", "-M", "-S", o.Control, "-T", "-o", "ControlPersist=no", "-o", "ServerAliveInterval=10", "-o", "ServerAliveCountMax=3"},
		o.connArgs(), []string{"sh -s"},
	)
}

// relayArgs are ssh's arguments for one forwarded connection: a channel of
// the master's to host:port in the VM. Without the master, ssh connects
// on its own: to the VM, as the master does, never anywhere else.
func (o SSH) relayArgs(host string, port int) []string {
	return slices.Concat(
		[]string{"-F", "none", "-S", o.Control, "-o", "ControlMaster=no", "-W", net.JoinHostPort(host, strconv.Itoa(port))},
		o.connArgs(),
	)
}

// watch starts the master, ports.sh fed to its session.
func (o SSH) watch(script []byte, log io.Writer) func(ctx context.Context) (io.ReadCloser, func() error, error) {
	return func(ctx context.Context) (io.ReadCloser, func() error, error) {
		if err := os.Remove(o.Control); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, nil, err
		}
		cmd := exec.CommandContext(ctx, "ssh", o.masterArgs()...) //nolint:gosec // devvm's own arguments
		cmd.Stdin, cmd.Stderr = bytes.NewReader(script), log
		out, err := cmd.StdoutPipe()
		if err != nil {
			return nil, nil, err
		}
		if err := cmd.Start(); err != nil {
			return nil, nil, err
		}
		return out, cmd.Wait, nil
	}
}

// relay carries conn through a channel of the master's.
func (o SSH) relay(ctx context.Context, conn net.Conn, host string, port int) {
	pipeSSH(ctx, conn, o.relayArgs(host, port))
}

// Flags are the forwarder's flags (devvm _ports): how it reaches the VM
// (SSH), how it knows the VM runs (its lifeline, for Forwarder's Alive),
// and what it forwards.
type Flags struct {
	SSH
	reserve, only, lifeline string
}

// Register adds the flags to fs.
func (f *Flags) Register(fs *flag.FlagSet) {
	fs.StringVar(&f.VM, "vm", "", "the VM")
	fs.StringVar(&f.lifeline, "lifeline", "", "the VM's process's pid file: exit when it changes or its process is gone")
	fs.IntVar(&f.Port, "port", 0, "the host loopback port the VM's sshd is forwarded from")
	fs.StringVar(&f.User, "user", "", "the VM's user")
	fs.StringVar(&f.Key, "key", "", "the VM's own key")
	fs.StringVar(&f.KnownHosts, "known-hosts", "", "the VM's host keys for its start, a known hosts file, under the VM's name")
	fs.StringVar(&f.Control, "control", "", "the master's control socket")
	fs.StringVar(&f.reserve, "reserve", "", "host ports never forwarded, comma-separated")
	fs.StringVar(&f.only, "only", "", "the ports forwarded, as devvm ports lists them (5173,8000-8010); every one from 1024 up without it")
}

// Forwarder returns the forwarder the parsed flags make, ports.sh being
// script, reserved the host ports VM vm's forwarder never takes
// (Forwards.Reserved), its lines going to log. Its caller sets Alive,
// Every and Note.
func (f *Flags) Forwarder(script []byte, reserved func(vm string) (map[int]bool, error), log io.Writer) (*Forwarder, error) {
	if f.VM == "" || f.User == "" || f.Key == "" || f.KnownHosts == "" || f.Control == "" || f.Port <= 0 {
		return nil, errors.New("-vm, -port, -user, -key, -known-hosts and -control are needed")
	}
	own := map[int]bool{}
	for p := range strings.SplitSeq(f.reserve, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(p)); err == nil {
			own[n] = true
		}
	}
	var only []backend.PortRange
	if f.only != "" {
		var err error
		if only, err = backend.ParsePortRanges(f.only); err != nil {
			return nil, err
		}
	}
	log = newLogLimit(log, time.Now)
	return &Forwarder{
		VM: f.VM, Only: only, Log: log, Tick: 2 * time.Second, Gap: 500 * time.Millisecond,
		Reserved: func() (map[int]bool, error) {
			out, err := reserved(f.VM)
			if out == nil {
				out = map[int]bool{}
			}
			for p := range own {
				out[p] = true
			}
			return out, err
		},
		Watch: f.watch(script, log), Relay: f.relay, Listen: platform.ListenLoopback,
	}, nil
}
