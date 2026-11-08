package vmssh

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/proxy"
	"github.com/devvmhq/devvm/internal/state"
)

// Links carries a client VM's links to other VMs' ports (the design
// note's "Links between VMs"), on the host, detached (devvm _links), one
// per running client with links:
//
//   - one ssh connection of its own to the client, never the forwarder's:
//     a master started with ClearAllForwardings=yes, so it asks for
//     nothing at first, to which each link is added with -O forward -R
//     ADDR:PORT:SOCKET and from which it's removed with -O cancel. Each
//     link is a forward of its own, so one the client refuses (a program
//     there holds its address) fails that link alone (Failed);
//   - each link's end on the host is a unix socket, mode 600 in the run
//     folder (mode 700), named by a hash of the client, the address and
//     the port;
//   - the client's links are listed in a file (List) that its proxy
//     reads, so an HTTP client there that sends NAME:PORT to its proxy
//     reaches the same socket;
//   - each connection is checked again (Check), then carried by ssh -W
//     127.0.0.1:PORT through a master to the target that Check names, one
//     per target and start, with ProxyCommand=/usr/bin/false: the channel
//     goes through the master or nowhere, never a connection of its own;
//   - at most maxLinkConns connections at once; the next is refused and
//     logged (EventLinkLimit);
//   - it never starts a VM, and ends once the client no longer runs as it
//     did when it started (Alive) or has no links.

// maxLinkConns is the most connections a client's links carry at once.
const maxLinkConns = 256

// The events a link session writes to the client's event log.
const (
	EventLink        = "link"                // a connection carried
	EventLinkRefused = "link_refused"        // refused, with why
	EventLinkStopped = "link_target_stopped" // refused: the target doesn't run
	EventLinkLimit   = "link_limit"          // refused: maxLinkConns carried already
)

// Link is one link a session carries: the client listens on Addr:Port.
// Name and Target are what its events name: the client's name for it and
// the target VM. Hosts are the names the VM's proxy takes it by (List).
type Link struct {
	Name, Target string
	Addr         netip.Addr
	Port         int
	Hosts        []string
}

// linkKey is what identifies a link in its client: its address and port.
type linkKey struct {
	addr netip.Addr
	port int
}

func (l Link) key() linkKey { return linkKey{l.Addr, l.Port} }

// listen is the address the client listens on for the link.
func (l Link) listen() string { return net.JoinHostPort(l.Addr.String(), strconv.Itoa(l.Port)) }

// LinkTarget is how a connection reaches a link's target: its ssh, whose
// Control the session sets, and its start, which gets a master of its own.
type LinkTarget struct {
	SSH
	Start string
}

// Links carries a running client VM's links (Run).
type Links struct {
	VM string
	// SSH reaches the client; its Control is set by Run.
	SSH SSH
	// Alive reports whether the client still runs as it did when the
	// session started: asked at the start, every Every, and whenever its
	// connection ends. Once it doesn't, the session ends.
	Alive func(ctx context.Context) bool
	Every time.Duration
	// Want returns the client's links as they are now, asked every Sync:
	// a new one is added, one that's gone removed.
	Want func() ([]Link, error)
	Sync time.Duration
	// Check is asked for each connection: how to reach the link's target
	// now, or the event (EventLinkRefused, EventLinkStopped) and why not.
	Check func(ctx context.Context, l Link) (t LinkTarget, event, why string)
	// Event writes an event to the client's event log.
	Event func(event string, l Link, why string)
	// Failed is told of a link the client refused to listen for.
	Failed func(l Link, err error)
	// Dir is the folder of the sockets and the masters' control sockets:
	// devvm's run folder.
	Dir string
	// List is the file the client's proxy reads its links from
	// (proxy.LinkList, mode 600 in Dir): every link the client has,
	// carried or not, written whenever they change (writeList). Empty:
	// none.
	List string
	// Log gets its lines.
	Log io.Writer
	// Tick is how long a lost connection waits to be made again, and
	// Wait how long a master gets to be ready.
	Tick, Wait time.Duration
	// Start starts ssh with args, returning how to wait for it; Command
	// runs ssh with args to its end; Relay carries conn through ssh with
	// args until either side ends. Tests set them; Run sets them unset.
	Start   func(ctx context.Context, args []string) (func() error, error)
	Command func(ctx context.Context, args []string) error
	Relay   func(ctx context.Context, conn net.Conn, args []string)

	mu      sync.Mutex
	open    map[linkKey]*linkEnd
	failed  map[linkKey]bool // refused: tried again only once it's removed and added, or in the next connection
	masters map[string]*targetMaster
	conns   chan struct{}
	listed  []byte // the list as last written; nil: none written
}

// linkEnd is a link being carried: its socket and the link as last read.
type linkEnd struct {
	link Link
	ln   net.Listener
	sock string
}

// targetMaster is a master to a link's target, for one of its starts.
type targetMaster struct {
	start, ctl string
	ready      chan struct{} // closed once it's ready or failed (err)
	err        error
	p          *proc
	cancel     context.CancelFunc
}

// dead reports whether the master failed or ended.
func (m *targetMaster) dead() bool {
	select {
	case <-m.ready:
	default:
		return false
	}
	if m.err != nil {
		return true
	}
	select {
	case <-m.p.done:
		return true
	default:
		return false
	}
}

// proc is an ssh that runs: done is closed once it ends, with err.
type proc struct {
	done chan struct{}
	err  error
}

// logf writes one of the session's own lines, the only ones that start
// with logPrefix: a line in what it says (an error's) is indented.
func (l *Links) logf(format string, args ...any) {
	s := strings.ReplaceAll(fmt.Sprintf(format, args...), "\n", "\n\t")
	_, _ = io.WriteString(l.Log, logPrefix+s+"\n")
}

// linkName is the name of one of the session's files in Dir: a hash of
// what it's for, short, as a socket's path has fewer than 104 characters.
func (l *Links) linkName(ext string, parts ...string) string {
	return filepath.Join(l.Dir, "link-"+proxy.LinkHash(l.VM, parts...)+ext)
}

// Socket is a link's socket on the host, named as the VM's proxy expects
// it (proxy.LinkSocket).
func (l *Links) Socket(lk Link) string { return proxy.LinkSocket(l.Dir, l.VM, lk.Addr, lk.Port) }

// Control is the control socket of the session's master (target "") or
// of its master to a target's start.
func (l *Links) Control(target, start string) string {
	if target == "" {
		return l.linkName(".ctl", "session")
	}
	return l.linkName(".ctl", "target", target, start)
}

// defaults sets what Run needs that its caller left unset.
func (l *Links) defaults() {
	l.Log = newLogLimit(cmp.Or[io.Writer](l.Log, io.Discard), time.Now)
	if l.Start == nil {
		l.Start = l.startSSH
	}
	if l.Command == nil {
		l.Command = l.commandSSH
	}
	if l.Relay == nil {
		l.Relay = pipeSSH
	}
	l.Wait = cmp.Or(l.Wait, 15*time.Second)
}

// Run carries the client's links until ctx ends, the client no longer
// runs as it did, or it has no links, making its connection again
// whenever it's lost. Its ssh masters get devvm's limit on open files:
// each receives two descriptors for every connection it carries, up to
// maxLinkConns.
func (l *Links) Run(ctx context.Context) error {
	l.defaults()
	if !l.Alive(ctx) {
		return fmt.Errorf("%s isn't running", l.VM)
	}
	if err := platform.ShareFileLimit(); err != nil {
		return err
	}
	if err := state.PrivateDir(l.Dir); err != nil {
		return err
	}
	if ctl := l.Control("", ""); len(ctl) > ControlMax || strings.ContainsAny(ctl, ": \t\n") {
		return fmt.Errorf("%s: its path is too long for a socket, or has a colon or a space", ctl)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		defer cancel()
		for l.Alive(ctx) {
			select {
			case <-ctx.Done():
				return
			case <-time.After(l.Every):
			}
		}
	}()
	l.open, l.failed, l.conns = map[linkKey]*linkEnd{}, map[linkKey]bool{}, make(chan struct{}, maxLinkConns)
	for ctx.Err() == nil {
		none, err := l.session(ctx)
		if none {
			l.logf("%s", l.VM+lineNoLinks)
			return nil
		}
		if err != nil && ctx.Err() == nil {
			l.logf("%s: %v", l.VM, err)
		}
		if ctx.Err() == nil && !l.Alive(ctx) {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(l.Tick):
		}
	}
	l.logf("%s", l.VM+lineStopped)
	return nil
}

// session makes one connection to the client and carries its links until
// it ends, or reports that the client has none.
func (l *Links) session(ctx context.Context) (none bool, err error) {
	want, err := l.Want()
	if err != nil {
		return false, err
	}
	// Listed before they're carried: until then the proxy finds no socket
	// for them, and refuses them rather than ask its rules.
	l.writeList(want)
	if len(want) == 0 {
		return true, nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ssh := l.SSH
	ssh.Control = l.Control("", "")
	p, err := l.master(ctx, ssh)
	if err != nil {
		return false, err
	}
	l.mu.Lock()
	l.masters = map[string]*targetMaster{}
	clear(l.failed)
	l.mu.Unlock()
	defer l.closeAll()
	defer func() { cancel(); p.wait(masterEndWait) }()
	l.logf("carrying %s's links", l.VM)
	t := time.NewTicker(l.Sync)
	defer t.Stop()
	for {
		l.writeList(want)
		if len(want) == 0 {
			return true, nil
		}
		l.sync(ctx, ssh.Control, want)
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-p.done:
			return false, fmt.Errorf("the connection ended: %w", p.err)
		case <-t.C:
		}
		if w, err := l.Want(); err != nil {
			l.logf("%s's links unknown: %v", l.VM, err)
		} else {
			want = w
		}
	}
}

// master starts a master connection (ssh's own control socket) and waits
// for it to be ready.
func (l *Links) master(ctx context.Context, ssh SSH) (*proc, error) {
	if err := os.Remove(ssh.Control); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	wait, err := l.Start(ctx, ssh.linkMasterArgs())
	if err != nil {
		return nil, err
	}
	p := &proc{done: make(chan struct{})}
	go func() {
		p.err = wait()
		// ssh killed leaves its control socket: it goes with the master.
		if fi, err := os.Lstat(ssh.Control); err == nil && fi.Mode()&os.ModeSocket != 0 {
			_ = os.Remove(ssh.Control)
		}
		close(p.done)
	}()
	deadline := time.Now().Add(l.Wait)
	for {
		if fi, err := os.Lstat(ssh.Control); err == nil && fi.Mode()&os.ModeSocket != 0 {
			return p, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-p.done:
			return nil, fmt.Errorf("its ssh ended: %w", p.err)
		case <-time.After(50 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return nil, errors.New("its ssh didn't connect")
		}
	}
}

// sync makes the links carried those in want: each new one listened for
// in the client, through the master whose control socket is ctl, and
// each that's gone cancelled there.
func (l *Links) sync(ctx context.Context, ctl string, want []Link) {
	wanted := map[linkKey]Link{}
	for _, w := range want {
		wanted[w.key()] = w
	}
	var gone []*linkEnd
	var add []Link
	l.mu.Lock()
	for k, end := range l.open {
		if w, ok := wanted[k]; ok {
			end.link = w
			continue
		}
		gone = append(gone, end)
		delete(l.open, k)
	}
	for k := range l.failed {
		if _, ok := wanted[k]; !ok {
			delete(l.failed, k)
		}
	}
	for k, w := range wanted {
		if l.open[k] == nil && !l.failed[k] {
			add = append(add, w)
		}
	}
	l.mu.Unlock()
	for _, end := range gone {
		if err := l.Command(ctx, forwardArgs(ctl, "cancel", end.link.listen(), end.sock)); err != nil {
			l.logf("%s: not cancelled: %v", end.link.listen(), err)
		}
		_ = end.ln.Close()
		l.logf("%s: no longer carried", end.link.listen())
	}
	slices.SortFunc(add, compareLinks)
	for _, w := range add {
		l.add(ctx, ctl, w)
	}
}

// compareLinks orders links by address, then port.
func compareLinks(a, b Link) int { return cmp.Or(a.Addr.Compare(b.Addr), cmp.Compare(a.Port, b.Port)) }

// writeList writes the list the client's proxy reads (List): every link
// the client has (want), carried or not, each with its names, its address
// and port, its target and its socket. A link the session doesn't carry
// has no socket there, so the proxy refuses it rather than ask its rules,
// and a session that ended leaves its list for the same reason: devvm
// removes it with the VM. It's written whole, when it changed or is gone.
func (l *Links) writeList(want []Link) {
	if l.List == "" {
		return
	}
	list := proxy.LinkList{Version: proxy.LinkListVersion, Links: []proxy.LinkEntry{}}
	listed := slices.Clone(want)
	slices.SortFunc(listed, compareLinks)
	for _, lk := range listed {
		list.Links = append(list.Links, proxy.LinkEntry{
			Hosts: slices.Clone(lk.Hosts), Addr: lk.Addr.String(), Port: lk.Port, Target: lk.Target, Socket: l.Socket(lk),
		})
	}
	b, err := json.Marshal(list)
	if err != nil {
		return
	}
	if _, serr := os.Lstat(l.List); serr == nil && bytes.Equal(b, l.listed) {
		return
	}
	// Whole, so the proxy reads the old list or the new one.
	if err := state.WriteFile(l.List, append(b, '\n')); err != nil {
		l.logf("%s: %v", l.List, err)
		return
	}
	l.listed = b
}

// add listens on a link's socket and asks the client to listen for it.
func (l *Links) add(ctx context.Context, ctl string, w Link) {
	fail := func(line string, err error) {
		l.mu.Lock()
		l.failed[w.key()] = true
		l.mu.Unlock()
		l.logf("%s: %v", w.listen()+line, err)
	}
	sock := l.Socket(w)
	ln, err := listenSocket(ctx, sock)
	if err != nil {
		fail(lineNotCarried, err)
		return
	}
	if err := l.Command(ctx, forwardArgs(ctl, "forward", w.listen(), sock)); err != nil {
		_ = ln.Close()
		fail(lineRefused, err)
		l.Failed(w, err)
		return
	}
	end := &linkEnd{link: w, ln: ln, sock: sock}
	l.mu.Lock()
	l.open[w.key()] = end
	l.mu.Unlock()
	go l.serve(ctx, end)
	l.logf("%s", w.listen()+lineCarried)
}

// What a session's log says of each link it adds, after the address and
// port the client listens on, and of itself: ReadLinksLog reads them.
const (
	logPrefix      = "devvm links: "
	lineCarried    = ": carried"
	lineNotCarried = ": not carried"
	lineRefused    = ": refused by the VM" // the client didn't listen: a program there holds the address
	lineNoLinks    = " has no links"
	lineStopped    = " stopped"
)

// LinksReport is what a session's log says of the links it was asked to
// carry: those the client refused to listen for, and whether it has said
// what came of each of them, or that it ended or failed, so there's
// nothing more to wait for.
type LinksReport struct {
	Refused []Link
	Settled bool
}

// ReadLinksLog reads what a session for client vm logged (log, from its
// start) of links: which of them the client refused, and whether every
// one has a line of its own or the session ended or failed first.
func ReadLinksLog(log []byte, vm string, links []Link) LinksReport {
	var r LinksReport
	said := map[linkKey]bool{}
	for line := range strings.SplitSeq(string(log), "\n") {
		rest, ok := strings.CutPrefix(line, logPrefix)
		if !ok {
			continue
		}
		switch {
		case rest == vm+lineNoLinks, rest == vm+lineStopped:
			r.Settled = true
		case strings.HasPrefix(rest, vm+": "):
			// The session failed (its ssh didn't connect, say), and tries
			// again later: nothing to wait for now.
			r.Settled = true
		}
		for _, lk := range links {
			p := lk.listen()
			switch {
			case strings.HasPrefix(rest, p+lineRefused):
				if !said[lk.key()] {
					r.Refused = append(r.Refused, lk)
				}
				said[lk.key()] = true
			case strings.HasPrefix(rest, p+lineCarried), strings.HasPrefix(rest, p+lineNotCarried):
				said[lk.key()] = true
			}
		}
	}
	if len(said) == len(links) {
		r.Settled = true
	}
	return r
}

// closeAll stops listening on every link's socket, which removes it, and
// ends the masters to targets: the connections they carry end too.
func (l *Links) closeAll() {
	l.mu.Lock()
	var ended []*targetMaster
	for k, end := range l.open {
		_ = end.ln.Close()
		delete(l.open, k)
	}
	for vm, m := range l.masters {
		m.cancel()
		ended = append(ended, m)
		delete(l.masters, vm)
	}
	l.mu.Unlock()
	// Each master's ssh ends, and its control socket goes with it, before
	// the session does.
	for _, m := range ended {
		<-m.ready
		m.p.wait(masterEndWait)
	}
}

// masterEndWait bounds how long a session waits for a master's ssh to end
// once it's stopped.
const masterEndWait = 2 * time.Second

// wait waits up to d for the ssh to end; a nil one has.
func (p *proc) wait(d time.Duration) {
	if p == nil {
		return
	}
	select {
	case <-p.done:
	case <-time.After(d):
	}
}

// How long a link's socket has nothing new to accept before the
// connections it accepted are carried, and how long the first of them
// waits at most.
const (
	linkSettle    = 5 * time.Millisecond
	linkSettleMax = 50 * time.Millisecond
)

// serve carries each connection to a link's socket, maxLinkConns at most
// at once. Its loop only accepts, and hands what it accepted off once
// nothing more waits (linkSettle), the first has waited linkSettleMax,
// or maxLinkConns+1 wait: the socket's backlog is 128 on macOS, and while
// the process starts a relay's ssh, or a check's command, it accepts
// nothing, so a burst would fill the backlog, and the client's ssh, or the
// VM's proxy, would have the rest refused with no event. What it hands
// off is taken by a goroutine of its own (take), so the loop goes on
// accepting while a batch is carried or refused: it waits for that only
// with maxLinkConns+1 of the next batch accepted. Accepted, each is
// carried or refused with link_limit.
func (l *Links) serve(ctx context.Context, end *linkEnd) {
	handoff := make(chan []net.Conn)
	taken := make(chan struct{})
	go func() {
		defer close(taken)
		for batch := range handoff {
			for _, c := range batch {
				l.take(ctx, end, c)
			}
		}
	}()
	var batch []net.Conn
	var first time.Time
	// release hands the batch off; unless wait, only if the last one is
	// taken already, or it waits for the next try.
	release := func(wait bool) {
		if len(batch) == 0 {
			return
		}
		if wait {
			handoff <- batch
		} else {
			select {
			case handoff <- batch:
			default:
				return
			}
		}
		batch = nil
	}
	defer func() {
		release(true)
		close(handoff)
		<-taken
	}()
	ul, _ := end.ln.(interface{ SetDeadline(time.Time) error })
	for {
		if ul != nil {
			var d time.Time
			if len(batch) > 0 {
				// A batch whose first has waited linkSettleMax already waits
				// for the last one to be taken: tried again after linkSettle.
				now := time.Now()
				d = now.Add(linkSettle)
				if last := first.Add(linkSettleMax); last.Before(d) && last.After(now) {
					d = last
				}
			}
			_ = ul.SetDeadline(d)
		}
		conn, err := end.ln.Accept()
		switch {
		case errors.Is(err, os.ErrDeadlineExceeded):
			release(false)
			continue
		case errors.Is(err, net.ErrClosed):
			return
		case err != nil:
			// Out of files, say, in a burst: what's accepted goes on, and the
			// socket is tried again shortly.
			release(true)
			l.logf("%s: %v", end.sock, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
			continue
		}
		if len(batch) == 0 {
			first = time.Now()
		}
		batch = append(batch, conn)
		switch {
		case ul == nil || len(batch) > maxLinkConns:
			release(true)
		case time.Since(first) >= linkSettleMax:
			release(false)
		}
	}
}

// take hands a connection accepted on a link's socket off to be carried,
// or refuses it, logged, when maxLinkConns are carried already: refused
// before anything starts, by serve's one goroutine for it, so a flood
// past the cap is accepted at most two batches ahead of its refusals
// being logged.
func (l *Links) take(ctx context.Context, end *linkEnd, conn net.Conn) {
	l.mu.Lock()
	lk := end.link
	l.mu.Unlock()
	select {
	case l.conns <- struct{}{}:
	default:
		// Logged before it's closed, so a client that sees it closed finds
		// why.
		l.Event(EventLinkLimit, lk, strconv.Itoa(maxLinkConns))
		_ = conn.Close()
		return
	}
	go func() {
		defer func() { <-l.conns }()
		l.carry(ctx, conn, lk)
	}()
}

// carry checks a connection's link again, then carries it to the target
// through a master to it, or refuses it.
func (l *Links) carry(ctx context.Context, conn net.Conn, lk Link) {
	defer func() { _ = conn.Close() }()
	t, event, why := l.Check(ctx, lk)
	if event != "" {
		l.Event(event, lk, why)
		return
	}
	ctl, err := l.targetMaster(ctx, t)
	if err != nil {
		l.logf("%s: %s: %v", lk.listen(), t.VM, err)
		l.Event(EventLinkRefused, lk, "ssh")
		return
	}
	l.Event(EventLink, lk, "")
	t.Control = ctl
	l.Relay(ctx, conn, t.linkRelayArgs(lk.Port))
}

// targetMaster returns the control socket of the master to a target's
// start, started first when there's none: one that ended, or was made for
// an earlier start, is replaced.
func (l *Links) targetMaster(ctx context.Context, t LinkTarget) (string, error) {
	l.mu.Lock()
	m := l.masters[t.VM]
	if m != nil && (m.start != t.Start || m.dead()) {
		m.cancel()
		m = nil
	}
	if m == nil {
		m = l.newTargetMaster(ctx, t)
		l.masters[t.VM] = m
	}
	l.mu.Unlock()
	select {
	case <-m.ready:
		return m.ctl, m.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// newTargetMaster starts a master to a target's start, for as long as ctx
// (the session's) lasts.
func (l *Links) newTargetMaster(ctx context.Context, t LinkTarget) *targetMaster {
	ctx, cancel := context.WithCancel(ctx)
	m := &targetMaster{start: t.Start, ctl: l.Control(t.VM, t.Start), ready: make(chan struct{}), cancel: cancel}
	go func() {
		defer close(m.ready)
		t.Control = m.ctl
		m.p, m.err = l.master(ctx, t.SSH)
		if m.err != nil {
			cancel()
		}
	}()
	return m
}

// linkMasterArgs are ssh's arguments for a master that runs nothing (-N):
// the link session's to the client, which asks for no forward until -O
// forward, or one to a target, whose channels carry the connections.
func (o SSH) linkMasterArgs() []string {
	return slices.Concat(
		[]string{
			"-F", "none", "-M", "-S", o.Control, "-N", "-o", "ControlPersist=no",
			"-o", "ServerAliveInterval=10", "-o", "ServerAliveCountMax=3", "-o", "ExitOnForwardFailure=no",
		},
		o.connArgs(),
	)
}

// forwardArgs are ssh's arguments for -O op (forward, cancel) of a remote
// forward from listen in the client to sock on the host, through the
// master whose control socket is ctl. ClearAllForwardings=no comes first,
// as ssh takes an option's first value: the master's yes would clear the
// -R. Without the master, it fails, never connecting.
func forwardArgs(ctl, op, listen, sock string) []string {
	return []string{
		"-F", "none", "-S", ctl, "-o", "ControlMaster=no", "-o", "ClearAllForwardings=no",
		"-o", "ProxyCommand=/usr/bin/false", "-O", op, "-R", listen + ":" + sock, "devvm-link",
	}
}

// linkRelayArgs are ssh's arguments for one connection to a target's
// port: a channel of the master's to 127.0.0.1:port in the target. Its
// ProxyCommand=/usr/bin/false comes before connArgs's, as ssh takes an
// option's first value: without the master, -S would make a connection
// of its own, which fails instead.
func (o SSH) linkRelayArgs(port int) []string {
	return slices.Concat(
		[]string{
			"-F", "none", "-S", o.Control, "-o", "ControlMaster=no", "-o", "ProxyCommand=/usr/bin/false",
			"-W", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
		},
		o.connArgs(),
	)
}

// startSSH starts ssh, its output to the log, each line behind sshPrefix
// (sshLines).
func (l *Links) startSSH(ctx context.Context, args []string) (func() error, error) {
	cmd := exec.CommandContext(ctx, "ssh", args...) //nolint:gosec // devvm's own arguments
	out := &sshLines{w: l.Log}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return func() error {
		err := cmd.Wait()
		out.flush()
		return err
	}, nil
}

// sshPrefix starts each line ssh writes to a session's log.
const sshPrefix = "ssh: "

// sshLines is ssh's output to a session's log, written a whole line at a
// time, each behind sshPrefix, so that no line of ssh's starts with
// logPrefix, which only the session's own lines do (ReadLinksLog). ssh
// prints what the remote side sends it, a disconnect's reason say, with
// its newlines, so either VM's root could otherwise write a line that
// reads as the session's. A line longer than logWriteMax is cut, its
// rest a line of its own; a carriage return ending a line is dropped.
type sshLines struct {
	w io.Writer

	mu  sync.Mutex
	buf []byte
}

func (s *sshLines) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = append(s.buf, p...)
	var out []byte
	for {
		line, rest, ok := bytes.Cut(s.buf, []byte("\n"))
		if !ok {
			if len(s.buf) < logWriteMax {
				break
			}
			line, rest = s.buf[:logWriteMax], s.buf[logWriteMax:]
		}
		out = appendSSHLine(out, line)
		s.buf = rest
	}
	s.buf = slices.Clone(s.buf)
	if len(out) > 0 {
		if _, err := s.w.Write(out); err != nil {
			return len(p), err
		}
	}
	return len(p), nil
}

// flush writes what's left, once ssh has ended, as a line.
func (s *sshLines) flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.buf) > 0 {
		_, _ = s.w.Write(appendSSHLine(nil, s.buf))
		s.buf = nil
	}
}

func appendSSHLine(b, line []byte) []byte {
	b = append(b, sshPrefix...)
	b = append(b, bytes.TrimSuffix(line, []byte("\r"))...)
	return append(b, '\n')
}

// commandSSH runs ssh to its end; its output, the VM's sshd's words among
// it, is in the error when it fails, on one line.
func (l *Links) commandSSH(ctx context.Context, args []string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ssh", args...).CombinedOutput() //nolint:gosec // devvm's own arguments
	if err != nil {
		return fmt.Errorf("%w: %s", err, oneLine(out[:min(len(out), logWriteMax)]))
	}
	return nil
}

// oneLine is ssh's output with its lines joined by "; ".
func oneLine(out []byte) string {
	lines := strings.FieldsFunc(string(out), func(r rune) bool { return r == '\n' || r == '\r' })
	var kept []string
	for _, l := range lines {
		if l = strings.TrimSpace(l); l != "" {
			kept = append(kept, l)
		}
	}
	return strings.Join(kept, "; ")
}

// listenSocket listens on a unix socket at path, mode 600, which closing
// it removes. A socket there that nothing listens on is replaced, and
// anything else there is refused.
func listenSocket(ctx context.Context, path string) (net.Listener, error) {
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	case fi.Mode()&os.ModeSocket == 0:
		return nil, fmt.Errorf("%s: something else is there", path)
	default:
		d := net.Dialer{Timeout: time.Second}
		if c, err := d.DialContext(ctx, "unix", path); err == nil {
			_ = c.Close()
			return nil, fmt.Errorf("%s: another program listens there", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}
