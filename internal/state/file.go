// Package state reads and writes the files devvm keeps its state in: each
// VM's state (vm.json), image and tool image records, rebuild counters and
// the cached --from digests.
//
// All of them are JSON only devvm writes (json.go): fields a reader
// doesn't know are ignored. Every write replaces the whole file atomically
// (a temporary file, then a rename), so a reader never sees half a file
// and reads take no lock; a VM's state changes under its lock (VM.Update). Files are private to the user (0600, or
// 0700 for a program devvm runs: WriteFileMode), and so are the folders
// devvm creates for them (0700: PrivateDir). Every file devvm writes
// whole goes through WriteFile or WriteFileMode, but for those of the
// packages that are the standard library only: internal/proxy's
// writeAtomic, and internal/daemon's pid and status files. A log a
// process writes as it runs, and a download, stream into their files.
package state

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
)

// CorruptError is a state file devvm can't read: edited by hand, or damaged.
type CorruptError struct {
	Path   string
	Line   int // 1-based; 0 when the problem isn't on one line
	Reason string
	// Msg is the reason's message, after Reason, when a check gave one
	// (a value's: size_not_gib); the zero message when none did. It isn't
	// unwrapped, so a failure shows the file and line around it.
	Msg msg.M
}

func (e *CorruptError) Error() string { return e.Shown(msg.M.Text) }

// Shown is the error with its message as text shows it: the UI's Text,
// so names mode shows the message's names.
func (e *CorruptError) Shown(text func(msg.M) string) string {
	reason := e.Reason
	if e.Msg.ID() != "" {
		reason += text(e.Msg)
	}
	if e.Line == 0 {
		return fmt.Sprintf("%s: %s", e.Path, reason)
	}
	return fmt.Sprintf("%s:%d: %s", e.Path, e.Line, reason)
}

// MakePrivate makes devvm's data and config folders, and makes both the
// user's alone (mode 700) whatever mode they had, so nothing beneath them
// is open to others. Unlike PrivateDir, it follows a link there: where
// these two folders live is the user's choice, a dotfiles repo's say.
func MakePrivate(d paths.Dirs) error {
	for _, dir := range []string{d.Data, d.Config} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // a folder: owner-only with search
			return err
		}
	}
	return nil
}

// PrivateDir makes the folder path, with any folders above it that are
// missing, and makes it the user's alone (mode 700) whatever mode it had.
// Anything at path but a folder, a link to one included, is refused, so
// the mode is never set on what a link points to.
func PrivateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s isn't a folder", path)
	}
	if fi.Mode().Perm() != 0o700 {
		return os.Chmod(path, 0o700) //nolint:gosec // a folder: owner-only with search
	}
	return nil
}

// WriteFile replaces path with data atomically, readable only by the
// user, creating its folder (0700) when it's missing.
func WriteFile(path string, data []byte) error { return writeFile(path, data) }

// WriteFileMode replaces path with data atomically, as WriteFile does, with
// mode's permissions: 0o700 for a program devvm runs.
func WriteFileMode(path string, data []byte, mode os.FileMode) error {
	return writeFileMode(path, data, mode.Perm())
}

// WriteFileIn replaces name, a path in root, with data atomically, with
// mode's permissions, as WriteFileMode does, making the folders on its way
// that aren't there (0700). Every step goes through root, so no link
// leads it out of root's folder, and a link at name itself is replaced,
// never followed.
func WriteFileIn(root *os.Root, name string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(name)
	if err := root.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("writing %s: %w", name, err)
	}
	tmp := filepath.Join(dir, "."+filepath.Base(name)+".tmp-"+rand.Text())
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("writing %s: %w", name, err)
	}
	_, err = f.Write(data)
	if err == nil && mode.Perm() != 0o600 {
		err = f.Chmod(mode.Perm())
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = root.Rename(tmp, name)
	}
	if err != nil {
		_ = root.Remove(tmp)
		return fmt.Errorf("writing %s: %w", name, err)
	}
	return nil
}

// writeFile replaces path with data atomically, mode 600, creating its
// folder (0700) when it's missing.
func writeFile(path string, data []byte) error { return writeFileMode(path, data, 0o600) }

// writeFileMode replaces path with data atomically, mode perm, creating
// its folder when it's missing as PrivateDir does (0700). A folder there
// keeps its mode: some, ~/.ssh say, are the user's, or a link to one.
func writeFileMode(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		if err := PrivateDir(dir); err != nil {
			return err
		}
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if err == nil && perm != 0o600 { // CreateTemp makes it 600
		err = f.Chmod(perm)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// removeFile deletes path; a missing file is not an error.
func removeFile(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// checkName refuses a VM name or image key that isn't a plain file name.
func checkName(what, name string) error {
	if name == "" || strings.HasPrefix(name, ".") || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("invalid %s %q", what, name)
	}
	return nil
}
