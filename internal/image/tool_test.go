package image

import (
	"errors"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/devvmhq/devvm/assets"
	"github.com/devvmhq/devvm/internal/agents"
	"github.com/devvmhq/devvm/internal/msg"
)

const nodeEntry = `[[tools.node]]
version = "24.21.0"
backend = "core:node"

[tools.node."platforms.linux-arm64"]
checksum = "sha256:aaaa"
url = "https://nodejs.org/dist/v24.21.0/node-v24.21.0-linux-arm64.tar.xz"
`

func node() Tool {
	return Tool{Name: "node", Version: "24.21.0", LockEntry: []byte(nodeEntry), Libc: "gnu"}
}

const miseConfig = `# The shared tools.
[tools]
node = "24"
python = "latest"

[settings]
experimental = true
python.compile = false
`

func toolRoot(script string) fstest.MapFS {
	return withConfig(script, miseConfig)
}

const baseScript = "#!/bin/bash\nmise_version=2026.1.0\n"

func withConfig(script, config string) fstest.MapFS {
	return fstest.MapFS{
		"shared-tools/build.sh": {Data: []byte(script)}, "shared-tools/mise.toml": {Data: []byte(config)},
		"layers/base.sh": {Data: []byte(baseScript)}, "prelude.sh": {Data: []byte("# prelude\n")},
	}
}

// withPrelude is root with this prelude.sh.
func withPrelude(root fstest.MapFS, prelude string) fstest.MapFS {
	out := maps.Clone(root)
	out["prelude.sh"] = &fstest.MapFile{Data: []byte(prelude)}
	return out
}

// withBase is root with this layers/base.sh.
func withBase(root fstest.MapFS, base string) fstest.MapFS {
	out := maps.Clone(root)
	out["layers/base.sh"] = &fstest.MapFile{Data: []byte(base)}
	return out
}

func toolFile(t *testing.T, root fstest.MapFS, tool Tool) string {
	t.Helper()
	f, err := ToolFile(root, tool)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestToolFile(t *testing.T) {
	root := toolRoot("#!/bin/bash\n")
	f := toolFile(t, root, node())
	if !regexp.MustCompile(`^node-24\.21\.0-[0-9a-f]{12}\.erofs$`).MatchString(f) {
		t.Fatalf("got %q", f)
	}
	if again := toolFile(t, toolRoot("#!/bin/bash\n"), node()); again != f {
		t.Errorf("not stable: %q, then %q", f, again)
	}
	tests := []struct {
		name   string
		root   fstest.MapFS
		change func(*Tool)
	}{
		{"the lock entry", root, func(t *Tool) { t.LockEntry = append(t.LockEntry, "# edit\n"...) }},
		{"shared-tools/build.sh", toolRoot("#!/bin/bash\n# edit\n"), func(*Tool) {}},
		{"the prelude", withPrelude(root, "# prelude\n# edit\n"), func(*Tool) {}},
		{"the libc (an Alpine base)", root, func(t *Tool) { t.Libc, t.Base = "musl", "alpine@sha256:aaaa" }},
		{"the version", root, func(t *Tool) { t.Version = "24.22.0" }},
		{"the format (plain)", root, func(t *Tool) { t.Format = "plain-0123456789ab" }},
		{"a setting in mise.toml", withConfig("#!/bin/bash\n", strings.Replace(miseConfig, "compile = false", "compile = true", 1)), func(*Tool) {}},
		{"a setting added to mise.toml", withConfig("#!/bin/bash\n", miseConfig+"node.gpg_verify = false\n"), func(*Tool) {}},
		{"the mise base.sh pins", withBase(root, "mise_version=2026.2.0\n"), func(*Tool) {}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool := node()
			tt.change(&tool)
			if got := toolFile(t, tt.root, tool); got == f {
				t.Errorf("still %q", got)
			}
		})
	}
	// What the lock entry already holds (the tools and their holds), the
	// comments and how the settings are written don't change it.
	same := map[string]string{
		"a tool's hold": strings.Replace(miseConfig, `node = "24"`, `node = "26"`, 1),
		"a comment":     "# edited\n" + miseConfig,
		"the settings written another way": strings.Replace(miseConfig, "experimental = true\npython.compile = false",
			"python = { compile = false }\nexperimental = true", 1),
	}
	for name, config := range same {
		if got := toolFile(t, withConfig("#!/bin/bash\n", config), node()); got != f {
			t.Errorf("%s: %q, want %q", name, got, f)
		}
	}
	// A musl image is keyed by the digest of the base it builds in, so
	// one built in a base from another image is never another's.
	musl := node()
	musl.Libc, musl.Base = "musl", "alpine@sha256:aaaa"
	m := toolFile(t, root, musl)
	musl.Base = "alpine@sha256:bbbb"
	if got := toolFile(t, root, musl); got == m {
		t.Errorf("musl on another base: still %q", got)
	}
	if got := toolFile(t, withBase(root, baseScript+"# edit\nnode_version=1\n"), node()); got != f {
		t.Errorf("the rest of base.sh: %q, want %q", got, f)
	}
	// rustup installs rust, and build.sh dotnet: mise's version doesn't
	// count, and base.sh's pin isn't read.
	noBase := maps.Clone(root)
	delete(noBase, "layers/base.sh")
	for _, tool := range []Tool{
		{Name: "rust", Version: "1.99.1", LockEntry: []byte("[[tools.rust]]\n"), Libc: "gnu"},
		{Name: "dotnet", Version: "10.0.403", LockEntry: []byte("[[tools.dotnet]]\n"), Libc: "gnu"},
	} {
		r := toolFile(t, root, tool)
		if got := toolFile(t, withBase(root, "mise_version=2026.2.0\n"), tool); got != r {
			t.Errorf("%s with another mise: %q, want %q", tool.Name, got, r)
		}
		if got := toolFile(t, noBase, tool); got != r {
			t.Errorf("%s without base.sh: %q, want %q", tool.Name, got, r)
		}
	}
}

func TestMiseVersion(t *testing.T) {
	if v, err := MiseVersion(assets.FS); err != nil || !ToolVersion(v) {
		t.Errorf("devvm's base.sh: %q, %v", v, err)
	}
	if v, err := MiseVersion(toolRoot("")); err != nil || v != "2026.1.0" {
		t.Errorf("MiseVersion = %q, %v", v, err)
	}
	for name, base := range map[string]string{
		"none":         "#!/bin/bash\n",
		"twice":        "mise_version=1\nmise_version=2\n",
		"not a value":  "mise_version=$(id)\n",
		"not the line": "  mise_version=1\n",
	} {
		if v, err := MiseVersion(withBase(toolRoot(""), base)); err == nil {
			t.Errorf("%s: %q, no error", name, v)
		}
	}
	noBase := toolRoot("")
	delete(noBase, "layers/base.sh")
	if _, err := MiseVersion(noBase); err == nil {
		t.Error("no layers/base.sh: no error")
	}
	if _, err := ToolFile(noBase, node()); err == nil {
		t.Error("node without layers/base.sh: no error")
	}
}

func TestToolFileRefuses(t *testing.T) {
	root := toolRoot("")
	bad := node()
	bad.Version = "1$(id)"
	_, err := ToolFile(root, bad)
	if msg.IDOf(err) != "tool_bad_version" {
		t.Errorf("version 1$(id): got %v, want tool_bad_version", err)
	}
	for name, change := range map[string]func(*Tool){
		"empty version": func(t *Tool) { t.Version = "" },
		"tool name":     func(t *Tool) { t.Name = "no-de" },
		"libc":          func(t *Tool) { t.Libc = "" },
		"musl, no base": func(t *Tool) { t.Libc = "musl" },
		"gnu, a base":   func(t *Tool) { t.Base = "debian@sha256:aaaa" },
	} {
		tool := node()
		change(&tool)
		if _, err := ToolFile(root, tool); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if _, err := ToolFile(fstest.MapFS{"shared-tools/mise.toml": {Data: []byte(miseConfig)}}, node()); err == nil {
		t.Error("no shared-tools/build.sh: no error")
	}
	if _, err := ToolFile(fstest.MapFS{"shared-tools/build.sh": {}}, node()); err == nil {
		t.Error("no shared-tools/mise.toml: no error")
	}
	if _, err := ToolFile(withConfig("", "[settings\n"), node()); err == nil {
		t.Error("a mise.toml that isn't TOML: no error")
	}
}

func TestParseToolFile(t *testing.T) {
	f := toolFile(t, toolRoot(""), node())
	tests := []struct {
		file, tool, version string
		ok                  bool
	}{
		{f, "node", "24.21.0", true},
		{"rust-1.90.0-rc.1-0123456789ab.erofs", "rust", "1.90.0-rc.1", true},
		{"node-24.21.0-0123456789ab", "", "", false},
		{"node-0123456789ab.erofs", "", "", false},
		{"node.erofs", "", "", false},
		{"node-$(id)-0123456789ab.erofs", "", "", false},
	}
	for _, tt := range tests {
		tool, version, ok := ParseToolFile(tt.file)
		if tool != tt.tool || version != tt.version || ok != tt.ok {
			t.Errorf("%s: got %q %q %v", tt.file, tool, version, ok)
		}
	}
}

func TestSHA256File(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := SHA256File(p)
	if err != nil {
		t.Fatal(err)
	}
	if want := "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
	if _, err := SHA256File(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing file: %v", err)
	}
}

// A VM's shared tools in their order, the agents among them, and the
// tools behind each tool name.
func TestSortToolsAndGroups(t *testing.T) {
	for in, want := range map[string]string{
		"claude python node": "node python claude",
		"opencode go claude": "go claude opencode",
		"node foo node":      "node",
		"":                   "",
	} {
		if got := strings.Join(SortTools(strings.Fields(in)), " "); got != want {
			t.Errorf("SortTools(%q) = %q, want %q", in, got, want)
		}
	}
	if got := strings.Join(AgentsOf([]string{"node", "codex", "claude"}), " "); got != "codex claude" {
		t.Errorf("AgentsOf = %q", got)
	}
	for name, want := range map[string]string{"node": "node pnpm bun", "python": "python uv", "go": "go", "claude": "claude"} {
		if got := strings.Join(GroupTools(name), " "); got != want {
			t.Errorf("GroupTools(%q) = %q, want %q", name, got, want)
		}
	}
	if !IsTool("rust") || !IsTool("codex") || IsTool("pnpm") || IsTool("base") || !isAgent("opencode") || isAgent("node") {
		t.Error("IsTool or IsAgent is wrong")
	}
	if got := GroupTools("nix"); !slices.Equal(got, []string{"nix"}) {
		t.Errorf("GroupTools(nix) = %q", got)
	}
}

// The table's views: the toolchains, then the agents, the registry's in
// its order, each its own one member; rust alone needs no download in the
// lock, dotnet alone has build.sh fetch its own, and an agent its registry
// marks (pi) alone has builds for glibc alone.
func TestSharedTools(t *testing.T) {
	if want := []string{"node", "python", "rust", "go", "dotnet"}; !slices.Equal(Toolchains, want) {
		t.Errorf("Toolchains = %q, want %q", Toolchains, want)
	}
	if want := agents.Names(); !slices.Equal(Agents, want) || !slices.Contains(want, "claude") {
		t.Errorf("Agents = %q, want %q", Agents, want)
	}
	for _, d := range SharedTools {
		a, isAgent := agents.Get(d.Name)
		if d.GlibcOnly != (isAgent && a.GlibcOnly) || GlibcOnly(d.Name) != d.GlibcOnly || isAgent != (d.Kind == Agent) {
			t.Errorf("%s: GlibcOnly %v, kind %v", d.Name, d.GlibcOnly, d.Kind)
		}
		if isAgent && !slices.Equal(d.Members, []string{d.Name}) {
			t.Errorf("%s: members %q", d.Name, d.Members)
		}
	}
	if !GlibcOnly("pi") || GlibcOnly("claude") {
		t.Error("GlibcOnly: pi has builds for glibc alone, Claude Code for musl too")
	}
	if GlibcOnly("nix") || GlibcOnly("bun") {
		t.Error("GlibcOnly of a layer or another member")
	}
	for tool, want := range map[string]bool{"rust": true, "dotnet": false, "node": false, "pnpm": false, "claude": false, "nix": false} {
		if got := NoDownload(tool); got != want {
			t.Errorf("NoDownload(%s) = %v, want %v", tool, got, want)
		}
	}
	for tool, want := range map[string]bool{"dotnet": true, "rust": false, "node": false, "claude": false, "nix": false} {
		if got := OwnDownload(tool); got != want {
			t.Errorf("OwnDownload(%s) = %v, want %v", tool, got, want)
		}
	}
	if _, ok := Def("pnpm"); ok {
		t.Error("Def(pnpm): a member isn't a shared tool")
	}
	seen := map[string]bool{}
	for i, d := range SharedTools {
		if i > 0 && d.Kind < SharedTools[i-1].Kind {
			t.Errorf("%s: an agent before a toolchain", d.Name)
		}
		if len(d.Members) == 0 || d.Members[0] != d.Name {
			t.Errorf("%s's members %q don't start with it", d.Name, d.Members)
		}
		if d.Kind == Agent && len(d.Detect) > 0 {
			t.Errorf("%s: an agent with files that call for it", d.Name)
		}
		for _, m := range d.Members {
			if seen[m] || !ToolName(m) {
				t.Errorf("member %q: repeated or not a tool's name", m)
			}
			seen[m] = true
		}
	}
}

// Each shared tool has its policy in shared-tools/, and every policy
// there is a shared tool's.
func TestSharedToolsPolicies(t *testing.T) {
	files, err := fs.Glob(assets.FS, "shared-tools/*.policy")
	if err != nil {
		t.Fatal(err)
	}
	var got, want []string
	for _, f := range files {
		got = append(got, strings.TrimSuffix(path.Base(f), ".policy"))
	}
	for _, d := range SharedTools {
		want = append(want, d.Name)
	}
	if slices.Sort(want); !slices.Equal(got, want) {
		t.Errorf("shared-tools' policies are %q, want one per shared tool %q", got, want)
	}
}
