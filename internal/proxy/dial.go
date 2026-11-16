package proxy

// The child's connections, through the broker: the child's sandbox refuses it every connection of
// its own (Seatbelt allows it no network out, and on Linux the seccomp filter refuses it every
// socket but a netlink route socket, and connect), so it asks the broker to look up each name the
// floor judges (lookup) and to open each connection (dial): to an address the child's floor
// kept, which the broker judges again with its own read of the VM's policy and the host's
// addresses, or to one of the company proxies of its own config. The broker hands each connection
// to the child as the frame's descriptor, as it hands a link's. So a child that a VM took over
// reaches this machine's loopback, its own addresses and the LAN only where a rule names the
// address, and a rule still applies from the next connection.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/netip"
	"os"
	"time"
)

// lookupWait bounds the broker's lookup of a name for the child: past the resolver's own
// timeouts, which end it first.
const lookupWait = 20 * time.Second

// dialWait bounds a connection the broker opens for the child, as the proxy's own connections
// are bounded.
const dialWait = 15 * time.Second

// lookupsMax is how many lookups the child has under way at once, and dialsMax how many
// connections: as many as its tunnels at once (maxConnects).
const (
	lookupsMax = 256
	dialsMax   = maxConnects
)

// The company proxies the child asks the broker to connect to (wireDialReq.Proxy): HTTPS_PROXY's,
// for tunnels and the connections that put a value in, and HTTP_PROXY's, for plain HTTP.
const (
	viaHTTPS = "https"
	viaHTTP  = "http"
)

// errRefused is a lookup or a connection the broker refuses: a lookup of what isn't a host name,
// or a connection to an address its own floor doesn't allow, or to what isn't an address.
var errRefused = errors.New("refused by the broker")

// newBrokerNet is the broker's own egress, for the child's lookups and connections: the VM's
// built-in policies (own, devvm's own files) and its policy files, which it looks at on every
// connection, the host's addresses, the company proxies of ups, and this machine's resolver and
// a dialer, which tests replace.
func newBrokerNet(c Config, own fs.FS, ups upstreams, l *log.Logger) (*egress, error) {
	rs, err := readBuiltins(own, c.BuiltinPolicies, l)
	if err != nil {
		return nil, err
	}
	pol := newPolicy(rs, c.Policies, c.DenyPolicies, l)
	pol.open = c.Open
	return &egress{
		pol: pol, own: &hostAddrs{}, up: ups, log: l, lookup: net.DefaultResolver.LookupNetIP,
		connect: (&net.Dialer{Timeout: dialWait}).DialContext,
	}, nil
}

// lookupable reports whether name, a lookup's, is a host name in the proxy's own form with its
// one trailing dot, as the child's floor looks one up (lookupName): no address, no other form.
func lookupable(name string) bool {
	host := NormHost(name).String()
	if name != host+"." || !HostOK(host) {
		return false
	}
	_, isAddr := hostAddr(host)
	return !isAddr
}

// lookup is the addresses of name for the child, through this machine's resolver, as the floor
// looks a name up (lookupName): name is a host name with its trailing dot (lookupable).
func (b *broker) lookup(ctx context.Context, name string) ([]netip.Addr, error) {
	if !lookupable(name) {
		return nil, fmt.Errorf("%w: a lookup of what isn't a host name", errRefused)
	}
	ctx, cancel := context.WithTimeout(ctx, lookupWait)
	defer cancel()
	return b.net.lookup(ctx, "ip", name)
}

// dial is a TCP connection to ap for the child, once the broker's own floor allows it
// (egress.addrOK): no deny names it, and it's neither internal nor this machine's own, or a rule
// names it, or the VM is open. An address with a zone, which no rule names, or port 0, is
// refused.
func (b *broker) dial(ctx context.Context, ap netip.AddrPort) (net.Conn, error) {
	a, port := ap.Addr().Unmap(), int(ap.Port())
	if !a.IsValid() || a.Zone() != "" || port == 0 {
		return nil, fmt.Errorf("%w: a connection to %s, which isn't an address and a port", errRefused, ap)
	}
	if !b.net.addrOK(a, port) {
		return nil, fmt.Errorf("%w: a connection to %s, which no rule names", errRefused, netip.AddrPortFrom(a, ap.Port()))
	}
	ctx, cancel := context.WithTimeout(ctx, dialWait)
	defer cancel()
	return b.net.connect(ctx, "tcp", netip.AddrPortFrom(a, ap.Port()).String())
}

// dialProxy is a TCP connection to the company proxy which names (viaHTTPS or viaHTTP), at the
// address of the broker's own config.
func (b *broker) dialProxy(ctx context.Context, which string) (net.Conn, error) {
	var up *upstream
	switch which {
	case viaHTTPS:
		up = b.net.up.https
	case viaHTTP:
		up = b.net.up.http
	default:
		return nil, fmt.Errorf("%w: a company proxy %q", errRefused, which)
	}
	if up == nil {
		return nil, fmt.Errorf("no %s company proxy: %w", which, errNotFound)
	}
	ctx, cancel := context.WithTimeout(ctx, dialWait)
	defer cancel()
	return up.dialUpstream(ctx)
}

// dialFile is the connection a dial request asks for, as a file to pass the child: an address
// and a port, or a company proxy, never both.
func (b *broker) dialFile(ctx context.Context, r wireDialReq) (*os.File, error) {
	var c net.Conn
	var err error
	switch {
	case r.Addr != "" && r.Proxy == "":
		ap, perr := netip.ParseAddrPort(r.Addr)
		if perr != nil {
			return nil, fmt.Errorf("%w: a connection to what isn't an address and a port", errRefused)
		}
		c, err = b.dial(ctx, ap)
	case r.Proxy != "" && r.Addr == "":
		c, err = b.dialProxy(ctx, r.Proxy)
	default:
		return nil, fmt.Errorf("%w: a connection to neither an address nor a company proxy, or both", errRefused)
	}
	if err != nil {
		return nil, err
	}
	return connFile(c)
}

// connFile is c, a socket's connection, as a file to pass the child; c is closed either way.
func connFile(c net.Conn) (*os.File, error) {
	defer func() { _ = c.Close() }()
	f, ok := c.(interface{ File() (*os.File, error) })
	if !ok {
		return nil, errors.New("a connection that isn't a socket")
	}
	return f.File()
}

// --- the child's side ---

// lookupError is err, the broker's answer to a lookup of name, as the resolver's error that the
// floor reads (lookupName's second try, unresolvedReason): a code of a lookup's failure is a
// net.DNSError, anything else is as it is.
func lookupError(name string, err error) error {
	var ce codeError
	if !errors.As(err, &ce) {
		return err
	}
	e := &net.DNSError{Name: name}
	switch ce.code {
	case codeNoSuchHost:
		e.Err, e.IsNotFound = "no such host", true
	case codeTimeout:
		e.Err, e.IsTimeout, e.IsTemporary = "i/o timeout", true, true
	case codeTemporary:
		e.Err, e.IsTemporary = "a temporary failure in the broker's lookup", true
	default:
		return err
	}
	return e
}

// dnsCode is the code of err, a lookup's error, for the child: "" for one that isn't a
// resolver's answer.
func dnsCode(err error) string {
	var e *net.DNSError
	switch {
	case !errors.As(err, &e):
		return ""
	case e.IsNotFound:
		return codeNoSuchHost
	case e.IsTimeout:
		return codeTimeout
	case e.IsTemporary:
		return codeTemporary
	}
	return ""
}

// lookup is the addresses of name, which the broker looked up, waited for while ctx lasts.
func (c *brokerClient) lookup(ctx context.Context, name string) ([]netip.Addr, error) {
	r, _, err := c.call(frameLookup, toBroker{Lookup: &wireLookupReq{Name: name}}, ctx.Done())
	if err != nil {
		return nil, lookupError(name, err)
	}
	return r.Lookup.Addrs, nil
}

// dial is a connection to ap, which the broker opened once its own floor allowed it: once ctx has
// ended, the child no longer wants it, and closes it as it comes.
func (c *brokerClient) dial(ctx context.Context, ap netip.AddrPort) (net.Conn, error) {
	return c.dialOf(ctx, wireDialReq{Addr: ap.String()})
}

// dialProxy is a connection to the company proxy which names, which the broker opened.
func (c *brokerClient) dialProxy(ctx context.Context, which string) (net.Conn, error) {
	return c.dialOf(ctx, wireDialReq{Proxy: which})
}

func (c *brokerClient) dialOf(ctx context.Context, r wireDialReq) (net.Conn, error) {
	_, fd, err := c.call(frameDial, toBroker{Dial: &r}, ctx.Done())
	if err != nil {
		return nil, err
	}
	return connOf(ctx, fd)
}

// connOf is the connection fd carries, a reply's descriptor: none, or one the child no longer
// wants once ctx has ended, is an error.
func connOf(ctx context.Context, fd *os.File) (net.Conn, error) {
	if fd == nil {
		return nil, errors.New("the broker's reply carried no connection")
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

// throughHost makes e, a child's egress, look up its names and open its connections through host,
// and its company proxies' connections too.
func throughHost(e *egress, host hostSide) {
	e.lookup = func(ctx context.Context, _, name string) ([]netip.Addr, error) { return host.lookup(ctx, name) }
	e.connect = func(ctx context.Context, _, address string) (net.Conn, error) {
		ap, err := netip.ParseAddrPort(address)
		if err != nil {
			return nil, err
		}
		return host.dial(ctx, ap)
	}
	if e.up.https != nil {
		e.up.https.dial = func(ctx context.Context) (net.Conn, error) { return host.dialProxy(ctx, viaHTTPS) }
	}
	if e.up.http != nil {
		e.up.http.dial = func(ctx context.Context) (net.Conn, error) { return host.dialProxy(ctx, viaHTTP) }
	}
}
