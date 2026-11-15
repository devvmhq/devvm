// Package memnet is a network in memory for tests: connections (Pipe)
// and listeners (Network) whose every wait is one a testing/synctest
// bubble counts as durably blocked, so a test of a timeout or of what
// ends what runs on the bubble's own clock, where a socket's wait would
// keep the bubble's time from moving. A connection closes its writing
// side alone (CloseWrite), as TCP's does and net.Pipe's can't, holds
// what it's sent up to a buffer's worth, so a writer to a reader that
// stops waits as it would on a socket, and has deadlines. Its errors are
// a Unix socket's: once the other end has closed, the first write fails
// with EPIPE, and a read gets EOF, never a reset.
//
// It's for tests only, and standard library only, so internal/proxy's
// tests use it too.
package memnet

import (
	"context"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"time"
)

// bufMax is how much one direction of a connection holds before a write
// waits for its reader.
const bufMax = 64 << 10

// addr is an address in memory: a listener's, as its test named it.
type addr string

func (a addr) Network() string { return "mem" }
func (a addr) String() string  { return string(a) }

// way is one direction of a connection: what's sent and not yet read,
// whether its writer has closed it (EOF once read), and whether its
// reader has (its writes fail).
type way struct {
	buf      []byte
	eof      bool
	readGone bool
}

// pipe is a connection's two directions and what its two ends wait on.
type pipe struct {
	mu   sync.Mutex
	cond *sync.Cond
	ways [2]way
}

// A Conn is one end of a connection in memory. It reads ways[side] and
// writes the other.
type Conn struct {
	p             *pipe
	side          int
	local, remote addr

	// Under p.mu.
	closed         bool
	rdline, wrline time.Time
	rtimer, wtimer *time.Timer
}

// Pipe is a connection in memory: a's writes are b's reads, and b's a's.
func Pipe() (a, b *Conn) { return pipeAt("a", "b") }

func pipeAt(local, remote string) (a, b *Conn) {
	p := &pipe{}
	p.cond = sync.NewCond(&p.mu)
	return &Conn{p: p, side: 0, local: addr(local), remote: addr(remote)},
		&Conn{p: p, side: 1, local: addr(remote), remote: addr(local)}
}

func (c *Conn) in() *way  { return &c.p.ways[c.side] }
func (c *Conn) out() *way { return &c.p.ways[1-c.side] }

// opError is err as a socket's would be, for op.
func (c *Conn) opError(op string, err error) error {
	return &net.OpError{Op: op, Net: "mem", Source: c.local, Addr: c.remote, Err: err}
}

// passed is whether deadline d has passed; its caller holds p.mu.
func passed(d time.Time) bool { return !d.IsZero() && !time.Now().Before(d) }

// Read reads what the other end sent: io.EOF once it has closed its
// writing side and all it sent is read.
func (c *Conn) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	c.p.mu.Lock()
	defer c.p.mu.Unlock()
	w := c.in()
	for {
		switch {
		case c.closed:
			return 0, c.opError("read", net.ErrClosed)
		case passed(c.rdline):
			return 0, c.opError("read", os.ErrDeadlineExceeded)
		case len(w.buf) > 0:
			n := copy(b, w.buf)
			w.buf = w.buf[n:]
			c.p.cond.Broadcast()
			return n, nil
		case w.eof:
			return 0, io.EOF
		}
		c.p.cond.Wait()
	}
}

// Write sends b to the other end, waiting while what it holds is full:
// EPIPE once the other end has closed.
func (c *Conn) Write(b []byte) (int, error) {
	c.p.mu.Lock()
	defer c.p.mu.Unlock()
	w := c.out()
	n := 0
	for {
		switch {
		case c.closed:
			return n, c.opError("write", net.ErrClosed)
		case w.eof:
			return n, c.opError("write", syscall.EPIPE)
		case w.readGone:
			return n, c.opError("write", syscall.EPIPE)
		case passed(c.wrline) && (n < len(b) || n == 0):
			return n, c.opError("write", os.ErrDeadlineExceeded)
		case n == len(b):
			return n, nil
		case len(w.buf) < bufMax:
			k := min(len(b)-n, bufMax-len(w.buf))
			w.buf = append(w.buf, b[n:n+k]...)
			n += k
			c.p.cond.Broadcast()
			continue
		}
		c.p.cond.Wait()
	}
}

// CloseWrite closes the writing side alone: the other end reads EOF once
// it has read what was sent, and can still send.
func (c *Conn) CloseWrite() error {
	c.p.mu.Lock()
	defer c.p.mu.Unlock()
	c.out().eof = true
	c.p.cond.Broadcast()
	return nil
}

// Close closes both sides: the other end reads EOF, and its writes fail.
func (c *Conn) Close() error {
	c.p.mu.Lock()
	defer c.p.mu.Unlock()
	if c.closed {
		return c.opError("close", net.ErrClosed)
	}
	c.closed = true
	c.out().eof = true
	in := c.in()
	in.readGone, in.buf = true, nil
	for _, t := range []*time.Timer{c.rtimer, c.wtimer} {
		if t != nil {
			t.Stop()
		}
	}
	c.p.cond.Broadcast()
	return nil
}

// LocalAddr is this end's address: its listener's for an accepted one.
func (c *Conn) LocalAddr() net.Addr { return c.local }

// RemoteAddr is the other end's address.
func (c *Conn) RemoteAddr() net.Addr { return c.remote }

// SetDeadline sets when a waiting read or write gives up.
func (c *Conn) SetDeadline(t time.Time) error {
	_ = c.SetReadDeadline(t)
	return c.SetWriteDeadline(t)
}

// SetReadDeadline sets when a waiting read gives up.
func (c *Conn) SetReadDeadline(t time.Time) error {
	c.p.mu.Lock()
	defer c.p.mu.Unlock()
	c.rdline, c.rtimer = t, c.wake(c.rtimer, t)
	return nil
}

// SetWriteDeadline sets when a waiting write gives up.
func (c *Conn) SetWriteDeadline(t time.Time) error {
	c.p.mu.Lock()
	defer c.p.mu.Unlock()
	c.wrline, c.wtimer = t, c.wake(c.wtimer, t)
	return nil
}

// wake stops old and returns a timer that wakes the waiting ends at t,
// none for no deadline. Its caller holds p.mu.
func (c *Conn) wake(old *time.Timer, t time.Time) *time.Timer {
	if old != nil {
		old.Stop()
	}
	c.p.cond.Broadcast()
	if t.IsZero() {
		return nil
	}
	return time.AfterFunc(time.Until(t), func() {
		c.p.mu.Lock()
		defer c.p.mu.Unlock()
		c.p.cond.Broadcast()
	})
}

// A Network is listeners by address, which its dials reach. Its zero
// value has none.
type Network struct {
	mu sync.Mutex
	ls map[string]*Listener
}

// Listen listens at address: EADDRINUSE while another listener has it.
func (n *Network) Listen(address string) (*Listener, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.ls[address] != nil {
		return nil, &net.OpError{Op: "listen", Net: "mem", Addr: addr(address), Err: syscall.EADDRINUSE}
	}
	if n.ls == nil {
		n.ls = map[string]*Listener{}
	}
	l := &Listener{n: n, addr: addr(address)}
	l.cond = sync.NewCond(&l.mu)
	n.ls[address] = l
	return l, nil
}

// Dial connects to the listener at address: ECONNREFUSED for none.
func (n *Network) Dial(address string) (*Conn, error) {
	n.mu.Lock()
	l := n.ls[address]
	n.mu.Unlock()
	if l == nil {
		return nil, refused(address)
	}
	return l.Dial()
}

// DialContext is Dial, as net.Dialer's: network is the test's to pick.
func (n *Network) DialContext(_ context.Context, _, address string) (net.Conn, error) {
	c, err := n.Dial(address)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// Listening reports whether a listener has address.
func (n *Network) Listening(address string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.ls[address] != nil
}

func refused(address string) error {
	return &net.OpError{Op: "dial", Net: "mem", Addr: addr(address), Err: syscall.ECONNREFUSED}
}

// A Listener is a Network's listener at an address, whose dials wait to
// be accepted.
type Listener struct {
	n    *Network
	addr addr

	mu     sync.Mutex
	cond   *sync.Cond
	queue  []*Conn
	closed bool
	line   time.Time
	timer  *time.Timer
}

// Dial connects to l: the other end waits for Accept. ECONNREFUSED once
// l is closed.
func (l *Listener) Dial() (*Conn, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, refused(string(l.addr))
	}
	client, server := pipeAt("client", string(l.addr))
	l.queue = append(l.queue, server)
	l.cond.Broadcast()
	return client, nil
}

// Accept waits for a dial: net.ErrClosed once l is closed,
// os.ErrDeadlineExceeded past its deadline.
func (l *Listener) Accept() (net.Conn, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for {
		switch {
		case l.closed:
			return nil, &net.OpError{Op: "accept", Net: "mem", Addr: l.addr, Err: net.ErrClosed}
		case len(l.queue) > 0:
			c := l.queue[0]
			l.queue = l.queue[1:]
			return c, nil
		case passed(l.line):
			return nil, &net.OpError{Op: "accept", Net: "mem", Addr: l.addr, Err: os.ErrDeadlineExceeded}
		}
		l.cond.Wait()
	}
}

// Close stops listening, and closes each dial not yet accepted.
func (l *Listener) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return &net.OpError{Op: "close", Net: "mem", Addr: l.addr, Err: net.ErrClosed}
	}
	l.closed = true
	queued := l.queue
	l.queue = nil
	if l.timer != nil {
		l.timer.Stop()
	}
	l.cond.Broadcast()
	l.mu.Unlock()
	for _, c := range queued {
		_ = c.Close()
	}
	if l.n != nil {
		l.n.mu.Lock()
		if l.n.ls[string(l.addr)] == l {
			delete(l.n.ls, string(l.addr))
		}
		l.n.mu.Unlock()
	}
	return nil
}

// Addr is the address it listens at.
func (l *Listener) Addr() net.Addr { return l.addr }

// SetDeadline sets when a waiting Accept gives up, as a TCP or Unix
// listener's does.
func (l *Listener) SetDeadline(t time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.timer != nil {
		l.timer.Stop()
		l.timer = nil
	}
	l.line = t
	l.cond.Broadcast()
	if !t.IsZero() {
		l.timer = time.AfterFunc(time.Until(t), func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.cond.Broadcast()
		})
	}
	return nil
}
