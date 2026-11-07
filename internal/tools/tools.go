// Package tools keeps the shared tools: the toolchains and agents image
// names hold (node, claude), which aren't image layers. Each tool version
// is one read-only image in the share, built once in a throwaway VM and
// mounted into every VM that uses it, where mise runs it.
//
// Which versions is per user: devvm's shared-tools/mise.toml says which
// releases (node 24, the latest claude), and the host's mise resolves it
// into the tools lock,
// with each download's checksum per platform. Builds install exactly the
// lock's versions, checked against those checksums. The lock is resolved
// on first use and again when mise.toml changes; once a day a
// launch resolves it again in the background, so VMs can move to newer
// versions. A VM lists its tool images in its state, so a newer version
// never changes a VM that has one.
//
// Env is what the shared tools run with; Versions keeps the lock (resolving
// it, the daily check, the image files an image name's tools are); Share
// builds, lists and deletes the share's tool images; VMs moves a VM to
// newer tools.
package tools

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/daemon"
	"github.com/devvmhq/devvm/internal/fetch"
	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/settings"
	"github.com/devvmhq/devvm/internal/state"
)

// ShareAt is where VMs have the share, read-only, and where the tools
// build has its output folder.
const ShareAt = "/mnt/devvm-tools"

// checkEvery is how often the lock is resolved again.
const checkEvery = 24 * time.Hour

// Env is what one user's shared tools run with, shared by Versions, Share
// and VMs; its methods are what they share: the platforms, devvm's
// mise.toml, a tool's image file and the share's lock. New sets every
// field; tests change them.
type Env struct {
	Dirs paths.Dirs
	// Assets holds devvm's own files (assets.FS): shared-tools/mise.toml
	// and shared-tools/build.sh.
	Assets fs.FS
	// Self is devvm's binary, which the daily check runs; Version its
	// version, which each tool image's record keeps.
	Self, Version string
	// OS and Arch are the host's, as Go names them.
	OS, Arch string
	Getenv   func(string) string
	// Client downloads the host's mise, through Proxy (its Route).
	Client *fetch.Client
	// Proxy is the company proxy the host's mise is downloaded through
	// and runs with, and that a tool build checks before it starts; its
	// caller sets its Password (secrets.Store.ProxyPassword).
	Proxy *settings.CompanyProxy
	// RetryDelay is the wait before a download's first retry; it doubles
	// after each.
	RetryDelay time.Duration
	Now        func() time.Time
	// Detach starts the daily check, which outlives the run
	// (daemon.Detach).
	Detach daemon.Starter
	// ShareNote is how long LockShare waits for another devvm holding the
	// share's lock before it says so.
	ShareNote time.Duration
	// Reporter is where it prints, the status line and run log its slow
	// steps show on, and the run's cleanup stack. Env's own Warn says
	// where the run log is too.
	msg.Reporter
	// FreeSpace returns the free space on the volume holding a path
	// (platform.Host's), checked before a build; nil checks none.
	FreeSpace func(path string) (uint64, error)
	// Mise is a test build's stand-in for devvm's pinned mise on the
	// host (internal/cli's Overrides); "" in a release.
	Mise string

	// The host's mise: its download's URL (a format taking the version
	// and platform) and checksums, by platform.
	miseURL  string
	miseSums map[string]string
}

// Versions is the tools lock: resolving it with the host's mise, the daily
// check, and the image files of an image name's tools at its versions.
type Versions struct{ *Env }

// Share is the share's tool images: building what it lacks, listing and
// deleting them.
type Share struct{ *Env }

// VMs is a VM's shared tools against the lock: whether newer ones are
// there, and moving the VM to them.
type VMs struct{ *Env }

// Warn prints a warning, and where the run log has more.
func (t *Env) Warn(m msg.M) {
	t.Reporter.Warn(m)
	t.LogNote()
}

// New returns the shared tools in devvm's folders, with devvm's own files
// (assets), for devvm's version, on r, checking host's free space before
// a build.
func New(d paths.Dirs, assets fs.FS, version string, r msg.Reporter, host platform.Host) *Env {
	self, err := os.Executable()
	if err == nil {
		if r, rerr := filepath.EvalSymlinks(self); rerr == nil {
			self = r
		}
	}
	t := &Env{
		Dirs: d, Assets: assets, Self: self, Version: version, OS: runtime.GOOS, Arch: runtime.GOARCH,
		Getenv: os.Getenv, Client: newClient(), RetryDelay: time.Second, Now: time.Now, Detach: daemon.Detach,
		ShareNote: 3 * time.Second, miseURL: miseURL, miseSums: miseSHA256,
		Reporter: r, FreeSpace: host.FreeSpace,
	}
	// The environment it reads is Getenv's, and the Proxy the one at each
	// request: tests and callers change them.
	t.Proxy = settings.NewCompanyProxy(d, func(k string) string { return t.Getenv(k) }, nil)
	if tr := t.Client.Transport(); tr != nil {
		tr.Proxy = func(req *http.Request) (*url.URL, error) { return t.Proxy.Route(req) }
	}
	return t
}

// MiseArch is mise's name for a Go architecture: the host's, and so its
// VMs'.
func MiseArch(goarch string) string {
	if goarch == "amd64" {
		return "x64"
	}
	return "arm64"
}

// platforms are the platforms the lock resolves: the VMs', glibc and musl.
func (t *Env) platforms() []string {
	a := MiseArch(t.Arch)
	return []string{"linux-" + a, "linux-" + a + "-musl"}
}

// platform is the platform of a VM whose base has libc.
func (t *Env) platform(libc string) string {
	p := "linux-" + MiseArch(t.Arch)
	if libc == "musl" {
		p += "-musl"
	}
	return p
}

// configFile is devvm's mise.toml of the shared tools, in its assets.
const configFile = "shared-tools/mise.toml"

// config is devvm's shared-tools/mise.toml.
func (t *Env) config() ([]byte, error) { return fs.ReadFile(t.Assets, configFile) }

// Aliases is the [tool_alias] table of devvm's shared-tools/mise.toml,
// each tool's backend, for a VM's mise config: the VM's mise then takes
// each tool from where its image's came from, whatever its registry says.
func Aliases(assets fs.FS) ([]byte, error) {
	data, err := fs.ReadFile(assets, configFile)
	if err != nil {
		return nil, err
	}
	aliases, err := configAliases(data)
	if err != nil {
		return nil, err
	}
	return aliasTable(aliases), nil
}

// Lock returns the tools lock, resolving it first when there's none yet,
// it isn't TOML, or shared-tools/mise.toml changed since (a devvm
// upgrade).
func (t Versions) Lock(ctx context.Context) (Lock, error) {
	want, err := t.config()
	if err != nil {
		return Lock{}, err
	}
	data, lerr := os.ReadFile(t.Dirs.ToolLock())
	from, ferr := os.ReadFile(t.Dirs.ToolLockConfig())
	if lerr == nil && ferr == nil && bytes.Equal(from, want) {
		if lock, err := ParseLock(data); err == nil {
			return lock, nil
		}
	}
	t.Info(msg.ResolvingTools())
	lock, err := t.Resolve(ctx, t.Dirs.ToolLock())
	if err != nil {
		// A certificate mise doesn't trust is said as it is: being online
		// again doesn't help.
		if _, ok := errors.AsType[*backend.LoggedError](err); ok && !errors.Is(err, errUntrusted) {
			return Lock{}, backend.Logged(msg.ToolsNeedResolve())
		}
		return Lock{}, err
	}
	return lock, t.touchChecked()
}

func (t Versions) touchChecked() error {
	now := t.Now()
	err := os.Chtimes(t.Dirs.ToolLockChecked(), now, now)
	if errors.Is(err, fs.ErrNotExist) {
		err = state.WriteFile(t.Dirs.ToolLockChecked(), nil)
		if err == nil {
			err = os.Chtimes(t.Dirs.ToolLockChecked(), now, now)
		}
	}
	return err
}

// CheckDue reports whether the daily check is due: there's a lock, not
// resolved in the last day. Never with DEVVM_NO_TOOL_CHECK set.
func (t Versions) CheckDue() bool {
	if t.Getenv("DEVVM_NO_TOOL_CHECK") != "" {
		return false
	}
	if _, err := os.Stat(t.Dirs.ToolLock()); err != nil {
		return false
	}
	fi, err := os.Stat(t.Dirs.ToolLockChecked())
	return err != nil || t.Now().Sub(fi.ModTime()) >= checkEvery
}

// RefreshDaily starts devvm _refresh-tools in the background once a day,
// when the check is due, so a later launch knows of newer versions. It
// doesn't wait for it.
func (t Versions) RefreshDaily() error {
	if !t.CheckDue() {
		return nil
	}
	if err := t.touchChecked(); err != nil {
		return err
	}
	if err := state.PrivateDir(t.Dirs.Logs()); err != nil {
		return err
	}
	out, err := os.OpenFile(t.Dirs.ToolsCheckLog(), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:forbidigo // the check's log, which it writes as it runs: each check's starts empty
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	_, err = t.Detach(daemon.Cmd{Path: t.Self, Args: []string{"_refresh-tools"}, Out: out})
	return err
}

// Refresh resolves the lock again, and swaps the new one in while no
// build is reading it. A failure leaves the lock as it was.
func (t Versions) Refresh(ctx context.Context) error {
	next := t.Dirs.ToolLock() + ".next"
	if _, err := t.Resolve(ctx, next); err != nil {
		return err
	}
	unlock, err := t.LockShare(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if err := os.Rename(next+".toml", t.Dirs.ToolLockConfig()); err != nil {
		return err
	}
	if err := os.Rename(next, t.Dirs.ToolLock()); err != nil {
		return err
	}
	return t.touchChecked()
}

// Check is Refresh for a user waiting on it: a failure is a warning, and
// the versions it has stay.
func (t Versions) Check(ctx context.Context) {
	t.Info(msg.CheckingTools())
	if err := t.Refresh(ctx); err != nil {
		if ctx.Err() == nil {
			t.Warn(msg.ToolsCheckFailed())
		}
	}
}

// Target is what a VM's tool images are built for: its base's C library
// and its backend's image format, with the glibc version a --from gnu base
// has, which each tool image's needs are checked against (Runs).
type Target struct {
	Libc   string // gnu or musl
	Format string // the backend's ToolFormat: "" for shared-tools/build.sh's own
	// Glibc is a --from gnu base's glibc version, as its record has it;
	// "" when it has none (Ubuntu's own base, musl), which isn't checked.
	// A gnu tool image is the same whatever glibc its base has, so its
	// key doesn't hash it.
	Glibc string
	// Base is, for musl, the digest of the --from base the images build
	// in (its record's), which their keys hash: musl images are never
	// shared beyond VMs on that base. gnu images always build in devvm's
	// own Ubuntu base, so have none.
	Base string
}

// File returns a tool's image file at the lock's version, for a VM of
// that target.
func (t *Env) File(lock Lock, tool string, tg Target) (string, error) {
	v := lock.Version(tool)
	if v == "" {
		return "", msg.ToolNotLocked(t.Dirs.ToolLock(), tool)
	}
	entry, err := lock.Entry(tool)
	if err != nil {
		return "", err
	}
	return image.ToolFile(t.Assets, image.Tool{Name: tool, Version: v, LockEntry: entry, Libc: tg.Libc, Base: tg.Base, Format: tg.Format})
}

// Files returns the image files of the tools behind each of these names
// (node is node, pnpm and bun), at the lock's versions.
func (t Versions) Files(ctx context.Context, names []string, tg Target) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	lock, err := t.Lock(ctx)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, n := range names {
		for _, tool := range image.GroupTools(n) {
			f, err := t.File(lock, tool, tg)
			if err != nil {
				return nil, err
			}
			files = append(files, f)
		}
	}
	return files, nil
}

// noDownload reports whether the lock has no download of a tool for a VM
// whose base has libc (no version had one), or builds it from source,
// which mise falls back to.
func (t Share) noDownload(lock Lock, tool, libc string) bool {
	return !lock.HasDownload(tool, []string{t.platform(libc)})
}

// split returns a VM's tool files as tool and version, keeping the order.
func split(files []string) (tools, versions []string) {
	for _, f := range files {
		tool, v, _ := image.ParseToolFile(f)
		tools, versions = append(tools, tool), append(versions, v)
	}
	return tools, versions
}

// Label lists tool files as "node 24.21.0, bun 1.4.2".
func Label(files []string) string {
	var out []string
	for _, f := range files {
		tool, v, _ := image.ParseToolFile(f)
		out = append(out, tool+" "+v)
	}
	return strings.Join(out, ", ")
}

// Mounts is a VM's tool list as the VM reads it: a "FILE TOOL VERSION"
// line per tool image.
func Mounts(files []string) []byte {
	var b bytes.Buffer
	tools, versions := split(files)
	for i, f := range files {
		if tools[i] == "" {
			continue
		}
		b.WriteString(f + " " + tools[i] + " " + versions[i] + "\n")
	}
	return b.Bytes()
}
