package vm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/state"
)

// devvm cursor warns that Cursor's Agent will run in the VM unless Cursor's user settings on this
// machine set its route to always, and warns the same way when the file is missing or isn't JSON
// with comments; it never fails, and it never writes the file.
func TestCursorAgentWarning(t *testing.T) {
	for name, tc := range map[string]struct {
		settings string // "" for no file
		warn     bool
	}{
		"always":        {"// mine\n{\n  \"editor.fontSize\": 14,\n  \"cursorAgentHost.remoteInferenceRoute\": \"always\",\n}\n", false},
		"absent":        {`{"editor.fontSize": 14}`, true},
		"another value": {`{"cursorAgentHost.remoteInferenceRoute": "remote"}`, true},
		"not a string":  {`{"cursorAgentHost.remoteInferenceRoute": true}`, true},
		"no file":       {"", true},
		"broken JSON":   {`{"cursorAgentHost.remoteInferenceRoute": "always"`, true},
		"not an object": {`["always"]`, true},
		"too long":      {`{"cursorAgentHost.remoteInferenceRoute": "always", "x": "` + strings.Repeat("x", cursorSettingsMax) + `"}`, true},
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			file := cursorUserSettings(home, filepath.Join(home, ".config"))
			if tc.settings != "" {
				if err := state.WriteFile(file, []byte(tc.settings)); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.ReadFile(file) //nolint:gosec // the test's own file
			m, warn := cursorAgentWarning(home, filepath.Join(home, ".config"))
			if warn != tc.warn {
				t.Fatalf("warn %v, want %v", warn, tc.warn)
			}
			if warn && msg.NamesOf(m) != "cursor_agent_in_vm file="+file {
				t.Errorf("the warning: %s", msg.NamesOf(m))
			}
			if after, _ := os.ReadFile(file); string(after) != string(before) { //nolint:gosec // the test's own file
				t.Error("the settings file changed")
			}
			if tc.settings == "" {
				if _, err := os.Stat(filepath.Dir(file)); err == nil {
					t.Error("the settings folder was made")
				}
			}
		})
	}
}
