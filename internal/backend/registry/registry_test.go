package registry

import (
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/assets"
	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/backend/backendtest"
	"github.com/devvmhq/devvm/internal/backend/lima"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/ubuntu"
	"github.com/devvmhq/devvm/internal/vmname"
	"github.com/devvmhq/devvm/internal/vmssh"
)

// A test build's overrides reach the registry and every backend; without
// them, the backends keep their own, whatever the environment says.
func TestOverrides(t *testing.T) {
	for k, v := range map[string]string{
		"DEVVM_KVM": "/x/kvm", "DEVVM_UBUNTU_URL": "file:///x/ubuntu", "DEVVM_UNREVIEWED": "lima:egress-proxy",
		platform.TestCPUs: "1234",
	} {
		t.Setenv(k, v)
	}
	d := paths.Dirs{Data: t.TempDir(), Config: t.TempDir(), Lima: t.TempDir()}
	o := Overrides{
		Host:       platform.Overrides{CPUs: "1234", MemoryGiB: "4321", FreeGiB: "0"},
		Unreviewed: "lima:egress-proxy",
		KVM:        "/x/kvm", UbuntuURL: "file:///x/ubuntu/",
		SSHPorts: "20001, 20002,x,0,70000", PortRange: "10000-10199",
	}
	for _, c := range []struct {
		o    Overrides
		want bool
	}{{Overrides{}, false}, {o, true}} {
		r, err := New(d, assets.FS, &msg.Fake{}, c.o, nil)
		if err != nil {
			t.Fatal(err)
		}
		lb, _ := r.Lookup("lima")
		l, _ := backend.As[*lima.Lima](lb)
		mem, _ := r.Host.Memory()
		free, _ := r.Host.FreeSpace(t.TempDir())
		for what, got := range map[string]bool{
			"let through":           r.Unreviewed["lima:egress-proxy"],
			"lima's KVM":            l.KVM == "/x/kvm",
			"lima's Ubuntu":         l.UbuntuURL == "file:///x/ubuntu",
			"the registry's CPUs":   r.Host.CPUs() == 1234,
			"the registry's memory": mem == 4321<<30,
			"the registry's free":   free == 0,
			"lima's CPUs":           l.CPUs() == 1234,
			// Entries that aren't ports skipped.
			"lima's ssh ports":  slices.Equal(l.Ports.SSHList, []int{20001, 20002}),
			"lima's port range": l.Ports.Range == platform.PortRange{Lo: 10000, Hi: 10199},
		} {
			if got != c.want {
				t.Errorf("overrides %v: %s overridden = %v", c.want, what, got)
			}
		}
		if !c.want && (l.KVM != "/dev/kvm" || l.UbuntuURL != ubuntu.DefaultURL) {
			t.Errorf("none: KVM %q, Ubuntu %q", l.KVM, l.UbuntuURL)
		}
	}
	// A range that isn't LO-HI of ports is none.
	for _, bad := range []string{"", "10000", "x-10", "10-x", "0-10", "10-70000", "20-10"} {
		if r := (Overrides{PortRange: bad}).Range(); r != (platform.PortRange{}) {
			t.Errorf("range %q: %v", bad, r)
		}
	}
	// The probe of the host's pins: on, or the error it stands in for.
	for v, want := range map[string]string{"on": "", "refused": "refused"} {
		r, err := New(d, assets.FS, &msg.Fake{}, Overrides{Pins: v}, nil)
		if err != nil {
			t.Fatal(err)
		}
		lb, _ := r.Lookup("lima")
		l, _ := backend.As[*lima.Lima](lb)
		if err := l.Pins(); err == nil && want != "" || err != nil && err.Error() != want {
			t.Errorf("DEVVM_PINS=%s: %v", v, err)
		}
	}
}

// A backend whose name is bad or already taken fails the registry, with
// no registry; good ones get the host, the unreviewed emulations and the
// UI's warnings.
func TestBuild(t *testing.T) {
	host := platform.NewHost(platform.Overrides{})
	for name, bs := range map[string][]backend.Backend{
		"an empty name": {backendtest.New("", nil)},
		"a name twice":  {backendtest.New("lima", nil), backendtest.New("lima", nil)},
	} {
		if r, err := build(&msg.Fake{}, host, "", bs...); err == nil || r != nil {
			t.Errorf("%s: %v, %v", name, r, err)
		}
	}
	r, err := build(&msg.Fake{}, host, "fake:ports", backendtest.New("fake", nil))
	if err != nil || r.Host != host || !r.Unreviewed["fake:ports"] || r.Reporter == nil {
		t.Errorf("build: %+v, %v", r, err)
	}
}

// lima is registered, and it's what a new VM gets by default.
func TestLimaIsTheDefault(t *testing.T) {
	t.Parallel()
	d := paths.Dirs{Data: t.TempDir(), Config: t.TempDir(), Lima: t.TempDir()}
	r, err := New(d, assets.FS, &msg.Fake{}, Overrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(r.Names(), " "); got != "lima" {
		t.Fatalf("the registry has %q, want lima alone", got)
	}
	vm, err := state.ReadVM(d, "devvm-p-0123456789abcd")
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.For(vm)
	if err != nil || c.Backend.Name() != backend.Default || backend.Default != "lima" {
		t.Errorf("For a new VM: %+v, %v", c, err)
	}
}

// homeMax is the longest home folder whose data folder (the default,
// ~/.local/share/devvm) keeps every socket devvm makes within the limit:
// a VM's folder holds none, and those in the run folder are named by
// hashes, so no VM's name lengthens them.
const homeMax = 37

// Every socket devvm makes for a VM, the longest VM name's included, fits
// a socket's path under the longest home: devvm's agent, the port
// forwarders' control sockets (ssh adds 17 to those), and a link session's
// sockets and control sockets.
func TestSocketsFit(t *testing.T) {
	home := "/" + strings.Repeat("h", homeMax-1)
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("DEVVM_LIMA_HOME", "")
	d, err := paths.FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(d, assets.FS, &msg.Fake{}, Overrides{}, nil); err != nil {
		t.Fatal(err)
	}
	vm := vmname.VM("devvm-" + strings.Repeat("a", 20) + "-0123456789abcd") // the longest folder part
	target := vmname.VM("devvm-" + strings.Repeat("b", 20) + "-0123456789abce")
	fw := vmssh.NewForwards(d, backend.Review{}, "")
	links := vmssh.Links{VM: vm, Dir: d.Run()}
	ctls := []string{
		fw.Control(vm),
		links.Control("", ""), links.Control(target, "12345 2026-11-03T00:00:00.000000000Z"),
	}
	for _, p := range ctls {
		if len(p) > vmssh.ControlMax {
			t.Errorf("%s: %d characters, over %d", p, len(p), vmssh.ControlMax)
		}
	}
	socks := []string{d.AgentSock(), links.Socket(vmssh.Link{Addr: netip.MustParseAddr("127.77.255.255"), Port: 65535})}
	for _, p := range socks {
		if len(p) >= paths.SocketMax {
			t.Errorf("%s: %d characters, %d at most", p, len(p), paths.SocketMax-1)
		}
	}
	// None of them is in the VM's folder, and none's length is the VM
	// name's: the same for the shortest name.
	for _, p := range slices.Concat(ctls, socks) {
		if strings.HasPrefix(p, d.VMs()) || strings.Contains(p, vm.String()) {
			t.Errorf("%s: in a VM's folder, or named by the VM", p)
		}
	}
	short := vmssh.Links{VM: "devvm-a-0123456789abcd", Dir: d.Run()}
	if len(short.Control("", "")) != len(links.Control("", "")) || len(fw.Control("devvm-a-0123456789abcd")) != len(fw.Control(vm)) {
		t.Error("a control socket's length depends on the VM's name")
	}
	// One more character of home and a control socket is too long.
	if over := len(fw.Control(vm)) + 1; over <= vmssh.ControlMax && len(links.Control(target, "x"))+1 <= vmssh.ControlMax {
		t.Errorf("homeMax %d isn't the longest: raise it", homeMax)
	}
}
