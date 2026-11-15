package platform

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/leaktest"
)

// Why the kernel refuses an unprivileged user namespace, from its
// settings: AppArmor's first, then the clone switch, then the count.
func TestUsernsLimit(t *testing.T) {
	for want, settings := range map[string]map[string]string{
		"apparmor": {"/proc/sys/kernel/apparmor_restrict_unprivileged_userns": "1", "/proc/sys/kernel/unprivileged_userns_clone": "0"},
		"sysctl":   {"/proc/sys/kernel/apparmor_restrict_unprivileged_userns": "0", "/proc/sys/kernel/unprivileged_userns_clone": "0"},
		"max":      {"/proc/sys/user/max_user_namespaces": "0"},
		"":         {"/proc/sys/kernel/unprivileged_userns_clone": "1", "/proc/sys/user/max_user_namespaces": "63768"},
	} {
		if got := usernsLimit(func(p string) string { return settings[p] }); got != want {
			t.Errorf("%v: %q, want %q", settings, got, want)
		}
	}
}

// A mount's root is pinned in place; read-only only on a read-only mount.
func TestPinnedMountRoot(t *testing.T) {
	root, err := OpenRoot("/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if !Pinned(root, "proc", false) || Pinned(root, "proc", true) {
		t.Error("/proc: not a writable mount's root")
	}
}

// keep, where it can't pin (no namespace here): a pin it can't make
// again is told once, a file with another name as linked, until the child
// ends, whose end it returns. A folder holding a hook with another name
// is linked too.
func TestKeep(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root's mounts would be this host's")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "hooks"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"f", "hooks/pre-commit"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Link(filepath.Join(dir, "hooks/pre-commit"), filepath.Join(dir, "other")); err != nil {
		t.Fatal(err)
	}
	root, err := OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := PinBeneath(root, "hooks", true); !errors.Is(err, ErrLinked) {
		t.Errorf("a hooks folder with a linked hook: %v", err)
	}
	if Pinned(root, "hooks", true) {
		t.Error("pinned")
	}
	changed := make(chan struct{}, 1)
	done := make(chan error, 1)
	var reports []string
	end := errors.New("child ended")
	go func() {
		changed <- struct{}{}
		time.Sleep(150 * time.Millisecond)
		done <- end
	}()
	err = keep(root, []Pin{{Path: "f", ReadOnly: true}, {Path: "hooks", ReadOnly: true}}, changed, done, 20*time.Millisecond, func(path, what string) {
		reports = append(reports, path+" "+what)
	})
	if !errors.Is(err, end) || !slices.Equal(reports, []string{"f " + PinLost, "hooks " + PinLinked}) {
		t.Errorf("keep: %v, reports %q", err, reports)
	}
	if got := pinFolders("/p", []Pin{{Path: ".git", ReadOnly: false}, {Path: ".git/config", ReadOnly: true}}); !slices.Equal(got, []string{"/p", "/p/.git"}) {
		t.Errorf("folders %q", got)
	}
}

// Linux has neither the user-immutable flag nor a keychain: their calls
// say so, and do nothing.
func TestLinuxHasNone(t *testing.T) {
	ctx := t.Context()
	if HasImmutableFlag || Chflags(ctx, "uchg", "/x") == nil || Immutable("/x") || HasKeychain {
		t.Error("an immutable flag on Linux")
	}
	if _, err := KeychainGet(ctx, "s", "a"); err == nil || KeychainSet(ctx, "s", "a", "l", "v") == nil || KeychainDelete(ctx, "s", "a") == nil {
		t.Error("a keychain on Linux")
	}
	if ExcludeFromBackup(ctx, "/x") != nil {
		t.Error("a backup to exclude from")
	}
}

// The probe's last check: a file it can write isn't pinned, nor one it
// can't for want of permission; only a read-only file system's refusal
// passes.
func TestReadOnlyFS(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "f")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if readOnlyFS(f) == nil {
		t.Error("a writable file passed")
	}
	if os.Getuid() != 0 {
		if err := os.Chmod(f, 0o400); err != nil {
			t.Fatal(err)
		}
		if err := readOnlyFS(f); err == nil || errors.Is(err, os.ErrNotExist) {
			t.Errorf("a file without write permission: %v", err)
		}
	}
	if readOnlyFS(filepath.Join(dir, "gone")) == nil {
		t.Error("a file that isn't there passed")
	}
}

// runChild runs a child to its end and returns its exit; one that can't
// start is an error. Nothing of it runs on once it returns: the goroutine
// that passes the child signals ends with it.
func TestRunChild(t *testing.T) {
	leaktest.Check(t)
	if err := runChild(exec.Command("/bin/true")); err != nil { //nolint:noctx // ends on its own
		t.Errorf("true: %v", err)
	}
	var exit *exec.ExitError
	if err := runChild(exec.Command("/bin/false")); !errors.As(err, &exit) { //nolint:noctx // ends on its own
		t.Errorf("false: %v", err)
	}
	if err := runChild(exec.Command("/no/such/program")); err == nil { //nolint:noctx // never runs
		t.Error("ran what isn't there")
	}
	// A watch that can't be made refuses RunPinned before anything is
	// pinned or run: here, a folder that isn't there to open.
	if err := RunPinned("/no/such/folder", []Pin{{Path: "f", ReadOnly: true}}, []string{"/bin/true"}, time.Second, nil); err == nil {
		t.Error("ran pinned beneath a folder that isn't there")
	}
}

// ExecUnprivileged drops what it can keep and fails like exec when the
// program isn't there (in a child: it sets no-new-privileges for good).
func TestExecUnprivileged(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.CommandContext(t.Context(), self, "pin-helper", "unprivileged").CombinedOutput() //nolint:gosec // the test binary itself
	if err == nil {
		t.Errorf("ran what isn't there: %s", out)
	}
}
