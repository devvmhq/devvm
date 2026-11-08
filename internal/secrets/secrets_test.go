package secrets

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/devvmhq/devvm/internal/agentlogin"
	"github.com/devvmhq/devvm/internal/agents"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/proxy"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/tools"
)

// testStore is a store in a temporary sandbox, its Keychain a map and its
// environment env.
func testStore(t *testing.T, keychain bool, env map[string]string) (*Store, map[string]string) {
	t.Helper()
	root := t.TempDir()
	d := paths.Dirs{Home: filepath.Join(root, "home"), Data: filepath.Join(root, "data"), Config: filepath.Join(root, "config")}
	s := NewStore(d, &msg.Fake{OnWarn: func(m msg.M) { t.Log(m.Names()) }})
	kc := map[string]string{}
	s.Keychain = keychain
	s.KeychainGet = func(_ context.Context, service, account string) (string, error) {
		v, ok := kc[service+"/"+account]
		if !ok {
			return "", platform.ErrNoSecret
		}
		return v, nil
	}
	s.KeychainSet = func(_ context.Context, service, account, label, value string) error {
		if label != "devvm "+account {
			t.Errorf("label %q for %s", label, account)
		}
		kc[service+"/"+account] = value
		return nil
	}
	s.KeychainDelete = func(_ context.Context, service, account string) error {
		if _, ok := kc[service+"/"+account]; !ok {
			return platform.ErrNoSecret
		}
		delete(kc, service+"/"+account)
		return nil
	}
	s.Getenv = func(k string) string { return env[k] }
	return s, kc
}

// A token is kept without whitespace; one that's empty, would end the
// Keychain's command line or a header, or for claude isn't a setup-token
// token, is refused.
func TestToken(t *testing.T) {
	for _, tc := range []struct{ name, given, want, err string }{
		{"github", " ghp_x \n", "ghp_x", ""},
		{"github", "ghp_ x\t", "ghp_x", ""},
		{"claude", "sk-ant-oat01-abc\r\n", "sk-ant-oat01-abc", ""},
		{"github", "  \n", "", "no_token"},
		{"github", `ghp_x" -A -T "`, "", "token_quotes"},
		{"github", `ghp_x\`, "", "token_quotes"},
		{"github", "ghp_\x00x", "", "token_quotes"},
		{"github", "ghp_\x7fx", "", "token_quotes"},
		{"claude", "some-browser-code", "", "not_setup_token"},
		// The company proxy's password keeps its spaces inside.
		{"proxy", " pa55 word \n", "pa55 word", ""},
		{"proxy", "pa55\tword", "", "token_quotes"},
		{"proxy", `pa55"`, "", "token_quotes"},
		{"proxy", " \n", "", "no_token"},
	} {
		v, err := Token(tc.name, tc.given)
		if v != tc.want || msg.NamesOf(err) != tc.err {
			t.Errorf("Token(%s, %q) = %q, %v", tc.name, tc.given, v, msg.NamesOf(err))
		}
	}
}

// Save and Delete: the Keychain under devvm's service, else a file only
// the user reads in a folder only the user opens; only devvm's names.
func TestSaveDelete(t *testing.T) {
	ctx := t.Context()
	s, kc := testStore(t, true, map[string]string{"DEVVM_SECRET_SERVICE": "devvm-test"}) //nolint:gosec // a service name
	if err := s.Save(ctx, "github", "ghp_x"); err != nil || kc["devvm-test/github"] != "ghp_x" {
		t.Errorf("Save: %v, %q", err, kc)
	}
	if v, _ := s.Value(ctx, "github"); v != "ghp_x" {
		t.Errorf("Value after Save = %q", v)
	}
	if err := s.Delete(ctx, "github"); err != nil || len(kc) != 0 {
		t.Errorf("Delete: %v, %q", err, kc)
	}
	if err := s.Delete(ctx, "github"); !errors.Is(err, ErrNone) {
		t.Errorf("Delete again: %v", err)
	}
	s.KeychainSet = func(context.Context, string, string, string, string) error { return errors.New("denied") }
	if err := s.Save(ctx, "claude", "sk-ant-oat01-x"); msg.NamesOf(err) != "keychain_save_failed secret=claude" {
		t.Errorf("a failed save: %v", msg.NamesOf(err))
	}
	for _, name := range []string{"../../x", "nope"} {
		if err := s.Save(ctx, name, "v"); msg.NamesOf(err) != "no_secret secret="+msg.OneLine(name) {
			t.Errorf("Save(%s): %v", name, msg.NamesOf(err))
		}
		if err := s.Delete(ctx, name); !errors.Is(err, ErrNone) {
			t.Errorf("Delete(%s): %v", name, err)
		}
	}

	f, _ := testStore(t, false, nil)
	// One made too open is made the user's alone.
	if err := os.MkdirAll(f.Dirs.Secrets(), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := f.Save(ctx, "github", "ghp_file"); err != nil {
		t.Fatal(err)
	}
	if v, _ := f.Value(ctx, "github"); v != "ghp_file" {
		t.Errorf("file Value = %q", v)
	}
	for path, want := range map[string]os.FileMode{f.Dirs.Secrets(): 0o700, f.File("github"): 0o600} {
		if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != want {
			t.Errorf("%s: %v, %v", path, fi.Mode(), err)
		}
	}
	if err := f.Delete(ctx, "github"); err != nil {
		t.Errorf("file Delete: %v", err)
	}
	if err := f.Delete(ctx, "github"); !errors.Is(err, ErrNone) {
		t.Errorf("file Delete again: %v", err)
	}
}

// newVM writes the state of a VM named name with these shared tools, as
// up does.
func newVM(t *testing.T, d paths.Dirs, name string, names ...string) *state.VM {
	t.Helper()
	st, err := state.New(d, name)
	if err != nil {
		t.Fatal(err)
	}
	st.Image, st.Dir, st.Backend = "base", "/p", "fake"
	if err := tools.Set(st, tools.InVM{Names: names}); err != nil {
		t.Fatal(err)
	}
	if err := st.Create(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	return st
}

// bind binds secrets to a VM, those it had replaced, as devvm does.
func bind(t *testing.T, st *state.VM, names ...string) {
	t.Helper()
	err := st.Update(t.Context(), func(st *state.VM) error { return state.SetSection(st, Section, Bound{Names: names}) })
	if err != nil {
		t.Fatal(err)
	}
}

// Bind adds a secret to a VM's own list once, and takes it out; claude
// stays with an image that has it.
func TestBind(t *testing.T) {
	d := paths.Dirs{Data: t.TempDir()}
	st := newVM(t, d, vm1, "node", "claude")
	for _, step := range []struct {
		name string
		use  bool
		want string
	}{{"github", true, "github"}, {"github", true, "github"}, {"claude", true, "github claude"}, {"github", false, "claude"}, {"claude", false, ""}} {
		err := st.Update(t.Context(), func(st *state.VM) error { return Bind(st, step.name, step.use) })
		b, _ := Of(st)
		if got := strings.Join(b.Names, " "); err != nil || got != step.want {
			t.Errorf("Bind(%s, %v): %q, %v", step.name, step.use, got, err)
		}
	}
	if !slices.Equal(Names(st), []string{"claude"}) || !FromTools(st, "claude") || FromTools(st, "github") {
		t.Errorf("Names = %q", Names(st))
	}
}

func TestValue(t *testing.T) {
	ctx := t.Context()
	s, kc := testStore(t, true, map[string]string{})
	if _, err := s.Value(ctx, "github"); !errors.Is(err, ErrNone) {
		t.Errorf("no value: %v", err)
	}
	kc["devvm/github"] = "ghp_x\n"
	if v, err := s.Value(ctx, "github"); v != "ghp_x" || err != nil {
		t.Errorf("Keychain: %q, %v", v, err)
	}
	// DEVVM_SECRET_SERVICE keeps test secrets apart, and is plain.
	env := map[string]string{}
	s.Getenv = func(k string) string { return env[k] }
	env["DEVVM_SECRET_SERVICE"] = "devvm-test"
	if _, err := s.Value(ctx, "github"); !errors.Is(err, ErrNone) {
		t.Errorf("another service's: %v", err)
	}
	env["DEVVM_SECRET_SERVICE"] = "x -a y"
	if _, err := s.Value(ctx, "github"); msg.NamesOf(err) != "bad_secret_service" {
		t.Errorf("a service with spaces: %v", msg.NamesOf(err))
	}
	// A claude token in the environment wins.
	delete(env, "DEVVM_SECRET_SERVICE")
	env["CLAUDE_CODE_OAUTH_TOKEN"] = "sk-ant-oat01-env"
	kc["devvm/claude"] = "sk-ant-oat01-kc"
	if v, _ := s.Value(ctx, "claude"); v != "sk-ant-oat01-env" {
		t.Errorf("claude = %q", v)
	}
	// Without a Keychain, a file only the user reads.
	f, _ := testStore(t, false, nil)
	if err := os.MkdirAll(f.Dirs.Secrets(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.File("github"), []byte("ghp_file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if v, err := f.Value(ctx, "github"); v != "ghp_file" || err != nil {
		t.Errorf("file: %q, %v", v, err)
	}
	if _, err := f.Value(ctx, "claude"); !errors.Is(err, ErrNone) {
		t.Errorf("no file: %v", err)
	}
}

// The company proxy's password: its value, or the message saying to save
// it.
func TestProxyPassword(t *testing.T) {
	ctx := t.Context()
	s, kc := testStore(t, true, map[string]string{})
	if _, err := s.ProxyPassword(ctx, proxy.UpstreamSecret); msg.NamesOf(err) != "proxy_password_missing" {
		t.Errorf("none saved: %v", msg.NamesOf(err))
	}
	kc["devvm/proxy"] = " pa55 word\n"
	if v, err := s.ProxyPassword(ctx, proxy.UpstreamSecret); v != "pa55 word" || err != nil {
		t.Errorf("saved: %q, %v", v, err)
	}
}

// secretOf is the binding of a secret devvm keeps, as the proxy has it.
func secretOf(name string) proxy.Binding { return proxy.Binding{Name: name, Kind: proxy.KindSecret} }

// loginOf is agent's login binding name, as the proxy has it.
func loginOf(agent, name string) proxy.Binding {
	return proxy.Binding{Name: name, Kind: proxy.KindLogin, Agent: agent}
}

// Get: a known secret's value, a login's from its agent's file in the
// user's home, by the binding's kind, and nothing else (a name is never a
// path, and never decides the kind).
func TestGet(t *testing.T) {
	s, kc := testStore(t, true, nil)
	kc["devvm/github"] = "ghp_x"
	ctx := t.Context()
	if v, err := s.Get(ctx, secretOf("github")); v != "ghp_x" || err != nil {
		t.Errorf("github = %q, %v", v, err)
	}
	// A login whose file isn't there fails, naming the file.
	if _, err := s.Get(ctx, loginOf("codex", "codex-key")); msg.IDOf(err) != "login_read_failed" {
		t.Errorf("Get(codex-key) without a login = %v", msg.NamesOf(err))
	}
	for kind, data := range map[string]string{
		"codex":    `{"OPENAI_API_KEY": "sk-codex-value"}`,
		"opencode": `{"anthropic": {"type": "api", "key": "sk-ant-value"}}`,
		"claude":   `{"mcpServers": {"s": {"type": "http", "url": "https://mcp.example.com/", "headers": {"X-Key": "mcp-value"}}}}`,
	} {
		writeLogin(t, s, kind, data)
	}
	mcp, err := agentlogin.File{Kind: agents.Claude, Path: LoginFile(s.Dirs, "claude")}.Bindings()
	if err != nil || len(mcp) != 1 {
		t.Fatalf("MCP bindings = %+v, %v", mcp, err)
	}
	for _, tc := range []struct {
		b    proxy.Binding
		want string
	}{
		{loginOf("codex", "codex-key"), "sk-codex-value"},
		{loginOf("opencode", "opencode-anthropic"), "sk-ant-value"},
		{mcp[0], "mcp-value"},
	} {
		if v, err := s.Get(ctx, tc.b); v != tc.want || err != nil {
			t.Errorf("Get(%+v) = %q, %v", tc.b, v, err)
		}
	}
	// Codex's file has no ChatGPT login: the message says to log in again.
	if _, err := s.Get(ctx, loginOf("codex", "codex")); msg.NamesOf(err) != "codex_no_chatgpt" {
		t.Errorf("Get(codex) = %v", msg.NamesOf(err))
	}
	// No Claude Code login of either kind.
	if _, err := s.Get(ctx, secretOf("claude")); msg.NamesOf(err) != "claude_no_login" {
		t.Errorf("Get(claude) = %v", msg.NamesOf(err))
	}
	for _, b := range []proxy.Binding{
		secretOf("../../etc/passwd"), secretOf("nope"),
		// A secret's name as a login, or a login's as a secret: neither is looked up.
		loginOf("github", "github"), loginOf("../../etc", "codex-key"), secretOf("codex-key"),
		{Name: "github", Kind: "env"},
	} {
		if _, err := s.Get(ctx, b); msg.NamesOf(err) != `no_secret secret=`+msg.OneLine(b.Name) {
			t.Errorf("Get(%+v) = %v", b, msg.NamesOf(err))
		}
	}
}

// A login's failure that logging in again fixes gets its own message.
func TestLoginFailed(t *testing.T) {
	cause := errors.New("401 Unauthorized")
	for _, tc := range []struct {
		err  error
		want string
	}{
		{agentlogin.ErrCodexNoKey, "codex_no_key"},
		{agentlogin.ErrCodexNoChatGPT, "codex_no_chatgpt"},
		{&agentlogin.RefreshError{Agent: agents.Codex, Err: cause}, "codex_refresh_failed reason=401 Unauthorized"},
		{&agentlogin.RefreshError{Agent: agents.Opencode, Provider: "openai", Err: cause}, "opencode_refresh_failed provider=openai reason=401 Unauthorized"},
		{&agentlogin.RefreshError{Agent: agents.Claude, Err: cause}, "claude_refresh_failed reason=401 Unauthorized"},
		{cause, "login_read_failed file=/h/auth.json reason=401 Unauthorized"},
	} {
		m := loginFailed("/h/auth.json", tc.err)
		if m.Names() != tc.want || !errors.Is(m, tc.err) {
			t.Errorf("loginFailed(%v) = %s, want %s", tc.err, m.Names(), tc.want)
		}
	}
}

// A VM uses the secrets bound to it, and claude when its image has it;
// its logins are its agents' with a login file.
func TestNamesAndLogins(t *testing.T) {
	s, _ := testStore(t, true, nil)
	d := paths.Dirs{Data: t.TempDir()}
	st := newVM(t, d, vm1, "node", "claude")
	if got := Names(st); !slices.Equal(got, []string{"claude"}) {
		t.Errorf("Names = %q", got)
	}
	bind(t, st, "github")
	if got := Names(st); !slices.Equal(got, []string{"github", "claude"}) {
		t.Errorf("Names = %q", got)
	}
	// The company proxy's password is never a VM's, even bound.
	bind(t, st, "github", "proxy")
	if got := Names(st); !slices.Equal(got, []string{"github", "claude"}) {
		t.Errorf("Names with proxy bound = %q", got)
	}
	if got := s.Logins(t.Context(), []string{"node", "codex", "claude"}); got != nil {
		t.Errorf("Logins without files = %q", got)
	}
	for _, k := range agents.LoginKinds() {
		f := LoginFile(s.Dirs, k)
		if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Claude Code's OAuth login file holds no login. The kinds come in the agents' order.
	if got := s.Logins(t.Context(), []string{"node", "codex", "claude"}); !slices.Equal(got, []string{"claude", "codex"}) {
		t.Errorf("Logins = %q", got)
	}
	if got := s.Logins(t.Context(), []string{"node"}); got != nil {
		t.Errorf("Logins without agents = %q", got)
	}
}

// A login the proxy captured goes in the store: Codex's in its auth.json,
// Claude Code's in its .credentials.json, the agent's logins folder kept out
// of backups; an agent devvm keeps no logins of is refused. A VM's
// allowlist has its agents' token endpoints' hosts.
// An API key saved in the store, its folder kept out of backups, changes
// the login's sum; the store's ChatGPT login is read as it is; another
// agent is refused.
func TestSaveKey(t *testing.T) {
	s, _ := testStore(t, false, nil)
	var excluded []string
	s.ExcludeFromBackup = func(_ context.Context, dir string) error { excluded = append(excluded, dir); return nil }
	if s.LoginSum("codex") != "" {
		t.Error("a sum without a login")
	}
	if err := s.SaveKey(t.Context(), "codex", "", "sk-real"); err != nil {
		t.Fatal(err)
	}
	sum := s.LoginSum("codex")
	if b, err := os.ReadFile(LoginFile(s.Dirs, "codex")); err != nil || !strings.Contains(string(b), "sk-real") || sum == "" {
		t.Errorf("the store's Codex login: %s, %v, sum %q", b, err, sum)
	}
	if _, ok, err := s.ChatGPT("codex"); ok || err != nil {
		t.Errorf("an API key as a ChatGPT login: %v, %v", ok, err)
	}
	if err := s.SaveKey(t.Context(), "codex", "", "sk-other"); err != nil || s.LoginSum("codex") == sum {
		t.Errorf("a new key: %v, the same sum", err)
	}
	if !slices.Equal(excluded, []string{s.Dirs.AgentLogins("codex"), s.Dirs.AgentLogins("codex")}) {
		t.Errorf("kept out of backups: %q", excluded)
	}
	if err := s.SaveKey(t.Context(), "aider", "", "k"); msg.NamesOf(err) != "no_secret secret=aider" {
		t.Errorf("another agent: %v", msg.NamesOf(err))
	}
}

func TestCapture(t *testing.T) {
	s, _ := testStore(t, false, nil)
	var excluded []string
	s.ExcludeFromBackup = func(_ context.Context, dir string) error { excluded = append(excluded, dir); return nil }
	code := []byte("grant_type=authorization_code&code=c")
	if b, answer, err := s.CaptureRequest("codex", "salt", "", code); err != nil || string(b) != string(code) || answer != nil {
		t.Errorf("CaptureRequest = %q, %q, %v", b, answer, err)
	}
	tok := "eyJhbGciOiJub25lIn0.eyJleHAiOjF9.sig"
	reply := []byte(`{"id_token":"` + tok + `","access_token":"` + tok + `","refresh_token":"rt-real"}`)
	out, err := s.Capture(t.Context(), "codex", "salt", "", code, reply)
	if err != nil || strings.Contains(string(out), "rt-real") {
		t.Fatalf("Capture = %s, %v", out, err)
	}
	if b, err := os.ReadFile(LoginFile(s.Dirs, "codex")); err != nil || !strings.Contains(string(b), "rt-real") || !s.HasLogin("codex") {
		t.Errorf("the store's Codex login: %s, %v", b, err)
	}
	claude := []byte(`{"grant_type":"authorization_code","code":"c"}`)
	if _, err := s.Capture(t.Context(), "claude", "salt", "application/json", claude, []byte(`{"access_token":"sk-ant-oat01-real","refresh_token":"rt-real","scope":"user:inference"}`)); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(s.Dirs.AgentLogins("claude"), ".claude", ".credentials.json")); err != nil || !strings.Contains(string(b), "claudeAiOauth") {
		t.Errorf("the store's Claude Code login: %s, %v", b, err)
	}
	if !slices.Equal(excluded, []string{s.Dirs.AgentLogins("codex"), s.Dirs.AgentLogins("claude")}) {
		t.Errorf("kept out of backups: %q", excluded)
	}
	for _, f := range []func() error{
		func() error { _, _, err := s.CaptureRequest("aider", "salt", "", code); return err },
		func() error { _, err := s.Capture(t.Context(), "aider", "salt", "", code, reply); return err },
	} {
		if err := f(); msg.NamesOf(err) != "no_secret secret=aider" {
			t.Errorf("another agent: %v", msg.NamesOf(err))
		}
	}
	if got := loginHosts(newVM(t, s.Dirs, vm1, "node", "codex", "claude")); !slices.Equal(got, []string{"auth.openai.com", "platform.claude.com", "console.anthropic.com"}) {
		t.Errorf("loginHosts = %q", got)
	}
	if got := loginHosts(newVM(t, s.Dirs, vm2, "node")); got != nil {
		t.Errorf("loginHosts without agents = %q", got)
	}
}

// The proxy's view of a VM's secrets: bindings for those with a value and
// for its logins (no values), and their hosts on 443.
func TestWriteFiles(t *testing.T) {
	s, kc := testStore(t, true, nil)
	kc["devvm/github"] = "ghp_secret"
	d := s.Dirs
	st := newVM(t, d, vm1, "node", "codex", "claude")
	bind(t, st, "github")
	auth := LoginFile(s.Dirs, "codex")
	if err := os.MkdirAll(filepath.Dir(auth), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(auth, []byte(`{"OPENAI_API_KEY":"sk-codex-secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	salt := strings.Repeat("ab", 16)
	if err := s.WriteFiles(t.Context(), st, salt); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(d.VM(vm1).Secrets())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "ghp_secret") || strings.Contains(string(data), "sk-codex-secret") {
		t.Errorf("a value in the bindings: %s", data)
	}
	l, err := proxy.ReadBindings(d.VM(vm1).Secrets())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, b := range l.Bindings {
		got = append(got, b.Name+" "+b.Kind+b.Agent+" "+strings.Join(b.Hosts, ",")+" "+b.Header+" "+b.Placeholder)
	}
	// claude has no value: no binding, but its host still comes with it.
	want := []string{
		"github secret api.github.com,github.com,codeload.github.com,raw.githubusercontent.com Authorization devvm-proxy-managed-github-" + salt,
	}
	if len(got) != 2 || got[0] != want[0] || !strings.HasPrefix(got[1], "codex-key logincodex api.openai.com Authorization ") || !strings.Contains(got[1], salt) {
		t.Errorf("bindings = %q", got)
	}
	pol, err := os.ReadFile(d.VM(vm1).SecretsPolicy())
	if err != nil {
		t.Fatal(err)
	}
	// Codex's token endpoint, which its proxy captures logins at, is allowed too.
	wantPol := "api.github.com:443\ngithub.com:443\ncodeload.github.com:443\nraw.githubusercontent.com:443\napi.anthropic.com:443\napi.openai.com:443\n" +
		"auth.openai.com:443\nplatform.claude.com:443\nconsole.anthropic.com:443\n"
	if string(pol) != wantPol {
		t.Errorf("policy = %q, want %q", pol, wantPol)
	}
	for _, f := range []string{d.VM(vm1).Secrets(), d.VM(vm1).SecretsPolicy()} {
		if fi, err := os.Stat(f); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v", f, fi.Mode(), err)
		}
	}
	// A refresh keeps the logins' bindings and hosts as written, without
	// reading any login file, and follows the secrets: unbound, github's
	// binding and hosts go. The login file is gone: had the refresh read
	// it, codex-key would have gone too.
	if err := os.Remove(auth); err != nil {
		t.Fatal(err)
	}
	bind(t, st)
	if err := s.RefreshFiles(t.Context(), st, salt); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(d.VM(vm1).Secrets())
	if strings.Contains(string(data), `"github"`) || !strings.Contains(string(data), `"codex-key"`) {
		t.Errorf("refreshed bindings = %s", data)
	}
	if pol, _ := os.ReadFile(d.VM(vm1).SecretsPolicy()); string(pol) != "api.anthropic.com:443\napi.openai.com:443\nauth.openai.com:443\nplatform.claude.com:443\nconsole.anthropic.com:443\n" {
		t.Errorf("refreshed policy = %q", pol)
	}
	// No secrets at all: an empty list.
	none := newVM(t, d, vm2, "node")
	if err := s.WriteFiles(t.Context(), none, salt); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(d.VM(vm2).Secrets()); string(data) != `{"version":1,"bindings":[]}`+"\n" {
		t.Errorf("no bindings = %q", data)
	}
}

// writeLogin writes an agent's login file, data, in devvm's store.
func writeLogin(t *testing.T, s *Store, kind, data string) {
	t.Helper()
	f := LoginFile(s.Dirs, kind)
	if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The VM's copies: a login's with placeholders ending in its salt.
func TestVMCopies(t *testing.T) {
	s, _ := testStore(t, false, nil)
	var warned []string
	s.Reporter = &msg.Fake{OnWarn: func(m msg.M) { warned = append(warned, m.Names()) }}
	writeLogin(t, s, "codex", `{"OPENAI_API_KEY":"sk-codex-secret"}`)
	salt := strings.Repeat("cd", 16)
	data, err := s.VMLogin("codex", salt)
	if err != nil || strings.Contains(string(data), "sk-codex-secret") || !strings.Contains(string(data), salt) || warned != nil {
		t.Errorf("VMLogin = %s, %v; warned %q", data, err, warned)
	}
	// What a copy holds as it is is warned about.
	writeLogin(t, s, "codex", `{"OPENAI_API_KEY":"sk-codex-secret","extra":"x","b":1}`)
	writeLogin(t, s, "opencode", `{"llama":{"type":"api","key":"local"},"other":{"type":"oauth","access":"a","refresh":"r"}}`)
	writeLogin(t, s, "pi", `{"anthropic":{"type":"api_key","key":"sk-ant-pi-secret"},"groq":{"type":"api_key","key":"g"},"openai-codex":{"type":"oauth","access":"a","refresh":"r"}}`)
	for _, kind := range []string{"codex", "opencode", "pi"} {
		if _, err := s.VMLogin(kind, salt); err != nil {
			t.Errorf("VMLogin(%s): %v", kind, err)
		}
	}
	if want := []string{
		"login_kept agent=Codex file=auth.json keys=b, extra",
		"login_withheld agent=opencode name=opencode providers=llama, other", "login_withheld agent=pi name=pi providers=groq, openai-codex",
	}; !slices.Equal(warned, want) {
		t.Errorf("warned %q, want %q", warned, want)
	}
	if f := LoginFile(s.Dirs, "pi"); !strings.HasSuffix(f, filepath.Join("logins", "pi", ".pi", "agent", "auth.json")) {
		t.Errorf("pi's login file is %s", f)
	}
	// config.toml's [pass-keys] gives the VM pi's groq key as it is, which is noted, not warned.
	if err := os.MkdirAll(s.Dirs.Config, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.Dirs.ConfigFile(), []byte("[pass-keys]\npi = [\"groq\"]\nopencode = [\"llama\", \"other\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var noted []string
	warned = nil
	s.Reporter = &msg.Fake{OnWarn: func(m msg.M) { warned = append(warned, m.Names()) }, OnNote: func(m msg.M) { noted = append(noted, m.Names()) }}
	data, err = s.VMLogin("pi", salt)
	switch {
	case err != nil || strings.Contains(string(data), "sk-ant-pi-secret") || !strings.Contains(string(data), "devvm-proxy-managed-pi-anthropic-"+salt):
		t.Errorf("pi's VM copy: %s, %v", data, err)
	case !strings.Contains(string(data), `"key": "g"`):
		t.Errorf("pi's VM copy lacks the passed key: %s", data)
	case !slices.Equal(noted, []string{"login_passed agent=pi providers=groq"}) || !slices.Equal(warned, []string{"login_withheld agent=pi name=pi providers=openai-codex"}):
		t.Errorf("noted %q, warned %q", noted, warned)
	}
	// opencode's list passes its llama key, but never its other OAuth login.
	noted, warned = nil, nil
	data, err = s.VMLogin("opencode", salt)
	switch {
	case err != nil || !strings.Contains(string(data), `"key": "local"`) || strings.Contains(string(data), `"refresh"`):
		t.Errorf("opencode's VM copy: %s, %v", data, err)
	case !slices.Equal(noted, []string{"login_passed agent=opencode providers=llama"}) || !slices.Equal(warned, []string{"login_withheld agent=opencode name=opencode providers=other"}):
		t.Errorf("noted %q, warned %q", noted, warned)
	}
	// A config.toml devvm can't read fails the copy.
	if err := os.WriteFile(s.Dirs.ConfigFile(), []byte("[pass-keys]\npi = [\"Not A Provider\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.VMLogin("pi", salt); err == nil {
		t.Error("VMLogin with a bad [pass-keys]: no error")
	}
}

// Two saves of one secret at once each write it whole: neither fails, and
// the file holds one of their values, with no temporary file left.
func TestSaveAtOnce(t *testing.T) {
	ctx := t.Context()
	s, _ := testStore(t, false, nil)
	var wg sync.WaitGroup
	errs := make(chan error, 200)
	for i := range 4 {
		wg.Go(func() {
			for range 50 {
				errs <- s.Save(ctx, "github", "ghp_"+strconv.Itoa(i))
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Save: %v", err)
		}
	}
	if v, _ := s.Value(ctx, "github"); !slices.Contains([]string{"ghp_0", "ghp_1", "ghp_2", "ghp_3"}, v) {
		t.Errorf("Value = %q", v)
	}
	if left := dirNames(t, s.Dirs.Secrets()); !slices.Equal(left, []string{"github"}) {
		t.Errorf("the secrets folder holds %q", left)
	}
	if fi, err := os.Stat(s.File("github")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("the secret's file: %v, %v", fi, err)
	}
	// A save that never finished left a temporary file: Delete takes it too.
	if err := os.WriteFile(filepath.Join(s.Dirs.Secrets(), ".github.tmp-123"), []byte("ghp_x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "github"); err != nil {
		t.Fatal(err)
	}
	if left := dirNames(t, s.Dirs.Secrets()); len(left) > 0 {
		t.Errorf("after Delete, the secrets folder holds %q", left)
	}
}

// A VM's files that can't be written leave no temporary file behind.
func TestWriteFilesFailing(t *testing.T) {
	s, kc := testStore(t, true, nil)
	kc["devvm/github"] = "ghp_secret"
	st := newVM(t, s.Dirs, vm1)
	bind(t, st, "github")
	// A folder where the bindings go: renaming over it fails.
	in := filepath.Join(s.Dirs.VM(vm1).Secrets(), "x")
	if err := os.MkdirAll(in, 0o700); err != nil {
		t.Fatal(err)
	}
	var le *os.LinkError
	if err := s.WriteFiles(t.Context(), st, strings.Repeat("ab", 16)); !errors.As(err, &le) || le.New != s.Dirs.VM(vm1).Secrets() {
		t.Fatalf("WriteFiles over a folder: %v; want its rename failing", err)
	}
	if left := dirNames(t, s.Dirs.VM(vm1).Run()); !slices.Equal(left, []string{filepath.Base(s.Dirs.VM(vm1).Secrets())}) {
		t.Errorf("the run folder holds %q", left)
	}
}

// Two VMs' names, for tests of their secret files.
const (
	vm1 = "devvm-vm1-0123456789abc1"
	vm2 = "devvm-vm2-0123456789abc2"
)

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}
