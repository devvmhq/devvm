package memnet

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/devvmhq/devvm/internal/leaktest"
)

// TestMain fails the tests if a goroutine of theirs outlives them.
func TestMain(m *testing.M) { leaktest.Main(m) }

// A connection carries each end's bytes to the other, in order; one end
// closing its writing side alone gives the other EOF once it has read
// what was sent, and it can still answer; closing gives the other end
// EOF and fails its writes; a closed end can't be used.
func TestPipe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, b := Pipe()
		go func() {
			_, _ = a.Write([]byte("hello"))
			_ = a.CloseWrite()
		}()
		if got, err := io.ReadAll(b); string(got) != "hello" || err != nil {
			t.Errorf("read %q, %v", got, err)
		}
		if _, err := a.Write([]byte("x")); !errors.Is(err, syscall.EPIPE) {
			t.Errorf("a write after CloseWrite: %v", err)
		}
		go func() { _, _ = b.Write([]byte("answer")) }()
		buf := make([]byte, 6)
		if _, err := io.ReadFull(a, buf); string(buf) != "answer" || err != nil {
			t.Errorf("the answer after a half close: %q, %v", buf, err)
		}
		_ = b.Close()
		if _, err := a.Read(buf); !errors.Is(err, io.EOF) {
			t.Errorf("a read once the other end closed: %v", err)
		}
		if _, err := a.Write([]byte("x")); !errors.Is(err, syscall.EPIPE) {
			t.Errorf("a write once the other end closed: %v", err)
		}
		if _, err := b.Read(buf); !errors.Is(err, net.ErrClosed) {
			t.Errorf("a read on a closed end: %v", err)
		}
		if err := b.Close(); !errors.Is(err, net.ErrClosed) {
			t.Errorf("closed twice: %v", err)
		}
		if a.LocalAddr().String() != "a" || a.RemoteAddr().String() != "b" || b.LocalAddr().Network() != "mem" {
			t.Errorf("addresses %s, %s, %s", a.LocalAddr(), a.RemoteAddr(), b.LocalAddr().Network())
		}
	})
}

// A writer waits once what the other end hasn't read fills its buffer,
// and goes on as it reads; a write that waits on a closed reader fails.
func TestPipeFull(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, b := Pipe()
		done := make(chan int)
		go func() {
			n, _ := a.Write(make([]byte, bufMax+10))
			done <- n
		}()
		synctest.Wait()
		select {
		case n := <-done:
			t.Fatalf("a write past the buffer returned %d without a read", n)
		default:
		}
		if _, err := io.ReadFull(b, make([]byte, 10)); err != nil {
			t.Fatal(err)
		}
		if n := <-done; n != bufMax+10 {
			t.Errorf("wrote %d", n)
		}
		errc := make(chan error)
		go func() {
			_, err := a.Write(make([]byte, 2*bufMax))
			errc <- err
		}()
		synctest.Wait()
		_ = b.Close()
		if err := <-errc; !errors.Is(err, syscall.EPIPE) {
			t.Errorf("a waiting write once the reader closed: %v", err)
		}
	})
}

// A read or a write waits until its deadline, to the moment, and a
// deadline moved or cleared while it waits holds from then on.
func TestPipeDeadlines(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, b := Pipe()
		start := time.Now()
		_ = a.SetReadDeadline(start.Add(time.Second))
		if _, err := a.Read(make([]byte, 1)); !errors.Is(err, os.ErrDeadlineExceeded) || time.Since(start) != time.Second {
			t.Errorf("read: %v after %s", err, time.Since(start))
		}
		_ = a.SetDeadline(time.Time{})
		got := make(chan error)
		go func() {
			_, err := a.Read(make([]byte, 1))
			got <- err
		}()
		time.Sleep(time.Hour)
		synctest.Wait()
		select {
		case err := <-got:
			t.Fatalf("a read without a deadline ended: %v", err)
		default:
		}
		_ = a.SetReadDeadline(time.Now().Add(time.Minute))
		start = time.Now()
		if err := <-got; !errors.Is(err, os.ErrDeadlineExceeded) || time.Since(start) != time.Minute {
			t.Errorf("a deadline set while it waits: %v after %s", err, time.Since(start))
		}
		_ = a.SetWriteDeadline(time.Now().Add(time.Second))
		start = time.Now()
		n, err := a.Write(make([]byte, bufMax+1))
		if n != bufMax || !errors.Is(err, os.ErrDeadlineExceeded) || time.Since(start) != time.Second {
			t.Errorf("write: %d, %v after %s", n, err, time.Since(start))
		}
		_ = b.Close()
		_ = a.Close()
	})
}

// A deadline that has passed fails a read or a write at once, as TCP's
// does, though there's data to read or room to write; a read of no bytes
// returns at once, whatever waits.
func TestPipePastDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, b := Pipe()
		if _, err := b.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
		if n, err := a.Read(nil); n != 0 || err != nil {
			t.Errorf("a read of no bytes: %d, %v", n, err)
		}
		_ = a.SetDeadline(time.Now())
		if n, err := a.Read(make([]byte, 1)); n != 0 || !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Errorf("a read with data, past its deadline: %d, %v", n, err)
		}
		if n, err := a.Write([]byte("y")); n != 0 || !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Errorf("a write with room, past its deadline: %d, %v", n, err)
		}
		_ = a.SetDeadline(time.Time{})
		got := make([]byte, 1)
		if n, err := a.Read(got); n != 1 || err != nil || got[0] != 'x' {
			t.Errorf("the data, once the deadline is cleared: %q, %v", got[:n], err)
		}
		_ = a.Close()
		_ = b.Close()
	})
}

// A network's listeners take the dials to their address, in order, one
// address one listener; a dial to none, or to a closed one, is refused,
// and a closed listener's waiting dials are closed.
func TestNetwork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var n Network
		l, err := n.Listen("127.0.0.1:80")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := n.Listen("127.0.0.1:80"); !errors.Is(err, syscall.EADDRINUSE) {
			t.Errorf("a second listener: %v", err)
		}
		if _, err := n.Dial("127.0.0.1:81"); !errors.Is(err, syscall.ECONNREFUSED) {
			t.Errorf("a dial to none: %v", err)
		}
		if !n.Listening("127.0.0.1:80") || l.Addr().String() != "127.0.0.1:80" {
			t.Error("not listening")
		}
		go func() {
			for _, s := range []string{"one", "two"} {
				c, err := n.DialContext(t.Context(), "tcp", "127.0.0.1:80")
				if err != nil {
					t.Error(err)
					return
				}
				_, _ = io.WriteString(c, s)
				_ = c.Close()
			}
		}()
		for _, want := range []string{"one", "two"} {
			c, err := l.Accept()
			if err != nil {
				t.Fatal(err)
			}
			if got, _ := io.ReadAll(c); string(got) != want {
				t.Errorf("accepted %q, want %q", got, want)
			}
			_ = c.Close()
		}
		waiting, err := n.Dial("127.0.0.1:80")
		if err != nil {
			t.Fatal(err)
		}
		_ = l.Close()
		if _, err := waiting.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
			t.Errorf("a dial not accepted, once its listener closed: %v", err)
		}
		if _, err := l.Accept(); !errors.Is(err, net.ErrClosed) {
			t.Errorf("accept once closed: %v", err)
		}
		if _, err := l.Dial(); !errors.Is(err, syscall.ECONNREFUSED) {
			t.Errorf("a dial once closed: %v", err)
		}
		if n.Listening("127.0.0.1:80") {
			t.Error("closed, still listening")
		}
		if _, err := n.Listen("127.0.0.1:80"); err != nil {
			t.Errorf("listening again once closed: %v", err)
		}
	})
}

// Accept waits until its deadline, to the moment; a deadline cleared
// waits for a dial.
func TestListenerDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var n Network
		l, _ := n.Listen("s")
		start := time.Now()
		_ = l.SetDeadline(start.Add(5 * time.Millisecond))
		if _, err := l.Accept(); !errors.Is(err, os.ErrDeadlineExceeded) || time.Since(start) != 5*time.Millisecond {
			t.Errorf("accept: %v after %s", err, time.Since(start))
		}
		_ = l.SetDeadline(time.Time{})
		go func() {
			time.Sleep(time.Hour)
			c, _ := l.Dial()
			_, _ = c.Write([]byte("late"))
			_ = c.Close()
		}()
		c, err := l.Accept()
		if err != nil || time.Since(start) != time.Hour+5*time.Millisecond {
			t.Fatalf("accept without a deadline: %v after %s", err, time.Since(start))
		}
		if got, _ := io.ReadAll(c); !bytes.Equal(got, []byte("late")) {
			t.Errorf("read %q", got)
		}
		_ = l.Close()
	})
}
