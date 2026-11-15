// Package backendtest has a fake backend for tests: it records every call
// and answers from what the test preset.
package backendtest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/vmname"
)

// Call is one call to the fake: the method's name and its arguments after
// the context.
type Call struct {
	Method string
	Args   []any
}

// Fake is a backend that runs nothing. Set its fields before use: they're
// what it answers, and it keeps Instances and ImageKeys current as VMs and
// images are made, changed and deleted. Calls returns every call, in order.
// Its roles' methods are on types of their own over it, as Lima's are.
type Fake struct {
	host
	images
	machines
	housekeeping

	BackendName  string
	Capabilities backend.Caps
	NameLimit    int                            // NameMax's answer: CreateVM and Rename refuse longer names
	Instances    map[vmname.VM]backend.Instance // by name
	ImageKeys    []string
	// ImageVMSize is the size a VM CreateVM makes has where its Want
	// leaves it unset, its image's: New sets 4 CPUs, 8 GiB of memory and
	// a 100 GiB disk.
	ImageVMSize  backend.Size
	ImageSizes   map[string]int64  // ImageSize's answers, by key
	Built        backend.Built     // BuildImage's answer
	Target       backend.SSHTarget // SSH's answer, for a running VM
	ShellCmd     backend.Command   // Shell's answer
	Unconfined   bool              // Running's Confined is false (else: true, or for a VM Start started, whether with NetProxy)
	LifelineFile string            // Running's Lifeline
	Missing      []backend.Problem // Problems' answer
	Format       string            // ToolFormat's answer
	OriginSteps  []image.Step      // Origin's answer, for any base
	Pinned       string            // PinOrigin's answer
	// BuildErr, when set, is what each BuildImage fails with, if
	// anything, after recording the call.
	BuildErr func(backend.ImageSpec) error
	// StartErr, when set, is what each Start of a stopped VM fails with,
	// if anything, after recording the call, the VM left stopped: a
	// backend that stopped it again (a RemountError). It's called with
	// the fake locked.
	StartErr func(vm vmname.VM, w backend.Want) error
	// PackagesOut is the tar stream a build's packages give back, read by
	// its Take (none: empty); PackagesSent is what the last build's Send
	// wrote.
	PackagesOut, PackagesSent []byte
	// ExecFunc, when set, is what Exec returns after recording the call
	// (Errs aside).
	ExecFunc func(vm vmname.VM, argv []string, o backend.ExecOpts) error
	Errs     map[string]error // by method name: returned in place of its answer, changing nothing
	// ToolsBuild, when set, is what BuildTools does after recording the
	// call, in place of writing the images.
	ToolsBuild func(backend.ToolsSpec) error
	// ToolGlibc is the glibc version each tool's build says it needs, by
	// its name in the lock: none for a tool it doesn't name.
	ToolGlibc map[string]string

	mu     sync.Mutex
	calls  []Call
	nets   map[vmname.VM]backend.Net // by name: the network Start last started it with
	starts map[vmname.VM]int         // by name: how many starts it had
}

// The fake's roles, each with the Fake it's part of.
type (
	host         struct{ *Fake } // backend.Host
	images       struct{ *Fake } // backend.Images
	machines     struct{ *Fake } // backend.Machines
	housekeeping struct{ *Fake } // backend.Housekeeping
)

var _ backend.Backend = (*Fake)(nil)

// New returns a fake named name with these capabilities, listing every
// feature caps leaves out as None, as a backend's Caps list every one.
func New(name string, caps backend.Caps) *Fake {
	all := backend.Caps{}
	for _, f := range backend.Features {
		all[f] = caps[f]
	}
	maps.Copy(all, caps)
	f := &Fake{
		BackendName: name, Capabilities: all, Instances: map[vmname.VM]backend.Instance{},
		ImageVMSize: backend.Size{CPUs: 4, MemoryGiB: 8, DiskGiB: 100},
	}
	f.host, f.images, f.machines, f.housekeeping = host{f}, images{f}, machines{f}, housekeeping{f}
	return f
}

// Calls returns the calls so far.
func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// Methods returns the names of the methods called so far, in order.
func (f *Fake) Methods() []string {
	var ms []string
	for _, c := range f.Calls() {
		ms = append(ms, c.Method)
	}
	return ms
}

// record notes a call and returns the error preset for its method. The
// caller holds the lock.
func (f *Fake) record(method string, args ...any) error {
	f.calls = append(f.calls, Call{method, args})
	return f.Errs[method]
}

// change applies fn to an instance. The caller holds the lock.
func (f *Fake) change(vm vmname.VM, fn func(*backend.Instance)) error {
	in, ok := f.Instances[vm]
	if !ok {
		return fmt.Errorf("%s: %w", vm, backend.ErrNotFound)
	}
	fn(&in)
	f.Instances[vm] = in
	return nil
}

// Name returns BackendName.
func (f *Fake) Name() string { return f.BackendName }

// Caps returns Capabilities.
func (f *Fake) Caps() backend.Caps { return f.Capabilities }

// NameMax returns NameLimit.
func (f *host) NameMax() int { return f.NameLimit }

// Problems records the call and returns Missing.
func (f *host) Problems(context.Context) []backend.Problem {
	f.mu.Lock()
	defer f.mu.Unlock()
	_ = f.record("Problems")
	return slices.Clone(f.Missing)
}

// Location is "" and true: the fake's VMs can't move.
func (f *host) Location() (string, bool) { return "", true }

// DiskDirs is none: the fake writes no disks (Stored's do).
func (f *host) DiskDirs() []string { return nil }

// Floors is none.
func (f *host) Floors() backend.Floors { return backend.Floors{} }

// Origin is OriginSteps, none unless a test sets them, or Errs' error for
// it. It isn't recorded: every key asks it.
func (f *images) Origin(*backend.From) ([]image.Step, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.Errs["Origin"]; err != nil {
		return nil, err
	}
	return f.OriginSteps, nil
}

// PinOrigin records the call and returns Pinned, or Errs' error for it.
func (f *images) PinOrigin(_ context.Context, from *backend.From, how backend.Pin) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("PinOrigin", from, how); err != nil {
		return "", err
	}
	return f.Pinned, nil
}

// Usage is none: the fake can't tell what its VMs take.
func (f *housekeeping) Usage(context.Context) (backend.Usage, error) { return backend.Usage{}, nil }

// Strays is none.
func (f *housekeeping) Strays(context.Context) ([]backend.Stray, error) { return nil, nil }

// DevvmCache is none: the fake has no caches.
func (f *housekeeping) DevvmCache(context.Context) ([]backend.CacheEntry, error) { return nil, nil }

// DeleteCache records the call.
func (f *housekeeping) DeleteCache(_ context.Context, entries []backend.CacheEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.record("DeleteCache", entries)
}

// BuildImage records the call, adds the key to ImageKeys and returns Built.
func (f *images) BuildImage(_ context.Context, spec backend.ImageSpec) (backend.Built, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("BuildImage", spec); err != nil {
		return backend.Built{}, err
	}
	if f.BuildErr != nil {
		if err := f.BuildErr(spec); err != nil {
			return backend.Built{}, err
		}
	}
	if p := spec.Packages; p != nil {
		var sent bytes.Buffer
		if p.Send != nil {
			if err := p.Send(&sent); err != nil {
				return backend.Built{}, err
			}
		}
		f.PackagesSent = sent.Bytes()
		if p.Take != nil {
			if err := p.Take(bytes.NewReader(f.PackagesOut)); err != nil {
				return backend.Built{}, err
			}
		}
	}
	if !slices.Contains(f.ImageKeys, spec.Key) {
		f.ImageKeys = append(f.ImageKeys, spec.Key)
	}
	return f.Built, nil
}

// Images records the call and returns ImageKeys.
func (f *images) Images(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("Images"); err != nil {
		return nil, err
	}
	return slices.Clone(f.ImageKeys), nil
}

// ImageSize records the call and returns ImageSizes' size for the key, an
// error matching fs.ErrNotExist for a key ImageKeys lacks.
func (f *images) ImageSize(_ context.Context, key string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("ImageSize", key); err != nil {
		return 0, err
	}
	if !slices.Contains(f.ImageKeys, key) {
		return 0, fmt.Errorf("image %s: %w", key, fs.ErrNotExist)
	}
	return f.ImageSizes[key], nil
}

// DeleteImage records the call and drops the key from ImageKeys.
func (f *images) DeleteImage(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("DeleteImage", key); err != nil {
		return err
	}
	f.ImageKeys = slices.DeleteFunc(f.ImageKeys, func(k string) bool { return k == key })
	return nil
}

// ToolImage is a made-up tool image of file: "image FILE", padded to a
// page, as a real image is never shorter than its superblock's end (the
// shared tools count a shorter one as cut short).
func ToolImage(file string) []byte {
	b := make([]byte, 4096)
	copy(b, "image "+file+"\n")
	return b
}

// BuildTools records the call, then runs ToolsBuild on the spec, or
// writes each image its script would (every third argument, from the
// third) into spec.Out as ToolImage, with FILE.glibc, the tool's
// ToolGlibc.
func (f *images) BuildTools(_ context.Context, spec backend.ToolsSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("BuildTools", spec); err != nil {
		return err
	}
	if f.ToolsBuild != nil {
		return f.ToolsBuild(spec)
	}
	for i := 2; i < len(spec.Args); i += 3 {
		file := spec.Args[i]
		if err := state.WriteFile(filepath.Join(spec.Out, file), ToolImage(file)); err != nil {
			return err
		}
		var glibc []byte
		if v := f.ToolGlibc[spec.Args[i-2]]; v != "" {
			glibc = []byte(v + "\n")
		}
		if err := state.WriteFile(filepath.Join(spec.Out, file+".glibc"), glibc); err != nil {
			return err
		}
	}
	return nil
}

// ToolFormat returns Format.
func (f *images) ToolFormat() string { return f.Format }

// CreateVM records the call and adds a stopped instance, ImageVMSize's
// where its Want leaves its size unset. An image ImageKeys lacks, or a
// name longer than NameLimit, is refused.
func (f *machines) CreateVM(_ context.Context, image string, spec backend.VMSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("CreateVM", image, spec); err != nil {
		return err
	}
	switch {
	case !slices.Contains(f.ImageKeys, image):
		return msg.ImageMissing(image)
	case f.NameLimit > 0 && len(spec.Name) > f.NameLimit:
		return fmt.Errorf("%s: longer than %d", spec.Name, f.NameLimit)
	}
	if f.Instances == nil {
		f.Instances = map[vmname.VM]backend.Instance{}
	}
	in := backend.Instance{Name: spec.Name, Status: backend.StatusStopped, Size: f.ImageVMSize}
	backend.Apply(&in, spec.Want)
	f.Instances[spec.Name] = in
	f.setNet(spec.Name, backend.NetDirect) // never started confined
	return nil
}

// Start records the call and starts a stopped instance, made what w wants
// first (backend.Validate's refusal returned), on a start of its own,
// confined as c says. A running one is left as it is.
func (f *machines) Start(_ context.Context, vm vmname.VM, w backend.Want, c backend.Confinement) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("Start", vm, w, c); err != nil {
		return err
	}
	in, ok := f.Instances[vm]
	switch {
	case !ok:
		return fmt.Errorf("%s: %w", vm, backend.ErrNotFound)
	case in.Status == backend.StatusRunning:
		return nil
	}
	if err := backend.Validate(w, in); err != nil {
		return err
	}
	if f.StartErr != nil {
		if err := f.StartErr(vm, w); err != nil {
			return err
		}
	}
	backend.Apply(&in, w)
	in.Status = backend.StatusRunning
	f.Instances[vm] = in
	f.setNet(vm, c.Net)
	f.bump(vm)
	return nil
}

// Running records the call and returns LifelineFile as its Lifeline, a
// running instance's start as its ID ("start-N" for its Nth by Start,
// "start-0" for one preset running; Restart, a start outside Start, gives
// it another) and Confined: false with Unconfined; else, for a VM the
// fake made or started (CreateVM, Start, Restart), whether it runs on a
// start Start made with NetProxy, as a backend says. A VM a test preset
// running in Instances is confined unless Unconfined; a stopped one, or
// one the fake doesn't hold, never is, as for every backend.
func (f *machines) Running(_ context.Context, vm vmname.VM) (backend.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("Running", vm); err != nil {
		return backend.Run{}, err
	}
	in, ok := f.Instances[vm]
	running := ok && in.Status == backend.StatusRunning
	r := backend.Run{Lifeline: f.LifelineFile, Confined: running && !f.Unconfined}
	if n, ok := f.nets[vm]; ok && r.Confined {
		r.Confined = n == backend.NetProxy
	}
	if running {
		r.ID = "start-" + strconv.Itoa(f.starts[vm])
	}
	return r, nil
}

// Stop records the call and marks the instance stopped.
func (f *machines) Stop(_ context.Context, vm vmname.VM) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("Stop", vm); err != nil {
		return err
	}
	return f.change(vm, func(in *backend.Instance) { in.Status = backend.StatusStopped })
}

// Delete records the call and removes the instance.
func (f *machines) Delete(_ context.Context, vm vmname.VM) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("Delete", vm); err != nil {
		return err
	}
	delete(f.Instances, vm)
	return nil
}

// Rename records the call and gives the instance its new name; one that
// runs, a name an instance has or one longer than NameLimit is refused.
func (f *machines) Rename(_ context.Context, vm, to vmname.VM) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("Rename", vm, to); err != nil {
		return err
	}
	in, ok := f.Instances[vm]
	switch _, taken := f.Instances[to]; {
	case !ok:
		return fmt.Errorf("%s: %w", vm, backend.ErrNotFound)
	case in.Status != backend.StatusStopped:
		return fmt.Errorf("%s runs", vm)
	case taken:
		return fmt.Errorf("%s exists", to)
	case f.NameLimit > 0 && len(to) > f.NameLimit:
		return fmt.Errorf("%s: longer than %d", to, f.NameLimit)
	}
	delete(f.Instances, vm)
	in.Name = to
	f.Instances[to] = in
	if n, ok := f.nets[vm]; ok {
		f.setNet(to, n)
		delete(f.nets, vm)
	}
	return nil
}

// List records the call and returns Instances, sorted by name.
func (f *machines) List(context.Context) ([]backend.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("List"); err != nil {
		return nil, err
	}
	var ins []backend.Instance
	for _, name := range slices.Sorted(maps.Keys(f.Instances)) {
		ins = append(ins, f.Instances[name])
	}
	return ins, nil
}

// Inspect records the call and returns the instance.
func (f *machines) Inspect(_ context.Context, vm vmname.VM) (backend.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("Inspect", vm); err != nil {
		return backend.Instance{}, err
	}
	in, ok := f.Instances[vm]
	if !ok {
		return backend.Instance{}, fmt.Errorf("%s: %w", vm, backend.ErrNotFound)
	}
	return in, nil
}

// Exec records the call.
func (f *machines) Exec(_ context.Context, vm vmname.VM, argv []string, o backend.ExecOpts) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("Exec", vm, slices.Clone(argv), o); err != nil {
		return err
	}
	// A check that the project folder is mounted (vm's checkMounted) is
	// answered as a guest whose folder is mounted, unless ExecFunc
	// answers it itself.
	check := len(argv) == 3 && argv[0] == "sh" && strings.Contains(argv[2], "then echo devvm-mounted;") && o.Stdout != nil
	if f.ExecFunc == nil {
		if check {
			_, _ = io.WriteString(o.Stdout, "devvm-mounted\n")
		}
		return nil
	}
	w := &counted{w: o.Stdout}
	if o.Stdout != nil {
		o.Stdout = w
	}
	err := f.ExecFunc(vm, argv, o)
	if err == nil && check && w.n == 0 {
		_, _ = io.WriteString(w.w, "devvm-mounted\n")
	}
	return err
}

// counted is a writer that counts what's written through it.
type counted struct {
	w io.Writer
	n int
}

func (c *counted) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += n
	return n, err
}

// Restart stands for a start of vm outside devvm: it runs, on a start of
// its own, unconfined.
func (f *Fake) Restart(vm vmname.VM) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_ = f.change(vm, func(in *backend.Instance) { in.Status = backend.StatusRunning })
	f.setNet(vm, backend.NetDirect)
	f.bump(vm)
}

// setNet notes the network vm last started with. The caller holds the
// lock.
func (f *Fake) setNet(vm vmname.VM, n backend.Net) {
	if f.nets == nil {
		f.nets = map[vmname.VM]backend.Net{}
	}
	f.nets[vm] = n
}

// bump gives vm's next start. The caller holds the lock.
func (f *Fake) bump(vm vmname.VM) {
	if f.starts == nil {
		f.starts = map[vmname.VM]int{}
	}
	f.starts[vm]++
}

// ProxyURL returns the proxy at proxy.test on c's port, with its user and
// password.
func (f *machines) ProxyURL(c backend.Confinement) string {
	if !c.GuestProxy() {
		return ""
	}
	return backend.ProxyURL("proxy.test", c.ProxyPort, c.ProxyPassword)
}

// Shell records the call and returns ShellCmd.
func (f *machines) Shell(_ context.Context, vm vmname.VM, o backend.ShellOpts) (backend.Command, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("Shell", vm, o); err != nil {
		return backend.Command{}, err
	}
	return f.ShellCmd, nil
}

// SSH records the call and returns Target for a running instance.
func (f *machines) SSH(_ context.Context, vm vmname.VM) (backend.SSHTarget, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("SSH", vm); err != nil {
		return backend.SSHTarget{}, err
	}
	if in, ok := f.Instances[vm]; !ok || in.Status != backend.StatusRunning {
		return backend.SSHTarget{}, msg.VMNotRunning(vm)
	}
	return f.Target, nil
}
