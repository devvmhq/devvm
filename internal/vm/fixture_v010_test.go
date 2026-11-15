package vm

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/devvmhq/devvm/internal/agentlogin"
	"github.com/devvmhq/devvm/internal/gitprotect"
	"github.com/devvmhq/devvm/internal/netpolicy"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/ports"
	"github.com/devvmhq/devvm/internal/secrets"
	"github.com/devvmhq/devvm/internal/settings"
	"github.com/devvmhq/devvm/internal/sshagent"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/tools"
)

// v010Fixture is devvm 0.1.0's state (testdata/v0.1.0, see its README):
// four VMs of state version 1, layout version 1, and web the one clone.
func v010Fixture() stateFixture {
	return stateFixture{
		release: "0.1.0", vms: []string{"app", "db", "web", "scratch"}, stateVersion: 1, layout: "1\n",
		clones: []string{"web"}, checkVMs: checkV010VMs, checkDefaults: checkV010Defaults, checkAgents: checkV010Agents,
	}
}

// checkV010VMs checks what each VM 0.1.0 made reads as today: its tier,
// .git protection, ports, secrets, tools, links and group, clone and
// agent forwarding. A .git watch 0.1.0 never recorded is absent, which
// the watch takes as its first scan.
func checkV010VMs(t *testing.T, goos string, vms map[string]*state.VM) {
	t.Helper()
	checkV010App(t, vms["app"], vms["db"])
	checkV010Git(t, goos, vms["app"])
	checkV010Others(t, vms)
}

// checkV010App checks app: node and both agents, allowlist, two ports,
// .git protected, github's secret, a link to db, group backend.
func checkV010App(t *testing.T, app, db *state.VM) {
	t.Helper()
	if tier := netpolicy.TierOf(app); tier != netpolicy.TierAllowlist || !tier.Confined() {
		t.Errorf("app's tier: %q", tier)
	}
	if n, _ := netpolicy.Of(app); n.Made.ProxyKey.Empty() || n.Made.ProxySalt == "" || n.Made.Check != netpolicy.CheckOK {
		t.Errorf("app's net: %+v", n)
	}
	if p, _ := ports.Of(app); p.Forward != "3000,8080" || p.Host.SSH == 0 || p.Host.Proxy == 0 {
		t.Errorf("app's ports: %+v", p)
	}
	if s, _ := secrets.Of(app); !slices.Equal(s.Names, []string{"github"}) {
		t.Errorf("app's secrets: %+v", s)
	}
	if tl, _ := tools.Of(app); !slices.Equal(tl.Names, []string{"node", "claude", "codex"}) || len(tl.Made.Files) != 5 {
		t.Errorf("app's tools: %+v", tl)
	}
	links, err := ReadLinks(app)
	if err != nil || len(links) != 1 || links[0].Name != "db" || links[0].Port != 5432 || links[0].Target != db.Name() ||
		links[0].TargetID != db.ID || !links[0].Addr.IsValid() {
		t.Errorf("app's links: %+v, %v", links, err)
	}
	if l := linksOf(app); l.Group != "backend" {
		t.Errorf("app's group: %+v", l)
	}
	if l := linksOf(db); l.Group != "backend" || l.Publish != "5432" {
		t.Errorf("db's links: %+v", l)
	}
	if a, _ := sshagent.Of(app); sshagent.Mode(app) != sshagent.Off || a.Made.Started == "" {
		t.Errorf("app's ssh agent: %+v", a)
	}
}

// checkV010Git checks app's .git protection: on and locked on a Mac,
// off on Linux, where 0.1.0 had no locks. Neither has a watch yet, which
// a host without locks takes as its first scan.
func checkV010Git(t *testing.T, goos string, app *state.VM) {
	t.Helper()
	g, err := gitprotect.Of(app)
	switch {
	case err != nil:
		t.Errorf("app's git: %v", err)
	case goos == "darwin" && (g.Mode != gitprotect.On || !g.Made.Protected || g.Made.GitID == "" || g.Made.Delegations == ""):
		t.Errorf("app's git on a Mac: %+v", g)
	case goos != "darwin" && (g.Mode != gitprotect.Off || !reflect.ValueOf(g.Made).IsZero()):
		t.Errorf("app's git on %s: %+v", goos, g)
	}
	if !reflect.ValueOf(g.Watched).IsZero() {
		t.Errorf("app's git watch: %+v, not none yet", g.Watched)
	}
}

// checkV010Others checks web (--clone, open) and scratch (no network, no
// .git protection), that every VM's size is config.toml's, and that none
// has a cache mode or a transparent mode of its own, which 0.1.0 didn't
// have.
func checkV010Others(t *testing.T, vms map[string]*state.VM) {
	t.Helper()
	web, scratch := vms["web"], vms["scratch"]
	if c := sectionOf[cloneInVM](web, cloneSection); !c.On || c.Made.Remote != "devvm" || !c.Made.Seeded {
		t.Errorf("web's clone: %+v", c)
	}
	if g, _ := gitprotect.Of(web); g.Mode != gitprotect.Clone {
		t.Errorf("web's git: %+v", g)
	}
	if tier := netpolicy.TierOf(web); tier != netpolicy.TierOpen {
		t.Errorf("web's tier: %q", tier)
	}
	if g, _ := gitprotect.Of(scratch); g.Mode != gitprotect.Off {
		t.Errorf("scratch's git: %+v", g)
	}
	if tier := netpolicy.TierOf(scratch); tier != netpolicy.TierNone {
		t.Errorf("scratch's tier: %q", tier)
	}
	for slug, st := range vms {
		if s := sectionOf[Size](st, sizeSection); s != (Size{CPUs: "4", Memory: "8"}) {
			t.Errorf("%s's size: %+v", slug, s)
		}
		if m, err := readMount(st); err != nil || cacheOf(st) != "" {
			t.Errorf("%s's mount: %+v, %v; want no cache mode chosen", slug, m, err)
		}
		if n, err := netpolicy.Of(st); err != nil || n.Transparent != "" {
			t.Errorf("%s's transparent mode: %q, %v; want none of its own", slug, n.Transparent, err)
		}
	}
}

// checkV010Defaults checks 0.1.0's defaults as it saved them:
// settings.json's tier, ports, agents, layers and features, config.toml's
// size and retention.
func checkV010Defaults(t *testing.T, def settings.Defaults) {
	t.Helper()
	saved := func(v settings.Value, want string, from settings.Source) {
		t.Helper()
		if v.Value != want || v.Source != from {
			t.Errorf("%+v, want %q from %s", v, want, from)
		}
	}
	saved(def.Network, "allowlist", settings.FromSettings)
	saved(def.Ports, "3000,5173", settings.FromSettings)
	saved(def.CPUs, "4", settings.FromConfig)
	saved(def.Memory, "8", settings.FromConfig)
	if !slices.Equal(def.Agents.Names, []string{"claude", "codex"}) || !def.Agents.Chosen ||
		len(def.Layers.Names) != 0 || !def.Layers.Chosen || len(def.Features.Names) != 0 || !def.Features.Chosen {
		t.Errorf("agents %+v, layers %+v, features %+v", def.Agents, def.Layers, def.Features)
	}
	if def.Retention.ExportsCount != 20 || def.Retention.Sources[settings.RetainHistoryCount] != settings.FromConfig {
		t.Errorf("retention %+v", def.Retention)
	}
}

// checkV010Agents checks 0.1.0's agent store: the settings it imported
// are what a VM mounts, and the logins are where devvm reads them.
func checkV010Agents(t *testing.T, d paths.Dirs) {
	t.Helper()
	dirs, comma := agentMounts(d, []string{"node", "claude", "codex"})
	if want := []string{filepath.Join(d.AgentSettings("claude"), ".claude", "skills")}; !slices.Equal(dirs, want) || comma != nil {
		t.Errorf("mounts %q (comma %q), want %q", dirs, comma, want)
	}
	for _, f := range []string{
		filepath.Join(d.AgentSettings("claude"), ".claude", "settings.json"),
		filepath.Join(d.AgentSettings("codex"), ".codex", "config.toml"),
		secrets.LoginFile(d, "claude"),
		secrets.LoginFile(d, "codex"),
	} {
		if _, err := os.Stat(f); err != nil {
			t.Error(err)
		}
	}
	checkV010Logins(t, d)
}

// checkV010Logins: 0.1.0's login files give the proxy their keys, and
// reading them, for a lookup, a VM's copy or its bindings, leaves each
// file's bytes as the release wrote them.
func checkV010Logins(t *testing.T, d paths.Dirs) {
	t.Helper()
	before := map[string][]byte{}
	for _, kind := range []string{"claude", "codex"} {
		b, err := os.ReadFile(secrets.LoginFile(d, kind))
		if err != nil {
			t.Fatal(err)
		}
		before[kind] = b
	}
	codex := agentlogin.File{Kind: "codex", Path: secrets.LoginFile(d, "codex"), Salt: "0123456789abcdef"}
	if v, err := codex.Get(t.Context(), "codex-key"); v.Reveal() != "sk-testfake-codex" || err != nil {
		t.Errorf("codex-key = %q, %v", v.Reveal(), err)
	}
	for _, kind := range []string{"claude", "codex"} {
		f := agentlogin.File{Kind: kind, Path: secrets.LoginFile(d, kind), Salt: "0123456789abcdef"}
		if _, err := f.VMCopy(); err != nil {
			t.Errorf("%s: VMCopy: %v", kind, err)
		}
		if _, err := f.Bindings(); err != nil {
			t.Errorf("%s: Bindings: %v", kind, err)
		}
		if after, _ := os.ReadFile(f.Path); string(after) != string(before[kind]) {
			t.Errorf("%s's login file changed:\n%s\nthe release wrote\n%s", kind, after, before[kind])
		}
	}
}
