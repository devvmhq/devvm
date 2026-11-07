package vm

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/devvmhq/devvm/assets"
	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/backend/backendtest"
	"github.com/devvmhq/devvm/internal/build"
	"github.com/devvmhq/devvm/internal/daemon"
	"github.com/devvmhq/devvm/internal/gitprotect"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/netpolicy"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/proxy"
	"github.com/devvmhq/devvm/internal/secrets"
	"github.com/devvmhq/devvm/internal/sshagent"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/stubtest"
	"github.com/devvmhq/devvm/internal/tools"
	"github.com/devvmhq/devvm/internal/vmssh"
)

// testUI is devvm's UI for these tests: it prints nothing and fails the
// test on a question, without a terminal (msg.Fake's quiet rest).
func testUI(t *testing.T) *msg.Fake {
	return &msg.Fake{
		OnAsk: unasked(t), OnConfirm: unasked(t),
		OnAnswer: func(m msg.M) string { t.Errorf("asked %s", m.Names()); return "" },
	}
}

// fake is the testUI e prints through and asks with, for a test to
// change.
func fake(e *Env) *msg.Fake { return e.Reporter.(*msg.Fake) }

// testVersion is upEnv's devvm's version.
const testVersion = "0.1.0-test"

// upEnv is up on a fake backend, in a temporary sandbox, with a project
// folder named web.
func upEnv(t *testing.T, caps backend.Caps) (*Env, *backendtest.Fake, *[]backend.Command) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	// The shared tools' versions come from the stub mise: the fixture lock.
	t.Setenv("MISE_STUB_LOCK", filepath.Join(repo, "testdata", "mise.lock"))
	d := paths.Dirs{Data: filepath.Join(root, "data"), Config: filepath.Join(root, "config"), Lima: filepath.Join(root, "lima")}
	tl := tools.New(d, assets.FS, testVersion, testUI(t), platform.Host{})
	tl.FreeSpace = func(string) (uint64, error) { return 1 << 40, nil }
	tl.Mise = stubtest.Command(t, "mise")
	dir := filepath.Join(root, "web")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f := backendtest.New("fake", caps)
	f.ShellCmd = backend.Command{Path: "/bin/shell", Args: []string{"shell", dir}}
	reg, err := backend.NewRegistry(f)
	if err != nil {
		t.Fatal(err)
	}
	reg.NewVMs = "fake"
	var execs []backend.Command
	d.Home = filepath.Join(root, "home")
	e := &Env{
		Dirs: d, Assets: assets.FS, Registry: reg, Tools: tl, Proxy: testProxy(t, d), Dir: dir, OS: "darwin", Version: testVersion,
		Host: func() (Host, error) { return Host{CPUs: 8, MemoryGiB: 32}, nil },
		Protector: func(ctx context.Context, dir string) *gitprotect.Protector {
			p := gitprotect.New(ctx, dir, d.Home, platform.Locker{}, &msg.Fake{})
			p.CanLock = true // as macOS, the Env's OS
			p.Chflags = func(context.Context, string, string) error { return nil }
			return p
		},
		Exec:   func(c backend.Command) error { execs = append(execs, c); return nil },
		Getenv: func(string) string { return "" },
		HostTZ: func() string { return "Europe/Paris" },
		GitConfig: func(_ context.Context, _, key string) string {
			return map[string]string{"user.name": "A User", "user.email": "a@example.com"}[key]
		},
		// The user's own agent: one that holds keys.
		SSHAdd:       func(context.Context, ...string) int { return 0 },
		Reporter:     testUI(t),
		PickFeatures: func(fs []build.Feature) []build.Feature { return fs },
		PortInUse:    func(context.Context, int) bool { return false },
	}
	// The user's agent, as devvm started with it, which tests check stays
	// as it is: each VM's agent travels with its own commands.
	t.Setenv("SSH_AUTH_SOCK", "/user/agent")
	e.Environ = []string{"SSH_AUTH_SOCK=/user/agent"}
	e.Agent = sshagent.New(d, e.mounts, testUI(t))
	e.Agent.Run = (&agentCalls{}).run
	wire(e)
	testForwarders(t, e, f)
	return e, f, &execs
}

// unasked fails the test on a question it doesn't answer itself
// (answers).
func unasked(t *testing.T) func(msg.M) bool {
	return func(m msg.M) bool {
		t.Errorf("asked %s", m.Names())
		return false
	}
}

// agentCalls stands in for devvm's own agent, which runs and holds a key.
type agentCalls struct{ calls []string }

func (a *agentCalls) run(_ context.Context, _ bool, argv ...string) (string, int) {
	a.calls = append(a.calls, strings.Join(argv, " "))
	if len(argv) == 2 && argv[0] == "ssh-add" && argv[1] == "-l" {
		return "256 SHA256:fp k (ED25519)\n", 0
	}
	return "", 0
}

// testProxy runs each VM's "proxy" as a sleep that says it listens, with
// no Keychain and no backups.
func testProxy(t *testing.T, d paths.Dirs) *netpolicy.Proxy {
	t.Helper()
	store := secrets.NewStore(d, &msg.Fake{})
	store.Keychain, store.Getenv = false, func(string) string { return "" }
	p := netpolicy.NewProxy(d, assets.FS, "/devvm", store, testUI(t))
	var mu sync.Mutex
	args := map[int][]string{}
	p.Start = func(c daemon.Cmd) (int, error) {
		path, a := c.Path, c.Args
		cmd := exec.CommandContext(t.Context(), "sleep", "30")
		if err := cmd.Start(); err != nil {
			return 0, err
		}
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		t.Cleanup(func() { _ = cmd.Process.Kill(); <-done })
		mu.Lock()
		defer mu.Unlock()
		args[cmd.Process.Pid] = append([]string{path}, a...)
		// It listens where its config says: _proxy -config FILE.
		cfg, err := proxy.ReadConfig(a[2])
		if err != nil {
			return 0, err
		}
		return cmd.Process.Pid, daemon.WriteStatus(c.Status, cmd.Process.Pid, cfg.Listen)
	}
	p.Args = func(pid int) ([]string, error) {
		mu.Lock()
		defer mu.Unlock()
		return args[pid], nil
	}
	p.Listening = func(context.Context, int) bool { return false }
	p.ExcludeFromBackup = func(context.Context, string) error { return nil }
	p.Tick = time.Millisecond
	return p
}

// testForwarders runs e's VMs' port forwarders as sleeps that say they
// forward, their control sockets in a short folder, and returns each
// start's arguments. Their host keys are the fake's: one each.
func testForwarders(t *testing.T, e *Env, f *backendtest.Fake) *[][]string {
	t.Helper()
	short, err := os.MkdirTemp("/tmp", "dv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(short) })
	var mu sync.Mutex
	starts := &[][]string{}
	args := map[int][]string{}
	fw := vmssh.NewForwards(e.Dirs, e.Registry.Review, "/devvm")
	fw.Run, fw.Tick = short, time.Millisecond
	fw.Detach = func(c daemon.Cmd) (int, error) {
		cmd := exec.CommandContext(t.Context(), "sleep", "30")
		if err := cmd.Start(); err != nil {
			return 0, err
		}
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		t.Cleanup(func() { _ = cmd.Process.Kill(); <-done })
		mu.Lock()
		defer mu.Unlock()
		a := append([]string{c.Path}, c.Args...)
		*starts = append(*starts, a)
		args[cmd.Process.Pid] = a
		return cmd.Process.Pid, daemon.WriteStatus(c.Status, cmd.Process.Pid, "")
	}
	fw.Args = func(pid int) ([]string, error) {
		mu.Lock()
		defer mu.Unlock()
		if a, ok := args[pid]; ok {
			return a, nil
		}
		return nil, errors.New("not ours")
	}
	e.Forwarders = fw
	f.Target = backend.SSHTarget{Host: "127.0.0.1", Port: 60022, User: "u", Key: "/k"}
	f.ExecFunc = func(_ string, argv []string, o backend.ExecOpts) error {
		if len(argv) == 3 && strings.Contains(argv[2], "ssh_host_") && o.Stdout != nil {
			_, err := io.WriteString(o.Stdout, testHostKey+"\n")
			return err
		}
		return nil
	}
	return starts
}

// forwarderRuns reports whether the VM's forwarder, one of
// testForwarders' sleeps, runs: its pid file names a live sleep started
// for the VM.
func forwarderRuns(e *Env, vm string) bool {
	return daemon.Proc{
		PIDFile: e.Dirs.VM(vm).Forwarder(".pid"), Args: e.Forwarders.Args,
		Is: func(args []string) bool { return daemon.HasFlag(args, vmssh.Cmd, "-vm", vm) },
	}.Running()
}

// testHostKey is a made-up host key line, in ssh's wire format.
const testHostKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGRldnZtLXRlc3QtaG9zdC1rZXktYWFhYWFhYWFhYWFh root@vm"

// pinHostKeys pins each VM's host keys for its current start, as a launch
// does, from f's VMs showing testHostKey.
func pinHostKeys(t *testing.T, d paths.Dirs, f *backendtest.Fake, vms ...string) {
	t.Helper()
	was := f.ExecFunc
	defer func() { f.ExecFunc = was }()
	f.ExecFunc = func(_ string, _ []string, o backend.ExecOpts) error {
		_, err := io.WriteString(o.Stdout, testHostKey+"\n")
		return err
	}
	for _, vm := range vms {
		if _, err := vmssh.PinHostKeys(t.Context(), d, f, vm); err != nil {
			t.Fatal(err)
		}
	}
}

// countExecs is how many commands f has run in its VMs.
func countExecs(f *backendtest.Fake) int {
	n := 0
	for _, m := range f.Methods() {
		if m == "Exec" {
			n++
		}
	}
	return n
}

func allNative() backend.Caps {
	caps := backend.Caps{}
	for _, f := range backend.Features {
		caps[f] = backend.Native
	}
	return caps
}

// existingVM makes the folder's VM with up, and returns its state.
func existingVM(t *testing.T, e *Env, o Options) *state.VM {
	t.Helper()
	if err := (Launch{e}).Up(t.Context(), o); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	vm := vmName(t, e)
	t.Cleanup(func() { e.Proxy.Stop(vm) }) // its proxy, if any: the test's sleep
	st, err := state.ReadVM(e.Dirs, vm)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func vmName(t *testing.T, e *Env) string {
	t.Helper()
	names, err := state.VMNames(e.Dirs)
	if err != nil || len(names) == 0 {
		t.Fatalf("no VM state: %v", err)
	}
	return names[0]
}

// answers gives e's questions a terminal, without --no-input, and these
// answers.
func answers(t *testing.T, e *Env, yes bool) *[]string {
	t.Helper()
	var asked []string
	f := fake(e)
	f.Terminal = true
	f.OnConfirm = func(m msg.M) bool { asked = append(asked, m.Names()); return yes }
	f.OnAsk = f.OnConfirm
	return &asked
}

// call returns the fake's last call of a method.
func call(f *backendtest.Fake, method string) backendtest.Call {
	var last backendtest.Call
	for _, c := range f.Calls() {
		if c.Method == method {
			last = c
		}
	}
	return last
}

// staged returns the files the VM's last setup was sent (its Exec's
// input).
func staged(t *testing.T, f *backendtest.Fake) map[string]string {
	t.Helper()
	var c backendtest.Call
	for _, e := range f.Calls() {
		if e.Method != "Exec" {
			continue
		}
		if argv, _ := e.Args[1].([]string); slices.Equal(argv, []string{"bash", "-c", syncCmd}) {
			c = e
		}
	}
	if c.Method == "" {
		t.Fatalf("no setup sent: %q", f.Methods())
	}
	o, _ := c.Args[2].(backend.ExecOpts)
	files := map[string]string{}
	r := tar.NewReader(o.Stdin)
	for {
		h, err := r.Next()
		if errors.Is(err, io.EOF) {
			return files
		} else if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		files[h.Name] = string(data)
	}
}
