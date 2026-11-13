package vmssh

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/daemon"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/state"
)

// ports.sh's report as the forwarder reads it: each port to the address
// in the VM its connections go to, loopback alone.
func TestListeners(t *testing.T) {
	got := listeners([]string{
		"0100007F:0BB8",                            // 127.0.0.1:3000
		"00000000:1F90",                            // 0.0.0.0:8080
		"00000000000000000000000001000000:1F90",    // [::1]:8080: IPv4 first
		"00000000000000000000000001000000:1435",    // [::1]:5173 alone
		"00000000000000000000000000000000:2328",    // [::]:9000
		"0000000000000000FFFF00000100007F:2329",    // [::ffff:127.0.0.1]:9001
		"0200007F:232A",                            // 127.0.0.2:9002
		"0100A8C0:232B",                            // 192.168.0.1:9003: not loopback
		"0000000000000000FFFF00000100A8C0:232C",    // [::ffff:192.168.0.1]:9004
		"0100007F:0000", "zz:0BB8", "0100007F", "", // not addresses
		"0100007F:00016", "01007F:0BB8",
		// links' listeners, in 127.77.0.0/16: never forwarded
		"01004D7F:1538",                         // 127.77.0.1:5432 alone
		"0000000000000000FFFF0000FEFF4D7F:1539", // [::ffff:127.77.255.254]:5433
		"02004D7F:0050",                         // 127.77.0.2:80
		"01004D7F:0BB8",                         // 127.77.0.1:3000, beside the VM's own 127.0.0.1:3000
		"01004E7F:153A",                         // 127.78.0.1:5434: not a link's
		"01004C7F:153B",                         // 127.76.0.1:5435: not a link's
		// the VM's local proxy, on 127.0.77.1: never forwarded, on any port
		"014D007F:0C38",                         // 127.0.77.1:3128
		"0000000000000000FFFF0000014D007F:153C", // [::ffff:127.0.77.1]:5436
		// transparent mode's listeners and its dnsmasq, on the stand-in address: never forwarded
		"010012C6:F003",                         // 198.18.0.1:61443
		"010012C6:0035",                         // 198.18.0.1:53
		"0000000000000000FFFF0000010012C6:EE98", // [::ffff:198.18.0.1]:61080
	})
	want := map[int]string{
		3000: "127.0.0.1", 8080: "127.0.0.1", 5173: "::1", 9000: "::1", 9001: "127.0.0.1", 9002: "127.0.0.2",
		5434: "127.78.0.1", 5435: "127.76.0.1",
	}
	if !maps.Equal(got, want) {
		t.Errorf("listeners = %v, want %v", got, want)
	}
}

// freePorts returns n ports free on the host's loopback now.
func freePorts(t *testing.T, n int) []int {
	t.Helper()
	var out []int
	for len(out) < n {
		l, err := net.Listen("tcp", "127.0.0.1:0") //nolint:noctx // a test's moment
		if err != nil {
			t.Fatal(err)
		}
		p := l.Addr().(*net.TCPAddr).Port
		_ = l.Close()
		if p >= minForwardPort && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// procLine is a listener on port as /proc/net/tcp has it.
func procLine(addr string, port int) string { return fmt.Sprintf("%s:%04X", addr, port) }

// fakeVM is a VM as the forwarder sees it: its reports, written by the
// test, one connection at a time.
type fakeVM struct {
	mu      sync.Mutex
	reports *io.PipeWriter
	watches int
	ready   chan struct{}
}

func (v *fakeVM) watch(ctx context.Context) (io.ReadCloser, func() error, error) {
	r, w := io.Pipe()
	v.mu.Lock()
	v.reports = w
	v.watches++
	v.mu.Unlock()
	go func() { <-ctx.Done(); _ = w.CloseWithError(ctx.Err()) }()
	v.ready <- struct{}{}
	return r, func() error { return nil }, nil
}

func (v *fakeVM) report(t *testing.T, lines ...string) {
	t.Helper()
	v.mu.Lock()
	w := v.reports
	v.mu.Unlock()
	if _, err := io.WriteString(w, strings.Join(append(lines, "."), "\n")+"\n"); err != nil {
		t.Fatal(err)
	}
}

func (v *fakeVM) drop() {
	v.mu.Lock()
	_ = v.reports.Close()
	v.mu.Unlock()
}

// reach connects to a host loopback port and returns what comes back.
func reach(t *testing.T, addr string) string {
	d := net.Dialer{Timeout: time.Second}
	c, err := d.DialContext(t.Context(), "tcp", addr)
	if err != nil {
		return "refused"
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	line, _ := bufio.NewReader(c).ReadString('\n')
	return strings.TrimSpace(line)
}

// eventually waits for addr to answer want.
func eventually(t *testing.T, addr, want string) {
	t.Helper()
	var got string
	for range 100 {
		if got = reach(t, addr); got == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("%s: %q, want %q", addr, got, want)
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(b)
}

// echo is a relay that answers where the connection would go.
func echo(_ context.Context, conn net.Conn, h string, p int) {
	_, _ = fmt.Fprintf(conn, "to %s\n", net.JoinHostPort(h, strconv.Itoa(p)))
	_ = conn.Close()
}

// The forwarder listens on the host's loopback for what the VM reports,
// from 1024 up, never on a reserved port or one the host has on any
// address; forwards go once the VM's listener does, and all of them when
// the connection ends, until it's made again; it ends once the VM no
// longer runs as it did.
func TestForwarder(t *testing.T) {
	ports := freePorts(t, 5)
	open, v6only, reserved, hosts, gone := ports[0], ports[1], ports[2], ports[3], ports[4]
	// The host's own program, on every address.
	host, err := net.Listen("tcp", ":"+strconv.Itoa(hosts)) //nolint:noctx // the test's
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = host.Close() }()
	var alive atomic.Bool
	alive.Store(true)
	vm := &fakeVM{ready: make(chan struct{}, 4)}
	var log bytes.Buffer
	var logMu sync.Mutex
	f := &Forwarder{
		VM: "devvm-f", Tick: 10 * time.Millisecond, Every: 10 * time.Millisecond, Log: lockedWriter{&logMu, &log},
		Alive:    func(context.Context) bool { return alive.Load() },
		Reserved: func() (map[int]bool, error) { return map[int]bool{reserved: true}, nil },
		Watch:    vm.watch, Relay: echo, Listen: platform.ListenLoopback,
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- f.Run(ctx) }()
	<-vm.ready
	vm.report(t,
		procLine("0100007F", open), procLine("00000000000000000000000001000000", v6only),
		procLine("00000000", reserved), procLine("00000000", hosts), procLine("00000000", gone),
		procLine("00000000", 80), procLine("0100A8C0", 9999),
	)
	at := func(p int) string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(p)) }
	eventually(t, at(open), "to "+at(open))
	eventually(t, net.JoinHostPort("::1", strconv.Itoa(open)), "to "+at(open))
	eventually(t, at(v6only), "to "+net.JoinHostPort("::1", strconv.Itoa(v6only)))
	eventually(t, at(gone), "to "+at(gone))
	if got := reach(t, at(reserved)); got != "refused" {
		t.Errorf("reserved port: %q", got)
	}
	if got := reach(t, at(hosts)); strings.HasPrefix(got, "to ") {
		t.Errorf("the host's port taken: %q", got)
	}
	// The VM's listener goes: so does its forward.
	vm.report(t, procLine("0100007F", open))
	eventually(t, at(gone), "refused")
	eventually(t, at(open), "to "+at(open))
	// The connection ends: every forward goes, then it's made again.
	vm.drop()
	<-vm.ready
	eventually(t, at(open), "refused")
	vm.report(t, procLine("00000000", open))
	eventually(t, at(open), "to "+at(open))
	// The VM no longer runs as it did (started again): it ends.
	alive.Store(false)
	select {
	case err := <-done:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(5 * time.Second):
		t.Error("still running with its VM gone")
	}
	cancel()
	eventually(t, at(open), "refused")
	logMu.Lock()
	defer logMu.Unlock()
	for _, want := range []string{"forwarding devvm-f's ports", fmt.Sprintf("port %d: not forwarded", hosts), fmt.Sprintf("port %d: forwarded", open), "devvm-f stopped"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, log.String())
		}
	}
}

// A VM that isn't running gets no forwarder; one whose connection ends
// because it stopped ends it at once, without waiting for Every.
func TestForwarderAliveOnEnd(t *testing.T) {
	f := &Forwarder{VM: "devvm-f", Alive: func(context.Context) bool { return false }}
	if err := f.Run(t.Context()); err == nil {
		t.Error("ran for a VM that isn't running")
	}
	var calls atomic.Int32
	vm := &fakeVM{ready: make(chan struct{}, 4)}
	f = &Forwarder{
		VM: "devvm-f", Tick: time.Hour, Every: time.Hour, Log: io.Discard,
		Alive:    func(context.Context) bool { return calls.Add(1) <= 2 },
		Reserved: func() (map[int]bool, error) { return nil, nil },
		Watch:    vm.watch, Listen: platform.ListenLoopback,
	}
	done := make(chan error, 1)
	go func() { done <- f.Run(t.Context()) }()
	<-vm.ready
	vm.drop()
	select {
	case err := <-done:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(5 * time.Second):
		t.Error("still running with its connection ended and its VM gone")
	}
}

// With Only (listed ports), the forwarder forwards those alone, below
// 1024 too, and nothing else the VM listens on.
func TestForwarderOnly(t *testing.T) {
	ports := freePorts(t, 3)
	listed, other, reserved := ports[0], ports[1], ports[2]
	vm := &fakeVM{ready: make(chan struct{}, 4)}
	f := &Forwarder{
		VM: "devvm-f", Tick: 10 * time.Millisecond, Every: time.Hour, Log: io.Discard,
		Only:     []backend.PortRange{{From: listed, To: listed}, {From: reserved, To: reserved}, {From: 80, To: 80}},
		Alive:    func(context.Context) bool { return true },
		Reserved: func() (map[int]bool, error) { return map[int]bool{reserved: true}, nil },
		Watch:    vm.watch, Relay: echo, Listen: platform.ListenLoopback,
		ports: map[int]*forward{}, skipped: map[int]failure{},
	}
	if !f.wanted(80) || f.wanted(other) || !f.wanted(listed) {
		t.Error("wanted: not the listed ports alone")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = f.Run(ctx) }()
	<-vm.ready
	vm.report(t, procLine("00000000", listed), procLine("00000000", other), procLine("00000000", reserved))
	at := func(p int) string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(p)) }
	eventually(t, at(listed), "to "+at(listed))
	if got := reach(t, at(other)); got != "refused" {
		t.Errorf("an unlisted port: %q", got)
	}
	if got := reach(t, at(reserved)); got != "refused" {
		t.Errorf("a listed port devvm reserves: %q", got)
	}
}

// A VM that reports faster than Gap waits: reports are applied Gap apart.
func TestForwarderGap(t *testing.T) {
	vm := &fakeVM{ready: make(chan struct{}, 4)}
	var mu sync.Mutex
	var applied []time.Time
	gap := 100 * time.Millisecond
	f := &Forwarder{
		VM: "devvm-f", Tick: 10 * time.Millisecond, Every: time.Hour, Gap: gap, Log: io.Discard,
		Alive: func(context.Context) bool { return true },
		Reserved: func() (map[int]bool, error) {
			mu.Lock()
			defer mu.Unlock()
			applied = append(applied, time.Now())
			return nil, nil
		},
		Watch: vm.watch, Listen: platform.ListenLoopback,
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = f.Run(ctx) }()
	<-vm.ready
	for range 5 {
		vm.report(t)
	}
	time.Sleep(gap)
	mu.Lock()
	defer mu.Unlock()
	if len(applied) < 4 {
		t.Fatalf("%d reports applied", len(applied))
	}
	for i := 1; i < len(applied); i++ {
		if d := applied[i].Sub(applied[i-1]); d < gap-10*time.Millisecond {
			t.Errorf("reports %d and %d applied %v apart", i-1, i, d)
		}
	}
}

// What the VM makes the forwarder log is limited: a burst, then one write
// per logEvery, those dropped counted; control characters dropped, C1
// included, long writes cut.
func TestLogLimit(t *testing.T) {
	var b bytes.Buffer
	now := time.Unix(1000, 0)
	l := newLogLimit(&b, func() time.Time { return now })
	for range logBurst + 50 {
		_, _ = l.Write([]byte("x\n"))
	}
	if got := strings.Count(b.String(), "x\n"); got != logBurst {
		t.Errorf("%d writes logged, want %d", got, logBurst)
	}
	b.Reset()
	now = now.Add(2 * logEvery)
	_, _ = l.Write([]byte("a\x1b[2Jb\302\233c\x9bd\r\n"))
	_, _ = l.Write([]byte(strings.Repeat("y", logWriteMax+10)))
	_, _ = l.Write([]byte("dropped\n"))
	want := "devvm ports: 50 writes not logged\na[2Jbcd\n" + strings.Repeat("y", logWriteMax) + "…\n"
	if b.String() != want {
		t.Errorf("log:\n%q\nwant\n%q", b.String(), want)
	}
}

// While devvm's ports can't be read, nothing new is forwarded.
func TestForwarderReservedUnknown(t *testing.T) {
	p := freePorts(t, 1)[0]
	var log bytes.Buffer
	f := &Forwarder{
		VM: "devvm-f", Log: &log, ports: map[int]*forward{}, skipped: map[int]failure{},
		Reserved: func() (map[int]bool, error) { return nil, errors.New("unreadable") },
		Listen:   platform.ListenLoopback,
	}
	f.apply(t.Context(), map[int]string{p: "127.0.0.1"})
	if len(f.ports) != 0 || !strings.Contains(log.String(), "no new ports forwarded") {
		t.Errorf("forwarded %v; log %q", f.ports, log.String())
	}
}

// A forward whose port devvm has since reserved goes at the next report,
// though the VM still listens there; while devvm's ports can't be read,
// the forwards it has stay.
func TestForwarderReservedLater(t *testing.T) {
	p := freePorts(t, 1)[0]
	var log bytes.Buffer
	reserved := map[int]bool{}
	var rerr error
	f := &Forwarder{
		VM: "devvm-f", Log: &log, ports: map[int]*forward{}, skipped: map[int]failure{},
		Reserved: func() (map[int]bool, error) { return reserved, rerr },
		Listen:   platform.ListenLoopback,
	}
	defer f.closeAll()
	want := map[int]string{p: "127.0.0.1"}
	f.apply(t.Context(), want)
	if f.ports[p] == nil {
		t.Fatalf("not forwarded: %s", log.String())
	}
	rerr = errors.New("unreadable")
	f.apply(t.Context(), want)
	if f.ports[p] == nil {
		t.Fatal("a forward dropped while devvm's ports are unknown")
	}
	reserved, rerr = map[int]bool{p: true}, nil
	f.apply(t.Context(), want)
	if f.ports[p] != nil {
		t.Errorf("a reserved port still forwarded")
	}
	if got := reach(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(p))); got != "refused" {
		t.Errorf("a reserved port: %q", got)
	}
	if !strings.Contains(log.String(), fmt.Sprintf("port %d: no longer forwarded", p)) {
		t.Errorf("log: %q", log.String())
	}
}

// A forward whose port devvm reserves goes on the host's own clock, though
// the VM never reports again (it holds its connection open, silent): a VM
// that forwards every port can't keep another VM's listed port by going
// quiet. The forwarder notes it forwards nothing when it starts, then each
// change.
func TestForwarderGivesUpSilently(t *testing.T) {
	p := freePorts(t, 1)[0]
	vm := &fakeVM{ready: make(chan struct{}, 4)}
	var mu sync.Mutex
	reserved := map[int]bool{}
	var notes [][]int
	f := &Forwarder{
		VM: "devvm-f", Tick: 10 * time.Millisecond, Every: 20 * time.Millisecond, Log: io.Discard,
		Alive: func(context.Context) bool { return true },
		Reserved: func() (map[int]bool, error) {
			mu.Lock()
			defer mu.Unlock()
			return maps.Clone(reserved), nil
		},
		Note: func(ports []int, _ failedPorts) {
			mu.Lock()
			defer mu.Unlock()
			notes = append(notes, ports)
		},
		Watch: vm.watch, Relay: echo, Listen: platform.ListenLoopback,
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = f.Run(ctx) }()
	<-vm.ready
	vm.report(t, procLine("00000000", p))
	at := net.JoinHostPort("127.0.0.1", strconv.Itoa(p))
	eventually(t, at, "to "+at)
	// Another VM now lists it; the VM says nothing more.
	mu.Lock()
	reserved[p] = true
	mu.Unlock()
	eventually(t, at, "refused")
	mu.Lock()
	defer mu.Unlock()
	if !slices.EqualFunc(notes, [][]int{nil, {p}, {}}, slices.Equal) {
		t.Errorf("noted %v", notes)
	}
}

// A forward that goes takes its connections with it: a browser's
// keep-alive connection, opened while the forward was up, ends when the
// port is given up, so its later requests never reach this VM.
func TestForwarderClosesConnections(t *testing.T) {
	p := freePorts(t, 1)[0]
	var reserved map[int]bool
	accepted := make(chan struct{}, 1)
	f := &Forwarder{
		VM: "devvm-f", Log: io.Discard, ports: map[int]*forward{}, skipped: map[int]failure{}, relays: make(chan struct{}, maxRelays),
		Reserved: func() (map[int]bool, error) { return reserved, nil },
		Relay: func(_ context.Context, conn net.Conn, _ string, _ int) {
			accepted <- struct{}{}
			_, _ = io.Copy(io.Discard, conn) // until it's closed
			_ = conn.Close()
		},
		Listen: platform.ListenLoopback,
	}
	defer f.closeAll()
	f.apply(t.Context(), map[int]string{p: "127.0.0.1"})
	d := net.Dialer{Timeout: time.Second}
	c, err := d.DialContext(t.Context(), "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	<-accepted
	reserved = map[int]bool{p: true}
	f.release()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, io.EOF) && !errors.Is(err, syscall.ECONNRESET) {
		t.Errorf("a connection accepted before the forward went: %v", err)
	}
}

// The forwarder records why it couldn't take each port: a program's
// (EADDRINUSE), past its limit of forwards, or refused by the host
// (EACCES, EPERM), such as below 1024 on Linux. A port in use or refused
// is tried again only once the VM's listener goes and comes back; one
// past the limit at the next report once the forwarder is below it. Any
// other error (EMFILE: out of descriptors for now) is logged alone, not
// recorded, and tried again at the next report.
func TestForwarderFailures(t *testing.T) {
	const refused, denied, inUse, busy = 80, 81, 5000, 7000
	limited := freePorts(t, 1)[0]
	var notes []failedPorts
	listens := map[int]int{}
	f := &Forwarder{
		VM: "devvm-f", Log: io.Discard, ports: map[int]*forward{}, skipped: map[int]failure{},
		Only:     []backend.PortRange{{From: 1, To: 65535}}, // 80 listed
		Reserved: func() (map[int]bool, error) { return nil, nil },
		Note:     func(_ []int, failed failedPorts) { notes = append(notes, failed) },
		Listen: func(ctx context.Context, addr string) (net.Listener, error) {
			_, ps, _ := net.SplitHostPort(addr)
			p, _ := strconv.Atoi(ps)
			listens[p]++
			bind := func(errno syscall.Errno) error {
				return &net.OpError{Op: "listen", Net: "tcp", Err: os.NewSyscallError("bind", errno)}
			}
			switch p {
			case refused:
				return nil, bind(syscall.EACCES)
			case denied:
				return nil, bind(syscall.EPERM)
			case inUse:
				return nil, bind(syscall.EADDRINUSE)
			case busy:
				return nil, bind(syscall.EMFILE)
			case limited:
				return platform.ListenLoopback(ctx, addr)
			}
			return nil, fmt.Errorf("port %d: not the test's", p)
		},
	}
	defer f.closeAll()
	want := map[int]string{refused: "127.0.0.1", denied: "127.0.0.1", inUse: "127.0.0.1", busy: "127.0.0.1"}
	f.apply(t.Context(), want)
	failed := failedPorts{inUse: []int{inUse}, refused: []int{refused, denied}}
	if got, w := notes, []failedPorts{failed}; !slices.EqualFunc(got, w, failedEqual) {
		t.Fatalf("noted %+v, want %+v", got, w)
	}
	f.apply(t.Context(), want)
	if listens[busy] != 2 || listens[refused] != 1 || listens[denied] != 1 || listens[inUse] != 1 {
		t.Errorf("EMFILE not tried again at the next report, or another one was: listened %v", listens)
	}
	// At its limit: a new port isn't tried, and is recorded as past it.
	delete(want, busy)
	for i := range MaxForwards {
		f.ports[30000+i] = &forward{}
		want[30000+i] = "127.0.0.1"
	}
	want[limited] = "127.0.0.1"
	f.apply(t.Context(), want)
	failed.limit = []int{limited}
	if got := notes[len(notes)-1]; !failedEqual(got, failed) {
		t.Errorf("noted %+v, want %+v", got, failed)
	}
	f.apply(t.Context(), want)
	if listens[limited] != 0 {
		t.Errorf("a port past the limit tried: listened %v", listens)
	}
	// Below it again (a forward went): forwarded at the next report.
	delete(want, 30000)
	f.apply(t.Context(), want)
	failed.limit = nil
	if got := notes[len(notes)-1]; f.ports[limited] == nil || !failedEqual(got, failed) {
		t.Errorf("past the limit, then below it: forwarded %v, noted %+v", f.ports[limited] != nil, got)
	}
}

func failedEqual(a, b failedPorts) bool {
	return slices.Equal(a.inUse, b.inUse) && slices.Equal(a.limit, b.limit) && slices.Equal(a.refused, b.refused)
}

// A relay whose far end never ends its channel (a VM whose sshd ignores
// EOF) still ends when its forward goes: its ssh is killed, and its slot
// is free again, so a VM can't keep the forwarder's relays by holding
// them open. The ssh here is a stand-in, on PATH, that never exits on its
// own.
func TestForwarderEndsStuckRelay(t *testing.T) {
	p := freePorts(t, 1)[0]
	bin := t.TempDir()
	pidFile := filepath.Join(bin, "pid")
	stub := "#!/bin/sh\necho $$ > " + pidFile + ".tmp && mv " + pidFile + ".tmp " + pidFile + "\nexec sleep 600\n"
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(stub), 0o700); err != nil { //nolint:gosec // gosec G306: the test's own script, which it runs
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var reserved map[int]bool
	ended := make(chan struct{})
	f := &Forwarder{
		VM: "devvm-f", Log: io.Discard, ports: map[int]*forward{}, skipped: map[int]failure{}, relays: make(chan struct{}, maxRelays),
		Reserved: func() (map[int]bool, error) { return reserved, nil },
		Relay: func(ctx context.Context, conn net.Conn, _ string, _ int) {
			defer close(ended)
			pipeSSH(ctx, conn, nil)
		},
		Listen: platform.ListenLoopback,
	}
	defer f.closeAll()
	f.apply(t.Context(), map[int]string{p: "127.0.0.1"})
	d := net.Dialer{Timeout: time.Second}
	c, err := d.DialContext(t.Context(), "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	var pid int
	for range 200 {
		if b, err := os.ReadFile(pidFile); err == nil { //nolint:gosec // gosec G304: the test's own temp file
			pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid <= 0 {
		t.Fatal("the ssh stand-in never started")
	}
	reserved = map[int]bool{p: true}
	f.release()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		_ = daemon.Signal(pid, syscall.SIGKILL)
		t.Fatal("the relay outlived its forward")
	}
	if daemon.Alive(pid) {
		_ = daemon.Signal(pid, syscall.SIGKILL)
		t.Error("its ssh still runs")
	}
	for range 100 {
		if len(f.relays) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("%d relay slots still taken", len(f.relays))
}

// A reserved port the VM listens on waits: another VM forwards it, and
// once that VM gives it up (it stops) it's forwarded at the next report,
// even after a try that found it taken. What it forwards is noted at each
// change, and only then.
func TestForwarderWaits(t *testing.T) {
	p := freePorts(t, 1)[0]
	var log bytes.Buffer
	var reserved map[int]bool
	var notes []string // what it forwards, then what it couldn't take
	f := &Forwarder{
		VM: "devvm-f", Log: &log, ports: map[int]*forward{}, skipped: map[int]failure{},
		Only:     []backend.PortRange{{From: p, To: p}},
		Reserved: func() (map[int]bool, error) { return reserved, nil },
		Note:     func(now []int, failed failedPorts) { notes = append(notes, fmt.Sprint(now, failed.inUse)) },
		Listen:   platform.ListenLoopback,
	}
	ps := fmt.Sprint([]int{p})
	defer f.closeAll()
	want := map[int]string{p: "127.0.0.1"}
	// The other VM's forward, which a try finds taken.
	other, err := platform.ListenLoopback(t.Context(), net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
	if err != nil {
		t.Fatal(err)
	}
	f.apply(t.Context(), want)
	if f.ports[p] != nil || f.skipped[p] != failedInUse || !slices.Equal(notes, []string{"[] " + ps}) {
		t.Fatalf("a taken port: forwarded %v, skipped %v, noted %q", f.ports, f.skipped, notes)
	}
	// It's the other VM's: waited for, with no try, and no longer one it
	// couldn't take.
	reserved = map[int]bool{p: true}
	f.apply(t.Context(), want)
	f.apply(t.Context(), want)
	if _, skipped := f.skipped[p]; f.ports[p] != nil || skipped || !slices.Equal(notes, []string{"[] " + ps, "[] []"}) {
		t.Fatalf("a reserved port: forwarded %v, skipped %v, noted %q", f.ports, f.skipped, notes)
	}
	notes = nil
	// The other VM stops: forwarded at the next report.
	_ = other.Close()
	reserved = nil
	f.apply(t.Context(), want)
	if f.ports[p] == nil {
		t.Fatalf("not forwarded once free: %s", log.String())
	}
	f.apply(t.Context(), want)
	f.apply(t.Context(), map[int]string{})
	if !slices.Equal(notes, []string{ps + " []", "[] []"}) {
		t.Errorf("noted %q", notes)
	}
}

// The master asks for nothing to be forwarded, reads no configuration of
// the user's and offers the VM's key alone; a relay is a channel of it,
// and without it connects to the VM the same way, never elsewhere: on a
// host port.
func TestForwarderSSH(t *testing.T) {
	o := SSH{VM: "devvm-v", User: "u", Key: "/k", KnownHosts: "/kh", Control: "/c.ctl", Port: 61022}
	conn := []string{
		"IdentitiesOnly=yes", "IdentityAgent=none", "IdentityFile=/k", "UserKnownHostsFile=/kh",
		"HostKeyAlias=devvm-v", "StrictHostKeyChecking=yes", "ForwardAgent=no", "ForwardX11=no", "Tunnel=no",
		"PermitRemoteOpen=none", "ClearAllForwardings=yes",
	}
	for name, tc := range map[string]struct {
		args []string
		want []string
	}{
		"master":  {o.masterArgs(), append([]string{"-F none -M -S /c.ctl -T ", "ProxyCommand=none -p 61022 -l u 127.0.0.1 sh -s"}, conn...)},
		"relay":   {o.relayArgs("::1", 5173), append([]string{"-F none -S /c.ctl -o ControlMaster=no -W [::1]:5173 ", "ProxyCommand=none -p 61022 -l u 127.0.0.1"}, conn...)},
		"command": {o.CommandArgs("git-upload-pack '/p'"), append([]string{"-F none -T -o BatchMode=yes ", "ProxyCommand=none -p 61022 -l u 127.0.0.1 git-upload-pack '/p'"}, conn...)},
	} {
		got := strings.Join(tc.args, " ")
		for _, want := range tc.want {
			if !strings.Contains(got, want) {
				t.Errorf("%s lacks %q: %s", name, want, got)
			}
		}
		for _, a := range tc.args {
			if a == "-R" || a == "-L" || a == "-D" || a == "-A" || a == "-X" || a == "-Y" {
				t.Errorf("%s asks for %s", name, a)
			}
		}
	}
	if got := o.relayArgs("::1", 5173); got[len(got)-1] != "127.0.0.1" {
		t.Errorf("relay runs a command: %q", got)
	}
}

// A backend's ssh target is taken only as a user with an absolute key
// file on a port of the host's loopback: any other address, a name, no
// user, key or port, or a key no ssh config line can quote, is refused.
func TestFromTarget(t *testing.T) {
	good := backend.SSHTarget{
		Host: "127.0.0.1", Port: 60022, User: "u", Key: "/k/user", ConfigFile: "/c",
		Options: []backend.SSHOption{{Key: "IdentitiesOnly", Value: "yes"}},
	}
	got, err := FromTarget("devvm-web-0123456789abcd", good)
	if want := (SSH{VM: "devvm-web-0123456789abcd", User: "u", Key: "/k/user", Port: 60022}); err != nil || got != want {
		t.Errorf("FromTarget = %+v, %v; want %+v", got, err, want)
	}
	for name, change := range map[string]func(*backend.SSHTarget){
		"another address": func(t *backend.SSHTarget) { t.Host = "10.0.0.2" },
		"all addresses":   func(t *backend.SSHTarget) { t.Host = "0.0.0.0" },
		"a name":          func(t *backend.SSHTarget) { t.Host = "localhost" },
		"IPv6 loopback":   func(t *backend.SSHTarget) { t.Host = "::1" },
		"another 127":     func(t *backend.SSHTarget) { t.Host = "127.0.0.2" },
		"no host":         func(t *backend.SSHTarget) { t.Host = "" },
		"no port":         func(t *backend.SSHTarget) { t.Port = 0 },
		"a port too high": func(t *backend.SSHTarget) { t.Port = 65536 },
		"no user":         func(t *backend.SSHTarget) { t.User = "" },
		"a user's space":  func(t *backend.SSHTarget) { t.User = "u ProxyCommand" },
		"a user's line":   func(t *backend.SSHTarget) { t.User = "u\nProxyCommand x" },
		"no key":          func(t *backend.SSHTarget) { t.Key = "" },
		"a relative key":  func(t *backend.SSHTarget) { t.Key = "key" },
		"a quoted key":    func(t *backend.SSHTarget) { t.Key = `/k"x` },
		"a key's newline": func(t *backend.SSHTarget) { t.Key = "/k\nProxyCommand x" },
	} {
		tt := good
		change(&tt)
		if got, err := FromTarget("v", tt); err == nil {
			t.Errorf("%s: %+v", name, got)
		}
	}
}

func newFlagSet(fl *Flags) *flag.FlagSet {
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fl.Register(fs)
	return fs
}

// The forwarder's flags: each backend's command adds its own, and reaches
// its VM on a port; listed ports are read as devvm ports lists them.
func TestFlags(t *testing.T) {
	var fl Flags
	fs := newFlagSet(&fl)
	if err := fs.Parse([]string{"-vm", "v", "-user", "u", "-key", "/k", "-known-hosts", "/kh", "-control", "/c", "-reserve", "1,2", "-only", "80,8000-8002"}); err != nil {
		t.Fatal(err)
	}
	if _, err := fl.Forwarder(nil, func(string) (map[int]bool, error) { return nil, nil }, io.Discard); err == nil {
		t.Error("no way to the VM: taken")
	}
	fl.Port = 61022
	f, err := fl.Forwarder(nil, func(vm string) (map[int]bool, error) { return map[int]bool{3: vm == "v"}, nil }, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.Only, []backend.PortRange{{From: 80, To: 80}, {From: 8000, To: 8002}}) {
		t.Errorf("Only = %v", f.Only)
	}
	if r, err := f.Reserved(); err != nil || !maps.Equal(r, map[int]bool{1: true, 2: true, 3: true}) {
		t.Errorf("Reserved = %v, %v", r, err)
	}
	for _, bad := range []string{"x", "0", "9-1", "70000"} {
		fl.only = bad
		if _, err := fl.Forwarder(nil, func(string) (map[int]bool, error) { return nil, nil }, io.Discard); err == nil {
			t.Errorf("-only %q: taken", bad)
		}
	}
}

// A forwarder that doesn't end on SIGTERM is killed: Stop returns with it
// gone, so a start that follows finds its ports free.
func TestForwardsStopKills(t *testing.T) {
	dir := t.TempDir()
	out, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = out.Close() }()
	p, err := daemon.Spawn(daemon.Cmd{Path: "/bin/sh", Args: []string{"-c", `trap "" TERM; echo ready; while :; do sleep 1; done`}, Out: out})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Stop(0)
	ended := p.Done()
	for range 100 {
		if b, _ := os.ReadFile(out.Name()); strings.Contains(string(b), "ready") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	f := Forwards{Dirs: paths.Dirs{Data: dir}, Args: func(int) ([]string, error) {
		select {
		case <-ended:
			return nil, errors.New("gone")
		default:
			return []string{"/devvm", Cmd, "-vm", "v"}, nil
		}
	}}
	pidFile := f.Dirs.VM("v").Forwarder(".pid")
	if err := state.WriteFile(pidFile, []byte(strconv.Itoa(p.PID())+"\n")); err != nil {
		t.Fatal(err)
	}
	f.Stop("v")
	select {
	case <-ended:
	case <-time.After(time.Second):
		t.Error("still running after Stop")
	}
	if _, err := os.Stat(pidFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("its pid file stayed: %v", err)
	}
}

// A pid file whose pid another program has now names no forwarder: Stop
// signals nothing (this test is that program), and neither does one
// that names no pid.
func TestForwardsStopReusedPID(t *testing.T) {
	dir := t.TempDir()
	f := Forwards{Dirs: paths.Dirs{Data: dir}, Args: func(int) ([]string, error) { return []string{"/usr/bin/go", "test"}, nil }}
	for _, pid := range []string{strconv.Itoa(os.Getpid()), "0", "-1", "x"} {
		if err := state.WriteFile(f.Dirs.VM("v").Forwarder(".pid"), []byte(pid+"\n")); err != nil {
			t.Fatal(err)
		}
		if f.proc("v").Running() {
			t.Errorf("pid %s: a forwarder", pid)
		}
		f.Stop("v")
	}
}

// Forwards: a VM's forwarder runs devvm _ports with how it reaches the
// VM and what it forwards, its host key pinned in a file of its own; it's
// found by its pid and arguments, and its files are in the VM's run and
// logs folders.
func TestForwardsRun(t *testing.T) {
	short, err := os.MkdirTemp("/tmp", "dv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(short) })
	var args []string
	running := false
	f := Forwards{
		Dirs: paths.Dirs{Data: short}, Self: "/devvm",
		Detach: func(c daemon.Cmd) (int, error) {
			args = append([]string{c.Path}, c.Args...)
			return os.Getpid(), daemon.WriteStatus(c.Status, os.Getpid(), "")
		},
		Args: func(int) ([]string, error) {
			if !running {
				return nil, errors.New("gone")
			}
			return []string{"/devvm", Cmd, "-vm", "v"}, nil
		},
		Tick: time.Millisecond,
	}
	s := spec{
		SSH: SSH{User: "u", Key: "/k", Port: 2222, KnownHosts: "/kh"}, Lifeline: "/ha.pid",
		Reserve: []int{0, 22}, Only: []backend.PortRange{{From: 80, To: 81}},
	}
	// It's started once it says so in its status file, and still runs.
	running = true
	if err := f.run("v", s); err != nil {
		t.Fatal(err)
	}
	running = false
	want := []string{
		"/devvm", "_ports", "-vm", "v", "-lifeline", "/ha.pid", "-port", "2222", "-user", "u", "-key", "/k", "-known-hosts", "/kh",
		"-control", f.Control("v"), "-reserve", "22", "-only", "80-81",
	}
	if !slices.Equal(args, want) || !strings.HasPrefix(f.Control("v"), filepath.Join(short, "run", "port-")) {
		t.Errorf("forwarder:\n%q\nwant\n%q", args, want)
	}
	if f.proc("v").Running() {
		t.Error("found with other arguments")
	}
	running = true
	if !f.proc("v").Running() || f.proc("w").Running() {
		t.Error("not found by its arguments")
	}
	running = false
	// Its lock: held by one at a time, waited for until the context ends;
	// another VM's is its own.
	unlock, err := f.Lock(t.Context(), "v")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if _, err := f.Lock(ctx, "v"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("locked twice: %v", err)
	}
	if unlockW, err := f.Lock(t.Context(), "w"); err != nil {
		t.Errorf("another VM's: %v", err)
	} else {
		unlockW()
	}
	unlock()
	if unlock, err = f.Lock(t.Context(), "v"); err != nil {
		t.Errorf("once unlocked: %v", err)
	} else {
		unlock()
	}
	// Its files are in the VM's run folder, its log in its logs folder.
	for _, p := range []string{
		f.Dirs.VM("v").Forwarder(".pid"), f.Dirs.VM("v").Forwarder(".status"), f.Dirs.VM("v").Forwarder(".lock"),
		f.Log("v"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	// One that says so and has ended: an error.
	f.Detach = func(c daemon.Cmd) (int, error) {
		h, err := daemon.Spawn(daemon.Cmd{Path: "true"})
		if err != nil {
			return 0, err
		}
		<-h.Done()
		return h.PID(), daemon.WriteStatus(c.Status, h.PID(), "")
	}
	if err := f.run("v", s); err == nil {
		t.Error("a forwarder that ended: no error")
	}
	// One that never says it forwards: an error, left running.
	f.Detach = func(daemon.Cmd) (int, error) { return os.Getpid(), nil }
	running = true
	f.Tick = time.Microsecond
	if err := f.run("v", s); err == nil {
		t.Error("a forwarder that doesn't forward: no error")
	}
}
