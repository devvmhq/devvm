//go:build unix

package platform

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"testing/synctest"
	"time"
)

// A file's lock is held by one at a time: another waits for it until its
// context ends, and gets it once it's let go.
func TestLockFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	unlock, err := LockFile(t.Context(), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if _, err := LockFile(ctx, path, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("held twice: %v", err)
	}
	got := make(chan error, 1)
	go func() {
		u, err := LockFile(t.Context(), path, nil)
		if err == nil {
			u()
		}
		got <- err
	}()
	time.Sleep(100 * time.Millisecond)
	unlock()
	select {
	case err := <-got:
		if err != nil {
			t.Errorf("once let go: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("not got once let go")
	}
	if _, err := LockFile(t.Context(), filepath.Join(t.TempDir(), "no", "x.lock"), nil); err == nil {
		t.Error("a lock in a missing folder: no error")
	}
}

// A lock's waiting is called once, only when another holds the lock, and
// what it returned once the wait ends, with the lock got or given up; a
// free lock calls neither, shared or not.
func TestLockFileWaiting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	waits, ends := 0, 0
	waiting := func() func() {
		waits++
		return func() { ends++ }
	}
	unlock, err := LockFile(t.Context(), path, waiting)
	if err != nil || waits != 0 || ends != 0 {
		t.Fatalf("a free lock: %v, waits %d, ends %d", err, waits, ends)
	}
	for _, shared := range []bool{false, true} {
		waits, ends = 0, 0
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		lock := LockFile
		if shared {
			lock = LockFileShared
		}
		if _, err := lock(ctx, path, waiting); !errors.Is(err, context.DeadlineExceeded) || waits != 1 || ends != 1 {
			t.Errorf("held, shared %v: %v, waits %d, ends %d", shared, err, waits, ends)
		}
		cancel()
	}
	// Got once let go: the wait ends then.
	waits, ends = 0, 0
	got := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		u, err := LockFile(t.Context(), path, func() func() { close(started); return func() { ends++ } })
		if err == nil {
			u()
		}
		got <- err
	}()
	<-started
	unlock()
	if err := <-got; err != nil || ends != 1 {
		t.Errorf("got once let go: %v, ends %d", err, ends)
	}
	shared, err := LockFileShared(t.Context(), path, waiting)
	if err != nil || waits != 0 {
		t.Errorf("a free shared lock: %v, waits %d", err, waits)
	} else {
		shared()
	}
}

// A folder's lock is held by one at a time, as a file's is: another waits
// for it, showing its wait once, until its context ends, a deadline or a
// cancel, and gets it once it's let go. It's the lock a plain flock of the
// folder takes, as an older devvm's proxy takes it. A folder that isn't
// there isn't made.
func TestLockDir(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		unlock, err := LockDir(t.Context(), dir, nil)
		if err != nil {
			t.Fatal(err)
		}
		waits, ends := 0, 0
		waiting := func() func() {
			waits++
			return func() { ends++ }
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		if _, err := LockDir(ctx, dir, waiting); !errors.Is(err, context.DeadlineExceeded) || waits != 1 || ends != 1 {
			t.Errorf("held, to a deadline: %v, waits %d, ends %d", err, waits, ends)
		}
		cancel()
		ctx, cancel = context.WithCancel(t.Context())
		got := make(chan error, 1)
		go func() {
			_, err := LockDir(ctx, dir, nil)
			got <- err
		}()
		synctest.Wait()
		cancel()
		if err := <-got; !errors.Is(err, context.Canceled) {
			t.Errorf("held, cancelled: %v", err)
		}
		go func() {
			u, err := LockDir(t.Context(), dir, nil)
			if err == nil {
				u()
			}
			got <- err
		}()
		synctest.Wait()
		unlock()
		if err := <-got; err != nil {
			t.Errorf("once let go: %v", err)
		}
		d, err := os.Open(dir) //nolint:gosec // G304: the test's own temp folder
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = d.Close() }()
		if err := syscall.Flock(int(d.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			t.Fatal(err)
		}
		ctx, cancel = context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		if _, err := LockDir(ctx, dir, nil); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("held by a plain flock: %v", err)
		}
		missing := filepath.Join(dir, "no")
		if _, err := LockDir(t.Context(), missing, nil); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a missing folder: %v", err)
		}
		if _, err := os.Stat(missing); err == nil {
			t.Error("LockDir made the folder")
		}
	})
}

// A holder may remove the lock file before it lets go: one waiting for
// the removed file takes the lock on the new one, so two never hold it.
func TestLockFileRemoved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	unlock, err := LockFile(t.Context(), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan func(), 1)
	go func() {
		u, err := LockFile(t.Context(), path, nil)
		if err != nil {
			t.Error(err)
		}
		got <- u
	}()
	time.Sleep(100 * time.Millisecond) // it has opened the file, and waits
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	unlock()
	u := <-got
	if _, ok, err := TryLockFile(path); ok || err != nil {
		t.Errorf("tried while the waiter holds the new file: ok %v, %v", ok, err)
	}
	u()
}

// LockHeld tells an exclusive lock another holds, and takes none.
func TestLockHeld(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	if held, err := LockHeld(path); held || err != nil {
		t.Errorf("no file: %v, %v", held, err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("LockHeld made the file")
	}
	unlock, err := LockFile(t.Context(), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if held, err := LockHeld(path); !held || err != nil {
		t.Errorf("held: %v, %v", held, err)
	}
	unlock()
	if held, err := LockHeld(path); held || err != nil {
		t.Errorf("let go: %v, %v", held, err)
	}
	if unlock, ok, err := TryLockFile(path); !ok || err != nil {
		t.Errorf("after LockHeld: ok %v, %v", ok, err)
	} else {
		unlock()
	}
}

// A shared lock is held by any number at once; TryLockFile doesn't get
// the lock while any holds it, and gets it once all let go, keeping
// shared ones out meanwhile.
func TestLockFileShared(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	one, err := LockFileShared(t.Context(), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	two, err := LockFileShared(t.Context(), path, nil)
	if err != nil {
		t.Fatalf("a second shared lock: %v", err)
	}
	if _, ok, err := TryLockFile(path); ok || err != nil {
		t.Errorf("tried while two held: ok %v, %v", ok, err)
	}
	one()
	if _, ok, err := TryLockFile(path); ok || err != nil {
		t.Errorf("tried while one held: ok %v, %v", ok, err)
	}
	two()
	unlock, ok, err := TryLockFile(path)
	if !ok || err != nil {
		t.Fatalf("tried once let go: ok %v, %v", ok, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if _, err := LockFileShared(ctx, path, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("shared while tried: %v", err)
	}
	unlock()
	if _, _, err := TryLockFile(filepath.Join(t.TempDir(), "no", "x.lock")); err == nil {
		t.Error("a lock in a missing folder: no error")
	}
}

// lockHelper is set in the environment of the test binary run as
// TestLockHolder: the lock file it holds.
const lockHelper = "DEVVM_TEST_LOCK_HOLDER"

// TestLockHolder is a process that holds a shared lock until it's killed,
// when run by TestLockFileKilled.
func TestLockHolder(t *testing.T) {
	path := os.Getenv(lockHelper)
	if path == "" {
		t.Skip("run by TestLockFileKilled")
	}
	if _, err := LockFileShared(t.Context(), path, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stdout.WriteString("held\n"); err != nil { //nolint:forbidigo // the helper process tells its parent
		t.Fatal(err)
	}
	time.Sleep(time.Minute)
}

// A process killed while holding a shared lock lets go of it.
func TestLockFileKilled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestLockHolder$") //nolint:gosec // the test binary itself
	cmd.Env = append(os.Environ(), lockHelper+"="+path)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || line != "held\n" {
		t.Fatalf("helper: %q, %v", line, err)
	}
	if _, ok, _ := TryLockFile(path); ok {
		t.Error("got while the helper held it")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	unlock, ok, err := TryLockFile(path)
	if !ok || err != nil {
		t.Fatalf("once the helper was killed: ok %v, %v", ok, err)
	}
	unlock()
}
