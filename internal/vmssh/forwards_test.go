package vmssh

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/daemon"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/ports"
	"github.com/devvmhq/devvm/internal/state"
)

// testForwards is Forwards whose forwarders are processes of the test's
// (sleep), which say they forward at once: it returns each start's
// arguments and what it warned.
func testForwards(t *testing.T) (*Forwards, *[][]string, *msg.Recorder) {
	t.Helper()
	short, err := os.MkdirTemp("/tmp", "dv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(short) })
	var mu sync.Mutex
	var starts [][]string
	procs := map[int][]string{}
	warned := &msg.Recorder{}
	f := &Forwards{
		Dirs: paths.Dirs{Data: short}, Self: "/devvm", Tick: time.Millisecond,
		Detach: func(c daemon.Cmd) (int, error) {
			cmd := exec.CommandContext(t.Context(), "sleep", "30")
			if err := cmd.Start(); err != nil {
				return 0, err
			}
			done := make(chan struct{})
			go func() { _ = cmd.Wait(); close(done) }()
			t.Cleanup(func() { _ = cmd.Process.Kill(); <-done })
			mu.Lock()
			defer mu.Unlock()
			starts = append(starts, append([]string{c.Path}, c.Args...))
			procs[cmd.Process.Pid] = append([]string{c.Path}, c.Args...)
			return cmd.Process.Pid, daemon.WriteStatus(c.Status, cmd.Process.Pid, "")
		},
		Args: func(pid int) ([]string, error) {
			mu.Lock()
			defer mu.Unlock()
			if a, ok := procs[pid]; ok {
				return a, nil
			}
			return nil, errors.New("not ours")
		},
		InUse:    func(context.Context, int) bool { return false },
		Reporter: &msg.Fake{OnWarn: warned.Add},
	}
	return f, &starts, warned
}

// The forwarder's start reads which ports the VM forwards from its state
// and how to reach it from its backend: every port, or its listed ones
// less those a host program has (warned), asked once its old forwarder
// has stopped; none, or all its listed ones taken, runs none. It never
// takes the VM's ssh or proxy port, and checks the host keys devvm pins
// for the VM's start.
func TestForwardsStart(t *testing.T) {
	f, starts, warned := testForwards(t)
	const vm = "devvm-p-0123456789abcd"
	key := "ssh-ed25519 " + keyED
	b, _ := hostKeysFake(t, vm, &key)
	// The forwarder's target is the VM's ssh, Options and ConfigFile
	// left out.
	b.Target = backend.SSHTarget{
		Host: Loopback, Port: 60022, User: "u", Key: "/k",
		Options: []backend.SSHOption{{Key: "IdentitiesOnly", Value: "yes"}}, ConfigFile: "/lima/ssh.config",
	}
	b.LifelineFile = "/lima/ha.pid"
	st, err := state.New(f.Dirs, vm)
	if err != nil {
		t.Fatal(err)
	}
	forward := func(list string) {
		t.Helper()
		if err := ports.Set(st, ports.Ports{Forward: list, Host: ports.Host{SSH: 60022, Proxy: 30001}}); err != nil {
			t.Fatal(err)
		}
	}
	ctx := t.Context()
	forward("")
	// Its old forwarder's record goes with it: a pid used again never
	// makes it the new one's.
	if err := f.record(vm, 1, []int{5173}, failedPorts{}); err != nil {
		t.Fatal(err)
	}
	f.Start(ctx, b, st)
	if _, err := os.Stat(f.Dirs.VM(vm).Forwarder(".forwarded")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the old record stays: %v", err)
	}
	want := []string{
		"/devvm", "_ports", "-vm", vm, "-lifeline", "/lima/ha.pid", "-port", "60022", "-user", "u", "-key", "/k",
		"-known-hosts", f.Dirs.VM(vm).KnownHosts(), "-control", f.Control(vm), "-reserve", "60022,30001",
	}
	if len(*starts) != 1 || !slices.Equal((*starts)[0], want) || len(warned.Names()) != 0 {
		t.Fatalf("every port:\n%q\nwant\n%q\nwarned %q", *starts, want, warned.Names())
	}
	if got := readFile(t, f.Dirs.VM(vm).KnownHosts()); got != startLine+"start-0\n"+vm+" ssh-ed25519 "+keyED+"\n" {
		t.Errorf("known_hosts: %q", got)
	}
	if !f.proc(vm).Running() {
		t.Error("not running")
	}

	// Listed: the free ones, asked with the old one stopped.
	asked := false
	f.InUse = func(_ context.Context, port int) bool {
		if f.proc(vm).Running() {
			t.Error("asked while the old forwarder ran")
		}
		asked = true
		return port == 5173
	}
	forward("5173,8000-8010")
	f.Start(ctx, b, st)
	got := (*starts)[len(*starts)-1]
	if len(*starts) != 2 || !asked || !slices.Equal(got[len(got)-2:], []string{"-only", "8000-8010"}) {
		t.Errorf("listed: %q", *starts)
	}
	if w := warned.Names(); !slices.Equal(w, []string{"ports_taken count=1 ports=5173 vm=" + vm}) {
		t.Errorf("warned %q", w)
	}
	warned.Reset()

	// All taken, or none: no forwarder, the old one stopped.
	for _, list := range []string{"5173", "none"} {
		forward(list)
		f.Start(ctx, b, st)
		if len(*starts) != 2 || f.proc(vm).Running() {
			t.Errorf("%s: started %q, running %v", list, *starts, f.proc(vm).Running())
		}
	}
	warned.Reset()

	// What it can't start with is warned about, naming its log: a state it
	// can't read, an ssh target FromTarget refuses (another address, a
	// name, no user, no key, a relative key), a host key it can't read, a
	// VM its backend can't reach. None of them starts a forwarder.
	failed := []string{"ports_forward_failed log=" + f.Log(vm) + " vm=" + vm}
	forward("x")
	f.Start(ctx, b, st)
	if w := warned.Names(); !slices.Equal(w, failed) || len(*starts) != 2 {
		t.Errorf("a state it can't read: started %q, warned %q", *starts, w)
	}
	warned.Reset()
	forward("")
	good := b.Target
	for _, bad := range []backend.SSHTarget{
		{Host: "10.0.0.2", Port: 22, User: "u", Key: "/k"},
		{Host: "localhost", Port: 60022, User: "u", Key: "/k"},
		{Host: "0.0.0.0", Port: 60022, User: "u", Key: "/k"},
		{Host: Loopback, Port: 60022, Key: "/k"},
		{Host: Loopback, Port: 60022, User: "u"},
		{Host: Loopback, Port: 60022, User: "u", Key: "k"},
		{Host: Loopback, User: "u", Key: "/k"},
	} {
		b.Target = bad
		f.Start(ctx, b, st)
		if w := warned.Names(); !slices.Equal(w, failed) || len(*starts) != 2 || f.proc(vm).Running() {
			t.Errorf("%+v: started %q, warned %q", bad, *starts, w)
		}
		warned.Reset()
	}
	b.Target = good
	key = "no keys"
	if err := ForgetHostKeys(f.Dirs, vm); err != nil {
		t.Fatal(err)
	}
	f.Start(ctx, b, st)
	key = "ssh-ed25519 " + keyED
	b.Instances[vm] = backend.Instance{Name: vm, Status: backend.StatusStopped}
	f.Start(ctx, b, st)
	if w := warned.Names(); len(*starts) != 2 || !slices.Equal(w, slices.Concat(failed, failed)) {
		t.Errorf("started %q, warned %q", *starts, w)
	}
}

// A port another running VM forwards is that VM's, as its forwarder's
// record says while it runs: a start keeps it in its listed ports, which
// its forwarder takes once the other gives it up, warned (ports_held),
// whether the other lists it or forwards every port. A port a program has
// is left out (ports_taken).
func TestForwardsHeld(t *testing.T) {
	f, _, warned := testForwards(t)
	const (
		a     = "devvm-a-0123456789abca"
		b     = "devvm-b-0123456789abcb" // lists 5173,8000, runs
		every = "devvm-c-0123456789abcc" // forwards every port, runs
		gone  = "devvm-d-0123456789abcd" // lists 7000, stopped
	)
	for name, fwd := range map[string]string{a: "5173,8000-8001,3000,9000", b: "5173,8000", every: "", gone: "7000"} {
		st, err := state.New(f.Dirs, name)
		if err != nil {
			t.Fatal(err)
		}
		st.Image, st.Dir, st.Backend = "base", "/"+name, "fake"
		if err := st.Create(t.Context(), func(st *state.VM) error { return ports.SetForward(st, fwd) }); err != nil {
			t.Fatal(err)
		}
	}
	run := func(vm string, now ...int) {
		t.Helper()
		if err := state.PrivateDir(f.Dirs.VM(vm).Run()); err != nil {
			t.Fatal(err)
		}
		pid, err := f.proc(vm).Start(f.Detach, daemon.Cmd{Path: f.Self, Args: []string{Cmd, "-vm", vm}, Status: f.Dirs.VM(vm).Forwarder(".status")})
		if err != nil {
			t.Fatal(err)
		}
		if err := f.record(vm, pid, now, failedPorts{}); err != nil {
			t.Fatal(err)
		}
	}
	run(b, 5173, 8000)
	run(every, 3000)
	// A record its process didn't write names none; a VM that's stopped,
	// or crashed, its forwarder gone, holds none.
	if err := f.record(gone, 1, []int{5173}, failedPorts{}); err != nil {
		t.Fatal(err)
	}
	if now, ok := f.live(gone); ok || now != nil {
		t.Errorf("a stopped VM: %v, %v", now, ok)
	}
	if now, ok := f.live(b); !ok || !slices.Equal(now, []int{5173, 8000}) {
		t.Errorf("a running VM: %v, %v", now, ok)
	}
	pid, _ := f.proc(every).PID()
	if err := f.record(every, pid+1, []int{3000}, failedPorts{}); err != nil {
		t.Fatal(err)
	}
	if now, ok := f.live(every); !ok || now != nil {
		t.Errorf("another process's record: %v, %v", now, ok)
	}
	if err := f.record(every, pid, []int{3000}, failedPorts{}); err != nil {
		t.Fatal(err)
	}

	// A running forwarder with no record (an older devvm's, or one whose
	// write failed) forwards none known, but runs.
	if err := os.Remove(f.Dirs.VM(every).Forwarder(".forwarded")); err != nil {
		t.Fatal(err)
	}
	if now, ok := f.live(every); !ok || now != nil {
		t.Errorf("no record: %v, %v", now, ok)
	}
	if err := f.record(every, pid, []int{3000}, failedPorts{}); err != nil {
		t.Fatal(err)
	}

	// Every VM that forwards one of a's listed ports holds it, one that
	// forwards every port too.
	held, err := f.Held(a, "5173,8000-8001,3000,9000")
	r := func(a, b int) backend.PortRange { return backend.PortRange{From: a, To: b} }
	if err != nil || len(held) != 2 || !slices.Equal(held[b], []backend.PortRange{r(5173, 5173), r(8000, 8000)}) ||
		!slices.Equal(held[every], []backend.PortRange{r(3000, 3000)}) {
		t.Errorf("Held = %v, %v", held, err)
	}
	if held, err := f.Held(a, "all"); err != nil || len(held) != 0 {
		t.Errorf("Held with every port = %v, %v", held, err)
	}

	f.InUse = func(_ context.Context, p int) bool { return p == 5173 || p == 8000 || p == 3000 || p == 9000 }
	listed := []backend.PortRange{r(5173, 5173), r(8000, 8001), r(3000, 3000), r(9000, 9000)}
	free := f.free(t.Context(), a, listed)
	if want := []backend.PortRange{r(5173, 5173), r(8000, 8001), r(3000, 3000)}; !slices.Equal(free, want) {
		t.Errorf("free = %v, want %v", free, want)
	}
	want := []string{
		"ports_taken count=1 ports=9000 vm=" + a,
		"ports_held count=2 other=" + b + " ports=5173,8000 vm=" + a,
		"ports_held count=1 other=" + every + " ports=3000 vm=" + a,
	}
	if w := warned.Names(); !slices.Equal(w, want) {
		t.Errorf("warned %q, want %q", w, want)
	}

	// What a's forwarder never takes: what b and every forward, and b's
	// listed ports a doesn't list; none of the stopped VM's.
	reserved, err := f.Reserved(a)
	if err != nil || !reserved[5173] || !reserved[8000] || !reserved[3000] || len(reserved) != 3 {
		t.Errorf("Reserved = %v, %v", reserved, err)
	}
	// A VM whose forwarder is being started again (its lock held, from its
	// old forwarder's stop to the new one's start) still holds its listed
	// ports: none other takes them in between.
	unlock, err := f.Lock(t.Context(), gone)
	if err != nil {
		t.Fatal(err)
	}
	reserved, err = f.Reserved(every)
	if err != nil || !reserved[7000] {
		t.Errorf("Reserved while a VM's forwarder starts again = %v, %v", reserved, err)
	}
	unlock()
	if reserved, err = f.Reserved(every); err != nil || !reserved[5173] || !reserved[8000] || len(reserved) != 2 {
		t.Errorf("Reserved after = %v, %v", reserved, err)
	}

	// a's own forwarder runs: its listed ports that it tried and couldn't
	// take, as its record says, by why, and no VM holds, are a program's
	// (in use), past its limit, or refused by this machine (devvm ports
	// says which). Nothing is bound to find out, so a port it may be
	// opening right then (8001, in use as it opens it) is what the record
	// says.
	const list = "80,5173,8000-8001,3000,9000"
	run(a, 8000)
	pidA, _ := f.proc(a).PID()
	if err := f.record(a, pidA, []int{8000}, failedPorts{inUse: []int{5173, 9000}, limit: []int{8001}, refused: []int{80}}); err != nil {
		t.Fatal(err)
	}
	rec := f.Dirs.VM(a).Forwarder(".forwarded")
	if got, want := readFile(t, rec), strconv.Itoa(pidA)+"\n8000\n5173,9000\n8001\n80\n"; got != want {
		t.Errorf("record %q, want %q", got, want)
	}
	f.InUse = func(_ context.Context, p int) bool { return p == 8001 || p == 9000 || p == 80 }
	un := f.Unforwarded(a, list)
	if !slices.Equal(un.InUse, []backend.PortRange{r(9000, 9000)}) || !slices.Equal(un.Limit, []backend.PortRange{r(8001, 8001)}) ||
		!slices.Equal(un.Refused, []backend.PortRange{r(80, 80)}) {
		t.Errorf("Unforwarded = %+v", un)
	}
	unforwardedUnknown(t, *f, a, list, pidA)
	if un := f.Unforwarded(gone, "7000"); un.InUse != nil {
		t.Errorf("Unforwarded of a stopped VM = %+v", un)
	}
}

// unforwardedUnknown checks that the running VM's forwarder, process pid,
// has no port it couldn't take known without its record, with one
// another process wrote (an older devvm's forwarder, or a write that
// failed), or with an older devvm's record, whose third line is every
// port it couldn't take, whatever the reason: none is wrongly said to be
// in use. An older devvm's record still says what it forwards.
func unforwardedUnknown(t *testing.T, f Forwards, vm, list string, pid int) {
	t.Helper()
	rec := f.Dirs.VM(vm).Forwarder(".forwarded")
	for name, data := range map[string]string{
		"another process's": strconv.Itoa(pid+1) + "\n8000\n9000\n\n80\n",
		"an older devvm's":  strconv.Itoa(pid) + "\n8000\n9000,80\n",
	} {
		if err := state.WriteFile(rec, []byte(data)); err != nil {
			t.Fatal(err)
		}
		if un := f.Unforwarded(vm, list); un.InUse != nil || un.Limit != nil || un.Refused != nil {
			t.Errorf("Unforwarded with %s record = %+v", name, un)
		}
	}
	if err := state.WriteFile(rec, []byte(strconv.Itoa(pid)+"\n8000\n9000,80\n")); err != nil {
		t.Fatal(err)
	}
	if now, ok := f.live(vm); !ok || !slices.Equal(now, []int{8000}) {
		t.Errorf("an older devvm's record forwards %v, %v", now, ok)
	}
	if err := os.Remove(rec); err != nil {
		t.Fatal(err)
	}
	if un := f.Unforwarded(vm, list); un.InUse != nil || un.Limit != nil || un.Refused != nil {
		t.Errorf("Unforwarded without a record = %+v", un)
	}
}

// A record the forwarder can't write is an error, and leaves none.
func TestForwardsRecordFails(t *testing.T) {
	f, _, _ := testForwards(t)
	const vm = "devvm-a-0123456789abca"
	if err := os.MkdirAll(f.Dirs.VM(vm).Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.Dirs.VM(vm).Run(), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.record(vm, 1, []int{5173}, failedPorts{}); err == nil {
		t.Error("a record where its folder is a file")
	}
}

// splitPorts splits listed ports into those free and those in use, each
// as ranges.
func TestSplitPorts(t *testing.T) {
	r := func(a, b int) backend.PortRange { return backend.PortRange{From: a, To: b} }
	inUse := func(p int) bool { return p == 5173 || p == 5175 || p == 8080 || p == 8081 }
	free, taken := splitPorts([]backend.PortRange{r(5173, 5173), r(5174, 5176), r(5177, 5177), r(8079, 8082)}, inUse)
	if want := []backend.PortRange{r(5174, 5174), r(5176, 5176), r(5177, 5177), r(8079, 8079), r(8082, 8082)}; !slices.Equal(free, want) {
		t.Errorf("free %v, want %v", free, want)
	}
	if want := []backend.PortRange{r(5173, 5173), r(5175, 5175), r(8080, 8081)}; !slices.Equal(taken, want) {
		t.Errorf("taken %v, want %v", taken, want)
	}
}
