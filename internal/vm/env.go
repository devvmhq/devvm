package vm

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/build"
	"github.com/devvmhq/devvm/internal/from"
	"github.com/devvmhq/devvm/internal/gitprotect"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/netpolicy"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/secrets"
	"github.com/devvmhq/devvm/internal/sshagent"
	"github.com/devvmhq/devvm/internal/sshconfig"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/tools"
	"github.com/devvmhq/devvm/internal/vmssh"
)

// Env is what the commands on a folder's VM run with, shared by Launch,
// Lifecycle, Settings and Net; its methods are what they share: starting
// the folder's VM, which the finder finds (finder.go), its size and ports,
// the image builder and the ssh config. NewEnv sets every field but Dir,
// which the finder reads when it's needed; tests change them. Its parts
// (wire) are made over its address, so a copy of an Env is wired again.
type Env struct {
	Dirs     paths.Dirs
	Assets   fs.FS // devvm's own files (assets.FS)
	Registry *backend.Registry
	Tools    *tools.Env
	Proxy    *netpolicy.Proxy // VMs' proxies
	Dir      string           // the folder devvm runs in, a physical path: "" until the finder needs it (finder.dir)
	// Folder is which VM the folder uses when a folder it's inside has
	// one (--here, --parent): resolveFolder's rule.
	Folder FolderRule
	OS     string // the host's, as Go names it
	// Version is devvm's version, which a new VM's state and each image's
	// record keep.
	Version string
	Host    func() (Host, error)
	// Protector returns the .git protection for a project folder.
	Protector func(dir string) *gitprotect.Protector
	// Exec runs the shell in devvm's place; it returns only on failure.
	Exec func(backend.Command) error
	// OpenURL opens a URL with macOS's open (devvm desktop's vnc://, in
	// Screen Sharing).
	OpenURL func(ctx context.Context, url string) error
	// ReadClip reads an image or one copied file's path from this
	// machine's clipboard, at most limit bytes, and CopyText puts text on
	// it (platform.Clipboard's, with the system's tools unless the CLI
	// gives a test build's): devvm paste's.
	ReadClip func(ctx context.Context, limit int64) (platform.Clip, error)
	CopyText func(ctx context.Context, text string) error
	// Environ is the environment devvm started with, the SSH agent
	// included: the editor's, which devvm's own connections don't change.
	Environ []string
	// What a VM's setup sends of the host's settings: the environment
	// (TZ, LANG, LC_ALL), the timezone, and git's settings in a folder.
	Getenv    func(string) string
	HostTZ    func() string
	GitConfig func(ctx context.Context, dir, key string) string
	// SSHAdd runs ssh-add with args, on the user's own agent, and returns
	// its exit code.
	SSHAdd func(ctx context.Context, args ...string) int
	// Agent is devvm's own SSH agent.
	Agent *sshagent.Agent
	// PortInUse reports whether a program on the host has a port, on any
	// address (platform.PortInUse): a listed port that isn't forwarded.
	PortInUse func(ctx context.Context, port int) bool
	// Forwarders are VMs' port forwarders, as processes (forwards adds
	// the rest).
	Forwarders vmssh.Forwards
	// PublicImage pins an image a devcontainer.json names to the digest its
	// registry gives anyone (from.Lookup.Public).
	PublicImage func(ctx context.Context, ref string) (string, error)
	// FreeSpace returns the free space on the volume holding a path
	// (the registry's Host), checked before a new VM and each build; nil
	// checks none.
	FreeSpace func(path string) (uint64, error)
	// HistoryMax is the most of an export of agents' history devvm
	// keeps, in bytes; 0 is historyMax.
	HistoryMax int
	// PasteWait is how long devvm paste waits for paste.sh in the VM, in
	// place of pastePutWait and pasteCountWait: a test build's; 0 is
	// those.
	PasteWait time.Duration
	// Links returns the links a VM's start and setup give it: its targets'
	// addresses and names, for its /etc/hosts and its NO_PROXY (checkLinks checks them first); nil gives none.
	Links func(st *state.VM) ([]GuestLink, error)
	// Reporter is where it prints and asks, data output, the run log
	// (where the output of a backend's Exec goes too), and the run's
	// cleanup stack.
	msg.Reporter
	// PickFeatures asks which features base gets (build.Env.Pick), and
	// PickParts shows the tools picker (nil shows none): each set by its
	// caller. PickOne shows rows under help for one to be chosen, starting
	// from the first that's on, and returns the index of the one chosen
	// (nil, or where the picker can't show: the one it started from).
	PickFeatures func([]build.Feature) []build.Feature
	PickParts    func(msg.M, []PickPart) []PickPart
	PickOne      func(msg.M, []PickRow) int
	// PickMany shows parts' rows under help, each part under its
	// heading, for any to be chosen, and returns them as chosen; or why
	// the picker couldn't show where it can (CanPick), or ended without
	// Enter, such as when its terminal went away, with them as given.
	// nil shows none.
	PickMany func(msg.M, []PickPart) ([]PickPart, error)
	// AddInclude is --ssh-config: devvm's Include line goes in
	// ~/.ssh/config, when it's missing, without asking.
	AddInclude bool

	// Its parts, each made once over it (wire): the commands and each
	// other reach them here. The finder finds the VM a command acts on;
	// the starter is its one start; the rest each do one part of a
	// command's work.
	finder        finder
	starter       starter
	linkRuns      linkRuns
	syncer        syncer
	remaking      remaking
	clones        clones
	vmFolders     vmFolders
	devcontainers devcontainers
	creation      creation
	launcher      Launch
	lifecycle     Lifecycle
	vmTools       Tools
	groups        Groups
	links         Links
	leftovers     Leftovers
}

// wire makes e's parts (Env's unexported fields), each over e itself,
// and returns e: NewEnv's, and every Env a test makes.
func wire(e *Env) *Env {
	e.finder, e.starter, e.linkRuns, e.syncer = finder{e}, starter{e}, linkRuns{e}, syncer{e}
	e.remaking, e.clones, e.vmFolders, e.devcontainers = remaking{e}, clones{e}, vmFolders{e}, devcontainers{e}
	e.launcher, e.lifecycle, e.vmTools = Launch{e}, Lifecycle{e}, Tools{e}
	e.groups, e.links, e.leftovers = Groups{e}, Links{e}, Leftovers{e}
	e.creation = creation{e.launcher}
	return e
}

// NewEnv returns the environment up runs in, with devvm's own files
// (assets), devvm being self at version, on r, with the run's registry,
// shared tools and secrets store. The shared tools' downloads and builds,
// and --from's lookups, go through tl's company proxy, whose password
// its caller takes from store, the same secrets as the VMs' proxies'.
// Its caller sets PickFeatures and PickParts.
func NewEnv(d paths.Dirs, assets fs.FS, self, version string, reg *backend.Registry, tl *tools.Env, store *secrets.Store, r msg.Reporter) *Env {
	e := &Env{
		Dirs: d, Assets: assets, Registry: reg, Tools: tl,
		Proxy: netpolicy.NewProxy(d, assets, self, store, r),
		OS:    runtime.GOOS, Version: version,
		Host: func() (Host, error) { return ThisHost(reg.Host) }, Exec: execShell, OpenURL: openURL, Environ: os.Environ(),
		ReadClip: platform.Clipboard{}.ReadClip, CopyText: platform.Clipboard{}.CopyText,
		Getenv: os.Getenv, HostTZ: hostTZ, GitConfig: gitConfig,
		SSHAdd:    func(ctx context.Context, args ...string) int { return sshAdd(ctx, r.Stderr(), args...) },
		Reporter:  r,
		PortInUse: platform.PortInUse, Forwarders: vmssh.NewForwards(d, reg.Review, self), PublicImage: from.NewLookup(d, r, tl.Proxy).Public, FreeSpace: reg.Host.FreeSpace,
	}
	locker := sync.OnceValue(func() gitprotect.Locker { return gitprotect.HostLocker(canPin(reg)) })
	e.Protector = func(dir string) *gitprotect.Protector {
		p := gitprotect.New(dir, d.Home, locker(), e.Reporter)
		p.Kept = func(vm string) func(string) msg.M {
			inv, err := ReadStates(d)
			if err != nil {
				// What another VM protects, devvm can't tell: all stays.
				return func(string) msg.M { return msg.GitKeptUnread(d.VMs()).Because(err) }
			}
			return inv.GitKeeper(vm)
		}
		p.Serial = func(ctx context.Context, f func() error) error {
			unlock, err := platform.LockFile(ctx, d.GitLock())
			if err != nil {
				return err
			}
			defer unlock()
			return f()
		}
		return p
	}
	e.Links = func(st *state.VM) ([]GuestLink, error) { return guestLinks(d, st) }
	e.Agent = sshagent.New(d, e.mounts, r)
	return wire(e)
}

// canPin reports whether the backend new VMs get pins paths in the
// project folder (read-only-paths), which .git protection locks with on a
// Linux host (gitprotect.Binds).
func canPin(reg *backend.Registry) bool {
	c, err := reg.For(new(state.VM))
	return err == nil && reg.Allowed(c.Backend, backend.FeatureReadOnlyPaths)
}

// Builder returns what builds images on b whose base starts from
// fromRef ("" for the backend's own): the only one, which cli's
// commands take too.
func (e *Env) Builder(b backend.Backend, fromRef string) *build.Env {
	return &build.Env{
		Dirs: e.Dirs, Assets: e.Assets, Backend: b, Review: e.Registry.Review, Tools: e.Tools, From: fromRef, Version: e.Version,
		Reporter: e.Reporter, Pick: e.PickFeatures, FreeSpace: e.FreeSpace,
	}
}

// execShell runs cmd in place of devvm, in its folder.
func execShell(cmd backend.Command) error {
	if cmd.Dir != "" {
		if err := os.Chdir(cmd.Dir); err != nil {
			return err
		}
	}
	return platform.Exec(cmd.Path, cmd.Args, cmd.Env)
}

// openURL opens url with macOS's open.
func openURL(ctx context.Context, url string) error {
	return exec.CommandContext(ctx, "open", url).Run() //nolint:gosec // devvm's own vnc://127.0.0.1:PORT
}

// FolderState is what's at the path of the project folder a VM's state
// records.
type FolderState string

// The folder states. A VM whose folder is gone or not connected is
// deleted only when asked for by name (Lifecycle.RmVM), never on its own.
const (
	// FolderThere: something is at the path, or it can't be read (in
	// time, too), or the state records no folder. A folder made again there is there too:
	// sameFolder tells it apart.
	FolderThere FolderState = "there"
	// FolderGone: nothing is at the path, and the nearest folder above it
	// that exists is on the device the folder was on (deleted or moved),
	// or the state records no device to compare.
	FolderGone FolderState = "gone"
	// FolderNotConnected: nothing is at the path, and the nearest folder
	// above it is on another device: the folder was on a disk or share
	// that isn't connected, or moved to another disk, then deleted.
	FolderNotConnected FolderState = "not_connected"
)

// FolderOf is the state of the project folder st records, as found within
// folderWait: a folder whose disk doesn't answer (an NFS share that hangs)
// is there, so it's never taken for gone.
func FolderOf(st *state.VM) FolderState {
	return folderWithin(st, func(st *state.VM) FolderState { return folderOf(st, platform.DeviceOf) }, folderWait)
}

// folderWait is how long FolderOf waits for a folder's disk.
const folderWait = 5 * time.Second

// folderWithin is of's answer for st, or FolderThere once wait is up. A
// look that hangs is left to finish on its own.
func folderWithin(st *state.VM, of func(*state.VM) FolderState, wait time.Duration) FolderState {
	got := make(chan FolderState, 1)
	go func() { got <- of(st) }()
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case s := <-got:
		return s
	case <-t.C:
		return FolderThere
	}
}

// folderOf is FolderOf with device, which returns the device of what's at
// a path (platform.DeviceOf), a test seam.
func folderOf(st *state.VM, device func(string) (string, error)) FolderState {
	if st.Dir == "" {
		return FolderThere
	}
	if _, err := os.Lstat(st.Dir); !errors.Is(err, fs.ErrNotExist) {
		return FolderThere
	}
	was := platform.FolderDevice(st.Folder)
	for dir := filepath.Dir(st.Dir); was != ""; dir = filepath.Dir(dir) {
		if dev, err := device(dir); err == nil {
			if dev != was {
				return FolderNotConnected
			}
			break
		} else if !errors.Is(err, fs.ErrNotExist) || dir == filepath.Dir(dir) {
			break
		}
	}
	return FolderGone
}

// runningAgent is the SSH agent choice a running VM runs with: its
// current start's (sshagent.Running).
func runningAgent(ctx context.Context, b backend.Backend, st *state.VM) (string, error) {
	run, err := b.Running(ctx, st.Name())
	if err != nil {
		return "", err
	}
	return sshagent.Running(st, run.ID), nil
}

// agentSocket is the SSH agent socket a VM whose choice is mode gets:
// devvm's, the user's (the one devvm started with), or none (""). Its
// start forwards it, and devvm's own connections to that VM carry it
// (target.agent), each VM's its own, as devvm's environment never changes.
func (e *Env) agentSocket(mode string) string {
	switch mode {
	case sshagent.Devvm:
		return e.Agent.Socket()
	case sshagent.On:
		user := ""
		for _, kv := range e.Environ {
			if v, ok := strings.CutPrefix(kv, "SSH_AUTH_SOCK="); ok {
				user = v
			}
		}
		return user
	}
	return ""
}

// mounts returns the host folders project VMs mount, as their states have
// them (Inventory.Mounts): no backend is asked.
func (e *Env) mounts(context.Context) ([]sshagent.Mount, error) {
	inv, err := ReadStates(e.Dirs)
	if err != nil {
		return nil, err
	}
	return inv.Mounts(e.Dirs)
}

// sshConfig is the ssh config for every backend's VMs.
func (e *Env) sshConfig() *sshconfig.Config {
	c := sshconfig.New(e.Dirs, e.Registry)
	c.Reporter, c.AddInclude = e.Reporter, e.AddInclude
	return c
}

// writeSSHConfig rewrites the ssh config with the running VMs of the
// backends some VM's state names. One of them that can't list fails it
// (Inventory.Err), and the config stays as it was.
func (e *Env) writeSSHConfig(ctx context.Context) error {
	inv, err := ReadInventory(ctx, e.Dirs, e.Registry, UsedBackends)
	if err != nil {
		return err
	}
	if err := inv.Err(); err != nil {
		return err
	}
	return e.sshConfig().Write(ctx, inv.Running())
}
