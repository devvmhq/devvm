package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const linkVM = "devvm-app-0123456789abcd"

// linkTarget is a link's socket as devvm _links serves it, in a test: each connection recorded,
// whole, then answered as HTTP when it starts with a request, else echoed.
type linkTarget struct {
	sock  string
	mu    sync.Mutex
	conns []string // what each connection sent, in order
}

func (lt *linkTarget) seen() []string {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	return append([]string(nil), lt.conns...)
}

// serveLink listens on the socket of linkVM's link at addr and port in dir.
func serveLink(t *testing.T, dir, addr string, port int) *linkTarget {
	t.Helper()
	lt := &linkTarget{sock: LinkSocket(dir, linkVM, netip.MustParseAddr(addr), port)}
	ln, err := new(net.ListenConfig).Listen(t.Context(), "unix", lt.sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go lt.answer(c)
		}
	}()
	return lt
}

func (lt *linkTarget) answer(c net.Conn) {
	defer func() { _ = c.Close() }()
	r := bufio.NewReader(c)
	if b, err := r.Peek(4); err == nil && string(b) == "GET " {
		req, err := http.ReadRequest(r)
		if err != nil {
			return
		}
		var dump bytes.Buffer
		_ = req.Write(&dump)
		lt.record(dump.String())
		body := "linked " + req.URL.Path
		fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
		return
	}
	var got bytes.Buffer
	buf := make([]byte, 4096)
	for {
		_ = c.SetReadDeadline(time.Now().Add(time.Second))
		n, err := r.Read(buf)
		got.Write(buf[:n])
		_, _ = c.Write(buf[:n])
		if err != nil {
			break
		}
	}
	lt.record(got.String())
}

func (lt *linkTarget) record(s string) {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	lt.conns = append(lt.conns, s)
}

// writeLinkList writes the list as devvm _links would, mode 600.
func writeLinkList(t *testing.T, path string, entries ...LinkEntry) {
	t.Helper()
	b, err := json.Marshal(LinkList{Version: LinkListVersion, Links: entries})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func linkEntry(dir, addr string, port int, hosts ...string) LinkEntry {
	return LinkEntry{
		Hosts: hosts, Addr: addr, Port: port, Target: "devvm-db-0123456789abcd",
		Socket: LinkSocket(dir, linkVM, netip.MustParseAddr(addr), port),
	}
}

// linkProxy is a proxy for linkVM with the list in a short folder (sockets' paths have fewer than
// 104 characters), a policy that allows nothing, and its events in a buffer, and the address it
// listens on.
func linkProxy(t *testing.T) (s *server, dir string, events *syncBuffer, addr string) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "pl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	// The list is in the VM's own run folder; the sockets are in devvm's.
	list := filepath.Join(dir, "vm", "run", "links.json")
	must(t, os.MkdirAll(filepath.Dir(list), 0o700))
	s = testServer(t, Config{VM: linkVM, Links: list, LinkSockets: dir}, "")
	events = &syncBuffer{}
	s.events.out = events
	return s, dir, events, listen(t, s)
}

// tunnel sends a CONNECT for hostPort, then data, and returns the status and what came back.
func tunnel(t *testing.T, addr, hostPort string, data []byte) (int, string) {
	t.Helper()
	c, err := new(net.Dialer).DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", hostPort, hostPort)
	r := bufio.NewReader(c)
	resp, err := http.ReadResponse(r, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	_, _ = c.Write(data)
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
	b, _ := io.ReadAll(r)
	return resp.StatusCode, string(b)
}

// A CONNECT for one of the VM's links, by either of its names, in any case and with a trailing
// dot, goes to its socket, the bytes as they are, before any rule (the policy allows nothing);
// anything else is the policy's, and blocked. The proxy logs nothing of a link it carried: its
// socket's server, devvm _links, does.
func TestLinkConnect(t *testing.T) {
	t.Parallel()
	s, dir, events, addr := linkProxy(t)
	db := serveLink(t, dir, "127.77.0.1", 5432)
	writeLinkList(t, s.links.file, linkEntry(dir, "127.77.0.1", 5432, "db", "db.devvm.internal"))
	for _, hp := range []string{"db:5432", "DB:5432", "db.:5432", "db.devvm.internal:5432", "Db.Devvm.Internal.:5432"} {
		if code, got := tunnel(t, addr, hp, []byte("ping "+hp)); code != http.StatusOK || got != "ping "+hp {
			t.Errorf("%s: %d %q", hp, code, got)
		}
	}
	if n := len(db.seen()); n != 5 {
		t.Errorf("the link's socket saw %d connections, want 5", n)
	}
	if events.String() != "" {
		t.Errorf("the proxy logged %q", events.String())
	}
	for _, hp := range []string{
		"db:5433", "db..:5432", "db.devvm.internal.devvm.internal:5432", "devvm.internal:5432", "other:5432",
		"db.devvm:5432", "127.77.0.1:5432", "db",
	} {
		if code, _ := tunnel(t, addr, hp, nil); code != http.StatusForbidden && code != http.StatusBadRequest {
			t.Errorf("%s: %d, want it blocked", hp, code)
		}
	}
	if n := len(db.seen()); n != 5 {
		t.Errorf("the link's socket saw %d connections after the others, want 5", n)
	}
}

// A tunnel to a link is never looked into: a TLS ClientHello naming another host passes as it is,
// and a secret bound to the link's name (or to every host) isn't put in, its port intercepted or
// not.
func TestLinkConnectUntouched(t *testing.T) {
	t.Parallel()
	s, dir, _, addr := linkProxy(t)
	db := serveLink(t, dir, "127.77.0.1", 443)
	writeLinkList(t, s.links.file, linkEntry(dir, "127.77.0.1", 443, "db", "db.devvm.internal"))
	ca := t.TempDir()
	writeCA(t, ca)
	var err error
	s.ca, err = loadCA(filepath.Join(ca, "ca.pem"), filepath.Join(ca, "ca.key"))
	must(t, err)
	s.secrets = newSecrets(filepath.Join(ca, "bindings.json"), nil, quiet())
	writeBindings(t, s.secrets.file.path, bound("db", "**"))
	fakeSecret(s.secrets, "real-secret")
	// A rule that allows the name too: the link still comes first, never intercepted.
	s.egress.pol = writePolicy(t, "db\n")
	hello := clientHello(t, "evil.example")
	if code, got := tunnel(t, addr, "db:443", hello); code != http.StatusOK || got != string(hello) {
		t.Errorf("a ClientHello for another host: %d, %d bytes back", code, len(got))
	}
	if code, got := tunnel(t, addr, "db:443", []byte("Authorization: Bearer PH")); code != http.StatusOK || got != "Authorization: Bearer PH" {
		t.Errorf("a placeholder: %d %q", code, got)
	}
	if s := db.seen(); len(s) != 2 || s[0] != string(hello) {
		t.Errorf("the link's socket saw %d connections", len(s))
	}
}

// An absolute-form request for a link goes to its socket in origin form, without the proxy's
// password or headers, one connection per request; on a kept-alive connection, a request for a link
// and then one for another host are each handled as they are: the link's carried, the other host
// blocked by the policy. HTTPS to a link is refused in absolute form: CONNECT carries it.
func TestLinkForward(t *testing.T) {
	t.Parallel()
	s, dir, events, addr := linkProxy(t)
	s.password = "k3y"
	db := serveLink(t, dir, "127.77.0.1", 8080)
	writeLinkList(t, s.links.file, linkEntry(dir, "127.77.0.1", 8080, "db", "db.devvm.internal"))
	c, err := new(net.Dialer).DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(c)
	auth := "Proxy-Authorization: Basic ZGV2dm06azN5\r\n" // devvm:k3y
	get := func(u, host string) (int, string) {
		t.Helper()
		fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: %s\r\n%sProxy-Connection: keep-alive\r\n\r\n", u, host, auth)
		resp, err := http.ReadResponse(r, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get("http://db:8080/a", "db:8080"); code != http.StatusOK || body != "linked /a" {
		t.Errorf("the link: %d %q", code, body)
	}
	if code, _ := get("http://blocked.test/", "blocked.test"); code != http.StatusForbidden {
		t.Errorf("another host after it: %d", code)
	}
	if code, body := get("http://DB.devvm.internal.:8080/b", "DB.devvm.internal.:8080"); code != http.StatusOK || body != "linked /b" {
		t.Errorf("the link again: %d %q", code, body)
	}
	if code, _ := get("https://db:8080/", "db:8080"); code != http.StatusBadRequest {
		t.Errorf("https in absolute form: %d", code)
	}
	seen := db.seen()
	if len(seen) != 2 {
		t.Fatalf("the link's socket saw %d connections, want 2: %q", len(seen), seen)
	}
	for _, req := range seen {
		if !strings.HasPrefix(req, "GET /") || strings.Contains(req, "Proxy-") || strings.Contains(req, "k3y") || strings.Contains(req, "ZGV2dm06azN5") {
			t.Errorf("the link got %q", req)
		}
	}
	if e := events.String(); strings.Count(e, "\n") != 1 || !strings.Contains(e, `"event":"blocked","host":"blocked.test"`) {
		t.Errorf("events: %q", e)
	}
}

// A link on the list whose socket doesn't answer (removed since, or devvm _links gone) is refused
// with 502 and logged once as a refused link.
func TestLinkGone(t *testing.T) {
	t.Parallel()
	s, dir, events, addr := linkProxy(t)
	writeLinkList(t, s.links.file, linkEntry(dir, "127.77.0.1", 5432, "db", "db.devvm.internal"))
	if code, _ := tunnel(t, addr, "db:5432", nil); code != http.StatusBadGateway {
		t.Errorf("CONNECT: %d", code)
	}
	if code, _ := roundTrip(t, addr, "GET http://db:5432/ HTTP/1.1\r\nHost: db:5432\r\n\r\n"); code != http.StatusBadGateway {
		t.Errorf("GET: %d", code)
	}
	e := events.String()
	if strings.Count(e, `"event":"link_refused","host":"db","port":5432,"reason":"not carried","target":"devvm-db-0123456789abcd"`) != 2 ||
		strings.Count(e, "\n") != 2 {
		t.Errorf("events: %q", e)
	}
}

// The list decides nothing it doesn't hold as devvm writes it: an entry whose socket is another
// VM's link's, a path outside the list's folder, or not the one its address and port name, is
// ignored, as is a list that's a symbolic link, that others may write, of another version, or not
// JSON. A request never names a socket: a host with a slash isn't one.
func TestLinkListRefused(t *testing.T) {
	t.Parallel()
	s, dir, _, addr := linkProxy(t)
	linksFile := s.links.file
	serveLink(t, dir, "127.77.0.1", 5432)
	other := LinkSocket(dir, "devvm-other-0123456789abcd", netip.MustParseAddr("127.77.0.1"), 5432)
	ln, err := new(net.ListenConfig).Listen(t.Context(), "unix", other)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	reached := make(chan struct{}, 10)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			reached <- struct{}{}
			_ = c.Close()
		}
	}()
	good := linkEntry(dir, "127.77.0.1", 5432, "db")
	for name, e := range map[string]LinkEntry{
		"another VM's socket": {Hosts: []string{"db"}, Addr: "127.77.0.1", Port: 5432, Socket: other},
		"outside the folder":  {Hosts: []string{"db"}, Addr: "127.77.0.1", Port: 5432, Socket: "/var/run/docker.sock"},
		"in the list's folder": {
			Hosts: []string{"db"}, Addr: "127.77.0.1", Port: 5432,
			Socket: LinkSocket(filepath.Dir(linksFile), linkVM, netip.MustParseAddr("127.77.0.1"), 5432),
		},
		"another port's":    {Hosts: []string{"db"}, Addr: "127.77.0.1", Port: 5432, Socket: LinkSocket(dir, linkVM, netip.MustParseAddr("127.77.0.1"), 5433)},
		"an address unread": {Hosts: []string{"db"}, Addr: "127.77.000.1", Port: 5432, Socket: good.Socket},
		"a mapped address": {
			Hosts: []string{"db"}, Addr: "::ffff:127.77.0.1", Port: 5432,
			Socket: LinkSocket(dir, linkVM, netip.MustParseAddr("::ffff:127.77.0.1"), 5432),
		},
		"not a link's address": {
			Hosts: []string{"db"}, Addr: "127.0.0.1", Port: 5432,
			Socket: LinkSocket(dir, linkVM, netip.MustParseAddr("127.0.0.1"), 5432),
		},
	} {
		writeLinkList(t, linksFile, e)
		if code, _ := tunnel(t, addr, "db:5432", nil); code != http.StatusForbidden {
			t.Errorf("%s: %d, want it blocked", name, code)
		}
	}
	select {
	case <-reached:
		t.Error("another VM's link's socket was reached")
	default:
	}
	// The list itself.
	writeLinkList(t, linksFile, good)
	if code, _ := tunnel(t, addr, "db:5432", []byte("x")); code != http.StatusOK {
		t.Fatalf("the list as devvm writes it: %d", code)
	}
	target := filepath.Join(dir, "real.json")
	must(t, os.Rename(linksFile, target))
	must(t, os.Symlink(target, linksFile))
	if code, _ := tunnel(t, addr, "db:5432", nil); code != http.StatusForbidden {
		t.Errorf("a symbolic link: %d", code)
	}
	must(t, os.Remove(linksFile))
	writeLinkList(t, linksFile, good)
	must(t, os.Chmod(linksFile, 0o620)) //nolint:gosec // the list as no devvm writes it: its group may write it
	if code, _ := tunnel(t, addr, "db:5432", nil); code != http.StatusForbidden {
		t.Errorf("a list others may write: %d", code)
	}
	// Without the sockets' folder, no link is reached.
	must(t, os.Chmod(linksFile, 0o600))
	s.links.sockets = ""
	if code, _ := tunnel(t, addr, "db:5432", nil); code != http.StatusForbidden {
		t.Errorf("without link_sockets: %d", code)
	}
	s.links.sockets = dir
	for name, data := range map[string]string{
		"another version": `{"version":2,"links":[{"hosts":["db"],"addr":"127.77.0.1","port":5432,"socket":"` + good.Socket + `"}]}`,
		"not JSON":        "db 5432 " + good.Socket,
	} {
		must(t, os.WriteFile(linksFile, []byte(data), 0o600))
		must(t, os.Chmod(linksFile, 0o600))
		if code, _ := tunnel(t, addr, "db:5432", nil); code != http.StatusForbidden {
			t.Errorf("%s: %d", name, code)
		}
	}
	must(t, os.Remove(linksFile))
	if code, _ := tunnel(t, addr, "db:5432", nil); code != http.StatusForbidden {
		t.Errorf("no list: %d", code)
	}
	for _, req := range []string{
		"CONNECT " + good.Socket + " HTTP/1.1\r\nHost: x\r\n\r\n",
		"GET http://" + strings.TrimPrefix(good.Socket, "/") + "/ HTTP/1.1\r\nHost: x\r\n\r\n",
	} {
		if code, _ := roundTrip(t, addr, req); code != http.StatusBadRequest && code != http.StatusForbidden {
			t.Errorf("%q: %d", strings.SplitN(req, "\r\n", 2)[0], code)
		}
	}
}

// A name under devvm.internal that's none of the VM's links is refused, whatever the rules
// allow: it's never asked of the policy, a resolver or a company proxy. A short name that's no
// link is the policy's.
func TestLinkDomainRefused(t *testing.T) {
	t.Parallel()
	s, _, events, addr := linkProxy(t)
	s.egress.pol = writePolicy(t, "**\n**:5432\n")
	for _, hp := range []string{"db.devvm.internal:5432", "devvm.internal:443", "x.y.devvm.internal.:5432"} {
		if code, _ := tunnel(t, addr, hp, nil); code != http.StatusForbidden {
			t.Errorf("CONNECT %s: %d", hp, code)
		}
		if code, _ := roundTrip(t, addr, "GET http://"+hp+"/ HTTP/1.1\r\nHost: "+hp+"\r\n\r\n"); code != http.StatusForbidden {
			t.Errorf("GET %s: %d", hp, code)
		}
	}
	if e := events.String(); strings.Count(e, `"refused":true`) != 6 || strings.Count(e, "\n") != 6 {
		t.Errorf("events: %q", e)
	}
}

// Without links, the proxy has no links: a name that would be one is the policy's.
func TestNoLinksFile(t *testing.T) {
	t.Parallel()
	s, dir, _, addr := linkProxy(t)
	serveLink(t, dir, "127.77.0.1", 5432)
	writeLinkList(t, s.links.file, linkEntry(dir, "127.77.0.1", 5432, "db"))
	s.links.file = ""
	if code, _ := tunnel(t, addr, "db:5432", nil); code != http.StatusForbidden {
		t.Errorf("CONNECT: %d", code)
	}
}

// A link's socket is named by a hash of the VM, "socket", its address and its port, each after a
// NUL: what devvm _links listens on and the e2e checks look for.
func TestLinkSocket(t *testing.T) {
	t.Parallel()
	got := LinkSocket("/run", "vm", netip.MustParseAddr("127.77.0.1"), 5432)
	if got != "/run/link-"+LinkHash("vm", "socket", "127.77.0.1", "5432")+".sock" || len(filepath.Base(got)) != len("link-0123456789abcdef.sock") {
		t.Errorf("LinkSocket = %q", got)
	}
	if LinkHash("vm", "a") == LinkHash("vma") || LinkHash("vm", "ab") == LinkHash("vm", "a", "b") {
		t.Error("the parts aren't kept apart")
	}
}
