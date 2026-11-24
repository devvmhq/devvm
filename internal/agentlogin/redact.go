package agentlogin

// Agents' settings files copied into VMs (Codex's config.toml, opencode's
// opencode.json, Claude Code's settings.json) can hold keys: an MCP server's headers or env, a
// provider's apiKey. The proxy can't put those in (it only knows the logins), so the VM's copy gets
// "devvm-redacted" for every value whose key looks like a secret's, every value of an env or headers
// table, and the secrets in arguments, URLs and free text (scrubArgs, scrubURL, scrubText), as
// ~/.claude.json's MCP servers get. Each file is parsed whole, and one that doesn't parse is refused: no line reaches the VM
// unread.
//
// What's kept although its key matches: values that point elsewhere rather than hold a secret (an
// env var's name, {env:X} or ${X}, a path), and non-strings.

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"

	"github.com/devvmhq/devvm/internal/agents"
)

var (
	// The key names: *key*, *token*, *secret*, *password*, Authorization (any case).
	redactName = regexp.MustCompile(`(?i)(key|token|secret|passw|^authorization$|^proxy-authorization$|^cookie$)`)
	envName    = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	envRef     = regexp.MustCompile(`^(\{(env|file):[^}]*\}|\$\{?[A-Za-z_][A-Za-z0-9_]*\}?)$`)
	// A switch or a port, kept in env tables: true, false or a number of at most five digits.
	plainValue = regexp.MustCompile(`^(?i:true|false|[0-9]{1,5})$`)
	// A setting's word, such as off, name-only or allow: lower-case letters and dashes alone.
	settingWord = regexp.MustCompile(`^[a-z]+(-[a-z]+)*$`)
	// An env variable whose name says it holds a credential, or headers, wherever it is: its
	// value goes whole, a URL too. TOKEN only as a word, as MAX_OUTPUT_TOKENS counts model tokens.
	credName = regexp.MustCompile(`(?i)(key|token($|_)|secret|pass|auth|credential|cookie|header)`)
	// Claude Code's own configuration variables (code.claude.com/docs/en/env-vars), which its
	// settings.json's env keeps unless credName matches: the model's, CLAUDE_CODE_*, DISABLE_*,
	// MAX_* and *_TIMEOUT_MS.
	claudeEnv = regexp.MustCompile(`^(ANTHROPIC_(MODEL|SMALL_FAST_MODEL|DEFAULT_[A-Z0-9]+_MODEL|CUSTOM_MODEL_OPTION)(_[A-Z0-9_]+)?|` +
		`CLAUDE_CODE_[A-Z0-9_]+|DISABLE_[A-Z0-9_]+|MAX_[A-Z0-9_]+|[A-Z0-9_]+_TIMEOUT_MS)$`)
)

// agentSetting: the value at path, under key k, is one of Claude Code's configuration variables in
// its settings' top-level env (claudeEnv), and its name doesn't say it's a credential (credName).
func agentSetting(path, k string) bool {
	return path == "env."+k && claudeEnv.MatchString(k) && !credName.MatchString(k)
}

// RedactMark is what the VM's copy holds in place of a secret.
const RedactMark = "devvm-redacted"

// redactKeep: a value that names where a secret is rather than holding it.
func redactKeep(v string) bool {
	switch {
	case v == "" || v == RedactMark:
		return true
	case envName.MatchString(v): // an env var's name (Codex's env_key)
		return true
	case envRef.MatchString(v):
		return true
	case strings.HasPrefix(v, "/") || strings.HasPrefix(v, "~/") || strings.HasPrefix(v, "./"):
		return true // a path: a helper command or a key file the VM doesn't have
	}
	return false
}

// wholeTable: a table, at path under key k, every string value of which is a secret's, such as an
// MCP server's env or headers.
func wholeTable(path, k string) bool {
	switch strings.ToLower(k) {
	case "env", "environment", "headers", "http_headers":
		return true
	case "set": // Codex's [shell_environment_policy] set = { VAR = "value" }
		return path == "shell_environment_policy.set" || strings.HasSuffix(path, ".shell_environment_policy.set")
	}
	return false
}

// Redact returns an agent's settings file, format "json" (JSONC too, written back as JSON) or
// "toml" (written back without its comments), with its secrets replaced by "devvm-redacted", and
// the replaced values' paths, sorted. A file that doesn't parse is an error: it's refused rather
// than copied unread.
func Redact(format string, data []byte) ([]byte, []string, error) {
	v, err := decode(format, data)
	if err != nil {
		return nil, nil, err
	}
	var r redaction
	r.names, r.values = agents.NameTables()
	out, err := encode(format, r.value(v, "", nil, "", false))
	if err != nil {
		return nil, nil, err
	}
	sort.Strings(r.paths)
	return out, r.paths, nil
}

// redaction is one file's: the paths of the values it replaced, and the tables whose keys are
// names you choose (agents.NameTables).
type redaction struct {
	paths         []string
	names, values [][]string
}

// named reports how the keys of the table at keys, its parents' keys from the file's top, are
// judged: as names when what's under them is a table (tables), such as an MCP server's, or a
// table or a setting's word (values), such as a skill's off (settingWord), while any other value is still judged by its key. A name doesn't make what's
// under it a secret, as a secret-looking key does, though the fields of a table under it are
// judged as anywhere else, and a name that's a table of secrets' key, such as env, still makes it
// one (wholeTable): a server named env loses its settings rather than a real env keeping its
// secrets. A * stands for any key but a list's.
func (r *redaction) named(keys []string) (tables, values bool) {
	at := func(p []string) bool {
		if len(p) != len(keys) {
			return false
		}
		for i, s := range p {
			if (s != "*" || keys[i] == "[]") && s != keys[i] {
				return false
			}
		}
		return true
	}
	return slices.ContainsFunc(r.names, at), slices.ContainsFunc(r.values, at)
}

func joinPath(path, k string) string {
	if path == "" {
		return k
	}
	return path + "." + k
}

// value returns v, at path (key k of its parent, and keys from the file's top), with its
// secrets taken out. secret: every string in v is one, as v is under a
// secret-looking key or in a table of secrets (secretIn).
func (r *redaction) value(v any, path string, keys []string, k string, secret bool) any {
	switch t := v.(type) {
	case string:
		return r.str(t, path, k, secret)
	case map[string]any:
		tables, values := r.named(keys)
		for ck, x := range t {
			_, table := x.(map[string]any)
			in := secretIn(path, k, ck, x)
			if word, _ := x.(string); (tables || values) && table || values && settingWord.MatchString(word) {
				in = !table && wholeTable(path, k) // a name you chose (named)
			}
			t[ck] = r.value(x, joinPath(path, ck), append(slices.Clip(keys), ck), ck, secret || in)
		}
	case []map[string]any: // TOML's [[tables]]
		for i, x := range t {
			r.value(x, fmt.Sprintf("%s[%d]", path, i), append(slices.Clip(keys), "[]"), k, secret)
		}
	case []any:
		return r.list(t, path, keys, k, secret)
	}
	return v
}

// secretIn: x, under key ck of the table at path (its parent's key k), is a secret, or a table or a
// list of them. A table is when it's under a secret-looking key, but key bindings (opencode's
// keybinds), and every value of a table of secrets (wholeTable) is one.
func secretIn(path, k, ck string, x any) bool {
	if _, ok := x.(map[string]any); ok {
		name := strings.ToLower(ck)
		return redactName.MatchString(ck) && !strings.Contains(name, "keybind") && !strings.Contains(name, "keymap")
	}
	return redactName.MatchString(ck) || wholeTable(path, k)
}

// str returns s, at path under key k, as the VM's copy holds it. A URL in a table of secrets, such
// as an env's base URL, is only scrubbed; under a secret-looking key it goes whole. Claude Code's
// configuration variables stay (agentSetting). In free text, such as a hook's command, only the
// keys and credentials go (scrubText).
func (r *redaction) str(s, path, k string, secret bool) string {
	url := strings.Contains(s, "://")
	named := redactName.MatchString(k) || credName.MatchString(k)
	keep := redactKeep(s) || plainValue.MatchString(s) || (url && !named) || agentSetting(path, k)
	if (secret && !keep) || secretShape.MatchString(s) { // a known key's shape, wherever it is
		r.paths = append(r.paths, path)
		return RedactMark
	}
	out := s
	if url && !strings.ContainsFunc(s, unicode.IsSpace) { // else text holding URLs (scrubText)
		out = scrubURL(out)
	}
	if out = scrubText(out); out != s {
		r.paths = append(r.paths, path)
	}
	return out
}

// list returns l, at path: every string in it replaced when they're secrets, else as arguments
// are (scrubArgs); the tables and lists in it each checked.
func (r *redaction) list(l []any, path string, keys []string, k string, secret bool) []any {
	changed := false
	for i, x := range l {
		s, ok := x.(string)
		if !ok || !secret {
			continue
		}
		if !redactKeep(s) {
			l[i], changed = RedactMark, true
		} else if t := scrubText(s); t != s {
			l[i], changed = t, true
		}
	}
	if !secret {
		if out, ok := scrubList(l); ok {
			l, changed = out, true
		}
	}
	if changed {
		r.paths = append(r.paths, path)
	}
	for i, x := range l {
		if _, ok := x.(string); !ok {
			l[i] = r.value(x, fmt.Sprintf("%s[%d]", path, i), append(slices.Clip(keys), "[]"), k, secret)
		}
	}
	return l
}
