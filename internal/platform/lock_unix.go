//go:build unix

package platform

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"
)

// lockTick is how often LockFile tries again for a lock another process
// holds.
const lockTick = 50 * time.Millisecond

// LockFile holds an exclusive lock on path (made if missing, mode 600)
// until unlock is called or the process ends, waiting for another process
// that holds it until ctx ends, while waiting (Waiting) says so. Its
// descriptor isn't inherited (close on exec), so a process started
// meanwhile doesn't keep it. The holder may remove the file before it lets
// go: one that got the lock on the removed file tries again on path.
func LockFile(ctx context.Context, path string, waiting Waiting) (unlock func(), err error) {
	return lockFile(ctx, path, syscall.LOCK_EX, waiting)
}

// LockFileShared holds a shared lock on path, as LockFile does: any
// number of processes hold it at once, and it waits only while one holds
// it exclusively (LockFile, TryLockFile).
func LockFileShared(ctx context.Context, path string, waiting Waiting) (unlock func(), err error) {
	return lockFile(ctx, path, syscall.LOCK_SH, waiting)
}

// TryLockFile is LockFile without waiting: ok is false while another
// holds the lock, shared or exclusive.
func TryLockFile(path string) (unlock func(), ok bool, err error) {
	for {
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // devvm's own lock file
		if err != nil {
			return nil, false, err
		}
		got, err := flock(f, syscall.LOCK_EX)
		switch {
		case err != nil:
			_ = f.Close()
			return nil, false, err
		case !got:
			_ = f.Close()
			return nil, false, nil
		case !current(f, path):
			_ = f.Close()
			continue
		}
		return func() { _ = f.Close() }, true, nil
	}
}

// LockHeld reports whether another process holds path's lock
// exclusively; false when there's no such file.
func LockHeld(path string) (bool, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0) //nolint:gosec // devvm's own lock file
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	got, err := flock(f, syscall.LOCK_SH)
	return !got, err
}

// lockFile takes the lock how says (LOCK_EX or LOCK_SH) on path, trying
// again lockTick apart until ctx ends; waiting, if any, is called once it
// finds the lock held, and what it returned once the wait ends.
func lockFile(ctx context.Context, path string, how int, waiting Waiting) (unlock func(), err error) {
	checkWaiting(waiting)
	var end func()
	defer func() {
		if end != nil {
			end()
		}
	}()
	for {
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // devvm's own lock file
		if err != nil {
			return nil, err
		}
		if err := waitLock(ctx, f, how, &waiting, &end); err != nil {
			_ = f.Close()
			return nil, err
		}
		if current(f, path) {
			return func() { _ = f.Close() }, nil
		}
		_ = f.Close()
	}
}

// LockDir holds an exclusive lock on the folder dir itself, as LockFile
// holds one on a file, until unlock is called or the process ends,
// waiting for another process that holds it until ctx ends, while waiting
// (Waiting) says so. It never makes the folder: one that isn't there is
// os.ErrNotExist. Its descriptor isn't inherited (close on exec).
func LockDir(ctx context.Context, dir string, waiting Waiting) (unlock func(), err error) {
	checkWaiting(waiting)
	var end func()
	defer func() {
		if end != nil {
			end()
		}
	}()
	f, err := os.Open(dir) //nolint:gosec // a folder, locked, never read
	if err != nil {
		return nil, err
	}
	if err := waitLock(ctx, f, syscall.LOCK_EX, &waiting, &end); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() { _ = f.Close() }, nil
}

// waitLock takes the lock how says on f, trying again lockTick apart
// until ctx ends. The first time it finds the lock held, it calls
// *waiting, if any, sets it nil and keeps what it returned in *end, for
// its caller to call once its wait is over.
func waitLock(ctx context.Context, f *os.File, how int, waiting *Waiting, end *func()) error {
	for {
		got, err := flock(f, how)
		if err != nil || got {
			return err
		}
		if *waiting != nil {
			*end = (*waiting)()
			*waiting = nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(lockTick):
		}
	}
}

// flock tries the lock how says on f once, without waiting: got is false
// while another holds it.
func flock(f *os.File, how int) (got bool, err error) {
	for {
		err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.EWOULDBLOCK):
			return false, nil
		}
		return false, err
	}
}

// current reports whether f is still the file at path: the last holder
// may have removed it, or put another there.
func current(f *os.File, path string) bool {
	a, err := f.Stat()
	if err != nil {
		return false
	}
	b, err := os.Stat(path) //nolint:gosec // devvm's own lock file, which f has open
	return err == nil && os.SameFile(a, b)
}
