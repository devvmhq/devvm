package state

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
)

// This devvm has a migration from each layout version before its own.
func TestMigrations(t *testing.T) {
	if n := len(migrations()); n != Layout-1 {
		t.Errorf("%d migrations for layout version %d", n, Layout)
	}
}

// Folders without version files get this devvm's; the same version goes
// on; an older one is migrated, once, then written; a newer one in
// either folder is refused, naming its file, and nothing is written.
func TestCheckLayout(t *testing.T) {
	tests := []struct {
		name         string
		data, config string // the version files, "" for none
		current      int
		wantRun      int    // the migrations run
		wantErr      string // the message's names, or "corrupt"
		want         string // both files after
	}{
		{name: "new folders", current: 1, want: "1\n"},
		{name: "the same version", data: "1\n", config: "1\n", current: 1, want: "1\n"},
		{name: "config folder without one", data: "1\n", current: 1, want: "1\n"},
		{name: "older", data: "1\n", config: "1\n", current: 2, wantRun: 1, want: "2\n"},
		{name: "older config folder", data: "2\n", config: "1\n", current: 2, wantRun: 1, want: "2\n"},
		{name: "older without a config version", data: "1\n", current: 3, wantRun: 2, want: "3\n"},
		{name: "newer data folder", data: "2\n", config: "1\n", current: 1, wantErr: "layout_newer current=1 path=DATA version=2"},
		{name: "newer config folder", data: "1\n", config: "3\n", current: 2, wantErr: "layout_newer current=2 path=CONFIG version=3"},
		{name: "not a version", data: "one\n", current: 1, wantErr: "corrupt"},
		{name: "zero", data: "0\n", current: 1, wantErr: "corrupt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := testDirs(t)
			for f, v := range map[string]string{d.Version(): tt.data, d.ConfigVersion(): tt.config} {
				if v != "" {
					write(t, f, v)
				}
			}
			var ran []int
			var ms []Migration
			for i := range tt.current - 1 {
				ms = append(ms, func(paths.Dirs) error { ran = append(ran, i+1); return nil })
			}
			err := checkLayout(t.Context(), d, tt.current, ms)
			if tt.wantErr == "corrupt" {
				if _, ok := errors.AsType[*CorruptError](err); !ok {
					t.Fatalf("checkLayout = %v, want a CorruptError", err)
				}
				return
			}
			if tt.wantErr != "" {
				m, ok := errors.AsType[msg.M](err)
				if want := replacePaths(tt.wantErr, d); !ok || m.Names() != want {
					t.Fatalf("checkLayout = %v, want %s", err, want)
				}
				if got := read(t, d.Version()); got != tt.data {
					t.Errorf("a refused check wrote the data folder's version: %q", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(ran) != tt.wantRun {
				t.Errorf("ran migrations %v, want %d", ran, tt.wantRun)
			}
			if !slices.IsSorted(ran) || (len(ran) > 0 && ran[len(ran)-1] != tt.current-1) {
				t.Errorf("ran migrations %v, want the last ones in order", ran)
			}
			for _, f := range []string{d.Version(), d.ConfigVersion()} {
				if got := read(t, f); got != tt.want {
					t.Errorf("%s = %q, want %q", f, got, tt.want)
				}
			}
		})
	}
}

// replacePaths puts d's version files for DATA and CONFIG in names.
func replacePaths(names string, d paths.Dirs) string {
	return strings.NewReplacer("path=DATA", "path="+d.Version(), "path=CONFIG", "path="+d.ConfigVersion()).Replace(names)
}

// A failed migration fails the check, and the versions stay as they were,
// so the next run migrates again.
func TestCheckLayoutMigrationFails(t *testing.T) {
	d := testDirs(t)
	write(t, d.Version(), "1\n")
	failed := errors.New("failed")
	if err := checkLayout(t.Context(), d, 2, []Migration{func(paths.Dirs) error { return failed }}); !errors.Is(err, failed) {
		t.Fatalf("checkLayout = %v, want the migration's error", err)
	}
	if got := read(t, d.Version()); got != "1\n" {
		t.Errorf("version = %q, want 1", got)
	}
	if err := checkLayout(t.Context(), d, 2, nil); err == nil {
		t.Error("a layout without its migrations was checked")
	}
}

// doctor's findings in the VMs' folder: what isn't a VM's folder, and a
// VM's folder without vm.json, and the VMs' folder when it can't read it;
// a VM's folder with vm.json is fine, and one it can't read is left to
// the state's check (vm.StateProblems), which names it and why.
func TestVMFolderProblems(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads any folder")
	}
	d := testDirs(t)
	if ms := VMFolderProblems(d); len(ms) != 0 {
		t.Fatalf("no VMs' folder: %v", ms)
	}
	const good, lockOnly, locked = "0123456789abcd", "0123456789abce", "0123456789abcf"
	write(t, d.VM("devvm-a-"+good).State(), "{}")
	write(t, d.VM("devvm-b-"+lockOnly).Lock(), "")
	write(t, d.VM("devvm-c-"+locked).State(), "{}")
	write(t, filepath.Join(d.VMs(), "notes.txt"), "x")
	if err := os.MkdirAll(filepath.Join(d.VMs(), "devvm-old"), 0o700); err != nil {
		t.Fatal(err)
	}
	unreadable(t, d.VMDir(locked))
	want := []string{
		"vm_folder_stateless path=" + d.VMDir(lockOnly),
		"vms_stray path=" + filepath.Join(d.VMs(), "devvm-old") + " vms=" + d.VMs(),
		"vms_stray path=" + filepath.Join(d.VMs(), "notes.txt") + " vms=" + d.VMs(),
	}
	if got := problemNames(VMFolderProblems(d)); !slices.Equal(got, want) {
		t.Errorf("VMFolderProblems = %q, want %q", got, want)
	}
	unreadable(t, d.VMs())
	if got, want := problemNames(VMFolderProblems(d)), []string{"vms_unreadable path=" + d.VMs()}; !slices.Equal(got, want) {
		t.Errorf("an unreadable VMs' folder: VMFolderProblems = %q, want %q", got, want)
	}
}

// unreadable takes every permission off dir until the test ends.
func unreadable(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // a folder's mode, the one it had
}

// problemNames are ms in names mode, sorted.
func problemNames(ms []msg.M) []string {
	got := make([]string, len(ms))
	for i, m := range ms {
		got[i] = m.Names()
	}
	slices.Sort(got)
	return got
}
