package lima

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/assets"
	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/redact"
	"github.com/devvmhq/devvm/internal/ubuntu"
	"github.com/devvmhq/devvm/internal/vmname"
)

// These tests run the real limactl on dummy instances, as Lima's own
// tests do: an instance whose image is a small local file, which can't
// boot, but which limactl creates, clones, edits, lists and deletes. They
// run in a sandbox of their own (a Lima home under /tmp, devvm's folders,
// HOME), never start a VM, and skip when limactl isn't on PATH.

// realLima returns the backend on a sandbox with the real limactl, and
// the folders and files its dummy instances use.
func realLima(t *testing.T) (*Lima, fixture) {
	t.Helper()
	if _, err := exec.LookPath("limactl"); err != nil {
		t.Skip("limactl isn't on PATH: these tests run real Lima")
	}
	return dummyLima(t, sandboxDirs(t), realRunner{t})
}

// dummyLima returns the backend on d, running its commands with r, and
// the fixture it makes in d's home.
func dummyLima(t *testing.T, d paths.Dirs, r Runner) (*Lima, fixture) {
	t.Helper()
	fx := newFixture(t, d.Home)
	l := New(d, assets.FS, &msg.Fake{}, platform.Host{}, "", nil)
	// Project VMs' disks aren't overlays here: those are qemu-img's,
	// not limactl's.
	l.Runner, l.OS = r, "darwin"
	l.Getenv = func(string) string { return "" }
	fx.ubuntu = pinUbuntu(t, l, fx.ubuntuDir, fx.ubuntuSum)
	if err := os.MkdirAll(filepath.Dir(fx.ubuntu.URL), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(d.Home, "ubuntu.img"), fx.ubuntu.URL); err != nil {
		t.Fatal(err)
	}
	return l, fx
}

// fixture is what dummy instances use: the image (as Ubuntu's, with its
// digest), a --from conversion's files, and the folders VMs mount.
type fixture struct {
	ubuntu    ubuntu.Image // pinned, at ubuntuDir, with ubuntuSum
	ubuntuDir string
	ubuntuSum string
	converted string // disk.raw, kernel and initrd
	project   string
	agents    []string
	tools     string
	out       string // the tools build's and the conversion's output
	scripts   string // the conversion's scripts
}

// newFixture makes a fixture in dir.
func newFixture(t *testing.T, dir string) fixture {
	t.Helper()
	fx := fixture{
		converted: filepath.Join(dir, "converted"),
		project:   filepath.Join(dir, "work", "web"),
		agents:    []string{filepath.Join(dir, ".claude"), filepath.Join(dir, ".codex")},
		tools:     filepath.Join(dir, "tools"),
		out:       filepath.Join(dir, "out"),
		scripts:   filepath.Join(dir, "oci"),
	}
	for _, d := range append([]string{fx.converted, fx.project, fx.tools, fx.out, fx.scripts}, fx.agents...) {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	img := []byte("not a disk\n")
	for _, f := range []string{
		filepath.Join(dir, "ubuntu.img"), filepath.Join(fx.converted, "disk.raw"),
		filepath.Join(fx.converted, "kernel"), filepath.Join(fx.converted, "initrd"),
	} {
		if err := os.WriteFile(f, img, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sum := sha256.Sum256(img)
	fx.ubuntuDir, fx.ubuntuSum = filepath.Join(dir, "ubuntu"), hex.EncodeToString(sum[:])
	return fx
}

// The instances the scenario makes.
const (
	dummyBase  vmname.VM = vmname.ImagePrefix + "0123456789ab"
	dummyFrom  vmname.VM = vmname.ImagePrefix + "0123456789ac"
	dummyLayer vmname.VM = vmname.ImagePrefix + "0123456789ad"
	dummyVM    vmname.VM = "devvm-web-0123456789abcd"
)

// step is one thing devvm does with limactl, as it does it.
type step struct {
	name string
	do   func(ctx context.Context, l *Lima) error
}

// scenario is every kind of config devvm gives an instance, step by step,
// on dummy instances: base from Ubuntu's image and from a --from
// conversion, a layer, the tools build VM and the conversion's VM, a
// project VM with its mounts, ports, size and environment, the edits each
// start makes (a build's networks, network policy with the proxy's
// password and the VM's Want changed in the same edit, none), the project
// VM set aside under another name and put back, as a remake does, and
// everything deleted.
func scenario(fx fixture) []step {
	size := backend.Size{CPUs: 2, MemoryGiB: 3}
	sshPort := 50022
	// A start's edits, w nil for a build's VM.
	start := func(vm vmname.VM, w *backend.Want, c backend.Confinement) func(context.Context, *Lima) error {
		return func(ctx context.Context, l *Lima) error {
			in, err := l.inspect(ctx, vm)
			if err != nil {
				return err
			}
			return configure(ctx, l, in, w, c, sshPort)
		}
	}
	made := backend.Want{
		Project: fx.project, AgentDirs: fx.agents, AgentForwarding: true, Size: backend.Size{CPUs: 3, MemoryGiB: 5, DiskGiB: 120},
	}
	changed := backend.Want{Project: fx.project, AgentDirs: fx.agents[:min(1, len(fx.agents))], Size: backend.Size{CPUs: 4, MemoryGiB: 6.5, DiskGiB: 130}}
	proxy := backend.Confinement{
		Net: backend.NetProxy, ProxyPort: 3128, ProxyPassword: redact.NewSecret("pAss-0123_word"),
		Env: map[string]string{"NODE_EXTRA_CA_CERTS": "/etc/ssl/devvm.pem", "NO_PROXY": "localhost,127.0.0.1,::1"},
	}
	env := map[string]string{"LANG": "C.UTF-8"}
	return []step{
		{"base from Ubuntu's image", func(ctx context.Context, l *Lima) error {
			yaml, err := fs.ReadFile(l.Assets, originConfig)
			if err != nil {
				return err
			}
			_, err = l.createFromUbuntu(ctx, dummyBase, yaml, size)
			return err
		}},
		{"base's build start", start(dummyBase, nil, backend.Confinement{Net: backend.NetInternet})},
		{"base from a conversion", func(ctx context.Context, l *Lima) error {
			yaml, err := fs.ReadFile(l.Assets, originConfig)
			if err != nil {
				return err
			}
			return l.create(ctx, msg.M{}, dummyFrom, yaml, createArgs(dummyFrom, convertedImages(fx.converted, "root=/dev/vda rw console=hvc0", l.Arch), size))
		}},
		{"a layer", func(ctx context.Context, l *Lima) error {
			return l.Runner.Run(ctx, msg.M{}, l.limactl(ctx, layerArgs(dummyBase, dummyLayer)...))
		}},
		{"the layer's build start", start(dummyLayer, nil, backend.Confinement{Net: backend.NetNone})},
		{"the tools build VM", func(ctx context.Context, l *Lima) error {
			mounts, err := mountsSet([]mount{{Location: fx.out, MountPoint: ptr("/mnt/devvm-tools"), Writable: ptr(true)}})
			if err != nil {
				return err
			}
			return l.Runner.Run(ctx, msg.M{}, l.limactl(ctx, toolsArgs(dummyBase, mounts)...))
		}},
		{"the conversion's VM", func(ctx context.Context, l *Lima) error {
			mounts, err := mountsSet([]mount{{Location: fx.out, Writable: ptr(true)}, {Location: fx.scripts, Writable: ptr(false)}})
			if err != nil {
				return err
			}
			yaml, err := fs.ReadFile(l.Assets, originConfig)
			if err != nil {
				return err
			}
			_, err = l.createFromUbuntu(ctx, OCIVM, yaml, size, "--set", mounts)
			return err
		}},
		{"a project VM", func(ctx context.Context, l *Lima) error {
			return l.CreateVM(ctx, strings.TrimPrefix(string(dummyLayer), vmname.ImagePrefix), backend.VMSpec{
				Name: dummyVM, Tools: fx.tools, ToolsAt: "/mnt/devvm-tools",
				Want: made, SSHPort: sshPort, Env: env,
			})
		}},
		{"its first start", start(dummyVM, &made, backend.Confinement{Net: backend.NetDirect, Env: env})},
		{"a start with network policy, its agents' folders, forwarding and size changed", start(dummyVM, &changed, proxy)},
		{"the same start again", start(dummyVM, &changed, proxy)},
		{"a start without network policy", start(dummyVM, &changed, backend.Confinement{Net: backend.NetDirect, Env: env})},
		{"a start with no network", start(dummyVM, &changed, backend.Confinement{Net: backend.NetNone})},
		{"the project VM set aside", func(ctx context.Context, l *Lima) error {
			aside, _ := dummyVM.Aside()
			return l.Rename(ctx, dummyVM, aside)
		}},
		{"the project VM put back", func(ctx context.Context, l *Lima) error {
			aside, _ := dummyVM.Aside()
			return l.Rename(ctx, aside, dummyVM)
		}},
		{"the build VMs deleted", func(ctx context.Context, l *Lima) error {
			if err := l.deleteIfThere(ctx, ToolsVM); err != nil {
				return err
			}
			return l.deleteIfThere(ctx, OCIVM)
		}},
		{"the project VM deleted", func(ctx context.Context, l *Lima) error { return l.Delete(ctx, dummyVM) }},
		{"the images deleted", func(ctx context.Context, l *Lima) error {
			for _, inst := range []vmname.VM{dummyLayer, dummyFrom, dummyBase} {
				key, _ := inst.ImageKey()
				if err := l.DeleteImage(ctx, key); err != nil {
					return err
				}
			}
			return nil
		}},
	}
}

// realRunner runs commands as they are, logging a failed one's output.
type realRunner struct{ t *testing.T }

func (r realRunner) Run(_ context.Context, _ msg.M, cmd *exec.Cmd) error {
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Logf("%s: %v\n%s", strings.Join(cmd.Args, " "), err, out)
	}
	return err
}

func (r realRunner) Output(cmd *exec.Cmd) ([]byte, error) {
	var errOut bytes.Buffer
	cmd.Stderr = &errOut
	out, err := cmd.Output()
	if err != nil {
		r.t.Logf("%s: %v\n%s", strings.Join(cmd.Args, " "), err, errOut.String())
	}
	return out, err
}

func (realRunner) Attach(cmd *exec.Cmd) error { return cmd.Run() }

// validate runs limactl validate on the Lima config of each instance in
// l's Lima home, and returns those it found.
func validate(t *testing.T, l *Lima) []vmname.VM {
	t.Helper()
	ins, err := l.list(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var names []vmname.VM
	for _, in := range ins {
		names = append(names, in.Name)
		yaml := filepath.Join(l.Dirs.LimaVM(in.Name), "lima.yaml")
		if err := l.Runner.Run(t.Context(), msg.M{}, l.limactl(t.Context(), "validate", yaml)); err != nil {
			data, _ := os.ReadFile(yaml) //nolint:gosec // the test's own instance
			t.Errorf("%s isn't valid: %v\n%s", in.Name, err, data)
		}
	}
	return names
}

// Every Lima config devvm makes passes limactl validate, step by step:
// what limactl create, clone and edit write, and the proxy's password
// devvm writes into lima.yaml itself.
func TestLimaConfigsValid(t *testing.T) {
	l, fx := realLima(t)
	for _, s := range scenario(fx) {
		if err := s.do(t.Context(), l); err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		names := validate(t, l)
		t.Logf("%s: %s valid", s.name, vmname.Join(names, ", "))
		if strings.HasPrefix(s.name, "a start with network policy") {
			data, err := os.ReadFile(filepath.Join(l.Dirs.LimaVM(dummyVM), "lima.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(data, []byte(":pAss-0123_word@")) || bytes.Contains(data, []byte(passwordStandIn)) {
				t.Errorf("the proxy's password isn't filled in:\n%s", data)
			}
		}
	}
	if names := validate(t, l); len(names) > 0 {
		t.Errorf("left behind: %q", names)
	}
	// limactl validate reads what it checks.
	bad := filepath.Join(l.Dirs.Home, "bad.yaml")
	if err := os.WriteFile(bad, []byte("cpus: many\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := l.Runner.Run(t.Context(), msg.M{}, l.limactl(t.Context(), "validate", bad)); err == nil {
		t.Error("limactl validate passed a config with cpus: many")
	}
}
