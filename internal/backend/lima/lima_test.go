package lima

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/devvmhq/devvm/assets"
	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/daemon"
	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/stubtest"
	"github.com/devvmhq/devvm/internal/ubuntu"
	"github.com/devvmhq/devvm/internal/vmname"
)

// call is a command the fake runner was given.
type call struct {
	argv  []string // the program's base name, then its arguments
	env   []string
	dir   string
	stdin string // what a script fed on its input got
}

func (c call) line() string { return strings.Join(c.argv, " ") }

// fakeRunner runs nothing: it records each command and answers Output from
// out, by command line ("limactl list --json"). Commands whose line starts
// with a key of fail fail.
type fakeRunner struct {
	mu    sync.Mutex
	calls []call
	out   map[string]string
	fail  []string
	do    func(c call) // run for each command
	// stdout is what a command that starts with a key writes to its
	// output, and exit the code one that starts with a key exits with.
	stdout map[string]string
	exit   map[string]int
}

func (f *fakeRunner) record(cmd *exec.Cmd) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	argv := append([]string{filepath.Base(cmd.Path)}, cmd.Args[1:]...)
	c := call{argv: argv, env: cmd.Env, dir: cmd.Dir}
	if cmd.Stdin != nil {
		b, _ := io.ReadAll(cmd.Stdin)
		c.stdin = string(b)
	}
	f.calls = append(f.calls, c)
	if f.do != nil {
		f.do(c)
	}
	for p, out := range f.stdout {
		if strings.HasPrefix(c.line(), p) && cmd.Stdout != nil {
			_, _ = io.WriteString(cmd.Stdout, out)
		}
	}
	for p, code := range f.exit {
		if strings.HasPrefix(c.line(), p) {
			return exec.Command("sh", "-c", "exit "+strconv.Itoa(code)).Run() //nolint:gosec,noctx // an exit code the test sets, from a shell that ends at once
		}
	}
	for _, p := range f.fail {
		if strings.HasPrefix(c.line(), p) {
			return errors.New("failed")
		}
	}
	return nil
}

func (f *fakeRunner) Run(_ context.Context, _ msg.M, cmd *exec.Cmd) error { return f.record(cmd) }

func (f *fakeRunner) Output(cmd *exec.Cmd) ([]byte, error) {
	if err := f.record(cmd); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return []byte(f.out[f.calls[len(f.calls)-1].line()]), nil
}

func (f *fakeRunner) Attach(cmd *exec.Cmd) error { return f.record(cmd) }

// lines returns the calls' command lines, without the read-only ones
// (list, --version, qemu-img info).
func (f *fakeRunner) lines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ls []string
	for _, c := range f.calls {
		if l := c.line(); !strings.HasPrefix(l, "limactl list") && !strings.HasPrefix(l, "qemu-img info") && l != "limactl --version" {
			ls = append(ls, l)
		}
	}
	return ls
}

// sandbox points devvm's folders into a new folder under /tmp (short, for
// Lima's socket paths), with the stubs first on PATH.
func sandbox(t *testing.T) (paths.Dirs, *stubtest.Stubs) {
	t.Helper()
	stubs := stubtest.New(t)
	return sandboxDirs(t), stubs
}

// sandboxDirs is sandbox without the stubs: HOME, devvm's folders and its
// Lima home in a new folder under /tmp.
func sandboxDirs(t *testing.T) paths.Dirs {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "dvl.")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"home", "data", "config"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("DEVVM_LIMA_HOME", filepath.Join(root, "lima"))
	t.Setenv("LIMA_HOME", "/elsewhere") // never devvm's
	d, err := paths.FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// testLima returns the backend on a sandbox, as on host os, with the fake
// runner; warnings go to warned.
func testLima(t *testing.T, goos string) (*Lima, *fakeRunner, *msg.Recorder) {
	t.Helper()
	l, f, warned, _ := limaOn(t, goos)
	return l, f, warned
}

// limaOn is testLima, with the sandbox's stubs.
func limaOn(t *testing.T, goos string) (*Lima, *fakeRunner, *msg.Recorder, *stubtest.Stubs) {
	t.Helper()
	d, stubs := sandbox(t)
	l := New(d, assets.FS, &msg.Fake{}, platform.Host{}, "", nil)
	f := &fakeRunner{out: map[string]string{}}
	warned := &msg.Recorder{}
	l.Runner, l.OS, l.Arch, l.Self = f, goos, "arm64", "/opt/my devvm/devvm"
	l.Getenv = func(string) string { return "" }
	l.Spawn = func(daemon.Cmd) (*daemon.Handle, error) { return nil, errors.New("no build proxy here") }
	l.Tick = time.Millisecond
	l.MacOS = func() string { return "26.0" }
	l.KVM = filepath.Join(t.TempDir(), "kvm")
	if err := os.WriteFile(l.KVM, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	l.Reporter = &msg.Fake{OnWarn: warned.Add}
	l.Ubuntu = &testFinder{}
	pinUbuntu(t, l, testUbuntuURL, testUbuntuSum)
	return l, f, warned, stubs
}

// testFinder stands in for ubuntu.Finder: releases, newest last, each with
// an image for every architecture, or err; it records what it's asked.
type testFinder struct {
	releases []string // none: release-20260101 alone
	err      error
	asked    []string // "newest <before>" or "release <name>"
}

func (f *testFinder) pin(rel string) state.UbuntuPin {
	return state.UbuntuPin{Release: rel, File: ubuntu.File("arm64"), Digest: "sha256:" + strings.Repeat("e", 64), Pinned: time.Now().UTC()}
}

func (f *testFinder) Newest(_ context.Context, before string) (state.UbuntuPin, error) {
	f.asked = append(f.asked, "newest "+before)
	if f.err != nil {
		return state.UbuntuPin{}, f.err
	}
	rels := f.releases
	if rels == nil {
		rels = []string{"release-20260101"}
	}
	for i := len(rels) - 1; i >= 0; i-- {
		if before == "" || rels[i] < before {
			return f.pin(rels[i]), nil
		}
	}
	return state.UbuntuPin{}, msg.NoUbuntuRelease("x", ubuntu.Version)
}

func (f *testFinder) Release(_ context.Context, rel string) (state.UbuntuPin, error) {
	f.asked = append(f.asked, "release "+rel)
	if f.err != nil {
		return state.UbuntuPin{}, f.err
	}
	return f.pin(rel), nil
}

// The cloud image test Limas have pinned, at a URL nothing answers, and
// its SHA-256.
const (
	testUbuntuURL = "https://example.com/r"
	testUbuntuSum = "1111111111111111111111111111111111111111111111111111111111111111"
)

// pinUbuntu pins a cloud image for l's architecture, at url, with sum as
// its SHA-256, and returns it.
func pinUbuntu(t *testing.T, l *Lima, url, sum string) ubuntu.Image {
	t.Helper()
	l.UbuntuURL = url
	p := state.UbuntuPin{Release: "release-20260101", File: ubuntu.File(l.Arch), Digest: "sha256:" + sum, Pinned: time.Now().UTC()}
	if err := state.WriteUbuntuPin(l.Dirs, p); err != nil {
		t.Fatal(err)
	}
	return ubuntu.ImageOf(url, p)
}

// stubLima is testLima with the real runner, so limactl is the stub.
func stubLima(t *testing.T) (*Lima, *msg.Recorder, *stubtest.Stubs) {
	t.Helper()
	l, _, warned, stubs := limaOn(t, "darwin")
	l.Runner = stubtest.ExecRunner{}
	return l, warned, stubs
}

// An image's instance is devvm-img-<key>; a project VM's from a folder
// named img-… isn't one, so gc never takes it for an image.
func TestImageKey(t *testing.T) {
	for _, c := range []struct {
		name, key string
		ok        bool
	}{
		{"devvm-img-0123456789ab", "0123456789ab", true},
		{"devvm-img-x", "x", true},
		{"devvm-img-keep-0123456789abcd", "keep-0123456789abcd", false},
		{"devvm-img-", "", false},
		{"devvm-web-0123456789abcd", "", false},
		{"other", "", false},
	} {
		if key, ok := imageKey(c.name); ok != c.ok || ok && key != c.key {
			t.Errorf("imageKey(%q) = %q, %v", c.name, key, ok)
		}
	}
}

// Lima runs on devvm's own Lima home (DEVVM_LIMA_HOME, never LIMA_HOME),
// from devvm's data folder, never the caller's: Lima writes
// lima.REJECTED.yaml into the folder it runs in.
// On Linux hosts an image's and a tools build's disks start as full
// copies of their parent's, so those floors are higher; a VM's is an
// overlay, so its floor is the same.
func TestFloors(t *testing.T) {
	mac, _, _ := testLima(t, "darwin")
	linux, _, _ := testLima(t, "linux")
	m, l := mac.Floors(), linux.Floors()
	if l.Image <= m.Image || l.Tools <= m.Tools || l.VM != m.VM || l.Base != m.Base || l.PerTool != m.PerTool {
		t.Errorf("macOS %+v, Linux %+v", m, l)
	}
}

func TestLimactlUsesDevvmsHomeAndFolder(t *testing.T) {
	l, _, stubs := stubLima(t)
	t.Chdir(t.TempDir())
	if _, err := l.List(t.Context()); err != nil {
		t.Fatal(err)
	}
	want := "cwd=" + l.Dirs.Data + " LIMA_HOME=" + os.Getenv("DEVVM_LIMA_HOME") + "\n"
	if got := stubs.Read(t, "limactl-env"); got != want {
		t.Errorf("limactl ran with %q, want %q", got, want)
	}
}

func TestSetEnv(t *testing.T) {
	got := setEnv([]string{"A=1", "B=2", "AB=3"}, "A=9", "B", "C=x=y")
	if want := []string{"AB=3", "A=9", "C=x=y"}; !slices.Equal(got, want) {
		t.Errorf("setEnv = %q, want %q", got, want)
	}
}

// Lima's global configs in devvm's Lima home would reach every VM, and a
// network devvm didn't give a VM would reach around its proxy: no start.
func TestStartRefusesGlobalConfigsAndNetworks(t *testing.T) {
	l, f, _ := testLima(t, "darwin")
	f.out["limactl list --json"] = `{"name":"devvm-img-x","status":"Stopped","config":{"networks":[{"vzNAT":true}]}}`
	cfg := filepath.Join(l.Dirs.Lima, "_config")
	if err := os.MkdirAll(cfg, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "default.yaml"), []byte("networks: [{vzNAT: true}]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := l.start(t.Context(), "devvm-img-x", nil, backend.Confinement{Net: backend.NetInternet})
	if msg.IDOf(err) != "lima_global_config" || !strings.Contains(msg.NamesOf(err), "files="+filepath.Join(cfg, "default.yaml")) {
		t.Errorf("with default.yaml: %v", err)
	}
	f.out["limactl --version"] = "limactl version 2.2.1\n"
	if err := backend.Check(t.Context(), l); msg.IDOf(err) != "lima_global_config" {
		t.Errorf("Check with default.yaml: %v", err)
	}
	if err := os.Remove(filepath.Join(cfg, "default.yaml")); err != nil {
		t.Fatal(err)
	}
	err = l.start(t.Context(), "devvm-img-x", nil, backend.Confinement{Net: backend.NetInternet})
	if want := "vm_has_networks file=" + filepath.Join(l.Dirs.Lima, "devvm-img-x", "lima.yaml") + " vm=devvm-img-x"; err == nil || msg.NamesOf(err) != want {
		t.Errorf("with a network: %v, want %s", err, want)
	}
	if got := f.lines(); len(got) > 0 {
		t.Errorf("ran %q", got)
	}
}

// Lima keeps a VM's ssh socket in its folder, OpenSSH adds 17 characters
// while making it, and Lima refuses a socket path of paths.SocketMax
// characters or more: the name gets what's left.
func TestNameMax(t *testing.T) {
	l, f, _ := testLima(t, "darwin")
	n := l.NameMax()
	if sock := len(l.Dirs.Lima) + 1 + n + len("/ssh.sock") + 17; sock != paths.SocketMax-1 {
		t.Errorf("NameMax %d gives a socket path of %d characters for a Lima home of %d", n, sock, len(l.Dirs.Lima))
	}
	long := "devvm-" + strings.Repeat("x", n) + "-0123456789abcd"
	err := l.CreateVM(t.Context(), "k", backend.VMSpec{Name: long})
	if msg.IDOf(err) != "lima_home_too_long" {
		t.Errorf("a name too long: %v", err)
	}
	if len(f.calls) > 0 {
		t.Errorf("ran %q", f.lines())
	}
	l.Dirs.Lima = "/" + strings.Repeat("x", 80)
	if l.NameMax() != 0 {
		t.Errorf("NameMax for a long home: %d", l.NameMax())
	}
}

// Everything is native but ports, which devvm's forwarder forwards: an
// emulation, reviewed.
func TestCaps(t *testing.T) {
	l, _, _ := testLima(t, "darwin")
	if err := l.Caps().Check(); err != nil {
		t.Error(err)
	}
	for _, f := range backend.Features {
		c := l.Caps()[f]
		if f == backend.FeaturePorts || f == backend.FeaturePortsAll {
			if c != backend.Emulated(true) {
				t.Errorf("%s: %+v, want emulated and reviewed", f, c)
			}
		} else if !c.IsNative() {
			t.Errorf("%s isn't native", f)
		}
	}
}

// stepNames are steps' names, in order.
func stepNames(steps []image.Step) []string {
	names := make([]string, len(steps))
	for i, s := range steps {
		names[i] = s.Name
	}
	return names
}

// Base's origin is the Ubuntu cloud image pinned on this machine, for the
// host ("" before one is, or when the one pinned is another
// architecture's), base's disk and base.yaml; with --from, the conversion's scripts in
// between, build.sh as root with the user it makes the image's, and none
// of oci/'s other files. Without the user, or devvm's files, there's
// none.
func TestOrigin(t *testing.T) {
	l, _, _ := testLima(t, "darwin")
	l.User = func() (*user.User, error) { return &user.User{Username: "me", Uid: "501"}, nil }
	f := &backend.From{Ref: "debian:12", Digest: "debian@sha256:aaaa"}
	want := "release-20260101 ubuntu-26.04-server-cloudimg-arm64.img sha256:" + testUbuntuSum
	for arch, pin := range map[string]string{"arm64": want, "amd64": ""} {
		l.Arch = arch
		steps, err := l.Origin(nil)
		if err != nil || !slices.Equal(stepNames(steps), []string{originUbuntu, originDisk, originConfig}) ||
			!slices.Equal(steps[0].Args, []string{pin}) || !slices.Equal(steps[1].Args, []string{"100"}) {
			t.Fatalf("%s: Origin = %q, %v", arch, stepNames(steps), err)
		}
		if yaml, _ := fs.ReadFile(assets.FS, "base.yaml"); !bytes.Equal(steps[2].Data, yaml) {
			t.Errorf("%s: base.yaml isn't devvm's", arch)
		}
	}
	l.Arch = "arm64"
	if err := os.Remove(l.Dirs.UbuntuPin()); err != nil {
		t.Fatal(err)
	}
	if steps, err := l.Origin(nil); err != nil || steps[0].Args[0] != "" {
		t.Errorf("nothing pinned: %+v, %v", steps, err)
	}
	if err := os.WriteFile(l.Dirs.UbuntuPin(), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Origin(nil); err == nil {
		t.Error("a broken pin: no error")
	}
	pinUbuntu(t, l, testUbuntuURL, testUbuntuSum)
	steps, err := l.Origin(f)
	if want := slices.Concat([]string{originUbuntu}, conversionScripts, []string{originDisk, originConfig}); err != nil || !slices.Equal(stepNames(steps), want) {
		t.Fatalf("Origin(--from) = %q, %v; want %q", stepNames(steps), err, want)
	}
	build := steps[1]
	if want := []string{f.Digest, f.Ref, "me", "501", "/home/me.guest", convertedSize}; !build.Root || !slices.Equal(build.Args, want) || len(want) != convertArgs {
		t.Errorf("build.sh: root %v, %q; want %q", build.Root, build.Args, want)
	}
	if _, err := l.originOf(steps, f); err != nil {
		t.Errorf("originOf(Origin(f)) = %v", err)
	}
	l.User = func() (*user.User, error) { return nil, errors.New("no user") }
	if _, err := l.Origin(f); err == nil {
		t.Error("Origin without a user: no error")
	}
	if _, err := l.Origin(nil); err != nil {
		t.Errorf("Ubuntu's base needs no user: %v", err)
	}
	for _, own := range []fstest.MapFS{{}, {"base.yaml": {Data: []byte("x")}}} {
		l.Assets = own
		if _, err := l.Origin(f); err == nil {
			t.Errorf("Origin without devvm's files %v: no error", slices.Collect(maps.Keys(own)))
		}
	}
}

// BuildImage runs only the origin it's given, and refuses one Origin
// wouldn't make: another Ubuntu, no base.yaml, a conversion that isn't
// what --from asks for or names other files.
func TestOriginOfRefuses(t *testing.T) {
	l, _, _ := testLima(t, "darwin")
	l.User = func() (*user.User, error) { return &user.User{Username: "me", Uid: "501"}, nil }
	f := &backend.From{Ref: "debian:12", Digest: "debian@sha256:aaaa"}
	good, err := l.Origin(f)
	if err != nil {
		t.Fatal(err)
	}
	ubuntuOnly, err := l.Origin(nil)
	if err != nil {
		t.Fatal(err)
	}
	change := func(fn func(steps []image.Step) []image.Step) []image.Step {
		steps := slices.Clone(good)
		steps[1].Args = slices.Clone(steps[1].Args)
		return fn(steps)
	}
	for name, c := range map[string]struct {
		steps []image.Step
		from  *backend.From
	}{
		"another Ubuntu": {change(func(s []image.Step) []image.Step { s[0].Args = []string{"x"}; return s }), f},
		"no base.yaml":   {change(func(s []image.Step) []image.Step { return s[:len(s)-1] }), f},
		"no disk": {change(func(s []image.Step) []image.Step {
			return slices.DeleteFunc(s, func(st image.Step) bool { return st.Name == originDisk })
		}), f},
		"a disk that isn't a size": {change(func(s []image.Step) []image.Step {
			s[len(s)-2] = image.Step{Name: originDisk, Args: []string{"big"}}
			return s
		}), f},
		"a conversion without one":  {good, nil},
		"no conversion with --from": {ubuntuOnly, f},
		"another digest":            {change(func(s []image.Step) []image.Step { s[1].Args[0] = "debian@sha256:bbbb"; return s }), f},
		"a script of its own":       {change(func(s []image.Step) []image.Step { s[2].Name = "oci/../../x"; return s }), f},
		"another script's arguments": {change(func(s []image.Step) []image.Step {
			s[1].Args = s[1].Args[:2]
			return s
		}), f},
	} {
		if _, err := l.originOf(c.steps, c.from); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

const listed = `{"name":"devvm-img-0123456789ab","status":"Stopped","cpus":4,"memory":8589934592,"disk":107374182400}
{"name":"devvm-img-x-0123456789abcd","status":"Running","cpus":2,"memory":4294967296,"disk":107374182400,"config":{"mounts":[{"location":"/p","writable":true},{"location":"/h/.claude","mountPoint":"/h/.claude","writable":false},{"location":"/d/tools","mountPoint":"/mnt/devvm-tools","writable":false}],"ssh":{"forwardAgent":true}}}
{"name":"devvm-tools-build","status":"Stopped"}
`

// Images are devvm-img-<key>; a project VM for a folder named img-… is
// still a project VM.
func TestListAndImages(t *testing.T) {
	l, f, _ := testLima(t, "darwin")
	f.out["limactl list --json"] = listed
	keys, err := l.Images(t.Context())
	if err != nil || !slices.Equal(keys, []string{"0123456789ab"}) {
		t.Errorf("Images: %q, %v", keys, err)
	}
	ins, err := l.List(t.Context())
	if err != nil || len(ins) != 2 || ins[0].Name != "devvm-img-x-0123456789abcd" || ins[1].Name != "devvm-tools-build" {
		t.Fatalf("List: %+v, %v", ins, err)
	}
	want := backend.Instance{
		Name: "devvm-img-x-0123456789abcd", Status: backend.StatusRunning,
		Size:      backend.Size{CPUs: 2, MemoryGiB: 4, DiskGiB: 100},
		AgentDirs: []string{"/h/.claude"}, AgentForwarding: true,
	}
	if got := ins[0]; got.Name != want.Name || got.Status != want.Status || got.Size != want.Size ||
		!slices.Equal(got.AgentDirs, want.AgentDirs) || got.AgentForwarding != want.AgentForwarding {
		t.Errorf("Inspect: %+v, want %+v", got, want)
	}
	if _, err := l.Inspect(t.Context(), "nope"); !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("Inspect nope: %v", err)
	}
	if err := l.Stop(t.Context(), "nope"); !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("Stop nope: %v", err)
	}
}

func TestDeleteImage(t *testing.T) {
	l, f, _ := testLima(t, "darwin")
	f.out["limactl list --json"] = listed
	if err := l.DeleteImage(t.Context(), "x-0123456789abcd"); msg.IDOf(err) != "not_an_image" {
		t.Errorf("a project VM: %v", err)
	}
	if err := l.DeleteImage(t.Context(), "missing0000"); err != nil {
		t.Errorf("a missing image: %v", err)
	}
	if err := l.DeleteImage(t.Context(), "0123456789ab"); err != nil {
		t.Fatal(err)
	}
	if got, want := f.lines(), []string{"limactl delete -f devvm-img-0123456789ab"}; !slices.Equal(got, want) {
		t.Errorf("ran %q, want %q", got, want)
	}
}

func TestImageSize(t *testing.T) {
	l, _, _ := testLima(t, "darwin")
	if _, err := l.ImageSize(t.Context(), "x-0123456789abcd"); msg.IDOf(err) != "not_an_image" {
		t.Errorf("a project VM: %v", err)
	}
	if _, err := l.ImageSize(t.Context(), "0123456789ab"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a missing image: %v", err)
	}
	d := l.Dirs.LimaVM("devvm-img-0123456789ab")
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.disk("devvm-img-0123456789ab"), make([]byte, 64<<10), 0o600); err != nil {
		t.Fatal(err)
	}
	if n, err := l.ImageSize(t.Context(), "0123456789ab"); err != nil || n < 64<<10 {
		t.Errorf("ImageSize = %d, %v; want at least its disk's 64 KiB", n, err)
	}
}

// Linux hosts: an image whose disk project VMs are overlays of is never
// deleted. A running VM's QEMU holds its disk's write lock, which refuses
// qemu-img info unless it shares the disk (-U).
func TestDeleteImageInUse(t *testing.T) {
	l, f, _ := testLima(t, "linux")
	f.out["limactl list --json"] = listed
	f.fail = []string{"qemu-img info --output=json"}
	vm := "devvm-img-x-0123456789abcd"
	for _, d := range []string{"devvm-img-0123456789ab", vm} {
		if err := os.MkdirAll(l.Dirs.LimaVM(d), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(l.disk(d), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f.out["qemu-img info -U --output=json "+l.disk(vm)] = `{"format":"qcow2","backing-filename":"` + l.disk("devvm-img-0123456789ab") + `"}`
	err := l.DeleteImage(t.Context(), "0123456789ab")
	if err == nil || msg.NamesOf(err) != "image_in_use key=0123456789ab vms="+vm {
		t.Errorf("DeleteImage: %v", err)
	}
	if got := f.lines(); len(got) > 0 {
		t.Errorf("ran %q", got)
	}
}

func TestCreateVM(t *testing.T) {
	l, f, _ := testLima(t, "darwin")
	f.out["limactl list --json"] = `{"name":"devvm-img-0123456789ab","status":"Running","cpus":4,"memory":8589934592,"disk":107374182400}
{"name":"devvm-p-0123456789abcd","status":"Stopped","cpus":4,"memory":8589934592,"disk":107374182400,"config":{"mounts":[` +
		`{"location":"/my project","mountPoint":"/my project","writable":true},{"location":"/h/.claude","mountPoint":"/h/.claude"},` +
		`{"location":"/d/tools","mountPoint":"/mnt/devvm-tools"}],"ssh":{"forwardAgent":true}}}`
	err := l.CreateVM(t.Context(), "0123456789ab", backend.VMSpec{
		Name:  "devvm-p-0123456789abcd",
		Tools: "/d/tools", ToolsAt: "/mnt/devvm-tools", SSHPort: 23456,
		Want: backend.Want{
			Project:   "/my project",
			AgentDirs: []string{"/h/.claude"}, AgentForwarding: true, Size: backend.Size{CPUs: 6, MemoryGiB: 8, DiskGiB: 150},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The clone has its mounts and forwarding; only its size is edited.
	want := []string{
		"limactl stop devvm-img-0123456789ab",
		`limactl clone --tty=false devvm-img-0123456789ab devvm-p-0123456789abcd --mount-none` +
			` --set .mounts = [{"location":"/my project","writable":true},{"location":"/h/.claude","writable":false},{"location":"/d/tools","mountPoint":"/mnt/devvm-tools","writable":false}]` +
			` --set .ssh.forwardAgent=true --set .ssh.localPort=23456 --set ` + noForwardsSet() + ` --set .propagateProxyEnv=true`,
		"limactl edit --tty=false devvm-p-0123456789abcd --cpus 6 --disk 150",
	}
	if got := f.lines(); !slices.Equal(got, want) {
		t.Errorf("ran\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// A disk smaller than its image's is refused.
	err = l.CreateVM(t.Context(), "0123456789ab", backend.VMSpec{Name: "devvm-p-0123456789abcd", Want: backend.Want{Size: backend.Size{DiskGiB: 50}}})
	if msg.NamesOf(err) != "disk_shrink have=100 vm=devvm-p-0123456789abcd want=50" {
		t.Errorf("a smaller disk: %v", err)
	}
	err = l.CreateVM(t.Context(), "0123456789ab", backend.VMSpec{Name: "devvm-q-0123456789abcd", Project: "/a,b"})
	if err == nil || msg.NamesOf(err) != "mount_comma path=/a,b" {
		t.Errorf("a comma: %v", err)
	}
	if err := l.CreateVM(t.Context(), "fedcba987654", backend.VMSpec{Name: "devvm-q-0123456789abcd"}); msg.IDOf(err) != "image_missing" {
		t.Errorf("no image: %v", err)
	}
	f.fail = []string{"limactl clone"}
	err = l.CreateVM(t.Context(), "0123456789ab", backend.VMSpec{Name: "devvm-q-0123456789abcd"})
	if le, ok := errors.AsType[*backend.LoggedError](err); !ok || le.Msg.ID() != "vm_create_failed" {
		t.Errorf("a failed clone: %v", err)
	}
}

// Linux hosts: a project VM's disk becomes an overlay of its image's,
// owner-only. macOS has none: APFS clones already share.
func TestOverlay(t *testing.T) {
	for _, host := range []string{"linux", "darwin"} {
		l, f, _ := testLima(t, host)
		img, vm := "devvm-img-0123456789ab", "devvm-p-0123456789abcd"
		f.out["limactl list --json"] = `{"name":"` + img + `","status":"Stopped"}` + "\n" + `{"name":"` + vm + `","status":"Stopped"}`
		for _, d := range []string{img, vm} {
			if err := os.MkdirAll(l.Dirs.LimaVM(d), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(l.disk(d), []byte("disk"), 0o644); err != nil { //nolint:gosec // a test's
				t.Fatal(err)
			}
		}
		f.out["qemu-img info -U --output=json "+l.disk(vm)] = `{"format":"qcow2"}`
		f.do = func(c call) {
			if c.argv[0] == "qemu-img" && c.argv[1] == "create" {
				if err := os.WriteFile(c.argv[len(c.argv)-1], []byte("overlay"), 0o644); err != nil { //nolint:gosec // as qemu-img would
					t.Error(err)
				}
			}
		}
		if err := l.CreateVM(t.Context(), "0123456789ab", backend.VMSpec{Name: vm}); err != nil {
			t.Fatal(err)
		}
		created := slices.Contains(f.lines(), "qemu-img create -q -f qcow2 -b "+l.disk(img)+" -F qcow2 "+l.disk(vm))
		if host == "linux" {
			if !created {
				t.Errorf("no overlay: %q", f.lines())
			}
			// The copy is gone; the overlay may hold tokens: owner only.
			b, err := os.ReadFile(l.disk(vm))
			if err != nil || string(b) != "overlay" {
				t.Errorf("the VM's disk: %q, %v", b, err)
			}
			if fi, err := os.Stat(l.disk(vm)); err != nil || fi.Mode().Perm() != 0o600 {
				t.Errorf("the overlay's mode: %v, %v", fi.Mode(), err)
			}
		} else if created || slices.ContainsFunc(f.calls, func(c call) bool { return c.argv[0] == "qemu-img" }) {
			t.Errorf("macOS ran qemu-img: %q", f.lines())
		}
	}
}

// A start edits what of a VM's Want differs, and only that: its size
// (a disk only grows, a smaller one refused), its agent folders, keeping
// its other mounts, and its agent forwarding.
func TestWantEdits(t *testing.T) {
	var in instance
	if err := json.Unmarshal([]byte(`{"name":"v","status":"Stopped","cpus":4,"memory":8589934592,"disk":107374182400,
		"config":{"mounts":[{"location":"/p","writable":true},{"location":"/h/.claude","mountPoint":"/h/.claude"},{"location":"/d/tools","mountPoint":"/mnt/devvm-tools"}],"ssh":{"forwardAgent":false}}}`), &in); err != nil {
		t.Fatal(err)
	}
	claude := []string{"/h/.claude"}
	for _, tc := range []struct {
		want backend.Want
		opts string // the edit's options, or the error
	}{
		{backend.Want{Project: "/p", AgentDirs: claude}, ""},
		{backend.Want{Project: "/p", AgentDirs: claude, Size: backend.Size{CPUs: 4, MemoryGiB: 8, DiskGiB: 100}}, ""},
		{backend.Want{Project: "/p", AgentDirs: claude, Size: backend.Size{CPUs: 6, MemoryGiB: 8, DiskGiB: 150}}, "--cpus 6 --disk 150"},
		{backend.Want{Project: "/p", AgentDirs: claude, Size: backend.Size{MemoryGiB: 3.5}}, "--memory 3.5"},
		{backend.Want{Project: "/p", AgentDirs: claude, Size: backend.Size{DiskGiB: 50}}, "disk_shrink have=100 vm=v want=50"},
		{
			backend.Want{Project: "/p", AgentDirs: []string{"/h/.codex"}},
			`--set .mounts = [{"location":"/p","writable":true},{"location":"/d/tools","mountPoint":"/mnt/devvm-tools"},{"location":"/h/.codex","writable":false}]`,
		},
		{backend.Want{Project: "/p"}, `--set .mounts = [{"location":"/p","writable":true},{"location":"/d/tools","mountPoint":"/mnt/devvm-tools"}]`},
		{backend.Want{Project: "/p", AgentDirs: []string{"/h/a,b"}}, "mount_comma path=/h/a,b"},
		{backend.Want{Project: "/p", AgentDirs: claude, AgentForwarding: true}, "--set .ssh.forwardAgent=true"},
		{
			backend.Want{Project: "/p", AgentForwarding: true, Size: backend.Size{CPUs: 2}},
			`--cpus 2 --set .mounts = [{"location":"/p","writable":true},{"location":"/d/tools","mountPoint":"/mnt/devvm-tools"}] --set .ssh.forwardAgent=true`,
		},
		// The project folder moved: its mount alone changes, in the
		// same --set as the rest, the project's first.
		{
			backend.Want{Project: "/q", AgentDirs: claude},
			`--set .mounts = [{"location":"/q","writable":true},{"location":"/d/tools","mountPoint":"/mnt/devvm-tools"},{"location":"/h/.claude","writable":false}]`,
		},
		{backend.Want{Project: "/q,r", AgentDirs: claude}, "mount_comma path=/q,r"},
	} {
		opts, err := wantEdits(in, tc.want)
		got := strings.Join(opts, " ")
		if err != nil {
			got = msg.NamesOf(err)
		}
		if got != tc.opts {
			t.Errorf("wantEdits(%+v): %q, want %q", tc.want, got, tc.opts)
		}
	}
}

// A vz image's clone mounts the project folder by its device and inode,
// which its first start keeps: nothing but its size is edited.
func TestCreateVMByInode(t *testing.T) {
	l, f, _ := testLima(t, "darwin")
	f.out["limactl list --json"] = `{"name":"devvm-img-0123456789ab","status":"Stopped","vmType":"vz","cpus":4,"memory":8589934592,"disk":107374182400}
{"name":"devvm-p-0123456789abcd","status":"Stopped","vmType":"vz","cpus":4,"memory":8589934592,"disk":107374182400,"config":{"mounts":[` +
		`{"location":"/.vol/16777232/4242","mountPoint":"/my project","writable":true,"9p":{"cache":"mmap"}}]}}`
	err := l.CreateVM(t.Context(), "0123456789ab", backend.VMSpec{
		Name: "devvm-p-0123456789abcd", SSHPort: 23456,
		Want: backend.Want{Project: "/my project", ProjectID: "16777232:4242:1.2", Size: backend.Size{CPUs: 6}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		`limactl clone --tty=false devvm-img-0123456789ab devvm-p-0123456789abcd --mount-none` +
			` --set .mounts = [{"location":"/.vol/16777232/4242","mountPoint":"/my project","writable":true}]` +
			` --set .ssh.forwardAgent=false --set .ssh.localPort=23456 --set ` + noForwardsSet() + ` --set .propagateProxyEnv=true`,
		"limactl edit --tty=false devvm-p-0123456789abcd --cpus 6",
	}
	if got := f.lines(); !slices.Equal(got, want) {
		t.Errorf("ran\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// On vz the project folder is mounted by its device and inode (/.vol),
// at its path in the VM, and a start edits it only when the folder
// recorded or its path differs, whatever limactl list fills in; QEMU's
// is by its path, as is one without a recorded folder.
func TestProjectMountByInode(t *testing.T) {
	vol := `{"location":"/.vol/16777232/4242","mountPoint":"/p","writable":true,"sshfs":{"cache":true},"9p":{"cache":"mmap"}}`
	tools := `{"location":"/d/tools","mountPoint":"/mnt/devvm-tools"}`
	vm := func(vmType, project string) instance {
		t.Helper()
		var in instance
		if err := json.Unmarshal([]byte(`{"name":"v","status":"Stopped","vmType":"`+vmType+`","cpus":4,"memory":8589934592,"disk":107374182400,
			"config":{"mounts":[`+project+`,`+tools+`]}}`), &in); err != nil {
			t.Fatal(err)
		}
		return in
	}
	id := "16777232:4242:1700000000.000000001"
	for _, tc := range []struct {
		name string
		in   instance
		want backend.Want
		opts string // the edit's options, or the error
	}{
		{"vz, as it is", vm("vz", vol), backend.Want{Project: "/p", ProjectID: id}, ""},
		{
			"vz, another folder at its path", vm("vz", vol),
			backend.Want{Project: "/p", ProjectID: "16777232:99:1.2"},
			`--set .mounts = [{"location":"/.vol/16777232/99","mountPoint":"/p","writable":true},` + tools + `]`,
		},
		{
			"vz, the folder moved", vm("vz", vol),
			backend.Want{Project: "/q", ProjectID: id},
			`--set .mounts = [{"location":"/.vol/16777232/4242","mountPoint":"/q","writable":true},` + tools + `]`,
		},
		{"vz, moved to a path with a comma", vm("vz", vol), backend.Want{Project: "/q,r", ProjectID: id}, "mount_comma path=/q,r"},
		{
			"vz, by its path before", vm("vz", `{"location":"/p","mountPoint":"/p","writable":true}`),
			backend.Want{Project: "/p", ProjectID: id},
			`--set .mounts = [{"location":"/.vol/16777232/4242","mountPoint":"/p","writable":true},` + tools + `]`,
		},
		{
			"vz, no folder recorded", vm("vz", vol),
			backend.Want{Project: "/p"},
			`--set .mounts = [{"location":"/p","writable":true},` + tools + `]`,
		},
		{"vz, --clone", vm("vz", vol), backend.Want{}, `--set .mounts = [` + tools + `]`},
		{"qemu, by its path", vm("qemu", `{"location":"/p","mountPoint":"/p","writable":true}`), backend.Want{Project: "/p", ProjectID: id}, ""},
	} {
		opts, err := wantEdits(tc.in, tc.want)
		got := strings.Join(opts, " ")
		if err != nil {
			got = msg.NamesOf(err)
		}
		if got != tc.opts {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.opts)
		}
	}
	if p := vm("vz", vol).backend().Project; p != "/p" {
		t.Errorf("a vz VM's project folder: %q, want its path in the VM", p)
	}
}

// Lima forwards nothing: every guest port, TCP or UDP, is ignored on
// 127.0.0.1 (::1 too), 0.0.0.0 and ::, before Lima's own rule.
func TestNoForwardsSet(t *testing.T) {
	want := `.portForwards = [{"guestIP":"127.0.0.1","guestPortRange":[1,65535],"proto":"any","ignore":true},` +
		`{"guestIP":"0.0.0.0","guestPortRange":[1,65535],"proto":"any","ignore":true},` +
		`{"guestIP":"::","guestPortRange":[1,65535],"proto":"any","ignore":true}]`
	if got := noForwardsSet(); got != want {
		t.Errorf("noForwardsSet() =\n%s\nwant\n%s", got, want)
	}
}

// SSH is what ssh, links and devvm's forwarder reach a running VM by: its
// key is the first IdentityFile of Lima's ssh config, Lima's own
// (_config/user), which every VM authorizes; a stopped VM has none. It
// runs nothing but limactl list: nothing of the VM's config changes.
func TestSSH(t *testing.T) {
	l, f, _ := testLima(t, "darwin")
	cfg := filepath.Join(t.TempDir(), "ssh.config")
	// Lima's own: host keys unchecked, a shared connection, and (here) a
	// command to run. Only the login options are kept.
	content := "Host lima-v\n  IdentityFile \"/h/.lima/_config/user\"\n  IdentityFile \"/h/.ssh/id_ed25519\"\n  StrictHostKeyChecking no\n  UserKnownHostsFile /dev/null\n" +
		"  NoHostAuthenticationForLocalhost yes\n  PreferredAuthentications publickey\n  Compression no\n  BatchMode yes\n  IdentitiesOnly yes\n" +
		"  ControlMaster auto\n  ControlPath \"/h/.lima/v/ssh.sock\"\n  ControlPersist yes\n  PermitLocalCommand yes\n  LocalCommand touch /x/ran\n" +
		"  User cj\n  Hostname 127.0.0.1\n  Port 41083\n"
	if err := os.WriteFile(cfg, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	f.out["limactl list --json"] = `{"name":"v","status":"Running","sshLocalPort":41083,"sshConfigFile":"` + cfg + `"}
{"name":"s","status":"Stopped"}`
	got, err := l.SSH(t.Context(), "v")
	// The first IdentityFile is its key, alone.
	want := backend.SSHTarget{Host: "127.0.0.1", Port: 41083, User: "cj", Key: "/h/.lima/_config/user", ConfigFile: cfg, Options: []backend.SSHOption{
		{Key: "PreferredAuthentications", Value: "publickey"},
		{Key: "Compression", Value: "no"},
		{Key: "BatchMode", Value: "yes"},
		{Key: "IdentitiesOnly", Value: "yes"},
	}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("SSH: %+v, %v; want %+v", got, err, want)
	}
	if _, err := l.SSH(t.Context(), "s"); msg.IDOf(err) != "vm_not_running" {
		t.Errorf("SSH to a stopped VM: %v", err)
	}
	for _, c := range f.lines() {
		if c != "limactl list --json" {
			t.Errorf("ran %q", c)
		}
	}
}

// Exec quotes the folder and each argument for the guest's shell. Exec
// and Shell carry the agent they're given, or none: never devvm's own
// SSH_AUTH_SOCK, which no other limactl command gets either.
func TestExecAndShell(t *testing.T) {
	l, f, _ := testLima(t, "darwin")
	t.Setenv("SSH_AUTH_SOCK", "/user/agent")
	if err := l.Exec(t.Context(), "v", []string{"echo", "it's $HOME"}, backend.ExecOpts{Dir: "/my dir"}); err != nil {
		t.Fatal(err)
	}
	want := []string{"limactl", "shell", "--workdir", "/", "v", "sh", "-c", `cd '/my dir' && exec 'echo' 'it'\''s $HOME'`}
	if got := f.calls[0].argv; !slices.Equal(got, want) {
		t.Errorf("Exec ran %q, want %q", got, want)
	}
	if v, ok := envOf(f.calls[0], "SSH_AUTH_SOCK"); ok {
		t.Errorf("Exec without an agent: SSH_AUTH_SOCK=%q", v)
	}
	if err := l.Exec(t.Context(), "v", []string{"true"}, backend.ExecOpts{Agent: "/d/agent.sock"}); err != nil {
		t.Fatal(err)
	}
	if v, _ := envOf(f.calls[1], "SSH_AUTH_SOCK"); v != "/d/agent.sock" {
		t.Errorf("Exec with devvm's agent: SSH_AUTH_SOCK=%q", v)
	}
	if _, err := l.Inspect(t.Context(), "v"); err == nil {
		t.Fatal("Inspect found a VM the fake has none of")
	}
	if v, ok := envOf(f.calls[2], "SSH_AUTH_SOCK"); ok {
		t.Errorf("limactl list: SSH_AUTH_SOCK=%q", v)
	}
	c, err := l.Shell(t.Context(), "v", backend.ShellOpts{Dir: "/p", Agent: "/d/agent.sock"})
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(c.Path) || !slices.Equal(c.Args[1:], []string{"shell", "--workdir", "/p", "v"}) ||
		c.Dir != l.Dirs.Data || !slices.Contains(c.Env, "LIMA_HOME="+l.Dirs.Lima) {
		t.Errorf("Shell: %+v", c)
	}
	if v, _ := envOf(call{env: c.Env}, "SSH_AUTH_SOCK"); v != "/d/agent.sock" {
		t.Errorf("Shell with devvm's agent: SSH_AUTH_SOCK=%q", v)
	}
	if c, err = l.Shell(t.Context(), "v", backend.ShellOpts{Dir: "/p"}); err != nil {
		t.Fatal(err)
	}
	if v, ok := envOf(call{env: c.Env}, "SSH_AUTH_SOCK"); ok {
		t.Errorf("Shell without an agent: SSH_AUTH_SOCK=%q", v)
	}
}

// devvm limactl passes the user's environment on, but on devvm's Lima home
// and without the user's SSH agent, which a shared ssh connection it
// opened would carry into devvm's own sessions.
func TestLimactlEnv(t *testing.T) {
	env := limactlEnv([]string{"PATH=/bin", "SSH_AUTH_SOCK=/user/agent", "LIMA_HOME=/mine"}, paths.Dirs{Lima: "/d/lima"})
	if want := []string{"PATH=/bin", "LIMA_HOME=/d/lima"}; !slices.Equal(env, want) {
		t.Errorf("limactlEnv = %q, want %q", env, want)
	}
}

// Against the limactl stub: a VM Lima reports as broken (its start cut
// short before its hostagent answers) is stopped with --force, which
// limactl stop alone refuses.
func TestStubStopBroken(t *testing.T) {
	l, _, stubs := stubLima(t)
	vm := "devvm-p-0123456789abcd"
	stubs.VM(t, vm, "Broken")
	if err := l.Stop(t.Context(), vm); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range stubs.Calls(t) {
		if c != "limactl list --json" {
			got = append(got, c)
		}
	}
	if want := []string{"limactl stop --force " + vm}; !slices.Equal(got, want) {
		t.Errorf("calls %q, want %q", got, want)
	}
}

// stubCalls is the stub's calls but list --json's.
func stubCalls(t *testing.T, stubs *stubtest.Stubs) []string {
	t.Helper()
	var got []string
	for _, c := range stubs.Calls(t) {
		if c != "limactl list --json" {
			got = append(got, c)
		}
	}
	return got
}

// Against the limactl stub: a VM whose start was cut short while its
// hostagent starts QEMU (broken: a hostagent, no driver yet) is waited
// for, and stopped as it then is, running: forced at once, the hostagent
// would go and leave QEMU running without it.
func TestStubStopStarting(t *testing.T) {
	l, _, stubs := stubLima(t)
	l.DriverWait = 10 * time.Second
	vm := "devvm-p-0123456789abcd"
	stubs.VM(t, vm, "Broken")
	stubs.PIDs(t, vm, 4242, 0)
	go func() {
		time.Sleep(300 * time.Millisecond)
		stubs.VM(t, vm, "Running")
	}()
	if err := l.Stop(t.Context(), vm); err != nil {
		t.Fatal(err)
	}
	if got, want := stubCalls(t, stubs), []string{"limactl stop " + vm}; !slices.Equal(got, want) {
		t.Errorf("calls %q, want %q", got, want)
	}
}

// Against the limactl stub: a VM whose QEMU runs on without its hostagent
// (broken), which a new start can't start, is stopped with --force first.
func TestStubStartOrphanedDriver(t *testing.T) {
	l, _, stubs := stubLima(t)
	vm := "devvm-p-0123456789abcd"
	stubs.VM(t, vm, "Broken")
	stubs.PIDs(t, vm, 0, 4242)
	if err := l.Start(t.Context(), vm, backend.Want{}, backend.Confinement{}); err != nil {
		t.Fatal(err)
	}
	got := stubCalls(t, stubs)
	if len(got) < 2 || got[0] != "limactl stop --force "+vm || !slices.Contains(got, "limactl start --tty=false "+vm) {
		t.Errorf("calls %q, want a forced stop, then the start", got)
	}
}

// Against the limactl stub: VMs stop, start and go.
func TestStubLifecycle(t *testing.T) {
	l, _, stubs := stubLima(t)
	ctx := t.Context()
	vm := "devvm-p-0123456789abcd"
	stubs.VM(t, vm, "Stopped")
	if err := l.Start(ctx, vm, backend.Want{}, backend.Confinement{}); err != nil {
		t.Fatal(err)
	}
	if in, err := l.Inspect(ctx, vm); err != nil || in.Status != backend.StatusRunning {
		t.Errorf("after Start: %+v, %v", in, err)
	}
	// Started without its proxy: running, not confined.
	if r, err := l.Running(ctx, vm); err != nil || r.Confined || r.ID == "" {
		t.Errorf("Running after a direct Start: %+v, %v", r, err)
	}
	if err := l.Start(ctx, vm, backend.Want{}, backend.Confinement{}); err != nil {
		t.Fatal(err)
	}
	if err := l.Stop(ctx, vm); err != nil {
		t.Fatal(err)
	}
	if err := l.Stop(ctx, vm); err != nil {
		t.Fatal(err)
	}
	if err := l.Delete(ctx, vm); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range stubs.Calls(t) {
		if c != "limactl list --json" {
			got = append(got, c)
		}
	}
	// Lima's own forwards turned off, once.
	want := []string{"limactl edit --tty=false " + vm + " --set " + noForwardsSet(), "limactl start --tty=false " + vm, "limactl stop " + vm, "limactl delete -f " + vm}
	if !slices.Equal(got, want) {
		t.Errorf("calls %q, want %q", got, want)
	}
	// A project VM's folder, its start record with it, is its teardown's.
	if _, err := os.Stat(l.Dirs.VM(vm).Started()); err != nil {
		t.Errorf("its start record: %v", err)
	}
}

// Against the limactl stub: a stopped VM's instance is renamed, its
// size with it, and a running one, a name too long for its socket or a
// failed rename is refused. Its old machine, set aside under its VM's
// aside name, is no stray while its VM has state.
func TestStubRename(t *testing.T) {
	l, _, stubs := stubLima(t)
	ctx := t.Context()
	vm := "devvm-p-0123456789abcd"
	aside, _ := vmname.Aside(vm)
	stubs.VM(t, vm, "Running")
	stubs.Size(t, vm, 2, 4294967296, 107374182400)
	if err := l.Rename(ctx, vm, aside); err == nil || msg.IDOf(err) != "" {
		t.Errorf("running: %v", err)
	}
	stubs.VM(t, vm, "Stopped")
	if err := l.Rename(ctx, vm, strings.Repeat("x", l.NameMax()+1)); msg.IDOf(err) != "lima_home_too_long" {
		t.Errorf("too long: %v", err)
	}
	stubs.Touch(t, "rename-fails")
	if err := l.Rename(ctx, vm, aside); msg.IDOf(err) != "rename_failed" {
		t.Errorf("failed: %v", err)
	}
	if err := os.Remove(stubs.Path("rename-fails")); err != nil {
		t.Fatal(err)
	}
	if err := l.Rename(ctx, vm, aside); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Inspect(ctx, vm); !errors.Is(err, backend.ErrNotFound) {
		t.Errorf("%s after the rename: %v", vm, err)
	}
	in, err := l.Inspect(ctx, aside)
	if err != nil || in.Size.CPUs != 2 || in.Status != backend.StatusStopped {
		t.Errorf("%s: %+v, %v", aside, in, err)
	}
	strays, err := l.Strays(ctx)
	if err != nil || len(strays) != 1 || strays[0].Name != aside {
		t.Errorf("without its VM's state, strays %+v, %v", strays, err)
	}
	if err := os.MkdirAll(l.Dirs.VM(vm).Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.Dirs.VM(vm).State(), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if strays, err := l.Strays(ctx); err != nil || len(strays) != 0 {
		t.Errorf("with its VM's state, strays %+v, %v", strays, err)
	}
}
