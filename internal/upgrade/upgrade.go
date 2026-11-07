// Package upgrade installs devvm's newest release the way this devvm was
// installed: for install.sh's, the release's archive, checked against its
// SHA256SUMS, whose minisign signature is checked against the release
// key built into devvm, unpacked beside the old version, and the link
// switched to it once it runs; for Homebrew's, brew upgrade devvm; for
// any other, nothing but saying how it was installed.
package upgrade

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/platform"
)

// runsTimeout is how long a new devvm gets to say its version.
const runsTimeout = 30 * time.Second

// Available is what Check finds: this devvm's version, the newest
// release's, and whether that's newer.
type Available struct {
	Current, Latest string
	Newer           bool
}

// Check returns the newest release at s and whether it's newer than
// current, this devvm's version. It installs nothing, for devvm upgrade
// --check and a notice that a release is out.
func (s Source) Check(ctx context.Context, current string) (Available, error) {
	latest, err := s.Latest(ctx)
	if err != nil {
		return Available{}, err
	}
	return Available{Current: current, Latest: latest, Newer: Valid(current) && Compare(latest, current) > 0}, nil
}

// An Upgrader installs a release over this devvm, reporting through its
// Reporter, which asks before anything is installed.
type Upgrader struct {
	msg.Reporter
	Source  Source
	Install Install
	// Current is this devvm's version.
	Current string
	// FromSource is a build without a release's version, which only
	// Go's build information gives (go install, a checkout's go build).
	FromSource bool
}

// Options are devvm upgrade's: Version, a release to install instead of
// the newest, which may be older than this one; Force, to install without
// asking.
type Options struct {
	Version string
	Force   bool
}

// Upgrade installs the newest release, or o.Version, over this devvm, as
// it was installed, once it's asked. The newest release is never a
// prerelease (Latest), so only o.Version installs one. It refuses a
// newest release older than this devvm, as a server sending an old
// release would, unless o.Version names it; for a prerelease of a
// release not yet out, that's no failure, only said.
func (u Upgrader) Upgrade(ctx context.Context, o Options) error {
	if u.Install.Kind == Other || !Valid(u.Current) {
		return u.NotInstalled()
	}
	if u.Install.Kind == Homebrew && o.Version != "" {
		return msg.UpgradeBrewVersion()
	}
	target := o.Version
	if target == "" {
		done := u.Status(msg.UpgradeChecking())
		latest, err := u.Source.Latest(ctx)
		done()
		if err != nil {
			return err
		}
		target = latest
	}
	c := Compare(target, u.Current)
	switch {
	case c == 0 && o.Version != "":
		u.Info(msg.UpgradeAlready(u.Current))
		return nil
	case c == 0:
		u.Info(msg.UpgradeAlreadyLatest(u.Current))
		return nil
	case c < 0 && o.Version == "" && Prerelease(u.Current):
		// A prerelease of the next release, before that release is out.
		u.Info(msg.UpgradePrereleaseAhead(u.Current, target))
		return nil
	case c < 0 && o.Version == "":
		return msg.UpgradeLatestOlder(u.Current, target)
	}
	if ok, err := u.confirm(o, target, c < 0); !ok || err != nil {
		return err
	}
	if u.Install.Kind == Homebrew {
		return u.brew(ctx)
	}
	return u.script(ctx, target)
}

// confirm asks before installing target, unless o.Force; false, having
// said nothing was installed, for no.
func (u Upgrader) confirm(o Options, target string, older bool) (bool, error) {
	if o.Force {
		return true, nil
	}
	q := msg.UpgradeConfirm(u.Current, target)
	switch {
	case u.Install.Kind == Homebrew:
		q = msg.UpgradeConfirmBrew(u.Current, target)
	case older:
		q = msg.UpgradeConfirmOlder(u.Current, target)
	}
	ok, err := u.Confirm(q.AnsweredBy("--force"))
	if err == nil && !ok {
		u.Info(msg.UpgradeNothingInstalled())
	}
	return ok && err == nil, err
}

// NotInstalled is the failure for a devvm that devvm upgrade can't
// upgrade, saying how it was installed: go install, a build from a
// checkout, or a copy somewhere else.
func (u Upgrader) NotInstalled() msg.M {
	switch {
	case !Valid(u.Current):
		// A checkout's pseudo-version, or (devel): not a release's.
		return msg.UpgradeCheckoutBuild(u.Current)
	case u.FromSource:
		// A module's version, without a release's ldflags.
		return msg.UpgradeGoInstall(u.Current)
	}
	return msg.UpgradeOtherInstall(u.Install.Exe)
}

// brew runs brew upgrade devvm, which upgrades Homebrew's devvm.
func (u Upgrader) brew(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "brew", "upgrade", "devvm")
	if err := u.LoggedCmd(ctx, msg.UpgradeBrewStep(), cmd); err != nil {
		return backend.Logged(msg.UpgradeBrewFailed())
	}
	u.Info(msg.UpgradeBrewDone())
	return nil
}

// script installs version as install.sh does: downloaded and checked
// (Source.Fetch), unpacked into a new folder beside the old version,
// run, moved to its version's folder, and the link switched to it, then
// run again through the link. Until that last run works, the link is
// put back on any failure, so the old version stays the one that runs.
func (u Upgrader) script(ctx context.Context, version string) error {
	tmp, err := os.MkdirTemp("", "devvm-upgrade.")
	if err != nil {
		return msg.UpgradeFailed(version).Because(err)
	}
	remove := u.AddCleanup(func(context.Context) { _ = os.RemoveAll(tmp) })
	defer func() { remove(); _ = os.RemoveAll(tmp) }()

	done := u.Status(msg.UpgradeDownloading(version))
	archive, err := u.Source.Fetch(ctx, version, tmp)
	done()
	if err != nil {
		return err
	}
	pid := strconv.Itoa(os.Getpid())
	lib := u.Install.Lib()
	staged := filepath.Join(lib, "."+version+".new."+pid)
	unstage := u.AddCleanup(func(context.Context) { _ = os.RemoveAll(staged) })
	defer func() { unstage(); _ = os.RemoveAll(staged) }()
	if err := unpack(archive, Archive(version), staged); err != nil {
		return msg.UpgradeBadArchive(version).Because(err)
	}
	if err := runs(ctx, filepath.Join(staged, "devvm"), version); err != nil {
		return msg.UpgradeNewFailed(version).Because(err)
	}
	if err := u.place(ctx, staged, version); err != nil {
		return err
	}
	u.prune(ctx, version)
	u.Info(msg.UpgradeDone(u.Install.Link(), version))
	return nil
}

// place moves staged to version's folder and switches the link to it,
// then runs it through the link; on any failure it puts the link and the
// folders back as they were.
func (u Upgrader) place(ctx context.Context, staged, version string) error {
	pid := strconv.Itoa(os.Getpid())
	final := filepath.Join(u.Install.Lib(), version)
	// A folder for version from before, which a running VM may use: set
	// aside, and back if this fails.
	aside := ""
	if _, err := os.Lstat(final); err == nil {
		aside = filepath.Join(u.Install.Lib(), "."+version+".old."+pid)
		if err := os.Rename(final, aside); err != nil {
			return msg.UpgradeFailed(version).Because(err)
		}
	}
	restore := func() {
		if aside != "" {
			_ = os.RemoveAll(final)
			_ = os.Rename(aside, final)
		}
	}
	if err := os.Rename(staged, final); err != nil {
		restore()
		return msg.UpgradeFailed(version).Because(err)
	}
	if err := switchLink(u.Install.Link(), filepath.Join(final, "devvm")); err != nil {
		_ = os.Rename(final, staged)
		restore()
		return msg.UpgradeFailed(version).Because(err)
	}
	if err := runs(ctx, u.Install.Link(), version); err != nil {
		if rerr := switchLink(u.Install.Link(), u.Install.Exe); rerr != nil {
			// The link points at the new folder, which stays: the user
			// puts the link back.
			return msg.UpgradeRollbackFailed(u.Install.Link(), u.Install.Exe).Because(errors.Join(err, rerr))
		}
		_ = os.Rename(final, staged)
		restore()
		return msg.UpgradeNewFailed(version).Because(err)
	}
	if aside != "" {
		_ = os.RemoveAll(aside)
	}
	return nil
}

// switchLink points link at target in one step: a new link beside it,
// renamed over it, so link is never missing or half made.
func switchLink(link, target string) error {
	tmp := filepath.Join(filepath.Dir(link), ".devvm.new."+strconv.Itoa(os.Getpid()))
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// runs checks that the devvm at path runs and says it's version.
func runs(ctx context.Context, path, version string) error {
	ctx, cancel := context.WithTimeout(ctx, runsTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return err
	}
	if got := strings.TrimSpace(string(out)); got != version {
		return errors.New(path + " --version says " + got)
	}
	return nil
}

// prune removes the versions in install.sh's folder but version and the
// one this devvm runs from, as install.sh does: except one a running
// process, such as a VM's proxy, runs from, which a later upgrade
// removes. When the processes can't be listed, it keeps them all.
func (u Upgrader) prune(ctx context.Context, version string) {
	lib := u.Install.Lib()
	entries, err := os.ReadDir(lib)
	if err != nil {
		return
	}
	out, err := exec.CommandContext(ctx, "ps", "-A", "-ww", "-o", "command=").Output()
	if err != nil {
		return
	}
	procs := string(out)
	running := filepath.Dir(u.Install.Exe)
	for _, e := range entries {
		dir := filepath.Join(lib, e.Name())
		if !e.IsDir() || e.Name() == version || dir == running {
			continue
		}
		if leftover, ok := leftoverOf(e.Name()); ok {
			if !leftover {
				continue // another devvm upgrade's, still running
			}
		} else if !Valid(e.Name()) {
			continue
		} else if strings.Contains(procs, dir+"/") || strings.Contains(procs, strings.ReplaceAll(dir, ",", ",,")+"/") {
			// QEMU's guestfwd command names devvm with its commas doubled.
			u.Info(msg.UpgradeKeptInUse(e.Name()))
			continue
		}
		_ = os.RemoveAll(dir)
	}
}

// leftoverOf reads name as a folder an upgrade sets aside or stages,
// .<version>.new.<pid> or .<version>.old.<pid> (install.sh's
// .<version>.new.<pid> too): ok when it is one, and leftover when no
// process <pid> runs, so nothing is still using it.
func leftoverOf(name string) (leftover, ok bool) {
	rest, hidden := strings.CutPrefix(name, ".")
	if !hidden {
		return false, false
	}
	i := max(strings.LastIndex(rest, ".new."), strings.LastIndex(rest, ".old."))
	if i < 0 || !Valid(rest[:i]) {
		return false, false
	}
	pid, err := strconv.Atoi(rest[i+len(".new."):])
	if err != nil || pid <= 0 {
		return true, true
	}
	_, err = platform.ProcessArgs(pid)
	return err != nil, true
}
