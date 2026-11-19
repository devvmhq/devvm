package vm

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/gitprotect"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/project"
	"github.com/devvmhq/devvm/internal/secrets"
	"github.com/devvmhq/devvm/internal/sshagent"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/vmname"
)

// subfolder makes dir (relative to the project folder web) and runs e in
// it.
func subfolder(t *testing.T, e *Env, web, dir string) string {
	t.Helper()
	sub := filepath.Join(web, dir)
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	e.Dir = sub
	return sub
}

// lookupVM is the VM the finder finds for the folder e runs in, a parent's
// (its folder and the subfolder it's for), else "" for the folder's own.
func lookupVM(t *testing.T, e *Env) (dir, sub string) {
	t.Helper()
	got, err := (finder{e}).target(t.Context(), need{})
	if err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if got.sub == "" {
		if got.f.Dir != e.Dir {
			t.Errorf("own VM = %+v", got.f)
		}
		return "", ""
	}
	return got.f.Dir, got.sub
}

// fakeParent is a parent folder's VM for resolveFolder: web's, below a
// repo of its own when repo isn't "".
func fakeParent(repo string) *target {
	return &target{f: project.Folder{Dir: "/w/web"}, sub: "/w/web/src", repo: repo}
}

// resolveFolder decides own, parent or none: the folder's own wins by
// default, then a parent's unless a repo of its own is in between; --here
// never looks up, and --parent takes the parent's past the folder's own
// and across a repo, refusing a folder inside none with a VM.
// TestFolderChanged: a nested project folder that a parent folder's VM
// replaced, as a link, below one, or another folder, is changed; the
// folder its state records is not.
func TestFolderChanged(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mk := func(p string) string {
		t.Helper()
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		return p
	}
	decoy := mk(filepath.Join(root, "decoy", "inner"))
	stateOf := func(dir string) *state.VM {
		t.Helper()
		id, err := platform.FolderID(dir)
		if err != nil {
			t.Fatal(err)
		}
		return vmAt(dir, id, "")
	}
	inner := mk(filepath.Join(root, "app", "inner"))
	same := stateOf(inner)
	if folderChanged(same) {
		t.Error("its own folder: changed")
	}
	for _, tc := range []struct {
		name    string
		replace func(dir string) error
	}{
		{"a link in its place", func(dir string) error {
			return errors.Join(os.Rename(dir, dir+".real"), os.Symlink(decoy, dir))
		}},
		{"a link above it", func(dir string) error {
			app := filepath.Dir(dir)
			return errors.Join(os.Rename(app, app+".real"), os.Symlink(filepath.Dir(decoy), app))
		}},
		{"another folder in its place", func(dir string) error {
			return errors.Join(os.Rename(dir, dir+".real"), os.Mkdir(dir, 0o700))
		}},
		{"gone", func(dir string) error { return os.Rename(dir, dir+".real") }},
	} {
		dir := mk(filepath.Join(root, tc.name, "app", "inner"))
		st := stateOf(dir)
		if err := tc.replace(dir); err != nil {
			t.Fatal(err)
		}
		if !folderChanged(st) {
			t.Errorf("%s: not changed", tc.name)
		}
	}
}

func TestResolveFolder(t *testing.T) {
	for _, tc := range []struct {
		name   string
		own    bool
		parent *target // nil: no parent folder has a VM
		rule   FolderRule
		want   folderUse
		looked bool   // parent was asked for
		err    string // the error's names
	}{
		{name: "own", own: true, parent: fakeParent(""), want: useOwn},
		{name: "parent", parent: fakeParent(""), want: useParent, looked: true},
		{name: "none", want: useNone, looked: true},
		{name: "nested repo", parent: fakeParent("/w/web/src"), want: useNone, looked: true},
		{name: "nested repo, its own", own: true, parent: fakeParent("/w/web/src"), want: useOwn},
		{name: "--here with a parent", parent: fakeParent(""), rule: FolderHere, want: useNone},
		{name: "--here, its own", own: true, parent: fakeParent(""), rule: FolderHere, want: useOwn},
		{name: "--parent", parent: fakeParent(""), rule: FolderParent, want: useParent, looked: true},
		{name: "--parent past its own", own: true, parent: fakeParent(""), rule: FolderParent, want: useParent, looked: true},
		{name: "--parent across a repo", parent: fakeParent("/w/web/src"), rule: FolderParent, want: useParent, looked: true},
		{name: "--parent without one", own: true, rule: FolderParent, want: useNone, looked: true, err: "no_parent_vm dir=/w/web/src"},
	} {
		looked := false
		use, p, err := resolveFolder("/w/web/src", tc.own, tc.rule, func() (*target, error) { looked = true; return tc.parent, nil })
		if use != tc.want || looked != tc.looked || msg.NamesOf(err) != tc.err {
			t.Errorf("%s: %v, looked up %v, %v; want %v, %v, %q", tc.name, use, looked, msg.NamesOf(err), tc.want, tc.looked, tc.err)
		}
		if (use == useParent) != (p != nil) || p != nil && p != tc.parent {
			t.Errorf("%s: parent %+v", tc.name, p)
		}
	}
	boom := errors.New("boom")
	if _, _, err := resolveFolder("/w/web/src", false, FolderAuto, func() (*target, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Errorf("a failed lookup: %v", err)
	}
}

// A folder without a VM finds the nearest parent folder's that was made
// for it, stopping before the home folder.
func TestLookupParents(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	web := e.Dir
	existingVM(t, e, Options{Network: "open"})
	lib := subfolder(t, e, web, "src/lib")
	if dir, sub := lookupVM(t, e); dir != web || sub != lib {
		t.Errorf("from src/lib: %q for %q, want web's", dir, sub)
	}
	// The nearest one wins.
	subfolder(t, e, web, "src")
	e.Folder = FolderHere
	if err := (Launch{e}).Up(t.Context(), Options{Network: "open"}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	e.Folder = FolderAuto
	src := e.Dir
	e.Dir = lib
	if dir, _ := lookupVM(t, e); dir != src {
		t.Errorf("from src/lib: %q, want src's", dir)
	}
	// Its own VM wins over both.
	e.Dir = src
	if dir, _ := lookupVM(t, e); dir != "" {
		t.Errorf("from src: %q, want its own", dir)
	}
	// So does its state, when its backend has lost the VM.
	own, err := (finder{e}).target(t.Context(), need{own: true})
	if err != nil {
		t.Fatal(err)
	}
	in := f.Instances[own.f.Name]
	delete(f.Instances, own.f.Name)
	if dir, _ := lookupVM(t, e); dir != "" {
		t.Errorf("from src with its state alone: %q, want its own", dir)
	}
	// The nearest parent with state stops the search when its backend has
	// lost its VM: no VM, never web's.
	e.Dir = lib
	delete(f.Instances, own.f.Name)
	if dir, _ := lookupVM(t, e); dir != "" {
		t.Errorf("past a parent's lost VM: %q, want none", dir)
	}
	f.Instances[own.f.Name] = in
	// And when its VM was made for an earlier folder at its path (a VM can
	// replace a nested folder through its mount): no VM, never web's.
	for _, n := range vmNames(t, e) {
		st, err := state.ReadVM(e.Dirs, n)
		if err != nil {
			t.Fatal(err)
		}
		if st.Dir == src {
			if err := set(t, st, keyFolder, "1:2"); err != nil {
				t.Fatal(err)
			}
		}
	}
	if dir, _ := lookupVM(t, e); dir != "" {
		t.Errorf("past another folder's VM: %q, want none", dir)
	}
	if _, err := (finder{e}).target(t.Context(), need{has: hasVM}); msg.NamesOf(err) != "no_vm dir="+lib {
		t.Errorf("finding one past another folder's VM = %v, want no_vm", msg.NamesOf(err))
	}
	// Never the home folder's, or one above it.
	e.Dirs.Home = web
	if dir, _ := lookupVM(t, e); dir != "" {
		t.Errorf("with web as home: %q", dir)
	}
	// --here: only its own.
	e.Dirs.Home = filepath.Dir(web)
	e.Folder = FolderHere
	if dir, _ := lookupVM(t, e); dir != "" {
		t.Errorf("--here: %q, want its own", dir)
	}
}

// A launch warns once when a folder it's inside has a project VM that
// mounts it (project_in_vm_folder), running or not, at the VM's creation
// and at each launch after it; never for that VM itself, opened in a
// subfolder or not, for a new --clone VM, or when the parent's VM is a
// --clone VM, which mounts nothing.
func TestWarnMounted(t *testing.T) {
	e, _, _ := upEnv(t, allNative())
	web := e.Dir
	webSt := existingVM(t, e, Options{Network: "open"})
	webVM := webSt.Name()
	var warns msg.Recorder
	fake(e).OnWarn = warns.Add
	up := func(what string, o Options, want string) {
		t.Helper()
		warns.Reset()
		err := (Launch{e}).Up(t.Context(), o)
		if !o.Clone && err != nil {
			t.Fatalf("%s: %v", what, msg.NamesOf(err))
		}
		n := 0
		for _, w := range warns.Names() {
			if strings.HasPrefix(w, "project_in_vm_folder ") {
				n++
				if w != want {
					t.Errorf("%s: warned %q, want %q", what, w, want)
				}
			}
		}
		if want != "" && n != 1 || want == "" && n != 0 {
			t.Errorf("%s: warned %d times: %q", what, n, warns.Names())
		}
	}
	up("web's own", Options{}, "")
	if err := (Lifecycle{e}).Stop(t.Context()); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	lib := subfolder(t, e, web, "src/lib")
	e.Folder = FolderHere
	nested := "project_in_vm_folder dir=" + lib + " vm=" + webVM.String()
	up("src/lib's new VM", Options{Network: "open"}, nested)
	up("src/lib's VM again", Options{}, nested)
	e.Folder = FolderAuto
	subfolder(t, e, web, "docs")
	up("web's VM in docs", Options{}, "")
	subfolder(t, e, web, "tests")
	up("a new --clone VM", Options{Network: "open", Clone: true}, "")
	if err := set(t, webSt, KeyMode, ModeClone); err != nil {
		t.Fatal(err)
	}
	e.Dir, e.Folder = lib, FolderHere
	up("below a --clone VM", Options{}, "")
}

func vmNames(t *testing.T, e *Env) []vmname.VM {
	t.Helper()
	ns, err := state.VMNames(e.Dirs)
	if err != nil {
		t.Fatal(err)
	}
	return ns
}

// up in a subfolder opens the parent's VM there, said in an info line,
// with or without a terminal, asking nothing; --here and --clone make a
// new VM, and --parent opens the parent's past the subfolder's own.
func TestUpInSubfolder(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	web := e.Dir
	existingVM(t, e, Options{Network: "open"})
	var infos msg.Recorder
	fake(e).OnInfo = infos.Add
	src := subfolder(t, e, web, "src")
	if err := (Launch{e}).Up(t.Context(), Options{}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	vm := vmName(t, e)
	if c := call(f, "Shell"); c.Args[0] != vm || shellOpts(c).Dir != src {
		t.Errorf("Shell = %v, want %s in %s", c.Args, vm, src)
	}
	if !slices.Contains(infos.Names(), "using_parent_vm dir="+web+" vm="+vm.String()) {
		t.Errorf("infos = %q", infos.Names())
	}
	// In a terminal the same, nothing asked.
	asked := answers(t, e, true)
	infos.Reset()
	if err := (Launch{e}).Up(t.Context(), Options{}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if len(*asked) != 0 || len(vmNames(t, e)) != 1 || !slices.Contains(infos.Names(), "using_parent_vm dir="+web+" vm="+vm.String()) {
		t.Errorf("in a terminal: asked %q, %d VMs, infos %q", *asked, len(vmNames(t, e)), infos.Names())
	}
	// --clone: as --here, a new VM's checks (this subfolder isn't a
	// repo), nothing asked.
	subfolder(t, e, web, "tests")
	if err := (Launch{e}).Up(t.Context(), Options{Network: "open", Clone: true}); err == nil ||
		strings.HasPrefix(msg.NamesOf(err), "clone_needs_new_vm") || len(*asked) != 0 {
		t.Errorf("--clone: %v, asked %q", msg.NamesOf(err), *asked)
	}
	// --here: a new VM, nothing asked.
	docs := subfolder(t, e, web, "docs")
	e.Folder = FolderHere
	if err := (Launch{e}).Up(t.Context(), Options{Network: "open"}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if len(*asked) != 0 || len(vmNames(t, e)) != 2 {
		t.Errorf("--here: asked %q, %d VMs", *asked, len(vmNames(t, e)))
	}
	// --parent: the parent's, past docs' own.
	e.Folder = FolderParent
	if err := (Launch{e}).Up(t.Context(), Options{}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if c := call(f, "Shell"); c.Args[0] != vm || shellOpts(c).Dir != docs || len(vmNames(t, e)) != 2 {
		t.Errorf("--parent: Shell = %v, %d VMs", c.Args, len(vmNames(t, e)))
	}
	// --parent where no folder above has one: refused, nothing made.
	e.Dir = web
	if err := (Launch{e}).Up(t.Context(), Options{}); msg.NamesOf(err) != "no_parent_vm dir="+web {
		t.Errorf("--parent in web = %v", msg.NamesOf(err))
	}
}

// A parent's VM opens in the subfolder; a --clone VM's, in it when its
// clone has it, else at the clone's top.
func TestShellDir(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	web := e.Dir
	existingVM(t, e, Options{Network: "open"})
	src := subfolder(t, e, web, "src")
	p, err := (finder{e}).parentVM(t.Context())
	if err != nil || p == nil {
		t.Fatalf("parentVM = %+v, %v", p, msg.NamesOf(err))
	}
	if got := p.shellDir(t.Context(), io.Discard); got != src {
		t.Errorf("shellDir = %q, want %q", got, src)
	}
	if err := set(t, p.st, KeyMode, ModeClone); err != nil {
		t.Fatal(err)
	}
	var missing bool
	f.ExecFunc = func(_ vmname.VM, argv []string, _ backend.ExecOpts) error {
		if !slices.Equal(argv, []string{"test", "-d", src}) {
			t.Errorf("Exec %q", argv)
		}
		if missing {
			return errors.New("exit status 1")
		}
		return nil
	}
	if got := p.shellDir(t.Context(), io.Discard); got != src {
		t.Errorf("clone's shellDir = %q, want %q", got, src)
	}
	missing = true
	if got := p.shellDir(t.Context(), io.Discard); got != web {
		t.Errorf("clone without it: shellDir = %q, want %q", got, web)
	}
	p.sub = ""
	if got := p.shellDir(t.Context(), io.Discard); got != web {
		t.Errorf("its own: shellDir = %q, want %q", got, web)
	}
}

// Commands on the folder's VM use a parent's, saying so first; adopt
// doesn't.
func TestCommandsInSubfolder(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	web := e.Dir
	existingVM(t, e, Options{Network: "open"})
	vm := vmName(t, e)
	var infos msg.Recorder
	fake(e).OnInfo = infos.Add
	subfolder(t, e, web, "src")
	if err := (Lifecycle{e}).Stop(t.Context()); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if want := []string{"using_parent_vm dir=" + web + " vm=" + vm.String(), "stopped vm=" + vm.String()}; !slices.Equal(infos.Names(), want) {
		t.Errorf("infos = %q, want %q", infos.Names(), want)
	}
	if c := call(f, "Stop"); c.Args[0] != vm {
		t.Errorf("Stop = %v", c.Args)
	}
	if err := (Lifecycle{e}).Adopt(t.Context(), "", false); msg.NamesOf(err) != "no_vm dir="+e.Dir {
		t.Errorf("Adopt = %v", msg.NamesOf(err))
	}
	// rm: refused without a terminal, -f or not; asked in one, a no
	// keeping it.
	infos.Reset()
	if err := (Lifecycle{e}).Rm(t.Context(), RmOptions{Force: true, NoExport: true}); msg.NamesOf(err) != "rm_parent_vm dir="+web+" vm="+vm.String() {
		t.Errorf("Rm without a terminal = %v", msg.NamesOf(err))
	}
	if i := infos.Names(); len(i) == 0 || i[0] != "using_parent_vm dir="+web+" vm="+vm.String() {
		t.Errorf("Rm's infos = %q", i)
	}
	asked := answers(t, e, false)
	if err := (Lifecycle{e}).Rm(t.Context(), RmOptions{Force: true, NoExport: true}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if n := len(vmNames(t, e)); n != 1 {
		t.Errorf("%d VMs after no", n)
	}
	if want := "delete_parent_vm dir=" + web + " vm=" + vm.String(); !slices.Equal(*asked, []string{want}) {
		t.Errorf("asked %q, want %q", *asked, want)
	}
	asked = answers(t, e, true)
	if err := (Lifecycle{e}).Rm(t.Context(), RmOptions{Force: true, NoExport: true}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if want := "delete_parent_vm dir=" + web + " vm=" + vm.String(); !slices.Equal(*asked, []string{want}) {
		t.Errorf("asked %q, want %q", *asked, want)
	}
	if n := len(vmNames(t, e)); n != 0 {
		t.Errorf("%d VMs after rm", n)
	}
}

// Commands that give a parent folder's VM more access, run from a
// subfolder: refused without a terminal, asked in one ([y/N]), a no
// changing nothing. The same commands in the parent folder, or ones that
// take access away, ask nothing.
func TestWidenParentVM(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	f.ExecFunc = noDirect
	web := e.Dir
	st := existingVM(t, e, Options{Network: "open"})
	vm := st.Name()
	if err := setNetwork(t.Context(), Net{e}, true); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if err := set(t, st, keyGitProtect, gitprotect.On); err != nil {
		t.Fatal(err)
	}
	subfolder(t, e, web, "src")
	n, s := Net{e}, Settings{e}
	cases := []struct {
		cmd     string
		do      func() error
		changed func() bool
	}{
		{
			"devvm network open", func() error { return setNetwork(t.Context(), n, false) },
			func() bool { return get(reread(t, e, vm), KeyNet) == "open" },
		},
		{
			"devvm secret use", func() error { return n.SecretUse(t.Context(), "github", true) },
			func() bool { return slices.Contains(secrets.Names(reread(t, e, vm)), "github") },
		},
		{
			"devvm ssh-agent on", func() error { return s.SetSSHAgent(t.Context(), sshagent.On) },
			func() bool { return sshagent.Mode(reread(t, e, vm)) == sshagent.On },
		},
		{
			"devvm network allow", func() error { return n.PolicyChange(t.Context(), []string{"example.com"}, RuleChange{}) },
			func() bool { return policyHas(t, e, vm, "example.com") },
		},
		{
			"devvm git-protect off", func() error { return s.SetGitProtect(t.Context(), gitprotect.Off) },
			func() bool { return gitprotect.Mode(reread(t, e, vm)) == gitprotect.Off },
		},
	}
	for _, c := range cases {
		fake(e).Terminal = false
		want := "widen_parent_vm_refused cmd=" + c.cmd + " dir=" + web + " vm=" + vm.String()
		if err := c.do(); msg.NamesOf(err) != want {
			t.Errorf("%s without a terminal = %v, want %s", c.cmd, msg.NamesOf(err), want)
		}
		asked := answers(t, e, false)
		if err := c.do(); err != nil {
			t.Errorf("%s, answered no: %v", c.cmd, msg.NamesOf(err))
		}
		if want := "widen_parent_vm cmd=" + c.cmd + " dir=" + web + " vm=" + vm.String(); !slices.Equal(*asked, []string{want}) {
			t.Errorf("%s asked %q, want %q", c.cmd, *asked, want)
		}
		if c.changed() {
			t.Errorf("%s changed the VM", c.cmd)
		}
		if c.cmd == "devvm git-protect off" {
			continue // a running VM's locks don't change on macOS
		}
		answers(t, e, true)
		if err := c.do(); err != nil {
			t.Errorf("%s, answered yes: %v", c.cmd, msg.NamesOf(err))
		}
		if !c.changed() {
			t.Errorf("%s, answered yes, didn't change the VM", c.cmd)
		}
	}
	// Taking access away asks nothing, and so does network rm of an allow;
	// rm of a deny asks. (The restart a running VM's change offers isn't
	// what this checks.)
	asked := answers(t, e, false)
	fake(e).OnConfirm = func(msg.M) bool { return false }
	if err := setNetwork(t.Context(), n, true); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if err := n.PolicyChange(t.Context(), []string{"!bad.example.com"}, RuleChange{Deny: true}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if err := n.PolicyChange(t.Context(), []string{"example.com"}, RuleChange{Remove: true}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if len(*asked) != 0 {
		t.Errorf("asked %q", *asked)
	}
	if err := n.PolicyChange(t.Context(), []string{"!bad.example.com"}, RuleChange{Remove: true}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if want := "widen_parent_vm cmd=devvm network rm dir=" + web + " vm=" + vm.String(); !slices.Equal(*asked, []string{want}) {
		t.Errorf("network rm of a deny asked %q", *asked)
	}
	if !policyHas(t, e, vm, "!bad.example.com") {
		t.Error("network rm of a deny, answered no, removed it")
	}
	// In the parent folder itself, nothing asks.
	e.Dir = web
	fake(e).Terminal = false
	if err := setNetwork(t.Context(), n, false); err != nil {
		t.Errorf("network open in the VM's own folder: %v", msg.NamesOf(err))
	}
}

// reread is vm's state as it is now.
func reread(t *testing.T, e *Env, vm vmname.VM) *state.VM {
	t.Helper()
	st, err := state.ReadVM(e.Dirs, vm)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// policyHas reports whether vm's own policy file has rule.
func policyHas(t *testing.T, e *Env, vm vmname.VM, rule string) bool {
	t.Helper()
	data, err := os.ReadFile(e.Dirs.VM(vm).Policy())
	if errors.Is(err, os.ErrNotExist) {
		return false
	} else if err != nil {
		t.Fatal(err)
	}
	return slices.Contains(strings.Fields(string(data)), rule)
}

// A subfolder in a repo of its own below the parent's folder (it, or a
// folder between): the other commands find no VM and up makes a new one,
// asking nothing, unless --parent, which takes the parent's.
func TestSubfolderRepo(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	web := e.Dir
	existingVM(t, e, Options{Network: "open"})
	vm := vmName(t, e)
	src := subfolder(t, e, web, "pkg/src")
	pkg := filepath.Dir(src)
	if err := os.Mkdir(filepath.Join(pkg, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if p, err := (finder{e}).parentVM(t.Context()); err != nil || p == nil || p.repo != pkg || p.f.Dir != web {
		t.Fatalf("parentVM = %+v, %v", p, msg.NamesOf(err))
	}
	// A .git file (a worktree's) in the folder itself is nearer.
	if err := os.WriteFile(filepath.Join(src, ".git"), []byte("gitdir: /x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if p, err := (finder{e}).parentVM(t.Context()); err != nil || p == nil || p.repo != src {
		t.Fatalf("parentVM = %+v, %v", p, msg.NamesOf(err))
	}
	if err := (Lifecycle{e}).Stop(t.Context()); msg.NamesOf(err) != "no_vm dir="+src {
		t.Errorf("Stop = %v", msg.NamesOf(err))
	}
	if c := call(f, "Stop"); c.Method != "" {
		t.Errorf("stopped %v", c.Args)
	}
	// --parent: the parent's, across the repo.
	e.Folder = FolderParent
	if err := (Lifecycle{e}).Stop(t.Context()); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if c := call(f, "Stop"); len(c.Args) == 0 || c.Args[0] != vm {
		t.Errorf("--parent: Stop = %v", c.Args)
	}
	if err := (Launch{e}).Up(t.Context(), Options{}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if c := call(f, "Shell"); c.Args[0] != vm || shellOpts(c).Dir != src || len(vmNames(t, e)) != 1 {
		t.Errorf("--parent: Shell = %v, %d VMs", c.Args, len(vmNames(t, e)))
	}
	// Without it, a new VM, nothing asked, in a terminal or not.
	e.Folder = FolderAuto
	asked := answers(t, e, true)
	if err := (Launch{e}).Up(t.Context(), Options{Network: "open"}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if n := len(vmNames(t, e)); n != 2 || len(*asked) != 0 {
		t.Errorf("in a terminal: %d VMs, asked %q", n, *asked)
	}
	subfolder(t, e, web, "pkg/docs")
	fake(e).Terminal = false
	fake(e).OnAsk, fake(e).OnConfirm = unasked(t), unasked(t)
	if err := (Launch{e}).Up(t.Context(), Options{Network: "open"}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if n := len(vmNames(t, e)); n != 3 {
		t.Errorf("%d VMs without a terminal", n)
	}
}
