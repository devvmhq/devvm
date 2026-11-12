package agentlogin

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/agents"
	"github.com/devvmhq/devvm/internal/proxy"
)

const (
	formType = "application/x-www-form-urlencoded"
	jsonType = "application/json"
	salt     = "s417"
)

func form(kv ...string) []byte {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	return []byte(v.Encode())
}

func reply(m map[string]any) []byte { b, _ := json.Marshal(m); return b }

// storeFile is a login file in a store of the test's own, not there yet.
func storeFile(t *testing.T, kind, name string) File {
	t.Helper()
	return File{Kind: kind, Path: filepath.Join(t.TempDir(), "logins", kind, name), Salt: salt}
}

func readStore(t *testing.T, f File) map[string]any {
	t.Helper()
	m, err := readJSON(f.Path)
	must(t, err)
	if fi, err := os.Stat(f.Path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("%s: %v, %v", f.Path, fi.Mode(), err)
	}
	return m
}

// capture is f.Capture, its reply to the VM decoded, checked to hold none of secrets.
func capture(t *testing.T, f File, ctype string, req, rep []byte, secrets ...string) map[string]any {
	t.Helper()
	out, err := f.Capture(ctype, req, rep)
	must(t, err)
	for _, s := range secrets {
		if strings.Contains(string(out), s) {
			t.Errorf("the VM's reply holds %q: %s", s, out)
		}
	}
	var vm map[string]any
	must(t, json.Unmarshal(out, &vm))
	if n, _ := vm["expires_in"].(float64); time.Duration(n)*time.Second < 50*365*24*time.Hour {
		t.Errorf("the VM's token expires in %vs", vm["expires_in"])
	}
	return vm
}

// openaiTokens are an OpenAI login's id_token and access token, with its account's claims.
func openaiTokens() (string, string) {
	return jwtWith(time.Now().Add(time.Hour), map[string]any{"email": "a@b", "https://api.openai.com/auth": openaiAuth}),
		jwtWith(time.Now().Add(time.Hour), map[string]any{"https://api.openai.com/auth": openaiAuth, "scp": "x"})
}

// Codex's ChatGPT login, as codex login makes it: the code's reply saved in the store as Codex
// keeps it, the VM given the placeholders its bindings name; then the API-key exchange, whose
// id_token placeholder becomes the real one on its way out, and whose key is saved and replaced.
func TestCodexCapture(t *testing.T) {
	idToken, accessToken := openaiTokens()
	f := storeFile(t, Codex, "auth.json")
	code := form("grant_type", grantCode, "code", "c", "client_id", openaiClientID, "code_verifier", "v")
	if b, answer, err := f.CaptureRequest(formType, code); err != nil || string(b) != string(code) || answer != nil {
		t.Fatalf("the code's request = %q, %v; want it as it is", b, err)
	}
	vm := capture(t, f, formType, code, reply(map[string]any{
		"id_token": idToken, "access_token": accessToken, "refresh_token": "rt-real", "expires_in": 3600, "token_type": "Bearer", "extra": "x",
	}), idToken, accessToken, "rt-real")
	m := readStore(t, f)
	tk := obj(m, "tokens")
	if str(tk, "id_token") != idToken || str(tk, "access_token") != accessToken || str(tk, "refresh_token") != "rt-real" ||
		str(tk, "account_id") != "acct-1" || m["auth_mode"] != "chatgpt" || m["last_refresh"] == nil {
		t.Errorf("the store's login = %v", m)
	}
	if v, ok := m["OPENAI_API_KEY"]; !ok || v != nil {
		t.Errorf("OPENAI_API_KEY = %v, %v; want null", v, ok)
	}
	bs := bindings(t, f)
	if len(bs) != 1 || bs[0].Name != "codex" || vm["access_token"] != bs[0].Placeholder {
		t.Errorf("the VM's access token %v, bindings %+v", vm["access_token"], bs)
	}
	if vm["refresh_token"] != managed || vm["token_type"] != "Bearer" || vm["extra"] != nil {
		t.Errorf("the VM's reply = %v", vm)
	}
	if c := jwtClaims(vm["id_token"].(string)); c["email"] != "a@b" || chatgptAccount(vm["id_token"].(string)) != "acct-1" {
		t.Errorf("the VM's id_token claims = %v", c)
	}

	vm0 := vm
	exchange := func(subject string) []byte {
		return form("grant_type", grantExchange, "client_id", openaiClientID, "requested_token", apiKeyToken,
			"subject_token", subject, "subject_token_type", idTokenType)
	}
	out, _, err := f.CaptureRequest(formType, exchange(vm["id_token"].(string)))
	if q, _ := url.ParseQuery(string(out)); err != nil || q.Get("subject_token") != idToken || q.Get("requested_token") != apiKeyToken {
		t.Fatalf("the exchange goes out as %q, %v", out, err)
	}
	for _, other := range []string{"", "x", openaiPlaceholder(idToken, "another-vm")} {
		if _, _, err := f.CaptureRequest(formType, exchange(other)); err == nil {
			t.Errorf("an exchange for %q went out", other)
		}
	}
	key := "sk-real-key"
	vm = capture(t, f, formType, exchange(vm["id_token"].(string)), reply(map[string]any{"access_token": key}), key)
	if vm["access_token"] != codexKeyPlaceholder(salt) || str(readStore(t, f), "OPENAI_API_KEY") != "sk-real-key" {
		t.Errorf("the exchange: VM %v, store %v", vm, readStore(t, f))
	}
	bs = bindings(t, f)
	if len(bs) != 2 || bs[0].Name != "codex-key" || bs[0].Placeholder != codexKeyPlaceholder(salt) {
		t.Fatalf("bindings = %+v", bs)
	}

	// The VM's own refresh (Codex's is JSON) carries managed: it goes nowhere, and the VM gets the
	// store's login as placeholders again.
	refresh, _ := json.Marshal(map[string]string{"grant_type": grantRefresh, "client_id": openaiClientID, "refresh_token": managed})
	out, answer, err := f.CaptureRequest(jsonType, refresh)
	var again map[string]any
	if err != nil || out != nil || json.Unmarshal(answer, &again) != nil || again["access_token"] != bs[1].Placeholder ||
		again["refresh_token"] != managed || again["id_token"] != vm0["id_token"] || strings.Contains(string(answer), "rt-real") {
		t.Errorf("the VM's refresh: %q, %s, %v", out, answer, err)
	}
	// A refresh of another refresh token goes out, and its reply is captured: the store keeps only
	// the newest refresh token, and the rest.
	refresh, _ = json.Marshal(map[string]string{"grant_type": grantRefresh, "client_id": openaiClientID, "refresh_token": "rt-elsewhere"})
	if b, answer, err := f.CaptureRequest(jsonType, refresh); err != nil || string(b) != string(refresh) || answer != nil {
		t.Errorf("another refresh: %q, %q, %v", b, answer, err)
	}
	capture(t, f, jsonType, refresh, reply(map[string]any{"access_token": "at-2", "refresh_token": "rt-2"}), "at-2", "rt-2")
	m = readStore(t, f)
	if tk := obj(m, "tokens"); str(tk, "refresh_token") != "rt-2" || str(tk, "access_token") != "at-2" || str(tk, "id_token") != idToken ||
		str(m, "OPENAI_API_KEY") != "sk-real-key" {
		t.Errorf("after the refresh: %v", m)
	}
	// A new login replaces the old one, its key included.
	capture(t, f, formType, code, reply(map[string]any{"id_token": idToken, "access_token": "at-3", "refresh_token": "rt-3"}))
	if m := readStore(t, f); m["OPENAI_API_KEY"] != nil || str(obj(m, "tokens"), "refresh_token") != "rt-3" {
		t.Errorf("after a new login: %v", m)
	}
}

// What doesn't parse as a login is refused, and nothing is saved: a request devvm doesn't read (a
// grant it doesn't capture, a JSON exchange, another type), a reply that isn't JSON, or one
// without the tokens a login needs, or a Claude login devvm doesn't keep.
//
//nolint:gosec // a test's fake tokens
func TestCaptureRefuses(t *testing.T) {
	code := form("grant_type", grantCode, "code", "c")
	for _, tc := range []struct {
		name, kind, ctype string
		req, reply        []byte
	}{
		{"client credentials", Codex, formType, form("grant_type", "client_credentials"), reply(map[string]any{"access_token": "a"})},
		{"no grant", Codex, formType, form("code", "c"), reply(map[string]any{"access_token": "a"})},
		{"a JSON exchange", Codex, jsonType, []byte(`{"grant_type":"` + grantExchange + `","requested_token":"openai-api-key","subject_token_type":"` + idTokenType + `"}`), reply(map[string]any{"access_token": "a"})},
		{"opencode's exchange", Opencode, formType, form("grant_type", grantExchange, "requested_token", apiKeyToken, "subject_token_type", idTokenType), reply(map[string]any{"access_token": "a"})},
		{"another type", Codex, "text/plain", code, reply(map[string]any{"access_token": "a"})},
		{"a request that isn't JSON", ClaudeOAuth, jsonType, []byte("grant_type=authorization_code"), reply(map[string]any{"access_token": "a", "refresh_token": "r"})},
		{"a reply that isn't JSON", Codex, formType, code, []byte("<html>")},
		{"a reply of null", Codex, formType, code, []byte("null")},
		{"no id_token", Codex, formType, code, reply(map[string]any{"access_token": "a", "refresh_token": "r"})},
		{"no refresh token", Opencode, formType, code, reply(map[string]any{"access_token": "a"})},
		{"no access token", ClaudeOAuth, jsonType, []byte(`{"grant_type":"authorization_code"}`), reply(map[string]any{"refresh_token": "r"})},
		{"a field twice in a form", Codex, formType, append(form("grant_type", grantCode), "&grant_type=refresh_token"...), reply(map[string]any{"access_token": "a"})},
		{"a field twice in JSON", ClaudeOAuth, jsonType, []byte(`{"grant_type":"authorization_code","grant_type":"refresh_token"}`), reply(map[string]any{"access_token": "a", "refresh_token": "r"})},
		{"a Console login", ClaudeOAuth, jsonType, []byte(`{"grant_type":"authorization_code"}`), reply(map[string]any{"access_token": "a", "refresh_token": "r", "scope": "org:create_api_key user:profile"})},
		{"a login that makes API keys", ClaudeOAuth, jsonType, []byte(`{"grant_type":"authorization_code"}`), reply(map[string]any{"access_token": "sk-ant-oat01-a", "refresh_token": "r", "scope": "user:inference org:create_api_key"})},
		{"a refresh to a login that makes API keys", ClaudeOAuth, jsonType, []byte(`{"grant_type":"refresh_token","refresh_token":"r"}`), reply(map[string]any{"access_token": "sk-ant-oat01-a", "scope": "user:inference org:create_api_key"})},
		{"a refresh to a Console login, with none in the store", ClaudeOAuth, jsonType, []byte(`{"grant_type":"refresh_token","refresh_token":"r"}`), reply(map[string]any{"access_token": "sk-ant-oat01-a", "scope": "user:profile"})},
		{"an access token that isn't Claude Code's", ClaudeOAuth, jsonType, []byte(`{"grant_type":"authorization_code"}`), reply(map[string]any{"access_token": "a", "refresh_token": "r", "scope": "user:inference"})},
		{"an access token with a quote", ClaudeOAuth, jsonType, []byte(`{"grant_type":"authorization_code"}`), reply(map[string]any{"access_token": `sk-ant-oat01-a"b`, "refresh_token": "r", "scope": "user:inference"})},
		{"a login without a scope", ClaudeOAuth, jsonType, []byte(`{"grant_type":"authorization_code"}`), reply(map[string]any{"access_token": "a", "refresh_token": "r"})},
		{"tokens that aren't strings", ClaudeOAuth, jsonType, []byte(`{"grant_type":"authorization_code"}`), reply(map[string]any{"access_token": 1, "refresh_token": 2})},
	} {
		f := storeFile(t, tc.kind, "auth.json")
		if out, err := f.Capture(tc.ctype, tc.req, tc.reply); err == nil {
			t.Errorf("%s: captured, the VM given %s", tc.name, out)
		}
		if _, err := os.Stat(f.Path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: the store's file is there: %v", tc.name, err)
		}
	}
	if _, err := (File{Kind: "other", Path: filepath.Join(t.TempDir(), "x")}).Capture(formType, code, reply(map[string]any{"access_token": "a"})); err == nil {
		t.Error("a login of no agent's captured")
	}
}

// opencode's OpenAI login: saved as opencode keeps it, beside its other providers, the VM given the
// placeholder its binding names and never the refresh token.
func TestOpencodeCapture(t *testing.T) {
	idToken, accessToken := openaiTokens()
	f := storeFile(t, Opencode, "auth.json")
	must(t, os.MkdirAll(filepath.Dir(f.Path), 0o700))
	must(t, os.WriteFile(f.Path, []byte(`{"anthropic":{"type":"api","key":"sk-ant-api"},"openai":{"type":"api","key":"sk-old"}}`), 0o600))
	code := form("grant_type", grantCode, "code", "c", "redirect_uri", "http://localhost:1455/auth/callback")
	vm := capture(t, f, formType, code, reply(map[string]any{
		"id_token": idToken, "access_token": accessToken, "refresh_token": "rt-real", "expires_in": 7200,
	}), idToken, accessToken, "rt-real")
	m := readStore(t, f)
	e := obj(m, "openai")
	exp, _ := e["expires"].(json.Number).Int64()
	if str(e, "type") != "oauth" || str(e, "access") != accessToken || str(e, "refresh") != "rt-real" || str(e, "accountId") != "acct-1" ||
		e["key"] != nil || time.Until(time.UnixMilli(exp)) < 110*time.Minute || time.Until(time.UnixMilli(exp)) > 2*time.Hour {
		t.Errorf("the store's openai login = %v", e)
	}
	if str(obj(m, "anthropic"), "key") != "sk-ant-api" {
		t.Errorf("another provider's login changed: %v", m)
	}
	var ph string
	for _, b := range bindings(t, f) {
		if b.Name == "opencode-openai" {
			ph = b.Placeholder
		}
	}
	if ph == "" || vm["access_token"] != ph || vm["refresh_token"] != managed || chatgptAccount(vm["id_token"].(string)) != "acct-1" {
		t.Errorf("the VM's reply %v, the binding's placeholder %q", vm, ph)
	}
	// The VM's own refresh, with managed or with its copy's refresh token, the access token's
	// placeholder, is answered with the placeholders.
	for _, rt := range []string{managed, ph} {
		if out, answer, err := f.CaptureRequest(formType, form("grant_type", grantRefresh, "refresh_token", rt)); err != nil || out != nil ||
			!strings.Contains(string(answer), ph) || strings.Contains(string(answer), "rt-real") {
			t.Errorf("the VM's refresh with %.20s: %q, %s, %v", rt, out, answer, err)
		}
	}
	// A refresh keeps the account and takes the newest refresh token.
	capture(t, f, formType, form("grant_type", grantRefresh, "refresh_token", "rt-elsewhere"), reply(map[string]any{"access_token": "at-2", "refresh_token": "rt-2"}))
	if e := obj(readStore(t, f), "openai"); str(e, "refresh") != "rt-2" || str(e, "access") != "at-2" || str(e, "accountId") != "acct-1" {
		t.Errorf("after the refresh: %v", e)
	}
}

// VMs capture each agent's logins at its capture format's endpoints, in the registry's order, and
// the login a capture saves goes in that format's file: Codex's and opencode's at OpenAI's token
// endpoint, and Claude Code's at both of Claude's, in its OAuth login's file.
func TestEndpoints(t *testing.T) {
	var want []proxy.Capture
	for _, d := range agents.Defs {
		c, kind, ok := captureOf(d.Name)
		if !ok {
			if got := CaptureKind(d.Name); got != d.Name {
				t.Errorf("CaptureKind(%s) = %s, without a capture format", d.Name, got)
			}
			continue
		}
		if CaptureKind(d.Name) != kind || !slices.ContainsFunc(d.Logins, func(l agents.Login) bool { return l.Kind == kind }) {
			t.Errorf("CaptureKind(%s) = %s, not a login file of its own", d.Name, CaptureKind(d.Name))
		}
		if len(c.endpoints()) == 0 {
			t.Errorf("%s: a capture format with no endpoints", d.Name)
		}
		for _, e := range c.endpoints() {
			want = append(want, proxy.Capture{Agent: d.Name, Host: e.host, Path: e.path, Method: e.method, Guard: e.guard, TokenKeys: e.tokenKeys})
		}
	}
	got := Endpoints()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Endpoints() = %+v, want %+v", got, want)
	}
	// Exactly these, whatever their order: a new or a repeated endpoint, which the proxy would
	// capture and allow, fails until it's added here.
	key := func(c proxy.Capture) string { return c.Agent + " " + c.Host + c.Path }
	sorted := slices.SortedFunc(slices.Values(got), func(a, b proxy.Capture) int { return strings.Compare(key(a), key(b)) })
	exact := []proxy.Capture{
		{Agent: Claude, Host: "console.anthropic.com", Path: "/v1/oauth/token"},
		{Agent: Claude, Host: "platform.claude.com", Path: "/v1/oauth/token"},
		{Agent: Codex, Host: "auth.openai.com", Path: "/oauth/token"},
		{
			Agent: agents.Cursor, Host: "api2.cursor.sh", Path: "/auth/poll", Method: "GET", Guard: "/auth/",
			TokenKeys: []string{"accessToken", "refreshToken", "xaiAccessToken", "xaiRefreshToken", "sessionToken", "authToken", "apiKey", "jwt", "token"},
		},
		{Agent: Opencode, Host: "auth.openai.com", Path: "/oauth/token"},
	}
	if !reflect.DeepEqual(sorted, exact) {
		t.Errorf("Endpoints() = %+v, want exactly %+v", sorted, exact)
	}
	if CaptureKind(Claude) != ClaudeOAuth {
		t.Errorf("CaptureKind(claude) = %s", CaptureKind(Claude))
	}
}

// reversed is a copy of s, last first.
func reversed(s []string) []string {
	out := slices.Clone(s)
	slices.Reverse(out)
	return out
}

// A request at an endpoint agents share is the agent's whose User-Agent it has, else the first's
// that names none, whatever their order; at one agent's own, it's that agent's whatever sends it.
// At OpenAI's, which Codex and opencode share, it's opencode's when Bun or opencode sends it.
func TestPick(t *testing.T) {
	shared := map[string][]string{}
	for _, c := range Endpoints() {
		shared[c.Host+c.Path] = append(shared[c.Host+c.Path], c.Agent)
	}
	for _, names := range shared {
		for _, a := range names {
			if got := Pick([]string{a}, "other/1.0"); got != a {
				t.Errorf("Pick(%s) = %s", a, got)
			}
		}
		if len(names) < 2 {
			continue
		}
		var plain string
		for _, a := range names {
			c, _, _ := captureOf(a)
			if len(c.userAgents()) == 0 && plain == "" {
				plain = a
			}
			for _, p := range c.userAgents() {
				for _, order := range [][]string{names, reversed(names)} {
					if got := Pick(order, strings.ToUpper(p[:1])+p[1:]+"1.2"); got != a {
						t.Errorf("Pick(%v, %q) = %s, want %s", order, p, got, a)
					}
				}
			}
		}
		for _, order := range [][]string{names, reversed(names)} {
			if got := Pick(order, "codex_cli_rs/0.1"); plain != "" && got != plain {
				t.Errorf("Pick(%v) = %s, want %s", order, got, plain)
			}
		}
	}
	openai := shared["auth.openai.com/oauth/token"]
	if got := Pick(openai, "Bun/1.2.3"); got != Opencode || len(openai) < 2 {
		t.Errorf("Pick(%v, Bun) = %s", openai, got)
	}
	if got := Pick(openai, ""); got != Codex {
		t.Errorf("Pick(%v) = %s", openai, got)
	}
	if got := Pick(nil, ""); got != "" {
		t.Errorf("Pick(nil) = %s", got)
	}
}
