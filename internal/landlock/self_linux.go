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

// check checks that r holds in this process, with its folders: each can be listed; with Landlock,
// the root folder can't be listed, nor /etc/hosts read, when it's there; with the seccomp filter, a
// unix or an IPv4 socket can't be made, and nothing can connect, listen or send with
// MSG_FASTOPEN, which the filter refuses before the kernel looks at the descriptor (-1 here).
func check(r Result, folders []string) error {
	for _, dir := range folders {
		if err := canOpen(dir, syscall.O_RDONLY|syscall.O_DIRECTORY); err != nil {
			return fmt.Errorf("its own folder %s: %w", dir, err)
		}
	}
	if r.ABI > 0 {
		if err := refused("listing /", canOpen("/", syscall.O_RDONLY|syscall.O_DIRECTORY), syscall.EACCES); err != nil {
			return err
		}
		if err := canOpen("/etc/hosts", syscall.O_RDONLY); !errors.Is(err, syscall.ENOENT) {
			if err := refused("reading /etc/hosts", err, syscall.EACCES); err != nil {
				return err
			}
		}
	}
	if !r.Seccomp {
		return nil
	}
	for _, c := range []struct {
		what string
		err  error
	}{
		{"a unix socket", socket(syscall.AF_UNIX, syscall.SOCK_STREAM, nil)},
		{"an IPv4 socket", socket(syscall.AF_INET, syscall.SOCK_STREAM, nil)},
		{"a connect", syscall.Connect(-1, &syscall.SockaddrInet4{Port: 1, Addr: [4]byte{127, 0, 0, 1}})},
		{"a listen", syscall.Listen(-1, 1)},
		{"a send with MSG_FASTOPEN", syscall.Sendto(-1, nil, msgFastOpen, &syscall.SockaddrInet4{Port: 1, Addr: [4]byte{127, 0, 0, 1}})},
	} {
		if err := refused(c.what, c.err, syscall.EPERM); err != nil {
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
