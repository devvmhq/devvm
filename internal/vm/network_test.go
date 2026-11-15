package vm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/netpolicy"
	"github.com/devvmhq/devvm/internal/proxy"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/vmname"
)

// setNetwork is devvm network allowlist or devvm network open.
func setNetwork(ctx context.Context, n Net, on bool) error {
	if on {
		return n.SetTier(ctx, netpolicy.TierAllowlist, "devvm network allowlist")
	}
	return n.SetTier(ctx, netpolicy.TierOpen, "devvm network open")
}

// devvm network: shown as devvm ls shows it, then the VM's allowlist;
// allowlist and open change the state only, applied when the VM next
// starts. A running open VM given allowlist isn't confined, so its next
// launch offers the restart; one made open stays on its proxy until it
// starts again.
func TestNetwork(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	f.LifelineFile = filepath.Join(e.Dirs.Lima, "vm", "ha.pid")
	f.ExecFunc = noDirect
	st := existingVM(t, e, Options{Network: "open"})
	vm := st.Name()
	n, l := Net{e}, Launch{e}
	shown := func(want string) {
		t.Helper()
		s, err := n.Network(t.Context())
		// Open has no rules.
		rules := want == "open" && len(s.Rules) == 0 ||
			len(s.Rules) > 0 && s.Rules[0].Path == "layers/base.policy" && s.Rules[0].Builtin && len(s.Rules[0].Rules()) > 0
		if err != nil || s.Policy != want || !rules {
			t.Errorf("Network = %+v, %v; want %s", s, msg.NamesOf(err), want)
		}
	}
	shown("open")
	for range 2 { // the second time it's already on
		if err := setNetwork(t.Context(), n, true); err != nil {
			t.Fatal(msg.NamesOf(err))
		}
	}
	if st, _ := state.ReadVM(e.Dirs, vm); get(st, KeyNet) != "allowlist" || slices.Contains(f.Methods(), "Stop") {
		t.Errorf("net=%q, calls %q", get(st, KeyNet), f.Methods())
	}
	shown(netpolicy.CheckUnenforced)
	// Its next launch, without a terminal to offer the restart in, stops
	// it; the one after starts it confined.
	if err := l.Up(t.Context(), Options{}); msg.NamesOf(err) != "net_unenforced_stop vm="+vm.String() {
		t.Fatalf("Up = %v", msg.NamesOf(err))
	}
	if err := l.Up(t.Context(), Options{}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if c, _ := call(f, "Start").Args[2].(backend.Confinement); c.Net != backend.NetProxy {
		t.Errorf("started %+v", c)
	}
	shown("allowlist")
	// Off while it runs confined: it stays confined until it starts again,
	// its proxy started again as open's.
	if err := setNetwork(t.Context(), n, false); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	before := len(f.Calls())
	if err := l.Up(t.Context(), Options{}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	cfg, err := proxy.ReadConfig(e.Dirs.VM(vm).ProxyConfig())
	if _, ok := e.Proxy.PID(vm); !ok || err != nil || !cfg.Open || slices.Contains(f.Methods()[before:], "Start") {
		t.Errorf("proxy running %v, open %v (%v); calls %q", ok, cfg.Open, err, f.Methods()[before:])
	}
	shown("open")
	if err := l.Restart(t.Context(), Options{}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	// Open: its backend's own network, with its proxy set in it.
	if c, _ := call(f, "Start").Args[2].(backend.Confinement); c.Net != backend.NetDirect || !c.GuestProxy() {
		t.Errorf("restarted %+v", c)
	}
	if _, ok := e.Proxy.PID(vm); !ok {
		t.Error("its proxy doesn't run")
	}
}

// Between confined tiers the change applies at once: a running VM's
// proxy starts again with the new tier's rules, and nothing restarts.
// A lower tier needs the widening check.
func TestSetTierConfined(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	f.LifelineFile = filepath.Join(e.Dirs.Lima, "vm", "ha.pid")
	f.ExecFunc = noDirect
	st := existingVM(t, e, Options{Network: "allowlist"})
	vm, n := st.Name(), Net{e}
	if err := (Launch{e}).Up(t.Context(), Options{}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	pid, _ := e.Proxy.PID(vm)
	before := len(f.Calls())
	var said msg.Recorder
	fake(e).OnInfo = said.Add
	if err := n.SetTier(t.Context(), netpolicy.TierNone, "devvm network none"); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if got, want := said.Names(), []string{"setting_applied vm=" + vm.String()}; !slices.Equal(got, want) {
		t.Errorf("said %q, want %q", got, want)
	}
	again, ok := e.Proxy.PID(vm)
	if !ok || again == pid || slices.Contains(f.Methods()[before:], "Stop") || slices.Contains(f.Methods()[before:], "Start") {
		t.Errorf("proxy %d, was %d; calls %q", again, pid, f.Methods()[before:])
	}
	if cfg, err := proxy.ReadConfig(e.Dirs.VM(vm).ProxyConfig()); err != nil || len(cfg.DenyPolicies) == 0 {
		t.Errorf("none's proxy: %+v, %v", cfg, err)
	}
	if st, _ := state.ReadVM(e.Dirs, vm); get(st, KeyNet) != "none" {
		t.Errorf("net=%q", get(st, KeyNet))
	}
	// Lower, to internet: widening, which a VM in a terminal of its own
	// would be asked; here it's let through.
	if err := n.SetTier(t.Context(), netpolicy.TierInternet, "devvm network internet"); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if st, _ := state.ReadVM(e.Dirs, vm); get(st, KeyNet) != "internet" {
		t.Errorf("net=%q", get(st, KeyNet))
	}
}

// learnedLog writes a learning VM's network log: what its proxy let it
// reach (example.com twice by curl, pastebin.com by HTTP, github.com,
// which its allowlist allows, a private address, and a host reached
// before it learned), and a block.
func learnedLog(t *testing.T, e *Env, vm vmname.VM, since time.Time) {
	t.Helper()
	at := func(d time.Duration) string { return since.Add(d).UTC().Format(time.RFC3339) }
	lines := []string{
		`{"time":"` + at(-time.Hour) + `","event":"tunnel","host":"before.test","port":443}`,
		`{"time":"` + at(0) + `","event":"learn","host":"","port":0}`,
		`{"time":"` + at(time.Second) + `","event":"tunnel","host":"example.com","port":443,"agent":"curl/8.5.0"}`,
		`{"time":"` + at(2*time.Second) + `","event":"intercept","host":"example.com","port":443,"agent":"curl/8.5.0 extra"}`,
		`{"time":"` + at(3*time.Second) + `","event":"forward","host":"pastebin.com","port":80,"method":"POST"}`,
		`{"time":"` + at(4*time.Second) + `","event":"tunnel","host":"github.com","port":443}`,
		`{"time":"` + at(5*time.Second) + `","event":"tunnel","host":"10.0.0.5","port":22}`,
		`{"time":"` + at(6*time.Second) + `","event":"blocked","host":"blocked.test","port":443}`,
	}
	p := e.Dirs.VM(vm).EventLog()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// devvm network learn: the internet tier, learning since now, applied at
// once, and said; again, it's that way already. A learning VM goes to
// the allowlist tier only through devvm network lock, and internet stops
// its learning.
func TestLearn(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	f.LifelineFile = filepath.Join(e.Dirs.Lima, "vm", "ha.pid")
	f.ExecFunc = noDirect
	st := existingVM(t, e, Options{Network: "allowlist"})
	vm, n := st.Name(), Net{e}
	var said msg.Recorder
	fake(e).OnInfo = said.Add
	if err := n.Learn(t.Context(), "devvm network learn"); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if err := n.Learn(t.Context(), "devvm network learn"); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	want := []string{"net_learn_started vm=" + vm.String(), "setting_applied vm=" + vm.String(), "setting_already command=devvm network learn vm=" + vm.String()}
	if got := said.Names(); !slices.Equal(got, want) {
		t.Errorf("said %q, want %q", got, want)
	}
	st, _ = state.ReadVM(e.Dirs, vm)
	if lr := netpolicy.LearningOf(st); get(st, KeyNet) != "internet" || !lr.On() || time.Since(lr.Since) > time.Minute {
		t.Errorf("net=%q, learning %+v", get(st, KeyNet), lr)
	}
	if s, err := n.Network(t.Context()); err != nil || s.Learning.IsZero() {
		t.Errorf("Network = %+v, %v", s, err)
	}
	if err := n.SetTier(t.Context(), netpolicy.TierAllowlist, "devvm network allowlist"); msg.NamesOf(err) != "net_learning_lock vm="+vm.String() {
		t.Errorf("allowlist while learning = %v", msg.NamesOf(err))
	}
	said = msg.Recorder{}
	if err := n.SetTier(t.Context(), netpolicy.TierInternet, "devvm network internet"); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if got := said.Names(); !slices.Equal(got, []string{"net_learn_stopped vm=" + vm.String()}) {
		t.Errorf("said %q", got)
	}
	if st, _ := state.ReadVM(e.Dirs, vm); get(st, KeyNet) != "internet" || netpolicy.LearningOf(st).On() {
		t.Errorf("net=%q, still learning", get(st, KeyNet))
	}
	if err := n.Lock(t.Context(), LockOptions{All: true}); msg.NamesOf(err) != "net_not_learning vm="+vm.String() {
		t.Errorf("Lock when not learning = %v", msg.NamesOf(err))
	}
}

// devvm network lock: the hosts the VM reached since it started learning
// that its allowlist doesn't allow, each once, the risky last; where the
// picker can't show, refused naming --all and --none; in a group,
// refused. --all adds every one to the VM's own rules and gives it the
// allowlist tier at once, its proxy started again; launches remind it
// before.
func TestLock(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	f.LifelineFile = filepath.Join(e.Dirs.Lima, "vm", "ha.pid")
	f.ExecFunc = noDirect
	st := existingVM(t, e, Options{Network: "internet", Learn: true})
	vm, n := st.Name(), Net{e}
	since := netpolicy.LearningOf(st).Since
	if since.IsZero() {
		t.Fatal("--network learn: not learning")
	}
	learnedLog(t, e, vm, since)
	var noted msg.Recorder
	fake(e).OnNote = noted.Add
	if err := (Launch{e}).Up(t.Context(), Options{}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if !slices.ContainsFunc(noted.Names(), func(s string) bool { return strings.HasPrefix(s, "net_learning count=2 since=") }) {
		t.Errorf("noted %q, want the reminder with 2 hosts", noted.Names())
	}
	l, err := n.Learned(t.Context())
	var got []string
	for _, h := range l.Hosts {
		got = append(got, h.Rule+" "+strconv.Itoa(h.Count)+" "+h.Agent+" "+string(h.Risk))
	}
	if want := []string{"example.com:443 2 curl/8.5.0 ", "pastebin.com:80 1  paste"}; err != nil || !slices.Equal(got, want) {
		t.Errorf("Learned = %q, %v; want %q", got, err, want)
	}
	if err := n.Lock(t.Context(), LockOptions{}); msg.NamesOf(err) != "no_input answer=--all or --none" {
		t.Errorf("Lock without a terminal = %v", msg.NamesOf(err))
	}
	// A list that couldn't show: refused, nothing added on its first ticks.
	fake(e).Picks = true
	e.PickMany = func(_ msg.M, parts []PickPart) ([]PickPart, error) { return parts, msg.PickFailed("no tty") }
	if err := n.Lock(t.Context(), LockOptions{}); msg.NamesOf(err) != "pick_failed reason=no tty" {
		t.Errorf("Lock with a list that couldn't show = %v", msg.NamesOf(err))
	}
	// Interrupted while the list was open: nothing added, still learning.
	ctx, cancel := context.WithCancel(t.Context())
	e.PickMany = func(_ msg.M, parts []PickPart) ([]PickPart, error) { cancel(); return parts, nil }
	if err := n.Lock(ctx, LockOptions{}); !errors.Is(err, context.Canceled) {
		t.Errorf("Lock interrupted = %v", err)
	}
	if st, _ := state.ReadVM(e.Dirs, vm); !netpolicy.LearningOf(st).On() {
		t.Error("an interrupted lock stopped its learning")
	}
	// Another devvm changed its tier while the list was open: refused,
	// nothing added, its tier as the other left it.
	e.PickMany = func(_ msg.M, parts []PickPart) ([]PickPart, error) {
		return parts, n.SetTier(t.Context(), netpolicy.TierInternet, "devvm network internet")
	}
	if err := n.Lock(t.Context(), LockOptions{}); msg.NamesOf(err) != "net_lock_changed vm="+vm.String() {
		t.Errorf("Lock after its tier changed = %v", msg.NamesOf(err))
	}
	if _, err := os.Stat(e.Dirs.VM(vm).Policy()); err == nil {
		t.Error("a lock that changed nothing added rules")
	}
	fake(e).Picks, e.PickMany = false, nil
	if err := n.Learn(t.Context(), "devvm network learn"); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	st, _ = state.ReadVM(e.Dirs, vm)
	learnedLog(t, e, vm, netpolicy.LearningOf(st).Since)
	change(t, st, func(st *state.VM) error {
		return edit(st, linksSection, func(l *linksInVM) error { l.Group = "web"; return nil })
	})
	if err := n.Lock(t.Context(), LockOptions{All: true}); msg.NamesOf(err) != "group_tier_fixed group=web vm="+vm.String() {
		t.Errorf("Lock in a group = %v", msg.NamesOf(err))
	}
	change(t, st, func(st *state.VM) error {
		return edit(st, linksSection, func(l *linksInVM) error { l.Group = ""; return nil })
	})
	pid, _ := e.Proxy.PID(vm)
	var said msg.Recorder
	fake(e).OnInfo = said.Add
	if err := n.Lock(t.Context(), LockOptions{All: true}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if got, want := said.Names(), []string{"net_locked count=2 vm=" + vm.String(), "setting_applied vm=" + vm.String()}; !slices.Equal(got, want) {
		t.Errorf("said %q, want %q", got, want)
	}
	if b, _ := os.ReadFile(e.Dirs.VM(vm).Policy()); string(b) != "example.com:443\npastebin.com:80\n" {
		t.Errorf("its policy:\n%s", b)
	}
	st, _ = state.ReadVM(e.Dirs, vm)
	if get(st, KeyNet) != "allowlist" || netpolicy.LearningOf(st).On() {
		t.Errorf("net=%q, learning %v", get(st, KeyNet), netpolicy.LearningOf(st).On())
	}
	if again, ok := e.Proxy.PID(vm); !ok || again == pid {
		t.Errorf("proxy %d, was %d", again, pid)
	}
}

// The lock's picker starts with a host ticked only when it's tickable and
// not risky, after its context, the risky hosts in a part of their own
// under a heading; what's ticked when it's confirmed is what's added.
// Past lockTickMax hosts none starts ticked, and the context says so.
// --none adds nothing.
func TestChooseLearned(t *testing.T) {
	l := netpolicy.LearnedHosts{Since: time.Now(), Hosts: []netpolicy.Learned{
		{Rule: "a.test:443", Count: 3, Agent: "npm/10", Tickable: true},
		{Rule: "b.test:443", Count: 1},
		{Rule: "1.2.3.4:443", Count: 2, Tickable: true, Risk: netpolicy.RiskAddress},
	}}
	var shown []string
	e := &Env{Reporter: &msg.Fake{Picks: true}, PickMany: func(_ msg.M, parts []PickPart) ([]PickPart, error) {
		shown = nil
		for _, p := range parts {
			shown = append(shown, "heading "+p.Heading.ID())
			for i, r := range p.Rows {
				shown = append(shown, r.Name+" "+r.Desc.ID()+" "+strconv.FormatBool(r.On))
				p.Rows[i].On = r.Name != "b.test:443"
			}
		}
		return parts, nil
	}}
	var said msg.Recorder
	e.Reporter.(*msg.Fake).OnSay = said.Add
	got, err := Net{e}.chooseLearned("vm", l, LockOptions{})
	want := []string{
		"heading ", "a.test:443 net_learned_host true", "b.test:443 net_learned_host_anon false",
		"heading net_learned_risky_heading", "1.2.3.4:443 net_learned_host_anon false",
	}
	if err != nil || !slices.Equal(shown, want) || !slices.Equal(got, []string{"a.test:443", "1.2.3.4:443"}) {
		t.Errorf("shown %q, chose %q, %v", shown, got, err)
	}
	if len(said.Names()) != 1 || !strings.HasPrefix(said.Names()[0], "net_lock_context since=") {
		t.Errorf("said %q", said.Names())
	}
	many := netpolicy.LearnedHosts{Since: l.Since}
	for i := range lockTickMax + 1 {
		many.Hosts = append(many.Hosts, netpolicy.Learned{Rule: "h" + strconv.Itoa(i) + ".test:443", Tickable: true})
	}
	said = msg.Recorder{}
	if _, err := (Net{e}).chooseLearned("vm", many, LockOptions{}); err != nil || slices.ContainsFunc(shown, func(s string) bool { return strings.HasSuffix(s, " true") }) ||
		len(said.Names()) != 1 || !strings.HasPrefix(said.Names()[0], "net_lock_context_many count=31 since=") {
		t.Errorf("%d hosts: shown ticked, or said %q, %v", len(many.Hosts), said.Names(), err)
	}
	if got, err := (Net{e}).chooseLearned("vm", l, LockOptions{None: true}); err != nil || len(got) != 0 {
		t.Errorf("--none: %q, %v", got, err)
	}
	// A log that lost its first hosts: warned of, before the list.
	var warned msg.Recorder
	e.Reporter.(*msg.Fake).OnWarn = warned.Add
	l.Partial = true
	if _, err := (Net{e}).chooseLearned("vm", l, LockOptions{}); err != nil || len(warned.Names()) != 1 ||
		!strings.HasPrefix(warned.Names()[0], "net_learned_partial since=") {
		t.Errorf("partial: warned %q, %v", warned.Names(), err)
	}
	// --all takes every host, the risky ones too, unasked.
	if got, err := (Net{e}).chooseLearned("vm", l, LockOptions{All: true}); err != nil || !slices.Equal(got, []string{"a.test:443", "b.test:443", "1.2.3.4:443"}) {
		t.Errorf("--all: %q, %v", got, err)
	}
}

// A backend that can't confine a VM to its proxy refuses devvm network
// on, changing nothing.
func TestNetworkRequiresEgressProxy(t *testing.T) {
	caps := allNative()
	caps[backend.FeatureEgressProxy] = backend.Emulated(false)
	e, _, _ := upEnv(t, caps)
	st := existingVM(t, e, Options{Network: "open"})
	err := setNetwork(t.Context(), Net{e}, true)
	if got := msg.NamesOf(err); got != "backend_unreviewed backend=fake feature=egress-proxy what="+st.Name().String() {
		t.Errorf("Network on = %v", got)
	}
	if st, _ := state.ReadVM(e.Dirs, st.Name()); get(st, KeyNet) != "open" {
		t.Errorf("net=%q", get(st, KeyNet))
	}
}
