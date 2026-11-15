package desktop

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/leaktest"
)

func TestNewPassword(t *testing.T) {
	seen := map[string]bool{}
	for range 50 {
		p, err := NewPassword()
		if err != nil {
			t.Fatal(err)
		}
		if !ValidPassword(p) {
			t.Fatalf("NewPassword() = %q, not valid", p)
		}
		seen[p] = true
	}
	if len(seen) < 50 {
		t.Errorf("50 passwords, %d different", len(seen))
	}
	for _, bad := range []string{"", "abcdefg", "abcdefghj", "abcdefg0", "ABCDEFGH", "abcd efg"} {
		if ValidPassword(bad) {
			t.Errorf("ValidPassword(%q) = true", bad)
		}
	}
}

func TestResponse(t *testing.T) {
	ch := bytes.Repeat([]byte{7}, 16)
	a, err := Response("abcdefgh", ch)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Response("abcdefgj", ch)
	// Only the first 8 characters count, as VNC has it.
	c, _ := Response("abcdefghxyz", ch)
	if len(a) != 16 || bytes.Equal(a, b) || !bytes.Equal(a, c) {
		t.Errorf("Response: %x %x %x", a, b, c)
	}
	if _, err := Response("x", ch[:8]); err == nil {
		t.Error("an 8-byte challenge: no error")
	}
}

// fakeVM is the VM's server: no authentication, then a ServerInit
// ("INIT"), then everything it reads goes to got.
type fakeVM struct {
	types []byte // the security types it offers; None without
	got   bytes.Buffer
	dials atomic.Int32
	done  chan struct{}
}

func (f *fakeVM) dial(_ context.Context) (io.ReadWriteCloser, error) {
	f.dials.Add(1)
	relay, vm := net.Pipe()
	go func() {
		defer close(f.done)
		defer func() { _ = vm.Close() }()
		_, _ = io.WriteString(vm, "RFB 003.008\n")
		v := make([]byte, 12)
		if _, err := io.ReadFull(vm, v); err != nil {
			return
		}
		types := f.types
		if types == nil {
			types = []byte{secNone}
		}
		_, _ = vm.Write(append([]byte{byte(len(types))}, types...)) //nolint:gosec // a test's one or two types
		c := make([]byte, 1)
		if _, err := io.ReadFull(vm, c); err != nil || c[0] != secNone {
			return
		}
		_, _ = vm.Write([]byte{0, 0, 0, 0})
		_, _ = io.WriteString(vm, "INIT")
		_, _ = io.Copy(&f.got, vm)
	}()
	return relay, nil
}

// serve runs a relay on a loopback port for the test, returning its
// address. Once the test ends, nothing of the relay's runs on.
func serve(t *testing.T, r *Relay) string {
	t.Helper()
	leaktest.Check(t)
	var lc net.ListenConfig
	l, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = r.Serve(ctx, l)
	}()
	t.Cleanup(func() { cancel(); <-done })
	return l.Addr().String()
}

// dial connects to the relay at addr, on this machine's loopback.
func dial(t *testing.T, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(t.Context(), "tcp", addr)
}

func newRelay(vm *fakeVM, clipboard bool) *Relay {
	return &Relay{
		Password: "abcdefgh", Clipboard: clipboard, Dial: vm.dial, Logf: func(string, ...any) {},
		Timeout: 5 * time.Second, FailDelay: 10 * time.Millisecond,
	}
}

// viewer connects as a VNC viewer with version, answering with password,
// and returns the connection and the server's result.
func viewer(t *testing.T, addr, version, password string) (net.Conn, uint32, string) {
	t.Helper()
	c, err := dial(t, addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	v := make([]byte, 12)
	if _, err := io.ReadFull(c, v); err != nil || string(v) != "RFB 003.008\n" {
		t.Fatalf("version %q, %v", v, err)
	}
	_, _ = io.WriteString(c, version)
	if version == "RFB 003.003\n" {
		var sec uint32
		if err := binary.Read(c, binary.BigEndian, &sec); err != nil || sec != secVncAuth {
			t.Fatalf("security type %d, %v", sec, err)
		}
	} else {
		types := make([]byte, 2)
		if _, err := io.ReadFull(c, types); err != nil || !bytes.Equal(types, []byte{1, secVncAuth}) {
			t.Fatalf("security types %v, %v", types, err)
		}
		_, _ = c.Write([]byte{secVncAuth})
	}
	ch := make([]byte, 16)
	if _, err := io.ReadFull(c, ch); err != nil {
		t.Fatal(err)
	}
	ans, _ := Response(password, ch)
	_, _ = c.Write(ans)
	var res uint32
	if err := binary.Read(c, binary.BigEndian, &res); err != nil {
		t.Fatal(err)
	}
	reason := ""
	if res != resultOK && version == "RFB 003.008\n" {
		var n uint32
		_ = binary.Read(c, binary.BigEndian, &n)
		b := make([]byte, n)
		_, _ = io.ReadFull(c, b)
		reason = string(b)
	}
	return c, res, reason
}

func TestRelayWrongPassword(t *testing.T) {
	vm := &fakeVM{done: make(chan struct{})}
	addr := serve(t, newRelay(vm, false))
	for _, v := range []string{"RFB 003.008\n", "RFB 003.003\n", "RFB 003.007\n"} {
		c, res, reason := viewer(t, addr, v, "abcdefgj")
		if res != resultFail {
			t.Errorf("%q: result %d", v, res)
		}
		if v == "RFB 003.008\n" && reason != "authentication failed" {
			t.Errorf("reason %q", reason)
		}
		if _, err := c.Read(make([]byte, 1)); err == nil {
			t.Errorf("%q: the connection stays open", v)
		}
	}
	if n := vm.dials.Load(); n != 0 {
		t.Errorf("the VM dialled %d times for wrong passwords", n)
	}
}

func TestRelayNotAViewer(t *testing.T) {
	vm := &fakeVM{done: make(chan struct{})}
	addr := serve(t, newRelay(vm, false))
	c, err := dial(t, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = io.ReadFull(c, make([]byte, 12))
	_, _ = io.WriteString(c, "GET / HTTP/1.")
	_, _ = io.ReadAll(c) // ended, or reset
	if vm.dials.Load() != 0 {
		t.Error("the VM dialled")
	}
}

// messages are a viewer's, after ClientInit: a key event, a clipboard,
// an extended clipboard one, a pointer event.
func messages() (all, noClipboard []byte) {
	key := []byte{msgKeyEvent, 1, 0, 0, 0, 0, 0, 0x61}
	clip := append([]byte{msgClientCutText, 0, 0, 0, 0, 0, 0, 6}, "secret"...)
	ext := []byte{msgClientCutText, 0, 0, 0, 0xff, 0xff, 0xff, 0xfc, 1, 2, 3, 4}
	ptr := []byte{msgPointerEvent, 0, 0, 1, 0, 2}
	enc := []byte{msgSetEncodings, 0, 0, 2, 0, 0, 0, 0, 0xff, 0xff, 0xfe, 0xfe}
	fence := []byte{msgFence, 0, 0, 0, 0, 0, 0, 1, 2, 9, 9}
	all = bytes.Join([][]byte{{1}, key, clip, ext, ptr, enc, fence}, nil)
	noClipboard = bytes.Join([][]byte{{1}, key, ptr, enc, fence}, nil)
	return all, noClipboard
}

func TestRelayClipboard(t *testing.T) {
	all, noClip := messages()
	for _, clipboard := range []bool{false, true} {
		vm := &fakeVM{done: make(chan struct{})}
		addr := serve(t, newRelay(vm, clipboard))
		c, res, _ := viewer(t, addr, "RFB 003.008\n", "abcdefgh")
		if res != resultOK {
			t.Fatalf("result %d", res)
		}
		init := make([]byte, 4)
		if _, err := io.ReadFull(c, init); err != nil || string(init) != "INIT" {
			t.Fatalf("ServerInit %q, %v", init, err)
		}
		_, _ = c.Write(all)
		_ = c.Close()
		<-vm.done
		want := noClip
		if clipboard {
			want = all
		}
		if !bytes.Equal(vm.got.Bytes(), want) {
			t.Errorf("clipboard %v: the VM got %v, want %v", clipboard, vm.got.Bytes(), want)
		}
	}
}

func TestRelayUnknownMessage(t *testing.T) {
	vm := &fakeVM{done: make(chan struct{})}
	addr := serve(t, newRelay(vm, false))
	c, _, _ := viewer(t, addr, "RFB 003.008\n", "abcdefgh")
	_, _ = io.ReadFull(c, make([]byte, 4))
	key := []byte{msgKeyEvent, 1, 0, 0, 0, 0, 0, 0x61}
	_, _ = c.Write(bytes.Join([][]byte{{1}, key, {99, 1, 2, 3}, key}, nil))
	<-vm.done
	if want := append([]byte{1}, key...); !bytes.Equal(vm.got.Bytes(), want) {
		t.Errorf("the VM got %v, want %v", vm.got.Bytes(), want)
	}
}

func TestRelayVMAsksForPassword(t *testing.T) {
	vm := &fakeVM{types: []byte{secVncAuth}, done: make(chan struct{})}
	addr := serve(t, newRelay(vm, false))
	_, res, reason := viewer(t, addr, "RFB 003.008\n", "abcdefgh")
	if res != resultFail || !strings.Contains(reason, "isn't answering") {
		t.Errorf("result %d %q", res, reason)
	}
}
