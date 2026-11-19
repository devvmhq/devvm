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
type volumes struct {
	answer string
	fail   error
	calls  int
}

func (v *volumes) look(context.Context, platform.FolderStat) (string, error) {
	v.calls++
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
	// Renumbered, its lookup failing: the new device recorded, the bound volume kept.
	set(stubtest.Renumbered(id), platform.VolumeOf("uuid:B", id))
	if got := launch("", errors.New("diskutil hangs")); got.Folder() != id || got.FolderVolume() != platform.VolumeOf("uuid:B", id) {
		t.Errorf("renumbered: %q %q", got.Folder(), got.FolderVolume())
	}
	// Recorded and bound: no lookup at all.
	if launch("uuid:C", nil); v.calls != 0 {
		t.Errorf("looked up %d times for a bound volume", v.calls)
	}
}

// A run's lookups: each mount once, none after one fails, but on after an unsupported one; a
// folder's own volume (Linux's f_fsid) unless VolumesOnly; none for a read-only folder or one
// whose identity isn't its alone.
func TestVolumeOfCache(t *testing.T) {
	v := &volumes{answer: "uuid:A"}
	e := &Env{Volumes: v.look, volumes: new(volumeCache)}
	s := platform.FolderStat{ID: "1:2:3.4", Unique: true, Mount: "/m", Device: "disk1s1"}
	for range 2 {
		if got := e.volumeOf(t.Context(), s, volumeWaitLaunch); got != "uuid:A" {
			t.Errorf("volumeOf = %q", got)
		}
	}
	if v.calls != 1 {
		t.Errorf("asked %d times for one mount", v.calls)
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
