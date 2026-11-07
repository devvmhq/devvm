// Package lima is the Lima backend: VMs run by limactl, in a Lima home of
// devvm's own. On macOS VMs run on Apple's Virtualization framework and are
// confined with Seatbelt (sandbox-exec); on Linux they run on QEMU and are
// confined with QEMU's restrict=on, through a wrapper (devvm _qemu).
package lima

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/daemon"
	"github.com/devvmhq/devvm/internal/from"
	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/ports"
	"github.com/devvmhq/devvm/internal/settings"
	"github.com/devvmhq/devvm/internal/shell"
	"github.com/devvmhq/devvm/internal/ubuntu"
	"github.com/devvmhq/devvm/internal/vmname"
)

// Name is the backend's name.
const Name = "lima"

// ImagePrefix starts the name of every image's instance: devvm-img-<key>.
const ImagePrefix = "devvm-img-"

// sshTimeout is how long a VM that Lima says is running, after a start
// Lima reports as failed, gets to answer over ssh.
const sshTimeout = 60 * time.Second

// driverWait is how long a stop waits for a hostagent to start its
// driver: QEMU, on a slow host (nested virtualization), takes seconds.
const driverWait = 30 * time.Second

// Lima is the backend. New sets every field; tests change them. Its own
// methods are Name, Caps and what its roles share (limactl, inspect…);
// the rest are its roles', host, images, machines and housekeeping, each
// a type of its own over this Lima (withRoles), as backend.Backend's
// roles are.
type Lima struct {
	host
	images
	machines
	housekeeping

	Dirs   paths.Dirs
	Runner Runner
	// OS and Arch are the host's, as Go names them: how VMs are confined
	// (Seatbelt on darwin, QEMU on linux) and which QEMU Lima runs.
	OS, Arch string
	// Self is devvm's binary, which the QEMU wrapper and a build's proxy
	// run.
	Self string
	// Spawn starts a build's proxy, a process that can outlive devvm,
	// and returns it (daemon.Spawn); Tick is how often it's looked at
	// while it starts, ten times at most.
	Spawn func(daemon.Cmd) (*daemon.Handle, error)
	Tick  time.Duration
	// Assets holds devvm's own files (assets.FS): base.yaml and the
	// conversion's oci/ scripts, which Origin puts in base's recipe and
	// BuildImage runs from there, and guest/ca.sh for a conversion with
	// the user's CAs.
	Assets fs.FS
	// SSHTimeout is how long a degraded start waits for ssh.
	SSHTimeout time.Duration
	// DriverWait is how long Stop waits for a hostagent starting its
	// driver before it stops the VM with --force.
	DriverWait time.Duration
	// OCINote is how long a --from conversion waits for another devvm's
	// before it says so.
	OCINote time.Duration
	// Getenv reads the environment (the proxy settings, USER).
	Getenv func(string) string
	// Company is the company proxy builds go through (buildUpstream).
	Company *settings.CompanyProxy
	// Ports chooses a build VM's ssh port (ports.Allocator.Build), which
	// no VM's state records.
	Ports ports.Allocator
	// Ubuntu finds Ubuntu's cloud image for base, which PinOrigin pins,
	// at UbuntuURL, where Ubuntu's dated releases are listed
	// (ubuntu.DefaultURL, unless a test build moves it). UbuntuRelease is
	// the dated release a test build pins in place of the newest: e2e's,
	// so its runs keep their images.
	Ubuntu        Finder
	UbuntuURL     string
	UbuntuRelease string
	// Memory returns the host's memory in bytes.
	Memory func() (uint64, error)
	// CPUs returns the host's CPU count.
	CPUs func() int
	// MacOS returns macOS's version, "" when unknown.
	MacOS func() string
	// KVM is Linux's KVM device: /dev/kvm, unless a test build stands in
	// for it.
	KVM string
	// DockerRunning reports whether Docker runs, for --from images only
	// it has.
	DockerRunning func(ctx context.Context) bool
	// User is the host's user, whom a --from base's conversion makes the
	// image's user (user.Current).
	User func() (*user.User, error)
	// Reporter is where the backend prints what it's doing, numbers its
	// logged commands' steps, and the run's cleanup stack. Lima's own
	// Warn says where the run log has more too.
	msg.Reporter
}

// Lima's roles, each with the Lima it's part of: its fields, and the
// other roles' methods.
type (
	host         struct{ *Lima } // backend.Host
	images       struct{ *Lima } // backend.Images
	machines     struct{ *Lima } // backend.Machines
	housekeeping struct{ *Lima } // backend.Housekeeping
)

var _ backend.Backend = (*Lima)(nil)

// withRoles returns l with its roles, each on l.
func withRoles(l *Lima) *Lima {
	l.host, l.images, l.machines, l.housekeeping = host{l}, images{l}, machines{l}, housekeeping{l}
	return l
}

// New returns the Lima backend for devvm's folders and its own files
// (assets), on r, sizing base to host, finding Ubuntu's cloud image at
// ubuntuURL (ubuntu.DefaultURL when it's "") through the company proxy cp,
// which builds go through too (nil for the one its Getenv and config.toml
// name, without config.toml's password).
func New(d paths.Dirs, assets fs.FS, r msg.Reporter, host platform.Host, ubuntuURL string, cp *settings.CompanyProxy) *Lima {
	ubuntuURL = strings.TrimRight(cmp.Or(ubuntuURL, ubuntu.DefaultURL), "/")
	self, err := os.Executable()
	if err == nil {
		if r, rerr := filepath.EvalSymlinks(self); rerr == nil {
			self = r
		}
	}
	finder := ubuntu.New(assets, ubuntuURL)
	if t := finder.Client.Transport(); t != nil {
		t.Proxy = cp.Route
	}
	l := withRoles(&Lima{
		Dirs: d, Runner: logRunner{r.LoggedCmd}, OS: runtime.GOOS, Arch: runtime.GOARCH, Company: cp, Ports: ports.Allocator{Dirs: d, Listening: ports.Listening},
		Self: self, Spawn: daemon.Spawn, Tick: 500 * time.Millisecond, Assets: assets, SSHTimeout: sshTimeout, DriverWait: driverWait, OCINote: 3 * time.Second, Getenv: os.Getenv,
		Ubuntu: finder, UbuntuURL: ubuntuURL, Memory: host.Memory, CPUs: host.CPUs,
		MacOS: platform.MacOSVersion, KVM: "/dev/kvm",
		DockerRunning: from.DockerRunning,
		User:          user.Current,
		Reporter:      r,
	})
	if l.Company == nil {
		l.Company = settings.NewCompanyProxy(d, func(k string) string { return l.Getenv(k) }, nil)
	}
	return l
}

// Warn prints a warning, and where the run log has more.
func (l *Lima) Warn(m msg.M) {
	l.Reporter.Warn(m)
	l.LogNote()
}

// Name returns lima.
func (l *Lima) Name() string { return Name }

// Origin is what Lima makes base from, which BuildImage is given back:
// the Ubuntu cloud image pinned on this machine (PinOrigin; "" before
// one is), which a --from conversion's VM boots too, with f, the
// conversion's scripts (convertSteps), then base's disk in GiB
// (backend.BaseDiskGiB), which limactl create gets as its --disk, and
// then base.yaml, the other settings limactl create reads. Base's CPUs
// and memory aren't in it: they're fitted to the host, and don't change
// what the image holds.
func (l *images) Origin(f *backend.From) ([]image.Step, error) {
	pin, _, err := l.pinned()
	if err != nil {
		return nil, err
	}
	steps := []image.Step{{Name: originUbuntu, Args: []string{pin}}}
	if f != nil {
		conv, err := l.convertSteps(f)
		if err != nil {
			return nil, err
		}
		steps = append(steps, conv...)
	}
	yaml, err := fs.ReadFile(l.Assets, originConfig)
	if err != nil {
		return nil, err
	}
	return append(steps,
		image.Step{Name: originDisk, Args: []string{gibString(backend.BaseDiskGiB)}},
		image.Step{Name: originConfig, Data: yaml}), nil
}

// The names of Lima's origin steps: the Ubuntu image's pin, base's disk,
// and base.yaml.
const (
	originUbuntu = "ubuntu-image"
	originDisk   = "disk"
	originConfig = "base.yaml"
)

// origin is base's origin steps as BuildImage runs them.
type origin struct {
	yaml       []byte       // base.yaml, for limactl create
	diskGiB    float64      // base's disk, limactl create's --disk
	conversion []image.Step // with --from, the conversion's (convertSteps); nil without
}

// originOf reads base's origin steps from its recipe, as Origin makes
// them: the Ubuntu image they pin must be the one pinned on this machine,
// and with f, the conversion's must be there, converting f.
func (l *images) originOf(steps []image.Step, f *backend.From) (origin, error) {
	want, ok, err := l.pinned()
	if err != nil {
		return origin{}, err
	}
	if !ok {
		return origin{}, errNoPin
	}
	var o origin
	pin := ""
	for _, s := range steps {
		switch {
		case s.Name == originUbuntu && len(s.Args) == 1:
			pin = s.Args[0]
		case s.Name == originDisk && len(s.Args) == 1:
			o.diskGiB, _ = strconv.ParseFloat(s.Args[0], 64)
		case s.Name == originConfig:
			o.yaml = s.Data
		case strings.HasPrefix(s.Name, "oci/"):
			o.conversion = append(o.conversion, s)
		}
	}
	switch {
	case pin != want:
		return origin{}, fmt.Errorf("lima: base's recipe pins Ubuntu %q, not %q", pin, want)
	case o.yaml == nil:
		return origin{}, errors.New("lima: base's recipe has no base.yaml")
	case o.diskGiB <= 0:
		return origin{}, errors.New("lima: base's recipe has no disk size")
	case (f != nil) != (o.conversion != nil):
		return origin{}, errors.New("lima: base's recipe has another conversion than its --from asks for")
	case f == nil:
		return o, nil
	}
	// The conversion's names become its files' names: only its own.
	names := make([]string, len(o.conversion))
	for i, s := range o.conversion {
		names[i] = s.Name
	}
	if args := o.conversion[0].Args; !slices.Equal(names, conversionScripts) || len(args) != convertArgs || args[0] != f.Digest || args[1] != f.Ref {
		return origin{}, fmt.Errorf("lima: base's recipe converts %q with %q", f.Ref, names)
	}
	return o, nil
}

// DiskDirs is devvm's Lima home, which holds every image's and VM's disk.
func (l *host) DiskDirs() []string { return []string{l.Dirs.Lima} }

// Floors is the free space Lima's steps want (backend.Space), measured
// with statfs on macOS (vz, APFS) in the e2e sandbox. Base took 1.1 GiB
// at its peak with Ubuntu's cloud image already in Lima's cache; the
// download adds up to 0.9 GiB, and every feature took base's disk from
// 2.6 to 5.1 GiB (du). The docker layer took 0.6 GiB, a VM up to 0.1 GiB
// with its first launch, and shared tools builds 0.35 GiB at their peak
// for claude, 2.1 for rust and 5.9 for all ten. On Linux hosts (QEMU, not
// measured here) an image's and a tools build VM's disks start as full
// copies of their parent's (2.0 GB as qcow2 without features), while a
// VM's is an overlay of its image's.
func (l *host) Floors() backend.Floors {
	f := backend.Floors{Base: 8 << 30, Image: 2 << 30, VM: 2 << 30, Tools: 3 << 30, PerTool: 512 << 20}
	if l.overlays() {
		f.Image += 6 << 30
		f.Tools += 6 << 30
	}
	return f
}

// Caps is what Lima has: everything, natively, but its ports, which
// devvm's forwarder forwards (ports.go), an emulation reviewed. A new
// feature is listed here by hand: the registry refuses Caps without it.
func (l *Lima) Caps() backend.Caps {
	return backend.Caps{
		backend.FeatureBuildImage:       backend.Native,
		backend.FeatureBuildConfinement: backend.Native,
		backend.FeatureFrom:             backend.Native,
		backend.FeatureClone:            backend.Native,
		backend.FeatureProjectMount:     backend.Native,
		backend.FeatureAgentFolders:     backend.Native,
		backend.FeatureToolsShare:       backend.Native,
		backend.FeatureEgressProxy:      backend.Native,
		backend.FeatureSSH:              backend.Native,
		backend.FeaturePorts:            backend.Emulated(true),
		backend.FeaturePortsAll:         backend.Emulated(true),
		backend.FeatureAgentForwarding:  backend.Native,
		backend.FeatureAgentToggle:      backend.Native,
		backend.FeatureCPUs:             backend.Native,
		backend.FeatureMemory:           backend.Native,
		backend.FeatureDisk:             backend.Native,
		backend.FeatureDockerLayer:      backend.Native,
		backend.FeatureNixLayer:         backend.Native,
		backend.FeatureLink:             backend.Native,
	}
}

// sshSocketExtra is what OpenSSH adds to the socket's name while making it.
const sshSocketExtra = 17

// NameMax is the longest VM name whose ssh socket fits: Lima keeps it in
// the VM's folder (<Lima home>/<vm>/ssh.sock) and refuses a path of
// paths.SocketMax characters or more, as the last byte is the NUL.
func (l *host) NameMax() int {
	return max(0, paths.SocketMax-1-sshSocketExtra-len(l.Dirs.Lima)-len("/")-len("/ssh.sock"))
}

// imageKey returns the key of an image's instance, and whether it is one.
// A folder named img-… gives devvm-img-…-<hash>, a project VM's name, so
// starting with ImagePrefix alone doesn't make an image.
func imageKey(name string) (string, bool) {
	key, ok := strings.CutPrefix(name, ImagePrefix)
	return key, ok && key != "" && !vmname.IsVM(name)
}

// Images lists the keys of the images in devvm's Lima home.
func (l *images) Images(ctx context.Context) ([]string, error) {
	ins, err := l.list(ctx)
	if err != nil {
		return nil, err
	}
	var keys []string
	for _, in := range ins {
		if k, ok := imageKey(in.Name); ok {
			keys = append(keys, k)
		}
	}
	return keys, nil
}

// ImageSize is the disk space of the image's instance folder: its disk
// and Lima's files. On macOS an image's disk is an APFS clone of its
// parent's, so the blocks they share count for both.
func (l *images) ImageSize(_ context.Context, key string) (int64, error) {
	name := ImagePrefix + key
	if _, ok := imageKey(name); !ok || strings.ContainsAny(key, "/\x00") {
		return 0, msg.NotAnImage(name)
	}
	return platform.DiskUsage(l.Dirs.LimaVM(name))
}

// DeleteImage deletes an image. On Linux hosts, an image whose disk
// project VMs are overlays of is refused.
func (l *images) DeleteImage(ctx context.Context, key string) error {
	name := ImagePrefix + key
	if _, ok := imageKey(name); !ok || strings.ContainsAny(key, "/\x00") {
		return msg.NotAnImage(name)
	}
	if _, err := l.inspect(ctx, name); errors.Is(err, backend.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	if l.overlays() {
		users, err := l.overlayUsers(ctx, name)
		if err != nil {
			return err
		}
		if len(users) > 0 {
			return msg.ImageInUse(key, strings.Join(users, " "))
		}
	}
	return l.delete(ctx, name)
}

// CreateVM clones the image's instance into a new, stopped VM. On Linux
// hosts the VM's disk is then an overlay of the image's.
func (l *machines) CreateVM(ctx context.Context, image string, spec backend.VMSpec) error {
	vm := spec.Name
	if len(vm) > l.NameMax() {
		return msg.LimaHomeTooLong(l.Dirs.Lima, vm)
	}
	src := ImagePrefix + image
	img, err := l.inspect(ctx, src)
	if errors.Is(err, backend.ErrNotFound) {
		return msg.ImageMissing(image)
	} else if err != nil {
		return err
	}
	// The clone has its image's driver, so its project mount is the one
	// its starts want (projectMount).
	var ms []mount
	if spec.Project != "" {
		ms = append(ms, projectMount(img.VMType, spec.Want))
	}
	ms = append(ms, readOnly(spec.AgentDirs)...)
	if spec.Tools != "" {
		ms = append(ms, mount{Location: spec.Tools, MountPoint: ptr(spec.ToolsAt), Writable: ptr(false)})
	}
	mounts, err := mountsSet(ms)
	if err != nil {
		return err
	}
	if backend.Status(img.Status) != backend.StatusStopped {
		if err := l.stop(ctx, src); err != nil {
			return err
		}
	}
	// --mount-none: the image's own mounts (the template's home folder)
	// would otherwise stay. Lima applies --set after it. Its environment
	// and the host's proxy settings too (the image's build turned those
	// off, and a clone keeps its config), so a start without network
	// policy edits nothing (Start).
	args := []string{
		"clone", "--tty=false", src, vm, "--mount-none",
		"--set", mounts,
		"--set", ".ssh.forwardAgent=" + strconv.FormatBool(spec.AgentForwarding),
		"--set", ".ssh.localPort=" + strconv.Itoa(spec.SSHPort),
		"--set", noForwardsSet(),
		"--set", ".propagateProxyEnv=true",
	}
	if len(spec.Env) > 0 {
		args = append(args, "--set", ".env = "+yqJSON(spec.Env))
	}
	if err := l.Runner.Run(ctx, msg.Creating(vm), l.limactl(ctx, args...)); err != nil {
		return backend.Logged(msg.VMCreateFailed(vm))
	}
	if l.overlays() {
		if err := l.overlay(ctx, src, vm); err != nil {
			return err
		}
	}
	// Its size, which the clone has from its image, where it differs:
	// what's left of the spec's (wantEdits), so its first start edits
	// nothing of it.
	in, err := l.inspect(ctx, vm)
	if err != nil {
		return err
	}
	opts, err := wantEdits(in, spec.Want)
	if err != nil || len(opts) == 0 {
		return err
	}
	return l.edit(ctx, vm, opts...)
}

// Stop stops a VM; a stopped one is left as it is. One Lima reports as
// broken, such as a VM whose start was cut short before its hostagent
// answers, is stopped with --force, as limactl stop refuses it. One whose
// hostagent is still starting its driver is waited for first (settled).
func (l *machines) Stop(ctx context.Context, vm string) error {
	in, err := l.settled(ctx, vm)
	if err != nil {
		return err
	}
	status := backend.Status(in.Status)
	if status == backend.StatusStopped {
		return nil
	}
	var opts []string
	if status == statusBroken {
		opts = []string{"--force"}
	}
	return l.stop(ctx, vm, opts...)
}

// statusBroken is how Lima reports a VM whose hostagent doesn't answer.
const statusBroken backend.Status = "Broken"

// driverPoll is how often settled looks at a VM whose hostagent is
// starting its driver.
const driverPoll = 100 * time.Millisecond

// settled inspects vm, waiting up to DriverWait while its hostagent runs
// without its driver yet: a start cut short, whose hostagent is still
// starting QEMU, which writes its pid file only once it runs. Stopped
// with --force then, the hostagent would go and the QEMU it started would
// run on without it, a VM the next start can't start.
func (l *machines) settled(ctx context.Context, vm string) (instance, error) {
	deadline := time.Now().Add(l.DriverWait)
	for {
		in, err := l.inspect(ctx, vm)
		if err != nil || backend.Status(in.Status) != statusBroken || in.HostAgentPID == 0 || in.DriverPID != 0 ||
			!time.Now().Before(deadline) {
			return in, err
		}
		select {
		case <-ctx.Done():
			return in, ctx.Err()
		case <-time.After(driverPoll):
		}
	}
}

func (l *Lima) stop(ctx context.Context, vm string, opts ...string) error {
	args := slices.Concat([]string{"stop"}, opts, []string{vm})
	if err := l.Runner.Run(ctx, msg.Stopping(vm), l.limactl(ctx, args...)); err != nil {
		return backend.Logged(msg.StopFailed(vm))
	}
	return nil
}

// Delete deletes a VM, stopping it first.
func (l *machines) Delete(ctx context.Context, vm string) error {
	if _, err := l.inspect(ctx, vm); err != nil {
		return err
	}
	return l.delete(ctx, vm)
}

// delete deletes a VM or an image's instance. A build VM (not a project
// VM: an image's, the tools build's) loses its whole folder in devvm's
// data folder too, its sandbox profile and records; a project VM's is its
// teardown's.
func (l *Lima) delete(ctx context.Context, vm string) error {
	if err := l.Runner.Run(ctx, msg.Deleting(shown(ctx, vm)), l.limactl(ctx, "delete", "-f", vm)); err != nil {
		return backend.Logged(msg.DeleteFailed(vm))
	}
	if vmname.IsVM(vm) {
		return nil
	}
	return os.RemoveAll(l.Dirs.VM(vm).Dir)
}

// Rename renames a stopped VM's instance (limactl rename), its disk and
// Lima's files with it; devvm's files for it keep the old name. A name too
// long for the VM's ssh socket is refused first.
func (l *machines) Rename(ctx context.Context, vm, to string) error {
	if len(to) > l.NameMax() {
		return msg.LimaHomeTooLong(l.Dirs.Lima, to)
	}
	in, err := l.inspect(ctx, vm)
	if err != nil {
		return err
	}
	// The remake stops it first.
	if backend.Status(in.Status) != backend.StatusStopped {
		return fmt.Errorf("lima: %s isn't stopped", vm)
	}
	if err := l.Runner.Run(ctx, msg.Renaming(to, vm), l.limactl(ctx, "rename", "--tty=false", vm, to)); err != nil {
		return backend.Logged(msg.RenameFailed(to, vm))
	}
	return nil
}

// List lists the VMs in devvm's Lima home, without images.
func (l *machines) List(ctx context.Context) ([]backend.Instance, error) {
	ins, err := l.list(ctx)
	if err != nil {
		return nil, err
	}
	var out []backend.Instance
	for _, in := range ins {
		if _, ok := imageKey(in.Name); !ok {
			out = append(out, in.backend())
		}
	}
	return out, nil
}

// Inspect returns one VM.
func (l *machines) Inspect(ctx context.Context, vm string) (backend.Instance, error) {
	in, err := l.inspect(ctx, vm)
	if err != nil {
		return backend.Instance{}, err
	}
	return in.backend(), nil
}

const gib = 1 << 30

func (in instance) size() backend.Size {
	return backend.Size{CPUs: in.CPUs, MemoryGiB: float64(in.Memory) / gib, DiskGiB: float64(in.Disk) / gib}
}

func (in instance) backend() backend.Instance {
	b := backend.Instance{Name: in.Name, Status: backend.Status(in.Status), Size: in.size()}
	for _, m := range in.Config.Mounts {
		switch {
		case m.agentDir():
			b.AgentDirs = append(b.AgentDirs, m.Location)
		case m.project():
			b.Project = m.at()
		}
	}
	b.AgentForwarding = in.Config.SSH.ForwardAgent != nil && *in.Config.SSH.ForwardAgent
	return b
}

// Exec runs argv in the VM, in o.Dir. limactl shell passes each of its
// arguments to the guest's shell quoted; the command devvm builds for it
// (the folder, then argv) is quoted word by word too.
func (l *machines) Exec(ctx context.Context, vm string, argv []string, o backend.ExecOpts) error {
	dir := o.Dir
	if dir == "" {
		dir = "/"
	}
	cmd := l.limactl(ctx, "shell", "--workdir", "/", vm, "sh", "-c", "cd "+shell.Quote(dir)+" && exec "+shell.QuoteJoin(argv))
	withAgent(cmd, o.Agent)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = o.Stdin, o.Stdout, o.Stderr
	return l.Runner.Attach(cmd)
}

// Shell returns limactl shell in o.Dir, carrying o.Agent, for the caller
// to run in its place.
func (l *machines) Shell(ctx context.Context, vm string, o backend.ShellOpts) (backend.Command, error) {
	cmd := l.limactl(ctx, "shell", "--workdir", o.Dir, vm)
	withAgent(cmd, o.Agent)
	if cmd.Err != nil {
		return backend.Command{}, cmd.Err
	}
	return backend.Command{Path: cmd.Path, Args: cmd.Args, Env: cmd.Env, Dir: cmd.Dir}, nil
}

// sshLogin is the options of Lima's ssh config, lowercase, that say how
// to log in. Anything else Lima (or a later version) writes there stays
// out: it turns host key checks off and shares one connection. Its first
// IdentityFile is the target's Key, apart.
var sshLogin = []string{
	"identitiesonly", "preferredauthentications", "batchmode",
	"compression", "gssapiauthentication", "ciphers",
}

// SSH returns how ssh reaches a running VM: the address, port, user, key
// (its first IdentityFile, Lima's _config/user) and login options in the
// ssh config Lima keeps for it.
func (l *machines) SSH(ctx context.Context, vm string) (backend.SSHTarget, error) {
	in, err := l.inspect(ctx, vm)
	if err != nil {
		return backend.SSHTarget{}, err
	}
	if backend.Status(in.Status) != backend.StatusRunning {
		return backend.SSHTarget{}, msg.VMNotRunning(vm)
	}
	t := backend.SSHTarget{Host: "127.0.0.1", Port: in.SSHLocalPort, ConfigFile: in.SSHConfigFile}
	if in.SSHConfigFile == "" {
		return t, nil
	}
	f, err := os.Open(in.SSHConfigFile)
	if err != nil {
		return backend.SSHTarget{}, err
	}
	defer func() { _ = f.Close() }()
	s := bufio.NewScanner(f)
	for s.Scan() {
		k, raw, _ := strings.Cut(strings.TrimSpace(s.Text()), " ")
		raw = strings.TrimSpace(raw)
		v := strings.Trim(raw, `"`)
		switch lk := strings.ToLower(k); {
		case lk == "hostname":
			t.Host = v
		case lk == "port":
			if p, err := strconv.Atoi(v); err == nil {
				t.Port = p
			}
		case lk == "user":
			t.User = v
		case lk == "identityfile":
			if t.Key == "" {
				t.Key = v
			}
		case slices.Contains(sshLogin, lk) && raw != "":
			t.Options = append(t.Options, backend.SSHOption{Key: k, Value: raw})
		}
	}
	return t, s.Err()
}

// wantEdits returns limactl edit's options that make a stopped VM (in,
// as limactl list has it) what w wants, none where it already is, after
// checking w (backend.Validate): its CPUs and memory where w sets them,
// its disk where w's is larger (Lima can't shrink one), its project
// folder's mount (a folder that moved) and its agent folder mounts,
// keeping its other mounts (the tools share), in one --set, and its
// agent forwarding.
func wantEdits(in instance, w backend.Want) ([]string, error) {
	if err := backend.Validate(w, in.backend()); err != nil {
		return nil, err
	}
	cur := in.size()
	var opts []string
	if s := w.Size; s.CPUs > 0 && s.CPUs != cur.CPUs {
		opts = append(opts, "--cpus", strconv.Itoa(s.CPUs))
	}
	if s := w.Size; s.MemoryGiB > 0 && s.MemoryGiB != cur.MemoryGiB {
		opts = append(opts, "--memory", gibString(s.MemoryGiB))
	}
	if s := w.Size; s.DiskGiB > cur.DiskGiB {
		opts = append(opts, "--disk", gibString(s.DiskGiB))
	}
	have := in.backend()
	ms, same := keptMounts(in, w)
	if !same || !slices.Equal(have.AgentDirs, w.AgentDirs) {
		set, err := mountsSet(append(ms, readOnly(w.AgentDirs)...))
		if err != nil {
			return nil, err
		}
		opts = append(opts, "--set", set)
	}
	if in.backend().AgentForwarding != w.AgentForwarding {
		opts = append(opts, "--set", ".ssh.forwardAgent="+strconv.FormatBool(w.AgentForwarding))
	}
	return opts, nil
}

// keptMounts is the mounts of in but its agent folders' that a VM as w
// wants keeps: its other mounts (the tools share) as they are, and the
// project mount w wants (projectMount), first when it's new. same: in's
// project mount is already that one, or neither has one.
func keptMounts(in instance, w backend.Want) (ms []mount, same bool) {
	want := projectMount(in.VMType, w)
	same = w.Project == "" && in.backend().Project == ""
	for _, m := range in.Config.Mounts {
		if m.project() && w.Project != "" && m.same(want) {
			same = true
		}
	}
	if w.Project != "" && !same {
		ms = append(ms, want)
	}
	for _, m := range in.Config.Mounts {
		if !m.agentDir() && (!m.project() || w.Project != "" && m.same(want)) {
			ms = append(ms, m)
		}
	}
	return ms, same
}

func gibString(g float64) string { return strconv.FormatFloat(g, 'g', -1, 64) }

// overlays reports whether project VMs' disks are overlays of their
// image's: on Linux hosts, where Lima's QEMU disks are qcow2 files and a
// clone would be a full copy. (On macOS clones are APFS copies, which
// share blocks already.)
func (l *Lima) overlays() bool { return l.OS == "linux" }

func (l *Lima) disk(vm string) string { return filepath.Join(l.Dirs.LimaVM(vm), "disk") }

func qemuImg(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "qemu-img", args...) //nolint:gosec // devvm's own arguments
}

// qemuImgInfo is what qemu-img info --output=json says about a disk.
type qemuImgInfo struct {
	Format  string `json:"format"`
	Backing string `json:"backing-filename"`
}

// diskInfo reads a disk's format and backing file, sharing it (-U): a
// running VM's QEMU holds its disk's write lock, which refuses qemu-img
// info otherwise. Neither changes while the VM runs.
func (l *Lima) diskInfo(ctx context.Context, path string) (qemuImgInfo, error) {
	var info qemuImgInfo
	out, err := l.Runner.Output(qemuImg(ctx, "info", "-U", "--output=json", path))
	if err != nil {
		return info, fmt.Errorf("qemu-img info %s: %w", path, err)
	}
	return info, json.Unmarshal(out, &info)
}

// overlay replaces vm's copy of src's disk with a qcow2 overlay of it: the
// image's blocks are shared and the VM's disk holds only its changes. A
// disk that isn't qcow2 (Lima's layout changed) is kept as it is.
func (l *machines) overlay(ctx context.Context, src, vm string) error {
	img, disk := l.disk(src), l.disk(vm)
	if fi, err := os.Stat(img); err != nil || !fi.Mode().IsRegular() {
		return nil //nolint:nilerr // no qcow2 disk to share: keep the copy
	}
	info, err := l.diskInfo(ctx, disk)
	if err != nil {
		return backend.Logged(msg.VMCreateFailed(vm))
	}
	if info.Format != "qcow2" {
		return nil
	}
	if err := os.Remove(disk); err != nil {
		return err
	}
	if err := l.Runner.Run(ctx, msg.M{}, qemuImg(ctx, "create", "-q", "-f", "qcow2", "-b", img, "-F", "qcow2", disk)); err != nil {
		return backend.Logged(msg.VMCreateFailed(vm))
	}
	return os.Chmod(disk, 0o600) // it may hold tokens: owner only
}

// overlayUsers returns the VMs whose disks are overlays of image's.
func (l *images) overlayUsers(ctx context.Context, image string) ([]string, error) {
	ins, err := l.list(ctx)
	if err != nil {
		return nil, err
	}
	var users []string
	for _, in := range ins {
		if _, ok := imageKey(in.Name); ok {
			continue
		}
		if _, err := os.Stat(l.disk(in.Name)); err != nil {
			continue
		}
		info, err := l.diskInfo(ctx, l.disk(in.Name))
		if err != nil {
			return nil, err
		}
		if info.Backing != "" && filepath.Clean(info.Backing) == l.disk(image) {
			users = append(users, in.Name)
		}
	}
	return users, nil
}
