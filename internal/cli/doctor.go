package cli

import (
	"cmp"
	"context"
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/netpolicy"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/settings"
	"github.com/devvmhq/devvm/internal/shell"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/trust"
	"github.com/devvmhq/devvm/internal/ui"
	"github.com/devvmhq/devvm/internal/upgrade"
	"github.com/devvmhq/devvm/internal/vm"
)

// newDoctorCmd is devvm doctor.
func newDoctorCmd(a *app) *cobra.Command {
	u := a.u
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check what devvm needs on this machine",
		Long: "Check what Lima needs on this machine, and say how to get what's missing, " +
			"because devvm never installs it itself. Then list the features Lima gives VMs. " +
			"doctor fails when something is missing, but never for a warning, such as one about low free space.",
		Example: examples(
			"devvm doctor", "check what devvm needs", "devvm doctor --json", "the same as JSON, for scripts",
		),
		Args:              noArgs,
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(cmd *cobra.Command, _ []string) error {
			reg, b, err := doctorBackend(a)
			if err != nil {
				return failed(err, msg.DoctorFailed)
			}
			d := doctor{
				Path: onPath{LookPath: exec.LookPath, Executable: os.Executable},
				Free: reg.Host.FreeSpace,
				Host: func() (vm.Host, error) { return vm.ThisHost(reg.Host) },
				Dirs: a.dirs,
				Policies: func() []msg.M {
					dirs, err := a.dirs()
					if err != nil {
						return nil
					}
					return netpolicy.NotRules(dirs)
				},
				Folders: func() []msg.M {
					dirs, err := a.dirs()
					if err != nil {
						return nil
					}
					return state.VMFolderProblems(dirs)
				},
				Release: func() upgrade.Known {
					dirs, err := a.dirs()
					if err != nil {
						return upgrade.Known{}
					}
					return upgrade.ReadKnown(dirs.ReleaseCheck())
				},
				Locker: a.locker(),
			}
			if asJSON {
				return printJSON(u, d.report(cmd.Context(), u.Text, reg.All(), b))
			}
			return d.run(cmd.Context(), u, b)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print what it found as JSON, and succeed whatever is missing")
	return cmd
}

// doctorOutput is devvm doctor --json: the backend new VMs get and what
// it's missing, doctor's warnings, whether Docker is installed, whether
// devvm can lock .git, and every backend's features.
type doctorOutput struct {
	Backend  string        `json:"backend"`
	Problems []problemJSON `json:"problems"`
	Warnings []string      `json:"warnings"`
	Docker   bool          `json:"docker"`
	// GitLock is whether devvm can lock .git for VMs on this machine:
	// always on macOS, and on Linux when devvm-immutable works.
	GitLock bool `json:"git_lock"`
	// Features is, for each backend, how it has each feature: native,
	// emulated, unreviewed or none.
	Features map[string]map[string]string `json:"features"`
	// Release is the newest release the daily check found, and when;
	// null before it found one.
	Release *releaseJSON `json:"release"`
}

// releaseJSON is the newest devvm release a launch's daily check found,
// and when, in RFC 3339.
type releaseJSON struct {
	Latest  string `json:"latest"`
	Checked string `json:"checked"`
}

// problemJSON is something the backend is missing: its key, the command
// that fixes it (null when there's none to run) and the message.
type problemJSON struct {
	Key     string  `json:"key"`
	Fix     *string `json:"fix"`
	Message string  `json:"message"`
}

// report is what doctor finds, as doctor --json shows it, each message
// as text shows it (the UI's Text). It prints nothing.
func (d doctor) report(ctx context.Context, text func(msg.M) string, all []backend.Backend, b backend.Backend) doctorOutput {
	out := doctorOutput{
		Backend:  b.Name(),
		Problems: []problemJSON{}, Warnings: []string{}, Features: map[string]map[string]string{},
	}
	for _, p := range b.Problems(ctx) {
		j := problemJSON{Key: p.Key, Message: text(p.Msg)}
		if p.Fix != "" {
			j.Fix = &p.Fix
		}
		out.Problems = append(out.Problems, j)
	}
	g := d.gitLock(ctx)
	for _, m := range d.warnings(b, g) {
		out.Warnings = append(out.Warnings, text(m))
	}
	_, err := exec.LookPath("docker")
	out.Docker = err == nil
	out.GitLock = g.ok
	for _, be := range all {
		fs := map[string]string{}
		for _, f := range backend.Features {
			fs[string(f)] = be.Caps()[f].String()
		}
		out.Features[be.Name()] = fs
	}
	if k := d.known(); k.Latest != "" {
		out.Release = &releaseJSON{Latest: k.Latest, Checked: k.Checked.UTC().Format(time.RFC3339)}
	}
	return out
}

// warnings are doctor's warnings, which never make it fail: another devvm
// first on PATH, low free space, a small host, policy files' lines that
// aren't rules, what in the VMs' folder isn't a VM's, and, as g says, a
// devvm-immutable that doesn't work.
func (d doctor) warnings(b backend.Backend, g gitLock) []msg.M {
	var ms []msg.M
	if m, ok := d.Path.other(); ok {
		ms = append(ms, m)
	}
	if !g.ok {
		ms = append(ms, g.warning)
	}
	if m, low := (backend.Space{Free: d.Free}).Warning(b); low {
		ms = append(ms, m)
	}
	ms = append(ms, smallHost(d.Host)...)
	if d.Policies != nil {
		ms = append(ms, d.Policies()...)
	}
	if d.Folders != nil {
		ms = append(ms, d.Folders()...)
	}
	return ms
}

// The host devvm recommends: base asks for 4 CPUs and 8 GiB.
const (
	recommendedCPUs      = 4
	recommendedMemoryGiB = 8
)

// doctor is devvm doctor's checks of this machine; tests set its fields.
type doctor struct {
	Path onPath
	Free func(path string) (uint64, error) // free bytes on path's volume
	Host func() (vm.Host, error)
	// Dirs is devvm's folders, whose CA folder the company proxy's check
	// reads (sayProxyCA).
	Dirs func() (paths.Dirs, error)
	// Policies are the warnings for your policy files' lines that aren't
	// rules (netpolicy.NotRules); nil for none.
	Policies func() []msg.M
	// Folders are the warnings for what in the VMs' folder isn't a VM's
	// (state.VMFolderProblems); nil for none.
	Folders func() []msg.M
	// Release is what the daily release check last found
	// (upgrade.ReadKnown); nil for nothing.
	Release func() upgrade.Known
	// Locker is how this machine locks .git, whose devvm-immutable
	// doctor checks on Linux.
	Locker platform.Locker
}

// gitLock is doctor's check of how this machine locks .git: on Linux,
// whether devvm-immutable works, and when it doesn't, the warning and how
// to install it.
type gitLock struct {
	ok      bool
	warning msg.M
	install msg.M
}

// gitLock checks devvm-immutable, on Linux; elsewhere locking needs none.
func (d doctor) gitLock(ctx context.Context) gitLock {
	if !platform.LocksNeedHelper {
		return gitLock{ok: true}
	}
	return gitLockOf(d.Locker.HelperUsable(ctx), cmp.Or(d.Locker.Helper, platform.ImmutableHelper), helperSource(d.Path.Executable), ownGroup())
}

// ownGroup is the name of the user's own group, or its number.
func ownGroup() string {
	gid := strconv.Itoa(os.Getgid())
	if g, err := user.LookupGroupId(gid); err == nil {
		return g.Name
	}
	return gid
}

// gitLockOf is what doctor says of devvm-immutable at helper, whose check
// returned err, src being the one that came with this devvm ("" for none),
// and group the user's own, which a shared machine's install can limit it
// to.
func gitLockOf(err error, helper, src, group string) gitLock {
	if err == nil {
		return gitLock{ok: true}
	}
	g := gitLock{warning: msg.DoctorImmutableMissing(), install: msg.DoctorImmutableNoSource()}
	if !errors.Is(err, platform.ErrNoHelper) {
		why, _ := strings.CutPrefix(err.Error(), platform.ErrHelperUnusable.Error()+": ")
		g.warning = msg.DoctorImmutableUnusable(why)
	}
	if src != "" {
		g.install = msg.DoctorImmutableInstall(shell.Word(group), shell.Word(helper), shell.Word(src))
	}
	return g
}

// helperSource is the devvm-immutable that came with this devvm, its links
// resolved: next to it, as in a release archive, or in the libexec folder
// beside its bin, as Homebrew installs it; "" when there's none.
func helperSource(executable func() (string, error)) string {
	self, err := executable()
	if err != nil {
		return ""
	}
	if self, err = filepath.EvalSymlinks(self); err != nil {
		return ""
	}
	dir := filepath.Dir(self)
	for _, p := range []string{filepath.Join(dir, "devvm-immutable"), filepath.Join(dir, "..", "libexec", "devvm-immutable")} {
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return p
		}
	}
	return ""
}

// known is what the daily release check last found, if anything.
func (d doctor) known() upgrade.Known {
	if d.Release == nil {
		return upgrade.Known{}
	}
	return d.Release()
}

// run checks b, the backend new VMs get, and lists its features. It
// returns the last problem's message when b has problems, having warned
// the others. Warnings (another devvm on PATH, low free space, a small
// host) never change what it returns.
func (d doctor) run(ctx context.Context, u *ui.UI, b backend.Backend) error {
	problems := b.Problems(ctx)
	if err := writeFeatures(u.Table(), b); err != nil {
		return err
	}
	if _, err := exec.LookPath("docker"); err == nil {
		u.Say(msg.DockerFound())
	} else {
		u.Say(msg.DockerMissing())
	}
	sayProxyCA(u, d.Dirs)
	g := d.gitLock(ctx)
	for _, m := range d.warnings(b, g) {
		u.Warn(m)
	}
	switch {
	case !platform.LocksNeedHelper:
	case g.ok:
		u.Say(msg.DoctorImmutableOK())
	default:
		u.Say(g.install)
	}
	if k := d.known(); k.Latest != "" {
		u.Say(msg.DoctorLatestRelease(k.Checked.Local().Format(time.DateOnly), k.Latest))
	}
	if len(problems) == 0 {
		u.Say(msg.DoctorOK(b.Name(), devvmVersion()))
		return nil
	}
	for _, p := range problems[:len(problems)-1] {
		u.Warn(p.Msg)
	}
	return problems[len(problems)-1].Msg
}

// smallHost returns a warning for each way this machine is below the host
// devvm recommends: less memory, fewer CPUs. None when host can't say.
func smallHost(host func() (vm.Host, error)) []msg.M {
	h, err := host()
	if err != nil {
		return nil
	}
	var ms []msg.M
	if h.MemoryGiB > 0 && h.MemoryGiB < recommendedMemoryGiB {
		ms = append(ms, msg.HostMemoryLow(strconv.Itoa(h.MemoryGiB), strconv.Itoa(recommendedMemoryGiB)))
	}
	if h.CPUs > 0 && h.CPUs < recommendedCPUs {
		ms = append(ms, msg.HostCpusFew(strconv.Itoa(h.CPUs), strconv.Itoa(recommendedCPUs)))
	}
	return ms
}

// sayProxyCA says, when HTTPS_PROXY or config.toml's proxy.https is set
// (settings.HostProxy) and devvm's CA folder has no CAs, that a proxy that
// inspects TLS needs its CA there: not a problem, as most proxies don't. A
// folder devvm would refuse is warned about.
func sayProxyCA(u *ui.UI, folders func() (paths.Dirs, error)) {
	dirs, err := folders()
	if err != nil {
		return
	}
	if host, err := settings.HostProxy(dirs, os.Getenv); err != nil || host.HTTPS == "" {
		return
	}
	cas, err := trust.Read(dirs.TrustedCAs())
	var m msg.M
	switch {
	case errors.As(err, &m):
		u.Warn(m)
	case err == nil && len(cas) == 0:
		u.Say(msg.DoctorProxyCA(dirs.TrustedCAs()))
	}
}

// doctorBackend returns devvm's backends and the one new VMs get.
func doctorBackend(a *app) (*backend.Registry, backend.Backend, error) {
	reg, err := a.registry()
	if err != nil {
		return nil, nil, err
	}
	c, err := reg.For(new(state.VM))
	if err != nil {
		return nil, nil, err
	}
	return reg, c.Backend, nil
}

// onPath finds the devvm first on PATH; tests set its fields.
type onPath struct {
	LookPath   func(file string) (string, error)
	Executable func() (string, error)
}

// other returns a warning when the devvm first on PATH isn't this binary,
// both with their links resolved; none when devvm isn't on PATH or either
// can't be found.
func (p onPath) other() (msg.M, bool) {
	first, err := p.LookPath("devvm")
	if err != nil {
		return msg.M{}, false
	}
	if first, err = filepath.EvalSymlinks(first); err != nil {
		return msg.M{}, false
	}
	self, err := p.Executable()
	if err != nil {
		return msg.M{}, false
	}
	if self, err = filepath.EvalSymlinks(self); err != nil || self == first {
		return msg.M{}, false
	}
	return msg.DevvmOtherFirst(first, self), true
}

// writeFeatures writes a table of every feature and how b has it: native
// green, emulated yellow, unreviewed red and none dim.
func writeFeatures(tab *ui.Table, b backend.Backend) error {
	tab.Header("FEATURE", "SUPPORT")
	for _, f := range backend.Features {
		tab.Row(cell(string(f)), supportCell(b.Caps()[f]))
	}
	return tab.Flush()
}

// supportCell is how a backend has a feature, colored by what it means:
// none is dim, as a backend lacking a feature is no fault; unreviewed, an
// emulation devvm refuses, stands out in red.
func supportCell(s backend.Support) ui.Cell {
	role := ui.Secondary
	switch {
	case s.IsNative():
		role = ui.Good
	case s.Usable():
		role = ui.Attention
	case s.IsEmulated():
		role = ui.Bad
	}
	return ui.Cell{Text: s.String(), Role: role}
}
