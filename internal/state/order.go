package state

import (
	"bytes"
	"flag"
	"fmt"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
)

// The state's locks are taken in one order, so no two devvms wait on each
// other: a VM's lock (VM.locked, RemoveFolder) first, then the claims
// lock (claimed), and at most one VM's lock at a time, as two devvms each
// holding one VM's lock and waiting for the other's would wait for ever.
// A lock is a file lock (flock), so a process that takes one it already
// holds through another descriptor waits for itself. Test builds
// (CheckLockOrder) and the tests check the order (held) as each lock is
// taken, and panic, naming both, on a lock taken out of it.

// A lockKind is which of the state's locks one is.
type lockKind int

const (
	vmLock     lockKind = iota // a VM's own (paths.VM.Lock)
	claimsLock                 // the claims lock (paths.Dirs.ClaimsLock)
)

// lockOrder is the locks each goroutine holds, by its goroutine number,
// once on says to check them.
type lockOrder struct {
	on   atomic.Bool
	mu   sync.Mutex
	held map[int64][]heldLock
}

// A heldLock is one lock a goroutine holds: its kind and its file.
type heldLock struct {
	kind lockKind
	path string
}

// held is this process's lockOrder: the locks are the process's.
var held lockOrder

// CheckLockOrder makes this process check the order its state locks are
// taken in, from then on: test builds' runs do (internal/cli's
// Overrides), and tests' processes always.
func CheckLockOrder() { held.on.Store(true) }

// take checks that the goroutine may take the lock at path, of kind, in
// the order, panicking when it may not, and returns the release that
// unlock calls with it; unless the process checks the order, it checks
// nothing.
func (o *lockOrder) take(kind lockKind, path string) (release func()) {
	if !o.on.Load() && !testProcess() {
		return func() {}
	}
	g := goroutine()
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := inOrder(o.held[g], kind, path); err != nil {
		panic(err)
	}
	if o.held == nil {
		o.held = map[int64][]heldLock{}
	}
	o.held[g] = append(o.held[g], heldLock{kind, path})
	return func() {
		o.mu.Lock()
		defer o.mu.Unlock()
		hs := o.held[g]
		for i := len(hs) - 1; i >= 0; i-- {
			if hs[i].path == path {
				hs = append(hs[:i], hs[i+1:]...)
				break
			}
		}
		if len(hs) == 0 {
			delete(o.held, g)
		} else {
			o.held[g] = hs
		}
	}
}

// inOrder is nil when a goroutine holding has may take the lock at path,
// of kind, next, or why not.
func inOrder(has []heldLock, kind lockKind, path string) error {
	for _, h := range has {
		switch {
		case h.path == path:
			return fmt.Errorf("state lock order: %s taken again, which waits for itself", path)
		case h.kind == claimsLock:
			return fmt.Errorf("state lock order: %s taken holding the claims lock %s, which comes after a VM's", path, h.path)
		case kind == vmLock && h.kind == vmLock:
			return fmt.Errorf("state lock order: %s taken holding another VM's lock, %s: one VM's lock at a time", path, h.path)
		}
	}
	return nil
}

// testProcess is whether this process is a test binary (go test's),
// whose testing package has registered its flags before any test runs.
func testProcess() bool { return flag.Lookup("test.v") != nil }

// goroutine is the calling goroutine's number, from its stack's first
// line ("goroutine 7 [running]:"): test builds' only, for the order's
// check.
func goroutine() int64 {
	var buf [64]byte
	b := buf[:runtime.Stack(buf[:], false)]
	b = bytes.TrimPrefix(b, []byte("goroutine "))
	if i := bytes.IndexByte(b, ' '); i >= 0 {
		b = b[:i]
	}
	n, _ := strconv.ParseInt(string(b), 10, 64) // runtime.Stack's own format
	return n
}
