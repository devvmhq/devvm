package proxy

import (
	"io"
	"os"
	"sync"
	"syscall"
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
// The event log (Config.Log) has two writers, the proxy and devvm _links
// (OpenEventLog), each in its own process: it's renamed to <log>.1 and a
// new one is opened, which devvm network log --follow follows by name.
// Before each line a writer checks that its open file is still the one
// at the path, and opens the path again when it isn't (the other writer
// rotated it, or it was deleted), and it takes the log's size from the
// file. A rotation holds the file's lock and checks again, so two
// writers past the limit at once rename it only once. The proxy's own
// log (Config.ProxyLog) is also the proxy process's stdout and stderr, which
// devvm opened, where its last error and any crash go: that file is
// copied to <log>.1 and emptied instead, so those writes, appended to
// the same file, reach the current log, and its size is taken from the
// file before each line, so they count too. The proxy's http.Servers
// log their errors through it (ErrorLog). devvm's other logs that a VM
// can drive are internal/vmlog's, which this package, the standard
// library only, doesn't import.
type rotating struct {
	mu    sync.Mutex
	path  string
	limit int64
	// w is where lines go: the file itself (renamed logs), or the
	// stderr the proxy was given, which appends to path (copied logs).
	w    io.Writer
	f    *os.File // the event log's open file; nil for a copied log
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

// copyRotating rotates path, which w appends to (the proxy's stderr), by
// copying.
func copyRotating(path string, w io.Writer, limit int64) (*rotating, error) {
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	return &rotating{path: path, limit: limit, w: w, size: fi.Size()}, nil
}

func (r *rotating) open() error {
	f, err := os.OpenFile(r.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err == nil {
		err = f.Chmod(0o600)
	}
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
	if r.f != nil {
		// A failed check keeps writing to the open file: a line kept
		// beats a line in the right file.
		_ = r.follow()
	} else if fi, err := os.Stat(r.path); err == nil {
		// A copied log is also the process's stderr: what reached it
		// there, an error a library printed, counts too.
		r.size = fi.Size()
	}
	if r.size > 0 && r.size+int64(len(p)) > r.limit {
		// A failed rotation keeps writing to the log as it is: a line
		// kept beats a log of the right size.
		_ = r.rotate()
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
	pi, err := os.Stat(r.path)
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

// rotate makes the log <log>.1 and starts a new one.
func (r *rotating) rotate() error {
	if r.f == nil {
		return r.copyOut()
	}
	// The other writer may be rotating the same file: the lock lets one
	// rename it, and the other, finding it renamed, opens the new one.
	fd := int(r.f.Fd())
	if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
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

// copyOut copies the log to <log>.1, the user's alone and written whole,
// then empties it.
func (r *rotating) copyOut() error {
	data, err := os.ReadFile(r.path)
	if err != nil {
		return err
	}
	if err := writeAtomic(r.path+".1", data, 0o600); err != nil {
		return err
	}
	if err := os.Truncate(r.path, 0); err != nil {
		return err
	}
	r.size = 0
	return nil
}

// Close closes the event log's file.
func (r *rotating) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	return r.f.Close()
}
