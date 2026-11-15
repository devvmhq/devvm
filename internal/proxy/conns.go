package proxy

// The proxy's connections and handlers, which its close ends and waits for: http.Server's Close
// closes the listener and the connections it still serves, but neither a connection a handler took
// from it (a CONNECT's tunnel, hijack, or a WebSocket's upgrade, which httputil.ReverseProxy takes
// itself) nor waits for any handler to return.

import (
	"errors"
	"net"
	"net/http"
	"sync"
	"time"
)

// errClosing is a request's once the proxy is closing (conns.close).
var errClosing = errors.New("the proxy is closing")

// conns are the connections the proxy's listener accepted (track), whatever then takes them, and
// its handlers (handle). Its close closes the one and waits for the other, so nothing of a request
// or a tunnel goes on after it.
//
// A connection waits until its first request with the VM's password admits it (admit): every
// program on this machine reaches the proxy's port, another VM's included, so one without the
// password must never take the VM's room. At most waitMax wait at once (0: no limit), and one
// accepted past that closes the one that has waited longest, which a real client, sending its
// request at once, never is. At most max are admitted at once (0: no limit), and admit refuses one
// past it, calling full.
type conns struct {
	mu      sync.Mutex
	open    map[net.Conn]struct{}  // admitted
	waiting map[net.Conn]time.Time // accepted, not admitted yet, with when
	closing bool
	wg      sync.WaitGroup
	max     int
	waitMax int
	full    func(limit int)
}

// connKey is the context key of a request's connection, as the listener accepted it
// (httpServer's ConnContext).
type connKey struct{}

// admit counts conn, a request's connection with the VM's password, toward max, and reports
// whether there's room: true for one admitted already, or that c never accepted.
func (c *conns) admit(conn net.Conn) bool {
	if t, ok := conn.(trackedConn); ok {
		conn = t.Conn
	}
	c.mu.Lock()
	if _, ok := c.waiting[conn]; !ok {
		c.mu.Unlock()
		return true
	}
	if c.max > 0 && len(c.open) >= c.max {
		c.mu.Unlock()
		if c.full != nil {
			c.full(c.max)
		}
		return false
	}
	delete(c.waiting, conn)
	if c.open == nil {
		c.open = map[net.Conn]struct{}{}
	}
	c.open[conn] = struct{}{}
	c.mu.Unlock()
	return true
}

// track is l, whose connections c holds until each is closed.
func (c *conns) track(l net.Listener) net.Listener { return trackedListener{l, c} }

// handle is h, counted until it returns; once the proxy is closing, it refuses each request.
func (c *conns) handle(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		closing := c.closing
		if !closing {
			c.wg.Add(1)
		}
		c.mu.Unlock()
		if closing {
			http.Error(w, "devvm proxy: "+errClosing.Error(), http.StatusServiceUnavailable)
			return
		}
		defer c.wg.Done()
		h(w, r)
	}
}

// close closes every connection held, and those accepted after it, and waits, for at most wait,
// for the handlers to return: false if one hasn't.
func (c *conns) close(wait time.Duration) bool {
	c.mu.Lock()
	c.closing = true
	for conn := range c.open {
		_ = conn.Close()
	}
	for conn := range c.waiting {
		_ = conn.Close()
	}
	c.mu.Unlock()
	done := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(wait):
		return false
	}
}

// A trackedListener is a listener whose connections its conns hold.
type trackedListener struct {
	net.Listener
	c *conns
}

func (l trackedListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		l.c.mu.Lock()
		if l.c.closing {
			l.c.mu.Unlock()
			_ = conn.Close()
			continue
		}
		if l.c.waitMax > 0 && len(l.c.waiting) >= l.c.waitMax {
			l.c.evictOldest()
		}
		if l.c.waiting == nil {
			l.c.waiting = map[net.Conn]time.Time{}
		}
		l.c.waiting[conn] = time.Now()
		l.c.mu.Unlock()
		return trackedConn{conn, l.c}, nil
	}
}

// evictOldest closes the connection that has waited longest to be admitted; c.mu is held.
func (c *conns) evictOldest() {
	var oldest net.Conn
	var since time.Time
	for conn, at := range c.waiting {
		if oldest == nil || at.Before(since) {
			oldest, since = conn, at
		}
	}
	if oldest != nil {
		delete(c.waiting, oldest)
		_ = oldest.Close()
	}
}

// A trackedConn is an accepted connection, which its close lets go of.
type trackedConn struct {
	net.Conn
	c *conns
}

func (t trackedConn) Close() error {
	t.c.mu.Lock()
	delete(t.c.open, t.Conn)
	delete(t.c.waiting, t.Conn)
	t.c.mu.Unlock()
	return t.Conn.Close()
}

// CloseWrite closes the connection's writing side, or the connection when it has none to close, as
// closeWrite does.
func (t trackedConn) CloseWrite() error {
	if cw, ok := t.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return t.Close()
}
