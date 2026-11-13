package vmssh

import (
	"context"
	"io"
	"net"
	"os/exec"
	"sync"
	"time"
)

// relayGrace is how long a relay goes on once its client has closed its
// side with nothing coming from the VM (pipeSSHFor).
const relayGrace = 30 * time.Second

// pipeSSH carries conn through ssh run with args (-W), until either side
// ends, then closes conn; a VM that never ends its side is cut off
// relayGrace after the client has (pipeSSHFor).
func pipeSSH(ctx context.Context, conn net.Conn, args []string) {
	pipeSSHFor(ctx, conn, args, relayGrace)
}

// pipeSSHFor is pipeSSH with its grace. The ssh (-W) ends only when the
// VM's sshd ends its channel, so a VM that ignored the client's EOF would
// keep it, and its owner's relay slot, for as long as the VM liked. Once
// the client's side has closed, the ssh is killed when the VM has sent
// nothing for grace, so a client that closes for writing alone still
// gets an answer that keeps coming, as long as the VM sends something at
// least every grace, its first byte included; once the client can take
// nothing more, it's killed when it hasn't exited within grace.
func pipeSSHFor(ctx context.Context, conn net.Conn, args []string, grace time.Duration) {
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh", args...) //nolint:gosec // devvm's own arguments
	in, err := cmd.StdinPipe()
	if err != nil {
		return
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	if err := cmd.Start(); err != nil {
		return
	}
	idle := newGraceTimer(grace, cancel)
	defer idle.stop()
	clientDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(in, conn)
		_ = in.Close()
		close(clientDone)
		idle.reset()
	}()
	_, _ = io.Copy(conn, &graceReader{r: out, clientDone: clientDone, idle: idle})
	_ = conn.Close()
	idle.reset()
	_ = cmd.Wait()
}

// graceTimer cancels a relay once its grace runs out, from its last reset.
// Once stopped, as the relay has ended, a reset does nothing, so the
// client's goroutine, which can outlive the relay, never starts it again.
type graceTimer struct {
	grace time.Duration
	t     *time.Timer

	mu      sync.Mutex
	stopped bool
}

// newGraceTimer is a graceTimer for cancel, not yet started.
func newGraceTimer(grace time.Duration, cancel context.CancelFunc) *graceTimer {
	g := &graceTimer{grace: grace, t: time.AfterFunc(grace, cancel)}
	g.t.Stop()
	return g
}

// reset starts the grace again, unless the timer was stopped.
func (g *graceTimer) reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.stopped {
		g.t.Reset(g.grace)
	}
}

// stop stops the timer for good.
func (g *graceTimer) stop() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.stopped = true
	g.t.Stop()
}

// graceReader is the VM's side of a relay: each read puts the grace off
// once the client's side has closed.
type graceReader struct {
	r          io.Reader
	clientDone chan struct{}
	idle       *graceTimer
}

func (g *graceReader) Read(p []byte) (int, error) {
	n, err := g.r.Read(p)
	if n > 0 {
		select {
		case <-g.clientDone:
			g.idle.reset()
		default:
		}
	}
	return n, err
}

// relayConns is the connections a forward, a link or a link's target
// master carries that are open, each with what ends its relay's context,
// and whether they were cut, after which it takes no more. Its owner's
// mutex guards it.
//
// A relay's ssh (-W) waits for the VM's sshd to end its channel, so a VM
// that ignored EOF would keep that ssh, and one of its owner's relay
// slots, after the connection closed. Each relay runs with a context of
// its own instead, which cut cancels, so exec.CommandContext kills its
// ssh, and a relay that ends by itself releases its context (done).
type relayConns struct {
	open map[net.Conn]context.CancelFunc
	cut  bool
}

// add tracks conn and its relay's cancel, and reports false, tracking
// neither, once they were cut: its caller then cancels and closes it.
func (r *relayConns) add(conn net.Conn, cancel context.CancelFunc) bool {
	if r.cut {
		return false
	}
	if r.open == nil {
		r.open = map[net.Conn]context.CancelFunc{}
	}
	r.open[conn] = cancel
	return true
}

// done stops tracking a connection whose relay ended.
func (r *relayConns) done(conn net.Conn) { delete(r.open, conn) }

// cutAll closes each connection and cancels its relay's context, so its
// ssh is killed even when the VM never ends its channel, and refuses any
// added after.
func (r *relayConns) cutAll() {
	r.cut = true
	for c, cancel := range r.open {
		_ = c.Close()
		cancel()
	}
	clear(r.open)
}
