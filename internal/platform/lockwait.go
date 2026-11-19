package platform

import "sync/atomic"

// Waiting is what a wait for a lock shows (LockFile, LockFileShared,
// LockDir): it's called once, when another process holds the lock, never
// when it's free, and the func it returns once the wait ends, with the
// lock got or not. Each caller with a terminal passes a status line of
// its own, a label that says what it waits for (msg.Waiting), so a
// terminal never looks hung while devvm waits for another devvm. A
// process with no terminal passes a Waiting all the same, whose status
// line draws nothing. NoWait shows nothing, only for a lock that never
// waits, such as one on a file just made, and the structure test's
// status-lines rule holds it, and nil, to its list; a test build panics
// on nil (CheckWaiting).
type Waiting func() (end func())

// NoWait is the Waiting of a lock that never waits, such as one on a file
// just made, which no other process has: it shows nothing.
func NoWait() (end func()) { return func() {} }

// waitingChecked is whether a lock taken with a nil Waiting panics
// (CheckWaiting).
var waitingChecked atomic.Bool

// CheckWaiting makes every lock this process takes from then on panic on
// a nil Waiting, which would show nothing while another devvm holds the
// lock: test builds' runs do (internal/cli's Overrides), so the CLI tests
// and the end-to-end tests catch one that the structure test's
// status-lines rule can't see, such as a zero agentlogin.File's.
func CheckWaiting() { waitingChecked.Store(true) }

// checkWaiting panics on a nil waiting once CheckWaiting was called.
func checkWaiting(waiting Waiting) {
	if waiting == nil && waitingChecked.Load() {
		panic("platform: a lock's Waiting is nil, so its wait would show nothing: pass msg.Waiting's, or NoWait for a lock that never waits")
	}
}
