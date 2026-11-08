package vm

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/gitprotect"
	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/netpolicy"
	"github.com/devvmhq/devvm/internal/settings"
	"github.com/devvmhq/devvm/internal/sshagent"
	"github.com/devvmhq/devvm/internal/state"
)

// With network policy: a running VM isn't started again and keeps its
// proxy; the self-check runs again on every launch. A stopped one is
// started confined.
func TestOpenNetworkPolicy(t *testing.T) {
	e, f, execs := upEnv(t, allNative())
	l := Launch{e}
	f.LifelineFile = filepath.Join(e.Dirs.Lima, "vm", "ha.pid")
	f.ExecFunc = noDirect
	st := existingVM(t, e, Options{Network: "allowlist"})
	vm := st.Name()
	pid, ok := e.Proxy.PID(vm)
	if !ok {
		t.Fatal("no proxy")
	}
	before := len(f.Calls())
	if err := l.Up(t.Context(), Options{}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	ms := f.Methods()[before:]
	if slices.Contains(ms, "Start") || !slices.Contains(ms, "Running") {
		t.Errorf("calls = %q", ms)
	}
	if again, _ := e.Proxy.PID(vm); again != pid {
		t.Errorf("proxy %d, was %d", again, pid)
	}
	if err := f.Stop(t.Context(), vm); err != nil {
		t.Fatal(err)
	}
	before = len(f.Calls())
	if err := l.Up(t.Context(), Options{}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if c, _ := call(f, "Start").Args[2].(backend.Confinement); !slices.Contains(f.Methods()[before:], "Start") || c.Net != backend.NetProxy {
		t.Errorf("calls = %q, confinement %+v", f.Methods()[before:], c)
	}
	// Started outside devvm: no terminal to ask in, so it's stopped.
	f.Unconfined = true
	if err := l.Up(t.Context(), Options{}); msg.NamesOf(err) != "net_unenforced_stop vm="+vm {
		t.Errorf("Up unconfined = %v", msg.NamesOf(err))
	}
	if f.Instances[vm].Status != backend.StatusStopped || len(*execs) != 3 {
		t.Errorf("status %s, execs %d", f.Instances[vm].Status, len(*execs))
	}
	st, _ = state.ReadVM(e.Dirs, vm)
	if get(st, keyCheck) != netpolicy.CheckUnenforced {
		t.Errorf("net_check = %q", get(st, keyCheck))
	}
}

// --tools, --agents and --layers change nothing on a VM that exists: it
// opens as it is, with a warning naming devvm tools or devvm layer add.
func TestPartsFlagsIgnored(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	l := Launch{e}
	st := existingVM(t, e, Options{Toolchains: &[]string{"go"}, Network: "open"})
	vm := st.Name()
	for o, want := range map[*Options]string{
		{Agents: &[]string{"claude"}}: "command=devvm tools flags=--agents",
		{Toolchains: &[]string{}}:     "command=devvm tools flags=--tools",
		{Layers: &[]string{"docker"}}: "command=devvm layer add flags=--layers",
	} {
		var warned msg.Recorder
		fake(e).OnWarn = warned.Add
		before := len(f.Calls())
		if err := l.Up(t.Context(), *o); err != nil {
			t.Fatalf("Up %+v = %v", o, msg.NamesOf(err))
		}
		if !slices.Equal(warned.Names(), []string{"new_vm_flags_ignored_run " + want + " vm=" + vm}) {
			t.Errorf("Up %+v warned %q", o, warned.Names())
		}
		if ms := f.Methods()[before:]; slices.Contains(ms, "BuildTools") || slices.Contains(ms, "BuildImage") {
			t.Errorf("Up %+v calls %q", o, ms)
		}
	}
	if st, _ = state.ReadVM(e.Dirs, vm); st.Image != "base" || get(st, keyToolNames) != "go" {
		t.Errorf("image %s, tool_names %s", st.Image, get(st, keyToolNames))
	}
}

// The flags only a new VM takes group by the command that changes the
// same on a VM that exists, naming only those given; --from naming what
// the VM was made from isn't one.
func TestIgnoredFlags(t *testing.T) {
	for _, c := range []struct {
		name string
		o    Options
		want []string // command: flags
	}{
		{"none", Options{}, nil},
		{"--from the same", Options{From: "debian:12"}, nil},
		{"--from another, --no-devcontainer", Options{From: "alpine", NoDevcontainer: true}, []string{": --from --no-devcontainer"}},
		{"--memory and --disk", Options{Size: Size{Memory: "8", Disk: "50"}}, []string{"devvm size: --memory --disk"}},
		{
			"every other",
			Options{
				Toolchains: &[]string{}, Layers: &[]string{}, Network: "allowlist", NoGitProtect: true,
				Size: Size{CPUs: "2"}, Ports: "8000", SSHAgent: true,
			},
			[]string{
				"devvm tools: --tools", "devvm layer add: --layers", "devvm network allowlist: --network",
				"devvm git-protect off: --no-git-protect", "devvm size: --cpus", "devvm ports LIST: --ports", "devvm ssh-agent on: --ssh-agent",
			},
		},
		{"--network open", Options{Network: "open"}, []string{"devvm network open: --network"}},
	} {
		var got []string
		for _, g := range ignoredFlags(c.o, "debian:12") {
			got = append(got, g.command+": "+strings.Join(g.flags, " "))
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// devvm tools add: the tools the share lacks built, then in the VM's
// list; a new agent's config folders mounted at once on a stopped VM,
// after asking on a running one; those it has said and left.
func TestToolsAdd(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	tl := Tools{e}
	st := existingVM(t, e, Options{Toolchains: &[]string{"go"}, Network: "open"})
	vm := st.Name()
	skills := filepath.Join(agentStore(e.Dirs, "claude"), ".claude", "skills")
	if err := os.MkdirAll(skills, 0o700); err != nil {
		t.Fatal(err)
	}
	var said msg.Recorder
	fake(e).OnInfo = said.Add
	if err := f.Stop(t.Context(), vm); err != nil {
		t.Fatal(err)
	}
	before := len(f.Calls())
	if err := tl.Add(t.Context(), Options{}, []string{"go", "claude", "claude"}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	st, _ = state.ReadVM(e.Dirs, vm)
	if st.Image != "base" || get(st, keyToolNames) != "go claude" || !strings.Contains(get(st, KeyTools), " claude-") {
		t.Errorf("state image=%s tool_names=%s tools=%s", st.Image, get(st, keyToolNames), get(st, KeyTools))
	}
	ms := f.Methods()[before:]
	start := slices.Index(ms, "Start")
	if !slices.Contains(ms, "BuildTools") || start < 0 {
		t.Fatalf("calls = %q", ms)
	}
	// Its start mounts the new agent's folders.
	if w, _ := f.Calls()[before+start].Args[1].(backend.Want); !slices.Equal(w.AgentDirs, []string{skills}) {
		t.Errorf("started wanting %+v", w)
	}
	if dirs := f.Instances[vm].AgentDirs; !slices.Equal(dirs, []string{skills}) {
		t.Errorf("agent dirs = %q", dirs)
	}
	if !slices.Contains(said.Names(), "has_tools tools=go vm="+vm) || !slices.Contains(said.Names(), "vm_tools_now tools=go claude vm="+vm) {
		t.Errorf("said %q", said.Names())
	}

	// Running: a new agent's folders after a restart it asks for.
	prompts := filepath.Join(agentStore(e.Dirs, "codex"), ".codex", "prompts")
	if err := os.MkdirAll(prompts, 0o700); err != nil {
		t.Fatal(err)
	}
	asked := answers(t, e, true)
	before = len(f.Calls())
	if err := tl.Add(t.Context(), Options{}, []string{"codex"}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if st, _ = state.ReadVM(e.Dirs, vm); get(st, keyToolNames) != "go claude codex" {
		t.Errorf("tool_names = %s", get(st, keyToolNames))
	}
	ms = f.Methods()[before:]
	if !slices.Equal(*asked, []string{"restart_now count=1 vm=" + vm}) || !slices.Contains(ms, "Stop") ||
		!slices.Contains(f.Instances[vm].AgentDirs, prompts) {
		t.Errorf("asked %q, calls %q, agent dirs %q", *asked, ms, f.Instances[vm].AgentDirs)
	}
}

// Adding tools to a VM on a container image: gnu tools build in a clone of devvm's
// own Ubuntu base, musl ones in the VM's base, keyed by its digest.
func TestToolsAddFrom(t *testing.T) {
	for _, tc := range []struct{ libc, from string }{
		{"gnu", "debian:12@sha256:" + strings.Repeat("d", 64)},
		{"musl", "alpine:3@sha256:" + strings.Repeat("a", 64)},
	} {
		t.Run(tc.libc, func(t *testing.T) {
			e, f, _ := upEnv(t, allNative())
			f.Built = backend.Built{Libc: tc.libc}
			existingVM(t, e, Options{From: tc.from, Toolchains: &[]string{}, Agents: &[]string{}, Network: "open"})
			if err := (Tools{e}).Add(t.Context(), Options{}, []string{"go"}); err != nil {
				t.Fatal(msg.NamesOf(err))
			}
			own, err := e.Builder(f, "").Key(image.Base)
			if err != nil {
				t.Fatal(err)
			}
			theirs, err := e.Builder(f, tc.from).Key(image.Base)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]string{"gnu": own, "musl": theirs}[tc.libc]
			if spec, _ := call(f, "BuildTools").Args[0].(backend.ToolsSpec); spec.Base != want {
				t.Errorf("built in %s, want %s", spec.Base, want)
			}
		})
	}
}

// devvm tools in a terminal: the picker shows the VM's toolchains and
// agents checked and its layers fixed, as they are; what's checked and
// unchecked becomes devvm tools add and rm, then the VM opens. Nothing
// changed opens nothing.
func TestToolsPick(t *testing.T) {
	e, f, execs := upEnv(t, allNative())
	tl := Tools{e}
	st := existingVM(t, e, Options{Toolchains: &[]string{"go"}, Agents: &[]string{"claude"}, Layers: &[]string{"nix"}, Network: "open"})
	vm := st.Name()
	var shown []PickPart
	pick := func(check, uncheck string) {
		e.PickParts = func(help msg.M, parts []PickPart) []PickPart {
			if help.ID() != "pick_vm_help" {
				t.Errorf("asked %s", help.Names())
			}
			shown = parts
			out := make([]PickPart, len(parts))
			for i, p := range parts {
				out[i] = PickPart{Heading: p.Heading, Rows: slices.Clone(p.Rows)}
				for j, r := range out[i].Rows {
					if r.Name == check {
						out[i].Rows[j].On = true
					}
					if r.Name == uncheck {
						out[i].Rows[j].On = false
					}
				}
			}
			return out
		}
	}
	pick("", "")
	before := len(*execs)
	var said msg.Recorder
	fake(e).OnInfo = said.Add
	if err := tl.Pick(t.Context(), Options{}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if len(*execs) != before || !slices.Equal(said.Names(), []string{"nothing_changed"}) {
		t.Errorf("unchanged: execs %d, said %q", len(*execs)-before, said.Names())
	}
	if shown[2].Heading.ID() != "pick_layers_fixed" {
		t.Errorf("layers heading %s", shown[2].Heading.ID())
	}
	for _, r := range shown[2].Rows {
		if !r.Fixed || r.On != (r.Name == "nix") {
			t.Errorf("layer row %+v", r)
		}
	}
	if !shown[0].Rows[3].On || shown[0].Rows[3].Name != "go" || !shown[1].Rows[0].On || shown[1].Rows[0].Fixed {
		t.Errorf("rows %+v", shown[:2])
	}
	pick("python", "claude")
	n := len(f.Calls())
	if err := tl.Pick(t.Context(), Options{}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if st, _ = state.ReadVM(e.Dirs, vm); get(st, keyToolNames) != "python go" || strings.Contains(get(st, KeyTools), "claude") {
		t.Errorf("tool_names=%s tools=%s", get(st, keyToolNames), get(st, KeyTools))
	}
	if ms := f.Methods()[n:]; !slices.Contains(ms, "BuildTools") || len(*execs) != before+1 {
		t.Errorf("calls %q, execs %d", ms, len(*execs)-before)
	}
}

// A new VM's default agents: set, refused with a did-you-mean, and asked
// for from the saved ones.
func TestAgentsChoice(t *testing.T) {
	e, _, _ := upEnv(t, allNative())
	var said msg.Recorder
	c := AgentsChoice{Dirs: e.Dirs, Reporter: &msg.Fake{OnInfo: said.Add}}
	if err := c.Set([]string{"codex", "claude"}); err != nil {
		t.Fatal(err)
	}
	if def, _ := settings.Read(e.Dirs); !slices.Equal(def.Agents.Names, []string{"claude", "codex"}) {
		t.Errorf("saved %q", def.Agents.Names)
	}
	for given, want := range map[string]string{
		"claud":  "unknown_part_like flag=--agents given=claud like=claude",
		"python": "unknown_part flag=--agents given=python names=" + strings.Join(image.Agents, " "),
	} {
		if err := c.Set([]string{given}); msg.NamesOf(err) != want {
			t.Errorf("Set(%s) = %v", given, msg.NamesOf(err))
		}
	}
	c.Pick = func(help msg.M, parts []PickPart) []PickPart {
		if help.ID() != "agents_help" || !parts[0].Rows[0].On || !parts[0].Rows[1].On || parts[0].Rows[2].On {
			t.Errorf("asked %s with %+v", help.Names(), parts)
		}
		parts[0].Rows[0].On, parts[0].Rows[1].On = false, false
		return parts
	}
	if err := c.Ask(); err != nil {
		t.Fatal(err)
	}
	if def, _ := settings.Read(e.Dirs); !def.Agents.Chosen || len(def.Agents.Names) != 0 {
		t.Errorf("saved %+v", def.Agents)
	}
	if !slices.Equal(said.Names(), []string{"default_agents agents=claude codex", "default_agents_none"}) {
		t.Errorf("said %q", said.Names())
	}
	// config.toml's agents: Set is refused, Ask says so and asks nothing.
	if err := os.MkdirAll(e.Dirs.Config, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.Dirs.ConfigFile(), []byte("agents = [\"codex\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.Set([]string{"claude"}); msg.NamesOf(err) != "set_in_config file="+e.Dirs.ConfigFile()+" key=agents" {
		t.Errorf("Set with config.toml's: %v", msg.NamesOf(err))
	}
	c.Pick = func(msg.M, []PickPart) []PickPart { t.Error("asked"); return nil }
	if err := c.Ask(); err != nil {
		t.Fatal(err)
	}
	if got := said.Names()[len(said.Names())-1]; got != "setup_in_config file="+e.Dirs.ConfigFile()+" key=agents" {
		t.Errorf("said %q", got)
	}
}

// New VMs' default layers: set bottom first as given, refused with a
// did-you-mean, and asked for from the saved ones, each row saying what
// its layer is; config.toml's are refused and not asked for.
func TestLayersChoice(t *testing.T) {
	e, _, _ := upEnv(t, allNative())
	var said msg.Recorder
	c := LayersChoice{Dirs: e.Dirs, Layers: []string{"desktop", "docker", "mine", "nix"}, Reporter: &msg.Fake{OnInfo: said.Add}}
	if err := c.Set([]string{"nix", "docker", "nix"}); err != nil {
		t.Fatal(err)
	}
	if def, _ := settings.Read(e.Dirs); !slices.Equal(def.Layers.Names, []string{"nix", "docker"}) {
		t.Errorf("saved %q", def.Layers.Names)
	}
	for given, want := range map[string]string{
		"dokcer": "unknown_part_like flag=--layers given=dokcer like=docker",
		"zzz":    "unknown_part flag=--layers given=zzz names=desktop docker mine nix",
	} {
		if err := c.Set([]string{given}); msg.NamesOf(err) != want {
			t.Errorf("Set(%s) = %v", given, msg.NamesOf(err))
		}
	}
	c.Pick = func(help msg.M, parts []PickPart) []PickPart {
		var on, descs []string
		for _, r := range parts[0].Rows {
			descs = append(descs, r.Desc.Names())
			if r.On {
				on = append(on, r.Name)
			}
		}
		if help.ID() != "layers_help" || !slices.Equal(on, []string{"docker", "nix"}) ||
			!slices.Equal(descs, []string{"pick_layer_desktop", "pick_layer_docker", "pick_layer_yours", "pick_layer_nix"}) {
			t.Errorf("asked %s with %+v", help.Names(), parts)
		}
		parts[0].Rows[0].On, parts[0].Rows[1].On = true, false // desktop, not docker
		return parts
	}
	if err := c.Ask(); err != nil {
		t.Fatal(err)
	}
	if def, _ := settings.Read(e.Dirs); !slices.Equal(def.Layers.Names, []string{"nix", "desktop"}) {
		t.Errorf("saved %+v", def.Layers)
	}
	if err := c.Set(nil); err != nil {
		t.Fatal(err)
	}
	if def, _ := settings.Read(e.Dirs); !def.Layers.Chosen || len(def.Layers.Names) != 0 {
		t.Errorf("saved %+v", def.Layers)
	}
	if want := []string{"default_layers layers=nix docker", "default_layers layers=nix desktop", "default_layers_none"}; !slices.Equal(said.Names(), want) {
		t.Errorf("said %q, want %q", said.Names(), want)
	}
	if err := os.MkdirAll(e.Dirs.Config, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.Dirs.ConfigFile(), []byte("layers = [\"nix\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.Set([]string{"docker"}); msg.NamesOf(err) != "set_in_config file="+e.Dirs.ConfigFile()+" key=layers" {
		t.Errorf("Set with config.toml's: %v", msg.NamesOf(err))
	}
	c.Pick = func(msg.M, []PickPart) []PickPart { t.Error("asked"); return nil }
	if err := c.Ask(); err != nil {
		t.Fatal(err)
	}
	if got := said.Names()[len(said.Names())-1]; got != "setup_in_config file="+e.Dirs.ConfigFile()+" key=layers" {
		t.Errorf("said %q", got)
	}
}

// tools rm and update open the VM after changing its tools; restart stops
// it first.
func TestRemoveUpdateRestart(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	l := Launch{e}
	tl := Tools{e}
	st := existingVM(t, e, Options{Toolchains: &[]string{"go"}, Agents: &[]string{"claude"}, Network: "open"})
	vm := st.Name()
	if err := tl.Remove(t.Context(), Options{}, []string{"claude"}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	st, _ = state.ReadVM(e.Dirs, vm)
	if get(st, keyToolNames) != "go" || strings.Contains(get(st, KeyTools), "claude") {
		t.Errorf("state tool_names=%s tools=%s", get(st, keyToolNames), get(st, KeyTools))
	}
	if err := tl.Remove(t.Context(), Options{}, []string{"python"}); msg.NamesOf(err) != "vm_lacks has=go tool=python vm="+vm {
		t.Errorf("Remove python = %v", msg.NamesOf(err))
	}
	if err := tl.Update(t.Context(), Options{}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if ts, err := tl.List(t.Context()); err != nil || len(ts) != 1 || ts[0].Tool != "go" || ts[0].Newer != "" {
		t.Errorf("List = %+v, %v", ts, err)
	}
	// go added again after claude, so its file is listed after claude's:
	// still the newest.
	if err := tl.Add(t.Context(), Options{}, []string{"claude"}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if err := tl.Remove(t.Context(), Options{}, []string{"go"}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if err := tl.Add(t.Context(), Options{}, []string{"go"}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	st, _ = state.ReadVM(e.Dirs, vm)
	had := get(st, KeyTools)
	if err := tl.Update(t.Context(), Options{}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if st, _ = state.ReadVM(e.Dirs, vm); !strings.HasPrefix(had, "claude-") || get(st, KeyTools) != had {
		t.Errorf("Update after adding: tools %q, were %q", get(st, KeyTools), had)
	}
	before := len(f.Calls())
	if err := l.Restart(t.Context(), Options{}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	ms := f.Methods()[before:]
	if i, j := slices.Index(ms, "Stop"), slices.Index(ms, "Start"); i < 0 || j < i {
		t.Errorf("restart calls = %q", ms)
	}
}

// A setting that a running VM gets only when it next starts offers to
// restart it: yes restarts it as devvm restart does (stopped, started, a
// shell opened), and the setting applies; no, and a question refused (no
// terminal, --no-input), each say when it applies.
func TestRestartToApply(t *testing.T) {
	for _, c := range []struct {
		cmd     string
		set     func(e *Env) error
		later   string // its message's names, before vm=
		applied func(st *state.VM) bool
	}{
		{
			"devvm size --cpus 2", func(e *Env) error { return Settings{e}.SetSize(t.Context(), Size{CPUs: "2"}, false) },
			"next_start count=1", nil,
		},
		{
			"devvm network allowlist", func(e *Env) error {
				return Net{e}.SetTier(t.Context(), netpolicy.TierAllowlist, "devvm network allowlist")
			},
			"next_start count=1", nil,
		},
		{
			"devvm network open", func(e *Env) error { return Net{e}.SetTier(t.Context(), netpolicy.TierOpen, "devvm network open") },
			"next_start count=1", nil,
		},
		{
			"devvm ssh-agent on", func(e *Env) error { return Settings{e}.SetSSHAgent(t.Context(), sshagent.On) },
			"next_start count=1", nil,
		},
		{
			"devvm git-protect strict", func(e *Env) error { return Settings{e}.SetGitProtect(t.Context(), gitprotect.Strict) },
			"git_protect_next_stop mode=strict",
			func(st *state.VM) bool { return gitprotect.Mode(st) == gitprotect.Strict },
		},
	} {
		for _, a := range []struct {
			name   string
			canAsk bool // the Reporter asks: false refuses, as no terminal and --no-input do
			yes    bool
		}{
			{"yes", true, true},
			{"no", true, false},
			{"unasked", false, true},
		} {
			// The name is a folder's (t.TempDir): no spaces or commas.
			name := strings.Join(strings.Fields(c.cmd+" "+a.name), "-")
			t.Run(name, func(t *testing.T) {
				e, f, execs := upEnv(t, allNative())
				f.LifelineFile = filepath.Join(e.Dirs.Lima, "vm", "ha.pid")
				f.ExecFunc = noDirect
				o := Options{Network: "open"}
				if c.cmd == "devvm network open" {
					o = Options{Network: "allowlist"}
				}
				st := existingVM(t, e, o)
				vm := st.Name()
				in := f.Instances[vm]
				in.Size = backend.Size{CPUs: 4, MemoryGiB: 8, DiskGiB: 100}
				f.Instances[vm] = in
				var said msg.Recorder
				fake(e).OnInfo, fake(e).OnWarn = said.Add, said.Add
				asked := answers(t, e, a.yes)
				fake(e).Terminal = a.canAsk
				before, shells := len(f.Calls()), len(*execs)
				if err := c.set(e); err != nil {
					t.Fatal(msg.NamesOf(err))
				}
				ms := f.Methods()[before:]
				i, j := slices.Index(ms, "Stop"), slices.Index(ms, "Start")
				restarted := i >= 0 && j > i && len(*execs) == shells+1
				later := c.later + " vm=" + vm
				switch {
				case a.canAsk && !slices.Equal(*asked, []string{"restart_now count=1 vm=" + vm}):
					t.Errorf("asked %q", *asked)
				case !a.canAsk && len(*asked) != 0:
					t.Errorf("asked %q without a question that shows", *asked)
				}
				if want := a.canAsk && a.yes; restarted != want {
					t.Errorf("restarted = %v, want %v: calls %q", restarted, want, ms)
				}
				if slices.Contains(said.Names(), later) == restarted {
					t.Errorf("said %q", said.Names())
				}
				if c.applied != nil && c.applied(reread(t, e, vm)) != restarted {
					t.Errorf("applied = %v", !restarted)
				}
			})
		}
	}
}
