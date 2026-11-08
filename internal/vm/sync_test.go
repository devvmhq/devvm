package vm

import (
	"context"
	"io"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/agents"
	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/backend/backendtest"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/netpolicy"
	"github.com/devvmhq/devvm/internal/state"
)

// A VM's links are checked before its setup gets them: addresses in
// 127.77.0.0/16, each once; names that are one lowercase DNS label, not
// localhost or the VM's hostname, each once.
func TestCheckLinks(t *testing.T) {
	a := netip.MustParseAddr
	db, cache := GuestLink{a("127.77.0.2"), []string{"db"}}, GuestLink{a("127.77.1.3"), []string{"cache", "redis-2"}}
	for _, c := range []struct {
		name  string
		links []GuestLink
		want  string
	}{
		{"none", nil, ""},
		{"two targets, one with two names", []GuestLink{db, cache}, ""},
		{"the range's last address", []GuestLink{{a("127.77.255.254"), []string{"x"}}}, ""},
		{"outside the range", []GuestLink{{a("127.0.0.1"), []string{"db"}}}, "link_setup_invalid address=127.0.0.1 name=db vm=v"},
		{"the next range", []GuestLink{{a("127.78.0.2"), []string{"db"}}}, "link_setup_invalid address=127.78.0.2 name=db vm=v"},
		{"IPv6", []GuestLink{{a("::ffff:127.77.0.2"), []string{"db"}}}, "link_setup_invalid address=::ffff:127.77.0.2 name=db vm=v"},
		{"no address", []GuestLink{{Names: []string{"db"}}}, "link_setup_invalid address=invalid IP name=db vm=v"},
		{"an address twice", []GuestLink{db, {a("127.77.0.2"), []string{"web"}}}, "link_setup_invalid address=127.77.0.2 name=web vm=v"},
		{"no names", []GuestLink{{a("127.77.0.2"), nil}}, "link_setup_invalid address=127.77.0.2 name= vm=v"},
		{"a name twice", []GuestLink{db, {a("127.77.0.3"), []string{"db"}}}, "link_setup_invalid address=127.77.0.3 name=db vm=v"},
		{"localhost", []GuestLink{{a("127.77.0.2"), []string{"localhost"}}}, "link_setup_invalid address=127.77.0.2 name=localhost vm=v"},
		{"the hostname", []GuestLink{{a("127.77.0.2"), []string{"web"}}}, "link_setup_invalid address=127.77.0.2 name=web vm=v"},
		{"a dot", []GuestLink{{a("127.77.0.2"), []string{"db.devvm.internal"}}}, "link_setup_invalid address=127.77.0.2 name=db.devvm.internal vm=v"},
		{"capitals", []GuestLink{{a("127.77.0.2"), []string{"DB"}}}, "link_setup_invalid address=127.77.0.2 name=DB vm=v"},
		{"a space", []GuestLink{{a("127.77.0.2"), []string{"db x"}}}, "link_setup_invalid address=127.77.0.2 name=db x vm=v"},
		{"a newline", []GuestLink{{a("127.77.0.2"), []string{"db\n1.2.3.4"}}}, "link_setup_invalid address=127.77.0.2 name=db1.2.3.4 vm=v"},
		{"a leading dash", []GuestLink{{a("127.77.0.2"), []string{"-db"}}}, "link_setup_invalid address=127.77.0.2 name=-db vm=v"},
		{"a trailing dash", []GuestLink{{a("127.77.0.2"), []string{"db-"}}}, "link_setup_invalid address=127.77.0.2 name=db- vm=v"},
		{"an empty name", []GuestLink{{a("127.77.0.2"), []string{""}}}, "link_setup_invalid address=127.77.0.2 name= vm=v"},
		{"63 characters", []GuestLink{{a("127.77.0.2"), []string{strings.Repeat("a", 63)}}}, ""},
		{"64 characters", []GuestLink{{a("127.77.0.2"), []string{strings.Repeat("a", 64)}}}, "link_setup_invalid address=127.77.0.2 name=" + strings.Repeat("a", 64) + " vm=v"},
	} {
		if got := msg.NamesOf(checkLinks("v", "web", c.links)); got != c.want {
			t.Errorf("%s: checkLinks = %q, want %q", c.name, got, c.want)
		}
	}
}

// The setup's hosts file: a line per target, each name and its
// NAME.devvm.internal; empty without links.
func TestLinkHosts(t *testing.T) {
	links := []GuestLink{
		{netip.MustParseAddr("127.77.0.2"), []string{"db"}},
		{netip.MustParseAddr("127.77.0.3"), []string{"cache", "redis"}},
	}
	want := "127.77.0.2 db db.devvm.internal\n127.77.0.3 cache cache.devvm.internal redis redis.devvm.internal\n"
	if got := string(linkHosts(links)); got != want {
		t.Errorf("linkHosts = %q, want %q", got, want)
	}
	if got := linkHosts(nil); len(got) != 0 {
		t.Errorf("linkHosts(nil) = %q", got)
	}
}

// With links (Env.Links, a hand-made list), a VM with network policy
// starts with no link name in its NO_PROXY, as its HTTP clients reach
// links through its proxy, and its setup gets the hosts file and the same
// NO_PROXY; links that aren't as devvm makes them fail the start, before
// the VM starts.
func TestUpLinks(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	f.LifelineFile = filepath.Join(e.Dirs.Lima, "vm", "ha.pid")
	f.ExecFunc = noDirect
	e.Links = func(*state.VM) ([]GuestLink, error) {
		return []GuestLink{{netip.MustParseAddr("127.77.0.2"), []string{"db", "pg"}}}, nil
	}
	if err := (Launch{e}).Up(t.Context(), Options{Network: "allowlist"}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	want := "localhost,127.0.0.1,::1"
	c, _ := call(f, "Start").Args[2].(backend.Confinement)
	if c.Env["NO_PROXY"] != want || c.Env["no_proxy"] != want {
		t.Errorf("Start's env = %q", c.Env)
	}
	got := staged(t, f)
	if got["hosts"] != "127.77.0.2 db db.devvm.internal pg pg.devvm.internal\n" || !strings.Contains(got["vm"], "\nDEVVM_NO_PROXY='"+want+"'\n") ||
		!strings.Contains(got["vm"], "\nDEVVM_LOCAL_PROXY='127.0.77.1:3128'\n") {
		t.Errorf("staged hosts = %q, vm = %q", got["hosts"], got["vm"])
	}
}

// The local proxy's address goes to a VM with a proxy, and none to one without.
func TestLocalProxy(t *testing.T) {
	t.Parallel()
	if got := localProxy("http://devvm:k@192.168.5.2:41000"); got != "127.0.77.1:3128" {
		t.Errorf("localProxy(a proxy) = %q", got)
	}
	if got := localProxy(""); got != "" {
		t.Errorf("localProxy(none) = %q", got)
	}
}

// Links that aren't as devvm makes them stop a VM's start before its
// backend starts it, and its proxy with it.
func TestUpLinksInvalid(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	f.LifelineFile = filepath.Join(e.Dirs.Lima, "vm", "ha.pid")
	f.ExecFunc = noDirect
	e.Links = func(*state.VM) ([]GuestLink, error) {
		return []GuestLink{{netip.MustParseAddr("127.0.0.1"), []string{"db"}}}, nil
	}
	err := Launch{e}.Up(t.Context(), Options{Network: "allowlist"})
	spec, _ := call(f, "CreateVM").Args[1].(backend.VMSpec)
	if msg.NamesOf(err) != "link_setup_invalid address=127.0.0.1 name=db vm="+spec.Name {
		t.Fatalf("Up = %v", msg.NamesOf(err))
	}
	if slices.Contains(f.Methods(), "Start") {
		t.Errorf("calls = %q", f.Methods())
	}
	if _, ok := e.Proxy.PID(spec.Name); ok {
		t.Error("its proxy runs")
	}
}

// An open VM's setup gets the hosts file too, and its links aren't in its
// NO_PROXY: it reaches them through its proxy.
func TestUpLinksNoPolicy(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	e.Links = func(*state.VM) ([]GuestLink, error) {
		return []GuestLink{{netip.MustParseAddr("127.77.0.9"), []string{"api"}}}, nil
	}
	if err := (Launch{e}).Up(t.Context(), Options{Network: "open"}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if c, _ := call(f, "Start").Args[2].(backend.Confinement); c.Env["NO_PROXY"] != netpolicy.NoProxy || !c.GuestProxy() {
		t.Errorf("Start's env = %q", c.Env)
	}
	if got := staged(t, f); got["hosts"] != "127.77.0.9 api api.devvm.internal\n" {
		t.Errorf("staged hosts = %q", got["hosts"])
	}
}

// Every VM's setup is the same for its secrets, open or confined: the
// VM's own CA and its next, which its proxy puts the secrets in with.
func TestStageSecretsEveryTier(t *testing.T) {
	for _, o := range []Options{{Network: "open"}, {Network: "internet"}, {Network: "allowlist"}, {Network: "none"}} {
		e, f, _ := upEnv(t, allNative())
		f.LifelineFile = filepath.Join(e.Dirs.Lima, "vm", "ha.pid")
		f.ExecFunc = noDirect
		if err := (Launch{e}).Up(t.Context(), o); err != nil {
			t.Fatal(msg.NamesOf(err))
		}
		got := staged(t, f)
		for _, name := range []string{"ca/devvm-proxy.crt", "ca/devvm-proxy-next.crt"} {
			if !strings.Contains(got[name], "BEGIN CERTIFICATE") {
				t.Errorf("%+v: staged %s = %q", o, name, got[name])
			}
		}
	}
}

// On every tier, opencode's config folder's files that are neither
// settings nor Markdown stay on the host; every other agent's file goes.
func TestWithheld(t *testing.T) {
	for rel, want := range map[string]bool{
		".config/opencode/.env":          true,
		".config/opencode/secrets.txt":   true,
		".config/opencode/AGENTS.md":     false,
		".config/opencode/opencode.json": false,
		".codex/AGENTS.md":               false,
		".claude/CLAUDE.md":              false,
		".pi/agent/models.json":          false,
		".pi/agent/keybindings.json":     false,
	} {
		if got := withheld(rel); got != want {
			t.Errorf("withheld(%s) = %v, want %v", rel, got, want)
		}
	}
}

// pi's settings files that can hold a key are redacted as JSON, and its
// keybindings and Markdown go as they are; every one of pi's files is
// one of its settings.
func TestPiSettingsFormat(t *testing.T) {
	for rel, want := range map[string]string{
		".pi/agent/settings.json":    "json",
		".pi/agent/models.json":      "json",
		".pi/agent/mcp.json":         "json",
		".pi/agent/keybindings.json": "",
		".pi/agent/AGENTS.md":        "",
		".pi/agent/auth.json":        "",
		".pi/agent/trust.json":       "",
		".pi/settings.json":          "",
	} {
		if got, _ := settingsFormat(rel); got != want {
			t.Errorf("settingsFormat(%s) = %q, want %q", rel, got, want)
		}
	}
	for _, rel := range []string{".pi/agent/settings.json", ".pi/agent/models.json", ".pi/agent/mcp.json"} {
		if !slices.Contains(agentFiles(t.TempDir(), "pi"), rel) {
			t.Errorf("%s isn't among pi's settings files", rel)
		}
	}
	for _, d := range agents.Of("pi").Dirs {
		if strings.HasPrefix(d, piSessions) || !strings.HasPrefix(d, piDir+"/") {
			t.Errorf("pi's settings folder %s", d)
		}
	}
}

// The tool images a VM's report lists: only the VM's own, each once, in
// the report's order; a line that's anything else, or cut short, doesn't
// count. capped keeps the read to its limit.
func TestReported(t *testing.T) {
	node, bun := "node-24.21.0-0123456789ab.erofs", "bun-1.4.2-0123456789ab.erofs"
	files := []string{node, bun}
	cases := []struct {
		name, report string
		want         []string
	}{
		{"none", "", nil},
		{"one", node + "\n", []string{node}},
		{"in its order, once", bun + "\n" + node + "\n" + bun + "\n", []string{bun, node}},
		{"not the VM's", "claude-2.1.295-0123456789ab.erofs\n../x\n\n", nil},
		{"more on the line", node + " x\nx " + node + "\n" + node + "\r\n", nil},
		{"cut short", bun + "\n" + node, []string{bun}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := reported(files, []byte(c.report)); !slices.Equal(got, c.want) {
				t.Errorf("reported = %q, want %q", got, c.want)
			}
		})
	}
	out := &bounded{max: unmountedMax}
	if _, err := out.Write([]byte(strings.Repeat("x\n", unmountedMax) + node + "\n")); err != nil {
		t.Fatal(err)
	}
	if out.Len() != unmountedMax || reported(files, out.Bytes()) != nil {
		t.Errorf("capped kept %d bytes", out.Len())
	}
}

// slowExec is a backend whose Exec writes its report, then holds the call
// until its context ends, as a VM's root can.
type slowExec struct {
	*backendtest.Fake
	report string
	hang   bool
}

func (s slowExec) Exec(ctx context.Context, _ string, _ []string, o backend.ExecOpts) error {
	if _, err := io.WriteString(o.Stdout, s.report); err != nil {
		return err
	}
	if !s.hang {
		return nil
	}
	<-ctx.Done()
	return ctx.Err()
}

// Reading the report gives up after its wait, and then no tool counts;
// a read in time counts.
func TestReadUnmountedWait(t *testing.T) {
	node := "node-24.21.0-0123456789ab.erofs"
	files := []string{node}
	b := slowExec{Fake: backendtest.New("fake", nil), report: node + "\n"}
	if got := readUnmounted(t.Context(), b, "vm", files, "", time.Minute); !slices.Equal(got, files) {
		t.Errorf("in time: %q, want %q", got, files)
	}
	b.hang = true
	start := time.Now()
	if got := readUnmounted(t.Context(), b, "vm", files, "", 50*time.Millisecond); got != nil {
		t.Errorf("hung: %q, want none", got)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("hung read took %s", d)
	}
}

// A setup clears the move it followed, and not one another move recorded
// meanwhile.
func TestFollowed(t *testing.T) {
	e, _, _ := upEnv(t, allNative())
	st := existingVM(t, e, Options{Network: "open"})
	if err := update(t.Context(), st, movedSection, func(m *movedInVM) error { m.From = "/a"; return nil }); err != nil {
		t.Fatal(err)
	}
	for _, sent := range []movedInVM{{}, {From: "/b"}} {
		if err := followed(t.Context(), st, sent); err != nil {
			t.Fatal(err)
		}
		if got, _ := state.ReadVM(e.Dirs, st.Name()); sectionOf[movedInVM](got, movedSection).From != "/a" {
			t.Errorf("followed %+v cleared /a", sent)
		}
	}
	if err := followed(t.Context(), st, movedInVM{From: "/a"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := state.ReadVM(e.Dirs, st.Name()); sectionOf[movedInVM](got, movedSection) != (movedInVM{}) {
		t.Errorf("still recorded: %+v", sectionOf[movedInVM](got, movedSection))
	}
}
