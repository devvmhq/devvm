package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"time"
)

var errStop = errors.New("stop after ClientHello")

// sniOf returns the server name in a TLS ClientHello record, "" for none, and whether the record
// held a whole ClientHello that parsed: one split across records, cut short or refused by
// crypto/tls (a trailing dot, two names) didn't, and names nothing. It lets crypto/tls parse the
// record and stops the handshake at the ClientHello callback. The record is in memory, so ctx's
// end changes nothing.
func sniOf(ctx context.Context, rec []byte) (string, bool) {
	var name string
	parsed := false
	c := tls.Server(helloConn{bytes.NewReader(rec)}, &tls.Config{
		GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
			name, parsed = h.ServerName, true
			return nil, errStop
		},
	})
	_ = c.HandshakeContext(ctx)
	return name, parsed
}

// helloConn: a net.Conn that reads the recorded bytes and discards writes.
type helloConn struct{ r io.Reader }

func (c helloConn) Read(b []byte) (int, error)       { return c.r.Read(b) }
func (c helloConn) Write(b []byte) (int, error)      { return len(b), nil }
func (c helloConn) Close() error                     { return nil }
func (c helloConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c helloConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (c helloConn) SetDeadline(time.Time) error      { return nil }
func (c helloConn) SetReadDeadline(time.Time) error  { return nil }
func (c helloConn) SetWriteDeadline(time.Time) error { return nil }
