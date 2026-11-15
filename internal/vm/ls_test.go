package vm

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/backend/backendtest"
	"github.com/devvmhq/devvm/internal/gitprotect"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/vmname"
)

// A folder is there when anything is at its path, or the state records
// none. With nothing there, it's gone when the nearest folder above that
// exists is on the device the folder was on, or no device is recorded,
// and not connected when it's on another.
func TestFolderOf(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	gone, deeper := filepath.Join(dir, "gone"), filepath.Join(dir, "gone", "deeper")
	// dir is on device 7, everything above it on 1.
	device := func(path string) (string, error) {
		switch {
		case path == dir:
			return "7", nil
		case strings.HasPrefix(dir, path):
			return "1", nil
		}
		return "", fs.ErrNotExist
	}
	for _, c := range []struct {
		dir, id string
		want    FolderState
	}{
		{dir, "9:1", FolderThere},
		{file, "9:1", FolderThere},
		{"", "", FolderThere},
		{gone, "7:5", FolderGone},
		{deeper, "7:5:1.2", FolderGone},
		{gone, "", FolderGone},
		{gone, "9:5", FolderNotConnected},
		{deeper, "9:5", FolderNotConnected},
	} {
		if got := folderOf(&state.VM{Dir: c.dir, Folder: c.id}, device); got != c.want {
			t.Errorf("folderOf(%q, %q) = %v, want %v", c.dir, c.id, got, c.want)
		}
	}
	// A device that can't be read: gone, as nothing tells it apart.
	broken := func(string) (string, error) { return "", errors.ErrUnsupported }
	if got := folderOf(&state.VM{Dir: gone, Folder: "9:5"}, broken); got != FolderGone {
		t.Errorf("folderOf with no device = %v", got)
	}
	// The real device: a folder deleted from this disk is gone.
	id := filepath.Join(dir, "real")
	if err := os.Mkdir(id, 0o700); err != nil {
		t.Fatal(err)
	}
	rec, err := platform.FolderID(id)
	if err != nil {
		t.Skip(err)
	}
	if err := os.Remove(id); err != nil {
		t.Fatal(err)
	}
	if got := FolderOf(&state.VM{Dir: id, Folder: rec}); got != FolderGone {
		t.Errorf("FolderOf a deleted folder = %v", got)
	}
}

// ls lists project VMs on every backend, from their state; images, build
// VMs and a VM without state aside.
func TestList(t *testing.T) {
	d := paths.Dirs{Data: t.TempDir()}
	const (
		web   = "devvm-web-0123456789abc4"
		img   = "devvm-img-foo-0123456789abcc" // a folder named img-foo
		clone = "devvm-cl-0123456789abc9"
		half  = "devvm-half-0123456789abce"
		other = "devvm-api-0123456789abc3"
	)
	// web's folder is there; the others' are gone.
	webDir := t.TempDir()
	for _, v := range []struct {
		name vmname.VM
		kv   map[string]string
	}{
		{web, map[string]string{keyImage: "docker", keyToolNames: "node claude", keyDir: webDir, KeyNet: "allowlist", keyGitProtect: "on"}},
		{img, map[string]string{keyDir: "/p/img-foo", keyFrom: "node:24", KeyNet: "open", keyGitProtect: "off"}},
		{clone, map[string]string{keyDir: "/p/cl", KeyNet: "allowlist", keyCheck: "unenforced", KeyMode: ModeClone, keyGitProtect: gitprotect.Clone}},
		{other, map[string]string{keyToolNames: "go", keyDir: "/p/api", KeyNet: "open", keyGitProtect: "on"}},
	} {
		be := "lima"
		if v.name == other {
			be = "other"
		}
		vmState(t, d, v.name, func(st *state.VM) error {
			st.Backend = be
			for k, val := range v.kv {
				if err := setField(st, k, val); err != nil {
					return err
				}
			}
			return nil
		})
	}
	lima := backendtest.New("lima", nil)
	lima.Unconfined = true
	for _, in := range []backend.Instance{
		{Name: web, Status: backend.StatusRunning},
		{Name: img, Status: backend.StatusStopped},
		{Name: clone, Status: backend.StatusStopped},
		{Name: half, Status: backend.StatusStopped},
		{Name: "devvm-tools-build", Status: backend.StatusStopped},
	} {
		lima.Instances[in.Name] = in
	}
	oth := backendtest.New("other", nil)
	oth.Instances[other] = backend.Instance{Name: other, Status: backend.StatusRunning}
	reg, err := backend.NewRegistry(lima, oth)
	if err != nil {
		t.Fatal(err)
	}
	got, err := list(t, d, reg)
	if err != nil {
		t.Fatal(err)
	}
	want := []Listed{
		{Name: other, Status: "Running", Image: "base", Tools: []string{"go"}, Git: "on", Net: "open", Backend: "other", Folder: "/p/api", FolderState: FolderGone},
		{Name: clone, Status: "Stopped", Image: "base", Git: "clone", Net: "unenforced", Backend: "lima", Folder: "/p/cl", FolderState: FolderGone},
		{Name: half, Status: "Stopped", Backend: "lima", FolderState: FolderThere},
		{Name: img, Status: "Stopped", Image: "base", From: "node:24", Git: "off", Net: "open", Backend: "lima", Folder: "/p/img-foo", FolderState: FolderGone},
		{Name: web, Status: "Running", Image: "docker", Tools: []string{"node", "claude"}, Git: "on", Net: "unenforced", Backend: "lima", Folder: webDir, FolderState: FolderThere},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("List =\n%+v\nwant\n%+v", got, want)
	}

	// A backend that can't list: skipped when no VM's state names it,
	// else the failure, its own check's first.
	broken := backendtest.New("broken", nil)
	broken.Errs = map[string]error{"List": errors.New("list failed")}
	reg, _ = backend.NewRegistry(broken)
	if got, err := list(t, d, reg); err != nil || got != nil {
		t.Errorf("unused broken backend: %v, %v", got, err)
	}
	vmState(t, d, "devvm-b-0123456789abcd", func(st *state.VM) error { st.Backend = "broken"; return nil })
	if _, err := list(t, d, reg); err == nil || err.Error() != "list failed" {
		t.Errorf("used broken backend: %v", err)
	}
	broken.Missing = []backend.Problem{{Key: "lima", Msg: msg.ProgramMissing("Lima", "limactl")}}
	if _, err := list(t, d, reg); msg.IDOf(err) != "program_missing" {
		t.Errorf("used broken backend, check failing: %v", err)
	}
}

// list is devvm ls's list: every backend's VMs.
func list(t *testing.T, d paths.Dirs, reg *backend.Registry) ([]Listed, error) {
	t.Helper()
	inv, err := ReadInventory(t.Context(), d, reg, AllBackends)
	if err != nil {
		t.Fatal(err)
	}
	return inv.List(t.Context())
}
