package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// transparentServer is a proxy of the test's own with a CA, a policy of rules and its events and
// log in a buffer, which waits a fifth of a second for a transparent connection's first bytes, and
// the address it listens on.
func transparentServer(t *testing.T, c Config, rules string) (*server, string, *syncBuffer) {
	t.Helper()
	dir := t.TempDir()
	writeCA(t, dir)
	c.CA, c.CAKey = filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca.key")
	s := testServer(t, c, rules)
	ev := logs(s)
	s.helloWait = 200 * time.Millisecond
	return s, listen(t, s), ev
}

// openTransparent connects to the proxy at addr as a VM's devvm-transparent does, with socat's
// CONNECT: HTTP/1.0, no Host header, password pw, the reserved target at port. It returns the
// connection once the proxy has answered 200.
func openTransparent(ctx context.Context, t *testing.T, addr, pw string, port int) bufConn {
	t.Helper()
	c, err := new(net.Dialer).DialContext(ctx, "tcp", addr)
	must(t, err)
	t.Cleanup(func() { _ = c.Close() })
	must(t, c.SetDeadline(time.Now().Add(10*time.Second)))
	fmt.Fprintf(c, "CONNECT %s:%d HTTP/1.0\r\nProxy-Authorization: %s\r\n\r\n", TransparentHost, port, authOf(ProxyUser, pw))
	r := bufio.NewReader(c)
	resp, err := http.ReadResponse(r, &http.Request{Method: http.MethodConnect})
	must(t, err)
	_ = resp.Body.Close() // a CONNECT's 200 has none: the tunnel's bytes follow it
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the reserved target: %d", resp.StatusCode)
	}
	return bufConn{c, r}
}

// eventsOf is the events in b, in order: its lines that are JSON.
func eventsOf(b *syncBuffer) []Event {
	var out []Event
	for l := range strings.SplitSeq(b.String(), "\n") {
		var e Event
		if strings.HasPrefix(l, "{") && json.Unmarshal([]byte(l), &e) == nil {
			out = append(out, e)
		}
	}
	return out
}

// waitEvent waits up to 5 seconds for an event in b that ok takes, and returns it.
func waitEvent(t *testing.T, b *syncBuffer, ok func(Event) bool) Event {
	t.Helper()
	for range 250 {
		if i := slices.IndexFunc(eventsOf(b), ok); i >= 0 {
			return eventsOf(b)[i]
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no such event in %s", b.String())
	return Event{}
}

// closed reports whether c's far end closes it within 2 seconds, sending nothing.
func closed(c net.Conn) bool {
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := c.Read(make([]byte, 1))
	return n == 0 && errors.Is(err, io.EOF)
}

// portOf is the port of a listener's address.
func portOf(t *testing.T, a net.Addr) int {
	t.Helper()
	_, p, err := net.SplitHostPort(a.String())
	must(t, err)
	n, err := strconv.Atoi(p)
	must(t, err)
	return n
}

// tlsUpstream is an HTTPS server for localhost with a CA of its own (not the proxy's), answering
// "far end", its port, and a pool holding its CA.
func tlsUpstream(t *testing.T) (int, *x509.CertPool) {
	t.Helper()
	upCA := testCA(t, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour))
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "far end") }))
	cert, err := upCA.leafFor("localhost")
	must(t, err)
	up.TLS = &tls.Config{Certificates: []tls.Certificate{*cert}}
	up.StartTLS()
	t.Cleanup(up.Close)
	pool := x509.NewCertPool()
	pool.AddCert(upCA.cert)
	return portOf(t, up.Listener.Addr()), pool
}

// rawHello is a TLS ClientHello record naming name in its SNI, as it is, or none for "": what no
// TLS library would send for an address or a name that isn't ASCII.
func rawHello(name string) []byte {
	be := func(n, size int) []byte { // n's last size bytes, big-endian: a test's few hundred bytes fit
		b := binary.BigEndian.AppendUint32(nil, uint32(n)) //nolint:gosec // G115: a hello's lengths, far below 2^32
		return b[4-size:]
	}
	var ext []byte
	if name != "" {
		list := slices.Concat([]byte{0}, be(len(name), 2), []byte(name))
		data := slices.Concat(be(len(list), 2), list)
		ext = slices.Concat([]byte{0, 0}, be(len(data), 2), data)
	}
	body := slices.Concat([]byte{3, 3}, make([]byte, 32), []byte{0, 0, 2, 0x13, 0x01, 1, 0}, be(len(ext), 2), ext)
	hs := slices.Concat([]byte{1}, be(len(body), 3), body)
	return slices.Concat([]byte{0x16, 3, 1}, be(len(hs), 2), hs)
}

func TestRawHello(t *testing.T) {
	t.Parallel()
	for _, n := range []string{"example.com", "1.2.3.4", "Kubernetes.io", ""} {
		if got, ok := sniOf(t.Context(), rawHello(n)); got != n || !ok {
			t.Errorf("sniOf(rawHello(%q)) = %q, %v", n, got, ok)
		}
	}
}

// A tool that skipped the proxy, its name the stand-in address: its TLS connection to an allowed
// host, in any case, goes to the far end as it is, the far end's own certificate and all, judged
// and logged as CONNECT host:port, marked as without proxy settings.
func TestTransparentTLSAllowed(t *testing.T) {
	t.Parallel()
	port, pool := tlsUpstream(t)
	_, addr, ev := transparentServer(t, Config{Password: "pw"}, fmt.Sprintf("localhost:%d\n127.0.0.1:%d\n", port, port))
	tc := tls.Client(openTransparent(t.Context(), t, addr, "pw", port), &tls.Config{ServerName: "LocalHost", RootCAs: pool})
	fmt.Fprint(tc, "GET / HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(tc), nil)
	must(t, err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "far end" {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
	e := waitEvent(t, ev, func(e Event) bool { return e.Event == "tunnel" })
	if e.Host != "localhost" || e.Port != port || e.Via != ViaDNS || e.Rule != fmt.Sprintf("localhost:%d", port) || e.Method != http.MethodConnect {
		t.Errorf("event %+v", e)
	}
}

// A ClientHello that names no host the proxy can judge is refused, with no handshake: logged as a
// refused block of the reserved target, or of the link's name, and closed.
func TestTransparentSNI(t *testing.T) {
	t.Parallel()
	_, addr, ev := transparentServer(t, Config{}, "**\n")
	for _, c := range []struct{ sni, host, why string }{
		{"", TransparentHost, reasonNoSNI},
		{"example.com.", TransparentHost, reasonHelloBroken}, // crypto/tls refuses a trailing dot as it parses
		{"1.2.3.4", TransparentHost, reasonSNIAddr},
		{"::1", TransparentHost, reasonSNIAddr},
		{"Kubernetes.io", TransparentHost, "isn't ASCII"},
		{"a..b", TransparentHost, "isn't a host name"},
		{"bad!host", TransparentHost, "isn't a host name"},
		{"example.123", TransparentHost, "isn't a host name"},
		{"devvm.proxy", TransparentHost, reasonOwnDomain},
		{"x.devvm.proxy", TransparentHost, reasonOwnDomain},
		{TransparentHost, TransparentHost, reasonOwnDomain},
		{"x.devvm.internal", "x.devvm.internal", reasonViaLink},
	} {
		c0 := openTransparent(t.Context(), t, addr, "", 443)
		_, _ = c0.Write(rawHello(c.sni))
		if !closed(c0) {
			t.Errorf("%q: not closed at once", c.sni)
		}
		e := waitEvent(t, ev, func(e Event) bool { return e.Host == c.host && strings.Contains(e.Reason, c.why) })
		if e.Event != "blocked" || !e.Refused || e.Via != ViaDNS || e.Port != 443 {
			t.Errorf("%q: event %+v", c.sni, e)
		}
	}
}

// A host off the allowlist gets a certificate for its name from the VM's CA, then the proxy's 403
// for every request, over HTTP/1.1 and HTTP/2, with the command that allows it: one event.
func TestTransparentBlockedPage(t *testing.T) {
	t.Parallel()
	s, addr, ev := transparentServer(t, Config{}, "")
	pool := x509.NewCertPool()
	pool.AddCert(s.ca.cert)
	tc := tls.Client(openTransparent(t.Context(), t, addr, "", 443), &tls.Config{ServerName: "blocked.test", RootCAs: pool, NextProtos: []string{"http/1.1"}})
	must(t, tc.HandshakeContext(t.Context()))
	if l := tc.ConnectionState().PeerCertificates[0]; !slices.Equal(l.DNSNames, []string{"blocked.test"}) {
		t.Errorf("the leaf names %v", l.DNSNames)
	}
	fmt.Fprint(tc, "GET /a HTTP/1.1\r\nHost: blocked.test\r\n\r\nGET /b HTTP/1.1\r\nHost: other.test\r\n\r\n")
	r := bufio.NewReader(tc)
	for range 2 {
		resp, err := http.ReadResponse(r, nil)
		must(t, err)
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden || resp.Header.Get("X-Devvm-Blocked") != "blocked.test" ||
			!strings.Contains(string(body), "devvm network allow blocked.test") {
			t.Errorf("got %d %q %q", resp.StatusCode, resp.Header.Get("X-Devvm-Blocked"), body)
		}
	}
	tr := &http.Transport{ForceAttemptHTTP2: true, DialTLSContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		c := tls.Client(openTransparent(ctx, t, addr, "", 443), &tls.Config{ServerName: "blocked.test", RootCAs: pool, NextProtos: []string{"h2"}})
		return c, c.HandshakeContext(ctx)
	}}
	defer tr.CloseIdleConnections()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://blocked.test/", nil)
	resp, err := tr.RoundTrip(req)
	must(t, err)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || resp.ProtoMajor != 2 {
		t.Errorf("HTTP/2: %d over %s", resp.StatusCode, resp.Proto)
	}
	blocks := slices.DeleteFunc(eventsOf(ev), func(e Event) bool { return e.Event != "blocked" })
	if len(blocks) != 2 || blocks[0].Host != "blocked.test" || blocks[0].Via != ViaDNS || blocks[0].Refused {
		t.Errorf("blocks: %+v", blocks)
	}
}

// Without a CA, or once it has ended, a blocked host's connection closes with no handshake: the
// block is logged.
func TestTransparentBlockedNoCA(t *testing.T) {
	t.Parallel()
	s := testServer(t, Config{}, "")
	ev := logs(s)
	addr := listen(t, s)
	c := openTransparent(t.Context(), t, addr, "", 443)
	_, _ = c.Write(rawHello("blocked.test"))
	if !closed(c) {
		t.Error("not closed")
	}
	waitEvent(t, ev, func(e Event) bool { return e.Event == "blocked" && e.Host == "blocked.test" })
	ended, addr2, ev2 := transparentServer(t, Config{}, "")
	ended.ca = testCA(t, time.Now().Add(-48*time.Hour), time.Now().Add(-time.Hour))
	c = openTransparent(t.Context(), t, addr2, "", 443)
	_, _ = c.Write(rawHello("blocked.test"))
	if !closed(c) {
		t.Error("not closed once the CA ended")
	}
	waitEvent(t, ev2, func(e Event) bool { return e.Event == "blocked" && e.Host == "blocked.test" })
}

// A bound host's secret goes in for its own name alone: SNI A with Host A gets it, SNI A with Host
// B gets 421 and no secret, as through CONNECT, and the requests' events say how they came.
func TestTransparentIntercept(t *testing.T) {
	t.Parallel()
	s, port, _, _ := interceptSetup(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, r.Header.Get("Authorization")) //nolint:gosec // G705: a test's far end, telling the test what it got
	})
	ev := logs(s)
	addr := listen(t, s)
	pool := x509.NewCertPool()
	pool.AddCert(s.ca.cert)
	for _, c := range []struct {
		host string
		code int
		body string
	}{{"localhost", http.StatusOK, "Bearer real"}, {"other.test", http.StatusMisdirectedRequest, ""}} {
		tc := tls.Client(openTransparent(t.Context(), t, addr, "", port), &tls.Config{ServerName: "localhost", RootCAs: pool, NextProtos: []string{"http/1.1"}})
		fmt.Fprintf(tc, "GET / HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer PH\r\nConnection: close\r\n\r\n", c.host)
		resp, err := http.ReadResponse(bufio.NewReader(tc), nil)
		must(t, err)
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != c.code || c.body != "" && string(body) != c.body || strings.Contains(string(body), "real") && c.code != http.StatusOK {
			t.Errorf("Host %s: %d %q", c.host, resp.StatusCode, body)
		}
	}
	waitEvent(t, ev, func(e Event) bool { return e.Event == "intercept" && e.Via == ViaDNS })
	waitEvent(t, ev, func(e Event) bool { return e.Event == "forward" && e.Via == ViaDNS && len(e.Secrets) == 1 })
}

// Plain HTTP: each request is judged by its own Host, on the connection's port alone, forwarded as
// http:// whatever its request line says; a CONNECT, no Host, another port and the links' domain
// are refused, and devvm.proxy's endpoints answer.
func TestTransparentHTTP(t *testing.T) {
	t.Parallel()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "far end "+r.Host+" "+r.Header.Get("Proxy-Authorization")) //nolint:gosec // G705: a test's far end, telling the test what it got
	}))
	t.Cleanup(up.Close)
	port := portOf(t, up.Listener.Addr())
	_, addr, ev := transparentServer(t, Config{}, fmt.Sprintf("localhost:%d\n127.0.0.1:%d\n", port, port))
	hp := fmt.Sprintf("localhost:%d", port)
	c := openTransparent(t.Context(), t, addr, "", port)
	fmt.Fprintf(c, "GET /a HTTP/1.1\r\nHost: %s\r\n\r\nGET /b HTTP/1.1\r\nHost: blocked.test:%d\r\n\r\n", hp, port)
	for _, want := range []int{http.StatusOK, http.StatusForbidden} {
		resp, err := http.ReadResponse(c.r, nil)
		must(t, err)
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != want || want == http.StatusOK && string(body) != "far end "+hp+" " {
			t.Errorf("got %d %q, want %d", resp.StatusCode, body, want)
		}
	}
	for _, x := range []struct {
		req  string
		code int
		has  string
	}{
		{"GET / HTTP/1.1\r\nHost: localhost:1\r\n\r\n", http.StatusBadRequest, "this connection is to port"},
		{"CONNECT localhost:1 HTTP/1.1\r\nHost: localhost:1\r\n\r\n", http.StatusBadRequest, "not a proxy here"},
		{"GET / HTTP/1.0\r\n\r\n", http.StatusBadRequest, "no Host"},
		{"GET https://" + hp + "/ HTTP/1.1\r\nHost: " + hp + "\r\n\r\n", http.StatusOK, "far end"},
		{"GET / HTTP/1.1\r\nHost: db.devvm.internal\r\n\r\n", http.StatusForbidden, "is refused"},
		{"GET / HTTP/1.1\r\nHost: x.devvm.proxy\r\n\r\n", http.StatusForbidden, reasonOwnDomain},
		{"GET /blocked HTTP/1.1\r\nHost: devvm.proxy\r\n\r\n", http.StatusOK, "blocked.test"},
		// Its other endpoints need the proxy settings, with the password, which this way has none of.
		{"GET /ca.pem HTTP/1.1\r\nHost: devvm.proxy\r\n\r\n", http.StatusForbidden, "need the proxy settings"},
		{"GET /transparent/ports HTTP/1.1\r\nHost: devvm.proxy\r\n\r\n", http.StatusForbidden, "need the proxy settings"},
		{"GET /nix/nix-cache-info HTTP/1.1\r\nHost: devvm.proxy\r\n\r\n", http.StatusForbidden, "need the proxy settings"},
		{"GET http://devvm.proxy/blocked/new?after=0 HTTP/1.1\r\nHost: devvm.proxy\r\n\r\n", http.StatusForbidden, "need the proxy settings"},
	} {
		c := openTransparent(t.Context(), t, addr, "", port)
		fmt.Fprint(c, x.req)
		resp, err := http.ReadResponse(c.r, nil)
		must(t, err)
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != x.code || !strings.Contains(string(body), x.has) {
			t.Errorf("%q: %d %q", x.req, resp.StatusCode, body)
		}
	}
	p := new(http.Protocols)
	p.SetUnencryptedHTTP2(true)
	tr := &http.Transport{Protocols: p, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return openTransparent(ctx, t, addr, "", port), nil
	}}
	defer tr.CloseIdleConnections()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+hp+"/", nil)
	resp, err := tr.RoundTrip(req)
	must(t, err)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ProtoMajor != 2 {
		t.Errorf("HTTP/2 with prior knowledge: %d over %s", resp.StatusCode, resp.Proto)
	}
	waitEvent(t, ev, func(e Event) bool { return e.Event == "forward" && e.Via == ViaDNS && e.Path == "/a" })
	waitEvent(t, ev, func(e Event) bool {
		return e.Event == "blocked" && e.Via == ViaDNS && e.Host == "blocked.test" && !e.Refused
	})
}

// First bytes that name no host, or none in time, are refused: a refused block of the reserved
// target, and closed.
func TestTransparentFirstBytes(t *testing.T) {
	t.Parallel()
	_, addr, ev := transparentServer(t, Config{}, "**\n")
	for i, c := range []struct {
		first []byte
		why   string
	}{
		{nil, reasonNoBytes},
		{[]byte("SSH-2.0-OpenSSH_9.9\r\n"), reasonNotNamed},
		{[]byte{0, 0, 0, 8, 4, 0xd2, 0x16, 0x2f}, reasonNotNamed}, // Postgres's SSLRequest
		{[]byte("GE"), reasonNotNamed},
		{[]byte{0x16, 3, 1, 0, 0xff}, reasonHelloBroken},
		{[]byte{0x16, 3, 1, 0xff, 0xff}, reasonHelloBroken},
	} {
		c0 := openTransparent(t.Context(), t, addr, "", 5432)
		_, _ = c0.Write(c.first)
		if !closed(c0) {
			t.Errorf("%d: not closed", i)
		}
		if n := len(slices.DeleteFunc(eventsOf(ev), func(e Event) bool { return e.Reason != c.why })); n == 0 {
			t.Errorf("%d: no block for %q", i, c.why)
		}
	}
	for _, e := range eventsOf(ev) {
		if e.Host != TransparentHost || !e.Refused || e.Port != 5432 {
			t.Errorf("event %+v", e)
		}
	}
}

// The reserved target needs the VM's password, and a port of its own.
func TestTransparentTarget(t *testing.T) {
	t.Parallel()
	_, addr, _ := transparentServer(t, Config{Password: "pw"}, "")
	if code, _ := roundTrip(t, addr, "CONNECT "+TransparentHost+":443 HTTP/1.0\r\n\r\n"); code != http.StatusProxyAuthRequired {
		t.Errorf("no password: %d", code)
	}
	if code, _ := roundTrip(t, addr, "CONNECT "+TransparentHost+":443 HTTP/1.0\r\nProxy-Authorization: "+authOf(ProxyUser, "no")+"\r\n\r\n"); code != http.StatusProxyAuthRequired {
		t.Errorf("another password: %d", code)
	}
	for _, target := range []string{TransparentHost, TransparentHost + ":0", TransparentHost + ":65536", TransparentHost + ":44x", TransparentHost + ":+443"} {
		req := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\nProxy-Authorization: " + authOf(ProxyUser, "pw") + "\r\n\r\n"
		if code, _ := roundTrip(t, addr, req); code != http.StatusBadRequest {
			t.Errorf("%s: %d", target, code)
		}
	}
}

// Transparent connections count toward the VM's tunnels: one past the limit gets 503.
func TestTransparentCap(t *testing.T) {
	t.Parallel()
	s, addr, _ := transparentServer(t, Config{}, "")
	s.connects = make(chan struct{}, 1)
	s.helloWait = 5 * time.Second
	openTransparent(t.Context(), t, addr, "", 443)
	if code, _ := roundTrip(t, addr, "CONNECT "+TransparentHost+":443 HTTP/1.0\r\n\r\n"); code != http.StatusServiceUnavailable {
		t.Errorf("past the limit: %d", code)
	}
}

// On an open proxy, a transparent connection reaches what an open CONNECT does.
func TestTransparentOpen(t *testing.T) {
	t.Parallel()
	port, pool := tlsUpstream(t)
	_, addr, ev := transparentServer(t, Config{Open: true}, "")
	tc := tls.Client(openTransparent(t.Context(), t, addr, "", port), &tls.Config{ServerName: "localhost", RootCAs: pool})
	must(t, tc.HandshakeContext(t.Context()))
	waitEvent(t, ev, func(e Event) bool { return e.Event == "tunnel" && e.Rule == openRule && e.Via == ViaDNS })
}

// devvm.proxy is the proxy's own: any name under it, and devvm.proxy itself in a CONNECT, is refused
// before the links and the policy, on the open tier with a company proxy too, which sees nothing.
func TestOwnDomain(t *testing.T) {
	t.Parallel()
	s := testServer(t, Config{Open: true}, "")
	f := newFakeUpstream(t, "", http.StatusOK)
	useUpstream(t, s, f, "")
	addr := listen(t, s)
	for _, req := range []string{
		"CONNECT devvm.proxy:443 HTTP/1.1\r\nHost: devvm.proxy:443\r\n\r\n",
		"CONNECT x.devvm.proxy:443 HTTP/1.1\r\nHost: x.devvm.proxy:443\r\n\r\n",
		"CONNECT DEVVM.PROXY.:8443 HTTP/1.1\r\nHost: DEVVM.PROXY.:8443\r\n\r\n",
		"GET http://x.devvm.proxy/ HTTP/1.1\r\nHost: x.devvm.proxy\r\n\r\n",
		"GET http://" + TransparentHost + "/ HTTP/1.1\r\nHost: " + TransparentHost + "\r\n\r\n",
	} {
		if code, body := roundTrip(t, addr, req); code != http.StatusForbidden || !strings.Contains(body, reasonOwnDomain) {
			t.Errorf("%q: %d %q", req, code, body)
		}
	}
	if code, _ := roundTrip(t, addr, "GET http://devvm.proxy/blocked HTTP/1.1\r\nHost: devvm.proxy\r\n\r\n"); code != http.StatusOK {
		t.Errorf("devvm.proxy's endpoints: %d", code)
	}
	if r := f.requests(); len(r) > 0 {
		t.Errorf("the company proxy saw %q", r)
	}
}

// A tool that resolved a name itself and sends the stand-in address is told so.
func TestSentinelRefusal(t *testing.T) {
	t.Parallel()
	s := testServer(t, Config{}, "**\n198.18.0.0/15\n")
	addr := listen(t, s)
	for _, req := range []string{
		"CONNECT 198.18.0.1:443 HTTP/1.1\r\nHost: 198.18.0.1:443\r\n\r\n",
		"CONNECT [::ffff:198.18.0.1]:443 HTTP/1.1\r\nHost: [::ffff:198.18.0.1]:443\r\n\r\n",
		"GET http://198.18.0.1/ HTTP/1.1\r\nHost: 198.18.0.1\r\n\r\n",
	} {
		if code, body := roundTrip(t, addr, req); code != http.StatusForbidden || !strings.Contains(body, reasonSentinel) {
			t.Errorf("%q: %d %q", req, code, body)
		}
	}
	if !IsSentinelAddr(netip.MustParseAddr("::ffff:198.18.0.1")) || IsSentinelAddr(netip.MustParseAddr("198.18.0.2")) {
		t.Error("IsSentinelAddr")
	}
}

// A host with anything but printable ASCII is never folded into an ASCII one: CONNECT's target, a
// Host and the SNI check refuse it.
func TestHostsNotASCII(t *testing.T) {
	t.Parallel()
	kelvin := "Kubernetes.io"
	if h, _ := splitHostPort(kelvin+":443", 443); HostOK(h) || h == "kubernetes.io" {
		t.Errorf("splitHostPort folded it: %q", h)
	}
	if sniMatches(kelvin, "kubernetes.io") || !sniMatches("Kubernetes.IO", "kubernetes.io") {
		t.Error("sniMatches")
	}
	if h, why := sniHost(kelvin); h != "" || why == "" {
		t.Errorf("sniHost: %q %q", h, why)
	}
	s := testServer(t, Config{}, "kubernetes.io\n")
	addr := listen(t, s)
	for _, req := range []string{
		"CONNECT " + kelvin + ":443 HTTP/1.1\r\nHost: " + kelvin + ":443\r\n\r\n",
		"GET http://" + kelvin + "/ HTTP/1.1\r\nHost: " + kelvin + "\r\n\r\n",
	} {
		if code, _ := roundTrip(t, addr, req); code != http.StatusBadRequest {
			t.Errorf("%q: %d", req, code)
		}
	}
}

// A CONNECT tunnel's ClientHello, once it starts, must arrive whole within the deadline, in a record
// no longer than TLS's: one that stops, or claims 64 KiB, is refused and closed.
func TestTunnelHelloDeadline(t *testing.T) {
	t.Parallel()
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	must(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(io.Discard, c) }()
		}
	}()
	port := portOf(t, ln.Addr())
	s, addr, ev := transparentServer(t, Config{}, fmt.Sprintf("localhost:%d\n127.0.0.1:%d\n", port, port))
	s.helloWait = 300 * time.Millisecond
	for _, first := range [][]byte{{0x16, 3, 1, 0, 0xff}, {0x16, 3, 1, 0xff, 0xff}} {
		c, err := new(net.Dialer).DialContext(t.Context(), "tcp", addr)
		must(t, err)
		fmt.Fprintf(c, "CONNECT localhost:%d HTTP/1.1\r\nHost: localhost:%d\r\n\r\n", port, port)
		r := bufio.NewReader(c)
		resp, err := http.ReadResponse(r, &http.Request{Method: http.MethodConnect})
		must(t, err)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("CONNECT: %d", resp.StatusCode)
		}
		_, _ = c.Write(first)
		start := time.Now()
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := r.ReadByte(); !errors.Is(err, io.EOF) || time.Since(start) > 3*time.Second {
			t.Errorf("% x: %v after %s", first, err, time.Since(start))
		}
		_ = c.Close()
	}
	if n := len(slices.DeleteFunc(eventsOf(ev), func(e Event) bool { return e.Reason != reasonHelloBroken })); n != 2 {
		t.Errorf("%d refusals: %s", n, ev.String())
	}
}

// A tunnel whose first byte starts a TLS record is TLS from then on: one that sends that byte alone
// and waits past the deadline, then a ClientHello naming another host, is refused, and so is one
// whose record header isn't TLS's version, and the far end gets none of their bytes.
func TestTunnelHelloStall(t *testing.T) {
	t.Parallel()
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	must(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	got := &syncBuffer{}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(got, c) }()
		}
	}()
	port := portOf(t, ln.Addr())
	s, addr, ev := transparentServer(t, Config{}, fmt.Sprintf("localhost:%d\n127.0.0.1:%d\n", port, port))
	s.helloWait = 200 * time.Millisecond
	for _, c := range []struct {
		first, then []byte
		wait        time.Duration
	}{
		{[]byte{0x16}, rawHello("other.test")[1:], 500 * time.Millisecond},
		{[]byte{0x16, 2, 1, 0, 5}, []byte("hello"), 0},
	} {
		conn, err := new(net.Dialer).DialContext(t.Context(), "tcp", addr)
		must(t, err)
		fmt.Fprintf(conn, "CONNECT localhost:%d HTTP/1.1\r\nHost: localhost:%d\r\n\r\n", port, port)
		r := bufio.NewReader(conn)
		resp, err := http.ReadResponse(r, &http.Request{Method: http.MethodConnect})
		must(t, err)
		_ = resp.Body.Close()
		_, _ = conn.Write(c.first)
		time.Sleep(c.wait)
		_, _ = conn.Write(c.then)
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		// Closed either way: Linux resets a connection closed with bytes
		// unread, as the proxy leaves what follows a broken hello.
		if _, err := r.ReadByte(); !errors.Is(err, io.EOF) && !errors.Is(err, syscall.ECONNRESET) {
			t.Errorf("% x: not closed: %v", c.first, err)
		}
		_ = conn.Close()
	}
	time.Sleep(100 * time.Millisecond)
	if got.String() != "" {
		t.Errorf("the far end got %q", got.String())
	}
	if n := len(slices.DeleteFunc(eventsOf(ev), func(e Event) bool { return e.Reason != reasonHelloBroken })); n != 2 {
		t.Errorf("%d refusals: %s", n, ev.String())
	}
}

// An address's tunnel may have no server name, but only in a ClientHello that parsed: one split
// across two records, naming another host in its second, is refused, and the far end gets nothing,
// while a whole one without a name goes on.
func TestTunnelAddressSplitHello(t *testing.T) {
	t.Parallel()
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	must(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	got := &syncBuffer{}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(got, c) }()
		}
	}()
	port := portOf(t, ln.Addr())
	_, addr, ev := transparentServer(t, Config{}, fmt.Sprintf("127.0.0.1:%d\n", port))
	hs := rawHello("other.test")[5:]                                                                             // the handshake message, without its record header
	record := func(b []byte) []byte { return append([]byte{0x16, 3, 1, byte(len(b) >> 8), byte(len(b))}, b...) } //nolint:gosec // G115: a test's few bytes
	tunnelWith := func(first []byte) {
		t.Helper()
		c, err := new(net.Dialer).DialContext(t.Context(), "tcp", addr)
		must(t, err)
		defer func() { _ = c.Close() }()
		fmt.Fprintf(c, "CONNECT 127.0.0.1:%d HTTP/1.1\r\nHost: 127.0.0.1:%d\r\n\r\n", port, port)
		r := bufio.NewReader(c)
		resp, err := http.ReadResponse(r, &http.Request{Method: http.MethodConnect})
		must(t, err)
		_ = resp.Body.Close()
		_, _ = c.Write(first)
		if cw, ok := c.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, _ = io.ReadAll(r)
	}
	tunnelWith(append(record(hs[:10]), record(hs[10:])...))
	time.Sleep(100 * time.Millisecond)
	if got.String() != "" {
		t.Errorf("a split ClientHello reached the far end: % x", got.String())
	}
	e := waitEvent(t, ev, func(e Event) bool { return e.Reason == reasonHelloBroken })
	if !e.Refused || e.Host != "127.0.0.1" {
		t.Errorf("event %+v", e)
	}
	whole := rawHello("")
	tunnelWith(whole)
	for range 50 {
		if got.String() != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got.String() != string(whole) {
		t.Errorf("a whole ClientHello without a name didn't reach the far end: %d bytes", len(got.String()))
	}
}

// A record header that claims more than a TLS record holds is never read past.
func TestPeekClientHelloCap(t *testing.T) {
	t.Parallel()
	r := bufio.NewReader(io.MultiReader(bytes.NewReader([]byte{0x16, 3, 1, 0xff, 0xff}), failReader{t}))
	if rec, sni, isTLS := peekClientHello(t.Context(), r); rec != nil || sni != "" || !isTLS {
		t.Errorf("got %d bytes, %q, %v", len(rec), sni, isTLS)
	}
}

// failReader fails its test when it's read.
type failReader struct{ t *testing.T }

func (f failReader) Read([]byte) (int, error) {
	f.t.Error("read past the record's header")
	return 0, io.EOF
}

func TestTransparentPorts(t *testing.T) {
	t.Parallel()
	many := ""
	for i := range 40 {
		many += fmt.Sprintf("h%d.test:%d\n", i, 9000+i)
	}
	for _, c := range []struct {
		name, rules string
		open        bool
		want        []int
		dropped     int
	}{
		{"none", "", false, []int{80, 443}, 0},
		{"named", "a.test:8443\n*.b.test:8080\n**:9000\n10.0.0.1:5432\nc.test:*\n!d.test:7000\ne.test:22\nf.test:53\ng.test:80\nh.test:8443\n", false, []int{80, 443, 8080, 8443, 9000}, 0},
		{"capped", many, false, append([]int{80, 443}, func() []int {
			var p []int
			for i := range 32 {
				p = append(p, 9000+i)
			}
			return p
		}()...), 8},
		{"broken deny", "a.test:8443\n!not a rule\n", false, []int{80, 443}, 0},
		{"open", "a.test:8443\n", true, []int{80, 443}, 0},
	} {
		p := writePolicy(t, c.rules)
		p.open = c.open
		got, dropped := p.transparentPorts()
		if !slices.Equal(got, c.want) || len(dropped) != c.dropped {
			t.Errorf("%s: %v, %v dropped", c.name, got, dropped)
		}
	}
	s, addr, ev := transparentServer(t, Config{Password: "pw"}, many)
	auth := "Proxy-Authorization: " + authOf(ProxyUser, "pw") + "\r\n"
	for range 2 {
		if code, body := roundTrip(t, addr, "GET http://devvm.proxy/transparent/ports HTTP/1.1\r\nHost: devvm.proxy\r\n"+auth+"\r\n"); code != http.StatusOK || !strings.HasPrefix(body, "80\n443\n9000\n") || strings.Count(body, "\n") != 34 {
			t.Errorf("served: %d %q", code, body)
		}
	}
	if n := strings.Count(ev.String(), "aren't redirected"); n != 1 {
		t.Errorf("the log named the ports left out %d times", n)
	}
	if code, _ := roundTrip(t, addr, "GET http://devvm.proxy/transparent/ports HTTP/1.1\r\nHost: devvm.proxy\r\n\r\n"); code != http.StatusProxyAuthRequired {
		t.Errorf("without the password: %d", code)
	}
	_ = s
}

// routeFor gives CONNECT's order: the links' domain, the policy, interception for a bound host
// unless the CA has ended, then a tunnel.
func TestRouteFor(t *testing.T) {
	t.Parallel()
	s, port, _, _ := interceptSetup(t, func(http.ResponseWriter, *http.Request) {})
	s.egress.pol = writePolicy(t, fmt.Sprintf("localhost:%d\n127.0.0.1:%d\nlocalhost:7\n127.0.0.1:7\n", port, port))
	for _, c := range []struct {
		host string
		port int
		want routeKind
	}{
		{"db.devvm.internal", port, routeNotLink},
		{"blocked.test", port, routeBlocked},
		{"localhost", port, routeIntercept},
		{"localhost", 7, routeTunnel},
	} {
		if rt := s.routeFor(t.Context(), c.host, c.port, Event{}); rt.kind != c.want {
			t.Errorf("%s:%d: %d, want %d (%+v)", c.host, c.port, rt.kind, c.want, rt.e)
		}
	}
	s.ca = testCA(t, time.Now().Add(-48*time.Hour), time.Now().Add(-time.Hour))
	if rt := s.routeFor(t.Context(), "localhost", port, Event{}); rt.kind != routeCAEnded || rt.e.Reason != caEndedReason {
		t.Errorf("the CA ended: %+v", rt)
	}
}

// Events and /blocked say a block came without proxy settings.
func TestBlockedVia(t *testing.T) {
	t.Parallel()
	rs := []numbered{{1, Event{Event: "blocked", Host: "a.test", Port: 443, Reason: "not on the allowlist", Via: ViaDNS}}}
	if got := newBlocks(rs, 1, "0"); !strings.Contains(got, "devvm: blocked a.test:443 (not on the allowlist) (without proxy settings)\n") {
		t.Errorf("newBlocks: %q", got)
	}
	b, _ := json.Marshal(Event{Event: "tunnel", Via: ViaDNS})
	if !strings.Contains(string(b), `"via":"dns"`) {
		t.Errorf("JSON: %s", b)
	}
	if b, _ := json.Marshal(Event{Event: "tunnel"}); strings.Contains(string(b), "via") {
		t.Errorf("JSON without via: %s", b)
	}
}

// Whatever its first bytes, a transparent connection on a proxy whose policy allows nothing reaches
// nothing and doesn't panic or hang.
func FuzzTransparent(f *testing.F) {
	f.Add(rawHello("example.com"))
	f.Add(rawHello("1.2.3.4"))
	f.Add([]byte("GET / HTTP/1.1\r\nHost: example.com\r\n\r\n"))
	f.Add([]byte("GET http://devvm.proxy/blocked HTTP/1.1\r\nHost: devvm.proxy\r\n\r\n"))
	f.Add([]byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"))
	f.Add([]byte("SSH-2.0-OpenSSH_9.9\r\n"))
	f.Add([]byte{0x16, 3, 1, 0xff, 0xff})
	out := &syncBuffer{}
	s, err := newServer(Config{VM: "vm"}, IO{}, quiet(), out)
	if err != nil {
		f.Fatal(err)
	}
	s.helloWait = 20 * time.Millisecond
	f.Fuzz(func(t *testing.T, first []byte) {
		a, b := net.Pipe()
		go func() {
			_, _ = b.Write(first)
			_ = b.Close()
		}()
		done := make(chan struct{})
		go func() { defer close(done); s.transparent.take(t.Context(), bufConn{a, bufio.NewReader(a)}, 443) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("% x: still serving", first)
		}
		for _, e := range eventsOf(out) {
			if e.Event != "blocked" {
				t.Fatalf("% x: %+v", first, e)
			}
		}
	})
}
