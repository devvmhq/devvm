package tools

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/backend/backendtest"
	"github.com/devvmhq/devvm/internal/daemon"
	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/stubtest"
)

// Lima's targets: build.sh's own format. gnu's build in devvm's own
// base, baseKey; musl's in a --from base of muslDigest, muslKey.
var (
	gnuT  = Target{Libc: "gnu"}
	muslT = Target{Libc: "musl", Base: muslDigest}
)

const (
	baseKey    = "0123456789ab"
	muslKey    = "abcdef012345"
	muslDigest = "alpine:3@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

func repo(t *testing.T, parts ...string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join(append([]string{"..", ".."}, parts...)...))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func read(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // the test's own files
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func write(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func fixture(t *testing.T) []byte { return read(t, repo(t, "testdata", "mise.lock")) }

// parsed is a lock file's Lock.
func parsed(t *testing.T, data []byte) Lock {
	t.Helper()
	l, err := ParseLock(data)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// lockTools returns the tools a lock file has entries for, sorted.
func lockTools(t *testing.T, data []byte) []string {
	t.Helper()
	ts, _ := parsed(t, data).doc["tools"].(map[string]any)
	return slices.Sorted(maps.Keys(ts))
}

// textEntry returns a tool's entries in a lock file as written: from its
// [[tools.TOOL]] line to the next entry of another tool.
func textEntry(data []byte, tool string) []byte {
	var b bytes.Buffer
	in := false
	for _, line := range lines(data) {
		if strings.HasPrefix(line, "[[") {
			in = line == "[[tools."+tool+"]]"
		}
		if in {
			b.WriteString(line + "\n")
		}
	}
	return b.Bytes()
}

// entryOf is a tool's canonical entry in a lock file.
func entryOf(t *testing.T, data []byte, tool string) []byte {
	t.Helper()
	e, err := parsed(t, data).Entry(tool)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// env is a sandbox: devvm's folders, devvm's files with a copy of
// shared-tools/, the stub mise resolving the fixture lock, and what the
// tools printed.
type env struct {
	*Env
	Versions
	Share
	VMs
	root   string // devvm's files, Assets
	log    string // the stub's calls
	said   *[]string
	detach *[]string
}

// configFile is devvm's shared-tools/mise.toml.
func (e env) configFile() string { return filepath.Join(e.root, "shared-tools", "mise.toml") }

func newEnv(t *testing.T) env {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "calls")
	write(t, log, nil)
	t.Setenv("STUB_LOG", log)
	t.Setenv("MISE_STUB_LOCK", repo(t, "testdata", "mise.lock"))
	t.Setenv("MISE_STUB_FAIL", "")
	root := filepath.Join(dir, "root")
	for _, f := range []string{"prelude.sh", "shared-tools/mise.toml", "shared-tools/build.sh", "layers/base.sh"} {
		write(t, filepath.Join(root, f), read(t, repo(t, "assets", f)))
	}
	d := paths.Dirs{Data: filepath.Join(dir, "data"), Config: filepath.Join(dir, "config"), Lima: filepath.Join(dir, "lima")}
	var said, detached []string
	tl := New(d, os.DirFS(root), "0.1.0-test", &msg.Fake{
		OnInfo: func(m msg.M) { said = append(said, "==> "+m.Names()) },
		OnWarn: func(m msg.M) { said = append(said, "devvm: "+m.Names()) },
	}, platform.Host{})
	tl.Self = "/devvm"
	tl.Arch = "arm64"
	tl.Getenv = func(string) string { return "" }
	tl.Mise = stubtest.Command(t, "mise")
	tl.ShareNote = time.Millisecond
	tl.RetryDelay = time.Millisecond
	tl.FreeSpace = func(string) (uint64, error) { return 1 << 40, nil }
	tl.Detach = func(c daemon.Cmd) (int, error) {
		detached = append(detached, c.Path+" "+strings.Join(c.Args, " ")+" > "+c.Out.Name())
		return 1, nil
	}
	// The bases tool images build in, as their records have them.
	for key, rec := range map[string]state.Image{
		baseKey: {Name: image.Base, Digest: "https://example.com/ubuntu.img sha256:1111", Libc: "gnu"},
		muslKey: {Name: image.Base, From: "alpine:3", Digest: muslDigest, Libc: "musl"},
	} {
		if err := state.WriteImage(d, key, rec); err != nil {
			t.Fatal(err)
		}
	}
	return env{Env: tl, Versions: Versions{tl}, Share: Share{tl}, VMs: VMs{tl}, root: root, log: log, said: &said, detach: &detached}
}

// calls returns the stub's calls, and forgets them.
func (e env) calls(t *testing.T) string {
	t.Helper()
	c := string(read(t, e.log))
	write(t, e.log, nil)
	return c
}

// locked gives the env a lock already resolved today: the fixture's.
func (e env) locked(t *testing.T, lock []byte) {
	t.Helper()
	write(t, e.Dirs.ToolLock(), lock)
	write(t, e.Dirs.ToolLockConfig(), read(t, e.configFile()))
	write(t, e.Dirs.ToolLockChecked(), nil)
}

func TestMiseArch(t *testing.T) {
	for in, want := range map[string]string{"amd64": "x64", "arm64": "arm64"} {
		if got := MiseArch(in); got != want {
			t.Errorf("MiseArch(%s) = %s, want %s", in, got, want)
		}
	}
}

// The first use resolves the versions, for every VM platform, once.
func TestLockResolvesOnce(t *testing.T) {
	e := newEnv(t)
	lock, err := e.Lock(t.Context())
	if err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if !bytes.Equal(lock.Bytes(), fixture(t)) || !bytes.Equal(read(t, e.Dirs.ToolLock()), fixture(t)) {
		t.Error("the lock isn't what mise wrote")
	}
	if !bytes.Equal(read(t, e.Dirs.ToolLockConfig()), read(t, e.configFile())) {
		t.Error("the lock's mise.toml isn't shared-tools/mise.toml")
	}
	if _, err := os.Stat(e.Dirs.ToolLockChecked()); err != nil {
		t.Error("not marked checked")
	}
	if c := e.calls(t); !strings.Contains(c, "mise lock --platform linux-arm64,linux-arm64-musl\n") {
		t.Errorf("calls = %q", c)
	}
	if !slices.Contains(*e.said, "==> resolving_tools") {
		t.Errorf("said %q", *e.said)
	}
	*e.said = nil
	if _, err := e.Lock(t.Context()); err != nil {
		t.Fatal(err)
	}
	if c := e.calls(t); c != "" || len(*e.said) > 0 {
		t.Errorf("resolved again: %q, said %q", c, *e.said)
	}
}

// A changed shared-tools/mise.toml (a devvm upgrade) resolves again: every tool,
// or nothing.
func TestLockResolvesAgainWhenConfigChanges(t *testing.T) {
	e := newEnv(t)
	e.locked(t, fixture(t))
	config := read(t, e.configFile())
	write(t, e.configFile(), bytes.Replace(config, []byte("[tools]\n"), []byte("[tools]\nzig = \"latest\"\n"), 1))
	if _, err := e.Lock(t.Context()); msg.NamesOf(err) != "tools_need_resolve" {
		t.Errorf("Lock with a tool mise didn't lock = %v", msg.NamesOf(err))
	}
	if !bytes.Equal(read(t, e.Dirs.ToolLock()), fixture(t)) || !bytes.Equal(read(t, e.Dirs.ToolLockConfig()), config) {
		t.Error("the lock changed")
	}
	write(t, e.configFile(), append(config, "# a comment\n"...))
	if _, err := e.Lock(t.Context()); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if !bytes.Equal(read(t, e.Dirs.ToolLockConfig()), read(t, e.configFile())) || !strings.Contains(e.calls(t), "mise lock") {
		t.Error("not resolved again")
	}
}

// Offline with no lock yet, it says what's needed, with the details in
// the run log (tools.txtar shows them).
func TestLockOffline(t *testing.T) {
	e := newEnv(t)
	t.Setenv("MISE_STUB_FAIL", "1")
	_, err := e.Lock(t.Context())
	var logged *backend.LoggedError
	if !errors.As(err, &logged) || logged.Msg.ID() != "tools_need_resolve" {
		t.Errorf("Lock offline = %v", err)
	}
	if _, err := os.Stat(e.Dirs.ToolLock()); !errors.Is(err, os.ErrNotExist) {
		t.Error("a lock was written")
	}
}

// A lock that isn't TOML is resolved again by Lock, and named as corrupt
// by ReadLock, which never resolves.
func TestLockNotTOML(t *testing.T) {
	e := newEnv(t)
	e.locked(t, []byte("[[tools.node]\n"))
	_, err := e.ReadLock()
	if c, ok := errors.AsType[*state.CorruptError](err); !ok || c.Path != e.Dirs.ToolLock() || c.Line == 0 {
		t.Errorf("ReadLock = %v", err)
	}
	lock, err := e.Lock(t.Context())
	if err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if !bytes.Equal(lock.Bytes(), fixture(t)) || !strings.Contains(e.calls(t), "mise lock") {
		t.Error("not resolved again")
	}
	if l, err := e.ReadLock(); err != nil || l.None() || l.Version("node") == "" {
		t.Errorf("ReadLock = %v", err)
	}
	write(t, e.Dirs.ToolLock(), nil)
	if l, err := e.ReadLock(); err != nil || l.None() || l.Has("node") {
		t.Errorf("an empty lock: %v", err)
	}
	if err := os.Remove(e.Dirs.ToolLock()); err != nil {
		t.Fatal(err)
	}
	if l, err := e.ReadLock(); err != nil || !l.None() {
		t.Errorf("no lock: %v", err)
	}
	if err := os.Mkdir(e.Dirs.ToolLock(), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ReadLock(); err == nil {
		t.Error("a folder for a lock read")
	}
}

// A lock mise wrote that isn't TOML fails the refresh and leaves the lock
// as it was; an older versions' lock that isn't TOML is no step back.
func TestResolveNotTOML(t *testing.T) {
	e := newEnv(t)
	e.locked(t, fixture(t))
	e.stubLocks(t, []byte("[[tools.node]\n"), nil, "")
	if err := e.Refresh(t.Context()); msg.NamesOf(err) != "tools_resolve_failed" {
		t.Errorf("Refresh = %v", msg.NamesOf(err))
	}
	if !bytes.Equal(read(t, e.Dirs.ToolLock()), fixture(t)) {
		t.Error("the lock changed")
	}
	newer := newerLock(t)
	e.stubLocks(t, newer, []byte("not = [toml\n"), "24.20.0")
	if err := e.Refresh(t.Context()); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if !slices.Contains(*e.said, "devvm: tool_no_download tool=node version=24.21.0.1") {
		t.Errorf("said %q", *e.said)
	}
}

// An entry JSON can't hold (a NaN) has no key.
func TestEntryNotJSON(t *testing.T) {
	e := newEnv(t)
	lock := parsed(t, []byte("[[tools.node]]\nversion = \"1.0.0\"\nx = nan\n"))
	if _, err := e.File(lock, "node", gnuT); err == nil {
		t.Error("a NaN made a key")
	}
	if b, err := (Lock{}).Entry("node"); b != nil || err != nil {
		t.Errorf("no lock's entry = %s, %v", b, err)
	}
}

// Only what the lock says counts for a tool's key, not how mise wrote it:
// its order, spacing, quoting and comments; a changed checksum does.
func TestEntryCanonical(t *testing.T) {
	e := newEnv(t)
	f := fixture(t)
	node := textEntry(f, "node")
	reformatted := regexp.MustCompile(`(?m)^(\w+) = "(.*)"$`).ReplaceAll(node, []byte("$1   =   '$2' # a comment"))
	lines := strings.Split(strings.TrimSuffix(string(reformatted), "\n"), "\n")
	// The entry's own fields after its header, in another order.
	lines[1], lines[2] = lines[2], lines[1]
	moved := bytes.Replace(f, node, []byte(strings.Join(lines, "\n")+"\n"), 1)
	if bytes.Equal(moved, f) {
		t.Fatal("the fixture didn't change")
	}
	want, err := e.File(parsed(t, f), "node", gnuT)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := e.File(parsed(t, moved), "node", gnuT); err != nil || got != want {
		t.Errorf("reformatted: %s, %v; want %s", got, err, want)
	}
	sum := regexp.MustCompile(`checksum = "sha256:[0-9a-f]`).Find(node)
	changed := bytes.Replace(f, node, bytes.Replace(node, sum, append(sum[:len(sum)-1:len(sum)-1], 'x'), 1), 1)
	if got, err := e.File(parsed(t, changed), "node", gnuT); err != nil || got == want {
		t.Errorf("a changed checksum: %s, %v", got, err)
	}
}

// mise reads no config above its folder: MISE_CEILING_PATHS stops it,
// with the physical path, and its state stays in the folder.
func TestMiseCmdCeiling(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "shared", "t"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "shared"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	cmd, err := (Versions{&Env{}}).miseCmd(t.Context(), "/bin/sh", filepath.Join(dir, "link", "t"), "-c",
		`echo "ceiling=$MISE_CEILING_PATHS trusted=$MISE_TRUSTED_CONFIG_PATHS data=$MISE_DATA_DIR pwd=$(pwd -P)"`)
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	p, err := filepath.EvalSymlinks(filepath.Join(dir, "shared"))
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("ceiling=%[1]s trusted=%[1]s/t data=%[1]s/t/data pwd=%[1]s/t\n", p); string(out) != want {
		t.Errorf("got %q, want %q", out, want)
	}
}

// The daily check: a lock not checked in the last day; never with
// DEVVM_NO_TOOL_CHECK, or without a lock (the first use resolves it).
func TestCheckDue(t *testing.T) {
	e := newEnv(t)
	if e.CheckDue() {
		t.Error("due without a lock")
	}
	e.locked(t, fixture(t))
	if e.CheckDue() {
		t.Error("due when checked now")
	}
	old := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(e.Dirs.ToolLockChecked(), old, old); err != nil {
		t.Fatal(err)
	}
	if !e.CheckDue() {
		t.Error("not due a day later")
	}
	getenv := e.Getenv
	e.Getenv = func(k string) string {
		if k == "DEVVM_NO_TOOL_CHECK" {
			return "1"
		}
		return getenv(k)
	}
	if e.CheckDue() {
		t.Error("due with DEVVM_NO_TOOL_CHECK")
	}
	e.Getenv = getenv
	if err := os.Remove(e.Dirs.ToolLockChecked()); err != nil {
		t.Fatal(err)
	}
	if !e.CheckDue() {
		t.Error("not due when never checked")
	}
}

// At most once a day, in the background: devvm _refresh-tools, its output
// in the logs folder.
func TestRefreshDaily(t *testing.T) {
	e := newEnv(t)
	e.locked(t, fixture(t))
	if err := e.RefreshDaily(); err != nil || len(*e.detach) > 0 {
		t.Fatalf("checked today, yet %v %q", err, *e.detach)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(e.Dirs.ToolLockChecked(), old, old); err != nil {
		t.Fatal(err)
	}
	if err := e.RefreshDaily(); err != nil {
		t.Fatal(err)
	}
	want := []string{"/devvm _refresh-tools > " + filepath.Join(e.Dirs.Logs(), "tools-check.log")}
	if !slices.Equal(*e.detach, want) {
		t.Errorf("started %q, want %q", *e.detach, want)
	}
	if err := e.RefreshDaily(); err != nil || len(*e.detach) != 1 {
		t.Errorf("started again: %v %q", err, *e.detach)
	}
	if fi, err := os.Stat(e.Dirs.Logs()); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("logs folder: %v %v", fi, err)
	}
}

// newerLock is the fixture with every tool newer, and node built from
// source on musl (its musl builds not out yet, as for days after a node
// release).
func newerLock(t *testing.T) []byte {
	t.Helper()
	out := regexp.MustCompile(`(?m)^version = "(.*)"$`).ReplaceAll(fixture(t), []byte(`version = "$1.1"`))
	return regexp.MustCompile(`(?m)^(\[tools\.node\."platforms\.linux-.*-musl"\])$`).ReplaceAll(out, []byte("$1\ninstall = \"source\""))
}

// olderLock is what mise locks for node's older versions: 24.20.1 also
// built from source on musl, 24.20.0 with every download, pinned.
func olderLock(t *testing.T) []byte {
	t.Helper()
	ver := regexp.MustCompile(`(?m)^version = .*$`)
	spec := regexp.MustCompile(`(?m)^specifiers = .*$`)
	a := ver.ReplaceAll(textEntry(newerLock(t), "node"), []byte(`version = "24.20.1"`))
	b := ver.ReplaceAll(textEntry(fixture(t), "node"), []byte(`version = "24.20.0"`))
	return spec.ReplaceAll(append(a, b...), []byte(`specifiers = ["pinned"]`))
}

func (e env) stubLocks(t *testing.T, newer, older []byte, versions string) {
	t.Helper()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "newer.lock"), newer)
	write(t, filepath.Join(dir, "older.lock"), older)
	t.Setenv("MISE_STUB_LOCK", filepath.Join(dir, "newer.lock"))
	t.Setenv("MISE_STUB_OLDER_LOCK", filepath.Join(dir, "older.lock"))
	t.Setenv("MISE_STUB_VERSIONS", versions)
}

// A tool whose newest version has no download for some platform steps back
// to the newest older one that has; the others move on.
func TestRefreshStepsBack(t *testing.T) {
	e := newEnv(t)
	e.locked(t, fixture(t))
	newer := newerLock(t)
	e.stubLocks(t, newer, olderLock(t), "24.19.0 24.20.0 24.20.1 24.21.0.1 24.22.0")
	config := filepath.Join(t.TempDir(), "older.toml")
	t.Setenv("MISE_STUB_OLDER_CONFIG", config)
	if err := e.Refresh(t.Context()); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	// The older versions come from the backend the newest came from, which
	// mise's registry might not give the tool's name.
	if got, want := string(read(t, config)), "[tool_alias]\nnode = \"core:node\"\n\n[tools]\nnode = [\"24.20.1\",\"24.20.0\",\"24.19.0\"]\n"; got != want {
		t.Errorf("the step back's mise.toml = %q, want %q", got, want)
	}
	if !slices.Contains(*e.said, "devvm: tool_stepped_back older=24.20.0 tool=node version=24.21.0.1") {
		t.Errorf("said %q", *e.said)
	}
	written := read(t, e.Dirs.ToolLock())
	lock := parsed(t, written)
	if v := lock.Version("node"); v != "24.20.0" {
		t.Errorf("node = %s", v)
	}
	if s, _ := lock.first("node")["specifiers"].([]any); !slices.Equal(s, []any{"24"}) {
		t.Errorf("node's specifiers aren't mise.toml's anymore: %q", s)
	}
	if !lock.HasDownload("node", e.platforms()) {
		t.Error("node has no download for every platform")
	}
	if !bytes.Equal(entryOf(t, written, "bun"), entryOf(t, newer, "bun")) {
		t.Error("bun didn't move on")
	}
	if !slices.Equal(lockTools(t, written), lockTools(t, newer)) {
		t.Errorf("tools = %q", lockTools(t, written))
	}
	if v := lock.doc["lockfile_version"]; v != parsed(t, newer).doc["lockfile_version"] {
		t.Errorf("lockfile_version = %v", v)
	}
	if c := e.calls(t); !strings.Contains(c, "mise ls-remote core:node 24\n") {
		t.Errorf("not from the backend: %q", c)
	}
}

// With no older version that has every download, the newest stays, and
// says so.
func TestRefreshStepsBackToNothing(t *testing.T) {
	e := newEnv(t)
	e.locked(t, fixture(t))
	newer := newerLock(t)
	e.stubLocks(t, newer, newer, "")
	if err := e.Refresh(t.Context()); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if !slices.Contains(*e.said, "devvm: tool_no_download tool=node version=24.21.0.1") {
		t.Errorf("said %q", *e.said)
	}
	if !bytes.Equal(read(t, e.Dirs.ToolLock()), newer) {
		t.Error("the lock isn't the newest")
	}
	// Older versions mise locked none of.
	e.stubLocks(t, newer, newer, "24.20.0")
	*e.said = nil
	if err := e.Refresh(t.Context()); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if !slices.Contains(*e.said, "devvm: tool_no_download tool=node version=24.21.0.1") || !bytes.Equal(read(t, e.Dirs.ToolLock()), newer) {
		t.Errorf("said %q", *e.said)
	}
}

// Newer versions replace the lock; a failure leaves it.
func TestRefresh(t *testing.T) {
	e := newEnv(t)
	e.locked(t, fixture(t))
	newer := regexp.MustCompile(`(?m)^version = "(.*)"$`).ReplaceAll(fixture(t), []byte(`version = "$1.1"`))
	e.stubLocks(t, newer, newer, "")
	t.Setenv("MISE_STUB_FAIL", "1")
	if err := e.Refresh(t.Context()); err == nil {
		t.Fatal("Refresh offline succeeded")
	}
	if !bytes.Equal(read(t, e.Dirs.ToolLock()), fixture(t)) {
		t.Error("a failure changed the lock")
	}
	if _, err := os.Stat(e.Dirs.ToolLock() + ".next"); !errors.Is(err, os.ErrNotExist) {
		t.Error("a failure left the next lock")
	}
	t.Setenv("MISE_STUB_FAIL", "")
	if err := e.Refresh(t.Context()); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if !bytes.Equal(read(t, e.Dirs.ToolLock()), newer) {
		t.Error("the lock isn't the newer one")
	}
	if held, _ := platform.LockHeld(e.Dirs.ShareLock()); held {
		t.Error("the share's lock is still held")
	}
	// Check says a failure, and carries on.
	t.Setenv("MISE_STUB_FAIL", "1")
	*e.said = nil
	e.Check(t.Context())
	if want := []string{"==> checking_tools", "devvm: tools_check_failed"}; !slices.Equal(*e.said, want) {
		t.Errorf("Check offline said %q, want %q", *e.said, want)
	}
}

// A download with its checksum for every platform, not a build from
// source; rust needs none.
func TestHasDownload(t *testing.T) {
	e := newEnv(t)
	lock := parsed(t, fixture(t))
	if !lock.HasDownload("node", e.platforms()) || !lock.HasDownload("rust", e.platforms()) {
		t.Error("the fixture's node or rust has no download")
	}
	if lock.HasDownload("zig", e.platforms()) {
		t.Error("a tool the lock hasn't has a download")
	}
	newer := parsed(t, newerLock(t))
	if newer.HasDownload("node", e.platforms()) {
		t.Error("a source build on musl counts")
	}
	if !newer.HasDownload("node", []string{"linux-arm64"}) {
		t.Error("glibc's download doesn't count")
	}
	// opencode without its platforms' tables.
	var bare bytes.Buffer
	skip := false
	for _, line := range lines(fixture(t)) {
		if strings.HasPrefix(line, "[") {
			skip = strings.HasPrefix(line, "[tools.opencode.")
		}
		if !skip {
			bare.WriteString(line + "\n")
		}
	}
	if parsed(t, bare.Bytes()).HasDownload("opencode", e.platforms()) {
		t.Error("a tag with no release (no platforms) counts")
	}
	// A URL without its checksum, which only TLS would check.
	var unsummed bytes.Buffer
	for _, line := range lines(fixture(t)) {
		if !strings.HasPrefix(line, "checksum = ") {
			unsummed.WriteString(line + "\n")
		}
	}
	if parsed(t, unsummed.Bytes()).HasDownload("node", e.platforms()) {
		t.Error("a download without its checksum counts")
	}
	// Only the VM's own platform counts.
	if e.noDownload(newer, "node", "gnu") || !e.noDownload(newer, "node", "musl") || e.noDownload(newer, "bun", "musl") {
		t.Error("noDownload looks at other platforms")
	}
}

func TestVersionEntry(t *testing.T) {
	lock := parsed(t, olderLock(t))
	if e := lock.versionEntry("node", "24.20.0"); str(e, "version") != "24.20.0" || !hasDownload(e, []string{"linux-arm64", "linux-arm64-musl"}) {
		t.Errorf("entry = %v", e)
	}
	if e := lock.versionEntry("node", "24.20.1"); str(e, "version") != "24.20.1" || hasDownload(e, []string{"linux-arm64-musl"}) {
		t.Errorf("entry = %v", e)
	}
	if e := lock.versionEntry("node", "1.0.0"); e != nil {
		t.Errorf("entry for a version it hasn't = %v", e)
	}
}

// A step back's lock is written again as TOML: what it says is kept, the
// tool's entry replaced, and mise reads it as the same lock.
func TestWithEntry(t *testing.T) {
	f := fixture(t)
	lock := parsed(t, f)
	one := maps.Clone(parsed(t, olderLock(t)).versionEntry("node", "24.20.0"))
	got, err := lock.withEntry("node", one)
	if err != nil {
		t.Fatal(err)
	}
	again := parsed(t, got.Bytes())
	if again.Version("node") != "24.20.0" || len(again.entries("node")) != 1 {
		t.Errorf("node = %s, %d entries", again.Version("node"), len(again.entries("node")))
	}
	for _, tool := range lockTools(t, f) {
		if tool != "node" && !bytes.Equal(entryOf(t, got.Bytes(), tool), entryOf(t, f, tool)) {
			t.Errorf("%s changed:\n%s", tool, got.Bytes())
		}
	}
	if lock.Version("node") == "24.20.0" {
		t.Error("the lock it came from changed")
	}
	if _, err := (Lock{}).withEntry("node", one); err != nil {
		t.Errorf("onto no lock: %v", err)
	}
	if _, err := lock.withEntry("node", lockEntry{"version": make(chan int)}); err == nil {
		t.Error("a value TOML can't hold was written")
	}
}

// Tool files: the lock's version, the groups' tools, and a locked version
// that isn't shaped like one refused; musl bases get their own images.
func TestFiles(t *testing.T) {
	e := newEnv(t)
	e.locked(t, fixture(t))
	files, err := e.Files(t.Context(), []string{"node", "claude"}, gnuT)
	if err != nil {
		t.Fatal(err)
	}
	var tools []string
	for _, f := range files {
		tool, v, ok := image.ParseToolFile(f)
		if !ok || v != parsed(t, fixture(t)).Version(tool) {
			t.Errorf("%s isn't at the lock's version", f)
		}
		tools = append(tools, tool)
	}
	if !slices.Equal(tools, []string{"node", "pnpm", "bun", "claude"}) {
		t.Errorf("tools = %q", tools)
	}
	musl, err := e.Files(t.Context(), []string{"node"}, muslT)
	if err != nil || musl[0] == files[0] {
		t.Errorf("musl's node = %q, %v", musl, err)
	}
	bad := regexp.MustCompile(`(?m)(\[\[tools\.go\]\]\n)version = ".*"`).ReplaceAll(fixture(t), []byte(`${1}version = "1$$(id)"`))
	e.locked(t, bad)
	if _, err := e.Files(t.Context(), []string{"go"}, gnuT); msg.NamesOf(err) != `tool_bad_version tool=go version="1$(id)"` {
		t.Errorf("a bad version: %v", msg.NamesOf(err))
	}
	if _, err := e.File(parsed(t, fixture(t)), "zig", gnuT); msg.NamesOf(err) != "tool_not_locked lock="+e.Dirs.ToolLock()+" tool=zig" {
		t.Errorf("a tool the lock hasn't: %v", msg.NamesOf(err))
	}
}

func fake() *backendtest.Fake {
	caps := backend.Caps{}
	for _, f := range backend.Features {
		caps[f] = backend.Native
	}
	return backendtest.New("fake", caps)
}

func (e env) files(t *testing.T, tools ...string) []string {
	t.Helper()
	var out []string
	for _, tool := range tools {
		f, err := e.File(parsed(t, read(t, e.Dirs.ToolLock())), tool, gnuT)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, f)
	}
	return out
}

func (e env) checkHashes(t *testing.T, files []string) {
	t.Helper()
	for _, f := range files {
		p := filepath.Join(e.Dirs.Tools(), f)
		sum, err := image.SHA256File(p)
		if err != nil {
			t.Fatal(err)
		}
		fi, err := os.Lstat(e.Dirs.ToolRecord(f))
		if err != nil || !fi.Mode().IsRegular() {
			t.Errorf("%s has no record: %v", f, err)
			continue
		}
		rec, err := state.ReadToolImage(e.Dirs, f)
		if err != nil || rec.SHA256 != sum || rec.Libc != gnuT.Libc || rec.Format != gnuT.Format || rec.Backend != "fake" ||
			rec.MadeBy != "0.1.0-test" || time.Since(rec.Built) > time.Minute {
			t.Errorf("%s's record: %+v, %v", f, rec, err)
		}
	}
}

// recorded gives a tool image in the share a record, built at built, for
// gnu on the fake backend.
func (e env) recorded(t *testing.T, file string, built time.Time) {
	t.Helper()
	if err := state.WriteToolImage(e.Dirs, file, state.ToolImage{SHA256: "x", Libc: "gnu", Backend: "fake", Built: built}); err != nil {
		t.Fatal(err)
	}
}

func (e env) noLeftovers(t *testing.T) {
	t.Helper()
	if left, _ := filepath.Glob(filepath.Join(e.Dirs.Data, "tools-build.*")); len(left) > 0 {
		t.Errorf("left %q", left)
	}
	if held, _ := platform.LockHeld(e.Dirs.ShareLock()); held {
		t.Error("the share's lock is still held")
	}
}

// The missing tool images build each in a throwaway VM, with a folder of
// its own (never the share); each moves into the share with its hash.
func TestBuild(t *testing.T) {
	e := newEnv(t)
	e.locked(t, fixture(t))
	f := fake()
	all := e.files(t, "node", "pnpm", "bun", "claude")
	write(t, filepath.Join(e.Dirs.Tools(), all[1]), []byte("built"))
	e.recorded(t, all[1], time.Now())
	if err := e.Build(t.Context(), backend.Review{}, f, baseKey, gnuT, all); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if ms := f.Methods(); !slices.Equal(ms, []string{"BuildTools", "BuildTools", "BuildTools"}) {
		t.Fatalf("calls = %q", ms)
	}
	lock := parsed(t, fixture(t))
	// pnpm is built: node, bun and claude are the three calls.
	for c, i := range []int{0, 2, 3} {
		tool, _, _ := image.ParseToolFile(all[i])
		spec, _ := f.Calls()[c].Args[0].(backend.ToolsSpec)
		if spec.Base != baseKey || spec.OutAt != ShareAt || spec.Net != backend.NetInternet || !slices.Equal(spec.Args, []string{tool, lock.Version(tool), all[i]}) ||
			spec.Name != Label(all[i:i+1]) || spec.Script.Name != "build.sh" ||
			!bytes.Equal(spec.Script.Data, slices.Concat(read(t, filepath.Join(e.root, "prelude.sh")), read(t, filepath.Join(e.root, "shared-tools", "build.sh")))) {
			t.Errorf("%s's spec = %+v", tool, spec)
		}
		if filepath.Dir(spec.Out) != e.Dirs.Data || !strings.HasPrefix(filepath.Base(spec.Out), "tools-build.") {
			t.Errorf("%s's build's folder is %s", tool, spec.Out)
		}
	}
	want := "==> building_tools tools=" + Label([]string{all[0], all[2], all[3]})
	if !slices.Contains(*e.said, want) {
		t.Errorf("said %q, want %q", *e.said, want)
	}
	e.checkHashes(t, []string{all[0], all[2], all[3]})
	e.noLeftovers(t)
	// Nothing to do when the share has them all.
	if err := e.Build(t.Context(), backend.Review{}, f, baseKey, gnuT, all); err != nil || len(f.Calls()) != 3 {
		t.Errorf("built again: %v %q", err, f.Methods())
	}
}

// The build sees the lock and mise.toml it was given, in its folder.
func TestBuildFolder(t *testing.T) {
	e := newEnv(t)
	e.locked(t, fixture(t))
	f := fake()
	f.ToolsBuild = func(spec backend.ToolsSpec) error {
		if !bytes.Equal(read(t, filepath.Join(spec.Out, ".build", "mise.lock")), fixture(t)) ||
			!bytes.Equal(read(t, filepath.Join(spec.Out, ".build", "mise.toml")), read(t, e.configFile())) {
			t.Error("the build's .build folder isn't the lock and mise.toml")
		}
		if held, err := platform.LockHeld(e.Dirs.ShareLock()); !held || err != nil {
			t.Errorf("the share isn't locked during the build: %v", err)
		}
		return nil
	}
	if err := e.Build(t.Context(), backend.Review{}, f, baseKey, gnuT, e.files(t, "uv")); msg.NamesOf(err) != "tool_bad_output file="+e.files(t, "uv")[0] {
		t.Errorf("Build with no image written = %v", msg.NamesOf(err))
	}
	e.noLeftovers(t)
}

// Each tool builds in a VM and folder of its own, so one tool's installer
// can't write into another tool's image: here node's build writes every
// tool's file in its folder, before and after the others' builds would,
// and only its own leaves it.
func TestBuildOneVMEach(t *testing.T) {
	e := newEnv(t)
	e.locked(t, fixture(t))
	f := fake()
	files := e.files(t, "node", "go", "bun")
	outs := map[string]bool{}
	f.ToolsBuild = func(spec backend.ToolsSpec) error {
		outs[spec.Out] = true
		if len(spec.Args) != 3 {
			t.Errorf("a build of %q", spec.Args)
		}
		if spec.Args[0] == "node" {
			for _, other := range files {
				write(t, filepath.Join(spec.Out, other), []byte("node's"))
				write(t, filepath.Join(spec.Out, other+".glibc"), nil)
			}
			return nil
		}
		write(t, filepath.Join(spec.Out, spec.Args[2]), []byte(spec.Args[0]+"'s"))
		write(t, filepath.Join(spec.Out, spec.Args[2]+".glibc"), nil)
		return nil
	}
	if err := e.Build(t.Context(), backend.Review{}, f, baseKey, gnuT, files); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if len(f.Calls()) != 3 || len(outs) != 3 {
		t.Errorf("%d builds in %d folders", len(f.Calls()), len(outs))
	}
	for _, file := range files {
		tool, _, _ := image.ParseToolFile(file)
		if got := string(read(t, e.Dirs.ToolImage(file))); got != tool+"'s" {
			t.Errorf("%s holds %q", file, got)
		}
	}
	e.checkHashes(t, files)
	e.noLeftovers(t)
}

// A build that leaves a symlink instead of an image is refused; one that
// plants FILE.json as a symlink can't make devvm write through it.
func TestBuildHostileOutput(t *testing.T) {
	e := newEnv(t)
	e.locked(t, fixture(t))
	victim := filepath.Join(t.TempDir(), "victim")
	write(t, victim, []byte("victim\n"))
	f := fake()
	f.ToolsBuild = func(spec backend.ToolsSpec) error { return os.Symlink(victim, filepath.Join(spec.Out, spec.Args[2])) }
	node := e.files(t, "node")
	if err := e.Build(t.Context(), backend.Review{}, f, baseKey, gnuT, node); msg.NamesOf(err) != "tool_bad_output file="+node[0] {
		t.Errorf("Build = %v", msg.NamesOf(err))
	}
	if _, err := os.Lstat(filepath.Join(e.Dirs.Tools(), node[0])); !errors.Is(err, os.ErrNotExist) {
		t.Error("the symlink reached the share")
	}
	e.noLeftovers(t)

	f.ToolsBuild = func(spec backend.ToolsSpec) error {
		file := spec.Args[2]
		write(t, filepath.Join(spec.Out, file), []byte("image"))
		write(t, filepath.Join(spec.Out, file+".glibc"), nil)
		// The share is the VM's to write in this scenario, through
		// whatever it planted.
		return os.Symlink(victim, e.Dirs.ToolRecord(file))
	}
	if err := e.Build(t.Context(), backend.Review{}, f, baseKey, gnuT, node); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if string(read(t, victim)) != "victim\n" {
		t.Error("devvm wrote through the planted link")
	}
	e.checkHashes(t, node)
}

// Tool images build only in the base they're for: gnu ones, which any VM
// may mount, in devvm's own Ubuntu base, never a --from image's, whose
// code ran as root in its build; musl ones in a --from base with their
// digest. So a hostile --from base can't build an image another VM
// mounts: its musl images have names no VM on another base looks for.
func TestBuildOnlyInItsBase(t *testing.T) {
	e := newEnv(t)
	e.locked(t, fixture(t))
	const hostile, other = "badbadbad000", "0123456789cd"
	for key, rec := range map[string]state.Image{
		hostile:        {Name: image.Base, From: "evil/debian:12", Digest: "evil/debian@sha256:" + strings.Repeat("e", 64), Libc: "gnu", Glibc: "2.36"},
		other:          {Name: image.Base, From: "evil/alpine:3", Digest: "evil/alpine@sha256:" + strings.Repeat("e", 64), Libc: "musl"},
		"0123456789ef": {Name: "foo", Chain: "base=" + baseKey + " foo=0123456789ef", Libc: "gnu"},
	} {
		if err := state.WriteImage(e.Dirs, key, rec); err != nil {
			t.Fatal(err)
		}
	}
	lock := parsed(t, fixture(t))
	gnu := e.files(t, "node")
	musl, err := e.File(lock, "node", muslT)
	if err != nil {
		t.Fatal(err)
	}
	f := fake()
	for _, tc := range []struct {
		name  string
		base  string
		tg    Target
		files []string
	}{
		{"gnu in a --from gnu base", hostile, gnuT, gnu},
		{"gnu in a musl base", muslKey, gnuT, gnu},
		{"gnu in a layer on base", "0123456789ef", gnuT, gnu},
		{"gnu in an image without a record", "fedcba987654", gnuT, gnu},
		{"musl in Ubuntu's base", baseKey, muslT, []string{musl}},
		{"musl in a base of another image", other, muslT, []string{musl}},
	} {
		if err := e.Build(t.Context(), backend.Review{}, f, tc.base, tc.tg, tc.files); msg.IDOf(err) != "tools_wrong_base" {
			t.Errorf("%s: Build = %v", tc.name, msg.NamesOf(err))
		}
	}
	if len(f.Calls()) > 0 {
		t.Errorf("calls = %q", f.Methods())
	}
	if e.Built(gnu[0]) || e.Built(musl) {
		t.Error("an image reached the share")
	}
	e.noLeftovers(t)
	// What the hostile musl base builds is keyed by its own digest.
	theirs, err := e.File(lock, "node", Target{Libc: "musl", Base: "evil/alpine@sha256:" + strings.Repeat("e", 64)})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Build(t.Context(), backend.Review{}, f, other, Target{Libc: "musl", Base: "evil/alpine@sha256:" + strings.Repeat("e", 64)}, []string{theirs}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if theirs == musl || theirs == gnu[0] || !e.Built(theirs) || e.Built(musl) || e.Built(gnu[0]) {
		t.Errorf("the hostile base's %s is another VM's: %s, %s", theirs, musl, gnu[0])
	}
}

// A tool the lock would build from source for the VM's platform says so,
// and builds nothing; so does a lock that moved on while devvm waited.
func TestBuildRefuses(t *testing.T) {
	e := newEnv(t)
	lock := regexp.MustCompile(`(?m)^(\[tools\.node\."platforms\.linux-.*-musl"\])$`).ReplaceAll(fixture(t), []byte("$1\ninstall = \"source\""))
	e.locked(t, lock)
	f := fake()
	musl, err := e.File(parsed(t, lock), "node", muslT)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Build(t.Context(), backend.Review{}, f, muslKey, muslT, []string{musl}); msg.NamesOf(err) != "tool_unbuilt tool=node version="+parsed(t, lock).Version("node") {
		t.Errorf("Build = %v", msg.NamesOf(err))
	}
	if err := e.Build(t.Context(), backend.Review{}, f, baseKey, gnuT, []string{"node-1.0.0-0123456789ab.erofs"}); msg.NamesOf(err) != "tools_changed" {
		t.Errorf("Build of a version the lock moved on from = %v", msg.NamesOf(err))
	}
	if len(f.Calls()) > 0 {
		t.Errorf("calls = %q", f.Methods())
	}
	e.noLeftovers(t)
}

// Below the free space a tools build wants, on the backend's disks (its
// floors for one tool: 3 GiB and 1 more) or in devvm's data folder, where
// the images come out (backend.ToolImages: 1 GiB for one), nothing is
// built. A backend whose disks aren't the host's has only the data
// folder checked.
func TestBuildRefusesLowSpace(t *testing.T) {
	e := newEnv(t)
	e.locked(t, fixture(t))
	all := e.files(t, "node")
	for _, tc := range []struct {
		name   string
		stored bool
		low    string
		free   uint64
		want   string // the message's names, "" for none
	}{
		{"the backend's disk", true, "/lima", 4<<30 - 1, "disk_low dir=/lima free=3.9 name=" + Label(all) + " need=4"},
		{"the data folder", true, e.Dirs.Data, 1<<30 - 1, "disk_low dir=" + e.Dirs.Data + " free=0.9 name=" + Label(all) + " need=1"},
		{"not the host's disks", false, e.Dirs.Data, 1<<30 - 1, "disk_low dir=" + e.Dirs.Data + " free=0.9 name=" + Label(all) + " need=1"},
	} {
		f := fake()
		var b backend.Backend = f
		if tc.stored {
			b = backendtest.Stored{Fake: f, Dirs: []string{"/lima"}}
		}
		e.FreeSpace = func(dir string) (uint64, error) {
			if dir == tc.low {
				return tc.free, nil
			}
			return 100 << 30, nil
		}
		err := e.Build(t.Context(), backend.Review{}, b, baseKey, gnuT, all)
		if got := msg.NamesOf(err); got != tc.want {
			t.Errorf("%s low: Build = %v, want %s", tc.name, got, tc.want)
		}
		if len(f.Calls()) > 0 {
			t.Errorf("%s low: calls = %q", tc.name, f.Methods())
		}
		e.noLeftovers(t)
	}
}

// Another devvm holding the share is waited for, saying so, until it lets
// go or the run ends; the lock is out of the share VMs mount, and
// ShareBusy says whether another devvm holds it.
func TestLockShare(t *testing.T) {
	e := newEnv(t)
	if err := os.MkdirAll(e.Dirs.Data, 0o700); err != nil {
		t.Fatal(err)
	}
	if e.ShareBusy() {
		t.Error("ShareBusy before anything holds it")
	}
	unlock, err := e.LockShare(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(e.Dirs.ShareLock()) != e.Dirs.Data {
		t.Errorf("lock %s", e.Dirs.ShareLock())
	}
	if !e.ShareBusy() {
		t.Error("ShareBusy while held = false")
	}
	soon, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if _, err := e.LockShare(soon); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("LockShare while held = %v", msg.NamesOf(err))
	}
	if !slices.Contains(*e.said, "==> share_waiting dir="+e.Dirs.Tools()) {
		t.Errorf("said %q", *e.said)
	}
	got := make(chan error, 1)
	go func() {
		u, err := e.LockShare(t.Context())
		if err == nil {
			u()
		}
		got <- err
	}()
	time.Sleep(50 * time.Millisecond)
	unlock()
	if err := <-got; err != nil {
		t.Errorf("LockShare once let go = %v", err)
	}
	if e.ShareBusy() {
		t.Error("ShareBusy once let go = true")
	}
}

// shareHolder is set in the environment of the test binary run as
// TestShareHolder: the data folder whose share it locks.
const shareHolder = "DEVVM_TEST_SHARE_HOLDER"

// TestShareHolder is a devvm that holds the share's lock until it's
// killed, when run by TestLockShareKilled.
func TestShareHolder(t *testing.T) {
	data := os.Getenv(shareHolder)
	if data == "" {
		t.Skip("run by TestLockShareKilled")
	}
	e := &Env{
		Dirs: paths.Dirs{Data: data}, Reporter: &msg.Fake{},
	}
	if _, err := e.LockShare(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stdout.WriteString("held\n"); err != nil { //nolint:forbidigo // the helper process tells its parent
		t.Fatal(err)
	}
	time.Sleep(time.Minute)
}

// A devvm killed while it holds the share's lock lets go of it: the next
// takes it at once.
func TestLockShareKilled(t *testing.T) {
	e := newEnv(t)
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestShareHolder$") //nolint:gosec // the test binary itself
	cmd.Env = append(os.Environ(), shareHolder+"="+e.Dirs.Data)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "held\n" {
		t.Fatalf("helper: %q, %v", line, err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	unlock, err := e.LockShare(ctx)
	if err != nil {
		t.Fatalf("LockShare once its holder was killed = %v", msg.NamesOf(err))
	}
	unlock()
}

// gc's list: the tool images List calls unused, neither current nor
// listed by a VM; none without a lock. Delete takes them and their
// hashes, and nothing but tool images.
func TestUnusedAndDelete(t *testing.T) {
	e := newEnv(t)
	e.locked(t, fixture(t))
	cur := e.files(t, "node")[0]
	old, used := "node-24.1.0-0123456789ab.erofs", "node-24.2.0-0123456789ab.erofs"
	for _, f := range []string{cur, old, used, "x.erofs.tmp", ".lock"} {
		write(t, filepath.Join(e.Dirs.Tools(), f), []byte("x"))
	}
	e.recorded(t, old, time.Now())
	if err := os.Remove(e.Dirs.ToolLock()); err != nil {
		t.Fatal(err)
	}
	unused := func() []string {
		ls, err := e.List([]Target{gnuT}, map[string]int{used: 1})
		if err != nil {
			t.Fatal(err)
		}
		var files []string
		for _, l := range Unused(ls) {
			files = append(files, l.File)
		}
		return files
	}
	if got := unused(); len(got) != 0 {
		t.Errorf("Unused without a lock = %q", got)
	}
	e.locked(t, fixture(t))
	got := unused()
	if want := []string{old}; !slices.Equal(got, want) {
		t.Fatalf("Unused = %q, want %q", got, want)
	}
	if err := e.Delete(got); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(e.Dirs.Tools(), old), e.Dirs.ToolRecord(old)} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: %v", p, err)
		}
	}
	for _, f := range []string{cur, used, "x.erofs.tmp", ".lock"} {
		if _, err := os.Stat(filepath.Join(e.Dirs.Tools(), f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	for _, bad := range []string{".lock", "../" + cur, "x.erofs.tmp"} {
		if err := e.Delete([]string{bad}); err == nil {
			t.Errorf("Delete(%q) took it", bad)
		}
	}
}

// List: by name, newest first, with how many VMs use each image and
// whether it's current; the lock's version when the share lacks it; a
// name with nothing in the share as not installed. It never resolves.
func TestList(t *testing.T) {
	e := newEnv(t)
	e.locked(t, fixture(t))
	goCur, claude := e.files(t, "go")[0], e.files(t, "claude")[0]
	old, used := "go-1.9.0-0123456789ab.erofs", "go-1.20.0-0123456789ab.erofs"
	for _, f := range []string{old, used, goCur, claude, "x.erofs.tmp"} {
		write(t, filepath.Join(e.Dirs.Tools(), f), []byte("x"))
	}
	ls, err := e.List([]Target{gnuT}, map[string]int{used: 2, claude: 1})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, l := range ls {
		got = append(got, fmt.Sprintf("%s %s %s %v %d %s", l.Name, l.Tool, l.Version, l.Built, l.VMs, l.State))
		if l.Built && l.Size <= 0 {
			t.Errorf("%s %s: size %d", l.Tool, l.Version, l.Size)
		}
	}
	want := []string{
		"node   false 0 not installed",
		"python   false 0 not installed",
		"rust   false 0 not installed",
		"go go 1.27.2 true 0 current",
		"go go 1.20.0 true 2 in use",
		"go go 1.9.0 true 0 unused",
		"claude claude 2.1.295 true 1 current",
		"codex   false 0 not installed",
		"opencode   false 0 not installed",
	}
	if !slices.Equal(got, want) {
		t.Errorf("List:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// The lock's newer go isn't built; without a lock, nothing is current
	// or unused.
	if err := os.Remove(filepath.Join(e.Dirs.Tools(), goCur)); err != nil {
		t.Fatal(err)
	}
	ls, _ = e.List([]Target{gnuT}, nil)
	if len(ls) < 4 || ls[3].State != StateNotBuilt || ls[3].Version != "1.27.2" || ls[3].Built || ls[4].State != StateUnused {
		t.Errorf("List without the lock's go: %+v", ls)
	}
	if err := os.Remove(e.Dirs.ToolLock()); err != nil {
		t.Fatal(err)
	}
	ls, _ = e.List([]Target{gnuT}, map[string]int{used: 1})
	if len(ls) < 5 || ls[3].State != StateInUse || ls[4].State != "" {
		t.Errorf("List without a lock: %+v", ls)
	}
	if c := e.calls(t); c != "" {
		t.Errorf("List ran %s", c)
	}
}

// What a tool image is built for, and the backend that built it, come
// from the record its build left beside it; one without a record (even a
// current one), or with a record that can't be read, has none. Delete
// deletes the record too.
func TestListTargets(t *testing.T) {
	e := newEnv(t)
	e.locked(t, fixture(t))
	goCur := e.files(t, "go")[0]
	recorded, bare, bad := "go-1.20.0-0123456789ab.erofs", "go-1.9.0-0123456789ab.erofs", "go-1.8.0-0123456789ab.erofs"
	for _, f := range []string{goCur, recorded, bare, bad} {
		write(t, filepath.Join(e.Dirs.Tools(), f), []byte("x"))
	}
	rec := state.ToolImage{SHA256: "x", Libc: "musl", Format: "plain-0123456789ab", Backend: "qemu", Built: time.Now()}
	if err := state.WriteToolImage(e.Dirs, recorded, rec); err != nil {
		t.Fatal(err)
	}
	write(t, e.Dirs.ToolRecord(bad), []byte("libc=musl\n"))
	ls, err := e.List([]Target{gnuT}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, l := range ls {
		if l.Built {
			got[l.File] = fmt.Sprintf("%v %s %s %s", l.Targeted, l.Backend, l.Target.Libc, l.Target.Format)
		}
	}
	want := map[string]string{
		goCur:    "false   ",
		recorded: "true qemu musl plain-0123456789ab",
		bare:     "false   ",
		bad:      "false   ",
	}
	if !maps.Equal(got, want) {
		t.Errorf("targets: got %v, want %v", got, want)
	}
	if err := e.Delete([]string{recorded}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.Dirs.ToolRecord(recorded)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Delete left the record: %v", err)
	}
}

// NoVM: current tool images no VM lists, of the names asked for, however
// new; never one a VM lists, one that isn't current, or one whose record
// is missing.
func TestNoVM(t *testing.T) {
	e := newEnv(t)
	old := time.Now().Add(-2 * time.Hour)
	ls := []Listed{
		{Name: "node", File: "node-1-0123456789ab.erofs", State: StateCurrent},
		{Name: "node", File: "bun-1-0123456789ab.erofs", State: StateCurrent, VMs: 1},
		{Name: "go", File: "go-1-0123456789ab.erofs", State: StateCurrent},
		{Name: "go", File: "go-2-0123456789ab.erofs", State: StateUnused},
		{Name: "claude", File: "claude-1-0123456789ab.erofs", State: StateCurrent},
		{Name: "codex", File: "codex-1-0123456789ab.erofs", State: StateCurrent},
	}
	for _, l := range ls {
		switch l.Name {
		case "codex": // no record
		case "claude": // built just now
			e.recorded(t, l.File, time.Now())
		default:
			e.recorded(t, l.File, old)
		}
	}
	files := func(ls []Listed) []string {
		var out []string
		for _, l := range ls {
			out = append(out, l.File)
		}
		return out
	}
	if got := files(e.NoVM(ls, nil)); !slices.Equal(got, []string{"node-1-0123456789ab.erofs", "go-1-0123456789ab.erofs", "claude-1-0123456789ab.erofs"}) {
		t.Errorf("NoVM = %v", got)
	}
	if got := files(e.NoVM(ls, []string{"go"})); !slices.Equal(got, []string{"go-1-0123456789ab.erofs"}) {
		t.Errorf("NoVM(go) = %v", got)
	}
}

// Any number hold Making at once and Exclude doesn't get it meanwhile;
// once they let go Exclude gets it, and Hold waits while it's held.
func TestMaking(t *testing.T) {
	m := Making{Dirs: paths.Dirs{Data: filepath.Join(t.TempDir(), "data")}}
	one, err := m.Hold(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	two, err := m.Hold(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := m.Exclude(); ok || err != nil {
		t.Errorf("Exclude while two are made: ok %v, %v", ok, err)
	}
	one()
	two()
	unlock, ok, err := m.Exclude()
	if !ok || err != nil {
		t.Fatalf("Exclude once made: ok %v, %v", ok, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, err := m.Hold(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Hold while excluded: %v", err)
	}
	// Hold waiting says so, once, and gets it once Exclude's let go.
	var said msg.Recorder
	m.Reporter = &msg.Fake{OnInfo: said.Add}
	got := make(chan error, 1)
	go func() {
		u, err := m.Hold(t.Context())
		if err == nil {
			u()
		}
		got <- err
	}()
	time.Sleep(400 * time.Millisecond)
	unlock()
	if err := <-got; err != nil {
		t.Errorf("Hold once let go: %v", err)
	}
	if !slices.Equal(said.IDs(), []string{"making_waiting"}) {
		t.Errorf("said %v", said.IDs())
	}
	said.Reset()
	if u, err := m.Hold(t.Context()); err != nil || len(said.IDs()) > 0 {
		t.Errorf("Hold with nothing in the way: %v, said %v", err, said.IDs())
	} else {
		u()
	}
}

// Idle records in a current tool image's record when it first has no VM
// (idle_since) and returns it once that's age ago; a VM listing it again
// clears it; a time in the future (a clock that went back) deletes
// nothing. IdleFor reads it back, changing nothing; an image without a
// record is skipped.
func TestIdle(t *testing.T) {
	e := newEnv(t)
	old := time.Now().Add(-2 * time.Hour)
	l := Listed{Name: "go", File: "go-1-0123456789ab.erofs", Built: true, State: StateCurrent}
	e.recorded(t, l.File, old)
	bare := Listed{Name: "go", File: "go-2-0123456789ab.erofs", Built: true, State: StateCurrent}
	now := time.Now()
	idleSince := func() time.Time {
		t.Helper()
		rec, err := state.ReadToolImage(e.Dirs, l.File)
		if err != nil {
			t.Fatal(err)
		}
		return rec.IdleSince
	}
	if e.IdleFor(l, now) != 0 {
		t.Error("IdleFor before Idle looked")
	}
	if got, err := e.Idle([]Listed{l, bare}, 24*time.Hour, now); err != nil || len(got) != 0 {
		t.Fatalf("first look: %v, %v", got, err)
	}
	if since := idleSince(); since.IsZero() || now.Sub(since) > time.Second {
		t.Fatalf("idle since %s, want %s", since, now)
	}
	if d := e.IdleFor(l, now.Add(time.Hour)); d < 59*time.Minute || d > time.Hour+time.Second {
		t.Errorf("IdleFor an hour later = %s", d)
	}
	if got, _ := e.Idle([]Listed{l}, 24*time.Hour, now.Add(25*time.Hour)); len(got) != 1 {
		t.Errorf("a day later: %v", got)
	}
	if got, _ := e.Idle([]Listed{l}, 24*time.Hour, now.Add(-25*time.Hour)); len(got) != 0 {
		t.Errorf("a clock that went back: %v", got)
	}
	used := l
	used.VMs = 1
	if e.IdleFor(used, now.Add(time.Hour)) != 0 {
		t.Error("IdleFor a VM's")
	}
	if got, _ := e.Idle([]Listed{used}, 24*time.Hour, now.Add(25*time.Hour)); len(got) != 0 {
		t.Errorf("a VM's: %v", got)
	}
	if since := idleSince(); !since.IsZero() {
		t.Errorf("a VM's is still idle since %s", since)
	}
	if rec, err := state.ReadToolImage(e.Dirs, l.File); err != nil || rec.SHA256 != "x" || !rec.Built.Equal(old) {
		t.Errorf("the rest of its record: %+v, %v", rec, err)
	}
}

// Only older versions of devvm's mise are stale: never this devvm's, nor
// one that changed in the last hour (another devvm downloading it), nor
// a file.
func TestStaleMise(t *testing.T) {
	e := newEnv(t)
	old := time.Now().Add(-2 * time.Hour)
	for _, v := range []string{miseVersion, "2020.1.1", "2099.1.1"} {
		dir := filepath.Join(e.Dirs.Mise(), v)
		write(t, filepath.Join(dir, "mise"), []byte("x"))
		if v != "2099.1.1" {
			if err := os.Chtimes(dir, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	write(t, filepath.Join(e.Dirs.Mise(), "notes"), []byte("x"))
	got, err := e.StaleMise()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{filepath.Join(e.Dirs.Mise(), "2020.1.1")}; !slices.Equal(got, want) {
		t.Errorf("StaleMise = %v, want %v", got, want)
	}
	if err := os.RemoveAll(e.Dirs.Mise()); err != nil {
		t.Fatal(err)
	}
	if got, err := e.StaleMise(); err != nil || got != nil {
		t.Errorf("StaleMise without a folder = %v, %v", got, err)
	}
}

func TestCompareVersions(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"1.10.0", "1.9.2", 1},
		{"24.21.0", "24.21.0", 0},
		{"1.2", "1.2.1", -1},
		{"0.162.0", "0.99.9", 1},
	} {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// devvm tools rm: the tools leave the VM's list and its tool files, the
// rest stays; what it lacks, and its layers, are refused.
func TestRemove(t *testing.T) {
	e := newEnv(t)
	e.locked(t, fixture(t))
	vm := "devvm-p-abc123abc123ab"
	all := e.files(t, "node", "pnpm", "bun", "python", "uv", "claude")
	names, files, err := Remove(vm, "base", []string{"node", "python", "claude"}, all, []string{"python", "claude"})
	if err != nil || !slices.Equal(names, []string{"node"}) || !slices.Equal(files, all[:3]) {
		t.Errorf("Remove = %q, %q, %v", names, files, err)
	}
	names, files, err = Remove(vm, "base", names, files, []string{"node"})
	if err != nil || len(names) != 0 || len(files) != 0 {
		t.Errorf("Remove the last = %q, %q, %v", names, files, err)
	}
	if _, _, err := Remove(vm, "foo", []string{"node", "go"}, nil, []string{"python"}); msg.NamesOf(err) != "vm_lacks has=node go tool=python vm="+vm {
		t.Errorf("Remove what it lacks = %v", msg.NamesOf(err))
	}
	if _, _, err := Remove(vm, "base", nil, nil, []string{"python"}); msg.NamesOf(err) != "vm_lacks has=none tool=python vm="+vm {
		t.Errorf("Remove from none = %v", msg.NamesOf(err))
	}
	if _, _, err := Remove(vm, "foo", []string{"node"}, nil, []string{"foo"}); msg.NamesOf(err) != "custom_layer_remove layer=foo vm="+vm {
		t.Errorf("Remove a layer = %v", msg.NamesOf(err))
	}
	// base is a base VM's one layer, and no other VM's.
	if _, _, err := Remove(vm, "base", []string{"node"}, nil, []string{"base"}); msg.NamesOf(err) != "custom_layer_remove layer=base vm="+vm {
		t.Errorf("Remove base from base = %v", msg.NamesOf(err))
	}
	if _, _, err := Remove(vm, "foo+bar", []string{"node"}, nil, []string{"base"}); msg.NamesOf(err) != "vm_lacks has=node tool=base vm="+vm {
		t.Errorf("Remove base from foo+bar = %v", msg.NamesOf(err))
	}
	if _, _, err := Remove(vm, "foo+bar", []string{"node"}, nil, []string{"bar"}); msg.NamesOf(err) != "custom_layer_remove layer=bar vm="+vm {
		t.Errorf("Remove bar from foo+bar = %v", msg.NamesOf(err))
	}
}

// devvm tools: each of a VM's tools with its version, and the lock's when
// it's another, built or not; without a lock, none is newer.
func TestShow(t *testing.T) {
	e := newEnv(t)
	if ts, err := e.Show([]string{"claude-2.0.0-0123456789ab.erofs"}, gnuT); err != nil || len(ts) != 1 || ts[0].Newer != "" {
		t.Errorf("Show without a lock = %+v, %v", ts, err)
	}
	e.locked(t, fixture(t))
	cur := e.files(t, "go")
	ts, err := e.Show(append(cur, "claude-2.0.0-0123456789ab.erofs", "not-a-tool-file"), gnuT)
	if err != nil {
		t.Fatal(err)
	}
	gov := parsed(t, fixture(t)).Version("go")
	cv := parsed(t, fixture(t)).Version("claude")
	want := []VMTool{{Tool: "go", Version: gov}, {Tool: "claude", Version: "2.0.0", Newer: cv}}
	if !slices.Equal(ts, want) {
		t.Errorf("Show = %+v, want %+v", ts, want)
	}
	// The lock's version, another build: the lock's is the one to move to.
	other := "go-" + gov + "-0123456789ab.erofs"
	if ts, _ := e.Show([]string{other}, gnuT); len(ts) != 1 || ts[0].Newer != gov {
		t.Errorf("Show another build = %+v", ts)
	}
	claude := e.files(t, "claude")[0]
	write(t, filepath.Join(e.Dirs.Tools(), claude), nil)
	e.recorded(t, claude, time.Now())
	if ts, _ := e.Show([]string{"claude-2.0.0-0123456789ab.erofs"}, gnuT); len(ts) != 1 || !ts[0].Built {
		t.Errorf("Show built = %+v", ts)
	}
}

// Newer shared tools than a VM's are named.
func TestWarnNewer(t *testing.T) {
	e := newEnv(t)
	e.locked(t, fixture(t))
	have := append(e.files(t, "node", "pnpm", "bun"), "claude-2.0.0-0123456789ab.erofs")
	if err := e.WarnNewer(t.Context(), []string{"node", "claude"}, have, gnuT); err != nil {
		t.Fatal(err)
	}
	want := []string{"devvm: newer_tools tools=claude " + parsed(t, fixture(t)).Version("claude")}
	if !slices.Equal(*e.said, want) {
		t.Errorf("said %q, want %q", *e.said, want)
	}
	*e.said = nil
	if err := e.WarnNewer(t.Context(), []string{"node", "claude"}, e.files(t, "node", "pnpm", "bun", "claude"), gnuT); err != nil || len(*e.said) > 0 {
		t.Errorf("said %q, %v", *e.said, err)
	}
}

// devvm tools update: resolves the newest versions, builds them, and says which
// the VM gets; offline, it says so and keeps the versions it has.
func TestUpdate(t *testing.T) {
	e := newEnv(t)
	e.locked(t, fixture(t))
	newer := regexp.MustCompile(`(?m)(\[\[tools\.go\]\]\n)version = ".*"`).ReplaceAll(fixture(t), []byte(`${1}version = "9.9.9"`))
	e.stubLocks(t, newer, newer, "")
	f := fake()
	build := func(ctx context.Context, files []string) error {
		return e.Build(ctx, backend.Review{}, f, baseKey, gnuT, files)
	}
	vm := "devvm-p-abc123abc123ab"
	before := e.files(t, "go")
	files, changed, err := e.Update(t.Context(), build, vm, []string{"go"}, before, gnuT)
	if err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if parsed(t, read(t, e.Dirs.ToolLock())).Version("go") != "9.9.9" || !changed || slices.Equal(files, before) {
		t.Errorf("Update = %q, %v", files, changed)
	}
	if !slices.Equal(*e.said, []string{"==> checking_tools", "==> building_tools tools=go 9.9.9", "==> tools_switch vm=" + vm}) {
		t.Errorf("said %q", *e.said)
	}
	*e.said = nil
	if again, changed, err := e.Update(t.Context(), build, vm, []string{"go"}, files, gnuT); err != nil || changed || !slices.Equal(again, files) {
		t.Errorf("Update again = %q, %v, %v", again, changed, err)
	}
	if !slices.Contains(*e.said, "==> tools_newest vm="+vm) {
		t.Errorf("said %q", *e.said)
	}
	// The newest, but gone from the share: built again.
	if err := os.Remove(filepath.Join(e.Dirs.Tools(), files[0])); err != nil {
		t.Fatal(err)
	}
	*e.said = nil
	if again, changed, err := e.Update(t.Context(), build, vm, []string{"go"}, files, gnuT); err != nil || changed || !slices.Equal(again, files) {
		t.Errorf("Update gone = %q, %v, %v", again, changed, err)
	}
	if !slices.Contains(*e.said, "==> building_tools tools=go 9.9.9") || !slices.Contains(*e.said, "==> tools_newest vm="+vm) || !e.Built(files[0]) {
		t.Errorf("said %q", *e.said)
	}
	t.Setenv("MISE_STUB_FAIL", "1")
	*e.said = nil
	if _, changed, err := e.Update(t.Context(), build, vm, []string{"go"}, before, gnuT); err != nil || !changed {
		t.Errorf("Update offline = %v, %v", changed, err)
	}
	if !slices.Contains(*e.said, "devvm: tools_check_failed") {
		t.Errorf("said %q", *e.said)
	}
}

// The host's mise is downloaded once and checked against its pin.
func TestHostMise(t *testing.T) {
	body := []byte("#!/bin/sh\necho mise\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v"+miseVersion+"/mise-v"+miseVersion+"-linux-x64" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	e := newEnv(t)
	e.Mise = ""
	e.OS, e.Arch = "linux", "amd64"
	e.miseURL = srv.URL + "/v%[1]s/mise-v%[1]s-%[2]s"
	e.miseSums = map[string]string{"linux-x64": strings.Repeat("0", 64)}
	if _, err := e.HostMise(t.Context()); msg.NamesOf(err) != "mise_checksum version="+miseVersion {
		t.Errorf("a download that doesn't match = %v", msg.NamesOf(err))
	}
	path := filepath.Join(e.Dirs.Mise(), miseVersion, "mise")
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Error("kept a download that doesn't match")
	}
	sum, err := image.SHA256File(writeTemp(t, body))
	if err != nil {
		t.Fatal(err)
	}
	e.miseSums["linux-x64"] = sum
	got, err := e.HostMise(t.Context())
	if err != nil || got != path {
		t.Fatalf("HostMise = %q, %v", got, err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("mise: %v %v", fi, err)
	}
	srv.Close()
	if again, err := e.HostMise(t.Context()); err != nil || again != path {
		t.Errorf("downloaded again: %q, %v", again, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := e.HostMise(t.Context()); msg.NamesOf(err) != "mise_download_failed version="+miseVersion {
		t.Errorf("offline = %v", msg.NamesOf(err))
	}
}

// mise's download follows redirects to https only, and keeps nothing it
// refused.
func TestHostMiseRedirect(t *testing.T) {
	var got int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mise" {
			got++
			_, _ = w.Write([]byte("mise"))
			return
		}
		http.Redirect(w, r, "/mise", http.StatusFound)
	}))
	defer srv.Close()
	e := newEnv(t)
	e.Mise = ""
	e.OS, e.Arch = "linux", "amd64"
	e.miseURL = srv.URL + "/v%[1]s/%[2]s"
	if _, err := e.HostMise(t.Context()); msg.NamesOf(err) != "mise_download_failed version="+miseVersion || got != 0 {
		t.Errorf("a redirect to http = %v, %d requests", msg.NamesOf(err), got)
	}
	if ents, _ := os.ReadDir(filepath.Join(e.Dirs.Mise(), miseVersion)); len(ents) != 0 {
		t.Errorf("left %v", ents)
	}
}

// mise's download is tried again after failures that may pass, and not
// after an answer that won't change.
func TestHostMiseRetry(t *testing.T) {
	body := []byte("#!/bin/sh\necho mise\n")
	sum, err := image.SHA256File(writeTemp(t, body))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		codes    []int // the answers before the file, one per request
		want     string
		requests int
	}{
		{"two server errors", []int{http.StatusInternalServerError, http.StatusBadGateway}, "", 3},
		{"too many requests", []int{http.StatusTooManyRequests}, "", 2},
		{"not found", []int{http.StatusNotFound}, "mise_download_failed version=" + miseVersion, 1},
		{"forbidden", []int{http.StatusForbidden}, "mise_download_failed version=" + miseVersion, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests++
				if requests <= len(tc.codes) {
					w.WriteHeader(tc.codes[requests-1])
					return
				}
				_, _ = w.Write(body)
			}))
			defer srv.Close()
			e := newEnv(t)
			e.Mise = ""
			e.OS, e.Arch = "linux", "amd64"
			e.miseURL = srv.URL + "/v%[1]s/mise-v%[1]s-%[2]s"
			e.miseSums = map[string]string{"linux-x64": sum}
			_, err := e.HostMise(t.Context())
			if got := msg.NamesOf(err); got != tc.want || requests != tc.requests {
				t.Errorf("HostMise = %q (%v) after %d requests, want %q after %d", got, err, requests, tc.want, tc.requests)
			}
		})
	}
}

func writeTemp(t *testing.T, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f")
	write(t, p, data)
	return p
}

// What a VM reads to mount its tools.
func TestMounts(t *testing.T) {
	got := string(Mounts([]string{"node-24.21.0-0123456789ab.erofs", "bad", "go-1.27.2-0123456789ab.erofs"}))
	if want := "node-24.21.0-0123456789ab.erofs node 24.21.0\ngo-1.27.2-0123456789ab.erofs go 1.27.2\n"; got != want {
		t.Errorf("Mounts = %q, want %q", got, want)
	}
}

// shared-tools/mise.toml's tools are the shared tools' members
// (image.SharedTools), each once, and the fixture lock has each.
func TestConfigTools(t *testing.T) {
	got, _ := configTools(read(t, repo(t, "assets", "shared-tools", "mise.toml")))
	var want []string
	for _, d := range image.SharedTools {
		want = append(want, d.Members...)
	}
	if slices.Sort(want); !slices.Equal(slices.Sorted(slices.Values(got)), want) {
		t.Errorf("configTools = %q, want the shared tools' members %q", got, want)
	}
	if locked := lockTools(t, fixture(t)); !slices.Equal(slices.Sorted(slices.Values(got)), locked) {
		t.Errorf("the fixture lock has %q", locked)
	}
}
