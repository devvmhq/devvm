package proxy

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/devvmhq/devvm/internal/leaktest"
	"github.com/devvmhq/devvm/internal/memnet"
)

// farEnd is the other end of a tunnel in a test. Each connection's first byte says what it does:
// 'e' reads to the end of what the client sends, answers "got:" and those bytes, and closes; 's'
// sends bytes until a write fails, then says so on gone.
type farEnd struct{ gone chan struct{} }

// serveFarEnd serves ln as a farEnd until it's closed.
func serveFarEnd(t *testing.T, ln net.Listener) *farEnd {
	t.Helper()
	t.Cleanup(func() { _ = ln.Close() })
	f := &farEnd{gone: make(chan struct{}, 16)}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *farEnd) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	mode := make([]byte, 1)
	if _, err := io.ReadFull(c, mode); err != nil {
		return
	}
	switch mode[0] {
	case 'e':
		b, _ := io.ReadAll(c)
		_, _ = c.Write(append([]byte("got:"), b...))
	case 's':
		chunk := bytes.Repeat([]byte("x"), 4096)
		_ = c.SetWriteDeadline(time.Now().Add(30 * time.Second))
		for {
			if _, err := c.Write(chunk); err != nil {
				f.gone <- struct{}{}
				return
			}
		}
	}
}

// everyByte is a megabyte holding every byte value, over and over.
func everyByte() []byte {
	b := make([]byte, 1<<20)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

// checkTunnel checks what every tunnel does, open asking open for a new one: the client's bytes
// reach the far end as they are, the client's end of sending (a half-close) reaches it too and the
// answer still comes back, and a client that goes away ends the tunnel, so the far end's writes
// fail.
func checkTunnel(t *testing.T, f *farEnd, open func() (net.Conn, io.Reader)) {
	t.Helper()
	c, r := open()
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))
	data := everyByte()
	go func() {
		_, _ = c.Write(append([]byte("e"), data...))
		_ = c.(interface{ CloseWrite() error }).CloseWrite()
	}()
	got, err := io.ReadAll(r)
	_ = c.Close()
	if err != nil || !bytes.Equal(got, append([]byte("got:"), data...)) {
		t.Errorf("after a half-close: %d bytes back, %v; want %d", len(got), err, 4+len(data))
	}

	c, r = open()
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))
	_, _ = c.Write([]byte("s"))
	if _, err := io.ReadFull(r, make([]byte, 64<<10)); err != nil {
		t.Fatalf("the far end's bytes: %v", err)
	}
	_ = c.Close()
	select {
	case <-f.gone:
	case <-time.After(15 * time.Second):
		t.Error("the client went away, and the tunnel goes on")
	}
}

// connectTo opens a CONNECT tunnel through the proxy at addr to hostPort, and returns the
// connection and what reads the tunnel's bytes.
func connectTo(t *testing.T, addr, hostPort string) (net.Conn, io.Reader) {
	t.Helper()
	c, err := new(net.Dialer).DialContext(t.Context(), "tcp", addr)
	must(t, err)
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", hostPort, hostPort)
	r := bufio.NewReader(c)
	resp, err := http.ReadResponse(r, &http.Request{Method: http.MethodConnect})
	must(t, err)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT %s: %d", hostPort, resp.StatusCode)
	}
	return c, r
}

// A tunnel to a host the policy allows.
func TestTunnelSplices(t *testing.T) {
	t.Parallel()
	s, _, _, addr := linkProxy(t)
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	must(t, err)
	f := serveFarEnd(t, ln)
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	s.egress.pol = writePolicy(t, "127.0.0.1:"+port+"\n")
	checkTunnel(t, f, func() (net.Conn, io.Reader) { return connectTo(t, addr, "127.0.0.1:"+port) })
}

// A tunnel to a host the policy allows passes the far end's bytes on at once: a protocol whose
// server speaks first (SMTP) gets its greeting before the client sends anything, and a client that
// sends fewer than five bytes that aren't TLS gets its answer.
func TestTunnelServerSpeaksFirst(t *testing.T) {
	t.Parallel()
	s, _, _, addr := linkProxy(t)
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	must(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		_, _ = io.WriteString(c, "220 hello\r\n")
		b := make([]byte, 2)
		if _, err := io.ReadFull(c, b); err == nil {
			_, _ = c.Write(append([]byte("got:"), b...))
		}
	}()
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	s.egress.pol = writePolicy(t, "127.0.0.1:"+port+"\n")
	c, r := connectTo(t, addr, "127.0.0.1:"+port)
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	greeting := make([]byte, len("220 hello\r\n"))
	if _, err := io.ReadFull(r, greeting); err != nil || string(greeting) != "220 hello\r\n" {
		t.Fatalf("the greeting before the client spoke: %q, %v", greeting, err)
	}
	_, _ = c.Write([]byte("hi"))
	answer := make([]byte, len("got:hi"))
	if _, err := io.ReadFull(r, answer); err != nil || string(answer) != "got:hi" {
		t.Errorf("two bytes that aren't TLS: %q, %v", answer, err)
	}
}

// The VM's CONNECTs open at once are bounded (connects, maxConnects for a proxy Main starts): one
// past the limit gets 503 and a refused block, the proxy's own log says so once, and a tunnel that
// ends makes room again.
func TestConnectLimit(t *testing.T) {
	t.Parallel()
	if s := testServer(t, Config{}, ""); cap(s.connects) != maxConnects {
		t.Errorf("a proxy carries %d CONNECTs at once, want %d", cap(s.connects), maxConnects)
	}
	synctest.Test(t, func(t *testing.T) {
		const target = "127.0.0.1:5000"
		s := testServer(t, Config{}, target+"\n")
		events, own := &syncBuffer{}, &syncBuffer{}
		s.events.out = events
		s.log.SetOutput(own)
		s.connects = make(chan struct{}, 2)
		p := memServe(t, s)
		ln, err := p.far.Listen(target)
		must(t, err)
		serveFarEnd(t, ln)
		var open []net.Conn
		for range 2 {
			c, _, code := p.connect(target)
			if code != http.StatusOK {
				t.Fatalf("within the limit: %d", code)
			}
			_, _ = c.Write([]byte("e")) // the far end holds it until the client's end
			open = append(open, c)
		}
		for range 2 {
			code, body := p.refused(target)
			if code != http.StatusServiceUnavailable || !strings.Contains(body, "2 tunnels open already") {
				t.Errorf("past the limit: %d %q", code, body)
			}
		}
		if l := events.String(); strings.Count(l, `"event":"blocked"`) != 2 || !strings.Contains(l, `"reason":"2 tunnels open already","refused":true`) {
			t.Errorf("the event log: %s", l)
		}
		if l := own.String(); strings.Count(l, "tunnels open at once") != 1 {
			t.Errorf("the proxy's log: %q", l)
		}
		// A tunnel that ends makes room at once.
		_ = open[0].Close()
		synctest.Wait()
		if _, _, code := p.connect(target); code != http.StatusOK {
			t.Errorf("a tunnel ended, and a new one still gets %d", code)
		}
	})
}

// A tunnel to one of the VM's links.
func TestLinkTunnelSplices(t *testing.T) {
	t.Parallel()
	s, dir, _, addr := linkProxy(t)
	sock := LinkSocket(dir, linkVM, netip.MustParseAddr("127.77.0.1"), 5432)
	ln, err := new(net.ListenConfig).Listen(t.Context(), "unix", sock)
	must(t, err)
	f := serveFarEnd(t, ln)
	writeLinkList(t, s.links.file, linkEntry(dir, "127.77.0.1", 5432, "db"))
	checkTunnel(t, f, func() (net.Conn, io.Reader) { return connectTo(t, addr, "db:5432") })
}

// splice closes a connection with no writing side of its own to close at the end of what the
// client sent, and returns once the server's bytes end, both connections closed.
func TestSpliceEnds(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		client, vm := net.Pipe()
		server, far := net.Pipe()
		done := make(chan struct{})
		go func() { splice(client, server, nil); close(done) }()
		go func() { _, _ = vm.Write([]byte("hi")); _ = vm.Close() }()
		if b, err := io.ReadAll(far); string(b) != "hi" || err != nil {
			t.Errorf("the server read %q, %v", b, err)
		}
		_ = far.Close()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("splice goes on after the server's end")
		}
		if _, err := client.Write([]byte("x")); err == nil {
			t.Error("the client's connection is open")
		}
	})
}

// hijack answers that the tunnel is open, and the bytes the client sent along with its request
// are the connection's first.
func TestHijack(t *testing.T) {
	t.Parallel()
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	must(t, err)
	var cs conns
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		c, err := hijack(w)
		if err != nil {
			return
		}
		b := make([]byte, 5)
		_, _ = io.ReadFull(c, b)
		_, _ = c.Write(append([]byte("read:"), b...))
		_ = c.Close()
	})}
	go func() { _ = srv.Serve(cs.track(ln)) }()
	t.Cleanup(func() { _ = srv.Close() })
	c, err := new(net.Dialer).DialContext(t.Context(), "tcp", ln.Addr().String())
	must(t, err)
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	_, _ = io.WriteString(c, "CONNECT x.test:443 HTTP/1.1\r\nHost: x.test:443\r\n\r\nearly")
	b, _ := io.ReadAll(c)
	if string(b) != "HTTP/1.1 200 Connection established\r\n\r\nread:early" {
		t.Errorf("got %q", b)
	}
	// Closed, the connection is let go of.
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if len(cs.open) != 0 {
		t.Errorf("%d connections still held once closed", len(cs.open))
	}
}

// The proxy's close ends every connection a handler took from the HTTP server, which http.Server's
// Close leaves, a CONNECT's (hijack) or an upgrade's that the handler took itself, as
// httputil.ReverseProxy does for a WebSocket, and waits for the handlers to return; a request
// after it is refused.
func TestCloseConns(t *testing.T) {
	t.Parallel()
	leaktest.Check(t)
	synctest.Test(t, testCloseConns)
}

func testCloseConns(t *testing.T) {
	var n memnet.Network
	ln, err := n.Listen("proxy")
	must(t, err)
	var cs conns
	returned := make(chan string, 2)
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: cs.handle(func(w http.ResponseWriter, r *http.Request) {
		var c io.Reader
		if r.Method == http.MethodConnect {
			b, err := hijack(w)
			if err != nil {
				return
			}
			c = b
		} else { // an upgrade, taken as ReverseProxy takes one
			raw, buf, err := http.NewResponseController(w).Hijack()
			if err != nil {
				return
			}
			_, _ = io.WriteString(raw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
			c = buf
		}
		_, _ = io.Copy(io.Discard, c) // until the close ends it
		returned <- r.Method
	})}
	go func() { _ = srv.Serve(cs.track(ln)) }()
	open := func(req string) (net.Conn, *bufio.Reader) {
		c, err := ln.Dial()
		must(t, err)
		t.Cleanup(func() { _ = c.Close() })
		_, _ = io.WriteString(c, req)
		r := bufio.NewReader(c)
		if line, _ := r.ReadString('\n'); !strings.HasPrefix(line, "HTTP/1.1 200") && !strings.HasPrefix(line, "HTTP/1.1 101") {
			t.Fatalf("%q: %q", req, line)
		}
		return c, r
	}
	tunnel, tr := open("CONNECT x.test:443 HTTP/1.1\r\nHost: x.test:443\r\n\r\n")
	upgrade, ur := open("GET /ws HTTP/1.1\r\nHost: x.test\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
	_ = srv.Close() // as the proxy's lifeline ends it: both go on
	for _, c := range []net.Conn{tunnel, upgrade} {
		if _, err := c.Write([]byte("x")); err != nil {
			t.Fatalf("a connection ended with the HTTP server: %v", err)
		}
	}
	start := time.Now()
	if !cs.close(closeWait) {
		t.Fatal("close: a handler is still running")
	}
	if took := time.Since(start); took != 0 {
		t.Errorf("close took %s with every handler ending at once", took)
	}
	if len(returned) != 2 {
		t.Errorf("close returned before the handlers: %d of 2 had", len(returned))
	}
	// Each one's end.
	for _, r := range []*bufio.Reader{tr, ur} {
		if _, err := io.ReadAll(r); err != nil {
			t.Errorf("a connection after the close: %v", err)
		}
	}
	// A request after the close is refused.
	w := httptest.NewRecorder()
	cs.handle(func(http.ResponseWriter, *http.Request) { t.Error("a request after the close ran") })(w,
		httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://x.test/", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("a request after the close: %d, want 503", w.Code)
	}
}

// A handler that doesn't end with its connection, such as one still dialing, is waited for, but
// for closeWait at most, to the moment on the bubble's clock: close then reports it.
func TestCloseConnsWaitsBounded(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var cs conns
		release := make(chan struct{})
		h := cs.handle(func(http.ResponseWriter, *http.Request) { <-release })
		go h(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://x.test/", nil))
		synctest.Wait()
		start := time.Now()
		if cs.close(closeWait) {
			t.Error("close: no handler running, though one is")
		}
		if took := time.Since(start); took != closeWait {
			t.Errorf("close waited %s, want %s", took, closeWait)
		}
		close(release)
	})
}

// A tunnel whose far end says nothing still ends once its client's connection is closed, as the
// proxy's close closes it: the far end's connection closes too.
func TestSpliceEndsWhenItsClientCloses(t *testing.T) {
	t.Parallel()
	leaktest.Check(t)
	synctest.Test(t, func(t *testing.T) {
		// memnet's, which close their writing side alone, as TCP's do: a net.Pipe's would close
		// whole.
		client, vm := memnet.Pipe()
		server, far := memnet.Pipe()
		defer func() { _ = vm.Close(); _ = far.Close() }()
		done := make(chan struct{})
		go func() {
			defer close(done)
			splice(client, server, nil)
		}()
		_ = client.Close()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("splice goes on after its client's connection closed")
		}
		if _, err := io.ReadAll(far); err != nil {
			t.Errorf("the far end's connection: %v", err)
		}
	})
}

// earlyServer is a tunnel's far end that has answered and closed while its client still sends:
// every write to it fails, and its answer reads to the end, until splice closes it.
type earlyServer struct {
	net.Conn
	answer *bytes.Reader
	mu     sync.Mutex
	closed bool
}

func (s *earlyServer) Read(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, net.ErrClosed
	}
	return s.answer.Read(b)
}

func (s *earlyServer) Write([]byte) (int, error) { return 0, syscall.EPIPE }

func (s *earlyServer) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *earlyServer) CloseWrite() error { return nil }

// A server that answers early and closes while the client still sends, such as a 413 to an
// upload, fails splice's writes to it; the client still gets its whole answer.
func TestSpliceKeepsAnEarlyAnswer(t *testing.T) {
	t.Parallel()
	answer := bytes.Repeat([]byte("413 "), 1<<20)
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	must(t, err)
	defer func() { _ = ln.Close() }()
	vm, err := new(net.Dialer).DialContext(t.Context(), "tcp", ln.Addr().String())
	must(t, err)
	defer func() { _ = vm.Close() }()
	client, err := ln.Accept()
	must(t, err)
	done := make(chan struct{})
	go func() {
		defer close(done)
		splice(client, &earlyServer{answer: bytes.NewReader(answer)}, nil)
	}()
	// The upload's first bytes, which the server refuses: the client hasn't closed its side, so
	// it's still sending as far as the tunnel knows. (Bytes splice never read would reset the
	// connection as it closes, which drops what the client hadn't read yet, on any tunnel.)
	if _, err := vm.Write(make([]byte, 1024)); err != nil {
		t.Fatal(err)
	}
	_ = vm.SetReadDeadline(time.Now().Add(10 * time.Second))
	got, err := io.ReadAll(vm)
	if len(got) != len(answer) {
		t.Errorf("the client got %d bytes of the server's %d: %v", len(got), len(answer), err)
	}
	<-done
}

// Once the proxy closes, a connection its listener still accepts is closed before anything of it
// is served.
func TestConnectAfterClose(t *testing.T) {
	t.Parallel()
	s := testServer(t, Config{}, "new.test")
	addr := listen(t, s)
	s.closeConns()
	c, err := new(net.Dialer).DialContext(t.Context(), "tcp", addr)
	must(t, err)
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	_, _ = io.WriteString(c, "CONNECT new.test:443 HTTP/1.1\r\nHost: new.test:443\r\n\r\n")
	if b, err := io.ReadAll(c); len(b) != 0 || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("a CONNECT after the close got %q, %v", b, err)
	}
}

// The links' addresses are 127.77.0.0/16, as themselves or mapped into IPv6, and LinkAddr gives
// each of them.
func TestIsLinkAddr(t *testing.T) {
	t.Parallel()
	for s, want := range map[string]bool{
		"127.77.0.1": true, "127.77.255.254": true, "127.77.0.0": true, "127.77.255.255": true, "::ffff:127.77.0.1": true,
		"127.0.0.1": false, "127.78.0.0": false, "127.76.255.255": false, "::1": false, "::ffff:127.0.0.1": false,
		"10.77.0.1": false, "0.0.0.0": false, "::": false, "::7f4d:1": false,
	} {
		if got := IsLinkAddr(netip.MustParseAddr(s)); got != want {
			t.Errorf("IsLinkAddr(%s) = %v", s, got)
		}
	}
	if IsLinkAddr(netip.Addr{}) {
		t.Error("IsLinkAddr(the zero Addr) = true")
	}
	for i := range 0x10000 {
		a := LinkAddr(uint16(i))
		if want := fmt.Sprintf("127.77.%d.%d", i>>8, i&0xff); a.String() != want || !IsLinkAddr(a) {
			t.Fatalf("LinkAddr(%d) = %s, IsLinkAddr %v; want %s", i, a, IsLinkAddr(a), want)
		}
	}
}

// The local proxy's address is 127.0.77.1, as itself or mapped into IPv6, and no link's.
func TestIsLocalProxyAddr(t *testing.T) {
	t.Parallel()
	for s, want := range map[string]bool{
		"127.0.77.1": true, "::ffff:127.0.77.1": true,
		"127.0.0.1": false, "127.0.77.2": false, "127.77.0.1": false, "::1": false, "0.0.0.0": false,
	} {
		if got := IsLocalProxyAddr(netip.MustParseAddr(s)); got != want {
			t.Errorf("IsLocalProxyAddr(%s) = %v", s, got)
		}
	}
	if IsLinkAddr(netip.MustParseAddr(LocalProxyAddr)) || IsLocalProxyAddr(netip.Addr{}) {
		t.Error("the local proxy's address is a link's, or the zero Addr is the local proxy's")
	}
}
