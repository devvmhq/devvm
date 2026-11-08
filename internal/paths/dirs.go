package paths

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/vmname"
)

// Dirs holds the folders devvm keeps its state in. Every file devvm writes
// outside a VM is under one of them, never in a project folder.
type Dirs struct {
	// Home is the user's home folder: $HOME, which must be absolute. It's
	// the one place devvm reads HOME; everything else takes it from here.
	Home string
	// Data is devvm's data folder: $XDG_DATA_HOME/devvm, by default
	// ~/.local/share/devvm.
	Data string
	// Config is devvm's config folder: $XDG_CONFIG_HOME/devvm, by default
	// ~/.config/devvm.
	Config string
	// Lima is devvm's own Lima home: $DEVVM_LIMA_HOME, else Data/lima. It
	// never follows LIMA_HOME, so Lima settings meant for other VMs don't
	// reach devvm's.
	Lima string
	// UserCache is the user's cache folder, as os.UserCacheDir finds it
	// but from Home: where Lima keeps its own caches, which
	// devvm space shows. devvm's own is Cache.
	UserCache string
}

// FromEnv finds devvm's folders from the environment.
func FromEnv() (Dirs, error) {
	return fromEnv(os.Getenv, runtime.GOOS)
}

func fromEnv(getenv func(string) string, goos string) (Dirs, error) {
	home := getenv("HOME")
	if !filepath.IsAbs(home) {
		return Dirs{}, msg.HomeNotAbsolute()
	}
	d := Dirs{
		Home:      filepath.Clean(home),
		Data:      filepath.Join(xdg(getenv, "XDG_DATA_HOME", home, ".local", "share"), "devvm"),
		Config:    filepath.Join(xdg(getenv, "XDG_CONFIG_HOME", home, ".config"), "devvm"),
		UserCache: xdg(getenv, "XDG_CACHE_HOME", home, ".cache"),
	}
	if goos == "darwin" {
		d.UserCache = filepath.Join(d.Home, "Library", "Caches")
	}
	d.Lima = d.DefaultLima()
	if l := getenv("DEVVM_LIMA_HOME"); l != "" {
		abs, err := filepath.Abs(l)
		if err != nil {
			return Dirs{}, err
		}
		d.Lima = abs
	}
	return d, nil
}

// xdg reads an XDG base directory variable. The spec says to ignore a
// relative path, so that falls back to the default too.
func xdg(getenv func(string) string, name, home string, def ...string) string {
	if v := getenv(name); filepath.IsAbs(v) {
		return filepath.Clean(v)
	}
	return filepath.Join(append([]string{home}, def...)...)
}

// Version holds the data folder's layout version: which layout of its
// folders and files it has. ConfigVersion is the config folder's.
func (d Dirs) Version() string { return filepath.Join(d.Data, "version") }

// ConfigVersion holds the config folder's layout version.
func (d Dirs) ConfigVersion() string { return filepath.Join(d.Config, "version") }

// VersionLock is held while a run checks the layout versions, and
// changes the folders to this devvm's layout when they're older.
func (d Dirs) VersionLock() string { return filepath.Join(d.Data, "version.lock") }

// VMs is the folder of the VMs' folders, one each (VMDir).
func (d Dirs) VMs() string { return filepath.Join(d.Data, "vms") }

// VMDir is the folder of the project VM whose hash is hash
// (vmname.HashOf): every file devvm keeps for it, so deleting the VM
// deletes the folder.
func (d Dirs) VMDir(hash string) string { return filepath.Join(d.VMs(), hash) }

// VM is the files devvm keeps for the VM named vm: a project VM's in its
// folder (VMDir, by the hash its name ends with), any other's (an image's
// build VM, the shared tools' build VM) in a folder of its own in the run
// folder's builds.
func (d Dirs) VM(vm string) VM {
	if h, ok := vmname.HashOf(vm); ok {
		return VM{Dir: d.VMDir(h)}
	}
	return VM{Dir: filepath.Join(d.Run(), "builds", vm)}
}

// BuildPorts holds the host port chosen for each build VM's ssh while the
// VM starts (ports.Allocator.Build), a file named by the VM's name: there
// only until its backend has bound the port, or failed to.
func (d Dirs) BuildPorts() string { return filepath.Join(d.Run(), "build-ports") }

// Images is the folder of images' records, one folder per image key.
func (d Dirs) Images() string { return filepath.Join(d.Data, "images") }

// Image is the folder of the image with this key.
func (d Dirs) Image(key string) string { return filepath.Join(d.Images(), key) }

// ImageRecord is the record of the image with this key.
func (d Dirs) ImageRecord(key string) string { return filepath.Join(d.Image(key), "record.json") }

// ImageLocks holds the lock of each image being built, one file per key.
func (d Dirs) ImageLocks() string { return filepath.Join(d.Images(), "locks") }

// Rebuilds holds the rebuild counters, which go into image keys. They
// can't be in an image's folder: a counter is an input to the key that
// names it.
func (d Dirs) Rebuilds() string { return filepath.Join(d.Images(), "rebuilds.json") }

// RebuildsLock is held by the devvm changing a rebuild counter, which
// reads Rebuilds and writes it whole.
func (d Dirs) RebuildsLock() string { return filepath.Join(d.ImageLocks(), "rebuilds") }

// UbuntuPin is the Ubuntu cloud image base starts from on this machine,
// pinned at its first build: base's key hashes it, so it can't be in an
// image's folder.
func (d Dirs) UbuntuPin() string { return filepath.Join(d.Images(), "ubuntu.json") }

// UbuntuPinLock is held by the devvm pinning Ubuntu's cloud image.
func (d Dirs) UbuntuPinLock() string { return filepath.Join(d.ImageLocks(), "ubuntu") }

// Packages is the cache of base's packages: each package file under its
// SHA-256 (PackageFile), and the packages each base key recorded at its
// first build (PackageSet), which later builds of that key install from
// here.
func (d Dirs) Packages() string { return filepath.Join(d.Data, "packages") }

// PackageFile is the package file with this SHA-256, in Packages.
func (d Dirs) PackageFile(sha256 string) string { return filepath.Join(d.Packages(), sha256+".deb") }

// PackageSets holds the package set each base key recorded.
func (d Dirs) PackageSets() string { return filepath.Join(d.Packages(), "sets") }

// PackageSet is the package set the base with this key recorded.
func (d Dirs) PackageSet(key string) string { return filepath.Join(d.PackageSets(), key+".json") }

// PackagesLock is held while the package cache changes: a build storing
// what it recorded, or devvm gc deleting what no set names.
func (d Dirs) PackagesLock() string { return filepath.Join(d.ImageLocks(), "packages") }

// Cache holds what devvm makes again whenever it's needed, which may be
// deleted at any time: devvm's own mise, the shared Nix cache and the
// digests --from bases were last looked up at.
func (d Dirs) Cache() string { return filepath.Join(d.Data, "cache") }

// FromDigests caches the digest of each image a base was built from, per
// reference.
func (d Dirs) FromDigests() string { return filepath.Join(d.Cache(), "from-digests.json") }

// ReleaseCheck is the daily release check's record: the newest devvm
// release it found, and when it last checked.
func (d Dirs) ReleaseCheck() string { return filepath.Join(d.Cache(), "release.json") }

// Run holds what running VMs need that isn't one VM's, or whose path
// must be short: the QEMU wrapper, the claims lock, the port forwarders'
// and link sessions' sockets, named by hashes (a socket's path has fewer
// than SocketMax characters), and build VMs' folders (VM).
func (d Dirs) Run() string { return filepath.Join(d.Data, "run") }

// ClaimsLock is held while a VM claims what no two VMs may have: its ssh
// alias as it's made, and the host ports devvm chooses for it. It's taken
// after the VM's own lock (VM.Lock), never before.
func (d Dirs) ClaimsLock() string { return filepath.Join(d.Run(), "claims.lock") }

// CreateLock is the creation lock of the project folder whose hash is
// hash (project.Hash): held while a devvm makes the folder's VM, from
// when it finds the folder has none until the VM's creation is recorded.
// It's outside the VM's folder (VMDir), which a creation deletes and
// makes again, so it stays when the VM goes.
func (d Dirs) CreateLock(hash string) string {
	return filepath.Join(d.Run(), "create-"+hash+".lock")
}

// QEMUWrapper is the folder of the wrapper a confined start on a Linux
// host gives Lima as QEMU, which runs QEMU with the VM's network.
func (d Dirs) QEMUWrapper() string { return filepath.Join(d.Run(), "qemu") }

// SocketMax is the size of a unix socket's path on macOS, its NUL
// included, so a path has fewer characters (Linux's is 108). ssh adds 17
// characters to a control socket's while it makes it.
const SocketMax = 104

// Logs holds the logs that aren't one VM's: the runs' (RunLogs) and the
// daily tools check's.
func (d Dirs) Logs() string { return filepath.Join(d.Data, "logs") }

// RunLogs holds the logs of devvm's runs: builds and the commands they ran.
func (d Dirs) RunLogs() string { return filepath.Join(d.Logs(), "runs") }

// Tools is the shared tools folder, mounted read-only into VMs.
func (d Dirs) Tools() string { return filepath.Join(d.Data, "tools") }

// ToolImage is a tool image in the share, by its file's name.
func (d Dirs) ToolImage(file string) string { return filepath.Join(d.Tools(), file) }

// ShareLock is the lock file the devvm writing to the share (a tool
// image build) or swapping the tools lock holds: beside the share, which
// VMs mount, not in it.
func (d Dirs) ShareLock() string { return filepath.Join(d.Data, "tools.lock") }

// ToolsBuildPrefix starts the name of the folder of its own, in the data
// folder, that a shared tools build writes its tool images into, with
// os.MkdirTemp's random ending after it.
const ToolsBuildPrefix = "tools-build."

// ToolLock is the lock file the shared tools are resolved into.
func (d Dirs) ToolLock() string { return filepath.Join(d.Data, "mise.lock") }

// ToolRecord is the record beside a tool image in the share: its hash,
// what it's built for, when, and since when no VM has had it.
func (d Dirs) ToolRecord(file string) string { return filepath.Join(d.Tools(), file+".json") }

// ToolLockConfig is the shared tools' mise.toml the lock was resolved
// from.
func (d Dirs) ToolLockConfig() string { return d.ToolLock() + ".toml" }

// ToolLockChecked is touched whenever the lock is resolved.
func (d Dirs) ToolLockChecked() string { return d.ToolLock() + ".checked" }

// ToolsCheckLog is the daily tools check's output.
func (d Dirs) ToolsCheckLog() string { return filepath.Join(d.Logs(), "tools-check.log") }

// Mise holds the mise versions devvm runs on the host.
func (d Dirs) Mise() string { return filepath.Join(d.Cache(), "mise") }

// Exports is where exported VM state goes by default.
func (d Dirs) Exports() string { return filepath.Join(d.Data, "exports") }

// ExportFolder is the file beside an export in Exports that names the
// project folder of the VM it's of: the export's path with .folder for
// .tar.gz.
func ExportFolder(export string) string { return strings.TrimSuffix(export, ".tar.gz") + ".folder" }

// NixCache is the Nix cache VMs share, which their
// proxies fill from cache.nixos.org.
func (d Dirs) NixCache() string { return filepath.Join(d.Cache(), "nix-cache") }

// AgentSock is the socket of devvm's own SSH agent.
func (d Dirs) AgentSock() string { return filepath.Join(d.Data, "agent.sock") }

// OCIBuildLock is held while a base image is built from a container image.
func (d Dirs) OCIBuildLock() string { return filepath.Join(d.Data, "oci-build.lock") }

// MakingLock is held, shared, by each devvm making a VM or giving one new
// shared tools, and tried exclusively by devvm gc --tool-images --force.
func (d Dirs) MakingLock() string { return filepath.Join(d.Data, "making.lock") }

// DefaultLima is devvm's Lima home when DEVVM_LIMA_HOME doesn't move it.
func (d Dirs) DefaultLima() string { return filepath.Join(d.Data, "lima") }

// LimaVM is a VM's folder in devvm's Lima home.
func (d Dirs) LimaVM(vm string) string { return filepath.Join(d.Lima, vm) }

// ConfigFile is config.toml, the defaults you set by hand: devvm reads it
// and never writes it.
func (d Dirs) ConfigFile() string { return filepath.Join(d.Config, "config.toml") }

// Settings is settings.json, the defaults devvm's commands set (devvm
// setup, devvm size -g, devvm retention…), below config.toml's.
func (d Dirs) Settings() string { return filepath.Join(d.Data, "settings.json") }

// AgentKeys lists the keys added to devvm's SSH agent.
func (d Dirs) AgentKeys() string { return filepath.Join(d.Data, "agent-keys.json") }

// RetentionRun is touched when retention last ran.
func (d Dirs) RetentionRun() string { return filepath.Join(d.Data, "retention.run") }

// RetentionLock is the lock file the devvm applying the retention
// settings holds, so that one at a time applies them.
func (d Dirs) RetentionLock() string { return filepath.Join(d.Data, "retention.lock") }

// GroupsLock is the lock file each devvm joining or leaving a group, or
// working a group's links out again, holds throughout, so that they take
// turns.
func (d Dirs) GroupsLock() string { return filepath.Join(d.Data, "groups.lock") }

// Layers holds the user's own layers.
func (d Dirs) Layers() string { return filepath.Join(d.Config, "layers") }

// Policy holds the user's network rules for every VM.
func (d Dirs) Policy() string { return filepath.Join(d.Config, "policy") }

// TrustedCAs holds the user's own CA certificates (a company's), which
// VMs and builds trust besides their systems' (internal/trust).
func (d Dirs) TrustedCAs() string { return filepath.Join(d.Config, "ca") }

// AgentSettings is devvm's store of an agent's settings, laid out as the
// agent's files are in the home folder (claude's .claude/settings.json,
// .claude/skills…), which every VM with the agent gets: its folders
// mounted read-only, its files copied in. devvm agent import fills it.
func (d Dirs) AgentSettings(agent string) string { return filepath.Join(d.Config, "agents", agent) }

// AgentLogins is devvm's store of an agent's logins, laid out as they are
// in the home folder (codex's .codex/auth.json, claude's .claude.json),
// only the user's to read and kept out of backups.
func (d Dirs) AgentLogins(agent string) string { return filepath.Join(d.Data, "logins", agent) }

// Secrets holds secrets' values on hosts without a Keychain, one file each.
func (d Dirs) Secrets() string { return filepath.Join(d.Data, "secrets") }

// SSHConfig is the SSH config devvm writes for its VMs.
func (d Dirs) SSHConfig() string { return filepath.Join(d.Data, "ssh_config") }
