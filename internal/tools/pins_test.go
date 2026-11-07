package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/image"
)

// TestMisePins checks the host's mise pins, one per host devvm runs on,
// 64 hex digits each, and that the mise base.sh installs in every VM,
// which installs the shared tools in their images, is the host's: the
// same version and, for Linux, the same checksums (scripts/bump.sh mise
// moves both).
func TestMisePins(t *testing.T) {
	for _, p := range []string{"macos-arm64", "macos-x64", "linux-arm64", "linux-x64"} {
		if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(miseSHA256[p]) {
			t.Errorf("%s's checksum = %q", p, miseSHA256[p])
		}
	}
	base := string(read(t, repo(t, "assets", "layers", "base.sh")))
	v, err := image.MiseVersion(os.DirFS(repo(t, "assets")))
	if err != nil {
		t.Fatal(err)
	}
	if v != miseVersion {
		t.Errorf("base.sh pins mise %s, internal/tools/mise.go %s", v, miseVersion)
	}
	for arch, platform := range map[string]string{"arm64": "linux-arm64", "amd64": "linux-x64"} {
		m := regexp.MustCompile(`(?m)^mise_sha256_` + arch + `=([0-9a-f]{64})$`).FindStringSubmatch(base)
		if m == nil {
			t.Errorf("base.sh has no mise_sha256_%s", arch)
			continue
		}
		if m[1] != miseSHA256[platform] {
			t.Errorf("base.sh's mise_sha256_%s is %s, mise.go's %s %s", arch, m[1], platform, miseSHA256[platform])
		}
	}
}

// TestE2ELockPins checks e2e's pinned lock against what it pins:
// tests/e2e-mise.lock's first line names the SHA-256 of the
// shared-tools/mise.toml it was resolved from (tests/e2e stops on
// another), and it locks each of mise.toml's tools with a download for
// every platform but rust's (scripts/bump.sh e2e-tools makes it).
func TestE2ELockPins(t *testing.T) {
	config := read(t, repo(t, "assets", "shared-tools", "mise.toml"))
	data := read(t, repo(t, "tests", "e2e-mise.lock"))
	first, _, _ := strings.Cut(string(data), "\n")
	sum := sha256.Sum256(config)
	if want := "# devvm e2e: shared-tools/mise.toml sha256:" + hex.EncodeToString(sum[:]); first != want {
		t.Errorf("tests/e2e-mise.lock starts %q, want %q: run scripts/bump.sh e2e-tools", first, want)
	}
	names, _ := configTools(config)
	if got := lockTools(t, data); !slices.Equal(got, slices.Sorted(slices.Values(names))) {
		t.Errorf("tests/e2e-mise.lock locks %q, mise.toml has %q", got, names)
	}
	lock := parsed(t, data)
	platforms := []string{"linux-arm64", "linux-arm64-musl", "linux-x64", "linux-x64-musl"}
	for _, tool := range names {
		if !lock.HasDownload(tool, platforms) {
			t.Errorf("tests/e2e-mise.lock has no download of %s for every one of %q", tool, platforms)
		}
	}
}

// TestToolBackendsNamed checks that shared-tools/mise.toml's [tool_alias]
// names the backend of every shared tool that isn't one of mise's own
// (core:), and that it's the one tests/e2e-mise.lock resolved: mise's
// registry picks a tool's backend otherwise, and a mise release can move
// its pick (opencode 2's to npm, which has no download per platform).
func TestToolBackendsNamed(t *testing.T) {
	config := read(t, repo(t, "assets", "shared-tools", "mise.toml"))
	aliases, err := configAliases(config)
	if err != nil {
		t.Fatal(err)
	}
	lock := parsed(t, read(t, repo(t, "tests", "e2e-mise.lock")))
	names, _ := configTools(config)
	for _, tool := range names {
		got, named := aliases[tool]
		switch b := lock.Backend(tool); {
		case b == "core:"+tool && !named:
		case !named:
			t.Errorf("mise.toml doesn't name %s's backend (the lock's is %s): add it to [tool_alias]", tool, b)
		case got != b:
			t.Errorf("mise.toml names %s's backend %s, tests/e2e-mise.lock has %s: run scripts/bump.sh e2e-tools", tool, got, b)
		}
	}
	for tool := range aliases {
		if !slices.Contains(names, tool) {
			t.Errorf("mise.toml's [tool_alias] names %s, which isn't one of its [tools]", tool)
		}
	}
}

// Aliases gives a VM's mise the [tool_alias] table, a tool a line.
func TestAliases(t *testing.T) {
	got, err := Aliases(os.DirFS(repo(t, "assets")))
	if err != nil {
		t.Fatal(err)
	}
	want, _ := configAliases(read(t, repo(t, "assets", "shared-tools", "mise.toml")))
	back, err := configAliases(got)
	if err != nil || !maps.Equal(back, want) || len(want) == 0 {
		t.Errorf("Aliases = %q (%v), want %q", got, err, want)
	}
	if !strings.HasPrefix(string(got), "[tool_alias]\nclaude = \"aqua:anthropics/claude-code\"\n") {
		t.Errorf("Aliases = %q", got)
	}
	if _, err := Aliases(os.DirFS(t.TempDir())); err == nil {
		t.Error("no mise.toml, no error")
	}
}
