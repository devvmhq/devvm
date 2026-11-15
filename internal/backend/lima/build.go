package lima

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/ubuntu"
	"github.com/devvmhq/devvm/internal/vmname"
)

// BuildImage builds the image's instance, devvm-img-<key>, which is then
// the image: base is a VM made from Ubuntu's cloud image with base.yaml's
// settings and base's size (buildSize) (with spec.From, from that image converted into a disk:
// convert), all from its recipe's origin (Origin, checked by originOf), a
// layer a clone of its parent's instance. Neither mounts anything from the
// host. It starts confined as spec.Net asks (no port forwards, and on
// macOS a sandbox), runs its recipe's steps in order, and stops.
//
// Until it's built, an interrupted run deletes the instance, and so does a
// failed create or clone: a cleanup on the ui stack, which runs after the
// caller has shown the run log's end. Once it exists, a failure keeps it,
// stopped, for inspection; the next build deletes it (it has no record).
// The registry's guard has checked spec (ImageSpec.Validate).
func (l *images) BuildImage(ctx context.Context, spec backend.ImageSpec) (backend.Built, error) {
	inst, err := vmname.Image(spec.Key)
	if err != nil {
		return backend.Built{}, err
	}
	if _, err := l.inspect(ctx, inst); err == nil {
		return backend.Built{}, fmt.Errorf("lima: %s exists already", inst)
	} else if !errors.Is(err, backend.ErrNotFound) {
		return backend.Built{}, err
	}
	var parent vmname.VM
	if spec.Parent != "" {
		if parent, err = vmname.Image(spec.Parent); err != nil {
			return backend.Built{}, msg.ImageMissing(spec.Parent)
		}
		// Its parent stays as it is: stopped, as clones need.
		in, err := l.inspect(ctx, parent)
		if errors.Is(err, backend.ErrNotFound) {
			return backend.Built{}, msg.ImageMissing(spec.Parent)
		} else if err != nil {
			return backend.Built{}, err
		}
		if backend.Status(in.Status) != backend.StatusStopped {
			if err := l.stop(ctx, parent); err != nil {
				return backend.Built{}, err
			}
		}
	}

	var created atomic.Bool // an interrupt's cleanups may run while this goes on
	remove := l.AddCleanup(func(ctx context.Context) {
		if !created.Load() || l.Interrupted() {
			l.unfinished(ctx, inst)
		}
	})
	// Its steps: create or clone, configure (its new ssh port, at each
	// start) and start, each of its recipe's, stop; with --from, the
	// conversion's first, and an image only Docker has its export.
	steps := 4 + len(spec.Recipe.Steps)
	if !spec.Trust.None() {
		steps += 2
	}
	// The company proxy, which the build reaches through a proxy of
	// devvm's (buildproxy.go): checked before anything is made.
	host, _, err := buildUpstream(ctx, l.Company, spec.Net)
	if err != nil {
		return backend.Built{}, err
	}
	if !host.None() {
		steps += 2
	}
	if spec.From != nil {
		steps += ociSteps
		if !spec.Trust.None() {
			steps++
		}
		if !strings.Contains(spec.From.Digest, "@") {
			steps++
		}
	}
	ctx = l.WithSteps(ctx, steps)
	var built backend.Built
	var conv *converted
	if spec.Parent == "" {
		// What it's made from is its recipe's origin, as given.
		o, err := l.originOf(spec.Recipe.Origin, spec.From)
		if err != nil {
			return backend.Built{}, err
		}
		want, size, err := l.buildSize(o)
		if err != nil {
			return backend.Built{}, err
		}
		if spec.MemoryGiB > 0 {
			size.MemoryGiB = spec.MemoryGiB
		}
		if spec.From == nil {
			img, err := l.createFromUbuntu(ctx, inst, o.yaml, size)
			if err != nil {
				return backend.Built{}, err
			}
			built = backend.Built{Origin: img.String(), Libc: "gnu"}
		} else {
			if conv, err = l.convert(ctx, spec.From, o, size, spec.Trust.Add); err != nil {
				return backend.Built{}, err
			}
			if err := l.create(ctx, msg.Creating(inst), inst, o.yaml, createArgs(inst, convertedImages(conv.dir, conv.cmdline, l.Arch), size)); err != nil {
				return backend.Built{}, err
			}
			built = backend.Built{Origin: spec.From.Digest, Libc: conv.libc, Glibc: conv.glibc}
		}
		built.Fitted = backend.Fitted(want, size)
	} else if err := l.Runner.Run(ctx, msg.CloningImage(image.Name(spec.Image).Parent().String()),
		l.limactl(ctx, layerArgs(parent, inst)...)); err != nil {
		return backend.Built{}, backend.Logged(msg.VMCreateFailed(inst))
	}
	created.Store(true)
	// Its proxy ends with it, or with the run.
	conf, hp, proxyDone, err := buildProxy(ctx, l.Lima, inst, spec.Net)
	if err != nil {
		return backend.Built{}, err
	}
	// A failed start leaves it stopped, with its console logs. Base from
	// Ubuntu's cloud image that doesn't boot says so: the caller may try
	// an older image.
	if err := l.start(ctx, inst, nil, conf); err != nil {
		if spec.Parent == "" && spec.From == nil && ctx.Err() == nil {
			return backend.Built{}, &backend.BootError{Err: err}
		}
		return backend.Built{}, err
	}
	if conv != nil {
		// Lima copied the converted files in as it started.
		conv.done(ctx)
	}
	// Once it's running, a failure stops it (unless the run was
	// interrupted: then it's deleted).
	failed := func(err error) (backend.Built, error) {
		if ctx.Err() == nil {
			_ = l.stop(ctx, inst)
		}
		return backend.Built{}, err
	}
	if err := buildScripts(ctx, l.Lima, inst, spec, hp); err != nil {
		return failed(err)
	}
	// Flushed first: a guest that ignores Lima's stop request is stopped
	// hard.
	if err := l.Runner.Run(ctx, msg.M{}, l.limactl(ctx, "shell", "--workdir", "/", inst.String(), "sync")); err != nil {
		return failed(backend.Logged(msg.StopFailed(inst)))
	}
	if err := l.stop(ctx, inst); err != nil {
		return backend.Built{}, err
	}
	proxyDone(true)
	// What its runs left (its sandbox profile, the record of its start)
	// is of no more use: an image's instance never starts again.
	if err := os.RemoveAll(l.Dirs.VM(inst).Dir); err != nil {
		return backend.Built{}, err
	}
	if l.overlays() {
		// Project VMs' disks are overlays of this one: it never changes
		// again.
		if fi, err := os.Stat(l.disk(inst)); err == nil {
			if err := os.Chmod(l.disk(inst), fi.Mode().Perm()&^0o222); err != nil {
				return backend.Built{}, err
			}
		}
	}
	remove()
	return built, nil
}

// buildScripts runs a build's scripts in its VM (inst), in order: the
// user's CAs added and sudo told to keep the build's proxy (hp) as root,
// spec's recipe as it is, each step after the CAs' and hp's variables,
// then both taken out again.
func buildScripts(ctx context.Context, l *Lima, inst vmname.VM, spec backend.ImageSpec, hp backend.HostProxy) error {
	sudoOn, sudoOff := sudoScripts(hp)
	for _, s := range []backend.Script{spec.Trust.Add, sudoOn} {
		if err := l.runScript(ctx, inst, s, true); err != nil {
			return err
		}
	}
	if err := sendPackages(ctx, l, inst, spec.Packages); err != nil {
		return err
	}
	for _, s := range spec.Recipe.Steps {
		if err := l.runStep(ctx, inst, s, slices.Concat(spec.Trust.Env, proxyExports(hp))); err != nil {
			return err
		}
	}
	if err := takePackages(ctx, l, inst, spec.Packages); err != nil {
		return err
	}
	for _, s := range []backend.Script{sudoOff, spec.Trust.Remove} {
		if err := l.runScript(ctx, inst, s, true); err != nil {
			return err
		}
	}
	return nil
}

// limaImage is an entry in a Lima config's images: a disk image, or a
// disk with the kernel and initrd Lima boots directly.
type limaImage struct {
	Location string      `json:"location"`
	Arch     string      `json:"arch"`
	Digest   string      `json:"digest,omitempty"`
	Kernel   *limaKernel `json:"kernel,omitempty"`
	Initrd   *limaFile   `json:"initrd,omitempty"`
}

type limaKernel struct {
	Location string `json:"location"`
	Cmdline  string `json:"cmdline"`
}

type limaFile struct {
	Location string `json:"location"`
}

// convertedImages are a --from base's images: base.yaml's settings
// booting the converted disk in dir with its kernel (and cmdline) and
// initrd.
func convertedImages(dir, cmdline, goarch string) []limaImage {
	return []limaImage{{
		Location: filepath.Join(dir, "disk.raw"), Arch: limaArch(goarch),
		Kernel: &limaKernel{Location: filepath.Join(dir, "kernel"), Cmdline: cmdline},
		Initrd: &limaFile{Location: filepath.Join(dir, "initrd")},
	}}
}

// layerArgs are limactl clone's arguments for a layer's instance (inst):
// a clone of its parent's, with nothing from the host mounted.
func layerArgs(parent, inst vmname.VM) []string {
	return []string{"clone", "--tty=false", "--mount-none", parent.String(), inst.String()}
}

// createFromUbuntu creates a VM from Ubuntu's cloud image, the one pinned
// on this machine, which base's origin holds, with yaml's settings
// (base.yaml) and nothing from the host mounted, or the mounts a --set in
// opts gives it (--set applies after --mount-none). It returns the image
// it used. It gets size's CPUs, memory and disk (buildSize's).
func (l *images) createFromUbuntu(ctx context.Context, vm vmname.VM, yaml []byte, size backend.Size, opts ...string) (ubuntu.Image, error) {
	img, err := l.pinnedImage()
	if err != nil {
		return ubuntu.Image{}, err
	}
	images := []limaImage{{Location: img.URL, Arch: limaArch(l.Arch), Digest: img.Digest}}
	if err := l.create(ctx, msg.CreatingFromUbuntu(vm), vm, yaml, createArgs(vm, images, size, opts...)); err != nil {
		return ubuntu.Image{}, err
	}
	return img, nil
}

// createArgs are limactl create's arguments for a VM booting images, with
// nothing from the host mounted but what opts' --set gives it, size's CPUs
// and memory, its disk unless that's zero (Lima's own then), and the rest
// of base.yaml's settings, which create gives it on its standard input
// (-).
func createArgs(vm vmname.VM, images []limaImage, size backend.Size, opts ...string) []string {
	args := append([]string{"create", "--tty=false", "--mount-none", "--name=" + vm.String(), "--set", ".images = " + yqJSON(images)}, opts...)
	args = append(args, "--cpus="+strconv.Itoa(size.CPUs), "--memory="+gibString(size.MemoryGiB))
	if size.DiskGiB > 0 {
		args = append(args, "--disk="+gibString(size.DiskGiB))
	}
	return append(args, "-")
}

// create runs limactl create with args (createArgs) as step, yaml
// (base.yaml, as base's origin has it) on its standard input: no copy of
// it on disk.
func (l *images) create(ctx context.Context, step msg.M, vm vmname.VM, yaml []byte, args []string) error {
	cmd := l.limactl(ctx, args...)
	cmd.Stdin = bytes.NewReader(yaml)
	if err := l.Runner.Run(ctx, step, cmd); err != nil {
		return backend.Logged(msg.VMCreateFailed(vm))
	}
	return nil
}

// limaArch is Lima's name for a Go architecture.
func limaArch(goarch string) string {
	switch goarch {
	case "arm64":
		return "aarch64"
	case "amd64":
		return "x86_64"
	}
	return goarch
}

// runScript runs a script in the VM, fed on its standard input to bash as
// the VM's user, or with root to sh as root: a build's Trust scripts, of
// which an empty one isn't run.
func (l *images) runScript(ctx context.Context, vm vmname.VM, s backend.Script, root bool) error {
	cmd := l.limactl(ctx, "shell", vm.String(), "bash", "-s")
	if root {
		if len(s.Data) == 0 {
			return nil
		}
		cmd = l.limactl(ctx, "shell", "--workdir", "/", vm.String(), "sudo", "sh", "-s")
	}
	cmd.Stdin = bytes.NewReader(s.Data)
	if err := l.Runner.Run(ctx, msg.RunningScript(s.Name, shown(ctx, vm)), cmd); err != nil {
		return backend.Logged(msg.ScriptFailed(s.Name, vm))
	}
	return nil
}

// runStep runs a recipe's step in the VM: its script after env (the
// build's own exports, which the image keeps nothing of), fed on its
// standard input to bash with its arguments, as the VM's user, or as root.
func (l *images) runStep(ctx context.Context, vm vmname.VM, s image.Step, env []byte) error {
	argv := []string{"shell", vm.String(), "bash", "-s"}
	if s.Root {
		argv = []string{"shell", "--workdir", "/", vm.String(), "sudo", "bash", "-s"}
	}
	if len(s.Args) > 0 {
		argv = slices.Concat(argv, []string{"--"}, s.Args)
	}
	cmd := l.limactl(ctx, argv...)
	cmd.Stdin = bytes.NewReader(slices.Concat(env, s.Data))
	if err := l.Runner.Run(ctx, msg.RunningScript(s.Name, vm.String()), cmd); err != nil {
		if e, ok := errors.AsType[*exec.ExitError](err); ok && e.ExitCode() == backend.ExitUnreachable {
			return backend.Logged(msg.BuildSourcesUnreachable(s.Name, vm))
		}
		return backend.Logged(msg.ScriptFailed(s.Name, vm))
	}
	return nil
}

// unfinished deletes an image's instance whose build didn't finish: on an
// interrupt, or when its create or clone failed.
func (l *images) unfinished(ctx context.Context, inst vmname.VM) {
	if _, err := l.inspect(ctx, inst); err != nil {
		return // never created, or Lima can't say: nothing devvm can delete
	}
	if l.Interrupted() {
		l.Info(msg.InterruptedImage(inst))
	} else {
		l.Info(msg.UnfinishedImage(inst))
	}
	_ = l.delete(ctx, inst)
}

// buildSize is base's size: its own CPUs and memory (backend.BaseCPUs,
// backend.BaseMemoryGiB) as want, and got, them fitted to the host
// (backend.FitCPUs, backend.FitMemory), so a host with fewer CPUs gets
// its own count and one that base's memory would leave less than 2 GiB
// gets half its memory, with o's disk. Images built from base, and their
// VMs, keep got.
func (l *images) buildSize(o origin) (want, got backend.Size, err error) {
	want = backend.Size{CPUs: backend.BaseCPUs, MemoryGiB: backend.BaseMemoryGiB}
	host, err := l.Memory()
	if err != nil {
		return want, got, err
	}
	got = backend.Size{
		CPUs: backend.FitCPUs(want.CPUs, l.CPUs()), MemoryGiB: backend.FitMemory(want.MemoryGiB, host),
		DiskGiB: o.diskGiB,
	}
	return want, got, nil
}
