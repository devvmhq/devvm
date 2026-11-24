package agentlogin

// A VM's settings files saved back into devvm's store (devvm agent save) come from the VM, so
// they're untrusted, and with network policy they hold the VM's copies' placeholders where the
// store has secrets. Restore puts the store's values back in their place, but only where the VM's
// copy had that placeholder and nothing around it changed, so a VM can't send a secret it never
// saw somewhere else, such as an MCP server's header to another URL.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strconv"

	"github.com/BurntSushi/toml"

	"github.com/devvmhq/devvm/internal/jsonc"
)

// managedMark is in every placeholder the proxy fills in (Placeholder), after any prefix, such as
// an MCP server's header's in a VM's ~/.claude.json and Claude Code's and GitHub's tokens in its
// environment.
const managedMark = "devvm-proxy-managed"

// HasPlaceholder reports whether data holds a placeholder of a VM's copies: devvm-redacted, one
// the proxy fills in, or a JWT-shaped one (a captured or copied login's tokens, fakeJWT).
func HasPlaceholder(data []byte) bool {
	for _, m := range []string{RedactMark, managedMark, jwtMark} {
		if bytes.Contains(data, []byte(m)) {
			return true
		}
	}
	return false
}

// PlaceholderError is Restore's error for a placeholder it can't put the store's value back for:
// at Path, the VM's copy had no such placeholder, the store has no value there, or the settings
// around it (its table and the one above it) changed.
type PlaceholderError struct{ Path string }

func (e *PlaceholderError) Error() string {
	return "a placeholder at " + e.Path + " devvm can't put back"
}

// ClaudeTopKeys are the keys of ~/.claude.json that devvm's store keeps (claudeTopKeys), sorted.
func ClaudeTopKeys() []string {
	keys := make([]string, 0, len(claudeTopKeys))
	for k := range claudeTopKeys {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// ClaudeSubset returns what devvm's store keeps of a ~/.claude.json, data: only claudeTopKeys
// (ClaudeCopy's), so never its per-project entries or an API key. data that isn't a JSON object
// is an error.
func ClaudeSubset(data []byte) ([]byte, error) {
	v, err := decode("json", data)
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("not a JSON object")
	}
	return encode("json", claudeTop(m))
}

// Restore returns vm, an agent's settings file as a VM has it, in format (Redact's), checked:
// one that doesn't parse whole is an error. Without placeholders it's vm as it is. With them, each
// is the value the store's file, store, has there, as long as the VM's copy of it, given, held the
// same placeholder there and the tables around it, its own and the one above it but the file's
// top, are the same as given's; else it's a *PlaceholderError. A file with placeholders is
// written back as Redact writes it.
func Restore(format string, vm, given, store []byte) ([]byte, error) {
	v, err := decode(format, vm)
	if err != nil {
		return nil, err
	}
	if !HasPlaceholder(vm) {
		return vm, nil
	}
	g, err := decode(format, given)
	if err != nil {
		return nil, &PlaceholderError{Path: "."}
	}
	s, err := decode(format, store)
	if err != nil {
		return nil, &PlaceholderError{Path: "."}
	}
	out, err := restore(v, g, s, "", nil)
	if err != nil {
		return nil, err
	}
	return encode(format, out)
}

// pair is a table, or a list, of the VM's file and given's at the same path.
type pair struct{ v, g any }

// restore returns v, at path, with its placeholders' values from s (Restore), g being given's
// value at path; above are the tables and lists v is in, the file's top first.
func restore(v, g, s any, path string, above []pair) (any, error) {
	switch t := v.(type) {
	case string:
		if !HasPlaceholder([]byte(t)) {
			return t, nil
		}
		gs, ok := g.(string)
		if !ok || gs != t || s == nil {
			return nil, &PlaceholderError{Path: path}
		}
		// A file that is one string has no table around it: nothing to compare.
		for _, p := range above[min(len(above), max(1, len(above)-2)):] {
			if !reflect.DeepEqual(p.v, p.g) {
				return nil, &PlaceholderError{Path: path}
			}
		}
		return s, nil
	case map[string]any:
		gm, _ := g.(map[string]any)
		sm, _ := s.(map[string]any)
		in := append(slices.Clip(above), pair{t, g})
		out := make(map[string]any, len(t))
		for k, x := range t {
			r, err := restore(x, gm[k], sm[k], joinPath(path, k), in)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	case []map[string]any: // TOML's [[tables]]
		l := make([]any, len(t))
		for i, x := range t {
			l[i] = x
		}
		return restore(l, asList(g), asList(s), path, above)
	case []any:
		gl, sl := asList(g), asList(s)
		in := append(slices.Clip(above), pair{t, g})
		out := make([]any, len(t))
		for i, x := range t {
			var gi, si any
			if i < len(gl) {
				gi = gl[i]
			}
			if i < len(sl) {
				si = sl[i]
			}
			r, err := restore(x, gi, si, path+"["+strconv.Itoa(i)+"]", in)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	}
	return v, nil
}

// asList is a list, or TOML's [[tables]], as a list; nil for anything else.
func asList(v any) []any {
	switch t := v.(type) {
	case []any:
		return t
	case []map[string]any:
		l := make([]any, len(t))
		for i, x := range t {
			l[i] = x
		}
		return l
	}
	return nil
}

// decode parses a settings file whole, in format (Redact's): JSON (JSONC too, its numbers as
// written) or TOML.
func decode(format string, data []byte) (any, error) {
	switch format {
	case "json":
		d := json.NewDecoder(bytes.NewReader(jsonc.Strip(data)))
		d.UseNumber()
		var v any
		if err := d.Decode(&v); err != nil {
			return nil, fmt.Errorf("not JSON: %w", err)
		}
		if _, err := d.Token(); !errors.Is(err, io.EOF) {
			return nil, errors.New("not JSON: more after its value")
		}
		return v, nil
	case "toml":
		var m map[string]any
		if _, err := toml.Decode(string(data), &m); err != nil {
			return nil, fmt.Errorf("not TOML: %w", err)
		}
		return m, nil
	}
	return nil, fmt.Errorf("unknown format %q", format)
}

// encode writes v as Redact writes a file: JSON indented by two spaces, or TOML.
func encode(format string, v any) ([]byte, error) {
	var b bytes.Buffer
	switch format {
	case "json":
		e := json.NewEncoder(&b)
		e.SetEscapeHTML(false)
		e.SetIndent("", "  ")
		if err := e.Encode(v); err != nil {
			return nil, err
		}
	case "toml":
		if err := toml.NewEncoder(&b).Encode(v); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown format %q", format)
	}
	return b.Bytes(), nil
}
