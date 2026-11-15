package vmssh

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"
)

// flakyListener fails its first fails Accepts with EMFILE, as a process
// out of files does, its listener still open.
type flakyListener struct {
	net.Listener
	fails atomic.Int32
}

func flaky(l net.Listener, fails int32) *flakyListener {
	f := &flakyListener{Listener: l}
	f.fails.Store(fails)
	return f
}

func (l *flakyListener) Accept() (net.Conn, error) {
	if l.fails.Add(-1) >= 0 {
		return nil, &net.OpError{Op: "accept", Net: "tcp", Err: os.NewSyscallError("accept", syscall.EMFILE)}
	}
	return l.Listener.Accept()
}

// An accept loop ends with its listener's close or its context's end,
// and tries again after any other error, waiting longer after each in a
// row, up to a second, and logging only the first of a run.
func TestAcceptRetry(t *testing.T) {
	emfile := &net.OpError{Op: "accept", Err: os.NewSyscallError("accept", syscall.EMFILE)}
	var logged []error
	log := func(err error) { logged = append(logged, err) }
	var a acceptRetry
	if a.failed(t.Context(), net.ErrClosed, log) {
		t.Error("a closed listener: tried again")
	}
	var waits []time.Duration
	for range 3 {
		if !a.failed(t.Context(), emfile, log) {
			t.Fatal("EMFILE: the loop ended")
		}
		waits = append(waits, a.wait)
	}
	if !slices.Equal(waits, []time.Duration{acceptRetryFirst, 2 * acceptRetryFirst, 4 * acceptRetryFirst}) {
		t.Errorf("waits %v, want doubling from %v", waits, acceptRetryFirst)
	}
	if len(logged) != 1 || !errors.Is(logged[0], syscall.EMFILE) {
		t.Errorf("logged %v, want the first EMFILE alone", logged)
	}
	a.wait = acceptRetryMax * 3 / 4
	_ = a.failed(t.Context(), emfile, log)
	if a.wait != acceptRetryMax {
		t.Errorf("a wait of %v, past its cap of %v", a.wait, acceptRetryMax)
	}
	a.accepted()
	_ = a.failed(t.Context(), emfile, log)
	if len(logged) != 2 || a.wait != acceptRetryFirst {
		t.Errorf("a new run: logged %d, waits %v; want it logged and the first wait", len(logged), a.wait)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if a.failed(ctx, emfile, nil) {
		t.Error("an ended context: tried again")
	}
}

// A link's socket that runs out of files is tried again, logged once,
// and its loop ends only with the socket's close.
func TestLinksAcceptRetries(t *testing.T) {
	f := &fakeLinks{}
	l := f.newLinks(t)
	var log bytes.Buffer
	var logMu sync.Mutex
	l.Log = lockedWriter{&logMu, &log}
	inner, err := listenAny(t)
	if err != nil {
		t.Fatal(err)
	}
	ln := flaky(inner, 3)
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.serve(t.Context(), &linkEnd{ln: ln, sock: "s", conns: &relayConns{}})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for ln.fails.Load() >= 0 {
		if time.Now().After(deadline) {
			t.Fatal("the loop didn't try the socket again")
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = inner.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop went on after its socket closed")
	}
	logMu.Lock()
	defer logMu.Unlock()
	if n := strings.Count(log.String(), "too many open files"); n != 1 {
		t.Errorf("logged the run of errors %d times, want once:\n%s", n, log.String())
	}
}

// A forward whose listener runs out of files goes on serving once it can.
func TestForwarderAcceptRetries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const port = 5000
		lo := &memLoopback{}
		vm := &fakeVM{ready: make(chan struct{}, 4)}
		f := &Forwarder{
			VM: "devvm-f", Tick: time.Second, Every: time.Hour, Log: io.Discard,
			Alive:    func(context.Context) bool { return true },
			Reserved: func() (map[int]bool, error) { return nil, nil },
			Watch:    vm.watch, Relay: echo,
			Listen: func(ctx context.Context, addr string) (net.Listener, error) {
				l, err := lo.listen(ctx, addr)
				if err != nil {
					return nil, err
				}
				return flaky(l, 3), nil
			},
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() { _ = f.Run(ctx) }()
		<-vm.ready
		vm.report(t, procLine("0100007F", port))
		// Past the retries' waits, on the bubble's clock.
		time.Sleep(time.Second)
		at := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
		if got := lo.reach(at); got != "to "+at {
			t.Errorf("after the listener ran out of files: %q, want it forwarded", got)
		}
	})
}

// A login's forward carries loopbackConnsMax connections at once, closing
// those past it, and goes on through a listener out of files.
func TestForwardLoopbackLimit(t *testing.T) {
	release := make(chan struct{})
	var carried atomic.Int32
	carry := func(_ context.Context, c net.Conn, _ int) {
		carried.Add(1)
		<-release
		_, _ = io.WriteString(c, "ok")
		_ = c.Close()
	}
	l, err := listenAny(t)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	f := &LoopbackForward{ls: []net.Listener{l}, cancel: cancel, slots: make(chan struct{}, loopbackConnsMax)}
	port := l.Addr().(*net.TCPAddr).Port
	f.wg.Go(func() { f.serve(ctx, flaky(l, 2), port, carry) })
	defer f.Close()
	var held []net.Conn
	defer func() {
		for _, c := range held {
			_ = c.Close()
		}
	}()
	for range loopbackConnsMax {
		c, err := dialPort(t, port)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, c)
	}
	deadline := time.Now().Add(10 * time.Second)
	for carried.Load() < loopbackConnsMax {
		if time.Now().After(deadline) {
			close(release)
			t.Fatalf("%d of %d connections carried", carried.Load(), loopbackConnsMax)
		}
		time.Sleep(10 * time.Millisecond)
	}
	past, err := dialPort(t, port)
	if err != nil {
		t.Fatal(err)
	}
	_ = past.SetReadDeadline(time.Now().Add(5 * time.Second))
	if b, err := io.ReadAll(past); err != nil || len(b) > 0 {
		t.Errorf("past the limit: %q, %v; want it closed", b, err)
	}
	_ = past.Close()
	close(release)
	for _, c := range held {
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if b, _ := io.ReadAll(c); string(b) != "ok" {
			t.Errorf("a connection within the limit: %q", b)
		}
	}
}
