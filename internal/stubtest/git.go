package stubtest

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// gitRepoVar reports whether k is one of git's variables that pick a repo, its files or its
// config: GIT_DIR, GIT_WORK_TREE, GIT_INDEX_FILE, GIT_COMMON_DIR, GIT_OBJECT_DIRECTORY,
// GIT_ALTERNATE_OBJECT_DIRECTORIES, GIT_NAMESPACE and GIT_CONFIG*. A git that runs the tests sets
// some of them, such as a hook's or git rebase --exec's, and a test's git that kept them would
// work on that repo, the checkout itself, instead of the test's own.
func gitRepoVar(k string) bool {
	switch k {
	case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR", "GIT_OBJECT_DIRECTORY",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE":
		return true
	}
	return strings.HasPrefix(k, "GIT_CONFIG")
}

// GitEnv is env without git's variables that pick a repo, its files or its config, so git run
// with it works on the repo its arguments or its folder name.
func GitEnv(env []string) []string {
	var out []string
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); !gitRepoVar(k) {
			out = append(out, kv)
		}
	}
	return out
}

// Git is plain git, as a test runs it on a repo of its own: in the test's environment without
// GitEnv's variables, whatever git ran the tests, and with no config but the repo's own (no
// global or system file). Every test's git runs through it, which the structure test checks.
func Git(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // a test's own git, on its own repo
	cmd.Env = append(GitEnv(os.Environ()), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	return cmd
}

// NoGitRepo takes GitEnv's variables out of the test's own environment until t ends, for what
// the test runs that runs git itself, such as a stub or a script. New calls it. Like t.Setenv,
// it's not for a parallel test.
func NoGitRepo(t testing.TB) {
	t.Helper()
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); gitRepoVar(k) {
			t.Setenv(k, "")    // restored when t ends
			_ = os.Unsetenv(k) //nolint:forbidigo // a test's own environment, restored by t.Setenv's cleanup
		}
	}
}
