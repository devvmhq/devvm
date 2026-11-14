package vm

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/backend/backendtest"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/netpolicy"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/vmssh"
)

// linkSessionEnv is an Env with a fake backend whose VMs are app, a
// client linked to db's port 5432, and db, which publishes it, runs, and
// has its host key pinned.
func linkSessionEnv(t *testing.T) (Links, *backendtest.Fake, *state.VM, *state.VM, Link) {
	t.Helper()
	root := t.TempDir()
	d := paths.Dirs{Data: filepath.Join(root, "data"), Config: filepath.Join(root, "config")}
	f := backendtest.New("fake", backend.Caps{backend.FeatureSSH: backend.Native, backend.FeatureLink: backend.Native})
	f.Target = backend.SSHTarget{
		Host: "127.0.0.1", Port: 62022, User: "u", Key: "/k",
		Options: []backend.SSHOption{{Key: "IdentitiesOnly", Value: "yes"}},
	}
	reg, err := backend.NewRegistry(f)
	if err != nil {
		t.Fatal(err)
	}
	client := vmState(t, d, "devvm-app-0123456789abc1")
	target := vmState(t, d, "devvm-db-0123456789abc2", withPublish("5432"))
	l := Link{Name: "db", Port: 5432, Target: target.Name(), TargetID: target.ID, Addr: netip.MustParseAddr("127.77.0.1")}
	if err := writeLinks(t.Context(), client, []Link{l}); err != nil {
		t.Fatal(err)
	}
	for _, vm := range []string{client.Name(), target.Name()} {
		f.Instances[vm] = backend.Instance{Name: vm, Status: backend.StatusRunning}
	}
	if err := os.MkdirAll(d.Config, 0o700); err != nil {
		t.Fatal(err)
	}
	pinHostKeys(t, d, f, client.Name(), target.Name())
	return Links{Env: &Env{Dirs: d, Registry: reg}}, f, client, target, l
}

// Each connection is checked against the states as they are then: a link
// that applies reaches its running target as devvm pins it; one the
// client no longer has, one whose target is gone, made again, no longer
// publishing the port or without the network policy the client has, or
// with it unenforced as it runs, one whose target's ssh isn't devvm's, and
// a target that doesn't run, are
// each refused with why, and the target is never started.
func TestLinkConn(t *testing.T) {
	e, f, client, target, l := linkSessionEnv(t)
	conn := vmssh.Link{Name: l.Name, Target: l.Target, Addr: l.Addr, Port: l.Port}
	check := func(want, why string) vmssh.LinkTarget {
		t.Helper()
		got, ev, w := e.linkConn(t.Context(), client.Name(), conn, &targetRuns{Now: time.Now})
		if ev != want || !strings.HasPrefix(w, why) {
			t.Errorf("linkConn = %q, %q; want %q, %q", ev, w, want, why)
		}
		return got
	}
	got := check("", "")
	want := vmssh.SSH{VM: target.Name(), User: "u", Key: "/k", KnownHosts: e.Dirs.VM(target.Name()).KnownHosts(), Port: 62022}
	if got.SSH != want || got.Start != "start-0" {
		t.Errorf("the target: %+v, want %+v", got, want)
	}

	// The target's ssh: on the host's loopback alone, on a port, and with
	// its host keys pinned for its current start, which a connection
	// never reads itself.
	f.Target.Host, f.Target.Port = "10.0.0.2", 22
	check(vmssh.EventLinkRefused, LinkNoSSH)
	f.Target.Host, f.Target.Port = "127.0.0.1", 0
	check(vmssh.EventLinkRefused, LinkNoSSH)
	f.Target.Port = 62022
	if err := vmssh.ForgetHostKeys(e.Dirs, target.Name()); err != nil {
		t.Fatal(err)
	}
	n := countExecs(f)
	check(vmssh.EventLinkRefused, LinkNoSSH)
	if countExecs(f) != n {
		t.Error("a connection read the target's host keys")
	}
	pinHostKeys(t, e.Dirs, f, target.Name())
	check("", "")

	// A target that doesn't run: refused, never started.
	f.Instances[target.Name()] = backend.Instance{Name: target.Name(), Status: backend.StatusStopped}
	check(vmssh.EventLinkStopped, "")
	for _, m := range f.Methods() {
		if m == "Start" {
			t.Error("a connection started the target")
		}
	}
	f.Instances[target.Name()] = backend.Instance{Name: target.Name(), Status: backend.StatusRunning}

	// The policy rule: a client with network policy, a target without.
	if err := set(t, client, KeyNet, "allowlist"); err != nil {
		t.Fatal(err)
	}
	check(vmssh.EventLinkRefused, LinkPolicy)
	if err := set(t, target, KeyNet, "allowlist"); err != nil {
		t.Fatal(err)
	}
	check("", "")

	// The rule as the target runs: given network policy while it ran, it
	// runs outside its proxy until its next start, and one whose
	// self-check failed may reach out directly. Either is refused for a
	// client with network policy, and neither matters to one without.
	f.Unconfined = true
	check(vmssh.EventLinkRefused, LinkUnenforced)
	f.Unconfined = false
	if err := set(t, target, keyCheck, netpolicy.CheckUnenforced); err != nil {
		t.Fatal(err)
	}
	check(vmssh.EventLinkRefused, LinkUnenforced)
	f.Unconfined = true
	if err := set(t, client, KeyNet, "open"); err != nil {
		t.Fatal(err)
	}
	check("", "")
	f.Unconfined = false
	if err := set(t, client, KeyNet, "allowlist"); err != nil {
		t.Fatal(err)
	}
	if err := set(t, target, keyCheck, netpolicy.CheckOK); err != nil {
		t.Fatal(err)
	}
	check("", "")

	if err := set(t, target, KeyPublish, "80"); err != nil {
		t.Fatal(err)
	}
	check(vmssh.EventLinkRefused, LinkUnpublished)
	if err := set(t, target, KeyPublish, "5432"); err != nil {
		t.Fatal(err)
	}

	// The link removed from the client, then another address's.
	if err := writeLinks(t.Context(), client, nil); err != nil {
		t.Fatal(err)
	}
	check(vmssh.EventLinkRefused, LinkRemoved)
	other := l
	other.Addr = netip.MustParseAddr("127.77.0.2")
	if err := writeLinks(t.Context(), client, []Link{other}); err != nil {
		t.Fatal(err)
	}
	check(vmssh.EventLinkRefused, LinkRemoved)
	if err := writeLinks(t.Context(), client, []Link{l}); err != nil {
		t.Fatal(err)
	}
	check("", "")

	// The target made again, then gone.
	vmState(t, e.Dirs, target.Name(), withPublish("5432"))
	check(vmssh.EventLinkRefused, LinkReplaced)
	if err := target.Remove(t.Context()); err != nil {
		t.Fatal(err)
	}
	check(vmssh.EventLinkRefused, LinkGone)
}

// A burst of a session's connections to one target asks its backend once
// whether it runs and how ssh reaches it, and each connection is still
// carried. The session's clock (Now) stands still, so however slowly a
// busy machine runs the burst, the first answer never gets older than
// linkRunFor.
func TestLinkCheckBurst(t *testing.T) {
	e, f, client, _, l := linkSessionEnv(t)
	conn := vmssh.Link{Name: l.Name, Target: l.Target, Addr: l.Addr, Port: l.Port}
	now := time.Unix(1e9, 0)
	e.Now = func() time.Time { return now }
	check := e.linkCheck(client.Name())
	var wg sync.WaitGroup
	refused := make(chan string, 256)
	for range 256 {
		wg.Go(func() {
			if got, ev, why := check(t.Context(), conn); ev != "" || got.Start != "start-0" {
				refused <- ev + ": " + why
			}
		})
	}
	wg.Wait()
	close(refused)
	for r := range refused {
		t.Errorf("a connection was refused: %s", r)
	}
	lookups := map[string]int{}
	for _, m := range f.Methods() {
		lookups[m]++
	}
	if lookups["Inspect"] != 1 || lookups["SSH"] != 1 {
		t.Errorf("256 checks asked the backend %v, want one Inspect and one SSH", lookups)
	}
}

// What a target's backend said of its run is reused for linkRunFor: a
// target that stops is noticed once that passes, while each connection's
// other checks, its policy's as the target runs included, are made again.
// With network policy, that check notices a target that stopped at once,
// as a stopped VM runs unconfined, and says it stopped.
func TestLinkRunReused(t *testing.T) {
	e, f, client, target, l := linkSessionEnv(t)
	now := time.Unix(1e9, 0)
	runs := &targetRuns{For: linkRunFor, Now: func() time.Time { return now }}
	conn := vmssh.Link{Name: l.Name, Target: l.Target, Addr: l.Addr, Port: l.Port}
	check := func(want, why string) {
		t.Helper()
		if _, ev, w := e.linkConn(t.Context(), client.Name(), conn, runs); ev != want || !strings.HasPrefix(w, why) {
			t.Errorf("linkConn = %q, %q; want %q, %q", ev, w, want, why)
		}
	}
	running := backend.Instance{Name: target.Name(), Status: backend.StatusRunning}
	stopped := backend.Instance{Name: target.Name(), Status: backend.StatusStopped}
	check("", "")
	f.Instances[target.Name()] = stopped
	check("", "")
	setNet(t, client, "on")
	setNet(t, target, "on")
	check(vmssh.EventLinkStopped, "")
	f.Instances[target.Name()] = running
	check("", "")
	f.Unconfined = true
	check(vmssh.EventLinkRefused, LinkUnenforced)
	f.Unconfined = false
	if err := set(t, target, KeyPublish, "80"); err != nil {
		t.Fatal(err)
	}
	check(vmssh.EventLinkRefused, LinkUnpublished)
	if err := set(t, target, KeyPublish, "5432"); err != nil {
		t.Fatal(err)
	}
	// A failed self-check is refused as it is, on the run looked up, or
	// when the backend can't say which run it's on.
	if err := set(t, target, keyCheck, netpolicy.CheckUnenforced); err != nil {
		t.Fatal(err)
	}
	check(vmssh.EventLinkRefused, LinkUnenforced)
	f.Errs = map[string]error{"Running": errors.New("no backend")}
	check(vmssh.EventLinkRefused, LinkUnenforced)
	f.Errs = nil
	if err := set(t, target, keyCheck, ""); err != nil {
		t.Fatal(err)
	}
	// Running on another start since: the run looked up has ended.
	f.Restart(target.Name())
	check(vmssh.EventLinkStopped, "")
	setNet(t, client, "off")
	f.Instances[target.Name()] = stopped
	now = now.Add(linkRunFor - time.Millisecond)
	check("", "")
	now = now.Add(time.Millisecond)
	check(vmssh.EventLinkStopped, "")

	// A connection whose session ends while it waits on the lookup in
	// flight finds the target stopped.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	runs.by[target.Name()+" "+target.ID] = &runLookup{done: make(chan struct{})}
	if run := runs.get(ctx, target.Name()+" "+target.ID, nil); run.start != "" {
		t.Errorf("a cancelled wait: %+v", run)
	}
}

// A lookup whose connection ends while it runs isn't kept: that
// connection finds the target stopped, while one waiting on the lookup and
// one coming in the same second get the target's run.
func TestLinkRunCancelled(t *testing.T) {
	now := time.Unix(1e9, 0)
	runs := &targetRuns{For: linkRunFor, Now: func() time.Time { return now }}
	started := make(chan struct{})
	var looks atomic.Int32
	look := func(ctx context.Context) targetRun {
		if looks.Add(1) == 1 {
			close(started)
			<-ctx.Done()
			return targetRun{}
		}
		return targetRun{start: "s"}
	}
	ctx, cancel := context.WithCancel(t.Context())
	first := make(chan targetRun)
	go func() { first <- runs.get(ctx, "k", look) }()
	<-started
	waiting := make(chan targetRun)
	go func() { waiting <- runs.get(t.Context(), "k", look) }()
	cancel()
	if run := <-first; run.start != "" {
		t.Errorf("the cancelled connection: %+v", run)
	}
	if run := <-waiting; run.start != "s" {
		t.Errorf("a connection waiting on the cancelled lookup: %+v", run)
	}
	if run := runs.get(t.Context(), "k", look); run.start != "s" {
		t.Errorf("a connection in the same second: %+v", run)
	}
	if n := looks.Load(); n != 2 {
		t.Errorf("%d lookups, want 2", n)
	}
}

// devvm link add and publish warn, refusing nothing, when a client with
// network policy links to a running target whose own isn't in force as
// it runs, which its connections would be refused for: never for a
// stopped target, whose policy applies at its next start, a target that
// runs confined, or a client without network policy.
func TestWarnUnenforced(t *testing.T) {
	e, f, client, target, _ := linkSessionEnv(t)
	var warned msg.Recorder
	e.Reporter = &msg.Fake{OnWarn: warned.Add}
	want := "link_target_unenforced target=" + target.Name() + " vm=" + client.Name()
	setNet(t, client, "on")
	setNet(t, target, "on")
	for _, c := range []struct {
		name               string
		unconfined, failed bool
		status             backend.Status
		clientNet          string
		warns              bool
	}{
		{name: "enforced", status: backend.StatusRunning, clientNet: "on"},
		{name: "unconfined", unconfined: true, status: backend.StatusRunning, clientNet: "on", warns: true},
		{name: "self-check failed", failed: true, status: backend.StatusRunning, clientNet: "on", warns: true},
		{name: "stopped", unconfined: true, status: backend.StatusStopped, clientNet: "on"},
		{name: "stopped, self-check failed", failed: true, status: backend.StatusStopped, clientNet: "on"},
		{name: "client without policy", unconfined: true, status: backend.StatusRunning, clientNet: "off"},
	} {
		f.Unconfined = c.unconfined
		check := netpolicy.CheckOK
		if c.failed {
			check = netpolicy.CheckUnenforced
		}
		if err := set(t, target, keyCheck, check); err != nil {
			t.Fatal(err)
		}
		f.Instances[target.Name()] = backend.Instance{Name: target.Name(), Status: c.status}
		setNet(t, client, c.clientNet)
		warned.Reset()
		e.warnUnenforced(t.Context(), client, target)
		w := warned.Names()
		if got := len(w) == 1 && w[0] == want; got != c.warns || len(w) > 1 {
			t.Errorf("%s: warned %q", c.name, w)
		}
		// The connection's check agrees with the warning.
		if c.status == backend.StatusRunning && policyUnenforced(t.Context(), f, client, target) != c.warns {
			t.Errorf("%s: policyUnenforced disagrees", c.name)
		}
	}
}

// A session carries every link its client's state has, applying or not,
// and none once the client is gone; it refuses a client that isn't
// running, or whose ssh isn't devvm's.
func TestSessionLinks(t *testing.T) {
	e, _, client, _, l := linkSessionEnv(t)
	got, err := e.sessionLinks(client.Name())
	want := vmssh.Link{Name: "db", Target: l.Target, Addr: l.Addr, Port: 5432, Hosts: []string{"db", "db.devvm.internal"}}
	if err != nil || len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Errorf("sessionLinks = %+v, %v", got, err)
	}
	if err := client.Remove(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got, err := e.sessionLinks(client.Name()); err != nil || len(got) != 0 {
		t.Errorf("a client that's gone: %+v, %v", got, err)
	}
	if err := e.Session(t.Context(), client.Name(), nil); err == nil || !strings.Contains(err.Error(), "no such VM") {
		t.Errorf("Session of a client that's gone: %v", err)
	}
	e, f, client, _, _ := linkSessionEnv(t)
	f.Instances[client.Name()] = backend.Instance{Name: client.Name(), Status: backend.StatusStopped}
	if err := e.Session(t.Context(), client.Name(), nil); msg.NamesOf(err) != "vm_not_running vm="+client.Name() {
		t.Errorf("Session of a stopped client: %v", err)
	}
	f.Instances[client.Name()] = backend.Instance{Name: client.Name(), Status: backend.StatusRunning}
	f.Capabilities = backend.Caps{backend.FeatureLink: backend.Native}
	if err := e.Session(t.Context(), client.Name(), nil); err == nil || !strings.Contains(err.Error(), "ssh isn't usable") {
		t.Errorf("Session without ssh: %v", err)
	}
	f.Capabilities = backend.Caps{backend.FeatureSSH: backend.Native}
	if err := e.Session(t.Context(), client.Name(), nil); msg.NamesOf(err) != "backend_lacks backend=fake feature=link vm="+client.Name() {
		t.Errorf("Session on a backend without links: %v", err)
	}
}

// A link session's events go to its client's event log, as devvm network
// log reads it, whether or not the client has network policy.
func TestLinkEvents(t *testing.T) {
	d := paths.Dirs{Data: t.TempDir()}
	le := linkEvents{path: d.VM("devvm-app-0123456789abc1").EventLog(), vm: "devvm-app-0123456789abc1", mu: new(sync.Mutex)}
	le.add(vmssh.EventLinkRefused, vmssh.Link{Name: "db", Target: "devvm-db-0123456789abc2", Port: 5432}, LinkUnpublished)
	events, err := netpolicy.NewLog(d, "devvm-app-0123456789abc1").Events(5)
	if err != nil || len(events) != 1 {
		t.Fatalf("Events = %+v, %v", events, err)
	}
	if ev := events[0]; ev.Event != "link_refused" || ev.Host != "db" || ev.Port != 5432 || ev.Target != "devvm-db-0123456789abc2" || ev.Reason != "unpublished" {
		t.Errorf("the event: %+v", ev)
	}
}
