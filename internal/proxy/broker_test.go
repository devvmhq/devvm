package proxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/devvmhq/devvm/internal/daemon"
	"github.com/devvmhq/devvm/internal/memnet"
	"github.com/devvmhq/devvm/internal/redact"
	"github.com/devvmhq/devvm/internal/vmname"
)

// fakeChild is a broker's child in a synctest bubble: it ends once ended is called, which its
// Kill does at once and its Stop after stopIn (a child that ignores SIGTERM: longer than the
// broker's wait), recording each.
type fakeChild struct {
	mu      sync.Mutex
	done    chan struct{}
	once    sync.Once
	stopIn  time.Duration
	stopped []time.Duration
	killed  bool
}

func newFakeChild() *fakeChild { return &fakeChild{done: make(chan struct{})} }

func (c *fakeChild) PID() int              { return 42 }
func (c *fakeChild) Done() <-chan struct{} { return c.done }
func (c *fakeChild) end()                  { c.once.Do(func() { close(c.done) }) }
func (c *fakeChild) Exit() string          { return "signal: killed" }
func (c *fakeChild) ExitCode() int         { return -1 }

func (c *fakeChild) ExitSignal() syscall.Signal { return syscall.SIGKILL }

func (c *fakeChild) Kill() {
	c.mu.Lock()
	c.killed = true
	c.mu.Unlock()
	c.end()
}

func (c *fakeChild) Stop(wait time.Duration) bool {
	c.mu.Lock()
	c.stopped = append(c.stopped, wait)
	in := c.stopIn
	c.mu.Unlock()
	t := time.NewTimer(in)
	defer t.Stop()
	select {
	case <-c.done:
	case <-t.C:
		if in <= wait {
			c.end()
		} else {
			c.Kill() // its SIGKILL, past wait
		}
	}
	<-c.done
	return true
}

// memPair is a broker's pair whose socket is in memory, and the child's end.
func memPair(child childProc) (pair, *frameConn) {
	a, b := memnet.Pipe()
	return pair{fc: newFrameConn(a), child: child}, newFrameConn(b)
}

// testBroker is a broker of an empty config, logging to l, which connects nowhere and looks up
// nothing.
func testBroker(t *testing.T) *broker {
	t.Helper()
	b, err := newBroker(t.Context(), Config{VM: "devvm-x", Password: redact.NewSecret("pw")}, IO{}, quiet())
	must(t, err)
	var d dialRecorder
	b.net.own, b.net.connect = hostOwn(), d.connect
	b.net.lookup = func(context.Context, string, string) ([]netip.Addr, error) { return nil, errors.New("no lookups") }
	return b
}

// The start: the child's ready, of the protocol's version and the broker's executable, is the
// address it serves; a failed child, one of another build, one that answers anything else, one
// that ends and one that says nothing within startWait, to the moment, fail the start.
func TestHandshake(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := testBroker(t)
		for _, tc := range []struct {
			name   string
			answer func(c *frameConn, child *fakeChild)
			ok     bool
			took   time.Duration
			why    string
		}{
			{"ready", func(c *frameConn, _ *fakeChild) {
				_ = c.send(toBroker{Type: frameReady, Ready: &wireReady{Version: protocolVersion, Exe: "e", Addr: "127.0.0.1:1", Confined: "none"}}, nil)
			}, true, 0, ""},
			{"failed", func(c *frameConn, _ *fakeChild) {
				_ = c.send(toBroker{Type: frameFailed, Failed: &wireFailed{Why: "no addresses"}}, nil)
			}, false, 0, "the child failed: no addresses"},
			{"another build", func(c *frameConn, _ *fakeChild) {
				_ = c.send(toBroker{Type: frameReady, Ready: &wireReady{Version: protocolVersion, Exe: "other"}}, nil)
			}, false, 0, "another build"},
			{"another version", func(c *frameConn, _ *fakeChild) {
				_ = c.send(toBroker{Type: frameReady, Ready: &wireReady{Version: 2, Exe: "e"}}, nil)
			}, false, 0, "another version"},
			{"a request first", func(c *frameConn, _ *fakeChild) {
				_ = c.send(toBroker{Type: frameValue, Value: &wireValueKey{}}, nil)
			}, false, 0, "neither ready nor failed"},
			{"ended", func(c *frameConn, child *fakeChild) { _ = c.close(); child.end() }, false, 0, "ended as it started"},
			{"silent", func(*frameConn, *fakeChild) {}, false, startWait, "within 4s"},
		} {
			child := newFakeChild()
			p, c := memPair(child)
			go func() {
				var f toChild
				if _, err := c.recv(&f); err != nil || f.Start == nil || f.Start.Exe != "e" {
					return
				}
				tc.answer(c, child)
			}()
			start := time.Now()
			ready, err := b.handshake(p, "e", unconfined(WhyUnsupported), start.Add(startWait))
			if took := time.Since(start); took != tc.took {
				t.Errorf("%s: took %v, want %v", tc.name, took, tc.took)
			}
			if (err == nil) != tc.ok || err != nil && !strings.Contains(err.Error(), tc.why) {
				t.Errorf("%s: %v", tc.name, err)
			}
			if tc.ok && ready.Addr != "127.0.0.1:1" {
				t.Errorf("%s: ready %+v", tc.name, ready)
			}
			_ = p.fc.close()
			_ = c.close()
			child.end()
			synctest.Wait()
		}
	})
}

// A child that ends with the start unread, as one that aborts as it loads does, ended early,
// whether the system reads its end as EOF or, as Linux does, as a reset of the socket; and both
// count as the socket's end.
func TestHandshakeChildGone(t *testing.T) {
	t.Parallel()
	b := testBroker(t)
	bfc, cfc := pairOf(t)
	go func() {
		// Wait, without reading, until the start is there, then close: the start goes unread.
		rc, err := cfc.c.(*net.UnixConn).SyscallConn()
		if err != nil {
			return
		}
		buf := make([]byte, 1)
		_ = rc.Read(func(fd uintptr) bool {
			_, _, err := syscall.Recvfrom(int(fd), buf, syscall.MSG_PEEK)
			return !errors.Is(err, syscall.EAGAIN)
		})
		_ = cfc.close()
	}()
	_, err := b.handshake(pair{fc: bfc, child: newEndedChild(-6)}, "e", unconfined(WhyUnsupported), time.Now().Add(testStartWait))
	if !errors.Is(err, errEndedEarly) {
		t.Errorf("a child gone with the start unread: %v", err)
	}
	for _, err := range []error{io.EOF, io.ErrUnexpectedEOF, syscall.ECONNRESET, fmt.Errorf("read: %w", syscall.ECONNRESET)} {
		if !socketEnded(err) {
			t.Errorf("%v: not the socket's end", err)
		}
	}
	if socketEnded(syscall.EPIPE) || socketEnded(errors.New("x")) {
		t.Error("another error taken for the socket's end")
	}
}

// The pair's end: the lifeline's closes the socket and waits for the child, killing it only
// closeWait+stopWait later; devvm stop's stops the child within stopWait; a child that ends ends
// the broker; and a frame that breaks the rules stops the child and names the rule.
func TestLive(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		b := testBroker(t)
		// The lifeline: a child that ends 2 seconds after its socket closes.
		child := newFakeChild()
		p, c := memPair(child)
		lifeline := make(chan struct{})
		p.lifeline = lifeline
		go func() {
			var f toBroker
			_, _ = c.recv(&f) // the socket's end
			time.Sleep(2 * time.Second)
			child.end()
		}()
		done := make(chan error)
		go func() { done <- b.live(t.Context(), p, nil) }()
		synctest.Wait()
		start := time.Now()
		close(lifeline)
		if err := <-done; err != nil || time.Since(start) != 2*time.Second || child.killed {
			t.Errorf("the lifeline: %v after %v, killed %v", err, time.Since(start), child.killed)
		}
		// A child that ignores its socket's end is killed closeWait+stopWait later.
		child = newFakeChild()
		p, c = memPair(child)
		lifeline = make(chan struct{})
		p.lifeline = lifeline
		go func() { done <- b.live(t.Context(), p, nil) }()
		synctest.Wait()
		start = time.Now()
		close(lifeline)
		if err := <-done; err != nil || time.Since(start) != closeWait+stopWait || !child.killed {
			t.Errorf("a stubborn child: %v after %v, killed %v", err, time.Since(start), child.killed)
		}
		_ = c.close()
		// devvm stop: the child is stopped, given stopWait.
		child = newFakeChild()
		child.stopIn = 100 * time.Millisecond
		p, c = memPair(child)
		stop := make(chan struct{})
		go func() { done <- b.live(t.Context(), p, stop) }()
		synctest.Wait()
		close(stop)
		if err := <-done; err != nil || len(child.stopped) != 1 || child.stopped[0] != stopWait {
			t.Errorf("stop: %v, stopped %v", err, child.stopped)
		}
		_ = c.close()
		// A child that ends.
		child = newFakeChild()
		p, c = memPair(child)
		go func() { done <- b.live(t.Context(), p, nil) }()
		synctest.Wait()
		child.end()
		if err := <-done; err == nil || !strings.Contains(err.Error(), "the child ended (signal: killed)") {
			t.Errorf("the child ended: %v", err)
		}
		_ = c.close()
		// A child that closes its end of the socket is stopped, and ends the broker.
		child = newFakeChild()
		p, c = memPair(child)
		go func() { done <- b.live(t.Context(), p, nil) }()
		synctest.Wait()
		_ = c.close()
		if err := <-done; err == nil || !strings.Contains(err.Error(), "the child ended") || len(child.stopped) != 1 {
			t.Errorf("the child's end closed: %v, stopped %v", err, child.stopped)
		}
		// A frame that breaks the rules: a second ready.
		child = newFakeChild()
		p, c = memPair(child)
		go func() { done <- b.live(t.Context(), p, nil) }()
		_ = c.send(toBroker{Type: frameReady, Ready: &wireReady{}}, nil)
		if err := <-done; !isRuleError(err) || len(child.stopped) != 1 {
			t.Errorf("a second ready: %v, stopped %v", err, child.stopped)
		}
		_ = c.close()
		synctest.Wait()
	})
}

// The broker answers at most one value at once, as lookups go one at a time: a request past its
// type's cap gets an error at once, never a wait.
func TestServeChildCaps(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		k, _ := testLookups(t, Binding{Name: "gh", Kind: KindSecret})
		b := testBroker(t)
		b.lookups = k
		release := make(chan struct{})
		k.lookup = func(context.Context, Binding) (redact.Secret, error) { <-release; return redact.NewSecret("v"), nil }
		a, c := memnet.Pipe()
		bfc, cfc := newFrameConn(a), newFrameConn(c)
		served := make(chan error)
		go func() { served <- b.serveChild(t.Context(), bfc) }()
		for id := range uint64(2) {
			must(t, cfc.send(toBroker{Type: frameValue, ID: id + 1, Value: &wireValueKey{Kind: KindSecret, Name: "gh"}}, nil))
		}
		var f toChild
		if _, err := cfc.recv(&f); err != nil || f.ID != 2 || f.Error == "" {
			t.Errorf("the second value: %+v, %v", f, err)
		}
		close(release)
		if _, err := cfc.recv(&f); err != nil || f.ID != 1 || f.Value == nil || f.Value.Value != "v" {
			t.Errorf("the first value: %+v, %v", f, err)
		}
		_ = cfc.close()
		if err := <-served; !errors.Is(err, io.EOF) {
			t.Errorf("serveChild: %v", err)
		}
		synctest.Wait()
	})
}

// The broker gives a request's place back as its reply starts to go, so a child that keeps the
// caps, sending its next value as the last one's reply comes, never gets busy; a reply that waits
// for the socket holds its place, so a child that doesn't read gets busy past the cap.
func TestServeChildFreesSlotsAsRepliesGo(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		k, _ := testLookups(t, Binding{Name: "gh", Kind: KindSecret})
		b := testBroker(t)
		b.lookups = k
		// Past what the pipe holds, so a reply waits for the child's reads.
		big := strings.Repeat("v", 1<<20)
		k.lookup = func(context.Context, Binding) (redact.Secret, error) { return redact.NewSecret(big), nil }
		a, c := memnet.Pipe()
		bfc, cfc := newFrameConn(a), newFrameConn(c)
		served := make(chan error)
		go func() { served <- b.serveChild(t.Context(), bfc) }()
		ask := func(id uint64) {
			must(t, cfc.send(toBroker{Type: frameValue, ID: id, Value: &wireValueKey{Kind: KindSecret, Name: "gh"}}, nil))
		}
		ask(1)
		// The first reply has begun: the child reads its length, as its reply comes.
		var hdr [4]byte
		_, err := io.ReadFull(c, hdr[:])
		must(t, err)
		// serveChild takes each request's place as it reads it, in order: the second finds the
		// first's free, and its reply waits for the socket, holding its place, so a third is past
		// the cap. (No synctest.Wait here: a reply waiting for the socket waits on a mutex.)
		ask(2)
		ask(3)
		_, err = io.ReadFull(c, make([]byte, binary.BigEndian.Uint32(hdr[:])))
		must(t, err)
		got := map[uint64]string{}
		for range 2 {
			var f toChild
			if _, err := cfc.recv(&f); err != nil {
				t.Fatal(err)
			}
			got[f.ID] = f.Error
			if f.Error == "" && (f.Value == nil || f.Value.Value != big) {
				t.Errorf("value %d: not the value", f.ID)
			}
		}
		if !maps.Equal(got, map[uint64]string{2: "", 3: codeBusy}) {
			t.Errorf("the replies' errors: %v", got)
		}
		_ = cfc.close()
		if err := <-served; !errors.Is(err, io.EOF) {
			t.Errorf("serveChild: %v", err)
		}
		synctest.Wait()
	})
}

// Every request names something in the broker's own config: a value of its bindings file alone,
// a file of its list alone, a link of its links' list alone, connected to that VM's link's socket,
// and a token exchange at its own endpoints.
func TestBrokerNamesItsOwn(t *testing.T) {
	t.Parallel()
	dir := shortAgentDir(t)
	vm := vmname.VM("devvm-x-0123456789abcd")
	addr := LinkAddr(1)
	sock := LinkSocket(dir, vm, addr, 5432)
	ln, err := new(net.ListenConfig).Listen(t.Context(), "unix", sock)
	must(t, err)
	defer ln.Close()
	other := LinkSocket(dir, "devvm-y-0123456789abcd", LinkAddr(2), 5433)
	oln, err := new(net.ListenConfig).Listen(t.Context(), "unix", other)
	must(t, err)
	defer oln.Close()
	list := filepath.Join(dir, "links.json")
	must(t, os.WriteFile(list, []byte(`{"version":1,"links":[`+
		`{"hosts":["db"],"addr":"`+addr.String()+`","port":5432,"target":"devvm-t","socket":"`+sock+`"},`+
		`{"hosts":["other"],"addr":"127.77.0.2","port":5433,"target":"devvm-t","socket":"`+other+`"}]}`), 0o600))
	bindings := filepath.Join(dir, "bindings.json")
	writeBindings(t, bindings, Binding{Name: "gh", Kind: KindSecret, Hosts: []string{"api.github.com"}, Header: "Authorization", Placeholder: "PH"})
	b, err := newBroker(t.Context(), Config{VM: vm, Password: redact.NewSecret("pw"), Secrets: bindings, Links: list, LinkSockets: dir},
		IO{Secret: func(context.Context, Binding) (redact.Secret, error) { return redact.NewSecret("v"), nil }}, quiet())
	must(t, err)
	if _, _, err := b.value(t.Context(), valueKey{KindSecret, "", "gh"}); err != nil {
		t.Errorf("its own binding: %v", err)
	}
	for _, k := range []valueKey{{KindSecret, "", "other"}, {KindLogin, "codex", "gh"}, {KindSecret, "", UpstreamSecret}} {
		if _, _, err := b.value(t.Context(), k); err == nil {
			t.Errorf("%+v: a value", k)
		}
	}
	if got, err := b.file(0); err != nil || !strings.Contains(string(got.data), `"gh"`) {
		t.Errorf("file 0, its bindings: %q, %v", got.data, err)
	}
	for _, n := range []int{-1, 2, 100} {
		if _, err := b.file(n); err == nil {
			t.Errorf("file %d: read", n)
		}
	}
	c, err := b.link(t.Context(), "db", 5432)
	if err != nil {
		t.Fatalf("its own link: %v", err)
	}
	_ = c.Close()
	for _, hp := range []struct {
		host string
		port int
	}{{"other", 5433}, {"db", 5433}, {"nope", 5432}} {
		if c, err := b.link(t.Context(), hp.host, hp.port); err == nil {
			_ = c.Close()
			t.Errorf("%s:%d: connected", hp.host, hp.port)
		}
	}
	if _, err := b.token(t.Context(), tokenRequest{host: "auth.test", path: "/oauth/token", method: "POST"}); err == nil {
		t.Error("a token exchange without endpoints")
	}
}

// The child's side, over a real socketpair: values, files, a link's connection as the descriptor
// the broker passes, and a token exchange; once the broker's end closes, every request fails at
// once.
func TestBrokerClient(t *testing.T) {
	t.Parallel()
	dir := shortAgentDir(t)
	vm := vmname.VM("devvm-x-0123456789abcd")
	addr := LinkAddr(1)
	sock := LinkSocket(dir, vm, addr, 5432)
	ln, err := new(net.ListenConfig).Listen(t.Context(), "unix", sock)
	must(t, err)
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("hello from the link"))
			_ = c.Close()
		}
	}()
	list := filepath.Join(dir, "links.json")
	must(t, os.WriteFile(list, []byte(`{"version":1,"links":[{"hosts":["db"],"addr":"`+addr.String()+`","port":5432,"target":"devvm-t","socket":"`+sock+`"}]}`), 0o600))
	bindings := filepath.Join(dir, "bindings.json")
	writeBindings(t, bindings, Binding{Name: "gh", Kind: KindSecret, Hosts: []string{"api.github.com"}, Header: "Authorization", Placeholder: "PH"})
	if _, err := newBroker(t.Context(), Config{VM: vm, Password: redact.NewSecret("pw"), CA: filepath.Join(dir, "x"), CAKey: filepath.Join(dir, "y")}, IO{}, quiet()); err == nil {
		t.Fatal("a CA that isn't there: no error")
	}
	b, err := newBroker(t.Context(), Config{
		VM: vm, Password: redact.NewSecret("pw"), Secrets: bindings, Links: list, LinkSockets: dir,
		Captures: []Capture{{Agent: "codex", Host: "auth.test", Path: "/oauth/token"}},
	}, IO{
		Secret:  func(context.Context, Binding) (redact.Secret, error) { return redact.NewSecret("v"), nil },
		Capture: capturerFunc(func(Exchange) ([]byte, []byte, error) { return nil, []byte(`{"ok":1}`), nil }),
	}, quiet())
	must(t, err)
	bfc, cfc := pairOf(t)
	served := make(chan error, 1)
	go func() { served <- b.serveChild(t.Context(), bfc) }()
	client := newBrokerClient(cfc)
	read := make(chan error, 1)
	go func() { read <- client.read() }()
	if v, at, err := client.value(t.Context(), valueKey{KindSecret, "", "gh"}); err != nil || v.Reveal() != "v" || at.IsZero() {
		t.Errorf("value: %q, %v, %v", v.Reveal(), at, err)
	}
	if _, _, err := client.value(t.Context(), valueKey{KindSecret, "", "nope"}); err == nil {
		t.Error("a value of no binding")
	}
	if f, err := client.file(0); err != nil || !strings.Contains(string(f.data), `"gh"`) || f.st.size != int64(len(f.data)) || f.st.ino == 0 {
		t.Errorf("file: %+v, %v", f, err)
	}
	// On macOS, a socket in transit over SCM_RIGHTS can lose its data when another test's
	// goroutine closes a unix socket that still holds descriptors (design.md, Things learned),
	// so a link that reads empty is asked for once more.
	var got []byte
	for range 2 {
		conn, err := client.link(t.Context(), "db", 5432)
		if err != nil {
			t.Fatalf("link: %v", err)
		}
		got, _ = io.ReadAll(conn)
		_ = conn.Close()
		if len(got) > 0 {
			break
		}
	}
	if string(got) != "hello from the link" {
		t.Errorf("the link's connection reads %q", got)
	}
	if _, err := client.link(t.Context(), "other", 5432); err == nil {
		t.Error("a link that isn't the VM's")
	}
	// A link the child no longer wants is closed as it arrives.
	gone, cancel := context.WithCancel(t.Context())
	cancel()
	if c, err := client.link(gone, "db", 5432); err == nil {
		_ = c.Close()
		t.Error("a link whose request ended: kept")
	}
	if _, err := client.token(t.Context(), tokenRequest{host: "other.test", path: "/oauth/token", method: "POST"}); err == nil {
		t.Error("a token exchange at no endpoint of the VM's")
	}
	if r, err := client.token(t.Context(), tokenRequest{host: "auth.test", path: "/oauth/token", method: "POST", header: map[string][]string{}}); err != nil ||
		r.status != 200 || string(r.body) != `{"ok":1}` || r.sent {
		t.Errorf("token: %+v, %v", r, err)
	}
	_ = bfc.close()
	if err := <-read; isRuleError(err) {
		t.Errorf("the client's read: %v", err)
	}
	if _, err := client.file(0); !errors.Is(err, errBrokerGone) {
		t.Errorf("once the broker is gone: %v", err)
	}
	if err := <-served; isRuleError(err) {
		t.Errorf("serveChild: %v", err)
	}
}

// A request the child gives up on, its request's context ended, returns at once; its reply, when
// it comes, is dropped, its slot given back only then, and the broker's errors reach the child as
// codes alone, the text in the broker's own log.
func TestBrokerClientGivesUp(t *testing.T) {
	t.Parallel()
	k, _ := testLookups(t, Binding{Name: "gh", Kind: KindSecret})
	var logged syncBuffer
	b := testBroker(t)
	b.log, b.lookups = log.New(&logged, "", 0), k
	release := make(chan struct{})
	k.lookup = func(context.Context, Binding) (redact.Secret, error) { <-release; return redact.NewSecret("v"), nil }
	bfc, cfc := pairOf(t)
	served := make(chan error, 1)
	go func() { served <- b.serveChild(t.Context(), bfc) }()
	client := newBrokerClient(cfc)
	read := make(chan error, 1)
	go func() { read <- client.read() }()
	gone, cancel := context.WithCancel(t.Context())
	cancel()
	key := valueKey{KindSecret, "", "gh"}
	if _, _, err := client.value(gone, key); !errors.Is(err, errGaveUp) {
		t.Errorf("a request whose context ended: %v", err)
	}
	ctx, stop := context.WithCancel(t.Context())
	go func() { time.Sleep(50 * time.Millisecond); stop() }()
	if _, _, err := client.value(ctx, key); !errors.Is(err, errGaveUp) {
		t.Errorf("a request given up on: %v", err)
	}
	close(release)
	// The late reply was dropped, and its slot is back: the next value comes.
	if v, _, err := client.value(t.Context(), key); err != nil || v.Reveal() != "v" {
		t.Errorf("after a dropped reply: %q, %v", v.Reveal(), err)
	}
	_, _, err := client.value(t.Context(), valueKey{KindSecret, "", "secret-xx"})
	if err == nil || !strings.Contains(err.Error(), codeNotFound) || strings.Contains(err.Error(), "secret-xx") {
		t.Errorf("a key of no binding: %v (a code alone)", err)
	}
	if !strings.Contains(logged.String(), "secret-xx") {
		t.Errorf("the broker's log: %q", logged.String())
	}
	_ = bfc.close()
	if err := <-read; isRuleError(err) {
		t.Errorf("the client's read: %v", err)
	}
	if err := <-served; isRuleError(err) {
		t.Errorf("serveChild: %v", err)
	}
}

// A child whose broker sent a second start, or a reply to no request, ends the pair; a
// descriptor on a reply that isn't a link's carrying one is closed, never taken.
func TestBrokerClientRules(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		frame toChild
	}{
		{"a second start", toChild{Type: frameStart, Start: &wireStart{}}},
		{"a reply to no request", toChild{Type: frameValue, ID: 99, Value: &wireValue{}}},
	} {
		bfc, cfc := pairOf(t)
		client := newBrokerClient(cfc)
		must(t, bfc.send(tc.frame, nil))
		if err := client.read(); !isRuleError(err) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}

// The child: a start from another build gets failed, and a first frame that isn't start ends it;
// a ready child serves until its broker's end closes, then closes its listener and returns.
func TestRunChildOn(t *testing.T) {
	t.Parallel()
	listen := func() net.Listener {
		ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
		must(t, err)
		return ln
	}
	std := IO{Stdout: io.Discard, Stderr: io.Discard, confine: noConfine}
	start := childStart{vm: "devvm-x", password: redact.NewSecret("pw"), exe: "built", spawned: unconfined(WhyUnsupported)}
	// No confinement: failed, as confineChild would confine the test's own process.
	bfc, cfc := pairOf(t)
	must(t, bfc.send(toChild{Type: frameStart, Start: start.wire()}, nil))
	if err := runChildOn(cfc, listen(), IO{Stdout: io.Discard, Stderr: io.Discard}, "built", "", quiet()); err == nil {
		t.Error("no confinement: no error")
	}
	var f toBroker
	if _, err := bfc.recv(&f); err != nil || f.Type != frameFailed || !strings.Contains(f.Failed.Why, "confinement") {
		t.Errorf("no confinement: %+v, %v", f, err)
	}
	// Another build's broker.
	bfc, cfc = pairOf(t)
	must(t, bfc.send(toChild{Type: frameStart, Start: start.wire()}, nil))
	if err := runChildOn(cfc, listen(), std, "other", "", quiet()); err == nil {
		t.Error("another build: no error")
	}
	if _, err := bfc.recv(&f); err != nil || f.Type != frameFailed || !strings.Contains(f.Failed.Why, "another build") {
		t.Errorf("another build: %+v, %v", f, err)
	}
	// Not start.
	bfc, cfc = pairOf(t)
	must(t, bfc.send(toChild{Type: frameValue, ID: 1, Value: &wireValue{}}, nil))
	if err := runChildOn(cfc, listen(), std, "built", "", quiet()); !isRuleError(err) {
		t.Errorf("a first frame that isn't start: %v", err)
	}
	// Ready, serving, then ended by the broker's end.
	bfc, cfc = pairOf(t)
	ln := listen()
	must(t, bfc.send(toChild{Type: frameStart, Start: start.wire()}, nil))
	done := make(chan error, 1)
	go func() { done <- runChildOn(cfc, ln, std, "built", "", quiet()) }()
	if _, err := bfc.recv(&f); err != nil || f.Type != frameReady || f.Ready.Addr != ln.Addr().String() || f.Ready.Confined != "none unsupported" || f.Ready.Exe != "built" {
		t.Fatalf("ready: %+v, %v", f, err)
	}
	c, err := new(net.Dialer).DialContext(t.Context(), "tcp", ln.Addr().String())
	must(t, err)
	_ = c.Close()
	_ = bfc.close()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("runChildOn: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the child outlived its broker's end")
	}
	if c, err := new(net.Dialer).DialContext(t.Context(), "tcp", ln.Addr().String()); err == nil {
		_ = c.Close()
		t.Error("its listener still accepts")
	}
}

// The child takes its descriptors only when they're the broker's sockets: a unix socket for the
// broker's, an IP one for the listener, and refuses a pipe, a descriptor that isn't open and the
// wrong kind.
func TestInheritedSocket(t *testing.T) {
	t.Parallel()
	fds, err := socketpair()
	must(t, err)
	unixEnd, other := os.NewFile(uintptr(fds[0]), "a"), os.NewFile(uintptr(fds[1]), "b")
	defer unixEnd.Close()
	defer other.Close()
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	must(t, err)
	defer ln.Close()
	lnFile, err := ln.(*net.TCPListener).File()
	must(t, err)
	defer lnFile.Close()
	r, w, err := os.Pipe()
	must(t, err)
	defer r.Close()
	defer w.Close()
	for _, tc := range []struct {
		name string
		fd   int
		unix bool
		ok   bool
	}{
		{"the broker's socket", int(unixEnd.Fd()), true, true},
		{"the listener", int(lnFile.Fd()), false, true},
		{"a unix socket for the listener", int(unixEnd.Fd()), false, false},
		{"the listener for the broker's socket", int(lnFile.Fd()), true, false},
		{"a pipe", int(r.Fd()), true, false},
		{"no descriptor", -1, false, false},
	} {
		if err := inheritedSocket(tc.fd, tc.unix); (err == nil) != tc.ok {
			t.Errorf("%s: %v, want it taken: %v", tc.name, err, tc.ok)
		}
	}
}

// devvm _proxy-child: -help prints its one flag, -for, which it needs; with its descriptors, it
// answers its broker's start ready, writes its own lines in the proxy's log through the file it
// was given, and returns once its broker's end closes. IsChild tells it apart by its arguments.
func TestChildMain(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if err := ChildMain([]string{"-help"}, IO{Stdout: &out}); err != nil || !strings.Contains(out.String(), "-for") {
		t.Errorf("-help: %v, %q", err, out.String())
	}
	for _, args := range [][]string{{}, {"-for", "/c", "extra"}, {"-nope"}} {
		if err := ChildMain(args, IO{Stdout: io.Discard}); err == nil {
			t.Errorf("%q: no error", args)
		}
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "proxy.log")
	logFile, err := OpenLog(logPath)
	must(t, err)
	defer logFile.Close()
	fds, err := socketpair()
	must(t, err)
	sock, brokerEnd := os.NewFile(uintptr(fds[1]), "child"), os.NewFile(uintptr(fds[0]), "broker")
	bconn, err := net.FileConn(brokerEnd)
	must(t, err)
	_ = brokerEnd.Close()
	bfc := newFrameConn(bconn)
	defer bfc.close()
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	must(t, err)
	lnFile, err := ln.(*net.TCPListener).File()
	must(t, err)
	_ = ln.Close()
	exe, err := selfSum()
	must(t, err)
	// A confinement of its own: confineChild would confine the test's process on Linux.
	std := IO{Stdout: io.Discard, Stderr: logFile, ErrFile: logFile, inherited: func() (*os.File, *os.File) { return sock, lnFile }, confine: noConfine}
	done := make(chan error, 1)
	go func() { done <- ChildMain([]string{"-for", filepath.Join(dir, "proxy.json")}, std) }()
	start := childStart{exe: exe, spawned: unconfined(WhyUnsupported), vm: "devvm-x", password: redact.NewSecret("pw"), proxyLog: logPath, files: []watchedFile{{watchPolicy, filepath.Join(dir, "policy")}}}
	must(t, bfc.send(toChild{Type: frameStart, Start: start.wire()}, nil))
	var f toBroker
	if _, err := bfc.recv(&f); err != nil || f.Type != frameReady || f.Ready.Exe != exe {
		t.Fatalf("ready: %+v, %v", f, err)
	}
	// Its policy file is gone: it looks, and asks nothing.
	_ = bfc.close()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("ChildMain: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the child outlived its broker's end")
	}
	if !IsChild([]string{"/bin/devvm", ChildCommand, "-for", "/c"}, "/c") || IsChild([]string{"/bin/devvm", "_proxy", "-config", "/c"}, "/c") {
		t.Error("IsChild")
	}
}

// A start the child can't run with is refused: of another version, without the VM's password, or
// with a watched file of a kind it doesn't know, or a second bindings file or links' list.
func TestStartOfRefuses(t *testing.T) {
	t.Parallel()
	ok := childStart{spawned: unconfined(WhyUnsupported), vm: "devvm-x", password: redact.NewSecret("pw"), files: []watchedFile{{watchPolicy, "/p"}, {watchDeny, "/d"}, {watchLinks, "/l"}}}
	if _, err := startOf(ok.wire()); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(w *wireStart){
		"another version":     func(w *wireStart) { w.Version = 2 },
		"no password":         func(w *wireStart) { w.Password = "" },
		"an unknown kind":     func(w *wireStart) { w.Files = append(w.Files, wireWatch{Kind: "other", Path: "/o"}) },
		"a second links list": func(w *wireStart) { w.Files = append(w.Files, wireWatch{Kind: watchLinks, Path: "/m"}) },
	} {
		w := ok.wire()
		change(w)
		if _, err := startOf(w); err == nil {
			t.Errorf("%s: taken", name)
		}
	}
	if quoteType("secret-xx") != "another" || quoteType(frameLink) != frameLink {
		t.Error("quoteType")
	}
}

// A frame past wireMax is never sent, nor a descriptor over a connection that isn't a unix
// socket's.
func TestFrameSendRefuses(t *testing.T) {
	t.Parallel()
	a, b := memnet.Pipe()
	defer a.Close()
	defer b.Close()
	f := newFrameConn(a)
	if err := f.send(toChild{Type: frameFile, File: &wireFile{Data: make([]byte, wireMax)}}, nil); err == nil {
		t.Error("a frame past wireMax was sent")
	}
	r, w, err := os.Pipe()
	must(t, err)
	defer r.Close()
	defer w.Close()
	if err := f.send(toChild{Type: frameLink, Link: &wireLink{FD: true}}, r); err == nil {
		t.Error("a descriptor over memory was sent")
	}
}

// An endpoint that can't be reached is refused, its error in the proxy's own log alone; a link
// whose request the child no longer wants is closed as it arrives.
func TestExchangeUnreachable(t *testing.T) {
	t.Parallel()
	var logged syncBuffer
	x := &exchanger{
		vm: "devvm-x", list: []Capture{{Agent: "codex", Host: "auth.test", Path: "/oauth/token"}}, log: log.New(&logged, "", 0), route: direct,
		c:    capturerFunc(func(Exchange) ([]byte, []byte, error) { return []byte("x"), nil, nil }),
		dial: func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("refused at 10.0.0.9") },
	}
	_, err := x.exchange(t.Context(), tokenRequest{host: "auth.test", path: "/oauth/token", method: "POST", header: map[string][]string{}})
	if err == nil || strings.Contains(err.Error(), "10.0.0.9") || !strings.Contains(logged.String(), "10.0.0.9") {
		t.Errorf("unreachable: %v, log %q", err, logged.String())
	}
	if _, err := x.exchange(t.Context(), tokenRequest{host: "auth.test", path: "/other", method: "POST"}); err == nil {
		t.Error("another path: exchanged")
	}
}

// What the broker can't start with is an error before any child runs: a CA, the user's CAs or
// the company proxy's password it can't read, a listener that isn't TCP's, and a child that
// doesn't start.
func TestBrokerStartRefuses(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeCA(t, dir)
	ca, key := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca.key")
	for name, c := range map[string]Config{
		"a next CA that isn't there": {CA: ca, CAKey: key, CANext: filepath.Join(dir, "none.pem"), CANextKey: key},
		"user CAs that aren't PEM":   {UpstreamCAs: "not PEM"},
		"a proxy secret, no lookup":  {Upstream: &Upstream{HTTPS: "http://u@corp:3128", Secret: UpstreamSecret}},
		"a company proxy's bad URL":  {Upstream: &Upstream{HTTPS: "socks5://corp:1080"}},
	} {
		c.VM, c.Password = "devvm-x", redact.NewSecret("pw")
		if _, err := newBroker(t.Context(), c, IO{}, quiet()); err == nil {
			t.Errorf("%s: a broker", name)
		}
	}
	dirU := shortAgentDir(t)
	uln, err := new(net.ListenConfig).Listen(t.Context(), "unix", filepath.Join(dirU, "s"))
	must(t, err)
	defer uln.Close()
	if _, err := spawnChild(uln, filepath.Join(dir, "proxy.json"), IO{}, nil); err == nil {
		t.Error("a unix listener: a child")
	}
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	must(t, err)
	defer ln.Close()
	failing := IO{spawn: func(daemon.Cmd) (childProc, error) { return nil, errors.New("no such program") }}
	if _, err := spawnChild(ln, filepath.Join(dir, "proxy.json"), failing, nil); err == nil {
		t.Error("a child that doesn't start: started")
	}
}

// A start the child can't make its server of gets failed: an event log it can't open, a CA or
// user CAs that don't parse, a company proxy it can't take, a built-in policy devvm's files lack,
// and a Nix cache with a negative cap.
func TestChildServerRefuses(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base := childStart{exe: "e", spawned: unconfined(WhyUnsupported), vm: "devvm-x", password: redact.NewSecret("pw")}
	for name, change := range map[string]func(s *childStart){
		"an event log it can't open": func(s *childStart) { s.log = filepath.Join(dir, "none", "network.jsonl") },
		"a CA that doesn't parse":    func(s *childStart) { s.ca, s.caKey = []byte("x"), redact.NewSecret("y") },
		"user CAs that aren't PEM":   func(s *childStart) { s.upstreamCAs = "x" },
		"a company proxy's bad URL":  func(s *childStart) { s.upstream = &Upstream{HTTPS: "socks5://corp:1080"} },
		"a built-in policy it lacks": func(s *childStart) { s.builtin = []string{"layers/gone.policy"} },
		"a negative Nix cap":         func(s *childStart) { s.nixCache, s.nixCacheMax = filepath.Join(dir, "nix"), -1 },
	} {
		st := base
		change(&st)
		if _, _, err := childServer(st.wire(), testBroker(t), IO{Stdout: io.Discard, Stderr: io.Discard}, "e", quiet()); err == nil {
			t.Errorf("%s: a server", name)
		}
	}
}

// The child's reads and token exchanges wait for one of captureReads' slots, at most until their
// context ends.
func TestCaptureSlotsWait(t *testing.T) {
	t.Parallel()
	cs := newCaptures(nil, nil)
	for range captureReads {
		if _, err := cs.slot(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := cs.slot(ctx); err == nil {
		t.Error("a slot past captureReads")
	}
	if _, err := cs.read(ctx, strings.NewReader("x"), 10); err == nil {
		t.Error("a read past captureReads")
	}
}

// A watched file is read as its kind says: the bindings and the links' list only when they're
// yours alone, and a kind the proxy doesn't know never.
func TestWatchedFileRead(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	open := filepath.Join(dir, "bindings.json")
	must(t, os.WriteFile(open, []byte("{}"), 0o644)) //nolint:gosec // a file others may read, as the test needs
	if _, err := (watchedFile{watchBindings, open}).read(); !errors.Is(err, ErrNotPrivate) {
		t.Errorf("bindings others may read: %v", err)
	}
	if _, err := (watchedFile{"other", open}).read(); err == nil {
		t.Error("an unknown kind: read")
	}
	if got, err := (watchedFile{watchLinks, filepath.Join(dir, "none")}).read(); err != nil || !got.missing {
		t.Errorf("a missing list: %+v, %v", got, err)
	}
}

// A reply from a token endpoint's host that would upgrade the connection is refused on every
// path, as its bytes would reach the VM unread.
func TestCaptureResponseRefusesUpgrades(t *testing.T) {
	t.Parallel()
	s := &server{captures: newCaptures([]Capture{{Agent: "codex", Host: "auth.test", Path: "/oauth/token"}}, nil)}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://auth.test/ws", http.NoBody)
	resp := &http.Response{StatusCode: http.StatusSwitchingProtocols, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: req}
	defer resp.Body.Close()
	if err := s.captureResponse("auth.test", 443)(resp); !errors.Is(err, errCaptureRefused) { //nolint:bodyclose // closed above
		t.Errorf("an upgrade: %v", err)
	}
	other := &http.Response{StatusCode: http.StatusSwitchingProtocols, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: req}
	defer other.Body.Close()
	if err := s.captureResponse("other.test", 443)(other); err != nil { //nolint:bodyclose // closed above
		t.Errorf("another host's upgrade: %v", err)
	}
	if got := string(errorReply([]byte(`{"error":"Bad Code"}`), true)); got != `{"error":"devvm_refused"}` {
		t.Errorf("an error code devvm doesn't read: %s", got)
	}
}

// The child's environment: TZ and the roots' variables as devvm has them, GODEBUG with
// updatemaxprocs=0, nothing else.
func TestChildEnv(t *testing.T) {
	t.Parallel()
	got := childEnv([]string{"HOME=/h", "TZ=UTC", "GITHUB_TOKEN=x", "SSL_CERT_FILE=/c", "GODEBUG=x=1", "DEVVM_DAEMON_STATUS=/s"})
	want := []string{"TZ=UTC", "SSL_CERT_FILE=/c", "GODEBUG=x=1,updatemaxprocs=0"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("childEnv = %q, want %q", got, want)
	}
	if got := childEnv(nil); len(got) != 1 || got[0] != "GODEBUG=updatemaxprocs=0" {
		t.Errorf("childEnv(nil) = %q", got)
	}
}

// A read of the host's addresses that fails, or finds none, keeps the last good one, so the floor
// never runs without them.
func TestHostAddrsKeepLastGood(t *testing.T) {
	t.Parallel()
	good := []net.Addr{&net.IPNet{IP: net.ParseIP("192.0.2.7"), Mask: net.CIDRMask(24, 32)}}
	var reads [][]net.Addr
	var fail bool
	h := &hostAddrs{read: func() ([]net.Addr, error) {
		if fail {
			return nil, errors.New("no netlink")
		}
		if len(reads) > 0 {
			r := reads[0]
			reads = reads[1:]
			return r, nil
		}
		return good, nil
	}}
	h.load()
	fail = true
	h.load()
	reads = [][]net.Addr{nil}
	fail = false
	h.load()
	if !h.addrs[netip.MustParseAddr("192.0.2.7")] {
		t.Errorf("the last good addresses went: %v", h.addrs)
	}
}
