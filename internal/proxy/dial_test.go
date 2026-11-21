package proxy

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/redact"
)

// ownAddr is the address a test's broker reads as this machine's own (hostOwn).
const ownAddr = "93.184.216.34"

// hostOwn is a read of the host's addresses that finds ownAddr alone.
func hostOwn() *hostAddrs {
	return &hostAddrs{read: func() ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: net.ParseIP(ownAddr), Mask: net.CIDRMask(32, 32)}}, nil
	}}
}

// dialRecorder is a broker's connect that records each address it's asked for and connects to
// none: each connection is an end of a pipe, its other end closed.
type dialRecorder struct {
	mu     sync.Mutex
	dialed []string
}

func (d *dialRecorder) connect(_ context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dialed = append(d.dialed, network+" "+addr)
	c, far := net.Pipe()
	_ = far.Close()
	return c, nil
}

func (d *dialRecorder) got() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.dialed...)
}

// netBroker is a broker of a config with a policy file of rules, whose host has ownAddr alone,
// and which connects through d; the policy file's path is returned too.
func netBroker(t *testing.T, c Config, rules string, d *dialRecorder) (*broker, string) {
	t.Helper()
	policy := filepath.Join(t.TempDir(), "policy")
	must(t, os.WriteFile(policy, []byte(rules), 0o600))
	c.Policies = append(c.Policies, policy)
	if c.VM == "" {
		c.VM = "devvm-x"
	}
	c.Password = redact.NewSecret("pw")
	b, err := newBroker(t.Context(), c, IO{}, quiet())
	must(t, err)
	b.net.own, b.net.connect = hostOwn(), d.connect
	b.net.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		t.Error("a lookup the test didn't expect")
		return nil, errors.New("no lookups")
	}
	return b, policy
}

// The broker connects the child only where its own floor allows: a public address, and an
// internal one, loopback's included, only when a rule names it, judged unmapped, a NAT64 address
// as the IPv4 address in it; never this machine's own address, an unspecified one, one with a
// zone, or port 0. What it dials is the address it judged, unmapped, over TCP.
func TestBrokerDial(t *testing.T) {
	t.Parallel()
	var d dialRecorder
	b, _ := netBroker(t, Config{}, "example.com\n127.0.0.1:5432\n[::1]:6000\n10.1.2.0/24:*\n!10.1.2.3:*\n", &d)
	for _, tc := range []struct {
		addr string
		ok   bool
	}{
		{"1.1.1.1:443", true},
		{"[2606:4700::1111]:443", true},
		{"127.0.0.1:5432", true},
		{"127.0.0.1:5433", false},
		{"127.0.0.2:5432", false},
		{"[::1]:5432", false},
		{"[::1]:6000", true},
		{"[::ffff:127.0.0.1]:5432", true},
		{"[::ffff:127.0.0.1]:22", false},
		{"[64:ff9b::7f00:1]:5432", true},
		{"[64:ff9b::7f00:1]:22", false},
		{"0.0.0.0:5432", false},
		{"[::]:5432", false},
		{"10.1.2.4:22", true},
		{"10.1.2.3:22", false},
		{"10.0.0.1:443", false},
		{"192.168.1.1:80", false},
		{"169.254.169.254:80", false},
		{"[fe80::1%en0]:443", false},
		{"[::1%.]:6000", false},
		{ownAddr + ":443", false},
		{"1.1.1.1:0", false},
		{"198.18.0.1:443", false},
	} {
		ap := netip.MustParseAddrPort(tc.addr)
		c, err := b.dial(t.Context(), ap)
		if c != nil {
			_ = c.Close()
		}
		if (err == nil) != tc.ok {
			t.Errorf("%s: %v, want it allowed: %v", tc.addr, err, tc.ok)
		}
		if err != nil && !errors.Is(err, errRefused) {
			t.Errorf("%s: %v, not the broker's refusal", tc.addr, err)
		}
	}
	want := []string{
		"tcp 1.1.1.1:443", "tcp [2606:4700::1111]:443", "tcp 127.0.0.1:5432", "tcp [::1]:6000",
		"tcp 127.0.0.1:5432", "tcp [64:ff9b::7f00:1]:5432", "tcp 10.1.2.4:22",
	}
	if got := d.got(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("dialed %q, want %q", got, want)
	}
}

// A rule applies to the broker's next connection, as the child's floor applies it to the next
// request, and is gone for the next once it's taken away; a broken deny refuses every address.
// The open tier's broker connects to any address, loopback included.
func TestBrokerDialFollowsRules(t *testing.T) {
	t.Parallel()
	var d dialRecorder
	b, policy := netBroker(t, Config{}, "example.com\n", &d)
	ap := netip.MustParseAddrPort("127.0.0.1:5432")
	try := func(want bool, when string) {
		t.Helper()
		c, err := b.dial(t.Context(), ap)
		if c != nil {
			_ = c.Close()
		}
		if (err == nil) != want {
			t.Errorf("%s: %v, want it allowed: %v", when, err, want)
		}
	}
	try(false, "before the rule")
	must(t, os.WriteFile(policy, []byte("example.com\n127.0.0.1:5432\n"), 0o600))
	try(true, "once the rule is written")
	must(t, os.WriteFile(policy, []byte("example.com\n"), 0o600))
	try(false, "once it's gone")
	must(t, os.WriteFile(policy, []byte("**\n1.1.1.1\n!not a rule\n"), 0o600))
	if c, err := b.dial(t.Context(), netip.MustParseAddrPort("1.1.1.1:443")); err == nil {
		_ = c.Close()
		t.Error("a broken deny: connected")
	}
	open, _ := netBroker(t, Config{Open: true}, "", &d)
	for _, a := range []string{"127.0.0.1:22", "[::1]:22", ownAddr + ":22", "10.0.0.1:22"} {
		c, err := open.dial(t.Context(), netip.MustParseAddrPort(a))
		if err != nil {
			t.Errorf("open, %s: %v", a, err)
			continue
		}
		_ = c.Close()
	}
}

// The child's company proxies are the broker's own config's, by which one alone: a name of
// neither, or one the config hasn't, is refused.
func TestBrokerDialProxy(t *testing.T) {
	t.Parallel()
	got := make(chan string, 1)
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	must(t, err)
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		b, _ := io.ReadAll(c)
		got <- string(b)
	}()
	var d dialRecorder
	b, _ := netBroker(t, Config{Upstream: &Upstream{HTTPS: "http://" + ln.Addr().String()}}, "", &d)
	c, err := b.dialProxy(t.Context(), viaHTTPS)
	if err != nil {
		t.Fatalf("its company proxy: %v", err)
	}
	_, _ = c.Write([]byte("CONNECT"))
	_ = c.Close()
	if s := <-got; s != "CONNECT" {
		t.Errorf("the company proxy got %q", s)
	}
	if _, err := b.dialProxy(t.Context(), viaHTTP); !errors.Is(err, errNotFound) {
		t.Errorf("a company proxy the config hasn't: %v", err)
	}
	if _, err := b.dialProxy(t.Context(), "socks"); !errors.Is(err, errRefused) {
		t.Errorf("a company proxy of no kind: %v", err)
	}
	for _, r := range []wireDialReq{{}, {Addr: "1.1.1.1:443", Proxy: viaHTTPS}, {Addr: "example.com:443"}, {Addr: "1.1.1.1"}} {
		if f, err := b.dialFile(t.Context(), r); err == nil {
			_ = f.Close()
			t.Errorf("%+v: a connection", r)
		}
	}
	if len(d.got()) != 0 {
		t.Errorf("dialed %q", d.got())
	}
}

// The broker looks up only a host name in the proxy's own form, with its one trailing dot, as
// the child's floor asks for one (lookupName): no address, no other form, and no name the proxy
// doesn't take.
func TestBrokerLookup(t *testing.T) {
	t.Parallel()
	var d dialRecorder
	b, _ := netBroker(t, Config{}, "", &d)
	var asked []string
	b.net.lookup = func(_ context.Context, network, name string) ([]netip.Addr, error) {
		asked = append(asked, network+" "+name)
		return []netip.Addr{netip.MustParseAddr("1.2.3.4")}, nil
	}
	for _, tc := range []struct {
		name string
		ok   bool
	}{
		{"example.com.", true},
		{"localhost.", true},
		{"a_b.example.com.", true},
		{"example.com", false},
		{"example.com..", false},
		{"Example.com.", false},
		{"1.2.3.4.", false},
		{"127.1.", false},
		{"[::1].", false},
		{"::1.", false},
		{"::1%.", false}, // Go's resolver reads ::1% as ::1
		{"::1%..", false},
		{"::1%lo0.", false},
		{".", false},
		{"", false},
		{"exa mple.com.", false},
		{"exKample.com.", false},
		{strings.Repeat("a.", 127) + "aa.", false},
	} {
		addrs, err := b.lookup(t.Context(), tc.name)
		if (err == nil) != tc.ok {
			t.Errorf("%q: %v, want it looked up: %v", tc.name, err, tc.ok)
		}
		if err == nil && (len(addrs) != 1 || addrs[0].String() != "1.2.3.4") {
			t.Errorf("%q: %v", tc.name, addrs)
		}
	}
	if strings.Join(asked, ",") != "ip example.com.,ip localhost.,ip a_b.example.com." {
		t.Errorf("looked up %q", asked)
	}
}

// A lookup's failure reaches the child as the resolver's own error, which its floor reads: no such
// host, a timeout and a temporary failure each as a code alone, which the child makes a
// net.DNSError of again; any other is no DNSError.
func TestLookupErrors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		err  *net.DNSError
		code string
	}{
		{&net.DNSError{Err: "no such host", Name: "x.", IsNotFound: true}, codeNoSuchHost},
		{&net.DNSError{Err: "i/o timeout", Name: "x.", IsTimeout: true, IsTemporary: true}, codeTimeout},
		{&net.DNSError{Err: "server misbehaving", Name: "x.", IsTemporary: true}, codeTemporary},
		{&net.DNSError{Err: "cannot unmarshal DNS message", Name: "x."}, ""},
	} {
		if got := dnsCode(tc.err); got != tc.code {
			t.Errorf("%v: %q, want %q", tc.err, got, tc.code)
			continue
		}
		code := tc.code
		if code == "" {
			code = codeFailed
		}
		if got := errorCodeOf(tc.err); got != code {
			t.Errorf("%v: the reply's code %q, want %q", tc.err, got, code)
		}
		back := lookupError("x.example.", codeError{code})
		var e *net.DNSError
		if errors.As(back, &e) != (tc.code != "") {
			t.Errorf("%s: %v", code, back)
			continue
		}
		if e != nil && (e.IsNotFound != tc.err.IsNotFound || e.IsTimeout != tc.err.IsTimeout || e.IsTemporary != tc.err.IsTemporary || e.Name != "x.example.") {
			t.Errorf("%s: %+v, want %+v", code, e, tc.err)
		}
	}
	if got := unresolvedReason("jenkins", lookupError("jenkins.", codeError{codeNoSuchHost})); got != reasonShortName {
		t.Errorf("a short name the broker found nothing for: %q", got)
	}
	if got := unresolvedReason("a.test", lookupError("a.test.", codeError{codeTimeout})); got != reasonDNSTimeout {
		t.Errorf("a lookup that timed out at the broker: %q", got)
	}
}

// A lookup or a connection the child gives up on is cancelled: the broker's lookup ends at once,
// and its slot is free again, while a value's lookup, which the broker keeps for the next
// request, runs on. A cancel of an ID that isn't under way is dropped.
func TestBrokerCancel(t *testing.T) {
	t.Parallel()
	b := testBroker(t)
	ended := make(chan error, 1)
	b.net.lookup = func(ctx context.Context, _, name string) ([]netip.Addr, error) {
		if name == "fast.test." {
			return []netip.Addr{netip.MustParseAddr("1.2.3.4")}, nil
		}
		<-ctx.Done()
		ended <- ctx.Err()
		return nil, ctx.Err()
	}
	bfc, cfc := pairOf(t)
	served := make(chan error, 1)
	go func() { served <- b.serveChild(t.Context(), bfc) }()
	must(t, cfc.send(toBroker{Type: frameCancel, ID: 42}, nil))
	client := newBrokerClient(cfc)
	read := make(chan error, 1)
	go func() { read <- client.read() }()
	for range lookupsMax + 1 {
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		if _, err := client.lookup(ctx, "slow.test."); !errors.Is(err, errGaveUp) {
			t.Errorf("a lookup given up on: %v", err)
		}
		cancel()
		select {
		case err := <-ended:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("the broker's lookup ended with %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the broker's lookup wasn't cancelled")
		}
	}
	// More lookups than the cap were given up on, so each slot came back.
	if addrs, err := client.lookup(t.Context(), "fast.test."); err != nil || len(addrs) != 1 {
		t.Errorf("after the cancels: %v, %v", addrs, err)
	}
	_ = bfc.close()
	if err := <-read; isRuleError(err) {
		t.Errorf("the client's read: %v", err)
	}
	if err := <-served; isRuleError(err) {
		t.Errorf("serveChild: %v", err)
	}
}

// The child's side, over a real socketpair: a lookup, and its failure as the resolver's error; a
// connection to a server on loopback that a rule names, as the frame's descriptor, which carries
// bytes both ways; and a connection to a loopback port that no rule names, refused with a code
// alone, which never reaches the server. Once the broker's end closes, both fail at once.
func TestBrokerClientDial(t *testing.T) {
	t.Parallel()
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	must(t, err)
	defer ln.Close()
	accepted := make(chan struct{}, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- struct{}{}
			go func() {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	other, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	must(t, err)
	defer other.Close()
	go func() {
		if c, err := other.Accept(); err == nil {
			_ = c.Close()
			t.Error("a port no rule names was reached")
		}
	}()
	b, err := newBroker(t.Context(), Config{VM: "devvm-x", Password: redact.NewSecret("pw"), Policies: policyFiles(t, "127.0.0.1:"+strconv.Itoa(port)+"\n")}, IO{}, quiet())
	must(t, err)
	b.net.lookup = func(_ context.Context, _, name string) ([]netip.Addr, error) {
		if name == "nx.test." {
			return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
		}
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	bfc, cfc := pairOf(t)
	served := make(chan error, 1)
	go func() { served <- b.serveChild(t.Context(), bfc) }()
	client := newBrokerClient(cfc)
	read := make(chan error, 1)
	go func() { read <- client.read() }()
	if addrs, err := client.lookup(t.Context(), "db.test."); err != nil || len(addrs) != 1 || addrs[0] != netip.MustParseAddr("127.0.0.1") {
		t.Errorf("lookup: %v, %v", addrs, err)
	}
	var dnsErr *net.DNSError
	if _, err := client.lookup(t.Context(), "nx.test."); !errors.As(err, &dnsErr) || !dnsErr.IsNotFound {
		t.Errorf("a name with no address: %v", err)
	}
	if _, err := client.lookup(t.Context(), "1.2.3.4."); err == nil || !strings.Contains(err.Error(), codeRefused) {
		t.Errorf("an address to look up: %v", err)
	}
	conn, err := client.dial(t.Context(), netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(port))) //nolint:gosec // a port the system gave
	if err != nil {
		t.Fatalf("a port a rule names: %v", err)
	}
	_, _ = conn.Write([]byte("ping"))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "ping" {
		t.Errorf("the connection reads %q, %v", buf, err)
	}
	_ = conn.Close()
	<-accepted
	oport := other.Addr().(*net.TCPAddr).Port
	if c, err := client.dial(t.Context(), netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(oport))); err == nil || !strings.Contains(err.Error(), codeRefused) { //nolint:gosec // a port the system gave
		if c != nil {
			_ = c.Close()
		}
		t.Errorf("a port no rule names: %v", err)
	}
	if _, err := client.dialProxy(t.Context(), viaHTTPS); err == nil || !strings.Contains(err.Error(), codeNotFound) {
		t.Errorf("a company proxy the config hasn't: %v", err)
	}
	_ = bfc.close()
	if err := <-read; isRuleError(err) {
		t.Errorf("the client's read: %v", err)
	}
	if _, err := client.dial(t.Context(), netip.MustParseAddrPort("1.1.1.1:443")); !errors.Is(err, errBrokerGone) {
		t.Errorf("once the broker is gone: %v", err)
	}
	if err := <-served; isRuleError(err) {
		t.Errorf("serveChild: %v", err)
	}
}

// The broker's log names at most failedLogMax of the child's failed connections a minute, then
// says once that it stops, so a VM that asks for one refused address after another can't fill
// it; another request's failure is logged each time.
func TestBrokerFailedLogCapped(t *testing.T) {
	t.Parallel()
	b := testBroker(t)
	var out syncBuffer
	b.log = log.New(&out, "", 0)
	bfc, cfc := pairOf(t)
	served := make(chan error, 1)
	go func() { served <- b.serveChild(t.Context(), bfc) }()
	client := newBrokerClient(cfc)
	read := make(chan error, 1)
	go func() { read <- client.read() }()
	for i := range failedLogMax + 5 {
		ap := netip.AddrPortFrom(netip.MustParseAddr("10.0.0.1"), uint16(1000+i))
		if _, err := client.dial(t.Context(), ap); err == nil || !strings.Contains(err.Error(), codeRefused) {
			t.Fatalf("%s: %v, want it refused", ap, err)
		}
	}
	if _, err := client.dialProxy(t.Context(), viaHTTPS); err == nil {
		t.Error("a company proxy the config hasn't: no error")
	}
	_ = bfc.close()
	t.Logf("the reader ended: %v", <-read)
	if err := <-served; isRuleError(err) {
		t.Errorf("serveChild: %v", err)
	}
	if n, capped := strings.Count(out.String(), "the child's dial request: refused"), strings.Count(out.String(), failedCapped); n != failedLogMax || capped != 1 {
		t.Errorf("%d refusals and %d caps logged, want %d and 1:\n%s", n, capped, failedLogMax, out.String())
	}
}

// perMinute lets limit lines through a minute, then one that says the rest aren't logged, and
// starts again once the minute is over.
func TestPerMinute(t *testing.T) {
	t.Parallel()
	var p perMinute
	now := time.Now()
	var logged, capped int
	for range 5 {
		l, c := p.next(now, 3)
		logged, capped = logged+boolInt(l), capped+boolInt(c)
	}
	if logged != 3 || capped != 1 {
		t.Errorf("%d logged, %d capped, want 3 and 1", logged, capped)
	}
	if l, c := p.next(now.Add(time.Minute+time.Second), 3); !l || c {
		t.Errorf("the next minute: %v, %v, want it logged", l, c)
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
