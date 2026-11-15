package backendtest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/redact"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/vmname"
)

// Subject is a backend as Conformance checks it, which its factory makes
// new for each check: it holds the image Image and no VMs.
type Subject struct {
	Backend backend.Backend
	// Image is the key of an image the backend holds, which the checks
	// make their VMs from.
	Image string
	// Dir is a folder of the check's own on the host, for the folders its
	// VMs mount.
	Dir string
	// Boots is whether the backend's VMs start here. The checks that need
	// a running VM are skipped when they don't: the dummy instances real
	// Lima's tests make can't boot.
	Boots bool
	// Restart starts a running VM again outside Start, as a command of
	// the user's own would, on a start of its own; nil when the test
	// can't, which skips the check that needs it.
	Restart func(ctx context.Context, vm vmname.VM) error
}

// The VMs the checks make, as project VMs are named.
const (
	conformVM    vmname.VM = "devvm-conformance-0123456789abcd"
	conformOther vmname.VM = "devvm-other-0123456789abce"
	missingImage           = "fedcba987654"
)

// check is one part of backend.Backend's contract: run returns what the
// subject did wrong, or nil. needs is what it needs of the subject.
type check struct {
	name  string
	needs need
	run   func(ctx context.Context, s Subject) error
}

// need is what a check needs of its subject.
type need int

const (
	anySubject need = iota
	booting         // VMs that start (Boots)
	restarting      // VMs that start, and Restart
)

// skip returns why s can't have the check, or "" when it can.
func (n need) skip(s Subject) string {
	switch {
	case n >= booting && !s.Boots:
		return "this backend's VMs don't start here: the check needs a running VM"
	case n == restarting && s.Restart == nil:
		return "the test can't start this backend's VMs outside Start"
	}
	return ""
}

// Conformance checks that the backends factory makes keep the contract
// backend.Backend's documentation gives, each check a subtest on a
// subject of its own: every role's answers, the empty answers each role
// allows, how Running's ID, Confined and Lifeline follow a VM's starts,
// and the registry's guard over each role. A new backend's tests run it,
// on the backend itself; the checks that need a running VM are skipped,
// with the reason, when its VMs don't start (Subject.Boots).
func Conformance(t *testing.T, factory func(t *testing.T) Subject) {
	t.Helper()
	for _, c := range checks() {
		t.Run(c.name, func(t *testing.T) {
			s := factory(t)
			if why := c.needs.skip(s); why != "" {
				t.Skip(why)
			}
			if err := c.run(t.Context(), s); err != nil {
				t.Error(err)
			}
		})
	}
}

// checks is every check, by role.
func checks() []check {
	return slices.Concat(hostChecks(), imageChecks(), machineChecks(), runningChecks(), housekeepingChecks(), []check{
		{"guard", anySubject, checkGuard},
	})
}

func hostChecks() []check {
	return []check{
		{"caps", anySubject, checkCaps},
		{"problems", anySubject, checkProblems},
		{"host", anySubject, checkHost},
	}
}

func imageChecks() []check {
	return []check{
		{"images", anySubject, checkImages},
		{"delete-image", anySubject, checkDeleteImage},
		{"origin", anySubject, checkOrigin},
		{"build-image", booting, checkBuildImage},
	}
}

func machineChecks() []check {
	return []check{
		{"create", anySubject, checkCreate},
		{"create-refused", anySubject, checkCreateRefused},
		{"inspect-missing", anySubject, checkInspectMissing},
		{"stopped", anySubject, checkStopped},
		{"start-shrink", anySubject, checkStartShrink},
		{"proxy-url", anySubject, checkProxyURL},
		{"rename", anySubject, checkRename},
		{"delete", anySubject, checkDelete},
	}
}

func runningChecks() []check {
	return []check{
		{"start", booting, checkStart},
		{"confined", booting, checkConfined},
		{"restart", restarting, checkRestart},
		{"reach", booting, checkReach},
		{"running-refused", booting, checkRunningRefused},
	}
}

func housekeepingChecks() []check {
	return []check{
		{"usage", anySubject, checkUsage},
		{"strays", anySubject, checkStrays},
		{"caches", anySubject, checkCaches},
	}
}

// spec is the VM the checks make, named name: the project folder, an
// agent's folder and forwarding, and a size whose disk is the image's.
func spec(s Subject, name vmname.VM) (backend.VMSpec, error) {
	project, agent := filepath.Join(s.Dir, "work", "project"), filepath.Join(s.Dir, ".claude")
	for _, d := range []string{project, agent} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return backend.VMSpec{}, err
		}
	}
	return backend.VMSpec{Name: name, Want: backend.Want{
		Size: backend.Size{CPUs: 3, MemoryGiB: 5}, Project: project, AgentDirs: []string{agent}, AgentForwarding: true,
	}}, nil
}

// create makes the checks' VM, named name, and returns its spec.
func create(ctx context.Context, s Subject, name vmname.VM) (backend.VMSpec, error) {
	sp, err := spec(s, name)
	if err != nil {
		return sp, err
	}
	if err := s.Backend.CreateVM(ctx, s.Image, sp); err != nil {
		return sp, fmt.Errorf("CreateVM: %w", err)
	}
	return sp, nil
}

// names returns the names of the backend's VMs.
func names(ctx context.Context, b backend.Backend) ([]vmname.VM, error) {
	ins, err := b.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("List: %w", err)
	}
	var out []vmname.VM
	for _, in := range ins {
		out = append(out, in.Name)
	}
	return out, nil
}

// holds reports whether the backend holds the image.
func holds(ctx context.Context, b backend.Backend, key string) (bool, error) {
	keys, err := b.Images(ctx)
	if err != nil {
		return false, fmt.Errorf("Images: %w", err)
	}
	return slices.Contains(keys, key), nil
}

// The Caps list every feature, as the registry wants them.
func checkCaps(_ context.Context, s Subject) error {
	if err := s.Backend.Caps().Check(); err != nil {
		return fmt.Errorf("Caps: %w", err)
	}
	if _, err := backend.NewRegistry(s.Backend); err != nil {
		return fmt.Errorf("the registry refuses it: %w", err)
	}
	return nil
}

// Each problem says what it is and what's missing, and Check gives the
// first.
func checkProblems(ctx context.Context, s Subject) error {
	ps := s.Backend.Problems(ctx)
	for _, p := range ps {
		if p.Key == "" || msg.IDOf(p.Msg) == "" {
			return fmt.Errorf("a problem without its key or message: %+v", p)
		}
	}
	err := backend.Check(ctx, s.Backend)
	if len(ps) == 0 && err != nil || len(ps) > 0 && msg.NamesOf(err) != ps[0].Msg.Names() {
		return fmt.Errorf("backend.Check gives %w for problems %+v", err, ps)
	}
	return nil
}

// The host's answers: a name limit or none, a location that's the
// default when there's none, and absolute disk folders.
func checkHost(_ context.Context, s Subject) error {
	if n := s.Backend.NameMax(); n < 0 {
		return fmt.Errorf("NameMax %d", n)
	}
	if where, isDefault := s.Backend.Location(); where == "" && !isDefault {
		return errors.New(`Location: "" isn't the default`)
	}
	for _, d := range s.Backend.DiskDirs() {
		if !filepath.IsAbs(d) {
			return fmt.Errorf("DiskDirs: %q isn't absolute", d)
		}
	}
	return nil
}

// The backend lists the image it holds, which has a size, and a missing
// one's size is fs.ErrNotExist's; deleting a missing one is no error.
func checkImages(ctx context.Context, s Subject) error {
	if ok, err := holds(ctx, s.Backend, s.Image); err != nil || !ok {
		return fmt.Errorf("Images doesn't list %s: %w", s.Image, err)
	}
	if n, err := s.Backend.ImageSize(ctx, s.Image); err != nil || n < 0 {
		return fmt.Errorf("ImageSize(%s): %d, %w", s.Image, n, err)
	}
	if _, err := s.Backend.ImageSize(ctx, missingImage); !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("ImageSize of a missing image: %w, want fs.ErrNotExist", err)
	}
	if err := s.Backend.DeleteImage(ctx, missingImage); err != nil {
		return fmt.Errorf("DeleteImage of a missing image: %w", err)
	}
	return nil
}

// A deleted image is gone, and deleting it again is no error.
func checkDeleteImage(ctx context.Context, s Subject) error {
	for range 2 {
		if err := s.Backend.DeleteImage(ctx, s.Image); err != nil {
			return fmt.Errorf("DeleteImage: %w", err)
		}
	}
	if ok, err := holds(ctx, s.Backend, s.Image); err != nil || ok {
		return fmt.Errorf("Images lists the deleted image: %w", err)
	}
	if _, err := s.Backend.ImageSize(ctx, s.Image); !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("ImageSize of the deleted image: %w, want fs.ErrNotExist", err)
	}
	return nil
}

// Base's origin steps, from Ubuntu or a container image, each have a name.
func checkOrigin(_ context.Context, s Subject) error {
	for _, f := range []*backend.From{nil, {Ref: "debian:12", Digest: "debian@sha256:" + strings.Repeat("a", 64)}} {
		steps, err := s.Backend.Origin(f)
		if err != nil {
			return fmt.Errorf("Origin(%v): %w", f, err)
		}
		for _, st := range steps {
			if st.Name == "" {
				return errors.New("Origin: a step without a name")
			}
		}
	}
	return nil
}

// A layer built on the image is held under its key, its recipe's steps run
// with their arguments, one as root (a step that fails fails the build).
func checkBuildImage(ctx context.Context, s Subject) error {
	const key = "0123456789fe"
	sp := backend.ImageSpec{
		Image: "base+conformance", Key: key, Parent: s.Image, Net: backend.NetNone,
		Recipe: image.Recipe{Steps: []image.Step{
			{Name: "conformance.sh", Data: []byte("test \"$1 $2\" = 'an arg'\n"), Args: []string{"an", "arg"}},
			{Name: "conformance-root.sh", Data: []byte("test \"$(id -u)\" = 0\n"), Root: true},
		}},
	}
	if _, err := s.Backend.BuildImage(ctx, sp); err != nil {
		return fmt.Errorf("BuildImage: %w", err)
	}
	if ok, err := holds(ctx, s.Backend, key); err != nil || !ok {
		return fmt.Errorf("Images doesn't list the built image: %w", err)
	}
	return nil
}

// A new VM is stopped, as its spec wants it, with its whole size, and
// listed alone, without the images.
func checkCreate(ctx context.Context, s Subject) error {
	sp, err := create(ctx, s, conformVM)
	if err != nil {
		return err
	}
	in, err := s.Backend.Inspect(ctx, conformVM)
	if err != nil {
		return fmt.Errorf("Inspect: %w", err)
	}
	return errors.Join(
		wantProblem(in, sp.Want),
		statusProblem(in, backend.StatusStopped),
		listProblem(ctx, s, conformVM),
	)
}

// wantProblem says how in isn't as w wants it, its size all set.
func wantProblem(in backend.Instance, w backend.Want) error {
	sz := in.Size
	switch {
	case sz.CPUs <= 0 || sz.MemoryGiB <= 0 || sz.DiskGiB <= 0:
		return fmt.Errorf("%s's size isn't all set: %+v", in.Name, sz)
	case w.Size.CPUs > 0 && sz.CPUs != w.Size.CPUs, w.Size.MemoryGiB > 0 && sz.MemoryGiB != w.Size.MemoryGiB,
		w.Size.DiskGiB > 0 && sz.DiskGiB != w.Size.DiskGiB:
		return fmt.Errorf("%s's size is %+v, want %+v", in.Name, sz, w.Size)
	case in.Project != w.Project:
		return fmt.Errorf("%s mounts the project folder %q, want %q", in.Name, in.Project, w.Project)
	case !slices.Equal(in.AgentDirs, w.AgentDirs) || in.AgentForwarding != w.AgentForwarding:
		return fmt.Errorf("%s has agent folders %q and forwarding %v, want %q and %v",
			in.Name, in.AgentDirs, in.AgentForwarding, w.AgentDirs, w.AgentForwarding)
	}
	return nil
}

// statusProblem says how in hasn't the status.
func statusProblem(in backend.Instance, want backend.Status) error {
	if in.Name == "" || in.Status != want {
		return fmt.Errorf("%q is %s, want %s", in.Name, in.Status, want)
	}
	return nil
}

// listProblem says how List doesn't list just the VMs.
func listProblem(ctx context.Context, s Subject, vms ...vmname.VM) error {
	got, err := names(ctx, s.Backend)
	if err != nil {
		return err
	}
	if slices.Sort(got); !slices.Equal(got, slices.Sorted(slices.Values(vms))) {
		return fmt.Errorf("List lists %q, want %q", got, vms)
	}
	return nil
}

// A VM from a missing image, or with a name longer than NameMax, is
// refused, and no VM is made.
func checkCreateRefused(ctx context.Context, s Subject) error {
	sp, err := spec(s, conformVM)
	if err != nil {
		return err
	}
	if err := s.Backend.CreateVM(ctx, missingImage, sp); err == nil {
		return errors.New("CreateVM from a missing image")
	}
	if n := s.Backend.NameMax(); n > 0 {
		long := sp
		long.Name, _ = vmname.ParseListed(strings.Repeat("x", n+1))
		if err := s.Backend.CreateVM(ctx, s.Image, long); err == nil {
			return fmt.Errorf("CreateVM of a name longer than NameMax (%d)", n)
		}
	}
	return listProblem(ctx, s)
}

// A missing VM is ErrNotFound.
func checkInspectMissing(ctx context.Context, s Subject) error {
	if _, err := s.Backend.Inspect(ctx, conformVM); !errors.Is(err, backend.ErrNotFound) {
		return fmt.Errorf("Inspect of a missing VM: %w, want backend.ErrNotFound", err)
	}
	return nil
}

// A stopped VM has its lifeline, no start and no confinement, nothing to
// reach, and stopping it again changes nothing.
func checkStopped(ctx context.Context, s Subject) error {
	if _, err := create(ctx, s, conformVM); err != nil {
		return err
	}
	if err := s.Backend.Stop(ctx, conformVM); err != nil {
		return fmt.Errorf("Stop of a stopped VM: %w", err)
	}
	if err := stoppedProblem(ctx, s.Backend, conformVM); err != nil {
		return err
	}
	if _, err := s.Backend.SSH(ctx, conformVM); err == nil {
		return errors.New("SSH reaches a stopped VM")
	}
	return nil
}

// stoppedProblem says how vm isn't stopped, as Inspect and Running say.
func stoppedProblem(ctx context.Context, b backend.Backend, vm vmname.VM) error {
	in, err := b.Inspect(ctx, vm)
	if err != nil {
		return fmt.Errorf("Inspect: %w", err)
	}
	if err := statusProblem(in, backend.StatusStopped); err != nil {
		return err
	}
	r, err := b.Running(ctx, vm)
	switch {
	case err != nil:
		return fmt.Errorf("Running: %w", err)
	case r.ID != "" || r.Confined:
		return fmt.Errorf("Running of a stopped VM: %+v, want no ID and not confined", r)
	case !filepath.IsAbs(r.Lifeline):
		return fmt.Errorf("Running of a stopped VM: lifeline %q, want a file", r.Lifeline)
	}
	return nil
}

// A start that would shrink a stopped VM's disk is refused with
// disk_shrink, changing nothing.
func checkStartShrink(ctx context.Context, s Subject) error {
	if _, err := create(ctx, s, conformVM); err != nil {
		return err
	}
	in, err := s.Backend.Inspect(ctx, conformVM)
	if err != nil {
		return fmt.Errorf("Inspect: %w", err)
	}
	w := backend.Want{Size: backend.Size{DiskGiB: in.Size.DiskGiB / 2}, Project: in.Project, AgentDirs: in.AgentDirs, AgentForwarding: in.AgentForwarding}
	if err := s.Backend.Start(ctx, conformVM, w, backend.Confinement{}); msg.IDOf(err) != "disk_shrink" {
		return fmt.Errorf("Start with a smaller disk: %w, want disk_shrink", err)
	}
	after, err := s.Backend.Inspect(ctx, conformVM)
	if err != nil {
		return fmt.Errorf("Inspect: %w", err)
	}
	if after.Size != in.Size {
		return fmt.Errorf("the refused start changed its size: %+v, was %+v", after.Size, in.Size)
	}
	return stoppedProblem(ctx, s.Backend, conformVM)
}

// The proxy's URL carries its user and the VM's password, for NetProxy
// and for NetDirect with a proxy port (an open VM's) alone.
func checkProxyURL(_ context.Context, s Subject) error {
	for _, c := range []backend.Confinement{
		{Net: backend.NetDirect},
		{Net: backend.NetInternet, ProxyPort: 3128, ProxyPassword: redact.NewSecret("pw")},
		{Net: backend.NetNone, ProxyPort: 3128, ProxyPassword: redact.NewSecret("pw")},
	} {
		if u := s.Backend.ProxyURL(c); u != "" {
			return fmt.Errorf("ProxyURL with network %d: %q", c.Net, u)
		}
	}
	const password = "0123456789abcdef"
	for _, n := range []backend.Net{backend.NetProxy, backend.NetDirect} {
		raw := s.Backend.ProxyURL(backend.Confinement{Net: n, ProxyPort: 3128, ProxyPassword: redact.NewSecret(password)})
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return fmt.Errorf("ProxyURL with network %d: %q: %w", n, raw, err)
		}
		if pw, _ := u.User.Password(); u.User.Username() != backend.ProxyUser || pw != password {
			return fmt.Errorf("ProxyURL with network %d: %q, want user %s and the password", n, raw, backend.ProxyUser)
		}
	}
	return nil
}

// A stopped VM renamed is the same VM under its new name; a name another
// VM has, or one longer than NameMax, is refused.
func checkRename(ctx context.Context, s Subject) error {
	for _, vm := range []vmname.VM{conformVM, conformOther} {
		if _, err := create(ctx, s, vm); err != nil {
			return err
		}
	}
	before, err := s.Backend.Inspect(ctx, conformVM)
	if err != nil {
		return fmt.Errorf("Inspect: %w", err)
	}
	if err := s.Backend.Rename(ctx, conformVM, conformOther); err == nil {
		return errors.New("Rename to a name another VM has")
	}
	if n := s.Backend.NameMax(); n > 0 {
		long, _ := vmname.ParseListed(strings.Repeat("x", n+1))
		if err := s.Backend.Rename(ctx, conformVM, long); err == nil {
			return fmt.Errorf("Rename to a name longer than NameMax (%d)", n)
		}
	}
	const to = "devvm-renamed-0123456789abcf"
	if err := s.Backend.Rename(ctx, conformVM, to); err != nil {
		return fmt.Errorf("Rename: %w", err)
	}
	if _, err := s.Backend.Inspect(ctx, conformVM); !errors.Is(err, backend.ErrNotFound) {
		return fmt.Errorf("Inspect of the old name: %w, want backend.ErrNotFound", err)
	}
	after, err := s.Backend.Inspect(ctx, to)
	if err != nil {
		return fmt.Errorf("Inspect of the new name: %w", err)
	}
	if after.Name != to || after.Size != before.Size || !slices.Equal(after.AgentDirs, before.AgentDirs) || after.Status != before.Status {
		return fmt.Errorf("renamed, it's %+v, was %+v", after, before)
	}
	return listProblem(ctx, s, to, conformOther)
}

// A deleted VM is gone, and its image stays.
func checkDelete(ctx context.Context, s Subject) error {
	if _, err := create(ctx, s, conformVM); err != nil {
		return err
	}
	if err := s.Backend.Delete(ctx, conformVM); err != nil {
		return fmt.Errorf("Delete: %w", err)
	}
	if _, err := s.Backend.Inspect(ctx, conformVM); !errors.Is(err, backend.ErrNotFound) {
		return fmt.Errorf("Inspect of a deleted VM: %w, want backend.ErrNotFound", err)
	}
	if ok, err := holds(ctx, s.Backend, s.Image); err != nil || !ok {
		return fmt.Errorf("deleting a VM deleted its image: %w", err)
	}
	return listProblem(ctx, s)
}

// started starts the checks' VM as w wants it, confined as c says, and
// returns how it runs.
func started(ctx context.Context, b backend.Backend, w backend.Want, c backend.Confinement) (backend.Run, error) {
	if err := b.Start(ctx, conformVM, w, c); err != nil {
		return backend.Run{}, fmt.Errorf("Start: %w", err)
	}
	in, err := b.Inspect(ctx, conformVM)
	if err != nil {
		return backend.Run{}, fmt.Errorf("Inspect: %w", err)
	}
	if err := statusProblem(in, backend.StatusRunning); err != nil {
		return backend.Run{}, err
	}
	r, err := b.Running(ctx, conformVM)
	switch {
	case err != nil:
		return r, fmt.Errorf("Running: %w", err)
	case r.ID == "" || !filepath.IsAbs(r.Lifeline):
		return r, fmt.Errorf("Running of a running VM: %+v, want its start's ID and its lifeline", r)
	}
	return r, nil
}

// A start makes a stopped VM what its Want says, its project folder moved
// too; a start of a running one leaves it as it is, on the same start,
// and Stop stops it.
func checkStart(ctx context.Context, s Subject) error {
	sp, err := create(ctx, s, conformVM)
	if err != nil {
		return err
	}
	moved := filepath.Join(s.Dir, "work", "moved")
	if err := os.MkdirAll(moved, 0o700); err != nil {
		return err
	}
	w := backend.Want{Size: backend.Size{CPUs: 2, MemoryGiB: 4}, Project: moved, AgentForwarding: !sp.AgentForwarding}
	first, err := started(ctx, s.Backend, w, backend.Confinement{})
	if err != nil {
		return err
	}
	in, err := s.Backend.Inspect(ctx, conformVM)
	if err != nil {
		return fmt.Errorf("Inspect: %w", err)
	}
	if err := wantProblem(in, w); err != nil {
		return fmt.Errorf("started: %w", err)
	}
	again, err := started(ctx, s.Backend, sp.Want, backend.Confinement{})
	if err != nil {
		return err
	}
	if after, err := s.Backend.Inspect(ctx, conformVM); err != nil || wantProblem(after, w) != nil || again.ID != first.ID {
		return fmt.Errorf("a start of a running VM changed it: %+v, %w (start %q, was %q)", after, err, again.ID, first.ID)
	}
	if err := s.Backend.Stop(ctx, conformVM); err != nil {
		return fmt.Errorf("Stop: %w", err)
	}
	return stoppedProblem(ctx, s.Backend, conformVM)
}

// A VM is confined only once Start started it with NetProxy, and each
// start has another ID.
func checkConfined(ctx context.Context, s Subject) error {
	sp, err := create(ctx, s, conformVM)
	if err != nil {
		return err
	}
	var ids []string
	for _, n := range []backend.Net{backend.NetDirect, backend.NetProxy, backend.NetDirect} {
		c := backend.Confinement{Net: n}
		if n == backend.NetProxy {
			c.ProxyPort, c.ProxyPassword = 3128, redact.NewSecret("0123456789abcdef")
		}
		r, err := started(ctx, s.Backend, sp.Want, c)
		if err != nil {
			return err
		}
		if r.Confined != (n == backend.NetProxy) || slices.Contains(ids, r.ID) {
			return fmt.Errorf("started with network %d: %+v, after starts %q", n, r, ids)
		}
		ids = append(ids, r.ID)
		if err := s.Backend.Stop(ctx, conformVM); err != nil {
			return fmt.Errorf("Stop: %w", err)
		}
		if err := stoppedProblem(ctx, s.Backend, conformVM); err != nil {
			return err
		}
	}
	return nil
}

// A VM started again outside Start is on another start, unconfined.
func checkRestart(ctx context.Context, s Subject) error {
	sp, err := create(ctx, s, conformVM)
	if err != nil {
		return err
	}
	c := backend.Confinement{Net: backend.NetProxy, ProxyPort: 3128, ProxyPassword: redact.NewSecret("0123456789abcdef")}
	r, err := started(ctx, s.Backend, sp.Want, c)
	if err != nil {
		return err
	}
	if err := s.Restart(ctx, conformVM); err != nil {
		return fmt.Errorf("Restart: %w", err)
	}
	after, err := s.Backend.Running(ctx, conformVM)
	if err != nil || after.ID == "" || after.ID == r.ID || after.Confined {
		return fmt.Errorf("restarted outside Start: %+v, %w (was %+v)", after, err, r)
	}
	return nil
}

// A running VM is reached by ssh (which the port forwarder and links use
// too), by a shell and by Exec.
func checkReach(ctx context.Context, s Subject) error {
	sp, err := create(ctx, s, conformVM)
	if err != nil {
		return err
	}
	if _, err := started(ctx, s.Backend, sp.Want, backend.Confinement{}); err != nil {
		return err
	}
	if t, err := s.Backend.SSH(ctx, conformVM); err != nil || t.Host == "" || t.Port <= 0 || t.User == "" || t.Key == "" {
		return fmt.Errorf("SSH: %+v, %w", t, err)
	}
	if c, err := s.Backend.Shell(ctx, conformVM, backend.ShellOpts{Dir: "/"}); err != nil || c.Path == "" || len(c.Args) == 0 {
		return fmt.Errorf("Shell: %+v, %w", c, err)
	}
	if err := s.Backend.Exec(ctx, conformVM, []string{"true"}, backend.ExecOpts{}); err != nil {
		return fmt.Errorf("Exec: %w", err)
	}
	return nil
}

// A running VM isn't renamed, and Delete stops and deletes it.
func checkRunningRefused(ctx context.Context, s Subject) error {
	sp, err := create(ctx, s, conformVM)
	if err != nil {
		return err
	}
	if _, err := started(ctx, s.Backend, sp.Want, backend.Confinement{}); err != nil {
		return err
	}
	if err := s.Backend.Rename(ctx, conformVM, conformOther); err == nil {
		return errors.New("Rename of a running VM")
	}
	if err := listProblem(ctx, s, conformVM); err != nil {
		return err
	}
	if err := s.Backend.Delete(ctx, conformVM); err != nil {
		return fmt.Errorf("Delete of a running VM: %w", err)
	}
	return listProblem(ctx, s)
}

// Usage sizes only the backend's VMs, and its caches.
func checkUsage(ctx context.Context, s Subject) error {
	if _, err := create(ctx, s, conformVM); err != nil {
		return err
	}
	u, err := s.Backend.Usage(ctx)
	if err != nil {
		return fmt.Errorf("Usage: %w", err)
	}
	for vm, n := range u.VMs {
		if vm != conformVM || n < 0 {
			return fmt.Errorf("Usage: %s takes %d", vm, n)
		}
	}
	for _, c := range u.Caches {
		if c.Name == "" || c.Count < 0 || c.Size < 0 || c.Size > 0 && !c.Sized {
			return fmt.Errorf("Usage: a cache %+v", c)
		}
	}
	return nil
}

// A stray is one of the backend's VMs, with the data folder it names.
func checkStrays(ctx context.Context, s Subject) error {
	if _, err := create(ctx, s, conformVM); err != nil {
		return err
	}
	ss, err := s.Backend.Strays(ctx)
	if err != nil {
		return fmt.Errorf("Strays: %w", err)
	}
	for _, st := range ss {
		if st.Name != conformVM || st.Data == "" {
			return fmt.Errorf("Strays: %+v", st)
		}
	}
	return nil
}

// devvm's caches are files, and DeleteCache leaves an entry that isn't
// one of them.
func checkCaches(ctx context.Context, s Subject) error {
	es, err := s.Backend.DevvmCache(ctx)
	if err != nil {
		return fmt.Errorf("DevvmCache: %w", err)
	}
	for _, e := range es {
		if e.What == "" || !filepath.IsAbs(e.Path) || e.Size < 0 {
			return fmt.Errorf("DevvmCache: %+v", e)
		}
	}
	mine := filepath.Join(s.Dir, "download")
	if err := state.WriteFile(mine, []byte("not the backend's\n")); err != nil {
		return err
	}
	if err := s.Backend.DeleteCache(ctx, []backend.CacheEntry{{What: "a download", Path: mine}}); err != nil {
		return fmt.Errorf("DeleteCache: %w", err)
	}
	if _, err := os.Stat(mine); err != nil {
		return fmt.Errorf("DeleteCache deleted a file that isn't the backend's: %w", err)
	}
	return nil
}

// lacking is a backend whose Caps lack clone, disk and build-image.
type lacking struct{ backend.Backend }

func (l lacking) Caps() backend.Caps {
	caps := maps.Clone(l.Backend.Caps())
	for _, f := range []backend.Feature{backend.FeatureClone, backend.FeatureDisk, backend.FeatureBuildImage} {
		caps[f] = backend.None
	}
	return caps
}

// The registry's guard refuses what the backend lacks, before the
// backend sees it, used as Images or Machines alone.
func checkGuard(ctx context.Context, s Subject) error {
	r, err := backend.NewRegistry(lacking{s.Backend})
	if err != nil {
		return fmt.Errorf("the registry refuses it: %w", err)
	}
	g, _ := r.Lookup(s.Backend.Name())
	var (
		ms backend.Machines = g
		im backend.Images   = g
	)
	sp, err := spec(s, conformVM)
	if err != nil {
		return err
	}
	if err := ms.CreateVM(ctx, s.Image, sp); msg.IDOf(err) != "backend_lacks" {
		return fmt.Errorf("Machines.CreateVM without clone: %w, want backend_lacks", err)
	}
	if err := listProblem(ctx, s); err != nil {
		return err
	}
	if _, err := create(ctx, s, conformVM); err != nil {
		return err
	}
	w := backend.Want{Size: backend.Size{DiskGiB: 4096}, Project: sp.Project, AgentDirs: sp.AgentDirs, AgentForwarding: sp.AgentForwarding}
	if err := ms.Start(ctx, conformVM, w, backend.Confinement{}); msg.IDOf(err) != "backend_lacks" {
		return fmt.Errorf("Machines.Start with a disk, without disk: %w, want backend_lacks", err)
	}
	if err := stoppedProblem(ctx, s.Backend, conformVM); err != nil {
		return err
	}
	return guardedBuilds(ctx, s, im)
}

// guardedBuilds checks that im, a guarded backend without build-image,
// refuses its builds.
func guardedBuilds(ctx context.Context, s Subject, im backend.Images) error {
	const key = "0123456789fe"
	img := backend.ImageSpec{Image: "base+conformance", Key: key, Parent: s.Image, Net: backend.NetNone}
	if _, err := im.BuildImage(ctx, img); msg.IDOf(err) != "backend_lacks" {
		return fmt.Errorf("Images.BuildImage without build-image: %w, want backend_lacks", err)
	}
	if ok, err := holds(ctx, s.Backend, key); err != nil || ok {
		return fmt.Errorf("the refused build made its image: %w", err)
	}
	tools := backend.ToolsSpec{
		Name: "conformance", Base: s.Image, Out: filepath.Join(s.Dir, "tools"), OutAt: "/mnt/devvm-tools",
		Format: im.ToolFormat(), Net: backend.NetNone, Script: backend.Script{Name: "build.sh", Data: []byte("true\n")},
	}
	if err := im.BuildTools(ctx, tools); msg.IDOf(err) != "backend_lacks" {
		return fmt.Errorf("Images.BuildTools without build-image: %w, want backend_lacks", err)
	}
	return listProblem(ctx, s, conformVM)
}
