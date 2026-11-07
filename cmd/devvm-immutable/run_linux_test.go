//go:build linux

package main

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// helper runs devvm-immutable with args, as the tests' user, and returns
// its exit status and what it wrote to stderr.
func helper(args ...string) (int, string) {
	var out, errs bytes.Buffer
	code := run(args, &out, &errs)
	return code, out.String() + errs.String()
}

// Usage errors exit 2, before anything is touched.
func TestUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"set"}, {"clear"}, {"get", "/x"}, {"check", "/x"}, {"+i", "/x"}} {
		if code, out := helper(args...); code != 2 || !strings.Contains(out, "usage:") {
			t.Errorf("%q: %d, %q", args, code, out)
		}
	}
}

// check fails without the capability, which a test never has, and says
// so.
func TestCheckWithoutTheCapability(t *testing.T) {
	if holdsCap() {
		t.Skip("this test runs with CAP_LINUX_IMMUTABLE")
	}
	if code, out := helper("check"); code != 1 || out != "devvm-immutable: it doesn't hold CAP_LINUX_IMMUTABLE\n" {
		t.Errorf("check: %d, %q", code, out)
	}
}

// kept keeps CAP_LINUX_IMMUTABLE alone, in effect when it's permitted,
// and nothing inheritable.
func TestKept(t *testing.T) {
	imm := uint32(1 << unix.CAP_LINUX_IMMUTABLE)
	all := ^uint32(0)
	for name, tc := range map[string]struct{ have, want capData }{
		"none":           {},
		"the file's":     {capData{{Effective: imm, Permitted: imm}}, capData{{Effective: imm, Permitted: imm}}},
		"permitted only": {capData{{Permitted: imm}}, capData{{Effective: imm, Permitted: imm}}},
		"root's":         {capData{{Effective: all, Permitted: all, Inheritable: all}, {Effective: all, Permitted: all}}, capData{{Effective: imm, Permitted: imm}}},
		"others only":    {capData{{Effective: all &^ imm, Permitted: all &^ imm}}, capData{}},
	} {
		if got := kept(tc.have); got != tc.want {
			t.Errorf("%s: kept = %+v, want %+v", name, got, tc.want)
		}
	}
}

// A test holds no capability, so limiting them changes nothing.
func TestLimitCapsWithNone(t *testing.T) {
	if err := limitCaps(); err != nil {
		t.Fatal(err)
	}
}

// flags returns path's attributes, or fails the test.
func flags(t *testing.T, path string) uint32 {
	t.Helper()
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFd(fd)
	f, err := unix.IoctlGetUint32(fd, unix.FS_IOC_GETFLAGS)
	if err != nil {
		t.Skipf("the test folder's file system has no attributes: %v", err)
	}
	return f
}

// What's refused, each by its reason, without touching anything; what's
// already as asked succeeds without the capability.
func TestRefusals(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config")
	hooks := filepath.Join(dir, "hooks")
	if err := os.WriteFile(file, []byte("[core]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(hooks, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = flags(t, file)
	link := filepath.Join(dir, "link")
	linkDir := filepath.Join(dir, "linkdir")
	fifo := filepath.Join(dir, "fifo")
	sock := filepath.Join(dir, "sock")
	for _, err := range []error{
		os.Symlink(file, link), os.Symlink(hooks, linkDir), syscall.Mkfifo(fifo, 0o600),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	for _, tc := range []struct {
		path, why string
	}{
		{"config", errPath.Error()},
		{dir + "/./config", errPath.Error()},
		{dir + "/hooks/../config", errPath.Error()},
		{hooks + "/", errPath.Error()},
		{"/", errPath.Error()},
		{link, errSymlink.Error()},
		{linkDir + "/x", errUnder.Error()},
		{file + "/x", errUnder.Error()},
		{fifo, errType.Error()},
		{sock, errType.Error()},
		{"/dev/null", errType.Error()},
		{filepath.Join(dir, "missing"), "no such file"},
	} {
		code, out := helper("set", tc.path)
		if code != 1 || !strings.Contains(out, tc.why) {
			t.Errorf("set %s: %d, %q, want %q", tc.path, code, out, tc.why)
		}
	}
	// The other paths are still done, and the run fails.
	if code, out := helper("clear", link, file); code != 1 || !strings.Contains(out, errSymlink.Error()) || strings.Contains(out, `"`+file+`"`) {
		t.Errorf("clear link file: %d, %q", code, out)
	}
	// Already clear: nothing to change, so no capability needed.
	if code, out := helper("clear", file, hooks); code != 0 {
		t.Errorf("clear: %d, %q", code, out)
	}
}

// Another user's file is refused, as checkStat sees it; and what checkStat
// passes.
func TestCheckStat(t *testing.T) {
	for name, tc := range map[string]struct {
		mode uint32
		uid  uint32
		want error
	}{
		"a file":         {unix.S_IFREG | 0o644, 1000, nil},
		"a folder":       {unix.S_IFDIR | 0o755, 1000, nil},
		"another's file": {unix.S_IFREG | 0o644, 0, errNotYours},
		"a symlink":      {unix.S_IFLNK | 0o777, 1000, errSymlink},
		"a device":       {unix.S_IFCHR | 0o666, 1000, errType},
		"a FIFO":         {unix.S_IFIFO | 0o600, 1000, errType},
	} {
		if err := checkStat(unix.Stat_t{Mode: tc.mode, Uid: tc.uid}, 1000); err != tc.want { //nolint:errorlint // its own sentinels
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}
	w := walker{uid: uint32(os.Getuid()) + 1, stderr: new(bytes.Buffer)} //nolint:gosec // a test's uid
	w.top(t.TempDir())
	if !w.failed || !strings.Contains(w.stderr.(*bytes.Buffer).String(), errNotYours.Error()) {
		t.Errorf("another user's folder: %v, %q", w.failed, w.stderr)
	}
}

// Setting the attribute needs the capability: without it, the kernel
// refuses, and the run says so; a folder isn't walked past its refusal.
func TestSetWithoutTheCapability(t *testing.T) {
	if holdsCap() {
		t.Skip("this test runs with CAP_LINUX_IMMUTABLE")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "config")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = flags(t, file)
	code, out := helper("set", dir)
	if code != 1 || !strings.Contains(out, "operation not permitted") || strings.Contains(out, "config") {
		t.Errorf("set: %d, %q", code, out)
	}
	if flags(t, file)&immutableFl != 0 {
		t.Error("locked without the capability")
	}
}

// A file with other names is never locked, as they may be outside the
// folder; clearing it is still done.
func TestHardLinksRefused(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "hook")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(file, filepath.Join(t.TempDir(), "elsewhere")); err != nil {
		t.Fatal(err)
	}
	_ = flags(t, file)
	if code, out := helper("set", file); code != 1 || !strings.Contains(out, errLinked.Error()) {
		t.Errorf("set: %d, %q", code, out)
	}
	if code, out := helper("clear", file); code != 0 {
		t.Errorf("clear: %d, %q", code, out)
	}
}

// A folder's walk skips symlinks, and refuses one too deep or too large.
func TestWalkBounds(t *testing.T) {
	dir := t.TempDir()
	deep := dir
	for range maxDepth + 1 {
		deep = filepath.Join(deep, "d")
	}
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "d", "out")); err != nil {
		t.Fatal(err)
	}
	_ = flags(t, dir)
	// Already clear, so only the walk can fail.
	code, out := helper("clear", dir)
	if code != 1 || !strings.Contains(out, errTooDeep.Error()) || strings.Contains(out, "/out") {
		t.Errorf("deep: %d, %q", code, out)
	}
	wide := t.TempDir()
	for i := range maxEntries + 1 {
		f, err := os.Create(filepath.Join(wide, strconv.Itoa(i))) //nolint:gosec // the test's own folder
		if err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	}
	if code, out := helper("clear", wide); code != 1 || !strings.Contains(out, errTooMany.Error()) {
		t.Errorf("wide: %d, %q", code, out)
	}
}
