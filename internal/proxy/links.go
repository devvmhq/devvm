package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// --- links between VMs ---
//
// The proxy is a second entrance to the VM's links (Config.Links): a request for one of their names and
// ports, CONNECT db:5432 or GET http://db:5432/, goes to that link's socket on the host, which
// devvm _links serves as it serves the link's remote forward, checking each connection there. The
// proxy checks the list devvm _links writes whenever the links it carries change on every request,
// before any rule, and reads it again when it has changed (watched): a host and port on it match exactly, after splitHostPort's one normalization
// (lower case, one trailing dot), and the list names each link's short name and its long one
// (NAME.devvm.internal). Its bytes pass as they are: no TLS check or interception, no secret put
// in, no rule asked, and no event of the proxy's own, as devvm _links logs the connection. A
// request names a host and a port, never a socket: the socket is the one the link sockets' folder
// (Config.LinkSockets, devvm's run folder) has for this VM's link at that address and port
// (LinkSocket), so a list holding anything else is ignored, and no request reaches another VM's
// link or any other socket. Without that folder, no link is reached.

// links is a proxy's way to the VM's links: set once, as it starts, and only read after.
type links struct {
	file    string             // Config.Links: the VM's links list, which devvm _links writes; empty: none
	list    *watched[LinkList] // file's list; a list that can't be read has none, logged once
	sockets string             // Config.LinkSockets: the folder of the links' sockets; empty: none
	vm      string             // Config.VM, whose links' sockets they are
	log     *log.Logger
	events  *events
}

func newLinks(file, sockets, vm string, l *log.Logger, ev *events) links {
	return links{
		file: file, sockets: sockets, vm: vm, log: l, events: ev,
		list: &watched[LinkList]{
			path: file, read: readLinkListFile,
			parse:  func(b []byte) (LinkList, error) { return parseLinkList(file, b) },
			onFail: func(err error) { l.Printf("links: %v", err) },
		},
	}
}

// LinkList is the file devvm _links writes for the VM's proxy: the links it carries now.
type LinkList struct {
	Version int         `json:"version"` // LinkListVersion
	Links   []LinkEntry `json:"links"`
}

// LinkListVersion is the list's version: a list of another isn't read.
const LinkListVersion = 1

// LinkEntry is one link the VM's proxy may reach: its names, its address and port in the VM, its
// target (for the log) and its socket on the host (LinkSocket).
type LinkEntry struct {
	Hosts  []string `json:"hosts"`
	Addr   string   `json:"addr"`
	Port   int      `json:"port"`
	Target string   `json:"target"`
	Socket string   `json:"socket"`
}

// linkListMax bounds the list the proxy reads.
const linkListMax = 1 << 20

// LinkSocket is the socket on the host of VM vm's link at addr and port in it, in dir, devvm's run
// folder: named by a hash of the three, as a socket's path has fewer than 104 characters.
func LinkSocket(dir, vm string, addr netip.Addr, port int) string {
	return filepath.Join(dir, "link-"+LinkHash(vm, "socket", addr.String(), strconv.Itoa(port))+".sock")
}

// LinkHash names one of a link session's files: the first 16 hex digits of the SHA-256 of the VM's
// name and parts, each after a NUL.
func LinkHash(vm string, parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(append([]string{vm}, parts...), "\x00")))
	return hex.EncodeToString(sum[:])[:16]
}

// readLinkListFile reads the list at path: a regular file, not a link, that only its owner, this
// process's user, may write, at most linkListMax long (readPrivate).
func readLinkListFile(path string) ([]byte, error) { return readPrivate(path, linkListMax, 0o022) }

// parseLinkList is the list b, read from path, of LinkListVersion.
func parseLinkList(path string, b []byte) (LinkList, error) {
	var l LinkList
	if err := json.Unmarshal(b, &l); err != nil {
		return LinkList{}, fmt.Errorf("%s: %w", path, err)
	}
	if l.Version != LinkListVersion {
		return LinkList{}, fmt.Errorf("%s: version %d", path, l.Version)
	}
	return l, nil
}

// find returns the VM's link that host and port name, from the list as it is now: its entry,
// with the socket it must have. A host and port on no entry, an entry whose socket isn't this VM's
// link's, and a list that can't be read give none, so the request is the policy's.
func (k links) find(host string, port int) (LinkEntry, bool) {
	if k.file == "" || k.sockets == "" {
		return LinkEntry{}, false
	}
	l := k.list.get()
	for _, e := range l.Links {
		if e.Port != port || !slices.Contains(e.Hosts, host) {
			continue
		}
		a, err := netip.ParseAddr(e.Addr)
		if err != nil || a.String() != e.Addr || !IsLinkAddr(a) || a != a.Unmap() || port < 1 || port > 65535 ||
			e.Socket != LinkSocket(k.sockets, k.vm, a, port) {
			k.log.Printf("links: %s:%d: not this VM's link's socket", host, port)
			continue
		}
		return e, true
	}
	return LinkEntry{}, false
}

// linkNetA and linkNetB are the first two bytes of 127.77.0.0/16, where every link's address in
// its client is.
const linkNetA, linkNetB = 127, 77

// IsLinkAddr reports whether a is in 127.77.0.0/16, the links' addresses in a client, as itself
// or mapped into IPv6.
func IsLinkAddr(a netip.Addr) bool {
	a = a.Unmap()
	b := a.As16()
	return a.Is4() && b[12] == linkNetA && b[13] == linkNetB
}

// LocalProxyAddr and LocalProxyPort are where a VM's local proxy listens in the VM, on its loopback:
// devvm-local-proxy, which the guest setup writes, adds the VM's password to each request and sends
// it here, for programs that ask for a proxy's password instead of taking it from HTTP_PROXY, such
// as the desktop's browsers. The address is devvm's alone, so no port forwarder puts the local
// proxy on the host (IsLocalProxyAddr).
const (
	LocalProxyAddr = "127.0.77.1"
	LocalProxyPort = 3128
)

// IsLocalProxyAddr reports whether a is LocalProxyAddr, as itself or mapped into IPv6.
func IsLocalProxyAddr(a netip.Addr) bool {
	return a.Unmap() == netip.MustParseAddr(LocalProxyAddr)
}

// LinkAddr is the address i in 127.77.0.0/16.
func LinkAddr(i uint16) netip.Addr {
	b := [4]byte{linkNetA, linkNetB}
	binary.BigEndian.PutUint16(b[2:], i)
	return netip.AddrFrom4(b)
}

// LinkDomain is the domain of the links' long names: a client reaches each of its links' targets
// as NAME.devvm.internal besides NAME. .internal is for private use, so it names no host on the
// internet, and a host under LinkDomain is a link's or nothing.
const LinkDomain = "devvm.internal"

// refuse refuses a host under LinkDomain that's none of the VM's links: such a name is never
// the policy's, so no rule or company proxy sends it anywhere. It reports whether it refused.
func (k links) refuse(w http.ResponseWriter, method, host string, port int) bool {
	if !strings.HasSuffix(host, "."+LinkDomain) && host != LinkDomain {
		return false
	}
	k.events.record(Event{Event: "blocked", Host: host, Port: port, Method: method, Reason: "not one of this VM's links", Refused: true})
	http.Error(w, "devvm proxy: "+net.JoinHostPort(host, strconv.Itoa(port))+" isn't one of this VM's links", http.StatusForbidden)
	return true
}

// linkDialTimeout bounds how long the proxy waits for a link's socket.
const linkDialTimeout = 10 * time.Second

// dialLink connects to a link's socket. A socket whose backlog is full (a burst of connections
// while devvm _links starts their relays) refuses a connection, or asks for another try, so a few
// more are made, for under a second, as a socket nothing listens on refuses too.
func dialLink(ctx context.Context, sock string) (net.Conn, error) {
	d := net.Dialer{Timeout: linkDialTimeout}
	wait := 10 * time.Millisecond
	for i := 0; ; i++ {
		c, err := d.DialContext(ctx, "unix", sock)
		if err == nil || i == 6 || !errors.Is(err, syscall.ECONNREFUSED) && !errors.Is(err, syscall.EAGAIN) {
			return c, err
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(wait):
		}
		wait *= 2
	}
}

// gone answers a request for a link whose socket doesn't answer (its link removed since the
// list was read, or devvm _links gone) and logs it as devvm _links logs a refusal.
func (k links) gone(w http.ResponseWriter, host string, port int, e LinkEntry, err error) {
	k.log.Printf("links: %s:%d: %v", host, port, err)
	k.events.record(Event{Event: "link_refused", Host: host, Port: port, Target: e.Target, Reason: linkNotCarried})
	http.Error(w, "devvm proxy: the link "+net.JoinHostPort(host, strconv.Itoa(port))+" isn't carried now", http.StatusBadGateway)
}

// linkNotCarried is the reason of a refusal the proxy logs: the link's socket didn't answer.
const linkNotCarried = "not carried"

// connect tunnels a CONNECT for a link to its socket, the bytes as they are.
func (k links) connect(w http.ResponseWriter, r *http.Request, host string, port int, e LinkEntry) {
	up, err := dialLink(r.Context(), e.Socket)
	if err != nil {
		k.gone(w, host, port, e, err)
		return
	}
	defer func() { _ = up.Close() }()
	client, err := hijack(w)
	if err != nil {
		return
	}
	splice(client, up, nil)
}

// linkDialError: a link's socket didn't answer.
type linkDialError struct{ err error }

func (e linkDialError) Error() string { return e.err.Error() }

// forward forwards an absolute-form request for a link (plain HTTP) to its socket, a new
// connection for each request, so devvm _links checks each one, and none of the proxy's headers.
func (k links) forward(w http.ResponseWriter, r *http.Request, host string, port int, e LinkEntry) {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			c, err := dialLink(ctx, e.Socket)
			if err != nil {
				return nil, linkDialError{err}
			}
			return c, nil
		},
		DisableKeepAlives: true, DisableCompression: true,
	}
	defer tr.CloseIdleConnections()
	rp := &httputil.ReverseProxy{
		Transport:     tr,
		ErrorLog:      k.log,
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = "http", r.URL.Host
			pr.Out.Header.Del("Proxy-Authorization")
			pr.Out.Header.Del("Proxy-Connection")
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			if de := (linkDialError{}); errors.As(err, &de) {
				k.gone(w, host, port, e, de.err)
				return
			}
			k.log.Printf("links: %s:%d: %v", host, port, err)
			http.Error(w, "devvm proxy: the link "+net.JoinHostPort(host, strconv.Itoa(port))+" didn't answer", http.StatusBadGateway)
		},
	}
	// The client's own context: a request it gave up on lets its link connection go, so a target
	// that never answers can't hold the client's links' connections.
	rp.ServeHTTP(w, r)
}
