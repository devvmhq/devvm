package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/devvmhq/devvm/internal/safefile"
)

// LogLimit is how large the event log and the proxy's own log grow
// before they're rotated: the log becomes <log>.1, replacing the one
// there, and a new one starts. One old file each is kept.
const LogLimit = 10 << 20

// rotating is a log file rotated once it's past its limit. Each Write is
// one whole line (record's and logger's are), and a line never spans two
// files: the log rotates before the Write that would take it past the
// limit. Writes are serialized, so none is lost or split by a rotation.
//
// The event log (Config.Log) has two writers, the proxy's child and
// devvm _links (OpenEventLog), each in its own process: it's renamed to
// <log>.1 and a new one is opened, which devvm network log --follow
// follows by name. Before each line a writer checks that its open file
// is still the one at the path, and opens the path again when it isn't
// (the other writer rotated it, or it was deleted), and it takes the
// log's size from the file. A rotation holds the file's lock and checks
// again, so two writers past the limit at once rename it only once.
//
// The proxy's own log (Config.ProxyLog) is the broker's and the child's
// stdout and stderr, which devvm opened, where their last errors and any
// crash go: both write it, and either may rotate it, through the
// descriptor it was given, never by its path (copyRotating). Its size is
// the file's before each line, and a rotation holds the file's lock and
// checks the size again, so the two rotate it once. What reached it
// otherwise, an error a library printed, counts too. The proxy's
// http.Servers log their errors through it (ErrorLog).
//
// The child can write the folder these logs are in, so a writer opens a
// log by its path never through a symbolic link at its end, and only a
// regular file with one link that's this user's (openLog). devvm's other
// logs that a VM can drive are internal/vmlog's, which this package, the
// standard library only, doesn't import.
type rotating struct {
	mu    sync.Mutex
	path  string
	limit int64
	// w is where lines go: the file itself (renamed logs), or the
	// stderr the proxy was given, which appends to fd (copied logs).
	w    io.Writer
	f    *os.File // the event log's open file; nil for a copied log
	fd   *os.File // the copied log's descriptor, which w appends to
	lock *os.File // the copied log, opened again, which its writers lock (lockOf); nil: fd
	size int64
}

// OpenEventLog opens the event log at path for appending, the user's
// alone, rotated past LogLimit as the proxy rotates it.
func OpenEventLog(path string) (io.WriteCloser, error) { return openRotating(path, LogLimit) }

// openRotating opens the event log at path for appending, the user's
// alone, whatever mode it had.
func openRotating(path string, limit int64) (*rotating, error) {
	r := &rotating{path: path, limit: limit}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

// copyRotating rotates path, whose open file is fd, which w appends to
// (the proxy's stderr), by copying, through fd.
func copyRotating(fd *os.File, path string, w io.Writer, limit int64) (*rotating, error) {
	if err := fd.Chmod(0o600); err != nil {
		return nil, err
	}
	return &rotating{path: path, limit: limit, w: w, fd: fd, lock: lockOf(fd, path)}, nil
}

// lockOf is a file description of its own of fd's file, at path, which the copied log's writers
// lock: the broker and its child share fd's description, which a lock can't tell apart, so each
// opens the file again, never through a link, and only when it's fd's file. nil when it can't,
// and fd itself is locked instead.
func lockOf(fd *os.File, path string) *os.File {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // the proxy's own log, checked to be the file it writes
	if err != nil {
		return nil
	}
	a, err1 := f.Stat()
	b, err2 := fd.Stat()
	if err1 != nil || err2 != nil || !os.SameFile(a, b) {
		_ = f.Close()
		return nil
	}
	return f
}

// lockFD is the descriptor the copied log's writers lock.
func (r *rotating) lockFD() int {
	if r.lock != nil {
		return int(r.lock.Fd())
	}
	return int(r.fd.Fd())
}

// OpenLog opens the log at path to append to, creating it, the user's
// alone: never through a symbolic link at its end, and only a regular
// file with one link that's this user's, as a VM's proxy's child can
// write the folder a VM's logs are in (safefile.Opts.OneLink,
// ErrNotPrivate). What a child left in a log's place, a link, a FIFO or
// another file's hard link, goes, its name alone, never what it leads to,
// and a new log starts there. It opens it to read too, so a rotation of a
// copied log reads it through its descriptor.
func OpenLog(path string) (*os.File, error) {
	f, err := openLog(path, 0)
	if errors.Is(err, safefile.ErrNotRegular) || errors.Is(err, safefile.ErrLinked) {
		if os.Remove(path) != nil {
			return nil, err
		}
		// Made new, or nothing: a link put back since isn't followed.
		return openLog(path, os.O_EXCL)
	}
	return f, err
}

// openLog is OpenLog's open of path, with flags besides its own.
func openLog(path string, flags int) (*os.File, error) {
	f, err := os.OpenFile(path, flags|os.O_RDWR|os.O_APPEND|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600) //nolint:gosec // a log of devvm's own, checked once open
	if errors.Is(err, syscall.ELOOP) {
		return nil, fmt.Errorf("%s: %w", path, safefile.ErrNotRegular)
	}
	if err != nil {
		return nil, err
	}
	sf, err := safefile.Checked(f, safefile.Opts{OneLink: true})
	if err != nil {
		return nil, err
	}
	if !ownedByMe(sf.Info) {
		_ = f.Close()
		return nil, fmt.Errorf("%s: %w", path, ErrNotPrivate)
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func (r *rotating) open() error {
	f, err := OpenLog(r.path)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	r.f, r.w, r.size = f, f, fi.Size()
	return nil
}

func (r *rotating) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// A failed check keeps writing to the open file: a line kept beats a
	// line in the right file.
	if r.f != nil {
		_ = r.follow()
	} else if fi, err := r.fd.Stat(); err == nil {
		// A copied log is also the process's stderr: what reached it
		// there, an error a library printed, counts too.
		r.size = fi.Size()
	}
	if r.size > 0 && r.size+int64(len(p)) > r.limit {
		// A failed rotation keeps writing to the log as it is: a line
		// kept beats a log of the right size.
		_ = r.rotate(int64(len(p)))
	}
	if r.fd != nil {
		// The other writer's rotation holds the lock: a line waits for it,
		// rather than land between its copy and its emptying, but only for
		// logLockWait, as a child that a VM took over could hold it for
		// ever: past that, the line goes in without it.
		fd := r.lockFD()
		if err := flockWithin(fd, syscall.LOCK_SH, logLockWait); err == nil {
			defer func() { _ = syscall.Flock(fd, syscall.LOCK_UN) }()
		}
	}
	n, err := r.w.Write(p)
	r.size += int64(n)
	return n, err
}

// follow makes the event log's open file the one at its path, opening
// the path again when the other writer rotated it or it was deleted, and
// takes the log's size from the file.
func (r *rotating) follow() error {
	if !r.current() {
		return r.reopen()
	}
	fi, err := r.f.Stat()
	if err != nil {
		return err
	}
	r.size = fi.Size()
	return nil
}

// current reports whether the open file is still the one at the path.
func (r *rotating) current() bool {
	fi, err := r.f.Stat()
	if err != nil {
		return false
	}
	pi, err := os.Lstat(r.path)
	return err == nil && os.SameFile(fi, pi)
}

// reopen opens the path, closing the old file once the new one is open.
func (r *rotating) reopen() error {
	old := r.f
	if err := r.open(); err != nil {
		// The old file still takes the lines.
		return err
	}
	_ = old.Close()
	return nil
}

// rotate makes the log <log>.1 and starts a new one, before a line of n
// bytes.
func (r *rotating) rotate(n int64) error {
	if r.f == nil {
		return r.copyOut(n)
	}
	// The other writer may be rotating the same file: the lock lets one
	// rename it, and the other, finding it renamed, opens the new one. A
	// lock held past logLockWait skips the rotation.
	fd := int(r.f.Fd())
	if err := flockWithin(fd, syscall.LOCK_EX, logLockWait); err != nil {
		return err
	}
	var err error
	if r.current() {
		err = os.Rename(r.path, r.path+".1")
	}
	_ = syscall.Flock(fd, syscall.LOCK_UN)
	if err != nil {
		return err
	}
	return r.reopen()
}

// copyOut copies the log to <log>.1, at most its last twice limit bytes
// from a line's start (copyFrom), the user's alone and written whole,
// streamed rather than read into memory, then empties the log, all
// through its descriptor, holding its lock, before a line of n bytes: the
// other writer, waiting for it, finds the log emptied and leaves it. A
// child that a VM took over can make the log as long as it likes, a
// sparse file of terabytes among them, so no more than that is ever read
// or written. A lock held past logLockWait skips the rotation.
func (r *rotating) copyOut(n int64) error {
	fd := r.lockFD()
	if err := flockWithin(fd, syscall.LOCK_EX, logLockWait); err != nil {
		return err
	}
	defer func() { _ = syscall.Flock(fd, syscall.LOCK_UN) }()
	fi, err := r.fd.Stat()
	if err != nil {
		return err
	}
	if r.size = fi.Size(); r.size == 0 || r.size+n <= r.limit {
		return nil // the other writer rotated it while this one waited
	}
	from := copyFrom(r.fd, fi.Size(), 2*r.limit)
	if err := copyAtomic(r.path+".1", io.NewSectionReader(r.fd, from, fi.Size()-from), 0o600); err != nil {
		return err
	}
	if err := r.fd.Truncate(0); err != nil {
		return err
	}
	r.size = 0
	return nil
}

// copyFrom is where a copied log's copy starts, in f, size bytes long: its start when it's at
// most keep long, as a log just past its limit is; else at the first line that starts in its last
// keep bytes (one long line's middle when none does, looking at most 64 KiB further), so a log
// that a child made huge, or sparse, never has more than keep read or written.
func copyFrom(f *os.File, size, keep int64) int64 {
	if size <= keep {
		return 0
	}
	from := size - keep
	buf := make([]byte, min(64<<10, keep))
	n, _ := f.ReadAt(buf, from-1)
	if i := bytes.IndexByte(buf[:n], '\n'); i >= 0 && from+int64(i) < size {
		from += int64(i)
	}
	return from
}

// logLockWait is how long a log's writer waits for its lock: a VM's proxy's child, which writes
// the same logs, could hold it for ever, and a line written without the lock, or a rotation
// skipped, beats a writer that never writes again.
const logLockWait = 200 * time.Millisecond

// errLocked is flockWithin's error past its wait.
var errLocked = errors.New("another process holds the lock")

// flockWithin takes the lock how (syscall.LOCK_SH or LOCK_EX) on fd, trying without blocking
// until wait has passed (errLocked), as no process that a VM can take over may make another
// wait on a lock for ever: the logs' and the Nix cache's.
func flockWithin(fd, how int, wait time.Duration) error {
	deadline, pause := time.Now().Add(wait), 5*time.Millisecond
	for {
		err := syscall.Flock(fd, how|syscall.LOCK_NB)
		switch {
		case err == nil:
			return nil
		case !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR):
			return err
		case !time.Now().Before(deadline):
			return errLocked
		}
		time.Sleep(min(pause, time.Until(deadline)))
		pause = min(2*pause, 100*time.Millisecond)
	}
}

// Close closes the event log's file.
func (r *rotating) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lock != nil {
		_ = r.lock.Close()
	}
	if r.f == nil {
		return nil
	}
	return r.f.Close()
}
