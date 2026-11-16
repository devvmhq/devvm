package proxy

// The child: devvm _proxy-child, which the broker starts with its listener and one end of the
// socketpair (broker.go), and which reads the VM's network: every request and tunnel, the rules
// and the floor, interception, the event log and the shared Nix cache. It never reads the VM's run
// folder or devvm's config: what it runs with comes in start, the changing files it looks at
// (stat) and asks the broker for once they change, and each value, link and token exchange it
// asks the broker for. It ends when the broker's end of the socket does.

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/devvmhq/devvm/internal/redact"
	"github.com/devvmhq/devvm/internal/vmname"
)

// childStart is what the child runs with, as the broker sends it (wireStart): the VM's config
// without the paths the child never opens, the CAs' files and the company proxy's password as the
// broker read them, and the files that change, which the child asks for by their number.
type childStart struct {
	exe string // the broker's executable's SHA-256, in hex
	// spawned is how the broker started the child: under a sandbox, or unconfined and why.
	spawned            Confinement
	vm                 vmname.VM
	password           redact.Secret
	builtin            []string
	files              []watchedFile
	open               bool
	log, proxyLog      string
	ca                 []byte
	caKey              redact.Secret
	caNext             []byte
	caNextKey          redact.Secret
	captures           []Capture
	upstreamCAs        string
	upstream           *Upstream
	upstreamPassword   redact.Secret
	upstreamUnresolved bool
	nixCache           string
	nixCacheMax        int64
	linkSockets        string
}

// wire is s as the start frame has it, its secrets revealed into it alone.
func (s childStart) wire() *wireStart {
	w := &wireStart{
		Version: protocolVersion, Exe: s.exe, Spawned: s.spawned.String(), VM: s.vm,
		Password:        s.password.Reveal(), //nolint:forbidigo // the start's wire struct: the child checks each request's password
		BuiltinPolicies: s.builtin, Open: s.open, Log: s.log, ProxyLog: s.proxyLog,
		CA: s.ca, CAKey: s.caKey.Reveal(), //nolint:forbidigo // the start's wire struct: the child intercepts with the VM's CA
		CANext: s.caNext, CANextKey: s.caNextKey.Reveal(), //nolint:forbidigo // the start's wire struct: the child intercepts with the VM's next CA
		Captures: s.captures, UpstreamCAs: s.upstreamCAs, Upstream: s.upstream,
		UpstreamPassword:   s.upstreamPassword.Reveal(), //nolint:forbidigo // the start's wire struct: the company proxy takes it with each plain HTTP request the child sends
		UpstreamUnresolved: s.upstreamUnresolved, NixCache: s.nixCache, NixCacheMax: s.nixCacheMax,
		LinkSockets: s.linkSockets,
	}
	for _, f := range s.files {
		w.Files = append(w.Files, wireWatch{Kind: f.kind, Path: f.path})
	}
	return w
}

// startOf is the start w holds, its secrets made redact.Secret at once: one of another version,
// without the VM's password, or with a file of a kind the child doesn't know, or a second bindings
// file or links' list, is refused.
func startOf(w *wireStart) (childStart, error) {
	switch {
	case w.Version != protocolVersion:
		return childStart{}, fmt.Errorf("a start of version %d, not %d", w.Version, protocolVersion)
	case w.Password == "":
		return childStart{}, errors.New("a start without the VM's password")
	}
	spawned, err := ParseConfinement(w.Spawned)
	if err != nil {
		return childStart{}, fmt.Errorf("how the broker started the child: %w", err)
	}
	s := childStart{
		exe: w.Exe, spawned: spawned, vm: w.VM, password: redact.NewSecret(w.Password), builtin: w.BuiltinPolicies, open: w.Open,
		log: w.Log, proxyLog: w.ProxyLog, ca: w.CA, caKey: redact.NewSecret(w.CAKey), caNext: w.CANext,
		caNextKey: redact.NewSecret(w.CANextKey), captures: w.Captures, upstreamCAs: w.UpstreamCAs,
		upstream: w.Upstream, upstreamPassword: redact.NewSecret(w.UpstreamPassword),
		upstreamUnresolved: w.UpstreamUnresolved, nixCache: w.NixCache, nixCacheMax: w.NixCacheMax,
		linkSockets: w.LinkSockets,
	}
	seen := map[string]bool{}
	for _, f := range w.Files {
		switch f.Kind {
		case watchPolicy, watchDeny:
		case watchBindings, watchLinks:
			if seen[f.Kind] {
				return childStart{}, fmt.Errorf("a start with a second watched file of kind %q", f.Kind)
			}
			seen[f.Kind] = true
		default:
			return childStart{}, fmt.Errorf("a start with a watched file of kind %q", f.Kind)
		}
		s.files = append(s.files, watchedFile{f.Kind, f.Path})
	}
	return s, nil
}

// sources are s's watched files of kind, each read through host as its number.
func (s childStart) sources(host hostSide, kind string) []watchSource {
	var out []watchSource
	for i, f := range s.files {
		if f.kind == kind {
			out = append(out, watchSource{path: f.path, read: func() (fileRead, error) { return host.file(i) }})
		}
	}
	return out
}

// source is s's one watched file of kind, read through host; none: its path is "".
func (s childStart) source(host hostSide, kind string) watchSource {
	if src := s.sources(host, kind); len(src) > 0 {
		return src[0]
	}
	return watchSource{}
}

// newChildServer makes the child's server from st, asking host for what only the broker has,
// logging to l and its events to out: it parses the CAs, takes the company proxy's settings,
// reads the built-in policies (from own, devvm's own files) and opens the Nix cache.
func newChildServer(st childStart, host hostSide, own fs.FS, l *log.Logger, out io.Writer) (*server, error) {
	ev := &events{vm: st.vm, out: out}
	s := &server{
		vm: st.vm, password: st.password, log: l, events: ev,
		secrets: newSecretsOf(st.source(host, watchBindings), func(ctx context.Context, b Binding) (redact.Secret, time.Time, error) {
			return host.value(ctx, valueKeyOf(b))
		}, l),
		captures:       newCaptures(st.captures, host.token),
		interceptPorts: map[int]bool{443: true},
		connects:       make(chan struct{}, maxConnects),
		helloWait:      helloTimeout,
		goneWait:       goneWait,
	}
	s.links = newLinksOf(st.source(host, watchLinks), st.linkSockets, st.vm, l, ev,
		func(ctx context.Context, h string, p int, _ LinkEntry) (net.Conn, error) { return host.link(ctx, h, p) })
	s.transparent = &transparent{s: s}
	s.requests = newRequests(maxRequests, l)
	s.conns.max, s.conns.waitMax = maxConns, maxWaiting
	var fullSaid everyMinute
	s.conns.full = func(n int) {
		if fullSaid.allow(time.Now()) {
			l.Printf("%d connections open at once, the VM's limit: refusing more until one ends", n)
		}
	}
	var err error
	if st.ca != nil {
		if s.ca, err = parseCA(st.ca, st.caKey); err != nil {
			return nil, err
		}
		if st.caNext != nil {
			if s.ca.next, err = parseCA(st.caNext, st.caNextKey); err != nil {
				return nil, err
			}
		}
	}
	if st.upstreamCAs != "" {
		if s.upstreamCAs, err = upstreamPool([]byte(st.upstreamCAs)); err != nil {
			return nil, err
		}
	}
	var up upstreams
	if st.upstream != nil {
		if up, err = newUpstreams(*st.upstream, st.upstreamPassword); err != nil {
			return nil, err
		}
		l.Printf("%s", up.names())
	}
	up.unresolved = st.upstreamUnresolved
	rs, err := readBuiltins(own, st.builtin, l)
	if err != nil {
		return nil, err
	}
	pol := newPolicyOf(rs, st.sources(host, watchPolicy), st.sources(host, watchDeny), l)
	pol.open = st.open
	if st.open {
		l.Printf("open: every host and address, no rules")
	}
	s.egress = &egress{
		pol: pol, own: &hostAddrs{}, up: up, log: l, lookup: net.DefaultResolver.LookupNetIP,
		connect: (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
	}
	s.egress.own.load()
	maxSize := st.nixCacheMax
	if maxSize == 0 {
		maxSize = NixCacheDefault
	}
	s.nix = newNixCache(st.nixCache, maxSize, s.egress, ev, l, s.upstreamCAs)
	if err := s.nix.open(); err != nil {
		return nil, err
	}
	return s, nil
}

// --- the broker, over the socket ---

// brokerClient is the broker as the child asks it, over the socketpair (hostSide): each request a
// frame of an ID of its own, its reply the broker's frame of the same ID, in any order. It keeps
// to the broker's caps on each type at once (requestCaps), waiting for a slot. Neither side
// cancels a request: the broker's own timeouts end each.
type brokerClient struct {
	fc    *frameConn
	slots map[string]chan struct{}

	mu      sync.Mutex
	next    uint64
	pending map[uint64]pendingReq
	ended   error         // why the broker's end did, once it has
	done    chan struct{} // closed once the reader has ended (read)
}

// pendingReq is a request waiting for its reply: its type and where its reply goes. A dropped
// one's caller gave up on it: its reply, when it comes, is closed and gives back its slot
// (release), as the broker answered it within its own cap.
type pendingReq struct {
	typ     string
	ch      chan childReply
	dropped bool
	release func()
}

// childReply is a reply the reader took, and the descriptor it carried, if any.
type childReply struct {
	f  toChild
	fd *os.File
}

func newBrokerClient(fc *frameConn) *brokerClient {
	c := &brokerClient{fc: fc, slots: map[string]chan struct{}{}, pending: map[uint64]pendingReq{}, done: make(chan struct{})}
	for t, n := range requestCaps() {
		c.slots[t] = make(chan struct{}, n)
	}
	return c
}

// errBrokerGone is a request's error once the broker's end has closed.
var errBrokerGone = errors.New("the broker is gone")

// read reads the broker's replies until its end closes or a frame breaks the rules: a second
// start, a reply to no request or of another type than its request's, which ends the pair. A
// descriptor that comes with anything but a link's reply that says it carries one is closed. It
// returns why it ended, and every request waiting then fails.
func (c *brokerClient) read() error {
	err := c.readReplies()
	c.mu.Lock()
	c.ended = err
	c.mu.Unlock()
	close(c.done)
	return err
}

func (c *brokerClient) readReplies() error {
	for {
		var f toChild
		fd, err := c.fc.recv(&f)
		if err != nil {
			return err
		}
		if f.Type == frameStart {
			closeFile(fd)
			return broke("a second start")
		}
		c.mu.Lock()
		p, ok := c.pending[f.ID]
		delete(c.pending, f.ID)
		c.mu.Unlock()
		if !ok || p.typ != f.Type || !f.fields() {
			closeFile(fd)
			return broke("a reply to no request")
		}
		if fd != nil && (f.Link == nil || !f.Link.FD) {
			closeFile(fd)
			fd = nil
		}
		if p.dropped {
			closeFile(fd)
			p.release()
			continue
		}
		p.ch <- childReply{f, fd}
	}
}

// closeFile closes f, if it isn't nil.
func closeFile(f *os.File) {
	if f != nil {
		_ = f.Close()
	}
}

// call sends the request req, of type typ, and waits for its reply, in one of its type's slots,
// until giveUp is closed (nil: never): the request is then dropped, and its reply, when it comes,
// closed, its slot held until then, so the child never has more under way than the broker
// allows. A reply's error is one of the broker's codes alone.
func (c *brokerClient) call(typ string, req toBroker, giveUp <-chan struct{}) (toChild, *os.File, error) {
	slot := c.slots[typ]
	select {
	case slot <- struct{}{}:
	case <-c.done:
		return toChild{}, nil, errBrokerGone
	case <-giveUp:
		return toChild{}, nil, errGaveUp
	}
	release := func() { <-slot }
	ch := make(chan childReply, 1)
	c.mu.Lock()
	if c.ended != nil {
		c.mu.Unlock()
		release()
		return toChild{}, nil, errBrokerGone
	}
	c.next++
	id := c.next
	c.pending[id] = pendingReq{typ: typ, ch: ch, release: release}
	c.mu.Unlock()
	req.Type, req.ID = typ, id
	if err := c.fc.send(req, nil); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		release()
		return toChild{}, nil, fmt.Errorf("the broker: %w", err)
	}
	select {
	case r := <-ch:
		release()
		if r.f.Error != "" {
			closeFile(r.fd)
			return toChild{}, nil, fmt.Errorf("the broker: %s", r.f.Error)
		}
		return r.f, r.fd, nil
	case <-c.done:
		release()
		select {
		case r := <-ch:
			closeFile(r.fd)
		default:
		}
		return toChild{}, nil, errBrokerGone
	case <-giveUp:
		c.mu.Lock()
		p, waiting := c.pending[id]
		if waiting {
			p.dropped = true
			c.pending[id] = p
		}
		c.mu.Unlock()
		if !waiting { // its reply came as the caller gave up, or comes as the broker ends
			select {
			case r := <-ch:
				closeFile(r.fd)
			case <-c.done:
			}
			release()
		}
		return toChild{}, nil, errGaveUp
	}
}

// errGaveUp is a request's error once its caller gave up on it.
var errGaveUp = errors.New("the request ended before the broker answered")

// value is the binding's value, waited for while ctx lasts: the broker goes on with its lookup,
// and keeps what it gets for the next request.
func (c *brokerClient) value(ctx context.Context, key valueKey) (redact.Secret, time.Time, error) {
	r, _, err := c.call(frameValue, toBroker{Value: &wireValueKey{Kind: key.kind, Agent: key.agent, Name: key.name}}, ctx.Done())
	if err != nil {
		return redact.Secret{}, time.Time{}, err
	}
	return redact.NewSecret(r.Value.Value), r.Value.At, nil
}

func (c *brokerClient) file(n int) (fileRead, error) {
	r, _, err := c.call(frameFile, toBroker{File: &wireFileReq{N: n}}, nil)
	if err != nil {
		return fileRead{}, err
	}
	f := r.File
	return fileRead{data: f.Data, st: stamp{mtime: f.MTime, size: f.Size, ino: f.Ino}, missing: f.Missing}, nil
}

// link is a connection to the link's socket that the broker connected to: once ctx has ended, the
// child no longer wants it, and closes it as it comes.
func (c *brokerClient) link(ctx context.Context, host string, port int) (net.Conn, error) {
	_, fd, err := c.call(frameLink, toBroker{Link: &wireLinkReq{Host: host, Port: port}}, ctx.Done())
	if err != nil {
		return nil, err
	}
	if fd == nil {
		return nil, errors.New("the broker's link reply carried no connection")
	}
	conn, err := net.FileConn(fd)
	_ = fd.Close()
	if err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		_ = conn.Close()
		return nil, ctx.Err()
	}
	return conn, nil
}

// token is a token exchange's reply, waited for while ctx lasts: captureToken's, its own 20
// seconds whatever happens to the VM's request.
func (c *brokerClient) token(ctx context.Context, r tokenRequest) (tokenReply, error) {
	got, _, err := c.call(frameToken, toBroker{Token: wireTokenReqOf(r)}, ctx.Done())
	if err != nil {
		return tokenReply{}, err
	}
	return tokenReplyOf(*got.Token), nil
}

// wireHeaders is h as a frame has it.
func wireHeaders(h http.Header) []wireHeader {
	var out []wireHeader
	for k, vs := range h {
		for _, v := range vs {
			out = append(out, wireHeader{Name: k, Value: []byte(v)})
		}
	}
	return out
}

// headersOf is the header a frame's hs hold.
func headersOf(hs []wireHeader) http.Header {
	h := http.Header{}
	for _, x := range hs {
		h.Add(x.Name, string(x.Value))
	}
	return h
}

func wireTokenReqOf(r tokenRequest) *wireTokenReq {
	return &wireTokenReq{Host: r.host, Path: r.path, Method: r.method, Query: r.query, Header: wireHeaders(r.header), Body: r.body}
}

func tokenRequestOf(w wireTokenReq) tokenRequest {
	return tokenRequest{host: w.Host, path: w.Path, method: w.Method, query: w.Query, header: headersOf(w.Header), body: w.Body}
}

func wireTokenReplyOf(r tokenReply) *wireTokenReply {
	return &wireTokenReply{Status: r.status, Header: wireHeaders(r.header), Body: r.body, Agent: r.agent, Sent: r.sent}
}

func tokenReplyOf(w wireTokenReply) tokenReply {
	return tokenReply{status: w.Status, header: headersOf(w.Header), body: w.Body, agent: w.Agent, sent: w.Sent}
}

// --- the child's process ---

// runChildOn runs the child on fc, its end of the socketpair, and ln, the listener the broker
// opened: it reads start, checks the broker's executable is its own (exe), opens its logs, makes
// its server, checks it reads the host's addresses, confines itself where its platform does that
// in the process and checks its confinement holds, refusing it probe, its broker's config
// (std.confine, else confineChild), answers ready with how it's confined, and serves until the
// broker's end closes, when it closes its listener, then its connections, waiting up to
// closeWait for its handlers (closeConns). A start it can't run with gets failed, with why, and
// returns the error. Nothing is asked of the broker before ready.
func runChildOn(fc *frameConn, ln net.Listener, std IO, exe, probe string, l *log.Logger) error {
	defer func() { _ = fc.close() }()
	var f toChild
	fd, err := fc.recv(&f)
	closeFile(fd)
	if err != nil {
		return fmt.Errorf("the broker's start: %w", err)
	}
	if f.Type != frameStart || !f.fields() {
		return broke("a first frame that isn't start")
	}
	client := newBrokerClient(fc)
	s, closeLog, err := childServer(f.Start, client, std, exe, l)
	if err != nil {
		_ = fc.send(toBroker{Type: frameFailed, Failed: &wireFailed{Why: err.Error()}}, nil)
		return err
	}
	defer closeLog()
	confine := std.confine
	if confine == nil {
		confine = confineChild
	}
	spawned, _ := ParseConfinement(f.Start.Spawned) // startOf took it, in childServer
	confined := confine(spawned, probe)
	ready := &wireReady{Version: protocolVersion, Exe: exe, Addr: ln.Addr().String(), Confined: confined.String()}
	if err := fc.send(toBroker{Type: frameReady, Ready: ready}, nil); err != nil {
		return err
	}
	srv := httpServer(s)
	ended := make(chan error, 1)
	go func() {
		err := client.read()
		if isRuleError(err) {
			l.Printf("%v; exiting", err)
		}
		ended <- err
		_ = srv.Close()
	}()
	s.egress.pol.all()
	err = srv.Serve(s.conns.track(ln))
	s.closeConns()
	_ = fc.close()
	if readErr := <-ended; isRuleError(readErr) {
		return readErr
	}
	if !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// childServer makes the child's server from w, the broker's start, asking host for what only the
// broker has: a start of another build than exe, or one it can't run with, is an error, and so
// is a host whose addresses it can't read, which would leave the floor without them. It opens the
// event log (start's Log, else std.Stdout), which closeLog closes.
func childServer(w *wireStart, host hostSide, std IO, exe string, l *log.Logger) (s *server, closeLog func(), err error) {
	st, err := startOf(w)
	if err != nil {
		return nil, nil, err
	}
	if st.exe != exe {
		return nil, nil, errors.New("the broker runs another build of devvm")
	}
	if st.proxyLog != "" && std.ErrFile != nil {
		f, err := copyRotating(std.ErrFile, st.proxyLog, std.Stderr, LogLimit)
		if err != nil {
			return nil, nil, err
		}
		l.SetOutput(f)
	}
	out, closeLog := std.Stdout, func() {}
	if st.log != "" {
		f, err := OpenEventLog(st.log)
		if err != nil {
			return nil, nil, err
		}
		out, closeLog = f, func() { _ = f.Close() }
	}
	if s, err = newChildServer(st, host, std.FS, l, out); err != nil {
		closeLog()
		return nil, nil, err
	}
	// The system's roots and the time zone, loaded now, before anything could confine the child.
	if _, err := x509.SystemCertPool(); err != nil {
		closeLog()
		return nil, nil, fmt.Errorf("the system's roots: %w", err)
	}
	_, _ = time.Now().Zone()
	if len(s.egress.own.own()) == 0 {
		closeLog()
		return nil, nil, errors.New("this machine's own addresses can't be read")
	}
	return s, closeLog, nil
}
