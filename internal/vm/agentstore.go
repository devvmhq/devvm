package vm

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/platform"
)

// devvm's agent store is below devvm's config folder, and only a hand-made
// store has a link in it, which could lead to any folder of this
// machine's, such as the home folder. devvm never writes, empties or
// cleans the store through such a link: it checks every path it will
// write first (storeGuard), so a link fails the command before the store
// changes, and then writes, empties and cleans a folder of the store only
// through it opened from the config folder as a root, every part of its
// path a folder (openStoreDir, makeStoreDir), so no write follows a link
// out of it.

// lockStore holds devvm's agent store lock (paths.Dirs.AgentStoreLock)
// until unlock is called or the process ends, waiting for another devvm
// that holds it until ctx ends, under a status line (agent_store_waiting)
// only while it waits. Whatever writes the store holds it while it reads
// what's there and writes: devvm agent import from its staging to its
// logins, devvm agent save's writes and a start's clean of a seed's
// folder (cleanSeeds), so no two of them change it at once. It's an OS
// lock on a file, which the OS lets go of when the process holding it
// ends, however it ends.
func lockStore(ctx context.Context, d paths.Dirs, r msg.Reporter) (unlock func(), err error) {
	if err := os.MkdirAll(d.Data, 0o700); err != nil {
		return nil, err
	}
	return platform.LockFile(ctx, d.AgentStoreLock(), msg.Waiting(r.Status, msg.AgentStoreWaiting()))
}

// storeLink is the error for a link at path, on the way to a folder of
// devvm's agent store (agent_store_link, agent_seed_link).
type storeLink struct{ path string }

func (l storeLink) Error() string { return l.path + " is a link" }

// storeErr is the message for an error that's a link in the store
// (agent_store_link), and err as it is for any other.
func storeErr(err error) error {
	if l, ok := errors.AsType[storeLink](err); ok {
		return msg.AgentStoreLink(l.path).Because(err)
	}
	return err
}

// openStoreDir opens dir, a folder of devvm's agent store below devvm's
// config folder config, as a root. It reaches the folder through config
// as a root, checking that every part of its path below config is a
// folder, never a link (storeLink), and that the folder it opened is the
// one it checked. A folder that isn't there, or that something else
// stands in place of, is nil and no error: it has nothing in it.
func openStoreDir(config, dir string) (*os.Root, error) {
	rel, err := filepath.Rel(config, dir)
	if err != nil || !filepath.IsLocal(rel) {
		return nil, fmt.Errorf("%s isn't in %s", dir, config)
	}
	root, err := os.OpenRoot(config)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	fi, err := walkStore(config, rel, root.Lstat, nil)
	if err != nil || fi == nil {
		return nil, err
	}
	d, err := root.OpenRoot(rel)
	if err != nil {
		return nil, err
	}
	// The folder opened is the one checked, never a link put in its place since.
	if here, err := d.Stat("."); err != nil || !os.SameFile(fi, here) {
		_ = d.Close()
		return nil, fmt.Errorf("%s changed while devvm read it", dir)
	}
	return d, nil
}

// makeStoreDir opens dir, a folder of devvm's agent store below devvm's
// config folder config, as a root (openStoreDir), making it first, with
// the folders on its way that aren't there (0700), through config as a
// root, so no link leads the making out of config. A link on its way
// fails, as openStoreDir's does (storeLink). Every write to the store
// goes through the root it returns.
func makeStoreDir(config, dir string) (*os.Root, error) {
	rel, err := filepath.Rel(config, dir)
	if err != nil || !filepath.IsLocal(rel) {
		return nil, fmt.Errorf("%s isn't in %s", dir, config)
	}
	if _, err := walkStore(config, rel, func(at string) (fs.FileInfo, error) { return os.Lstat(filepath.Join(config, at)) }, nil); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(config, 0o700); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(config)
	if err != nil {
		return nil, err
	}
	err = root.MkdirAll(rel, 0o700)
	_ = root.Close()
	if err != nil {
		return nil, err
	}
	d, err := openStoreDir(config, dir)
	if err == nil && d == nil {
		err = fmt.Errorf("%s changed while devvm made it", dir)
	}
	return d, err
}

// storeGuard checks the paths that devvm agent import and save will write
// in the store, by path, before the first write: no part of a path below
// devvm's config folder (config) is a link (storeLink), so the command
// names the link and fails before the store changes. A part that isn't
// there yet is made a folder by the write itself, through a root
// (makeStoreDir). The folders it found are checked once (dirs).
type storeGuard struct {
	config string
	dirs   map[string]bool
}

func newStoreGuard(config string) *storeGuard {
	return &storeGuard{config: config, dirs: map[string]bool{}}
}

// folder checks dir, a folder that a write makes or writes into, and the
// folders above it.
func (g *storeGuard) folder(dir string) error {
	rel, err := filepath.Rel(g.config, dir)
	if err != nil || !filepath.IsLocal(rel) {
		return fmt.Errorf("%s isn't in %s", dir, g.config)
	}
	lstat := func(at string) (fs.FileInfo, error) { return os.Lstat(filepath.Join(g.config, at)) }
	_, err = walkStore(g.config, rel, lstat, g.dirs)
	return err // a part that isn't a folder fails the write on its own
}

// walkStore is the one check of a path in devvm's agent store: it looks at
// each part of rel, a path below devvm's config folder config, from the
// top, with lstat, which takes the part's path relative to config. A link
// is an error (storeLink). It returns the last part's information, or
// none when a part isn't there or isn't a folder, where the walk stops.
// The parts in seen, by their path relative to config, are folders it
// found before, and it adds those it finds.
func walkStore(config, rel string, lstat func(string) (fs.FileInfo, error), seen map[string]bool) (fs.FileInfo, error) {
	var fi fs.FileInfo
	at := ""
	for part := range strings.SplitSeq(rel, string(filepath.Separator)) {
		at = filepath.Join(at, part)
		if seen[at] && at != rel {
			continue
		}
		var err error
		fi, err = lstat(at)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return nil, nil
		case err != nil:
			return nil, err
		case fi.Mode()&fs.ModeSymlink != 0:
			return nil, storeLink{filepath.Join(config, at)}
		case !fi.IsDir():
			return nil, nil
		}
		if seen != nil {
			seen[at] = true
		}
	}
	return fi, nil
}

// file checks the folders above p, a file that a write replaces whole
// (state.WriteFileIn renames a new file into place, so a link at p
// itself is replaced, never followed).
func (g *storeGuard) file(p string) error { return g.folder(filepath.Dir(p)) }
