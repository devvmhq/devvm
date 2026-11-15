package lima

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/platform"
)

const fwd = "/x/devvm _qemu connect 127.0.0.1:5"

func TestRestrictNetdev(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		guestfwd string
		want     []string // nil: refused
	}{
		{
			"Lima's network gets restrict=on and the proxy", []string{
				"-m", "4G", "-netdev", "user,id=net0,net=192.168.5.0/24,hostfwd=tcp:127.0.0.1:41083-:22", "-device", "virtio-net-pci,netdev=net0",
			}, fwd, []string{
				"-m", "4G", "-netdev", "user,id=net0,net=192.168.5.0/24,hostfwd=tcp:127.0.0.1:41083-:22,restrict=on,guestfwd=tcp:192.168.5.4:3128-cmd:" + fwd,
				"-device", "virtio-net-pci,netdev=net0",
			},
		},
		{
			"commas in the command are doubled",
			[]string{"-netdev", "user,id=net0,x=1"},
			"/a,b/devvm",
			[]string{"-netdev", "user,id=net0,x=1,restrict=on,guestfwd=tcp:192.168.5.4:3128-cmd:/a,,b/devvm"},
		},
		{
			"a build with no network: restricted, nothing forwarded",
			[]string{"-netdev", "user,id=net0,x=1"},
			"",
			[]string{"-netdev", "user,id=net0,x=1,restrict=on"},
		},
		{"two dashes", []string{"--netdev", "user,id=net0,x=1"}, "", []string{"--netdev", "user,id=net0,x=1,restrict=on"}},
		{"a probe", []string{"-accel", "help"}, fwd, []string{"-accel", "help"}},
		{"which networks QEMU has", []string{"-M", "none", "-netdev", "help"}, fwd, []string{"-M", "none", "-netdev", "help"}},
		{"no default network card", []string{"-net", "none"}, fwd, []string{"-net", "none"}},
		{"a second network", []string{"-netdev", "user,id=net0,x=1", "-netdev", "socket,id=net1,listen=:1234"}, fwd, nil},
		{"a second network first", []string{"--netdev", "socket,id=net1,listen=:1234", "-netdev", "user,id=net0,x=1"}, fwd, nil},
		{"not Lima's user network", []string{"-netdev", "tap,id=net0"}, fwd, nil},
		{"already says something about restrict", []string{"-netdev", "user,id=net0,restrict=off"}, fwd, nil},
		{"a -nic", []string{"-nic", "user"}, fwd, nil},
		{"a -net", []string{"-net", "user"}, fwd, nil},
		{"a -net at the end", []string{"-net"}, fwd, nil},
		{"a config file", []string{"-readconfig", "/x.cfg"}, fwd, nil},
		{"an option as -netdev's value", []string{"-name", "-netdev", "-netdev", "socket,id=net1"}, fwd, nil},
	} {
		got, err := RestrictNetdev(tc.args, tc.guestfwd)
		if tc.want == nil {
			if msg.IDOf(err) != "qemu_refused" {
				t.Errorf("%s: %q, %v; want it refused", tc.name, got, err)
			}
			continue
		}
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("%s: %q, %v\nwant %q", tc.name, got, err, tc.want)
		}
	}
}

// Whatever it's given, RestrictNetdev either refuses or returns at most one
// network, restricted, and nothing else changed.
func FuzzRestrictNetdev(f *testing.F) {
	f.Add("-m\x004G\x00-netdev\x00user,id=net0,net=192.168.5.0/24\x00-device\x00virtio-net-pci,netdev=net0", fwd)
	f.Add("-netdev\x00help", "")
	f.Add("--netdev\x00user,id=net0,\x00-nic\x00none", "")
	f.Fuzz(func(t *testing.T, joined, guestfwd string) {
		args := strings.Split(joined, "\x00")
		out, err := RestrictNetdev(args, guestfwd)
		if err != nil {
			return
		}
		if len(out) != len(args) {
			t.Fatalf("%q became %q", args, out)
		}
		changed := 0
		for i := range out {
			if out[i] != args[i] {
				changed++
				if !strings.HasPrefix(out[i], args[i]+",restrict=on") {
					t.Fatalf("%q became %q", args[i], out[i])
				}
			}
		}
		netdevs := 0
		for i := 0; i < len(out); i++ {
			opt, ok := option(out[i])
			if !ok || i+1 >= len(out) {
				continue
			}
			switch opt {
			case "netdev":
				i++
				if strings.Contains(out[i], "id=") {
					netdevs++
					if !strings.Contains(out[i], ",restrict=on") {
						t.Fatalf("unrestricted: %q", out)
					}
				}
			case "nic", "net":
				i++
				if out[i] != "none" {
					t.Fatalf("a network card: %q", out)
				}
			case "readconfig":
				t.Fatalf("a config file: %q", out)
			}
		}
		if netdevs > 1 || changed != netdevs {
			t.Fatalf("%d networks, %d changed: %q", netdevs, changed, out)
		}
	})
}

// The real QEMU is the first on PATH outside the wrapper's own folder; a
// relative entry names whatever folder Lima runs in, so it's skipped.
func TestFindQEMU(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	wrapper, realQEMU, cwd := filepath.Join(tmp, "wrapper"), filepath.Join(tmp, "real"), filepath.Join(tmp, "cwd")
	for _, d := range []string{wrapper, realQEMU, cwd} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "qemu-system-aarch64"), []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // a program
			t.Fatal(err)
		}
	}
	if err := os.Symlink(wrapper, filepath.Join(tmp, "link")); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)
	path := strings.Join([]string{filepath.Join(tmp, "link"), wrapper, ".", "", "cwd", realQEMU}, string(os.PathListSeparator))
	got, err := findQEMU("qemu-system-aarch64", wrapper, path)
	if err != nil || got != filepath.Join(realQEMU, "qemu-system-aarch64") {
		t.Errorf("findQEMU = %q, %v", got, err)
	}
	if _, err := findQEMU("qemu-system-aarch64", wrapper, wrapper+":."); msg.NamesOf(err) != "program_missing name=QEMU program=qemu-system-aarch64" {
		t.Errorf("none outside the wrapper: %v", err)
	}
}

// The wrapper runs devvm, quoted, with its own path and every argument.
func TestWrapper(t *testing.T) {
	dir := t.TempDir()
	w, err := writeWrapper(dir, "/opt/it's devvm/devvm", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(w) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	if want := `exec '/opt/it'\''s devvm/devvm' _qemu exec "$0" "$@"`; !strings.Contains(string(b), want) || !strings.HasPrefix(string(b), "#!/bin/sh\n") {
		t.Errorf("wrapper:\n%s", b)
	}
	if fi, err := os.Stat(w); err != nil || filepath.Base(w) != "qemu-system-x86_64" || fi.Mode().Perm() != 0o700 {
		t.Errorf("wrapper %s: %v, %v", w, fi.Mode(), err)
	}
	if qemuEnvVar("arm64") != "QEMU_SYSTEM_AARCH64" || qemuEnvVar("amd64") != "QEMU_SYSTEM_X86_64" {
		t.Error("QEMU_SYSTEM_<ARCH>")
	}
}

// Connect: everything from in reaches the server, the server answers only
// after the client is done sending (so the half-close must work), and the
// answer reaches out.
func TestConnect(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0") //nolint:noctx // a test's
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		var got bytes.Buffer
		_, _ = got.ReadFrom(c) // until the client closes its side
		_, _ = c.Write([]byte("got: " + got.String()))
	}()
	var out bytes.Buffer
	if err := Connect(ln.Addr().String(), t.TempDir(), bytes.NewBufferString("CONNECT example.com:443"), &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "got: CONNECT example.com:443" {
		t.Errorf("out = %q", out.String())
	}
	if err := Connect("127.0.0.1:1", "", bytes.NewBufferString(""), &out); err == nil {
		t.Error("connecting to a closed port should fail")
	}
}

// Connect carries connectMax connections at once, a slot each: past that
// it refuses before it connects, saying so once a minute, and a slot let
// go is taken again.
func TestConnectSlots(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0") //nolint:noctx // a test's
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	accepted := make(chan struct{}, 1)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- struct{}{}
			_ = c.Close()
		}
	}()
	dir := t.TempDir()
	var held []func()
	defer func() {
		for _, r := range held {
			r()
		}
	}()
	for range connectMax {
		r, ok, err := takeSlot(dir)
		if err != nil || !ok {
			t.Fatalf("slot %d: %v, %v", len(held), ok, err)
		}
		held = append(held, r)
	}
	var out bytes.Buffer
	err = Connect(ln.Addr().String(), dir, bytes.NewBufferString(""), &out)
	if msg.IDOf(err) != "qemu_connects_full" {
		t.Errorf("past the slots: %v, want qemu_connects_full", err)
	}
	// The client gets a 503 it can show.
	resp, rerr := http.ReadResponse(bufio.NewReader(&out), nil)
	if rerr != nil {
		t.Fatal(rerr)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || !resp.Close || !strings.Contains(string(b), strconv.Itoa(connectMax)) {
		t.Errorf("past the slots, the client got %d (closing %v) %q; want a 503 that closes and says why", resp.StatusCode, resp.Close, b)
	}
	if err := Connect(ln.Addr().String(), dir, bytes.NewBufferString(""), &out); err != nil {
		t.Errorf("again within a minute: %v, want it quiet", err)
	}
	later := time.Now().Add(connectFullNote)
	// Another refusal deciding at once (full.lock held) keeps this one quiet.
	unlock, got, lerr := platform.TryLockFile(filepath.Join(dir, "full.lock"))
	if lerr != nil || !got {
		t.Fatal(lerr)
	}
	if err := connectsFull(dir, later); err != nil {
		t.Errorf("with another refusal deciding: %v, want it quiet", err)
	}
	unlock()
	if msg.IDOf(connectsFull(dir, later)) != "qemu_connects_full" {
		t.Error("a minute on: quiet, want it said again")
	}
	select {
	case <-accepted:
		t.Error("a refused connection reached the proxy")
	default:
	}
	held[0]()
	held = held[1:]
	if err := Connect(ln.Addr().String(), dir, bytes.NewBufferString(""), &out); err != nil {
		t.Fatal(err)
	}
	<-accepted
}
