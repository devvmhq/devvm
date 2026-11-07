package platform

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// helperScript is a devvm-immutable that runs the shell script body.
func helperScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "devvm-immutable")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil { //nolint:gosec // a test's own script
		t.Fatal(err)
	}
	return path
}

// The helper works when its check passes in devvm's protocol; otherwise
// it's missing, or it doesn't work, saying why.
func TestHelperUsable(t *testing.T) {
	for name, tc := range map[string]struct {
		helper string
		want   error
		why    string
	}{
		"none":         {"", ErrNoHelper, ""},
		"not there":    {filepath.Join(t.TempDir(), "devvm-immutable"), ErrNoHelper, ""},
		"works":        {helperScript(t, "echo 1"), nil, ""},
		"no cap":       {helperScript(t, `echo "devvm-immutable: it doesn't hold CAP_LINUX_IMMUTABLE" >&2; exit 1`), ErrHelperUnusable, "it doesn't hold CAP_LINUX_IMMUTABLE"},
		"silent":       {helperScript(t, "exit 3"), ErrHelperUnusable, "exit status 3"},
		"another one":  {helperScript(t, "echo 2"), ErrHelperUnusable, `protocol "2"`},
		"not runnable": {t.TempDir(), ErrHelperUnusable, "permission denied"},
	} {
		err := Locker{Helper: tc.helper, Unvetted: true}.HelperUsable(t.Context())
		if !errors.Is(err, tc.want) || tc.want == nil && err != nil || tc.why != "" && !strings.Contains(err.Error(), tc.why) {
			t.Errorf("%s: %v, want %v (%s)", name, err, tc.want, tc.why)
		}
	}
}

// Only a helper that root alone can change is run: one the user owns, or
// in a folder the user owns, or through a link the user owns, is refused,
// saying which; one root owns all the way up passes.
func TestVetHelper(t *testing.T) {
	mine := helperScript(t, "echo 1")
	if err := (Locker{Helper: mine}).HelperUsable(t.Context()); !errors.Is(err, ErrHelperUnusable) || !strings.Contains(err.Error(), mine+" isn't root's") {
		t.Errorf("the user's: %v", err)
	}
	sys := "/usr/bin/true"
	if err := vetHelper(sys); err != nil {
		t.Skipf("%s isn't root's all the way up here: %v", sys, err)
	}
	link := filepath.Join(t.TempDir(), "devvm-immutable")
	if err := os.Symlink(sys, link); err != nil {
		t.Fatal(err)
	}
	if err := vetHelper(link); err == nil || !strings.Contains(err.Error(), "isn't root's") {
		t.Errorf("a link of the user's: %v", err)
	}
}

// Usable needs the helper first, then a file system with the attribute:
// procfs has none.
func TestUsable(t *testing.T) {
	ok := Locker{Helper: helperScript(t, "echo 1"), Unvetted: true}
	if err := (Locker{}).Usable(t.Context(), t.TempDir()); !errors.Is(err, ErrNoHelper) {
		t.Errorf("no helper: %v", err)
	}
	if err := ok.Usable(t.Context(), "/proc"); !errors.Is(err, ErrNoAttribute) {
		t.Errorf("/proc: %v", err)
	}
	if err := ok.Usable(t.Context(), filepath.Join(t.TempDir(), "gone")); !errors.Is(err, ErrNoAttribute) {
		t.Errorf("a folder that's gone: %v", err)
	}
	if err := ok.Usable(t.Context(), t.TempDir()); err != nil && !errors.Is(err, ErrNoAttribute) {
		t.Errorf("a temporary folder: %v", err)
	}
}

// Chflags runs the helper's set for uchg and clear for nouchg, on the one
// path, and its error has what the helper said.
func TestChflagsRunsTheHelper(t *testing.T) {
	log := filepath.Join(t.TempDir(), "log")
	l := Locker{Helper: helperScript(t, `echo "$*" >> `+log+`; [ "$1" = set ] || { echo "devvm-immutable: no" >&2; exit 1; }`), Unvetted: true}
	if err := l.Chflags(t.Context(), "uchg", "/p/.git/hooks"); err != nil {
		t.Fatal(err)
	}
	if err := l.Chflags(t.Context(), "nouchg", "/p/.git/config"); err == nil || !strings.Contains(err.Error(), "devvm-immutable: no") {
		t.Errorf("clear: %v", err)
	}
	if err := l.Chflags(t.Context(), "schg", "/p"); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("schg: %v", err)
	}
	if err := (Locker{}).Chflags(t.Context(), "uchg", "/p"); !errors.Is(err, ErrNoHelper) {
		t.Errorf("no helper: %v", err)
	}
	if got, _ := os.ReadFile(log); string(got) != "set /p/.git/hooks\nclear /p/.git/config\n" { //nolint:gosec // the test's own
		t.Errorf("calls %q", got)
	}
}

// Immutable reads the attribute without opening the file: a file nobody
// can read has none, and nothing that isn't there has it.
func TestImmutableUnreadable(t *testing.T) {
	f := filepath.Join(t.TempDir(), "x")
	if err := os.WriteFile(f, nil, 0o000); err != nil {
		t.Fatal(err)
	}
	if Immutable(f) || Immutable(f+"-gone") {
		t.Error("immutable")
	}
}
