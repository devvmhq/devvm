package landlock

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
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
	if a.socket != syscall.SYS_SOCKET || a.refused[0] != syscall.SYS_SOCKETPAIR || a.refused[1] != syscall.SYS_CONNECT ||
		a.refused[2] != syscall.SYS_LISTEN || a.flags3[0] != syscall.SYS_SENDTO || a.flags3[1] != sysSendmmsg() ||
		a.flags2 != syscall.SYS_SENDMSG || a.prctl != syscall.SYS_PRCTL || !slices.Contains(a.refused, syscall.SYS_PTRACE) {
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
	for _, r := range []Result{{ABI: 5}, {Seccomp: true}} {
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
}

// A child confined by Self, as the proxy's child is, can still write, rename between its
// folders and list its own folder, read the host's addresses, write to a connection it held and
// use the runtime's threads; and it can't read a file outside its folder or /etc/hosts, list /,
// read its own executable, make a folder, a link or a FIFO, connect to a unix socket (such as an
// ssh master's), make one, make an IPv4, IPv6 or other netlink socket, connect a socket it held,
// send with MSG_FASTOPEN, listen, bind a TCP port, or signal its parent from ABI 6, as far as
// this kernel confines it
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
	// A server on loopback, which stands in for a dev server of yours.
	tl, err := net.Listen("tcp", "127.0.0.1:0") //nolint:noctx // a listener for the test's life
	must(t, err)
	defer func() { _ = tl.Close() }()
	for _, l := range []net.Listener{ln, tl} {
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				// Read until the child closes, so its write to a connection it held goes through.
				go func() { _, _ = io.Copy(io.Discard, c); _ = c.Close() }()
			}
		}()
	}
	self, err := os.Executable()
	must(t, err)
	cmd := exec.CommandContext(t.Context(), self, "-test.v", "-test.run=^TestSelf$") //nolint:gosec // the test binary itself
	cmd.Env = append(os.Environ(), confinedChild+"="+inside, "DEVVM_CONFINE_TEST_OUTSIDE="+outside, "DEVVM_CONFINE_TEST_SOCK="+sock,
		"DEVVM_CONFINE_TEST_TCP="+tl.Addr().String())
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
		"write": true, "rename": true, "list": true, "threads": true, "new threads": true,
		"fchmod": true, "ftruncate": true, "futimens": true, "prlimit self": true,
		"host addresses": true, "write held": true,
	}
	// What the filter alone refuses, with its own errno, whatever Landlock gives.
	filtered := map[string]syscall.Errno{
		"chmod": syscall.EPERM, "chown": syscall.EPERM, "setxattr": syscall.EPERM, "utimes": syscall.EPERM,
		"truncate": syscall.EPERM, "keyring": syscall.EPERM, "unshare": syscall.EPERM, "exec": syscall.EPERM,
		"clone3": syscall.ENOSYS, "prlimit parent": syscall.EPERM, "setpriority": syscall.EPERM, "inotify": syscall.EPERM,
		"inet": syscall.EPERM, "inet6": syscall.EPERM, "netlink diag": syscall.EPERM,
		"connect held": syscall.EPERM, "fastopen": syscall.EPERM,
	}
	for k, errno := range filtered {
		if want.Seccomp && got[k] != errno.Error() {
			t.Errorf("%s: %s, not the filter's %s", k, got[k], errno.Error())
		}
	}
	lands := want.ABI > 0
	refused := map[string]bool{
		"outside": lands, "hosts": lands, "root": lands, "exe": lands, "mkdir": lands, "symlink": lands, "fifo": lands,
		"unix": want.Seccomp || want.ABI >= ABIUnix, "socketpair": want.Seccomp, "listen": want.Seccomp,
		"bind":   want.Seccomp || want.ABI >= ABIPorts,
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
	// What the child holds before it's confined, as the proxy's child holds its listener and the
	// connections it got: a connection to the loopback server, a TCP socket not yet connected,
	// and a listener.
	server, err := netip.ParseAddrPort(os.Getenv("DEVVM_CONFINE_TEST_TCP"))
	must(t, err)
	sa := &syscall.SockaddrInet4{Port: int(server.Port()), Addr: server.Addr().As4()}
	conn, err := net.Dial("tcp", server.String()) //nolint:noctx,gosec // the test's own server on loopback, from its parent
	must(t, err)
	defer func() { _ = conn.Close() }()
	held, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	must(t, err)
	defer func() { _ = syscall.Close(held) }()
	lfd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	must(t, err)
	defer func() { _ = syscall.Close(lfd) }()
	must(t, syscall.Bind(lfd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}))
	must(t, syscall.Listen(lfd, 1))
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
	say("hosts", err)
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
	confinedChildNetwork(conn, held, lfd, sa, say)
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

// confinedChildNetwork is the confined child's tries at the network, each logged as say does: it
// still writes to conn, a connection it held before, and reads the host's addresses through a
// netlink route socket, but makes no IPv4, IPv6 or other netlink socket, connects held, a TCP
// socket it made before, to the loopback server at sa, and sends with MSG_FASTOPEN from lfd, its
// listener, once a shutdown has closed it, which would connect it to sa as it sends.
func confinedChildNetwork(conn net.Conn, held, lfd int, sa syscall.Sockaddr, say func(string, error)) {
	errnoOf := func(err error) error {
		var e syscall.Errno
		if errors.As(err, &e) {
			return e
		}
		return err
	}
	_, err := conn.Write([]byte("x"))
	say("write held", err)
	addrs, err := net.InterfaceAddrs()
	if err == nil && len(addrs) == 0 {
		err = errors.New("no addresses")
	}
	say("host addresses", err)
	say("inet", errnoOf(socket(syscall.AF_INET, syscall.SOCK_STREAM, nil)))
	say("inet6", errnoOf(socket(syscall.AF_INET6, syscall.SOCK_DGRAM, nil)))
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, 4) // NETLINK_SOCK_DIAG, which lists sockets
	if err == nil {
		_ = syscall.Close(fd)
	}
	say("netlink diag", errnoOf(err))
	say("connect held", errnoOf(syscall.Connect(held, sa)))
	_ = syscall.Shutdown(lfd, syscall.SHUT_RDWR)
	say("fastopen", errnoOf(syscall.Sendto(lfd, []byte("x"), msgFastOpen, sa)))
}

// sysSendmmsg is sendmmsg's number on this machine, which the syscall package doesn't name on
// arm64.
func sysSendmmsg() uint32 {
	if runtime.GOARCH == "arm64" {
		return 269
	}
	return 307
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
