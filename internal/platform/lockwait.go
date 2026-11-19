package platform

// Waiting is what a wait for a lock shows (LockFile, LockFileShared,
// LockDir): it's
// called once, when another process holds the lock, never when it's free,
// and the func it returns once the wait ends, with the lock got or not.
// Each caller with a terminal passes a status line of its own, a label
// that says what it waits for (msg.Waiting), so a terminal never looks
// hung while devvm waits for another devvm; nil shows nothing, only for a
// lock that never waits, such as one on a file just made, which the
// structure test's status-lines rule holds to its list. A process with no
// terminal passes a Waiting all the same, whose status line draws nothing.
type Waiting func() (end func())
