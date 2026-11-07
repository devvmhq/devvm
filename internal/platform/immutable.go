package platform

import "errors"

// ImmutableHelper is where devvm-immutable is installed on Linux, root's,
// with the file capability CAP_LINUX_IMMUTABLE: the program that sets and
// clears Linux's immutable attribute on the user's own files, which needs
// that capability.
const ImmutableHelper = "/usr/local/libexec/devvm/devvm-immutable"

// Locker locks files with this host's immutable flag, which nothing in a
// VM can clear: macOS's user-immutable flag (uchg), which the user sets
// without root, or Linux's immutable attribute, through devvm-immutable.
// Its zero value has no helper, so it can't lock on Linux.
type Locker struct {
	// Helper is devvm-immutable's path, on Linux (ImmutableHelper); ""
	// for none.
	Helper string
}

// Why a Locker can't lock files (Usable).
var (
	// ErrNoHelper: devvm-immutable isn't installed.
	ErrNoHelper = errors.New("devvm-immutable isn't installed")
	// ErrHelperUnusable: it's there, but it can't set the attribute, as
	// it doesn't hold its capability, or it speaks another protocol.
	ErrHelperUnusable = errors.New("devvm-immutable doesn't work")
	// ErrNoAttribute: the folder's file system has no immutable
	// attribute (NFS, FUSE).
	ErrNoAttribute = errors.New("the file system has no immutable attribute")
)
