package vmssh

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/daemon"
	"github.com/devvmhq/devvm/internal/proxy"
	"github.com/devvmhq/devvm/internal/state"
)

// shortDir is a folder whose paths are short enough for sockets: a test's
// own temporary folder isn't, on macOS.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "dl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// optionValues returns the values args give an ssh option, in order.
func optionValues(args []string, name string) []string {
	var out []string
	for i, a := range args {
		if v, ok := strings.CutPrefix(a, name+"="); ok && i > 0 && args[i-1] == "-o" {
			out = append(out, v)
		}
	}
	return out
}

// The link session's master asks for nothing at first and reads nothing
// of the user's; each link is a forward of its own through it, which never
// clears itself nor connects without the master; a connection is a
// channel of a target's master to the target's own loopback, and without
// the master fails (ProxyCommand=/usr/bin/false first, as ssh takes an
// option's first value).
func TestLinkArgs(t *testing.T) {
	o := SSH{VM: "devvm-app", User: "u", Key: "/k", KnownHosts: "/kh", Control: "/s.ctl", Port: 61022}
	m := o.linkMasterArgs()
	got := strings.Join(m, " ")
	for _, want := range []string{"-F none -M -S /s.ctl -N ", "ClearAllForwardings=yes", "ExitOnForwardFailure=no", "IdentityAgent=none", "ForwardAgent=no", "PermitRemoteOpen=none", "-p 61022 -l u 127.0.0.1"} {
		if !strings.Contains(got, want) {
			t.Errorf("master lacks %q: %s", want, got)
		}
	}
	for _, a := range m {
		if a == "-R" || a == "-L" || a == "-D" || a == "-A" || a == "-X" || a == "-Y" || a == "-W" {
			t.Errorf("the master asks for %s: %s", a, got)
		}
	}
	for _, op := range []string{"forward", "cancel"} {
		f := forwardArgs("/s.ctl", op, "127.77.0.1:5432", "/run/link-0123.sock")
		got := strings.Join(f, " ")
		if !strings.HasPrefix(got, "-F none -S /s.ctl ") || !strings.Contains(got, " -O "+op+" -R 127.77.0.1:5432:/run/link-0123.sock ") {
			t.Errorf("%s: %s", op, got)
		}
		if v := optionValues(f, "ClearAllForwardings"); !slices.Equal(v, []string{"no"}) {
			t.Errorf("%s: ClearAllForwardings %q", op, v)
		}
		if v := optionValues(f, "ProxyCommand"); !slices.Equal(v, []string{"/usr/bin/false"}) {
			t.Errorf("%s: ProxyCommand %q", op, v)
		}
	}
	r := o.linkRelayArgs(5432)
	got = strings.Join(r, " ")
	if v := optionValues(r, "ProxyCommand"); len(v) < 2 || v[0] != "/usr/bin/false" {
		t.Errorf("relay: ProxyCommand %q", v)
	}
	if !strings.Contains(got, " -W 127.0.0.1:5432") || !strings.Contains(got, "ControlMaster=no") || !strings.Contains(got, "IdentityAgent=none") || !slices.Contains(r, "/s.ctl") {
		t.Errorf("relay: %s", got)
	}
	if slices.Contains(r, "-M") || slices.Contains(r, "-R") {
		t.Errorf("relay: %s", got)
	}
}

// Each link's socket and each master's control socket is named by a hash
// of what it's for, short enough for a socket however long the VM's name
// is, as the forwarder's are (Forwards.Control).
func TestLinkFiles(t *testing.T) {
	home := "/Users/" + strings.Repeat("h", 24)
	long := "devvm-" + strings.Repeat("f", 35) + "-0123456789abcd"
	l := &Links{VM: long, Dir: home + "/.local/share/devvm/run"}
	a := netip.MustParseAddr
	names := []string{
		l.Socket(Link{Addr: a("127.77.0.1"), Port: 5432}),
		l.Socket(Link{Addr: a("127.77.0.1"), Port: 5433}),
		l.Socket(Link{Addr: a("127.77.0.2"), Port: 5432}),
		l.Control("", ""),
		l.Control("devvm-db-0123456789abcd", "1 a"),
		l.Control("devvm-db-0123456789abcd", "2 b"),
		l.Control("devvm-pg-0123456789abcd", "1 a"),
		(&Links{VM: "devvm-x-0123456789abcd", Dir: l.Dir}).Socket(Link{Addr: a("127.77.0.1"), Port: 5432}),
		(&Links{VM: "devvm-x-0123456789abcd", Dir: l.Dir}).Control("", ""),
	}
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] || filepath.Dir(n) != l.Dir || len(n) > ControlMax || strings.ContainsAny(filepath.Base(n), ": ") {
			t.Errorf("%s: a repeat, elsewhere, or too long (%d)", n, len(n))
		}
		seen[n] = true
	}
	if !strings.HasSuffix(names[0], ".sock") || !strings.HasSuffix(names[3], ".ctl") {
		t.Errorf("names: %q", names)
	}
}

// A link's socket is mode 600 in a folder of 700: a stale socket there is
// replaced, one a program listens on and anything else is refused, and
// closing it removes it.
func TestListenSocket(t *testing.T) {
	dir := filepath.Join(shortDir(t), "run")
	if err := state.PrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := listenSocket(t.Context(), file); err == nil {
		t.Error("a file replaced")
	}
	sock := filepath.Join(dir, "s.sock")
	ln, err := listenSocket(t.Context(), sock)
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(sock); err != nil || fi.Mode().Perm() != 0o600 || fi.Mode()&os.ModeSocket == 0 {
		t.Errorf("socket: %v, %v", fi.Mode(), err)
	}
	if _, err := listenSocket(t.Context(), sock); err == nil {
		t.Error("a socket in use replaced")
	}
	_ = ln.Close()
	if _, err := os.Lstat(sock); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("closed, the socket stays: %v", err)
	}
	// A stale one: left by a process that ended.
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	_ = stale.Close()
	ln, err = listenSocket(t.Context(), sock)
	if err != nil {
		t.Fatalf("a stale socket: %v", err)
	}
	_ = ln.Close()
}

// fakeLinks is the ssh a session runs, and the client and targets behind
// it: masters make their control sockets and end with their context or
// when dropped; forwards and cancels are recorded, a port the client
// holds refused; each relay answers where it went.
type fakeLinks struct {
	mu       sync.Mutex
	starts   [][]string
	commands [][]string
	relays   [][]string
	events   []string
	failed   []string
	taken    map[string]bool // ADDR:PORT a program in the client holds
	drop     map[string]chan struct{}
	want     []Link
	target   func(Link) (LinkTarget, string, string)
	hold     chan struct{} // relays wait for it to close, when set
	slow     chan struct{} // link_limit's events wait for it to close, when set
}

func (f *fakeLinks) start(ctx context.Context, args []string) (func() error, error) {
	ctl := args[slices.Index(args, "-S")+1]
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "unix", ctl)
	if err != nil {
		return nil, err
	}
	drop := make(chan struct{})
	f.mu.Lock()
	f.starts = append(f.starts, args)
	f.drop[ctl] = drop
	f.mu.Unlock()
	return func() error {
		defer func() { _ = ln.Close() }()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-drop:
			return errors.New("dropped")
		}
	}, nil
}

func (f *fakeLinks) command(_ context.Context, args []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, args)
	spec := args[slices.Index(args, "-R")+1]
	listen := spec[:strings.LastIndex(spec, ":/")]
	if f.taken[listen] {
		return errors.New("remote port forwarding failed for listen port")
	}
	return nil
}

func (f *fakeLinks) relay(_ context.Context, conn net.Conn, args []string) {
	defer func() { _ = conn.Close() }()
	f.mu.Lock()
	f.relays = append(f.relays, args)
	hold := f.hold
	f.mu.Unlock()
	ctl := args[slices.Index(args, "-S")+1]
	_, _ = fmt.Fprintf(conn, "%s via %s\n", args[slices.Index(args, "-W")+1], filepath.Base(ctl))
	if hold != nil {
		<-hold
	}
}

func (f *fakeLinks) event(event string, l Link, why string) {
	if event == EventLinkLimit && f.slow != nil {
		<-f.slow
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, strings.TrimSpace(event+" "+l.Name+" "+why))
}

func (f *fakeLinks) newLinks(t *testing.T) *Links {
	f.drop, f.taken = map[string]chan struct{}{}, map[string]bool{}
	return &Links{
		VM: "devvm-app-0123456789abcd", Dir: filepath.Join(shortDir(t), "run"),
		SSH:   SSH{VM: "devvm-app-0123456789abcd", User: "u", Key: "/k", KnownHosts: "/kh", Port: 61022},
		Alive: func(context.Context) bool { return true }, Every: time.Hour, Sync: 20 * time.Millisecond,
		Tick: 20 * time.Millisecond, Wait: 2 * time.Second, Log: io.Discard,
		Want: func() ([]Link, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return slices.Clone(f.want), nil
		},
		Check: func(_ context.Context, l Link) (LinkTarget, string, string) { return f.target(l) },
		Event: f.event,
		Failed: func(l Link, _ error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.failed = append(f.failed, l.Name)
		},
		Start: f.start, Command: f.command, Relay: f.relay,
	}
}

func (f *fakeLinks) setWant(ls ...Link) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.want = ls
}

// dial connects to a link's socket and returns what comes back: "" for a
// connection closed without a word, "refused" for none.
func dial(t *testing.T, sock string) string {
	t.Helper()
	d := net.Dialer{Timeout: time.Second}
	c, err := d.DialContext(t.Context(), "unix", sock)
	if err != nil {
		return "refused"
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	line, _ := bufio.NewReader(c).ReadString('\n')
	return strings.TrimSpace(line)
}

// dialMany makes n connections to a link's socket, one after another,
// closed once the test ends. One the socket's backlog refuses is tried
// again for up to five seconds: a busy host can keep the session's loop
// from running long enough for a dialer in the same process to fill the
// backlog, whatever the loop does, but a loop that stops accepting fails.
func dialMany(t *testing.T, sock string, n int) []net.Conn {
	t.Helper()
	var conns []net.Conn
	t.Cleanup(func() {
		for _, c := range conns {
			_ = c.Close()
		}
	})
	d := net.Dialer{Timeout: time.Second}
	again := 0
	for i := range n {
		var c net.Conn
		var err error
		for give := time.Now().Add(5 * time.Second); ; again++ {
			c, err = d.DialContext(t.Context(), "unix", sock)
			if !errors.Is(err, syscall.ECONNREFUSED) || time.Now().After(give) {
				break
			}
			time.Sleep(time.Millisecond)
		}
		if err != nil {
			t.Fatalf("connection %d: %v", i+1, err)
		}
		conns = append(conns, c)
	}
	t.Logf("%d dials tried again", again)
	return conns
}

// waitFor waits for cond, failing the test after two seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for range 200 {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("waited for %s", what)
}

func (f *fakeLinks) count(of *[][]string) int { return len(f.snap(of)) }

// snap is what was recorded so far.
func (f *fakeLinks) snap(of *[][]string) [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(*of)
}

// A session carries each link through its own master to the client: each
// connection checked, then carried through a master to the target's own
// loopback, one per target and start; a link the client refuses fails
// alone; refusals carry nothing; a link that's gone is cancelled and its
// socket removed; with no links left, the session ends.
func TestLinks(t *testing.T) {
	a := netip.MustParseAddr
	db := Link{Name: "db", Target: "devvm-db-0123456789abcd", Addr: a("127.77.0.1"), Port: 5432}
	cache := Link{Name: "db", Target: db.Target, Addr: a("127.77.0.1"), Port: 6379}
	api := Link{Name: "api", Target: "devvm-api-0123456789abcd", Addr: a("127.77.0.2"), Port: 80}
	f := &fakeLinks{}
	l := f.newLinks(t)
	f.taken = map[string]bool{"127.77.0.2:80": true}
	start := "s1"
	why := map[int]string{}
	f.target = func(lk Link) (LinkTarget, string, string) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if w := why[lk.Port]; w != "" {
			ev := EventLinkRefused
			if w == "stopped" {
				ev = EventLinkStopped
			}
			return LinkTarget{}, ev, w
		}
		return LinkTarget{SSH: SSH{VM: lk.Target, User: "u", Key: "/k2", KnownHosts: "/kh", Port: 62022}, Start: start}, "", ""
	}
	f.setWant(db, cache, api)
	done := make(chan error, 1)
	go func() { done <- l.Run(t.Context()) }()
	waitFor(t, "the links", func() bool { return f.count(&f.commands) == 3 })
	session := l.Control("", "")
	if s := f.snap(&f.starts)[0]; s[slices.Index(s, "-S")+1] != session || !slices.Contains(s, "ClearAllForwardings=yes") {
		t.Errorf("the session's master: %q", s)
	}
	for _, c := range f.snap(&f.commands) {
		if c[slices.Index(c, "-S")+1] != session || c[slices.Index(c, "-O")+1] != "forward" {
			t.Errorf("a forward: %q", c)
		}
	}
	// A link is marked failed after its forward command returns, so wait for it.
	waitFor(t, "api failed", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.failed) > 0
	})
	f.mu.Lock()
	if !slices.Equal(f.failed, []string{"api"}) {
		t.Errorf("failed: %q", f.failed)
	}
	f.mu.Unlock()
	if got := dial(t, l.Socket(api)); got != "refused" {
		t.Errorf("api's socket: %q", got)
	}
	ctl := filepath.Base(l.Control(db.Target, "s1"))
	if got := dial(t, l.Socket(db)); got != "127.0.0.1:5432 via "+ctl {
		t.Errorf("db: %q", got)
	}
	if got := dial(t, l.Socket(cache)); got != "127.0.0.1:6379 via "+ctl {
		t.Errorf("cache: %q", got)
	}
	if n := f.count(&f.starts); n != 2 {
		t.Errorf("%d masters for one target's start", n-1)
	}
	for _, s := range []string{l.Socket(db), l.Socket(cache)} {
		if fi, err := os.Lstat(s); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v, %v", s, fi, err)
		}
	}

	// Refused: each with why, nothing carried.
	f.mu.Lock()
	why[5432], why[6379] = "unpublished", "stopped"
	f.mu.Unlock()
	relays := f.count(&f.relays)
	if got := dial(t, l.Socket(db)); got != "" {
		t.Errorf("unpublished: %q", got)
	}
	if got := dial(t, l.Socket(cache)); got != "" {
		t.Errorf("stopped: %q", got)
	}
	if n := f.count(&f.relays); n != relays {
		t.Errorf("refused, yet carried: %d", n-relays)
	}

	// The target started again: a master of its own; one that ended is
	// made again.
	f.mu.Lock()
	why, start = map[int]string{}, "s2"
	f.mu.Unlock()
	ctl2 := filepath.Base(l.Control(db.Target, "s2"))
	if got := dial(t, l.Socket(db)); got != "127.0.0.1:5432 via "+ctl2 {
		t.Errorf("after a restart: %q", got)
	}
	f.mu.Lock()
	close(f.drop[l.Control(db.Target, "s2")])
	f.mu.Unlock()
	waitFor(t, "the master's end", func() bool {
		l.mu.Lock()
		defer l.mu.Unlock()
		return l.masters[db.Target].dead()
	})
	if got := dial(t, l.Socket(db)); got != "127.0.0.1:5432 via "+ctl2 {
		t.Errorf("after its master ended: %q", got)
	}
	if n := f.count(&f.starts); n != 4 {
		t.Errorf("%d masters, want 4", n)
	}

	// cache goes: cancelled through the session, its socket removed.
	f.setWant(db, api)
	waitFor(t, "the cancel", func() bool { return f.count(&f.commands) == 4 })
	if c := f.snap(&f.commands)[3]; c[slices.Index(c, "-O")+1] != "cancel" || !slices.Contains(c, "127.77.0.1:6379:"+l.Socket(cache)) {
		t.Errorf("the cancel: %q", c)
	}
	waitFor(t, "the socket's removal", func() bool { _, err := os.Lstat(l.Socket(cache)); return err != nil })
	if n := f.count(&f.commands); n != 4 {
		t.Errorf("api tried again: %d commands", n)
	}

	f.setWant()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no links, yet it runs")
	}
	if _, err := os.Lstat(l.Socket(db)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("db's socket stays: %v", err)
	}
	wantEvents := []string{"link db", "link db", "link_refused db unpublished", "link_target_stopped db stopped", "link db", "link db"}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !slices.Equal(f.events, wantEvents) {
		t.Errorf("events: %q, want %q", f.events, wantEvents)
	}
}

// The session's connection made again once it's lost, its links with it;
// once the client no longer runs as it did, the session ends.
func TestLinksSessionLost(t *testing.T) {
	db := Link{Name: "db", Target: "devvm-db-0123456789abcd", Addr: netip.MustParseAddr("127.77.0.1"), Port: 5432}
	f := &fakeLinks{}
	l := f.newLinks(t)
	var alive sync.Mutex
	up := true
	l.Alive = func(context.Context) bool { alive.Lock(); defer alive.Unlock(); return up }
	f.target = func(Link) (LinkTarget, string, string) { return LinkTarget{}, EventLinkRefused, "x" }
	f.setWant(db)
	done := make(chan error, 1)
	go func() { done <- l.Run(t.Context()) }()
	waitFor(t, "the link", func() bool { return f.count(&f.commands) == 1 })
	f.mu.Lock()
	close(f.drop[l.Control("", "")])
	f.mu.Unlock()
	waitFor(t, "the link again", func() bool { return f.count(&f.commands) == 2 })
	alive.Lock()
	up = false
	alive.Unlock()
	f.mu.Lock()
	close(f.drop[l.Control("", "")])
	f.mu.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the client stopped, yet it runs")
	}
	if _, err := os.Lstat(l.Socket(db)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the socket stays: %v", err)
	}
	l.Alive = func(context.Context) bool { return false }
	if err := l.Run(t.Context()); err == nil {
		t.Error("a client that doesn't run: no error")
	}
}

// At most maxLinkConns connections at once: the next is refused and
// logged, and one goes once another ends.
func TestLinksCap(t *testing.T) {
	db := Link{Name: "db", Target: "devvm-db-0123456789abcd", Addr: netip.MustParseAddr("127.77.0.1"), Port: 5432}
	f := &fakeLinks{hold: make(chan struct{})}
	l := f.newLinks(t)
	f.target = func(lk Link) (LinkTarget, string, string) {
		return LinkTarget{SSH: SSH{VM: lk.Target, User: "u", Key: "/k2", KnownHosts: "/kh", Port: 62022}, Start: "s"}, "", ""
	}
	f.setWant(db)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- l.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	}()
	waitFor(t, "the link", func() bool { _, err := os.Lstat(l.Socket(db)); return err == nil })
	dialMany(t, l.Socket(db), maxLinkConns)
	waitFor(t, "every relay", func() bool { return f.count(&f.relays) == maxLinkConns })
	if got := dial(t, l.Socket(db)); got != "" {
		t.Errorf("one past the cap: %q", got)
	}
	f.mu.Lock()
	limit := slices.Contains(f.events, "link_limit db 256")
	f.mu.Unlock()
	if !limit {
		t.Error("the refusal isn't logged")
	}
	close(f.hold)
	waitFor(t, "a free slot", func() bool { return dial(t, l.Socket(db)) != "" })
}

// A burst of connections, made one after another as fast as the client's
// ssh makes them, past the cap and past macOS's backlog of 128 on its
// own, is accepted whole: each is carried, up to the cap, or refused with
// link_limit. The loop goes on accepting while the refusals are logged,
// however slowly: one that waited for them would leave the socket's
// backlog full until they were (dialMany). The relays are held, so the
// cap is how many are carried at once, never more.
func TestLinksBurst(t *testing.T) {
	db := Link{Name: "db", Target: "devvm-db-0123456789abcd", Addr: netip.MustParseAddr("127.77.0.1"), Port: 5432}
	f := &fakeLinks{hold: make(chan struct{}), slow: make(chan struct{})}
	logged := sync.OnceFunc(func() { close(f.slow) })
	l := f.newLinks(t)
	f.target = func(lk Link) (LinkTarget, string, string) {
		return LinkTarget{SSH: SSH{VM: lk.Target, User: "u", Key: "/k2", KnownHosts: "/kh", Port: 62022}, Start: "s"}, "", ""
	}
	f.setWant(db)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- l.Run(ctx) }()
	defer func() {
		logged()
		close(f.hold)
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	}()
	waitFor(t, "the link", func() bool { _, err := os.Lstat(l.Socket(db)); return err == nil })
	const n = maxLinkConns + 144
	conns := dialMany(t, l.Socket(db), n)
	logged()
	limited := func() int {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(slices.DeleteFunc(slices.Clone(f.events), func(e string) bool { return e != "link_limit db 256" }))
	}
	waitFor(t, "each carried or refused", func() bool { return f.count(&f.relays) == maxLinkConns && limited() == n-maxLinkConns })
	// Each connection, as the client sees it: carried, its relay's line,
	// or refused, closed without a word.
	carried, refused := 0, 0
	for i, c := range conns {
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		line, err := bufio.NewReader(c).ReadString('\n')
		switch {
		case err == nil && strings.HasPrefix(line, "127.0.0.1:5432 via "):
			carried++
		case errors.Is(err, io.EOF) && line == "":
			refused++
		default:
			t.Errorf("connection %d: %q, %v", i+1, line, err)
		}
	}
	if carried != maxLinkConns || refused != n-maxLinkConns || f.count(&f.relays) != maxLinkConns {
		t.Errorf("%d carried, %d refused, %d relays; want %d, %d, %d", carried, refused, f.count(&f.relays), maxLinkConns, n-maxLinkConns, maxLinkConns)
	}
}

// stuckSSH puts an ssh stand-in first on PATH that never exits on its
// own, as a relay's ssh whose channel the target never ends, and returns
// how to read the pids of those that started, in order. Any still running
// when the test ends are killed.
func stuckSSH(t *testing.T) func() []int {
	t.Helper()
	pids := filepath.Join(t.TempDir(), "pids")
	fakeSSH(t, "echo $$ >> "+pids+"\nexec sleep 600")
	read := func() []int {
		b, _ := os.ReadFile(pids) //nolint:gosec // gosec G304: the test's own temp file
		var started []int
		for f := range strings.FieldsSeq(string(b)) {
			if pid, err := strconv.Atoi(f); err == nil {
				started = append(started, pid)
			}
		}
		return started
	}
	t.Cleanup(func() {
		for _, pid := range read() {
			if daemon.Alive(pid) {
				_ = daemon.Signal(pid, syscall.SIGKILL)
			}
		}
	})
	return read
}

// A relay whose target never ends its channel (a VM whose sshd ignores
// EOF) still ends when its link is removed, or when its target's master
// is replaced (the target started again): its ssh is killed and its slot
// is free again, so a target can't keep the session's relays by holding
// them open.
func TestLinksEndStuckRelays(t *testing.T) {
	a := netip.MustParseAddr
	db := Link{Name: "db", Target: "devvm-db-0123456789abcd", Addr: a("127.77.0.1"), Port: 5432}
	api := Link{Name: "api", Target: "devvm-api-0123456789abcd", Addr: a("127.77.0.2"), Port: 80}
	pids := stuckSSH(t)
	f := &fakeLinks{}
	l := f.newLinks(t)
	l.Relay = pipeSSH
	start := "s1"
	f.target = func(lk Link) (LinkTarget, string, string) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return LinkTarget{SSH: SSH{VM: lk.Target, User: "u", Key: "/k2", KnownHosts: "/kh", Port: 62022}, Start: start}, "", ""
	}
	f.setWant(db, api)
	before := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- l.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
		// Nothing of the session's is left once it ends: every relay's
		// goroutines, and the stand-in's pipes, are gone.
		waitFor(t, "the session's goroutines to end", func() bool { return runtime.NumGoroutine() <= before })
	}()
	for _, lk := range []Link{db, api} {
		waitFor(t, lk.Name+"'s socket", func() bool { _, err := os.Lstat(l.Socket(lk)); return err == nil })
	}
	// l.conns was made before the first Check, which the test follows
	// through f.mu.
	slots := func() int { return len(l.conns) }

	// db's link removed: its relay ends, though api's link stays.
	dialMany(t, l.Socket(db), 1)
	waitFor(t, "db's ssh", func() bool { return len(pids()) == 1 })
	f.setWant(api)
	waitFor(t, "db's ssh to end", func() bool { return !daemon.Alive(pids()[0]) })
	waitFor(t, "db's slot", func() bool { return slots() == 0 })

	// api's target started again: the next connection gets a master of
	// its own, and the old master's relay ends with it.
	dialMany(t, l.Socket(api), 1)
	waitFor(t, "api's ssh", func() bool { return len(pids()) == 2 })
	f.mu.Lock()
	start = "s2"
	f.mu.Unlock()
	dialMany(t, l.Socket(api), 1)
	waitFor(t, "api's second ssh", func() bool { return len(pids()) == 3 })
	waitFor(t, "api's first ssh to end", func() bool { return !daemon.Alive(pids()[1]) })
	waitFor(t, "api's first slot", func() bool { return slots() == 1 })
	if !daemon.Alive(pids()[2]) {
		t.Error("the new master's relay ended too")
	}

	// api's link names another target at the same address and port, as
	// a link removed and another added within one sync would: its open
	// relay, to the old target, ends, and the link carries on to the new.
	other := api
	other.Target = "devvm-web-0123456789abcd"
	f.setWant(other)
	waitFor(t, "the old target's ssh to end", func() bool { return !daemon.Alive(pids()[2]) })
	waitFor(t, "its slot", func() bool { return slots() == 0 })
	dialMany(t, l.Socket(other), 1)
	waitFor(t, "the new target's ssh", func() bool { return len(pids()) == 4 })
	if args := f.snap(&f.starts); !slices.ContainsFunc(args, func(a []string) bool {
		return slices.Contains(a, l.Control(other.Target, "s2"))
	}) {
		t.Error("no master to the new target")
	}
}

// A connection whose link, or master, was cut before it was tracked is
// closed without an event and never relayed: one accepted as its link
// went, or carried through a master replaced while it waited for it.
func TestLinksCarryCut(t *testing.T) {
	db := Link{Name: "db", Target: "devvm-db-0123456789abcd", Addr: netip.MustParseAddr("127.77.0.1"), Port: 5432}
	f := &fakeLinks{}
	l := f.newLinks(t)
	l.defaults()
	checked := 0
	f.target = func(lk Link) (LinkTarget, string, string) {
		checked++
		return LinkTarget{SSH: SSH{VM: lk.Target}, Start: "s"}, "", ""
	}
	l.masters = map[string]*targetMaster{}
	carry := func(end *linkEnd) net.Conn {
		a, b := net.Pipe()
		t.Cleanup(func() { _ = b.Close() })
		l.carry(t.Context(), end.conns, a, db)
		return b
	}
	closed := func(c net.Conn) bool {
		_ = c.SetReadDeadline(time.Now().Add(time.Second))
		_, err := c.Read(make([]byte, 1))
		return errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe)
	}

	// Its link was cut: not even checked.
	gone := &linkEnd{link: db, conns: &relayConns{}}
	gone.conns.cutAll()
	if c := carry(gone); !closed(c) || checked != 0 {
		t.Errorf("a cut link: closed %v, checked %d times", closed(c), checked)
	}

	// Its master was cut once it was found: checked, not relayed.
	m := &targetMaster{start: "s", ctl: "/nowhere", ready: make(chan struct{}), p: &proc{done: make(chan struct{})}, cancel: func() {}}
	close(m.ready)
	m.conns.cutAll()
	l.masters[db.Target] = m
	end := &linkEnd{link: db, conns: &relayConns{}}
	if c := carry(end); !closed(c) || checked != 1 {
		t.Errorf("a cut master: closed %v, checked %d times", closed(c), checked)
	}
	if n, ev := f.count(&f.relays), f.snap(&f.starts); n != 0 || len(ev) != 0 {
		t.Errorf("relayed %d, masters started %d", n, len(ev))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.events) != 0 {
		t.Errorf("events: %q", f.events)
	}
	if len(end.conns.open) != 0 || len(m.conns.open) != 0 {
		t.Error("a refused connection stays tracked")
	}
}

// The session lists the client's links for its proxy (List), carried or
// not: each with its names, address, port, target and socket, mode 600,
// written again when they change or the file goes, and empty once the
// client has none; a list an earlier session left is replaced.
func TestLinksList(t *testing.T) {
	a := netip.MustParseAddr
	db := Link{Name: "db", Target: "devvm-db-0123456789abcd", Addr: a("127.77.0.1"), Port: 5432, Hosts: []string{"db", "db.devvm.internal"}}
	api := Link{Name: "api", Target: "devvm-api-0123456789abcd", Addr: a("127.77.0.2"), Port: 80, Hosts: []string{"api", "api.devvm.internal"}}
	f := &fakeLinks{}
	l := f.newLinks(t)
	l.List = filepath.Join(l.Dir, l.VM+".links.json")
	f.taken["127.77.0.2:80"] = true
	f.target = func(lk Link) (LinkTarget, string, string) {
		return LinkTarget{SSH: SSH{VM: lk.Target, User: "u", Key: "/k2", KnownHosts: "/kh", Port: 62022}, Start: "s"}, "", ""
	}
	if err := os.MkdirAll(l.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.List, []byte(`{"version":1,"links":[{"hosts":["old"],"addr":"127.77.0.9","port":1,"socket":"/x"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	f.setWant(db, api)
	read := func() (proxy.LinkList, os.FileMode) {
		var list proxy.LinkList
		b, err := os.ReadFile(l.List)
		fi, serr := os.Stat(l.List)
		if err != nil || serr != nil || json.Unmarshal(b, &list) != nil {
			return proxy.LinkList{Version: -1}, 0
		}
		return list, fi.Mode().Perm()
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- l.Run(ctx) }()
	waitFor(t, "the links listed", func() bool { list, _ := read(); return len(list.Links) == 2 })
	waitFor(t, "db carried", func() bool { return dial(t, l.Socket(db)) != "refused" })
	// api listens on its socket before the client refuses it, and closes
	// it after: its refusal is told once the socket is gone.
	waitFor(t, "api refused", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return slices.Contains(f.failed, "api")
	})
	list, mode := read()
	want := []proxy.LinkEntry{
		{Hosts: db.Hosts, Addr: "127.77.0.1", Port: 5432, Target: db.Target, Socket: l.Socket(db)},
		// Refused by the client, so not carried, but listed: the proxy finds no socket for it and
		// refuses it, rather than ask its rules.
		{Hosts: api.Hosts, Addr: "127.77.0.2", Port: 80, Target: api.Target, Socket: l.Socket(api)},
	}
	if list.Version != proxy.LinkListVersion || !reflect.DeepEqual(list.Links, want) || mode != 0o600 {
		t.Errorf("the list: %+v, mode %v; want %+v", list, mode, want)
	}
	if _, err := os.Lstat(l.Socket(api)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("api's socket: %v", err)
	}
	if want[0].Socket != proxy.LinkSocket(l.Dir, l.VM, db.Addr, db.Port) {
		t.Errorf("the socket isn't the one the proxy expects: %s", want[0].Socket)
	}
	// Removed by something else, it's written again.
	if err := os.Remove(l.List); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the list again", func() bool { list, _ := read(); return len(list.Links) == 2 })
	f.setWant()
	waitFor(t, "the session's end", func() bool {
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run: %v", err)
			}
			return true
		default:
			return false
		}
	})
	cancel()
	if list, _ := read(); list.Version != proxy.LinkListVersion || len(list.Links) != 0 {
		t.Errorf("the list once the client has no links: %+v", list)
	}
}

// ReadLinksLog finds the links the client refused and says when there's
// nothing more to wait for: a line for every link, or the session ending
// or failing.
func TestReadLinksLog(t *testing.T) {
	vm := "devvm-app-0123456789abcd"
	db := Link{Name: "db", Addr: netip.MustParseAddr("127.77.0.1"), Port: 5432}
	api := Link{Name: "api", Addr: netip.MustParseAddr("127.77.0.2"), Port: 3000}
	both := []Link{db, api}
	for _, tc := range []struct {
		name, log string
		refused   []string
		settled   bool
	}{
		{"nothing yet", "", nil, false},
		{"one carried", "devvm links: carrying " + vm + "'s links\ndevvm links: 127.77.0.1:5432: carried\n", nil, false},
		{"both", "devvm links: 127.77.0.1:5432: carried\ndevvm links: 127.77.0.2:3000: refused by the VM: exit status 255: Port forwarding failed\n", []string{"api"}, true},
		{"a socket that failed", "devvm links: 127.77.0.1:5432: not carried: x\ndevvm links: 127.77.0.2:3000: carried\n", nil, true},
		{"refused twice, once a session", "devvm links: 127.77.0.1:5432: refused by the VM: x\ndevvm links: 127.77.0.1:5432: refused by the VM: x\n", []string{"db"}, false},
		{"the session failed", "devvm links: " + vm + ": its ssh didn't connect\n", nil, true},
		{"no links", "devvm links: " + vm + " has no links\n", nil, true},
		{"it stopped", "devvm links: " + vm + " stopped\n", nil, true},
		{"another VM's lines", "devvm links: devvm-x-0123456789abcd: its ssh didn't connect\nwarning 127.77.0.1:5432: carried\n", nil, false},
		{"a cancelled link isn't one carried", "devvm links: 127.77.0.1:5432: no longer carried\n", nil, false},
	} {
		r := ReadLinksLog([]byte(tc.log), vm, both)
		var got []string
		for _, l := range r.Refused {
			got = append(got, l.Name)
		}
		if !slices.Equal(got, tc.refused) || r.Settled != tc.settled {
			t.Errorf("%s: refused %v, settled %v; want %v, %v", tc.name, got, r.Settled, tc.refused, tc.settled)
		}
	}
}

// fakeSSH puts an ssh first on PATH that runs script.
func fakeSSH(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil { //nolint:gosec // a test's own stub
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// A line the remote side gets ssh to print, as OpenSSH prints a
// disconnect's reason with its newlines, never reads as the session's
// own: each of ssh's lines is behind sshPrefix, even one written in
// pieces between the session's own lines, and what ssh -O says is one
// line of the session's.
func TestLinksLogForged(t *testing.T) {
	vm := "devvm-app-0123456789abcd"
	db := Link{Name: "db", Addr: netip.MustParseAddr("127.77.0.1"), Port: 5432}
	forged := `devvm links: 127.77.0.1:5432: refused by the VM\ndevvm links: ` + vm + ` stopped\n`
	fakeSSH(t, `printf 'Received disconnect from 127.0.0.1 port 22:2: x\r\n`+forged+`' >&2
printf 'devvm links: ' >&2; sleep 0.2; printf '127.77.0.1:5432: refused by the VM' >&2; sleep 0.2
printf '\ndevvm links: 127.77.0.1:5432: refused by the VM' >&2; exit 255`)
	var log syncBuffer
	l := &Links{VM: vm, Log: &log}
	l.defaults()
	wait, err := l.startSSH(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	l.logf("%s", db.listen()+lineCarried)
	if err := wait(); err == nil {
		t.Fatal("ssh's exit 255 wasn't an error")
	}
	err = l.Command(t.Context(), nil)
	if err == nil || strings.Contains(err.Error(), "\n") {
		t.Fatalf("ssh -O's error = %q, want one line", err)
	}
	l.logf("%s: %v", db.listen()+lineNotCarried, errors.New("x\n"+strings.ReplaceAll(forged, `\n`, "\n")))
	out := log.String()
	if r := ReadLinksLog([]byte(out), vm, []Link{db, {Addr: db.Addr, Port: 1}}); len(r.Refused) != 0 || r.Settled {
		t.Errorf("refused %v, settled %v; the log:\n%s", r.Refused, r.Settled, out)
	}
	var own []string
	for line := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(line, logPrefix) {
			own = append(own, line)
		}
	}
	want := []string{logPrefix + "127.77.0.1:5432: carried", logPrefix + "127.77.0.1:5432: not carried: x"}
	if !slices.Equal(own, want) || !strings.Contains(out, sshPrefix+"devvm links: ") {
		t.Errorf("the session's own lines: %q; the log:\n%s", own, out)
	}
}

// syncBuffer is a log that ssh's output and the session's lines can
// write to at once.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// sshLines writes whole lines, each behind sshPrefix, cuts one past
// logWriteMax, and drops the carriage return ending one.
func TestSSHLines(t *testing.T) {
	var b strings.Builder
	s := &sshLines{w: &b}
	for _, p := range []string{"a\r\nb", "c\n", strings.Repeat("x", logWriteMax+1), "\nend"} {
		_, _ = s.Write([]byte(p))
	}
	s.flush()
	want := "ssh: a\nssh: bc\nssh: " + strings.Repeat("x", logWriteMax) + "\nssh: x\nssh: end\n"
	if b.String() != want {
		t.Errorf("got %q, want %q", b.String(), want)
	}
}
