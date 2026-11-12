package vm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/agents"
	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/backend/backendtest"
	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/vmname"
)

// homeFiles is a home folder's tar stream, gzipped, with these entries:
// name to content, a name ending in / a folder, "-> target" a symlink.
func homeFiles(t *testing.T, files ...string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	tw := tar.NewWriter(zw)
	for i := 0; i+1 < len(files); i += 2 {
		name, data := files[i], files[i+1]
		h := &tar.Header{Name: name, Mode: 0o644, ModTime: time.Unix(1, 0)}
		switch {
		case strings.HasSuffix(name, "/"):
			h.Typeflag, h.Mode = tar.TypeDir, 0o755
		case strings.HasPrefix(data, "-> "):
			h.Typeflag, h.Linkname = tar.TypeSymlink, strings.TrimPrefix(data, "-> ")
		default:
			h.Typeflag, h.Size = tar.TypeReg, int64(len(data))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, data); h.Typeflag == tar.TypeReg && err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// entries lists a home folder's gzipped tar stream: one line per entry,
// its type, mode and name, and a file's content or a link's target.
func entries(t *testing.T, data []byte) []string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	var out []string
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		} else if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		out = append(out, fmt.Sprintf("%c %03o %s %s%s", h.Typeflag, h.Mode, h.Name, body, h.Linkname))
	}
}

// A home folder's copy keeps files, folders and symlinks inside it, with
// their permissions but no setuid bit and no owner, and hard links to a
// file it kept. It leaves out what a new VM's setup writes itself, the
// home folder's own entry, paths out of it, special files, and anything
// under a symlink, which tar would follow.
func TestCopyHome(t *testing.T) {
	var in bytes.Buffer
	zw := gzip.NewWriter(&in)
	tw := tar.NewWriter(zw)
	for _, h := range []*tar.Header{
		{Typeflag: tar.TypeDir, Name: "./", Mode: 0o777},
		{Typeflag: tar.TypeReg, Name: "./.bash_history", Mode: 0o600, Size: 2, Uid: 1000, Uname: "me"},
		{Typeflag: tar.TypeDir, Name: "./bin/", Mode: 0o755},
		{Typeflag: tar.TypeReg, Name: "./bin/tool", Mode: 0o4755, Size: 2},
		{Typeflag: tar.TypeLink, Name: "./bin/again", Linkname: "./bin/tool"},
		{Typeflag: tar.TypeLink, Name: "./bin/passwd", Linkname: "/etc/passwd"},
		{Typeflag: tar.TypeSymlink, Name: "./etc", Linkname: "/etc"},
		{Typeflag: tar.TypeReg, Name: "./etc/sudoers.d/me", Mode: 0o644, Size: 2},
		{Typeflag: tar.TypeReg, Name: "../outside", Mode: 0o644, Size: 2},
		{Typeflag: tar.TypeReg, Name: "/abs", Mode: 0o644, Size: 2},
		{Typeflag: tar.TypeReg, Name: "./a/../../up", Mode: 0o644, Size: 2},
		{Typeflag: tar.TypeChar, Name: "./dev", Mode: 0o666},
		{Typeflag: tar.TypeFifo, Name: "./fifo", Mode: 0o666},
		{Typeflag: tar.TypeReg, Name: "./.config/devvm/env", Mode: 0o600, Size: 2},
		{Typeflag: tar.TypeReg, Name: "./.ssh/authorized_keys", Mode: 0o600, Size: 2},
		{Typeflag: tar.TypeReg, Name: "./.ssh/id_ed25519", Mode: 0o600, Size: 2},
		{Typeflag: tar.TypeReg, Name: "./.claude/.credentials.json", Mode: 0o600, Size: 2},
		{Typeflag: tar.TypeReg, Name: "./.cache/pip/x", Mode: 0o644, Size: 2},
		{Typeflag: tar.TypeReg, Name: "./line\nbreak", Mode: 0o644, Size: 2},
	} {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			if _, err := io.WriteString(tw, "ok"); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := copyHome(&in, &out, homeSkips(paths.Dirs{Config: t.TempDir()}, []string{"claude"})); err != nil {
		t.Fatal(err)
	}
	got := entries(t, out.Bytes())
	want := []string{
		"0 600 .bash_history ok",
		"5 755 bin/ ",
		"0 755 bin/tool ok",
		"1 000 bin/again bin/tool",
		"2 000 etc /etc",
		"0 600 .ssh/id_ed25519 ok",
	}
	if !slices.Equal(got, want) {
		t.Errorf("kept\n\t%s\nwant\n\t%s", strings.Join(got, "\n\t"), strings.Join(want, "\n\t"))
	}
	zr, _ := gzip.NewReader(bytes.NewReader(out.Bytes()))
	tr := tar.NewReader(zr)
	for h, err := tr.Next(); err == nil; h, err = tr.Next() {
		if h.Uid != 0 || h.Uname != "" {
			t.Errorf("%s keeps its owner %d %s", h.Name, h.Uid, h.Uname)
		}
	}
	if err := copyHome(strings.NewReader("not gzip"), io.Discard, nil); err == nil {
		t.Error("copied a stream that isn't gzip")
	}
}

// A remade VM's home never takes an agent's login file from the old one, whatever agents the VM
// has: every login file the registry names, such as Cursor's and pi's, is skipped, but one
// that's history too (Claude Code's .claude.json, whose per-folder state stays).
func TestHomeSkipsEveryLogin(t *testing.T) {
	skips := homeSkips(paths.Dirs{Config: t.TempDir()}, nil)
	for _, rel := range []string{".config/cursor/auth.json", ".pi/agent/auth.json"} {
		if !skipped(rel, skips) {
			t.Errorf("%s is kept", rel)
		}
	}
	if skipped(agents.ClaudeJSON, skips) {
		t.Errorf("%s isn't kept", agents.ClaudeJSON)
	}
	for _, d := range agents.Defs {
		for _, l := range d.Logins {
			if !slices.Contains(d.History, l.Rel) && !skipped(l.Rel, skips) {
				t.Errorf("%s's %s is kept", d.Name, l.Rel)
			}
		}
	}
}

// What devvm layer add makes: the VM's layers with the new ones on top in
// the order given, those it has said and left as they are.
func TestAddedImage(t *testing.T) {
	for _, c := range []struct {
		img    image.Name
		layers []string
		want   image.Name
		had    []string
	}{
		{"base", []string{"desktop"}, "desktop", nil},
		{"docker", []string{"desktop"}, "docker+desktop", nil},
		{"docker", []string{"nix", "desktop"}, "docker+nix+desktop", nil},
		{"docker+nix", []string{"desktop", "docker"}, "docker+nix+desktop", []string{"docker"}},
		{"docker", []string{"docker", "docker"}, "docker", []string{"docker"}},
		{"base", []string{"nix", "nix"}, "nix", nil},
	} {
		got, had := addedImage(c.img, c.layers)
		if got != c.want || !slices.Equal(had, c.had) {
			t.Errorf("%s with %q = %s, had %q; want %s, %q", c.img, c.layers, got, had, c.want, c.had)
		}
	}
}

// remakeEnv is upEnv with a VM made, network policy on, whose home the
// fake keeps: what the old VM's tar sends (homeOut) and what the new
// one's gets (homeIn, *restored).
func remakeEnv(t *testing.T) (*Env, *backendtest.Fake, *state.VM, *[]byte) {
	t.Helper()
	e, f, _ := upEnv(t, allNative())
	home := homeFiles(t, ".bash_history", "ls\n", ".config/devvm/env", "GH_TOKEN=x\n")
	var restored []byte
	f.ExecFunc = func(vm string, argv []string, o backend.ExecOpts) error {
		switch {
		case len(argv) == 3 && argv[2] == homeOut:
			_, err := o.Stdout.Write(home)
			return err
		case len(argv) == 3 && argv[2] == homeIn:
			var err error
			restored, err = io.ReadAll(o.Stdin)
			return err
		}
		return noDirect(vm, argv, o)
	}
	st := existingVM(t, e, Options{Network: "allowlist", Ports: "3000,5173", SSHAgent: true, Size: Size{CPUs: "3", Memory: "6"}})
	return e, f, st, &restored
}

// devvm layer add --force makes the VM again from its image with the
// layer on top: built, then its home saved, stopped, set aside, made
// again with what its state asks for, started, its home put back, and
// the old machine deleted, then a shell. Every setting is kept, its
// ID too, and its per-VM files.
func TestAddLayers(t *testing.T) {
	e, f, st, restored := remakeEnv(t)
	vm := st.Name()
	aside, _ := vmname.Aside(vm)
	for k, v := range map[string]string{
		keyPublish: "8080", keyGroup: "team", keyGroupAs: "api", keyDesktopPass: "Abcdefgh", keyDesktopPort: "47001",
	} {
		if err := set(t, st, k, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := state.WriteFile(e.Dirs.VM(vm).Policy(), []byte("allow example.com\n")); err != nil {
		t.Fatal(err)
	}
	before, err := state.ReadVM(e.Dirs, vm)
	if err != nil {
		t.Fatal(err)
	}
	n := len(f.Calls())
	if err := (Lifecycle{e}).AddLayers(t.Context(), "devvm layer add docker", []string{"docker"}, RemakeOptions{Force: true}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	after, err := state.ReadVM(e.Dirs, vm)
	if err != nil {
		t.Fatal(err)
	}
	// The state keys a remake keeps: every one but the image
	// (keys_test.go).
	for _, k := range []string{
		keyNet, keyProxyKey, keySSHPort, keyProxyPort, keyDesktopPort, keyPorts, keyDir, keyFolder, keyFrom,
		keyBackend, keyID, keyToolNames, keyTools, keyLibc, keyGitProtect, keyPublish, keyGroup, keyGroupAs, keyLinks,
		keyCPUs, keyMemory, keyDisk, keyAgent, keyAgentToggle, keyClone, keySecrets, keyDevcontainer, keyDevEnv,
		keyDesktopPass,
	} {
		if get(after, k) != get(before, k) {
			t.Errorf("%s: %q, was %q", k, get(after, k), get(before, k))
		}
	}
	if after.Image != "docker" || after.Chain == before.Chain || !after.Created.After(before.Created) || remakePending(after) {
		t.Errorf("image %s, chain %s (was %s), created %v (was %v), pending %v", after.Image, after.Chain, before.Chain, after.Created, before.Created, remakePending(after))
	}
	if data, err := os.ReadFile(e.Dirs.VM(vm).Policy()); err != nil || string(data) != "allow example.com\n" {
		t.Errorf("its policy: %q, %v", data, err)
	}
	if _, err := os.Stat(e.Dirs.VM(vm).Home()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("its saved home: %v", err)
	}
	// What its home was, less devvm's own files.
	if got := entries(t, *restored); !slices.Equal(got, []string{"0 644 .bash_history ls\n"}) {
		t.Errorf("restored %q", got)
	}
	var steps []string
	for _, c := range f.Calls()[n:] {
		switch c.Method {
		case "BuildImage", "Stop", "CreateVM", "Start", "Shell":
			steps = append(steps, c.Method)
		case "Rename", "Delete": //nolint:usestdlibvars // the fake's method names, not HTTP's
			steps = append(steps, c.Method+" "+strings.Join(strs(c.Args), " "))
		case "Exec":
			if argv, _ := c.Args[1].([]string); len(argv) == 3 && (argv[2] == homeOut || argv[2] == homeIn) {
				steps = append(steps, "Exec "+map[bool]string{true: "out", false: "in"}[argv[2] == homeOut])
			}
		}
	}
	want := []string{"BuildImage", "Exec out", "Stop", "Rename " + vm + " " + aside, "CreateVM", "Start", "Exec in", "Delete " + aside, "Shell"}
	if !slices.Equal(steps, want) {
		t.Errorf("steps %q, want %q", steps, want)
	}
	spec, _ := call(f, "CreateVM").Args[1].(backend.VMSpec)
	if spec.Name != vm || spec.Project != e.Dir || !spec.AgentForwarding || spec.Size.CPUs != 3 || spec.Size.MemoryGiB != 6 ||
		spec.SSHPort == 0 || spec.Tools != e.Dirs.Tools() {
		t.Errorf("made with %+v", spec)
	}
	if _, ok := f.Instances[aside]; ok {
		t.Error("its old machine is still there")
	}
}

// strs is a call's string arguments.
func strs(args []any) []string {
	var out []string
	for _, a := range args {
		if s, ok := a.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// Without a terminal devvm layer add refuses unless --force, and in one
// it asks, [y/N], naming the image and whether home is kept; no keeps
// the VM. Layers the VM has are said, and with all of them nothing runs.
// --clean-home keeps no home, and --no-export exports nothing.
func TestAddLayersAsks(t *testing.T) {
	e, f, st := func() (*Env, *backendtest.Fake, *state.VM) {
		e, f, st, _ := remakeEnv(t)
		return e, f, st
	}()
	vm := st.Name()
	lc := Lifecycle{e}
	if err := lc.AddLayers(t.Context(), "devvm layer add nix", []string{"nix"}, RemakeOptions{}); msg.NamesOf(err) != "no_input answer=--force" {
		t.Errorf("no terminal: %v", msg.NamesOf(err))
	}
	asked := answers(t, e, false)
	var said []string
	fake(e).OnInfo = func(m msg.M) { said = append(said, m.Names()) }
	n := len(f.Calls())
	if err := lc.AddLayers(t.Context(), "devvm layer add nix", []string{"nix"}, RemakeOptions{}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if !slices.Equal(*asked, []string{"remake_confirm image=nix vm=" + vm}) || !slices.Contains(said, "kept vm="+vm) {
		t.Errorf("asked %q, said %q", *asked, said)
	}
	if slices.ContainsFunc(f.Methods()[n:], func(m string) bool { return m == "BuildImage" || m == "Stop" || m == "Rename" }) {
		t.Errorf("declined, then %q", f.Methods()[n:])
	}
	*asked = nil
	if err := lc.AddLayers(t.Context(), "devvm layer add nix", []string{"nix"}, RemakeOptions{CleanHome: true}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if !slices.Equal(*asked, []string{"remake_confirm_clean_home image=nix vm=" + vm}) {
		t.Errorf("--clean-home asked %q", *asked)
	}
	answers(t, e, true)
	n = len(f.Calls())
	if err := lc.AddLayers(t.Context(), "devvm layer add nix", []string{"nix"}, RemakeOptions{CleanHome: true, NoExport: true}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	for _, c := range f.Calls()[n:] {
		if c.Method != "Exec" {
			continue
		}
		if argv, _ := c.Args[1].([]string); len(argv) == 3 && (argv[2] == homeOut || argv[2] == homeIn || strings.Contains(argv[2], "tar -czf")) {
			t.Errorf("--clean-home --no-export ran %q", argv[2])
		}
	}
	said = nil
	n = len(f.Calls())
	if err := lc.AddLayers(t.Context(), "devvm layer add nix", []string{"nix"}, RemakeOptions{}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if !slices.Equal(said, []string{"has_layers layers=nix vm=" + vm}) || slices.Contains(f.Methods()[n:], "Stop") {
		t.Errorf("has it: said %q, then %q", said, f.Methods()[n:])
	}
}

// What devvm layer add refuses, before anything runs: a --clone VM, one
// whose creation didn't finish, a layer its backend can't run, a parent
// folder's VM without a terminal, and no VM.
func TestAddLayersRefuses(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	lc := Lifecycle{e}
	add := func() string {
		return msg.NamesOf(lc.AddLayers(t.Context(), "devvm layer add docker", []string{"docker"}, RemakeOptions{Force: true}))
	}
	if got := add(); got != "no_vm dir="+e.Dir {
		t.Errorf("no VM: %s", got)
	}
	st := existingVM(t, e, Options{Network: "open"})
	vm := st.Name()
	if err := set(t, st, keyClone, "clone"); err != nil {
		t.Fatal(err)
	}
	if got := add(); got != "remake_clone vm="+vm {
		t.Errorf("a clone: %s", got)
	}
	if err := set(t, st, keyClone, ""); err != nil {
		t.Fatal(err)
	}
	change(t, st, func(st *state.VM) error { st.Created = time.Time{}; return nil })
	if got := add(); got != "vm_unfinished vm="+vm {
		t.Errorf("unfinished: %s", got)
	}
	change(t, st, func(st *state.VM) error { st.Created = time.Now(); return nil })
	f.Capabilities = allNative()
	delete(f.Capabilities, backend.FeatureDockerLayer)
	if got := add(); !strings.HasPrefix(got, "backend_lacks ") {
		t.Errorf("no docker: %s", got)
	}
	if slices.ContainsFunc(f.Methods(), func(m string) bool { return m == "Rename" }) {
		t.Errorf("ran %q", f.Methods())
	}
}

// What devvm layer rm makes: the VM's layers without those given, the
// rest in their order, base when none is left; and those it lacks, once
// each.
// A remake whose export is over the most devvm keeps warns, keeps no
// export and goes on, though the VM keeps writing.
func TestRemakeHistoryTooLarge(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	e.HistoryMax = 1 << 20
	vm := existingVM(t, e, Options{Agents: &[]string{"claude"}, Network: "open"}).Name()
	f.ExecFunc = func(_ string, argv []string, o backend.ExecOpts) error { return streamForever(argv, o) }
	var warned []string
	fake(e).OnWarn = func(m msg.M) { warned = append(warned, m.Names()) }
	if err := (Lifecycle{e}).AddLayers(t.Context(), "devvm layer add docker", []string{"docker"}, RemakeOptions{Force: true, CleanHome: true}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if !slices.Contains(warned, "history_too_large vm="+vm) || !slices.Contains(f.Methods(), "Rename") {
		t.Errorf("warned %q, ran %q", warned, f.Methods())
	}
	if es, _ := os.ReadDir(e.Dirs.Exports()); len(es) != 0 {
		t.Errorf("exports %v", es)
	}
}

func TestRemovedImage(t *testing.T) {
	for _, c := range []struct {
		img    image.Name
		layers []string
		want   image.Name
		lacks  []string
	}{
		{"docker+nix+desktop", []string{"nix"}, "docker+desktop", nil},
		{"docker+nix+desktop", []string{"desktop", "docker"}, "nix", nil},
		{"docker", []string{"docker"}, "base", nil},
		{"docker", []string{"nix", "docker", "nix"}, "base", []string{"nix"}},
		{"base", []string{"desktop"}, "base", []string{"desktop"}},
	} {
		got, lacks := removedImage(c.img, c.layers)
		if got != c.want || !slices.Equal(lacks, c.lacks) {
			t.Errorf("%s without %q = %s, lacks %q; want %s, %q", c.img, c.layers, got, lacks, c.want, c.lacks)
		}
	}
}

// devvm layer rm's arguments: base, a shared tool's name and one that
// can't be a layer's are refused as devvm layer add refuses them; any
// other name may be one of the VM's layers, even one whose script is
// gone.
func TestCheckLayerNames(t *testing.T) {
	layers := []string{"desktop", "docker", "nix"}
	for _, c := range []struct {
		given []string
		want  string
	}{
		{[]string{"docker", "gone-layer"}, ""},
		{[]string{"docker", "node"}, "tool_not_layer tool=node"},
		{[]string{"base"}, "unknown_layer layer=base layers=desktop docker nix"},
		{[]string{"Dockr!"}, "unknown_layer_like layer=Dockr! like=docker"},
		{[]string{"x_y_z_w"}, "unknown_layer layer=x_y_z_w layers=desktop docker nix"},
	} {
		if got := msg.NamesOf(CheckLayerNames(c.given, layers)); got != c.want {
			t.Errorf("%q: %q, want %q", c.given, got, c.want)
		}
	}
}

// devvm layer rm --force makes the VM again from its image without the
// layers given, the remake devvm layer add makes: its settings kept. One
// of the user's layers whose script is gone can be taken out. Layers it
// lacks are said, and with none of them nothing runs; a name that isn't
// a layer is refused before anything runs.
func TestRemoveLayers(t *testing.T) {
	e, f, st, _ := remakeEnv(t)
	vm := st.Name()
	lc := Lifecycle{e}
	var said []string
	fake(e).OnInfo = func(m msg.M) { said = append(said, m.Names()) }
	change(t, st, func(st *state.VM) error { st.Image = "docker+gone+nix"; return nil })
	before := readState(t, e.Dirs, vm)
	rm := func(layers ...string) error {
		said = nil
		return lc.RemoveLayers(t.Context(), "devvm layer rm "+strings.Join(layers, " "), layers, RemakeOptions{Force: true})
	}

	n := len(f.Calls())
	if err := rm("dokcer"); msg.NamesOf(err) != "unknown_layer_like layer=dokcer like=docker" {
		t.Errorf("a typo: %s", msg.NamesOf(err))
	}
	if err := rm("desktop"); err != nil || !slices.Equal(said, []string{"lacks_layers layers=desktop vm=" + vm}) {
		t.Errorf("one it lacks: said %q, %v", said, msg.NamesOf(err))
	}
	if slices.ContainsFunc(f.Methods()[n:], func(m string) bool { return m == "BuildImage" || m == "Stop" }) {
		t.Errorf("ran %q", f.Methods()[n:])
	}

	n = len(f.Calls())
	if err := rm("gone", "desktop"); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	after := readState(t, e.Dirs, vm)
	if after.Image != "docker+nix" || remakePending(after) || !slices.Equal(said[:1], []string{"lacks_layers layers=desktop vm=" + vm}) {
		t.Errorf("image %s, pending %v, said %q", after.Image, remakePending(after), said)
	}
	for _, k := range []string{keyID, keyPorts, keyNet, keyCPUs, keyMemory, keyAgent, keyToolNames} {
		if get(after, k) != get(before, k) {
			t.Errorf("%s: %q, was %q", k, get(after, k), get(before, k))
		}
	}
	var steps []string
	for _, m := range f.Methods()[n:] {
		if slices.Contains([]string{"BuildImage", "Stop", "Rename", "CreateVM", "Start", "Delete", "Shell"}, m) {
			steps = append(steps, m)
		}
	}
	// docker, then docker+nix built.
	if want := []string{"BuildImage", "Stop", "Rename", "CreateVM", "Start", "Delete", "Shell"}; !slices.Equal(slices.Compact(steps), want) {
		t.Errorf("steps %q, want %q", steps, want)
	}

	// Every layer out: base.
	if err := rm("nix", "docker"); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if got := readState(t, e.Dirs, vm).Image; got != "base" {
		t.Errorf("image %s, want base", got)
	}
}

// A remake that fails once the old machine is set aside puts it back as
// it was: the new one deleted, the old one renamed back,
// its state as before, and nothing saved left.
func TestRemakeUndo(t *testing.T) {
	e, f, st, _ := remakeEnv(t)
	vm := st.Name()
	aside, _ := vmname.Aside(vm)
	var cleanups []func(context.Context)
	fake(e).OnCleanup = func(c func(context.Context)) func() {
		i := len(cleanups)
		cleanups = append(cleanups, c)
		return func() { cleanups[i] = nil }
	}
	f.Errs = map[string]error{"CreateVM": errors.New("no room")}
	err := (Lifecycle{e}).AddLayers(t.Context(), "devvm layer add docker", []string{"docker"}, RemakeOptions{Force: true})
	if err == nil || err.Error() != "no room" {
		t.Fatalf("AddLayers: %v", err)
	}
	if !remakePending(readState(t, e.Dirs, vm)) {
		t.Error("not pending before its cleanup")
	}
	n := len(f.Calls())
	for _, c := range slices.Backward(cleanups) {
		if c != nil {
			c(t.Context())
		}
	}
	if got := f.Methods()[n:]; !slices.Contains(got, "Rename") || !slices.Contains(got, "Delete") {
		t.Errorf("cleanup ran %q", got)
	}
	if _, ok := f.Instances[vm]; !ok {
		t.Error("its old machine isn't back")
	}
	if _, ok := f.Instances[aside]; ok {
		t.Error("its old machine is still aside")
	}
	after := readState(t, e.Dirs, vm)
	if after.Image != st.Image || remakePending(after) {
		t.Errorf("image %s, pending %v", after.Image, remakePending(after))
	}
	if _, err := os.Stat(e.Dirs.VM(vm).Home()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("its saved home: %v", err)
	}
}

// A remake a killed devvm left is finished by the next devvm, which asks
// nothing: with its old machine still aside, that's put back and the
// remake done again; once it's gone, the new VM is made with the home
// folder saved for it. Meanwhile other commands refuse, devvm gc
// --orphans keeps it, and devvm rm deletes it and its old machine.
func TestRemakePending(t *testing.T) {
	e, f, st, restored := remakeEnv(t)
	vm := st.Name()
	aside, _ := vmname.Aside(vm)
	pend := func(made bool) {
		t.Helper()
		if err := update(t.Context(), st, remakeSection, func(r *remakeInVM) error {
			*r = remakeInVM{Image: "docker", Command: "devvm layer add docker", Home: true, Made: made}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Killed once its old machine was set aside.
	pend(false)
	if err := f.Stop(t.Context(), vm); err != nil {
		t.Fatal(err)
	}
	if err := f.Rename(t.Context(), vm, aside); err != nil {
		t.Fatal(err)
	}
	if _, err := (Settings{e}).Ports(t.Context()); msg.NamesOf(err) != "remake_pending dir="+e.Dir+" vm="+vm {
		t.Errorf("devvm ports: %v", msg.NamesOf(err))
	}
	if r, err := (Leftovers{e}).Find(t.Context()); err != nil || len(r.Orphans) > 0 {
		t.Errorf("orphans %+v, %v", r.Orphans, err)
	}
	if err := (Launch{e}).Up(t.Context(), Options{}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if st := readState(t, e.Dirs, vm); st.Image != "docker" || remakePending(st) {
		t.Errorf("image %s, pending %v", st.Image, remakePending(st))
	}
	if _, ok := f.Instances[aside]; ok {
		t.Error("its old machine is still aside")
	}

	// Killed once the old machine was deleted: the new one, made or not,
	// is made again with the saved home folder.
	st = readState(t, e.Dirs, vm)
	pend(true)
	if err := os.WriteFile(e.Dirs.VM(vm).Home(), homeFiles(t, "notes", "kept"), 0o600); err != nil {
		t.Fatal(err)
	}
	*restored = nil
	n := len(f.Calls())
	if err := (Launch{e}).Up(t.Context(), Options{}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if got := entries(t, *restored); !slices.Equal(got, []string{"0 644 notes kept"}) {
		t.Errorf("restored %q", got)
	}
	if ms := f.Methods()[n:]; slices.Contains(ms, "Rename") || !slices.Contains(ms, "CreateVM") {
		t.Errorf("finished with %q", ms)
	}
	if st := readState(t, e.Dirs, vm); remakePending(st) {
		t.Error("still pending")
	}

	// devvm rm of one pending deletes its old machine too.
	st = readState(t, e.Dirs, vm)
	pend(false)
	if err := f.Stop(t.Context(), vm); err != nil {
		t.Fatal(err)
	}
	if err := f.Rename(t.Context(), vm, aside); err != nil {
		t.Fatal(err)
	}
	if err := (Lifecycle{e}).Rm(t.Context(), RmOptions{Force: true}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if len(f.Instances) != 0 {
		t.Errorf("left %v", f.Instances)
	}
	if _, err := os.Stat(e.Dirs.VM(vm).Dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("its folder: %v", err)
	}
}

// While another devvm holds a VM's remake lock, making it again and
// finishing its remake both refuse, and nothing changes.
func TestRemakeHeld(t *testing.T) {
	e, f, st, _ := remakeEnv(t)
	vm := st.Name()
	if err := state.PrivateDir(e.Dirs.VM(vm).Run()); err != nil {
		t.Fatal(err)
	}
	unlock, ok, err := platform.TryLockFile(e.Dirs.VM(vm).RemakeLock())
	if err != nil || !ok {
		t.Fatalf("lock: %v, %v", ok, err)
	}
	defer unlock()
	want := "remake_running vm=" + vm
	n := len(f.Calls())
	err = (Lifecycle{e}).AddLayers(t.Context(), "devvm layer add docker", []string{"docker"}, RemakeOptions{Force: true})
	if msg.NamesOf(err) != want {
		t.Errorf("devvm layer add: %v", msg.NamesOf(err))
	}
	if err := update(t.Context(), st, remakeSection, func(r *remakeInVM) error {
		*r = remakeInVM{Image: "docker", Command: "devvm layer add docker", Home: true}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := (Launch{e}).Up(t.Context(), Options{}); msg.NamesOf(err) != want {
		t.Errorf("devvm: %v", msg.NamesOf(err))
	}
	for _, m := range f.Methods()[n:] {
		if slices.Contains([]string{"BuildImage", "Stop", "Rename", "CreateVM", "Start", "Delete", "Exec"}, m) {
			t.Errorf("ran %s", m)
		}
	}
	if !remakePending(readState(t, e.Dirs, vm)) {
		t.Error("its remake was ended")
	}
}

// Once the new machine has the home folder, the remake is done: when
// deleting the old machine fails, the cleanup leaves the new one as it
// is, and the next devvm only deletes the old one and ends the remake.
func TestRemakeDone(t *testing.T) {
	e, f, st, _ := remakeEnv(t)
	vm := st.Name()
	aside, _ := vmname.Aside(vm)
	var cleanups []func(context.Context)
	fake(e).OnCleanup = func(c func(context.Context)) func() {
		i := len(cleanups)
		cleanups = append(cleanups, c)
		return func() { cleanups[i] = nil }
	}
	f.Errs = map[string]error{"Delete": errors.New("busy")}
	err := (Lifecycle{e}).AddLayers(t.Context(), "devvm layer add docker", []string{"docker"}, RemakeOptions{Force: true})
	if err == nil || err.Error() != "busy" {
		t.Fatalf("AddLayers: %v", err)
	}
	n := len(f.Calls())
	for _, c := range slices.Backward(cleanups) {
		if c != nil {
			c(t.Context())
		}
	}
	if got := f.Methods()[n:]; len(got) > 0 {
		t.Errorf("cleanup ran %q", got)
	}
	after := readState(t, e.Dirs, vm)
	if p := sectionOf[remakeInVM](after, remakeSection); after.Image != "docker" || !p.Done {
		t.Errorf("image %s, remake %+v", after.Image, p)
	}
	f.Errs = nil
	n = len(f.Calls())
	if err := (Launch{e}).Up(t.Context(), Options{}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if ms := f.Methods()[n:]; slices.Contains(ms, "CreateVM") || slices.Contains(ms, "Rename") {
		t.Errorf("finished with %q", ms)
	}
	if _, ok := f.Instances[aside]; ok {
		t.Error("its old machine is still aside")
	}
	if _, ok := f.Instances[vm]; !ok {
		t.Error("its new machine is gone")
	}
	if st := readState(t, e.Dirs, vm); remakePending(st) || st.Image != "docker" {
		t.Errorf("image %s, pending %v", st.Image, remakePending(st))
	}
	if _, err := os.Stat(e.Dirs.VM(vm).Home()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("its saved home: %v", err)
	}
}

// A remake whose old machine is gone is checked again before the new
// machine is made: one of its layers gone refuses, and the machine by
// the VM's name stays.
func TestRemakePendingChecked(t *testing.T) {
	e, f, st, _ := remakeEnv(t)
	vm := st.Name()
	if err := update(t.Context(), st, remakeSection, func(r *remakeInVM) error {
		*r = remakeInVM{Image: "gone", Command: "devvm layer add gone", Made: true}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := (Launch{e}).Up(t.Context(), Options{}); !strings.HasPrefix(msg.NamesOf(err), "unknown_layer ") {
		t.Errorf("devvm: %v", msg.NamesOf(err))
	}
	if _, ok := f.Instances[vm]; !ok {
		t.Error("its machine was deleted")
	}
}

// devvm update on a current VM says so and changes nothing. On an
// outdated one it names what changed, refuses without a terminal unless
// --force, asks in one, and with --force makes the VM again from the same
// image name, its new build: built, home saved and put back, its settings
// kept, then a shell.
func TestUpdate(t *testing.T) {
	e, f, st, restored := remakeEnv(t)
	vm := st.Name()
	lc := Lifecycle{e}
	var said []string
	fake(e).OnInfo = func(m msg.M) { said = append(said, m.Names()) }
	n := len(f.Calls())
	if err := lc.Update(t.Context(), "devvm update", RemakeOptions{}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if !slices.Equal(said, []string{"vm_current image=base vm=" + vm}) ||
		slices.ContainsFunc(f.Methods()[n:], func(m string) bool { return m == "BuildImage" || m == "Stop" }) {
		t.Errorf("current: said %q, then %q", said, f.Methods()[n:])
	}

	// Outdated: base is rebuilt, as devvm rebuild base does.
	if err := state.BumpRebuild(e.Dirs, f.Name(), "", "base"); err != nil {
		t.Fatal(err)
	}
	before := readState(t, e.Dirs, vm)
	said = nil
	if err := lc.Update(t.Context(), "devvm update", RemakeOptions{}); msg.NamesOf(err) != "no_input answer=--force" {
		t.Errorf("no terminal: %v", msg.NamesOf(err))
	}
	if !slices.Equal(said, []string{"vm_changed changed=base vm=" + vm}) {
		t.Errorf("said %q", said)
	}
	asked := answers(t, e, false)
	if err := lc.Update(t.Context(), "devvm update", RemakeOptions{}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if !slices.Equal(*asked, []string{"remake_confirm image=base vm=" + vm}) || slices.Contains(f.Methods()[n:], "Stop") {
		t.Errorf("declined: asked %q, then %q", *asked, f.Methods()[n:])
	}

	n = len(f.Calls())
	if err := lc.Update(t.Context(), "devvm update", RemakeOptions{Force: true}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	after := readState(t, e.Dirs, vm)
	if after.Image != "base" || after.Chain == before.Chain || remakePending(after) {
		t.Errorf("image %s, chain %s (was %s), pending %v", after.Image, after.Chain, before.Chain, remakePending(after))
	}
	for _, k := range []string{keyID, keyPorts, keyNet, keyCPUs, keyMemory, keyAgent, keyToolNames} {
		if get(after, k) != get(before, k) {
			t.Errorf("%s: %q, was %q", k, get(after, k), get(before, k))
		}
	}
	if got := entries(t, *restored); !slices.Equal(got, []string{"0 644 .bash_history ls\n"}) {
		t.Errorf("restored %q", got)
	}
	var steps []string
	for _, m := range f.Methods()[n:] {
		if slices.Contains([]string{"BuildImage", "Stop", "Rename", "CreateVM", "Start", "Delete", "Shell"}, m) {
			steps = append(steps, m)
		}
	}
	if want := []string{"BuildImage", "Stop", "Rename", "CreateVM", "Start", "Delete", "Shell"}; !slices.Equal(steps, want) {
		t.Errorf("steps %q, want %q", steps, want)
	}

	// Current again.
	said = nil
	if err := lc.Update(t.Context(), "devvm update", RemakeOptions{Force: true}); err != nil || !slices.Equal(said, []string{"vm_current image=base vm=" + vm}) {
		t.Errorf("after: said %q, %v", said, msg.NamesOf(err))
	}
}

// staleTools gives st's VM an older version of its first shared tool, as
// when the tools lock moved on since the VM got it, and returns the tool
// files it had.
func staleTools(t *testing.T, st *state.VM) string {
	t.Helper()
	had := get(st, KeyTools)
	files := strings.Fields(had)
	_, v, ok := image.ParseToolFile(files[0])
	if !ok {
		t.Fatalf("tool file %q", files[0])
	}
	files[0] = strings.Replace(files[0], "-"+v+"-", "-0.0.1-", 1)
	if err := set(t, st, KeyTools, strings.Join(files, " ")); err != nil {
		t.Fatal(err)
	}
	return had
}

// devvm update moves the VM to newer shared tools first, which needs no
// remake: with only those newer, it records them and opens a shell,
// asking nothing. With its image outdated too, it asks to make the VM
// again, naming only the image: no keeps the VM with its newer tools and
// opens a shell, and yes makes it again with them. Without a terminal or
// --force, an outdated VM is refused before its tools change.
func TestUpdateTools(t *testing.T) {
	e, f, st, _ := remakeEnv(t)
	vm := st.Name()
	lc := Lifecycle{e}
	var said []string
	fake(e).OnInfo = func(m msg.M) { said = append(said, m.Names()) }
	steps := func(n int) []string {
		var out []string
		for _, m := range f.Methods()[n:] {
			if slices.Contains([]string{"BuildImage", "Stop", "Rename", "CreateVM", "Delete", "Shell"}, m) {
				out = append(out, m)
			}
		}
		return out
	}

	if err := (Tools{e}).Add(t.Context(), Options{}, []string{"go"}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	said = nil

	// Only the tools newer: recorded, then a shell, with no question.
	had := staleTools(t, readState(t, e.Dirs, vm))
	asked := answers(t, e, false)
	n := len(f.Calls())
	if err := lc.Update(t.Context(), "devvm update", RemakeOptions{}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if got := get(readState(t, e.Dirs, vm), KeyTools); got != had {
		t.Errorf("tools only: tools %q, want %q", got, had)
	}
	if len(*asked) > 0 || len(said) > 0 || !slices.Equal(steps(n), []string{"Shell"}) {
		t.Errorf("tools only: asked %q, said %q, then %q", *asked, said, steps(n))
	}

	// Both, without a terminal: refused before its tools change.
	if err := state.BumpRebuild(e.Dirs, f.Name(), "", "base"); err != nil {
		t.Fatal(err)
	}
	staleTools(t, readState(t, e.Dirs, vm))
	stale := get(readState(t, e.Dirs, vm), KeyTools)
	fake(e).Terminal = false
	said, n = nil, len(f.Calls())
	if err := lc.Update(t.Context(), "devvm update", RemakeOptions{}); msg.NamesOf(err) != "no_input answer=--force" {
		t.Errorf("no terminal: %v", msg.NamesOf(err))
	}
	if got := get(readState(t, e.Dirs, vm), KeyTools); got != stale || len(steps(n)) > 0 {
		t.Errorf("no terminal: tools %q, want %q; then %q", got, stale, steps(n))
	}

	// Both, declined: kept with its newer tools, and a shell.
	asked = answers(t, e, false)
	said, n = nil, len(f.Calls())
	if err := lc.Update(t.Context(), "devvm update", RemakeOptions{}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if want := []string{"remake_confirm image=base vm=" + vm}; !slices.Equal(*asked, want) {
		t.Errorf("declined: asked %q, want %q", *asked, want)
	}
	if want := []string{"vm_changed changed=base vm=" + vm, "kept vm=" + vm}; !slices.Equal(said, want) {
		t.Errorf("declined: said %q, want %q", said, want)
	}
	if got := get(readState(t, e.Dirs, vm), KeyTools); got != had || !slices.Equal(steps(n), []string{"Shell"}) {
		t.Errorf("declined: tools %q, want %q; then %q", got, had, steps(n))
	}

	// Both, agreed: made again, with its newer tools.
	staleTools(t, readState(t, e.Dirs, vm))
	answers(t, e, true)
	n = len(f.Calls())
	if err := lc.Update(t.Context(), "devvm update", RemakeOptions{}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	after := readState(t, e.Dirs, vm)
	if get(after, KeyTools) != had || remakePending(after) {
		t.Errorf("agreed: tools %q, want %q; pending %v", get(after, KeyTools), had, remakePending(after))
	}
	if want := []string{"BuildImage", "Stop", "Rename", "CreateVM", "Delete", "Shell"}; !slices.Equal(steps(n), want) {
		t.Errorf("agreed: steps %q, want %q", steps(n), want)
	}

	// Neither: current, and nothing runs.
	said, n = nil, len(f.Calls())
	if err := lc.Update(t.Context(), "devvm update", RemakeOptions{}); err != nil || !slices.Equal(said, []string{"vm_current image=base vm=" + vm}) || len(steps(n)) > 0 {
		t.Errorf("neither: said %q, then %q, %v", said, steps(n), msg.NamesOf(err))
	}
}

// devvm update refuses what devvm layer add does, before anything runs: no
// VM, a --clone VM and one whose creation didn't finish.
func TestUpdateRefuses(t *testing.T) {
	e, f, _ := upEnv(t, allNative())
	lc := Lifecycle{e}
	up := func() string { return msg.NamesOf(lc.Update(t.Context(), "devvm update", RemakeOptions{Force: true})) }
	if got := up(); got != "no_vm dir="+e.Dir {
		t.Errorf("no VM: %s", got)
	}
	st := existingVM(t, e, Options{Network: "open"})
	vm := st.Name()
	change(t, st, func(st *state.VM) error { st.Chain = "base=000000000000"; return nil })
	if err := set(t, st, keyClone, "clone"); err != nil {
		t.Fatal(err)
	}
	if got := up(); got != "remake_clone vm="+vm {
		t.Errorf("a clone: %s", got)
	}
	if err := set(t, st, keyClone, ""); err != nil {
		t.Fatal(err)
	}
	change(t, st, func(st *state.VM) error { st.Created = time.Time{}; return nil })
	if got := up(); got != "vm_unfinished vm="+vm {
		t.Errorf("unfinished: %s", got)
	}
	if slices.Contains(f.Methods(), "Rename") {
		t.Errorf("ran %q", f.Methods())
	}
}
