package structure

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestNilawayBaseline runs scripts/nilaway.sh with a stand-in nilaway
// on PATH, which prints out and exits with status, against baseline: a
// finding the baseline has as often as it's found passes, one found more
// often is new (3), and a stale or malformed baseline line, a failed
// nilaway or output the script can't read fails (1).
func TestNilawayBaseline(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	const (
		a = "internal/x/a.go:10:2: Potential nil panic detected. Observed nil flow from source to dereference point: \n" +
			"\t- x/a.go:10:2: result 0 of `Stat()` lacking guarding; called `Mode()` via the assignment(s):\n" +
			"\t\t- `os.Stat(...)` to `fi` at x/a.go:9:2\n\n"
		// The same finding further down the file: the same key.
		a2 = "internal/x/a.go:30:2: Potential nil panic detected. Observed nil flow from source to dereference point: \n" +
			"\t- x/a.go:30:2: result 0 of `Stat()` lacking guarding; called `Mode()` via the assignment(s):\n" +
			"\t\t- `os.Stat(...)` to `fi` at x/a.go:29:2\n\n"
		keyA  = "checked internal/x/a.go: - x/a.go: result 0 of `Stat()` lacking guarding; called `Mode()` via the assignment(s): - `os.Stat(...)` to `fi` at x/a.go\n"
		other = "internal/x/a.go:10:2: some other report\n"
	)
	for _, tc := range []struct {
		name, out, baseline string
		status, want        int
		says                string
	}{
		{"known", a, keyA, 3, 0, "1 findings, 0 not in"},
		{"moved", a2, keyA, 3, 0, "0 not in"},
		{"none", "", "# nothing\n", 0, 0, "0 findings"},
		{"a second with a known key", a + a2, keyA, 3, 3, "new: internal/x/a.go:"},
		{"both known", a + a2, keyA + keyA, 3, 0, "2 findings, 0 not in"},
		{"new", a, "# nothing\n", 3, 3, "new: internal/x/a.go:"},
		{"stale", "", keyA, 0, 1, "stale: internal/x/a.go:"},
		{"a duplicate line stale", a, keyA + keyA, 3, 1, "stale: internal/x/a.go:"},
		{"no group", a, strings.TrimPrefix(keyA, "checked "), 3, 1, "aren't a group"},
		{"an unknown group", a, "fine " + strings.TrimPrefix(keyA, "checked "), 3, 1, "aren't a group"},
		{"output it can't read", other, keyA, 3, 1, "can't read its output"},
		{"findings with status 0", a, keyA, 0, 1, "can't read its output"},
		{"nilaway fails", "", keyA, 1, 1, "failed with status 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			must(os.WriteFile(filepath.Join(dir, "out.txt"), []byte(tc.out), 0o600))
			must(os.WriteFile(filepath.Join(dir, "baseline.txt"), []byte(tc.baseline), 0o600))
			fake := "#!/bin/sh\ncat \"$(dirname \"$0\")/out.txt\"\nexit " + strconv.Itoa(tc.status) + "\n"
			must(os.WriteFile(filepath.Join(dir, "nilaway"), []byte(fake), 0o700))                        //nolint:gosec // a stand-in nilaway that runs
			cmd := exec.CommandContext(t.Context(), "bash", filepath.Join(root, "scripts", "nilaway.sh")) //nolint:gosec // the repo's own script
			cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
				"NILAWAY_BASELINE="+filepath.Join(dir, "baseline.txt"))
			out, err := cmd.CombinedOutput()
			got := 0
			if ee, ok := errors.AsType[*exec.ExitError](err); ok {
				got = ee.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			if got != tc.want || !strings.Contains(string(out), tc.says) {
				t.Errorf("status %d, want %d, and %q in:\n%s", got, tc.want, tc.says, out)
			}
		})
	}
}
