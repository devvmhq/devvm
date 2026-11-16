package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/devvmhq/devvm/internal/safefile"
)

// watchTick is the coarsest mtime a host's filesystem keeps (2 s on some, 1 s on Linux's older
// ones): a file whose mtime is within it of now may change again without its stamp changing, so
// it's read again at each call until its mtime is older.
const watchTick = 2 * time.Second

// watched is a file the proxy reads again when it changes, its contents parsed into a T: the
// bindings, the policy files and the links list. Each call stats it (Lstat, or Stat with follow),
// and reads it when its stamp (mtime, size and inode) differs from the last read's, or the
// last read's mtime was within watchTick of then; it parses what it read only when the bytes
// differ. A read gives the stamp of the file it read, which the next call compares, so a write
// after the read changes it: the child looks at a file itself and has the broker read it
// (watchSource), which costs no hop while the file is unchanged. A file that's gone has none,
// silently. A file that can't be stat'd, read or parsed is logged once for each distinct error
// (onFail) and, with keep, keeps its last good T; without, it has none until it's read again.
type watched[T any] struct {
	path   string
	follow bool // Stat, for a file read through a symbolic link (ReadPolicy)
	keep   bool // on a failure, keep the last good T rather than none
	// read reads the file, as watchedFile.read does, in this process or the broker's.
	read   func() (fileRead, error)
	parse  func(b []byte) (T, error)
	onFail func(err error) // called once for each distinct failure in a row
	// changed, when set, runs with mu held whenever the file changes: its stamp or its bytes (a
	// touch counts), or its T is dropped. A lock it takes comes after mu.
	changed func()

	mu      sync.Mutex
	st      stamp
	settled bool   // st's mtime was older than watchTick when it was read
	raw     []byte // what the last good read gave; nil: none
	val     T
	failed  string // the last failure logged, until a read succeeds or the file goes
}

// stamp is what tells a file's versions apart without reading it.
type stamp struct {
	mtime time.Time
	size  int64
	ino   uint64
}

func stampOf(fi fs.FileInfo) stamp {
	s := stamp{mtime: fi.ModTime(), size: fi.Size()}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		s.ino = st.Ino
	}
	return s
}

// fileRead is a watched file as it was read: its bytes and the stamp of the file read, or missing
// when there was none.
type fileRead struct {
	data    []byte
	st      stamp
	missing bool
}

// get returns the file's T as it is now.
func (w *watched[T]) get() T {
	w.mu.Lock()
	defer w.mu.Unlock()
	stat := os.Lstat
	if w.follow {
		stat = os.Stat
	}
	fi, err := stat(w.path)
	if errors.Is(err, fs.ErrNotExist) {
		w.gone()
		return w.val
	}
	if err != nil {
		return w.fail(err)
	}
	if stampOf(fi) == w.st && w.settled {
		return w.val
	}
	got, err := w.read()
	if err != nil {
		return w.fail(err)
	}
	if got.missing {
		w.gone()
		return w.val
	}
	w.failed = ""
	touched := got.st != w.st
	w.st, w.settled = got.st, time.Since(got.st.mtime) >= watchTick
	if w.raw != nil && bytes.Equal(got.data, w.raw) {
		if touched && w.changed != nil { // the same bytes, written again or touched
			w.changed()
		}
		return w.val
	}
	v, err := w.parse(got.data)
	if err != nil {
		return w.fail(err)
	}
	w.raw, w.val = got.data, v
	if w.changed != nil {
		w.changed()
	}
	return w.val
}

// gone forgets a file that's no longer there, silently.
func (w *watched[T]) gone() {
	w.drop()
	w.failed = ""
}

// fail logs err unless it's the failure already logged, and returns the last good T with keep,
// or none. The stamp is forgotten, so the next call tries again.
func (w *watched[T]) fail(err error) T {
	if why := err.Error(); why != w.failed {
		w.failed = why
		w.onFail(err)
	}
	w.st = stamp{}
	if !w.keep {
		w.drop()
	}
	return w.val
}

// drop forgets the T, running changed if there was one.
func (w *watched[T]) drop() {
	w.st = stamp{}
	if w.raw == nil {
		return
	}
	var zero T
	w.raw, w.val = nil, zero
	if w.changed != nil {
		w.changed()
	}
}

// The kinds of watched file, each read as its file must be (watchedFile.read).
const (
	watchPolicy   = "policy"   // a policy file (ReadPolicy), through a symbolic link
	watchDeny     = "deny"     // a policy file whose denies alone apply (Config.DenyPolicies)
	watchBindings = "bindings" // the VM's bindings file (Config.Secrets)
	watchLinks    = "links"    // the VM's links' list (Config.Links)
)

// watchedFile is one of the files that change while the proxy runs: its kind and its path. The
// broker reads it, and the child looks at it alone (start's Files).
type watchedFile struct {
	kind string
	path string
}

// watchSource is a watched file as one process has it: its path, which it looks at, and how it's
// read, by this process (local) or by the broker (the child's).
type watchSource struct {
	path string
	read func() (fileRead, error)
}

// local is the file read by this process.
func (f watchedFile) local() watchSource { return watchSource{path: f.path, read: f.read} }

// localSources are paths, files of kind read by this process.
func localSources(kind string, paths []string) []watchSource {
	out := make([]watchSource, len(paths))
	for i, p := range paths {
		out[i] = watchedFile{kind, p}.local()
	}
	return out
}

// read reads the file as its kind must be read, and the stamp of the file it opened, taken before
// its read: a policy file through a symbolic link, at most PolicyMax (ReadPolicy); the bindings
// file, only its owner's to read or write, at most bindingListMax; the links' list, only its
// owner's to write, at most linkListMax (readPrivate). One that isn't there is missing.
func (f watchedFile) read() (fileRead, error) {
	var file *safefile.File
	var err error
	limit := int64(PolicyMax)
	switch f.kind {
	case watchPolicy, watchDeny:
		file, err = safefile.Open(f.path, safefile.Opts{Follow: true})
	case watchBindings:
		limit = bindingListMax
		file, err = openPrivate(f.path, 0o077)
	case watchLinks:
		limit = linkListMax
		file, err = openPrivate(f.path, 0o022)
	default:
		return fileRead{}, fmt.Errorf("a watched file of kind %q", f.kind)
	}
	if errors.Is(err, fs.ErrNotExist) {
		return fileRead{missing: true}, nil
	}
	if err != nil {
		return fileRead{}, err
	}
	defer func() { _ = file.Close() }()
	b, err := file.ReadAll(limit)
	if err != nil {
		return fileRead{}, err
	}
	return fileRead{data: b, st: stampOf(file.Info)}, nil
}
