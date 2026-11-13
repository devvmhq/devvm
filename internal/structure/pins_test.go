package structure

import (
	"io/fs"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// pinPattern is where TestBumpCoversPins looks for one kind of pin: a
// file (a glob, or a folder for every file below it, ending in /), and
// in it only a TOML section when one is named, a pattern whose first
// group names a pin, and the key's prefix it gets, before the name. A
// key is "FILE:NAME", or "FILE:KIND:NAME" for the files with more than
// one kind of pin.
type pinPattern struct {
	glob, section, prefix string
	re                    *regexp.Regexp
}

// pinPatterns are the pins TestBumpCoversPins finds.
func pinPatterns() []pinPattern {
	type p = pinPattern
	version := `v?[0-9]+\.[0-9]+`
	return []p{
		// A pinned download's <tool>_version, _commit and _sha256* lines,
		// and a version in a mise.toml's inline table (cursor's).
		{"assets/", "", "", regexp.MustCompile(`(?m)^([a-z][a-z0-9_]*?)_(?:version|commit|sha256(?:_[a-z0-9]+)*)=`)},
		{"assets/", "", "", regexp.MustCompile(`(?m)^"?([A-Za-z0-9_.:/-]+)"? = \{ ?version = "[^"]*[0-9]+\.[0-9]+`)},
		{"mise.toml", "tools", "tool:", regexp.MustCompile(`(?m)^"?([A-Za-z0-9_.:/-]+)"? = `)},
		{"mise.toml", "", "run:", regexp.MustCompile(`([A-Za-z0-9_./-]+)@` + version)},
		{"go.mod", "", "", regexp.MustCompile(`(?m)^(go|toolchain) `)},
		{"go.mod", "", "require:", regexp.MustCompile(`(?m)^(?:require )?\t?([a-z0-9]\S*) v[0-9]+\.\S+$`)},
		{"go.mod", "", "indirect:", regexp.MustCompile(`(?m)^(?:require )?\t?([a-z0-9]\S*) v[0-9]+\.\S+ // indirect$`)},
		{"docs/go.mod", "", "", regexp.MustCompile(`(?m)^(go|toolchain) `)},
		{"docs/go.mod", "", "require:", regexp.MustCompile(`(?m)^(?:require )?\t?([a-z0-9]\S*) v[0-9]+\.\S+$`)},
		{"docs/go.mod", "", "indirect:", regexp.MustCompile(`(?m)^(?:require )?\t?([a-z0-9]\S*) v[0-9]+\.\S+ // indirect$`)},
		// Every workflow's uses:, and every value that is a version or a
		// SHA-256, by its key.
		{".github/workflows/*.yml", "", "uses:", regexp.MustCompile(`uses: ([^.\s@][^\s@]*)@`)},
		{".github/workflows/*.yml", "", "", regexp.MustCompile(`(?m)^\s*([A-Za-z_][A-Za-z0-9_-]*): ["']?` + version + `[0-9A-Za-z.+-]*["']?\s*$`)},
		{".github/workflows/*.yml", "", "", regexp.MustCompile(`(?m)^\s*([A-Za-z_][A-Za-z0-9_-]*): [0-9a-f]{64}\s*$`)},
		{"tests/e2e-mise.lock", "", "", regexp.MustCompile(`(?m)^\[\[tools\.("?[^\]"]+"?)\]\]$`)},
		// tests/e2e's pinned consts: an image digest, a dated release or a
		// version.
		{"tests/e2e/*.go", "", "", regexp.MustCompile(`(?m)^const (\w+) = ".*(?:sha256:[0-9a-f]{64}|release-[0-9]{8}|[0-9]+\.[0-9]+\.[0-9]+).*"$`)},
		{"docs/hugo.yaml", "", "", regexp.MustCompile(`([A-Za-z0-9_.-]+)@` + version)},
		{"docs/hugo.yaml", "", "", regexp.MustCompile(`(?m)^\s*([A-Za-z_]+): ["']?` + version + `\.[0-9]+["']?\s*$`)},
	}
}

// pinFiles is the files p reads, by their names in the repo.
func pinFiles(t *testing.T, fsys fs.FS, p pinPattern) []string {
	t.Helper()
	if dir, ok := strings.CutSuffix(p.glob, "/"); ok {
		var files []string
		err := fs.WalkDir(fsys, dir, func(name string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				files = append(files, name)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return files
	}
	files, err := fs.Glob(fsys, p.glob)
	if err != nil || len(files) == 0 {
		t.Fatalf("%s: no files (%v)", p.glob, err)
	}
	return files
}

// tomlSection is the lines of data's [name] section, or all of data
// when name is "".
func tomlSection(data, name string) string {
	if name == "" {
		return data
	}
	_, s, ok := strings.Cut(data, "\n["+name+"]\n")
	if !ok {
		return ""
	}
	s, _, _ = strings.Cut(s, "\n[")
	return s
}

// foundPins is every pin pinPatterns finds, each key once. The
// workflows' keys are all .github/workflows's, tests/e2e's files' all
// tests/e2e's.
func foundPins(t *testing.T) []string {
	t.Helper()
	fsys := os.DirFS(root)
	var keys []string
	for _, p := range pinPatterns() {
		for _, f := range pinFiles(t, fsys, p) {
			data, err := fs.ReadFile(fsys, f)
			if err != nil {
				t.Fatal(err)
			}
			file := f
			if d := path.Dir(f); d == ".github/workflows" || d == "tests/e2e" {
				file = d
			}
			for _, m := range p.re.FindAllStringSubmatch(tomlSection(string(data), p.section), -1) {
				keys = append(keys, file+":"+p.prefix+strings.Trim(m[1], `"`))
			}
		}
	}
	slices.Sort(keys)
	return slices.Compact(keys)
}

// TestBumpCoversPins checks that scripts/bump.sh moves every pin in the
// files pinPatterns reads, or says why not: each pin found is a "# pin
// KEY TOOL" line of its header (a KEY ending in * covers every key it
// starts), whose TOOL is one bump.sh has, or "-" and a reason. A line
// that covers nothing fails too, so the list stays the pins there are.
// It reads only the repo: it checks coverage, not freshness, which
// bump.sh --check does.
func TestBumpCoversPins(t *testing.T) {
	data, err := os.ReadFile(path.Join(root, "scripts/bump.sh"))
	if err != nil {
		t.Fatal(err)
	}
	bump := string(data)
	every := regexp.MustCompile(`(?m)^every="([^"]*)"$`).FindStringSubmatch(bump)
	if every == nil {
		t.Fatal(`scripts/bump.sh has no every="TOOL..." line`)
	}
	tools := strings.Fields(every[1])
	for _, tool := range tools {
		if !strings.Contains(bump, "\nbump_"+tool+"() {") {
			t.Errorf("scripts/bump.sh lists %s but has no bump_%s", tool, tool)
		}
	}
	entries := regexp.MustCompile(`(?m)^# pin (\S+) (\S+)(?: (.*))?$`).FindAllStringSubmatch(bump, -1)
	if len(entries) == 0 {
		t.Fatal("scripts/bump.sh's header has no # pin lines")
	}
	used := make(map[string]bool)
	for _, e := range entries {
		switch key, tool, why := e[1], e[2], e[3]; {
		case tool == "-" && strings.TrimSpace(why) == "":
			t.Errorf("scripts/bump.sh: # pin %s is left without a reason", key)
		case tool != "-" && !slices.Contains(tools, tool):
			t.Errorf("scripts/bump.sh: # pin %s names %s, which isn't one of its tools", key, tool)
		}
	}
	for _, key := range foundPins(t) {
		// The exact key, else the longest * that starts it.
		match := ""
		for _, e := range entries {
			k := e[1]
			if k == key {
				match = k
				break
			}
			if p, ok := strings.CutSuffix(k, "*"); ok && strings.HasPrefix(key, p) && len(k) > len(match) {
				match = k
			}
		}
		if match == "" {
			t.Errorf("%s is a pin that scripts/bump.sh doesn't cover: give it a tool, or a # pin line with - and why it's left", key)
			continue
		}
		used[match] = true
	}
	for _, e := range entries {
		if !used[e[1]] {
			t.Errorf("scripts/bump.sh: # pin %s matches no pin: remove it, or fix its key", e[1])
		}
	}
}
