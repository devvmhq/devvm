package vm

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/backend/backendtest"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/proxy"
	"github.com/devvmhq/devvm/internal/sshagent"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/vmname"
)

// shellOpts is a recorded Shell call's options.
func shellOpts(c backendtest.Call) backend.ShellOpts {
	o, _ := c.Args[1].(backend.ShellOpts)
	return o
}

// execAgents returns the agent each Exec call from calls on vm carried,
// but for the reads of its host keys, which carry none (vmssh.PinHostKeys
// reads every VM's).
func execAgents(calls []backendtest.Call, vm vmname.VM) []string {
	var out []string
	for _, c := range calls {
		if c.Method != "Exec" || c.Args[0] != vm {
			continue
		}
		// Host keys, and the self-check of the VM's network, which carries
		// no agent.
		if argv, _ := c.Args[1].([]string); strings.Contains(strings.Join(argv, " "), "/etc/ssh/ssh_host_") ||
			strings.Contains(strings.Join(argv, " "), "http://devvm.proxy/") {
			continue
		}
		o, _ := c.Args[2].(backend.ExecOpts)
		out = append(out, o.Agent)
	}
	return out
}

// sameAgent reports whether every agent in got is want, and there's one.
func sameAgent(got []string, want string) bool {
	return len(got) > 0 && !slices.ContainsFunc(got, func(a string) bool { return a != want })
}

// relayed returns the agent vm's proxy relays, as its config has it, ""
// for none, and checks that it relays it on the VM's own socket.
func relayed(t *testing.T, e *Env, vm vmname.VM) string {
	t.Helper()
	cfg, err := proxy.ReadConfig(e.Dirs.VM(vm).ProxyConfig())
	if err != nil {
		t.Fatal(err)
	}
	if want := map[bool]string{true: e.Dirs.AgentRelay(vm)}[cfg.Agent != ""]; cfg.AgentSocket != want {
		t.Errorf("%s's proxy relays its agent on %q, want %q", vm, cfg.AgentSocket, want)
	}
	return cfg.Agent
}

// --ssh-agent: the new VM forwards the user's agent, which is checked
// first, through its proxy's relay, and devvm's connections to it carry
// the relay. Without, they carry none. devvm's own environment stays as
// it was.
func TestUpSSHAgent(t *testing.T) {
	for _, on := range []bool{true, false} {
		e, f, _ := upEnv(t, allNative())
		var adds []string
		e.SSHAdd = func(_ context.Context, args ...string) int { adds = append(adds, strings.Join(args, " ")); return 0 }
		st := existingVM(t, e, Options{Network: "open", SSHAgent: on})
		spec, _ := call(f, "CreateVM").Args[1].(backend.VMSpec)
		mode, sock, agent := sshagent.Off, "", ""
		if on {
			mode, sock, agent = sshagent.On, e.Dirs.AgentRelay(st.Name()), "/user/agent"
		}
		if got := relayed(t, e, st.Name()); got != agent {
			t.Errorf("--ssh-agent %v: its proxy relays %q, want %q", on, got, agent)
		}
		if spec.AgentForwarding != on || get(st, keyAgent) != mode {
			t.Errorf("--ssh-agent %v: forwarding %v, state %q", on, spec.AgentForwarding, get(st, keyAgent))
		}
		if got := shellOpts(call(f, "Shell")).Agent; got != sock {
			t.Errorf("--ssh-agent %v: shell's agent %q, want %q", on, got, sock)
		}
		if got := execAgents(f.Calls(), st.Name()); !sameAgent(got, sock) {
			t.Errorf("--ssh-agent %v: Exec agents %q, want %q", on, got, sock)
		}
		if got := os.Getenv("SSH_AUTH_SOCK"); got != "/user/agent" {
			t.Errorf("--ssh-agent %v: devvm's SSH_AUTH_SOCK=%q", on, got)
		}
		if want := map[bool][]string{true: {"-l"}, false: nil}[on]; !slices.Equal(adds, want) {
			t.Errorf("--ssh-agent %v: ssh-add calls %q", on, adds)
		}
		if c, _ := call(f, "Start").Args[2].(backend.Confinement); c.AgentSocket != sock {
			t.Errorf("--ssh-agent %v: started with agent %q, want %q", on, c.AgentSocket, sock)
		}
	}
}

// Two VMs with different agents in one run, as a group's sync or a
// link's change reaches the others: each start, Exec and shell carries its
// own VM's agent, whichever was found last, and devvm's environment, which
// its later commands and background processes inherit, never changes.
func TestAgentPerVM(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	ctx := t.Context()
	web := e.Dir
	api := filepath.Join(filepath.Dir(web), "api")
	if err := os.MkdirAll(api, 0o700); err != nil {
		t.Fatal(err)
	}
	// web: devvm's agent, from its next start.
	st := existingVM(t, e, Options{Network: "open"})
	if err := set(t, st, keyAgent, sshagent.Devvm); err != nil {
		t.Fatal(err)
	}
	if err := f.Stop(ctx, st.Name()); err != nil {
		t.Fatal(err)
	}
	// api: the user's.
	e.Dir = api
	if err := (Launch{e}).Up(ctx, Options{Network: "open", SSHAgent: true}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	targets := map[string]*target{}
	for _, dir := range []string{web, api} {
		e.Dir = dir
		tg, err := finder{e}.target(ctx, need{has: hasMade})
		if err != nil {
			t.Fatal(msg.NamesOf(err))
		}
		targets[dir] = tg
	}
	for _, dir := range []string{web, api} {
		if err := targets[dir].ensureRunning(ctx); err != nil {
			t.Fatal(msg.NamesOf(err))
		}
	}
	before := len(f.Calls())
	for _, dir := range []string{web, api, web} {
		if err := targets[dir].exec(ctx, []string{"true"}, backend.ExecOpts{}); err != nil {
			t.Fatal(err)
		}
	}
	calls := f.Calls()[before:]
	for dir, agent := range map[string]string{web: e.Agent.Socket(), api: "/user/agent"} {
		vm := targets[dir].f.Name
		if got := relayed(t, e, vm); got != agent {
			t.Errorf("%s's proxy relays %q, want %q", vm, got, agent)
		}
		want := e.Dirs.AgentRelay(vm)
		if got := execAgents(calls, vm); len(got) != map[string]int{web: 2, api: 1}[dir] || !sameAgent(got, want) {
			t.Errorf("%s: Exec agents %q, want %q", vm, got, want)
		}
	}
	if c, _ := call(f, "Start").Args[2].(backend.Confinement); call(f, "Start").Args[0] != targets[web].f.Name || c.AgentSocket != e.Dirs.AgentRelay(targets[web].f.Name) {
		t.Errorf("web started with %+v", c)
	}
	if got := os.Getenv("SSH_AUTH_SOCK"); got != "/user/agent" {
		t.Errorf("devvm's SSH_AUTH_SOCK=%q, want the user's as it started", got)
	}
}

// A VM with network policy whose mode is devvm starts confined with
// devvm's agent too, through its proxy's relay: the proxy's confinement
// keeps it.
func TestOpenAgentNetworkPolicy(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	f.LifelineFile = filepath.Join(e.Dirs.Lima, "vm", "ha.pid")
	f.ExecFunc = noDirect
	e.Agent.Run = (&agentCalls{}).run
	st := existingVM(t, e, Options{Network: "allowlist", SSHAgent: true})
	vm := st.Name()
	if err := set(t, st, keyAgent, sshagent.Devvm); err != nil {
		t.Fatal(err)
	}
	if err := (Lifecycle{e}).Stop(t.Context()); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	before := len(f.Calls())
	if err := (Launch{e}).Up(t.Context(), Options{}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	calls := f.Calls()[before:]
	start := slices.IndexFunc(calls, func(c backendtest.Call) bool { return c.Method == "Start" })
	if start < 0 {
		t.Fatalf("%s didn't start: %v", vm, calls)
	}
	if c, _ := calls[start].Args[2].(backend.Confinement); c.Net != backend.NetProxy || c.AgentSocket != e.Dirs.AgentRelay(vm) || c.ProxyPassword.Empty() || c.NoAgent {
		t.Errorf("started with %+v, want the proxy and agent %q", c, e.Dirs.AgentRelay(vm))
	}
	if got := relayed(t, e, vm); got != e.Agent.Socket() {
		t.Errorf("its proxy relays %q, want %q", got, e.Agent.Socket())
	}
	// Off: the proxy's confinement says no agent too.
	if err := set(t, st, keyAgent, sshagent.Off); err != nil {
		t.Fatal(err)
	}
	if err := (Lifecycle{e}).Stop(t.Context()); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if err := (Launch{e}).Up(t.Context(), Options{}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if c, _ := call(f, "Start").Args[2].(backend.Confinement); c.Net != backend.NetProxy || c.AgentSocket != "" || !c.NoAgent {
		t.Errorf("off: started with %+v", c)
	}
}

// A VM whose mode is devvm starts with devvm's agent, its keys loaded,
// and devvm's connections to it carry that agent; a stopped VM's backend
// forwarding is made to match its mode, a running one's waits for its
// next start. --ssh-agent on an existing VM is ignored.
func TestOpenAgent(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	agent := &agentCalls{}
	e.Agent.Run = agent.run
	st := existingVM(t, e, Options{Network: "open"})
	vm := st.Name()
	if err := set(t, st, keyAgent, sshagent.Devvm); err != nil {
		t.Fatal(err)
	}
	if err := f.Stop(t.Context(), vm); err != nil {
		t.Fatal(err)
	}
	before := len(f.Calls())
	if err := (Launch{e}).Up(t.Context(), Options{SSHAgent: true}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	calls := f.Calls()[before:]
	start := slices.IndexFunc(calls, func(c backendtest.Call) bool { return c.Method == "Start" })
	if start < 0 {
		t.Fatalf("calls = %v", calls)
	}
	// Its start turns forwarding on.
	if w, _ := calls[start].Args[1].(backend.Want); !w.AgentForwarding || !f.Instances[vm].AgentForwarding {
		t.Errorf("started wanting %+v", w)
	}
	relay := e.Dirs.AgentRelay(vm)
	if c, _ := calls[start].Args[2].(backend.Confinement); c.AgentSocket != relay {
		t.Errorf("started with agent %q, want %q", c.AgentSocket, relay)
	}
	if got := relayed(t, e, vm); got != e.Agent.Socket() {
		t.Errorf("its proxy relays %q, want %q", got, e.Agent.Socket())
	}
	if !slices.Contains(agent.calls, "ssh-add -l") {
		t.Errorf("devvm's agent wasn't checked: %q", agent.calls)
	}
	if got := shellOpts(call(f, "Shell")).Agent; got != relay {
		t.Errorf("shell's agent %q", got)
	}
	// Running: its agent is checked again; a change waits.
	agent.calls = nil
	before = len(f.Calls())
	if err := (Launch{e}).Up(t.Context(), Options{}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if !slices.Contains(agent.calls, "ssh-add -l") || slices.Contains(f.Methods()[before:], "Start") {
		t.Errorf("running: agent calls %q, calls %q", agent.calls, f.Methods()[before:])
	}
	if err := set(t, st, keyAgent, sshagent.Off); err != nil {
		t.Fatal(err)
	}
	before = len(f.Calls())
	if err := (Launch{e}).Up(t.Context(), Options{}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if slices.Contains(f.Methods()[before:], "Start") || !f.Instances[vm].AgentForwarding {
		t.Errorf("running, turned off: calls %q", f.Methods()[before:])
	}
	// devvm's connections keep the agent it started with, and so does
	// its proxy.
	if got := execAgents(f.Calls()[before:], vm); !sameAgent(got, relay) {
		t.Errorf("running, turned off: Exec agents %q, want %q", got, relay)
	}
	if got := shellOpts(call(f, "Shell")).Agent; got != relay {
		t.Errorf("running, turned off: shell's agent %q, want %q", got, relay)
	}
	if got := relayed(t, e, vm); got != e.Agent.Socket() {
		t.Errorf("running, turned off: its proxy relays %q, want %q", got, e.Agent.Socket())
	}
	before = len(f.Calls())
	if err := (Launch{e}).Restart(t.Context(), Options{}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if f.Instances[vm].AgentForwarding {
		t.Error("restart didn't turn forwarding off")
	}
	if got := execAgents(f.Calls()[before:], vm); !sameAgent(got, "") {
		t.Errorf("restarted: Exec agents %q, want none", got)
	}
	if got := os.Getenv("SSH_AUTH_SOCK"); got != "/user/agent" {
		t.Errorf("devvm's SSH_AUTH_SOCK=%q", got)
	}
}

// A running VM whose proxy relays no agent: a start devvm recorded as
// relayed, whose proxy lost its config, and a start devvm didn't record,
// such as one outside devvm, get no agent, never the agent's own socket;
// one an older devvm started, forwarding the agent itself, goes on with
// that agent until its next start. Its proxy doesn't take one up.
func TestAgentUnrelayed(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	e.Agent.Run = (&agentCalls{}).run
	st := existingVM(t, e, Options{Network: "open"})
	vm := st.Name()
	if err := set(t, st, keyAgent, sshagent.Devvm); err != nil {
		t.Fatal(err)
	}
	if err := (Launch{e}).Restart(t.Context(), Options{}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	config := e.Dirs.VM(vm).ProxyConfig()
	cfg, err := proxy.ReadConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Agent, cfg.AgentSocket = "", ""
	if err := os.WriteFile(config, cfg.Encode(), 0o600); err != nil {
		t.Fatal(err)
	}
	run, err := f.Running(t.Context(), vm)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		start   string // the start devvm recorded
		relayed bool
		want    string
	}{
		{"relayed, its proxy's config lost", run.ID, true, ""},
		{"an older devvm's start", run.ID, false, e.Agent.Socket()},
		{"a start devvm didn't record", "", false, ""},
	} {
		if err := sshagent.RecordStart(t.Context(), st, sshagent.Devvm, tc.start); err != nil {
			t.Fatal(err)
		}
		if !tc.relayed {
			olderStart(t, st)
		}
		before := len(f.Calls())
		if err := (Launch{e}).Up(t.Context(), Options{}); err != nil {
			t.Fatal(msg.NamesOf(err))
		}
		if got := execAgents(f.Calls()[before:], vm); !sameAgent(got, tc.want) {
			t.Errorf("%s: Exec agents %q, want %q", tc.name, got, tc.want)
		}
		if got := relayed(t, e, vm); got != "" {
			t.Errorf("%s: its proxy relays %q", tc.name, got)
		}
	}
}

// olderStart makes st's recorded start one an older devvm recorded,
// without the relay, as it forwarded the agent itself.
func olderStart(t *testing.T, st *state.VM) {
	t.Helper()
	err := st.Update(t.Context(), func(st *state.VM) error {
		a, err := sshagent.Of(st)
		if err != nil {
			return err
		}
		a.Made.Relayed = false
		return sshagent.Set(st, a)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// With your agent as its choice, a start from an environment without
// SSH_AUTH_SOCK forwards no agent, and a later command from one with it
// never gets your agent's own socket: not devvm's connections, which get
// the relay's socket, where nothing listens, and not its ssh entry.
func TestAgentOnWithoutSocket(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	st := existingVM(t, e, Options{Network: "open"})
	vm := st.Name()
	if err := set(t, st, keyAgent, sshagent.On); err != nil {
		t.Fatal(err)
	}
	if err := f.Stop(t.Context(), vm); err != nil {
		t.Fatal(err)
	}
	user := e.Environ
	e.Environ = slices.DeleteFunc(slices.Clone(user), func(kv string) bool { return strings.HasPrefix(kv, "SSH_AUTH_SOCK=") })
	if err := (Launch{e}).Up(t.Context(), Options{}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if c, _ := call(f, "Start").Args[2].(backend.Confinement); c.AgentSocket != "" {
		t.Errorf("started with agent %q, want none", c.AgentSocket)
	}
	if got := relayed(t, e, vm); got != "" {
		t.Errorf("its proxy relays %q", got)
	}
	e.Environ = user
	before := len(f.Calls())
	if err := (Launch{e}).Up(t.Context(), Options{}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	for _, a := range execAgents(f.Calls()[before:], vm) {
		if a == "/user/agent" {
			t.Errorf("a later command carries your agent's own socket: %q", execAgents(f.Calls()[before:], vm))
			break
		}
	}
	if got := shellOpts(call(f, "Shell")).Agent; got == "/user/agent" {
		t.Errorf("the shell's agent is your agent's own socket")
	}
	conf, err := os.ReadFile(e.Dirs.SSHConfig())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(conf), "ForwardAgent yes") || strings.Contains(string(conf), "/user/agent") {
		t.Errorf("its ssh entry forwards your agent:\n%s", conf)
	}
}

// A running VM runs with the agent choice its start was made with, as
// devvm recorded it; one started some other way, or stopped, with its
// choice.
func TestRunningAgent(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	st := existingVM(t, e, Options{Network: "open", SSHAgent: true})
	vm := st.Name()
	if err := set(t, st, keyAgent, sshagent.Devvm); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		do   func()
		want string
	}{
		{"started by devvm", func() {}, sshagent.On},
		{"started outside devvm", func() { f.Restart(vm) }, sshagent.Devvm},
		{"stopped", func() { _ = f.Stop(t.Context(), vm) }, sshagent.Devvm},
	} {
		c.do()
		if got, err := runningAgent(t.Context(), f, st); err != nil || got != c.want {
			t.Errorf("%s: %q, %v; want %q", c.name, got, err, c.want)
		}
	}
}

// devvm ssh-agent: shown; changed in the state only, from the VM's next
// start. devvm needs its agent running; forwarding needs the backend's
// feature.
func TestSetSSHAgent(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	agent := &agentCalls{}
	e.Agent.Run = agent.run
	s := Settings{e}
	if _, err := s.SSHAgent(t.Context()); msg.NamesOf(err) != "no_vm_ssh_agent dir="+e.Dir {
		t.Errorf("no VM: %s", msg.NamesOf(err))
	}
	st := existingVM(t, e, Options{Network: "open"})
	shown := func(want string) {
		t.Helper()
		if mode, err := s.SSHAgent(t.Context()); err != nil || mode != want {
			t.Errorf("SSHAgent = %q, %v; want %s", mode, msg.NamesOf(err), want)
		}
	}
	shown("off")
	before := len(f.Calls())
	if err := s.SetSSHAgent(t.Context(), sshagent.Devvm); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	shown("devvm")
	if !slices.Contains(agent.calls, "ssh-add -l") {
		t.Errorf("devvm's agent wasn't started: %q", agent.calls)
	}
	if slices.Contains(f.Methods()[before:], "Start") {
		t.Errorf("calls = %q", f.Methods()[before:])
	}
	if err := s.SetSSHAgent(t.Context(), sshagent.Devvm); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if err := s.SetSSHAgent(t.Context(), sshagent.Off); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	shown("off")
	caps := allNative()
	caps[backend.FeatureAgentForwarding] = backend.None
	f.Capabilities = caps
	if err := s.SetSSHAgent(t.Context(), sshagent.On); !strings.HasPrefix(msg.NamesOf(err), "backend_lacks ") {
		t.Errorf("without the feature: %s", msg.NamesOf(err))
	}
	if sshagent.Mode(st) != sshagent.Off {
		t.Errorf("mode = %s", sshagent.Mode(st))
	}
}

// devvm ssh-agent on or devvm warns who can use the keys while you're
// connected.
func TestSetSSHAgentRisk(t *testing.T) {
	e, _, _ := upEnv(t, allNative())
	e.Agent.Run = (&agentCalls{}).run
	var warned msg.Recorder
	fake(e).OnWarn = warned.Add
	st := existingVM(t, e, Options{Network: "open"})
	vm := st.Name()
	for _, mode := range []string{sshagent.On, sshagent.Devvm} {
		if err := (Settings{e}).SetSSHAgent(t.Context(), mode); err != nil {
			t.Fatal(msg.NamesOf(err))
		}
	}
	want := []string{"agent_on_risk vm=" + vm.String(), "agent_devvm_risk vm=" + vm.String()}
	if !slices.Equal(warned.Names(), want) {
		t.Errorf("warned %q, want %q", warned.Names(), want)
	}
}

// A backend that emulates turning forwarding on or off (agent-toggle):
// a new VM is made ready for it only while it's let through (warned when
// unreviewed), and its state says so; each start says whether the VM gets
// no agent; once it isn't let through, that VM no longer starts.
func TestAgentToggleEmulated(t *testing.T) {
	caps := allNative()
	caps[backend.FeatureAgentToggle] = backend.Emulated(false)
	e, f, _ := upEnv(t, caps)
	var warned msg.Recorder
	e.Registry.Reporter = &msg.Fake{OnWarn: warned.Add}
	e.Registry.Unreviewed = map[string]bool{"fake:agent-toggle": true}
	st := existingVM(t, e, Options{Network: "open"})
	vm := st.Name()
	if spec, _ := call(f, "CreateVM").Args[1].(backend.VMSpec); !spec.AgentToggle || spec.AgentForwarding {
		t.Errorf("spec = %+v", spec)
	}
	if !slices.Contains(warned.Names(), "backend_unreviewed_used backend=fake feature=agent-toggle what="+vm.String()) || get(st, KeyAgentToggle) != "on" {
		t.Errorf("warned %q, state %q", warned.Names(), get(st, KeyAgentToggle))
	}
	if c, _ := call(f, "Start").Args[2].(backend.Confinement); !c.NoAgent || c.AgentSocket != "" {
		t.Errorf("off: started with %+v", c)
	}
	// devvm's own, from its next start.
	if err := (Settings{e}).SetSSHAgent(t.Context(), sshagent.Devvm); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if err := (Launch{e}).Restart(t.Context(), Options{}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if c, _ := call(f, "Start").Args[2].(backend.Confinement); c.NoAgent || c.AgentSocket != e.Dirs.AgentRelay(vm) || !f.Instances[vm].AgentForwarding {
		t.Errorf("devvm: started with %+v", c)
	}
	// No longer let through: it doesn't start.
	e.Registry.Unreviewed = nil
	if err := f.Stop(t.Context(), vm); err != nil {
		t.Fatal(err)
	}
	if err := (Launch{e}).Up(t.Context(), Options{}); msg.NamesOf(err) != "backend_unreviewed backend=fake feature=agent-toggle what="+vm.String() {
		t.Errorf("refused: %s", msg.NamesOf(err))
	}
}

// Without agent-toggle let through, a new VM isn't made ready for it.
func TestAgentToggleRefused(t *testing.T) {
	caps := allNative()
	caps[backend.FeatureAgentToggle] = backend.Emulated(false)
	e, f, _ := upEnv(t, caps)
	st := existingVM(t, e, Options{Network: "open"})
	if spec, _ := call(f, "CreateVM").Args[1].(backend.VMSpec); spec.AgentToggle || get(st, KeyAgentToggle) != "" {
		t.Errorf("spec = %+v, state %q", spec, get(st, KeyAgentToggle))
	}
	if err := (Settings{e}).SetSSHAgent(t.Context(), sshagent.On); msg.NamesOf(err) != "backend_unreviewed backend=fake feature=agent-toggle what="+st.Name().String() {
		t.Errorf("devvm ssh-agent on: %s", msg.NamesOf(err))
	}
}

// checkAgent offers to load a default key only into an agent that runs
// with none; without a terminal it says how.
func TestCheckAgent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rc    int    // ssh-add -l's
		key   string // in ~/.ssh
		os    string
		calls []string
	}{
		{name: "keys loaded", rc: 0, key: "id_ed25519", os: "darwin", calls: []string{"-l"}},
		{name: "no default key", rc: 1, os: "darwin", calls: []string{"-l"}},
		{name: "no agent", rc: 2, key: "id_rsa", os: "darwin", calls: []string{"-l"}},
		{name: "empty, macOS", rc: 1, key: "id_ecdsa", os: "darwin", calls: []string{"-l"}},
		{name: "empty, Linux", rc: 1, key: "id_ed25519", os: "linux", calls: []string{"-l"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			if tc.key != "" {
				if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(home, ".ssh", tc.key), []byte("k"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var calls []string
			e := &Env{Dirs: paths.Dirs{Home: home}, OS: tc.os, SSHAdd: func(_ context.Context, args ...string) int {
				calls = append(calls, strings.Join(args, " "))
				return tc.rc
			}}
			var said strings.Builder
			e.Reporter = &msg.Fake{
				OnWarn:   func(m msg.M) { said.WriteString("devvm: " + m.Names() + "\n") },
				Terminal: true,
			}
			Launch{e}.checkAgent(t.Context())
			out := said.String()
			if !slices.Equal(calls, tc.calls) {
				t.Errorf("ssh-add calls = %q, want %q", calls, tc.calls)
			}
			want := map[string]string{
				"keys loaded":    "",
				"no default key": "",
				"no agent":       "devvm: no_ssh_agent\n",
				"empty, macOS": "devvm: ssh_agent_empty\ndevvm: load_key_with cmd=ssh-add --apple-use-keychain " +
					filepath.Join(home, ".ssh", "id_ecdsa") + "\n",
				"empty, Linux": "devvm: ssh_agent_empty\ndevvm: load_key_with cmd=ssh-add " + filepath.Join(home, ".ssh", "id_ed25519") + "\n",
			}[tc.name]
			if out != want {
				t.Errorf("stderr = %q, want %q", out, want)
			}
		})
	}
}
