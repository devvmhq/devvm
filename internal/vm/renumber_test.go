package vm

import (
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/stubtest"
)

// A project folder whose device a boot numbered anew, as macOS does, is
// the same folder: its VM starts, and its state records the folder's
// identity on the device it has now before the start, which mounts it by
// that, so the start mounts the folder, never what the old device's
// number names now. Another inode on the new device is another folder,
// refused as before.
func TestRenumberedFolder(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	st := existingVM(t, e, Options{Network: "open"})
	stopVM(t, f, st)
	id := st.Folder()
	record := func(was string) {
		t.Helper()
		if err := st.Update(t.Context(), func(st *state.VM) error { st.SetFolder(was, st.FolderVolume()); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	record(stubtest.Renumbered(id))
	if err := (Launch{e}).Up(t.Context(), Options{}); err != nil {
		t.Fatalf("its folder's device numbered anew: %v", msg.NamesOf(err))
	}
	got, err := state.ReadVM(e.Dirs, st.Name())
	if err != nil || got.Folder() != id {
		t.Errorf("recorded %q, %v, want the folder's identity now, %q", got.Folder(), err, id)
	}
	if w, _ := call(f, "Start").Args[1].(backend.Want); w.ProjectID != id {
		t.Errorf("started mounting %q, want %q", w.ProjectID, id)
	}

	// Another inode on the device it has now: another folder.
	stopVM(t, f, got)
	dev, rest, _ := strings.Cut(id, ":")
	ino, birth, _ := strings.Cut(rest, ":")
	st = got
	record(stubtest.Renumbered(dev + ":" + ino + "0:" + birth))
	if err := (Launch{e}).Up(t.Context(), Options{}); msg.NamesOf(err) != "vm_other_folder vm="+st.Name().String()+" was="+e.Dir {
		t.Errorf("another inode: %v", msg.NamesOf(err))
	}
}
