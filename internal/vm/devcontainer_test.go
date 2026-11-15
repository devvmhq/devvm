package vm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/backend/backendtest"
	"github.com/devvmhq/devvm/internal/from"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/netpolicy"
	"github.com/devvmhq/devvm/internal/settings"
	"github.com/devvmhq/devvm/internal/vmname"
)

// devcontainerEnv is up on a fake backend with a devcontainer.json in the
// project folder, its warnings and the devcontainer.json commands it runs
// collected.
func devcontainerTestEnv(t *testing.T, src string) (*Env, *backendtest.Fake, *msg.Recorder) {
	t.Helper()
	e, f, _ := upEnv(t, allNative())
	if err := os.WriteFile(filepath.Join(e.Dir, ".devcontainer.json"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	warned := &msg.Recorder{}
	fake(e).OnWarn = warned.Add
	e.PublicImage = func(_ context.Context, ref string) (string, error) {
		if ref == "private:1" {
			return "", fmt.Errorf("%w: no", from.ErrNotFound)
		}
		return from.Name(ref) + "@sha256:" + strings.Repeat("a", 64), nil
	}
	return e, f, warned
}

// commandsRun returns the devcontainer.json commands the fake ran, each
// as its argv after the login shell, and the folders they ran in.
func commandsRun(f *backendtest.Fake) (cmds []string, dirs []string) {
	for _, c := range f.Calls() {
		if c.Method != "Exec" || len(c.Args) < 3 {
			continue
		}
		argv, _ := c.Args[1].([]string)
		if len(argv) < 4 || !slices.Equal(argv[:4], []string{"bash", "-lc", commandScript, "devvm"}) {
			continue
		}
		cmds = append(cmds, strings.Join(argv[4:], " "))
		o, _ := c.Args[2].(backend.ExecOpts)
		dirs = append(dirs, o.Dir)
	}
	return cmds, dirs
}

func TestUpDevcontainer(t *testing.T) {
	e, f, warned := devcontainerTestEnv(t, `{
		"image": "node:24",
		"forwardPorts": [3000],
		"remoteEnv": {"A": "${containerWorkspaceFolderBasename}"},
		"onCreateCommand": ["touch", "x"],
		"postStartCommand": "echo hi",
	}`)
	if _, err := settings.Save(e.Dirs, settings.Values{Ports: settings.Of("5173")}); err != nil {
		t.Fatal(err)
	}
	st := existingVM(t, e, Options{Network: "open"})
	for k, want := range map[string]string{
		keyFrom: "node@sha256:" + strings.Repeat("a", 64), KeyPorts: "5173,3000",
		KeyDevcontainerCreate: "", KeyDevcontainerEnv: `[{"name":"A","value":"'web'"}]`,
	} {
		if got := get(st, k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if !strings.HasSuffix(get(st, KeyDevcontainer), " .devcontainer.json") {
		t.Errorf("devcontainer = %q", get(st, KeyDevcontainer))
	}
	cmds, dirs := commandsRun(f)
	if want := []string{"touch x", "sh -c echo hi"}; !slices.Equal(cmds, want) {
		t.Errorf("ran %q, want %q", cmds, want)
	}
	for _, d := range dirs {
		if d != e.Dir {
			t.Errorf("ran in %q", d)
		}
	}
	s, err := syncer{e}.stage(t.Context(), st, setupInputs{slug: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if env := string(s.files[s.index("env")].data); !strings.HasPrefix(env, "export A='web'\n") {
		t.Errorf("env = %q", env)
	}
	// Its ports are added to the user's, recorded as the file's.
	if d := sectionOf[devcontainerInVM](st, devcontainerSection); !slices.Equal(d.Ports, []string{"3000"}) || !slices.Equal(d.PortsGave, []string{"3000"}) {
		t.Errorf("file's ports %q, gave %q", d.Ports, d.PortsGave)
	}
	if len(warned.Names()) > 0 {
		t.Errorf("warned %q", warned.Names())
	}
	warned.Reset()

	// Launched again, running: nothing runs; changed, it's said.
	if err := (Launch{e}).Up(t.Context(), Options{}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if cmds, _ := commandsRun(f); len(cmds) != 2 {
		t.Errorf("ran again: %q", cmds)
	}
	if err := os.WriteFile(filepath.Join(e.Dir, ".devcontainer.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (Launch{e}).Up(t.Context(), Options{}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if want := "devcontainer_changed file=.devcontainer.json vm=" + st.Name().String(); !slices.Equal(warned.Names(), []string{want}) {
		t.Errorf("warned %q, want %q", warned.Names(), want)
	}
	// Started again: postStartCommand runs again.
	if err := f.Stop(t.Context(), st.Name()); err != nil {
		t.Fatal(err)
	}
	if err := (Launch{e}).Up(t.Context(), Options{}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if cmds, _ := commandsRun(f); !slices.Equal(cmds, []string{"touch x", "sh -c echo hi", "sh -c echo hi"}) {
		t.Errorf("ran %q", cmds)
	}
}

// A command that fails is said, those after it don't run, and the VM is
// opened all the same.
func TestUpDevcontainerCommandFails(t *testing.T) {
	e, f, warned := devcontainerTestEnv(t, `{"onCreateCommand": "false", "postCreateCommand": "true", "postStartCommand": "echo hi"}`)
	hostKeys := f.ExecFunc // the forwarder's
	f.ExecFunc = func(vm vmname.VM, argv []string, o backend.ExecOpts) error {
		if slices.Contains(argv, "false") {
			return errors.New("exit status 1")
		}
		return hostKeys(vm, argv, o)
	}
	st := existingVM(t, e, Options{Network: "open"})
	if want := "devcontainer_command_failed command=onCreateCommand vm=" + st.Name().String(); !slices.Equal(warned.Names(), []string{want}) {
		t.Errorf("warned %q, want %q", warned.Names(), want)
	}
	if cmds, _ := commandsRun(f); !slices.Equal(cmds, []string{"sh -c false", "sh -c echo hi"}) {
		t.Errorf("ran %q", cmds)
	}
}

// Refused before anything is made.
func TestUpDevcontainerRefused(t *testing.T) {
	for _, tc := range []struct{ name, src, want string }{
		{"not public", `{"image": "private:1"}`, "devcontainer_image_not_public file=.devcontainer.json from=private:1"},
		{"not a reference", `{"image": "a b"}`, "bad_from from=a b"},
		{"local env", `{"remoteEnv": {"A": "${localEnv:HOME}"}}`, "devcontainer_local_env file=.devcontainer.json variable=${localEnv:HOME}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, f, _ := devcontainerTestEnv(t, tc.src)
			err := (Launch{e}).Up(t.Context(), Options{Network: "open"})
			if got := msg.NamesOf(err); got != tc.want {
				t.Errorf("error = %s, want %s", got, tc.want)
			}
			for _, c := range f.Calls() {
				if c.Method == "CreateVM" || c.Method == "BuildImage" {
					t.Errorf("made: %s", c.Method)
				}
			}
		})
	}
}

// The plan of a new VM whose devcontainer.json pins its image: its From
// is the file's image, pinned, which its builder builds from; its size
// and ports are the flags' and the defaults', and the file's ports beyond
// the user's are said, not added. The new VM keeps the file's From and
// plan; the options stay as they were asked for.
func TestPlanDevcontainerFrom(t *testing.T) {
	e, _, _ := devcontainerTestEnv(t, `{"image": "node:24", "forwardPorts": [3000]}`)
	if _, err := settings.Save(e.Dirs, settings.Values{Ports: settings.Of("5173")}); err != nil {
		t.Fatal(err)
	}
	tg, err := (finder{e}).target(t.Context(), need{folder: true})
	if err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	o := Options{Network: "open", Size: Size{Memory: "4G"}}
	n, err := creation{Launch{e}}.plan(t.Context(), o, tg.f, tg.b)
	if err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	pinned := "node@sha256:" + strings.Repeat("a", 64)
	if n.from != pinned || n.builder.From != pinned {
		t.Errorf("from = %q, builder's %q, want %q", n.from, n.builder.From, pinned)
	}
	if n.dc == nil || n.dc.Ports != "3000" {
		t.Errorf("devcontainer plan = %+v", n.dc)
	}
	if n.size.Memory != "4" || n.spec.Size.MemoryGiB != 4 || n.ports != "5173,3000" || !slices.Equal(n.dcPorts, []string{"3000"}) {
		t.Errorf("size %+v (spec %+v), ports %q, the file's %q", n.size, n.spec.Size, n.ports, n.dcPorts)
	}
}

// chosen is chooseNet's tier, or its refusal's names.
func chosen(t netpolicy.Tier, err error) string {
	if err != nil {
		return msg.NamesOf(err)
	}
	return string(t)
}

// A new VM's summary shows the allowlist tier unless it's given another,
// the docker layer's VMs as any other, and nothing is asked here, in a
// terminal or not: customizing it asks. On a backend without
// egress-proxy, a VM can only be open: any other tier is refused in one
// line, never dropped.
func TestChooseNet(t *testing.T) {
	noEgress := allNative()
	delete(noEgress, backend.FeatureEgressProxy)
	unreviewed := allNative()
	unreviewed[backend.FeatureEgressProxy] = backend.Emulated(false)
	const refused = "net_tier_backend backend=fake vm=v"
	for _, tc := range []struct {
		name string
		o    Options
		caps backend.Caps
		want string // the tier, or the refusal
	}{
		{"the default", Options{}, nil, "allowlist"},
		{"--network internet", Options{Network: "internet"}, nil, "internet"},
		{"--network open", Options{Network: "open"}, nil, "open"},
		{"the docker layer", Options{Layers: &[]string{"docker"}}, nil, "allowlist"},
		{"no egress-proxy", Options{}, noEgress, refused},
		{"unreviewed egress-proxy", Options{}, unreviewed, refused},
		{"no egress-proxy, --network none", Options{Network: "none"}, noEgress, refused},
		{"no egress-proxy, --network open", Options{Network: "open"}, noEgress, "open"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caps := tc.caps
			if caps == nil {
				caps = allNative()
			}
			e, f, _ := upEnv(t, caps)
			e.Proxy.Reporter = &msg.Fake{Terminal: true, OnAnswer: func(m msg.M) string { t.Errorf("asked %s", m.Names()); return "" }}
			if got := chosen(creation{Launch{e}}.chooseNet(tc.o, f, "v", settings.NetworkDefault)); got != tc.want {
				t.Errorf("chooseNet = %s, want %s", got, tc.want)
			}
		})
	}
}

// The default (config.toml's, devvm network -g's or devvm setup's): a
// tier gives a new VM's summary that tier, open even where it couldn't
// be anything else, and a confined one that can't have it is refused;
// --network beats it.
func TestChooseNetSaved(t *testing.T) {
	noEgress := allNative()
	delete(noEgress, backend.FeatureEgressProxy)
	for _, tc := range []struct {
		name  string
		saved string
		o     Options
		caps  backend.Caps
		want  string
	}{
		{"open", "open", Options{}, nil, "open"},
		{"open, no egress-proxy", "open", Options{}, noEgress, "open"},
		{"open, --network allowlist", "open", Options{Network: "allowlist"}, nil, "allowlist"},
		{"internet", "internet", Options{}, nil, "internet"},
		{"none", "none", Options{}, nil, "none"},
		{"allowlist, --network open", "allowlist", Options{Network: "open"}, nil, "open"},
		{"allowlist, no egress-proxy", "allowlist", Options{}, noEgress, "net_tier_backend backend=fake vm=v"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caps := tc.caps
			if caps == nil {
				caps = allNative()
			}
			e, f, _ := upEnv(t, caps)
			if got := chosen(creation{Launch{e}}.chooseNet(tc.o, f, "v", tc.saved)); got != tc.want {
				t.Errorf("chooseNet = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestDevcontainerEnvFromState(t *testing.T) {
	e, _, _ := upEnv(t, allNative())
	st := vmState(t, e.Dirs, "devvm-x-0123456789abcd")
	if err := set(t, st, KeyDevcontainerEnv, `[{"name":"A","value":"'x'"},{"name":"B;rm","value":"'y'"}]`); err != nil {
		t.Fatal(err)
	}
	got, err := devcontainerEnv(st)
	if err != nil || got != "export A='x'\n" {
		t.Errorf("devcontainerEnv = %q, %v", got, err)
	}
}
