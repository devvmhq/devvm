package proxy

// The transparent path: tools that skip the proxy settings. On a confined VM, the guest's
// devvm-transparent answers every name with SentinelAddr, and sends a connection to one of its
// ports, as an ordinary CONNECT with the VM's password, to the reserved target, TransparentHost, at
// that port. The connection's first bytes name its host: a TLS ClientHello's server name (SNI), or
// each HTTP request's Host. From there it's judged as CONNECT host:port is (routeFor), its events
// marked ViaDNS. The VM's own bytes never choose the port: the target's is the one judged and
// dialed.

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TransparentHost is the CONNECT target a VM's devvm-transparent sends for a tool that skipped the
// proxy: the host is in the connection's first bytes, the port is the target's. It's under
// ProxyDomain, which no rule, link or resolver ever sees.
const TransparentHost = "transparent." + ProxyDomain

// ProxyDomain is the proxy's own domain: devvm.proxy itself is its endpoints' (serveReserved), in
// absolute form; TransparentHost is the reserved target; any other name under it, and devvm.proxy in
// a CONNECT, which has no endpoint, is refused before the links and the policy (reservedRefusal).
// .proxy isn't a top-level domain, so it names no host on the internet.
const ProxyDomain = "devvm.proxy"

// SentinelAddr is the one address a VM's devvm-transparent gives every name, in 198.18.0.0/15
// (benchmarking), which clients count as public and the floor counts as internal (notPublic).
const SentinelAddr = "198.18.0.1"

// ViaDNS is Event.Via for a tool that skipped the proxy settings, which the VM's names sent here.
const ViaDNS = "dns"

// IsSentinelAddr reports whether a is SentinelAddr, as itself or mapped into IPv6. No port
// forwarder puts a VM's listener on it on the host.
func IsSentinelAddr(a netip.Addr) bool { return a.Unmap() == netip.MustParseAddr(SentinelAddr) }

// ownDomain reports whether host is ProxyDomain or under it.
func ownDomain(host string) bool {
	return host == ProxyDomain || strings.HasSuffix(host, "."+ProxyDomain)
}

// The reasons of refusals the reserved names and the transparent path give.
const (
	reasonOwnDomain = ProxyDomain + " is devvm's own"
	reasonSentinel  = SentinelAddr + " is devvm's stand-in address for names: the tool resolved the name itself; give it the name"
	reasonNoBytes   = "sent nothing in 10 s; devvm needs a TLS server name or an HTTP Host to know where it goes"
	reasonNotNamed  = "neither TLS with a server name nor HTTP; devvm can't tell where it goes"
	reasonNoSNI     = "a TLS connection without a server name (SNI); devvm can't tell where it goes"
	reasonSNIAddr   = "the TLS server name is an address"
	reasonViaLink   = "one of this VM's links, or under their domain: its name in /etc/hosts reaches it"
)

// reservedRefusal is why a connection to host is refused before anything else judges it, or "": a
// name under ProxyDomain (the endpoints' devvm.proxy is answered before this, in absolute form), or
// SentinelAddr, which only a tool that resolved a name itself would send.
func reservedRefusal(host string) string {
	if ownDomain(host) {
		return reasonOwnDomain
	}
	if a, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil && IsSentinelAddr(a) {
		return reasonSentinel
	}
	return ""
}

// viaKey is the context key of how a request reached the proxy (withVia).
type viaKey struct{}

// withVia is ctx, its requests marked as reaching the proxy via (Event.Via).
func withVia(ctx context.Context, via string) context.Context {
	return context.WithValue(ctx, viaKey{}, via)
}

// viaOf is how ctx's request reached the proxy (withVia), "" for a proxy request of its own.
func viaOf(ctx context.Context) string {
	v, _ := ctx.Value(viaKey{}).(string)
	return v
}

// transparentPortsMax is how many ports besides 80 and 443 a VM's transparent mode redirects.
const transparentPortsMax = 32

// transparent serves the reserved target's connections, for its server: the host from their first
// bytes, then routeFor, as for CONNECT host:port. It keeps which ports past transparentPortsMax its
// log last named.
type transparent struct {
	s       *server
	mu      sync.Mutex
	dropped string
}

// serve takes a connection to the reserved target at port: it answers 200, then serves what follows
// (take).
func (t *transparent) serve(ctx context.Context, w http.ResponseWriter, port int) {
	c, err := hijack(w)
	if err != nil {
		return
	}
	t.take(ctx, c, port)
}

// take serves c, a connection to the reserved target at port, and closes it: it reads the client's
// first bytes for at most helloWait. A TLS record goes the TLS way, an HTTP/1.x request line or
// HTTP/2's preface the HTTP way, and anything else, or nothing, is refused.
func (t *transparent) take(ctx context.Context, c bufConn, port int) {
	defer func() { _ = c.Close() }()
	_ = c.SetReadDeadline(time.Now().Add(t.s.helloWait))
	first, err := c.r.Peek(1)
	switch {
	case err != nil:
		t.refuse(TransparentHost, port, reasonNoBytes)
	case first[0] == 0x16:
		t.tls(ctx, c, port)
	case looksHTTP(c.r):
		_ = c.SetReadDeadline(time.Time{}) // the HTTP server keeps its own
		serveOne(c, t.handler(port), connServing{header: 30 * time.Second, idle: 10 * time.Minute, maxHeader: maxHeaderBytes}, t.s.log)
	default:
		t.refuse(TransparentHost, port, reasonNotNamed)
	}
}

// refuse records a refused block of a transparent connection to host at port, for why: host is
// TransparentHost while no host is known, so nothing suggests allowing it.
func (t *transparent) refuse(host string, port int, why string) {
	t.s.events.record(Event{Event: "blocked", Host: host, Port: port, Method: "CONNECT", Via: ViaDNS, Reason: why, Refused: true})
}

// looksHTTP reports whether r's first bytes start an HTTP request: one of HTTP/1.x's methods, or
// PRI, HTTP/2's preface, each with its space. The shortest request line (GET / HTTP/1.0) is longer
// than the eight bytes it looks at, which r's buffer holds, so a real client's are there at once;
// the HTTP server refuses the rest of what isn't HTTP.
func looksHTTP(r *bufio.Reader) bool {
	methods := []string{"GET ", "HEAD ", "POST ", "PUT ", "DELETE ", "OPTIONS ", "PATCH ", "TRACE ", "CONNECT ", "PRI "}
	b, _ := r.Peek(8)
	return slices.ContainsFunc(methods, func(m string) bool { return bytes.HasPrefix(b, []byte(m)) })
}

// tls takes a connection whose first bytes are a TLS record: its ClientHello's server name is the
// host (sniHost), then routeFor decides, and the ClientHello goes on first to whatever serves it
// (replay): the far end (carry), interception (intercept), or the 403 page (refusePage).
func (t *transparent) tls(ctx context.Context, c bufConn, port int) {
	s := t.s
	rec, sni, _ := peekClientHello(c.r)
	if rec == nil {
		t.refuse(TransparentHost, port, reasonHelloBroken)
		return
	}
	host, why := sniHost(sni)
	if why != "" {
		t.refuse(TransparentHost, port, why)
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	c = replay(c, rec)
	rt := s.routeFor(ctx, host, port, Event{Host: host, Port: port, Method: "CONNECT", Via: ViaDNS})
	switch rt.kind {
	case routeTunnel:
		up, err := s.egress.dialOut(ctx, host, port)
		if err != nil {
			s.log.Printf("%s:%d: connect failed: %v", host, port, err)
			rt.e.Reason = reasonConnectFailed
			t.block(ctx, c, rt.e)
			return
		}
		defer func() { _ = up.Close() }()
		s.carry(c, up, host, rt.e)
	case routeIntercept:
		s.intercept(ctx, c, host, port, rt.e)
	case routeBlocked:
		t.block(ctx, c, rt.e)
	case routeCAEnded:
		s.events.record(rt.e)
	case routeLink, routeNotLink:
		t.refuse(host, port, reasonViaLink)
	}
}

// replay is c with rec, the bytes already read from it, read again first.
func replay(c bufConn, rec []byte) bufConn {
	return bufConn{c.Conn, bufio.NewReader(io.MultiReader(bytes.NewReader(rec), c.r))}
}

// sniHost is the host a transparent TLS connection names, from its ClientHello's server name, or
// why it names none: no name, anything but printable ASCII (refused before its case is folded,
// which would turn the Kelvin sign into k), an address, which RFC 6066 forbids, a name HostOK
// refuses, or one under ProxyDomain. crypto/tls has already refused a name with a trailing dot, or
// two names, so the ClientHello gave none.
func sniHost(sni string) (string, string) {
	switch {
	case sni == "":
		return "", reasonNoSNI
	case !hostASCII(sni):
		return "", "the TLS server name " + quoteCapped(sni) + " isn't ASCII"
	}
	h := strings.ToLower(sni)
	if _, err := netip.ParseAddr(strings.Trim(h, "[]")); err == nil {
		return "", reasonSNIAddr
	}
	if !HostOK(h) {
		return "", "the TLS server name " + quoteCapped(sni) + " isn't a host name"
	}
	if ownDomain(h) {
		return "", reasonOwnDomain
	}
	return h, ""
}

// quoteCapped is s quoted as Go writes it (strconv.Quote), cut to its first 255 bytes, for a reason
// that shows what the VM sent.
func quoteCapped(s string) string {
	q := strconv.Quote(s)
	if len(q) > 255 {
		q = q[:255]
	}
	return q
}

// block records e, a transparent TLS connection's block, and serves c the 403 page (refusePage).
func (t *transparent) block(ctx context.Context, c net.Conn, e Event) {
	e = t.s.blocked(e)
	t.s.events.record(e)
	t.refusePage(ctx, c, e)
}

// refusePage serves the 403 page of e, a block, over TLS on c, for every request: a certificate for
// e.Host from the VM's CA (handshake), which the VM trusts, then the text CONNECT's 403 has
// (blockPage), so the tool shows the user what curl through the proxy would. Without a CA, or once
// it has ended, no certificate can be made: c closes.
func (t *transparent) refusePage(ctx context.Context, c net.Conn, e Event) {
	s := t.s
	if s.ca == nil || s.ca.ended(time.Now()) {
		return
	}
	tconn, err := handshake(ctx, c, s.ca, e.Host)
	if err != nil {
		return
	}
	serveOne(tconn, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { blockPage(w, e) }),
		connServing{header: 10 * time.Second, idle: 30 * time.Second}, s.log)
}

// handler serves each HTTP request of a transparent connection to port, judged by its own Host as
// an absolute-form request for http://Host is (handleHTTP), its events marked ViaDNS: never a
// CONNECT of its own, never a Host naming another port, never one of the VM's links, which their
// names in /etc/hosts reach, never devvm.proxy's endpoints but /blocked, and never HTTPS, whatever
// its request line says, so no secret goes in.
func (t *transparent) handler(port int) http.Handler {
	s := t.s
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			http.Error(w, "devvm proxy: not a proxy here", http.StatusBadRequest)
			return
		}
		if r.Host == "" {
			http.Error(w, "devvm proxy: no Host", http.StatusBadRequest)
			return
		}
		host, p := splitHostPort(r.Host, port)
		if p != port {
			http.Error(w, fmt.Sprintf("devvm proxy: Host names port %d, this connection is to port %d", p, port), http.StatusBadRequest)
			return
		}
		if !HostOK(host) {
			http.Error(w, "devvm proxy: not a host name", http.StatusBadRequest)
			return
		}
		// This way in needs no password of its own (socat adds it), so a container that has none
		// reaches devvm.proxy's /blocked alone, never the VM's CA, its ports or the Nix cache.
		if host == ProxyDomain && r.URL.Path != "/blocked" {
			http.Error(w, "devvm proxy: devvm.proxy's endpoints but /blocked need the proxy settings", http.StatusForbidden)
			return
		}
		if _, ok := s.links.find(host, port); ok || isLinkDomain(host) {
			s.block(w, Event{Host: host, Port: port, Method: r.Method, Path: r.URL.Path, Agent: r.UserAgent(), Via: ViaDNS, Reason: reasonViaLink, Refused: true})
			return
		}
		target := net.JoinHostPort(host, strconv.Itoa(port))
		if port == 80 {
			target = strings.TrimSuffix(target, ":80") // [v6] keeps its brackets
		}
		r.URL.Scheme, r.URL.Host = "http", target
		s.handleHTTP(w, r.WithContext(withVia(r.Context(), ViaDNS)))
	})
}

// ports are the ports a VM's transparent mode redirects (policy.transparentPorts), for
// /transparent/ports. The proxy's own log names those past transparentPortsMax, once each time they
// change.
func (t *transparent) ports() []int {
	ports, dropped := t.s.egress.pol.transparentPorts()
	d := fmt.Sprint(dropped)
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(dropped) > 0 && d != t.dropped {
		t.s.log.Printf("transparent: the rules name more than %d ports besides 80 and 443; these aren't redirected: %v", transparentPortsMax, dropped)
	}
	t.dropped = d
	return ports
}
