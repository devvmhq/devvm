package proxy

// The broker: devvm _proxy, which keeps what only the host should do, unconfined, and runs the
// child, devvm _proxy-child, which reads the VM's network and can be confined (child.go). The
// broker reads the config, looks up the company proxy's password, reads the CAs' files, listens on
// the VM's proxy port, relays the VM's SSH agent, and starts the child with the listener and one
// end of a socketpair, over which it answers the child's questions (wire.go): a binding's value
// (IO.Secret), a watched file's bytes, a connection to a link's socket, a login's token exchange
// (IO.Capture). Each names something in the broker's own config, one VM's, which the broker finds
// in its own reads, so a child that a VM took over can't reach another VM's binding, any other
// secret or any file. The two end together, and neither starts the other again.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/devvmhq/devvm/internal/daemon"
	"github.com/devvmhq/devvm/internal/redact"
)

// hostSide is what the child asks the broker for: the broker itself, in the same process (a
// test's), or brokerClient, over the socketpair.
type hostSide interface {
	// value is the value of the binding of key, and when the broker looked it up.
	value(ctx context.Context, key valueKey) (redact.Secret, time.Time, error)
	// file is the nth of the start's watched files, as the broker read it.
	file(n int) (fileRead, error)
	// link is a connection to the socket of the VM's link that host and port name.
	link(ctx context.Context, host string, port int) (net.Conn, error)
	// token exchanges a captured token request (exchanger.exchange).
	token(ctx context.Context, r tokenRequest) (tokenReply, error)
}

// broker is the broker's side of a VM's proxy: what its config gave it and what it keeps for the
// child's questions.
type broker struct {
	cfg     Config
	log     *log.Logger
	lookups *lookups
	links   links // the broker's own read of the links' list; file "": none
	files   []watchedFile
	tokens  *exchanger
	// start is what the child runs with, but its Exe, which the broker's process sets.
	start childStart
}

// watchedFiles are c's files that change while the proxy runs, in the order the child names them:
// its policy files, those whose denies alone apply, the bindings file and the links' list.
func watchedFiles(c Config) []watchedFile {
	var out []watchedFile
	for _, p := range c.Policies {
		out = append(out, watchedFile{watchPolicy, p})
	}
	for _, p := range c.DenyPolicies {
		out = append(out, watchedFile{watchDeny, p})
	}
	if c.Secrets != "" {
		out = append(out, watchedFile{watchBindings, c.Secrets})
	}
	if c.Links != "" {
		out = append(out, watchedFile{watchLinks, c.Links})
	}
	return out
}

// newBroker is the broker of c, logging to l, its values from std.Secret and its logins saved
// through std.Capture: it looks up the company proxy's password, when c names its secret, and
// reads the CAs' files and the user's CAs, while ctx lasts.
func newBroker(ctx context.Context, c Config, std IO, l *log.Logger) (*broker, error) {
	b := &broker{
		cfg: c, log: l, lookups: newLookups(c.Secrets, std.Secret, l), files: watchedFiles(c),
		tokens: &exchanger{vm: c.VM, list: c.Captures, c: std.Capture, log: l, route: direct},
	}
	// Without a company proxy, the broker's own requests go directly, never through one in
	// devvm's environment that the config doesn't name.
	b.lookups.route = direct
	if c.Links != "" {
		b.links = newLinks(c.Links, c.LinkSockets, c.VM, l, nil)
	}
	st := childStart{
		vm: c.VM, password: c.Password, builtin: c.BuiltinPolicies, files: b.files, open: c.Open,
		log: c.Log, proxyLog: c.ProxyLog, captures: c.Captures, upstreamCAs: c.UpstreamCAs,
		upstream: c.Upstream, upstreamUnresolved: c.UpstreamUnresolved, nixCache: c.NixCache,
		nixCacheMax: c.NixCacheMax, linkSockets: c.LinkSockets,
	}
	var err error
	if c.CA != "" {
		if st.ca, st.caKey, err = readCA(c.CA, c.CAKey); err != nil {
			return nil, err
		}
	}
	if c.CANext != "" {
		if st.caNext, st.caNextKey, err = readCA(c.CANext, c.CANextKey); err != nil {
			return nil, err
		}
	}
	if c.UpstreamCAs != "" {
		if b.lookups.roots, err = upstreamPool([]byte(c.UpstreamCAs)); err != nil {
			return nil, err
		}
		b.tokens.roots = b.lookups.roots
	}
	if c.Upstream != nil {
		if st.upstreamPassword, err = upstreamPassword(ctx, c.Upstream.Secret, std.Secret); err != nil {
			return nil, err
		}
		up, err := newUpstreams(*c.Upstream, st.upstreamPassword)
		if err != nil {
			return nil, err
		}
		b.lookups.route, b.tokens.route = up.host, up.host
	}
	b.start = st
	return b, nil
}

// look looks at the bindings file and the links' list, as the broker does at every value, link
// and token request, whatever the child asks: a binding or a link devvm took away is gone for the
// next request, and a change to the bindings file drops the values and the failures.
func (b *broker) look() {
	if b.lookups.file != nil {
		b.lookups.file.get()
	}
	if b.links.file != "" {
		b.links.list.get()
	}
}

func (b *broker) value(ctx context.Context, key valueKey) (redact.Secret, time.Time, error) {
	b.look()
	return b.lookups.value(ctx, key)
}

func (b *broker) file(n int) (fileRead, error) {
	if n < 0 || n >= len(b.files) {
		return fileRead{}, fmt.Errorf("watched file %d: %w", n, errNotFound)
	}
	return b.files[n].read()
}

// errNotLink refuses a link request for a host and port that are none of the VM's links.
var errNotLink = fmt.Errorf("not one of this VM's links: %w", errNotFound)

func (b *broker) link(ctx context.Context, host string, port int) (net.Conn, error) {
	b.look()
	if b.links.file == "" {
		return nil, errNotLink
	}
	e, ok := b.links.find(host, port)
	if !ok {
		return nil, errNotLink
	}
	return b.links.dial(ctx, host, port, e)
}

func (b *broker) token(ctx context.Context, r tokenRequest) (tokenReply, error) {
	b.look()
	if b.tokens.c == nil {
		return tokenReply{}, fmt.Errorf("no token endpoints: %w", errNotFound)
	}
	return b.tokens.exchange(ctx, r)
}

// requestCaps are the broker's caps on the child's requests at once, by type: one value, as
// lookups go one at a time, captureReads token exchanges, 4 files, and a link session's own cap
// on a client's connections. A request past its type's cap gets an error, which a child that
// keeps the same caps never gets.
func requestCaps() map[string]int {
	return map[string]int{frameValue: 1, frameToken: captureReads, frameFile: 4, frameLink: linkConnsMax}
}

// linkConnsMax is how many link connections the child asks for at once: the link session's cap
// on a client's connections.
const linkConnsMax = 256

// serveChild answers the child's requests on fc, each in a goroutine of its own, until the child
// closes its end (io.EOF) or sends a frame that breaks the rules: one that isn't a request, or
// carries a descriptor. ctx is the broker's life, which a request's lookup or exchange outlives
// no further than its own timeout.
func (b *broker) serveChild(ctx context.Context, fc *frameConn) error {
	slots := map[string]chan struct{}{}
	for t, n := range requestCaps() {
		slots[t] = make(chan struct{}, n)
	}
	for {
		var f toBroker
		fd, err := fc.recv(&f)
		if fd != nil {
			_ = fd.Close()
			return broke("a descriptor from the child")
		}
		if err != nil {
			return err
		}
		slot, ok := slots[f.Type]
		if !ok || !f.fields() {
			return broke("a frame of type " + quoteType(f.Type) + " after ready")
		}
		select {
		case slot <- struct{}{}:
		default:
			_ = fc.send(toChild{Type: f.Type, ID: f.ID, Error: codeBusy}, nil)
			continue
		}
		go func() {
			defer func() { <-slot }()
			b.answer(ctx, fc, f)
		}()
	}
}

// quoteType is a frame's type as an error names it: one of the protocol's, or "another".
func quoteType(t string) string {
	switch t {
	case frameStart, frameReady, frameFailed, frameValue, frameFile, frameLink, frameToken:
		return t
	}
	return "another"
}

// answer answers f, one of the child's requests, on fc.
func (b *broker) answer(ctx context.Context, fc *frameConn, f toBroker) {
	reply := toChild{Type: f.Type, ID: f.ID}
	var fd *os.File
	var err error
	switch f.Type {
	case frameValue:
		var v redact.Secret
		var at time.Time
		if v, at, err = b.value(ctx, valueKey{f.Value.Kind, f.Value.Agent, f.Value.Name}); err == nil {
			reply.Value = &wireValue{Value: v.Reveal(), At: at} //nolint:forbidigo // the value goes to the child, which puts it in its binding's header
		}
	case frameFile:
		var got fileRead
		if got, err = b.file(f.File.N); err == nil {
			reply.File = &wireFile{Data: got.data, MTime: got.st.mtime, Size: got.st.size, Ino: got.st.ino, Missing: got.missing}
		}
	case frameLink:
		fd, err = b.linkFile(ctx, f.Link.Host, f.Link.Port)
		if err == nil {
			defer func() { _ = fd.Close() }()
			reply.Link = &wireLink{FD: true}
		}
	case frameToken:
		var r tokenReply
		if r, err = b.token(ctx, tokenRequestOf(*f.Token)); err == nil {
			reply.Token = wireTokenReplyOf(r)
		}
	}
	if err != nil {
		// The child gets a code alone: the error's text, such as a lookup's or an endpoint's,
		// stays in the broker's own log.
		b.log.Printf("the child's %s request: %v", f.Type, err)
		reply = toChild{Type: f.Type, ID: f.ID, Error: errorCodeOf(err)}
		fd = nil
	}
	serr := fc.send(reply, fd)
	if errors.Is(serr, errFrameTooLong) {
		// A reply too long to send, such as a file the broker read to its cap, is refused, so
		// the child's request never waits on it.
		serr = fc.send(toChild{Type: f.Type, ID: f.ID, Error: codeTooLong}, nil)
	}
	if serr != nil && !errors.Is(serr, net.ErrClosed) {
		b.log.Printf("the child's %s request: %v", f.Type, serr)
	}
}

// The codes of a request the broker refuses, which are all the child learns of why.
const (
	codeNotFound = "not_found" // nothing in the broker's config names it
	codeRefused  = "refused"   // a capture refused the token request or its reply
	codeBusy     = "busy"      // past its type's cap at once
	codeTooLong  = "too_long"  // the reply wouldn't fit in a frame
	codeFailed   = "failed"    // anything else: a lookup, a connection, an endpoint
)

// errNotFound is a request for something the broker's config doesn't name.
var errNotFound = errors.New("not in the broker's config")

// errorCodeOf is err's code for the child.
func errorCodeOf(err error) string {
	switch {
	case errors.Is(err, errNotFound):
		return codeNotFound
	case errors.Is(err, errCaptureRefused):
		return codeRefused
	}
	return codeFailed
}

// linkFile is a connection to the link host and port name, as a file to pass the child.
func (b *broker) linkFile(ctx context.Context, host string, port int) (*os.File, error) {
	c, err := b.link(ctx, host, port)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	u, ok := c.(*net.UnixConn)
	if !ok {
		return nil, errors.New("a link's connection that isn't a socket")
	}
	return u.File()
}

// --- the broker's process ---

// startWait is how long after its own start the broker waits for the child to say it's ready:
// within devvm's 5 seconds for the proxy's status file, so a child that fails or hangs fails the
// start, which devvm reports (proxy_start_failed).
const startWait = 4 * time.Second

// stopWait is how long the broker waits for its child to end after SIGTERM, or past its own
// close's closeWait, before it kills it: within the 2 seconds devvm waits before its own SIGKILL.
const stopWait = time.Second

// childProc is the broker's child as the broker has it: daemon.Handle, or a test's.
type childProc interface {
	PID() int
	Done() <-chan struct{}
	Stop(wait time.Duration) bool
	Kill()
	Exit() string
}

// pair is a started child and the broker's end of the socketpair to it, which the broker's life
// runs on.
type pair struct {
	fc    *frameConn
	child childProc
	// lifeline is closed once the VM's hostagent is gone; nil: none.
	lifeline <-chan struct{}
}

// handshake sends the child start, with the broker's executable's SHA-256 (exe), and waits until
// deadline for its answer: ready, of the protocol's version and the broker's executable, or
// failed. A child that answers anything else, or nothing in time, fails the start.
func (b *broker) handshake(p pair, exe string, deadline time.Time) (wireReady, error) {
	st := b.start
	st.exe = exe
	// A child that never reads its socket can't hold the broker past the start's deadline.
	_ = p.fc.c.SetWriteDeadline(deadline)
	err := p.fc.send(toChild{Type: frameStart, Start: st.wire()}, nil)
	_ = p.fc.c.SetWriteDeadline(time.Time{})
	if err != nil {
		return wireReady{}, fmt.Errorf("the child's start: %w", err)
	}
	type answer struct {
		f   toBroker
		err error
	}
	got := make(chan answer, 1)
	go func() {
		var f toBroker
		fd, err := p.fc.recv(&f)
		if fd != nil {
			_ = fd.Close()
			err = broke("a descriptor from the child")
		}
		got <- answer{f, err}
	}()
	t := time.NewTimer(time.Until(deadline))
	defer t.Stop()
	// A child that ends closes its end, so its answer, or the end, always comes.
	var a answer
	select {
	case a = <-got:
	case <-t.C:
		return wireReady{}, fmt.Errorf("the child didn't say it's ready within %s", startWait)
	}
	switch {
	case errors.Is(a.err, io.EOF) || errors.Is(a.err, io.ErrUnexpectedEOF):
		select {
		case <-p.child.Done():
		case <-t.C:
		}
		return wireReady{}, fmt.Errorf("the child ended as it started (%s)", p.child.Exit())
	case a.err != nil:
		return wireReady{}, fmt.Errorf("the child's answer: %w", a.err)
	case a.f.Type == frameFailed && a.f.fields():
		return wireReady{}, fmt.Errorf("the child failed: %s", a.f.Failed.Why)
	case a.f.Type != frameReady || !a.f.fields():
		return wireReady{}, broke("an answer to start that's neither ready nor failed")
	case a.f.Ready.Version != protocolVersion || a.f.Ready.Exe != exe:
		return wireReady{}, broke("a ready of another version or another build")
	}
	return *a.f.Ready, nil
}

// live runs the started pair until one of them ends, answering the child's requests: once the
// lifeline ends (the VM's hostagent is gone), the broker closes the socket, the child ends as it
// reads that, and the broker waits for it, killing it stopWait past its closeWait; once stop is
// closed (devvm stop's SIGTERM), the broker stops the child, SIGTERM and SIGKILL after stopWait;
// once the child ends, or its end of the socket does, the broker logs how; and once a frame breaks
// the rules, the broker stops the child and logs the rule, never the frame. It returns nil for the
// first two, the reason for the others.
func (b *broker) live(ctx context.Context, p pair, stop <-chan struct{}) error {
	served := make(chan error, 1)
	go func() { served <- b.serveChild(ctx, p.fc) }()
	select {
	case <-p.lifeline:
		// The socket closes, and the kill's timer starts, before anything is logged: the log's
		// lock is one the child shares, so no line may come first.
		_ = p.fc.close()
		t := time.NewTimer(closeWait + stopWait)
		defer t.Stop()
		killed := false
		select {
		case <-p.child.Done():
		case <-t.C:
			p.child.Kill()
			<-p.child.Done()
			killed = true
		}
		// Logged only once the child has ended, so no wait for the log's lock delays its kill.
		b.log.Printf("the VM's hostagent is gone; exiting")
		if killed {
			b.log.Printf("the child was still running %s after its socket closed; killed it", closeWait+stopWait)
		}
		return nil
	case <-stop:
		p.child.Stop(stopWait)
		_ = p.fc.close()
		return nil
	case <-p.child.Done():
		_ = p.fc.close()
		return fmt.Errorf("the child ended (%s)", p.child.Exit())
	case err := <-served:
		_ = p.fc.close()
		p.child.Stop(stopWait)
		if isRuleError(err) {
			return fmt.Errorf("the child broke the protocol: %w", err)
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return fmt.Errorf("the child ended (%s)", p.child.Exit())
		}
		return fmt.Errorf("the child's socket: %w", err)
	}
}

// ChildPIDFile is the pid file of the child of the broker whose config is at config, beside it:
// run/proxy-child.pid for a VM's proxy, which the broker writes once the child is ready.
func ChildPIDFile(config string) string {
	return filepath.Join(filepath.Dir(config), "proxy-child.pid")
}

// ConfinedFile says how the child of the broker whose config is at config is confined, beside it:
// run/proxy.confined, which devvm doctor reads while the broker runs.
func ConfinedFile(config string) string {
	return filepath.Join(filepath.Dir(config), "proxy.confined")
}

// ChildCommand is the child's command, devvm's own: _proxy-child.
const ChildCommand = "_proxy-child"

// IsChild reports whether args, a process's arguments, its program first, are the child of the
// broker whose config is at config: _proxy-child -for config.
func IsChild(args []string, config string) bool {
	return daemon.HasFlag(args, ChildCommand, "-for", config)
}

// selfExe is the broker's own executable to run as the child: on Linux its own file, even once an
// upgrade replaced the file at its path, and elsewhere its path.
func selfExe() (string, error) {
	if runtime.GOOS == "linux" {
		return "/proc/self/exe", nil
	}
	return os.Executable()
}

// selfSum is the SHA-256 of this process's own executable, in hex (selfExe's file).
func selfSum() (string, error) {
	p, err := selfExe()
	if err != nil {
		return "", err
	}
	f, err := os.Open(p) //nolint:gosec // this process's own executable
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// childEnv is the child's whole environment, from env (devvm's): TZ, SSL_CERT_FILE and
// SSL_CERT_DIR when devvm has them, so it trusts the roots the broker does, and GODEBUG, with
// updatemaxprocs=0, so Go's runtime doesn't read its cgroup's CPU limit again as it runs. Nothing
// else: no token in the shell's environment, and not the status file's name.
func childEnv(env []string) []string {
	var out []string
	debug := "updatemaxprocs=0"
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "TZ", "SSL_CERT_FILE", "SSL_CERT_DIR":
			out = append(out, kv)
		case "GODEBUG":
			if v != "" {
				debug = v + "," + debug
			}
		}
	}
	return append(out, "GODEBUG="+debug)
}
