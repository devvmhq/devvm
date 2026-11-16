package landlock

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// confinedChild, set in the environment to a folder, makes TestSelf the child it starts: it
// confines itself to that folder with Self, and logs what it could do then.
const confinedChild = "DEVVM_CONFINE_TEST_CHILD"

// The filter's numbers for this machine's architecture are the system's.
func TestArchNumbers(t *testing.T) {
	t.Parallel()
	a, ok := archOf(runtime.GOARCH)
	if !ok {
		t.Skip("no filter for " + runtime.GOARCH)
	}
	if a.family[0] != syscall.SYS_SOCKET || a.family[1] != syscall.SYS_SOCKETPAIR || a.refused[0] != syscall.SYS_LISTEN ||
		a.prctl != syscall.SYS_PRCTL || !slices.Contains(a.refused, syscall.SYS_PTRACE) {
		t.Errorf("%s's numbers: %+v", runtime.GOARCH, a)
	}
}

// expected is what Self confines a process with on this kernel: Landlock from the floor, its
// signals scoped as signalsScoped says, and the seccomp filter, which every kernel devvm runs on
// has. Reading it confines nothing.
func expected() Result {
	r := Result{Seccomp: true}
	if abi := landlockABI(); abi >= Floor {
		r.ABI, r.SignalsScoped = abi, signalsScoped(abi)
	}
	return r
}

// The kernel's ABI and its fix for signals read without confining the test's process, which still
// lists /.
func TestKernel(t *testing.T) {
	t.Parallel()
	if r := expected(); r.SignalsScoped && r.ABI < ABISignals {
		t.Errorf("signals scoped at ABI %d", r.ABI)
	}
	if err := canOpen("/", syscall.O_RDONLY|syscall.O_DIRECTORY); err != nil {
		t.Errorf("reading the kernel confined the process: %v", err)
	}
}

// The check after confining fails when the confinement it was told of doesn't hold, here in the
// test's unconfined process: / lists, and a unix socket and a listen go through. With nothing to
// check, it lists the child's folders, and fails on one it can't open.
func TestCheck(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := check(Result{}, []string{dir}); err != nil {
		t.Errorf("nothing to check: %v", err)
	}
	if err := check(Result{}, []string{filepath.Join(dir, "gone")}); err == nil {
		t.Error("a folder that isn't there: no error")
	}
	for _, r := range []Result{{ABI: 5}, {Seccomp: true}, {ABI: abiUDP}} {
		if err := check(r, []string{dir}); err == nil {
			t.Errorf("%v, unconfined: no error", r)
		}
	}
	if refused("x", syscall.EACCES, syscall.EACCES) != nil || refused("x", nil, syscall.EACCES) == nil || refused("x", syscall.EPERM, syscall.EACCES) == nil {
		t.Error("refused")
	}
}

// Each ABI's ruleset handles every file right that ABI knows, and the child's folders get only
// what it does with regular files: never EXECUTE, MAKE_DIR, MAKE_SYM, MAKE_FIFO or MAKE_SOCK.
func TestRights(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		abi             int
		handled, folder uint64
	}{
		{1, 1<<13 - 1, 0b1_0010_1110},
		{2, 1<<14 - 1, 1<<13 | 0b1_0010_1110},
		{3, 1<<15 - 1, 1<<14 | 1<<13 | 0b1_0010_1110},
		{5, 1<<16 - 1, 1<<14 | 1<<13 | 0b1_0010_1110},
		{9, 1<<17 - 1, 1<<14 | 1<<13 | 0b1_0010_1110},
	} {
		if got := handledFS(tc.abi); got != tc.handled {
			t.Errorf("ABI %d handles %#x, want %#x", tc.abi, got, tc.handled)
		}
		if got := folderRights(tc.abi); got != tc.folder {
			t.Errorf("ABI %d's folders get %#x, want %#x", tc.abi, got, tc.folder)
		}
	}
	if !containsFolder([]string{"/etc"}, "/etc/x") || !containsFolder([]string{"/etc"}, "/etc") || containsFolder([]string{"/etc"}, "/etcx") || containsFolder([]string{"/etc"}, "/run") {
		t.Error("containsFolder")
	}
	if fs := resolverFolders(homeFolder()); fs[0] != "/etc" {
		t.Errorf("resolverFolders: %v", fs)
	}
	// A resolver's file that links into /, the home folder, a folder that holds it or one in it,
	// gives the child no folder.
	for _, tc := range []struct {
		dir, home string
		refused   bool
	}{
		{"/", "/home/me", true},
		{"/", "", true},
		{"/home", "/home/me", true},
		{"/home/me", "/home/me", true},
		{"/home/me/.config", "/home/me", true},
		{"/run/systemd/resolve", "/home/me", false},
		{"/home/meme", "/home/me", false},
		{"/home/me/x", "", false},
	} {
		if got := refusedFolder(tc.dir, tc.home); got != tc.refused {
			t.Errorf("refusedFolder(%q, %q): %v", tc.dir, tc.home, got)
		}
	}
}

// A child confined by Self, as the proxy's child is, can still write, rename between its
// folders and list its own folder, read the resolver's files and use the runtime's threads; and
// it can't read a file outside its folder, list /, read its own executable, make a folder, a
// link or a FIFO, connect to a unix socket (such as an ssh master's), make one, listen, bind a
// TCP port from ABI 4, or signal its parent from ABI 6, as far as this kernel confines it
// (expected). It runs only in a build without cgo, whose threads Go can reach; with cgo, Self
// confines nothing and says so (ErrUnsupported).
func TestSelf(t *testing.T) {
	if dir := os.Getenv(confinedChild); dir != "" {
		confinedChildRun(t, dir)
		return
	}
	if _, _, e := syscall.AllThreadsSyscall(syscall.SYS_GETPID, 0, 0, 0); e == syscall.ENOTSUP {
		if _, err := Self([]string{t.TempDir()}); !errors.Is(err, ErrUnsupported) {
			t.Errorf("Self with cgo: %v", err)
		}
		t.Skip("a build with cgo, whose threads Go can't reach: run with CGO_ENABLED=0")
	}
	want := expected()
	dir := t.TempDir()
	inside := filepath.Join(dir, "inside")
	must(t, os.MkdirAll(filepath.Join(inside, "nar"), 0o700))
	outside := filepath.Join(dir, "outside")
	must(t, os.WriteFile(outside, []byte("secret"), 0o600))
	sock := filepath.Join(dir, "ssh.sock")
	ln, err := net.Listen("unix", sock) //nolint:noctx // a listener for the test's life
	must(t, err)
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	self, err := os.Executable()
	must(t, err)
	cmd := exec.CommandContext(t.Context(), self, "-test.v", "-test.run=^TestSelf$") //nolint:gosec // the test binary itself
	cmd.Env = append(os.Environ(), confinedChild+"="+inside, "DEVVM_CONFINE_TEST_OUTSIDE="+outside, "DEVVM_CONFINE_TEST_SOCK="+sock)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	got := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		_, said, ok := strings.Cut(line, " confined.")
		if k, v, ok2 := strings.Cut(said, ": "); ok && ok2 {
			got[k] = v
		}
	}
	if got["result"] != fmt.Sprintf("%+v", want) {
		t.Fatalf("the child got %q, the kernel gives %+v: %s", got["result"], want, out)
	}
	t.Logf("confined: %+v", want)
	ok := map[string]bool{
		"write": true, "rename": true, "list": true, "resolver": true, "threads": true, "new threads": true,
		"fchmod": true, "ftruncate": true, "futimens": true, "prlimit self": true,
	}
	// What the filter alone refuses, with its own errno, whatever Landlock gives.
	filtered := map[string]syscall.Errno{
		"chmod": syscall.EPERM, "chown": syscall.EPERM, "setxattr": syscall.EPERM, "utimes": syscall.EPERM,
		"truncate": syscall.EPERM, "keyring": syscall.EPERM, "unshare": syscall.EPERM, "exec": syscall.EPERM,
		"clone3": syscall.ENOSYS, "prlimit parent": syscall.EPERM, "setpriority": syscall.EPERM, "inotify": syscall.EPERM,
	}
	for k, errno := range filtered {
		if want.Seccomp && got[k] != errno.Error() {
			t.Errorf("%s: %s, not the filter's %s", k, got[k], errno.Error())
		}
	}
	lands := want.ABI > 0
	refused := map[string]bool{
		"outside": lands, "root": lands, "exe": lands, "mkdir": lands, "symlink": lands, "fifo": lands,
		"unix": want.Seccomp || want.ABI >= ABIUnix, "socketpair": want.Seccomp, "listen": want.Seccomp,
		"bind":   want.ABI >= ABIPorts,
		"signal": want.SignalsScoped,
	}
	for k := range ok {
		if got[k] != "ok" {
			t.Errorf("%s: %s", k, got[k])
		}
	}
	for k, r := range refused {
		if r && got[k] == "ok" {
			t.Errorf("%s wasn't refused", k)
		}
		if !r && got[k] != "ok" {
			t.Errorf("%s: %s, where nothing refuses it", k, got[k])
		}
	}
}

// confinedChildRun is the child's side: it confines itself to dir and logs each thing it tried,
// "ok" or its error.
func confinedChildRun(t *testing.T, dir string) {
	t.Helper()
	r, err := Self([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	say := func(k string, err error) {
		v := "ok"
		if err != nil {
			v = err.Error()
		}
		t.Logf("confined.%s: %s", k, v)
	}
	t.Logf("confined.result: %+v", r)
	file := filepath.Join(dir, "events.jsonl")
	say("write", os.WriteFile(file, []byte("{}\n"), 0o600))
	say("rename", os.Rename(file, filepath.Join(dir, "nar", "x")))
	_, err = os.ReadDir(dir)
	say("list", err)
	_, err = os.ReadFile("/etc/hosts")
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	say("resolver", err)
	_, err = os.ReadFile(os.Getenv("DEVVM_CONFINE_TEST_OUTSIDE"))
	say("outside", err)
	_, err = os.ReadDir("/")
	say("root", err)
	_, err = os.ReadFile("/proc/self/exe")
	say("exe", err)
	say("mkdir", os.Mkdir(filepath.Join(dir, "d"), 0o700))
	say("symlink", os.Symlink("/etc/passwd", filepath.Join(dir, "l")))
	say("fifo", syscall.Mkfifo(filepath.Join(dir, "f"), 0o600))
	c, err := net.DialTimeout("unix", os.Getenv("DEVVM_CONFINE_TEST_SOCK"), 2*time.Second) //nolint:gosec,noctx // the test's own socket, which stands in for an ssh master's
	if err == nil {
		_ = c.Close()
	}
	say("unix", err)
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err == nil {
		_ = syscall.Close(fds[0])
		_ = syscall.Close(fds[1])
	}
	say("socketpair", err)
	say("listen", socket(syscall.AF_INET, syscall.SOCK_STREAM, func(fd int) error { return syscall.Listen(fd, 1) }))
	say("bind", socket(syscall.AF_INET, syscall.SOCK_STREAM, func(fd int) error {
		return syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}})
	}))
	say("signal", syscall.Kill(os.Getppid(), 0)) //nolint:forbidigo // signal 0 to its parent, the test, only to see whether it may
	// The runtime still runs goroutines on its threads, which preempt one another by signals.
	var wg sync.WaitGroup
	for range 4 * runtime.GOMAXPROCS(0) {
		wg.Go(func() {
			end := time.Now().Add(50 * time.Millisecond)
			for time.Now().Before(end) {
				// Spins, so the runtime has to preempt it.
			}
		})
	}
	wg.Wait()
	say("threads", nil)
	// New threads still start: each goroutine that locks its thread makes the runtime clone
	// another for the rest.
	started := make(chan struct{}, 8)
	for range 8 {
		go func() {
			runtime.LockOSThread()
			started <- struct{}{}
			time.Sleep(100 * time.Millisecond)
		}()
	}
	for range 8 {
		<-started
	}
	say("new threads", nil)
	confinedChildMetadata(dir, say)
}

// confinedChildMetadata is the confined child's tries at changing a file's metadata, the kernel's
// keyrings, a namespace, running a program and clone3, each logged as say does: by its path,
// which the filter refuses, and through a descriptor it opened, which it keeps. A refusal is its
// errno's text alone.
func confinedChildMetadata(dir string, say func(string, error)) {
	errnoOf := func(err error) error {
		var e syscall.Errno
		if errors.As(err, &e) {
			return e
		}
		return err
	}
	file := filepath.Join(dir, "meta")
	f, err := os.OpenFile(file, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // the test's own file
	if err != nil {
		say("chmod", err)
		return
	}
	defer func() { _ = f.Close() }()
	say("chmod", errnoOf(os.Chmod(file, 0o644))) //nolint:gosec // a mode the filter must refuse it, on the test's own file
	say("fchmod", f.Chmod(0o600))
	say("chown", errnoOf(os.Chown(file, -1, -1)))
	say("setxattr", errnoOf(syscall.Setxattr(file, "user.devvm", []byte("x"), 0)))
	now := time.Now()
	say("utimes", errnoOf(os.Chtimes(file, now, now)))
	ts := [2]syscall.Timespec{syscall.NsecToTimespec(now.UnixNano()), syscall.NsecToTimespec(now.UnixNano())}
	_, _, e := syscall.Syscall6(syscall.SYS_UTIMENSAT, f.Fd(), 0, uintptr(unsafe.Pointer(&ts[0])), 0, 0, 0) //nolint:gosec // the kernel reads the two times
	say("futimens", errnoOrNil(e))
	say("truncate", errnoOf(os.Truncate(file, 0)))
	say("ftruncate", f.Truncate(0))
	keyType, desc, payload := []byte("user\x00"), []byte("devvm-test\x00"), []byte("x")
	_, _, e = syscall.Syscall6(addKeyNr(), uintptr(unsafe.Pointer(&keyType[0])), uintptr(unsafe.Pointer(&desc[0])), //nolint:gosec // C strings the kernel reads
		uintptr(unsafe.Pointer(&payload[0])), 1, keySpecProcessKeyring, 0) //nolint:gosec // the payload the kernel reads
	say("keyring", errnoOrNil(e))
	say("unshare", errnoOf(syscall.Unshare(syscall.CLONE_NEWUSER)))
	say("exec", errnoOf(syscall.Exec(filepath.Join(dir, "nothing-to-run"), []string{"x"}, nil))) //nolint:gosec // a program that isn't there, which the filter refuses before the kernel looks
	_, _, e = syscall.RawSyscall(sysClone3, 0, 0, 0)
	say("clone3", errnoOrNil(e))
	// Another process's limits and scheduling, its parent's here, read alone, and its own limits.
	var lim syscall.Rlimit
	_, _, e = syscall.RawSyscall6(prlimitNr(), uintptr(os.Getppid()), syscall.RLIMIT_NOFILE, 0, uintptr(unsafe.Pointer(&lim)), 0, 0) //nolint:gosec // the kernel writes the limit
	say("prlimit parent", errnoOrNil(e))
	_, _, e = syscall.RawSyscall6(prlimitNr(), 0, syscall.RLIMIT_NOFILE, 0, uintptr(unsafe.Pointer(&lim)), 0, 0) //nolint:gosec // the kernel writes the limit
	say("prlimit self", errnoOrNil(e))
	prio, err := syscall.Getpriority(syscall.PRIO_PROCESS, 0)
	if err == nil {
		err = syscall.Setpriority(syscall.PRIO_PROCESS, 0, prio)
	}
	say("setpriority", errnoOf(err))
	fd, err := syscall.InotifyInit1(syscall.IN_CLOEXEC)
	if err == nil {
		_ = syscall.Close(fd)
	}
	say("inotify", errnoOf(err))
}

// prlimitNr is prlimit64's number on this machine.
func prlimitNr() uintptr {
	if runtime.GOARCH == "arm64" {
		return 261
	}
	return 302
}

// keySpecProcessKeyring is KEY_SPEC_PROCESS_KEYRING, -2: the process's own keyring.
const keySpecProcessKeyring = ^uintptr(1)

// addKeyNr is add_key's number on this machine.
func addKeyNr() uintptr {
	if runtime.GOARCH == "arm64" {
		return 217
	}
	return 248
}

// errnoOrNil is e as an error, nil for 0.
func errnoOrNil(e syscall.Errno) error {
	if e == 0 {
		return nil
	}
	return e
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
