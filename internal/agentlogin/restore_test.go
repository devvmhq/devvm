package agentlogin

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// restored is Restore of a VM's file, the copy the VM got being store's Redact.
func restored(t *testing.T, format, vm, store string) (string, error) {
	t.Helper()
	given, _, err := Redact(format, []byte(store))
	if err != nil {
		t.Fatal(err)
	}
	out, err := Restore(format, []byte(vm), given, []byte(store))
	return string(out), err
}

// A file without placeholders is kept as it is, comments and all, once it parses whole; one that
// doesn't parse is refused.
func TestRestoreWithoutPlaceholders(t *testing.T) {
	in := "// mine\n{\"theme\": \"dark\"}\n"
	out, err := Restore("json", []byte(in), nil, nil)
	if err != nil || string(out) != in {
		t.Errorf("Restore = %q, %v", out, err)
	}
	for _, tc := range []struct{ format, in string }{
		{"json", "{"}, {"json", "{} {}"}, {"toml", "a = 1\na = 2\n"}, {"yaml", "a: b"},
	} {
		if out, err := Restore(tc.format, []byte(tc.in), nil, nil); err == nil || out != nil {
			t.Errorf("Restore(%s, %q) = %q, %v", tc.format, tc.in, out, err)
		} else if _, ok := errors.AsType[*PlaceholderError](err); ok {
			t.Errorf("Restore(%s, %q): a placeholder error for a file that doesn't parse", tc.format, tc.in)
		}
	}
}

// The store's secrets go back where the VM's copy had their placeholders, the VM's own changes
// elsewhere kept: JSON and TOML, in tables, lists and Codex's [[tables]].
func TestRestorePutsSecretsBack(t *testing.T) {
	store := `{"theme": "dark", "env": {"ANTHROPIC_AUTH_TOKEN": "testfake-auth"},
		"mcpServers": {"s": {"command": "srv", "args": ["--api-key", "testfake-arg"], "env": {"K": "testfake-env"}}}}`
	given, _, _ := Redact("json", []byte(store))
	vm := strings.Replace(string(given), `"dark"`, `"light"`, 1)
	out, err := Restore("json", []byte(vm), given, []byte(store))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{`"light"`, `"testfake-auth"`, `"testfake-arg"`, `"testfake-env"`} {
		if !strings.Contains(string(out), s) {
			t.Errorf("no %s in:\n%s", s, out)
		}
	}
	if HasPlaceholder(out) {
		t.Errorf("a placeholder stayed:\n%s", out)
	}

	toml := "model = \"o4\"\n[mcp_servers.s.env]\nAPI_KEY = \"testfake-toml\"\n[[profiles]]\nname = \"p\"\n[profiles.env]\nTOKEN = \"testfake-list\"\n"
	tgiven, _, _ := Redact("toml", []byte(toml))
	tvm := strings.Replace(string(tgiven), `"o4"`, `"o5"`, 1)
	tout, err := Restore("toml", []byte(tvm), tgiven, []byte(toml))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{`"o5"`, `"testfake-toml"`, `"testfake-list"`} {
		if !strings.Contains(string(tout), s) {
			t.Errorf("no %s in:\n%s", s, tout)
		}
	}

	// A file that is one string, a placeholder as the VM got it, has no table around it to
	// compare: the store's value goes back.
	one := `"testfake devvm-redacted"`
	if out, err := Restore("json", []byte(one), []byte(one), []byte(`"testfake-one"`)); err != nil || string(out) != "\"testfake-one\"\n" {
		t.Errorf("Restore of a file that is one string = %q, %v", out, err)
	}
}

// A placeholder that wasn't in the VM's copy at that path, that the store has no value for, or
// whose surroundings changed (its table, or the one above it: where the secret is sent) is
// refused, naming its path; a change to the file's top level elsewhere isn't such a change.
func TestRestoreRefuses(t *testing.T) {
	store := `{"theme": "dark", "mcpServers": {"s": {"type": "http", "url": "https://mcp.example.com", "headers": {"X-Key": "testfake-header"}},
		"t": {"command": "srv", "env": {"K": "testfake-env", "BASE": "https://a.example.com"}}}}`
	for _, tc := range []struct{ name, from, to, path string }{
		{"moved", `"theme": "dark"`, `"theme": "devvm-redacted"`, "theme"},
		{"new", `"theme": "dark"`, `"theme": "dark", "x": {"y": "devvm-redacted"}`, "x.y"},
		{"url", `"https://mcp.example.com"`, `"https://evil.example.com"`, "mcpServers.s.headers.X-Key"},
		{"sibling", `"BASE": "https://a.example.com"`, `"BASE": "https://evil.example.com"`, "mcpServers.t.env.K"},
		{"command", `"command": "srv"`, `"command": "evil"`, "mcpServers.t.env.K"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			given, _, _ := Redact("json", []byte(store))
			vm := strings.Replace(string(given), tc.from, tc.to, 1)
			if vm == string(given) {
				t.Fatalf("%q isn't in the copy:\n%s", tc.from, given)
			}
			_, err := Restore("json", []byte(vm), given, []byte(store))
			if pe, ok := errors.AsType[*PlaceholderError](err); !ok || pe.Path != tc.path {
				t.Errorf("Restore = %v, want a placeholder error at %s", err, tc.path)
			}
		})
	}
	// Elsewhere at the top: fine.
	if out, err := restored(t, "json", strings.Replace(mustRedact(t, store), `"dark"`, `"light"`, 1), store); err != nil || !strings.Contains(out, "testfake-header") {
		t.Errorf("Restore = %q, %v", out, err)
	}
	// No store file, or one that doesn't parse: nothing to put back.
	vm := mustRedact(t, store)
	for _, s := range []string{"", "{"} {
		if _, err := Restore("json", []byte(vm), []byte(vm), []byte(s)); err == nil {
			t.Errorf("Restore with the store's file %q: no error", s)
		}
	}
	if _, err := Restore("json", []byte(vm), []byte("{"), []byte(store)); err == nil {
		t.Error("Restore with a copy that doesn't parse: no error")
	}
}

func mustRedact(t *testing.T, in string) string {
	t.Helper()
	out, _, err := Redact("json", []byte(in))
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// A ~/.claude.json's subset is ClaudeCopy's: the store's keys only, never its projects or API key;
// anything but a JSON object is refused. ClaudeTopKeys lists those keys, sorted.
func TestClaudeSubset(t *testing.T) {
	out, err := ClaudeSubset([]byte(`{"theme": "dark", "primaryApiKey": "testfake-key", "projects": {"/p": {}}, "mcpServers": {}}`))
	if err != nil {
		t.Fatal(err)
	}
	if s := string(out); !strings.Contains(s, `"theme": "dark"`) || strings.Contains(s, "testfake-key") || strings.Contains(s, "projects") {
		t.Errorf("subset:\n%s", s)
	}
	for _, in := range []string{"[]", "{", `"x"`} {
		if _, err := ClaudeSubset([]byte(in)); err == nil {
			t.Errorf("ClaudeSubset(%q): no error", in)
		}
	}
	keys := ClaudeTopKeys()
	if !slices.IsSorted(keys) || !slices.Contains(keys, "mcpServers") || slices.Contains(keys, "projects") || len(keys) != len(claudeTopKeys) {
		t.Errorf("ClaudeTopKeys = %v", keys)
	}
}

// HasPlaceholder finds devvm-redacted and the proxy's placeholders, salted or not.
func TestHasPlaceholder(t *testing.T) {
	for in, want := range map[string]bool{
		"devvm-redacted": true, "sk-ant-oat01-devvm-proxy-managed-abc": true, "devvm-proxy-managed-github-x": true,
		fakeJWT(map[string]any{"exp": 1}): true,
		"devvm":                           false, "redacted": false, "": false,
	} {
		if got := HasPlaceholder([]byte(in)); got != want {
			t.Errorf("HasPlaceholder(%q) = %v", in, got)
		}
	}
}
