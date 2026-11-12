package stubtest

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

// GitEnv takes out git's variables that pick a repo, its files or its
// config, and keeps the rest.
func TestGitEnv(t *testing.T) {
	env := []string{
		"HOME=/h", "GIT_DIR=/r/.git", "GIT_WORK_TREE=/r", "GIT_INDEX_FILE=/r/.git/index", "GIT_COMMON_DIR=/r/.git",
		"GIT_OBJECT_DIRECTORY=/r/.git/objects", "GIT_ALTERNATE_OBJECT_DIRECTORIES=/o", "GIT_NAMESPACE=n",
		"GIT_CONFIG=/c", "GIT_CONFIG_PARAMETERS='core.bare'='true'", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=k",
		"GIT_CONFIG_VALUE_0=v", "GIT_CONFIG_GLOBAL=/g", "GIT_AUTHOR_NAME=a", "PATH=/bin",
	}
	if got, want := GitEnv(env), []string{"HOME=/h", "GIT_AUTHOR_NAME=a", "PATH=/bin"}; !slices.Equal(got, want) {
		t.Errorf("GitEnv = %q, want %q", got, want)
	}
}

// Git works on the repo its arguments name, never the one GIT_DIR names
// for the git that ran the tests, and reads no config but the repo's;
// NoGitRepo takes the same variables out of the test's environment, for
// what it runs, until the test ends.
func TestGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	decoy, mine := t.TempDir(), t.TempDir()
	if out, err := Git(t.Context(), "init", "-q", decoy).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	config := filepath.Join(decoy, ".git", "config")
	was, err := os.ReadFile(config) //nolint:gosec // the test's own
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_DIR", filepath.Join(decoy, ".git"))
	t.Setenv("GIT_CONFIG_PARAMETERS", "'core.bare'='true'")
	global := filepath.Join(decoy, "global")
	if err := os.WriteFile(global, []byte("[decoy]\n\tk = global\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	for _, args := range [][]string{{"init", "-q"}, {"config", "core.fsmonitor", "/nowhere"}} {
		if out, err := Git(t.Context(), append([]string{"-C", mine}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if now, err := os.ReadFile(config); err != nil || string(now) != string(was) { //nolint:gosec // the test's own
		t.Errorf("the decoy's config changed:\n%s", now)
	}
	if out, _ := Git(t.Context(), "-C", mine, "config", "decoy.k").Output(); len(out) != 0 {
		t.Errorf("read the global config GIT_CONFIG_GLOBAL named: %q", out)
	}
	if out, err := Git(t.Context(), "-C", mine, "config", "core.fsmonitor").Output(); err != nil || string(out) != "/nowhere\n" {
		t.Errorf("the test's repo: core.fsmonitor %q, %v", out, err)
	}
	t.Run("NoGitRepo", func(t *testing.T) {
		NoGitRepo(t)
		for _, k := range []string{"GIT_DIR", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_GLOBAL"} {
			if v, ok := os.LookupEnv(k); ok {
				t.Errorf("%s=%s", k, v)
			}
		}
	})
	if v := os.Getenv("GIT_DIR"); v != filepath.Join(decoy, ".git") {
		t.Errorf("GIT_DIR after NoGitRepo's test: %q", v)
	}
}
