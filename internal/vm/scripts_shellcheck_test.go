package vm

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/devvmhq/devvm/internal/image"
)

// The scripts devvm builds at run time from the agents' registry pass
// shellcheck as bash, which runs them, for every agent at once and each
// alone. The structure test's TestEmbeddedScripts checks every script
// that's a constant, and lists these there (dynamicScripts).
func TestBuiltScriptsShellcheck(t *testing.T) {
	sc, err := exec.LookPath("shellcheck")
	if err != nil {
		t.Skip("no shellcheck here")
	}
	scripts := map[string]string{
		"agentSaveScript(all)": agentSaveScript(image.Agents),
		"historyExport":        historyExport(),
		"historyImport":        historyImport(),
	}
	for _, a := range image.Agents {
		scripts["agentSaveScript("+a+")"] = agentSaveScript([]string{a})
	}
	dir := t.TempDir()
	for name, s := range scripts {
		file := filepath.Join(dir, "script.sh")
		if err := os.WriteFile(file, []byte(s+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		// Warnings and errors, as the structure test's TestEmbeddedScripts.
		if out, err := exec.CommandContext(t.Context(), sc, "-s", "bash", "-S", "warning", file).CombinedOutput(); err != nil { //nolint:gosec // shellcheck, on the test's own file
			t.Errorf("%s: shellcheck -s bash -S warning:\n%s", name, out)
		}
	}
}
