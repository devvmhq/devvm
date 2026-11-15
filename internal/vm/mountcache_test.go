package vm

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/backend/backendtest"
	"github.com/devvmhq/devvm/internal/daemon"
	"github.com/devvmhq/devvm/internal/gitprotect"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/shell"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/vmname"
)

// A new VM gets none where its backend has the choice: in its state, made
// with it and started with it. On a backend without it, it gets none to
// choose.
func TestUpCacheMode(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps backend.Caps
		want string
	}{
		{"the choice", allNative(), backend.CacheNone},
		{"no choice", without(allNative(), backend.FeatureMountCache), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, f, _ := upEnv(t, tc.caps)
			st := existingVM(t, e, Options{Network: "open"})
			if got := cacheOf(st); got != tc.want {
				t.Errorf("state's mode %q, want %q", got, tc.want)
			}
			spec, _ := call(f, "CreateVM").Args[1].(backend.VMSpec)
			w, _ := call(f, "Start").Args[1].(backend.Want)
			if spec.ProjectCache != tc.want || w.ProjectCache != tc.want {
				t.Errorf("made with %q, started with %q, want %q", spec.ProjectCache, w.ProjectCache, tc.want)
			}
		})
	}
}

// without is caps with fs none.
func without(caps backend.Caps, fs ...backend.Feature) backend.Caps {
	for _, f := range fs {
		caps[f] = backend.None
	}
	return caps
}

// A launch warns of a project folder mount whose cache mode nobody chose,
// where the mode its backend keeps can lose writes: not for a --clone VM,
// a mode chosen, none, or a backend with no mode to report.
func TestWarnMountCache(t *testing.T) {
	d := paths.Dirs{Data: t.TempDir()}
	for _, tc := range []struct {
		name  string
		set   []func(*state.VM) error
		has   string // the instance's mode
		warns bool
	}{
		{"unchosen, mmap", nil, backend.CacheMmap, true},
		{"unchosen, none", nil, backend.CacheNone, false},
		{"unchosen, nothing reported", nil, "", false},
		{"chosen none, mmap until its start", []func(*state.VM) error{withCache(backend.CacheNone)}, backend.CacheMmap, false},
		{"--clone", []func(*state.VM) error{func(st *state.VM) error { return state.SetSection(st, cloneSection, cloneInVM{On: true}) }}, backend.CacheMmap, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			warned := &msg.Recorder{}
			e := &Env{Reporter: &msg.Fake{OnWarn: warned.Add}}
			st := vmState(t, d, "devvm-p-00000000000000", tc.set...)
			e.warnMountCache(st, backend.Instance{ProjectCache: tc.has})
			want := []string(nil)
			if tc.warns {
				want = []string{"mount_cache_unchosen mode=mmap vm=devvm-p-00000000000000"}
			}
			if got := warned.Names(); !slices.Equal(got, want) {
				t.Errorf("warned %q, want %q", got, want)
			}
		})
	}
}

// withCache chooses a VM's cache mode.
func withCache(mode string) func(*state.VM) error {
	return func(st *state.VM) error { return state.SetSection(st, mountSection, mountInVM{Cache: mode}) }
}

// A running VM whose chosen mode its mount doesn't have yet gets it at its
// next start, and its start needs the backend's mount-cache; a mode
// nobody chose, or a --clone VM's, waits for nothing and needs nothing.
func TestCacheAtNextStart(t *testing.T) {
	d := paths.Dirs{Data: t.TempDir()}
	clone := func(st *state.VM) error { return state.SetSection(st, cloneSection, cloneInVM{On: true}) }
	for _, tc := range []struct {
		name   string
		set    []func(*state.VM) error
		has    string
		waits  bool
		needed bool
	}{
		{"chosen, other", []func(*state.VM) error{withCache(backend.CacheNone)}, backend.CacheMmap, true, true},
		{"chosen, had", []func(*state.VM) error{withCache(backend.CacheNone)}, backend.CacheNone, false, true},
		{"chosen, nothing reported", []func(*state.VM) error{withCache(backend.CacheNone)}, "", false, true},
		{"unchosen", nil, backend.CacheMmap, false, false},
		{"--clone", []func(*state.VM) error{withCache(backend.CacheNone), clone}, "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := vmState(t, d, "devvm-p-00000000000000", tc.set...)
			in := backend.Instance{Status: backend.StatusRunning, Project: projectMount(st), ProjectCache: tc.has}
			cs := nextStartChanges(st, in, false, false)
			if got := slices.ContainsFunc(cs, func(m msg.M) bool { return m.ID() == "next_start_mount_cache" }); got != tc.waits {
				t.Errorf("waits %v, want %v: %q", got, tc.waits, cs)
			}
			n, err := startNeeds(st, in)
			if err != nil {
				t.Fatal(err)
			}
			if got := slices.Contains(requirements(n), backend.FeatureMountCache); got != tc.needed {
				t.Errorf("needs mount-cache %v, want %v", got, tc.needed)
			}
		})
	}
}

// devvm clone off mounts the folder anew: with the mode chosen for the
// VM, else none, where its backend has the choice, and that's recorded;
// devvm clone on and a backend without the choice record nothing. A mode
// recorded is kept.
func TestSwitchCache(t *testing.T) {
	d := paths.Dirs{Data: t.TempDir()}
	native, lacks := backendtest.New("fake", allNative()), backendtest.New("fake", without(allNative(), backend.FeatureMountCache))
	for _, tc := range []struct {
		name   string
		set    []func(*state.VM) error
		b      backend.Backend
		clone  string
		mounts string // the new mount's mode
		want   string // the state's after
	}{
		{"unchosen", nil, native, CloneOff, backend.CacheNone, backend.CacheNone},
		{"chosen", []func(*state.VM) error{withCache("a-mode")}, native, CloneOff, "a-mode", "a-mode"},
		// A stored mmap is none chosen: the new mount gets none, recorded.
		{"stored mmap", []func(*state.VM) error{withCache(backend.CacheMmap)}, native, CloneOff, backend.CacheNone, backend.CacheNone},
		{"no choice", nil, lacks, CloneOff, "", ""},
		{"clone on", nil, native, CloneOn, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg, err := backend.NewRegistry(native)
			if err != nil {
				t.Fatal(err)
			}
			e := remaking{&Env{Registry: reg}}
			st := vmState(t, d, "devvm-p-00000000000000", append([]func(*state.VM) error{withCache("")}, tc.set...)...)
			tg := &target{st: st, b: tc.b}
			got := ""
			if tc.clone == CloneOff {
				got = e.switchCache(tg)
			}
			if got != tc.mounts {
				t.Errorf("mounted with %q, want %q", got, tc.mounts)
			}
			if err := switchedTo(st, tc.clone, nil, gitprotect.InVM{}, got); err != nil {
				t.Fatal(err)
			}
			if c := sectionOf[mountInVM](st, mountSection).Cache; c != tc.want {
				t.Errorf("recorded %q, want %q", c, tc.want)
			}
		})
	}
}

// devvm mount-cache: shown with whether it was chosen and what its next
// start changes, warning of a mode nobody chose; none, the one mode it
// sets, waits for a running VM's next start (no terminal: no restart),
// and a stopped one's start makes it; a mode as it has is said so; any
// other mode is refused; a backend without the choice refuses.
func TestSetMountCache(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	s := Settings{e}
	said, warned := &msg.Recorder{}, &msg.Recorder{}
	fake(e).OnInfo, fake(e).OnWarn = said.Add, warned.Add
	if _, err := s.MountCache(t.Context()); msg.NamesOf(err) != "no_vm_mount_cache dir="+e.Dir {
		t.Errorf("no VM: %s", msg.NamesOf(err))
	}
	st := existingVM(t, e, Options{Network: "open"})
	vm := st.Name()
	if got, err := s.MountCache(t.Context()); err != nil || got != (MountCacheShown{Mode: backend.CacheNone, Chosen: true}) {
		t.Errorf("new VM: %+v, %v", got, msg.NamesOf(err))
	}
	// As a VM made before devvm chose a mode: none recorded, its
	// backend's mmap.
	change(t, st, withCache(""))
	in := f.Instances[vm]
	in.ProjectCache = backend.CacheMmap
	f.Instances[vm] = in
	if got, err := s.MountCache(t.Context()); err != nil || got != (MountCacheShown{Mode: backend.CacheMmap}) ||
		!slices.Equal(warned.Names(), []string{"mount_cache_unchosen mode=mmap vm=" + vm.String()}) {
		t.Errorf("unchosen: %+v, %v, warned %q", got, msg.NamesOf(err), warned.Names())
	}
	*warned = msg.Recorder{}
	before := len(f.Calls())
	if err := s.SetMountCache(t.Context(), backend.CacheNone); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if ms := f.Methods()[before:]; slices.Contains(ms, "Start") || slices.Contains(ms, "Stop") {
		t.Errorf("a running VM was changed: %q", ms)
	}
	if got := warned.Names(); !slices.Equal(got, []string{"next_start count=1 vm=" + vm.String()}) {
		t.Errorf("warned %q", got)
	}
	if got, err := s.MountCache(t.Context()); err != nil || got != (MountCacheShown{Mode: backend.CacheMmap, Chosen: true, Pending: backend.CacheNone}) {
		t.Errorf("pending: %+v, %v", got, msg.NamesOf(err))
	}
	if err := f.Stop(t.Context(), vm); err != nil {
		t.Fatal(err)
	}
	if err := (Launch{e}).Up(t.Context(), Options{}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if w := call(f, "Start").Args[1].(backend.Want); w.ProjectCache != backend.CacheNone {
		t.Errorf("started wanting %+v", w)
	}
	// As it has: said so.
	*said = msg.Recorder{}
	if err := s.SetMountCache(t.Context(), backend.CacheNone); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if got := said.Names(); !slices.Equal(got, []string{"vm_mount_cache mode=none vm=" + vm.String()}) {
		t.Errorf("said %q", got)
	}
	for _, mode := range []string{backend.CacheMmap, "loose"} {
		if err := s.SetMountCache(t.Context(), mode); msg.NamesOf(err) != `mount_cache_invalid value="`+mode+`"` {
			t.Errorf("%s: %s", mode, msg.NamesOf(err))
		}
	}
	// Stopped, then set: from its next start.
	if err := f.Stop(t.Context(), vm); err != nil {
		t.Fatal(err)
	}
	change(t, st, withCache(""))
	in = f.Instances[vm]
	in.ProjectCache = backend.CacheMmap
	f.Instances[vm] = in
	*said = msg.Recorder{}
	if err := s.SetMountCache(t.Context(), backend.CacheNone); err != nil || !slices.Equal(said.Names(), []string{"mount_cache_pending mode=none vm=" + vm.String()}) {
		t.Errorf("stopped: %v, said %q", msg.NamesOf(err), said.Names())
	}
	// A mount with no cache mode to report (not 9p): refused, as a mode
	// saved for it would never apply.
	in = f.Instances[vm]
	in.ProjectCache = ""
	f.Instances[vm] = in
	if err := s.SetMountCache(t.Context(), backend.CacheNone); msg.NamesOf(err) != "mount_cache_no_choice vm="+vm.String() {
		t.Errorf("no 9p mount: %s", msg.NamesOf(err))
	}
	if _, err := s.MountCache(t.Context()); msg.IDOf(err) != "mount_cache_no_choice" {
		t.Errorf("shown, no 9p mount: %s", msg.NamesOf(err))
	}
	f.Capabilities = without(allNative(), backend.FeatureMountCache)
	change(t, st, withCache(""))
	if err := s.SetMountCache(t.Context(), backend.CacheNone); msg.IDOf(err) != "backend_lacks" {
		t.Errorf("without the feature: %s", msg.NamesOf(err))
	}
	if _, err := s.MountCache(t.Context()); msg.IDOf(err) != "backend_lacks" {
		t.Errorf("shown without the feature: %s", msg.NamesOf(err))
	}
	if c := cacheOf(readState(t, e.Dirs, vm)); c != "" {
		t.Errorf("refused, but recorded %q", c)
	}
}

// A start whose backend stopped the VM again, as its project folder
// couldn't be mounted again with its new cache mode
// (backend.RemountError), gets one start again, which asks for the check
// and gets a proxy of its own: the launch goes on when that start does,
// and fails with its error when it fails too, with no third start.
func TestRemountRestart(t *testing.T) {
	e, f, execs := upEnv(t, allNative())
	st := existingVM(t, e, Options{Network: "open"})
	vm := st.Name()
	proxies, start := 0, e.Proxy.Start
	e.Proxy.Start = func(c daemon.Cmd) (int, error) { proxies++; return start(c) }
	for _, tc := range []struct {
		name  string
		fails int // how many starts fail
		err   string
	}{
		{"mounted by the restart", 1, ""},
		{"not mounted after it", 2, "mount_cache_unmounted vm=" + vm.String()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := f.Stop(t.Context(), vm); err != nil {
				t.Fatal(err)
			}
			before, shells := len(f.Calls()), len(*execs)
			n := 0
			proxies = 0
			f.StartErr = func(vmname.VM, backend.Want) error {
				if n++; n <= tc.fails {
					return &backend.RemountError{Msg: msg.MountCacheUnmounted(vm)}
				}
				return nil
			}
			defer func() { f.StartErr = nil }()
			err := (Launch{e}).Up(t.Context(), Options{})
			if got := msg.NamesOf(err); got != tc.err {
				t.Errorf("launch: %q, want %q", got, tc.err)
			}
			var wants []backend.Want
			for _, c := range f.Calls()[before:] {
				if c.Method == "Start" {
					wants = append(wants, c.Args[1].(backend.Want))
				}
			}
			if len(wants) != 2 || wants[0].CheckCache || !wants[1].CheckCache {
				t.Errorf("started %d times, wanting %+v", len(wants), wants)
			}
			if proxies != 2 {
				t.Errorf("%d proxies started, want one for each start", proxies)
			}
			if opened := len(*execs) > shells; opened != (tc.err == "") {
				t.Errorf("opened a shell %v", opened)
			}
		})
	}
}

// On a confined tier, each start has its interrupt's undo (undoStart)
// while it runs, removed once it returns, so the restart's is the only
// one then and none is left. An interrupt between the two starts gets no
// start again: its proxy is stopped, and only the first start's undo is
// left, for the run's cleanups. A start again whose proxy fails to start
// fails the launch with the VM stopped and no proxy.
func TestRemountRestartConfined(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	f.LifelineFile = filepath.Join(e.Dirs.Lima, "vm", "ha.pid")
	f.ExecFunc = noDirect
	st := existingVM(t, e, Options{Network: "allowlist"})
	vm := st.Name()
	var mu sync.Mutex
	live := map[int]func(context.Context){}
	next := 0
	fake(e).OnCleanup = func(c func(context.Context)) func() {
		mu.Lock()
		defer mu.Unlock()
		n := next
		next++
		live[n] = c
		return func() { mu.Lock(); defer mu.Unlock(); delete(live, n) }
	}
	count := func() int { mu.Lock(); defer mu.Unlock(); return len(live) }
	starts := func(before int) int {
		n := 0
		for _, c := range f.Calls()[before:] {
			if c.Method == "Start" {
				n++
			}
		}
		return n
	}
	remount := &backend.RemountError{Msg: msg.MountCacheUnmounted(vm)}
	t.Run("restart", func(t *testing.T) {
		if err := f.Stop(t.Context(), vm); err != nil {
			t.Fatal(err)
		}
		base, before := count(), len(f.Calls())
		var during []int
		f.StartErr = func(vmname.VM, backend.Want) error {
			during = append(during, count())
			if len(during) == 1 {
				return remount
			}
			return nil
		}
		defer func() { f.StartErr = nil }()
		if err := (Launch{e}).Up(t.Context(), Options{}); err != nil {
			t.Fatal(msg.NamesOf(err))
		}
		if len(during) != 2 || during[0] != base+1 || during[1] != base+1 || count() != base {
			t.Errorf("cleanups: %d before, %v during the starts, %d after", base, during, count())
		}
		if _, ok := e.Proxy.PID(vm); !ok || starts(before) != 2 {
			t.Errorf("%d starts, proxy running %v", starts(before), ok)
		}
	})
	t.Run("interrupted between the starts", func(t *testing.T) {
		if err := f.Stop(t.Context(), vm); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		base, before := count(), len(f.Calls())
		f.StartErr = func(vmname.VM, backend.Want) error {
			fake(e).Interrupt = true
			cancel()
			return remount
		}
		defer func() { f.StartErr, fake(e).Interrupt = nil, false }()
		if err := (Launch{e}).Up(ctx, Options{}); err == nil {
			t.Fatal("an interrupted launch went on")
		}
		if n := starts(before); n != 1 {
			t.Errorf("%d starts, want 1", n)
		}
		if _, ok := e.Proxy.PID(vm); ok {
			t.Error("its proxy runs")
		}
		if count() != base+1 {
			t.Fatalf("cleanups: %d before, %d after, want the one undo kept", base, count())
		}
		mu.Lock()
		var undo func(context.Context)
		for n, c := range live {
			undo = c
			delete(live, n)
		}
		mu.Unlock()
		undo(t.Context())
		if f.Instances[vm].Status != backend.StatusStopped {
			t.Errorf("the undo left it %s", f.Instances[vm].Status)
		}
	})
	t.Run("its proxy fails at the start again", func(t *testing.T) {
		if err := f.Stop(t.Context(), vm); err != nil {
			t.Fatal(err)
		}
		before, proxies := len(f.Calls()), 0
		start := e.Proxy.Start
		e.Proxy.Start = func(c daemon.Cmd) (int, error) {
			if proxies++; proxies == 2 {
				return 0, errors.New("no proxy")
			}
			return start(c)
		}
		defer func() { e.Proxy.Start = start }()
		f.StartErr = func(vmname.VM, backend.Want) error { return remount }
		defer func() { f.StartErr = nil }()
		if err := (Launch{e}).Up(t.Context(), Options{}); err == nil {
			t.Fatal("the launch went on without a proxy")
		}
		if n := starts(before); n != 1 || f.Instances[vm].Status != backend.StatusStopped {
			t.Errorf("%d starts, the VM %s", n, f.Instances[vm].Status)
		}
		if _, ok := e.Proxy.PID(vm); ok {
			t.Error("a proxy runs")
		}
	})
}

// A state whose cache mode isn't one devvm sets is refused as the VM is
// found.
func TestCheckStateCache(t *testing.T) {
	d := paths.Dirs{Data: t.TempDir()}
	st := vmState(t, d, "devvm-p-00000000000000", withCache("loose"))
	err := checkState(st)
	ce, ok := errors.AsType[*state.CorruptError](err)
	if !ok || ce.Path != st.File() || ce.Msg.ID() != "mount_cache_invalid" {
		t.Errorf("a mode that isn't one: %v, want the state file named", err)
	}
	// A stored mmap, as a build of devvm before none alone could write,
	// is none chosen: the VM still works, and warns.
	for _, mode := range []string{backend.CacheMmap, backend.CacheNone} {
		st := vmState(t, d, "devvm-p-00000000000000", withCache(mode))
		if err := checkState(readState(t, d, st.Name())); err != nil {
			t.Errorf("%s: %v", mode, err)
		}
	}
	st = vmState(t, d, "devvm-p-00000000000000", withCache(backend.CacheMmap))
	if c := cacheOf(st); c != "" {
		t.Errorf("a stored mmap reads as %q", c)
	}
	warned := &msg.Recorder{}
	(&Env{Reporter: &msg.Fake{OnWarn: warned.Add}}).warnMountCache(st, backend.Instance{ProjectCache: backend.CacheMmap})
	if len(warned.Names()) != 1 {
		t.Errorf("a stored mmap: warned %q", warned.Names())
	}
}

// A launch into a running VM whose project folder has a cache mode (a
// Linux host's 9p) checks the folder is mounted, refusing one that isn't;
// one without a mode, or a --clone VM, isn't checked.
func TestCheckMounted(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cache   string // the instance's
		answer  string // what the guest says, or "" for a check that fails to run
		want    string // the launch's refusal
		checked bool
	}{
		{"mounted", backend.CacheNone, "devvm-mounted\n", "", true},
		{"not mounted", backend.CacheNone, "devvm-not-mounted\n", "project_unmounted", true},
		{"the check fails", backend.CacheNone, "", "project_unchecked", true},
		{"no answer", backend.CacheNone, "\n", "project_unchecked", true},
		{"no mode to report", "", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, f, _ := upEnv(t, allNative())
			st := existingVM(t, e, Options{Network: "open"})
			vm := st.Name()
			in := f.Instances[vm]
			in.ProjectCache = tc.cache
			f.Instances[vm] = in
			checks := 0
			f.ExecFunc = func(_ vmname.VM, argv []string, o backend.ExecOpts) error {
				if len(argv) == 3 && argv[0] == "sh" && strings.HasPrefix(argv[2], "if mountpoint -q "+shell.Quote(e.Dir)+";") {
					checks++
					if tc.name == "the check fails" {
						return errors.New("ssh: connect to host: connection refused")
					}
					_, _ = io.WriteString(o.Stdout, tc.answer)
				}
				return nil
			}
			err := (Launch{e}).Up(t.Context(), Options{})
			want := ""
			if tc.want != "" {
				want = tc.want + " vm=" + vm.String()
			}
			if got := msg.NamesOf(err); got != want {
				t.Errorf("launch: %q, want %q", got, want)
			}
			if (checks > 0) != tc.checked {
				t.Errorf("checked %d times, want %v", checks, tc.checked)
			}
		})
	}
}
