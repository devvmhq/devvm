package agentlogin

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/proxy"
)

// golden compares got with testdata/golden/name, or writes it there with UPDATE_GOLDEN=1 in the
// test's environment: UPDATE_GOLDEN=1 go test ./internal/agentlogin -run Golden.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	p := filepath.Join("testdata", "golden", name)
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		must(t, os.MkdirAll(filepath.Dir(p), 0o750))
		must(t, os.WriteFile(p, got, 0o600))
		return
	}
	want, err := os.ReadFile(p) //nolint:gosec // the test's own golden file
	must(t, err)
	if !bytes.Equal(got, want) {
		t.Errorf("%s changed:\n--- got\n%s\n--- want\n%s", name, got, want)
	}
}

// lastRefresh is Codex's VM copy's last_refresh, which is the time of the copy.
var lastRefresh = regexp.MustCompile(`"last_refresh": "[^"]*"`)

// goldenLogins are a login file of each kind on the host, its keys and tokens fakes, with every
// shape each kind's VM copy, bindings and lists tell apart.
func goldenLogins() map[string]string {
	future := time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC)
	access := jwtWith(future, map[string]any{
		"https://api.openai.com/auth": openaiAuth, "email": "e2efake@example.com", "other": "dropped",
	})
	id := jwtWith(future, map[string]any{"https://api.openai.com/profile": map[string]any{"email": "e2efake@example.com"}})
	enc := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	return map[string]string{
		Codex: enc(map[string]any{
			"OPENAI_API_KEY": "sk-e2efake", "auth_mode": "chatgpt", "last_refresh": "2026-01-01T00:00:00Z",
			"tokens":   map[string]any{"id_token": id, "access_token": access, "refresh_token": "rt-e2efake", "account_id": "acct-1"}, //nolint:gosec // fake values
			"zz_extra": map[string]any{"kept": true}, "aa_other": "kept",
		}),
		Opencode: enc(map[string]any{
			"anthropic":      map[string]any{"type": "api", "key": "sk-ant-e2efake", "metadata": map[string]any{"m": "md-e2efake"}},
			"ollama":         map[string]any{"type": "api", "key": "ollama-local", "metadata": map[string]any{"host": "kept"}},
			"openrouter":     map[string]any{"type": "api", "key": "sk-or-e2efake"},
			"openai":         map[string]any{"type": "oauth", "access": access, "refresh": "rt-e2efake", "expires": future.UnixMilli(), "accountId": "acct-1"},
			"github-copilot": map[string]any{"type": "oauth", "access": "gho_e2efake", "refresh": "gho_e2efake", "expires": 0},
			"copilot-ghe":    map[string]any{"type": "oauth", "access": "at-e2efake", "refresh": "rt-e2efake", "expires": 1, "enterpriseUrl": "ghe.example.com"},
			"llama-cpp":      map[string]any{"type": "api", "key": "local"},
			"mcp":            map[string]any{"type": "wellknown", "key": "k-e2efake", "token": "t-e2efake"}, //nolint:gosec // fake values
		}),
		Pi: piHostLogins,
		Claude: enc(map[string]any{ //nolint:gosec // fake values
			"theme": "dark", "primaryApiKey": "sk-ant-api03-e2efake", "oauthAccount": map[string]any{"emailAddress": "a@b"},
			"projects": map[string]any{"/x": map[string]any{"history": []any{"e2efake-history"}}},
			"mcpServers": map[string]any{
				"remote": map[string]any{"type": "http", "url": "https://mcp.example.com/mcp?key=e2efake-key-1234567890abcdef", "headers": map[string]any{"Authorization": "Bearer e2efake"}},
				"plain":  map[string]any{"type": "http", "url": "http://mcp.example.net/", "headers": map[string]any{"X-Key": "e2efake-plain"}},
				"local":  map[string]any{"command": "npx srv --token sk-e2efake-1234567890", "args": []any{"--api-key", "e2efake", "srv"}, "env": map[string]any{"API_KEY": "e2efake-env"}, "extra": 1},
				"oauth":  map[string]any{"type": "http", "url": "https://o.example.com/", "oauth": map[string]any{"clientId": "c", "callbackPort": 1, "clientSecret": "e2efake"}},
			},
		}),
		ClaudeOAuth: enc(map[string]any{"claudeAiOauth": map[string]any{ //nolint:gosec // fake values
			"accessToken": claudeTokenStart + "e2efake", "refreshToken": "rt-e2efake", "expiresAt": future.UnixMilli(),
			"scopes": []any{"user:inference", "user:profile"}, "subscriptionType": "max", "rateLimitTier": "t", "other": "dropped",
		}}),
	}
}

// What devvm makes of each kind's login file, byte for byte: the VM's copy, what it keeps and
// withholds, the bindings, opencode's import, the ChatGPT login, and the API keys devvm saves.
func TestLoginsGolden(t *testing.T) {
	for kind, data := range goldenLogins() {
		t.Run(kind, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "auth.json")
			must(t, os.WriteFile(p, []byte(data), 0o600))
			f := File{Kind: kind, Path: p, Salt: "s4lt", Pass: []string{"ollama", "groq-oauth"}}
			var out bytes.Buffer
			bs, err := f.Bindings()
			must(t, err)
			// opencode's come in its map's order.
			slices.SortFunc(bs, func(a, b proxy.Binding) int { return strings.Compare(a.Name, b.Name) })
			for _, b := range bs {
				line, _ := json.Marshal(b)
				out.WriteString("binding " + string(line) + "\n")
			}
			c, err := f.VMCopy()
			must(t, err)
			out.WriteString("kept " + strings.Join(c.Kept, ",") + "\n")
			out.WriteString("withheld " + strings.Join(c.Withheld, ",") + "\n")
			out.WriteString("providers " + strings.Join(KeyProviders(kind), ",") + "\n")
			email, ok, err := f.ChatGPT()
			must(t, err)
			out.WriteString("chatgpt " + email + " " + map[bool]string{true: "yes", false: "no"}[ok] + "\n")
			out.WriteString("vm copy\n" + lastRefresh.ReplaceAllString(string(c.Data), `"last_refresh": "NOW"`))
			if kind == Opencode {
				imp, err := OpencodeImport(c.Data)
				must(t, err)
				out.WriteString("opencode import\n" + string(imp) + "\n")
			}
			for _, provider := range append([]string{""}, KeyProviders(kind)...) {
				k := File{Kind: kind, Path: filepath.Join(t.TempDir(), "auth.json")}
				must(t, os.WriteFile(k.Path, []byte(data), 0o600))
				if err := k.SetKey(provider, "sk-e2efake-saved"); err != nil {
					out.WriteString("set key " + provider + ": " + err.Error() + "\n")
					continue
				}
				saved, err := os.ReadFile(k.Path)
				must(t, err)
				out.WriteString("set key " + provider + "\n" + string(saved))
			}
			golden(t, kind+".txt", out.Bytes())
		})
	}
}
