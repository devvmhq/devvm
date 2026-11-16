package lima

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/ports"
	"github.com/devvmhq/devvm/internal/redact"
	"github.com/devvmhq/devvm/internal/shell"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/vmlog"
)

// envOf returns the value the call's environment gives key, and whether
// it's set.
func envOf(c call, key string) (string, bool) {
	for _, e := range slices.Backward(c.env) {
		if v, ok := strings.CutPrefix(e, key+"="); ok {
			return v, true
		}
	}
	return "", false
}

// startCall returns the call that started the VM.
func startCall(t *testing.T, f *fakeRunner) call {
	t.Helper()
	for _, c := range f.calls {
		if slices.Contains(c.argv, "start") {
			return c
		}
	}
	t.Fatalf("no start in %q", f.lines())
	return call{}
}

const stoppedVM = `{"name":"v","status":"Stopped","config":{"ssh":{"forwardAgent":true}}}`

func TestStartConfined(t *testing.T) {
	proxy := backend.Confinement{
		Net: backend.NetProxy, ProxyPort: 23456, ProxyPassword: redact.NewSecret("0123456789abcdef0123456789abcdef"),
		SSHPort: 2222, Env: map[string]string{"NO_PROXY": "localhost"},
	}
	open := proxy
	open.Net = backend.NetDirect
	pinned := proxy
	pinned.Pins = backend.Pins{Root: "/p", Fixed: []string{".git"}, ReadOnly: []string{".git/config", ".git/hooks"}, File: filepath.Join(t.TempDir(), "git-pins.json")}
	for _, tc := range []struct {
		name, os string
		c        backend.Confinement
		edits    []string // the --set values
		wrapped  bool     // under sandbox-exec
		qemu     bool     // through the QEMU wrapper
		guestfwd string   // DEVVM_GUESTFWD, SLOTS the VM's slots; "" for unset
		net      string   // DEVVM_QEMU_NET
		pins     string   // DEVVM_QEMU_PINS
	}{
		{
			name: "macOS, the proxy", os: "darwin", c: proxy, wrapped: true,
			edits: []string{
				".ssh.localPort=2222",
				`.env = {"HTTPS_PROXY":"http://devvm:devvm-proxy-password@192.168.5.2:23456","HTTP_PROXY":"http://devvm:devvm-proxy-password@192.168.5.2:23456","NO_PROXY":"localhost","http_proxy":"http://devvm:devvm-proxy-password@192.168.5.2:23456","https_proxy":"http://devvm:devvm-proxy-password@192.168.5.2:23456"}`,
				".propagateProxyEnv=false", ".hostResolver.enabled=false", `.dns=["127.0.0.1"]`, noForwardsSet(),
			},
		},
		{
			name: "Linux, the proxy", os: "linux", c: proxy, qemu: true,
			guestfwd: `'/opt/my devvm/devvm' _qemu connect 127.0.0.1:23456 SLOTS`,
			edits: []string{
				".ssh.localPort=2222",
				`.env = {"HTTPS_PROXY":"http://devvm:devvm-proxy-password@192.168.5.4:3128","HTTP_PROXY":"http://devvm:devvm-proxy-password@192.168.5.4:3128","NO_PROXY":"localhost","http_proxy":"http://devvm:devvm-proxy-password@192.168.5.4:3128","https_proxy":"http://devvm:devvm-proxy-password@192.168.5.4:3128"}`,
				".propagateProxyEnv=false", ".hostResolver.enabled=false", `.dns=["127.0.0.1"]`, noForwardsSet(),
			},
		},
		{
			name: "macOS, a build with no network", os: "darwin", c: backend.Confinement{Net: backend.NetNone, SSHPort: 2223}, wrapped: true,
			edits: []string{".ssh.localPort=2223", ".propagateProxyEnv=false", noForwardsSet()},
		},
		{
			name: "Linux, a build with no network", os: "linux", c: backend.Confinement{Net: backend.NetNone, SSHPort: 2224}, qemu: true,
			edits: []string{".ssh.localPort=2224", ".propagateProxyEnv=false", noForwardsSet()},
		},
		{
			// Not the host's proxy settings: its image would keep them.
			name: "Linux, a build with the internet", os: "linux", c: backend.Confinement{Net: backend.NetInternet, SSHPort: 2225}, qemu: true, net: netOpen,
			edits: []string{".ssh.localPort=2225", ".propagateProxyEnv=false", noForwardsSet()},
		},
		{
			// Pins, which QEMU's namespace pins: .git first.
			name: "Linux, the proxy and pins", os: "linux", c: pinned, qemu: true,
			guestfwd: `'/opt/my devvm/devvm' _qemu connect 127.0.0.1:23456 SLOTS`,
			pins:     `{"root":"/p","pins":[{"path":".git","read_only":false},{"path":".git/config","read_only":true},{"path":".git/hooks","read_only":true}]}`,
			edits: []string{
				".ssh.localPort=2222",
				`.env = {"HTTPS_PROXY":"http://devvm:devvm-proxy-password@192.168.5.4:3128","HTTP_PROXY":"http://devvm:devvm-proxy-password@192.168.5.4:3128","NO_PROXY":"localhost","http_proxy":"http://devvm:devvm-proxy-password@192.168.5.4:3128","https_proxy":"http://devvm:devvm-proxy-password@192.168.5.4:3128"}`,
				".propagateProxyEnv=false", ".hostResolver.enabled=false", `.dns=["127.0.0.1"]`, noForwardsSet(),
			},
		},
		{name: "macOS, unconfined", os: "darwin", c: backend.Confinement{}, edits: []string{noForwardsSet()}},
		{
			// Open: unconfined, the proxy on Lima's host address, Lima's
			// host name past it, and no host proxy settings.
			name: "macOS, open", os: "darwin", c: open,
			edits: []string{
				".ssh.localPort=2222",
				`.env = {"HTTPS_PROXY":"http://devvm:devvm-proxy-password@192.168.5.2:23456","HTTP_PROXY":"http://devvm:devvm-proxy-password@192.168.5.2:23456","NO_PROXY":"localhost,host.lima.internal,192.168.5.2","http_proxy":"http://devvm:devvm-proxy-password@192.168.5.2:23456","https_proxy":"http://devvm:devvm-proxy-password@192.168.5.2:23456"}`,
				".propagateProxyEnv=false", noForwardsSet(),
			},
		},
		{
			// Open on Linux: the wrapper without restrict=on, so QEMU's
			// user-mode network reaches the proxy on host loopback.
			name: "Linux, open", os: "linux", c: open, qemu: true, net: netOpen,
			edits: []string{
				".ssh.localPort=2222",
				`.env = {"HTTPS_PROXY":"http://devvm:devvm-proxy-password@192.168.5.2:23456","HTTP_PROXY":"http://devvm:devvm-proxy-password@192.168.5.2:23456","NO_PROXY":"localhost,host.lima.internal,192.168.5.2","http_proxy":"http://devvm:devvm-proxy-password@192.168.5.2:23456","https_proxy":"http://devvm:devvm-proxy-password@192.168.5.2:23456"}`,
				".propagateProxyEnv=false", noForwardsSet(),
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, f, _ := testLima(t, tc.os)
			l.Getenv = func(k string) string { return map[string]string{"HTTPS_PROXY": "http://corp.test:8080"}[k] }
			l.Pins = func() error { return nil }
			t.Setenv(GuestFwdEnv, "left over")
			yaml := stoppedWithYAML(t, l, f)
			if err := l.start(t.Context(), "v", nil, tc.c); err != nil {
				t.Fatal(err)
			}
			if edits := editSets(f); !slices.Equal(edits, tc.edits) {
				t.Errorf("edits\n%s\nwant\n%s", strings.Join(edits, "\n"), strings.Join(tc.edits, "\n"))
			}
			// The password is in no command's arguments: devvm writes it into
			// lima.yaml, which the setup's proxy address matches.
			for _, c := range f.calls {
				if strings.Contains(c.line(), proxy.ProxyPassword.Reveal()) {
					t.Errorf("the password is in %q", c.line())
				}
			}
			checkLimaYAML(t, l, yaml, tc.c)
			s := startCall(t, f)
			checkStartCall(t, l, s, tc.wrapped)
			// Only the proxy's VMs have their binds denied: builds keep
			// Lima's host resolver, which binds loopback ports.
			if tc.wrapped {
				sb, err := os.ReadFile(l.Dirs.VM("v").Profile())
				if err != nil || strings.Contains(string(sb), "(deny network-bind)") != (tc.c.Net == backend.NetProxy) {
					t.Errorf("profile (%v):\n%s", err, sb)
				}
				if fi, err := os.Stat(l.Dirs.VM("v").Profile()); err != nil || fi.Mode().Perm() != 0o600 {
					t.Errorf("profile: %v, %v", fi.Mode(), err)
				}
			}
			w, ok := envOf(s, "QEMU_SYSTEM_AARCH64")
			if ok != tc.qemu || ok && w != filepath.Join(l.Dirs.Run(), "qemu", "qemu-system-aarch64") {
				t.Errorf("QEMU_SYSTEM_AARCH64=%q (%v)", w, ok)
			}
			wantFwd := strings.ReplaceAll(tc.guestfwd, "SLOTS", shell.Quote(l.Dirs.VM("v").Proxy(".slots")))
			if fwd, _ := envOf(s, GuestFwdEnv); tc.qemu && fwd != wantFwd {
				t.Errorf("%s=%q, want %q", GuestFwdEnv, fwd, wantFwd)
			}
			if n, _ := envOf(s, NetEnv); tc.qemu && n != tc.net {
				t.Errorf("%s=%q, want %q", NetEnv, n, tc.net)
			}
			// The pins go in their file, which the variable names.
			if p, _ := envOf(s, PinsEnv); tc.qemu && (p == "") != (tc.pins == "") {
				t.Errorf("%s=%q, want %q", PinsEnv, p, tc.pins)
			} else if b, _ := os.ReadFile(p); tc.qemu && p != "" && (p != tc.c.Pins.File || string(b) != tc.pins) { //nolint:gosec // the test's own
				t.Errorf("%s=%q holds %s, want %s", PinsEnv, p, b, tc.pins)
			}
			// Lima propagates the host's proxy settings only to a VM on its
			// own network without devvm's proxy.
			if _, ok := envOf(s, "HTTPS_PROXY"); ok != (tc.c.Net == backend.NetDirect && !tc.c.GuestProxy()) {
				t.Errorf("limactl start has the host's HTTPS_PROXY: %v", ok)
			}
			if r, err := l.Running(t.Context(), "v"); err != nil {
				t.Fatal(err)
			} else if r.Confined {
				t.Error("Confined with a hostagent that doesn't run")
			}
		})
	}
}

// A build VM starts with an ssh port the ports allocator chooses, on
// every host: never a VM's, and held for it while it starts, then given
// up.
func TestStartBuildSSHPort(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) { testStartBuildSSHPort(t, goos) })
	}
}

func testStartBuildSSHPort(t *testing.T, goos string) {
	l, f, _ := testLima(t, goos)
	stoppedWithYAML(t, l, f)
	st, err := state.New(l.Dirs, "devvm-web-0123456789abcd")
	if err != nil {
		t.Fatal(err)
	}
	st.Image, st.Dir, st.Backend = "base", "/w", Name
	if err := st.Create(t.Context(), func(st *state.VM) error { return ports.Set(st, ports.Ports{Host: ports.Host{SSH: 20000}}) }); err != nil {
		t.Fatal(err)
	}
	// The ports VMs' ssh comes from: the VM's, which a build takes only
	// when no other is free, and one more.
	listening := map[int]bool{}
	l.Ports = ports.Allocator{Dirs: l.Dirs, SSHList: []int{20000, 20001}, Listening: func(_ context.Context, port int) bool { return listening[port] }}
	rec := filepath.Join(l.Dirs.BuildPorts(), "v")
	held, edit := "", f.do
	f.do = func(c call) {
		edit(c)
		if slices.Contains(c.argv, "start") {
			b, _ := os.ReadFile(rec) //nolint:gosec // the test's own file
			held = string(b)
		}
	}
	if err := l.start(t.Context(), "v", nil, backend.Confinement{Net: backend.NetInternet}); err != nil {
		t.Fatal(err)
	}
	if edits := editSets(f); !slices.Contains(edits, ".ssh.localPort=20001") {
		t.Errorf("edits %q, want the port no VM has", edits)
	}
	if held != "20001\n" {
		t.Errorf("held %q while it started", held)
	}
	if _, err := os.Stat(rec); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the port's still held: %v", err)
	}
	// No port free: no start.
	l.Ports.SSHList = []int{20000}
	listening[20000] = true // the VM runs
	f.calls = nil
	if err := l.start(t.Context(), "v", nil, backend.Confinement{Net: backend.NetInternet}); err == nil || slices.Contains(f.lines(), "limactl start --tty=false v") {
		t.Errorf("started without a port: %v, %q", err, f.lines())
	}
	// A clone keeps its image's port: never chosen again, so the start
	// always edits a new one in (each time, of the two ports left, the one
	// the config doesn't have).
	f.out["limactl list --json"] = `{"name":"v","status":"Stopped","config":{"ssh":{"forwardAgent":true,"localPort":20001}}}`
	l.Ports.SSHList = []int{20000, 20001, 20002}
	for range 10 {
		f.calls = nil
		if err := l.start(t.Context(), "v", nil, backend.Confinement{Net: backend.NetInternet}); err != nil {
			t.Fatal(err)
		}
		if edits := editSets(f); !slices.Contains(edits, ".ssh.localPort=20002") {
			t.Fatalf("edits %q, want the port neither a VM nor the clone has", edits)
		}
	}
}

// A start turns Lima's own forwards off where the VM's rules, as limactl
// list fills them in, forward anything, whatever ports it's given: those
// are the forwarder's.
func TestStartSetsNoForwards(t *testing.T) {
	// As limactl list --json has noForwards: the host side filled in.
	const ignore = `{"guestIPMustBeZero":false,"guestIP":"127.0.0.1","guestPortRange":[1,65535],"hostIP":"127.0.0.1","hostPortRange":[1,65535],"proto":"any","ignore":true},
		{"guestIPMustBeZero":true,"guestIP":"0.0.0.0","guestPortRange":[1,65535],"hostIP":"127.0.0.1","hostPortRange":[1,65535],"proto":"any","ignore":true},
		{"guestIPMustBeZero":false,"guestIP":"::","guestPortRange":[1,65535],"hostIP":"127.0.0.1","hostPortRange":[1,65535],"proto":"any","ignore":true}`
	const listed = `{"guestIPMustBeZero":false,"guestIP":"127.0.0.1","guestPortRange":[5173,5173],"hostIP":"127.0.0.1","hostPortRange":[5173,5173],"proto":"any"}`
	l, _, _ := testLima(t, "darwin")
	set := []string{"--set", noForwardsSet()}
	for _, tc := range []struct {
		name, cfg string
		want      []string
	}{
		{"off", `{"portForwards":[` + ignore + `]}`, nil},
		{"Lima's own rule", `{"portForwards":[]}`, set},
		{"a listed port of Lima's", `{"portForwards":[` + listed + `,` + ignore + `]}`, set},
		{"another address", `{"portForwards":[` + strings.Replace(ignore, `"::"`, `"::1"`, 1) + `]}`, set},
		{"TCP alone", `{"portForwards":[` + strings.ReplaceAll(ignore, `"any"`, `"tcp"`) + `]}`, set},
	} {
		var cfg config
		if err := json.Unmarshal([]byte(tc.cfg), &cfg); err != nil {
			t.Fatal(err)
		}
		if got, _ := l.startSets(&cfg, backend.Confinement{}, 0); !slices.Equal(got, tc.want) {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A start with network policy edits the VM's environment only when it
// differs from the one the VM has: the same NO_PROXY changes nothing, so
// its ssh host keys stay, and another edits it.
func TestStartSetsChangedEnv(t *testing.T) {
	l, _, _ := testLima(t, "darwin")
	conf := func(noProxy string) backend.Confinement {
		return backend.Confinement{
			Net: backend.NetProxy, ProxyPort: 20001, ProxyPassword: redact.NewSecret("pw"),
			Env: map[string]string{"NO_PROXY": noProxy, "no_proxy": noProxy},
		}
	}
	const was = "localhost,127.0.0.1,::1"
	f, enabled := false, false
	cfg := config{Env: l.guestEnv(conf(was)), DNS: []string{"127.0.0.1"}, PropagateProxyEnv: &f}
	cfg.HostResolver.Enabled = &enabled
	if err := json.Unmarshal([]byte(`[{"guestIPMustBeZero":false,"guestIP":"127.0.0.1","guestPortRange":[1,65535],"hostIP":"127.0.0.1","hostPortRange":[1,65535],"proto":"any","ignore":true},
		{"guestIPMustBeZero":true,"guestIP":"0.0.0.0","guestPortRange":[1,65535],"hostIP":"127.0.0.1","hostPortRange":[1,65535],"proto":"any","ignore":true},
		{"guestIPMustBeZero":false,"guestIP":"::","guestPortRange":[1,65535],"hostIP":"127.0.0.1","hostPortRange":[1,65535],"proto":"any","ignore":true}]`), &cfg.PortForwards); err != nil {
		t.Fatal(err)
	}
	if got, _ := l.startSets(&cfg, conf(was), 0); len(got) != 0 {
		t.Errorf("the same NO_PROXY: %q", got)
	}
	got, fill := l.startSets(&cfg, conf(was+",.corp"), 0)
	if len(got) != 2 || !strings.HasPrefix(got[1], ".env = ") || !strings.Contains(got[1], "::1,.corp") || !fill {
		t.Errorf("another NO_PROXY: %q, fill %v", got, fill)
	}
}

// stoppedWithYAML makes v a stopped VM with a stale hostagent pid and a
// lima.yaml that limactl edit writes the environment it's given into,
// and returns lima.yaml's path.
func stoppedWithYAML(t *testing.T, l *Lima, f *fakeRunner) string {
	t.Helper()
	f.out["limactl list --json"] = stoppedVM
	if err := os.MkdirAll(l.Dirs.LimaVM("v"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.haPIDFile("v"), []byte("12345\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return editWritesYAML(t, l, f)
}

// editWritesYAML gives v a lima.yaml that limactl edit writes the
// environment it's given into, as fillPassword then finds it, and returns
// its path.
func editWritesYAML(t *testing.T, l *Lima, f *fakeRunner) string {
	t.Helper()
	yaml := filepath.Join(l.Dirs.LimaVM("v"), "lima.yaml")
	if err := os.WriteFile(yaml, []byte("cpus: 4\n"), 0o644); err != nil { //nolint:gosec // as Lima writes it
		t.Fatal(err)
	}
	f.do = func(c call) {
		if i := slices.IndexFunc(c.argv, func(a string) bool { return strings.HasPrefix(a, ".env = ") }); i >= 0 && c.argv[1] == "edit" {
			if err := os.WriteFile(yaml, []byte("cpus: 4\nenv: "+strings.TrimPrefix(c.argv[i], ".env = ")+"\n"), 0o644); err != nil { //nolint:gosec // as Lima writes it
				t.Error(err)
			}
		}
	}
	return yaml
}

// editSets returns the --set values of every limactl edit, in order.
func editSets(f *fakeRunner) []string {
	var edits []string
	for _, c := range f.calls {
		if c.argv[1] != "edit" {
			continue
		}
		for i, a := range c.argv {
			if a == "--set" {
				edits = append(edits, c.argv[i+1])
			}
		}
	}
	return edits
}

// checkLimaYAML checks that a start with c left lima.yaml with the proxy's
// address and password (owner-only), or untouched without the proxy.
func checkLimaYAML(t *testing.T, l *Lima, yaml string, c backend.Confinement) {
	t.Helper()
	data, err := os.ReadFile(yaml) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	u := l.ProxyURL(c)
	if !c.GuestProxy() {
		if u != "" || string(data) != "cpus: 4\n" {
			t.Errorf("ProxyURL = %q, lima.yaml:\n%s", u, data)
		}
		return
	}
	fi, err := os.Stat(yaml)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"HTTP_PROXY":"`+u+`"`) || strings.Contains(string(data), passwordStandIn) || fi.Mode().Perm() != 0o600 {
		t.Errorf("lima.yaml (%v):\n%s", fi.Mode(), data)
	}
}

// checkStartCall checks limactl start's arguments, folder and, when
// wrapped, its sandbox-exec profile.
func checkStartCall(t *testing.T, l *Lima, s call, wrapped bool) {
	t.Helper()
	args := s.argv[slices.Index(s.argv, "start"):]
	if !slices.Equal(args, []string{"start", "--tty=false", "v"}) {
		t.Errorf("start's arguments: %q", args)
	}
	if s.dir != l.Dirs.Data {
		t.Errorf("ran in %s", s.dir)
	}
	if (s.argv[0] == "sandbox-exec") != wrapped {
		t.Errorf("under sandbox-exec: %q", s.argv)
		return
	}
	if wrapped {
		lc := s.argv[3]
		if !slices.Equal(s.argv[1:3], []string{"-f", l.Dirs.VM("v").Profile()}) || !filepath.IsAbs(lc) || filepath.Base(lc) != "limactl" {
			t.Errorf("sandbox-exec's arguments: %q", s.argv)
		}
	}
}

// fillPassword fails, changing nothing, when lima.yaml lacks the stand-in or
// the URL isn't plain YAML.
func TestFillPassword(t *testing.T) {
	l, _, _ := testLima(t, "darwin")
	yaml := filepath.Join(l.Dirs.LimaVM("v"), "lima.yaml")
	if err := os.MkdirAll(filepath.Dir(yaml), 0o700); err != nil {
		t.Fatal(err)
	}
	c := backend.Confinement{Net: backend.NetProxy, ProxyPort: 1, ProxyPassword: redact.NewSecret("pw")}
	for _, tc := range []struct{ name, yaml, password string }{
		{"no stand-in", "env: {}\n", "pw"},
		{"an anchor", "env: {HTTP_PROXY: http://devvm:devvm-proxy-password@192.168.5.2:1}\n", "p&w"},
	} {
		if err := os.WriteFile(yaml, []byte(tc.yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		c.ProxyPassword = redact.NewSecret(tc.password)
		if err := l.fillPassword("v", c); err == nil {
			t.Errorf("%s: filled", tc.name)
		}
		if b, _ := os.ReadFile(yaml); string(b) != tc.yaml { //nolint:gosec // the test's own file
			t.Errorf("%s: changed to %q", tc.name, b)
		}
	}
}

// A VM whose config already matches gets no edit: an edit means new ssh host
// keys at its next boot.
func TestStartEditsOnlyWhatDiffers(t *testing.T) {
	l, f, _ := testLima(t, "linux")
	c := backend.Confinement{Net: backend.NetProxy, ProxyPort: 23456, ProxyPassword: redact.NewSecret("pw"), SSHPort: 2222}
	u := l.proxyURL(c)
	f.out["limactl list --json"] = `{"name":"v","status":"Stopped","config":{"ssh":{"localPort":2222},"propagateProxyEnv":false,"portForwards":` + yqJSON(noForwards()) + `,` +
		`"hostResolver":{"enabled":false},"dns":["127.0.0.1"],"env":{"HTTPS_PROXY":"` + u + `","HTTP_PROXY":"` + u + `",` +
		`"http_proxy":"` + u + `","https_proxy":"` + u + `"}}}`
	if err := os.MkdirAll(l.Dirs.LimaVM("v"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.haPIDFile("v"), []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := l.start(t.Context(), "v", nil, c); err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(f.lines(), func(l string) bool { return strings.HasPrefix(l, "limactl edit") }) {
		t.Errorf("edited: %q", f.lines())
	}
	// Started without the proxy, the proxy's settings go.
	f.calls = nil
	if err := l.start(t.Context(), "v", nil, backend.Confinement{SSHPort: 2222}); err != nil {
		t.Fatal(err)
	}
	want := "limactl edit --tty=false v --set del(.env) --set .propagateProxyEnv=true --set del(.hostResolver) --set del(.dns)"
	if got := f.lines(); len(got) == 0 || got[0] != want {
		t.Errorf("ran %q, want %q first", got, want)
	}
}

// What a start's Want changes goes in the same edit as its
// confinement's: one edit, one set of new host keys.
func TestStartWantInOneEdit(t *testing.T) {
	l, f, _ := testLima(t, "linux")
	c := backend.Confinement{Net: backend.NetProxy, ProxyPort: 23456, ProxyPassword: redact.NewSecret("pw"), SSHPort: 2222}
	u := l.proxyURL(c)
	matching := `"ssh":{"localPort":2222,"forwardAgent":false},"propagateProxyEnv":false,"portForwards":` + yqJSON(noForwards()) + `,` +
		`"hostResolver":{"enabled":false},"dns":["127.0.0.1"],"env":{"HTTPS_PROXY":"` + u + `","HTTP_PROXY":"` + u + `",` +
		`"http_proxy":"` + u + `","https_proxy":"` + u + `"}`
	f.out["limactl list --json"] = `{"name":"v","status":"Stopped","cpus":4,"memory":8589934592,"disk":107374182400,"config":{` + matching + `}}`
	if err := os.MkdirAll(l.Dirs.LimaVM("v"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.haPIDFile("v"), []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := backend.Want{Size: backend.Size{CPUs: 6, MemoryGiB: 8}, AgentForwarding: true}
	if err := l.Start(t.Context(), "v", w, c); err != nil {
		t.Fatal(err)
	}
	var edits []string
	for _, line := range f.lines() {
		if strings.HasPrefix(line, "limactl edit") {
			edits = append(edits, line)
		}
	}
	if want := []string{"limactl edit --tty=false v --cpus 6 --set .ssh.forwardAgent=true"}; !slices.Equal(edits, want) {
		t.Errorf("edits %q, want %q", edits, want)
	}
	// Without network policy, both in one edit too.
	f.calls = nil
	w.Size = backend.Size{DiskGiB: 120}
	if err := l.Start(t.Context(), "v", w, backend.Confinement{SSHPort: 2222}); err != nil {
		t.Fatal(err)
	}
	want := "limactl edit --tty=false v --disk 120 --set .ssh.forwardAgent=true --set del(.env) --set .propagateProxyEnv=true --set del(.hostResolver) --set del(.dns)"
	if got := f.lines(); len(got) == 0 || got[0] != want || strings.Count(strings.Join(got, "\n"), "limactl edit") != 1 {
		t.Errorf("ran %q, want %q first, alone", got, want)
	}
	// A smaller disk: refused before anything runs.
	f.calls = nil
	w.Size = backend.Size{DiskGiB: 50}
	if err := l.Start(t.Context(), "v", w, c); msg.NamesOf(err) != "disk_shrink have=100 vm=v want=50" || len(f.lines()) > 0 {
		t.Errorf("a smaller disk: %v, ran %q", err, f.lines())
	}
}

// A cache mode chosen for a QEMU VM's project folder is set by one edit
// at its next start, which then mounts the folder again, and the start
// after that, which finds it set, edits nothing.
func TestStartSetsProjectCacheOnce(t *testing.T) {
	l, f, warned := testLima(t, "linux")
	c := backend.Confinement{SSHPort: 2222}
	vm := func(cache string) string {
		return `{"name":"v","status":"Stopped","vmType":"qemu","cpus":4,"memory":8589934592,"disk":107374182400,"config":{` +
			`"ssh":{"localPort":2222,"forwardAgent":false},"portForwards":` + yqJSON(noForwards()) + `,"mounts":[` +
			`{"location":"/p","mountPoint":"/p","writable":true,"9p":{"msize":"128KiB","cache":"` + cache + `"}}]}}`
	}
	// The folder as the guest has it after the remount: mounted with none.
	check := checkOfP
	remount := `limactl shell --workdir / v sudo sh -c umount '/p' || exit 3; mount '/p' || { mount '/p'; mountpoint -q '/p' || exit 4; }`
	f.stdout = map[string]string{check: mountinfoOfP("rw,access=client,msize=131072,trans=virtio") + "\n"}
	f.out["limactl list --json"] = vm("mmap")
	if err := os.MkdirAll(l.Dirs.LimaVM("v"), 0o700); err != nil {
		t.Fatal(err)
	}
	w := backend.Want{Project: "/p", ProjectCache: backend.CacheNone}
	if err := l.Start(t.Context(), "v", w, c); err != nil {
		t.Fatal(err)
	}
	edits := func() []string {
		var out []string
		for _, line := range f.lines() {
			if strings.HasPrefix(line, "limactl edit") {
				out = append(out, line)
			}
		}
		return out
	}
	want := []string{`limactl edit --tty=false v --set .mounts = [{"location":"/p","writable":true,"9p":{"cache":"none"}}]`}
	if got := edits(); !slices.Equal(got, want) {
		t.Errorf("edits %q, want %q", got, want)
	}
	// Once it runs, the folder is mounted again, as the fstab its boot
	// wrote has it, and checked: mounted with none, nothing to say.
	lines := f.lines()
	if i, j, k := slices.Index(lines, "limactl start --tty=false v"), slices.Index(lines, remount), slices.Index(lines, check); i < 0 || j < i || k < j {
		t.Errorf("not mounted again and checked after the start: %q", lines)
	}
	if got := warned.Names(); len(got) > 0 {
		t.Errorf("warned %q", got)
	}
	f.calls = nil
	f.out["limactl list --json"] = vm("none")
	if err := l.Start(t.Context(), "v", w, c); err != nil {
		t.Fatal(err)
	}
	if got := edits(); len(got) > 0 || slices.Contains(f.lines(), remount) {
		t.Errorf("set already, edited or mounted again: %q", f.lines())
	}
	// Anything but the folder mounted with none stops the VM, and the start
	// fails with a RemountError, for its caller's one start again; the run
	// log says how the remount left it.
	for _, tc := range []struct {
		name  string
		check string // what the check prints; "" for a check that fails
		fail  string // a command that fails
		err   string // the start's error, a RemountError's
		why   string // the run log's line ends so
	}{
		// The unmount refused (the share busy): the old mount stays.
		{"kept the old mount", mountinfoOfP("rw,cache=0x5,access=client,trans=virtio"), remount, "mount_cache_remount_failed has=mmap vm=v want=none", "it's mounted with mmap"},
		// A degraded start's old fstab: mounted again, with the old mode.
		{"the old mode again", mountinfoOfP("rw,cache=0x5,trans=virtio"), "", "mount_cache_remount_failed has=mmap vm=v want=none", "it's mounted with mmap"},
		// Stacked mounts: the last line, the mount visible there, counts.
		{"stacked", mountinfoOfP("rw,cache=0x5,trans=virtio") + "\n" + mountinfoOfP("rw,trans=virtio") + "\n", "", "", ""},
		// A mode devvm doesn't know is named as such, never as the guest's
		// text.
		{"another mode", mountinfoOfP("rw,cache=0xf,trans=virtio\x1b[2J"), "", "mount_cache_remount_failed has=unknown vm=v want=none", "it's mounted with unknown"},
		// Unmounted (an Alpine guest's mount refusing a share it just
		// unmounted), or not known to be mounted.
		{"not mounted", "devvm-not-mounted\n", remount, "mount_cache_unmounted vm=v", "it isn't mounted"},
		{"mounted, then not", mountinfoOfP("rw,trans=virtio") + "\ndevvm-not-mounted", "", "mount_cache_unmounted vm=v", "it isn't mounted"},
		{"the check fails", "", check, "mount_cache_unchecked vm=v", "the check failed: failed"},
		{"no answer", "\n", "", "mount_cache_unchecked vm=v", "the check failed: the guest gave no answer"},
		// An answer that isn't a line of mountinfo can't be read.
		{"not mountinfo", "rw,trans=virtio\n", "", "mount_cache_unchecked vm=v", "the check failed: the guest's answer isn't a line of mountinfo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			*warned = msg.Recorder{}
			log := &strings.Builder{}
			l.Reporter.(*msg.Fake).Log = log
			f.calls, f.fail = nil, nil
			if tc.fail != "" {
				f.fail = []string{tc.fail}
			}
			f.stdout[check] = tc.check
			f.out["limactl list --json"] = vm("mmap")
			err := l.Start(t.Context(), "v", w, c)
			if got := msg.NamesOf(err); got != tc.err {
				t.Errorf("start: %q, want %q", got, tc.err)
			}
			if _, ok := errors.AsType[*backend.RemountError](err); ok != (tc.err != "") {
				t.Errorf("a RemountError %v, want %v", ok, tc.err != "")
			}
			if stopped := slices.Contains(f.lines(), "limactl stop v"); stopped != (tc.err != "") {
				t.Errorf("stopped %v, want %v: %q", stopped, tc.err != "", f.lines())
			}
			if want := "devvm: v's project folder after its remount: " + tc.why + "; restarting the VM\n"; tc.why != "" && log.String() != want {
				t.Errorf("logged %q, want %q", log.String(), want)
			}
			if got := warned.Names(); len(got) > 0 {
				t.Errorf("warned %q", got)
			}
		})
	}
}

// The restart's stop runs with the interrupt's cleanup still in place,
// which goes once it returns. Where the stop fails, a folder mounted with
// the old mode is warned of and the VM goes on with no RemountError; one
// that isn't mounted fails the start as a logged stop_failed, the VM
// maybe still running.
func TestRemountRestartStop(t *testing.T) {
	w := backend.Want{Project: "/p", ProjectCache: backend.CacheNone}
	for _, tc := range []struct {
		name  string
		check string
		fails bool // the stop fails
		err   string
		warn  string
	}{
		{"stopped", "devvm-not-mounted\n", false, "mount_cache_unmounted vm=v", ""},
		{"old mode, the stop fails", mountinfoOfP("rw,cache=0x5,trans=virtio"), true, "", "mount_cache_remount_failed has=mmap vm=v want=none"},
		{"not mounted, the stop fails", "devvm-not-mounted\n", true, "stop_failed vm=v", ""},
		{"no answer, the stop fails", "\n", true, "stop_failed vm=v", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, f, _ := testLima(t, "linux")
			warned := &msg.Recorder{}
			live := 0
			l.Reporter = &msg.Fake{OnWarn: warned.Add, OnCleanup: func(func(context.Context)) func() {
				live++
				return func() { live-- }
			}}
			f.stdout = map[string]string{checkOfP: tc.check}
			held := -1
			f.do = func(c call) {
				if c.line() == "limactl stop v" {
					held = live
				}
			}
			if tc.fails {
				f.fail = []string{"limactl stop v"}
			}
			err := remountProject(t.Context(), &l.machines, "v", w, true)
			if got := msg.NamesOf(err); got != tc.err {
				t.Errorf("start: %q, want %q", got, tc.err)
			}
			_, restart := errors.AsType[*backend.RemountError](err)
			_, logged := errors.AsType[*backend.LoggedError](err)
			if restart != (!tc.fails) || logged != (tc.err == "stop_failed vm=v") {
				t.Errorf("a RemountError %v, logged %v: %v", restart, logged, err)
			}
			if held != 1 || live != 0 {
				t.Errorf("cleanups during the stop %d, after %d; want 1, then 0", held, live)
			}
			var warns []string
			if tc.warn != "" {
				warns = []string{tc.warn}
			}
			if got := warned.Names(); !slices.Equal(got, warns) {
				t.Errorf("warned %q, want %q", got, warns)
			}
		})
	}
}

// The start again after a RemountError checks the folder, its config's
// mode set already, without mounting it again: mounted with none, the
// VM goes on; not mounted, or a check that fails, stops it and fails the
// start; another mode is warned of. None of them restarts it again, and
// neither does a remount that fails on that start.
func TestStartRechecksProjectCache(t *testing.T) {
	l, f, warned := testLima(t, "linux")
	c := backend.Confinement{SSHPort: 2222}
	vm := func(cache string) string {
		return `{"name":"v","status":"Stopped","vmType":"qemu","cpus":4,"memory":8589934592,"disk":107374182400,"config":{` +
			`"ssh":{"localPort":2222,"forwardAgent":false},"portForwards":` + yqJSON(noForwards()) + `,"mounts":[` +
			`{"location":"/p","mountPoint":"/p","writable":true,"9p":{"msize":"128KiB","cache":"` + cache + `"}}]}}`
	}
	if err := os.MkdirAll(l.Dirs.LimaVM("v"), 0o700); err != nil {
		t.Fatal(err)
	}
	remount := `limactl shell --workdir / v sudo sh -c umount`
	w := backend.Want{Project: "/p", ProjectCache: backend.CacheNone, CheckCache: true}
	for _, tc := range []struct {
		name   string
		config string // the cache mode lima.yaml has
		check  string // what the check prints; "" for a check that fails
		err    string
		warn   string
	}{
		{"mounted with none", backend.CacheNone, mountinfoOfP("rw,trans=virtio"), "", ""},
		{"not mounted", backend.CacheNone, "devvm-not-mounted\n", "mount_cache_unmounted vm=v", ""},
		{"the check fails", backend.CacheNone, "", "mount_cache_unchecked vm=v", ""},
		{"the old mode", backend.CacheNone, mountinfoOfP("rw,cache=0x5,trans=virtio"), "", "mount_cache_remount_failed has=mmap vm=v want=none"},
		// Its config lost the mode: it's edited and mounted again, and
		// what fails stops it, with no second restart.
		{"mounted again, not mounted", backend.CacheMmap, "devvm-not-mounted\n", "mount_cache_unmounted vm=v", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			*warned = msg.Recorder{}
			f.calls, f.fail = nil, nil
			f.stdout = map[string]string{checkOfP: tc.check}
			if tc.check == "" {
				f.fail = []string{checkOfP}
			}
			f.out["limactl list --json"] = vm(tc.config)
			err := l.Start(t.Context(), "v", w, c)
			if got := msg.NamesOf(err); got != tc.err {
				t.Errorf("start: %q, want %q", got, tc.err)
			}
			if _, ok := errors.AsType[*backend.RemountError](err); ok {
				t.Errorf("restarts again: %v", err)
			}
			lines := f.lines()
			if !slices.Contains(lines, checkOfP) {
				t.Errorf("not checked: %q", lines)
			}
			if mounted := slices.ContainsFunc(lines, func(s string) bool { return strings.HasPrefix(s, remount) }); mounted != (tc.config != backend.CacheNone) {
				t.Errorf("mounted again %v: %q", mounted, lines)
			}
			stops, want := 0, 0
			if tc.err != "" {
				want = 1
			}
			for _, s := range lines {
				if s == "limactl stop v" {
					stops++
				}
			}
			if stops != want {
				t.Errorf("stopped %d times, want %d: %q", stops, want, lines)
			}
			var warns []string
			if tc.warn != "" {
				warns = []string{tc.warn}
			}
			if got := warned.Names(); !slices.Equal(got, warns) {
				t.Errorf("warned %q, want %q", got, warns)
			}
		})
	}
	// A VM whose folder has no cache mode to check (not 9p) isn't checked.
	f.calls = nil
	f.out["limactl list --json"] = `{"name":"v","status":"Stopped","vmType":"vz","config":{"ssh":{"localPort":2222},"portForwards":` + yqJSON(noForwards()) + `}}`
	_ = l.Start(t.Context(), "v", w, c)
	if slices.Contains(f.lines(), checkOfP) {
		t.Errorf("checked a folder with no cache mode: %q", f.lines())
	}
}

// An interrupt during the remount leaves a cleanup that stops the VM, on
// any tier; without one, the cleanup goes once the folder's checked. An
// interrupted remount whose folder isn't mounted gets no restart: the
// start fails, and the cleanup alone stops the VM.
func TestRemountInterrupted(t *testing.T) {
	w := backend.Want{Project: "/p", ProjectCache: backend.CacheNone}
	for _, interrupted := range []bool{false, true} {
		l, f, _ := testLima(t, "linux")
		var cleanups []func(context.Context)
		removed := 0
		fk := &msg.Fake{Interrupt: interrupted, OnCleanup: func(c func(context.Context)) func() {
			cleanups = append(cleanups, c)
			return func() { removed++ }
		}}
		l.Reporter = fk
		f.stdout = map[string]string{checkOfP: mountinfoOfP("rw,trans=virtio") + "\n"}
		if err := remountProject(t.Context(), &l.machines, "v", w, true); err != nil {
			t.Fatal(err)
		}
		if len(cleanups) != 1 || (removed == 1) == interrupted {
			t.Fatalf("interrupted %v: %d cleanups, %d removed", interrupted, len(cleanups), removed)
		}
		if interrupted {
			f.calls = nil
			cleanups[0](t.Context())
			if !slices.Contains(f.lines(), "limactl stop v") {
				t.Errorf("the cleanup didn't stop the VM: %q", f.lines())
			}
		}
	}
	l, f, _ := testLima(t, "linux")
	l.Reporter = &msg.Fake{Interrupt: true, OnCleanup: func(func(context.Context)) func() { return func() {} }}
	f.stdout = map[string]string{checkOfP: "devvm-not-mounted\n"}
	err := remountProject(t.Context(), &l.machines, "v", w, true)
	if _, ok := errors.AsType[*backend.RemountError](err); ok || msg.NamesOf(err) != "mount_cache_unmounted vm=v" {
		t.Errorf("interrupted, not mounted: %v", err)
	}
	if slices.Contains(f.lines(), "limactl stop v") {
		t.Errorf("stopped before the cleanup: %q", f.lines())
	}
}

// The remount's commands end with their timeout though a child they left
// holds their output open, and only so much of what they print is kept.
func TestRemountBounded(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	cmd := bounded(exec.CommandContext(ctx, "sh", "-c", "sleep 30 & echo started; sleep 30"))
	out := &capped{max: 4}
	cmd.Stdout = out
	start := time.Now()
	_ = logRunner{}.Attach(cmd)
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("it waited %s for the child", d)
	}
	if out.String() != "star" {
		t.Errorf("kept %q", out.String())
	}
}

// A running VM is left as it is, its Want and confinement with it.
func TestStartLeavesARunningVM(t *testing.T) {
	l, f, _ := testLima(t, "darwin")
	f.out["limactl list --json"] = `{"name":"v","status":"Running"}`
	if err := l.Start(t.Context(), "v", backend.Want{Size: backend.Size{CPUs: 6}, AgentDirs: []string{"/h/.claude"}}, backend.Confinement{Net: backend.NetProxy}); err != nil {
		t.Fatal(err)
	}
	if got := f.lines(); len(got) > 0 {
		t.Errorf("ran %q", got)
	}
}

// The profile lets the hostagent reach unix sockets only in the VM's Lima
// folder and the ssh agent, with links resolved, and loopback only at the
// VM's ssh port and its proxy.
func TestProfile(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"real/agent", "real/lima/vm1"} {
		if err := os.MkdirAll(filepath.Join(tmp, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(tmp, "real"), filepath.Join(tmp, "link")); err != nil {
		t.Fatal(err)
	}
	p := profile{LimaDir: filepath.Join(tmp, "link", "lima", "vm1"), AgentSocket: filepath.Join(tmp, "link", "agent", "s"), SSHPort: 2222, ProxyPort: 3128}
	got := strings.Split(p.render(), "\n")
	for _, line := range []string{
		"(deny network-outbound)",
		`(allow network-outbound (remote unix-socket (subpath "` + tmp + `/real/lima/vm1")))`,
		`(allow network-outbound (remote unix-socket (path-literal "` + tmp + `/real/agent/s")))`,
		`(allow network-outbound (remote ip "localhost:2222"))`,
		`(allow network-outbound (remote ip "localhost:3128"))`,
		`(deny mach-lookup (global-name "com.apple.dnssd.service"))`,
	} {
		if !slices.Contains(got, line) {
			t.Errorf("no %s in\n%s", line, p.render())
		}
	}
	if slices.Contains(got, "(allow network-outbound (remote unix-socket))") {
		t.Error("every unix socket allowed")
	}
	if strings.Contains(p.render(), "network-bind") {
		t.Error("binds denied without NoBinds")
	}
	p.AgentSocket, p.ProxyPort = "", 0
	if n := strings.Count(p.render(), "unix-socket"); n != 1 {
		t.Errorf("%d unix-socket rules without an agent", n)
	}
	if strings.Contains(p.render(), "localhost:3128") {
		t.Error("a proxy port without a proxy")
	}
	// NoBinds: no bind but the ssh port's and the VM's own unix sockets',
	// after every other rule, so none allows more.
	p.NoBinds = true
	wantBinds := "(deny network-bind)\n(allow network-bind (local tcp \"localhost:2222\"))\n" +
		"(allow network-bind (local unix-socket (subpath \"" + tmp + "/real/lima/vm1\")))\n"
	if got := p.render(); !strings.HasSuffix(got, wantBinds) || strings.Count(got, "network-bind") != 3 {
		t.Errorf("NoBinds profile:\n%s\nwant it to end\n%s", got, wantBinds)
	}
	// A build with the internet: everything but host loopback.
	p = profile{Internet: true, LimaDir: "/l/vm", SSHPort: 2224}
	want := "(version 1)\n(allow default)\n(deny network-outbound (remote ip \"localhost:*\"))\n(allow network-outbound (remote ip \"localhost:2224\"))\n"
	if got := p.render(); got != want {
		t.Errorf("internet profile:\n%s\nwant\n%s", got, want)
	}
}

// A VM using devvm's own agent may reach that agent, not the user's; one
// using the user's, that one; one without forwarding, or given none,
// neither: devvm's own SSH_AUTH_SOCK never counts.
func TestProfileAgent(t *testing.T) {
	for _, tc := range []struct {
		forwarding bool
		socket     string
		want       string
	}{
		{true, "/d/agent.sock", "/d/agent.sock"},
		{true, "/user/agent", "/user/agent"},
		{true, "", ""},
		{false, "/d/agent.sock", ""},
	} {
		l, f, _ := testLima(t, "darwin")
		t.Setenv("SSH_AUTH_SOCK", "/user/agent")
		l.Getenv = func(k string) string {
			if k == "SSH_AUTH_SOCK" {
				return "/user/agent"
			}
			return ""
		}
		// Its Want's forwarding, which its start edits in, not its config's.
		f.out["limactl list --json"] = `{"name":"v","status":"Stopped","config":{"ssh":{"forwardAgent":` + map[bool]string{true: "false", false: "true"}[tc.forwarding] + `}}}`
		if err := os.MkdirAll(l.Dirs.LimaVM("v"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(l.haPIDFile("v"), []byte("1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		editWritesYAML(t, l, f)
		w := backend.Want{AgentForwarding: tc.forwarding}
		if err := l.Start(t.Context(), "v", w, backend.Confinement{Net: backend.NetProxy, ProxyPort: 1, ProxyPassword: redact.NewSecret("pw"), SSHPort: 2, AgentSocket: tc.socket}); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(l.Dirs.VM("v").Profile())
		if err != nil {
			t.Fatal(err)
		}
		lits := regexp.MustCompile(`path-literal "([^"]*)"`).FindAllStringSubmatch(string(b), -1)
		got := ""
		if len(lits) > 0 {
			got = lits[0][1]
		}
		if got != tc.want || len(lits) > 1 {
			t.Errorf("forwarding %v, socket %q: agent %q in\n%s", tc.forwarding, tc.socket, got, b)
		}
		if v, ok := envOf(startCall(t, f), "SSH_AUTH_SOCK"); v != tc.socket || ok != (tc.socket != "") {
			t.Errorf("socket %q: SSH_AUTH_SOCK=%q (set %v)", tc.socket, v, ok)
		}
		if fi, err := os.Stat(l.Dirs.Run()); err != nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("run folder: %v, %v", fi.Mode(), err)
		}
	}
}

// Lima can report a failed start for a VM that's running: carry on with a
// warning, unless it doesn't answer over ssh; then it's stopped.
func TestDegradedStart(t *testing.T) {
	l, warned, stubs := stubLima(t)
	const vm = "devvm-img-0123456789ab"
	stubs.VM(t, vm, "Stopped")
	stubs.Touch(t, "start-fails")
	if err := l.start(t.Context(), vm, nil, backend.Confinement{}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"started_with_warnings vm=" + vm}; !slices.Equal(warned.Names(), want) {
		t.Errorf("warned %q, want %q", warned.Names(), want)
	}
	stubs.VM(t, vm, "Stopped")
	stubs.Touch(t, "shell-hangs")
	l.SSHTimeout = 200 * time.Millisecond
	err := l.start(t.Context(), vm, nil, backend.Confinement{})
	le, ok := errors.AsType[*backend.LoggedError](err)
	if !ok || le.Msg.Names() != "start_no_ssh console="+filepath.Join(l.Dirs.LimaVM(vm), "serial*.log")+" vm="+vm {
		t.Errorf("no ssh: %v", err)
	}
	if !slices.Contains(stubs.Calls(t), "limactl stop --force "+vm) {
		t.Errorf("not stopped: %q", stubs.Calls(t))
	}
	// Not running after a failed start: it failed.
	f := &fakeRunner{out: map[string]string{"limactl list --json": `{"name":"v","status":"Stopped"}`}, fail: []string{"limactl start"}}
	l.Runner = f
	err = l.start(t.Context(), "v", nil, backend.Confinement{})
	if le, ok := errors.AsType[*backend.LoggedError](err); !ok || le.Msg.ID() != "start_failed" {
		t.Errorf("a failed start: %v", err)
	}
}

// The hostagent logs the shell's proxy password as it starts a VM without
// network policy, and Lima's boot scripts trace the VM's proxy URL to its
// serial consoles: once limactl start ends, started or failed, each
// password in those logs is stars, the file's length kept. A log devvm
// can't change warns.
func TestStartRedactsTheHostagentLog(t *testing.T) {
	l, f, warned := testLima(t, "darwin")
	pass := "e2e-not-a-secret" //nolint:gosec // a fake password
	log := filepath.Join(l.Dirs.LimaVM("v"), "ha.stderr.log")
	line := "level=info msg=\"Replacing \\\"HTTPS_PROXY\\\" value `http://alice:" + pass + "@127.0.0.1:3128`\"\n"
	// The boot scripts' trace of the guest's environment, on each serial console.
	key := strings.Repeat("0123456789abcdef", 4)
	serials := []string{filepath.Join(l.Dirs.LimaVM("v"), "serial.log"), filepath.Join(l.Dirs.LimaVM("v"), "serialv.log")}
	trace := "+ echo HTTPS_PROXY=http://devvm:" + key + "@192.168.5.2:3129\n"
	f.do = func(c call) {
		if slices.Contains(c.argv, "start") {
			if err := os.MkdirAll(filepath.Dir(log), 0o700); err != nil {
				t.Error(err)
			}
			if err := os.WriteFile(log, []byte(line), 0o600); err != nil {
				t.Error(err)
			}
			for _, s := range serials {
				if err := os.WriteFile(s, []byte(trace), 0o600); err != nil {
					t.Error(err)
				}
			}
		}
	}
	f.out["limactl list --json"] = `{"name":"v","status":"Stopped"}`
	want := strings.ReplaceAll(line, pass, strings.Repeat("*", len(pass)))
	wantTrace := strings.ReplaceAll(trace, key, strings.Repeat("*", len(key)))
	defer func() {
		for _, s := range serials {
			if b, _ := os.ReadFile(s); string(b) != wantTrace { //nolint:gosec // the test's own file
				t.Errorf("%s: %q, want %q", filepath.Base(s), b, wantTrace)
			}
		}
	}()
	for _, fail := range []bool{false, true} {
		f.fail = nil
		if fail {
			f.fail = []string{"limactl start"}
		}
		err := l.start(t.Context(), "v", nil, backend.Confinement{Net: backend.NetDirect})
		if (err != nil) != fail {
			t.Errorf("failed %v: %v", fail, err)
		}
		if b, _ := os.ReadFile(log); string(b) != want { //nolint:gosec // the test's own file
			t.Errorf("failed %v: log %q, want %q", fail, b, want)
		}
	}
	if len(warned.Names()) != 0 {
		t.Errorf("warned %q", warned.Names())
	}
	f.fail, f.do = nil, nil
	if err := os.Remove(log); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(log, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := l.start(t.Context(), "v", nil, backend.Confinement{Net: backend.NetDirect}); err != nil {
		t.Fatal(err)
	}
	if ids := warned.IDs(); !slices.Equal(ids, []string{"lima_log_unredacted"}) {
		t.Errorf("an unreadable log: warned %q", warned.Names())
	}
}

// A build without a company proxy has no proxy to hold its VM's logs to
// their cap: the build's own process does, until it's done.
func TestBuildKeepsLogs(t *testing.T) {
	l, _, _ := testLima(t, "darwin")
	const vm = "devvm-img-0123456789ab"
	serial := filepath.Join(l.Dirs.LimaVM(vm), "serialv.log")
	if err := os.MkdirAll(filepath.Dir(serial), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(serial, []byte(strings.Repeat("boot\n", vmlog.VMMax/4)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, done, err := buildProxy(t.Context(), l, vm, backend.NetNone)
	if err != nil {
		t.Fatal(err)
	}
	defer done(true)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if fi, err := os.Stat(serial); err == nil && fi.Size() == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("not rotated")
		}
	}
	if fi, err := os.Stat(serial + ".1"); err != nil || fi.Size() > vmlog.VMMax {
		t.Errorf(".1: %v, %v", fi, err)
	}
}

// Running: the hostagent's pid file is the lifeline, stopped or not; its
// pid and when it was written are the start's ID; and the VM is confined
// while it runs on the hostagent Start started with NetProxy, which runs.
func TestRunning(t *testing.T) {
	l, f, _ := testLima(t, "darwin")
	f.out["limactl list --json"] = `{"name":"v","status":"Stopped"}`
	ha := l.haPIDFile("v")
	// The logs Lima writes as the VM runs, which its proxy holds to their
	// cap: the serial consoles' and the hostagent's.
	dir := l.Dirs.LimaVM("v")
	logs := []string{
		filepath.Join(dir, "serial.log"), filepath.Join(dir, "serialv.log"), filepath.Join(dir, "serialp.log"),
		filepath.Join(dir, "ha.stdout.log"), filepath.Join(dir, "ha.stderr.log"),
	}
	if r, err := l.Running(t.Context(), "v"); !reflect.DeepEqual(r, backend.Run{Lifeline: ha, Logs: logs}) || err != nil {
		t.Errorf("never started: %+v, %v", r, err)
	}
	if err := os.MkdirAll(l.Dirs.LimaVM("v"), 0o700); err != nil {
		t.Fatal(err)
	}
	pid := strconv.Itoa(os.Getpid())
	if err := os.WriteFile(ha, []byte(pid+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	editWritesYAML(t, l, f)
	if err := l.start(t.Context(), "v", nil, backend.Confinement{Net: backend.NetProxy, ProxyPort: 1, ProxyPassword: redact.NewSecret("pw"), SSHPort: 2}); err != nil {
		t.Fatal(err)
	}
	r, err := l.Running(t.Context(), "v")
	if !r.Confined || r.Lifeline != ha || !strings.HasPrefix(r.ID, pid+" ") || err != nil {
		t.Errorf("after Start: %+v, %v", r, err)
	}
	// Started again some other way: a new hostagent, a new start.
	if err := os.WriteFile(ha, []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if again, err := l.Running(t.Context(), "v"); again.Confined || again.ID == r.ID || err != nil {
		t.Errorf("another hostagent: %+v, %v", again, err)
	}
	// Started by Start, but without its proxy (network policy off then).
	if err := os.WriteFile(ha, []byte(pid+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := l.start(t.Context(), "v", nil, backend.Confinement{Net: backend.NetDirect}); err != nil {
		t.Fatal(err)
	}
	if r, err := l.Running(t.Context(), "v"); r.Confined || r.ID == "" || err != nil {
		t.Errorf("started direct: %+v, %v", r, err)
	}
}
