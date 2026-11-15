package proxy

// Tunnels: a CONNECT the proxy carries itself (to a host the policy allows, or to one of the VM's
// links) and each connection the relay passes on are spliced, their bytes passed as they are both
// ways (splice), the far end's from the start; a CONNECT's connection is first taken from the HTTP
// server (hijack).

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
)

// hijack takes a CONNECT request's connection from the HTTP server and tells the client its tunnel
// is open. Bytes the client sent after the request, which the server may already have read, are
// the connection's first.
func hijack(w http.ResponseWriter) (bufConn, error) {
	c, buf, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return bufConn{}, err
	}
	if _, err := c.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
		_ = c.Close()
		return bufConn{}, err
	}
	return bufConn{c, buf.Reader}, nil
}

// splice passes client's bytes to server and server's to client, as they are. The end of what
// client sends ends what server reads (closeWrite), and server can still answer. Once server's
// bytes end, or client can't be written to (it went away), or client can't be read from (it was
// reset, or closed as the proxy closes: conns.close), splice closes both connections and returns.
// A server that can't be written to, as it answered early and closed, still has its answer passed
// to the end. Server's bytes pass from the start; client's only once before, when there's one, has
// returned true (it may read client's first bytes and pass them on itself), and when it returns
// false, none do, and both connections close.
func splice(client, server net.Conn, before func() bool) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		if before != nil && !before() {
			_ = client.Close()
			_ = server.Close()
			return
		}
		// Only the client's own failure ends both: a server that answers early and closes while
		// the client still sends (a 413, a refused login) fails the writes, and its answer still
		// passes.
		rc := &readErr{r: client}
		_, _ = io.Copy(server, rc)
		if rc.err != nil && !errors.Is(rc.err, io.EOF) {
			_ = client.Close()
			_ = server.Close()
			return
		}
		closeWrite(server)
	}()
	_, _ = io.Copy(client, server)
	_ = client.Close()
	_ = server.Close()
	<-done
}

// readErr is r, keeping the error its last read returned: what splice tells a client that failed
// from a server it couldn't write to.
type readErr struct {
	r   io.Reader
	err error
}

func (r *readErr) Read(b []byte) (int, error) {
	n, err := r.r.Read(b)
	r.err = err
	return n, err
}

// closeWrite closes c's writing side, so the other end reads the end of what c sent, or closes c
// when it can't.
func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}

// bufConn is a connection whose first bytes may already sit in a bufio.Reader: a hijacked one's in
// the HTTP server's, an upstream tunnel's in what read the upstream's answer.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c bufConn) Read(b []byte) (int, error) { return c.r.Read(b) }

// CloseWrite closes the writing side of the connection underneath, when it has one.
func (c bufConn) CloseWrite() error {
	if t, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return t.CloseWrite()
	}
	return nil
}
