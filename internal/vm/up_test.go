package vm

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/backend/backendtest"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/netpolicy"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/project"
	"github.com/devvmhq/devvm/internal/settings"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/tools"
)

// A new VM: built, created with the folder, its state written, started
// open, its proxy first, and the backend's shell run.
func TestUp(t *testing.T) {
	e, f, execs := upEnv(t, allNative())
	if err := (Launch{e}).Up(t.Context(), Options{Network: "open", Size: Size{CPUs: "2"}, Ports: "none"}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	ms := f.Methods()
	// Its proxy's lifeline (Running) before it starts; the open self-check
	// (Exec) once it runs; the ssh config's entry: how to reach it, its
	// host keys for its start (asked which start, read, then asked again).
	want := []string{"Problems", "Inspect", "PinOrigin", "Images", "BuildImage", "CreateVM", "Running", "Start", "Running", "Exec", "Exec", "List", "SSH", "Running", "Exec", "Running", "Shell"}
	if !slices.Equal(ms, want) {
		t.Errorf("calls = %q, want %q", ms, want)
	}
	create, start := call(f, "CreateVM"), call(f, "Start")
	spec, _ := create.Args[1].(backend.VMSpec)
	if spec.Project != e.Dir || spec.Size.CPUs != 2 || !strings.HasPrefix(spec.Name, "devvm-web-") {
		t.Errorf("CreateVM spec = %+v", spec)
	}
	// Every VM has the share, read-only, even with no tools of its own.
	if spec.Tools != e.Dirs.Tools() || spec.ToolsAt != tools.ShareAt {
		t.Errorf("CreateVM's tools share = %q at %q", spec.Tools, spec.ToolsAt)
	}
	got := staged(t, f)
	if got["tools"] != "" || !strings.HasPrefix(got["sync.sh"], "#!/bin/bash") || got["tools.sh"] == "" {
		t.Errorf("staged = %q", got)
	}
	// The backends each tool's image comes from, for the VM's mise.
	if want, err := tools.Aliases(e.Assets); err != nil || got["tool-alias.toml"] != string(want) || !strings.HasPrefix(string(want), "[tool_alias]\n") {
		t.Errorf("staged tool-alias.toml = %q, want %q (%v)", got["tool-alias.toml"], want, err)
	}
	// Every agent's environment, whichever the VM has: what base's
	// devvm-tools.sh and devvm-telemetry.sh set before the agents'
	// registry did, among them.
	if !strings.HasPrefix(got["agent-env"], "export DISABLE_AUTOUPDATER='1'\nexport DISABLE_TELEMETRY='1'\nexport DISABLE_ERROR_REPORTING='1'\n") {
		t.Errorf("staged agent-env = %q", got["agent-env"])
	}
	for _, line := range []string{"export OPENCODE_DISABLE_AUTOUPDATE='1'\n", "export PI_SKIP_VERSION_CHECK='1'\n", "export PI_TELEMETRY='0'\n"} {
		if !strings.Contains(got["agent-env"], line) {
			t.Errorf("staged agent-env lacks %q", line)
		}
	}
	if len(*execs) != 1 || (*execs)[0].Path != "/bin/shell" {
		t.Errorf("exec = %+v", *execs)
	}
	st, err := state.ReadVM(e.Dirs, spec.Name)
	if err != nil {
		t.Fatal(err)
	}
	if st.MadeBy != testVersion {
		t.Errorf("made by %q, want %q", st.MadeBy, testVersion)
	}
	// Made and started with the folder itself, as its state records it,
	// which a backend may mount it by.
	if w, _ := start.Args[1].(backend.Want); st.Folder == "" || spec.ProjectID != st.Folder || w.ProjectID != st.Folder {
		t.Errorf("the folder: made with %q, started with %q, recorded %q", spec.ProjectID, w.ProjectID, st.Folder)
	}
	// Without network policy too, its ssh port is devvm's: recorded, made
	// with it and started on it.
	c, _ := start.Args[2].(backend.Confinement)
	if sp := get(st, keySSHPort); c.Net != backend.NetDirect || sp == "" || strconv.Itoa(c.SSHPort) != sp || strconv.Itoa(spec.SSHPort) != sp {
		t.Errorf("Start confinement = %+v, VMSpec's ssh port %d, state's %q", c, spec.SSHPort, sp)
	}
	for k, v := range map[string]string{
		keyImage: "base", keyDir: e.Dir, keyBackend: "fake", KeyNet: "open", KeyCPUs: "2",
		KeyPorts: "none", keyGitProtect: "on",
	} {
		if got := get(st, k); got != v {
			t.Errorf("state %s = %q, want %q", k, got, v)
		}
	}
	// It exists now, and runs: opened as it is, set up again.
	before := len(f.Calls())
	if err := (Launch{e}).Up(t.Context(), Options{Network: "open"}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if ms := f.Methods()[before:]; slices.Contains(ms, "Start") || slices.Contains(ms, "CreateVM") || !slices.Contains(ms, "Exec") {
		t.Errorf("calls again = %q", ms)
	}
	if len(*execs) != 2 {
		t.Errorf("exec = %+v", *execs)
	}
}

// noDirect makes the fake VM's direct connections fail, as confined.
func noDirect(_ string, argv []string, _ backend.ExecOpts) error {
	if argv[0] == "curl" {
		return errors.New("curl: (7) Failed to connect")
	}
	return nil
}

// With network policy: its proxy runs before the VM starts confined, with
// the VM's key as the proxy's password, and the self-check passes before
// the setup and the shell.
func TestUpNetworkPolicy(t *testing.T) {
	e, f, execs := upEnv(t, allNative())
	f.LifelineFile = filepath.Join(e.Dirs.Lima, "vm", "ha.pid")
	starts := testForwarders(t, e, f)
	hostKeys := f.ExecFunc
	f.ExecFunc = func(vm string, argv []string, o backend.ExecOpts) error {
		if err := noDirect(vm, argv, o); err != nil {
			return err
		}
		return hostKeys(vm, argv, o)
	}
	if err := (Launch{e}).Up(t.Context(), Options{Network: "allowlist"}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if len(*starts) != 1 {
		t.Errorf("forwarder starts = %q, want one", *starts)
	}
	// Its forwarder, once it runs: how to reach it, its host keys for its
	// start (asked which start, read, then asked again); then the ssh
	// config's entry, which finds them pinned for this start.
	want := []string{"Problems", "Inspect", "PinOrigin", "Images", "BuildImage", "CreateVM", "Running", "Start", "SSH", "Running", "Exec", "Running", "Running", "Running", "Running", "Exec", "Exec", "List", "SSH", "Running", "Shell"}
	if ms := f.Methods(); !slices.Equal(ms, want) {
		t.Errorf("calls = %q, want %q", ms, want)
	}
	spec, _ := call(f, "CreateVM").Args[1].(backend.VMSpec)
	st, err := state.ReadVM(e.Dirs, spec.Name)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := call(f, "Start").Args[2].(backend.Confinement)
	if c.Net != backend.NetProxy || c.ProxyPassword != get(st, keyProxyKey) ||
		strconv.Itoa(c.ProxyPort) != get(st, keyProxyPort) || strconv.Itoa(c.SSHPort) != get(st, keySSHPort) {
		t.Errorf("Start confinement = %+v", c)
	}
	if get(st, KeyNet) != "allowlist" || get(st, keyCheck) != "ok" {
		t.Errorf("state net=%q net_check=%q", get(st, KeyNet), get(st, keyCheck))
	}
	if _, ok := e.Proxy.PID(spec.Name); !ok {
		t.Error("its proxy doesn't run")
	}
	if len(*execs) != 1 {
		t.Errorf("exec = %+v", *execs)
	}
}

// A VM devvm started confined that still reaches the internet directly is
// stopped: no setup, no shell.
func TestUpNetworkPolicyFailsClosed(t *testing.T) {
	e, f, execs := upEnv(t, allNative())
	err := Launch{e}.Up(t.Context(), Options{Network: "allowlist"})
	spec, _ := call(f, "CreateVM").Args[1].(backend.VMSpec)
	if msg.NamesOf(err) != "net_direct_route vm="+spec.Name {
		t.Fatalf("Up = %v", msg.NamesOf(err))
	}
	want := []string{"Problems", "Inspect", "PinOrigin", "Images", "BuildImage", "CreateVM", "Running", "Start", "SSH", "Running", "Exec", "Running", "Running", "Running", "Running", "Exec", "Running", "Stop"}
	if ms := f.Methods(); !slices.Equal(ms, want) || len(*execs) > 0 {
		t.Errorf("calls = %q, want %q; execs %+v", ms, want, *execs)
	}
	if f.Instances[spec.Name].Status != backend.StatusStopped {
		t.Error("left running")
	}
	st, _ := state.ReadVM(e.Dirs, spec.Name)
	if get(st, keyCheck) != "unenforced" {
		t.Errorf("net_check = %q", get(st, keyCheck))
	}
}

// The network choice is in the state before the VM is made: one whose
// creation fails later keeps it.
func TestUpKeepsTheNetworkChoice(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	f.Errs = map[string]error{"CreateVM": errors.New("clone failed")}
	if err := (Launch{e}).Up(t.Context(), Options{Network: "allowlist"}); err == nil {
		t.Fatal("Up succeeded")
	}
	spec, _ := call(f, "CreateVM").Args[1].(backend.VMSpec)
	st, _ := state.ReadVM(e.Dirs, spec.Name)
	if get(st, KeyNet) != "allowlist" || slices.Contains(f.Methods(), "Start") {
		t.Errorf("net=%q, calls %q", get(st, KeyNet), f.Methods())
	}
}

// A backend that can't confine a VM to its proxy is refused before
// anything is built, never started open.
func TestUpRequiresEgressProxy(t *testing.T) {
	caps := allNative()
	caps[backend.FeatureEgressProxy] = backend.Emulated(false)
	e, f, _ := upEnv(t, caps)
	err := Launch{e}.Up(t.Context(), Options{Network: "allowlist"})
	if got := msg.NamesOf(err); !strings.HasPrefix(got, "net_tier_backend backend=fake vm=devvm-web-") {
		t.Errorf("Up = %v", got)
	}
	if slices.Contains(f.Methods(), "BuildImage") {
		t.Errorf("calls = %q", f.Methods())
	}
	// Without network policy it doesn't need it.
	if err := (Launch{e}).Up(t.Context(), Options{Network: "open"}); err != nil {
		t.Errorf("Up without network policy = %v", msg.NamesOf(err))
	}
}

// On a backend without egress-proxy, a VM can only be open: without
// --network open it's refused before anything is built, and with it
// it's made open, its proxy set in it.
func TestUpWithoutEgressProxy(t *testing.T) {
	caps := allNative()
	delete(caps, backend.FeatureEgressProxy)
	e, f, _ := upEnv(t, caps)
	for _, o := range []Options{{Network: "allowlist"}, {}} {
		err := Launch{e}.Up(t.Context(), o)
		if got := msg.NamesOf(err); !strings.HasPrefix(got, "net_tier_backend backend=fake vm=devvm-web-") ||
			slices.Contains(f.Methods(), "BuildImage") {
			t.Errorf("Up %+v = %v, calls %q", o, got, f.Methods())
		}
	}
	if err := (Launch{e}).Up(t.Context(), Options{Network: "open"}); err != nil {
		t.Fatalf("Up = %v", msg.NamesOf(err))
	}
	spec, _ := call(f, "CreateVM").Args[1].(backend.VMSpec)
	st, _ := state.ReadVM(e.Dirs, spec.Name)
	c, _ := call(f, "Start").Args[2].(backend.Confinement)
	if get(st, KeyNet) != "open" || c.Net != backend.NetDirect || !c.GuestProxy() {
		t.Errorf("net=%q, started %+v", get(st, KeyNet), c)
	}
}

// --agents goes with the folder's toolchains; the shared tools are built
// from base, listed in the VM's state with base's libc, and sent to the VM
// to mount. The image is base: tools aren't layers.
func TestUpTools(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	if err := os.WriteFile(filepath.Join(e.Dir, "package.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	var said msg.Recorder
	fake(e).OnSay = said.Add
	if err := (Launch{e}).Up(t.Context(), Options{Agents: &[]string{"claude"}, Network: "open"}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if got := strings.Join(partLines(said.Names()), ","); got != "new_vm_toolchain_for file=package.json tool=node,new_vm_agent_from agent=claude from=--agents,new_vm_layers_none" {
		t.Errorf("said %q", said.Names())
	}
	// A tools build for each of node, pnpm, bun and claude.
	if ms := f.Methods(); !slices.Equal(ms, []string{"Problems", "Inspect", "PinOrigin", "Images", "BuildImage", "Images", "BuildTools", "BuildTools", "BuildTools", "BuildTools", "CreateVM", "Running", "Start", "SSH", "Running", "Exec", "Running", "Running", "Running", "Exec", "Exec", "List", "SSH", "Running", "Shell"}) {
		t.Errorf("calls = %q", ms)
	}
	data, err := os.ReadFile(e.Dirs.ToolLock())
	if err != nil {
		t.Fatal(err)
	}
	lock, err := tools.ParseLock(data)
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, tool := range []string{"node", "pnpm", "bun", "claude"} {
		file, err := e.Tools.File(lock, tool, tools.Target{Libc: "gnu"})
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	spec, _ := call(f, "CreateVM").Args[1].(backend.VMSpec)
	st, err := state.ReadVM(e.Dirs, spec.Name)
	if err != nil {
		t.Fatal(err)
	}
	if get(st, keyImage) != "base" || !slices.Equal(tools.Names(st), []string{"node", "claude"}) || get(st, KeyLibc) != "gnu" || get(st, KeyTools) != strings.Join(files, " ") {
		t.Errorf("state image=%q tool_names=%q libc=%q tools=%q", get(st, keyImage), tools.Names(st), get(st, KeyLibc), get(st, KeyTools))
	}
	// Made from base's image: the tools aren't layers.
	if img, _ := call(f, "CreateVM").Args[0].(string); img != call(f, "BuildImage").Args[0].(backend.ImageSpec).Key {
		t.Errorf("CreateVM from %q, not base's image", img)
	}
	if got := staged(t, f)["tools"]; got != string(tools.Mounts(files)) || !strings.HasPrefix(got, files[0]+" node 24.21.0\n") {
		t.Errorf("staged tools = %q", got)
	}
	for _, file := range files {
		if !(tools.Share{Env: e.Tools}).Built(file) {
			t.Errorf("%s isn't in the share", file)
		}
	}
}

// A --from VM's gnu tool images build in a clone of devvm's own Ubuntu
// base, built first and said so, never in the --from image, as any VM
// mounts them; a musl VM's build in its own base, keyed by its digest.
func TestUpFromTools(t *testing.T) {
	for _, tc := range []struct {
		libc, from string
		own        bool // they build in devvm's own base
	}{
		{"gnu", "evil/debian:12@sha256:" + strings.Repeat("e", 64), true},
		{"musl", "evil/alpine:3@sha256:" + strings.Repeat("e", 64), false},
	} {
		t.Run(tc.libc, func(t *testing.T) {
			e, f, _ := upEnv(t, allNative())
			f.Built = backend.Built{Libc: tc.libc}
			var said msg.Recorder
			fake(e).OnInfo = said.Add
			if err := (Launch{e}).Up(t.Context(), Options{From: tc.from, Toolchains: &[]string{"go"}, Agents: &[]string{}, Network: "open"}); err != nil {
				t.Fatal(msg.NamesOf(err))
			}
			bases := map[bool]string{} // by whether it's from tc.from
			for _, c := range f.Calls() {
				if c.Method != "BuildImage" {
					continue
				}
				if spec, ok := c.Args[0].(backend.ImageSpec); ok {
					bases[spec.From != nil] = spec.Key
				}
			}
			tspec, _ := call(f, "BuildTools").Args[0].(backend.ToolsSpec)
			if want := bases[!tc.own]; want == "" || tspec.Base != want {
				t.Errorf("tools built in %q, want %q (bases %v)", tspec.Base, want, bases)
			}
			if _, ok := bases[false]; ok != tc.own {
				t.Errorf("devvm's own base built: %v", ok)
			}
			if got := slices.Contains(said.Names(), "tools_need_own_base"); got != tc.own {
				t.Errorf("said %q", said.Names())
			}
			tg := tools.Target{Libc: tc.libc}
			if !tc.own {
				tg.Base = tc.from
			}
			data, err := os.ReadFile(e.Dirs.ToolLock())
			if err != nil {
				t.Fatal(err)
			}
			lock, err := tools.ParseLock(data)
			if err != nil {
				t.Fatal(err)
			}
			file, err := e.Tools.File(lock, "go", tg)
			if err != nil {
				t.Fatal(err)
			}
			spec, _ := call(f, "CreateVM").Args[1].(backend.VMSpec)
			st, _ := state.ReadVM(e.Dirs, spec.Name)
			if get(st, KeyTools) != file || get(st, KeyLibc) != tc.libc {
				t.Errorf("state tools=%q libc=%q, want %s", get(st, KeyTools), get(st, KeyLibc), file)
			}
		})
	}
}

// A new VM's defaults: the toolchains the folder's files call for, devvm
// setup's agents, and no layers, the nix layer a flake.nix calls for
// included; each flag replaces its part, none with an empty list. Without
// a terminal the summary says what the VM gets of each and why, then the
// flags that choose others.
func TestUpParts(t *testing.T) {
	const (
		nodeGo   = "new_vm_toolchain_for file=package.json tool=node,new_vm_toolchain_for file=go.mod tool=go,"
		noAgents = "new_vm_agents_none,"
		noLayers = "new_vm_layers_none"
		setup    = "new_vm_layer_from from=devvm setup layer="
		flag     = "new_vm_layer_from from=--layers layer="
	)
	for _, tc := range []struct {
		name                 string
		o                    Options
		img, tools, gets     string
		setup                []string // devvm setup's agents; nil for unset
		setupLayers          []string // devvm setup's layers; nil for unset
		warned               string   // what's warned, when anything is
		withFlake, withLayer bool
	}{
		{name: "setup's layers", setupLayers: []string{"nix", "mine"}, img: "nix+mine", tools: "node go", gets: nodeGo + noAgents + setup + "nix," + setup + "mine", withLayer: true},
		{name: "--layers replaces setup's", o: Options{Layers: &[]string{"docker"}}, setupLayers: []string{"nix"}, img: "docker", tools: "node go", gets: nodeGo + noAgents + flag + "docker"},
		{name: "--layers none", o: Options{Layers: &[]string{}}, setupLayers: []string{"nix"}, img: "base", tools: "node go", gets: nodeGo + noAgents + noLayers},
		{
			name: "setup's layer gone", setupLayers: []string{"mine", "nix"}, img: "nix", tools: "node go", gets: nodeGo + noAgents + setup + "nix",
			warned: "default_layer_gone dir=LAYERS layer=mine vm=VM",
		},
		{name: "a gone layer --layers replaces", o: Options{Layers: &[]string{}}, setupLayers: []string{"mine"}, img: "base", tools: "node go", gets: nodeGo + noAgents + noLayers},
		{name: "the defaults", img: "base", tools: "node go", gets: nodeGo + noAgents + noLayers, withFlake: true},
		{name: "setup's agents", setup: []string{"codex"}, img: "base", tools: "node go codex", gets: nodeGo + "new_vm_agent_from agent=codex from=devvm setup," + noLayers},
		{name: "setup's none", setup: []string{}, img: "base", tools: "node go", gets: nodeGo + noAgents + noLayers},
		{name: "--tools", o: Options{Toolchains: &[]string{"python"}}, img: "base", tools: "python", gets: "new_vm_toolchain_from from=--tools tool=python," + noAgents + noLayers},
		{
			name: "--tools none", o: Options{Toolchains: &[]string{}}, setup: []string{"claude"}, img: "base", tools: "claude",
			gets: "new_vm_toolchains_none,new_vm_agent_from agent=claude from=devvm setup," + noLayers,
		},
		{name: "--agents none", o: Options{Agents: &[]string{}}, setup: []string{"claude"}, img: "base", tools: "node go", gets: nodeGo + noAgents + noLayers},
		{name: "--layers", o: Options{Layers: &[]string{"mine", "nix"}}, img: "mine+nix", tools: "node go", gets: nodeGo + noAgents + flag + "mine," + flag + "nix", withLayer: true},
		{name: "all none", o: Options{Toolchains: &[]string{}, Agents: &[]string{}, Layers: &[]string{}}, img: "base", gets: "new_vm_toolchains_none," + noAgents + noLayers},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, f, _ := upEnv(t, allNative())
			for _, file := range []string{"package.json", "go.mod"} {
				if err := os.WriteFile(filepath.Join(e.Dir, file), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.withFlake {
				if err := os.WriteFile(filepath.Join(e.Dir, "flake.nix"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.withLayer {
				if err := os.MkdirAll(e.Dirs.Layers(), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(e.Dirs.Layers(), "mine.sh"), []byte("#!/bin/bash\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.setup != nil {
				if _, err := settings.Save(e.Dirs, settings.Values{Agents: &tc.setup}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.setupLayers != nil {
				if _, err := settings.Save(e.Dirs, settings.Values{Layers: &tc.setupLayers}); err != nil {
					t.Fatal(err)
				}
			}
			var said, says, warned msg.Recorder
			fake(e).OnInfo, fake(e).OnSay, fake(e).OnWarn = said.Add, says.Add, warned.Add
			o := tc.o
			o.Network, o.Ports = "open", "none"
			if err := (Launch{e}).Up(t.Context(), o); err != nil {
				t.Fatal(msg.NamesOf(err))
			}
			spec, _ := call(f, "CreateVM").Args[1].(backend.VMSpec)
			st, _ := state.ReadVM(e.Dirs, spec.Name)
			if get(st, keyImage) != tc.img || get(st, keyToolNames) != tc.tools {
				t.Errorf("image=%q tool_names=%q, want %q and %q", get(st, keyImage), get(st, keyToolNames), tc.img, tc.tools)
			}
			if !slices.Contains(said.Names(), "new_vm_summary vm="+spec.Name) {
				t.Errorf("said %q, want new_vm_summary", said.Names())
			}
			if got := strings.Join(partLines(says.Names()), ","); got != tc.gets {
				t.Errorf("summary's parts %q, want %q", got, tc.gets)
			}
			if names := says.Names(); !slices.Contains(names, "new_vm_choose") {
				t.Errorf("summary %q doesn't say new_vm_choose", names)
			}
			want := strings.NewReplacer("LAYERS", e.Dirs.Layers(), "VM", spec.Name).Replace(tc.warned)
			var layerWarnings []string
			for _, w := range warned.Names() {
				if strings.HasPrefix(w, "default_layer") {
					layerWarnings = append(layerWarnings, w)
				}
			}
			if got := strings.Join(layerWarnings, "\n"); got != want {
				t.Errorf("warned %q, want %q", got, want)
			}
		})
	}
}

// A layer of devvm setup's that the backend can't run fails before
// anything is built, naming the layer; --layers without it makes the VM.
func TestUpSetupLayerLacks(t *testing.T) {
	caps := allNative()
	delete(caps, backend.FeatureDockerLayer)
	e, f, _ := upEnv(t, caps)
	if _, err := settings.Save(e.Dirs, settings.Values{Layers: &[]string{"docker"}}); err != nil {
		t.Fatal(err)
	}
	err := Launch{e}.Up(t.Context(), Options{Network: "open", Ports: "none"})
	if got := msg.NamesOf(err); !strings.HasPrefix(got, "default_layer_lacks backend=fake feature=docker-layer layer=docker vm=devvm-web-") {
		t.Errorf("Up = %v", got)
	}
	if ms := f.Methods(); slices.Contains(ms, "BuildImage") {
		t.Errorf("calls = %q", ms)
	}
	if err := (Launch{e}).Up(t.Context(), Options{Layers: &[]string{}, Network: "open", Ports: "none"}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
}

// In a terminal, once the user says to customize the VM, the picker
// shows the defaults checked, each detected toolchain naming its file
// and setup's agents and layers saying so, the nix layer naming the
// flake.nix, and the other layers unchecked; what's checked is what the
// VM gets. On the first run it asks for setup's agents and base's
// features first, under the first run's line, before the summary.
func TestUpPicker(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	for _, file := range []string{"package.json", "flake.nix"} {
		if err := os.WriteFile(filepath.Join(e.Dir, file), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Enter at each size question: the first run's, then the VM's.
	fake(e).Picks, fake(e).Terminal = true, true
	fake(e).OnAsk = func(m msg.M) bool { return m.ID() == "new_vm_customize" }
	fake(e).OnAnswer = func(msg.M) string { return "" }
	if _, err := settings.Save(e.Dirs, settings.Values{Layers: &[]string{"nix", "desktop"}}); err != nil {
		t.Fatal(err)
	}
	var asked []string
	var shown []PickPart
	var said msg.Recorder
	fake(e).OnAny = said.Add
	e.PickParts = func(help msg.M, parts []PickPart) []PickPart {
		asked = append(asked, help.ID())
		said.Add(help)
		if help.ID() == "agents_help" {
			parts[0].Rows[1].On = true // codex
			return parts
		}
		shown = parts
		out := make([]PickPart, len(parts))
		for i, p := range parts {
			out[i] = PickPart{Heading: p.Heading, Rows: slices.Clone(p.Rows)}
		}
		out[0].Rows[1].On = true // python
		out[2].Rows[1].On = true // docker
		return out
	}
	if err := (Launch{e}).Up(t.Context(), Options{Network: "open", Ports: "none"}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if !slices.Equal(asked, []string{"agents_help", "pick_new_help"}) {
		t.Fatalf("asked %q", asked)
	}
	// The first run's line, setup's agents and base's features, then the
	// summary, then the picker.
	want := []string{"first_run_defaults", "agents_help", "base_features", "new_vm_summary", "pick_new_help"}
	order := slices.DeleteFunc(said.Names(), func(s string) bool {
		return !slices.ContainsFunc(want, func(w string) bool { return strings.HasPrefix(s, w) })
	})
	if len(order) != len(want) || !slices.EqualFunc(order, want, strings.HasPrefix) {
		t.Errorf("order %q, want %q", order, want)
	}
	if def, _ := settings.Read(e.Dirs); !def.Agents.Chosen || !slices.Equal(def.Agents.Names, []string{"codex"}) {
		t.Errorf("setup's agents = %+v", def.Agents)
	}
	// Each row as its name, its description's names and whether it's on.
	type row struct {
		Name, Desc string
		On         bool
	}
	rows := map[string]row{}
	var headings []string
	for _, p := range shown {
		headings = append(headings, p.Heading.ID())
		for _, r := range p.Rows {
			rows[r.Name] = row{r.Name, r.Desc.Names(), r.On}
		}
	}
	if !slices.Equal(headings, []string{"pick_toolchains", "pick_agents", "pick_layers"}) {
		t.Errorf("headings %q", headings)
	}
	var layers []string
	for _, r := range shown[2].Rows {
		layers = append(layers, r.Name)
	}
	if !slices.Equal(layers, []string{"desktop", "docker", "nix"}) {
		t.Errorf("layers %q", layers)
	}
	for name, want := range map[string]row{
		"desktop": {Name: "desktop", Desc: "pick_from_setup", On: true},
		"node":    {Name: "node", Desc: "pick_called_for file=package.json", On: true},
		"python":  {Name: "python"},
		"codex":   {Name: "codex", Desc: "pick_from_setup", On: true},
		"claude":  {Name: "claude"},
		"docker":  {Name: "docker", Desc: "pick_layer_docker"},
		"nix":     {Name: "nix", Desc: "pick_called_for file=flake.nix", On: true},
	} {
		if rows[name] != want {
			t.Errorf("row %s = %+v, want %+v", name, rows[name], want)
		}
	}
	// setup's layers keep their order, bottom first, under the one added.
	spec, _ := call(f, "CreateVM").Args[1].(backend.VMSpec)
	st, _ := state.ReadVM(e.Dirs, spec.Name)
	if get(st, keyImage) != "nix+desktop+docker" || get(st, keyToolNames) != "node python codex" {
		t.Errorf("image=%q tool_names=%q", get(st, keyImage), get(st, keyToolNames))
	}
	if slices.Contains(said.Names(), "new_vm_choose") {
		t.Errorf("said %q", said.Names())
	}
}

// A new VM's summary is followed by the question whether to customize
// it, where the picker shows and anything is left to choose: no makes it
// as shown, yes shows the picker, a part a flag gave greyed out, then
// asks its tier, where no flag gave it, and its CPUs, memory and disk, each
// only where no flag gave it.
// Without the picker, or with nothing left, there's no question, and
// only the first ends with the flags that choose others.
func TestUpCustomize(t *testing.T) {
	none := &[]string{}
	for _, tc := range []struct {
		name      string
		o         Options
		picks     bool
		yes       bool
		saved     string // the tier devvm network -g saved, "" for none saved
		asked     string // what's asked, in order
		net, cpus string // what the VM gets
		ports     string // the ports it forwards, "" for every port
		choose    bool   // the summary ends with new_vm_choose
		fixed     bool   // the picker's layers are greyed out, docker's from --layers
	}{
		{name: "Enter", picks: true, asked: "new_vm_customize", net: "allowlist"},
		{name: "no picker", net: "allowlist", choose: true},
		{
			name: "everything given", picks: true, net: "open", cpus: "2", ports: "none",
			o: Options{Toolchains: none, Agents: none, Layers: none, Network: "open", Ports: "none", Size: Size{CPUs: "2", Memory: "4", Disk: "100"}},
		},
		{
			name: "yes", picks: true, yes: true, net: "internet", cpus: "2", ports: "none",
			asked: "new_vm_customize,pick_new_help,net_tier_help,new_vm_ports_help,new_vm_cpus_prompt,new_vm_memory_prompt,new_vm_disk_prompt",
		},
		{
			name: "yes with layers and cpus given", picks: true, yes: true, net: "internet", cpus: "3", fixed: true, ports: "none",
			o:     Options{Layers: &[]string{"docker"}, Size: Size{CPUs: "3"}},
			asked: "new_vm_customize,pick_new_help,net_tier_help,new_vm_ports_help,new_vm_memory_prompt,new_vm_disk_prompt",
		},
		{
			name: "yes with the ports and disk given", picks: true, yes: true, net: "internet", cpus: "2", ports: "3000",
			o:     Options{Ports: "3000", Size: Size{Disk: "200"}},
			asked: "new_vm_customize,pick_new_help,net_tier_help,new_vm_cpus_prompt,new_vm_memory_prompt",
		},
		{
			name: "yes with a saved tier", picks: true, yes: true, saved: "none", net: "internet", cpus: "2", ports: "none",
			asked: "new_vm_customize,pick_new_help,net_tier_help,new_vm_ports_help,new_vm_cpus_prompt,new_vm_memory_prompt,new_vm_disk_prompt",
		},
		{name: "Enter with a saved tier", picks: true, saved: "none", asked: "new_vm_customize", net: "none"},
		{
			name: "every part and the size given", picks: true, net: "allowlist", cpus: "2",
			o:     Options{Toolchains: none, Agents: none, Layers: none, Size: Size{CPUs: "2", Memory: "4", Disk: "100"}},
			asked: "new_vm_customize",
		},
		{
			name: "yes with every part and the tier given", picks: true, yes: true, net: "none", cpus: "2", ports: "none",
			o:     Options{Toolchains: none, Agents: none, Layers: none, Network: "none"},
			asked: "new_vm_customize,new_vm_ports_help,new_vm_cpus_prompt,new_vm_memory_prompt,new_vm_disk_prompt",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, f, _ := upEnv(t, allNative())
			// Past the first run: its agents and base's features chosen.
			v := settings.Values{Agents: none, Features: none}
			if tc.saved != "" {
				v.Network = &tc.saved
			}
			if _, err := settings.Save(e.Dirs, v); err != nil {
				t.Fatal(err)
			}
			hostKeys := f.ExecFunc
			f.ExecFunc = func(vm string, argv []string, o backend.ExecOpts) error {
				if err := noDirect(vm, argv, o); err != nil {
					return err
				}
				return hostKeys(vm, argv, o)
			}
			var asked []string
			var says msg.Recorder
			fk := fake(e)
			fk.Picks, fk.Terminal, fk.OnSay = tc.picks, tc.picks, says.Add
			fk.OnAsk = func(m msg.M) bool { asked = append(asked, m.ID()); return tc.yes }
			fk.OnAnswer = func(m msg.M) string {
				asked = append(asked, m.ID())
				return map[string]string{"new_vm_cpus_prompt": "2", "new_vm_disk_prompt": "150"}[m.ID()]
			}
			e.PickOne = func(help msg.M, _ []PickRow) int {
				asked = append(asked, help.ID())
				return 1 // internet, or no ports
			}
			e.Proxy.Reporter = fk
			var layers PickPart
			e.PickParts = func(help msg.M, parts []PickPart) []PickPart {
				asked = append(asked, help.ID())
				layers = parts[2]
				return parts
			}
			if err := (Launch{e}).Up(t.Context(), tc.o); err != nil {
				t.Fatal(msg.NamesOf(err))
			}
			if got := strings.Join(asked, ","); got != tc.asked {
				t.Errorf("asked %q, want %q", got, tc.asked)
			}
			spec, _ := call(f, "CreateVM").Args[1].(backend.VMSpec)
			st, _ := state.ReadVM(e.Dirs, spec.Name)
			if get(st, KeyNet) != tc.net || get(st, KeyCPUs) != tc.cpus {
				t.Errorf("net=%q cpus=%q, want %q and %q", get(st, KeyNet), get(st, KeyCPUs), tc.net, tc.cpus)
			}
			if got := get(st, KeyPorts); got != tc.ports {
				t.Errorf("ports %q, want %q", got, tc.ports)
			}
			// The disk typed, else the one given or the summary's.
			wantDisk := map[bool]float64{true: 150, false: 100}[strings.HasSuffix(tc.asked, "new_vm_disk_prompt")]
			if tc.o.Size.Disk == "200" {
				wantDisk = 200
			}
			if spec.Size.DiskGiB != wantDisk && (tc.yes || tc.o.Size.Disk != "") {
				t.Errorf("disk %v, want %v", spec.Size.DiskGiB, wantDisk)
			}
			if got := slices.Contains(says.Names(), "new_vm_choose"); got != tc.choose {
				t.Errorf("said %q", says.Names())
			}
			if !tc.fixed {
				return
			}
			for _, r := range layers.Rows {
				if !r.Fixed || r.On != (r.Name == "docker") || r.On && r.Desc.Names() != "pick_from_flag flag=--layers" {
					t.Errorf("layer row %+v", r)
				}
			}
		})
	}
}

// A backend without a feature the VM needs is refused before anything is
// built.
func TestUpRequires(t *testing.T) {
	caps := allNative()
	delete(caps, backend.FeatureProjectMount)
	e, f, execs := upEnv(t, caps)
	err := Launch{e}.Up(t.Context(), Options{Network: "open"})
	if got := msg.NamesOf(err); !strings.HasPrefix(got, "backend_lacks backend=fake feature=project-mount vm=devvm-web-") {
		t.Errorf("Up = %v", got)
	}
	if ms := f.Methods(); slices.Contains(ms, "BuildImage") || slices.Contains(ms, "CreateVM") || len(*execs) > 0 {
		t.Errorf("calls = %q, execs %+v", ms, *execs)
	}
}

// Below the free space a new VM wants, nothing is built or made, and the
// VM has no state; with enough for the VM but not base, base's build is
// refused before it starts.
func TestUpRefusesLowSpace(t *testing.T) {
	e, f, execs := upEnv(t, allNative())
	reg, err := backend.NewRegistry(backendtest.Stored{Fake: f, Dirs: []string{e.Dirs.Lima}})
	if err != nil {
		t.Fatal(err)
	}
	reg.NewVMs = "fake"
	e.Registry = reg
	for _, tc := range []struct {
		free uint64
		want string
	}{
		{4<<30 - 1, "disk_low dir=" + e.Dirs.Lima + " free=3.9 name=devvm-web-"},
		{12<<30 - 1, "disk_low dir=" + e.Dirs.Lima + " free=11.9 name=base need=12"},
	} {
		e.FreeSpace = func(string) (uint64, error) { return tc.free, nil }
		err := Launch{e}.Up(t.Context(), Options{Network: "open", Ports: "none"})
		if got := msg.NamesOf(err); !strings.HasPrefix(got, tc.want) {
			t.Errorf("with %d free: Up = %v, want %s…", tc.free, got, tc.want)
		}
		if ms := f.Methods(); slices.Contains(ms, "BuildImage") || slices.Contains(ms, "CreateVM") || len(*execs) > 0 {
			t.Errorf("with %d free: calls = %q, execs %+v", tc.free, ms, *execs)
		}
		if vms, _ := os.ReadDir(e.Dirs.VMs()); len(vms) > 0 {
			t.Errorf("with %d free: a VM's state is left", tc.free)
		}
	}
}

// The docker layer needs docker-layer of the backend, before anything is
// built.
// The tier picker's rows are the tiers in order, each with what it
// reaches, the one shown on; without a picker, the one shown is kept.
func TestTierRows(t *testing.T) {
	var got []string
	for _, r := range tierRows(netpolicy.TierInternet) {
		s := r.Name + ":" + r.Desc.ID()
		if r.On {
			s = "*" + s
		}
		got = append(got, s)
	}
	want := "open:net_tier_open *internet:net_tier_internet allowlist:net_tier_allowlist none:net_tier_none"
	if strings.Join(got, " ") != want {
		t.Errorf("rows %v, want %s", got, want)
	}
	if tier := (creation{Launch{&Env{}}}).chooseTier("vm", netpolicy.TierNone); tier != netpolicy.TierNone {
		t.Errorf("without a picker: %s, want none", tier)
	}
}

func TestUpRequiresDockerLayer(t *testing.T) {
	caps := allNative()
	delete(caps, backend.FeatureDockerLayer)
	e, f, _ := upEnv(t, caps)
	err := Launch{e}.Up(t.Context(), Options{Layers: &[]string{"docker"}, Network: "open", Ports: "none"})
	if got := msg.NamesOf(err); !strings.HasPrefix(got, "backend_lacks backend=fake feature=docker-layer vm=devvm-web-") {
		t.Errorf("Up = %v", got)
	}
	if ms := f.Methods(); slices.Contains(ms, "BuildImage") {
		t.Errorf("calls = %q", ms)
	}
}

// A disk size, the default's or --disk's, needs disk of the backend,
// before anything is built.
func TestUpDiskNeedsDisk(t *testing.T) {
	caps := allNative()
	delete(caps, backend.FeatureDisk)
	e, f, _ := upEnv(t, caps)
	saveSize(t, e.Dirs, Size{CPUs: "2", Disk: "150"})
	err := (Launch{e}).Up(t.Context(), Options{Network: "open", Ports: "none"})
	if got := msg.NamesOf(err); !strings.HasPrefix(got, "backend_lacks backend=fake feature=disk ") {
		t.Errorf("the default's: %v", got)
	}
	if ms := f.Methods(); slices.Contains(ms, "BuildImage") || slices.Contains(ms, "CreateVM") {
		t.Errorf("calls = %q", ms)
	}
	e2, _, _ := upEnv(t, caps)
	err = (Launch{e2}).Up(t.Context(), Options{Network: "open", Ports: "none", Size: Size{Disk: "150"}})
	if got := msg.NamesOf(err); !strings.HasPrefix(got, "backend_lacks backend=fake feature=disk ") {
		t.Errorf("--disk: %v", got)
	}
}

// Only an unfinished VM of this folder's own is offered a delete and
// remake, and only when the question can be asked: a finished VM never.
func TestRemakeUnfinished(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name            string
		finished, ask   bool
		sub             string
		canAsk, offered bool
	}{
		{name: "finished", finished: true, canAsk: true, ask: true},
		{name: "unfinished, yes", canAsk: true, ask: true, offered: true},
		{name: "unfinished, no", canAsk: true},
		{name: "no question", ask: true},
		{name: "parent's", sub: "/p/sub", canAsk: true, ask: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			d := paths.Dirs{Data: filepath.Join(root, "data"), Config: filepath.Join(root, "config")}
			st := vmState(t, d, "devvm-p-00000000000000", func(st *state.VM) error {
				if !tc.finished {
					st.Created = time.Time{}
				}
				return nil
			})
			asked := false
			e := &Env{Reporter: &msg.Fake{Terminal: tc.canAsk, OnAsk: func(msg.M) bool { asked = true; return tc.ask }}}
			got := remakeUnfinished(e, &target{f: project.Folder{Name: st.Name()}, st: st, sub: tc.sub})
			if got != tc.offered {
				t.Errorf("remakeUnfinished = %v, want %v", got, tc.offered)
			}
			if asked != (!tc.finished && tc.canAsk && tc.sub == "") {
				t.Errorf("asked = %v", asked)
			}
		})
	}
}
