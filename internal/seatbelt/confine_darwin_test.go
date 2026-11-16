package seatbelt

import (
	"flag"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/devvmhq/devvm/internal/daemon"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/proxy"
)

// The child's profile holds on this Mac: this test's binary, run again under sandbox-exec with
// the profile of a child whose executable it is (TestHelperChild), writes regular files in its
// folder and looks at its policy file, but makes nothing else there, reads no file outside, opens
// no broker's config, connects to no unix socket, runs nothing, listens on no port and signals no
// other process, and still connects out.
func TestChildProfileConfines(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat(Exec); err != nil {
		t.Skip("no sandbox-exec")
	}
	// The temporary folder is under /var, a link, as a folder of devvm's can be under one.
	dir := t.TempDir()
	logs, cfg, home, run := filepath.Join(dir, "logs"), filepath.Join(dir, "cfg"), filepath.Join(dir, "home"), filepath.Join(dir, "run")
	for _, d := range []string{logs, cfg, home, run} {
		must(t, os.Mkdir(d, 0o700))
	}
	policy, secret, probe := filepath.Join(cfg, "policy"), filepath.Join(home, "secret"), filepath.Join(run, "proxy.json")
	for _, f := range []string{policy, secret, probe} {
		must(t, os.WriteFile(f, []byte("x\n"), 0o600))
	}
	// A socket's path is at most 104 bytes on macOS, so it's in a short folder of its own.
	sockDir, err := os.MkdirTemp("/tmp", "sb")
	must(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "s")
	lc := new(net.ListenConfig)
	ul, err := lc.Listen(t.Context(), "unix", sock)
	must(t, err)
	defer ul.Close()
	// A port on loopback, which the profile leaves to the floor.
	tl, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	must(t, err)
	defer tl.Close()
	go func() {
		for {
			c, err := tl.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	must(t, os.Mkdir(filepath.Join(logs, "sub"), 0o700))
	self, err := os.Executable()
	must(t, err)
	exe, err := filepath.EvalSymlinks(self)
	must(t, err)
	profile, err := ChildProfile(proxy.ChildNeeds{Exe: exe, Folders: []string{logs}, Watched: []string{policy}})
	must(t, err)
	args := []string{"-p", profile, exe, "-test.run=^TestHelperChild$", "-test.v"}
	// A coverage build writes its data into its folder, the one it may write, not a temporary one.
	if testing.CoverMode() != "" {
		args = append(args, "-test.gocoverdir="+logs)
	}
	args = append(args, "--", logs, policy, secret, probe, sock, tl.Addr().String())
	cmd := exec.CommandContext(t.Context(), Exec, args...) //nolint:gosec // G204: this test's own binary, under its own profile
	// A session of its own, as daemon.Spawn starts the real child, which keeps it from its
	// parent's arguments.
	cmd.Env, cmd.Dir, cmd.SysProcAttr = []string{}, "/", &syscall.SysProcAttr{Setsid: true}
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "--- PASS: TestHelperChild") {
		t.Errorf("under the profile: %v\n%s", err, out)
	}
}

// TestHelperChild is TestChildProfileConfines's process under the profile, given its folder, its
// policy file, a file outside, a broker's config, a unix socket and a TCP address: it checks each
// thing the profile allows or refuses.
func TestHelperChild(t *testing.T) {
	a := flag.Args()
	if len(a) != 6 {
		t.Skip("only under TestChildProfileConfines")
	}
	logs, policy, secret, probe, sock, addr := a[0], a[1], a[2], a[3], a[4], a[5]
	file := filepath.Join(logs, "a.log")
	var d net.Dialer
	for _, c := range []struct {
		name string
		ok   bool
		do   func() error
	}{
		{"a regular file in its folder", true, func() error { return os.WriteFile(file, []byte("x"), 0o600) }},
		{"reading it", true, func() error { return readFile(file) }},
		{"renaming it", true, func() error { return os.Rename(file, file+".1") }},
		{"a symbolic link there", false, func() error { return os.Symlink(secret, filepath.Join(logs, "l")) }},
		{"a folder there", false, func() error { return os.Mkdir(filepath.Join(logs, "d"), 0o700) }},
		{"a FIFO there", false, func() error { return syscall.Mkfifo(filepath.Join(logs, "f"), 0o600) }},
		{"a hard link there", false, func() error { return os.Link(file+".1", filepath.Join(logs, "h")) }},
		{"changing its file's mode", true, func() error { return os.Chmod(file+".1", 0o600) }},
		{"changing its file's flags", false, func() error { return syscall.Chflags(file+".1", 0x1) }}, // UF_NODUMP, harmless if it took
		{"removing a folder there", false, func() error { return os.Remove(filepath.Join(logs, "sub")) }},
		{"removing its file", true, func() error { return os.Remove(file + ".1") }},
		{"looking at its policy file", true, func() error { _, err := os.Stat(policy); return err }},
		{"reading its policy file", false, func() error { return readFile(policy) }},
		{"reading a file outside", false, func() error { return readFile(secret) }},
		{"looking at a file outside", false, func() error { _, err := os.Stat(secret); return err }},
		{"opening its broker's config", false, func() error { return readFile(probe) }},
		{"a unix socket", false, func() error { return dial(t, &d, "unix", sock) }},
		{"running a program", false, func() error {
			// Its streams are pipes, so it's the fork that's refused, not the null device.
			c := exec.CommandContext(t.Context(), "/usr/bin/true")
			c.Stdin, c.Stdout, c.Stderr = strings.NewReader(""), io.Discard, io.Discard
			return c.Run()
		}},
		{"listening on a port", false, func() error {
			l, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
			if err == nil {
				_ = l.Close()
			}
			return err
		}},
		{"signalling its parent", false, func() error { return daemon.Signal(os.Getppid(), 0) }},
		{"listing every process", false, func() error { _, err := syscall.Sysctl("kern.proc.all"); return err }},
		{"reading its parent's arguments", false, func() error { _, err := platform.ProcessArgs(os.Getppid()); return err }},
		{"connecting out, loopback included", true, func() error { return dial(t, &d, "tcp", addr) }},
	} {
		if err := c.do(); (err == nil) != c.ok {
			t.Errorf("%s: %v, want it allowed: %v", c.name, err, c.ok)
		}
	}
}

// readFile opens and reads the file at path.
func readFile(path string) error {
	f, err := os.Open(path) //nolint:gosec // the test's own files
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = io.ReadAll(f)
	return err
}

// dial connects to addr and closes the connection.
func dial(t *testing.T, d *net.Dialer, network, addr string) error {
	c, err := d.DialContext(t.Context(), network, addr)
	if err == nil {
		_ = c.Close()
	}
	return err
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
