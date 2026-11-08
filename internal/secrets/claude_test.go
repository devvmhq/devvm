package secrets

import (
	"context"
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/agentlogin"
	"github.com/devvmhq/devvm/internal/agents"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/proxy"
	"github.com/devvmhq/devvm/internal/state"
)

// The Claude Code OAuth login in the store, its access token fresh.
func writeClaudeOAuth(t *testing.T, s *Store, access string) {
	t.Helper()
	exp := strconv.FormatInt(time.Now().Add(time.Hour).UnixMilli(), 10)
	writeLogin(t, s, agents.ClaudeOAuth, `{"claudeAiOauth":{"accessToken":"`+access+`","refreshToken":"sk-ant-ort01-r","expiresAt":`+exp+`,"scopes":["user:inference"]}}`)
}

func bindingNames(t *testing.T, s *Store, st *state.VM) []string {
	t.Helper()
	l, err := proxy.ReadBindings(s.Dirs.VM(st.Name()).Secrets())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, b := range l.Bindings {
		names = append(names, b.Name)
	}
	return names
}

// devvm keeps one Claude Code login, the newest: setup-token's token, captured, is the claude
// secret, and takes the OAuth login away; an OAuth login, captured, takes the saved token away;
// devvm secret set claude takes the OAuth login away too. A token in devvm's environment wins.
func TestOneClaudeLogin(t *testing.T) {
	ctx := t.Context()
	s, kc := testStore(t, true, nil)
	if l, err := ClaudeOf(s).Login(ctx); l != "" || err != nil {
		t.Errorf("no login: %q, %v", l, err)
	}
	login := []byte(`{"grant_type":"authorization_code","code":"c","client_id":"x"}`)
	if _, err := s.Capture(ctx, "claude", "salt", "application/json", login,
		[]byte(`{"access_token":"sk-ant-oat01-oauth","refresh_token":"sk-ant-ort01-r","scope":"user:inference user:profile"}`)); err != nil {
		t.Fatal(err)
	}
	if l, err := ClaudeOf(s).Login(ctx); l != "oauth" || err != nil || !s.HasLogin(agents.ClaudeOAuth) {
		t.Errorf("after the OAuth login: %q, %v", l, err)
	}
	oauth, token, err := ClaudeOf(s).Sums(ctx)
	if oauth == "" || token != "" || err != nil {
		t.Errorf("ClaudeSums = %q, %q, %v", oauth, token, err)
	}
	setup := []byte(`{"grant_type":"authorization_code","code":"c","client_id":"x","expires_in":31536000}`)
	out, err := s.Capture(ctx, "claude", "s417", "application/json", setup, []byte(`{"access_token":"sk-ant-oat01-setup","expires_in":31536000}`))
	if err != nil || !strings.Contains(string(out), `"sk-ant-oat01-devvm-proxy-managed-s417"`) || strings.Contains(string(out), "setup") {
		t.Fatalf("setup-token's capture = %s, %v", out, err)
	}
	if kc["devvm/claude"] != "sk-ant-oat01-setup" || s.HasLogin(agents.ClaudeOAuth) {
		t.Errorf("after setup-token: %q, OAuth login kept %v", kc["devvm/claude"], s.HasLogin(agents.ClaudeOAuth))
	}
	if l, _ := ClaudeOf(s).Login(ctx); l != "token" {
		t.Errorf("after setup-token: %q", l)
	}
	if o2, t2, _ := ClaudeOf(s).Sums(ctx); o2 != "" || t2 == "" {
		t.Errorf("ClaudeSums after setup-token = %q, %q", o2, t2)
	}
	// A reply that isn't a Claude Code token saves nothing.
	if _, err := s.Capture(ctx, "claude", "s417", "application/json", setup, []byte(`{"access_token":"not-a-token"}`)); err == nil {
		t.Error("a token that isn't Claude Code's was saved")
	}
	if _, err := s.Capture(ctx, "claude", "s417", "application/json", setup, []byte(`{"access_token":"sk-ant-oat01-a\"b"}`)); msg.IDOf(err) != "token_quotes" {
		t.Errorf("a token with a quote: %v", err)
	}
	// The OAuth login again: the saved token goes.
	if _, err := s.Capture(ctx, "claude", "salt", "application/json", login,
		[]byte(`{"access_token":"sk-ant-oat01-oauth2","refresh_token":"sk-ant-ort01-r","scope":"user:inference"}`)); err != nil {
		t.Fatal(err)
	}
	if _, ok := kc["devvm/claude"]; ok {
		t.Error("the saved token stayed")
	}
	// devvm secret set claude: the OAuth login goes.
	if err := s.Save(ctx, "claude", "sk-ant-oat01-pasted"); err != nil || s.HasLogin(agents.ClaudeOAuth) {
		t.Errorf("Save: %v, OAuth login kept %v", err, s.HasLogin(agents.ClaudeOAuth))
	}
	// The environment's token wins, but the sums are the saved ones'.
	writeClaudeOAuth(t, s, "sk-ant-oat01-oauth3")
	delete(kc, "devvm/claude")
	env := map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat01-env"} //nolint:gosec // a test's fake token
	s.Getenv = func(k string) string { return env[k] }
	if l, _ := ClaudeOf(s).Login(ctx); l != "token" || ClaudeOf(s).Env() != "sk-ant-oat01-env" {
		t.Errorf("with the environment's token: %q", l)
	}
	if o, tk, _ := ClaudeOf(s).Sums(ctx); o == "" || tk != "" {
		t.Errorf("ClaudeSums with the environment's token = %q, %q", o, tk)
	}
	if got := s.Logins(ctx, []string{"claude"}); !slices.Equal(got, nil) {
		t.Errorf("Logins with the environment's token = %q", got)
	}
	delete(env, "CLAUDE_CODE_OAUTH_TOKEN")
	if got := s.Logins(ctx, []string{"claude"}); !slices.Equal(got, []string{agents.ClaudeOAuth}) {
		t.Errorf("Logins with the OAuth login = %q", got)
	}
	// The saved token can't be deleted: the login is saved still, the VM gets its reply, and
	// devvm warns.
	var warned []string
	s.Reporter = &msg.Fake{OnWarn: func(m msg.M) { warned = append(warned, msg.IDOf(m)) }}
	s.KeychainDelete = func(context.Context, string, string) error { return errors.New("locked") }
	out, err = s.Capture(ctx, "claude", "salt", "application/json", login,
		[]byte(`{"access_token":"sk-ant-oat01-oauth4","refresh_token":"sk-ant-ort01-r","scope":"user:inference"}`))
	if err != nil || len(out) == 0 || !slices.Equal(warned, []string{"secret_failed"}) {
		t.Errorf("a token that can't be deleted: %s, %v, warned %q", out, err, warned)
	}
}

// A VM with Claude Code has both Claude Code logins' bindings while devvm keeps either, and each
// gets the login devvm keeps: the token, or the OAuth login's access token.
func TestClaudeBindings(t *testing.T) {
	ctx := t.Context()
	s, kc := testStore(t, true, nil)
	st := newVM(t, s.Dirs, vm1, "node", "claude")
	salt := strings.Repeat("cd", 16)
	if err := s.WriteFiles(ctx, st, salt); err != nil {
		t.Fatal(err)
	}
	if got := bindingNames(t, s, st); got != nil {
		t.Errorf("no login: %q", got)
	}
	writeClaudeOAuth(t, s, "sk-ant-oat01-oauth")
	if err := s.WriteFiles(ctx, st, salt); err != nil {
		t.Fatal(err)
	}
	if got := bindingNames(t, s, st); !slices.Equal(got, []string{"claude", agentlogin.ClaudeOAuthName}) {
		t.Errorf("the OAuth login: %q", got)
	}
	for _, b := range []proxy.Binding{secretOf("claude"), loginOf(agents.ClaudeOAuth, agentlogin.ClaudeOAuthName)} {
		if v, err := s.Get(ctx, b); v != "sk-ant-oat01-oauth" || err != nil {
			t.Errorf("Get(%s) with the OAuth login = %q, %v", b.Name, v, err)
		}
	}
	if err := s.Save(ctx, "claude", "sk-ant-oat01-token"); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteFiles(ctx, st, salt); err != nil {
		t.Fatal(err)
	}
	if got := bindingNames(t, s, st); !slices.Equal(got, []string{"claude", agentlogin.ClaudeOAuthName}) {
		t.Errorf("the token: %q", got)
	}
	for _, b := range []proxy.Binding{secretOf("claude"), loginOf(agents.ClaudeOAuth, agentlogin.ClaudeOAuthName)} {
		if v, err := s.Get(ctx, b); v != "sk-ant-oat01-token" || err != nil {
			t.Errorf("Get(%s) with the token = %q, %v", b.Name, v, err)
		}
	}
	delete(kc, "devvm/claude")
	if _, err := s.Get(ctx, loginOf(agents.ClaudeOAuth, agentlogin.ClaudeOAuthName)); msg.IDOf(err) != "claude_no_login" {
		t.Errorf("no login: %v", msg.NamesOf(err))
	}
	// A login file that can't be read says so.
	writeLogin(t, s, agents.ClaudeOAuth, "{")
	if _, err := s.Get(ctx, secretOf("claude")); msg.IDOf(err) != "login_read_failed" {
		t.Errorf("a broken login file: %v", msg.NamesOf(err))
	}
	if _, err := os.Stat(LoginFile(s.Dirs, agents.ClaudeOAuth)); err != nil || !strings.HasSuffix(LoginFile(s.Dirs, agents.ClaudeOAuth), "/logins/claude/.claude/.credentials.json") {
		t.Errorf("the OAuth login's file: %s, %v", LoginFile(s.Dirs, agents.ClaudeOAuth), err)
	}
	if _, err := ClaudeOf(s).Login(ctx); err == nil {
		t.Error("ClaudeLogin read a broken file")
	}
	if _, _, err := ClaudeOf(s).Sums(ctx); err == nil || errors.Is(err, ErrNone) {
		t.Errorf("ClaudeSums read a broken file: %v", err)
	}
}

// The VM's copy of the OAuth login holds its placeholder, never its tokens.
func TestClaudeOAuthVMCopy(t *testing.T) {
	s, _ := testStore(t, false, nil)
	writeClaudeOAuth(t, s, "sk-ant-oat01-secret")
	data, err := s.VMLogin(agents.ClaudeOAuth, "s417")
	if err != nil || strings.Contains(string(data), "secret") || strings.Contains(string(data), "ort01") ||
		!strings.Contains(string(data), "sk-ant-oat01-devvm-proxy-managed-oauth-s417") {
		t.Errorf("VMLogin = %s, %v", data, err)
	}
	if LoginRel(agents.ClaudeOAuth) != ".claude/.credentials.json" {
		t.Errorf("LoginRel = %q", LoginRel(agents.ClaudeOAuth))
	}
}
