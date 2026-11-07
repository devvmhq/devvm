package upgrade_test

import (
	"context"
	"crypto/x509"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/upgrade"
	"github.com/devvmhq/devvm/internal/upgrade/upgradetest"
)

// fixture is an install.sh install of devvm current (a script that says
// its version) under prefix, and a release server with the release key.
type fixture struct {
	t       *testing.T
	prefix  string
	current string
	srv     *upgradetest.Server
	key     upgradetest.Key
	infos   msg.Recorder
	fake    *msg.Fake
}

func newFixture(t *testing.T, current string) *fixture {
	t.Helper()
	prefix, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, prefix: prefix, current: current, srv: upgradetest.NewServer(t), key: upgradetest.NewKey()}
	f.fake = &msg.Fake{OnInfo: f.infos.Add}
	writeExe(t, f.versionDir(current), upgradetest.Script(current))
	if err := os.MkdirAll(filepath.Join(prefix, "bin"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.exe(current), f.link()); err != nil {
		t.Fatal(err)
	}
	return f
}

func writeExe(t *testing.T, dir string, body []byte) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "devvm"), body, 0o700); err != nil { //nolint:gosec // a test's own fixture
		t.Fatal(err)
	}
}

func (f *fixture) versionDir(v string) string { return filepath.Join(f.prefix, "lib", "devvm", v) }
func (f *fixture) exe(v string) string        { return filepath.Join(f.versionDir(v), "devvm") }
func (f *fixture) link() string               { return filepath.Join(f.prefix, "bin", "devvm") }

// release serves version's release, signed with the release key.
func (f *fixture) release(version string, latest bool) {
	f.srv.Add(version, upgradetest.NewRelease(f.key, version, nil), latest)
}

func (f *fixture) upgrader() upgrade.Upgrader {
	src := upgrade.NewSource(f.srv.URL, f.key.Public())
	src.Client.Trust(f.srv.Roots())
	return upgrade.Upgrader{Reporter: f.fake, Source: src, Install: upgrade.Detect(f.link()), Current: f.current}
}

// linked is the version folder the link points at.
func (f *fixture) linked() string {
	f.t.Helper()
	target, err := os.Readlink(f.link())
	if err != nil {
		f.t.Fatal(err)
	}
	return filepath.Base(filepath.Dir(target))
}

// lib lists install.sh's folder.
func (f *fixture) lib() []string {
	f.t.Helper()
	entries, err := os.ReadDir(filepath.Join(f.prefix, "lib", "devvm"))
	if err != nil {
		f.t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// unchanged checks that nothing was installed: the link still at
// current, and only current's folder.
func (f *fixture) unchanged() {
	f.t.Helper()
	if got := f.linked(); got != f.current {
		f.t.Errorf("the link points at %s", got)
	}
	if got := f.lib(); !slices.Equal(got, []string{f.current}) {
		f.t.Errorf("lib holds %q", got)
	}
}

// install.sh's devvm is its version folder's, which bin/devvm links to;
// a copy the link doesn't point at isn't; Homebrew's is in a Cellar.
func TestDetect(t *testing.T) {
	f := newFixture(t, "0.1.0")
	if in := upgrade.Detect(f.link()); in.Kind != upgrade.Script || in.Prefix != f.prefix || in.Exe != f.exe("0.1.0") {
		t.Errorf("Detect(link) = %+v", in)
	}
	if in := upgrade.Detect(f.exe("0.1.0")); in.Kind != upgrade.Script {
		t.Errorf("Detect(exe) = %+v", in)
	}
	writeExe(t, f.versionDir("0.0.9"), upgradetest.Script("0.0.9"))
	if in := upgrade.Detect(f.exe("0.0.9")); in.Kind != upgrade.Other {
		t.Errorf("a version the link doesn't point at: %+v", in)
	}
	keg := filepath.Join(f.prefix, "Cellar", "devvm", "0.1.0", "bin")
	writeExe(t, keg, nil)
	if in := upgrade.Detect(filepath.Join(keg, "devvm")); in.Kind != upgrade.Homebrew {
		t.Errorf("a keg's: %+v", in)
	}
	writeExe(t, filepath.Join(f.prefix, "go", "bin"), nil)
	if in := upgrade.Detect(filepath.Join(f.prefix, "go", "bin", "devvm")); in.Kind != upgrade.Other {
		t.Errorf("go install's: %+v", in)
	}
	if in := upgrade.Detect(filepath.Join(f.prefix, "nowhere")); in.Kind != upgrade.Other {
		t.Errorf("a path that isn't there: %+v", in)
	}
}

// An install.sh install gets the newest release beside it, the link
// switched to it; the version it ran from stays, and so do a version a
// running process uses and a running upgrade's folder, while older
// versions and the leftovers of upgrades that ended go.
func TestUpgradeScript(t *testing.T) {
	f := newFixture(t, "0.1.0")
	f.release("0.2.0", true)
	writeExe(t, f.versionDir("0.0.9"), upgradetest.Script("0.0.9"))
	writeExe(t, f.versionDir("0.0.8"), []byte("#!/bin/sh\nsleep 60\n"))
	lib := filepath.Join(f.prefix, "lib", "devvm")
	live := ".0.3.0.new." + strconv.Itoa(os.Getpid())
	for _, d := range []string{".0.3.0.new.2147483646", ".0.2.0.old.2147483646", live, ".other", "notes"} {
		writeExe(t, filepath.Join(lib, d), nil)
	}
	ctx, cancel := context.WithCancel(t.Context())
	inUse := exec.CommandContext(ctx, f.exe("0.0.8")) //nolint:gosec // the test's own script, which a running VM's proxy stands for
	if err := inUse.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); _ = inUse.Wait() })

	if err := f.upgrader().Upgrade(t.Context(), upgrade.Options{Force: true}); err != nil {
		t.Fatal(err)
	}
	if got := f.linked(); got != "0.2.0" {
		t.Errorf("the link points at %s", got)
	}
	if got := f.lib(); !slices.Equal(got, []string{live, ".other", "0.0.8", "0.1.0", "0.2.0", "notes"}) {
		t.Errorf("lib holds %q", got)
	}
	if _, err := os.Stat(filepath.Join(f.versionDir("0.2.0"), "README.md")); err != nil {
		t.Error(err)
	}
	if fi, err := os.Stat(f.exe("0.2.0")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("devvm: %v, %v", fi, err)
	}
	if got := f.infos.Names(); !slices.Equal(got, []string{"upgrade_kept_in_use version=0.0.8", "upgrade_done link=" + f.link() + " version=0.2.0"}) {
		t.Errorf("said %q", got)
	}
}

// It asks first, without --force: no terminal refuses, naming --force,
// and no installs nothing.
func TestUpgradeAsks(t *testing.T) {
	f := newFixture(t, "0.1.0")
	f.release("0.2.0", true)
	err := f.upgrader().Upgrade(t.Context(), upgrade.Options{})
	if r, ok := errors.AsType[*msg.Refusal](err); !ok || r.Question.ID() != "upgrade_confirm" {
		t.Errorf("without a terminal: %v", err)
	}
	f.unchanged()

	var asked msg.Recorder
	f.fake.Terminal = true
	f.fake.OnConfirm = func(m msg.M) bool { asked.Add(m); return false }
	if err := f.upgrader().Upgrade(t.Context(), upgrade.Options{}); err != nil {
		t.Fatal(err)
	}
	f.unchanged()
	if got := asked.Names(); !slices.Equal(got, []string{"upgrade_confirm current=0.1.0 version=0.2.0"}) {
		t.Errorf("asked %q", got)
	}
	if got := f.infos.IDs(); !slices.Equal(got, []string{"upgrade_nothing_installed"}) {
		t.Errorf("said %q", got)
	}

	f.fake.OnConfirm = func(msg.M) bool { return true }
	if err := f.upgrader().Upgrade(t.Context(), upgrade.Options{}); err != nil {
		t.Fatal(err)
	}
	if got := f.linked(); got != "0.2.0" {
		t.Errorf("the link points at %s", got)
	}
}

// The newest release when it's this one: nothing to do.
func TestUpgradeAlreadyLatest(t *testing.T) {
	f := newFixture(t, "0.1.0")
	f.release("0.1.0", true)
	if err := f.upgrader().Upgrade(t.Context(), upgrade.Options{Force: true}); err != nil {
		t.Fatal(err)
	}
	f.unchanged()
	if err := f.upgrader().Upgrade(t.Context(), upgrade.Options{Version: "0.1.0", Force: true}); err != nil {
		t.Fatal(err)
	}
	f.unchanged()
	if got := f.infos.Names(); !slices.Equal(got, []string{"upgrade_already_latest version=0.1.0", "upgrade_already version=0.1.0"}) {
		t.Errorf("said %q", got)
	}
}

// A newest release older than this devvm is refused, as a server sending
// an old release would be, even with --force; --to installs it, asking
// about the older version.
func TestUpgradeRefusesOlder(t *testing.T) {
	f := newFixture(t, "0.2.0")
	f.release("0.1.0", true)
	err := f.upgrader().Upgrade(t.Context(), upgrade.Options{Force: true})
	if got := msg.NamesOf(err); got != "upgrade_latest_older current=0.2.0 latest=0.1.0" {
		t.Errorf("Upgrade = %s", got)
	}
	f.unchanged()

	var asked msg.Recorder
	f.fake.Terminal = true
	f.fake.OnConfirm = func(m msg.M) bool { asked.Add(m); return true }
	if err := f.upgrader().Upgrade(t.Context(), upgrade.Options{Version: "0.1.0"}); err != nil {
		t.Fatal(err)
	}
	if got := asked.IDs(); !slices.Equal(got, []string{"upgrade_confirm_older"}) {
		t.Errorf("asked %q", got)
	}
	if got := f.linked(); got != "0.1.0" {
		t.Errorf("the link points at %s", got)
	}
}

// A release that isn't the one signed, or whose devvm doesn't run, is
// refused, and the devvm that runs stays as it was.
func TestUpgradeRefusesBadRelease(t *testing.T) {
	name := upgrade.Archive("0.2.0") + ".tar.gz"
	top := upgrade.Archive("0.2.0")
	archive := func(entries map[string]upgradetest.Entry) []byte { return upgradetest.Archive(top, entries) }
	devvm := upgradetest.Entry{Body: upgradetest.Script("0.2.0")}
	for _, tc := range []struct {
		name    string
		release func(k upgradetest.Key) upgradetest.Release
		noKey   bool
		want    string
	}{
		{"signed with another key", func(upgradetest.Key) upgradetest.Release {
			return upgradetest.NewRelease(upgradetest.NewKey(), "0.2.0", nil)
		}, false, "upgrade_bad_signature"},
		{"signed for another version", func(k upgradetest.Key) upgradetest.Release {
			r := upgradetest.NewRelease(k, "0.2.0", nil)
			r["SHA256SUMS.minisig"] = k.Sign(r["SHA256SUMS"], "devvm 0.3.0")
			return r
		}, false, "upgrade_bad_signature"},
		{"SHA256SUMS changed after signing", func(k upgradetest.Key) upgradetest.Release {
			r := upgradetest.NewRelease(k, "0.2.0", nil)
			r["SHA256SUMS"] = append(r["SHA256SUMS"], "x\n"...)
			return r
		}, false, "upgrade_bad_signature"},
		{"no signature", func(k upgradetest.Key) upgradetest.Release {
			r := upgradetest.NewRelease(k, "0.2.0", nil)
			delete(r, "SHA256SUMS.minisig")
			return r
		}, false, "upgrade_download_failed"},
		{"the archive replaced", func(k upgradetest.Key) upgradetest.Release {
			r := upgradetest.NewRelease(k, "0.2.0", nil)
			r[name] = archive(map[string]upgradetest.Entry{"devvm": {Body: []byte("#!/bin/sh\necho 0.2.0 evil\n")}})
			return r
		}, false, "upgrade_bad_checksum"},
		{"no archive for this machine", func(k upgradetest.Key) upgradetest.Release {
			return upgradetest.Signed(k, "0.2.0", "devvm-0.2.0-plan9-mips.tar.gz", archive(map[string]upgradetest.Entry{"devvm": devvm}))
		}, false, "upgrade_no_archive"},
		{"a link in the archive", func(k upgradetest.Key) upgradetest.Release {
			return upgradetest.Signed(k, "0.2.0", name, archive(map[string]upgradetest.Entry{"devvm": devvm, "x": {Link: "/etc/passwd"}}))
		}, false, "upgrade_bad_archive"},
		{"a path out of the archive's folder", func(k upgradetest.Key) upgradetest.Release {
			return upgradetest.Signed(k, "0.2.0", name, archive(map[string]upgradetest.Entry{"devvm": devvm, "../x": {Body: []byte("x")}}))
		}, false, "upgrade_bad_archive"},
		{"a folder in the archive's folder", func(k upgradetest.Key) upgradetest.Release {
			return upgradetest.Signed(k, "0.2.0", name, archive(map[string]upgradetest.Entry{"devvm": devvm, "sub/x": {Body: []byte("x")}}))
		}, false, "upgrade_bad_archive"},
		{"no devvm in the archive", func(k upgradetest.Key) upgradetest.Release {
			return upgradetest.Signed(k, "0.2.0", name, archive(map[string]upgradetest.Entry{"README.md": {Body: []byte("x")}}))
		}, false, "upgrade_bad_archive"},
		{"a devvm that fails", func(k upgradetest.Key) upgradetest.Release {
			return upgradetest.NewRelease(k, "0.2.0", []byte("#!/bin/sh\nexit 1\n"))
		}, false, "upgrade_new_failed"},
		{"a devvm of another version", func(k upgradetest.Key) upgradetest.Release {
			return upgradetest.NewRelease(k, "0.2.0", upgradetest.Script("0.1.9"))
		}, false, "upgrade_new_failed"},
		{"a devvm that fails through the link", func(k upgradetest.Key) upgradetest.Release {
			return upgradetest.NewRelease(k, "0.2.0", []byte("#!/bin/sh\ncase $0 in */bin/devvm) exit 1;; esac\necho 0.2.0\n"))
		}, false, "upgrade_new_failed"},
		{"no release key", func(k upgradetest.Key) upgradetest.Release {
			return upgradetest.NewRelease(k, "0.2.0", nil)
		}, true, "upgrade_no_key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, "0.1.0")
			f.srv.Add("0.2.0", tc.release(f.key), true)
			up := f.upgrader()
			if tc.noKey {
				up.Source.Key = ""
			}
			if got := msg.IDOf(up.Upgrade(t.Context(), upgrade.Options{Force: true})); got != tc.want {
				t.Errorf("Upgrade = %s, want %s", got, tc.want)
			}
			f.unchanged()
			if out, err := runVersion(t, f.link()); err != nil || out != "0.1.0" {
				t.Errorf("the link runs %q, %v", out, err)
			}
		})
	}
}

// A switch that can't be made leaves the old devvm the one that runs, and
// no new version's folder.
func TestUpgradeKeepsOldOnFailedSwitch(t *testing.T) {
	f := newFixture(t, "0.1.0")
	f.release("0.2.0", true)
	bin := filepath.Dir(f.link())
	if err := os.Chmod(bin, 0o555); err != nil { //nolint:gosec // a test's own folder, made unwritable
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(bin, 0o755) }) //nolint:gosec // put back, for the temporary folder's removal
	if got := msg.IDOf(f.upgrader().Upgrade(t.Context(), upgrade.Options{Force: true})); got != "upgrade_failed" {
		t.Errorf("Upgrade = %s", got)
	}
	f.unchanged()
}

// A new devvm that fails through the link, whose link can't be put back,
// stays, with the link at it, and the failure says how to put it back.
func TestUpgradeRollbackFails(t *testing.T) {
	f := newFixture(t, "0.1.0")
	bin := filepath.Dir(f.link())
	t.Cleanup(func() { _ = os.Chmod(bin, 0o755) }) //nolint:gosec // put back, for the temporary folder's removal
	f.srv.Add("0.2.0", upgradetest.NewRelease(f.key, "0.2.0",
		[]byte("#!/bin/sh\ncase $0 in */bin/devvm) chmod 0555 "+bin+"; exit 1;; esac\necho 0.2.0\n")), true)
	err := f.upgrader().Upgrade(t.Context(), upgrade.Options{Force: true})
	if got := msg.NamesOf(err); got != "upgrade_rollback_failed link="+f.link()+" old="+f.exe("0.1.0") {
		t.Errorf("Upgrade = %s", got)
	}
	if got := f.linked(); got != "0.2.0" {
		t.Errorf("the link points at %s", got)
	}
	if _, err := os.Stat(f.exe("0.2.0")); err != nil {
		t.Error(err)
	}
}

// A folder of the new version from before, which a running VM may have
// used, is replaced.
func TestUpgradeReplacesFolder(t *testing.T) {
	f := newFixture(t, "0.1.0")
	f.release("0.2.0", true)
	writeExe(t, f.versionDir("0.2.0"), []byte("old"))
	if err := f.upgrader().Upgrade(t.Context(), upgrade.Options{Force: true}); err != nil {
		t.Fatal(err)
	}
	if out, err := runVersion(t, f.link()); err != nil || out != "0.2.0" {
		t.Errorf("the link runs %q, %v", out, err)
	}
	if got := f.lib(); !slices.Equal(got, []string{"0.1.0", "0.2.0"}) {
		t.Errorf("lib holds %q", got)
	}
}

// Homebrew's devvm is upgraded by brew upgrade devvm, asked first, and
// never to a version of --to's.
func TestUpgradeHomebrew(t *testing.T) {
	dir := t.TempDir()
	keg := filepath.Join(dir, "Cellar", "devvm", "0.1.0", "bin")
	writeExe(t, keg, upgradetest.Script("0.1.0"))
	calls := filepath.Join(dir, "brew-calls")
	brews := filepath.Join(dir, "brews")
	writeExe(t, brews, []byte("#!/bin/sh\necho \"$@\" >> "+calls+"\n[ ! -e "+calls+".fail ] || exit 1\n"))
	if err := os.Rename(filepath.Join(brews, "devvm"), filepath.Join(brews, "brew")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", brews+string(os.PathListSeparator)+os.Getenv("PATH"))

	f := newFixture(t, "0.1.0")
	f.release("0.2.0", true)
	up := f.upgrader()
	up.Install = upgrade.Detect(filepath.Join(keg, "devvm"))

	err := up.Upgrade(t.Context(), upgrade.Options{})
	if r, ok := errors.AsType[*msg.Refusal](err); !ok || r.Question.ID() != "upgrade_confirm_brew" {
		t.Errorf("without a terminal: %v", err)
	}
	if got := msg.IDOf(up.Upgrade(t.Context(), upgrade.Options{Version: "0.2.0", Force: true})); got != "upgrade_brew_version" {
		t.Errorf("--to: %s", got)
	}
	if _, err := os.Stat(calls); err == nil {
		t.Fatal("brew ran")
	}
	if err := up.Upgrade(t.Context(), upgrade.Options{Force: true}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(calls); string(b) != "upgrade devvm\n" { //nolint:gosec // the test's own file
		t.Errorf("brew ran %q", b)
	}
	if got := f.infos.IDs(); !slices.Equal(got, []string{"upgrade_brew_done"}) {
		t.Errorf("said %q", got)
	}
	if err := os.WriteFile(calls+".fail", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err = up.Upgrade(t.Context(), upgrade.Options{Force: true})
	if e, ok := errors.AsType[*backend.LoggedError](err); !ok || e.Msg.ID() != "upgrade_brew_failed" {
		t.Errorf("brew failing: %v", err)
	}
}

// Any other devvm is refused, saying how it was installed.
func TestUpgradeOther(t *testing.T) {
	f := newFixture(t, "0.1.0")
	f.release("0.2.0", true)
	copied := filepath.Join(f.prefix, "elsewhere")
	writeExe(t, copied, upgradetest.Script("0.1.0"))
	for _, tc := range []struct {
		current    string
		fromSource bool
		want       string
	}{
		{"(devel)", true, "upgrade_checkout_build version=(devel)"},
		{"0.0.0-20261105120000-0123456789ab+dirty", true, "upgrade_checkout_build version=0.0.0-20261105120000-0123456789ab+dirty"},
		{"0.1.0", true, "upgrade_go_install version=0.1.0"},
		{"0.1.0", false, "upgrade_other_install path=" + filepath.Join(copied, "devvm")},
	} {
		up := f.upgrader()
		up.Install = upgrade.Detect(filepath.Join(copied, "devvm"))
		up.Current, up.FromSource = tc.current, tc.fromSource
		if got := msg.NamesOf(up.Upgrade(t.Context(), upgrade.Options{Force: true})); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.current, got, tc.want)
		}
	}
	f.unchanged()
}

// Check says what's out without installing it: newer only when it is,
// and never for a version that isn't a release's.
func TestCheck(t *testing.T) {
	f := newFixture(t, "0.1.0")
	f.release("0.2.0", true)
	src := f.upgrader().Source
	for current, newer := range map[string]bool{"0.1.0": true, "0.2.0": false, "0.3.0": false, "(devel)": false, "0.2.0-rc.1": true} {
		av, err := src.Check(t.Context(), current)
		if err != nil || av.Latest != "0.2.0" || av.Newer != newer || av.Current != current {
			t.Errorf("Check(%s) = %+v, %v", current, av, err)
		}
	}
	f.unchanged()
}

// No newest release, a redirect elsewhere than a tag, and one away from
// HTTPS each fail as no newest release.
func TestLatestFails(t *testing.T) {
	plain := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(plain.Close)
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/elsewhere/releases/latest":
			http.Redirect(w, r, "/somewhere", http.StatusFound)
		case "/plain/releases/latest":
			http.Redirect(w, r, plain.URL+"/releases/tag/v1.0.0", http.StatusFound)
		case "/somewhere":
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(tls.Close)
	for _, repo := range []string{"/none", "/elsewhere", "/plain"} {
		src := upgrade.NewSource(tls.URL+repo, "")
		src.Client.Trust(rootsOf(tls))
		if _, err := src.Latest(t.Context()); msg.IDOf(err) != "upgrade_no_latest" {
			t.Errorf("%s: %v", repo, err)
		}
	}
}

// Release versions compare as numbers, a -suffix before the release.
func TestCompare(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"0.1.0", "0.1.0", 0},
		{"0.1.0", "0.2.0", -1},
		{"0.10.0", "0.9.0", 1},
		{"1.0.0", "0.99.99", 1},
		{"1.0.0-rc.1", "1.0.0", -1},
		{"1.0.0", "1.0.0-rc.1", 1},
		{"1.0.0-rc.1", "1.0.0-rc.2", -1},
		{"1.0.0-rc.9", "1.0.0-rc.10", -1},
		{"1.0.0-alpha.10", "1.0.0-alpha.2", 1},
		{"1.0.0-1", "1.0.0-alpha", -1},
		{"1.0.0-alpha", "1.0.0-alpha.1", -1},
		{"1.0.0-beta", "1.0.0-alpha", 1},
		{"0.2.0-beta.3", "0.2.0-rc.1", -1},
		{"0.2.0-alpha.1", "0.2.0-beta.1", -1},
		{"0.2.0-rc.2", "0.2.0-rc.1", 1},
		{"0.2.0-rc.1", "0.1.0", 1},
		{"0.2.0-rc.1", "0.2.1-alpha.1", -1},
		{"0.2.0-rc.1", "0.2.0-rc.1", 0},
		{"99999999999999999999.0.0", "18446744073709551615.0.0", 1},
		{"01.0.0", "1.0.0", 0},
	} {
		if got := upgrade.Compare(tc.a, tc.b); got != tc.want {
			t.Errorf("Compare(%s, %s) = %d", tc.a, tc.b, got)
		}
		if got := upgrade.Compare(tc.b, tc.a); got != -tc.want {
			t.Errorf("Compare(%s, %s) = %d", tc.b, tc.a, got)
		}
	}
	for v, valid := range map[string]bool{
		"0.1.0": true, "1.2.3-rc.1": true, "0.2.0-beta.2": true, "v0.1.0": false, "0.1": false, "0.1.0/../x": false,
		"": false, "(devel)": false, "0.1.0-rc/1": false,
	} {
		if upgrade.Valid(v) != valid {
			t.Errorf("Valid(%q) = %v", v, !valid)
		}
	}
	for v, pre := range map[string]bool{
		"0.2.0": false, "0.2.0-rc.1": true, "0.2.0-beta.3": true, "0.2.0-alpha.1": true, "": false, "(devel)": false,
		"0.0.0-20261106-abcdef": false,
	} {
		if upgrade.Prerelease(v) != pre {
			t.Errorf("Prerelease(%q) = %v", v, !pre)
		}
	}
}

// A prerelease is never the newest release: one at releases/latest is
// refused, so only --to installs one.
func TestLatestIsNoPrerelease(t *testing.T) {
	f := newFixture(t, "0.1.0")
	f.release("0.2.0-rc.1", true)
	if _, err := f.upgrader().Source.Latest(t.Context()); msg.IDOf(err) != "upgrade_no_latest" {
		t.Errorf("Latest = %v", err)
	}
	if err := f.upgrader().Upgrade(t.Context(), upgrade.Options{Force: true}); msg.IDOf(err) != "upgrade_no_latest" {
		t.Errorf("Upgrade = %v", err)
	}
	f.unchanged()
	if err := f.upgrader().Upgrade(t.Context(), upgrade.Options{Version: "0.2.0-rc.1", Force: true}); err != nil {
		t.Fatal(err)
	}
	if got := f.linked(); got != "0.2.0-rc.1" {
		t.Errorf("the link points at %s", got)
	}
}

// On a prerelease of a release that isn't out, the older newest release
// is said, not a failure, and nothing is installed; once the release is
// out, it's installed over the prerelease.
func TestUpgradeFromPrerelease(t *testing.T) {
	f := newFixture(t, "0.2.0-rc.1")
	f.release("0.1.0", true)
	if err := f.upgrader().Upgrade(t.Context(), upgrade.Options{Force: true}); err != nil {
		t.Fatal(err)
	}
	f.unchanged()
	if got := f.infos.Names(); !slices.Equal(got, []string{"upgrade_prerelease_ahead current=0.2.0-rc.1 latest=0.1.0"}) {
		t.Errorf("said %q", got)
	}
	f.release("0.2.0-rc.2", false)
	f.release("0.2.0", true)
	if err := f.upgrader().Upgrade(t.Context(), upgrade.Options{Force: true}); err != nil {
		t.Fatal(err)
	}
	if got := f.linked(); got != "0.2.0" {
		t.Errorf("the link points at %s", got)
	}
}

// runVersion runs path --version.
func runVersion(t *testing.T, path string) (string, error) {
	out, err := exec.CommandContext(t.Context(), path, "--version").Output()
	return strings.TrimSpace(string(out)), err
}

// rootsOf trusts srv's certificate alone.
func rootsOf(srv *httptest.Server) *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(srv.Certificate())
	return p
}
