package netpolicy

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/proxy"
	"github.com/devvmhq/devvm/internal/secrets"
	"github.com/devvmhq/devvm/internal/state"
)

// Every VM's proxy captures logins at OpenAI's and Claude's token endpoints, on every tier,
// whatever its image holds: its config names them for Codex, opencode and Claude Code. In a Codex VM, the capture is
// Codex's, whatever sends it, and it saves the login in devvm's store, gives the VM its
// placeholders, and writes the VM's bindings again, so the proxy puts the login in at once.
func TestCapturer(t *testing.T) {
	d, st := sandbox(t, "base", "codex")
	p, _ := testProxy(t, d)
	both := []proxy.Capture{
		{Agent: "codex", Host: "auth.openai.com", Path: "/oauth/token"},
		{Agent: "opencode", Host: "auth.openai.com", Path: "/oauth/token"},
		{Agent: "claude", Host: "platform.claude.com", Path: "/v1/oauth/token"},
		{Agent: "claude", Host: "console.anthropic.com", Path: "/v1/oauth/token"},
	}
	for _, vm := range []*state.VM{st, newVM(t, d, "devvm-plain-0123456789abc4", "base")} {
		for _, tier := range []Tier{TierAllowlist, TierInternet, TierNone, TierOpen} {
			cfg, err := p.config(t.Context(), vm, tier, "k3y", "")
			if err != nil || !slices.Equal(cfg.Captures, both) {
				t.Errorf("%s, %s: captures %+v, %v", vm.Name(), tier, cfg.Captures, err)
			}
		}
	}
	salt, err := Salt(t.Context(), st)
	must(t, err)
	c := Capturer{Dirs: d, Secrets: p.Secrets}
	req := []byte(url.Values{"grant_type": {"authorization_code"}, "code": {"c"}}.Encode())
	x := proxy.Exchange{VM: st.Name(), Agents: []string{"codex", "opencode"}, UserAgent: "Bun/1.2", ContentType: "application/x-www-form-urlencoded", Body: req}
	if b, answer, err := c.Request(t.Context(), x); err != nil || string(b) != string(req) || answer != nil {
		t.Fatalf("Request = %q, %q, %v", b, answer, err)
	}
	// A JWT-shaped token, as OpenAI's are.
	tok := "eyJhbGciOiJub25lIn0.eyJleHAiOjF9.sig"
	reply, _ := json.Marshal(map[string]string{"id_token": tok, "access_token": tok + "-at", "refresh_token": "rt-real"})
	out, agent, err := c.Response(t.Context(), x, reply)
	if err != nil || agent != "codex" || strings.Contains(string(out), "rt-real") || strings.Contains(string(out), tok) {
		t.Fatalf("Response = %s, %q, %v", out, agent, err)
	}
	if b, err := os.ReadFile(secrets.LoginFile(d, "codex")); err != nil || !strings.Contains(string(b), "rt-real") {
		t.Errorf("the store's login: %s, %v", b, err)
	}
	var vm map[string]any
	must(t, json.Unmarshal(out, &vm))
	l, err := proxy.ReadBindings(d.VM(st.Name()).Secrets())
	if err != nil || len(l.Bindings) != 1 || l.Bindings[0].Name != "codex" || l.Bindings[0].Placeholder != vm["access_token"] {
		t.Fatalf("bindings %+v, %v; the VM's access token %v", l.Bindings, err, vm["access_token"])
	}
	// The VM's own placeholder: its claims end in the VM's salt.
	parts := strings.Split(l.Bindings[0].Placeholder, ".")
	if claims, err := base64.RawURLEncoding.DecodeString(parts[1]); err != nil || !strings.Contains(string(claims), salt) {
		t.Errorf("the placeholder's claims %s, %v; want the salt %s", claims, err, salt)
	}
	if pol, _ := os.ReadFile(d.VM(st.Name()).SecretsPolicy()); !strings.Contains(string(pol), "auth.openai.com:443\n") {
		t.Errorf("the VM's allowlist = %q", pol)
	}
	// No VM by that name, or one with no placeholders yet: nothing is captured.
	other := newVM(t, d, vmV, "base", "codex")
	for _, vm := range []string{"devvm-gone-0123456789abc5", other.Name()} {
		x.VM = vm
		if _, _, err := c.Request(t.Context(), x); err == nil {
			t.Errorf("%s: Request went on", vm)
		}
		if _, _, err := c.Response(t.Context(), x, reply); err == nil {
			t.Errorf("%s: Response saved", vm)
		}
	}
	// Claude Code's login, from a VM whose image hasn't Claude Code: nothing is captured, so it
	// can't replace the login every VM with Claude Code uses.
	x.VM, x.Agents, x.ContentType, x.Body = st.Name(), []string{"claude"}, "application/json", []byte(`{"grant_type":"authorization_code","code":"c"}`)
	refused := "login_capture_refused agent=claude vm=" + st.Name()
	if _, _, err := c.Request(t.Context(), x); msg.NamesOf(err) != refused {
		t.Errorf("Claude Code's login from a VM without it: %v", msg.NamesOf(err))
	}
	claudeReply := []byte(`{"access_token":"sk-ant-oat01-x","refresh_token":"r","scope":"user:inference"}`)
	if _, _, err := c.Response(t.Context(), x, claudeReply); msg.NamesOf(err) != refused {
		t.Errorf("Claude Code's login from a VM without it was saved: %v", msg.NamesOf(err))
	}
	cl := newVM(t, d, "devvm-claude-0123456789abc6", "base", "claude")
	_, err = Salt(t.Context(), cl)
	must(t, err)
	x.VM = cl.Name()
	if _, agent, err := c.Response(t.Context(), x, claudeReply); err != nil || agent != "claude" {
		t.Errorf("Claude Code's login from its VM: %q, %v", agent, err)
	}
	// An agent the VM's image doesn't have, such as one devvm keeps no logins of.
	x.VM, x.Agents = st.Name(), []string{"aider"}
	if _, _, err := c.Request(t.Context(), x); msg.IDOf(err) != "login_capture_refused" {
		t.Errorf("another agent: %v", msg.NamesOf(err))
	}
}

// A VM captures a login only for an agent its image has, so a VM can't replace the store's
// login of an agent it doesn't have, which every VM with that agent uses. Where the endpoint is
// shared by both of the VM's agents, the User-Agent picks one of them.
func TestCaptureOwnAgents(t *testing.T) {
	d, plain := sandbox(t, "base")
	p, _ := testProxy(t, d)
	c := Capturer{Dirs: d, Secrets: p.Secrets}
	opencode := newVM(t, d, "devvm-oc-0123456789abc7", "base", "opencode")
	codex := newVM(t, d, "devvm-cx-0123456789abc8", "base", "codex")
	both := newVM(t, d, "devvm-both-0123456789abc9", "base", "codex", "opencode")
	for _, vm := range []*state.VM{plain, opencode, codex, both} {
		_, err := Salt(t.Context(), vm)
		must(t, err)
	}
	const codexUA, bunUA = "codex_cli_rs/0.166.0", "Bun/1.2"
	tok := "eyJhbGciOiJub25lIn0.eyJleHAiOjF9.sig"
	reply, _ := json.Marshal(map[string]string{"id_token": tok, "access_token": tok + "-at", "refresh_token": "rt-real"})
	req := []byte(url.Values{"grant_type": {"authorization_code"}, "code": {"c"}}.Encode())
	for _, tc := range []struct {
		vm      *state.VM
		ua      string
		agent   string // the login saved, or "" when refused
		refused string // the agent the refusal names
	}{
		{vm: plain, ua: codexUA, refused: "codex"},
		{vm: plain, ua: bunUA, refused: "opencode"},
		{vm: opencode, ua: codexUA, agent: "opencode"},
		{vm: opencode, ua: bunUA, agent: "opencode"},
		{vm: codex, ua: bunUA, agent: "codex"},
		{vm: both, ua: bunUA, agent: "opencode"},
		{vm: both, ua: codexUA, agent: "codex"},
	} {
		for _, a := range []string{"codex", "opencode"} {
			_ = os.Remove(secrets.LoginFile(d, a))
		}
		x := proxy.Exchange{VM: tc.vm.Name(), Agents: []string{"codex", "opencode"}, UserAgent: tc.ua, ContentType: "application/x-www-form-urlencoded", Body: req}
		_, _, reqErr := c.Request(t.Context(), x)
		_, agent, err := c.Response(t.Context(), x, reply)
		if tc.agent == "" {
			want := "login_capture_refused agent=" + tc.refused + " vm=" + tc.vm.Name()
			if msg.NamesOf(reqErr) != want || msg.NamesOf(err) != want {
				t.Errorf("%s, %s: Request %v, Response %v; want %s", tc.vm.Name(), tc.ua, msg.NamesOf(reqErr), msg.NamesOf(err), want)
			}
		} else if reqErr != nil || err != nil || agent != tc.agent {
			t.Errorf("%s, %s: Request %v, Response %q, %v; want %s", tc.vm.Name(), tc.ua, reqErr, agent, err, tc.agent)
		}
		for _, a := range []string{"codex", "opencode"} {
			if _, err := os.Stat(secrets.LoginFile(d, a)); (err == nil) != (a == tc.agent) {
				t.Errorf("%s, %s: the store's %s login: %v", tc.vm.Name(), tc.ua, a, err)
			}
		}
	}
}
