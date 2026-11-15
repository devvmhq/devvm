package vm

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/gitprotect"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/project"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/vmname"
)

// A move records the path the agents' state is under: the first move's
// old path, kept through more moves until a setup follows it, and none
// once the folder is back there.
func TestMovedTo(t *testing.T) {
	for _, c := range []struct {
		last     movedInVM
		was, dir string
		want     movedInVM
	}{
		{movedInVM{}, "/a", "/b", movedInVM{From: "/a"}},
		{movedInVM{From: "/a"}, "/b", "/c", movedInVM{From: "/a"}},
		{movedInVM{From: "/a"}, "/b", "/a", movedInVM{}},
		{movedInVM{}, "/a", "/a", movedInVM{}},
	} {
		if got := movedTo(c.last, c.was, c.dir); got != c.want {
			t.Errorf("movedTo(%+v, %s, %s) = %+v, want %+v", c.last, c.was, c.dir, got, c.want)
		}
	}
}

// A VM's folder moved to h when it's h's folder itself (its identity, the
// device in it included), unique and this user's, at another path, with
// nothing left at the old one; its creation finished and it mounts its
// folder. A copy, a folder made again, an inode number used again and
// another user's folder never match.
func TestMovedFrom(t *testing.T) {
	e, _, _ := upEnv(t, allNative())
	made := existingVM(t, e, Options{Network: "open"})
	clone, err := state.ReadVM(e.Dirs, made.Name()) // its own sections
	if err != nil {
		t.Fatal(err)
	}
	if err := setField(clone, keyClone, "clone"); err != nil {
		t.Fatal(err)
	}
	unfinished := *made
	unfinished.Created = time.Time{}
	h := here{dir: "/new/web", id: made.Folder, unique: true, mine: true}
	gone := func(*state.VM, string) bool { return true }
	there := func(*state.VM, string) bool { return false }
	other := func(st *state.VM, id string) *state.VM { c := *st; c.Folder = id; return &c }
	at := func(st *state.VM, dir string) *state.VM { c := *st; c.Dir = dir; return &c }
	dev := platform.FolderDevice(made.Folder)
	for _, tc := range []struct {
		name string
		h    here
		st   *state.VM
		away func(*state.VM, string) bool
		want bool
	}{
		{"moved: the same folder, the old path gone", h, made, gone, true},
		{"the old folder still there", h, made, there, false},
		{"a copy: another inode", h, other(made, dev+":999999999"), gone, false},
		{"the same inode on another device", h, other(made, "1"+made.Folder), gone, false},
		{"an identity that may be a deleted folder's", here{dir: h.dir, id: h.id, mine: true}, made, gone, false},
		{"another user's folder", here{dir: h.dir, id: h.id, unique: true}, made, gone, false},
		{"no identity", here{dir: h.dir, unique: true, mine: true}, other(made, ""), gone, false},
		{"already this folder's", h, at(made, h.dir), gone, false},
		{"no folder recorded", h, at(made, ""), gone, false},
		{"a --clone VM", h, clone, gone, false},
		{"its creation didn't finish", h, &unfinished, gone, false},
	} {
		if got := tc.h.movedFrom(tc.st, tc.away); got != tc.want {
			t.Errorf("%s: movedFrom = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A folder moved away when nothing is at its old path on its disk, or a
// symlink there leads to where it is now; never when a folder (made
// again, or the same) is there, or a symlink leads elsewhere.
func TestMovedAway(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	to, elsewhere := filepath.Join(root, "new"), filepath.Join(root, "other")
	for _, d := range []string{to, elsewhere} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	id, err := platform.FolderID(to)
	if err != nil {
		t.Fatal(err)
	}
	link := func(name, target string) string {
		p := filepath.Join(root, name)
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, tc := range []struct {
		name, dir string
		want      bool
	}{
		{"nothing at the old path", filepath.Join(root, "old"), true},
		{"a folder at the old path", elsewhere, false},
		{"a symlink to where it is now", link("tonew", to), true},
		{"a symlink elsewhere", link("toother", elsewhere), false},
		{"a symlink to nothing", link("dangling", filepath.Join(root, "none")), false},
	} {
		st := &state.VM{Dir: tc.dir, Folder: id}
		if got := movedAway(st, to); got != tc.want {
			t.Errorf("%s: movedAway = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// moveFolder renames the folder e runs in to name, beside it, and runs
// there; it returns the old path and the new.
func moveFolder(t *testing.T, e *Env, name string) (was, now string) {
	t.Helper()
	was, now = e.Dir, filepath.Join(filepath.Dir(e.Dir), name)
	if err := os.Rename(was, now); err != nil {
		t.Fatal(err)
	}
	e.Dir = now
	return was, now
}

// A folder moved or renamed keeps its VM, only after a yes in a terminal:
// without one devvm refuses, naming devvm adopt; a no leaves the VM where
// it was and the folder gets a new one; a yes records the folder in the
// VM's state, from which the finder finds it, and its next start mounts
// the folder where it is now, a running VM restarted for it after
// asking. A new folder at the old path gets a VM of its own, by its next
// name.
func TestMoveHere(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	st := existingVM(t, e, Options{Network: "open"})
	vm := st.Name()
	was, now := moveFolder(t, e, "site")
	fd := finder{e}
	if _, err := fd.target(t.Context(), need{}); msg.NamesOf(err) != "vm_moved_here vm="+vm.String()+" was="+was {
		t.Fatalf("no terminal: %v", msg.NamesOf(err))
	}
	asked := answers(t, e, false)
	tg, err := fd.target(t.Context(), need{})
	if err != nil || tg.exists || tg.f.Name == vm || !slices.Equal(*asked, []string{"move_vm vm=" + vm.String() + " was=" + was}) {
		t.Fatalf("a no: %+v, %v, asked %q", tg, msg.NamesOf(err), *asked)
	}
	if got, _ := state.ReadVM(e.Dirs, vm); got.Dir != was {
		t.Errorf("a no moved it: %s", got.Dir)
	}
	var infos []string
	fake(e).OnInfo = func(m msg.M) { infos = append(infos, m.Names()) }
	answers(t, e, true)
	if tg, err = fd.target(t.Context(), need{}); err != nil || tg.f.Name != vm || !tg.moved || tg.f.Dir != now {
		t.Fatalf("a yes: %+v, %v", tg, msg.NamesOf(err))
	}
	if !slices.Contains(infos, "vm_adopted dir="+now+" vm="+vm.String()) {
		t.Errorf("said %q", infos)
	}
	moved, err := state.ReadVM(e.Dirs, vm)
	if id, _ := platform.FolderID(now); err != nil || moved.Dir != now || moved.Folder != id {
		t.Fatalf("its state: %+v, %v", moved, err)
	}
	if m := sectionOf[movedInVM](moved, movedSection); m.From != was {
		t.Errorf("recorded it moved from %q, want %s", m.From, was)
	}
	// Found by the folder its state records, asking nothing.
	asked = answers(t, e, false)
	if tg, err = fd.target(t.Context(), need{has: hasMade}); err != nil || tg.f.Name != vm || tg.moved || len(*asked) != 0 {
		t.Fatalf("found again: %+v, %v, asked %q", tg, msg.NamesOf(err), *asked)
	}
	// It ran, mounting the old path: the move stopped it, and its next
	// start mounts the new one.
	if in := f.Instances[vm]; in.Status != backend.StatusStopped || !slices.Contains(infos, "stopped vm="+vm.String()) {
		t.Errorf("moved while running: %s, said %q", in.Status, infos)
	}
	if err := (Launch{e}).Up(t.Context(), Options{}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if w := call(f, "Start").Args[1].(backend.Want); w.Project != now {
		t.Errorf("started with %q mounted, want %s", w.Project, now)
	}
	// Its setup moved the agents' state from the old path, and the
	// record is gone.
	if got := staged(t, f)["moved"]; got == "" || got != string(claudeMoved(was, now)) {
		t.Errorf("staged moved = %q", got)
	}
	if st, _ := state.ReadVM(e.Dirs, vm); sectionOf[movedInVM](st, movedSection) != (movedInVM{}) {
		t.Errorf("still recorded: %+v", sectionOf[movedInVM](st, movedSection))
	}
	// Running with the old path mounted all the same: up restarts it for
	// the new one, after asking, yes by default.
	in := f.Instances[vm]
	in.Project = was
	f.Instances[vm] = in
	asked = answers(t, e, true)
	if err := (Launch{e}).Up(t.Context(), Options{}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if !slices.Contains(*asked, "restart_now count=1 vm="+vm.String()) || f.Instances[vm].Project != now {
		t.Errorf("asked %q, mounts %q", *asked, f.Instances[vm].Project)
	}
	if got, ok := staged(t, f)["moved"]; ok {
		t.Errorf("moved again: %q", got)
	}
	// A new folder at the old path: a VM of its own, by its next name.
	if err := os.Mkdir(was, 0o700); err != nil {
		t.Fatal(err)
	}
	e.Dir = was
	if tg, err = fd.target(t.Context(), need{}); err != nil || tg.exists || tg.st.Exists() {
		t.Fatalf("the old path: %+v, %v", tg, msg.NamesOf(err))
	}
	second, _ := project.Nth(was, f.NameMax(), 1)
	if tg.f.Name != second.Name {
		t.Errorf("the old path's new VM is %s, want %s", tg.f.Name, second.Name)
	}
	if err := (Launch{e}).Up(t.Context(), Options{Network: "open"}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	for dir, want := range map[string]vmname.VM{was: second.Name, now: vm} {
		e.Dir = dir
		if tg, err := fd.target(t.Context(), need{has: hasMade}); err != nil || tg.f.Name != want {
			t.Errorf("%s's VM: %+v, %v, want %s", dir, tg, msg.NamesOf(err), want)
		}
	}
}

// A folder replaced at the new path while devvm asked isn't the one asked
// about: nothing moves.
func TestMoveFolderChanged(t *testing.T) {
	e, _, _ := upEnv(t, allNative())
	vm := existingVM(t, e, Options{Network: "open"}).Name()
	was, now := moveFolder(t, e, "site")
	answers(t, e, true)
	fake(e).OnAsk = func(msg.M) bool {
		if err := os.Rename(now, now+".x"); err != nil {
			t.Fatal(err)
		}
		return os.Mkdir(now, 0o700) == nil
	}
	if _, err := (finder{e}).target(t.Context(), need{}); msg.NamesOf(err) != "move_folder_changed dir="+now+" vm="+vm.String() {
		t.Errorf("replaced while asked: %v", msg.NamesOf(err))
	}
	if st, _ := state.ReadVM(e.Dirs, vm); st.Dir != was {
		t.Errorf("moved to %s", st.Dir)
	}
}

// A folder moved where devvm makes no new VM, inside devvm's own config
// folder, gets no VM moved to it, even with devvm adopt --force.
func TestMoveUnsafe(t *testing.T) {
	e, _, _ := upEnv(t, allNative())
	st := existingVM(t, e, Options{Network: "open"})
	was := e.Dir
	now := filepath.Join(e.Dirs.Config, "site")
	if err := os.MkdirAll(e.Dirs.Config, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(was, now); err != nil {
		t.Fatal(err)
	}
	e.Dir = now
	if err := (Lifecycle{e}).Adopt(t.Context(), "", true); msg.IDOf(err) != "unsafe_folder_inside" {
		t.Errorf("adopt --force inside the config folder: %v", msg.NamesOf(err))
	}
	if got, _ := state.ReadVM(e.Dirs, st.Name()); got.Dir != was {
		t.Errorf("moved to %s", got.Dir)
	}
}

// devvm adopt --force moves a VM whose folder moved here without asking,
// a terminal or not; adopt alone asks, as any command does.
func TestAdoptMoved(t *testing.T) {
	e, _, _ := upEnv(t, allNative())
	vm := existingVM(t, e, Options{Network: "open"}).Name()
	_, now := moveFolder(t, e, "site")
	lc := Lifecycle{e}
	if err := lc.Adopt(t.Context(), "", false); msg.IDOf(err) != "vm_moved_here" {
		t.Errorf("adopt without a terminal: %v", msg.NamesOf(err))
	}
	if err := lc.Adopt(t.Context(), "", true); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if st, _ := state.ReadVM(e.Dirs, vm); st.Dir != now {
		t.Errorf("adopt --force: %s's folder is %s", vm, st.Dir)
	}
}

// Two VMs made for one folder at paths it had before: devvm moves
// neither, and warns.
func TestMoveHereSeveral(t *testing.T) {
	e, _, _ := upEnv(t, allNative())
	first := existingVM(t, e, Options{Network: "open"})
	// The first one declined at the folder's second path, which got a
	// VM of its own; then the folder moved again.
	moveFolder(t, e, "two")
	answers(t, e, false)
	if err := (Launch{e}).Up(t.Context(), Options{Network: "open"}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	moveFolder(t, e, "three")
	var warned []string
	fake(e).OnWarn = func(m msg.M) { warned = append(warned, msg.IDOf(m)) }
	tg, err := (finder{e}).target(t.Context(), need{})
	if err != nil || tg.exists || !slices.Contains(warned, "vms_moved_here") {
		t.Errorf("several: %+v, %v, warned %q", tg, msg.NamesOf(err), warned)
	}
	if st, _ := state.ReadVM(e.Dirs, first.Name()); st.Dir == e.Dir {
		t.Error("moved one of several")
	}
}

// devvm adopt VM moves a VM whose folder is gone to this folder, a copy
// of it (another folder), after asking, its .git protection started
// again; it refuses a VM whose folder is there, a --clone VM, and a
// folder with a VM of its own.
func TestAdoptVM(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	st := existingVM(t, e, Options{Network: "open"})
	vm, web := st.Name(), e.Dir
	change(t, st, func(st *state.VM) error {
		g, err := gitprotect.Of(st)
		g.Made = gitprotect.Made{Protected: true, GitID: "1:2"}
		return errors.Join(err, gitprotect.Set(st, g))
	})
	cp := filepath.Join(filepath.Dir(web), "copy")
	if err := os.Mkdir(cp, 0o700); err != nil {
		t.Fatal(err)
	}
	e.Dir = cp
	lc := Lifecycle{e}
	if err := lc.Adopt(t.Context(), vm.String(), true); msg.NamesOf(err) != "adopt_folder_there dir="+web+" vm="+vm.String() {
		t.Errorf("its folder there: %v", msg.NamesOf(err))
	}
	if err := os.RemoveAll(web); err != nil {
		t.Fatal(err)
	}
	if err := lc.Adopt(t.Context(), vm.String(), false); msg.NamesOf(err) != "adopt_move_refused vm="+vm.String() {
		t.Errorf("no terminal: %v", msg.NamesOf(err))
	}
	asked := answers(t, e, false)
	if err := lc.Adopt(t.Context(), vm.String(), false); err != nil || !slices.Equal(*asked, []string{"adopt_move_vm vm=" + vm.String() + " was=" + web}) {
		t.Errorf("a no: %v, asked %q", msg.NamesOf(err), *asked)
	}
	if got, _ := state.ReadVM(e.Dirs, vm); got.Dir != web {
		t.Errorf("a no moved it to %s", got.Dir)
	}
	answers(t, e, true)
	if err := lc.Adopt(t.Context(), vm.String(), false); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	got, _ := state.ReadVM(e.Dirs, vm)
	g, _ := gitprotect.Of(got)
	if id, _ := platform.FolderID(cp); got.Dir != cp || got.Folder != id || g.Made.Protected || g.Made.GitID != "" {
		t.Errorf("adopted: %s %s, .git %+v", got.Dir, got.Folder, g.Made)
	}
	// Here already: nothing to do.
	if err := lc.Adopt(t.Context(), vm.String(), true); err != nil {
		t.Errorf("adopt of this folder's VM: %v", msg.NamesOf(err))
	}
	// Another VM to a folder with a VM of its own; a --clone VM.
	other := filepath.Join(filepath.Dir(web), "other")
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	e.Dir = other
	if err := (Launch{e}).Up(t.Context(), Options{Network: "open"}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	ovm, _ := (finder{e}).folderVM(other)
	e.Dir = cp
	if err := lc.Adopt(t.Context(), ovm.String(), true); msg.NamesOf(err) != "adopt_folder_there dir="+other+" vm="+ovm.String() {
		t.Errorf("another's: %v", msg.NamesOf(err))
	}
	if err := os.RemoveAll(other); err != nil {
		t.Fatal(err)
	}
	if err := lc.Adopt(t.Context(), ovm.String(), true); msg.NamesOf(err) != "adopt_has_vm vm="+vm.String() {
		t.Errorf("a folder with its own: %v", msg.NamesOf(err))
	}
	ost, _ := state.ReadVM(e.Dirs, ovm)
	if err := set(t, ost, keyClone, "clone"); err != nil {
		t.Fatal(err)
	}
	if err := lc.Adopt(t.Context(), ovm.String(), true); msg.NamesOf(err) != "adopt_clone dir="+other+" vm="+ovm.String() {
		t.Errorf("a --clone VM: %v", msg.NamesOf(err))
	}
	if _, ok := f.Instances[ovm]; !ok {
		t.Error("refused VM deleted")
	}
}
