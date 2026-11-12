package platform

import (
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain makes the test binary the namespace's side of the pin tests
// when it's run as pin-helper (pinHelper). Its children write their
// coverage where the test's goes.
func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == "pin-helper" {
		if err := pinHelper(os.Args[2], os.Args[3:]); err != nil {
			_, _ = os.Stderr.WriteString(err.Error() + "\n") //nolint:forbidigo // the test binary as a helper process
			os.Exit(1)                                       //nolint:forbidigo // the test binary as a helper process
		}
		os.Exit(0) //nolint:forbidigo // the test binary as a helper process
	}
	flag.Parse()
	if f := flag.Lookup("test.gocoverdir"); f != nil && f.Value.String() != "" {
		_ = os.Setenv("GOCOVERDIR", f.Value.String()) // the test binary's children's coverage
	}
	os.Exit(m.Run()) //nolint:forbidigo // the test binary
}

// pinHelper is the namespace's side: probe DIR (ProbePin); run ROOT,
// which pins ROOT/f and runs waiter ROOT as QEMU would run (RunPinned),
// recording what changed in ROOT/events; waiter ROOT, which says it's
// ready, waits for the test to replace f, and checks f is pinned again,
// as the new file; and unprivileged, which drops its capabilities to run
// what isn't there.
func pinHelper(mode string, args []string) error {
	self, _ := os.Executable()
	pins := []Pin{{Path: "f", ReadOnly: true}}
	switch mode {
	case "probe":
		return ProbePin(args[0])
	case "run":
		root := args[0]
		return RunPinned(root, pins, []string{self, "pin-helper", "waiter", root}, 50*time.Millisecond, func(path, what string) {
			f, err := os.OpenFile(filepath.Join(root, "events"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600) //nolint:gosec // the test's own folder
			if err == nil {
				_, _ = f.WriteString(path + " " + what + "\n")
				_ = f.Close()
			}
		})
	case "waiter":
		return waitPinnedAgain(args[0])
	case "unprivileged":
		return ExecUnprivileged("/no/such/program", []string{"program"}, nil)
	}
	return errors.New("no such helper: " + mode)
}

// waitPinnedAgain, in the namespace, says it's ready, waits for the test
// to replace root/f, and checks f is pinned again, as the new file.
func waitPinnedAgain(root string) error {
	if err := os.WriteFile(filepath.Join(root, "ready"), nil, 0o600); err != nil {
		return err
	}
	r, err := OpenRoot(root)
	if err != nil {
		return err
	}
	for range 200 {
		if b, _ := os.ReadFile(filepath.Join(root, "f")); string(b) == "new" && Pinned(r, "f", true) { //nolint:gosec // the test's own folder
			if os.WriteFile(filepath.Join(root, "f"), []byte("guest"), 0o600) == nil {
				return errors.New("the new f is writable")
			}
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return errors.New("the new f isn't pinned")
}

// A file's link count: 1 for a file of its own, 2 once it has another
// name.
func TestLinks(t *testing.T) {
	f := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	one, err := os.Lstat(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Link(f, f+"2"); err != nil {
		t.Fatal(err)
	}
	two, err := os.Lstat(f)
	if err != nil {
		t.Fatal(err)
	}
	if Links(one) != 1 || Links(two) != 2 {
		t.Errorf("links %d, %d", Links(one), Links(two))
	}
	// The same file system, and the file's owner: this process's user.
	if !SameDevice(one, two) {
		t.Error("two names of one file on two file systems")
	}
	if uid, _, ok := Owner(one); ok && uid != uint32(os.Getuid()) { //nolint:gosec // a uid fits
		t.Errorf("owner %d", uid)
	}
}

// Pins without a namespace: a path is opened beneath its folder alone,
// never through a symlink nor a folder that's one, and only a file or a
// folder is pinned; this process has no capability to bind, so the bind
// itself is refused. Off Linux, nothing pins.
func TestPinChecks(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !HasPins {
		if _, err := OpenRoot(dir); err == nil || PinBeneath(nil, "f", true) == nil || Pinned(nil, "f", true) ||
			PrivateMounts() == nil || ExecUnprivileged("/bin/true", nil, nil) == nil || UsernsLimit() != "" {
			t.Error("pins off Linux")
		}
		if _, _, err := WatchFolders([]string{dir}); err == nil {
			t.Error("a watch off Linux")
		}
		if err := InUserNamespace(exec.Command("/bin/true")); err == nil { //nolint:noctx // never run
			t.Error("a namespace off Linux")
		}
		if RunInNamespace("/bin/true", nil) == nil || PinAll(dir, nil) == nil || ProbePin(dir) == nil ||
			RunPinned(dir, nil, []string{"/bin/true"}, time.Second, nil) == nil ||
			Probe(func(string) []string { return []string{"/bin/true"} }, time.Second) == nil {
			t.Error("pinned off Linux")
		}
		done, stop := WaitExit(os.Getpid())
		defer stop()
		<-done
		return
	}
	if os.Getuid() == 0 {
		t.Skip("root could bind for real here")
	}
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, l := range [][2]string{{"f", "link"}, {dir, "up"}} {
		if err := os.Symlink(l[0], filepath.Join(dir, l[1])); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := OpenRoot(filepath.Join(dir, "up")); err == nil {
		t.Error("opened a folder through a link")
	}
	root, err := OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	for _, rel := range []string{"link", "up/f", "../f", "/etc/passwd", "", "fifo", "gone"} {
		if err := PinBeneath(root, rel, true); err == nil {
			t.Errorf("%q pinned", rel)
		}
	}
	if err := PinBeneath(root, "f", true); err == nil {
		t.Error("pinned without CAP_SYS_ADMIN")
	}
	if Pinned(root, "f", true) || Pinned(root, "gone", false) {
		t.Error("a file that isn't pinned is")
	}
	if l := UsernsLimit(); !slices.Contains([]string{"", "apparmor", "sysctl", "max"}, l) {
		t.Errorf("limit %q", l)
	}
	if err := InUserNamespace(exec.Command("/bin/true")); err != nil { //nolint:noctx // never run
		t.Errorf("namespace: %v", err)
	}
}

// In a namespace (the test binary as devvm's _qemu, pinHelper): the probe
// passes, and once this side replaces a pinned file, as git config on the
// host does, RunPinned pins the new file within its watch, and says so,
// while its child, QEMU's stand-in, runs. A file with another name isn't
// pinned, and the child doesn't run. Nothing stays pinned here.
func TestPinsInNamespace(t *testing.T) {
	if !HasPins || UsernsLimit() != "" {
		t.Skip("pins need a Linux host that lets devvm in a user namespace")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := Probe(func(dir string) []string { return []string{self, "pin-helper", "probe", dir} }, 10*time.Second); err != nil {
		t.Skipf("this host can't pin: %v", err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(root, "f")
	if err := os.WriteFile(f, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(f, filepath.Join(root, "g")); err != nil {
		t.Fatal(err)
	}
	if err := RunInNamespace(self, []string{"pin-helper", "run", root}); err == nil {
		t.Error("pinned a file with another name")
	}
	if _, err := os.Stat(filepath.Join(root, "ready")); err == nil {
		t.Error("its child ran")
	}
	if err := os.Remove(filepath.Join(root, "g")); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- RunInNamespace(self, []string{"pin-helper", "run", root}) }()
	for i := 0; i < 200; i++ {
		if _, err := os.Stat(filepath.Join(root, "ready")); err == nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err := os.WriteFile(f+".lock", []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(f+".lock", f); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Errorf("kept: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "events")); string(b) != "f "+PinAgain+"\n" {
		t.Errorf("events %q", b)
	}
	if err := os.WriteFile(f, []byte("host"), 0o600); err != nil {
		t.Errorf("pinned here: %v", err)
	}
}

// Outside a namespace of its own, where nothing has CAP_SYS_ADMIN, the
// namespace's side fails at its first mount and never starts QEMU. A host
// whose kernel refuses the namespace (Ubuntu's AppArmor) refuses
// RunInNamespace too, or gives it no capability.
func TestPinsWithoutNamespace(t *testing.T) {
	if !HasPins {
		t.Skip("Linux's")
	}
	if os.Getuid() == 0 {
		t.Skip("root's mounts would be this host's")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"probe", "run"} {
		if out, err := exec.CommandContext(t.Context(), self, "pin-helper", mode, root).CombinedOutput(); err == nil { //nolint:gosec // the test binary itself
			t.Errorf("%s pinned without a namespace: %s", mode, out)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "ready")); err == nil {
		t.Error("QEMU's stand-in ran unpinned")
	}
	if UsernsLimit() != "" {
		if err := Probe(func(dir string) []string { return []string{self, "pin-helper", "probe", dir} }, 10*time.Second); err == nil {
			t.Error("probed through the kernel's limit")
		}
		_ = RunInNamespace(self, []string{"pin-helper", "probe", root})
	}
	if err := os.WriteFile(filepath.Join(root, "f"), []byte("y"), 0o600); err != nil {
		t.Errorf("f pinned: %v", err)
	}
}

// The keeper's two waits: a change in a watched folder, and a process
// that ends.
func TestPinWaits(t *testing.T) {
	if !HasPins {
		t.Skip("Linux's")
	}
	dir := t.TempDir()
	// A folder it can't watch fails the watch, naming it.
	if _, _, err := WatchFolders([]string{dir, filepath.Join(dir, "gone")}); err == nil || !strings.Contains(err.Error(), "gone") {
		t.Errorf("a folder that isn't there: %v", err)
	}
	changed, stop, err := WatchFolders([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if err := os.WriteFile(filepath.Join(dir, "new"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Error("no change seen")
	}
	cmd := exec.Command("sleep", "0.1") //nolint:noctx // ends on its own
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done, unwait := WaitExit(cmd.Process.Pid)
	defer unwait()
	go func() { _ = cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("no end seen")
	}
	// A process that isn't there is taken as ended.
	gone, unwaitGone := WaitExit(1 << 30)
	defer unwaitGone()
	<-gone
}
