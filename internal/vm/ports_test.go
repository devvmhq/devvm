package vm

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/project"
)

func TestAddRemovePorts(t *testing.T) {
	for _, c := range []struct {
		rm             bool
		old, list, out string
		err            string
	}{
		{old: "none", list: "5173", out: "5173"},
		{old: "5173", list: "8080,5173", out: "5173,8080"},
		{old: "all", list: "8080", out: "all"},
		{old: "5173", list: "all", err: `ports_list_needed value="all"`},
		{old: "5173", list: "0", err: `port_invalid value="0"`},
		{rm: true, old: "5173,8000-8010", list: "5173", out: "8000-8010"},
		{rm: true, old: "5173", list: " 5173 ", out: "none"},
		{rm: true, old: "all", list: "5173", err: "ports_rm_all"},
		{rm: true, old: "5173", list: "8080", err: "port_not_listed port=8080 ports=5173"},
		{rm: true, old: "none", list: "5173", err: "port_not_listed port=5173 ports=none"},
		{rm: true, old: "5173", list: "nope", err: `port_invalid value="nope"`},
		{rm: true, old: "5173", list: "none", err: `ports_list_needed value="none"`},
		{rm: true, old: "80,x", list: "80", err: `port_invalid value="80,x"`},
	} {
		f, name := AddPorts, "AddPorts"
		if c.rm {
			f, name = RemovePorts, "RemovePorts"
		}
		got, err := f(c.old, c.list)
		if got != c.out || msg.NamesOf(err) != c.err {
			t.Errorf("%s(%q, %q) = %q, %v; want %q, %s", name, c.old, c.list, got, msg.NamesOf(err), c.out, c.err)
		}
	}
}

// only is a forwarder start's -only, "" for every port.
func only(args []string) string {
	if i := slices.Index(args, "-only"); i >= 0 {
		return args[i+1]
	}
	return ""
}

// A start forwards its state's ports once the VM runs, through devvm's
// forwarder, after its backend's start: the listed ports less those a
// host program has, warned; every port; none for none. Its backend's
// start never has them.
func TestStartPorts(t *testing.T) {
	for _, tc := range []struct {
		ports, warned, only string
		starts              int
	}{
		{"5173-5175,8080", "ports_taken count=2 ports=5174,8080 vm=", "5173,5175", 1},
		{"5173", "", "5173", 1},
		{"none", "", "", 0},
		{"all", "", "", 1},
	} {
		t.Run(strings.ReplaceAll(tc.ports, ",", "+"), func(t *testing.T) { // a comma isn't a project path's
			e, f, _ := upEnv(t, allNative())
			starts := testForwarders(t, e, f)
			e.PortInUse = func(_ context.Context, p int) bool { return p == 5174 || p == 8080 }
			var warned msg.Recorder
			fake(e).OnWarn = warned.Add
			st := existingVM(t, e, Options{Network: "open", Ports: tc.ports})
			ms := f.Methods()
			if i, j := slices.Index(ms, "Start"), slices.Index(ms, "SSH"); tc.starts > 0 && (i < 0 || j < i) {
				t.Errorf("calls %q: no forwarder after its start", ms)
			}
			if len(*starts) != tc.starts || tc.starts > 0 && only((*starts)[0]) != tc.only {
				t.Errorf("forwarders %q, want %d with -only %q", *starts, tc.starts, tc.only)
			}
			if tc.starts > 0 && !forwarderRuns(e, st.Name()) {
				t.Error("its forwarder isn't running")
			}
			want := []string(nil)
			if tc.warned != "" {
				want = []string{tc.warned + st.Name()}
			}
			if w := warned.Names(); !slices.Equal(w, want) {
				t.Errorf("warned %q, want %q", w, want)
			}
			// devvm stop stops it.
			if err := (Lifecycle{e}).Stop(t.Context()); err != nil {
				t.Fatal(msg.NamesOf(err))
			}
			if forwarderRuns(e, st.Name()) {
				t.Error("its forwarder runs with the VM stopped")
			}
		})
	}
}

// A running VM's ports change at once: its state first, then its
// forwarder started again with them (its proxy's port reserved, taken
// ports warned about), and devvm ports shows them. A stopped VM's state
// changes alone, and its backend isn't asked to change.
func TestSetPortsRunning(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	starts := testForwarders(t, e, f)
	var infos, warned msg.Recorder
	fake(e).OnInfo = infos.Add
	fake(e).OnWarn = warned.Add
	st := existingVM(t, e, Options{Network: "open", Ports: "5173"})
	vm := st.Name()
	if err := set(t, st, "proxy_port", "30001"); err != nil {
		t.Fatal(err)
	}
	e.PortInUse = func(_ context.Context, p int) bool { return p == 8080 }
	n, before := len(f.Calls()), len(*starts)
	infos.Reset()
	warned.Reset()
	s := Settings{e}
	if err := s.SetPorts(t.Context(), PortsChange{Op: "add", List: "8080-8081"}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	noRestart := func(n int) {
		t.Helper()
		for _, c := range f.Calls()[n:] {
			if c.Method == "Start" || c.Method == "Stop" {
				t.Errorf("%s called", c.Method)
			}
		}
	}
	noRestart(n)
	if len(*starts) != before+1 {
		t.Fatalf("forwarders %q", *starts)
	}
	got := (*starts)[len(*starts)-1]
	if i := slices.Index(got, "-reserve"); i < 0 || got[i+1] != "60022,30001" || only(got) != "5173,8081" {
		t.Errorf("forwarder %q", got)
	}
	if !slices.Equal(infos.Names(), []string{"ports_forwarding ports=5173,8080-8081 vm=" + vm}) || !slices.Equal(warned.Names(), []string{"ports_taken count=1 ports=8080 vm=" + vm}) {
		t.Errorf("said %q, warned %q", infos.Names(), warned.Names())
	}
	if ports, err := s.Ports(t.Context()); err != nil || ports.List != "5173,8080-8081" || ports.Held != nil {
		t.Errorf("ports: %q, %v", ports, err)
	}
	// None: its forwarder stops.
	if err := s.SetPorts(t.Context(), PortsChange{List: "none"}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if forwarderRuns(e, vm) || len(*starts) != before+1 {
		t.Errorf("none: forwarder running %v, started %q", forwarderRuns(e, vm), *starts)
	}
	if err := f.Stop(t.Context(), vm); err != nil {
		t.Fatal(err)
	}
	n = len(f.Calls())
	infos.Reset()
	if err := s.SetPorts(t.Context(), PortsChange{List: "5173"}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	noRestart(n)
	if len(*starts) != before+1 || !slices.Equal(infos.Names(), []string{"ports_now ports=5173 vm=" + vm}) {
		t.Errorf("stopped: started %q, said %q", *starts, infos.Names())
	}
	if ports, err := s.Ports(t.Context()); err != nil || ports.List != "5173" {
		t.Errorf("stopped: ports %q, %v", ports, err)
	}
}

// A VM whose state lists a port that isn't one doesn't start.
func TestStartBadPorts(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	st := existingVM(t, e, Options{Network: "open", Ports: "5173"})
	if err := set(t, st, KeyPorts, "5173,x"); err != nil {
		t.Fatal(err)
	}
	if err := f.Stop(t.Context(), st.Name()); err != nil {
		t.Fatal(err)
	}
	before := len(f.Calls())
	if err := e.newTarget(project.Folder{Dir: st.Dir, Name: st.Name()}, st, f).ensureRunning(t.Context()); msg.NamesOf(err) != `port_invalid value="5173,x"` {
		t.Errorf("start = %v", msg.NamesOf(err))
	}
	if slices.Contains(f.Methods()[before:], "Start") {
		t.Errorf("started: %q", f.Methods()[before:])
	}
}
