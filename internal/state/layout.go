package state

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/vmname"
)

// Layout is the layout version this devvm keeps its folders in: which
// folders and files the data and config folders hold, and their formats.
// A change to the layout that an older devvm would misread raises it,
// with a migration (migrations) from the version before.
const Layout = 1

// Migration changes devvm's folders from one layout version to the next.
type Migration func(paths.Dirs) error

// migrations are the migrations from each layout version to the next:
// the first takes version 1 to 2, so there are Layout-1 of them.
func migrations() []Migration { return nil }

// CheckLayout checks the layout versions of devvm's data and config
// folders (paths.Dirs.Version, ConfigVersion) against this devvm's, at a
// run's start, under the version lock: the same version goes on, an
// older one is migrated to this one, and a newer one is refused with
// layout_newer, because a newer devvm changed the folders. A folder
// without a version file gets this devvm's: nothing older has one. It
// waits for the lock, and migrates, under ctx's status line (msg.StatusOf).
func CheckLayout(ctx context.Context, d paths.Dirs) error {
	return checkLayout(ctx, d, Layout, migrations())
}

func checkLayout(ctx context.Context, d paths.Dirs, current int, ms []Migration) error {
	if len(ms) != current-1 {
		return fmt.Errorf("layout version %d has %d migrations", current, len(ms))
	}
	if err := PrivateDir(filepath.Dir(d.VersionLock())); err != nil {
		return err
	}
	status := msg.StatusOf(ctx)
	unlock, err := platform.LockFile(ctx, d.VersionLock(), msg.Waiting(status, msg.LayoutWaiting()))
	if err != nil {
		return err
	}
	defer unlock()
	files := []string{d.Version(), d.ConfigVersion()}
	from := current
	stale := false
	for _, f := range files {
		v, err := readVersion(f)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			stale = true
			continue
		case err != nil:
			return err
		case v > current:
			return msg.LayoutNewer(strconv.Itoa(current), f, strconv.Itoa(v))
		}
		stale = stale || v < current
		from = min(from, v)
	}
	if !stale {
		return nil
	}
	if len(ms[from-1:]) > 0 && status != nil {
		defer status(msg.LayoutMigrating())() // a migration may move every VM's folder
	}
	for _, m := range ms[from-1:] {
		if err := m(d); err != nil {
			return err
		}
	}
	data := []byte(strconv.Itoa(current) + "\n")
	for _, f := range files {
		if err := writeFile(f, data); err != nil {
			return err
		}
	}
	return nil
}

// readVersion reads a layout version file: a whole number from 1 up. A
// missing file is an error that matches fs.ErrNotExist.
func readVersion(path string) (int, error) {
	data, err := os.ReadFile(path) //nolint:gosec // paths come from paths.Dirs
	if err != nil {
		return 0, err
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || v < 1 {
		return 0, &CorruptError{Path: path, Reason: "not a layout version"}
	}
	return v, nil
}

// VMFolderProblems are the entries in the VMs' folder that aren't VMs'
// folders (vms_stray): anything not a folder named like a hash
// (vmname.IsHash); and the VMs' folders without a state file
// (vm_folder_stateless), which a devvm that stopped while making or
// deleting a VM leaves; and the VMs' folder itself when it can't read it
// (vms_unreadable), with why. A VM's folder it can't read is
// vm.StateProblems', so doctor warns of it once. It changes nothing.
func VMFolderProblems(d paths.Dirs) []msg.M {
	es, err := os.ReadDir(d.VMs())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return []msg.M{msg.VMsUnreadable(d.VMs()).Because(err)}
	}
	var ms []msg.M
	for _, e := range es {
		path := filepath.Join(d.VMs(), e.Name())
		if !e.IsDir() || !vmname.IsHash(e.Name()) {
			ms = append(ms, msg.VMsStray(path, d.VMs()))
			continue
		}
		// A folder whose vm.json can't be looked at is vm.StateProblems', which names the VM and why.
		if _, err := os.Lstat(paths.VM{Dir: path}.State()); errors.Is(err, fs.ErrNotExist) {
			ms = append(ms, msg.VMFolderStateless(path))
		}
	}
	return ms
}
