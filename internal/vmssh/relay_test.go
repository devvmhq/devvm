package vmssh

import (
	"bufio"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/daemon"
)

// relayPair is a connection to relay and its client's end, over TCP on
// the loopback, so the client can close its side for writing alone.
func relayPair(t *testing.T) (relayed net.Conn, client *net.TCPConn) {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		accepted <- c
	}()
	var d net.Dialer
	c, err := d.DialContext(t.Context(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	relayed = <-accepted
	if relayed == nil {
		t.Fatal("not accepted")
	}
	return relayed, c.(*net.TCPConn)
}

// pidOf waits for the ssh stand-in to write its pid to file, for up to
// 10 s, as a busy machine may be slow to start it.
func pidOf(t *testing.T, file string) int {
	t.Helper()
	for range 1000 {
		if b, err := os.ReadFile(file); err == nil { //nolint:gosec // gosec G304: the test's own temp file
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
				t.Cleanup(func() {
					if daemon.Alive(pid) {
						_ = daemon.Signal(pid, syscall.SIGKILL)
					}
				})
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the ssh stand-in never started")
	return 0
}

// A relay whose client has closed its side ends once the VM's side has
// been silent for the grace, though the VM never ends its channel: its
// ssh is killed. One whose VM side keeps sending after the client closed
// its side for writing goes on, so a client that half-closes still gets
// its answer. One whose client has closed fully while the VM keeps
// sending ends too: its writes to the client fail, and the ssh gets the
// grace to exit.
//
// The grace is short where the relay only has to end, and long where it
// must not: the stand-in writes every 50 ms, so a talking relay's grace of
// 2 s leaves a slow machine 40 times the room it needs between writes.
// The test reads for a second past the grace, so a grace that kept
// running through the writes would end the relay while it reads, and one
// that didn't start again after them would keep the relay once the
// stand-in goes quiet.
func TestPipeSSHGrace(t *testing.T) {
	const short, long = 200 * time.Millisecond, 2 * time.Second
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	// The stand-in writes its pid, then a line every 50 ms while the file
	// talk exists, and never exits on its own.
	talk := filepath.Join(dir, "talk")
	fakeSSH(t, "echo $$ > "+pidFile+".tmp && mv "+pidFile+".tmp "+pidFile+
		"\nwhile :; do if [ -e "+talk+" ]; then echo x || exit 1; fi; sleep 0.05; done")

	for _, tc := range []struct {
		name  string
		talk  bool
		half  bool // the client closes for writing alone, and reads on
		grace time.Duration
	}{
		{"silent, closed", false, false, short},
		{"silent, half closed", false, true, short},
		{"talking, half closed", true, true, long},
		{"talking, closed", true, false, short},
	} {
		grace := tc.grace
		_ = os.Remove(pidFile)
		_ = os.Remove(talk)
		if tc.talk {
			if err := os.WriteFile(talk, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		relayed, client := relayPair(t)
		ended := make(chan struct{})
		go func() {
			defer close(ended)
			pipeSSHFor(t.Context(), relayed, nil, grace)
		}()
		pid := pidOf(t, pidFile)
		if tc.half {
			_ = client.CloseWrite()
		} else {
			_ = client.Close()
		}
		if tc.talk && tc.half {
			// Still carried a second past the grace.
			r := bufio.NewReader(client)
			_ = client.SetReadDeadline(time.Now().Add(grace + 5*time.Second))
			for give := time.Now().Add(grace + time.Second); time.Now().Before(give); {
				if _, err := r.ReadString('\n'); err != nil {
					t.Fatalf("%s: the relay ended while the VM talked: %v", tc.name, err)
				}
			}
			select {
			case <-ended:
				t.Fatalf("%s: the relay ended while the VM talked", tc.name)
			default:
			}
			// Silent now: it ends after the grace.
			_ = os.Remove(talk)
		}
		select {
		case <-ended:
		case <-time.After(grace + 10*time.Second):
			_ = daemon.Signal(pid, syscall.SIGKILL)
			<-ended
			t.Fatalf("%s: the relay outlived its client's side", tc.name)
		}
		if daemon.Alive(pid) {
			t.Errorf("%s: its ssh still runs", tc.name)
		}
		_ = client.Close()
		_ = os.Remove(talk)
	}
}

// A half-closed client that stops reading for a while, as a slow one
// would, holds the VM's answer back, and the relay's writes to it wait:
// the grace doesn't run while they do, so the relay goes on, however long
// the client takes, for as long as the VM has more to send. Once the
// client closes, it ends.
func TestPipeSSHGraceSlowClient(t *testing.T) {
	const grace = 300 * time.Millisecond
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	fakeSSH(t, "echo $$ > "+pidFile+".tmp && mv "+pidFile+".tmp "+pidFile+"\nexec yes")
	relayed, client := relayPair(t)
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		pipeSSHFor(t.Context(), relayed, nil, grace)
	}()
	pid := pidOf(t, pidFile)
	_ = client.CloseWrite()
	// Not read for many graces: the VM's lines fill the buffers between
	// them, and the relay's write waits.
	time.Sleep(5 * grace)
	select {
	case <-ended:
		t.Fatal("the relay ended while its write to a slow client waited")
	default:
	}
	// Far more than the buffers hold: a relay whose ssh was killed while
	// it waited gives what they held, then EOF.
	_ = client.SetReadDeadline(time.Now().Add(20 * time.Second))
	if _, err := io.CopyN(io.Discard, client, 64<<20); err != nil {
		t.Fatalf("the slow client's answer: %v", err)
	}
	_ = client.Close()
	select {
	case <-ended:
	case <-time.After(grace + 10*time.Second):
		_ = daemon.Signal(pid, syscall.SIGKILL)
		<-ended
		t.Fatal("the relay outlived its client")
	}
	if daemon.Alive(pid) {
		t.Error("its ssh still runs")
	}
}

// A relay whose write waits on a client that stopped reading still ends
// with its owner's context, as LoopbackForward.Close or a forward's close
// cancels it: the cancel closes the connection, which ends the write.
func TestPipeSSHCancelEndsBlockedWrite(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	fakeSSH(t, "echo $$ > "+pidFile+".tmp && mv "+pidFile+".tmp "+pidFile+"\nexec yes")
	relayed, client := relayPair(t)
	ctx, cancel := context.WithCancel(t.Context())
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		pipeSSHFor(ctx, relayed, nil, time.Hour)
	}()
	pid := pidOf(t, pidFile)
	// Never read: the relay's write to the client waits once the buffers
	// fill.
	time.Sleep(500 * time.Millisecond)
	cancel()
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		_ = client.Close()
		_ = daemon.Signal(pid, syscall.SIGKILL)
		<-ended
		t.Fatal("the relay outlived its context while its write waited")
	}
	if daemon.Alive(pid) {
		t.Error("its ssh still runs")
	}
}

// deafConn is a client that can take nothing more, and whose reads never
// end, not even when it's closed: only the relay's grace after its last
// write failed ends the relay.
type deafConn struct {
	net.Conn
	block chan struct{}
}

func (c deafConn) Read([]byte) (int, error)    { <-c.block; return 0, net.ErrClosed }
func (c deafConn) Write([]byte) (int, error)   { return 0, syscall.EPIPE }
func (c deafConn) Close() error                { return nil }
func (c deafConn) SetDeadline(time.Time) error { return nil }

// A relay whose writes to its client fail ends within the grace while the
// VM keeps sending, even with the client's side never seen to close.
func TestPipeSSHGraceWriteError(t *testing.T) {
	const grace = 200 * time.Millisecond
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	fakeSSH(t, "echo $$ > "+pidFile+".tmp && mv "+pidFile+".tmp "+pidFile+"\nwhile :; do echo x || exit 1; sleep 0.05; done")
	block := make(chan struct{})
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		pipeSSHFor(t.Context(), deafConn{block: block}, nil, grace)
	}()
	defer func() { close(block) }()
	pid := pidOf(t, pidFile)
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		_ = daemon.Signal(pid, syscall.SIGKILL)
		<-ended
		t.Fatal("the relay outlived its failed writes")
	}
	if daemon.Alive(pid) {
		t.Error("its ssh still runs")
	}
}

// A grace timer runs only once armed, never while a write waits, and
// never once stopped: an arm or a write after the relay has ended, from
// the client's goroutine that outlives it, doesn't start it again. Each
// case's timer either fires within a second, or doesn't within ten
// graces.
func TestGraceTimer(t *testing.T) {
	const grace = 20 * time.Millisecond
	write := func(g *graceTimer, d time.Duration) {
		_, _ = g.write(func() (int, error) { time.Sleep(d); return 0, nil })
	}
	for _, tc := range []struct {
		name  string
		steps func(g *graceTimer)
		fires bool
	}{
		{"never armed, though written to", func(g *graceTimer) { write(g, 0) }, false},
		{"armed", func(g *graceTimer) { g.arm() }, true},
		{"armed, then written to", func(g *graceTimer) { g.arm(); write(g, 0) }, true},
		{"stopped, then armed", func(g *graceTimer) { g.arm(); g.stop(); g.arm() }, false},
		{"stopped, then written to", func(g *graceTimer) { g.arm(); g.stop(); write(g, 0) }, false},
	} {
		fired := make(chan struct{}, 1)
		g := newGraceTimer(grace, func() { fired <- struct{}{} })
		tc.steps(g)
		wait := 10 * grace
		if tc.fires {
			wait = time.Second
		}
		select {
		case <-fired:
			if !tc.fires {
				t.Errorf("%s: fired", tc.name)
			}
		case <-time.After(wait):
			if tc.fires {
				t.Errorf("%s: never fired", tc.name)
			}
		}
	}
	// A write that waits many graces, armed while it waits: the timer
	// doesn't fire until it's done.
	fired := make(chan struct{}, 1)
	g := newGraceTimer(grace, func() { fired <- struct{}{} })
	_, _ = g.write(func() (int, error) {
		g.arm()
		time.Sleep(10 * grace)
		return 0, nil
	})
	select {
	case <-fired:
		t.Error("fired while a write waited")
	default:
	}
}
