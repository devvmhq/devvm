package vmssh

import (
	"bufio"
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

// pidOf waits for the ssh stand-in to write its pid to file.
func pidOf(t *testing.T, file string) int {
	t.Helper()
	for range 200 {
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
func TestPipeSSHGrace(t *testing.T) {
	const grace = 200 * time.Millisecond
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	// The stand-in writes its pid, then a line every 50 ms while the file
	// talk exists, and never exits on its own.
	talk := filepath.Join(dir, "talk")
	fakeSSH(t, "echo $$ > "+pidFile+".tmp && mv "+pidFile+".tmp "+pidFile+
		"\nwhile :; do if [ -e "+talk+" ]; then echo x || exit 1; fi; sleep 0.05; done")

	for _, tc := range []struct {
		name string
		talk bool
		half bool // the client closes for writing alone, and reads on
	}{
		{"silent, closed", false, false},
		{"silent, half closed", false, true},
		{"talking, half closed", true, true},
		{"talking, closed", true, false},
	} {
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
			// Still carried well past the grace.
			r := bufio.NewReader(client)
			_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
			for give := time.Now().Add(4 * grace); time.Now().Before(give); {
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
		case <-time.After(5 * time.Second):
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

// A grace timer stopped, as its relay has ended, stays stopped: a reset
// after it, from the client's goroutine that outlives the relay, never
// starts it again.
func TestGraceTimerStopped(t *testing.T) {
	const grace = 20 * time.Millisecond
	fired := make(chan struct{}, 1)
	g := newGraceTimer(grace, func() { fired <- struct{}{} })
	g.reset()
	g.stop()
	g.reset()
	select {
	case <-fired:
		t.Error("a stopped timer fired after a reset")
	case <-time.After(10 * grace):
	}
	// The control: one that isn't stopped fires.
	g = newGraceTimer(grace, func() { fired <- struct{}{} })
	g.reset()
	select {
	case <-fired:
	case <-time.After(time.Second):
		t.Error("a reset timer never fired")
	}
}
