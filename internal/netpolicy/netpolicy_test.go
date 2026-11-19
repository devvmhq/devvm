package netpolicy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/fstest"
	"time"

	"github.com/devvmhq/devvm/assets"
	"github.com/devvmhq/devvm/internal/agentlogin"
	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/backend/backendtest"
	"github.com/devvmhq/devvm/internal/daemon"
	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/ports"
	"github.com/devvmhq/devvm/internal/proxy"
	"github.com/devvmhq/devvm/internal/redact"
	"github.com/devvmhq/devvm/internal/secrets"
	"github.com/devvmhq/devvm/internal/settings"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/tools"
	"github.com/devvmhq/devvm/internal/vmname"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// sandbox returns devvm's folders in a temporary folder, and a VM's state
// there with that image.
// vmV is a VM's name, for tests of a VM the sandbox's isn't.
const vmV = "devvm-v-0123456789abc6"

func sandbox(t *testing.T, img string, tools ...string) (paths.Dirs, *state.VM) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_DATA_HOME", root) // the run log
	d := paths.Dirs{Data: filepath.Join(root, "data"), Config: filepath.Join(root, "config"), Lima: filepath.Join(root, "lima")}
	return d, newVM(t, d, "devvm-web-0123456789abcd", img, tools...)
}

// newVM writes the state of a VM named name, made from img with these
// shared tools, as up does.
func newVM(t *testing.T, d paths.Dirs, name vmname.VM, img string, names ...string) *state.VM {
	t.Helper()
	st, err := state.New(d, name)
	if err != nil {
		t.Fatal(err)
	}
	st.Image, st.Dir, st.Backend, st.Created = img, "/w", "fake", time.Now()
	if err := tools.Set(st, tools.InVM{Names: names}); err != nil {
		t.Fatal(err)
	}
	if err := st.Create(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	return st
}

// change changes a VM's state as f says, as devvm does.
func change(t *testing.T, st *state.VM, f func(*state.VM) error) {
	t.Helper()
	if err := st.Update(t.Context(), f); err != nil {
		t.Fatal(err)
	}
}

// setNet sets a VM's network policy section as f says.
func setNet(t *testing.T, st *state.VM, f func(*Net)) {
	t.Helper()
	change(t, st, func(st *state.VM) error {
		n, err := Of(st)
		if err != nil {
			return err
		}
		f(&n)
		return Set(st, n)
	})
}

// netOf is a VM's network policy section.
func netOf(t *testing.T, st *state.VM) Net {
	t.Helper()
	n, err := Of(st)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// hostPorts is the host ports devvm chose for a VM.
func hostPorts(t *testing.T, st *state.VM) ports.Host {
	t.Helper()
	p, err := ports.Of(st)
	if err != nil {
		t.Fatal(err)
	}
	return p.Host
}

// setPorts sets a VM's ports section as f says.
func setPorts(t *testing.T, st *state.VM, f func(*ports.Ports)) {
	t.Helper()
	change(t, st, func(st *state.VM) error {
		p, err := ports.Of(st)
		if err != nil {
			return err
		}
		f(&p)
		return ports.Set(st, p)
	})
}

// A VM with network policy reaches only itself without its proxy: its
// links go through the proxy, so no link name is in its NO_PROXY.
func TestNoProxy(t *testing.T) {
	want := map[string]string{
		"NO_PROXY": "localhost,127.0.0.1,::1", "no_proxy": "localhost,127.0.0.1,::1", "NODE_USE_ENV_PROXY": "1",
	}
	if NoProxy != "localhost,127.0.0.1,::1" || !maps.Equal(Env, want) {
		t.Errorf("NoProxy = %q, Env = %q", NoProxy, Env)
	}
}

// A VM's policies: devvm's built-in ones by name, then the user's files.
func TestPolicies(t *testing.T) {
	d := paths.Dirs{Data: "/d", Config: "/c", Lima: "/l"}
	user := []string{"/c/policy", "/d/vms/0123456789abc1/policy", "/d/vms/0123456789abc1/run/secrets.policy"}
	for _, c := range []struct {
		img           image.Name
		tools         []string
		builtin, file []string
	}{
		{"base", []string{"node", "claude"}, []string{"layers/base.policy", "shared-tools/node.policy", "shared-tools/claude.policy"}, user},
		// A custom layer's allowlist is the user's, next to its script.
		{"pg", []string{"node"}, []string{"layers/base.policy", "shared-tools/node.policy"}, slices.Concat([]string{"/c/layers/pg.policy"}, user)},
		// A built-in layer's is devvm's.
		{"docker", []string{"node"}, []string{"layers/base.policy", "shared-tools/node.policy", "layers/docker.policy"}, user},
		{"base", nil, []string{"layers/base.policy"}, user},
		// No name, and a name with empty parts or base among them, as a
		// VM's record may hold.
		{"", nil, []string{"layers/base.policy"}, user},
		{"base+pg++docker", nil, []string{"layers/base.policy", "layers/docker.policy"}, slices.Concat([]string{"/c/layers/pg.policy"}, user)},
	} {
		s := Policies(d, "devvm-vm1-0123456789abc1", TierAllowlist, c.img, c.tools)
		if !slices.Equal(s.Builtin, c.builtin) || !slices.Equal(s.Files, c.file) || len(s.Denies) > 0 {
			t.Errorf("Policies(%s, %q) = %q; want %q, %q", c.img, c.tools, s, c.builtin, c.file)
		}
	}
	// A part that's no layer's name never names a file outside the
	// user's layers, and a name that's no shared tool's names none of
	// devvm's, on any tier.
	for _, tier := range Tiers() {
		for _, bad := range []image.Name{"../x", "pg+../../etc/x", "a/b", "X", ".x"} {
			s := Policies(d, "devvm-vm1-0123456789abc1", tier, bad, []string{"../x", "X"})
			if got := slices.Concat(s.Builtin, s.Files, s.Denies); slices.ContainsFunc(got, func(f string) bool {
				return strings.Contains(f, "x.policy") || strings.Contains(f, "b.policy") || strings.Contains(f, "X.policy")
			}) {
				t.Errorf("Policies(%s, %q) = %q", tier, bad, got)
			}
		}
	}
}

// Each tier's policies: open has none; internet allows every public host
// and port, and none its agents' hosts alone, each with its secrets'
// hosts, and with only the denies of the user's files, layers' included.
func TestTierPolicies(t *testing.T) {
	d := paths.Dirs{Data: "/d", Config: "/c", Lima: "/l"}
	vm, img, tools := vmname.VM("devvm-vm1-0123456789abc1"), image.Name("base+pg+docker"), []string{"node", "claude", "codex"}
	secrets := []string{"/d/vms/0123456789abc1/run/secrets.policy"}
	denies := []string{"/c/layers/pg.policy", "/c/policy", "/d/vms/0123456789abc1/policy"}
	for _, c := range []struct {
		tier Tier
		want PolicySet
	}{
		{TierOpen, PolicySet{}},
		{TierInternet, PolicySet{Builtin: []string{InternetPolicy}, Files: secrets, Denies: denies}},
		{TierNone, PolicySet{Builtin: []string{"shared-tools/claude.policy", "shared-tools/codex.policy"}, Files: secrets, Denies: denies}},
	} {
		got := Policies(d, vm, c.tier, img, tools)
		if !slices.Equal(got.Builtin, c.want.Builtin) || !slices.Equal(got.Files, c.want.Files) || !slices.Equal(got.Denies, c.want.Denies) {
			t.Errorf("%s: %q, want %q", c.tier, got, c.want)
		}
	}
	if _, err := fs.Stat(assets.FS, InternetPolicy); err != nil {
		t.Errorf("the internet tier's policy: %v", err)
	}
}

// The tiers' order, what each confines, and a VM's tier as its state has
// it: the default without one, refused when it's no tier's, the
// strictest where it can't be read.
func TestTiers(t *testing.T) {
	if !TierNone.AtLeast(TierAllowlist) || !TierAllowlist.AtLeast(TierInternet) || !TierInternet.AtLeast(TierOpen) ||
		!TierOpen.AtLeast(TierOpen) || TierOpen.AtLeast(TierInternet) || TierAllowlist.AtLeast(TierNone) ||
		Tier("x").AtLeast(TierOpen) || TierNone.AtLeast("x") {
		t.Error("the tiers' order")
	}
	for _, tier := range Tiers() {
		want := backend.NetProxy
		if tier == TierOpen {
			want = backend.NetDirect
		}
		if tier.Confined() != (tier != TierOpen) || tier.Net() != want {
			t.Errorf("%s: confined %v, net %v", tier, tier.Confined(), tier.Net())
		}
	}
	d, st := sandbox(t, "base")
	if n, err := Of(st); err != nil || n.Tier != TierDefault || TierOf(st) != TierAllowlist {
		t.Errorf("no tier: %+v, %v", n, err)
	}
	change := func(v any) {
		t.Helper()
		if err := st.Update(t.Context(), func(st *state.VM) error { return state.SetSection(st, Section, v) }); err != nil {
			t.Fatal(err)
		}
	}
	change(map[string]string{"tier": "open"})
	if TierOf(st) != TierOpen {
		t.Errorf("open read as %s", TierOf(st))
	}
	change(map[string]string{"tier": "wide"})
	again, _ := state.ReadVM(d, st.Name())
	if _, err := Of(again); err == nil || TierOf(again) != TierNone {
		t.Errorf("an unknown tier: %v, read as %s", err, TierOf(again))
	}
}

// Every image part and shared tool devvm has comes with its allowlist, in
// devvm's files.
func TestEveryPartHasAPolicy(t *testing.T) {
	for _, p := range slices.Concat([]string{"base"}, image.Toolchains, image.Agents, image.Builtins) {
		d := paths.Dirs{Data: "/d", Config: "/c"}
		img, tools := image.Name(p), []string(nil)
		if image.IsTool(p) {
			img, tools = "base", []string{p}
		}
		builtin := Policies(d, "devvm-vm1-0123456789abc1", TierAllowlist, img, tools).Builtin
		if p != "base" {
			builtin = builtin[1:]
		}
		if len(builtin) != 1 {
			t.Fatalf("%s's built-in policies: %q", p, builtin)
		}
		if _, err := fs.Stat(assets.FS, builtin[0]); err != nil {
			t.Errorf("%s has no policy: %v", p, err)
		}
	}
}

// Each VM has its own random key (its proxy's password) and salt (the end
// of its placeholders), kept.
func TestKeyAndSalt(t *testing.T) {
	d, a := sandbox(t, "base")
	b := newVM(t, d, "devvm-api-0123456789abc7", "base")
	ka, _ := Key(t.Context(), a)
	kb, _ := Key(t.Context(), b)
	sa, _ := Salt(t.Context(), a)
	sb, _ := Salt(t.Context(), b)
	for v, re := range map[string]string{ka.Reveal(): `^[0-9a-f]{64}$`, kb.Reveal(): `^[0-9a-f]{64}$`, sa: `^[0-9a-f]{32}$`, sb: `^[0-9a-f]{32}$`} {
		if !regexp.MustCompile(re).MatchString(v) {
			t.Errorf("%q doesn't match %s", v, re)
		}
	}
	if ka.Reveal() == kb.Reveal() || sa == sb || sa == ka.Reveal() {
		t.Errorf("keys %s %s, salts %s %s: not each its own", ka.Reveal(), kb.Reveal(), sa, sb)
	}
	again, _ := state.ReadVM(d, a.Name())
	if k, _ := Key(t.Context(), again); k.Reveal() != ka.Reveal() {
		t.Errorf("key not kept: %s, then %s", ka.Reveal(), k.Reveal())
	}
	claude, _ := secrets.Lookup("claude")
	github, _ := secrets.Lookup("github")
	if got := claude.Placeholder(sa); got != "sk-ant-oat01-devvm-proxy-managed-"+sa {
		t.Errorf("claude's placeholder = %q", got)
	}
	if got := github.Placeholder(sb); got != "devvm-proxy-managed-github-"+sb {
		t.Errorf("github's placeholder = %q", got)
	}
	if fi, err := os.Stat(d.VMs()); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("the state folder: %v, %v", fi.Mode(), err)
	}
}

// Each tier says what it reaches, its own message; no tier says nothing.
func TestTierDesc(t *testing.T) {
	want := map[Tier]string{
		TierOpen: "net_tier_open", TierInternet: "net_tier_internet",
		TierAllowlist: "net_tier_allowlist", TierNone: "net_tier_none", "": "",
	}
	for tier, id := range want {
		if got := tier.Desc().ID(); got != id {
			t.Errorf("%q.Desc() = %q, want %q", tier, got, id)
		}
	}
}

// settings' network defaults are the tiers, in their order, without
// learn, which is for one VM alone, and its default is TierDefault: its
// own list, as settings can't import this package.
func TestNetworkChoicesAreTheTiers(t *testing.T) {
	if got, want := settings.NetworkChoices(), TierNames(); !slices.Equal(got, want) {
		t.Errorf("settings.NetworkChoices() = %v, want %v", got, want)
	}
	if settings.NetworkDefault != string(TierDefault) {
		t.Errorf("settings.NetworkDefault = %s, want %s", settings.NetworkDefault, TierDefault)
	}
}

// proxies is a fake start: each "proxy" is a sleep, which says it listens
// unless quiet.
type proxies struct {
	mu    sync.Mutex
	quiet bool
	// stubborn proxies ignore SIGTERM.
	stubborn bool
	args     map[int][]string
	// reaped is each running proxy's, closed once it has ended and been
	// reaped.
	reaped map[int]chan struct{}
	n      int
}

// signal sends a proxy sig (daemon.Signal), as Stop does, and returns
// once a signal that ends it has and it's reaped, however busy the
// machine: a stubborn proxy goes on after SIGTERM.
func (f *proxies) signal(pid int, sig syscall.Signal) error {
	if err := daemon.Signal(pid, sig); err != nil {
		return err
	}
	f.mu.Lock()
	reaped, ok := f.reaped[pid]
	ends := sig == syscall.SIGKILL || !f.stubborn
	f.mu.Unlock()
	if ok && ends {
		select {
		case <-reaped:
		case <-time.After(30 * time.Second):
			return errors.New("a proxy that won't end")
		}
	}
	return nil
}

func (f *proxies) start(t *testing.T) daemon.Starter {
	return func(c daemon.Cmd) (int, error) {
		path, args := c.Path, c.Args
		f.mu.Lock()
		defer f.mu.Unlock()
		// Until the test ends, however slow.
		cmd := exec.CommandContext(t.Context(), "sleep", "3600")
		if f.stubborn {
			cmd = exec.CommandContext(t.Context(), "sh", "-c", `trap "" TERM; exec sleep 3600`)
		}
		if f.quiet {
			cmd = exec.CommandContext(t.Context(), "true")
		}
		if err := cmd.Start(); err != nil {
			return 0, err
		}
		pid := cmd.Process.Pid
		// It reads its config as the proxy does: _proxy -config FILE.
		cfg, err := proxy.ReadConfig(args[2])
		if err != nil {
			return 0, err
		}
		if f.quiet {
			_ = cmd.Wait()
			_, _ = fmt.Fprintf(c.Out, "devvm proxy: listen tcp %s: bind: address already in use\n", cfg.Listen)
		} else {
			// Reaped once it ends, as a detached process is.
			done := make(chan struct{})
			go func() { _ = cmd.Wait(); close(done) }()
			f.reaped[pid] = done
			t.Cleanup(func() { _ = cmd.Process.Kill(); <-done })
			// Its child, as the broker starts it and writes its pid file.
			child := exec.CommandContext(t.Context(), "sleep", "30")
			if err := child.Start(); err != nil {
				return 0, err
			}
			childDone := make(chan struct{})
			go func() { _ = child.Wait(); close(childDone) }()
			t.Cleanup(func() { _ = child.Process.Kill(); <-childDone })
			f.args[child.Process.Pid] = []string{path, proxy.ChildCommand, "-for", args[2]}
			if err := daemon.WritePID(proxy.ChildPIDFile(args[2]), child.Process.Pid); err != nil {
				return 0, err
			}
			if err := daemon.WriteStatus(c.Status, pid, cfg.Listen); err != nil {
				return 0, err
			}
		}
		f.args[pid] = append([]string{path}, args...)
		f.n++
		return pid, nil
	}
}

func (f *proxies) argsOf(pid int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a, ok := f.args[pid]; ok {
		return a, nil
	}
	return nil, errors.New("not a proxy")
}

func (f *proxies) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}

// testUI logs what the proxies and backends print, and refuses what they
// ask, without a terminal.
func testUI(t *testing.T) *msg.Fake {
	return &msg.Fake{OnAny: func(m msg.M) { t.Log(m.Names()) }}
}

func testProxy(t *testing.T, d paths.Dirs) (*Proxy, *proxies) {
	t.Helper()
	home := d
	home.Home = t.TempDir()
	store := secrets.NewStore(home, testUI(t))
	store.Keychain, store.Getenv = false, func(string) string { return "" }
	f := &proxies{args: map[int][]string{}, reaped: map[int]chan struct{}{}}
	p := NewProxy(d, assets.FS, "/bin/sh", store, testUI(t))
	p.Start, p.Args, p.Tick, p.Signal = f.start(t), f.argsOf, time.Millisecond, f.signal
	p.Getenv = func(string) string { return "" } // not the host's proxy settings
	p.Listening = func(context.Context, int) bool { return false }
	p.ExcludeFromBackup = func(context.Context, string) error { return nil }
	return p, f
}

// lifelined is a backend whose VMs' lifeline is /ha.pid.
func lifelined() *backendtest.Fake {
	f := backendtest.New("fake", nil)
	f.LifelineFile = "/ha.pid"
	return f
}

// A VM about to start: the CA, its ports, the confinement its backend
// gets, and its proxy, told the password in a file.
func TestPrepare(t *testing.T) {
	d, st := sandbox(t, "base", "node", "claude")
	p, f := testProxy(t, d)
	lima := backendtest.New("lima", nil)
	lima.LifelineFile = "/lima/vm/ha.pid"
	lima.LogFiles = []string{"/lima/vm/serial.log", "/lima/vm/ha.stderr.log"}
	var excluded []string
	p.OS = "darwin"
	p.ExcludeFromBackup = func(_ context.Context, dir string) error { excluded = append(excluded, dir); return nil }
	// The run, the VM's run and logs, and CA folders are the user's alone
	// after, whatever mode they had.
	files := d.VM(st.Name())
	ca := files.CA()
	for _, dir := range []string{d.Run(), files.Run(), files.Logs(), ca.Dir} {
		if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // the test's own folder
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // the test's own folder
			t.Fatal(err)
		}
	}
	conf, err := p.Prepare(t.Context(), lima, st, false, "")
	if err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	key := netOf(t, st).Made.ProxyKey
	sp, pp := hostPorts(t, st).SSH, hostPorts(t, st).Proxy
	if conf.Net != backend.NetProxy || conf.ProxyPassword.Reveal() != key.Reveal() || key.Empty() ||
		conf.ProxyPort != pp || pp == 0 || conf.SSHPort != sp || sp == 0 || sp == pp || conf.Env["NO_PROXY"] != "localhost,127.0.0.1,::1" {
		t.Errorf("confinement = %+v", conf)
	}
	// The VM's own CA and its next one, in its run folder.
	for _, f := range []string{ca.Cert(), ca.Key(), ca.NextCert(), ca.NextKey()} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("the CA: %v", err)
		}
	}
	if !slices.Equal(excluded, []string{ca.Dir}) {
		t.Errorf("kept out of backups: %q", excluded)
	}
	pid, ok := p.PID(st.Name())
	if !ok {
		t.Fatal("no proxy runs")
	}
	// devvm's files stay in the binary: nothing is written out for the proxy.
	if _, err := os.Stat(filepath.Join(d.Data, "assets")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("devvm's files written out: %v", err)
	}
	// Its arguments name its config alone: the key and salt are in no
	// process's arguments.
	config := files.ProxyConfig()
	if want := []string{"/bin/sh", "_proxy", "-config", config}; !slices.Equal(f.args[pid], want) {
		t.Errorf("the proxy's arguments %q, want %q", f.args[pid], want)
	}
	cfg, err := proxy.ReadConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	bin, _ := image.SHA256File("/bin/sh")
	caSum, _ := image.SHA256File(ca.Cert())
	nextSum, _ := image.SHA256File(ca.NextCert())
	if want := (proxy.Config{
		Version: proxy.ConfigVersion, Listen: "127.0.0.1:" + strconv.Itoa(pp), VM: st.Name(), Password: key,
		BuiltinPolicies: []string{"layers/base.policy", "shared-tools/node.policy", "shared-tools/claude.policy"},
		Policies:        []string{d.Policy(), files.Policy(), files.SecretsPolicy()},
		Log:             files.EventLog(), ProxyLog: files.ProxyLog(), Lifeline: "/lima/vm/ha.pid",
		// The logs its backend writes as it runs, which the proxy holds to their cap.
		VMLogs: []string{"/lima/vm/serial.log", "/lima/vm/ha.stderr.log"},
		CA:     ca.Cert(), CAKey: ca.Key(), CANext: ca.NextCert(), CANextKey: ca.NextKey(),
		Secrets: files.Secrets(), Captures: agentlogin.Endpoints(),
		// Its links' list: in its run folder, devvm's alone.
		Links: files.LinkList(), LinkSockets: d.Run(),
		DevvmSHA256: bin, CASHA256: caSum, CANextSHA256: nextSum,
	}); !reflect.DeepEqual(cfg, want) || bin == "" || caSum == "" || nextSum == "" || caSum == nextSum {
		t.Errorf("config = %+v\nwant %+v", cfg, want)
	}
	for path, mode := range map[string]os.FileMode{
		config: 0o600, d.Run(): 0o700, files.Run(): 0o700, files.Logs(): 0o700, files.ProxyLog(): 0o600,
		files.Secrets(): 0o600, ca.Dir: 0o700, ca.Key(): 0o600, ca.NextKey(): 0o600,
	} {
		if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != mode {
			t.Errorf("%s: %v, want %v (%v)", path, fi.Mode().Perm(), mode, err)
		}
	}
	// Not on Linux.
	p.OS, excluded = "linux", nil
	p.Stop(st.Name())
	if _, err := p.Prepare(t.Context(), lima, st, false, ""); err != nil || excluded != nil {
		t.Errorf("Linux: %v, excluded %q", err, excluded)
	}
	// Its ssh port is one of its own forwards now: a new one, as its
	// backend would refuse it.
	p.Stop(st.Name())
	setPorts(t, st, func(p *ports.Ports) { p.Forward = strconv.Itoa(p.Host.SSH) })
	conf, err = p.Prepare(t.Context(), lima, st, false, "")
	if err != nil || conf.SSHPort == sp || hostPorts(t, st).SSH != conf.SSHPort {
		t.Errorf("forwarded ssh port: %d (had %d), %v", conf.SSHPort, sp, err)
	}
}

// The proxy has started only when it says it's listening: not when
// something else holds the port and it exits.
func TestPrepareWaitsForTheProxy(t *testing.T) {
	d, st := sandbox(t, "base")
	p, f := testProxy(t, d)
	f.quiet = true
	_, err := p.Prepare(t.Context(), lifelined(), st, false, "")
	if want := "proxy_start_failed log=" + d.VM(st.Name()).ProxyLog() + " vm=" + st.Name().String(); msg.NamesOf(err) != want {
		t.Errorf("Prepare = %v, want %s", msg.NamesOf(err), want)
	}
}

// A start's proxy relays the agent it forwards on the VM's own socket,
// which its confinement carries; a running VM's goes on relaying the one
// its start's did, without starting again, whichever agent devvm has now.
// A start without one relays none.
func TestPrepareRelaysTheAgent(t *testing.T) {
	d, st := sandbox(t, "base")
	p, f := testProxy(t, d)
	vm, config := st.Name(), d.VM(st.Name()).ProxyConfig()
	relayed := func() (string, string) {
		t.Helper()
		cfg, err := proxy.ReadConfig(config)
		if err != nil {
			t.Fatal(err)
		}
		return cfg.Agent, cfg.AgentSocket
	}
	conf, err := p.Prepare(t.Context(), lifelined(), st, false, "/agent.sock")
	if err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if a, s := relayed(); a != "/agent.sock" || s != d.AgentRelay(vm) || conf.AgentSocket != s {
		t.Errorf("a start relays %q on %q, the VM gets %q", a, s, conf.AgentSocket)
	}
	conf, err = p.Prepare(t.Context(), lifelined(), st, true, "")
	if err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if a, s := relayed(); a != "/agent.sock" || conf.AgentSocket != d.AgentRelay(vm) || f.count() != 1 {
		t.Errorf("running: relays %q on %q, the VM gets %q, %d starts", a, s, conf.AgentSocket, f.count())
	}
	conf, err = p.Prepare(t.Context(), lifelined(), st, false, "")
	if err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if a, s := relayed(); a != "" || s != "" || conf.AgentSocket != "" {
		t.Errorf("a start without one relays %q on %q, the VM gets %q", a, s, conf.AgentSocket)
	}
	// Stopped by a signal, a proxy leaves its socket: Stop deletes it.
	if err := os.WriteFile(d.AgentRelay(vm), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !p.Stop(vm) {
		t.Fatal("the proxy didn't stop")
	}
	if _, err := os.Stat(d.AgentRelay(vm)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the relay's socket after Stop: %v", err)
	}
}

// A running VM keeps its proxy, unless the proxy is from another devvm
// binary, holds a CA since replaced, or its allowlist files changed. A
// stopped VM's always starts again.
func TestPrepareRestartsTheProxy(t *testing.T) {
	d, st := sandbox(t, "base", "node")
	p, f := testProxy(t, d)
	prepare := func(running bool) {
		t.Helper()
		if _, err := p.Prepare(t.Context(), lifelined(), st, running, ""); err != nil {
			t.Fatal(msg.NamesOf(err))
		}
	}
	prepare(false)
	pp := strconv.Itoa(hostPorts(t, st).Proxy)
	prepare(true)
	if f.count() != 1 {
		t.Fatalf("a running VM's proxy started again: %d starts", f.count())
	}
	config := d.VM(st.Name()).ProxyConfig()
	for i, change := range []func(){
		// It started from another devvm binary.
		func() {
			cfg, err := proxy.ReadConfig(config)
			if err != nil {
				t.Fatal(err)
			}
			cfg.DevvmSHA256 = "old"
			must(t, os.WriteFile(config, cfg.Encode(), 0o600))
		},
		// Its CA, or its next one, was replaced.
		func() {
			_ = os.WriteFile(d.VM(st.Name()).CA().Cert(), []byte("rotated\n"), 0o600)
			p.InitCA = func(context.Context, paths.CA) error { return nil }
		},
		func() { _ = os.WriteFile(d.VM(st.Name()).CA().NextCert(), []byte("rotated\n"), 0o600) },
		// Its built-in policies change, then its files alone (a layer of
		// the user's).
		func() {
			change(t, st, func(st *state.VM) error { return tools.Set(st, tools.InVM{Names: []string{"node", "claude"}}) })
		},
		func() { change(t, st, func(st *state.VM) error { st.Image = "pg"; return nil }) },
	} {
		change()
		prepare(true)
		if f.count() != i+2 {
			t.Errorf("change %d: %d starts", i, f.count())
		}
	}
	if got := strconv.Itoa(hostPorts(t, st).Proxy); got != pp {
		t.Errorf("the port moved: %s, then %s", pp, got)
	}
	// What it started with: both lists.
	cfg, err := proxy.ReadConfig(config)
	if want := []string{"layers/base.policy", "shared-tools/node.policy", "shared-tools/claude.policy"}; err != nil || !slices.Equal(cfg.BuiltinPolicies, want) {
		t.Errorf("built-in policies %q (%v), want %q", cfg.BuiltinPolicies, err, want)
	}
	if want := []string{filepath.Join(d.Layers(), "pg.policy"), d.Policy(), d.VM(st.Name()).Policy(), d.VM(st.Name()).SecretsPolicy()}; !slices.Equal(cfg.Policies, want) {
		t.Errorf("policies %q, want %q", cfg.Policies, want)
	}
	// A config that's gone, or that the proxy wouldn't read, counts as
	// another.
	for i, change := range []func(){
		func() { must(t, os.Remove(config)) },
		func() { must(t, os.Chmod(config, 0o644)) }, //nolint:gosec // a config others may read, which the proxy refuses
	} {
		n := f.count()
		change()
		prepare(true)
		if f.count() != n+1 {
			t.Errorf("config change %d: %d starts, want %d", i, f.count(), n+1)
		}
	}
	prepare(false)
	if f.count() != 9 {
		t.Errorf("a stopped VM's proxy didn't start again: %d starts", f.count())
	}
	// A running VM whose proxy is gone, with its port taken by another
	// program.
	p.Stop(st.Name())
	p.Listening = func(_ context.Context, port int) bool { return strconv.Itoa(port) == pp }
	if _, err := p.Prepare(t.Context(), lifelined(), st, true, ""); msg.NamesOf(err) != "proxy_port_taken port="+pp+" vm="+st.Name().String() {
		t.Errorf("port taken: %v", msg.NamesOf(err))
	}
	// Stopped, it moves.
	if _, err := p.Prepare(t.Context(), lifelined(), st, false, ""); err != nil || strconv.Itoa(hostPorts(t, st).Proxy) == pp {
		t.Errorf("stopped with its port taken: %v, port %d", err, hostPorts(t, st).Proxy)
	}
	// A port only VMs' ssh may have, which an older devvm chose: kept while
	// the VM runs, a new one at its next start.
	p.Stop(st.Name())
	p.Listening = func(context.Context, int) bool { return false }
	ssh := ports.SSHPorts()[0]
	must(t, st.Update(t.Context(), func(st *state.VM) error {
		pp, err := ports.Of(st)
		if err != nil {
			return err
		}
		pp.Host.Proxy = ssh
		return ports.Set(st, pp)
	}))
	if _, err := p.Prepare(t.Context(), lifelined(), st, true, ""); err != nil || hostPorts(t, st).Proxy != ssh {
		t.Errorf("running on an ssh port: %v, port %d", err, hostPorts(t, st).Proxy)
	}
	p.Stop(st.Name())
	if _, err := p.Prepare(t.Context(), lifelined(), st, false, ""); err != nil || hostPorts(t, st).Proxy == ssh {
		t.Errorf("stopped on an ssh port: %v, port %d", err, hostPorts(t, st).Proxy)
	}
}

// The company proxy in devvm's environment goes to the VM's proxy in its
// config, the user's alone, never in its arguments; a change starts it
// again, and one it can't go through is refused.
func TestPrepareUpstream(t *testing.T) {
	d, st := sandbox(t, "base")
	p, f := testProxy(t, d)
	const corp = "http://corp:pa55@proxy.corp:8080" //nolint:gosec // a fake company proxy's fake password
	env := map[string]string{"https_proxy": corp, "NO_PROXY": ".corp.example"}
	p.Getenv = func(k string) string { return env[k] }
	prepare := func(running bool) error {
		t.Helper()
		_, err := p.Prepare(t.Context(), lifelined(), st, running, "")
		return err
	}
	if err := prepare(false); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	config := d.VM(st.Name()).ProxyConfig()
	pid, _ := p.PID(st.Name())
	if args := strings.Join(f.args[pid], " "); strings.Contains(args, "pa55") {
		t.Errorf("the proxy's arguments hold the company proxy's password: %q", args)
	}
	upstream := func() *proxy.Upstream {
		t.Helper()
		cfg, err := proxy.ReadConfig(config)
		if err != nil {
			t.Fatal(err)
		}
		return cfg.Upstream
	}
	if up := upstream(); up == nil || *up != (proxy.Upstream{HTTPS: corp, NoProxy: ".corp.example"}) {
		t.Errorf("upstream = %+v", up)
	}
	if fi, err := os.Stat(config); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("config mode: %v %v", fi, err)
	}
	// Running, the same settings keep it; others start it again.
	if err := prepare(true); err != nil || f.count() != 1 {
		t.Errorf("unchanged: %v, %d starts", msg.NamesOf(err), f.count())
	}
	env["HTTP_PROXY"] = "proxy.corp:3128"
	if err := prepare(true); err != nil || f.count() != 2 {
		t.Errorf("changed: %v, %d starts", msg.NamesOf(err), f.count())
	}
	// None: none in its config, with a warning, as a shell without the
	// settings would start it; the next start without one warns no more.
	var warned []string
	p.Reporter = &msg.Fake{OnWarn: func(m msg.M) { warned = append(warned, m.Names()) }}
	env = map[string]string{}
	if err := prepare(true); err != nil || f.count() != 3 {
		t.Fatalf("none: %v, %d starts", msg.NamesOf(err), f.count())
	}
	if up := upstream(); up != nil {
		t.Errorf("without a company proxy: %+v", up)
	}
	gone := "upstream_proxy_gone file=" + d.ConfigFile() + " vm_name=" + st.Name().String()
	if !slices.Equal(warned, []string{gone}) {
		t.Errorf("warned %q, want %s", warned, gone)
	}
	warned = nil
	if err := prepare(false); err != nil || warned != nil {
		t.Errorf("none again: %v, warned %q", msg.NamesOf(err), warned)
	}
	for _, c := range []struct{ env, value, want string }{
		{"HTTPS_PROXY", "https://corp:pa55@proxy.corp", "upstream_proxy_scheme scheme=https:// setting=HTTPS_PROXY"},
		{"http_proxy", "socks5://proxy.corp:1080", "upstream_proxy_scheme scheme=socks5:// setting=HTTP_PROXY"},
		{"HTTPS_PROXY", "http://proxy.corp:http", "upstream_proxy_invalid setting=HTTPS_PROXY"},
	} {
		env = map[string]string{c.env: c.value}
		if err := prepare(false); msg.NamesOf(err) != c.want {
			t.Errorf("%s=%s: %v, want %s", c.env, c.value, msg.NamesOf(err), c.want)
		}
	}
}

// config.toml's company proxy, when devvm's environment sets none: its URL
// names the user, and the password stays in devvm's secrets, which the
// proxy looks up itself, so its config never holds it. Without the password
// saved, the VM doesn't start.
func TestPrepareUpstreamConfig(t *testing.T) {
	d, st := sandbox(t, "base")
	p, _ := testProxy(t, d)
	if err := os.MkdirAll(d.Config, 0o700); err != nil {
		t.Fatal(err)
	}
	const url = "http://alice@proxy.corp:3128"
	if err := os.WriteFile(d.ConfigFile(), []byte("[proxy]\nhttps = \""+url+"\"\nno-proxy = \".corp\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prepare := func() error {
		t.Helper()
		_, err := p.Prepare(t.Context(), lifelined(), st, false, "")
		return err
	}
	if err := prepare(); msg.NamesOf(err) != "proxy_password_missing" {
		t.Errorf("no password saved: %v", msg.NamesOf(err))
	}
	if err := p.Secrets.Save(t.Context(), proxy.UpstreamSecret, redact.NewSecret("s3cret pa55")); err != nil {
		t.Fatal(err)
	}
	if err := prepare(); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	config := d.VM(st.Name()).ProxyConfig()
	cfg, err := proxy.ReadConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	if want := (proxy.Upstream{HTTPS: url, NoProxy: ".corp", Secret: proxy.UpstreamSecret}); cfg.Upstream == nil || *cfg.Upstream != want {
		t.Errorf("upstream = %+v", cfg.Upstream)
	}
	if b, _ := os.ReadFile(config); strings.Contains(string(b), "pa55") { //nolint:gosec // G304: the test's own sandbox
		t.Error("the proxy's config holds the company proxy's password")
	}
	// The environment's wins.
	p.Getenv = func(k string) string { return map[string]string{"HTTPS_PROXY": "http://env.corp:8080"}[k] }
	if err := prepare(); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if cfg, _ := proxy.ReadConfig(config); cfg.Upstream == nil || *cfg.Upstream != (proxy.Upstream{HTTPS: "http://env.corp:8080"}) {
		t.Errorf("the environment's: %+v", cfg.Upstream)
	}
}

// A proxy that needs starting again is stopped, killed if SIGTERM doesn't
// end it; one that won't end fails the start, never kept with its old
// settings.
func TestPrepareStopsTheOldProxy(t *testing.T) {
	d, st := sandbox(t, "base")
	p, f := testProxy(t, d)
	p.Tick = 20 * time.Millisecond
	f.stubborn = true
	if _, err := p.Prepare(t.Context(), lifelined(), st, false, ""); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	old, _ := p.PID(st.Name())
	if _, err := p.Prepare(t.Context(), lifelined(), st, false, ""); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if pid, ok := p.PID(st.Name()); !ok || pid == old || daemon.Alive(old) {
		t.Errorf("the old proxy %d: alive %v; now %d", old, daemon.Alive(old), pid)
	}
	p.Signal = func(int, syscall.Signal) error { return nil } // signals that never arrive
	if _, err := p.Prepare(t.Context(), lifelined(), st, false, ""); msg.NamesOf(err) != "proxy_stop_failed vm="+st.Name().String() {
		t.Errorf("a proxy that won't stop: %v", msg.NamesOf(err))
	}
}

// An open VM starts on its backend's own network with its proxy, which is
// open, without policies, and puts its secrets in as every tier's does:
// with the VM's own CAs, its salt and its secret files. A VM made open
// after a confined tier keeps its CAs.
func TestPrepareOpen(t *testing.T) {
	d, st := sandbox(t, "base", "node")
	p, _ := testProxy(t, d)
	p.Tick = 20 * time.Millisecond
	vm, ca := st.Name(), d.VM(st.Name()).CA()
	setNet(t, st, func(n *Net) { n.Tier = TierOpen })
	conf, err := p.Prepare(t.Context(), lifelined(), st, false, "")
	if err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if conf.Net != backend.NetDirect || conf.ProxyPort == 0 || conf.ProxyPassword.Empty() || !conf.GuestProxy() {
		t.Errorf("open's confinement: %+v", conf)
	}
	for _, f := range []string{ca.Cert(), ca.Key(), ca.NextCert(), ca.NextKey(), d.VM(vm).Secrets()} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("open's %s: %v", f, err)
		}
	}
	if n, err := Of(st); err != nil || n.Made.ProxySalt == "" {
		t.Errorf("open's salt: %+v, %v", n.Made, err)
	}
	cfg, err := proxy.ReadConfig(d.VM(vm).ProxyConfig())
	if err != nil || !cfg.Open || cfg.CA != ca.Cert() || cfg.CANext != ca.NextCert() || cfg.Secrets != d.VM(vm).Secrets() ||
		cfg.CASHA256 == "" || len(cfg.BuiltinPolicies)+len(cfg.Policies)+len(cfg.DenyPolicies) > 0 {
		t.Errorf("open's proxy: %+v, %v", cfg, err)
	}
	cert, _ := os.ReadFile(ca.Cert())
	setNet(t, st, func(n *Net) { n.Tier = TierAllowlist })
	if _, err := p.Prepare(t.Context(), lifelined(), st, false, ""); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	setNet(t, st, func(n *Net) { n.Tier = TierOpen })
	if _, err := p.Prepare(t.Context(), lifelined(), st, false, ""); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if again, err := os.ReadFile(ca.Cert()); err != nil || !bytes.Equal(again, cert) {
		t.Errorf("the CA changed with the tier: %v", err)
	}
	if _, ok := p.PID(vm); !ok {
		t.Error("no proxy runs")
	}
}

// A running VM keeps the confinement it started with: one made open while
// it ran confined stays on its proxy alone until its next start, and one
// given a confined tier while it ran open gets its proxy's.
func TestPrepareRunning(t *testing.T) {
	for _, tc := range []struct {
		tier     Tier
		confined bool
		want     backend.Net
	}{
		{TierOpen, true, backend.NetProxy},
		{TierOpen, false, backend.NetDirect},
		{TierAllowlist, false, backend.NetProxy},
		{TierNone, true, backend.NetProxy},
	} {
		d, st := sandbox(t, "base")
		p, _ := testProxy(t, d)
		setNet(t, st, func(n *Net) { n.Tier = tc.tier })
		b := lifelined()
		b.Instances[st.Name()] = backend.Instance{Name: st.Name(), Status: backend.StatusRunning}
		b.Unconfined = !tc.confined
		if conf, err := p.Prepare(t.Context(), b, st, true, ""); err != nil || conf.Net != tc.want {
			t.Errorf("%s, confined %v: %v, %v; want %v", tc.tier, tc.confined, conf.Net, msg.NamesOf(err), tc.want)
		}
	}
}

// Each tier's proxy: internet's built-in policy and the user's denies,
// none's agents' policies and its secrets' hosts, each with the CA and
// secrets a confined tier has.
func TestPrepareTiers(t *testing.T) {
	for _, tier := range []Tier{TierInternet, TierNone} {
		d, st := sandbox(t, "base", "node", "claude")
		p, _ := testProxy(t, d)
		setNet(t, st, func(n *Net) { n.Tier = tier })
		if _, err := p.Prepare(t.Context(), lifelined(), st, false, ""); err != nil {
			t.Fatal(msg.NamesOf(err))
		}
		cfg, err := proxy.ReadConfig(d.VM(st.Name()).ProxyConfig())
		want := Policies(d, st.Name(), tier, "base", []string{"node", "claude"})
		if err != nil || cfg.Open || cfg.CA == "" || cfg.Secrets == "" || !slices.Equal(cfg.BuiltinPolicies, want.Builtin) ||
			!slices.Equal(cfg.Policies, want.Files) || !slices.Equal(cfg.DenyPolicies, want.Denies) {
			t.Errorf("%s's proxy: %+v, %v", tier, cfg, err)
		}
	}
}

func TestPrepareCAFails(t *testing.T) {
	d, st := sandbox(t, "base")
	p, _ := testProxy(t, d)
	p.InitCA = func(context.Context, paths.CA) error { return errors.New("disk full") }
	if _, err := p.Prepare(t.Context(), lifelined(), st, false, ""); msg.NamesOf(err) != "proxy_ca_failed dir="+d.VM(st.Name()).CA().Dir {
		t.Errorf("Prepare = %v", msg.NamesOf(err))
	}
}

// A launch with network policy warns of each line of the VM's policy files
// that isn't a rule, a broken deny as one.
func TestPrepareWarnsPolicy(t *testing.T) {
	d, st := sandbox(t, "base")
	p, _ := testProxy(t, d)
	var warned msg.Recorder
	p.Reporter = &msg.Fake{OnWarn: warned.Add}
	f := d.VM(st.Name()).Policy()
	if err := os.WriteFile(f, []byte("ok.test\n! bad.test\na b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Prepare(t.Context(), lifelined(), st, false, ""); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	want := []string{"policy_broken_deny file=" + f + " line=2 rule=! bad.test", "policy_not_rule file=" + f + " line=3 rule=a b"}
	if !slices.Equal(warned.Names(), want) {
		t.Errorf("warned %q; want %q", warned.Names(), want)
	}
}

// Two devvm runs writing a VM's proxy's config at once each write it
// whole: neither fails, the config is one of theirs, and no temporary file
// is left.
func TestWriteConfigAtOnce(t *testing.T) {
	d, st := sandbox(t, "base")
	p, _ := testProxy(t, d)
	run := d.VM(st.Name()).Run()
	if err := os.MkdirAll(run, 0o700); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 200)
	for i := range 4 {
		wg.Go(func() {
			for range 50 {
				errs <- p.writeConfig(st.Name(), proxy.Config{
					Listen: "127.0.0.1:0", Password: redact.NewSecret("k3y"), UpstreamCAs: strings.Repeat("cas "+strconv.Itoa(i), 1000),
					Upstream: &proxy.Upstream{HTTPS: "http://up-" + strconv.Itoa(i)},
				})
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("writeConfig: %v", err)
		}
	}
	es, _ := os.ReadDir(run)
	var left []string
	for _, e := range es {
		left = append(left, e.Name())
	}
	if want := []string{"proxy.json"}; !slices.Equal(left, want) {
		t.Errorf("the run folder holds %q; want %q", left, want)
	}
	cfg, err := proxy.ReadConfig(d.VM(st.Name()).ProxyConfig())
	if err != nil || cfg.Upstream == nil || !regexp.MustCompile(`\Ahttp://up-[0-3]\z`).MatchString(cfg.Upstream.HTTPS) ||
		cfg.UpstreamCAs != strings.Repeat("cas "+strings.TrimPrefix(cfg.Upstream.HTTPS, "http://up-"), 1000) {
		t.Errorf("config: %v; one writer's whole: %v", err, cfg.Upstream)
	}
}

// A proxy file that can't be written fails the start and leaves no
// temporary file behind.
func TestPrepareWriteFails(t *testing.T) {
	d, st := sandbox(t, "base")
	p, _ := testProxy(t, d)
	// A folder where the config goes: renaming over it fails.
	config := d.VM(st.Name()).ProxyConfig()
	if err := os.MkdirAll(filepath.Join(config, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	var le *os.LinkError
	if _, err := p.Prepare(t.Context(), lifelined(), st, false, ""); !errors.As(err, &le) || le.New != config {
		t.Fatalf("Prepare: %v; want the config's rename failing", err)
	}
	es, _ := os.ReadDir(filepath.Dir(config))
	for _, e := range es {
		if strings.Contains(e.Name(), "tmp") {
			t.Errorf("left in the run folder: %s", e.Name())
		}
	}
}

// A change to a rule by method and path warns, naming the VM, while its
// proxy runs from another devvm binary than this one, which may read the
// rule otherwise; a change to a host's rule, the same binary's proxy and a
// stopped one don't, and with no VM every VM's proxy is asked.
func TestOtherBinaries(t *testing.T) {
	d, st := sandbox(t, "base")
	p, f := testProxy(t, d)
	var warned []string
	p.Reporter = &msg.Fake{OnWarn: func(m msg.M) { warned = append(warned, m.Names()) }}
	if _, err := p.Prepare(t.Context(), lifelined(), st, false, ""); err != nil {
		t.Fatal(err)
	}
	narrowed := []string{"!api.example.com POST"}
	p.OtherBinaries(narrowed, st.Name())
	if len(warned) != 0 {
		t.Errorf("the same binary's proxy: %q", warned)
	}
	other := filepath.Join(t.TempDir(), "devvm")
	if err := os.WriteFile(other, []byte("another devvm"), 0o600); err != nil {
		t.Fatal(err)
	}
	p.Self = other
	p.OtherBinaries([]string{"example.com", "!example.com"}, st.Name())
	if len(warned) != 0 {
		t.Errorf("a host's rule: %q", warned)
	}
	want := "proxy_other_binary vm=" + st.Name().String()
	p.OtherBinaries(narrowed, st.Name())
	p.OtherBinaries(narrowed, "")
	if !slices.Equal(warned, []string{want, want}) {
		t.Errorf("another binary's proxy: %q, want %q twice", warned, want)
	}
	// Another VM whose state can't be read takes no VM's warning away.
	broken := d.VM("devvm-y-0123456789abce").State()
	if err := os.MkdirAll(filepath.Dir(broken), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(broken, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	warned = nil
	if p.OtherBinaries(narrowed, ""); !slices.Equal(warned, []string{want}) {
		t.Errorf("beside a state that can't be read: %q, want %q", warned, want)
	}
	if err := os.Remove(broken); err != nil {
		t.Fatal(err)
	}
	pid, _ := p.PID(st.Name())
	delete(f.args, pid)
	warned = nil
	p.OtherBinaries(narrowed, st.Name())
	if len(warned) != 0 {
		t.Errorf("a stopped proxy: %q", warned)
	}
}

// A bindings file that a proxy from another devvm binary may refuse whole,
// one of version 2 for a binding by method or pattern, warns as it's
// written, naming the VM; one of version 1, which such a proxy reads alike,
// doesn't, and neither does the same binary's proxy. A proxy whose config
// can't be read counts as another binary's.
func TestRefreshSecretsOtherBinary(t *testing.T) {
	d, st := sandbox(t, "base")
	p, _ := testProxy(t, d)
	var warned []string
	p.Reporter = &msg.Fake{OnWarn: func(m msg.M) { warned = append(warned, m.Names()) }}
	if _, err := p.Prepare(t.Context(), lifelined(), st, false, ""); err != nil {
		t.Fatal(err)
	}
	file := d.VM(st.Name()).Secrets()
	write := func(bs ...proxy.Binding) {
		t.Helper()
		if err := state.WriteFile(file, proxy.BindingList{Bindings: bs}.Encode()); err != nil {
			t.Fatal(err)
		}
	}
	narrowed := proxy.Binding{Name: "k", Kind: proxy.KindSecret, Hosts: []string{"api.example.com"}, Methods: "GET", Header: "Authorization", Placeholder: "PH"}
	write(narrowed)
	p.SecretsWritten(st.Name())
	if len(warned) != 0 {
		t.Errorf("the same binary's proxy: %q", warned)
	}
	other := filepath.Join(t.TempDir(), "devvm")
	if err := os.WriteFile(other, []byte("another devvm"), 0o600); err != nil {
		t.Fatal(err)
	}
	p.Self = other
	plain := narrowed
	plain.Methods = ""
	write(plain)
	p.SecretsWritten(st.Name())
	if len(warned) != 0 {
		t.Errorf("a file of version 1: %q", warned)
	}
	write(narrowed)
	p.SecretsWritten(st.Name())
	if want := []string{"secrets_other_binary vm=" + st.Name().String()}; !slices.Equal(warned, want) {
		t.Errorf("a file of version 2, another binary's proxy: %q, want %q", warned, want)
	}
	// A config that can't be read: devvm can't tell, so it warns.
	p.Self = "/bin/sh"
	warned = nil
	if err := os.Chmod(d.VM(st.Name()).ProxyConfig(), 0o644); err != nil { //nolint:gosec // G302: a config others may read, which the proxy refuses
		t.Fatal(err)
	}
	p.OtherBinaries([]string{"!api.example.com POST"}, st.Name())
	if want := []string{"proxy_other_binary vm=" + st.Name().String()}; !slices.Equal(warned, want) {
		t.Errorf("a config that can't be read: %q, want %q", warned, want)
	}
}

// NarrowNotes runs once the rule is written, so a VM whose rules can't be
// read is a warning naming it, not a failure, and the host's note follows,
// as devvm can't tell that the allow narrows nothing there.
func TestNarrowNotesUnreadable(t *testing.T) {
	d, st := sandbox(t, "base")
	p, _ := testProxy(t, d)
	var got []string
	p.Reporter = &msg.Fake{OnAny: func(m msg.M) { got = append(got, msg.IDOf(m)) }}
	p.Assets = fstest.MapFS{} // no built-in policy, which reading the allowlist needs
	p.NarrowNotes([]string{"api.github.com GET"}, st, false)
	if want := []string{"rule_narrows_unknown", "rule_narrows"}; !slices.Equal(got, want) {
		t.Errorf("said %q, want %q", got, want)
	}
}

// PID names only a process running as the VM's proxy.
func TestPID(t *testing.T) {
	d, st := sandbox(t, "base")
	p, f := testProxy(t, d)
	if _, err := p.Prepare(t.Context(), lifelined(), st, false, ""); err != nil {
		t.Fatal(err)
	}
	pid, ok := p.PID(st.Name())
	if !ok {
		t.Fatal("no proxy")
	}
	f.args[pid] = []string{"/bin/sh", "_proxy", "-config", d.VM("devvm-other-0123456789abc9").ProxyConfig()}
	if _, ok := p.PID(st.Name()); ok {
		t.Error("another VM's proxy counts")
	}
	f.args[pid] = []string{"/bin/sh", "_proxy", "-vm", st.Name().String()}
	if _, ok := p.PID(st.Name()); ok {
		t.Error("a proxy without the VM's config counts")
	}
	f.args[pid] = []string{"/bin/sleep", "30"}
	if _, ok := p.PID(st.Name()); ok {
		t.Error("another program counts")
	}
	// Stopping it signals nothing: the pid is another program's now.
	var signalled []int
	// Its child, which still runs as the VM's proxy's child, is stopped as it is.
	p.Signal = func(got int, sig syscall.Signal) error {
		if got != pid {
			return daemon.Signal(got, sig)
		}
		signalled = append(signalled, got)
		return nil
	}
	if !p.Stop(st.Name()) || len(signalled) != 0 {
		t.Errorf("Stop of another program's pid: signalled %v", signalled)
	}
	// Nor does a pid file that names no pid, or this test's own.
	for _, pidFile := range []string{"x\n", "0\n", "-1\n", strconv.Itoa(os.Getpid()) + "\n"} {
		if err := os.WriteFile(d.VM(st.Name()).Proxy(".pid"), []byte(pidFile), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, ok := p.PID(st.Name()); ok || !p.Stop(st.Name()) || len(signalled) != 0 {
			t.Errorf("pid file %q: a proxy, or signalled %v", pidFile, signalled)
		}
	}
}

// Stop stops a VM's proxy; its files stay in its folder, which goes
// with the VM.
func TestStopKeepsFiles(t *testing.T) {
	d, st := sandbox(t, "base")
	p, _ := testProxy(t, d)
	if _, err := p.Prepare(t.Context(), lifelined(), st, false, ""); err != nil {
		t.Fatal(err)
	}
	pid, _ := p.PID(st.Name())
	if !p.Stop(st.Name()) || daemon.Alive(pid) {
		t.Error("the proxy still runs")
	}
	if _, err := os.Stat(d.VM(st.Name()).ProxyConfig()); err != nil {
		t.Errorf("its files: %v", err)
	}
}

// Stop stops a child that outlived its broker, found by its pid file and
// its arguments, and removes what the broker said of it; a pid file that
// names another program signals nothing.
func TestStopEndsAnOrphanedChild(t *testing.T) {
	d, st := sandbox(t, "base")
	p, f := testProxy(t, d)
	files := d.VM(st.Name())
	if err := os.MkdirAll(files.Run(), 0o700); err != nil {
		t.Fatal(err)
	}
	child := exec.CommandContext(t.Context(), "sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = child.Wait(); close(done) }()
	t.Cleanup(func() { _ = child.Process.Kill(); <-done })
	pid := child.Process.Pid
	f.mu.Lock()
	f.args[pid] = []string{"/bin/sh", "_proxy-child", "-for", d.VM("devvm-other-0123456789abc9").ProxyConfig()}
	f.mu.Unlock()
	confined := proxy.ConfinedFile(files.ProxyConfig())
	for _, file := range []string{proxy.ChildPIDFile(files.ProxyConfig()), confined} {
		if err := daemon.WritePID(file, pid); err != nil {
			t.Fatal(err)
		}
	}
	var signalled []int
	p.Signal = func(pid int, sig syscall.Signal) error {
		signalled = append(signalled, pid)
		return daemon.Signal(pid, sig)
	}
	if !p.Stop(st.Name()) || len(signalled) != 0 {
		t.Errorf("another VM's child: signalled %v", signalled)
	}
	if err := daemon.WritePID(proxy.ChildPIDFile(files.ProxyConfig()), pid); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.args[pid] = []string{"/bin/sh", "_proxy-child", "-for", files.ProxyConfig()}
	f.mu.Unlock()
	if !p.Stop(st.Name()) || !slices.Equal(signalled, []int{pid}) {
		t.Errorf("the VM's child: signalled %v", signalled)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the child still runs")
	}
	for _, file := range []string{proxy.ChildPIDFile(files.ProxyConfig()), confined} {
		if _, err := os.Stat(file); !os.IsNotExist(err) {
			t.Errorf("%s: %v", filepath.Base(file), err)
		}
	}
}

// RefreshSecrets writes the secret files again only of VMs whose proxy has
// them, so a token saved since gets its binding at once.
func TestRefreshSecrets(t *testing.T) {
	d, st := sandbox(t, "base", "node")
	p, _ := testProxy(t, d)
	ctx := t.Context()
	change(t, st, func(st *state.VM) error { return secrets.Bind(st, "github", true) })
	other := newVM(t, d, "devvm-other-0123456789abc9", "base", "node")
	change(t, other, func(st *state.VM) error { return secrets.Bind(st, "github", true) })
	salt, _ := Salt(ctx, st)
	if err := p.Secrets.WriteFiles(ctx, st, salt); err != nil {
		t.Fatal(err)
	}
	if err := p.Secrets.Save(ctx, "github", redact.NewSecret("ghp_testfake")); err != nil {
		t.Fatal(err)
	}
	// A bindings file whose VM devvm no longer has is left alone.
	if err := state.WriteFile(d.VM("devvm-gone-0123456789abc8").Secrets(), []byte("[]\n")); err != nil {
		t.Fatal(err)
	}
	if err := p.RefreshSecrets(ctx); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(d.VM(st.Name()).Secrets())
	if !strings.Contains(string(data), `"devvm-proxy-managed-github-`+salt+`"`) || strings.Contains(string(data), "ghp_testfake") {
		t.Errorf("bindings = %s", data)
	}
	if _, err := os.Stat(d.VM(other.Name()).Secrets()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a VM without a proxy got files: %v", err)
	}
	if data, _ := os.ReadFile(d.VM("devvm-gone-0123456789abc8").Secrets()); string(data) != "[]\n" {
		t.Errorf("a gone VM's = %s", data)
	}
	if err := p.RefreshSecrets(ctx, other.Name()); err != nil {
		t.Fatal(err)
	}
	// A VM whose files can't be written again (its bindings unreadable)
	// doesn't keep the others' from it: its error comes back after.
	broken := newVM(t, d, "devvm-aaa-0123456789abc7", "base", "node")
	if _, err := Salt(ctx, broken); err != nil {
		t.Fatal(err)
	}
	if err := state.WriteFile(d.VM(broken.Name()).Secrets(), []byte("not json\n")); err != nil {
		t.Fatal(err)
	}
	if err := p.Secrets.Save(ctx, "github", redact.NewSecret("ghp_testfake2")); err != nil {
		t.Fatal(err)
	}
	if err := state.WriteFile(d.VM(st.Name()).Secrets(), []byte(`{"version":1,"bindings":[]}`+"\n")); err != nil {
		t.Fatal(err)
	}
	if err := p.RefreshSecrets(ctx, broken.Name(), st.Name()); err == nil {
		t.Error("the broken VM's error was lost")
	}
	if data, _ := os.ReadFile(d.VM(st.Name()).Secrets()); !strings.Contains(string(data), `"name":"github"`) {
		t.Errorf("the VM after the broken one = %s", data)
	}
}

// The proxy's variables, which every VM's environment has, are ones a
// secret of yours can't take (secrets.CheckEnv).
func TestEnvReservedForSecrets(t *testing.T) {
	for name := range Env {
		if secrets.CheckEnv(name) == nil {
			t.Errorf("a secret of yours may take %s", name)
		}
	}
}

// A running VM whose proxy couldn't start is stopped, and the reason
// returned; a stop that fails is returned instead.
func TestUnproxied(t *testing.T) {
	why := msg.ProxyStartFailed("/log", vmV)
	f := backendtest.New("fake", nil)
	f.Instances[vmV] = backend.Instance{Name: vmV, Status: backend.StatusRunning}
	var warned msg.Recorder
	p := &Proxy{Reporter: &msg.Fake{OnWarn: warned.Add}}
	if err := unproxied(t.Context(), p, f, vmV, why); msg.NamesOf(err) != "proxy_start_failed log=/log vm="+vmV ||
		f.Instances[vmV].Status != backend.StatusStopped {
		t.Errorf("Unproxied = %v, VM %s", msg.NamesOf(err), f.Instances[vmV].Status)
	}
	if !slices.Equal(warned.Names(), []string{"net_proxy_down_stopped vm=" + vmV}) {
		t.Errorf("warned %q", warned.Names())
	}
	// Interrupted (its context cancelled), it still stops the VM.
	f.Instances[vmV] = backend.Instance{Name: vmV, Status: backend.StatusRunning}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	cs := ctxStopper{Fake: f}
	if err := unproxied(ctx, p, &cs, vmV, why); msg.NamesOf(err) != "proxy_start_failed log=/log vm="+vmV || cs.err != nil ||
		f.Instances[vmV].Status != backend.StatusStopped {
		t.Errorf("interrupted: %v, stop's context %v, VM %s", msg.NamesOf(err), cs.err, f.Instances[vmV].Status)
	}
	stopErr := errors.New("stop failed")
	f.Errs = map[string]error{"Stop": stopErr}
	warned.Reset()
	if err := unproxied(t.Context(), p, f, vmV, why); !errors.Is(err, stopErr) ||
		!slices.Equal(warned.Names(), []string{"proxy_start_failed log=/log vm=" + vmV}) {
		t.Errorf("a failed stop: %v, warned %q", err, warned.Names())
	}
}

// refusedRun is a running VM, its proxy started, whose next readying
// TestPrepareRunningRefused's rows change.
type refusedRun struct {
	p  *Proxy
	f  *proxies
	d  paths.Dirs
	st *state.VM
}

// noPassword names a user in config.toml's company proxy, whose password
// isn't saved.
func (r refusedRun) noPassword(t *testing.T) {
	must(t, os.WriteFile(r.d.ConfigFile(), []byte("[proxy]\nhttps = \"http://alice@proxy.corp:3128\"\n"), 0o600))
}

// corp sets a company proxy in devvm's environment, which makes the
// running proxy's config stale.
func (r refusedRun) corp() {
	r.p.Getenv = func(k string) string { return map[string]string{"HTTPS_PROXY": "http://proxy.corp:3128"}[k] }
}

// A running VM whose proxy can't be readied: what refuses its proxy's
// start before the proxy is touched (the company proxy's password, the
// user's CAs, the VM's CA) leaves the VM and its proxy as they were, and
// says so, while that proxy enforces the VM's tier as its state has it;
// one with a looser tier fails open, so the VM is stopped. A failure once
// its proxy was stopped, or tried to be, or while none of its runs,
// stops the VM. Refusal checks only what refuses every VM's proxy.
func TestPrepareRunningRefused(t *testing.T) {
	for _, c := range []struct {
		name       string
		from       Tier // the tier the proxy started with; "": the VM's
		change     func(t *testing.T, r refusedRun)
		want       string // the error's names
		kept       bool   // the VM and its proxy as they were; else the VM stopped
		proxyStays bool   // a stopped VM's proxy runs on (it didn't stop)
		refused    bool   // Refusal refuses too
	}{
		{
			"password missing", "", func(t *testing.T, r refusedRun) { r.noPassword(t) },
			"proxy_password_missing", true, false, true,
		},
		{
			"CAs refused", "", func(t *testing.T, r refusedRun) { must(t, os.Symlink(t.TempDir(), r.d.TrustedCAs())) },
			"ca_folder_link dir=CAS", true, false, true,
		},
		{"the VM's CA can't be readied", "", func(_ *testing.T, r refusedRun) {
			r.p.InitCA = func(context.Context, paths.CA) error { return errors.New("no CA") }
		}, "proxy_ca_failed dir=VMCA", true, false, false},
		{"password missing, the proxy's tier looser than the state's", TierInternet, func(t *testing.T, r refusedRun) {
			change(t, r.st, func(st *state.VM) error { return SetTier(st, TierAllowlist, Learning{}) })
			r.noPassword(t)
		}, "proxy_password_missing", false, false, true},
		{"password missing, no proxy running", "", func(t *testing.T, r refusedRun) {
			r.noPassword(t)
			r.p.Stop(r.st.Name())
		}, "proxy_password_missing", false, false, true},
		{"restarted proxy fails", "", func(_ *testing.T, r refusedRun) {
			r.corp()
			r.f.quiet = true
		}, "proxy_start_failed log=LOG vm=VM", false, false, false},
		{"its port taken once stopped", "", func(_ *testing.T, r refusedRun) {
			r.corp()
			r.p.Listening = func(context.Context, int) bool { return true }
		}, "proxy_port_taken port=PORT vm=VM", false, false, false},
		{"the old proxy doesn't stop", "", func(_ *testing.T, r refusedRun) {
			r.corp()
			r.p.Signal = func(int, syscall.Signal) error { return nil } // signals that never arrive
		}, "proxy_stop_failed vm=VM", false, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			d, st := sandbox(t, "base")
			must(t, os.MkdirAll(d.Config, 0o700))
			p, f := testProxy(t, d)
			vm := st.Name()
			if c.from != "" {
				change(t, st, func(st *state.VM) error { return SetTier(st, c.from, Learning{}) })
			}
			b := lifelined()
			b.Instances[vm] = backend.Instance{Name: vm, Status: backend.StatusRunning}
			if _, err := p.Prepare(t.Context(), b, st, false, ""); err != nil {
				t.Fatal(msg.NamesOf(err))
			}
			before, _ := p.PID(vm)
			var warned msg.Recorder
			p.Reporter = &msg.Fake{OnWarn: warned.Add}
			c.change(t, refusedRun{p: p, f: f, d: d, st: st})
			want := strings.NewReplacer("VMCA", d.VM(vm).CA().Dir, "CAS", d.TrustedCAs(), "LOG", d.VM(vm).ProxyLog(),
				"VM", vm.String(), "PORT", strconv.Itoa(hostPorts(t, st).Proxy)).Replace(c.want)
			if err := p.Refusal(t.Context()); c.refused != (msg.NamesOf(err) == want) || !c.refused && err != nil {
				t.Errorf("Refusal = %v", msg.NamesOf(err))
			}
			_, err := p.PrepareRunning(t.Context(), b, st)
			if msg.NamesOf(err) != want {
				t.Errorf("PrepareRunning = %v, want %s", msg.NamesOf(err), want)
			}
			after, up := p.PID(vm)
			running := b.Instances[vm].Status == backend.StatusRunning
			if !c.kept {
				if running || up != c.proxyStays || !slices.Equal(warned.Names(), []string{"net_proxy_down_stopped vm=" + vm.String()}) {
					t.Errorf("VM running %v, proxy %v, warned %q; want it stopped", running, up, warned.Names())
				}
				return
			}
			if !running || after != before || !slices.Equal(warned.Names(), []string{"net_proxy_kept vm=" + vm.String()}) {
				t.Errorf("VM running %v, proxy %d (was %d), warned %q; want both as they were", running, after, before, warned.Names())
			}
		})
	}
}

// ctxStopper is a fake backend that notes whether Stop's context was done.
type ctxStopper struct {
	*backendtest.Fake
	err error
}

func (c *ctxStopper) Stop(ctx context.Context, vm vmname.VM) error {
	c.err = ctx.Err()
	return c.Fake.Stop(ctx, vm)
}

// After a start: enforced is ok; a direct route, or a VM started outside
// devvm without a terminal (or still after a restart), stops the VM.
func TestSelfCheck(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		unconfined, direct, tty, yes bool
		fixed                        bool // the restart confines it
		want, check                  string
		stops, restarts              bool
	}{
		{name: "enforced", check: "ok"},
		{name: "a direct route", direct: true, want: "net_direct_route vm=" + vmV, check: "unenforced", stops: true},
		{name: "unconfined, no terminal", unconfined: true, want: "net_unenforced_stop vm=" + vmV, check: "unenforced", stops: true},
		{name: "unconfined, restarted", unconfined: true, tty: true, yes: true, fixed: true, check: "ok", stops: true, restarts: true},
		{name: "unconfined, restarted, still", unconfined: true, tty: true, yes: true, want: "net_unenforced_stop vm=" + vmV, check: "unenforced", stops: true, restarts: true},
		{name: "unconfined, declined", unconfined: true, tty: true, check: "unenforced"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := paths.Dirs{Data: t.TempDir()}
			st := newVM(t, d, vmV, "base")
			setNet(t, st, func(n *Net) { n.Tier = TierAllowlist })
			f := backendtest.New("fake", nil)
			f.Instances[vmV] = backend.Instance{Name: vmV, Status: backend.StatusRunning}
			f.Unconfined = tc.unconfined
			f.ExecFunc = func(_ vmname.VM, argv []string, _ backend.ExecOpts) error {
				if tc.direct && slices.Equal(argv, directArgv) {
					return nil
				}
				return errors.New("curl: (7) failed")
			}
			p := &Proxy{Reporter: &msg.Fake{OnConfirm: func(msg.M) bool { return tc.yes }, Terminal: tc.tty}}
			restarted := false
			err := p.SelfCheck(t.Context(), f, st, func(ctx context.Context) error {
				restarted = true
				f.Unconfined = !tc.fixed
				return f.Start(ctx, vmV, backend.Want{}, backend.Confinement{Net: backend.NetProxy})
			})
			if msg.NamesOf(err) != tc.want {
				t.Errorf("SelfCheck = %v, want %q", msg.NamesOf(err), tc.want)
			}
			if got := netOf(t, st).Made.Check; got != tc.check {
				t.Errorf("check = %q", got)
			}
			if got := slices.Contains(f.Methods(), "Stop"); got != tc.stops || restarted != tc.restarts {
				t.Errorf("stopped %v, restarted %v: %q", got, restarted, f.Methods())
			}
			if tc.want != "" && f.Instances[vmV].Status != backend.StatusStopped {
				t.Error("the VM it doesn't hold is left running")
			}
		})
	}
}

// An open VM's self-check asks its proxy through the VM's own settings:
// one that doesn't answer is warned of, and the VM is used as it is.
func TestSelfCheckOpen(t *testing.T) {
	for _, answers := range []bool{true, false} {
		d := paths.Dirs{Data: t.TempDir()}
		st := newVM(t, d, vmV, "base")
		setNet(t, st, func(n *Net) { n.Tier = TierOpen })
		f := backendtest.New("fake", nil)
		f.Instances[vmV] = backend.Instance{Name: vmV, Status: backend.StatusRunning}
		var ran [][]string
		f.ExecFunc = func(_ vmname.VM, argv []string, _ backend.ExecOpts) error {
			ran = append(ran, argv)
			if answers {
				return nil
			}
			return errors.New("curl: (7) failed")
		}
		var warned msg.Recorder
		err := (&Proxy{Reporter: &msg.Fake{OnWarn: warned.Add}}).SelfCheck(t.Context(), f, st, nil)
		want := ""
		if !answers {
			want = "net_proxy_unreachable vm=" + vmV
		}
		if err != nil || strings.Join(warned.Names(), ",") != want || len(ran) != 1 || !slices.Equal(ran[0], proxyArgv) ||
			slices.Contains(f.Methods(), "Stop") {
			t.Errorf("answers %v: %v, warned %q, ran %q, %q", answers, err, warned.Names(), ran, f.Methods())
		}
		if strings.Contains(strings.Join(proxyArgv, " "), "devvm:") {
			t.Error("the password in the check's arguments")
		}
	}
}

// What ls shows: running VMs are checked on the host; a failed self-check
// stands either way.
func TestListed(t *testing.T) {
	for _, tc := range []struct {
		name, net, check    string
		running, unconfined bool
		want                string
		asks                bool
	}{
		{name: "unfinished", want: ""},
		{name: "open", net: "open", running: true, want: "open"},
		{name: "open, failed self-check", net: "open", check: "unenforced", want: "open"},
		{name: "allowlist, stopped", net: "allowlist", want: "allowlist"},
		{name: "allowlist, stopped, unconfined", net: "allowlist", unconfined: true, want: "allowlist"},
		{name: "allowlist, running", net: "allowlist", running: true, want: "allowlist", asks: true},
		{name: "internet, running", net: "internet", running: true, want: "internet", asks: true},
		{name: "none, running elsewhere", net: "none", running: true, unconfined: true, want: "unenforced", asks: true},
		{name: "allowlist, running elsewhere", net: "allowlist", running: true, unconfined: true, want: "unenforced", asks: true},
		{name: "failed self-check", net: "allowlist", check: "unenforced", running: true, want: "unenforced"},
		{name: "failed self-check, stopped", net: "internet", check: "unenforced", want: "unenforced"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := paths.Dirs{Data: t.TempDir()}
			st := newVM(t, d, vmV, "base")
			if tc.net == "" {
				change(t, st, func(st *state.VM) error { st.Created = time.Time{}; return nil })
			}
			setNet(t, st, func(n *Net) { n.Tier, n.Made.Check = Tier(tc.net), tc.check })
			f := backendtest.New("fake", nil)
			f.Unconfined = tc.unconfined
			if tc.running {
				f.Instances[st.Name()] = backend.Instance{Name: st.Name(), Status: backend.StatusRunning}
			}
			got, err := Listed(t.Context(), f, st, tc.running)
			if err != nil || got != tc.want {
				t.Errorf("Listed = %q, %v; want %q", got, err, tc.want)
			}
			if asked := slices.Contains(f.Methods(), "Running"); asked != tc.asks {
				t.Errorf("asked the backend: %v", asked)
			}
		})
	}
}

// Only a VM with the nix layer gets the shared Nix cache, with the cap
// nix-cache.size gives, or its default.
func TestNixCache(t *testing.T) {
	if settings.NixCacheDefault != proxy.NixCacheDefault {
		t.Errorf("the cap's default: settings' %d, the proxy's %d", settings.NixCacheDefault, proxy.NixCacheDefault)
	}
	d, st := sandbox(t, "base")
	var warned msg.Recorder
	p := &Proxy{Dirs: d, Reporter: &msg.Fake{OnWarn: warned.Add}}
	if dir, capped := p.nixCache(st); dir != "" || capped != 0 {
		t.Errorf("base: %q %d", dir, capped)
	}
	change(t, st, func(st *state.VM) error { st.Image = "pg+nix"; return nil })
	if dir, capped := p.nixCache(st); dir != d.NixCache() || capped != settings.NixCacheDefault {
		t.Errorf("pg+nix: %q %d", dir, capped)
	}
	if _, err := settings.Save(d, settings.Values{Retention: map[string]string{settings.RetainNixCacheSize: "2G"}}); err != nil {
		t.Fatal(err)
	}
	if _, capped := p.nixCache(st); capped != 2<<30 {
		t.Errorf("nix-cache.size=2G: %d", capped)
	}
	if err := os.MkdirAll(d.Config, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.ConfigFile(), []byte("[retention]\nnix-cache.size = \"lots\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, capped := p.nixCache(st); capped != settings.NixCacheDefault || len(warned.Names()) != 1 || !strings.HasPrefix(warned.Names()[0], "retention_unreadable") {
		t.Errorf("unreadable: %d, warned %q", capped, warned.Names())
	}
}

// A proxy.confined that a broker which has ended left behind, as a newer
// devvm's broker leaves it when an older devvm starts the VM's proxy
// after a downgrade, never speaks for the proxy running now: the older
// devvm's one process gets proxy_old whatever the file says, whether it
// names another broker, the same pid with another start, or no broker at
// all. The control: the same file naming the running broker is read.
func TestUnconfinedStaleMarker(t *testing.T) {
	d, st := sandbox(t, "base")
	p, f := testProxy(t, d)
	files := d.VM(st.Name())
	pid := runningBroker(t, f, files)
	p.Started = func(got int) (string, error) {
		if got != pid {
			return "", errors.New("no such process")
		}
		return "1700000000.000042", nil
	}
	vm := st.Name().String()
	must(t, os.WriteFile(files.ProxyConfig(), (proxy.Config{
		Version: proxy.ConfigVersion, Listen: "127.0.0.1:1", VM: st.Name(), Password: redact.NewSecret("k3y"), DevvmSHA256: "0.1.0's",
	}).Encode(), 0o600))
	for marker, want := range map[string]string{
		"none unsupported\n": "proxy_old vm=" + vm,
		"full seatbelt\n":    "proxy_old vm=" + vm,
		"full seatbelt\n" + strconv.Itoa(pid+1) + " 1700000000.000042\n":  "proxy_old vm=" + vm,
		"full seatbelt\n" + strconv.Itoa(pid) + " 1600000000.000001\n":    "proxy_old vm=" + vm,
		"none unsupported\n" + strconv.Itoa(pid) + " 1700000000.000042\n": "proxy_unconfined_unsupported vm=" + vm,
	} {
		must(t, os.WriteFile(proxy.ConfinedFile(files.ProxyConfig()), []byte(marker), 0o600))
		var names []string
		for _, m := range Unconfined(p, []*state.VM{st}) {
			names = append(names, msg.NamesOf(m))
		}
		if got := strings.Join(names, "; "); got != want {
			t.Errorf("%q: %q, want %q", marker, got, want)
		}
	}
	// Without the running broker's start, devvm can't vouch for any file, and says so.
	p.Started = func(int) (string, error) { return "", errors.New("no start") }
	if got := Unconfined(p, []*state.VM{st}); len(got) != 1 || msg.NamesOf(got[0]) != "proxy_confinement_unknown vm="+vm+" why=no start" {
		t.Errorf("no start: %v", got)
	}
}

// runningBroker starts a process that the fake procs f take for the VM's
// proxy's broker, with files its VM's files, and returns its pid.
func runningBroker(t *testing.T, f *proxies, files paths.VM) int {
	t.Helper()
	if err := os.MkdirAll(files.Run(), 0o700); err != nil {
		t.Fatal(err)
	}
	broker := exec.CommandContext(t.Context(), "sleep", "30")
	if err := broker.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = broker.Wait(); close(done) }()
	t.Cleanup(func() { _ = broker.Process.Kill(); <-done })
	pid := broker.Process.Pid
	f.mu.Lock()
	f.args[pid] = []string{"/bin/devvm", "_proxy", "-config", files.ProxyConfig()}
	f.mu.Unlock()
	if err := daemon.WritePID(files.Proxy(".pid"), pid); err != nil {
		t.Fatal(err)
	}
	return pid
}

// Unconfined warns of a running VM's proxy whose child isn't fully
// confined, saying why, and says nothing of a confined one, one that
// hasn't said, or one that doesn't run.
func TestUnconfined(t *testing.T) {
	d, st := sandbox(t, "base")
	p, f := testProxy(t, d)
	files := d.VM(st.Name())
	if err := os.MkdirAll(files.Run(), 0o700); err != nil {
		t.Fatal(err)
	}
	vms := []*state.VM{st}
	if got := Unconfined(p, vms); len(got) != 0 {
		t.Errorf("no proxy: %v", got)
	}
	pid := runningBroker(t, f, files)
	p.Started = func(int) (string, error) { return "1700000000.000042", nil }
	id := "\n" + strconv.Itoa(pid) + " 1700000000.000042\n"
	// One that hasn't said: this binary's, still starting, gets none; another binary's, an
	// older devvm's one process, which never says, or one whose config can't be read, gets
	// proxy_old.
	log, vm := files.ProxyLog(), st.Name().String()
	self, err := image.SHA256File(p.Self)
	if err != nil {
		t.Fatal(err)
	}
	for sum, want := range map[string]string{self: "", "0.1.0's": "proxy_old vm=" + vm, "": "proxy_old vm=" + vm} {
		_ = os.Remove(files.ProxyConfig())
		if sum != "" {
			must(t, os.WriteFile(files.ProxyConfig(), (proxy.Config{
				Version: proxy.ConfigVersion, Listen: "127.0.0.1:1", VM: st.Name(), Password: redact.NewSecret("k3y"), DevvmSHA256: sum,
			}).Encode(), 0o600))
		}
		var names []string
		for _, m := range Unconfined(p, vms) {
			names = append(names, m.Names())
		}
		if got := strings.Join(names, "; "); got != want {
			t.Errorf("a proxy that hasn't said, started by %q: %q, want %q", sum, got, want)
		}
	}
	for line, want := range map[string]string{
		"full seatbelt":                 "",
		"none unsupported":              "proxy_unconfined_unsupported vm=" + vm,
		"none sandbox-exec-missing":     "proxy_unconfined_no_sandbox vm=" + vm,
		"none sandbox-exec-failed":      "proxy_unconfined_sandbox_failed log=" + log + " vm=" + vm,
		"none seatbelt check-failed":    "proxy_unconfined_check_failed log=" + log + " vm=" + vm,
		"partial seatbelt check-failed": "proxy_unconfined confinement=partial seatbelt check-failed log=" + log + " vm=" + vm,
		"not one":                       "",
		// Linux: Landlock and the seccomp filter, by what the kernel lacks first.
		"full landlock":                        "",
		"none no-landlock-or-seccomp":          "proxy_unconfined_no_kernel vm=" + vm,
		"partial seccomp no-landlock":          "proxy_unconfined_no_landlock vm=" + vm,
		"partial landlock no-seccomp":          "proxy_unconfined_no_seccomp vm=" + vm,
		"partial landlock landlock-below-abi4": "proxy_unconfined_old_ports vm=" + vm,
		"partial landlock landlock-signals":    "proxy_unconfined_old_signals vm=" + vm,
		"partial landlock landlock-below-abi9": "", // a word no devvm writes now
		"none landlock check-failed":           "proxy_unconfined_check_failed log=" + log + " vm=" + vm,
		"partial landlock check-failed":        "proxy_unconfined confinement=partial landlock check-failed log=" + log + " vm=" + vm,
	} {
		if err := os.WriteFile(proxy.ConfinedFile(files.ProxyConfig()), []byte(line+id), 0o600); err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, m := range Unconfined(p, vms) {
			names = append(names, msg.NamesOf(m))
		}
		if got := strings.Join(names, "; "); got != want {
			t.Errorf("%q: %q, want %q", line, got, want)
		}
	}
}
