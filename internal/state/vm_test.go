package state

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/vmname"
)

// vmP is a project VM's name, for tests of a VM's state.
const vmP vmname.VM = "devvm-p-00000000000000"

// counter is a section the tests change.
type counter struct {
	N    int      `json:"n,omitempty"`
	Seen []string `json:"seen,omitempty"`
}

// create writes a VM's state, as up does, with set's changes.
func create(t *testing.T, _ paths.Dirs, v *VM, set func(*VM)) *VM {
	t.Helper()
	v.Image, v.Dir, v.Backend = "base", "/p", "lima"
	if set != nil {
		set(v)
	}
	if err := v.Create(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	return v
}

// A VM's state reads back as written: its core fields, then each section
// by name, in vm.json; ReadHash finds it by its folder's hash. Fields and
// sections this devvm doesn't know are kept.
func TestVMRoundTrip(t *testing.T) {
	d := testDirs(t)
	v, err := New(d, vmP)
	if err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	create(t, d, v, func(v *VM) {
		v.Alias, v.Chain, v.From, v.BackendAt, v.Created, v.MadeBy = "devvm-p", "base=abc", "debian:12", "/lima", created, "0.1.0"
		v.SetFolder("1:2:3.4", "uuid:EC5E5014-21A3-4154-ACE8-0C8C1C254852:2:3.4")
		if err := SetSection(v, "count", counter{N: 3, Seen: []string{"a<b"}}); err != nil {
			t.Fatal(err)
		}
	})
	back, err := ReadVM(d, vmP)
	if err != nil {
		t.Fatal(err)
	}
	if back.Name() != vmP || back.ID == "" || back.ID != v.ID || back.Alias != "devvm-p" || back.Chain != "base=abc" ||
		back.From != "debian:12" || back.Folder() != "1:2:3.4" || back.FolderVolume() != "uuid:EC5E5014-21A3-4154-ACE8-0C8C1C254852:2:3.4" || back.BackendAt != "/lima" || !back.Created.Equal(created) ||
		back.Image != "base" || back.Dir != "/p" || back.Backend != "lima" || back.MadeBy != "0.1.0" || !back.Exists() || !back.Finished() {
		t.Errorf("read back %+v", back)
	}
	if c, err := Section[counter](back, "count"); err != nil || c.N != 3 || !slices.Equal(c.Seen, []string{"a<b"}) {
		t.Errorf("section = %+v, %v", c, err)
	}
	if c, err := Section[counter](back, "none"); err != nil || c.N != 0 {
		t.Errorf("no section = %+v, %v", c, err)
	}
	file := read(t, d.VM(vmP).State())
	if !strings.HasPrefix(file, "{\n  \"version\": 1,\n  \"id\": ") || !strings.Contains(file, `"seen": [`) || !strings.Contains(file, "a<b") {
		t.Errorf("vm.json:\n%s", file)
	}
	byHash, err := ReadHash(d, "00000000000000")
	if err != nil || byHash.Name() != vmP || byHash.Image != "base" {
		t.Errorf("ReadHash = %+v, %v", byHash, err)
	}
	// A newer devvm's fields and sections are kept by this one's writes,
	// and so is the version of the devvm that made the VM.
	write(t, d.VM(vmP).State(), strings.Replace(file, `"version": 1,`, `"version": 1, "newer": {"x": 1},`, 1))
	if err := back.Update(t.Context(), func(v *VM) error { return SetSection(v, "count", counter{N: 4}) }); err != nil {
		t.Fatal(err)
	}
	if got := read(t, d.VM(vmP).State()); !strings.Contains(got, `"newer": {`) || !strings.Contains(got, `"made_by": "0.1.0"`) {
		t.Errorf("unknown fields or made_by lost:\n%s", got)
	}
	// A section set to its zero value goes.
	if err := back.Update(t.Context(), func(v *VM) error { return SetSection(v, "count", counter{}) }); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(read(t, d.VM(vmP).State()), `"count"`) {
		t.Error("an empty section was kept")
	}
	if err := SetSection(back, "dir", counter{N: 1}); err == nil {
		t.Error("a section named like a core field: want an error")
	}
}

// A folder's state is read by the name it records alone: another name
// with the same hash (made for the folder by another backend) reads as no
// state, and is never written or removed through it.
func TestVMOtherName(t *testing.T) {
	d := testDirs(t)
	v, _ := New(d, vmP)
	create(t, d, v, nil)
	other, err := ReadVM(d, "devvm-pp-00000000000000")
	if err != nil || other.Exists() || other.Image != "" {
		t.Fatalf("ReadVM with another name = %+v, %v", other, err)
	}
	if err := other.Update(t.Context(), func(v *VM) error { v.Image = "evil"; return nil }); err == nil {
		t.Error("Update through another name: want an error")
	}
	if err := other.Claim(t.Context(), func(*VM) error { return nil }); err == nil {
		t.Error("Claim through another name: want an error")
	}
	if err := other.Remove(t.Context()); err != nil {
		t.Fatal(err)
	}
	if st, err := ReadVM(d, vmP); err != nil || !st.Exists() || st.Image != "base" {
		t.Errorf("the VM's own after: %+v, %v", st, err)
	}
	for _, h := range []string{"", "vm", "../00000000000000", "0000000000000"} {
		if _, err := ReadHash(d, h); err == nil {
			t.Errorf("ReadHash(%q): want an error", h)
		}
	}
}

// Every VM Create makes gets an ID of its own, a new one when it's made
// again under the same name.
func TestVMID(t *testing.T) {
	d := testDirs(t)
	hex32 := regexp.MustCompile(`^[0-9a-f]{32}$`)
	a, _ := New(d, vmP)
	first := create(t, d, a, nil).ID
	b, _ := New(d, vmP)
	again := create(t, d, b, nil).ID
	back, err := ReadVM(d, vmP)
	if err != nil {
		t.Fatal(err)
	}
	if !hex32.MatchString(first) || back.ID != again || again == first {
		t.Errorf("IDs %q, then %q (read back as %q): want 32 hex digits, a new one each time", first, again, back.ID)
	}
}

// Update reads the state again first, so another devvm's change is kept;
// a failed change writes nothing and leaves v as the file has it.
func TestVMUpdate(t *testing.T) {
	d := testDirs(t)
	a, _ := New(d, vmP)
	create(t, d, a, nil)
	b, err := ReadVM(d, vmP)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Update(t.Context(), func(v *VM) error { return SetSection(v, "b", counter{N: 1}) }); err != nil {
		t.Fatal(err)
	}
	if err := a.Update(t.Context(), func(v *VM) error { v.Alias = "devvm-p"; return SetSection(v, "a", counter{N: 2}) }); err != nil {
		t.Fatal(err)
	}
	ca, _ := Section[counter](a, "a")
	cb, _ := Section[counter](a, "b")
	if ca.N != 2 || cb.N != 1 || a.Alias != "devvm-p" {
		t.Errorf("after both: a=%d b=%d alias=%q", ca.N, cb.N, a.Alias)
	}
	before := read(t, d.VM(vmP).State())
	boom := errors.New("boom")
	err = a.Update(t.Context(), func(v *VM) error {
		v.Dir = "/elsewhere"
		if err := SetSection(v, "a", counter{N: 9}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("got %v, want boom", err)
	}
	if got := read(t, d.VM(vmP).State()); got != before {
		t.Errorf("a failed change wrote:\n%s", got)
	}
	if ca, _ := Section[counter](a, "a"); a.Dir != "/p" || ca.N != 2 {
		t.Errorf("v after a failed change: dir %q, a=%d", a.Dir, ca.N)
	}
}

// add appends who to the VM's section "count" n times, each in an Update
// of its own, as devvms changing one VM at once do.
func add(ctx context.Context, d paths.Dirs, name vmname.VM, who string, n int, errs chan<- error) {
	for i := range n {
		v, err := ReadVM(d, name)
		if err == nil {
			err = v.Update(ctx, func(v *VM) error {
				c, err := Section[counter](v, "count")
				if err != nil {
					return err
				}
				c.N++
				c.Seen = append(c.Seen, who+strconv.Itoa(i))
				return SetSection(v, "count", c)
			})
		}
		if err != nil {
			errs <- err
			return
		}
	}
}

// Two devvms changing one VM at once lose neither's changes, and two
// changing two VMs in crossed order never deadlock: a change holds one
// VM's lock at a time.
func TestVMUpdateTogether(t *testing.T) {
	d := testDirs(t)
	const other vmname.VM = "devvm-q-00000000000001"
	for _, name := range []vmname.VM{vmP, other} {
		v, _ := New(d, name)
		create(t, d, v, nil)
	}
	const n = 25
	errs := make(chan error, 4)
	var wg sync.WaitGroup
	for _, w := range []struct {
		who           string
		first, second vmname.VM
	}{{"a", vmP, other}, {"b", other, vmP}} {
		wg.Go(func() {
			for i := range n {
				add(t.Context(), d, w.first, w.who, 1, errs)
				add(t.Context(), d, w.second, w.who+"x"+strconv.Itoa(i), 1, errs)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	for _, name := range []vmname.VM{vmP, other} {
		v, err := ReadVM(d, name)
		if err != nil {
			t.Fatal(err)
		}
		c, _ := Section[counter](v, "count")
		if c.N != 2*n || len(c.Seen) != 2*n {
			t.Errorf("%s: %d changes, %d recorded, want %d", name, c.N, len(c.Seen), 2*n)
		}
	}
}

// A VM's folder deleted while another devvm changes its state waits for
// that change, so it lands before the folder goes, never after, and the
// state of a VM made again in the folder survives it; a missing folder
// is no error.
func TestRemoveFolder(t *testing.T) {
	d := testDirs(t)
	if err := RemoveFolder(t.Context(), d, vmP); err != nil {
		t.Fatalf("no folder: %v", err)
	}
	v, _ := New(d, vmP)
	create(t, d, v, nil)
	old := v.ID
	inside, release := make(chan struct{}), make(chan struct{})
	updated := make(chan error, 1)
	go func() {
		updated <- v.Update(t.Context(), func(v *VM) error {
			close(inside)
			<-release
			v.Image = "stale"
			return nil
		})
	}()
	<-inside
	removed := make(chan error, 1)
	go func() { removed <- RemoveFolder(t.Context(), d, vmP) }()
	select {
	case err := <-removed:
		t.Fatalf("removed while another devvm changes its state: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if err := <-updated; err != nil {
		t.Fatal(err)
	}
	if err := <-removed; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(d.VM(vmP).Dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("its folder stayed: %v", err)
	}
	again, _ := New(d, vmP)
	create(t, d, again, nil)
	if got, err := ReadVM(d, vmP); err != nil || got.ID == old || got.Image != "base" {
		t.Errorf("the VM made again: %+v, %v", got, err)
	}
}

// Claims are made one at a time, across VMs: two VMs claiming at once
// never choose the same value, as two VMs made at once never get one ssh
// alias.
func TestVMClaim(t *testing.T) {
	d := testDirs(t)
	names := []vmname.VM{"devvm-a-00000000000001", "devvm-b-00000000000002", "devvm-c-00000000000003", "devvm-d-00000000000004"}
	var wg sync.WaitGroup
	errs := make(chan error, len(names))
	for _, name := range names {
		wg.Go(func() {
			v, err := New(d, name)
			if err == nil {
				v.Image, v.Dir, v.Backend = "base", "/p", "lima"
				err = v.Create(t.Context(), func(v *VM) error {
					// The lowest number no other VM has.
					taken := map[string]bool{}
					all, err := VMNames(d)
					if err != nil {
						return err
					}
					for _, n := range all {
						o, err := ReadVM(d, n)
						if err != nil {
							return err
						}
						taken[o.Alias] = true
					}
					time.Sleep(5 * time.Millisecond) // a window for another to choose the same
					for i := 0; ; i++ {
						if a := "alias" + strconv.Itoa(i); !taken[a] {
							v.Alias = a
							return nil
						}
					}
				})
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	for _, name := range names {
		v, err := ReadVM(d, name)
		if err != nil || seen[v.Alias] {
			t.Errorf("%s: alias %q taken twice (%v)", name, v.Alias, err)
		}
		seen[v.Alias] = true
	}
	// Claim changes a VM that exists under both locks.
	v, _ := ReadVM(d, names[0])
	if err := v.Claim(t.Context(), func(v *VM) error { return SetSection(v, "count", counter{N: 1}) }); err != nil {
		t.Fatal(err)
	}
	if c, _ := Section[counter](v, "count"); c.N != 1 {
		t.Errorf("after Claim: %+v", c)
	}
}

func TestVMMissing(t *testing.T) {
	d := testDirs(t)
	v, err := ReadVM(d, "devvm-none-00000000000000")
	if err != nil {
		t.Fatal(err)
	}
	if v.Exists() || v.Image != "" || v.Finished() {
		t.Errorf("missing VM: exists=%v image=%q", v.Exists(), v.Image)
	}
	if err := v.Update(t.Context(), func(*VM) error { return nil }); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Update of none: %v, want ErrNotExist", err)
	}
	if _, err := os.Stat(d.VM(v.Name()).State()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Update made a state file: %v", err)
	}
	if err := v.Remove(t.Context()); err != nil {
		t.Errorf("removing none: %v", err)
	}
	// One that's made, then removed, is gone, and removing it again is no
	// error.
	n, _ := New(d, v.Name())
	create(t, d, n, nil)
	if names, err := VMNames(d); err != nil || !slices.Equal(names, []vmname.VM{v.Name()}) {
		t.Errorf("VMNames = %q, %v", names, err)
	}
	if n.Finished() {
		t.Error("a VM without Created is finished")
	}
	for range 2 {
		if err := n.Remove(t.Context()); err != nil || n.Exists() {
			t.Errorf("Remove: %v, exists %v", err, n.Exists())
		}
	}
	if _, err := os.Stat(d.VM(v.Name()).State()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("after Remove: %v", err)
	}
}

// vmJSON is a minimal vm.json for name.
func vmJSON(name vmname.VM) string {
	return fmt.Sprintf(`{"version": 1, "id": "x", "name": %q, "image": "base", "dir": "/p", "backend": "lima"}`, name)
}

func TestVMNames(t *testing.T) {
	d := testDirs(t)
	if names, err := VMNames(d); err != nil || names != nil {
		t.Fatalf("no state folder: %v, %v", names, err)
	}
	// Each VM's folder records its name; one named by another hash, a
	// folder without state, a file and a folder not named by a hash
	// aren't VMs.
	write(t, d.VM("devvm-b-00000000000001").State(), vmJSON("devvm-b-00000000000001"))
	write(t, d.VM("devvm-a-00000000000002").State(), vmJSON("devvm-a-00000000000002"))
	write(t, d.VM("devvm-a-00000000000002").Policy(), "example.com\n")
	write(t, filepath.Join(d.VMDir("00000000000003"), "lock"), "")
	write(t, filepath.Join(d.VMs(), "00000000000004"), vmJSON("devvm-c-00000000000004"))
	write(t, filepath.Join(d.VMs(), "folder", "vm.json"), vmJSON("devvm-d-00000000000005"))
	names, err := VMNames(d)
	want := []vmname.VM{"devvm-a-00000000000002", "devvm-b-00000000000001"}
	if err != nil || !reflect.DeepEqual(names, want) {
		t.Errorf("VMNames = %v, %v; want %v", names, err, want)
	}
}

// ReadAll reads each VM folder's state on its own: one that can't be read
// is its folder's error, and one whose name isn't its folder's is a
// CorruptError with its state, which VMNames still names; the rest are
// read.
func TestReadAll(t *testing.T) {
	d := testDirs(t)
	const ok, bad, moved vmname.VM = "devvm-a-00000000000001", "devvm-b-00000000000002", "devvm-c-00000000000003"
	write(t, d.VM(ok).State(), vmJSON(ok))
	write(t, d.VM(bad).State(), "[]")
	write(t, filepath.Join(d.VMDir("00000000000009"), "vm.json"), vmJSON(moved))
	all, err := ReadAll(d)
	if err != nil || len(all) != 3 {
		t.Fatalf("ReadAll = %+v, %v", all, err)
	}
	for _, r := range all {
		switch r.Hash {
		case "00000000000001":
			if r.Err != nil || r.VM.Name() != ok {
				t.Errorf("readable: %+v", r)
			}
		case "00000000000002":
			if r.VM != nil {
				t.Errorf("unreadable: %+v", r)
			}
			wantCorrupt(t, r.Err, d.VM(bad).State(), 0)
		case "00000000000009":
			if r.VM == nil || r.VM.Name() != moved {
				t.Errorf("moved: %+v", r)
			}
			wantCorrupt(t, r.Err, filepath.Join(d.VMDir("00000000000009"), "vm.json"), 0)
		}
	}
	if err := os.Remove(d.VM(bad).State()); err != nil {
		t.Fatal(err)
	}
	if names, err := VMNames(d); err != nil || !slices.Equal(names, []vmname.VM{ok, moved}) {
		t.Errorf("VMNames = %v, %v", names, err)
	}
	// Only the VMs' folder failing to be read fails it.
	if err := os.RemoveAll(d.VMs()); err != nil {
		t.Fatal(err)
	}
	write(t, d.VMs(), "")
	if _, err := ReadAll(d); err == nil {
		t.Error("ReadAll of a file for the VMs' folder: want an error")
	}
}

// The state an older release left for a folder whose name has separators
// in a row, recording a name with dashes in a row that Lima refused, is
// no VM's: the folder's VM by the name it gets now reads none, and makes
// its own. One that says it was made is corrupt, as no backend made it.
func TestVMWithRuns(t *testing.T) {
	const old, now vmname.VM = "devvm-x--y-00000000000001", "devvm-x-y-00000000000001"
	d := testDirs(t)
	path := filepath.Join(d.VMDir("00000000000001"), "vm.json")
	write(t, path, vmJSON(old))
	for _, read := range []func() (*VM, error){
		func() (*VM, error) { return ReadHash(d, "00000000000001") },
		func() (*VM, error) { return ReadVM(d, now) },
	} {
		if v, err := read(); err != nil || v.Exists() {
			t.Errorf("an unmade VM's state: exists=%v, %v", v != nil && v.Exists(), err)
		}
	}
	if all, err := ReadAll(d); err != nil || len(all) != 0 {
		t.Errorf("ReadAll = %+v, %v; want none", all, err)
	}
	n, err := New(d, now)
	if err != nil {
		t.Fatal(err)
	}
	create(t, d, n, nil)
	if v, err := ReadVM(d, now); err != nil || !v.Exists() || v.Name() != now {
		t.Errorf("the VM made in its place: %+v, %v", v, err)
	}
	write(t, path, strings.Replace(vmJSON(old), "}", `, "created": "2026-01-01T00:00:00Z"}`, 1))
	_, err = ReadHash(d, "00000000000001")
	wantCorrupt(t, err, path, 0)
}

// A state file that isn't a JSON object, lacks a core field or has a
// section of the wrong shape is a CorruptError naming it; VMNames stops
// at it.
func TestVMCorrupt(t *testing.T) {
	path := func(d paths.Dirs) string { return d.VM(vmP).State() }
	for _, data := range []string{
		"name=devvm-p-00000000000000\n", "[]", "null", `{"version": "1"}`,
		strings.Replace(vmJSON(vmP), `"version": 1`, `"version": 0`, 1),
		strings.Replace(vmJSON(vmP), `"id": "x"`, `"id": ""`, 1),
		strings.Replace(vmJSON(vmP), string(vmP), "vm", 1),
		strings.Replace(vmJSON(vmP), `"image": "base"`, `"other": 1`, 1),
		strings.Replace(vmJSON(vmP), `"dir": "/p"`, `"dir": ""`, 1),
		strings.Replace(vmJSON(vmP), `"backend": "lima"`, `"backend": ""`, 1),
	} {
		d := testDirs(t)
		write(t, path(d), data)
		_, err := ReadVM(d, vmP)
		wantCorrupt(t, err, path(d), 0)
		_, err = VMNames(d)
		wantCorrupt(t, err, path(d), 0)
	}
	d := testDirs(t)
	write(t, path(d), strings.Replace(vmJSON(vmP), "}", `, "count": "three"}`, 1))
	v, err := ReadVM(d, vmP)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Section[counter](v, "count")
	wantCorrupt(t, err, path(d), 0)
}

// What can't be done fails and writes nothing: a claim that fails, a VM
// folder or claims lock where a file is, a state file that's a folder, a
// folder that can't be written, a section that can't be encoded.
func TestVMFailures(t *testing.T) {
	d := testDirs(t)
	boom := errors.New("boom")
	v, _ := New(d, vmP)
	v.Image, v.Dir, v.Backend = "base", "/p", "lima"
	if err := v.Create(t.Context(), func(*VM) error { return boom }); !errors.Is(err, boom) {
		t.Errorf("Create with a failed claim: %v", err)
	}
	if _, err := os.Stat(d.VM(vmP).State()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a failed claim wrote: %v", err)
	}
	if err := SetSection(v, "count", make(chan int)); err == nil {
		t.Error("a section that isn't JSON: want an error")
	}
	// The claims lock's folder is a file.
	if err := os.RemoveAll(d.Run()); err != nil {
		t.Fatal(err)
	}
	write(t, d.Run(), "")
	if err := v.Create(t.Context(), func(*VM) error { return nil }); err == nil {
		t.Error("Create without a claims lock: want an error")
	}
	if err := os.Remove(d.Run()); err != nil {
		t.Fatal(err)
	}
	create(t, d, v, nil)
	// A state that can't be encoded (a time JSON can't hold): the change
	// fails, and the file stays.
	before := read(t, d.VM(vmP).State())
	err := v.Update(t.Context(), func(v *VM) error { v.Created = time.Date(10001, 1, 1, 0, 0, 0, 0, time.UTC); return nil })
	if err == nil || read(t, d.VM(vmP).State()) != before || !v.Created.IsZero() {
		t.Errorf("Update to a state JSON can't hold: %v, created %v", err, v.Created)
	}
	// A VM folder where a file is.
	const other = "devvm-q-00000000000001"
	write(t, d.VMDir("00000000000001"), "")
	o, _ := New(d, other)
	o.Image, o.Dir, o.Backend = "base", "/p", "lima"
	if err := o.Create(t.Context(), nil); err == nil {
		t.Error("Create where a file is: want an error")
	}
	// A state file that's a folder can't be read.
	const third = "devvm-r-00000000000002"
	write(t, filepath.Join(d.VM(third).State(), "f"), "")
	if _, err := ReadVM(d, third); err == nil {
		t.Error("ReadVM of a folder: want an error")
	}
	if _, err := VMNames(d); err == nil {
		t.Error("VMNames with a folder for a state: want an error")
	}
	r, _ := New(d, third)
	if err := r.Remove(t.Context()); err == nil {
		t.Error("Remove of a folder with files: want an error")
	}
}

func TestVMRefuses(t *testing.T) {
	d := testDirs(t)
	for _, name := range []vmname.VM{"", ".", "..", ".hidden", "a/b", "../x", "vm", "devvm-img-0123456789ab", "devvm-tools-build", "devvm-p/../00000000000000"} {
		if _, err := ReadVM(d, name); err == nil {
			t.Errorf("ReadVM(%q): want an error", name)
		}
		if _, err := New(d, name); err == nil {
			t.Errorf("New(%q): want an error", name)
		}
	}
	// A VM's state can't be changed before it's made.
	v, _ := New(d, vmP)
	if err := v.Update(t.Context(), func(*VM) error { return nil }); err == nil {
		t.Error("Update before Create: want an error")
	}
}
