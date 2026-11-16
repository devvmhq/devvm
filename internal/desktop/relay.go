package desktop

import (
	"context"
	"crypto/des" //nolint:gosec // VNC's own authentication is DES, as the protocol has it (RFC 6143 7.2.2)
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"net"
	"slices"
	"sync"
	"time"

	"github.com/devvmhq/devvm/internal/daemon"
)

// RFB's (VNC's) security types and results (RFC 6143).
const (
	secNone    = 1
	secVncAuth = 2
	resultOK   = 0
	resultFail = 1
)

// Relay serves a VM's desktop on this machine's loopback: each connection
// a VNC viewer makes is checked against Password with VNC's own
// authentication, here, and only then carried to the VM (Dial), whose
// server it reaches without a password of its own; the viewer's clipboard
// is kept from the VM unless Clipboard.
type Relay struct {
	Password string
	// Clipboard carries the viewer's clipboard to the VM; without it the
	// relay drops the viewer's clipboard messages.
	Clipboard bool
	// Dial connects to the VM's VNC server.
	Dial func(ctx context.Context) (io.ReadWriteCloser, error)
	// Logf gets its lines: connections, refusals.
	Logf func(format string, args ...any)
	// Timeout bounds a connection's handshake, the viewer's and the
	// VM's; FailDelay is how long a wrong password holds every other
	// connection's check.
	Timeout, FailDelay time.Duration

	mu sync.Mutex // held while a password is checked, and FailDelay after a wrong one
}

// maxConns is the most connections the relay carries at once.
const maxConns = 8

// Serve carries connections to l until ctx ends or l closes. An accept error
// that leaves l open, such as running out of files, is tried again after a
// wait (daemon.AcceptRetry).
func (r *Relay) Serve(ctx context.Context, l net.Listener) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		_ = l.Close()
	}()
	slots := make(chan struct{}, maxConns)
	var wg sync.WaitGroup
	defer wg.Wait()
	var retry daemon.AcceptRetry
	for {
		conn, err := l.Accept()
		if err != nil {
			// Out of files (EMFILE) in a burst, say: the listener stays open, so the relay tries
			// again shortly, logging only the first error of a run.
			if retry.Failed(ctx, err, func(err error) { r.Logf("accepting a viewer: %v", err) }) {
				continue
			}
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		retry.Accepted()
		select {
		case slots <- struct{}{}:
		default:
			_ = conn.Close()
			r.Logf("a connection refused: %d already", maxConns)
			continue
		}
		wg.Go(func() {
			defer func() { <-slots }()
			r.handle(ctx, conn)
		})
	}
}

// handle checks a viewer's connection, then carries it to the VM until
// either side ends or ctx does.
func (r *Relay) handle(ctx context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(r.Timeout))
	minor, err := r.authenticate(conn)
	if err != nil {
		r.Logf("a connection refused: %v", err)
		return
	}
	vm, err := r.dialVM(ctx)
	if err != nil {
		r.Logf("the VM's desktop: %v", err)
		_ = result(conn, minor, false, "the VM's desktop isn't answering")
		return
	}
	defer func() { _ = vm.Close() }()
	if err := result(conn, minor, true, ""); err != nil {
		return
	}
	_ = conn.SetDeadline(time.Time{})
	r.Logf("a viewer connected")
	r.carry(ctx, conn, vm)
	r.Logf("a viewer disconnected")
}

// authenticate is the server's side of the handshake up to the result: the
// protocol version (3.3, 3.7 or 3.8; returned as its minor number), VNC
// authentication alone, and the viewer's answer checked. A wrong one is
// told so.
func (r *Relay) authenticate(conn net.Conn) (int, error) {
	if _, err := io.WriteString(conn, "RFB 003.008\n"); err != nil {
		return 0, err
	}
	minor, err := readVersion(conn)
	if err != nil {
		return 0, err
	}
	if minor == 3 {
		err = binary.Write(conn, binary.BigEndian, uint32(secVncAuth))
	} else {
		_, err = conn.Write([]byte{1, secVncAuth})
		if err == nil {
			var chosen [1]byte
			if _, err = io.ReadFull(conn, chosen[:]); err == nil && chosen[0] != secVncAuth {
				err = fmt.Errorf("security type %d asked for", chosen[0])
			}
		}
	}
	if err != nil {
		return 0, err
	}
	challenge := make([]byte, 16)
	if _, err := rand.Read(challenge); err != nil {
		return 0, err
	}
	if _, err := conn.Write(challenge); err != nil {
		return 0, err
	}
	answer := make([]byte, 16)
	if _, err := io.ReadFull(conn, answer); err != nil {
		return 0, err
	}
	if !r.check(challenge, answer) {
		_ = result(conn, minor, false, "authentication failed")
		return 0, errors.New("a wrong password")
	}
	return minor, nil
}

// check reports whether answer is challenge encrypted with the password,
// one check at a time, a wrong answer holding the next for FailDelay.
func (r *Relay) check(challenge, answer []byte) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	want, err := Response(r.Password, challenge)
	if err == nil && subtle.ConstantTimeCompare(want, answer) == 1 {
		return true
	}
	time.Sleep(r.FailDelay)
	return false
}

// Response is VNC authentication's answer to challenge (16 bytes) with
// password: the challenge encrypted with DES, its key the password's first
// 8 bytes, each byte's bits reversed.
func Response(password string, challenge []byte) ([]byte, error) {
	if len(challenge) != 16 {
		return nil, errors.New("a challenge is 16 bytes")
	}
	key := make([]byte, 8)
	copy(key, password)
	for i, b := range key {
		key[i] = bits.Reverse8(b)
	}
	c, err := des.NewCipher(key) //nolint:gosec // VNC's own authentication is DES
	if err != nil {
		return nil, err
	}
	out := make([]byte, 16)
	c.Encrypt(out[:8], challenge[:8])
	c.Encrypt(out[8:], challenge[8:])
	return out, nil
}

// result tells the viewer whether it may go on, with a reason when it may
// not, as its protocol version has it: 3.3 and 3.7 without one.
func result(conn net.Conn, minor int, ok bool, reason string) error {
	if ok {
		return binary.Write(conn, binary.BigEndian, uint32(resultOK))
	}
	b := binary.BigEndian.AppendUint32(nil, resultFail)
	if minor >= 8 {
		b = binary.BigEndian.AppendUint32(b, uint32(len(reason))) //nolint:gosec // devvm's own short reasons
		b = append(b, reason...)
	}
	_, err := conn.Write(b)
	return err
}

// readVersion reads a protocol version (RFB 003.00x\n) and returns its
// minor number: 3, 7 or 8, a later one (Apple's 3.889) read as 8.
func readVersion(r io.Reader) (int, error) {
	var v [12]byte
	if _, err := io.ReadFull(r, v[:]); err != nil {
		return 0, err
	}
	var major, minor int
	if _, err := fmt.Sscanf(string(v[:]), "RFB %03d.%03d\n", &major, &minor); err != nil || major != 3 {
		return 0, fmt.Errorf("not a VNC viewer: %q", v[:])
	}
	switch {
	case minor >= 8:
		return 8, nil
	case minor == 7:
		return 7, nil
	case minor >= 3:
		return 3, nil
	}
	return 0, fmt.Errorf("protocol version 3.%d", minor)
}

// dialVM connects to the VM's server, for as long as ctx lasts, and does
// the viewer's side of its handshake within Timeout: version 3.8, no
// authentication (the relay's was the check).
func (r *Relay) dialVM(ctx context.Context) (io.ReadWriteCloser, error) {
	vm, err := r.Dial(ctx)
	if err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- handshakeVM(vm) }()
	timer := time.NewTimer(r.Timeout)
	defer timer.Stop()
	select {
	case err = <-done:
	case <-timer.C:
		err = errors.New("its handshake timed out")
	case <-ctx.Done():
		err = ctx.Err()
	}
	if err != nil {
		_ = vm.Close()
		return nil, err
	}
	return vm, nil
}

// handshakeVM is the viewer's side of the VM server's handshake, up to
// its result.
func handshakeVM(vm io.ReadWriter) error {
	minor, err := readVersion(vm)
	if err != nil {
		return err
	}
	if minor < 8 {
		return fmt.Errorf("its server speaks 3.%d", minor)
	}
	if _, err := io.WriteString(vm, "RFB 003.008\n"); err != nil {
		return err
	}
	var n [1]byte
	if _, err := io.ReadFull(vm, n[:]); err != nil {
		return err
	}
	if n[0] == 0 {
		return errors.New("its server refused the connection")
	}
	types := make([]byte, n[0])
	if _, err := io.ReadFull(vm, types); err != nil {
		return err
	}
	if !slices.Contains(types, secNone) {
		return fmt.Errorf("its server asks for authentication (%v)", types)
	}
	if _, err := vm.Write([]byte{secNone}); err != nil {
		return err
	}
	var res uint32
	if err := binary.Read(vm, binary.BigEndian, &res); err != nil {
		return err
	}
	if res != resultOK {
		return errors.New("its server refused the connection")
	}
	return nil
}

// carry copies the viewer's messages to the VM (filtered, without
// Clipboard) and the VM's to the viewer until either side ends or ctx
// does.
func (r *Relay) carry(ctx context.Context, conn net.Conn, vm io.ReadWriteCloser) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		_ = conn.Close()
		_ = vm.Close()
	}()
	go func() {
		defer cancel()
		_, _ = io.Copy(conn, vm)
	}()
	var err error
	if r.Clipboard {
		_, err = io.Copy(vm, conn)
	} else {
		err = filter(vm, conn, r.Logf)
	}
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) && ctx.Err() == nil {
		r.Logf("the viewer's connection ended: %v", err)
	}
}
