//go:build linux

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// protocol is what check prints: devvm runs a helper whose protocol it
// knows, so a later one can change its commands.
const protocol = "1"

// immutableFl is FS_IMMUTABLE_FL, which golang.org/x/sys lacks.
const immutableFl = 0x00000010

// A walk's bounds: devvm's paths are a .git's hooks folders and configs,
// so a deeper or larger folder is refused rather than walked.
const (
	maxDepth   = 32
	maxEntries = 10000  // in one folder
	maxTotal   = 100000 // files and folders in one run
)

const usage = "usage: devvm-immutable check | set PATH... | clear PATH..."

// The reasons a path is refused.
var (
	errPath     = errors.New("not an absolute, clean path")
	errSymlink  = errors.New("a symlink")
	errUnder    = errors.New("under a symlink or a file, not only folders")
	errType     = errors.New("not a regular file or a folder")
	errNotYours = errors.New("owned by another user")
	errChanged  = errors.New("changed while it was opened")
	errOtherFS  = errors.New("on another file system than the folder above it")
	errTooDeep  = fmt.Errorf("more than %d folders deep", maxDepth)
	errTooMany  = fmt.Errorf("a folder with more than %d entries", maxEntries)
	errTooMuch  = fmt.Errorf("past the %d files and folders of one run", maxTotal)
	errLinked   = errors.New("a file with other names (hard links), which a lock would lock too")
	errNoFlag   = errors.New("its file system has no immutable attribute")
)

// run is devvm-immutable with its arguments: its exit status, 0 when it
// did all it was asked, 1 when it couldn't, 2 for a usage error.
func run(args []string, stdout, stderr io.Writer) int {
	if err := limitCaps(); err != nil {
		_, _ = fmt.Fprintln(stderr, "devvm-immutable: giving up its other capabilities failed:", err)
		return 1
	}
	switch {
	case len(args) == 1 && args[0] == "check":
		if !holdsCap() {
			_, _ = fmt.Fprintln(stderr, "devvm-immutable: it doesn't hold CAP_LINUX_IMMUTABLE")
			return 1
		}
		_, _ = fmt.Fprintln(stdout, protocol)
		return 0
	case len(args) >= 2 && (args[0] == "set" || args[0] == "clear"):
		w := walker{uid: uint32(os.Getuid()), on: args[0] == "set", stderr: stderr} //nolint:gosec // a uid is never negative
		for _, p := range args[1:] {
			w.top(p)
		}
		if w.failed {
			return 1
		}
		return 0
	}
	_, _ = fmt.Fprintln(stderr, usage)
	return 2
}

// capData is a process's capability sets, as capget and capset take them
// (version 3: two words of each set).
type capData [2]unix.CapUserData

// kept is the capabilities it keeps of those it has: CAP_LINUX_IMMUTABLE,
// when it's permitted, in effect; nothing else, nothing inheritable.
func kept(have capData) capData {
	keep := have[0].Permitted & (1 << unix.CAP_LINUX_IMMUTABLE)
	return capData{{Effective: keep, Permitted: keep}}
}

// limitCaps gives up every capability but CAP_LINUX_IMMUTABLE, on every
// thread of the process, and checks that it did. The file capability
// gives only that one, so this changes something only when root runs it.
func limitCaps() error {
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var have capData
	if err := unix.Capget(&hdr, &have[0]); err != nil {
		return err
	}
	want := kept(have)
	if have == want {
		return nil
	}
	// capset changes only the calling thread's, and the Go runtime has
	// others.
	if _, _, e := syscall.AllThreadsSyscall(unix.SYS_CAPSET, uintptr(unsafe.Pointer(&hdr)), uintptr(unsafe.Pointer(&want[0])), 0); e != 0 { //nolint:gosec // capset's own structures
		return e
	}
	if err := unix.Capget(&hdr, &have[0]); err != nil {
		return err
	}
	if have != want {
		return errors.New("it still holds others")
	}
	return nil
}

// holdsCap reports whether CAP_LINUX_IMMUTABLE is in effect: not when the
// file has no capability, its file system is mounted nosuid, or devvm
// runs with no_new_privs.
func holdsCap() bool {
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var have capData
	return unix.Capget(&hdr, &have[0]) == nil && have[0].Effective&(1<<unix.CAP_LINUX_IMMUTABLE) != 0
}

// walker sets or clears the immutable attribute on the paths it's given
// and everything under them, as uid.
type walker struct {
	uid    uint32
	on     bool // set, rather than clear
	stderr io.Writer
	failed bool
	seen   int // files and folders opened, at most maxTotal
}

func (w *walker) fail(path string, err error) {
	w.failed = true
	_, _ = fmt.Fprintf(w.stderr, "devvm-immutable: %q: %v\n", path, err)
}

// top acts on the absolute path p: its folder is reached from / one
// folder at a time, never through a symlink.
func (w *walker) top(p string) {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p || p == "/" {
		w.fail(p, errPath)
		return
	}
	dir, name := filepath.Split(p)
	dfd, err := openDir(dir)
	if err != nil {
		w.fail(p, err)
		return
	}
	defer closeFd(dfd)
	w.entry(dfd, p, name, 0, nil)
}

// entry acts on name in the folder dfd, whose path is path, and, when
// it's a folder, on what's in it. dev is the file system of the folder
// above, nil for the path given.
func (w *walker) entry(dfd int, path, name string, depth int, dev *uint64) {
	fd, st, err := openEntry(dfd, name, w.uid)
	if err != nil {
		w.fail(path, err)
		return
	}
	defer closeFd(fd)
	switch w.seen++; {
	case dev != nil && st.Dev != *dev:
		w.fail(path, errOtherFS)
		return
	case w.seen > maxTotal:
		w.fail(path, errTooMuch)
		return
	case w.on && st.Mode&unix.S_IFMT == unix.S_IFREG && st.Nlink != 1:
		// Its other names may be outside the folder: a lock on it would
		// lock them too.
		w.fail(path, errLinked)
		return
	}
	if err := setFlag(fd, w.on); err != nil {
		w.fail(path, err)
		return
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return
	}
	if depth >= maxDepth {
		w.fail(path, errTooDeep)
		return
	}
	names, err := list(fd)
	if err != nil {
		w.fail(path, err)
		return
	}
	for _, n := range names {
		child := filepath.Join(path, n)
		var cst unix.Stat_t
		if err := unix.Fstatat(fd, n, &cst, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			w.fail(child, err)
			continue
		}
		if cst.Mode&unix.S_IFMT == unix.S_IFLNK {
			continue // no attribute of its own, and never followed
		}
		w.entry(fd, child, n, depth+1, &st.Dev)
	}
}

// openDir opens the folder dir, an absolute path, as an O_PATH file,
// walking from / one folder at a time, so a symlink anywhere in it fails.
func openDir(dir string) (int, error) {
	fd, err := unix.Open("/", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	for c := range strings.SplitSeq(strings.Trim(dir, "/"), "/") {
		if c == "" {
			continue
		}
		next, err := unix.Openat(fd, c, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		closeFd(fd)
		if err != nil {
			if errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
				return -1, errUnder
			}
			return -1, err
		}
		fd = next
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFDIR {
		closeFd(fd)
		return -1, errUnder
	}
	return fd, nil
}

// openEntry opens name in the folder dfd, after checking what it is
// (checkStat), without following a symlink or waiting on a FIFO, then
// checks that what it opened is what it checked.
func openEntry(dfd int, name string, uid uint32) (int, unix.Stat_t, error) {
	var st unix.Stat_t
	if err := unix.Fstatat(dfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return -1, st, err
	}
	if err := checkStat(st, uid); err != nil {
		return -1, st, err
	}
	flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_NOCTTY | unix.O_CLOEXEC
	if st.Mode&unix.S_IFMT == unix.S_IFDIR {
		flags |= unix.O_DIRECTORY
	}
	fd, err := unix.Openat(dfd, name, flags, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			return -1, st, errChanged
		}
		return -1, st, err
	}
	var got unix.Stat_t
	if err := unix.Fstat(fd, &got); err != nil {
		closeFd(fd)
		return -1, st, err
	}
	if got.Dev != st.Dev || got.Ino != st.Ino || checkStat(got, uid) != nil {
		closeFd(fd)
		return -1, st, errChanged
	}
	return fd, got, nil
}

// checkStat refuses what isn't a regular file or a folder that uid owns.
func checkStat(st unix.Stat_t, uid uint32) error {
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFREG, unix.S_IFDIR:
	case unix.S_IFLNK:
		return errSymlink
	default:
		return errType
	}
	if st.Uid != uid {
		return errNotYours
	}
	return nil
}

// setFlag sets or clears the immutable attribute of the open file fd,
// keeping its other attributes as they are, and does nothing when it's
// already so.
func setFlag(fd int, on bool) error {
	flags, err := unix.IoctlGetUint32(fd, unix.FS_IOC_GETFLAGS)
	if err != nil {
		if errors.Is(err, unix.ENOTTY) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EINVAL) {
			return errNoFlag
		}
		return err
	}
	want := flags &^ immutableFl
	if on {
		want |= immutableFl
	}
	if want == flags {
		return nil
	}
	return unix.IoctlSetPointerInt(fd, unix.FS_IOC_SETFLAGS, int(want))
}

// list returns the names in the open folder fd, at most maxEntries.
func list(fd int) ([]string, error) {
	d, err := unix.FcntlInt(uintptr(fd), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(d), "")
	defer func() { _ = f.Close() }()
	names, err := f.Readdirnames(maxEntries + 1)
	switch {
	case errors.Is(err, io.EOF):
		return nil, nil
	case err != nil:
		return nil, err
	case len(names) > maxEntries:
		return nil, errTooMany
	}
	return names, nil
}

func closeFd(fd int) { _ = unix.Close(fd) }
