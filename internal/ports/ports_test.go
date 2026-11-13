package ports

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/state"
)

func TestForwarding(t *testing.T) {
	for in, want := range map[string]backend.Ports{
		"":               {All: true},
		"all":            {All: true},
		"none":           {},
		"5173,8000-8010": {Ranges: []backend.PortRange{{From: 5173, To: 5173}, {From: 8000, To: 8010}}},
	} {
		if got, err := Forwarding(in); err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("Forwarding(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	// A bad entry, as a hand-edited state file has it, is refused, never
	// port 0.
	for _, bad := range []string{"80,x", "x", "80-y", "5173,,80"} {
		got, err := Forwarding(bad)
		if msg.NamesOf(err) != `port_invalid value="`+bad+`"` || len(got.Ranges) != 0 {
			t.Errorf("Forwarding(%q) = %+v, %v", bad, got, msg.NamesOf(err))
		}
	}
}

// newVM writes the state of a VM named name, as up does.
func newVM(t *testing.T, d paths.Dirs, name string) *state.VM {
	t.Helper()
	st, err := state.New(d, name)
	if err != nil {
		t.Fatal(err)
	}
	st.Image, st.Dir, st.Backend = "base", "/w", "fake"
	if err := st.Create(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	return st
}

func testDirs(t *testing.T) paths.Dirs {
	t.Helper()
	root := t.TempDir()
	return paths.Dirs{Data: filepath.Join(root, "data"), Config: filepath.Join(root, "config")}
}

// set sets a VM's ports section as f says, as devvm does.
func set(t *testing.T, st *state.VM, f func(*Ports)) {
	t.Helper()
	err := st.Update(t.Context(), func(st *state.VM) error {
		p, err := Of(st)
		if err != nil {
			return err
		}
		f(&p)
		return Set(st, p)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func host(t *testing.T, st *state.VM) Host {
	t.Helper()
	p, err := Of(st)
	if err != nil {
		t.Fatal(err)
	}
	return p.Host
}

// Used lists the ports VMs have; free avoids them.
func TestUsed(t *testing.T) {
	d := testDirs(t)
	st := newVM(t, d, "devvm-web-0123456789abcd")
	set(t, st, func(p *Ports) { p.Host = Host{SSH: 23456, Proxy: 34567, Desktop: 45678} })
	used, err := Used(d)
	if err != nil || !used[23456] || !used[34567] || !used[45678] || len(used) != 3 {
		t.Errorf("Used = %v, %v", used, err)
	}
	// And the ports VMs forward, entries that aren't ports skipped: all
	// and none name none. A stopped VM's are used too: a port devvm
	// chooses never takes them.
	set(t, st, func(p *Ports) { p.Forward = "5173,40000-40002, 41000, bad,9-1" })
	used, err = Used(d)
	if err != nil || len(used) != 8 || !used[5173] || !used[40000] || !used[40002] || !used[41000] {
		t.Errorf("Used with forwards = %v, %v", used, err)
	}
	for _, f := range []string{"all", "none", ""} {
		if got := (Ports{Forward: f}).Forwarded(); len(got) != 0 {
			t.Errorf("Forwarded with %q = %v", f, got)
		}
	}
	a := Allocator{Dirs: d, Listening: func(_ context.Context, port int) bool { return port%2 == 0 }}
	for range 200 {
		port, err := a.free(t.Context())
		if r := platform.ChoosablePorts(); err != nil || port < r.Lo || port > r.Hi || port%2 == 0 || used[port] {
			t.Fatalf("free = %d, %v", port, err)
		}
	}
	// A VM whose ports section can't be read reserves none.
	if err := state.WriteFile(d.VM("devvm-bad-0123456789abc1").State(),
		[]byte(`{"version": 1, "id": "x", "name": "devvm-bad-0123456789abc1", "image": "base", "dir": "/b", "backend": "fake", "ports": 1}`)); err != nil {
		t.Fatal(err)
	}
	if again, err := Used(d); err != nil || len(again) != len(used) {
		t.Errorf("Used with an unreadable section = %v, %v", again, err)
	}
}

// Who holds a forwarded host port: the running VM that forwards it, never
// a stopped one. Reserved is what a VM's forwarder never takes, Holders
// who forwards each port now.
func TestReserved(t *testing.T) {
	d := testDirs(t)
	const (
		self    = "devvm-a-0123456789abca"
		lister  = "devvm-b-0123456789abcb" // lists 5173,8000
		every   = "devvm-c-0123456789abcc" // forwards every port
		stopped = "devvm-d-0123456789abcd" // lists 9000, stopped
	)
	for name, fwd := range map[string]string{self: "5173,3000", lister: "5173,8000", every: "", stopped: "9000"} {
		st, err := state.New(d, name)
		if err != nil {
			t.Fatal(err)
		}
		st.Image, st.Dir, st.Backend = "base", "/"+name, "fake"
		if err := st.Create(t.Context(), nil); err != nil {
			t.Fatal(err)
		}
		set(t, st, func(p *Ports) { p.Forward, p.Host = fwd, Host{SSH: 40000 + len(fwd)} })
	}
	hosts := []int{40009, 40009, 40000, 40004} // each one's ssh port
	for _, tc := range []struct {
		name     string
		self     string
		live     map[string][]int // running forwarders, what they forward now
		reserved []int            // beyond every VM's ssh port
		holders  map[int]string
	}{{
		// CJ's case: a stopped VM lists 9000 and 5173 is listed by a VM
		// that's stopped too; neither is reserved.
		name: "all stopped", self: every,
	}, {
		name: "a stopped VM's listed port is free to a VM that forwards every port", self: every,
		live: map[string][]int{self: nil},
		// self lists 5173 and 3000 and runs: they're its, even before
		// it forwards them.
		reserved: []int{3000, 5173},
	}, {
		name: "two that list a port: neither waits until one forwards it", self: self,
		live: map[string][]int{lister: nil},
		// 8000 is the lister's alone; 5173 both list.
		reserved: []int{8000},
	}, {
		name: "the first to forward it keeps it", self: self,
		live:     map[string][]int{lister: {5173}},
		reserved: []int{5173, 8000},
		holders:  map[int]string{5173: lister},
	}, {
		name: "one that forwards every port is waited for, and gives it up", self: self,
		live:     map[string][]int{every: {3000, 4000}},
		reserved: []int{3000, 4000},
		holders:  map[int]string{3000: every, 4000: every},
	}, {
		name: "it gives up a port a running VM lists", self: every,
		live:     map[string][]int{self: nil, every: {3000}},
		reserved: []int{3000, 5173},
	}, {
		name: "its own forwards are never reserved from it", self: self,
		live: map[string][]int{self: {5173, 3000}},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			live := func(vm string) ([]int, bool) {
				now, ok := tc.live[vm]
				return now, ok
			}
			want := map[int]bool{}
			for _, n := range slices.Concat(hosts, tc.reserved) {
				want[n] = true
			}
			got, err := Reserved(d, tc.self, live)
			if err != nil || !maps.Equal(got, want) {
				t.Errorf("Reserved = %v, %v; want %v", slices.Sorted(maps.Keys(got)), err, slices.Sorted(maps.Keys(want)))
			}
			holders, err := Holders(d, tc.self, live)
			if tc.holders == nil {
				tc.holders = map[int]string{}
			}
			if err != nil || !maps.Equal(holders, tc.holders) {
				t.Errorf("Holders = %v, %v; want %v", holders, err, tc.holders)
			}
		})
	}
	// Used, for the ports devvm chooses, still has every VM's listed
	// ports, running or not.
	used, err := Used(d)
	if err != nil || !used[9000] || !used[5173] || !used[8000] || !used[3000] {
		t.Errorf("Used = %v, %v", used, err)
	}
}

// A VM's ssh port: chosen once and recorded, kept while it's free, or
// while the VM runs on it; a stopped VM gets a new one when another
// program has it, or it lists it to forward.
func TestSSH(t *testing.T) {
	d := testDirs(t)
	st := newVM(t, d, "devvm-web-0123456789abcd")
	taken := 0
	a := Allocator{Dirs: d, Listening: func(_ context.Context, port int) bool { return port == taken || port%2 == 0 }}
	sp, err := a.SSH(t.Context(), st, false)
	if err != nil || sp == 0 || sp%2 == 0 || host(t, st).SSH != sp {
		t.Fatalf("new: %d, %v, state %d", sp, err, host(t, st).SSH)
	}
	if got, err := a.SSH(t.Context(), st, false); err != nil || got != sp {
		t.Errorf("free: %d, %v, want %d", got, err, sp)
	}
	// Another program has it: kept while the VM runs (its backend has
	// it), a new one once it's stopped.
	taken = sp
	if got, err := a.SSH(t.Context(), st, true); err != nil || got != sp {
		t.Errorf("running: %d, %v, want %d", got, err, sp)
	}
	got, err := a.SSH(t.Context(), st, false)
	if err != nil || got == sp || host(t, st).SSH != got {
		t.Errorf("taken: %d (had %d), %v, state %d", got, sp, err, host(t, st).SSH)
	}
	// One of its own forwards.
	sp = got
	set(t, st, func(p *Ports) { p.Forward = "5173," + strconv.Itoa(sp) })
	if got, err := a.SSH(t.Context(), st, false); err != nil || got == sp || host(t, st).SSH != got {
		t.Errorf("forwarded: %d (had %d), %v", got, sp, err)
	}
	// Its forwards stay as they were asked.
	if p, _ := Of(st); p.Forward != "5173,"+strconv.Itoa(sp) {
		t.Errorf("forwards after: %q", p.Forward)
	}
}

// Two devvms choosing ports for VMs at once never choose the same one:
// each chooses under the claims lock, which no other holds meanwhile.
// With one port to choose from, and a slow check of it, one VM gets it
// and the others find it taken.
func TestChooseTogether(t *testing.T) {
	d := testDirs(t)
	names := []string{"devvm-a-0123456789abc1", "devvm-b-0123456789abc2", "devvm-c-0123456789abc3"}
	var vms []*state.VM
	for _, n := range names {
		vms = append(vms, newVM(t, d, n))
	}
	a := Allocator{Dirs: d, Range: platform.PortRange{Lo: 20000, Hi: 20000}, Listening: func(context.Context, int) bool {
		time.Sleep(20 * time.Millisecond) // a window for another to choose it too
		return false
	}}
	var wg sync.WaitGroup
	errs := make(chan error, len(vms))
	for _, st := range vms {
		wg.Go(func() {
			_, err := a.Choose(t.Context(), st, Proxy, func(int, Ports) bool { return true })
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	failed := 0
	for err := range errs {
		if err != nil {
			failed++
		}
	}
	got := 0
	for _, n := range names {
		st, err := state.ReadVM(d, n)
		if err != nil {
			t.Fatal(err)
		}
		if host(t, st).Proxy == 20000 {
			got++
		}
	}
	if got != 1 || failed != len(names)-1 {
		t.Errorf("%d VMs got the port and %d found it taken, want 1 and %d", got, failed, len(names)-1)
	}
	// A port kept is kept; one not kept is chosen again.
	b := Allocator{Dirs: d, Listening: func(context.Context, int) bool { return false }}
	st := vms[0]
	had := host(t, st).Proxy
	if p, err := b.Choose(t.Context(), st, Proxy, func(int, Ports) bool { return true }); had != 0 && (err != nil || p != had) {
		t.Errorf("kept: %d, %v, want %d", p, err, had)
	}
	if p, err := b.Choose(t.Context(), st, Desktop, func(int, Ports) bool { return false }); err != nil || p == 0 || host(t, st).Desktop != p {
		t.Errorf("desktop: %d, %v", p, err)
	}
}

// A build VM's ssh port, which no state records, is chosen under the
// claims lock too, never a VM's, and held until it's released: two builds
// starting at once never choose one port, and a VM's port avoids a build's
// until then.
func TestBuild(t *testing.T) {
	d := testDirs(t)
	st := newVM(t, d, "devvm-a-0123456789abc1")
	set(t, st, func(p *Ports) { p.Host.SSH = 20000 })
	a := Allocator{Dirs: d, Range: platform.PortRange{Lo: 20000, Hi: 20002}, Listening: func(context.Context, int) bool {
		time.Sleep(10 * time.Millisecond) // a window for another to choose it too
		return false
	}}
	builds := []string{"devvm-img-0123456789ab", "devvm-tools-build"}
	got := make([]int, len(builds))
	releases := make([]func(), len(builds))
	var wg sync.WaitGroup
	for i, vm := range builds {
		wg.Go(func() {
			var err error
			if got[i], releases[i], err = a.Build(t.Context(), vm, 0); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if got[0] == got[1] || slices.Contains(got, 20000) || slices.Contains(got, 0) {
		t.Fatalf("builds' ports %v (a VM has 20000)", got)
	}
	used, err := Used(d)
	if err != nil || !used[got[0]] || !used[got[1]] || !used[20000] {
		t.Errorf("Used with two builds starting = %v, %v", used, err)
	}
	// Full: a VM finds none, until a build's port is released.
	if _, err := a.Choose(t.Context(), st, Proxy, func(int, Ports) bool { return false }); err == nil {
		t.Error("a VM chose a port two builds and a VM hold")
	}
	releases[0]()
	if p, err := a.Choose(t.Context(), st, Proxy, func(int, Ports) bool { return false }); err != nil || p != got[0] {
		t.Errorf("after a release: %d, %v, want %d", p, err, got[0])
	}
	// A record that isn't a port reserves nothing.
	if err := state.WriteFile(filepath.Join(d.BuildPorts(), "x"), []byte("x\n")); err != nil {
		t.Fatal(err)
	}
	if used, err := Used(d); err != nil || len(used) != 3 {
		t.Errorf("Used with a bad record = %v, %v", used, err)
	}
	releases[1]()
	// A record no starting build holds (its devvm was killed mid-start)
	// reserves its port while the start it left may still bind it, then
	// only until the next port is chosen.
	stale := filepath.Join(d.BuildPorts(), "devvm-img-killed00000")
	if err := state.WriteFile(stale, []byte("20003\n")); err != nil {
		t.Fatal(err)
	}
	a.Range = platform.PortRange{Lo: 20003, Hi: 20003}
	if _, _, err := a.Build(t.Context(), builds[1], 0); err == nil {
		t.Error("a build took the port a killed devvm's start may still bind")
	}
	old := time.Now().Add(-staleAfter - time.Minute)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	if used, err := Used(d); err != nil || !used[20003] {
		t.Errorf("Used with a stale record = %v, %v", used, err)
	}
	n, release, err := a.Build(t.Context(), builds[1], 0)
	if err != nil || n != 20003 {
		t.Fatalf("a build beside a stale record: %d, %v, want 20003", n, err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the stale record stays: %v", err)
	}
	// A starting build's record stays while it's held, however long.
	if err := os.Chtimes(filepath.Join(d.BuildPorts(), builds[1]), old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Choose(t.Context(), st, Proxy, func(int, Ports) bool { return false }); err == nil {
		t.Error("a VM chose the port a starting build holds")
	}
	release()
	// Never the port the build VM's config has from its image.
	if n, _, err := a.Build(t.Context(), builds[1], 20003); err == nil {
		t.Errorf("a build chose %d, the port its clone has", n)
	}
	// Without a claims lock it can take, there's no port.
	bad := Allocator{Dirs: paths.Dirs{Data: filepath.Join(d.ClaimsLock(), "x")}, Listening: a.Listening}
	if _, _, err := bad.Build(t.Context(), builds[0], 0); err == nil {
		t.Error("a port without the claims lock")
	}
}
