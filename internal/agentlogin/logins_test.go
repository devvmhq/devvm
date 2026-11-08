package agentlogin

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/fetch"
	"github.com/devvmhq/devvm/internal/proxy"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, v any) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "auth.json")
	b, _ := json.Marshal(v)
	must(t, os.WriteFile(p, b, 0o600))
	return p
}

// A fake token endpoint: records the request, answers with new tokens. It returns its URL.
func tokenServer(t *testing.T, reply map[string]any) (string, *string) {
	t.Helper()
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = r.Header.Get("Content-Type") + " " + string(b)
		_ = json.NewEncoder(w).Encode(reply)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &got
}

func jwtWith(exp time.Time, extra map[string]any) string {
	c := map[string]any{"exp": exp.Unix()}
	for k, v := range extra {
		c[k] = v
	}
	return fakeJWT(c) + "real-signature"
}

// vmCopy is f's VMCopy, as text.
func vmCopy(t *testing.T, f File) (string, []string) {
	t.Helper()
	c, err := f.VMCopy()
	must(t, err)
	return string(c.Data), c.Kept
}

// bindings is f's Bindings, each checked to be one of f's logins.
func bindings(t *testing.T, f File) []proxy.Binding {
	t.Helper()
	bs, err := f.Bindings()
	must(t, err)
	for _, b := range bs {
		if b.Kind != proxy.KindLogin || b.Agent != f.Kind {
			t.Errorf("binding %s: kind %q, agent %q; want %q, %q", b.Name, b.Kind, b.Agent, proxy.KindLogin, f.Kind)
		}
	}
	return bs
}

var openaiAuth = map[string]any{"chatgpt_account_id": "acct-1", "chatgpt_plan_type": "pro"}

func TestCodexVMCopyHoldsOnlyPlaceholders(t *testing.T) {
	access := jwtWith(time.Now().Add(time.Hour), map[string]any{"https://api.openai.com/auth": openaiAuth})
	f := File{Kind: Codex, Path: writeFile(t, map[string]any{
		"OPENAI_API_KEY": "sk-real",
		"tokens": map[string]any{
			"id_token":     jwtWith(time.Now(), map[string]any{"email": "a@b", "https://api.openai.com/auth": openaiAuth}),
			"access_token": access, "refresh_token": "rt-real", "account_id": "acct-1",
		},
		"last_refresh": "2026-01-01T00:00:00Z", "auth_mode": "chatgpt",
	})}
	out, kept := vmCopy(t, f)
	if kept != nil {
		t.Errorf("kept = %q", kept)
	}
	for _, secret := range []string{"sk-real", "rt-real", access} {
		if strings.Contains(out, secret) {
			t.Errorf("the VM's copy holds %q:\n%s", secret, out)
		}
	}
	var m map[string]any
	must(t, json.Unmarshal([]byte(out), &m))
	tk := m["tokens"].(map[string]any)
	// Codex decodes these: the account claims survive, and the access token doesn't expire.
	if c := jwtClaims(tk["id_token"].(string)); c["email"] != "a@b" || c["https://api.openai.com/auth"] == nil {
		t.Errorf("id_token claims = %v", c)
	}
	if exp, ok := jwtExp(tk["access_token"].(string)); !ok || exp.Before(time.Now().AddDate(10, 0, 0)) {
		t.Errorf("access_token exp = %v %v", exp, ok)
	}
	if tk["account_id"] != "acct-1" || m["auth_mode"] != "chatgpt" {
		t.Errorf("other fields changed: %v", m)
	}
	// The bindings name exactly the placeholders in the copy.
	bs := bindings(t, f)
	if len(bs) != 2 || bs[0].Name != "codex-key" || bs[1].Placeholder != tk["access_token"] {
		t.Errorf("bindings = %+v", bs)
	}
}

// A tokens object without an access token still has its other tokens scrubbed, and what the copy
// holds as it is is named.
func TestCodexVMScrubsTokensWithoutAnAccessToken(t *testing.T) {
	f := File{Kind: Codex, Path: writeFile(t, map[string]any{"tokens": map[string]any{"refresh_token": "rt-real", "id_token": "id-real"}, "extra": "x", "b": 1})}
	out, kept := vmCopy(t, f)
	if strings.Contains(out, "rt-real") || strings.Contains(out, "id-real") {
		t.Errorf("the VM's copy holds a token:\n%s", out)
	}
	if !slices.Equal(kept, []string{"b", "extra"}) {
		t.Errorf("kept = %q", kept)
	}
}

func TestCodexGetRefreshesOnTheHostWhenDue(t *testing.T) {
	u, got := tokenServer(t, map[string]any{"access_token": "at-new", "refresh_token": "rt-new", "id_token": "id-new"})
	f := File{Kind: Codex, TokenURL: u, Path: writeFile(t, map[string]any{"tokens": map[string]any{
		"access_token": jwtWith(time.Now().Add(time.Minute), nil), "refresh_token": "rt-old", "id_token": "x",
	}})}
	v, err := f.Get(t.Context(), "codex")
	if err != nil || v != "at-new" {
		t.Fatalf("value = %q, %v", v, err)
	}
	if !strings.HasPrefix(*got, "application/json ") || !strings.Contains(*got, `"refresh_token":"rt-old"`) ||
		!strings.Contains(*got, openaiClientID) {
		t.Errorf("refresh request = %s", *got)
	}
	m, _ := readJSON(f.Path)
	if tk := obj(m, "tokens"); str(tk, "refresh_token") != "rt-new" || str(tk, "access_token") != "at-new" || str(m, "last_refresh") == "" {
		t.Errorf("host file = %v", m)
	}
	if st, _ := os.Stat(f.Path); st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", st.Mode())
	}
}

// A refresh goes through the route its lookup's context carries (proxy.WithRoute): the company
// proxy a VM's proxy goes through, its password in Proxy-Authorization only.
func TestRefreshGoesThroughTheProxysRoute(t *testing.T) {
	var target, auth string
	company := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target, auth = r.URL.String(), r.Header.Get("Proxy-Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at-new", "refresh_token": "rt-new"})
	}))
	t.Cleanup(company.Close)
	route, err := proxy.Upstream{HTTP: "http://alice:e2e-not-a-secret@" + company.Listener.Addr().String()}.Route()
	must(t, err)
	f := File{Kind: Codex, TokenURL: "http://auth.example/oauth/token", Path: writeFile(t, map[string]any{"tokens": map[string]any{ //nolint:gosec // fake values
		"access_token": jwtWith(time.Now(), nil), "refresh_token": "rt-old",
	}})}
	if v, err := f.Get(proxy.WithRoute(t.Context(), route), "codex"); err != nil || v != "at-new" {
		t.Fatalf("value = %q, %v", v, err)
	}
	if target != "http://auth.example/oauth/token" || auth != "Basic YWxpY2U6ZTJlLW5vdC1hLXNlY3JldA==" {
		t.Errorf("the company proxy got %q, Proxy-Authorization %q", target, auth)
	}
}

// A refresh trusts the roots its lookup's context carries (proxy.WithRoots): the VM's proxy's, the
// user's CAs among them, so a company proxy that inspects TLS is trusted. Without them, a server
// signed by a CA the system doesn't trust is refused.
func TestRefreshTrustsTheProxysRoots(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at-new", "refresh_token": "rt-new"})
	}))
	t.Cleanup(srv.Close)
	file := func() File {
		return File{Kind: Codex, TokenURL: srv.URL, Path: writeFile(t, map[string]any{"tokens": map[string]any{
			"access_token": jwtWith(time.Now(), nil), "refresh_token": "rt-old",
		}})}
	}
	if _, err := file().Get(t.Context(), "codex"); err == nil {
		t.Fatal("refreshed against a server the system's roots don't trust")
	}
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	if v, err := file().Get(proxy.WithRoots(t.Context(), roots), "codex"); err != nil || v != "at-new" {
		t.Fatalf("value = %q, %v", v, err)
	}
}

// A refresh that started finishes, and is written back, though its caller has gone: the server may
// have rotated the refresh token already.
func TestRefreshOutlivesItsCaller(t *testing.T) {
	u, _ := tokenServer(t, map[string]any{"access_token": "at-new", "refresh_token": "rt-new"})
	f := File{Kind: Codex, TokenURL: u, Path: writeFile(t, map[string]any{"tokens": map[string]any{
		"access_token": jwtWith(time.Now(), nil), "refresh_token": "rt-old",
	}})}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if v, err := f.Get(ctx, "codex"); err != nil || v != "at-new" {
		t.Fatalf("value = %q, %v", v, err)
	}
}

// A refresh the token endpoint refuses, or answers without a token, fails, and the host's file
// stays as it was.
func TestRefreshFails(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    http.HandlerFunc
	}{
		{"refused", func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no", http.StatusBadRequest) }},
		{"no token", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"refresh_token":"x"}`) }},
		{"not JSON", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `<html>`) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.h)
			t.Cleanup(srv.Close)
			f := File{Kind: Opencode, TokenURL: srv.URL, Path: writeFile(t, map[string]any{
				"openai": map[string]any{"type": "oauth", "access": "at-old", "refresh": "rt-old", "expires": 1},
			})}
			before, _ := os.ReadFile(f.Path)
			if v, err := f.Get(t.Context(), "opencode-openai"); err == nil {
				t.Errorf("value = %q", v)
			}
			if after, _ := os.ReadFile(f.Path); string(after) != string(before) {
				t.Errorf("host file = %s", after)
			}
		})
	}
}

func TestCodexGetLeavesAFreshTokenAlone(t *testing.T) {
	u, got := tokenServer(t, map[string]any{"access_token": "at-new"})
	access := jwtWith(time.Now().Add(time.Hour), nil)
	f := File{Kind: Codex, TokenURL: u, Path: writeFile(t, map[string]any{"tokens": map[string]any{"access_token": access, "refresh_token": "rt"}, "OPENAI_API_KEY": "sk-k"})}
	before, _ := os.ReadFile(f.Path)
	if v, err := f.Get(t.Context(), "codex"); err != nil || v != access {
		t.Fatalf("value = %q, %v", v, err)
	}
	if v, err := f.Get(t.Context(), "codex-key"); err != nil || v != "sk-k" {
		t.Fatalf("codex-key = %q, %v", v, err)
	}
	after, _ := os.ReadFile(f.Path)
	if *got != "" || string(before) != string(after) {
		t.Errorf("refreshed or rewrote a fresh login: %s", *got)
	}
}

// What Get refuses: a name the login doesn't have, a login that isn't there, and an agent devvm
// doesn't know.
func TestGetRefuses(t *testing.T) {
	codex := writeFile(t, map[string]any{"tokens": map[string]any{}})
	opencode := writeFile(t, map[string]any{"anthropic": map[string]any{"type": "api", "key": "k"}})
	claude := writeFile(t, map[string]any{"mcpServers": map[string]any{}})
	for _, tc := range []struct {
		f    File
		name string
	}{
		{File{Kind: Codex, Path: codex}, "codex-key"},
		{File{Kind: Codex, Path: codex}, "codex"},
		{File{Kind: Codex, Path: codex}, "codex-other"},
		{File{Kind: Opencode, Path: opencode}, "opencode-nope"},
		{File{Kind: Claude, Path: claude}, "claude-mcp-000000000000"},
		{File{Kind: Codex, Path: filepath.Join(t.TempDir(), "none.json")}, "codex"},
		{File{Kind: "gemini", Path: codex}, "gemini"},
	} {
		if v, err := tc.f.Get(t.Context(), tc.name); err == nil {
			t.Errorf("%s %s = %q", tc.f.Kind, tc.name, v)
		}
	}
	for _, f := range []File{{Kind: "gemini", Path: codex}, {Kind: Codex, Path: filepath.Join(t.TempDir(), "none.json")}} {
		if _, err := f.Bindings(); err == nil {
			t.Errorf("Bindings of %+v", f)
		}
		if _, err := f.VMCopy(); err == nil {
			t.Errorf("VMCopy of %+v", f)
		}
	}
}

// An agent's file is read only as a regular file: a FIFO there can't hold devvm.
func TestLoginFileNotRegular(t *testing.T) {
	p := filepath.Join(t.TempDir(), "auth.json")
	must(t, syscall.Mkfifo(p, 0o600))
	_, err := File{Kind: Codex, Path: p}.Bindings()
	if !errors.Is(err, proxy.ErrNotRegular) {
		t.Errorf("Bindings of a FIFO = %v", err)
	}
	if _, err := ClaudeCopy(p); !errors.Is(err, proxy.ErrNotRegular) {
		t.Errorf("ClaudeCopy of a FIFO = %v", err)
	}
}

func TestOpencodeLogins(t *testing.T) {
	u, got := tokenServer(t, map[string]any{"access_token": "at-new", "refresh_token": "rt-new", "expires_in": 7200})
	f := File{Kind: Opencode, TokenURL: u, Path: writeFile(t, map[string]any{
		"anthropic":      map[string]any{"type": "api", "key": "sk-ant-real"},
		"openai":         map[string]any{"type": "oauth", "access": jwtWith(time.Now(), map[string]any{"https://api.openai.com/auth": openaiAuth}), "refresh": "rt-old", "expires": time.Now().UnixMilli()},
		"github-copilot": map[string]any{"type": "oauth", "access": "gho_real", "refresh": "gho_real", "expires": 0},
		"llama-cpp":      map[string]any{"type": "api", "key": "local"},
		"mcp":            map[string]any{"type": "wellknown", "key": "k", "token": "t"},
	})}
	out, kept := vmCopy(t, f)
	for _, secret := range []string{"sk-ant-real", "rt-old", "gho_real"} {
		if strings.Contains(out, secret) {
			t.Errorf("the VM's copy holds %q", secret)
		}
	}
	// Nothing passes without config.toml's [pass-keys]: an unknown provider's key is withheld.
	if strings.Contains(out, `"local"`) || kept != nil {
		t.Errorf("an unknown provider's key reached the VM (kept %v):\n%s", kept, out)
	}
	if v, _ := f.Get(t.Context(), "opencode-anthropic"); v != "sk-ant-real" {
		t.Errorf("anthropic = %q", v)
	}
	if v, _ := f.Get(t.Context(), "opencode-github-copilot"); v != "gho_real" {
		t.Errorf("copilot = %q", v)
	}
	// openai's access token has expired: refreshed here, as a form, and written back.
	if v, err := f.Get(t.Context(), "opencode-openai"); err != nil || v != "at-new" {
		t.Fatalf("openai = %q, %v", v, err)
	}
	if !strings.HasPrefix(*got, "application/x-www-form-urlencoded ") || !strings.Contains(*got, "refresh_token=rt-old") {
		t.Errorf("refresh request = %s", *got)
	}
	m, _ := readJSON(f.Path)
	if e := obj(m, "openai"); str(e, "refresh") != "rt-new" || str(e, "access") != "at-new" {
		t.Errorf("host file = %v", e)
	}
}

// opencode's auth.json in the VM, whole, as pi's is. A bound key (Anthropic's) becomes {type, key}
// alone, its metadata left on the host, and a bound OAuth login (OpenAI's) keeps only the fields
// that name its account beside its placeholders. Every other entry is withheld, keeping only those
// fields: an unknown provider's key, an OAuth login devvm doesn't bind (a provider it doesn't know,
// Copilot Enterprise) and a well-known login, which holds a token. The api entry of a provider
// File.Pass lists (a local model server) goes as it is, but neither an OAuth login File.Pass names
// nor a bound provider's key File.Pass names.
func TestOpencodeVMCopy(t *testing.T) {
	f := File{Kind: Opencode, Salt: "s1", Pass: []string{"ollama", "groq-oauth", "anthropic", "mcp"}, Path: writeFile(t, map[string]any{
		"anthropic": map[string]any{"type": "api", "key": "sk-ant-e2efake", "metadata": map[string]any{"account": "md-e2efake"}, "extra": "x-e2efake"},
		"openai": map[string]any{
			"type": "oauth", "access": jwtWith(time.Now().Add(time.Hour), map[string]any{"https://api.openai.com/auth": openaiAuth}),
			"refresh": "rt-e2efake-openai", "expires": 1, "accountId": "acct-1", "other": "o-e2efake",
		},
		"ollama":         map[string]any{"type": "api", "key": "ollama-local", "metadata": map[string]any{"host": "kept"}},
		"groq":           map[string]any{"type": "api", "key": "gsk-e2efake", "metadata": map[string]any{"m": "md-e2efake"}},
		"groq-oauth":     map[string]any{"type": "oauth", "access": "at-e2efake-passed", "refresh": "rt-e2efake-passed", "expires": 1},
		"github-copilot": map[string]any{"type": "oauth", "access": "at-e2efake-ghe", "refresh": "rt-e2efake-ghe", "expires": 0, "enterpriseUrl": "ghe.example.com"},
		"mcp":            map[string]any{"type": "wellknown", "key": "k-e2efake", "token": "t-e2efake"}, //nolint:gosec // fake values
	})}
	c, err := f.VMCopy()
	must(t, err)
	var vm map[string]any
	must(t, json.Unmarshal(c.Data, &vm))
	ph := openaiPlaceholder(str(obj(vm, "openai"), "access"), "s1")
	var wantVM map[string]any
	must(t, json.Unmarshal([]byte(`{
		"anthropic": {"type": "api", "key": "devvm-proxy-managed-opencode-anthropic-s1"},
		"openai": {"type": "oauth", "access": "`+ph+`", "refresh": "`+ph+`", "expires": 4070908800000, "accountId": "acct-1"},
		"ollama": {"type": "api", "key": "ollama-local", "metadata": {"host": "kept"}},
		"groq": {"type": "api"},
		"groq-oauth": {"type": "oauth", "expires": 1},
		"github-copilot": {"type": "oauth", "expires": 0, "enterpriseUrl": "ghe.example.com"},
		"mcp": {"type": "wellknown"}
	}`), &wantVM))
	if !reflect.DeepEqual(vm, wantVM) {
		t.Errorf("the VM's copy:\n%s", c.Data)
	}
	if strings.Contains(string(c.Data), "e2efake") {
		t.Errorf("the VM's copy holds a key or token:\n%s", c.Data)
	}
	if !slices.Equal(c.Kept, []string{"ollama"}) || !slices.Equal(c.Withheld, []string{"github-copilot", "groq", "groq-oauth", "mcp"}) {
		t.Errorf("kept %v, withheld %v", c.Kept, c.Withheld)
	}
	// opencode 2's import gets the passed key, its metadata too, beside the bound ones.
	imp, err := OpencodeImport(c.Data)
	must(t, err)
	if !strings.Contains(string(imp), `"id":"cred_devvm_ollama","integrationID":"ollama","label":"devvm","value":{"key":"ollama-local","metadata":{"host":"kept"},"type":"key"}`) ||
		strings.Contains(string(imp), "groq") || strings.Contains(string(imp), "mcp") {
		t.Errorf("opencode's import: %s", imp)
	}
}

// With a salt, every placeholder ends in the VM's key, so two VMs' differ.
func TestLoginSalt(t *testing.T) {
	p := writeFile(t, map[string]any{
		"OPENAI_API_KEY": "sk-real",
		"tokens":         map[string]any{"access_token": jwtWith(time.Now().Add(time.Hour), nil), "refresh_token": "rt"},
	})
	phs := map[string]bool{}
	for _, salt := range []string{"aaa", "bbb"} {
		f := File{Kind: Codex, Path: p, Salt: salt}
		bs := bindings(t, f)
		for _, b := range bs {
			if phs[b.Placeholder] {
				t.Errorf("%s: two VMs share placeholder %q", b.Name, b.Placeholder)
			}
			phs[b.Placeholder] = true
		}
		vm, _ := vmCopy(t, f)
		for _, b := range bs {
			if !strings.Contains(vm, b.Placeholder) {
				t.Errorf("salt %s: the VM's copy lacks %s's placeholder", salt, b.Name)
			}
		}
	}
	if len(phs) != 4 {
		t.Errorf("placeholders: %v", phs)
	}
}

// ~/.claude.json: MCP servers' env values and headers never reach the VM; an HTTPS server's headers
// become bindings for its host, and Get returns the real value.
func TestClaudeMCP(t *testing.T) {
	f := File{Kind: Claude, Salt: "s4lt", Path: writeFile(t, map[string]any{
		"oauthAccount": map[string]any{"emailAddress": "a@b"},
		"projects":     map[string]any{"/x": map[string]any{"mcpServers": map[string]any{"p": map[string]any{"env": map[string]any{"K": "proj-secret"}}}}},
		"mcpServers": map[string]any{
			"remote": map[string]any{"type": "http", "url": "https://mcp.example.com/mcp", "headers": map[string]any{"Authorization": "Bearer remote-secret"}},
			"plain":  map[string]any{"type": "http", "url": "http://mcp.example.net/", "headers": map[string]any{"X-Key": "plain-secret"}},
			"local":  map[string]any{"command": "npx", "args": []any{"srv"}, "env": map[string]any{"API_KEY": "env-secret"}},
			"open":   map[string]any{"command": "srv"},
		},
	})}
	vm, kept := vmCopy(t, f)
	if kept != nil {
		t.Errorf("kept = %q", kept)
	}
	for _, s := range []string{"remote-secret", "plain-secret", "env-secret", "proj-secret", "projects"} {
		if strings.Contains(vm, s) {
			t.Errorf("the VM's copy holds %q:\n%s", s, vm)
		}
	}
	if !strings.Contains(vm, `"a@b"`) || !strings.Contains(vm, `"npx"`) {
		t.Errorf("the VM's copy lost settings:\n%s", vm)
	}
	bs := bindings(t, f)
	if len(bs) != 1 || bs[0].Header != "Authorization" || !slices.Equal(bs[0].Hosts, []string{"mcp.example.com"}) ||
		!strings.Contains(vm, bs[0].Placeholder) || !strings.HasSuffix(bs[0].Placeholder, "-s4lt") {
		t.Fatalf("bindings %+v", bs)
	}
	if v, err := f.Get(t.Context(), bs[0].Name); err != nil || v != "Bearer remote-secret" {
		t.Errorf("get %s: %q %v", bs[0].Name, v, err)
	}
}

// The agent store's copy of ~/.claude.json keeps only its settings, account state and MCP
// servers (these as they are), never an API key or the projects' history.
func TestClaudeCopy(t *testing.T) {
	p := writeFile(t, map[string]any{ //nolint:gosec // fake values
		"theme": "dark", "primaryApiKey": "sk-ant-api03-fake", "customApiKeyResponses": map[string]any{"approved": []any{"fake-approved"}},
		"projects":   map[string]any{"/x": map[string]any{"history": []any{"fake-history"}}},
		"mcpServers": map[string]any{"local": map[string]any{"command": "npx", "env": map[string]any{"API_KEY": "fake-mcp-env"}}},
	})
	b, err := ClaudeCopy(p)
	must(t, err)
	out := string(b)
	for _, s := range []string{"sk-ant-api03-fake", "fake-approved", "fake-history", "projects", "primaryApiKey"} {
		if strings.Contains(out, s) {
			t.Errorf("the copy holds %q:\n%s", s, out)
		}
	}
	for _, s := range []string{`"theme": "dark"`, `"fake-mcp-env"`} {
		if !strings.Contains(out, s) {
			t.Errorf("the copy lost %s:\n%s", s, out)
		}
	}
	if _, err := ClaudeCopy(filepath.Join(t.TempDir(), "none.json")); err == nil {
		t.Error("ClaudeCopy of no file")
	}
}

// One lookup at a time per login folder: a second waits until the first has written back.
func TestLockLogin(t *testing.T) {
	p := filepath.Join(t.TempDir(), "auth.json")
	unlock, err := lockLogin(p)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan bool)
	go func() {
		u, err := lockLogin(p)
		if err == nil {
			u()
		}
		got <- true
	}()
	select {
	case <-got:
		t.Fatal("a second lock didn't wait")
	case <-time.After(200 * time.Millisecond):
	}
	unlock()
	<-got
}

// An MCP server's host goes into the allowlist: only a DNS name, on 443.
func TestClaudeHostIsANameOnly(t *testing.T) {
	for u, want := range map[string]string{
		"https://mcp.example.com/x": "mcp.example.com", "https://MCP.Example.com.:443/": "mcp.example.com",
		"https://1.2.3.4/": "", "https://[::1]/": "", "https://*.example.com/": "", "https://localhost/": "",
		"http://mcp.example.com/": "", "https://mcp.example.com:8443/": "", "https://a;b.com/": "",
		"https://evil..example.com/": "",
	} {
		if got := claudeHost(map[string]any{"url": u}); got != want {
			t.Errorf("claudeHost(%q) = %q, want %q", u, got, want)
		}
	}
}

// A Codex login's bindings: its key's for api.openai.com, its ChatGPT login's for both hosts.
func TestCodexBindings(t *testing.T) {
	f := File{Kind: Codex, Path: writeFile(t, map[string]any{
		"OPENAI_API_KEY": "sk-real",
		"tokens":         map[string]any{"access_token": jwtWith(time.Now().Add(time.Hour), nil)},
	})}
	var got []string
	for _, b := range bindings(t, f) {
		got = append(got, b.Name+" "+strings.Join(b.Hosts, ","))
	}
	if want := []string{"codex-key api.openai.com", "codex chatgpt.com,api.openai.com"}; !slices.Equal(got, want) {
		t.Errorf("bindings = %q, want %q", got, want)
	}
}

// Get's errors that logging in again fixes are typed, so devvm names what to do: Codex without the
// login a binding names, and a refresh the token endpoint refuses.
func TestGetErrorsSayWhatToDo(t *testing.T) {
	none := File{Kind: Codex, Path: writeFile(t, map[string]any{"tokens": map[string]any{}})}
	if _, err := none.Get(t.Context(), "codex-key"); !errors.Is(err, ErrCodexNoKey) {
		t.Errorf("codex-key: %v", err)
	}
	if _, err := none.Get(t.Context(), "codex"); !errors.Is(err, ErrCodexNoChatGPT) {
		t.Errorf("codex: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no", http.StatusBadRequest) }))
	t.Cleanup(srv.Close)
	for _, tc := range []struct {
		f        File
		name     string
		provider string
	}{
		{File{Kind: Codex, TokenURL: srv.URL, Path: writeFile(t, map[string]any{"tokens": map[string]any{
			"access_token": jwtWith(time.Now(), nil), "refresh_token": "rt",
		}})}, "codex", ""},
		{File{Kind: Opencode, TokenURL: srv.URL, Path: writeFile(t, map[string]any{
			"openai": map[string]any{"type": "oauth", "access": "at", "refresh": "rt", "expires": 1},
		})}, "opencode-openai", "openai"},
	} {
		_, err := tc.f.Get(t.Context(), tc.name)
		var re *RefreshError
		var se *fetch.StatusError
		if !errors.As(err, &re) || re.Agent != tc.f.Kind || re.Provider != tc.provider || !errors.As(err, &se) || err.Error() == "" {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}

// Nothing in the VM's ~/.claude.json carries a secret: not URLs, not
// args, not top-level keys; settings and harmless values stay.
func TestClaudeVMScrubsURLsArgsAndUnknownKeys(t *testing.T) {
	f := File{Kind: Claude, Path: writeFile(t, map[string]any{ //nolint:gosec // fake values
		"primaryApiKey": "sk-ant-api03-TOPSECRET", "someFutureKey": "x", "theme": "dark",
		"mcpServers": map[string]any{
			"u": map[string]any{"type": "http", "url": "https://user:DUMMYPASS@mcp.example.com/s/Ab3dEf6hIj9kLm2nOp5qRs8tUv/mcp?api_key=Q1&region=eu&x=zz9Yy8Xx7Ww6Vv5Uu4Tt3Ss2Rr1"}, //nolint:gosec // fake values
			"z": map[string]any{"type": "http", "url": "https://mcp.zapier.com/api/mcp/s/3f2c1b4a-9e8d-4c7b-a6f5-0e1d2c3b4a59/mcp"},
			"a": map[string]any{
				"command": "npx", "args": []any{
					"-y", "@scope/server-x", "--api-key", "DUMMYARG", "--token=DUMMYEQ",
					"--port", "8080", "ghp_DUMMYTOKENVALUE123", "/home/me/data",
				}, "extra": "DUMMYEXTRA",
				"oauth": map[string]any{"clientId": "cid", "clientSecret": "DUMMYCS"},
			},
			"h": map[string]any{
				"type": "http", "url": "https://h.example.com/mcp",
				"headersHelper": `echo '{"Authorization": "Bearer DUMMYHELPER0123456789"}'`,
			},
			"c": map[string]any{"command": "env KEY=sk-DUMMYCOMMAND run-server"},
		},
	})}
	out, _ := vmCopy(t, f)
	for _, s := range []string{
		"TOPSECRET", "someFutureKey", "DUMMYPASS", "Ab3dEf6hIj9kLm2nOp5qRs8tUv", "Q1", "zz9Yy8Xx7Ww6",
		"3f2c1b4a-9e8d", "DUMMYARG", "DUMMYEQ", "ghp_DUMMY", "DUMMYEXTRA", "DUMMYCS", "DUMMYHELPER", "DUMMYCOMMAND",
	} {
		if strings.Contains(out, s) {
			t.Errorf("the VM's copy holds %q:\n%s", s, out)
		}
	}
	for _, s := range []string{
		`"theme": "dark"`, "region=eu", "mcp.example.com", "@scope/server-x", `"8080"`, "/home/me/data", `"cid"`, "--token=devvm-redacted",
		`"command": "npx"`, "env KEY=devvm-redacted run-server", `Bearer devvm-redacted`,
	} {
		if !strings.Contains(out, s) {
			t.Errorf("the VM's copy lost %q:\n%s", s, out)
		}
	}
}
