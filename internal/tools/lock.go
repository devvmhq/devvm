package tools

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/state"
)

// Lock is a mise lock file, read as TOML: an entry per tool version,
// [[tools.TOOL]], each with a table per platform, "platforms.PLATFORM",
// holding its download's URL and checksum, or install = "source" for a
// build from source. Only what it says counts, not how mise writes it: its
// order, spacing, quoting and comments. The zero Lock has no tools.
type Lock struct {
	raw []byte
	doc map[string]any
}

// ParseLock reads a mise lock file.
func ParseLock(data []byte) (Lock, error) {
	var doc map[string]any
	if err := toml.Unmarshal(data, &doc); err != nil {
		return Lock{}, err
	}
	return Lock{raw: data, doc: doc}, nil
}

// parseLockFile reads and parses the lock file mise wrote at path, in a
// temporary folder of devvm's.
func parseLockFile(path string) (Lock, error) {
	data, err := os.ReadFile(path) //nolint:gosec // devvm's own temporary folder
	if err != nil {
		return Lock{}, err
	}
	return ParseLock(data)
}

// readLock reads the lock file at path: a CorruptError when it isn't TOML.
func readLock(path string, data []byte) (Lock, error) {
	l, err := ParseLock(data)
	if err != nil {
		c := &state.CorruptError{Path: path, Reason: err.Error()}
		if pe, ok := errors.AsType[toml.ParseError](err); ok {
			c.Line, c.Reason = pe.Position.Line, pe.Message
		}
		return Lock{}, c
	}
	return l, nil
}

// Bytes is the lock file as written: mise's own, or devvm's after a step
// back.
func (l Lock) Bytes() []byte { return l.raw }

// None reports whether it's the zero Lock: there's no lock file yet.
func (l Lock) None() bool { return l.raw == nil && l.doc == nil }

// lockEntry is one of a tool's entries, its fields as TOML gives them.
type lockEntry = map[string]any

// entries returns a tool's entries, in the lock's order.
func (l Lock) entries(tool string) []lockEntry {
	ts, _ := l.doc["tools"].(map[string]any)
	es, _ := ts[tool].([]map[string]any)
	return es
}

// first returns a tool's first entry, the one its version installs: nil
// when the lock has none.
func (l Lock) first(tool string) lockEntry {
	if es := l.entries(tool); len(es) > 0 {
		return es[0]
	}
	return nil
}

func str(e lockEntry, key string) string {
	s, _ := e[key].(string)
	return s
}

// Has reports whether the lock has an entry for a tool.
func (l Lock) Has(tool string) bool { return l.first(tool) != nil }

// Version returns a tool's locked version: its first entry's.
func (l Lock) Version(tool string) string { return str(l.first(tool), "version") }

// Backend returns the mise backend of a tool's first entry: core:node,
// aqua:anthropics/claude-code.
func (l Lock) Backend(tool string) string { return str(l.first(tool), "backend") }

// Entry returns a tool's first entry, the one its version installs, in a
// canonical form for its image's key: its fields as JSON, keys sorted. Nil
// when the lock has none.
func (l Lock) Entry(tool string) ([]byte, error) {
	e := l.first(tool)
	if e == nil {
		return nil, nil
	}
	return json.Marshal(e)
}

// HasDownload reports whether a tool's first entry has a download (a URL
// and its checksum, which mise or build.sh checks it against, not a build
// from source) for every one of the platforms. A tool whose installer
// fetches it needs none (image.NoDownload): rustup installs rust.
func (l Lock) HasDownload(tool string, platforms []string) bool {
	if image.NoDownload(tool) {
		return true
	}
	return hasDownload(l.first(tool), platforms)
}

func hasDownload(e lockEntry, platforms []string) bool {
	if e == nil {
		return false
	}
	for _, p := range platforms {
		table, _ := e["platforms."+p].(map[string]any)
		if str(table, "url") == "" || str(table, "checksum") == "" || str(table, "install") == "source" {
			return false
		}
	}
	return true
}

// versionEntry returns the entry for one of a tool's versions, nil when
// the lock has none (a lock can hold several).
func (l Lock) versionEntry(tool, version string) lockEntry {
	for _, e := range l.entries(tool) {
		if str(e, "version") == version {
			return e
		}
	}
	return nil
}

// Download returns the URL and checksum of a tool's first entry's
// download for a platform: "" for none.
func (l Lock) Download(tool, platform string) (url, checksum string) {
	table, _ := l.first(tool)["platforms."+platform].(map[string]any)
	return str(table, "url"), str(table, "checksum")
}

// withEntry returns the lock with a tool's entries replaced by e alone,
// written again as TOML.
func (l Lock) withEntry(tool string, e lockEntry) (Lock, error) {
	return l.withEntries(tool, []lockEntry{e})
}

// withEntries returns the lock with a tool's entries replaced by es,
// written again as TOML.
func (l Lock) withEntries(tool string, es []lockEntry) (Lock, error) {
	doc, ts := map[string]any{}, map[string]any{}
	maps.Copy(doc, l.doc)
	if old, ok := l.doc["tools"].(map[string]any); ok {
		maps.Copy(ts, old)
	}
	ts[tool] = es
	doc["tools"] = ts
	var b bytes.Buffer
	if err := toml.NewEncoder(&b).Encode(doc); err != nil {
		return Lock{}, err
	}
	return Lock{raw: b.Bytes(), doc: doc}, nil
}

func lines(data []byte) []string {
	var out []string
	s := bufio.NewScanner(bytes.NewReader(data))
	s.Buffer(nil, 1<<20)
	for s.Scan() {
		out = append(out, s.Text())
	}
	return out
}

// configTools reads the [tools] table of a mise.toml: each tool and its
// version as given ("24", "latest"), in order.
func configTools(data []byte) (names []string, versions map[string]string) {
	versions = map[string]string{}
	in := false
	for _, line := range lines(data) {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "["):
			in = t == "[tools]"
			continue
		case !in || t == "" || strings.HasPrefix(t, "#"):
			continue
		}
		k, v, ok := strings.Cut(t, "=")
		if !ok {
			continue
		}
		if i := strings.Index(v, "#"); i >= 0 {
			v = v[:i]
		}
		k = strings.TrimSpace(k)
		names = append(names, k)
		versions[k] = strings.Trim(strings.TrimSpace(v), `"`)
	}
	return names, versions
}

// configAliases reads the [tool_alias] table of a mise.toml: each tool
// whose backend it names, and that backend (aqua:openai/codex).
func configAliases(data []byte) (map[string]string, error) {
	var config struct {
		Aliases map[string]string `toml:"tool_alias"`
	}
	if err := toml.Unmarshal(data, &config); err != nil {
		return nil, err
	}
	return config.Aliases, nil
}

// aliasTable is a [tool_alias] table as a mise.toml takes it: a line per
// tool, naming its backend, in the tools' order.
func aliasTable(aliases map[string]string) []byte {
	var b bytes.Buffer
	b.WriteString("[tool_alias]\n")
	for _, tool := range slices.Sorted(maps.Keys(aliases)) {
		b.WriteString(tool + " = " + strconv.Quote(aliases[tool]) + "\n")
	}
	return b.Bytes()
}
