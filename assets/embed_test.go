package assets

import (
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// heredoc is the body of the heredoc that base.sh starts with this line,
// up to its end marker.
func heredoc(t *testing.T, start, end string) string {
	t.Helper()
	b, err := fs.ReadFile(FS, "layers/base.sh")
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(b), start+"\n")
	block, _, closed := strings.Cut(rest, "\n"+end+"\n")
	if !ok || !closed {
		t.Fatalf("base.sh has no %q", start)
	}
	return block
}

// Every VM's shells get the tools' environment from base's
// /etc/profile.d/devvm-tools.sh, which turns off the agents'
// self-updates: their installs are read-only shared tools, and mise pins
// their versions. It turns off .NET's telemetry, first-run banner, profile
// edit and workload update check too, and puts its global tools on the
// PATH.
func TestToolsEnvironment(t *testing.T) {
	block := heredoc(t, "sudo tee /etc/profile.d/devvm-tools.sh >/dev/null <<'SH'", "SH")
	for _, line := range []string{
		"export DISABLE_AUTOUPDATER=1", "export OPENCODE_DISABLE_AUTOUPDATE=1",
		"export DOTNET_CLI_TELEMETRY_OPTOUT=1", "export DOTNET_NOLOGO=1",
		"export DOTNET_ADD_GLOBAL_TOOLS_TO_PATH=false", "export DOTNET_CLI_WORKLOAD_UPDATE_NOTIFY_DISABLE=true",
	} {
		if !strings.Contains("\n"+block+"\n", "\n"+line+"\n") {
			t.Errorf("devvm-tools.sh lacks %q", line)
		}
	}
	for _, dir := range []string{"$HOME/.npm-global/bin:", "$HOME/go/bin:", "$HOME/.cargo/bin:", "$HOME/.dotnet/tools:", "$HOME/.local/bin:"} {
		if !strings.Contains(block, `export PATH="`) || !strings.Contains(block, dir) {
			t.Errorf("devvm-tools.sh's PATH lacks %s", dir)
		}
	}
}

// base.sh's devvm-fetch keeps a download only when its checksum is the
// one given: a SHA-256, or a SHA-512 by its length.
func TestDevvmFetch(t *testing.T) {
	for _, c := range []string{"sh", "curl", "sha256sum", "sha512sum", "cut"} {
		if _, err := exec.LookPath(c); err != nil {
			t.Skip("no " + c)
		}
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "devvm-fetch")
	if err := os.WriteFile(script, []byte(heredoc(t, "sudo tee /usr/local/bin/devvm-fetch >/dev/null <<'SH'", "SH")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "src")
	body := []byte("an archive\n")
	if err := os.WriteFile(src, body, 0o600); err != nil {
		t.Fatal(err)
	}
	s256, s512 := sha256.Sum256(body), sha512.Sum512(body)
	for _, tc := range []struct {
		name, sum string
		ok        bool
	}{
		{"its SHA-256", hex.EncodeToString(s256[:]), true},
		{"its SHA-512", hex.EncodeToString(s512[:]), true},
		{"another SHA-256", strings.Repeat("0", 64), false},
		{"another SHA-512", strings.Repeat("0", 128), false},
		{"its SHA-256 twice", strings.Repeat(hex.EncodeToString(s256[:]), 2), false},
	} {
		out := filepath.Join(dir, "out")
		err := exec.CommandContext(t.Context(), "sh", script, "file://"+src, tc.sum, out).Run() //nolint:gosec // base.sh's devvm-fetch, on the test's own files
		_, kept := os.Stat(out)
		if (err == nil) != tc.ok || (kept == nil) != tc.ok {
			t.Errorf("%s: %v, kept %v", tc.name, err, kept == nil)
		}
		_ = os.Remove(out)
	}
}
