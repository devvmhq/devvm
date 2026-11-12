package proxy

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fixtureBinaries are the SHA-256s of the devvm binary that made each
// release's frozen state (testdata/v<release>/GOOS, which that release
// itself made: never regenerated), by release and system: what its proxy
// configs record.
func fixtureBinaries() map[string]map[string]string {
	return map[string]map[string]string{
		"0.1.0": {
			"darwin": "f8016a61f83f96d6ace74a381630be63d56979eeab3e789d29e4b09106d5d3a6",
			"linux":  "f6e713e44d65ca81d6d035e6f2fedf4534acc1ee72391702ff302dcd3ae53029",
		},
	}
}

// fixtureFile is a file in a VM's run folder of a release's frozen state.
type fixtureFile struct{ path, release, goos string }

// fixtureFiles are the files of name in every VM's run folder of every
// release's frozen state, every system's.
func fixtureFiles(t *testing.T, name string) []fixtureFile {
	t.Helper()
	found, err := filepath.Glob(filepath.Join("..", "..", "testdata", "v*", "*", "data", "devvm", "vms", "*", "run", name))
	if err != nil || len(found) == 0 {
		t.Fatalf("no %s in the releases' states: %v", name, err)
	}
	out := make([]fixtureFile, 0, len(found))
	for _, f := range found {
		// testdata/vRELEASE/GOOS/data/devvm/vms/HASH/run/NAME
		parts := strings.Split(filepath.ToSlash(f), "/")
		i := slices.Index(parts, "testdata")
		out = append(out, fixtureFile{path: f, release: strings.TrimPrefix(parts[i+1], "v"), goos: parts[i+2]})
	}
	return out
}

// Every bindings file a release wrote reads as it was, of version 1, and writing its bindings again
// gives the same bytes: a version 1 file, which that release's proxy reads alike, so a launch after
// an upgrade, or a proxy still running from before it, keeps the VM's secrets.
func TestFixtureBindings(t *testing.T) {
	t.Parallel()
	for _, f := range fixtureFiles(t, "secrets.json") {
		b, err := os.ReadFile(f.path)
		must(t, err)
		l, err := parseBindings(f.path, b)
		if err != nil || l.Version != 1 {
			t.Errorf("%s: version %d, %v", f.path, l.Version, err)
			continue
		}
		if again := (BindingList{Bindings: l.Bindings}).Encode(); !bytes.Equal(again, b) {
			t.Errorf("%s: written again as\n%s\nnot\n%s", f.path, again, b)
		}
	}
}

// Every proxy config a release wrote, as the file the user has (mode 600: git keeps no mode), reads
// with today's ReadConfig, which refuses a field it doesn't know, and records the binary that
// started its proxy, so devvm tells that proxy is another binary's (otherBinary).
func TestFixtureProxyConfig(t *testing.T) {
	t.Parallel()
	recorded := fixtureBinaries()
	dir := t.TempDir()
	for i, f := range fixtureFiles(t, "proxy.json") {
		b, err := os.ReadFile(f.path)
		must(t, err)
		cp := filepath.Join(dir, filepath.Base(filepath.Dir(filepath.Dir(f.path)))+"-"+string(rune('a'+i)))
		must(t, os.WriteFile(cp, b, 0o600))
		c, err := ReadConfig(cp)
		if err != nil {
			t.Errorf("%s: %v", f.path, err)
			continue
		}
		if want, ok := recorded[f.release][f.goos]; !ok || c.DevvmSHA256 != want {
			t.Errorf("%s: devvm_sha256 %q, want %s on %s's %q (fixtureBinaries)", f.path, c.DevvmSHA256, f.release, f.goos, want)
		}
	}
}
