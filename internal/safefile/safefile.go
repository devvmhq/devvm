// Package safefile reads files a VM can write, or swap: a project folder's, its .git's, a policy
// file, an agent's login. Each read opens its file without blocking, and without following a
// symbolic link at its end unless the caller asks (Follow), then checks the file it opened, not
// the name: a regular file (never a FIFO, a device or a folder: ErrNotRegular), with one hard link
// where asked (ErrLinked), at most its cap (ErrTooLong). So a FIFO swapped in after a check can't
// hold its reader, nor a link to /dev/zero flood it. A Root opens beneath a folder, never through
// a link out of it. Standard library only, so the proxy reads with it too.
package safefile

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

var (
	// ErrNotRegular is a read's error for a path that isn't a regular file (a FIFO, a device, a
	// folder), or, without Follow, a symbolic link.
	ErrNotRegular = errors.New("not a regular file")
	// ErrLinked is a read's error, with OneLink, for a file with more than one hard link.
	ErrLinked = errors.New("more than one hard link")
	// ErrTooLong is a read's error for a file past its cap.
	ErrTooLong = errors.New("too long")
)

// Opts says what a read accepts besides a regular file that isn't a symbolic link.
type Opts struct {
	// Follow follows a symbolic link at the path's end: beneath a Root, only to a file in it.
	Follow bool
	// OneLink refuses a file with another hard link, which a link in another folder could change.
	OneLink bool
}

// flags are the open flags for o.
func (o Opts) flags() int {
	f := os.O_RDONLY | noBlock
	if !o.Follow {
		f |= noFollow
	}
	return f
}

// File is a regular file Open opened, and what fstat said of it then.
type File struct {
	*os.File
	Info fs.FileInfo
}

// Open opens the regular file at path, as o says. Its caller closes it, and may read it as it
// likes: in full with ReadAll, or a part of it.
func Open(path string, o Opts) (*File, error) {
	f, err := os.OpenFile(path, o.flags(), 0) //nolint:gosec // the caller names the file; what it opened is checked
	return checked(path, f, err, o)
}

// Read reads the regular file at path, as o says, when it's at most limit bytes long.
func Read(path string, limit int64, o Opts) ([]byte, error) {
	f, err := Open(path, o)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return f.ReadAll(limit)
}

// Checked checks f, a file its caller opened another way, as Open checks its own: such as
// devvm paste's, opened through no link (platform.OpenNoLinks) with a call this package can't
// make on the standard library alone. Anything that isn't a regular file is ErrNotRegular, and
// with OneLink a file with another hard link is ErrLinked; Follow isn't Checked's, as the open
// was the caller's. A file it refuses is closed; one it takes is the caller's to close.
func Checked(f *os.File, o Opts) (*File, error) { return checked(f.Name(), f, nil, o) }

// checked is what an open of path gave, f or err, checked as o says: a link that isn't followed
// is ErrNotRegular, as is anything that isn't a regular file; with OneLink, a file with another
// hard link is ErrLinked. A file it refuses is closed.
func checked(path string, f *os.File, err error, o Opts) (*File, error) {
	if err != nil {
		if isLoop(err) {
			return nil, fmt.Errorf("%s: %w", path, ErrNotRegular)
		}
		return nil, err
	}
	fi, err := f.Stat()
	switch {
	case err != nil:
	case !fi.Mode().IsRegular():
		err = fmt.Errorf("%s: %w", path, ErrNotRegular)
	case o.OneLink && links(fi) > 1:
		err = fmt.Errorf("%s: %w", path, ErrLinked)
	default:
		return &File{File: f, Info: fi}, nil
	}
	_ = f.Close()
	return nil, err
}

// ReadAll reads f to its end, when it's at most limit bytes long, as it was opened and as it's
// read (it may grow): ErrTooLong past it.
func (f *File) ReadAll(limit int64) ([]byte, error) {
	if f.Info.Size() > limit {
		return nil, fmt.Errorf("%s: %w", f.Name(), ErrTooLong)
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s: %w", f.Name(), ErrTooLong)
	}
	return b, nil
}

// Root is a folder whose files are opened beneath it (os.Root's): no name, and no symbolic link
// on the way, leads out of it.
type Root struct{ root *os.Root }

// OpenRoot opens the folder dir as a Root, which its caller closes.
func OpenRoot(dir string) (*Root, error) {
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &Root{root: r}, nil
}

// Close closes the root's folder.
func (r *Root) Close() error { return r.root.Close() }

// Open opens the regular file at name in the root, as o says (Open's). os.Root follows a link at
// the end whatever the flags, so without Follow, name is looked at first (Lstat): a link there is
// refused, and the file opened must be the one that was there (ErrNotRegular when it changed).
func (r *Root) Open(name string, o Opts) (*File, error) {
	path := filepath.Join(r.root.Name(), name)
	var was fs.FileInfo
	if !o.Follow {
		fi, err := r.root.Lstat(name)
		if err != nil {
			return nil, err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return nil, fmt.Errorf("%s: %w", path, ErrNotRegular)
		}
		was = fi
	}
	f, err := r.root.OpenFile(name, o.flags(), 0)
	file, err := checked(path, f, err, o)
	if err == nil && was != nil && !os.SameFile(was, file.Info) {
		_ = file.Close()
		return nil, fmt.Errorf("%s: %w", path, ErrNotRegular)
	}
	return file, err
}

// Read reads the regular file at name in the root, as o says, when it's at most limit bytes long.
func (r *Root) Read(name string, limit int64, o Opts) ([]byte, error) {
	f, err := r.Open(name, o)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return f.ReadAll(limit)
}

// Lstat is what's at name in the root, a link's own, never what it leads to.
func (r *Root) Lstat(name string) (fs.FileInfo, error) { return r.root.Lstat(name) }

// Readlink is the target of the symbolic link at name in the root, as the link has it, which
// may lead out of it: read, never followed.
func (r *Root) Readlink(name string) (string, error) { return r.root.Readlink(name) }

// ErrSwapped is OpenDir's error for a folder that changed as it was opened, such as one swapped
// for a link.
var ErrSwapped = errors.New("changed as it was opened")

// OpenDir opens the folder at name in the root as a Root of its own, which its caller closes:
// what's at name once it's open must be a folder, never a link, and the folder opened (ErrSwapped), so a walk that opens each folder from its parent's Root, and each file by its
// own name in its folder's, never goes through a link, even one swapped in as it walks.
func (r *Root) OpenDir(name string) (*Root, error) {
	// Opened first, then what's at name looked at, never followed: a link
	// there, then or as it opened, makes them differ.
	sub, err := r.root.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	opened, err1 := sub.Stat(".")
	there, err2 := r.root.Lstat(name)
	if err1 != nil || err2 != nil || !there.IsDir() || !os.SameFile(opened, there) {
		_ = sub.Close()
		return nil, fmt.Errorf("%s: %w", filepath.Join(r.root.Name(), name), ErrSwapped)
	}
	return &Root{root: sub}, nil
}

// ReadDir lists the root's own folder, sorted by name, each entry's type its own, never a
// link's target's.
func (r *Root) ReadDir() ([]fs.DirEntry, error) { return fs.ReadDir(r.root.FS(), ".") }

// Stat is what the root's own folder is, as it was opened.
func (r *Root) Stat() (fs.FileInfo, error) { return r.root.Stat(".") }

// FS is the root as an fs.FS: Open opens only regular files, following links in the root
// (Follow); Stat and ReadDir read what's there, opening no file.
func (r *Root) FS() fs.FS { return rootFS{r} }

type rootFS struct{ r *Root }

func (f rootFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	file, err := f.r.Open(filepath.FromSlash(name), Opts{Follow: true})
	if err != nil {
		return nil, err
	}
	return file.File, nil
}

func (f rootFS) Stat(name string) (fs.FileInfo, error) { return fs.Stat(f.r.root.FS(), name) }

func (f rootFS) ReadDir(name string) ([]fs.DirEntry, error) { return fs.ReadDir(f.r.root.FS(), name) }
