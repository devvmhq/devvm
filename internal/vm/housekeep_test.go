package vm

import (
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/backend/backendtest"
	"github.com/devvmhq/devvm/internal/build"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/settings"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/tools"
)

// An image the backend refuses to delete with a message (on Linux hosts,
// one whose disk VMs use) stays, with its record, and Apply goes on,
// returning it refused; any other failure ends it, with what it did up
// to there.
func TestApplyKeepsImagesInUse(t *testing.T) {
	dir := t.TempDir()
	d := paths.Dirs{Data: filepath.Join(dir, "data"), Config: filepath.Join(dir, "config"), Lima: filepath.Join(dir, "lima")}
	f := backendtest.New("fake", backend.Caps{})
	f.ImageKeys = []string{"0123456789ab", "ba9876543210"}
	if err := state.WriteImage(d, "0123456789ab", state.Image{Name: "base"}); err != nil {
		t.Fatal(err)
	}
	b := build.Images{Env: &build.Env{Dirs: d, Backend: f}}
	p := GCPlan{Images: []ListedImage{
		{Listed: build.Listed{Image: build.Image{Key: "0123456789ab", Name: "base"}}, images: b},
		{Listed: build.Listed{Image: build.Image{Key: "ba9876543210"}}, images: b},
	}}
	inUse := msg.ImageInUse("0123456789ab", "devvm-p-0123456789abcd")
	f.Errs = map[string]error{"DeleteImage": inUse}
	done, err := p.Apply(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Methods(); !slices.Equal(got, []string{"DeleteImage", "DeleteImage"}) {
		t.Errorf("calls: %v", got)
	}
	if len(done.Images) != 2 || !done.Images[0].Refused || msg.IDOf(done.Images[0].Why) != "image_in_use" {
		t.Errorf("done %+v, want both refused", done.Images)
	}
	if _, err := state.ReadImage(d, "0123456789ab"); err != nil {
		t.Errorf("the image in use lost its record: %v", err)
	}
	boom := errors.New("boom")
	f.Errs = map[string]error{"DeleteImage": boom}
	if done, err := p.Apply(t.Context()); !errors.Is(err, boom) || len(done.Images) != 0 {
		t.Errorf("Apply = %+v, %v, want %v", done, err, boom)
	}
}

// A skipped backend's orphans aren't forgotten: its VMs' state stays, and
// the backend is named once among those that couldn't be asked.
func TestWithoutSkipped(t *testing.T) {
	r := LeftoverReport{
		Orphans: []Orphan{{Name: "a", Backend: "lima"}, {Name: "b", Backend: "qemu"}, {Name: "c", Backend: "qemu"}, {Name: "d", Backend: "fake"}},
		Unknown: []string{"fake"},
	}
	got := withoutSkipped(r, map[string]bool{"qemu": true, "fake": true})
	if len(got.Orphans) != 1 || got.Orphans[0].Name != "a" {
		t.Errorf("orphans %+v, want a alone", got.Orphans)
	}
	if !slices.Equal(got.Unknown, []string{"fake", "qemu"}) {
		t.Errorf("unknown %v", got.Unknown)
	}
	if got := withoutSkipped(r, nil); len(got.Orphans) != 4 || len(got.Unknown) != 1 {
		t.Errorf("nothing skipped: %+v", got)
	}
}

// A backend that can't list its images is skipped with a warning, and
// its tool images, and those whose record names no backend, are left
// out of every list.
func TestGCPlanSkip(t *testing.T) {
	var p GCPlan
	p.skip(nil)
	if p.Skipped != nil || len(p.Warnings) != 0 {
		t.Errorf("nothing unlisted: %+v", p)
	}
	p.Tools = []tools.Listed{{Backend: "lima", File: "a"}, {Backend: "qemu", File: "b"}, {File: "c"}}
	p.skip([]Unlisted{{Backend: "qemu", Err: errors.New("no")}})
	if !p.Skipped["qemu"] || len(p.Warnings) != 1 || msg.IDOf(p.Warnings[0]) != "gc_backend_skipped" {
		t.Errorf("skipped %v, warnings %v", p.Skipped, p.Warnings)
	}
	if got := ToolFiles(p.Tools); !slices.Equal(got, []string{"a"}) {
		t.Errorf("tools %v, want a alone", got)
	}
}

// Retention runs under its status line, which is gone once it's done.
func TestRetainStatus(t *testing.T) {
	dir := t.TempDir()
	d := paths.Dirs{Data: filepath.Join(dir, "data"), Config: filepath.Join(dir, "config")}
	var labels msg.Recorder
	fake := &msg.Fake{OnStatus: labels.Add}
	if _, err := (Housekeeping{&Env{Dirs: d, Reporter: fake}}).Retain(t.Context(), &build.Env{Dirs: d}, settings.Retention{}, false); err != nil {
		t.Fatal(err)
	}
	if got := labels.IDs(); !slices.Equal(got, []string{"retaining"}) {
		t.Errorf("status lines %q, want retaining", got)
	}
	if _, on := fake.Showing(); on {
		t.Error("its status line still shows")
	}
}

// Retained says whether it deleted anything.
func TestRetainedAny(t *testing.T) {
	if (Retained{}).Any() || !(Retained{NixFiles: 1}).Any() {
		t.Error("Any")
	}
}
