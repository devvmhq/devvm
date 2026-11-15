package agentlogin

import (
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
)

// redacted returns Redact's copy of in, and the redacted keys' paths, a line each.
func redacted(t *testing.T, format, in string) (string, string) {
	t.Helper()
	out, paths, err := Redact(format, []byte(in))
	if err != nil {
		t.Fatal(err)
	}
	names := ""
	for _, p := range paths {
		names += p + "\n"
	}
	return string(out), names
}

// What Redact refuses: a format it doesn't know, and a file that isn't one.
func TestRedactRefuses(t *testing.T) {
	for _, tc := range []struct{ format, in string }{
		{"yaml", "a: b"},
		{"json", "{"},
		{"toml", "x = \"\"\"\nunclosed\n"},
		{"toml", "a = 1\na = 2\n"},
		{"json", "{} {}"},
	} {
		if out, paths, err := Redact(tc.format, []byte(tc.in)); err == nil || out != nil || paths != nil {
			t.Errorf("Redact(%s, %q) = %q, %q, %v", tc.format, tc.in, out, paths, err)
		}
	}
}

// A JSONC file's strings stay as they are, commas and comment markers in them included; and a file
// with nothing to redact has no paths.
func TestRedactJSONKeepsStrings(t *testing.T) {
	out, names := redacted(t, "json", `{"a": ",}", "b": "x // y", "c": [1,], /* z */ "d": ", ]"}`)
	for _, s := range []string{`"a": ",}"`, `"b": "x // y"`, `"d": ", ]"`} {
		if !strings.Contains(out, s) {
			t.Errorf("copy changed %s:\n%s", s, out)
		}
	}
	if names != "" {
		t.Errorf("names = %q", names)
	}
}

// Codex's config.toml with secrets in headers and env, and settings that only name them.
func TestRedactTOML(t *testing.T) {
	in := `model = "o4"
preferred_auth_method = "apikey"
mcp_servers.u.env.SECRET_X = 'DUMMY-SQ'

[model_providers.x]
env_key = "OPENAI_API_KEY"
http_headers = { "Authorization" = "Bearer DUMMY-CODEX", "X-Org" = "acme" }

[mcp_servers.s]
command = "npx"
args = ["srv"]
env = { API_KEY = "DUMMY-ENV", MODE = "fast" }

[mcp_servers.k]
args = ["-y", "srv-k", "--api-key", "DUMMY-ARG"]

[mcp_servers.t.env]
GITHUB_TOKEN = "DUMMY-GH" # a comment
`
	out, names := redacted(t, "toml", in)
	for _, s := range []string{"DUMMY-CODEX", "DUMMY-ENV", "DUMMY-GH", "DUMMY-SQ", "DUMMY-ARG", "acme", "fast"} {
		if strings.Contains(out, s) {
			t.Errorf("copy holds %q:\n%s", s, out)
		}
	}
	var got map[string]any
	if _, err := toml.Decode(out, &got); err != nil {
		t.Fatalf("the copy isn't TOML: %v\n%s", err, out)
	}
	for path, want := range map[string]any{
		"model":                     "o4",
		"preferred_auth_method":     "apikey",
		"model_providers.x.env_key": "OPENAI_API_KEY",
		"model_providers.x.http_headers.Authorization": redactMark,
		"mcp_servers.s.command":                        "npx",
		"mcp_servers.s.env.API_KEY":                    redactMark,
		"mcp_servers.t.env.GITHUB_TOKEN":               redactMark,
	} {
		v := any(got)
		for _, k := range strings.Split(path, ".") {
			v = v.(map[string]any)[k]
		}
		if v != want {
			t.Errorf("%s = %v, want %v", path, v, want)
		}
	}
	want := "mcp_servers.k.args\nmcp_servers.s.env.API_KEY\nmcp_servers.s.env.MODE\nmcp_servers.t.env.GITHUB_TOKEN\nmcp_servers.u.env.SECRET_X\n" +
		"model_providers.x.http_headers.Authorization\nmodel_providers.x.http_headers.X-Org\n"
	if names != want {
		t.Errorf("names = %q, want %q", names, want)
	}
	// A multi-line string is read like any other.
	if out, _ := redacted(t, "toml", "[mcp_servers.s.env]\nX = \"\"\"\nDUMMY-ML\n\"\"\"\n"); strings.Contains(out, "DUMMY-ML") {
		t.Errorf("a multi-line string was copied:\n%s", out)
	}
}

// TOML that a line-by-line reading misses: each fake value is taken out of the copy, and a file that
// isn't TOML is refused. Nothing reaches the VM unread.
func TestRedactTOMLLeavesNothingUnread(t *testing.T) {
	for name, in := range map[string]string{ //nolint:gosec // fake values
		"a multi-line array":   "[mcp_servers.s]\nargs = [\n  \"--api-key\",\n  \"e2e-not-a-secret\",\n]\n",
		"a quoted dotted key":  "mcp_servers.\"my server\".env.API_KEY = \"e2e-not-a-secret\"\n",
		"spaces around dots":   "mcp_servers . s . env . TOKEN = \"e2e-not-a-secret\"\n",
		"an env value":         "[mcp_servers.s.env]\nDD_SITE = \"e2e-not-a-secret\"\n",
		"a header":             "[mcp_servers.s]\nhttp_headers = { \"X-Custom\" = \"e2e-not-a-secret\" }\n",
		"a set variable":       "[shell_environment_policy]\nset = { MY_VAR = \"e2e-not-a-secret\" }\n",
		"a URL's user info":    "[mcp_servers.s]\nurl = \"https://me:e2e-not-a-secret@mcp.example.com/\"\n",
		"a URL's query string": "[mcp_servers.s]\nurl = \"https://mcp.example.com/sse?apikey=e2e-not-a-secret\"\n",
		"a line it can't read": "model = \"o4\"\nnot toml at all e2e-not-a-secret\n",
		"an array of tables":   "[[profiles]]\nname = \"a\"\napi_key = \"e2e-not-a-secret\"\n",
		"URLs in arguments":    "[mcp_servers.s]\nargs = [\"--url=https://me:e2e-not-a-secret@x.example.com/\", \"https://e2e-not-a-secret@y.example.com/\"]\n",
	} {
		out, _, err := Redact("toml", []byte(in))
		switch {
		case name == "a line it can't read":
			if err == nil {
				t.Errorf("%s: copied:\n%s", name, out)
			}
		case err != nil:
			t.Errorf("%s: refused: %v", name, err)
		case strings.Contains(string(out), "e2e-not-a-secret"):
			t.Errorf("%s: the copy holds the secret:\n%s", name, out)
		}
	}
}

// JSON's arrays, URLs, env tables and headers: as ~/.claude.json's MCP servers are scrubbed.
func TestRedactJSONArraysURLsAndEnv(t *testing.T) {
	in := //nolint:gosec // fake values
		`{
  "mcp": {
    "a": {"type": "local", "command": ["npx", "srv", "--api-key", "fake-arg"], "environment": {"DD_SITE": "fake-env", "PORT": "8080"}},
    "b": {"type": "remote", "url": "https://mcp.example.com/sse?apikey=fake-query&region=eu", "headers": {"X-Custom": "fake-header"}},
    "c": {"type": "remote", "url": "https://me:fake-pass@mcp.example.com/"}
  },
  "apiKeys": ["fake-list"],
  "env": {"MY_CRED": "fake-cred", "HOOK_TOKEN": "https://hooks.example.com/fake-in-url","ANTHROPIC_BASE_URL": "https://gw.example.com", "OTHER": "{env:X}", "DISABLE_TELEMETRY": "1", "REF": "${X}", "KEYFILE": "~/k", "HELPER": "./k", "EMPTY": ""},
  "keybinds": {"leader": "ctrl+x"},
  "tokens": {"gh": "fake-nested"},
  "note": "sk-fake-anywhere"
}`
	out, names := redacted(t, "json", in)
	for _, s := range []string{"fake-arg", "fake-env", "fake-query", "fake-header", "fake-pass", "fake-list", "fake-cred", "fake-nested", "fake-anywhere", "fake-in-url"} {
		if strings.Contains(out, s) {
			t.Errorf("copy holds %q:\n%s", s, out)
		}
	}
	for _, s := range []string{`"8080"`, "region=eu", `"https://gw.example.com"`, `"{env:X}"`, `"DISABLE_TELEMETRY": "1"`, `"${X}"`, `"~/k"`, `"./k"`, `"leader": "ctrl+x"`, `"srv"`} {
		if !strings.Contains(out, s) {
			t.Errorf("copy lost %s:\n%s", s, out)
		}
	}
	want := "apiKeys\nenv.HOOK_TOKEN\nenv.MY_CRED\nmcp.a.command\nmcp.a.environment.DD_SITE\nmcp.b.headers.X-Custom\nmcp.b.url\nmcp.c.url\nnote\ntokens.gh\n"
	if names != want {
		t.Errorf("names = %q, want %q", names, want)
	}
}

// Claude Code's own settings env keeps its known configuration variables, but none whose name says
// it's a credential, and nothing else; an MCP server's env keeps no value at all.
func TestRedactClaudeEnv(t *testing.T) {
	in := //nolint:gosec // fake values
		`{"env": {
  "ANTHROPIC_MODEL": "claude-opus-4-1", "ANTHROPIC_SMALL_FAST_MODEL": "claude-haiku-4-5",
  "ANTHROPIC_DEFAULT_SONNET_MODEL": "claude-sonnet-4-5", "CLAUDE_CODE_EFFORT_LEVEL": "high",
  "CLAUDE_CODE_MAX_OUTPUT_TOKENS": "128000", "BASH_DEFAULT_TIMEOUT_MS": "600000", "DISABLE_TELEMETRY": "yes",
  "MAX_THINKING_TOKENS": "100000",
  "ANTHROPIC_AUTH_TOKEN": "fake-auth-token", "CLAUDE_CODE_OAUTH_TOKEN": "fake-oauth",
  "CLAUDE_CODE_GATEWAY_HINT_HEADERS": "fake-hint-header", "CLAUDE_CODE_CLIENT_KEY_PASSPHRASE": "fake-passphrase",
  "MY_SERVICE_URL_WITH_PASS": "https://me:fake-url-pass@svc.example.com/", "MY_SERVICE": "fake-unknown"
},
"mcpServers": {"m": {"env": {"ANTHROPIC_MODEL": "fake-mcp-model"}}}}`
	out, names := redacted(t, "json", in)
	for _, s := range []string{
		`"ANTHROPIC_MODEL": "claude-opus-4-1"`, `"ANTHROPIC_SMALL_FAST_MODEL": "claude-haiku-4-5"`,
		`"ANTHROPIC_DEFAULT_SONNET_MODEL": "claude-sonnet-4-5"`, `"CLAUDE_CODE_EFFORT_LEVEL": "high"`,
		`"CLAUDE_CODE_MAX_OUTPUT_TOKENS": "128000"`, `"BASH_DEFAULT_TIMEOUT_MS": "600000"`, `"DISABLE_TELEMETRY": "yes"`,
		`"MAX_THINKING_TOKENS": "100000"`,
	} {
		if !strings.Contains(out, s) {
			t.Errorf("copy lost %s:\n%s", s, out)
		}
	}
	for _, s := range []string{"fake-auth-token", "fake-oauth", "fake-hint-header", "fake-passphrase", "fake-url-pass", "svc.example.com", "fake-unknown", "fake-mcp-model"} {
		if strings.Contains(out, s) {
			t.Errorf("copy holds %q:\n%s", s, out)
		}
	}
	want := "env.ANTHROPIC_AUTH_TOKEN\nenv.CLAUDE_CODE_CLIENT_KEY_PASSPHRASE\nenv.CLAUDE_CODE_GATEWAY_HINT_HEADERS\nenv.CLAUDE_CODE_OAUTH_TOKEN\n" +
		"env.MY_SERVICE\nenv.MY_SERVICE_URL_WITH_PASS\nmcpServers.m.env.ANTHROPIC_MODEL\n"
	if names != want {
		t.Errorf("names = %q, want %q", names, want)
	}
}

// Free text, such as a hook's command or an argument holding a header, loses the keys of a known
// shape and the Bearer and Basic credentials in it, and keeps its other words; prose and references
// stay.
func TestRedactFreeText(t *testing.T) {
	in := `{
  "hooks": {"Stop": [{"hooks": [{"type": "command", "command": "/usr/bin/curl -H 'x-api-key: sk-fake-in-hook' https://hooks.example.com"}]}]},
  "statusLine": {"command": "notify --auth \"Bearer fakefakefakefake0001\" done"},
  "mcp": {"m": {"command": ["curl", "-H", "Authorization: Basic ZmFrZTpmYWtlZmFrZWZha2U=", "-H", "X-Key: AKIAFAKEFAKEFAKE"]}},
  "note": "a bearer token goes in the basic setup, as risk-assessment says",
  "theme": "basic monokai-darker-extended",
  "notify": {"command": "/usr/bin/curl -s https://hooks.example.com/services/T0/B0/fakefakefakefakefake0001 ; notify https://x.example.com/?token=fake-hook-query"},
  "quoted": "send --auth Bearer 'fakefakefakefake0002'",
  "paths": "/opt/sk-tools/bin/run --style basic /Users/me/notes/x1",
  "tokenList": ["/bin/helper ghp_fakefakefake0003"],
  "ref": "curl -H \"Authorization: Bearer ${GITHUB_TOKEN}\""
}`
	out, names := redacted(t, "json", in)
	for _, s := range []string{
		"sk-fake-in-hook", "fakefakefakefake0001", "ZmFrZTpmYWtlZmFrZWZha2U=", "AKIAFAKEFAKEFAKE",
		"fakefakefakefakefake0001", "fake-hook-query", "fakefakefakefake0002", "fakefakefake0003",
	} {
		if strings.Contains(out, s) {
			t.Errorf("copy holds %q:\n%s", s, out)
		}
	}
	for _, s := range []string{
		"/usr/bin/curl -H 'x-api-key: devvm-redacted' https://hooks.example.com",
		`notify --auth \"Bearer devvm-redacted\" done`, `"Authorization: Basic devvm-redacted"`, `"X-Key: devvm-redacted"`,
		"a bearer token goes in the basic setup, as risk-assessment says", `Bearer ${GITHUB_TOKEN}`, "basic monokai-darker-extended",
		"/usr/bin/curl -s https://hooks.example.com/services/T0/B0/devvm-redacted ; notify https://x.example.com/?token=devvm-redacted",
		"send --auth Bearer 'devvm-redacted'", "/opt/sk-tools/bin/run --style basic /Users/me/notes/x1", `"/bin/helper devvm-redacted"`,
	} {
		if !strings.Contains(out, s) {
			t.Errorf("copy lost %s:\n%s", s, out)
		}
	}
	if want := "hooks.Stop[0].hooks[0].command\nmcp.m.command\nnotify.command\nquoted\nstatusLine.command\ntokenList\n"; names != want {
		t.Errorf("names = %q, want %q", names, want)
	}
}

// opencode.json (JSONC) and Claude Code's settings.json.
func TestRedactJSON(t *testing.T) {
	in := `{
  // a comment with "quotes"
  "provider": {"anthropic": {"options": {"apiKey": "DUMMY-OC", "baseURL": "https://x"}}},
  "other": {"options": {"apiKey": "{env:OTHER_KEY}"}},
  "mcp": {"m": {"headers": {"Authorization": "Bearer DUMMY-H"}, "command": ["a", "b"]}},
  "env": {"ANTHROPIC_API_KEY": "DUMMY-CL", "ANTHROPIC_BASE_URL": "https://gw", "N": 1},
  "apiKeyHelper": "/usr/local/bin/key-helper", /* trailing */
}`
	out, names := redacted(t, "json", in)
	for _, s := range []string{"DUMMY-OC", "DUMMY-H", "DUMMY-CL"} {
		if strings.Contains(out, s) {
			t.Errorf("copy holds %q:\n%s", s, out)
		}
	}
	for _, s := range []string{"{env:OTHER_KEY}", "https://gw", "/usr/local/bin/key-helper", `"baseURL": "https://x"`} {
		if !strings.Contains(out, s) {
			t.Errorf("copy lost %q:\n%s", s, out)
		}
	}
	if want := "env.ANTHROPIC_API_KEY\nmcp.m.headers.Authorization\nprovider.anthropic.options.apiKey\n"; names != want {
		t.Errorf("names = %q, want %q", names, want)
	}
}

// The secrets FuzzRedact plants: one of a known key's shape, and one that only where it is says
// is a secret.
const (
	plantedShape = "ghp_Zq8vX2mK9pL4wR7tY1nB3c"
	plantedPlain = "Zq8vX2mK9pL4wR7tY1nB"
)

// plant puts a secret in the settings file m, below its keys k1 and k2, in the place how picks:
// a known key's shape as a value and among other words, a URL's password and query, a value under
// a secret-looking key, an env or headers table's value, and a flag's argument. It returns the
// secret, and false when Redact keeps what it planted by design: one of Claude Code's own
// settings in its top-level env (agentSetting).
func plant(m map[string]any, k1, k2, key, around string, how uint8) (string, bool) {
	at := func(keys ...string) map[string]any {
		cur := m
		for _, k := range keys {
			next, ok := cur[k].(map[string]any)
			if !ok {
				next = map[string]any{}
				cur[k] = next
			}
			cur = next
		}
		return cur
	}
	switch how % 8 {
	case 0:
		at(k1, k2)[key] = plantedShape
		return plantedShape, true
	case 1:
		at(k1, k2)[key] = around + " " + plantedShape + " " + around
		return plantedShape, true
	case 2:
		at(k1, k2)[key] = "https://user:" + plantedPlain + "@proxy.example/x"
		return plantedPlain, true
	case 3:
		at(k1, k2)[key] = "https://h.example/p?" + url.QueryEscape(around) + "=" + plantedShape
		return plantedShape, true
	case 4:
		at(k1, k2)[key+"_token"] = plantedPlain
		return plantedPlain, true
	case 5:
		at(k1, "env")[key] = plantedPlain
		return plantedPlain, k1 != "" || !agentSetting("env."+key, key)
	case 6:
		at(k1, k2)["args"] = []any{"--api-key", plantedPlain}
		return plantedPlain, true
	default:
		at(k1, "headers")["Authorization"] = "Bearer " + plantedPlain
		return plantedPlain, true
	}
}

// FuzzRedact: no secret planted anywhere in an agent's settings file, JSON or TOML, survives into
// the VM's copy, which is the format's own and reads back.
func FuzzRedact(f *testing.F) {
	for i := range uint8(8) {
		f.Add(false, []byte(`{"mcp":{"m":{"command":"x","env":{"A":"b"}}},"model":"sonnet"}`), "mcp", "m", "TOKEN", "run it", i)
		f.Add(true, []byte("[mcp_servers.m]\ncommand = \"x\"\nargs = [\"-y\"]\n"), "mcp_servers", "m", "x", "", i)
		f.Add(false, []byte(`{}`), "", "", "CLAUDE_CODE_X", "Bearer", i)
	}
	f.Fuzz(func(t *testing.T, isTOML bool, doc []byte, k1, k2, key, around string, how uint8) {
		// A file that isn't UTF-8 is refused before Redact reads it.
		if strings.Contains(string(doc), plantedPlain) || strings.Contains(around, plantedPlain) || !utf8.ValidString(k1+k2+key+around) {
			return
		}
		format := "json"
		if isTOML {
			format = "toml"
		}
		m := tableOf(format, doc)
		secret, gone := plant(m, k1, k2, key, around, how)
		in, err := encode(format, m)
		if err != nil || !utf8.Valid(in) {
			return
		}
		out, _, err := Redact(format, in)
		if err != nil {
			t.Fatalf("Redact refused what it wrote itself: %v\n%s", err, in)
		}
		if gone && strings.Contains(string(out), secret) {
			t.Fatalf("the secret survived:\n%s\nfrom\n%s", out, in)
		}
		if _, err := decode(format, out); err != nil {
			t.Fatalf("the copy doesn't read back: %v\n%s", err, out)
		}
	})
}

// tableOf is the settings file doc as a table, or an empty one when it isn't one.
func tableOf(format string, doc []byte) map[string]any {
	v, err := decode(format, doc)
	if m, ok := v.(map[string]any); err == nil && ok {
		return m
	}
	return map[string]any{}
}
