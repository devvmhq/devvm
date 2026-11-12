// Package backend is what runs devvm's VMs: one interface every backend
// implements (Lima's), the features each one has (Caps), the
// registry of backends and the choice of backend for a VM.
//
// A backend only runs what it's told. What a VM is made of (its image, its
// mounts, its size, its network tier) is decided outside, and anything
// that doesn't depend on the backend (.git protection, the VM's
// proxy) stays outside too. Before asking a backend for something, callers
// check it has the features that needs with Require.
package backend

import (
	"context"
	"errors"
	"io"
	"slices"
	"strconv"

	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/msg"
)

// Backend runs VMs and builds their images: its name and Caps, then its
// four roles, each an interface of its own that every backend implements.
// Errors are messages (msg.M) where the user can act on them. A backend
// may implement Forgetter too, which callers find with As.
type Backend interface {
	// Name is the backend's name, as --backend and VM state have it: lima.
	Name() string
	// Caps says which features the backend has, and how.
	Caps() Caps
	Host
	Images
	Machines
	Housekeeping
}

// Host is what a backend needs of this host, and where on it the backend
// keeps its VMs and their disks.
type Host interface {
	// Problems lists everything the backend needs that this host lacks
	// (installed, a version devvm supports, a host it supports); none
	// when it can run here. Check returns the first.
	Problems(ctx context.Context) []Problem
	// NameMax is the longest VM name the backend takes; 0 for no limit.
	// Callers shorten the names they make to fit.
	NameMax() int
	// Location says where this run's VMs live, when that can change
	// between runs of devvm (Lima's home, DEVVM_LIMA_HOME), and whether
	// it's the backend's default. A VM records where it was made
	// (LocationOf), so a run that looks elsewhere never takes it for one
	// its backend lost. A backend whose VMs can't move returns "" and
	// true.
	Location() (where string, isDefault bool)
	// DiskDirs returns a folder on each volume of the host's own the
	// backend writes images' and VMs' disks to, made or not, and Floors
	// the free space it wants on them (Space). A backend whose disks are
	// elsewhere returns none, and its free space isn't checked.
	DiskDirs() []string
	Floors() Floors
}

// Images builds images and shared tools' images, and keeps the images.
type Images interface {
	// BuildImage builds an image from its parent (or, for base, from the
	// backend's own Ubuntu or spec.From) by running its scripts in order.
	BuildImage(ctx context.Context, spec ImageSpec) (Built, error)
	// Images lists the keys of the images the backend holds.
	Images(ctx context.Context) ([]string, error)
	// ImageSize is the disk space an image takes on this machine, in
	// bytes; blocks it shares with other images or VMs count too. A
	// missing one is an error matching fs.ErrNotExist.
	ImageSize(ctx context.Context, key string) (int64, error)
	// DeleteImage deletes an image. A missing one is not an error.
	DeleteImage(ctx context.Context, key string) error
	// BuildTools runs the shared tools' build script in a throwaway VM
	// made from base's image, with spec.Out mounted writable, and deletes
	// the VM: at once, or after a failure once the run ends (a ui
	// cleanup), so the run log ends with what failed. The images it
	// leaves are in spec.Format, which is ToolFormat's.
	BuildTools(ctx context.Context, spec ToolsSpec) error
	// ToolFormat is the format of the tool images the backend's VMs
	// mount, which their keys hash: "" for the build script's own
	// (compressed EROFS), else a name of the backend's that changes
	// whenever what it makes of them does.
	ToolFormat() string
	// Origin is the backend's own steps that base starts with, for base
	// from f (nil for the backend's own Ubuntu): what it makes base's
	// disk from, such as its settings, the image it starts from and a
	// conversion. They go first in base's recipe (image.Recipe's
	// Origin), so base's key hashes them, and BuildImage is given them
	// back and runs those, not its own files. None for a backend that
	// puts nothing of its own in.
	Origin(f *From) ([]image.Step, error)
	// PinOrigin pins what base from f (nil for the backend's own Ubuntu)
	// starts from on this machine, as how asks, so Origin has it to put
	// in base's recipe: callers pin before they key base for a build.
	// It returns what it pinned, for messages, "" when it pinned nothing:
	// a pin was there already (PinFirst), or the backend has nothing of
	// its own to pin. A --from image's digest is devvm's own record.
	PinOrigin(ctx context.Context, f *From, how Pin) (string, error)
}

// Pin is how PinOrigin pins base's origin.
type Pin int

const (
	// PinFirst pins the newest only when nothing is pinned yet: a build.
	PinFirst Pin = iota
	// PinNewest pins the newest again: devvm rebuild base.
	PinNewest
	// PinOlder pins the newest older than what's pinned: the fallback
	// when what's pinned doesn't boot.
	PinOlder
)

// Machines makes, starts and reaches VMs. A VM is made as its spec wants
// (CreateVM), and each start of a stopped VM makes it what the caller
// wants then (Start's Want), changing only what differs: nothing changes a
// stopped VM's config in between.
type Machines interface {
	// CreateVM makes a VM from an image, stopped, as spec wants it.
	CreateVM(ctx context.Context, image string, spec VMSpec) error
	// Start starts a stopped VM as w wants it, confined as c says, and
	// returns once it answers: it first changes what of w and c the VM's
	// config doesn't have yet, and nothing else, after checking w with
	// Validate. Its ports are devvm's forwarder's to forward once it
	// runs, over its ssh (SSH): the backend forwards none of its own. A
	// running VM is left as it is, w and c unapplied (callers say what
	// changes at its next start); its confinement is checked with
	// Running.
	Start(ctx context.Context, vm string, w Want, c Confinement) error
	// Running says how the VM runs now: its start, whether it's
	// confined, and its lifeline, which a stopped VM has too.
	Running(ctx context.Context, vm string) (Run, error)
	// ProxyURL is devvm's proxy as a VM started with c reaches it, its
	// user and password included; "" unless c names it (GuestProxy).
	ProxyURL(c Confinement) string
	// Stop stops a VM. A stopped one is left as it is.
	Stop(ctx context.Context, vm string) error
	// Delete deletes a VM, stopping it first.
	Delete(ctx context.Context, vm string) error
	// Rename gives a stopped VM another name, to, which no VM has and
	// which fits NameMax: a remake keeps the VM's old machine, its disk
	// included, under to while it makes the new one by the VM's name,
	// and gives it back if that fails. What the backend keeps for the VM
	// besides it (Forgetter's) stays the old name's.
	Rename(ctx context.Context, vm, to string) error
	// List lists the backend's VMs, without its images.
	List(ctx context.Context) ([]Instance, error)
	// Inspect returns one VM, or an error matching ErrNotFound.
	Inspect(ctx context.Context, vm string) (Instance, error)

	// Exec runs argv in a running VM and waits for it.
	Exec(ctx context.Context, vm string, argv []string, o ExecOpts) error
	// Shell returns the command that opens an interactive shell in a
	// running VM, as o says. The caller runs it in place of devvm.
	Shell(ctx context.Context, vm string, o ShellOpts) (Command, error)
	// SSH says how ssh, editors, links and devvm's port forwarder reach
	// a running VM.
	SSH(ctx context.Context, vm string) (SSHTarget, error)
}

// Housekeeping is what devvm space and devvm gc ask of a backend beyond
// its images and VMs: the disk they take, its strays and its own caches.
type Housekeeping interface {
	// Usage says what the backend's VMs and caches take (devvm space).
	// It changes nothing and runs only commands that end with ctx; a
	// backend that can't tell returns none, and shows no sizes.
	Usage(ctx context.Context) (Usage, error)
	// Strays lists the backend's VMs with devvm's label that this devvm
	// has no state for (devvm space), which devvm never deletes: they may
	// be another devvm's.
	Strays(ctx context.Context) ([]Stray, error)
	// DevvmCache lists devvm's share of the backend's own caches, which
	// devvm's builds fill (devvm gc --caches): entries only devvm's
	// builds make, that no image or VM of devvm's needs, never the
	// user's own. It changes nothing; none for a backend whose caches it
	// can't tell apart.
	DevvmCache(ctx context.Context) ([]CacheEntry, error)
	// DeleteCache deletes entries DevvmCache listed, each checked again
	// first: one that isn't devvm's any more stays.
	DeleteCache(ctx context.Context, entries []CacheEntry) error
}

// Run is how a VM runs now, as its backend says (Machines.Running).
type Run struct {
	// ID identifies the running VM's current start: every start gets
	// another, whether devvm or something else made it. "" when it isn't
	// running, or the backend can't tell.
	ID string
	// Confined is whether it runs confined to devvm's proxy: started by
	// Start with NetProxy, not some other way since.
	Confined bool
	// Lifeline is the pid file of the host process that runs the VM once
	// it starts, which the VM's proxy and port forwarder follow: they exit
	// when that process does. A stopped VM has it too, as its proxy starts
	// first. Not a VM's own file: the VM can't write it.
	Lifeline string
	// ReadOnly is, where the backend can tell (FeatureReadOnlyPaths), the
	// host paths its running VM sees through a read-only mount of their
	// own, as the process that serves its folders has them now: what its
	// pins hold. nil when it can't tell. Mounted is every host path it
	// sees through a mount of its own, read-only or not: the fixed pins
	// too.
	ReadOnly, Mounted []string
}

// Problem is something a backend needs that the host lacks, as devvm
// doctor reports it.
type Problem struct {
	// Key says what it is, for scripts that offer fixes: lima, qemu,
	// kvm, kvm-access…
	Key string
	// Fix is the command that fixes it, its words separated by spaces
	// (no shell syntax); "" when there's none to run.
	Fix string
	// Msg says what's missing and what to do.
	Msg msg.M
}

// Check reports whether b can run here: the first of its Problems, or nil.
func Check(ctx context.Context, b Backend) error {
	if ps := b.Problems(ctx); len(ps) > 0 {
		return ps[0].Msg
	}
	return nil
}

// ErrNotFound is the error, or wraps it, for a VM the backend doesn't have.
var ErrNotFound = errors.New("no such VM")

// LoggedError is a failure whose details are in the run log, from a
// command run into it: the CLI shows it with the UI's ErrorLogged(e.Msg),
// the log's end first, rather than the message alone.
type LoggedError struct{ Msg msg.M }

// Logged returns m as a LoggedError.
func Logged(m msg.M) error { return &LoggedError{Msg: m} }

func (e *LoggedError) Error() string { return e.Msg.Error() }

// Unwrap returns the message, so errors.As finds it.
func (e *LoggedError) Unwrap() error { return e.Msg }

// RemountError is a Start's error for a VM it started, then stopped
// again, because its project folder couldn't be mounted again with the
// cache mode Want asked while it ran (Msg says how it was left). The VM's
// config has that mode, so its next boot mounts the folder with it: the
// caller starts it again once, with Want.CheckCache, and otherwise fails
// with Msg.
type RemountError struct{ Msg msg.M }

func (e *RemountError) Error() string { return e.Msg.Error() }

// Unwrap returns the message, so errors.As finds it.
func (e *RemountError) Unwrap() error { return e.Msg }

// ImageSpec is an image to build. Its name, its parent's key, From and
// its Recipe are what go into it, and its key hashes every one of them;
// the rest is how its build runs, which the image keeps nothing of.
type ImageSpec struct {
	Image  string       // its name: base, or base's layers joined with + (docker+foo; image.Name)
	Key    string       // its key (image.Input's); the backend stores it under it
	Parent string       // its parent's key; "" for base
	From   *From        // base only: the container image it starts from; nil for Ubuntu
	Recipe image.Recipe // what the build runs, in order, as it is

	MemoryGiB float64 // memory while building; 0 for the backend's default
	Net       Net     // the network the build gets: NetInternet or NetNone
	// Trust is the user's CAs for the build VM: added before anything in
	// it downloads, base's preparation included, and removed once its
	// steps are done, before it stops.
	Trust Trust
	// Packages is what base's build is given for its packages, and what
	// it gives back (internal/build's): nil for neither.
	Packages *Packages
}

// ErrBoot marks a build whose VM didn't boot from what base starts from
// (BootError): the caller may pin an older one (PinOlder) and try again.
var ErrBoot = errors.New("the build VM didn't boot")

// BootError is a build that failed as its VM started, with why: errors.Is
// finds ErrBoot, and errors.As the error it wraps (a LoggedError).
type BootError struct{ Err error }

func (e *BootError) Error() string { return e.Err.Error() }

// Unwrap returns the error and ErrBoot.
func (e *BootError) Unwrap() []error { return []error{e.Err, ErrBoot} }

// ExitUnreachable is the exit code of a recipe's step that reached none of
// what it downloads from (assets/layers/apt.sh: no package source
// answered), which the backend reports as build_sources_unreachable, not
// as the step failing.
const ExitUnreachable = 75

// PackagesDir is the folder in a build VM that Packages fills and empties:
// assets/layers/apt.sh reads in/ and writes out/.
const PackagesDir = "/var/cache/devvm/packages"

// Packages is how a build's packages go in and out of its VM. Send writes
// a tar stream, which the backend unpacks into PackagesDir as root before
// the recipe's first step. Take reads a tar stream of PackagesDir/out,
// which the backend makes as root after the recipe's last step, and the
// backend then deletes PackagesDir. Each may be nil. What Take reads
// comes from the build VM, so it checks every byte.
type Packages struct {
	Send func(w io.Writer) error
	Take func(r io.Reader) error
}

// Trust is how a build VM trusts the user's own CAs while it builds: Add
// installs them and Remove takes them out again, so the image doesn't
// keep them; both are run as root, fed to sh. Env exports the variables
// that point tools at them, which each of the build's steps starts with.
// It's commands on no line of their own, each ending in "; ", so a backend
// puts it directly before the step's script, on its first line: a newline
// between would put each of the script's lines a line further down than
// its file's (image.WithPrelude). Empty for none.
type Trust struct {
	Add, Remove Script
	Env         []byte
}

// None reports whether there are no CAs to trust.
func (t Trust) None() bool { return len(t.Add.Data) == 0 }

// From is the container image base starts from (--from).
type From struct {
	Ref    string // as given: ghcr.io/org/image:tag
	Digest string // the digest devvm recorded for it (sha256:…), which is what's built
}

// Script is a script run in a VM, fed on its standard input to bash.
type Script struct {
	Name string // for status lines and errors: base.sh
	Data []byte
}

// ToolsSpec is a shared tools build: Script, fed to bash with Args, in a
// VM made from base's image, which mounts nothing from the host but Out.
type ToolsSpec struct {
	Name   string // what it builds, as messages name it: its tools
	Base   string // base's image key
	Out    string // a host folder of the build's own, writable in the VM at OutAt
	OutAt  string
	Script Script
	Args   []string
	Format string // the images' format: the backend's ToolFormat
	Net    Net    // the network the build gets: NetInternet or NetNone
	// Trust is the user's CAs for the build VM: added before the script
	// runs (Remove isn't needed: the VM is deleted, only Out is kept).
	Trust Trust
}

// Built is what a build reports, for the image's record and the build's
// output.
type Built struct {
	Origin string // what base started from: the Ubuntu image (URL sha256:DIGEST) or From's digest; "" above base
	Libc   string // base's C library: gnu, or musl (Alpine); "" above base
	// Glibc is a --from gnu base's glibc version (image.ValidGlibc);
	// "" for musl, for base from the backend's own Ubuntu, and above base.
	Glibc string
	// Fitted is base's CPUs and memory when fitting them to the host made
	// either less than BaseCPUs or BaseMemoryGiB (Fitted); zero when it didn't, and
	// above base.
	Fitted Size
}

// Want is what of a VM can change after it's made: CreateVM makes it so,
// and each Start of the stopped VM makes it so again where it differs.
// The project folder and agent folders are mounted at the same path in
// the VM as on the host.
type Want struct {
	// Size is its CPUs, memory and disk: zero fields are left as they
	// are (the image's, when it's made). A disk only grows: a smaller
	// one is refused (Validate).
	Size Size
	// Project is the project folder, read-write; "" for none (--clone).
	// It changes when the folder moved (vm's finder moves the VM with it).
	Project string
	// ProjectID is the project folder's identity as devvm recorded it
	// (platform.FolderID): a backend that can mount the folder by its
	// identity does, at Project in the VM, so a link put at Project's
	// path on the host is never followed. "" for none.
	ProjectID string
	// ProjectCache is how the guest caches the project folder's mount,
	// CacheNone or CacheMmap, on a backend whose mount has that choice
	// (FeatureMountCache); "" leaves the mount's as it is.
	ProjectCache string
	// CheckCache asks the start to check, once the VM runs, that the
	// guest mounts the project folder with ProjectCache, where its mount
	// has that choice: the start again after a RemountError. A folder
	// that isn't mounted, or a check that fails, stops the VM and fails
	// the start, and one mounted with another mode is warned of.
	CheckCache bool
	AgentDirs  []string // agents' host folders, read-only
	// AgentForwarding is whether the VM forwards an ssh-agent; changing
	// it once the VM is made needs FeatureAgentToggle.
	AgentForwarding bool
}

// The project folder mount's cache modes (Want.ProjectCache), as a 9p
// mount's cache option names them: none reads and writes through to the
// host, and mmap caches file contents in the guest, which shared writable
// mmap needs and Linux's 9p client can lose writes with.
const (
	CacheNone = "none"
	CacheMmap = "mmap"
)

// CacheModes are the cache modes devvm sets: none alone, as Linux's 9p
// client loses data with mmap, and with loose and fscache serves stale
// files too. A VM made before devvm set one keeps its backend's (mmap)
// until it's set.
func CacheModes() []string { return []string{CacheNone} }

// Validate returns the error a start of the VM in, stopped, would refuse
// w with, before it changes anything: a disk smaller than its own
// (disk_shrink).
func Validate(w Want, in Instance) error {
	if w.Size.DiskGiB > 0 && w.Size.DiskGiB < in.Size.DiskGiB {
		return msg.DiskShrink(gibText(in.Size.DiskGiB), in.Name, gibText(w.Size.DiskGiB))
	}
	return nil
}

// Apply makes in what w asks where it differs, as a Start does: a size's
// zero fields are left as they are.
func Apply(in *Instance, w Want) {
	if w.Size.CPUs > 0 {
		in.Size.CPUs = w.Size.CPUs
	}
	if w.Size.MemoryGiB > 0 {
		in.Size.MemoryGiB = w.Size.MemoryGiB
	}
	if w.Size.DiskGiB > 0 {
		in.Size.DiskGiB = w.Size.DiskGiB
	}
	in.Project = w.Project
	if w.ProjectCache != "" && w.Project != "" {
		in.ProjectCache = w.ProjectCache
	}
	in.AgentDirs = slices.Clone(w.AgentDirs)
	in.AgentForwarding = w.AgentForwarding
}

// gibText is a size in GiB as messages show it: 100, 6.5.
func gibText(g float64) string { return strconv.FormatFloat(g, 'g', -1, 64) }

// VMSpec is a VM to create from an image, as Want wants it. Mounts are
// at the same path in the VM as on the host, except the tools share.
type VMSpec struct {
	Want
	Name string
	// CloneAt is where a --clone VM keeps its own clone of the project (the
	// folder's path); "" for a VM that mounts the folder. A backend may keep
	// it on a disk of its own.
	CloneAt string
	Tools   string // the shared tools folder, read-only at ToolsAt; "" for none
	ToolsAt string // where the tools folder is in the VM

	// AgentToggle is whether its forwarding may be turned on or off later
	// (FeatureAgentToggle let through): a backend that sets forwarding
	// only when a VM is made makes it ready for that.
	AgentToggle bool
	// SSHPort is the host port the VM's ssh is forwarded from, as devvm
	// records it (Confinement.SSHPort at each start); 0 for none yet.
	SSHPort int
	// Env is the guest environment every start gives it besides its
	// proxy's (Confinement.Env): a backend that keeps it in the VM's
	// config sets it at create.
	Env map[string]string
}

// Size is a VM's CPUs, memory and disk. Zero is unset.
type Size struct {
	CPUs      int
	MemoryGiB float64
	DiskGiB   float64
}

// Ports is which guest ports a VM forwards to the same port on host
// loopback: all of them, or those in Ranges (none when it's empty).
type Ports struct {
	All    bool
	Ranges []PortRange
}

// PortRange is a range of ports, both ends included.
type PortRange struct{ From, To int }

// Net is the network a VM starts with.
type Net int

const (
	// NetDirect is the backend's own network: the internet and host
	// loopback, unfiltered. Project VMs on the open tier, whose guest
	// proxy settings name devvm's proxy (Confinement.ProxyPort set): every
	// backend reaches a port on host loopback from its own network.
	NetDirect Net = iota
	// NetProxy reaches nothing but devvm's proxy on host loopback, which
	// the guest's proxy settings name. Project VMs on a confined tier.
	NetProxy
	// NetInternet reaches the internet but not host loopback, and has no
	// port forwards. Image and tool builds.
	NetInternet
	// NetNone reaches nothing. Builds that need no network.
	NetNone
)

// Confinement is how a VM is started.
type Confinement struct {
	Net Net
	// NetProxy, and NetDirect with a ProxyPort (GuestProxy): the port
	// devvm's proxy listens on (host 127.0.0.1), and the password the
	// guest's proxy settings carry, with ProxyUser (ProxyURL).
	ProxyPort     int
	ProxyPassword string
	// Env is more guest environment, for every login (the proxy's CA
	// files, NO_PROXY).
	Env map[string]string
	// SSHPort is the host port the VM's ssh listens on; 0 lets the
	// backend choose.
	SSHPort int
	// SSH is whether the VM gets ssh for editors from this start
	// (FeatureSSH usable): a backend that emulates it runs it only then,
	// one with its own always has it.
	SSH bool
	// AgentSocket is the ssh-agent socket the VM's forwarding uses, and
	// the backend's connections to it carry: the user's own or devvm's,
	// as the VM's choice says; "" for none. The backend never takes one
	// from devvm's environment.
	AgentSocket string
	// NoAgent is set when the VM's choice is no agent: a backend whose VM
	// forwards one anyway (made so, for agent-toggle) gives it none.
	NoAgent bool
	// Pins are what of the project folder the guest can't change while
	// this start runs (FeatureReadOnlyPaths); none for an empty one.
	Pins Pins
}

// Pins are paths in a VM's project folder that a start keeps the guest
// from changing while it runs, each relative to Root, the folder as the
// VM mounts it, a physical path. The guest can write in what Fixed names
// but not rename or remove it, and it can't change what ReadOnly names at
// all. The backend pins each through no symlink, and refuses to start the
// VM when it can't.
type Pins struct {
	Root     string
	Fixed    []string
	ReadOnly []string
	// Events is a file the backend adds a PinEvent to, a JSON object a
	// line: a PinKeeping one as it starts keeping the pins (it refuses the
	// start when it can't), then one each time a pin changed while the VM
	// ran; "" for none.
	Events string
	// File is a file of devvm's own, in the VM's folder, that a backend
	// whose pinning process reads the pins from a file writes them to:
	// a VM with many nested repos has more than an environment variable
	// holds.
	File string
}

// A PinEvent is a pin that changed while the VM ran: a path, relative to
// Pins.Root, that lost its pin (a file renamed over it on the host) and
// was pinned again, that has another name now, or that can't be pinned
// again (What: platform.PinAgain, PinLinked, PinLost).
type PinEvent struct {
	Path string `json:"path,omitempty"`
	What string `json:"what"`
}

// What a PinEvent says besides how a pin changed: a run keeps its pins
// from here (each run's first line, so a file without one first lost
// lines), and an event of the path couldn't be added to the file, so
// what changed went unrecorded.
const (
	PinKeeping    = "keeping"
	PinUnrecorded = "unrecorded"
)

// Empty reports whether p pins nothing.
func (p Pins) Empty() bool { return len(p.Fixed)+len(p.ReadOnly) == 0 }

// GuestProxy reports whether the guest's proxy settings name devvm's
// proxy: NetProxy, or NetDirect with a ProxyPort, an open VM's. A build's
// NetInternet proxy goes to its scripts on their input instead.
func (c Confinement) GuestProxy() bool {
	return c.Net == NetProxy || c.Net == NetDirect && c.ProxyPort != 0
}

// Status is a VM's state as its backend reports it.
type Status string

// Statuses. A backend reports anything else (broken, starting) as it has it.
const (
	StatusRunning Status = "Running"
	StatusStopped Status = "Stopped"
)

// Instance is a VM as its backend has it.
type Instance struct {
	Name    string
	Status  Status
	Size    Size   // what it has, all set
	Project string // its project folder's mount; "" for none
	// ProjectCache is its project folder mount's cache mode (CacheNone,
	// CacheMmap, or another its backend's mount has), where the backend
	// has the choice (FeatureMountCache); "" otherwise.
	ProjectCache    string
	AgentDirs       []string // its agent folder mounts
	AgentForwarding bool
}

// ExecOpts is where a command run in a VM runs and what it's connected to.
// Nil streams are empty input and discarded output.
type ExecOpts struct {
	Dir    string // the folder in the VM; "" for /
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Agent is the ssh-agent socket the command's connection to the VM
	// carries, the VM's own (Confinement.AgentSocket); "" for none. The
	// backend never takes one from devvm's environment.
	Agent string
}

// ShellOpts is where an interactive shell in a VM opens and the ssh-agent
// its connection carries, as ExecOpts has them.
type ShellOpts struct {
	Dir   string // the folder in the VM
	Agent string // the ssh-agent socket; "" for none
}

// Command is a host command to run: Path with Args (Args[0] included), in
// Dir, with Env as its whole environment.
type Command struct {
	Path string
	Args []string
	Env  []string
	Dir  string
}

// SSHTarget is how ssh reaches a VM: devvm takes it only on the host's
// loopback, as a user with a key (vmssh.FromTarget).
type SSHTarget struct {
	Host string
	Port int
	User string
	// Key is the private key file that logs in as User, which the VM
	// authorizes.
	Key string
	// Options are the other ssh options that log in (IdentitiesOnly…),
	// in order, values as an ssh config has them. Nothing that turns off
	// host key checks or changes what ssh runs, and no IdentityFile:
	// Key is the one.
	Options []SSHOption
	// ConfigFile is an ssh config the backend keeps for the VM, with its
	// identity; "" when it has none.
	ConfigFile string
}

// SSHOption is an ssh config option: IdentityFile "/path".
type SSHOption struct{ Key, Value string }
