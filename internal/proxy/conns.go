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
type conns struct {
	mu      sync.Mutex
	open    map[net.Conn]struct{}
	closing bool
	wg      sync.WaitGroup
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
		if l.c.open == nil {
			l.c.open = map[net.Conn]struct{}{}
		}
		l.c.open[conn] = struct{}{}
		l.c.mu.Unlock()
		return trackedConn{conn, l.c}, nil
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
