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
// nothing more, it's killed when it hasn't exited within grace. The grace
// doesn't run while a write to the client waits for it, so a slow client
// is never cut off from what the VM has sent it, but the owner's context
// (ctx) closes the connection when it ends, which ends such a write too.
func pipeSSHFor(ctx context.Context, conn net.Conn, args []string, grace time.Duration) {
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
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
	go func() {
		_, _ = io.Copy(in, conn)
		_ = in.Close()
		idle.arm()
	}()
	_, _ = io.Copy(&graceWriter{w: conn, idle: idle}, out)
	_ = conn.Close()
	idle.arm()
	_ = cmd.Wait()
}

// graceTimer cancels a relay once its grace runs out. It runs only once
// armed, as the client's side has closed, or the copy to the client has
// ended (a write that failed, or the VM's EOF), and starts again with
// each write to the client, which it waits out (write): the grace is time
// the VM sends nothing, never time a slow client takes. Once stopped, as the
// relay has ended, nothing starts it again, so the client's goroutine,
// which can outlive the relay, never does.
type graceTimer struct {
	grace time.Duration
	t     *time.Timer

	mu                    sync.Mutex
	armed, writing, ended bool
}

// newGraceTimer is a graceTimer for cancel, not yet armed.
func newGraceTimer(grace time.Duration, cancel context.CancelFunc) *graceTimer {
	g := &graceTimer{grace: grace, t: time.AfterFunc(grace, cancel)}
	g.t.Stop()
	return g
}

// arm starts the grace, unless a write is waiting, which starts it once
// done.
func (g *graceTimer) arm() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.armed = true
	g.restart()
}

// write holds the grace while a write to the client waits, then starts it
// again (once armed): the VM has just sent something.
func (g *graceTimer) write(w func() (int, error)) (int, error) {
	g.mu.Lock()
	g.writing = true
	g.t.Stop()
	g.mu.Unlock()
	n, err := w()
	g.mu.Lock()
	defer g.mu.Unlock()
	g.writing = false
	g.restart()
	return n, err
}

// restart starts the grace again when it's armed, no write waits and the
// relay hasn't ended. Its caller holds mu.
func (g *graceTimer) restart() {
	if g.armed && !g.writing && !g.ended {
		g.t.Reset(g.grace)
	}
}

// stop stops the timer for good.
func (g *graceTimer) stop() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ended = true
	g.t.Stop()
}

// graceWriter is the client's side of a relay, each write to which holds
// the grace while it waits and starts it again after (graceTimer.write).
type graceWriter struct {
	w    io.Writer
	idle *graceTimer
}

func (g *graceWriter) Write(p []byte) (int, error) {
	return g.idle.write(func() (int, error) { return g.w.Write(p) })
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
