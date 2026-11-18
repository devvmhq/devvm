package agentlogin

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// ~/.claude.json's oauthAccount keeps claudeAccountKeep's fields; one that isn't an object is
// withheld whole, and a file without one gets none.
func TestClaudeAccount(t *testing.T) {
	for _, c := range []struct {
		in       map[string]any
		want     map[string]any
		withheld []string
	}{
		{
			map[string]any{"oauthAccount": map[string]any{"accountUuid": "a", "fullName": map[string]any{"x": 1}}},
			map[string]any{"oauthAccount": map[string]any{"accountUuid": "a"}},
			[]string{"oauthAccount.fullName"},
		},
		{map[string]any{"oauthAccount": "sk-e2efake", "theme": "dark"}, map[string]any{"theme": "dark"}, []string{"oauthAccount"}},
		{map[string]any{"theme": "dark"}, map[string]any{"theme": "dark"}, nil},
	} {
		w := claudeAccount(c.in)
		got, _ := json.Marshal(c.in)
		want, _ := json.Marshal(c.want)
		if string(got) != string(want) || !slices.Equal(w, c.withheld) {
			t.Errorf("claudeAccount = %s, %q; want %s, %q", got, w, want, c.withheld)
		}
	}
}

func strs(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

// Claude Code's OAuth login: saved as Claude Code keeps it (.credentials.json's claudeAiOauth), the
// VM given its account and organization, a placeholder of its own and no refresh token; its binding
// names that placeholder for Anthropic's API, and a second for claude.ai's connectors, below
// mcp-proxy.anthropic.com's /v1/mcp/ alone, and the VM's copy holds it with an expiry at farFuture.
func TestClaudeCapture(t *testing.T) {
	f := storeFile(t, ClaudeOAuth, ".credentials.json")
	req := []byte(`{"grant_type":"authorization_code","code":"c","client_id":"x","code_verifier":"v","state":"s"}`)
	vm := capture(t, f, jsonType+"; charset=utf-8", req, reply(map[string]any{ //nolint:gosec // a test's fake login
		"access_token": "sk-ant-oat01-real", "refresh_token": "sk-ant-ort01-real", "expires_in": 28800, "scope": "user:inference user:profile",
		"token_type": "Bearer", "account": map[string]any{"uuid": "u-1", "email_address": "a@b"}, "organization": map[string]any{"uuid": "o-1"},
	}), "sk-ant-oat01-real", "sk-ant-ort01-real")
	o := obj(readStore(t, f), "claudeAiOauth")
	exp, _ := o["expiresAt"].(json.Number).Int64()
	if str(o, "accessToken") != "sk-ant-oat01-real" || str(o, "refreshToken") != "sk-ant-ort01-real" ||
		time.Until(time.UnixMilli(exp)) < 7*time.Hour || !slices.Equal(strs(o["scopes"]), []string{"user:inference", "user:profile"}) {
		t.Errorf("the store's login = %v", o)
	}
	ph := "sk-ant-oat01-devvm-proxy-managed-oauth-" + salt
	if vm["access_token"] != ph || vm["refresh_token"] != managed ||
		obj(vm, "account")["email_address"] != "a@b" || obj(vm, "organization")["uuid"] != "o-1" || vm["scope"] != "user:inference user:profile" {
		t.Errorf("the VM's reply = %v", vm)
	}
	bs := bindings(t, f)
	if len(bs) != 2 || bs[0].Name != ClaudeOAuthName || bs[0].Placeholder != ph || bs[0].Header != "Authorization" ||
		!slices.Equal(bs[0].Hosts, []string{"api.anthropic.com"}) || bs[0].Path != "" ||
		bs[1].Name != ClaudeConnectorsName || bs[1].Agent != ClaudeOAuth || bs[1].Placeholder != ph || bs[1].Header != "Authorization" ||
		!slices.Equal(bs[1].Hosts, []string{"mcp-proxy.anthropic.com"}) || bs[1].Path != "/v1/mcp/" {
		t.Errorf("bindings = %+v", bs)
	}
	data, _ := vmCopy(t, f)
	var c map[string]any
	must(t, json.Unmarshal([]byte(data), &c))
	co := obj(c, "claudeAiOauth")
	if strings.Contains(data, "real") || len(c) != 1 || co["accessToken"] != ph || co["refreshToken"] != managed ||
		co["expiresAt"] != float64(farFuture)*1000 || !slices.Equal(strs(co["scopes"]), []string{"user:inference", "user:profile"}) {
		t.Errorf("the VM's copy = %s", data)
	}
	if ok, err := f.ClaudeLogin(); !ok || err != nil {
		t.Errorf("ClaudeLogin = %v, %v", ok, err)
	}
	// The VM's own refresh, with its managed refresh token, is answered with the placeholder.
	refresh := []byte(`{"grant_type":"refresh_token","refresh_token":"devvm-proxy-managed","client_id":"x","scope":"user:inference"}`)
	if out, answer, err := f.CaptureRequest(jsonType, refresh); err != nil || out != nil || !strings.Contains(string(answer), ph) ||
		strings.Contains(string(answer), "real") || !strings.Contains(string(answer), "user:profile") {
		t.Errorf("the VM's refresh: %q, %s, %v", out, answer, err)
	}
	// A refresh with another token goes on, and its reply adds to the login.
	other := []byte(`{"grant_type":"refresh_token","refresh_token":"sk-ant-ort01-other","client_id":"x"}`)
	if out, answer, err := f.CaptureRequest(jsonType, other); err != nil || string(out) != string(other) || answer != nil {
		t.Errorf("another refresh: %q, %s, %v", out, answer, err)
	}
	capture(t, f, jsonType, other, reply(map[string]any{"access_token": "sk-ant-oat01-new", "expires_in": 3600}), "sk-ant-oat01-new") //nolint:gosec // a test's fake token
	if o := obj(readStore(t, f), "claudeAiOauth"); str(o, "accessToken") != "sk-ant-oat01-new" || str(o, "refreshToken") != "sk-ant-ort01-real" {
		t.Errorf("after the refresh: %v", o)
	}
	must(t, f.Remove())
	if _, err := os.Stat(f.Path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("after Remove: %v", err)
	}
	must(t, f.Remove())
	if ok, err := f.ClaudeLogin(); ok || err != nil {
		t.Errorf("ClaudeLogin with none = %v, %v", ok, err)
	}
	if out, _, err := f.CaptureRequest(jsonType, refresh); err == nil {
		t.Errorf("the VM's refresh with no login answered: %s", out)
	}
	if bs := bindings(t, File{Kind: ClaudeOAuth, Path: writeFile(t, map[string]any{})}); len(bs) != 0 {
		t.Errorf("bindings with no login = %+v", bs)
	}
	if data, _ := vmCopy(t, File{Kind: ClaudeOAuth, Path: writeFile(t, map[string]any{})}); data != "{}\n" {
		t.Errorf("the VM's copy with no login = %q", data)
	}
}

// claude setup-token's code grant asks for a long-lived token: its reply is a Claude Code token,
// which the VM gets as the placeholder given; anything else isn't setup-token's.
func TestClaudeSetupToken(t *testing.T) {
	setup := []byte(`{"grant_type":"authorization_code","code":"c","client_id":"x","code_verifier":"v","state":"s","expires_in":31536000}`)
	login := []byte(`{"grant_type":"authorization_code","code":"c","client_id":"x","code_verifier":"v","state":"s"}`)
	if !SetupToken(jsonType, setup) || SetupToken(jsonType, login) || SetupToken(jsonType, []byte(`{"grant_type":"refresh_token","expires_in":1}`)) ||
		SetupToken(formType, []byte("grant_type=authorization_code&expires_in=1&expires_in=2")) {
		t.Error("SetupToken tells setup-token's request wrong")
	}
	tok, out, err := ClaudeToken(jsonType, setup, reply(map[string]any{ //nolint:gosec // a test's fake token
		"access_token": "sk-ant-oat01-long", "expires_in": 31536000, "scope": "user:inference", "account": map[string]any{"uuid": "u"},
	}), "sk-ant-oat01-devvm-proxy-managed-"+salt)
	var vm map[string]any
	must(t, err)
	must(t, json.Unmarshal(out, &vm))
	if tok.Reveal() != "sk-ant-oat01-long" || vm["access_token"] != "sk-ant-oat01-devvm-proxy-managed-"+salt || vm["refresh_token"] != nil ||
		strings.Contains(string(out), "long") || obj(vm, "account")["uuid"] != "u" {
		t.Errorf("ClaudeToken = %q, %s", tok, out)
	}
	if _, out, err := ClaudeToken(jsonType, setup, reply(map[string]any{"access_token": "a", "refresh_token": "r"}), "p"); err == nil {
		t.Errorf("a token that isn't Claude Code's: %s", out)
	}
	if _, _, err := ClaudeToken(jsonType, setup, []byte("<html>"), "p"); err == nil {
		t.Error("a reply that isn't JSON")
	}
	if _, _, err := ClaudeToken(jsonType, login, reply(map[string]any{"access_token": "sk-ant-oat01-x"}), "p"); err == nil { //nolint:gosec // a test's fake token
		t.Error("a login's reply taken for a token")
	}
	_, out, err = ClaudeToken(jsonType, setup, reply(map[string]any{"access_token": "sk-ant-oat01-x", "refresh_token": "sk-ant-ort01-x"}), "p") //nolint:gosec // a test's fake token
	if err != nil || strings.Contains(string(out), "ort01") || !strings.Contains(string(out), managed) {
		t.Errorf("with a refresh token: %s, %v", out, err)
	}
}

// The OAuth login's access token is refreshed on the host when it's due, at Claude's token
// endpoint with Claude Code's client ID and the login's scopes, and written back; a fresh one is
// left alone; a failed refresh says to log in again; none is ErrClaudeNoLogin.
//
//nolint:gosec // a test's fake tokens
func TestClaudeOAuthGet(t *testing.T) {
	u, got := tokenServer(t, map[string]any{"access_token": "sk-ant-oat01-new", "refresh_token": "sk-ant-ort01-new", "expires_in": 28800})
	f := File{Kind: ClaudeOAuth, TokenURL: u, Path: writeFile(t, map[string]any{"claudeAiOauth": map[string]any{
		"accessToken": "sk-ant-oat01-old", "refreshToken": "sk-ant-ort01-old", "expiresAt": time.Now().Add(time.Minute).UnixMilli(),
		"scopes": []string{"user:inference", "user:profile"},
	}})}
	v, err := f.Get(t.Context(), ClaudeOAuthName)
	if err != nil || v.Reveal() != "sk-ant-oat01-new" {
		t.Fatalf("value = %q, %v", v, err)
	}
	if !strings.HasPrefix(*got, "application/json ") || !strings.Contains(*got, `"refresh_token":"sk-ant-ort01-old"`) ||
		!strings.Contains(*got, `"client_id":"`+claudeClientID+`"`) || !strings.Contains(*got, `"scope":"user:inference user:profile"`) ||
		!strings.Contains(*got, `"grant_type":"refresh_token"`) {
		t.Errorf("refresh request = %s", *got)
	}
	m, _ := readJSON(f.Path)
	o := obj(m, "claudeAiOauth")
	exp, _ := o["expiresAt"].(json.Number).Int64()
	if str(o, "refreshToken") != "sk-ant-ort01-new" || str(o, "accessToken") != "sk-ant-oat01-new" || time.Until(time.UnixMilli(exp)) < 7*time.Hour {
		t.Errorf("host file = %v", m)
	}
	*got = ""
	if v, err := f.Get(t.Context(), ClaudeOAuthName); err != nil || v.Reveal() != "sk-ant-oat01-new" || *got != "" {
		t.Errorf("a fresh token: %q, %v, refreshed %q", v, err, *got)
	}
	if _, err := f.Get(t.Context(), "claude-other"); err == nil {
		t.Error("another name looked up")
	}
	bad := File{Kind: ClaudeOAuth, TokenURL: "http://127.0.0.1:1/token", Path: writeFile(t, map[string]any{"claudeAiOauth": map[string]any{
		"accessToken": "sk-ant-oat01-old", "refreshToken": "sk-ant-ort01-old", "expiresAt": 0,
	}})}
	var re *RefreshError
	if _, err := bad.Get(t.Context(), ClaudeOAuthName); !errors.As(err, &re) || re.Agent != Claude {
		t.Errorf("a failed refresh: %v", err)
	}
	// A refresh's reply that isn't Claude Code's token, or grants API keys, is refused, the login kept.
	for _, rep := range []map[string]any{
		{"access_token": "not-claudes"},
		{"access_token": "sk-ant-oat01-k", "scope": "user:inference org:create_api_key"},
	} {
		bad.TokenURL, _ = tokenServer(t, rep)
		if _, err := bad.Get(t.Context(), ClaudeOAuthName); !errors.As(err, &re) {
			t.Errorf("a refresh's reply %v: %v", rep, err)
		}
		if m, _ := readJSON(bad.Path); str(claudeOAuth(m), "accessToken") != "sk-ant-oat01-old" {
			t.Errorf("after the refused refresh: %v", m)
		}
	}
	for _, p := range []string{filepath.Join(t.TempDir(), "gone", "x.json"), filepath.Join(t.TempDir(), "x.json"), writeFile(t, map[string]any{})} {
		if _, err := (File{Kind: ClaudeOAuth, Path: p}).Get(t.Context(), ClaudeOAuthName); !errors.Is(err, ErrClaudeNoLogin) {
			t.Errorf("%s: %v", p, err)
		}
	}
	if (File{Kind: ClaudeOAuth}).tokenURL() != ClaudeTokenURL || (File{Kind: Codex}).tokenURL() != OpenAITokenURL {
		t.Error("the default token endpoints")
	}
}
