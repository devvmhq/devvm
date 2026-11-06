package main

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/stubtest/stubstate"
)

// A sandbox runs stubs in this process, in a folder of its own: their
// environment, what they ran in their place and how long they slept.
type sandbox struct {
	t        *testing.T
	dir      string
	env      map[string]string
	replaced [][]string
	extra    [][]string
	slept    []time.Duration
	// onSleep runs at each sleep, as another process meanwhile.
	onSleep func()
	// stdout and stderr, when set, are where a stub writes in place of
	// what run returns.
	stdout, stderr io.Writer
}

func newSandbox(t *testing.T) *sandbox {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	return &sandbox{t: t, dir: dir, env: map[string]string{
		"STUB_DIR":  dir,
		"STUB_LOG":  filepath.Join(dir, "calls"),
		"LIMA_HOME": filepath.Join(dir, "lima"),
	}}
}

// run runs the stub name with args and stdin, returning its exit status,
// stdout and stderr.
func (s *sandbox) run(stdin, name string, args ...string) (int, string, string) {
	s.t.Helper()
	var out, errOut bytes.Buffer
	stdout, stderr := io.Writer(&out), io.Writer(&errOut)
	if s.stdout != nil {
		stdout, stderr = s.stdout, s.stderr
	}
	c := &call{
		ctx: s.t.Context(), name: name, args: args, stdin: strings.NewReader(stdin), stdout: stdout, stderr: stderr,
		lookup: func(k string) (string, bool) { v, ok := s.env[k]; return v, ok },
		replace: func(argv, extra []string) int {
			s.replaced = append(s.replaced, argv)
			s.extra = append(s.extra, extra)
			return 0
		},
		sleep: func(d time.Duration) {
			s.slept = append(s.slept, d)
			if s.onSleep != nil {
				s.onSleep()
			}
		},
	}
	return c.run(), out.String(), errOut.String()
}

// ok runs a stub that must succeed, returning its stdout.
func (s *sandbox) ok(stdin, name string, args ...string) string {
	s.t.Helper()
	code, out, errOut := s.run(stdin, name, args...)
	if code != 0 {
		s.t.Fatalf("%s %q: exit %d\n%s", name, args, code, errOut)
	}
	return out
}

// fails runs a stub that must exit with code.
func (s *sandbox) fails(code int, name string, args ...string) {
	s.t.Helper()
	if got, _, errOut := s.run("", name, args...); got != code {
		s.t.Errorf("%s %q: exit %d, want %d\n%s", name, args, got, code, errOut)
	}
}

func (s *sandbox) touch(name string) { s.write(name, "") }

func (s *sandbox) write(name, content string) {
	s.t.Helper()
	if err := writeFile(filepath.Join(s.dir, name), []byte(content)); err != nil {
		s.t.Fatal(err)
	}
}

func (s *sandbox) read(name string) string {
	s.t.Helper()
	b, err := os.ReadFile(filepath.Join(s.dir, name)) //nolint:gosec // the test's own folder
	if err != nil {
		s.t.Fatal(err)
	}
	return string(b)
}

func (s *sandbox) rm(name string) {
	s.t.Helper()
	if err := os.Remove(filepath.Join(s.dir, name)); err != nil {
		s.t.Fatal(err)
	}
}

func (s *sandbox) state() *stubstate.State {
	s.t.Helper()
	st, err := stubstate.Read(s.dir)
	if err != nil {
		s.t.Fatal(err)
	}
	return st
}

// calls are the logged calls, one a line.
func (s *sandbox) calls() []string {
	s.t.Helper()
	b, err := os.ReadFile(filepath.Join(s.dir, "calls"))
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// list is limactl list --json, decoded.
func (s *sandbox) list() []listed {
	s.t.Helper()
	var ls []listed
	dec := json.NewDecoder(strings.NewReader(s.ok("", "limactl", "list", "--json")))
	for dec.More() {
		var l listed
		if err := dec.Decode(&l); err != nil {
			s.t.Fatal(err)
		}
		ls = append(ls, l)
	}
	return ls
}

// An unknown name is no stub; the stub command lists every one.
func TestDispatch(t *testing.T) {
	s := newSandbox(t)
	s.fails(127, "nope")
	names := strings.Fields(s.ok("", "stub", "names"))
	if !slices.Contains(names, "limactl") || !slices.Contains(names, "stub") || !slices.IsSorted(names) {
		t.Errorf("names %q", names)
	}
}

// Each call is logged as its words; with no log, nothing is.
func TestLog(t *testing.T) {
	s := newSandbox(t)
	s.ok("", "chflags", "-R", "uchg", "a b")
	s.ok("", "qemu-system-x86_64", "--version")
	if got, want := s.calls(), []string{"chflags -R uchg a b", "qemu-system-x86_64 --version"}; !slices.Equal(got, want) {
		t.Errorf("calls %q, want %q", got, want)
	}
	delete(s.env, "STUB_LOG")
	s.ok("", "chflags", "x")
	if got := s.calls(); len(got) != 2 {
		t.Errorf("logged without a log: %q", got)
	}
	s.env["STUB_LOG"] = filepath.Join(s.dir, "no", "such", "calls")
	if _, _, errOut := s.run("", "chflags", "x"); !strings.Contains(errOut, "stub:") {
		t.Errorf("an unwritable log: %q", errOut)
	}
}

// create, clone, edit, start, stop and delete keep Lima's instances as
// list --json shows them.
func TestLimactlLifecycle(t *testing.T) {
	s := newSandbox(t)
	s.ok("template: x\n", "limactl", "create", "--name=a", "--tty=false", "--cpus=2", "--memory=1.5", "--disk=10",
		"--set", `.mounts = [{"location":"/x"}]`, "--set", ".ssh.forwardAgent=false", "--set", `.env = {"A":"b"}`, "-")
	if got := s.read("create-template"); got != "template: x\n" {
		t.Errorf("template %q", got)
	}
	ls := s.list()
	if len(ls) != 1 || ls[0].Name != "a" || ls[0].Status != "Stopped" || ls[0].CPUs != 2 || ls[0].Memory != 1610612736 || ls[0].Disk != 10<<30 {
		t.Fatalf("after create: %+v", ls)
	}
	if got, want := string(ls[0].Config), `{"mounts":[{"location":"/x"}],"ssh":{"forwardAgent":false},"env":{"A":"b"}}`; got != want {
		t.Errorf("config %s, want %s", got, want)
	}
	if !strings.HasPrefix(s.read("limactl-env"), "cwd="+s.dir+" LIMA_HOME=") {
		t.Errorf("limactl-env %q", s.read("limactl-env"))
	}
	// A clone: its source's config, size and lima.yaml, its mounts gone.
	s.ok("", "limactl", "clone", "a", "b", "--mount-none", "--set", ".hostResolver.enabled=true")
	s.ok("", "limactl", "edit", "b", "--cpus", "3", "--memory", "2", "--disk", "20", "--set", "del(.ssh)", "--set", "del(.env)")
	ls = s.list()
	if len(ls) != 2 || ls[1].Name != "b" || ls[1].CPUs != 3 || ls[1].Memory != 2<<30 || ls[1].Disk != 20<<30 {
		t.Fatalf("after clone and edit: %+v", ls)
	}
	if got, want := string(ls[1].Config), `{"hostResolver":{"enabled":true}}`; got != want {
		t.Errorf("clone's config %s, want %s", got, want)
	}
	// Editing a VM that isn't there lists nothing.
	s.ok("", "limactl", "edit", "nope", "--set", ".x=1")
	if len(s.list()) != 2 {
		t.Errorf("an edit made an instance: %+v", s.list())
	}
	// start: running, its ha.pid, ssh config and agent.
	s.env["SSH_AUTH_SOCK"] = "/agent"
	s.write("hostagent", "4242\n")
	s.ok("", "limactl", "start", "--tty=false", "a")
	if s.list()[0].Status != "Running" || s.read("lima/a/ha.pid") != "4242\n" || s.read("start-agent-a") != "/agent\n" ||
		!strings.Contains(s.read("lima/a/ssh.config"), "Host lima-a\n") {
		t.Errorf("after start: %+v", s.list())
	}
	s.ok("", "limactl", "stop", "a")
	if s.list()[0].Status != "Stopped" || exists(filepath.Join(s.dir, "lima/a/ha.pid")) {
		t.Errorf("after stop: %+v", s.list())
	}
	s.ok("", "limactl", "delete", "-f", "a", "b")
	if ls := s.list(); len(ls) != 0 || exists(filepath.Join(s.dir, "lima/a")) {
		t.Errorf("after delete: %+v", ls)
	}
	if out := s.ok("", "limactl", "--version"); out != "limactl version 2.2.1\n" {
		t.Errorf("--version %q", out)
	}
	s.ok("", "limactl", "list")
}

// Each of limactl's switches fails or hangs what it names.
func TestLimactlSwitches(t *testing.T) {
	s := newSandbox(t)
	s.touch("clone-fails")
	s.fails(1, "limactl", "clone", "base", "part")
	if ls := s.list(); len(ls) != 1 || ls[0].Name != "part" {
		t.Errorf("a partial clone: %+v", ls)
	}
	s.touch("delete-fails")
	s.fails(1, "limactl", "delete", "part")
	if ls := s.list(); len(ls) != 0 {
		t.Errorf("a failed delete kept it: %+v", ls)
	}
	s.touch("start-fails")
	s.fails(1, "limactl", "start", "vm")
	if s.list()[0].Status != "Running" {
		t.Error("a degraded start isn't running")
	}
	s.rm("start-fails")
	s.write("start-hangs-running", "v")
	s.ok("", "limactl", "start", "vm")
	s.rm("start-hangs-running")
	s.touch("start-hangs")
	s.ok("", "limactl", "start", "vm")
	if len(s.slept) != 2 || !exists(filepath.Join(s.dir, "hanging")) {
		t.Errorf("slept %v", s.slept)
	}
	s.touch("list-fails")
	s.fails(1, "limactl", "list", "--json")
	s.rm("list-fails")
	s.touch("list-hangs")
	s.ok("", "limactl", "list", "--json")
	if len(s.slept) != 3 {
		t.Errorf("list didn't hang: %v", s.slept)
	}
}

// rename moves a stopped instance, and refuses a running one, a name
// that's taken, or any with rename-fails.
func TestLimactlRename(t *testing.T) {
	s := newSandbox(t)
	s.ok("", "limactl", "create", "--name=a", "--cpus=2")
	s.ok("", "limactl", "create", "--name=c")
	s.fails(1, "limactl", "rename", "a", "c")
	s.fails(1, "limactl", "rename", "nope", "x")
	s.ok("", "limactl", "start", "a")
	s.fails(1, "limactl", "rename", "a", "b")
	s.ok("", "limactl", "stop", "a")
	s.touch("rename-fails")
	s.fails(1, "limactl", "rename", "a", "b")
	s.rm("rename-fails")
	s.ok("", "limactl", "rename", "a", "b")
	if ls := s.list(); len(ls) != 2 || ls[0].Name != "b" || ls[0].CPUs != 2 || !exists(filepath.Join(s.dir, "lima/b")) {
		t.Errorf("after rename: %+v", ls)
	}
}

// A call that names interrupt-on's text interrupts the stub's parent,
// once.
func TestLimactlInterrupt(t *testing.T) {
	s := newSandbox(t)
	s.write("interrupt-on", "oci/build.sh\n")
	child := exec.CommandContext(t.Context(), "sleep", "10")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	c := &call{
		ctx: t.Context(), name: "limactl", args: []string{"shell", "x", "/oci/build.sh"}, stdin: strings.NewReader(""),
		stdout: &out, stderr: &out, ppid: child.Process.Pid,
		lookup: func(k string) (string, bool) { v, ok := s.env[k]; return v, ok },
	}
	if code := c.run(); code != 130 {
		t.Errorf("exit %d", code)
	}
	err := child.Wait()
	if exit, ok := err.(*exec.ExitError); !ok || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGINT { //nolint:errorlint // the child's own status
		t.Errorf("the parent: %v", err)
	}
	if exists(filepath.Join(s.dir, "interrupt-on")) {
		t.Error("interrupt-on stayed")
	}
}

// The tools build VM writes the images its build.sh is asked for, in the
// folder its clone mounts, or a hostile build's symlinks.
func TestToolsBuild(t *testing.T) {
	s := newSandbox(t)
	out := filepath.Join(s.dir, "share")
	s.ok("", "limactl", "clone", "base", "devvm-tools-build", "--set", `.mounts = [{"location": "`+out+`", "writable": true}]`)
	s.write("tools-glibc-node", "2.28\n")
	build := []string{"shell", "--workdir", "/", "devvm-tools-build", "bash", "-s", "--", "node", "22", "node-22.erofs", "go", "1.24", "go-1.24.erofs"}
	s.ok("#!/bin/bash\n", "limactl", build...)
	if s.read("share/node-22.erofs") != "image node-22.erofs\n" || s.read("share/node-22.erofs.glibc") != "2.28\n" || s.read("share/go-1.24.erofs.glibc") != "" {
		t.Error("the images")
	}
	if err := os.RemoveAll(out); err != nil {
		t.Fatal(err)
	}
	s.touch("tools-symlink")
	s.touch("tools-hash-symlink")
	s.ok("", "limactl", build[:10]...)
	if l, err := os.Readlink(filepath.Join(out, "node-22.erofs")); err != nil || l != filepath.Join(s.dir, "tools-symlink") {
		t.Errorf("symlink %q, %v", l, err)
	}
	if _, err := os.Readlink(filepath.Join(out, "node-22.erofs.json")); err != nil {
		t.Errorf("hash symlink: %v", err)
	}
}

// The --from build VM's build.sh writes what it would, or a hostile
// image's files, or fails.
func TestOCIBuild(t *testing.T) {
	s := newSandbox(t)
	out := filepath.Join(s.dir, "out")
	build := []string{"shell", "devvm-oci-build", "sudo", "bash", "/tmp/x/oci/build.sh", "--", out}
	s.ok("", "limactl", build...)
	disk := s.read("out/disk.raw")
	if s.read("out/libc") != "gnu\n" || s.read("out/glibc") != "2.36\n" || len(disk) != 1082 || disk[1080:] != "\x53\xef" ||
		!strings.HasPrefix(s.read("out/cmdline"), "root=/dev/vda") {
		t.Error("the default build")
	}
	if err := os.RemoveAll(out); err != nil {
		t.Fatal(err)
	}
	s.write("build-libc", "musl\n")
	s.touch("build-qcow2")
	s.write("build-symlinks", "kernel /etc/passwd\n\n")
	s.ok("", "limactl", build...)
	if exists(filepath.Join(out, "glibc")) || s.read("out/disk.raw") != "QFI\xfb" {
		t.Error("a musl qcow2 build")
	}
	if l, err := os.Readlink(filepath.Join(out, "kernel")); err != nil || l != "/etc/passwd" {
		t.Errorf("kernel %q, %v", l, err)
	}
	s.touch("build-fails")
	s.fails(1, "limactl", build...)
}

// limactl shell: the guest's answers and switches.
func TestShell(t *testing.T) {
	s := newSandbox(t)
	sh := func(args ...string) []string { return append([]string{"shell", "--workdir", "/", "vm"}, args...) }
	if out := s.ok("", "limactl", sh("sh", "-c", "cat /etc/ssh/ssh_host_*_key.pub")...); !strings.HasPrefix(out, hostKey("vm")+"\nnot a key\n") {
		t.Errorf("host keys %q", out)
	}
	if !strings.HasPrefix(hostKey("vm"), "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI") {
		t.Errorf("not an ed25519 key: %s", hostKey("vm"))
	}
	if s.read("shell-agent") != "unset\n" {
		t.Errorf("shell-agent %q", s.read("shell-agent"))
	}
	s.touch("hostkeys-fails")
	s.fails(1, "limactl", sh("cat /etc/ssh/ssh_host_*_key.pub")...)
	s.fails(7, "limactl", sh("cd '/' && exec 'curl' '--noproxy' '*'")...)
	s.touch("net-direct")
	s.ok("", "limactl", sh("cd '/' && exec 'curl' '--noproxy' '*'")...)
	// The setup's tar, and the unmounted tools it reports.
	s.ok("tarball", "limactl", sh("sh", "-c", "tar -xf - -C x")...)
	if s.read("sync.tar") != "tarball" {
		t.Error("sync.tar")
	}
	s.fails(1, "limactl", sh("cat /run/devvm/tools-unmounted")...)
	s.write("sync-fails-with", "node\n")
	s.fails(1, "limactl", sh("sh", "-c", "tar -xf - -C x")...)
	if out := s.ok("", "limactl", sh("cat /run/devvm/tools-unmounted")...); out != "node\n" {
		t.Errorf("unmounted %q", out)
	}
	// A base build's packages, in and out.
	s.ok("pkgs", "limactl", sh("sudo", "sh", "-c", "tar -x -C /var/cache")...)
	if s.read("packages-in.tar") != "pkgs" {
		t.Error("packages-in.tar")
	}
	if out := s.ok("", "limactl", sh("sudo", "sh", "-c", "tar -c -C /out .")...); len(out) != 1024 {
		t.Errorf("no packages: %d bytes", len(out))
	}
	s.write("packages-out.tar", "out")
	if out := s.ok("", "limactl", sh("sudo", "sh", "-c", "tar -c -C /out .")...); out != "out" {
		t.Errorf("packages %q", out)
	}
	// devvm agent save's stream: an empty one, the test's, and a failure.
	if out := s.ok("", "limactl", sh("bash", "-c", "tar -cf - -C \"$t\" .devvm-save")...); len(out) != 1024 {
		t.Errorf("no agent settings: %d bytes", len(out))
	}
	s.write("agent-save.tar", "settings")
	if out := s.ok("", "limactl", sh("bash", "-c", "tar -cf - -C \"$t\" .devvm-save")...); out != "settings" {
		t.Errorf("agent settings %q", out)
	}
	s.write("shell-fails-on", ".devvm-save\n")
	s.fails(1, "limactl", sh("bash", "-c", "tar -cf - -C \"$t\" .devvm-save")...)
	s.rm("shell-fails-on")
	// A home folder's copy, out and in.
	s.ok("", "limactl", sh("sh", "-c", "cd ~ && tar -czf - --one-file-system .")...)
	if got := s.replaced[len(s.replaced)-1]; !slices.Equal(got, []string{"tar", "-czf", "-", "-C", filepath.Join(s.dir, "vmhome-vm"), "."}) ||
		!slices.Equal(s.extra[len(s.extra)-1], []string{"COPYFILE_DISABLE=1"}) {
		t.Errorf("home out ran %q", got)
	}
	s.ok("tgz", "limactl", sh("sh", "-c", "cd ~ && tar -xzf - --skip-old-files")...)
	if s.read("home-in.tgz") != "tgz" {
		t.Error("home-in.tgz")
	}
	s.write("shell-fails-on", "one-file-system\n")
	s.fails(1, "limactl", sh("sh", "-c", "cd ~ && tar -czf - --one-file-system .")...)
	s.fails(1, "limactl", sh("echo", "one-file-system")...)
	s.ok("", "limactl", sh("echo", "hi")...)
	s.touch("shell-fails")
	s.fails(1, "limactl", sh("echo", "hi")...)
	s.rm("shell-fails")
	s.touch("shell-hangs")
	s.ok("", "limactl", sh("echo", "hi")...)
	if len(s.slept) != 1 {
		t.Errorf("slept %v", s.slept)
	}
}

// A shell waits while shell-waits-on is there, once it's named.
func TestShellWaits(t *testing.T) {
	s := newSandbox(t)
	s.write("shell-waits-on", "slow\n")
	s.onSleep = func() {
		if !exists(filepath.Join(s.dir, "waiting")) {
			t.Error("waiting not touched")
		}
		s.rm("shell-waits-on")
	}
	s.ok("", "limactl", "shell", "vm", "slow")
	if len(s.slept) != 1 {
		t.Errorf("slept %v", s.slept)
	}
}

// A script fed to bash -s is logged by its first line, past devvm's
// prelude.
func TestShellScripts(t *testing.T) {
	s := newSandbox(t)
	prelude := "#!/bin/bash\n# devvm's prelude: x\nf() {\n  :\n}\n\n# the script\nmore\n"
	s.ok(prelude, "limactl", "shell", "vm", "bash", "-s")
	s.ok("#!/bin/bash\n# plain\n", "limactl", "shell", "vm", "sh", "-c", "cd '/' && exec 'bash' '-s'")
	s.ok("#!/bin/bash\n# devvm's prelude, never closed\n", "limactl", "shell", "vm", "bash", "-s")
	if got := s.read("scripts"); got != "# the script\n# plain\n\n" {
		t.Errorf("scripts %q", got)
	}
}

// A --clone VM's git runs on the guest's files; its worktree list gives
// the guest's paths.
func TestCloneGit(t *testing.T) {
	s := newSandbox(t)
	s.ok("", "limactl", "shell", "vm", "sh", "-c", "cd '/' && exec 'git' '-C' '/w' 'status'")
	if got := s.replaced[0]; !slices.Equal(got, []string{"git", "-C", filepath.Join(s.dir, "vmfs/w"), "status"}) {
		t.Errorf("git ran %q", got)
	}
	s.ok("", "limactl", "shell", "vm", "sh", "-c", `cd '/' && exec 'sh' '-c' 'git config receive.denyCurrentBranch x' 'clone' '/w' 'a b'`)
	if got := s.replaced[1]; len(got) != 6 || got[0] != "bash" || !strings.HasSuffix(got[2], "receive.denyCurrentBranch x") ||
		got[4] != filepath.Join(s.dir, "vmfs/w") || got[5] != "a b" {
		t.Errorf("sh ran %q", got)
	}
	s.fails(1, "limactl", "shell", "vm", "cd '/' && exec 'git' '-C' ")
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git here")
	}
	repo := filepath.Join(s.dir, "vmfs", "w")
	if out, err := exec.CommandContext(t.Context(), "git", "init", "-q", repo).CombinedOutput(); err != nil { //nolint:gosec // git init in the test's own folder
		t.Fatalf("git init: %v\n%s", err, out)
	}
	out := s.ok("", "limactl", "shell", "vm", "cd '/' && exec 'git' '-C' '/w' 'worktree' 'list' '--porcelain' '-z'")
	if !strings.HasPrefix(out, "worktree /w\x00") {
		t.Errorf("worktree list %q", out)
	}
	s.touch("shell-fails")
	s.fails(1, "limactl", "shell", "vm", "cd '/' && exec 'git' '-C' '/w' 'status'")
}

// ssh: a link's master and forwards, a port forwarder's master, host
// git's packs, and its failures.
func TestSSH(t *testing.T) {
	s := newSandbox(t)
	ctl := filepath.Join(s.dir, "ctl")
	s.ok("", "ssh", "-F", "none", "-M", "-S", ctl, "-N", "devvm-link")
	s.ok("", "ssh", "-S", ctl, "-O", "forward", "-R", "127.77.0.1:80:/sock", "devvm-link")
	s.write("link-taken", "127.77.0.1:80\n")
	s.fails(255, "ssh", "-S", ctl, "-O", "forward", "-R", "127.77.0.1:80:/sock", "devvm-link")
	s.write("guest-ports", "0100007F:1F90\n")
	if out := s.ok("the script", "ssh", "-F", "none", "-M", "-S", ctl, "-T", "vm"); out != "0100007F:1F90\n.\n" {
		t.Errorf("ports %q", out)
	}
	s.ok("", "ssh", "vm", "git-receive-pack '/w'")
	if got := s.replaced[0]; !slices.Equal(got, []string{"git", "receive-pack", filepath.Join(s.dir, "vmfs/w")}) {
		t.Errorf("pack ran %q", got)
	}
	s.fails(255, "ssh", "vm", "git-upload-pack")
	s.fails(255, "ssh", "vm", "uptime")
	s.fails(255, "ssh", "-M", "-N", "-S", filepath.Join(s.dir, "no", "such", "ctl"))
	s.touch("ssh-fails")
	s.fails(255, "ssh", "vm", "git-receive-pack '/w'")
}

// ssh-agent runs an agent; ssh-add adds, lists and deletes its keys on
// that agent alone; ssh-keygen fingerprints a key file.
func TestAgent(t *testing.T) {
	s := newSandbox(t)
	s.fails(1, "ssh-agent", "-D")
	s.fails(2, "ssh-add", "-l")
	s.ok("", "ssh-agent", "-a", "/sock")
	s.env["SSH_AUTH_SOCK"] = "/other"
	s.fails(2, "ssh-add", "-l")
	s.env["SSH_AUTH_SOCK"] = "/sock"
	if _, out, _ := s.run("", "ssh-add", "-l"); out != "The agent has no identities.\n" {
		t.Errorf("-l %q", out)
	}
	s.write("deploy", "deploy\n")
	s.ok("", "ssh-add", "-t", "12h", filepath.Join(s.dir, "deploy"))
	s.ok("", "ssh-add", filepath.Join(s.dir, "deploy"))
	if out := s.ok("", "ssh-add", "-l"); out != "256 SHA256:fp-deploy deploy (ED25519)\n" {
		t.Errorf("-l %q", out)
	}
	if out := s.ok("", "ssh-add", "-L"); out != "deploy\n" {
		t.Errorf("-L %q", out)
	}
	s.ok("", "ssh-add", "-d", filepath.Join(s.dir, "deploy"))
	if keys := s.state().Agent.Keys; len(keys) != 0 {
		t.Errorf("after -d: %q", keys)
	}
	s.fails(1, "ssh-add", filepath.Join(s.dir, "missing"))
	if got := s.calls(); !slices.Contains(got, "ssh-add -t 12h key:deploy") || !slices.Contains(got, "ssh-add key:deploy") {
		t.Errorf("calls %q", got)
	}
	if out := s.ok("", "ssh-keygen", "-lf", filepath.Join(s.dir, "deploy")); out != "256 SHA256:fp-deploy c (ED25519)\n" {
		t.Errorf("ssh-keygen %q", out)
	}
	s.touch("empty")
	s.fails(1, "ssh-keygen", "-lf", filepath.Join(s.dir, "empty"))
}

// security keeps the Keychain's items in the state, a value never in
// the log.
func TestSecurity(t *testing.T) {
	s := newSandbox(t)
	s.fails(notFound, "security", "find-generic-password", "-s", "devvm-test", "-a", "github", "-w")
	s.ok(`add-generic-password -U -s "devvm-test" -a "github" -l "devvm github" -w "ghp_x"`+"\n", "security", "-i")
	if out := s.ok("", "security", "find-generic-password", "-s", "devvm-test", "-a", "github", "-w"); out != "ghp_x\n" {
		t.Errorf("find %q", out)
	}
	s.ok("", "security", "delete-generic-password", "-s", "devvm-test", "-a", "github")
	s.fails(notFound, "security", "delete-generic-password", "-s", "devvm-test", "-a", "github")
	if code, _, _ := s.run("nonsense\n", "security", "-i"); code != 1 {
		t.Errorf("a bad line: %d", code)
	}
	s.fails(1, "security", "list-keychains")
	if strings.Contains(strings.Join(s.calls(), "\n"), "ghp_x") {
		t.Errorf("the value logged: %q", s.calls())
	}
}

// gone takes left bytes, then fails every write, as a pipe whose reader
// has stopped.
type gone struct{ left int }

func (g *gone) Write(p []byte) (int, error) {
	if len(p) > g.left {
		return 0, io.ErrClosedPipe
	}
	g.left -= len(p)
	return len(p), nil
}

// On its switch, opencode.sh and an export of agents' history write
// stdout and stderr until a write fails, then fail.
func TestShellFloods(t *testing.T) {
	s := newSandbox(t)
	for sw, sh := range map[string]string{
		"opencode-floods": "cd '/' && exec 'bash' '-lc' 'script' 'opencode.sh'",
		"history-floods":  "cd ~ && ls -d .claude .codex 2>/dev/null | tar -czf - .",
	} {
		s.touch(sw)
		for _, out := range [][2]*gone{{{left: 1 << 20}, {left: 1 << 30}}, {{left: 1 << 30}, {left: 1 << 20}}} {
			s.stdout, s.stderr = out[0], out[1]
			if code, _, _ := s.run("", "limactl", "shell", "--workdir", "/", "vm", "sh", "-c", sh); code != 1 {
				t.Errorf("%s: %d", sw, code)
			}
		}
		s.stdout, s.stderr = nil, nil
		if err := os.Remove(filepath.Join(s.dir, sw)); err != nil {
			t.Fatal(err)
		}
	}
}

// A history import's tar: its input kept, and refused as unsafe on its
// switch.
func TestShellHistory(t *testing.T) {
	s := newSandbox(t)
	sh := []string{"shell", "--workdir", "/", "vm", "sh", "-c", "tar -xzf - --quoting-style=escape -C x"}
	s.ok("history", "limactl", sh...)
	if s.read("history-in.tgz") != "history" {
		t.Errorf("history-in.tgz %q", s.read("history-in.tgz"))
	}
	s.touch("history-unsafe")
	if code, out, _ := s.run("history", "limactl", sh...); code != 1 || out != "devvm-history-unsafe\n" {
		t.Errorf("unsafe: %d %q", code, out)
	}
	// devvm-tools-build's word without its bash -s -- isn't the tools'
	// build, and a shell without a VM's name runs in no VM's home.
	if code, _, _ := s.run("", "limactl", "shell", "--workdir", "/", "vm", "echo", "devvm-tools-build"); code != 0 {
		t.Errorf("echo devvm-tools-build: %d", code)
	}
	s.ok("", "limactl", "shell")
}

// opencode: missing without its version; its version and its auth
// export, which fails on its switch; anything else fails.
func TestOpencode(t *testing.T) {
	s := newSandbox(t)
	s.fails(127, "opencode", "--version")
	s.write("opencode-version", "opencode v2.2.5\n")
	if out := s.ok("", "opencode", "--version"); out != "opencode v2.2.5\n" {
		t.Errorf("--version %q", out)
	}
	if out := s.ok("", "opencode", "auth", "export"); out != "[]" {
		t.Errorf("an empty export %q", out)
	}
	s.write("opencode-export", `[{"id":"x"}]`)
	if out := s.ok("", "opencode", "auth", "export"); out != `[{"id":"x"}]` {
		t.Errorf("export %q", out)
	}
	s.touch("opencode-export-fails")
	s.fails(1, "opencode", "auth", "export")
	s.fails(1, "opencode", "run")
}

// docker: down, an image or none, and save's tarball.
func TestDocker(t *testing.T) {
	s := newSandbox(t)
	s.ok("", "docker", "info", "--format", "x")
	s.fails(1, "docker", "image", "inspect", "--format", "{{.Id}}", "debian:12")
	s.fails(2, "docker", "image", "inspect", "debian:12")
	s.write("docker-image", "sha256:abc\n")
	if out := s.ok("", "docker", "image", "inspect", "--format", "{{.Id}}", "debian:12"); out != "sha256:abc\n" {
		t.Errorf("inspect %q", out)
	}
	s.write("docker-saved", "sha256:def\n")
	s.ok("", "docker", "save", "-o", filepath.Join(s.dir, "img.tar"), "debian:12")
	f, err := os.Open(filepath.Join(s.dir, "img.tar"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	r := tar.NewReader(f)
	if h, err := r.Next(); err != nil || h.Name != "manifest.json" {
		t.Fatalf("tar: %v", err)
	}
	if b, _ := io.ReadAll(r); string(b) != `[{"Config":"blobs/sha256/def","RepoTags":["debian:12"],"Layers":[]}]` {
		t.Errorf("manifest %s", b)
	}
	s.touch("docker-down")
	s.fails(1, "docker", "info")
	s.fails(1, "docker", "image", "inspect", "--format", "{{.Id}}", "debian:12")
	s.fails(2, "docker", "ps")
}

// mise: its versions, a lock or an older one, offline.
func TestMise(t *testing.T) {
	s := newSandbox(t)
	if out := s.ok("", "mise", "ls-remote", "node"); out != "\n" {
		t.Errorf("no versions %q", out)
	}
	s.env["MISE_STUB_VERSIONS"] = "22.1.0 24.0.0"
	if out := s.ok("", "mise", "ls-remote", "node"); out != "22.1.0\n24.0.0\n" {
		t.Errorf("versions %q", out)
	}
	s.ok("", "mise", "trust")
	s.write("new.lock", "new")
	s.write("old.lock", "old")
	s.env["MISE_STUB_LOCK"] = filepath.Join(s.dir, "new.lock")
	s.env["MISE_STUB_OLDER_LOCK"] = filepath.Join(s.dir, "old.lock")
	s.ok("", "mise", "lock")
	if s.read("mise.lock") != "new" {
		t.Error("the lock")
	}
	s.write("mise.toml", "node = [\"22\"]\n")
	s.ok("", "mise", "lock")
	if s.read("mise.lock") != "old" {
		t.Error("the older lock")
	}
	s.env["MISE_STUB_LOCK"] = filepath.Join(s.dir, "none")
	s.rm("mise.toml")
	s.fails(1, "mise", "lock")
	s.env["MISE_STUB_FAIL"] = "1"
	s.fails(1, "mise", "lock")
	// A certificate it doesn't trust, in its log file too; the bundle it
	// was pointed at, copied.
	s.env["MISE_STUB_FAIL"] = "untrusted"
	s.env["MISE_LOG_FILE"] = filepath.Join(s.dir, "mise.log")
	s.write("bundle.pem", "roots")
	s.env["MISE_EXTRA_CA_CERTS"] = filepath.Join(s.dir, "bundle.pem")
	s.write("roots.pem", "system")
	s.env["SSL_CERT_FILE"] = filepath.Join(s.dir, "roots.pem")
	s.fails(1, "mise", "lock")
	if !strings.Contains(s.read("mise.log"), "invalid peer certificate: UnknownIssuer") {
		t.Error("no certificate error in its log")
	}
	if s.read("mise-ca.pem") != "roots" || s.read("mise-roots.pem") != "system" {
		t.Error("the bundles")
	}
}

// The host's other stand-ins: open, code, qemu-img, sw_vers, uname and
// tmutil.
func TestHost(t *testing.T) {
	s := newSandbox(t)
	s.ok("", "open", "vnc://x")
	s.touch("open-fails")
	s.fails(1, "open", "vnc://x")
	s.ok("", "code", "--remote", "x")
	if s.read("code-agent") != "unset\n" {
		t.Errorf("code-agent %q", s.read("code-agent"))
	}
	if out := s.ok("", "qemu-img", "info", "-U", "--output=json", "/l/vm/disk"); out != `{"format":"qcow2"}`+"\n" {
		t.Errorf("info %q", out)
	}
	s.write("backing-vm", "/l/img/disk\n")
	if out := s.ok("", "qemu-img", "info", "--output=json", "/l/vm/disk"); out != `{"format":"qcow2","backing-filename":"/l/img/disk"}`+"\n" {
		t.Errorf("info %q", out)
	}
	s.ok("", "qemu-img", "create", "-f", "qcow2", filepath.Join(s.dir, "d", "disk"))
	if !exists(filepath.Join(s.dir, "d", "disk")) {
		t.Error("create")
	}
	s.env["STUB_SW_VERS"] = "15.0"
	s.env["STUB_UNAME_S"] = "Linux"
	if s.ok("", "sw_vers", "-productVersion") != "15.0\n" || s.ok("", "uname", "-s") != "Linux\n" {
		t.Error("the overrides")
	}
	s.ok("", "uname", "-m")
	if got := s.replaced[len(s.replaced)-1]; !strings.HasSuffix(got[0], "/uname") || got[1] != "-m" {
		t.Errorf("uname ran %q", got)
	}
	if code, _, _ := s.run("", "sw_vers"); code != 0 && code != 127 {
		t.Errorf("sw_vers: %d", code)
	}
	if out := s.ok("", "tmutil", "isexcluded", "/d"); out != "[Included]    /d\n" {
		t.Errorf("isexcluded %q", out)
	}
	s.ok("", "tmutil", "addexclusion", "/d")
	if out := s.ok("", "tmutil", "isexcluded", "/d"); out != "[Excluded]    /d\n" {
		t.Errorf("isexcluded %q", out)
	}
	s.ok("", "tmutil", "version")
}

// The stub command reads and changes the state for scripts.
func TestControl(t *testing.T) {
	s := newSandbox(t)
	s.fails(2, "stub")
	s.fails(2, "stub", "vm", "a")
	s.ok("", "stub", "vm", "b", "Running")
	s.ok("", "stub", "vm", "a", "Stopped")
	s.ok("", "stub", "size", "a", "2", "3", "4")
	s.fails(1, "stub", "size", "nope", "2", "3", "4")
	s.fails(1, "stub", "size", "a", "x", "3", "4")
	s.fails(1, "stub", "size", "a", "2", "x", "4")
	s.fails(1, "stub", "size", "a", "2", "3", "x")
	if out := s.ok("", "stub", "vms"); out != "a Stopped\nb Running\n" {
		t.Errorf("vms %q", out)
	}
	if in := s.state().Instances["a"]; in.CPUs != 2 || in.Memory != 3 || in.Disk != 4 {
		t.Errorf("size %+v", in)
	}
	s.fails(1, "stub", "keychain", "github")
	s.ok("ghp_x\n", "stub", "keychain-set", "github")
	if out := s.ok("", "stub", "keychain", "github"); out != "ghp_x\n" {
		t.Errorf("keychain %q", out)
	}
	s.fails(1, "stub", "agent")
	s.ok("", "ssh-agent", "-a", "/sock")
	if out := s.ok("", "stub", "agent"); out != "" {
		t.Errorf("agent %q", out)
	}
	s.ok("", "stub", "agent-stop")
	s.fails(1, "stub", "agent")
	if got := s.calls(); !slices.Equal(got, []string{"ssh-agent -a /sock"}) {
		t.Errorf("logged %q, the stub command's calls among them", got)
	}
}

// A broken state fails each stub that reads it.
func TestBrokenState(t *testing.T) {
	s := newSandbox(t)
	s.write(stubstate.File, "{")
	for _, c := range [][]string{
		{"limactl", "list", "--json"},
		{"limactl", "start", "vm"},
		{"stub", "vms"},
		{"ssh-add", "-l"},
		{"security", "find-generic-password", "-a", "x"},
		{"tmutil", "isexcluded", "/d"},
	} {
		if code, _, _ := s.run("", c[0], c[1:]...); code == 0 {
			t.Errorf("%q succeeded", c)
		}
	}
}

// Words split as a shell's set -- splits them, unexpanded.
func TestShellWords(t *testing.T) {
	for _, c := range []struct {
		line string
		want []string
	}{
		{"", nil},
		{"a  b\tc", []string{"a", "b", "c"}},
		{`'a b' 'it'\''s' ''`, []string{"a b", "it's", ""}},
		{`"a \"b\" \$c \x" d\ e`, []string{`a "b" $c \x`, "d e"}},
		{`git-receive-pack '/w'`, []string{"git-receive-pack", "/w"}},
	} {
		if got := shellWords(c.line); !slices.Equal(got, c.want) {
			t.Errorf("shellWords(%q) = %q, want %q", c.line, got, c.want)
		}
	}
}
