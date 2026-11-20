package vm

import (
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/backend/backendtest"
	"github.com/devvmhq/devvm/internal/daemon"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/netpolicy"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/vmlog"
	"github.com/devvmhq/devvm/internal/vmname"
)

// testVM is the name of the test VM of a folder named f: each folder name
// has a hash of its own, as VMs that shared one would share a folder.
func testVM(f string) vmname.VM {
	h := map[string]string{
		"app": "abc1", "db": "abc2", "api": "abc3", "web": "abc4", "web2": "abc5", "cache": "abc6",
		"local": "abc7", "x": "abc8", "cl": "abc9", "half": "abce", "b": "abcb", "img-foo": "abcc",
	}[f]
	if h == "" {
		h = "abcd"
	}
	return vmname.VM("devvm-" + f + "-0123456789" + h)
}

// linkVMs makes the states of project VMs by folder name, each finished
// (its network choice made, off), on the fake backend.
func linkVMs(t *testing.T, d paths.Dirs, folders ...string) []*state.VM {
	t.Helper()
	var out []*state.VM
	for _, f := range folders {
		out = append(out, vmState(t, d, testVM(f), withNet(false)))
	}
	return out
}

// linkTo is a link from a client to target's port, at addr.
func linkTo(target *state.VM, name string, port int, addr string) Link {
	return Link{Name: name, Port: port, Target: target.Name(), TargetID: target.ID, Addr: netip.MustParseAddr(addr)}
}

func setNet(t *testing.T, st *state.VM, v string) {
	t.Helper()
	change(t, st, withNet(v == "on"))
}

// A client's guest gets an address and a name for each target, by
// address, but none for a target whose policy rule no longer holds.
func TestGuestLinks(t *testing.T) {
	d := paths.Dirs{Data: t.TempDir()}
	vms := linkVMs(t, d, "app", "db", "api")
	app, db, api := vms[0], vms[1], vms[2]
	links := []Link{linkTo(api, "api", 3000, "127.77.0.2"), linkTo(db, "db", 5432, "127.77.0.1"), linkTo(db, "db", 6379, "127.77.0.1")}
	if err := writeLinks(t.Context(), app, links); err != nil {
		t.Fatal(err)
	}
	show := func(gs []GuestLink) string {
		var s []string
		for _, g := range gs {
			s = append(s, g.Addr.String()+"="+strings.Join(g.Names, ","))
		}
		return strings.Join(s, " ")
	}
	if got, err := guestLinks(d, app); err != nil || show(got) != "127.77.0.1=db 127.77.0.2=api" {
		t.Errorf("guestLinks = %q, %v", show(got), err)
	}
	setNet(t, app, "on")
	setNet(t, api, "on")
	if got, err := guestLinks(d, app); err != nil || show(got) != "127.77.0.2=api" {
		t.Errorf("with network policy on app and api alone: %q, %v", show(got), err)
	}
}

// A tier lower than a linked client's for a target, or higher than a
// linked target's for a client, is refused; links to a VM that's gone or
// was made again don't count.
func TestTierLinked(t *testing.T) {
	d := paths.Dirs{Data: t.TempDir()}
	vms := linkVMs(t, d, "app", "web", "db")
	app, web, db := vms[0], vms[1], vms[2]
	for _, c := range []*state.VM{app, web} {
		if err := writeLinks(t.Context(), c, []Link{linkTo(db, "db", 5432, "127.77.0.1")}); err != nil {
			t.Fatal(err)
		}
	}
	check := func(st *state.VM, to netpolicy.Tier, want string) {
		t.Helper()
		if got := msg.NamesOf(tierLinked(d, st, to)); got != want {
			t.Errorf("tierLinked(%s, %s) = %q, want %q", st.Name(), to, got, want)
		}
	}
	check(db, netpolicy.TierOpen, "")
	check(app, netpolicy.TierAllowlist, "tier_linked_targets targets="+db.Name().String()+" tier=allowlist vm="+app.Name().String())
	setNet(t, db, "on")
	check(app, netpolicy.TierAllowlist, "")
	check(app, netpolicy.TierInternet, "")
	check(app, netpolicy.TierNone, "tier_linked_targets targets="+db.Name().String()+" tier=none vm="+app.Name().String())
	setNet(t, app, "on")
	setNet(t, web, "on")
	check(db, netpolicy.TierNone, "")
	check(db, netpolicy.TierOpen, "tier_linked_clients tier=open vm="+db.Name().String()+" vms="+app.Name().String()+", "+web.Name().String())
	check(db, netpolicy.TierInternet, "tier_linked_clients tier=internet vm="+db.Name().String()+" vms="+app.Name().String()+", "+web.Name().String())
	setNet(t, web, "off")
	check(db, netpolicy.TierOpen, "tier_linked_clients tier=open vm="+db.Name().String()+" vms="+app.Name().String())
	// db made again: app's link is to the one before.
	db = vmState(t, d, db.Name(), withNet(false))
	check(db, netpolicy.TierOpen, "")
	setNet(t, app, "off")
	check(app, netpolicy.TierAllowlist, "")
}

// A learning VM is an internet one to links' tier rule: as a target, an
// internet client may link to it and an allowlist one may not; as a
// client of an internet target, devvm network lock's allowlist is
// refused.
func TestTierLinkedLearning(t *testing.T) {
	d := paths.Dirs{Data: t.TempDir()}
	vms := linkVMs(t, d, "app", "db")
	app, db := vms[0], vms[1]
	if err := writeLinks(t.Context(), app, []Link{linkTo(db, "db", 5432, "127.77.0.1")}); err != nil {
		t.Fatal(err)
	}
	change(t, app, withLearning(time.Now()))
	change(t, db, withLearning(time.Now()))
	if got := msg.NamesOf(tierLinked(d, db, netpolicy.TierInternet)); got != "" {
		t.Errorf("a learning target for a learning client: %q", got)
	}
	want := "tier_linked_targets targets=" + db.Name().String() + " tier=allowlist vm=" + app.Name().String()
	if got := msg.NamesOf(tierLinked(d, app, netpolicy.TierAllowlist)); got != want {
		t.Errorf("locking a client of a learning target: %q, want %q", got, want)
	}
}

// A target's removal takes the links to it out of every client's state,
// and names the clients; their other links stay.
func TestUnlinkTarget(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	vms := linkVMs(t, e.Dirs, "app", "web", "db", "api")
	app, web, db, api := vms[0], vms[1], vms[2], vms[3]
	if err := writeLinks(t.Context(), app, []Link{linkTo(db, "db", 5432, "127.77.0.1"), linkTo(api, "api", 3000, "127.77.0.2")}); err != nil {
		t.Fatal(err)
	}
	if err := writeLinks(t.Context(), web, []Link{linkTo(db, "pg", 5432, "127.77.0.1")}); err != nil {
		t.Fatal(err)
	}
	for _, st := range vms {
		f.Instances[st.Name()] = backend.Instance{Name: st.Name(), Status: backend.StatusStopped}
	}
	var said msg.Recorder
	fake(e).OnInfo = said.Add
	fake(e).OnWarn = func(m msg.M) { t.Errorf("warned %s", m.Names()) }
	if err := (linkRuns{e}).unlinkTarget(t.Context(), db.Name()); err != nil {
		t.Fatal(err)
	}
	want := "links_target_gone count=2 vm=" + db.Name().String() + " vms=" + app.Name().String() + ", " + web.Name().String()
	if !slices.Equal(said.Names(), []string{want}) {
		t.Errorf("said %q, want %q", said.Names(), want)
	}
	for _, c := range []struct {
		st   *state.VM
		want string
	}{{app, "api"}, {web, ""}} {
		st, _ := state.ReadVM(e.Dirs, c.st.Name())
		links, err := ReadLinks(st)
		var got []string
		for _, l := range links {
			got = append(got, l.Name)
		}
		if err != nil || strings.Join(got, " ") != c.want {
			t.Errorf("%s's links: %v, %v; want %q", c.st.Name(), got, err, c.want)
		}
	}
	said.Reset()
	if err := (linkRuns{e}).unlinkTarget(t.Context(), db.Name()); err != nil || len(said.Names()) != 0 {
		t.Errorf("again: %v, said %q", err, said.Names())
	}
}

// Links to VMs without state are dead links; a socket nothing listens on
// is a leftover, and one something listens on isn't.
func TestLinkLeftovers(t *testing.T) {
	run, err := os.MkdirTemp("/tmp", "dl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(run) })
	d := paths.Dirs{Data: t.TempDir()}
	vms := linkVMs(t, d, "app", "db")
	app, db := vms[0], vms[1]
	if err := writeLinks(t.Context(), app, []Link{linkTo(db, "db", 5432, "127.77.0.1")}); err != nil {
		t.Fatal(err)
	}
	if got, err := deadLinks(d); err != nil || len(got) != 0 {
		t.Errorf("deadLinks with db there: %+v, %v", got, err)
	}
	if err := db.Remove(t.Context()); err != nil {
		t.Fatal(err)
	}
	want := DeadLink{Client: app.Name(), Name: "db", Target: db.Name(), Port: 5432}
	if got, err := deadLinks(d); err != nil || !slices.Equal(got, []DeadLink{want}) {
		t.Errorf("deadLinks = %+v, %v; want %+v", got, err, want)
	}

	d = paths.Dirs{Data: run}
	if err := os.MkdirAll(d.Run(), 0o700); err != nil {
		t.Fatal(err)
	}
	listen := func(name string, keep bool) {
		t.Helper()
		ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(d.Run(), name), Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		ln.SetUnlinkOnClose(false)
		if keep {
			t.Cleanup(func() { _ = ln.Close() })
		} else {
			_ = ln.Close()
		}
	}
	listen("link-0000000000000001.sock", false)
	listen("link-0000000000000002.ctl", false)
	listen("link-0000000000000003.sock", true)
	listen("port-0000000000000005.ctl", false)
	listen("port-0000000000000006.ctl", true)
	listen("devvm-app-0123456789abcd.proxy.sock", false)
	listen("agent-0123456789abcd.sock", false) // a killed proxy's relay
	listen("agent-0123456789abce.sock", true)
	if err := os.WriteFile(filepath.Join(d.Run(), "link-0000000000000004.sock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LeftoverSockets(t.Context(), d)
	stale := []string{
		filepath.Join(d.Run(), "link-0000000000000001.sock"), filepath.Join(d.Run(), "link-0000000000000002.ctl"),
		filepath.Join(d.Run(), "agent-0123456789abcd.sock"), filepath.Join(d.Run(), "port-0000000000000005.ctl"),
	}
	slices.Sort(stale)
	if err != nil || !slices.Equal(got, stale) {
		t.Errorf("LeftoverSockets = %v, %v; want %v", got, err, stale)
	}
	for _, p := range append(got, filepath.Join(d.Run(), "link-0000000000000003.sock"), filepath.Join(d.Run(), "link-0000000000000004.sock")) {
		if err := RemoveLeftoverSocket(t.Context(), p); err != nil {
			t.Fatal(err)
		}
	}
	left, _ := filepath.Glob(filepath.Join(d.Run(), "link-*"))
	if !slices.Equal(left, []string{filepath.Join(d.Run(), "link-0000000000000003.sock"), filepath.Join(d.Run(), "link-0000000000000004.sock")}) {
		t.Errorf("left: %v", left)
	}
}

// linkRunsEnv is an Env whose link sessions are sleeps, each started with
// what start writes to its log.
func linkRunsEnv(t *testing.T, logged string) (linkRuns, *[][]string) {
	t.Helper()
	e, _, _ := upEnv(t, allNative())
	var mu sync.Mutex
	args := map[int][]string{}
	var started [][]string
	e.Proxy.Start = func(c daemon.Cmd) (int, error) {
		path, a, out := c.Path, c.Args, c.Out
		pid, _, err := sleeper(t)
		if err != nil {
			return 0, err
		}
		mu.Lock()
		defer mu.Unlock()
		args[pid] = append([]string{path}, a...)
		started = append(started, a)
		_, err = out.WriteString(logged)
		return pid, err
	}
	e.Proxy.Args = func(pid int) ([]string, error) {
		mu.Lock()
		defer mu.Unlock()
		return args[pid], nil
	}
	return linkRuns{e}, &started
}

// A client's link session is started once, found by its pid file and
// arguments, waited for to say what came of its links, and stopped.
func TestLinkRuns(t *testing.T) {
	vm := vmname.VM("devvm-app-0123456789abc1")
	db := Link{Name: "db", Port: 5432, Addr: netip.MustParseAddr("127.77.0.1")}
	api := Link{Name: "api", Port: 3000, Addr: netip.MustParseAddr("127.77.0.2")}
	r, started := linkRunsEnv(t, "devvm links: 127.77.0.1:5432: carried\ndevvm links: 127.77.0.2:3000: refused by the VM: exit status 255\n")
	var warned msg.Recorder
	fake(r.Env).OnWarn = warned.Add
	if _, ok := r.pid(vm); ok {
		t.Fatal("a session before any started")
	}
	ok, from, err := r.ensure(vm)
	if err != nil || !ok || from != 0 {
		t.Fatalf("ensure = %v, %d, %v", ok, from, err)
	}
	if ok, _, err := r.ensure(vm); err != nil || ok || len(*started) != 1 || !slices.Equal((*started)[0], []string{"_links", vm.String()}) {
		t.Errorf("ensure again: %v, %v; started %v", ok, err, *started)
	}
	start := time.Now()
	r.report(t.Context(), vm, []Link{db, api}, from)
	if !slices.Equal(warned.Names(), []string{"link_port_taken name=api port=3000 vm=" + vm.String()}) || time.Since(start) > time.Second {
		t.Errorf("warned %q after %v", warned.Names(), time.Since(start))
	}
	// Lines before from aren't this change's.
	warned.Reset()
	r.report(t.Context(), vm, []Link{api}, r.logSize(vm))
	if len(warned.Names()) != 0 {
		t.Errorf("warned %q of an earlier line", warned.Names())
	}
	// Another process with the pid file's pid isn't the session.
	if err := os.WriteFile(r.Dirs.VM(vm).LinksPID(), []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.pid(vm); ok {
		t.Error("pid 1 is the session")
	}
	// Nor is this test, which stopping the session never signals.
	if err := os.WriteFile(r.Dirs.VM(vm).LinksPID(), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	signal := r.Proxy.Signal
	r.Proxy.Signal = func(pid int, sig syscall.Signal) error {
		if sig != 0 {
			t.Errorf("signalled %d (%v)", pid, sig)
		}
		return signal(pid, sig)
	}
	if _, ok := r.pid(vm); ok {
		t.Error("the test is the session")
	}
	r.stop(t.Context(), vm)
	r.Proxy.Signal = signal
	if _, _, err := r.ensure(vm); err != nil {
		t.Fatal(err)
	}
	pid, ok := r.pid(vm)
	if !ok {
		t.Fatal("no session")
	}
	r.stop(t.Context(), vm)
	if _, ok := r.pid(vm); ok || daemon.Alive(pid) && !zombie(pid) {
		t.Errorf("still running: %d", pid)
	}
	if _, err := os.Stat(r.Dirs.VM(vm).LinksPID()); !os.IsNotExist(err) {
		t.Errorf("its pid file: %v", err)
	}
}

// A session's log that an earlier one left past its cap is rotated as the
// next starts, which writes from its start.
func TestLinkRunTrimsLog(t *testing.T) {
	vm := vmname.VM("devvm-app-0123456789abc1")
	r, _ := linkRunsEnv(t, "devvm links: started\n")
	log := r.Dirs.VM(vm).LinksLog()
	if err := os.MkdirAll(filepath.Dir(log), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(log, []byte(strings.Repeat("ssh: a line from the VM\n", vmlog.OwnMax/16)), 0o600); err != nil {
		t.Fatal(err)
	}
	ok, from, err := r.ensure(vm)
	if err != nil || !ok || from != 0 {
		t.Fatalf("ensure = %v, %d, %v", ok, from, err)
	}
	if b, _ := os.ReadFile(log); string(b) != "devvm links: started\n" { //nolint:gosec // the test's own file
		t.Errorf("log %q", b)
	}
	if fi, err := os.Stat(log + ".1"); err != nil || fi.Size() > vmlog.OwnMax {
		t.Errorf(".1: %v, %v", fi, err)
	}
	r.stop(t.Context(), vm)
}

// zombie reports whether pid ended and only waits to be reaped: the
// test's own sleeps are reaped by their Wait.
func zombie(pid int) bool {
	for range 50 {
		if !daemon.Alive(pid) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// A launch of a client warns once for each target its links can't reach
// now, one whose network policy isn't in force as it runs included, and
// starts nothing for a client without links.
func TestLinksAtLaunch(t *testing.T) {
	r, started := linkRunsEnv(t, "devvm links: 127.77.0.1:5432: carried\ndevvm links: 127.77.0.1:6379: carried\n"+
		"devvm links: 127.77.0.3:80: carried\ndevvm links: 127.77.0.4:1: carried\n")
	f, _ := backend.As[*backendtest.Fake](r.Registry.All()[0])
	vms := linkVMs(t, r.Dirs, "app", "db", "web", "gone")
	app, db, web, gone := vms[0], vms[1], vms[2], vms[3]
	for _, st := range vms {
		f.Instances[st.Name()] = backend.Instance{Name: st.Name(), Status: backend.StatusRunning}
	}
	if err := r.atLaunch(t.Context(), f, app); err != nil || len(*started) != 0 {
		t.Fatalf("no links: %v, started %v", err, *started)
	}
	links := []Link{linkTo(db, "db", 5432, "127.77.0.1"), linkTo(db, "db", 6379, "127.77.0.1"), linkTo(web, "web", 80, "127.77.0.3"), linkTo(gone, "gone", 1, "127.77.0.4")}
	if err := writeLinks(t.Context(), app, links); err != nil {
		t.Fatal(err)
	}
	if err := gone.Remove(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.Instances[db.Name()] = backend.Instance{Name: db.Name(), Status: backend.StatusStopped}
	setNet(t, app, "on")
	setNet(t, db, "on")
	var warned msg.Recorder
	fake(r.Env).OnWarn = warned.Add
	if err := r.atLaunch(t.Context(), f, app); err != nil || len(*started) != 1 {
		t.Fatalf("atLaunch: %v, started %v", err, *started)
	}
	want := []string{
		"link_target_stopped target=" + db.Name().String() + " vm=" + app.Name().String(),
		"link_tier_mismatch target=" + web.Name().String() + " target_tier=open tier=allowlist vm=" + app.Name().String(),
	}
	if !slices.Equal(warned.Names(), want) {
		t.Errorf("warned %q, want %q", warned.Names(), want)
	}

	// A running target whose network policy isn't in force as it runs:
	// warned, for a client with network policy alone, and nothing is
	// refused.
	f.Instances[db.Name()] = backend.Instance{Name: db.Name(), Status: backend.StatusRunning}
	f.Unconfined = true
	warned.Reset()
	if err := r.atLaunch(t.Context(), f, app); err != nil {
		t.Fatalf("atLaunch, unenforced: %v", err)
	}
	want = []string{
		"link_target_unenforced target=" + db.Name().String() + " vm=" + app.Name().String(),
		"link_tier_mismatch target=" + web.Name().String() + " target_tier=open tier=allowlist vm=" + app.Name().String(),
	}
	if !slices.Equal(warned.Names(), want) {
		t.Errorf("unenforced: warned %q, want %q", warned.Names(), want)
	}
	setNet(t, app, "off")
	warned.Reset()
	if err := r.atLaunch(t.Context(), f, app); err != nil || len(warned.Names()) != 0 {
		t.Errorf("a client without network policy: %v, warned %q", err, warned.Names())
	}
	f.Unconfined = false

	f.Capabilities = backend.Caps{}
	if err := r.atLaunch(t.Context(), f, app); msg.NamesOf(err) != "backend_lacks backend=fake feature=link what="+app.Name().String() {
		t.Errorf("a backend without links: %v", err)
	}
}
