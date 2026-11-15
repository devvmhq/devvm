package proxy

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/redact"
)

// listenServer serves s on a port of its own with the server its listener gets (httpServer), and
// returns its address.
func listenServer(t *testing.T, s *server) string {
	t.Helper()
	p := httptest.NewUnstartedServer(nil)
	p.Config = httpServer(s)
	p.Listener = s.conns.track(p.Listener)
	p.Start()
	t.Cleanup(p.Close)
	return p.Listener.Addr().String()
}

// dialProxy connects to addr, for at most 10 seconds.
func dialProxy(t *testing.T, addr string) net.Conn {
	t.Helper()
	var d net.Dialer
	c, err := d.DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	return c
}

// The proxy's listener closes a connection idle between requests after idleTimeout, and one whose
// request lacks the password after its 407, so nothing without it keeps a connection.
func TestProxyConnectionBounds(t *testing.T) {
	s := testServer(t, Config{Password: redact.NewSecret("k3y")}, "")
	if srv := httpServer(s); srv.IdleTimeout != idleTimeout || idleTimeout <= 0 || srv.ReadHeaderTimeout <= 0 || srv.MaxHeaderBytes != maxHeaderBytes {
		t.Errorf("idle %v, headers %v and %d bytes", srv.IdleTimeout, srv.ReadHeaderTimeout, srv.MaxHeaderBytes)
	}
	c := dialProxy(t, listenServer(t, s))
	_, _ = io.WriteString(c, "GET http://example.com/ HTTP/1.1\r\nHost: example.com\r\n\r\n")
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusProxyAuthRequired {
		t.Fatalf("without the password: %d", resp.StatusCode)
	}
	if _, err := br.ReadByte(); !errors.Is(err, io.EOF) {
		t.Errorf("after the 407: %v, want the connection closed", err)
	}
}

// The proxy as Main makes it has its limits set.
func TestProxyLimitsSet(t *testing.T) {
	if maxConns < maxConnects {
		t.Errorf("maxConns %d is below maxConnects %d: a tunnel past its limit would get no 503", maxConns, maxConnects)
	}
	s := testServer(t, Config{}, "")
	if s.conns.max != maxConns || s.conns.waitMax != maxWaiting || s.conns.full == nil || cap(s.requests.slots) != maxRequests {
		t.Errorf("conns %d (waiting %d, full set %v), requests %d; want %d, %d, true, %d",
			s.conns.max, s.conns.waitMax, s.conns.full != nil, cap(s.requests.slots), maxConns, maxWaiting, maxRequests)
	}
}

// keptAlive sends one request without the password on c and reads its answer, leaving c open.
func keptAlive(t *testing.T, c net.Conn) int {
	t.Helper()
	_, _ = io.WriteString(c, "GET http://devvm.proxy/blocked HTTP/1.1\r\nHost: devvm.proxy\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

// closedSoon reports whether c is closed within 5 seconds.
func closedSoon(c net.Conn) bool {
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err := c.Read(make([]byte, 1))
	return err != nil && !errors.Is(err, os.ErrDeadlineExceeded)
}

// A connection counts toward maxConns from its first request with the VM's password, idle and plain
// HTTP ones included: a request on one past it gets 503, the log says so once a minute, and a
// connection that closes makes room.
func TestProxyConnectionLimit(t *testing.T) {
	s := testServer(t, Config{}, "")
	out := logs(s)
	s.conns.max = 3
	addr := listenServer(t, s)
	var held []net.Conn
	for range 3 {
		c := dialProxy(t, addr)
		if code := keptAlive(t, c); code != http.StatusOK {
			t.Fatalf("within the limit: %d", code)
		}
		held = append(held, c)
	}
	for range 2 {
		c := dialProxy(t, addr)
		if code := keptAlive(t, c); code != http.StatusServiceUnavailable {
			t.Errorf("past the limit: %d, want 503", code)
		}
		if !closedSoon(c) {
			t.Error("a connection past the limit kept open")
		}
	}
	if n := strings.Count(out.String(), "connections open at once"); n != 1 {
		t.Errorf("the log says the limit %d times, want once:\n%s", n, out.String())
	}
	_ = held[0].Close()
	deadline := time.Now().Add(5 * time.Second)
	for keptAlive(t, dialProxy(t, addr)) != http.StatusOK {
		if time.Now().After(deadline) {
			t.Fatal("no room after a connection closed")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Connections that have sent no request with the password never take the VM's room: they wait in
// a pool of their own, maxWaiting at most, where a new one closes the one that waited longest.
func TestProxyWaitingConnections(t *testing.T) {
	s := testServer(t, Config{Password: redact.NewSecret("k3y")}, "")
	s.conns.max, s.conns.waitMax = 1, 2
	addr := listenServer(t, s)
	first := dialProxy(t, addr)
	time.Sleep(100 * time.Millisecond) // accepted first
	dialProxy(t, addr)
	time.Sleep(100 * time.Millisecond)
	dialProxy(t, addr) // past the pool
	if !closedSoon(first) {
		t.Error("the connection that waited longest kept open past the pool")
	}
	// The VM's own connection still gets its room, however many wait.
	c := dialProxy(t, addr)
	_, _ = io.WriteString(c, "GET http://devvm.proxy/blocked HTTP/1.1\r\nHost: devvm.proxy\r\nProxy-Authorization: Basic ZGV2dm06azN5\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("the VM's request with others waiting: %d", resp.StatusCode)
	}
}

// The proxy has at most maxRequests requests upstream at once, its tunnels' included: one past
// them gets 503, the log says so once a minute, and one that ends makes room.
func TestUpstreamRequestLimit(t *testing.T) {
	held, stop := make(chan struct{}, 1), make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hold" {
			held <- struct{}{}
			<-stop
		}
	}))
	t.Cleanup(up.Close)
	upAddr := up.Listener.Addr().String()
	_, port, _ := net.SplitHostPort(upAddr)
	s := testServer(t, Config{}, "127.0.0.1:"+port+"\n")
	out := logs(s)
	s.requests = newRequests(1, s.log)
	addr := listenServer(t, s)
	var once sync.Once
	release := func() { once.Do(func() { close(stop) }) }
	t.Cleanup(release) // before the servers close, which wait for their handlers
	get := func(path string) int {
		c := dialProxy(t, addr)
		_, _ = fmt.Fprintf(c, "GET http://%s%s HTTP/1.1\r\nHost: %s\r\n\r\n", upAddr, path, upAddr)
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	hold := dialProxy(t, addr)
	_, _ = fmt.Fprintf(hold, "GET http://%s/hold HTTP/1.1\r\nHost: %s\r\n\r\n", upAddr, upAddr)
	<-held
	for range 2 {
		if code := get("/x"); code != http.StatusServiceUnavailable {
			t.Errorf("past the limit: %d, want 503", code)
		}
	}
	if n := strings.Count(out.String(), "requests upstream at once"); n != 1 {
		t.Errorf("the log says the limit %d times, want once:\n%s", n, out.String())
	}
	release()
	deadline := time.Now().Add(5 * time.Second)
	for get("/x") != http.StatusOK {
		if time.Now().After(deadline) {
			t.Fatal("no room after a request ended")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// An intercepted tunnel's requests take the same slots as plain HTTP's: with one held, another
// tunnel's request gets 503.
func TestUpstreamRequestLimitIntercepted(t *testing.T) {
	held, stop := make(chan struct{}, 1), make(chan struct{})
	s, port, _, client := interceptSetup(t, func(_ http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hold" {
			held <- struct{}{}
			<-stop
		}
	})
	var once sync.Once
	release := func() { once.Do(func() { close(stop) }) }
	t.Cleanup(release) // before the servers close, which wait for their handlers
	s.requests = newRequests(1, s.log)
	base := fmt.Sprintf("https://localhost:%d", port)
	go func() {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/hold", nil)
		if resp, err := client.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-held
	// Another tunnel of its own: the first client's holds its one connection.
	other := &http.Client{Transport: client.Transport.(*http.Transport).Clone()}
	if code, _ := get(t, other, base+"/x", ""); code != http.StatusServiceUnavailable {
		t.Errorf("an intercepted request past the limit: %d, want 503", code)
	}
	release()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if code, _ := get(t, other, base+"/x", ""); code == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no room after the intercepted request ended")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A plain ws:// upgrade through the proxy gets its 101, and the connection carries both ways, quiet
// or not.
func TestPlainWebSocket(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		c, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		_ = brw.Flush()
		line, _ := brw.ReadString('\n')
		_, _ = c.Write([]byte("echo " + line))
	}))
	t.Cleanup(up.Close)
	upAddr := up.Listener.Addr().String()
	_, port, _ := net.SplitHostPort(upAddr)
	s := testServer(t, Config{}, "127.0.0.1:"+port+"\n")
	s.goneWait = 200 * time.Millisecond
	c := dialProxy(t, listenServer(t, s))
	_, _ = fmt.Fprintf(c, "GET http://%s/ws HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n", upAddr, upAddr)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close() // a 101's body is empty: the connection goes on through br
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("a ws:// upgrade: %d, want 101", resp.StatusCode)
	}
	// A WebSocket that stays quiet past goneWait goes on: only a plain request's client stopping
	// ends one.
	time.Sleep(3 * s.goneWait)
	_, _ = io.WriteString(c, "ping\n")
	if line, _ := br.ReadString('\n'); line != "echo ping\n" {
		t.Errorf("through the upgrade: %q", line)
	}
}

// A plain-HTTP request whose client has gone ends once nothing has passed on for goneWait, so its
// upstream isn't waited for without end; a client that only stopped sending (a half-close) still
// gets an answer that keeps coming.
func TestPlainHTTPEndsWithClient(t *testing.T) {
	ended, stop := make(chan struct{}), make(chan struct{})
	var once sync.Once
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/silent" {
			select {
			case <-r.Context().Done():
				once.Do(func() { close(ended) })
			case <-stop:
			}
			return
		}
		for range 6 {
			_, _ = io.WriteString(w, "x")
			_ = http.NewResponseController(w).Flush()
			time.Sleep(500 * time.Millisecond) // within goneWait, past half of it
		}
	}))
	t.Cleanup(up.Close)
	upAddr := up.Listener.Addr().String()
	_, port, _ := net.SplitHostPort(upAddr)
	s := testServer(t, Config{}, "127.0.0.1:"+port+"\n")
	s.goneWait = time.Second
	addr := listenServer(t, s)
	t.Cleanup(func() { close(stop) }) // before the servers close, which wait for their handlers

	c := dialProxy(t, addr)
	_, _ = fmt.Fprintf(c, "GET http://%s/silent HTTP/1.1\r\nHost: %s\r\n\r\n", upAddr, upAddr)
	time.Sleep(50 * time.Millisecond)
	_ = c.Close()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Error("the upstream request goes on with its client gone")
	}

	c = dialProxy(t, addr)
	_, _ = fmt.Fprintf(c, "GET http://%s/trickle HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", upAddr, upAddr)
	_ = c.(*net.TCPConn).CloseWrite()
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(b) != "xxxxxx" {
		t.Errorf("a half-closed client got %q, want all of it", b)
	}
}

// serveOneConn is a connection to serveOne, serving 200 for every request.
func serveOneConn(t *testing.T) net.Conn {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		serveOne(c, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), connServing{header: 5 * time.Second, idle: 5 * time.Second}, log.New(io.Discard, "", 0))
	}()
	var d net.Dialer
	c, err := d.DialContext(t.Context(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	return c
}

// serveOne, which serves intercepted tunnels, transparent connections and the 403 page, bounds what
// each makes the proxy hold: headers past maxHeaderBytes get 431, as the proxy's own listener's do,
// where http.Server's default takes a megabyte.
func TestServeOneHeaderLimit(t *testing.T) {
	for _, tc := range []struct {
		size int
		want int
	}{{maxHeaderBytes / 2, http.StatusOK}, {maxHeaderBytes + 8<<10, http.StatusRequestHeaderFieldsTooLarge}} {
		c := serveOneConn(t)
		go func() {
			_, _ = fmt.Fprintf(c, "GET / HTTP/1.1\r\nHost: x\r\nX-Big: %s\r\n\r\n", strings.Repeat("a", tc.size))
		}()
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil {
			t.Fatalf("%d bytes of headers: %v", tc.size, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("%d bytes of headers: %d, want %d", tc.size, resp.StatusCode, tc.want)
		}
	}
}

// Over HTTP/2, serveOne lets a connection ask for streamsMax requests at once, each a request
// upstream, where http.Server's default lets it ask for 250: its SETTINGS say so.
func TestServeOneStreamsLimit(t *testing.T) {
	c := serveOneConn(t)
	// The client's preface and an empty SETTINGS frame.
	if _, err := io.WriteString(c, "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n\x00\x00\x00\x04\x00\x00\x00\x00\x00"); err != nil {
		t.Fatal(err)
	}
	// The server's first frame is its SETTINGS: a 9-byte header, then 6 bytes for each setting.
	hdr := make([]byte, 9)
	if _, err := io.ReadFull(c, hdr); err != nil {
		t.Fatal(err)
	}
	if hdr[3] != 0x4 {
		t.Fatalf("first frame of type %d, want SETTINGS", hdr[3])
	}
	payload := make([]byte, int(hdr[0])<<16|int(hdr[1])<<8|int(hdr[2]))
	if _, err := io.ReadFull(c, payload); err != nil {
		t.Fatal(err)
	}
	const maxConcurrentStreams = 0x3
	for i := 0; i+6 <= len(payload); i += 6 {
		if binary.BigEndian.Uint16(payload[i:]) == maxConcurrentStreams {
			if got := binary.BigEndian.Uint32(payload[i+2:]); got != streamsMax {
				t.Errorf("MAX_CONCURRENT_STREAMS %d, want %d", got, streamsMax)
			}
			return
		}
	}
	t.Error("no MAX_CONCURRENT_STREAMS in the server's SETTINGS")
}
