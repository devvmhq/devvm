package vm

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/stubtest"
)

// volumes is a fake Env.Volumes: it answers with answer, or fails with fail, and counts its calls.
// With fake, the Reporter it runs under, shown has the status line each lookup ran under, "" for
// none.
type volumes struct {
	answer string
	fail   error
	calls  int
	fake   *msg.Fake
	shown  []string
}

func (v *volumes) look(context.Context, platform.FolderStat) (string, error) {
	v.calls++
	if v.fake != nil {
		m, _ := v.fake.Showing()
		v.shown = append(v.shown, m.ID())
	}
	return v.answer, v.fail
}

// A VM records its folder's volume when it's made, bound to the folder; a VM whose state records
// none, as one made before, records it at its next launch, and one whose lookup fails records
// nothing and asks again at the next; a launch after a renumbering whose lookup fails keeps the
// volume recorded, as the inode and birth time it's bound to are the folder's still.
func TestRecordFolder(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	v := &volumes{answer: "uuid:A"}
	e.Volumes, e.VolumesOnly, e.volumes = v.look, true, new(volumeCache)
	st := existingVM(t, e, Options{Network: "open"})
	id := st.Folder()
	if _, err := platform.StatFolder(st.Dir); err != nil {
		t.Skipf("the folder can't be read with no link on its path: %v", err)
	}
	if got, want := st.FolderVolume(), platform.VolumeOf("uuid:A", id); got != want || want == "" {
		t.Fatalf("made: volume %q, want %q", got, want)
	}
	set := func(id, vol string) {
		t.Helper()
		if err := st.Update(t.Context(), func(st *state.VM) error { st.SetFolder(id, vol); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	launch := func(answer string, fail error) *state.VM {
		t.Helper()
		*v = volumes{answer: answer, fail: fail}
		e.volumes = new(volumeCache)
		stopVM(t, f, st)
		if err := (Launch{e}).Up(t.Context(), Options{}); err != nil {
			t.Fatal(msg.NamesOf(err))
		}
		got, err := state.ReadVM(e.Dirs, st.Name())
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	// A VM made before records none: its lookup fails, nothing's written; then it records it.
	set(id, "")
	if got := launch("", errors.New("diskutil hangs")); got.FolderVolume() != "" || got.Folder() != id {
		t.Errorf("a failed lookup: %q %q", got.Folder(), got.FolderVolume())
	}
	if got := launch("uuid:B", nil); got.FolderVolume() != platform.VolumeOf("uuid:B", id) {
		t.Errorf("recorded at its next launch: %q", got.FolderVolume())
	}
	// Renumbered, its lookup failing: the new device recorded, the bound volume kept, unchecked.
	set(stubtest.Renumbered(id), platform.VolumeOf("uuid:B", id))
	if got := launch("", errors.New("diskutil hangs")); got.Folder() != id || got.FolderVolume() != platform.VolumeOf("uuid:B", id) || !got.FolderUnchecked() {
		t.Errorf("renumbered: %q %q, unchecked %v", got.Folder(), got.FolderVolume(), got.FolderUnchecked())
	}
	// Unchecked, its lookup failing again: launched, still unchecked.
	if got := launch("", errors.New("diskutil hangs")); !got.FolderUnchecked() || v.calls == 0 {
		t.Errorf("unchecked, a failed lookup: unchecked %v, asked %d", got.FolderUnchecked(), v.calls)
	}
	// Unchecked, then read on its volume: checked.
	if got := launch("uuid:B", nil); got.FolderUnchecked() || got.FolderVolume() != platform.VolumeOf("uuid:B", id) {
		t.Errorf("checked: unchecked %v, volume %q", got.FolderUnchecked(), got.FolderVolume())
	}
	// Recorded and bound: no lookup at all.
	if launch("uuid:C", nil); v.calls != 0 {
		t.Errorf("looked up %d times for a bound volume", v.calls)
	}
}

// A renumbered folder recorded while its volume couldn't be read stays to check: a later launch
// that reads it on another volume than the one recorded takes it for another folder, as it
// would have at the renumbering, and records nothing.
func TestUncheckedOnAnotherVolume(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	v := &volumes{answer: "uuid:A"}
	e.Volumes, e.VolumesOnly, e.volumes = v.look, true, new(volumeCache)
	st := existingVM(t, e, Options{Network: "open"})
	id := st.Folder()
	if _, err := platform.StatFolder(st.Dir); err != nil {
		t.Skipf("the folder can't be read with no link on its path: %v", err)
	}
	if err := st.Update(t.Context(), func(st *state.VM) error {
		st.SetFolder(stubtest.Renumbered(id), platform.VolumeOf("uuid:A", id))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	launch := func(answer string, fail error) (*state.VM, error) {
		t.Helper()
		*v = volumes{answer: answer, fail: fail}
		e.volumes = new(volumeCache)
		stopVM(t, f, st)
		err := (Launch{e}).Up(t.Context(), Options{})
		got, rerr := state.ReadVM(e.Dirs, st.Name())
		if rerr != nil {
			t.Fatal(rerr)
		}
		return got, err
	}
	if got, err := launch("", errors.New("diskutil hangs")); err != nil || !got.FolderUnchecked() {
		t.Fatalf("renumbered, a failed lookup: %v, unchecked %v", msg.NamesOf(err), got.FolderUnchecked())
	}
	got, err := launch("uuid:B", nil)
	if msg.IDOf(err) != "vm_other_folder" || !got.FolderUnchecked() || got.FolderVolume() != platform.VolumeOf("uuid:A", id) {
		t.Errorf("on another volume: launched (%v), unchecked %v, volume %q", err, got.FolderUnchecked(), got.FolderVolume())
	}
}

// keepsVolume looks a folder's volume up only when it must: renumbered, unchecked, or free to read
// (Linux's f_fsid), so on Linux a folder on another volume than the one recorded is another
// folder even on the device it was recorded on, as a copy with a volume ID of its own mounted at
// its path can be.
func TestKeepsVolume(t *testing.T) {
	const id = "16777230:12345:1700000000.000000001"
	renumbered := "9" + id
	v := &volumes{answer: "uuid:B"}
	for _, tc := range []struct {
		name           string
		folder, volume string
		unchecked      bool
		s              platform.FolderStat
		want           bool
		asked          int
	}{
		{"as recorded, on a Mac: no lookup", id, platform.VolumeOf("uuid:A", id), false, platform.FolderStat{ID: id, Unique: true}, true, 0},
		{"renumbered on another volume", renumbered, platform.VolumeOf("uuid:A", id), false, platform.FolderStat{ID: id, Unique: true}, false, 1},
		{"unchecked on another volume", id, platform.VolumeOf("uuid:A", id), true, platform.FolderStat{ID: id, Unique: true}, false, 1},
		{"no volume recorded", renumbered, "", false, platform.FolderStat{ID: id, Unique: true}, true, 0},
		{"as recorded, on Linux, on its volume", id, platform.VolumeOf("fsid:0a", id), false, platform.FolderStat{ID: id, Unique: true, Volume: "fsid:0a"}, true, 0},
		{"as recorded, on Linux, on another volume", id, platform.VolumeOf("fsid:0a", id), false, platform.FolderStat{ID: id, Unique: true, Volume: "fsid:0b"}, false, 0},
	} {
		*v = volumes{answer: "uuid:B"}
		e := &Env{Volumes: v.look, volumes: new(volumeCache), Reporter: &msg.Fake{}}
		st := new(state.VM)
		if tc.unchecked {
			st.SetFolderUnchecked(tc.folder, tc.volume)
		} else {
			st.SetFolder(tc.folder, tc.volume)
		}
		if got := e.keepsVolume(t.Context(), st, tc.s); got != tc.want || v.calls != tc.asked {
			t.Errorf("%s: keepsVolume = %v, asked %d, want %v, %d", tc.name, got, v.calls, tc.want, tc.asked)
		}
	}
}

// recordFolder compares again under the state's lock: when another devvm recorded another volume
// for the folder since this one read the state, the volume this one read isn't recorded over it.
func TestRecordFolderUnderLock(t *testing.T) {
	e, _, _ := upEnv(t, allNative())
	v := &volumes{answer: "uuid:A"}
	e.Volumes, e.VolumesOnly, e.volumes = v.look, true, new(volumeCache)
	st := existingVM(t, e, Options{Network: "open"})
	id := st.Folder()
	if _, err := platform.StatFolder(st.Dir); err != nil {
		t.Skipf("the folder can't be read with no link on its path: %v", err)
	}
	if err := st.Update(t.Context(), func(st *state.VM) error {
		st.SetFolder(stubtest.Renumbered(id), platform.VolumeOf("uuid:A", id))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	stale, err := state.ReadVM(e.Dirs, st.Name())
	if err != nil {
		t.Fatal(err)
	}
	// Another devvm records volume B since.
	if err := st.Update(t.Context(), func(st *state.VM) error {
		st.SetFolder(stubtest.Renumbered(id), platform.VolumeOf("uuid:B", id))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.starter.recordFolder(t.Context(), stale); err != nil {
		t.Fatal(err)
	}
	got, err := state.ReadVM(e.Dirs, st.Name())
	if err != nil {
		t.Fatal(err)
	}
	if got.Folder() != stubtest.Renumbered(id) || got.FolderVolume() != platform.VolumeOf("uuid:B", id) {
		t.Errorf("recorded over another devvm's record: %q %q", got.Folder(), got.FolderVolume())
	}
}

// A folder at its own path on a disk numbered anew is the recorded one only on the volume its
// state records: on another, devvm takes it for another folder, launches nothing and records
// nothing; when its volume can't be told, the device's rule alone decides, as before.
func TestRenumberedOnAnotherVolume(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	v := &volumes{answer: "uuid:A"}
	e.Volumes, e.VolumesOnly, e.volumes = v.look, true, new(volumeCache)
	st := existingVM(t, e, Options{Network: "open"})
	id := st.Folder()
	if _, err := platform.StatFolder(st.Dir); err != nil {
		t.Skipf("the folder can't be read with no link on its path: %v", err)
	}
	renumbered := stubtest.Renumbered(id)
	launch := func(answer string, fail error) (*state.VM, error) {
		t.Helper()
		if err := st.Update(t.Context(), func(st *state.VM) error { st.SetFolder(renumbered, platform.VolumeOf("uuid:A", id)); return nil }); err != nil {
			t.Fatal(err)
		}
		*v = volumes{answer: answer, fail: fail}
		e.volumes = new(volumeCache)
		stopVM(t, f, st)
		err := (Launch{e}).Up(t.Context(), Options{})
		got, rerr := state.ReadVM(e.Dirs, st.Name())
		if rerr != nil {
			t.Fatal(rerr)
		}
		return got, err
	}
	if got, err := launch("uuid:B", nil); msg.IDOf(err) != "vm_other_folder" || got.Folder() != renumbered {
		t.Errorf("on another volume: launched (%v), folder %q", err, got.Folder())
	}
	// recordFolder's own check, under the state's lock: nothing recorded on another volume.
	e.volumes = new(volumeCache)
	if err := e.starter.recordFolder(t.Context(), st); err != nil {
		t.Fatal(err)
	}
	if got, _ := state.ReadVM(e.Dirs, st.Name()); got.Folder() != renumbered {
		t.Errorf("recordFolder on another volume recorded %q", got.Folder())
	}
	if got, err := launch("", errors.New("diskutil hangs")); err != nil || got.Folder() != id {
		t.Errorf("a failed lookup: %v, folder %q", msg.NamesOf(err), got.Folder())
	}
	if got, err := launch("uuid:A", nil); err != nil || got.Folder() != id {
		t.Errorf("on its own volume: %v, folder %q", msg.NamesOf(err), got.Folder())
	}
}

// A run's lookups: each mount once, none after one fails, but on after an unsupported one; a
// folder's own volume (Linux's f_fsid) unless VolumesOnly; none for a read-only folder or one
// whose identity isn't its alone.
func TestVolumeOfCache(t *testing.T) {
	v := &volumes{answer: "uuid:A", fake: &msg.Fake{}}
	e := &Env{Volumes: v.look, volumes: new(volumeCache), Reporter: v.fake}
	s := platform.FolderStat{ID: "1:2:3.4", Unique: true, Mount: "/m", Device: "disk1s1"}
	for range 2 {
		if got := e.volumeOf(t.Context(), s, volumeWaitLaunch); got != "uuid:A" {
			t.Errorf("volumeOf = %q", got)
		}
	}
	// The one lookup, under its status line.
	if v.calls != 1 || len(v.shown) != 1 || v.shown[0] != "looking_up_volume" {
		t.Errorf("asked %d times for one mount, under %q", v.calls, v.shown)
	}
	if _, on := v.fake.Showing(); on {
		t.Error("its status line still shows")
	}
	ro, other := s, s
	ro.ReadOnly, other.Unique = true, false
	if e.volumeOf(t.Context(), ro, volumeWaitLaunch) != "" || e.volumeOf(t.Context(), other, volumeWaitLaunch) != "" || v.calls != 1 {
		t.Errorf("a read-only or shared identity asked: %d", v.calls)
	}
	linux := s
	linux.Volume, linux.Mount = "fsid:0a", "/l"
	if got := e.volumeOf(t.Context(), linux, volumeWaitLaunch); got != "fsid:0a" || v.calls != 1 {
		t.Errorf("Linux's own volume = %q, asked %d", got, v.calls)
	}
	e.VolumesOnly = true
	if got := e.volumeOf(t.Context(), linux, volumeWaitLaunch); got != "uuid:A" || v.calls != 2 {
		t.Errorf("VolumesOnly = %q, asked %d", got, v.calls)
	}

	*v = volumes{fail: errors.ErrUnsupported}
	e.volumes = new(volumeCache)
	_ = e.volumeOf(t.Context(), s, volumeWaitLaunch)
	other.Unique, other.Mount = true, "/n"
	_ = e.volumeOf(t.Context(), other, volumeWaitLaunch)
	if v.calls != 2 {
		t.Errorf("unsupported stopped the run's lookups: %d", v.calls)
	}
	*v = volumes{fail: errors.New("hangs")}
	e.volumes = new(volumeCache)
	_ = e.volumeOf(t.Context(), s, volumeWaitLaunch)
	if got := e.volumeOf(t.Context(), other, volumeWaitLaunch); got != "" || v.calls != 1 {
		t.Errorf("asked again after a failure: %q, %d", got, v.calls)
	}
	if !strings.HasPrefix(platform.VolumeOf("uuid:A", s.ID), "uuid:A:") {
		t.Error("VolumeOf")
	}
}
