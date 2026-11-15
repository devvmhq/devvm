package vmssh

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/devvmhq/devvm/internal/daemon"
	"github.com/devvmhq/devvm/internal/leaktest"
	"github.com/devvmhq/devvm/internal/memnet"
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
			sshRelay{grace: grace, start: execSSH}.carry(t.Context(), relayed, nil)
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
		sshRelay{grace: grace, start: execSSH}.carry(t.Context(), relayed, nil)
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
func TestRelayCancelEndsBlockedWrite(t *testing.T) {
	leaktest.Check(t)
	synctest.Test(t, func(t *testing.T) {
		ssh := &memSSH{vm: vmYes}
		ctx, cancel := context.WithCancel(t.Context())
		_, ended := ssh.relay(ctx, time.Hour)
		// Never read: the relay's write to the client waits once its
		// buffer fills.
		time.Sleep(time.Minute)
		synctest.Wait()
		if isClosed(ended) {
			t.Fatal("the relay ended while its context lasted")
		}
		cancel()
		synctest.Wait()
		if !isClosed(ended) {
			t.Fatal("the relay outlived its context while its write waited")
		}
		if ssh.running() != 0 {
			t.Error("its ssh still runs")
		}
	})
}

// memSSH is an ssh -W in memory, for a relay in a synctest bubble: what
// the VM sends (vm) until it's killed, as exec.CommandContext kills ssh
// once its context ends, and its input read and dropped. Killed, its
// output ends and its input takes nothing more.
type memSSH struct {
	vm func(ctx context.Context, out io.Writer)

	mu    sync.Mutex
	procs []chan struct{} // each one started, closed once it has ended
	ended int
}

func (m *memSSH) start(ctx context.Context, _ []string) (sshPipes, error) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan struct{})
	m.mu.Lock()
	m.procs = append(m.procs, done)
	m.mu.Unlock()
	kill := func() {
		_ = outW.Close()
		_ = inR.CloseWithError(syscall.EPIPE)
	}
	stop := context.AfterFunc(ctx, kill)
	go func() { _, _ = io.Copy(io.Discard, inR) }()
	go func() {
		m.vm(ctx, outW)
		<-ctx.Done() // it never ends its channel: only the kill ends it
		stop()
		kill()
		m.mu.Lock()
		m.ended++
		close(done)
		m.mu.Unlock()
	}()
	return sshPipes{in: inW, out: outR, wait: func() error { <-done; return nil }}, nil
}

// count is how many of its ssh started; running how many run.
func (m *memSSH) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.procs)
}

func (m *memSSH) running() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.procs) - m.ended
}

// alive is whether the i-th ssh started still runs.
func (m *memSSH) alive(i int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return !isClosed(m.procs[i])
}

// relay carries a connection in memory through m, with grace, until ctx
// ends: the client's end, and what's closed once the relay has ended.
func (m *memSSH) relay(ctx context.Context, grace time.Duration) (*memnet.Conn, chan struct{}) {
	client, relayed := memnet.Pipe()
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		sshRelay{grace: grace, start: m.start}.carry(ctx, relayed, nil)
	}()
	return client, ended
}

// vmSilent is a VM that sends nothing; vmYes one that sends without end,
// as yes does.
func vmSilent(context.Context, io.Writer) {}

func vmYes(_ context.Context, out io.Writer) {
	line := bytes.Repeat([]byte("y\n"), 2048)
	for {
		if _, err := out.Write(line); err != nil {
			return
		}
	}
}

// vmSays is a VM that sends each line it's given on say.
func vmSays(say chan string) func(context.Context, io.Writer) {
	return func(ctx context.Context, out io.Writer) {
		for {
			select {
			case <-ctx.Done():
				return
			case s := <-say:
				if _, err := io.WriteString(out, s+"\n"); err != nil {
					return
				}
			}
		}
	}
}

// isClosed reports whether ch is closed.
func isClosed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// endsAt checks, in a bubble, that a relay ends exactly after d more,
// with its ssh killed, and not a moment before.
func endsAt(t *testing.T, what string, ended chan struct{}, ssh *memSSH, d time.Duration) {
	t.Helper()
	time.Sleep(d - time.Nanosecond)
	synctest.Wait()
	if isClosed(ended) {
		t.Fatalf("%s: ended before %s", what, d)
	}
	time.Sleep(time.Nanosecond)
	synctest.Wait()
	if !isClosed(ended) {
		t.Fatalf("%s: still carried after %s", what, d)
	}
	if ssh.running() != 0 {
		t.Errorf("%s: its ssh still runs", what)
	}
}

// The grace, on the bubble's clock: a relay whose client has closed,
// fully or for writing alone, ends exactly the grace after, its ssh
// killed, though the VM never ends its channel. One whose VM keeps
// sending to a half-closed client goes on as long as it sends within each
// grace, and ends the grace after its last line. One whose client has
// closed fully, its VM still sending, ends the grace after the write that
// failed.
func TestRelayGrace(t *testing.T) {
	const grace = 30 * time.Second
	for _, tc := range []struct {
		name string
		half bool
	}{{"closed", false}, {"half closed", true}} {
		t.Run("silent, "+tc.name, func(t *testing.T) {
			leaktest.Check(t)
			synctest.Test(t, func(t *testing.T) {
				ssh := &memSSH{vm: vmSilent}
				client, ended := ssh.relay(t.Context(), grace)
				time.Sleep(time.Hour) // nothing ends it while the client's side is open
				synctest.Wait()
				if isClosed(ended) {
					t.Fatal("ended with its client's side open")
				}
				if tc.half {
					_ = client.CloseWrite()
				} else {
					_ = client.Close()
				}
				endsAt(t, "silent", ended, ssh, grace)
			})
		})
	}
	t.Run("talking, half closed", func(t *testing.T) {
		leaktest.Check(t)
		synctest.Test(t, func(t *testing.T) {
			say := make(chan string)
			ssh := &memSSH{vm: vmSays(say)}
			client, ended := ssh.relay(t.Context(), grace)
			_ = client.CloseWrite()
			r := bufio.NewReader(client)
			for i := range 10 {
				time.Sleep(grace - time.Nanosecond)
				say <- strconv.Itoa(i)
				if line, err := r.ReadString('\n'); err != nil || line != strconv.Itoa(i)+"\n" {
					t.Fatalf("line %d: %q, %v", i, line, err)
				}
			}
			endsAt(t, "talking", ended, ssh, grace)
		})
	})
	t.Run("talking, closed", func(t *testing.T) {
		leaktest.Check(t)
		synctest.Test(t, func(t *testing.T) {
			say := make(chan string)
			ssh := &memSSH{vm: vmSays(say)}
			client, ended := ssh.relay(t.Context(), grace)
			_ = client.Close()
			time.Sleep(grace / 2)
			say <- "x" // its write to the client fails: the grace starts again
			endsAt(t, "talking", ended, ssh, grace)
		})
	})
}

// A half-closed client that stops reading holds the VM's answer back, and
// the relay's write to it waits: the grace doesn't run while it does,
// however long that is. Read again, the answer keeps coming; closed, the
// relay ends the grace after.
func TestRelayGraceSlowClient(t *testing.T) {
	const grace = 30 * time.Second
	leaktest.Check(t)
	synctest.Test(t, func(t *testing.T) {
		ssh := &memSSH{vm: vmYes}
		client, ended := ssh.relay(t.Context(), grace)
		_ = client.CloseWrite()
		time.Sleep(10 * grace)
		synctest.Wait()
		if isClosed(ended) {
			t.Fatal("the relay ended while its write to a slow client waited")
		}
		if _, err := io.CopyN(io.Discard, client, 1<<20); err != nil {
			t.Fatalf("the slow client's answer: %v", err)
		}
		_ = client.Close()
		endsAt(t, "slow client", ended, ssh, grace)
	})
}

// A relay whose writes to its client fail ends the grace after the first,
// with the client's side never seen to close.
func TestRelayGraceWriteError(t *testing.T) {
	const grace = 30 * time.Second
	leaktest.Check(t)
	synctest.Test(t, func(t *testing.T) {
		say := make(chan string)
		ssh := &memSSH{vm: vmSays(say)}
		block := make(chan struct{})
		defer close(block)
		ended := make(chan struct{})
		go func() {
			defer close(ended)
			sshRelay{grace: grace, start: ssh.start}.carry(t.Context(), deafConn{block: block}, nil)
		}()
		time.Sleep(time.Hour)
		say <- "x"
		endsAt(t, "deaf client", ended, ssh, grace)
	})
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
		sshRelay{grace: grace, start: execSSH}.carry(t.Context(), deafConn{block: block}, nil)
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
// the client's goroutine that outlives it, doesn't start it again. On the
// bubble's clock, a timer that fires does so exactly the grace after its
// last arm or write, and one that doesn't never does.
func TestGraceTimer(t *testing.T) {
	const grace = 30 * time.Second
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
		{"armed, then written to", func(g *graceTimer) { g.arm(); time.Sleep(grace / 2); write(g, 0) }, true},
		// Armed, then a write that waits many graces: the grace doesn't run
		// while it waits, and starts again once it's done.
		{"armed, then a long write", func(g *graceTimer) { g.arm(); write(g, 10*grace) }, true},
		// A write that waits many graces, armed while it waits: the grace
		// starts once it's done.
		{"armed while a write waits", func(g *graceTimer) {
			_, _ = g.write(func() (int, error) { g.arm(); time.Sleep(10 * grace); return 0, nil })
		}, true},
		{"stopped, then armed", func(g *graceTimer) { g.arm(); g.stop(); g.arm() }, false},
		{"stopped, then written to", func(g *graceTimer) { g.arm(); g.stop(); write(g, 0) }, false},
	} {
		synctest.Test(t, func(t *testing.T) {
			var fired atomic.Bool
			g := newGraceTimer(grace, func() { fired.Store(true) })
			tc.steps(g)
			time.Sleep(grace - time.Nanosecond)
			synctest.Wait()
			if fired.Load() {
				t.Errorf("%s: fired before the grace", tc.name)
			}
			time.Sleep(time.Nanosecond)
			synctest.Wait()
			if fired.Load() != tc.fires {
				t.Errorf("%s: fired %v once the grace passed, want %v", tc.name, fired.Load(), tc.fires)
			}
			time.Sleep(time.Hour)
			synctest.Wait()
			if fired.Load() != tc.fires {
				t.Errorf("%s: fired %v an hour on, want %v", tc.name, fired.Load(), tc.fires)
			}
			g.stop()
		})
	}
}
