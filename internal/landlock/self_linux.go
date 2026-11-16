package landlock

import (
	"errors"
	"fmt"
	"runtime"
	"syscall"
)

// ErrUnsupported is Self's error where it can reach no thread but its own, so confines nothing: a
// build with cgo (syscall.AllThreadsSyscall), as the race detector's is, or an architecture the
// filter has no numbers for.
var ErrUnsupported = errors.New("this build can't confine every thread of the process")

// Self confines this process, every thread of it, to folders, where it makes, reads, writes,
// renames and deletes regular files, and checks that it holds (check): no_new_privs, then Landlock
// from ABI 2 (Floor), then the seccomp filter. A kernel without Landlock, or below ABI 2, or
// without seccomp, leaves that part out, which the Result says; a build that can't reach every
// thread confines nothing (ErrUnsupported). Anything else that fails is an error, and the process
// mustn't go on to read a VM's bytes then: part of it may already be confined.
func Self(folders []string) (Result, error) {
	if _, ok := archOf(runtime.GOARCH); !ok {
		return Result{}, ErrUnsupported
	}
	if _, _, e := syscall.AllThreadsSyscall(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0); e != 0 {
		if e == syscall.ENOTSUP {
			return Result{}, ErrUnsupported
		}
		return Result{}, fmt.Errorf("no_new_privs: %w", e)
	}
	var r Result
	if abi := landlockABI(); abi >= Floor {
		scoped := signalsScoped(abi)
		if err := landlock(abi, folders, scoped); err != nil {
			return Result{}, err
		}
		r.ABI, r.SignalsScoped = abi, scoped
	}
	switch err := installSeccomp(); {
	case err == nil:
		r.Seccomp = true
	case !errors.Is(err, errNoSeccomp):
		return Result{}, err
	}
	if err := check(r, folders); err != nil {
		return Result{}, fmt.Errorf("the confinement's check: %w", err)
	}
	return r, nil
}

// check checks that r holds in this process, with its folders: each can be listed, and the
// resolver's files that are there read; with Landlock, the root folder can't be listed, and from
// ABI 10 a UDP socket can connect to port 53 and no other; with the seccomp filter, a unix socket
// can't be made, and a TCP socket can't listen.
func check(r Result, folders []string) error {
	for _, dir := range folders {
		if err := canOpen(dir, syscall.O_RDONLY|syscall.O_DIRECTORY); err != nil {
			return fmt.Errorf("its own folder %s: %w", dir, err)
		}
	}
	for _, f := range resolverFiles() {
		if err := canOpen(f, syscall.O_RDONLY); err != nil && !errors.Is(err, syscall.ENOENT) {
			return fmt.Errorf("the resolver's %s: %w", f, err)
		}
	}
	if r.ABI > 0 {
		if err := refused("listing /", canOpen("/", syscall.O_RDONLY|syscall.O_DIRECTORY), syscall.EACCES); err != nil {
			return err
		}
	}
	if r.ABI >= abiUDP {
		if err := udpConnect(dnsPort); err != nil {
			return fmt.Errorf("a resolver's port: %w", err)
		}
		if err := refused("a UDP port but the resolvers'", udpConnect(dnsPort+1), syscall.EACCES); err != nil {
			return err
		}
	}
	if r.Seccomp {
		if err := refused("a unix socket", socket(syscall.AF_UNIX, syscall.SOCK_STREAM, nil), syscall.EPERM); err != nil {
			return err
		}
		listen := func(fd int) error { return syscall.Listen(fd, 1) }
		if err := refused("a listen", socket(syscall.AF_INET, syscall.SOCK_STREAM, listen), syscall.EPERM); err != nil {
			return err
		}
	}
	return nil
}

// refused is nil when what failed with want, the confinement's refusal, and an error otherwise.
func refused(what string, err error, want syscall.Errno) error {
	switch {
	case errors.Is(err, want):
		return nil
	case err == nil:
		return fmt.Errorf("%s went through", what)
	}
	return fmt.Errorf("%s: %w, not the confinement's refusal", what, err)
}

// canOpen opens path with flags and closes it.
func canOpen(path string, flags int) error {
	fd, err := syscall.Open(path, flags|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	return syscall.Close(fd)
}

// socket makes a socket of family and type, runs then on it (nil: nothing), and closes it.
func socket(family, typ int, then func(fd int) error) error {
	fd, err := syscall.Socket(family, typ|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() { _ = syscall.Close(fd) }()
	if then != nil {
		return then(fd)
	}
	return nil
}

// udpConnect connects a UDP socket to port on this machine, which sends nothing.
func udpConnect(port int) error {
	return socket(syscall.AF_INET, syscall.SOCK_DGRAM, func(fd int) error {
		return syscall.Connect(fd, &syscall.SockaddrInet4{Port: port, Addr: [4]byte{127, 0, 0, 1}})
	})
}
