package build

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/backend/backendtest"
	"github.com/devvmhq/devvm/internal/from"
	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/platform"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/stubtest"
	"github.com/devvmhq/devvm/internal/tools"
	"github.com/devvmhq/devvm/internal/ubuntu"
)

func write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil { //nolint:gosec,nolintlint // the test's own folder; gosec's G703 flags it on some runs only
		t.Fatal(err)
	}
}

// testEnv returns an Env on a fake backend with every feature, in a
// temporary folder with devvm's files and the user's layers foo and bar.
func testEnv(t *testing.T) (*Env, *backendtest.Fake) {
	t.Helper()
	dir := t.TempDir()
	d := paths.Dirs{Data: filepath.Join(dir, "data"), Config: filepath.Join(dir, "config"), Lima: filepath.Join(dir, "lima")}
	root := filepath.Join(dir, "root")
	write(t, filepath.Join(root, "base.yaml"), "cpus: 4\n")
	write(t, filepath.Join(root, "prelude.sh"), testPrelude)
	write(t, filepath.Join(root, "layers", "base.sh"), "#!/bin/bash\n")
	write(t, filepath.Join(root, "layers", "apt.sh"), "#!/bin/bash\n# apt\n")
	write(t, filepath.Join(root, ubuntu.ArchiveKeyring), string(archiveFixture(t, "archive.gpg")))
	write(t, filepath.Join(d.Layers(), "foo.sh"), "#!/bin/bash\necho foo\n")
	write(t, filepath.Join(d.Layers(), "bar.sh"), "#!/bin/bash\necho bar\n")
	write(t, d.Settings(), `{"features": []}`) // chosen: none
	caps := backend.Caps{}
	for _, f := range backend.Features {
		caps[f] = backend.Native
	}
	f := backendtest.New("fake", caps)
	f.Built = backend.Built{Origin: "https://example.com/ubuntu.img sha256:1111", Libc: "gnu"}
	// The shared tools: the repo's assets/shared-tools/, the stub mise resolving the
	// fixture lock.
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"mise.toml", "build.sh"} {
		data, err := os.ReadFile(filepath.Join(repo, "assets", "shared-tools", name)) //nolint:gosec // the repo's own files
		if err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(root, "shared-tools", name), string(data))
	}
	t.Setenv("MISE_STUB_LOCK", filepath.Join(repo, "testdata", "mise.lock"))
	tl := tools.New(d, os.DirFS(root), testVersion, &msg.Fake{}, platform.Host{})
	tl.FreeSpace = func(string) (uint64, error) { return 1 << 40, nil }
	tl.Getenv = func(string) string { return "" }
	tl.Mise = stubtest.Command(t, "mise")
	// The cloud image the backend pinned, which base's recorded package
	// lists must be as new as.
	pin := state.UbuntuPin{Release: testRelease, File: "ubuntu-26.04-server-cloudimg-arm64.img", Digest: "sha256:" + strings.Repeat("1", 64)}
	if err := state.WriteUbuntuPin(d, pin); err != nil {
		t.Fatal(err)
	}
	return quiet(&Env{Dirs: d, Assets: os.DirFS(root), Backend: f, Tools: tl, Version: testVersion}), f
}

// testRelease is the cloud image release testEnv's backend pinned.
const testRelease = "release-20261016"

// testVersion is testEnv's devvm's version.
const testVersion = "0.1.0-test"

// record reads the record of the image with key, which must say it was
// built in the last minute by testEnv's devvm, and returns it without
// that time and version.
func record(t *testing.T, b *Env, key string) state.Image {
	t.Helper()
	rec, err := state.ReadImage(b.Dirs, key)
	if err != nil {
		t.Fatal(err)
	}
	if since := time.Since(rec.Built); since < -time.Second || since > time.Minute {
		t.Errorf("%s was built at %s", key, rec.Built)
	}
	if rec.MadeBy != testVersion {
		t.Errorf("%s was made by %q, want %q", key, rec.MadeBy, testVersion)
	}
	rec.Built, rec.MadeBy = time.Time{}, ""
	return rec
}

// assetsDir is the folder of testEnv's devvm's files, its Assets.
func assetsDir(b *Env) string { return filepath.Join(filepath.Dir(b.Dirs.Data), "root") }

// testPrelude is testEnv's prelude.sh, which every script a build runs
// with bash gets before it: devvm's own and the user's layers.
const testPrelude = "# prelude\n"

// script is a script's step as a build runs it: testPrelude, then data.
func script(name, data string) image.Step {
	return image.Step{Name: name, Data: []byte(testPrelude + data)}
}

// layerSteps is a layer's steps: testEnv's apt.sh lists as root, then s.
func layerSteps(s image.Step) []image.Step {
	return []image.Step{{Name: "apt.sh", Data: []byte(testPrelude + "#!/bin/bash\n# apt\n"), Args: []string{"lists"}, Root: true}, s}
}

// aptSteps is base's steps around steps: testEnv's apt.sh begin, then
// steps, then apt.sh end, both as root.
func aptSteps(steps ...image.Step) []image.Step {
	apt := func(arg string) image.Step {
		return image.Step{Name: "apt.sh", Data: []byte(testPrelude + "#!/bin/bash\n# apt\n"), Args: []string{arg}, Root: true}
	}
	return slices.Concat([]image.Step{apt("begin")}, steps, []image.Step{apt("end")})
}

// sameScripts reports whether two lists of steps have the same names,
// scripts, arguments and users, in order.
func sameScripts(a, b []image.Step) bool {
	return slices.EqualFunc(a, b, func(x, y image.Step) bool {
		return x.Name == y.Name && bytes.Equal(x.Data, y.Data) && slices.Equal(x.Args, y.Args) && x.Root == y.Root
	})
}

// quiet makes b print nothing, keep the features chosen, and answer yes,
// without a terminal.
func quiet(b *Env) *Env {
	b.Reporter = &msg.Fake{OnConfirm: func(msg.M) bool { return true }}
	b.Pick = func(fs []Feature) []Feature { return fs }
	return b
}

// fake is what quiet gave b to print through, for a test to change.
func fake(b *Env) *msg.Fake { return b.Reporter.(*msg.Fake) }

// Layers are devvm's built-in ones and the user's scripts with names image
// names can hold; nothing else gets past Names' Check.
func TestLayersAndCheck(t *testing.T) {
	b, _ := testEnv(t)
	write(t, filepath.Join(b.Dirs.Layers(), "Bad_Name.sh"), "")
	write(t, filepath.Join(b.Dirs.Layers(), "base.sh"), "")
	write(t, filepath.Join(assetsDir(b), "layers", "stray.sh"), "") // not a built-in layer
	if err := os.Mkdir(filepath.Join(b.Dirs.Layers(), "dir.sh"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A layer with a shared tool's name isn't one; one with a built-in
	// layer's is the built-in one, listed once.
	write(t, filepath.Join(b.Dirs.Layers(), "node.sh"), "")
	write(t, filepath.Join(b.Dirs.Layers(), "docker.sh"), "")
	if got, want := b.Layers(), []string{"base", "bar", "desktop", "docker", "foo", "nix"}; !slices.Equal(got, want) {
		t.Errorf("Layers() = %q, want %q", got, want)
	}
	names := Names{b}
	if err := names.Check("stray"); err == nil || err.Error() != msg.UnknownLayer("stray", "base bar desktop docker foo nix").Error() {
		t.Errorf("Check(stray) = %v", err)
	}
	for _, ok := range []image.Name{"base", "foo", "foo+bar", "base+foo", "docker", "foo+docker", "nix", "docker+nix", "desktop", "foo+desktop"} {
		if err := names.Check(ok); err != nil {
			t.Errorf("Check(%q): %v", ok, err)
		}
	}
	// A recorded name's parts as they are, an empty one among them.
	for _, bad := range []image.Name{"", "stray", "Bad_Name", "dir", "foo+", "foo++bar", "../../x", "foo+nope", "-x"} {
		if err := names.Check(bad); msg.IDOf(err) != "unknown_layer" {
			t.Errorf("Check(%q) = %v, want unknown_layer", bad, err)
		}
	}
	if err := names.Check("foo+node"); msg.NamesOf(err) != "tool_not_layer tool=node" {
		t.Errorf("Check(foo+node) = %v", msg.NamesOf(err))
	}
}

// A name as the user gives it: base, empty parts and repeats dropped,
// each layer checked bottom first, the first refusal the name's.
func TestParseName(t *testing.T) {
	b, _ := testEnv(t)
	parse := Names{b}.Parse
	const layers = " layers=base bar desktop docker foo nix"
	for in, want := range map[string]image.Name{
		"base": "base", "+": "base", "base+base": "base", "foo": "foo", "base+foo": "foo", "foo+base+bar": "foo+bar",
		"foo++bar": "foo+bar", "foo+bar+foo": "foo+bar", "bar+foo": "bar+foo", "foo+": "foo", "docker+nix": "docker+nix",
	} {
		if got, err := parse(in); err != nil || got != want {
			t.Errorf("parse(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for in, want := range map[string]string{
		"":              "unknown_layer layer=" + layers,
		"stray":         "unknown_layer layer=stray" + layers,
		"nosuch+node":   "unknown_layer layer=nosuch" + layers,
		"node+nosuch":   "tool_not_layer tool=node",
		"foo+claude":    "tool_not_layer tool=claude",
		"foo+Bad_Name":  "unknown_layer layer=Bad_Name" + layers,
		"../../x":       "unknown_layer layer=../../x" + layers,
		"foo/bar":       "unknown_layer layer=foo/bar" + layers,
		"-x+foo":        "unknown_layer layer=-x" + layers,
		"foo+bar+stray": "unknown_layer layer=stray" + layers,
	} {
		if _, err := parse(in); msg.NamesOf(err) != want {
			t.Errorf("parse(%q) = %v, want %s", in, msg.NamesOf(err), want)
		}
	}
}

// An image is base and layers alone: build and rebuild refuse a shared
// tool's name, naming it, before anything runs.
func TestBuildRefusesTools(t *testing.T) {
	b, f := testEnv(t)
	for _, name := range []string{"node", "foo+claude", "go+foo"} {
		tool := map[string]string{"node": "node", "foo+claude": "claude", "go+foo": "go"}[name]
		for verb, do := range map[string]func(context.Context, image.Name) error{"Build": Builds{b}.Build, "Rebuild": Builds{b}.Rebuild} {
			if err := do(t.Context(), image.Name(name)); err == nil || msg.NamesOf(err) != "tool_not_layer tool="+tool {
				t.Errorf("%s(%q) = %v", verb, name, err)
			}
		}
	}
	if len(f.Calls()) > 0 {
		t.Errorf("calls = %q", f.Methods())
	}
}

// The shared tools of a new VM are built for base's libc as it's built,
// and on a musl --from base, for its digest.
func TestToolTarget(t *testing.T) {
	b, f := testEnv(t)
	b.From = "alpine:3@sha256:" + strings.Repeat("a", 64)
	f.Built.Libc = "musl"
	if err := (Builds{b}).Build(t.Context(), "base"); err != nil {
		t.Fatal(err)
	}
	if tg, err := b.ToolTarget(); tg != (tools.Target{Libc: "musl", Base: b.From}) || err != nil {
		t.Errorf("ToolTarget = %+v, %v", tg, err)
	}
}

// A --from gnu base's glibc version is in its record, and every image's
// on it, and its VMs' tools are checked against it.
func TestToolTargetGlibc(t *testing.T) {
	b, f := testEnv(t)
	f.Built.Libc, f.Built.Glibc = "gnu", "2.36"
	if err := (Builds{b}).Build(t.Context(), "foo"); err != nil {
		t.Fatal(err)
	}
	if tg, err := b.ToolTarget(); tg != (tools.Target{Libc: "gnu", Glibc: "2.36"}) || err != nil {
		t.Errorf("ToolTarget = %+v, %v", tg, err)
	}
	for _, name := range []image.Name{"base", "foo"} {
		key, err := b.Key(name)
		if err != nil {
			t.Fatal(err)
		}
		if rec, err := state.ReadImage(b.Dirs, key); err != nil || rec.Glibc != "2.36" {
			t.Errorf("%s's record: %+v, %v", name, rec, err)
		}
	}
}

// Before base is built, its libc is gnu, with no glibc version.
func TestToolTargetUnbuilt(t *testing.T) {
	b, _ := testEnv(t)
	if tg, err := b.ToolTarget(); tg != (tools.Target{Libc: "gnu"}) || err != nil {
		t.Errorf("ToolTarget = %+v, %v", tg, err)
	}
}

func TestBuild(t *testing.T) {
	b, f := testEnv(t)
	ctx := t.Context()
	if err := (Builds{b}).Build(ctx, "foo+bar"); err != nil {
		t.Fatal(err)
	}
	chain, err := b.Chain("foo+bar")
	if err != nil {
		t.Fatal(err)
	}
	var specs []backend.ImageSpec
	for _, c := range f.Calls() {
		if c.Method == "BuildImage" {
			specs = append(specs, c.Args[0].(backend.ImageSpec))
		}
	}
	if len(specs) != 3 {
		t.Fatalf("built %d images, want 3: %+v", len(specs), specs)
	}
	for i, want := range []backend.ImageSpec{
		{Image: "base", Key: chain[0].Key, Recipe: image.Recipe{Steps: aptSteps(script("base.sh", "#!/bin/bash\n"))}},
		{Image: "foo", Key: chain[1].Key, Parent: chain[0].Key, Recipe: image.Recipe{Steps: layerSteps(script("foo.sh", "#!/bin/bash\necho foo\n"))}},
		{Image: "foo+bar", Key: chain[2].Key, Parent: chain[1].Key, Recipe: image.Recipe{Steps: layerSteps(script("bar.sh", "#!/bin/bash\necho bar\n"))}},
	} {
		want.Net = backend.NetInternet
		got := specs[i]
		if got.Image != want.Image || got.Key != want.Key || got.Parent != want.Parent || got.Net != want.Net ||
			got.From != nil || got.MemoryGiB != 0 || !sameScripts(got.Recipe.Steps, want.Recipe.Steps) {
			t.Errorf("build %d: %+v, want %+v", i, got, want)
		}
	}
	if base := record(t, b, chain[0].Key); base != (state.Image{Name: "base", Chain: chain[:1].String(), Digest: f.Built.Origin, Libc: "gnu"}) {
		t.Errorf("base's record: %+v", base)
	}
	if top := record(t, b, chain[2].Key); top != (state.Image{Name: "foo+bar", Chain: chain.String(), Libc: "gnu"}) {
		t.Errorf("foo+bar's record: %+v", top)
	}

	// Built: nothing to do, for the name as the user may give it.
	n := len(f.Calls())
	given, err := Names{b}.Parse("base+foo+bar")
	if err != nil {
		t.Fatal(err)
	}
	if err := (Builds{b}).Build(ctx, given); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.Calls()[n:] {
		if c.Method != "Images" {
			t.Errorf("a built image called %s", c.Method)
		}
	}
}

// A built-in layer runs devvm's script, not the user's of the same name.
func TestBuildBuiltinLayer(t *testing.T) {
	b, f := testEnv(t)
	write(t, filepath.Join(assetsDir(b), "layers", "docker.sh"), "#!/bin/bash\necho devvm's\n")
	write(t, filepath.Join(b.Dirs.Layers(), "docker.sh"), "#!/bin/bash\necho yours\n")
	if err := (Builds{b}).Build(t.Context(), "foo+docker"); err != nil {
		t.Fatal(err)
	}
	calls := f.Calls()
	last := calls[len(calls)-1]
	if last.Method != "BuildImage" {
		t.Fatalf("last call %s", last.Method)
	}
	got := last.Args[0].(backend.ImageSpec)
	if got.Image != "foo+docker" || !sameScripts(got.Recipe.Steps, layerSteps(script("docker.sh", "#!/bin/bash\necho devvm's\n"))) {
		t.Errorf("built %+v", got)
	}
	if data, err := b.LayerData("docker"); err != nil || string(data) != testPrelude+"#!/bin/bash\necho devvm's\n" {
		t.Errorf("LayerData(docker) = %q, %v", data, err)
	}
}

// An image above base keeps its parent's C library.
func TestBuildKeepsTheLibc(t *testing.T) {
	b, f := testEnv(t)
	f.Built.Libc = "musl"
	if err := (Builds{b}).Build(t.Context(), "foo"); err != nil {
		t.Fatal(err)
	}
	key, err := b.Key("foo")
	if err != nil {
		t.Fatal(err)
	}
	if rec, err := state.ReadImage(b.Dirs, key); err != nil || rec.Libc != "musl" || rec.Digest != "" {
		t.Errorf("foo's record: %+v, %v", rec, err)
	}
}

// A build says once what base got when the backend fitted it to the host,
// and says nothing about it when it didn't.
func TestBuildSaysFitted(t *testing.T) {
	for _, c := range []struct {
		fitted backend.Size
		want   []string
	}{
		{backend.Size{}, nil},
		{backend.Size{CPUs: 2, MemoryGiB: 4}, []string{"base_fitted cpus=2 memory=4"}},
		{backend.Size{CPUs: 4, MemoryGiB: 3.5}, []string{"base_fitted cpus=4 memory=3.5"}},
	} {
		b, f := testEnv(t)
		f.Built.Fitted = c.fitted
		var got []string
		fake(b).OnInfo = func(m msg.M) {
			if m.ID() == "base_fitted" {
				got = append(got, m.Names())
			}
		}
		if err := (Builds{b}).Build(t.Context(), "base"); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("fitted %+v: said %q, want %q", c.fitted, got, c.want)
		}
	}
}

// A backend that can't build, or not confined, is refused before anything
// is built.
func TestBuildRequires(t *testing.T) {
	b, f := testEnv(t)
	delete(f.Capabilities, backend.FeatureBuildConfinement)
	if err := (Builds{b}).Build(t.Context(), "foo"); msg.IDOf(err) != "backend_lacks" {
		t.Errorf("Build = %v, want backend_lacks", err)
	}
	if slices.Contains(f.Methods(), "BuildImage") {
		t.Error("built anyway")
	}
}

// A build and a rebuild check the company proxy before anything: one in
// config.toml that names a user needs its password saved, as a new VM
// does.
func TestBuildChecksCompanyProxy(t *testing.T) {
	b, f := testEnv(t)
	write(t, b.Dirs.ConfigFile(), "[proxy]\nhttps = \"http://alice@proxy.corp:3128\"\n")
	if err := (Builds{b}).Build(t.Context(), "foo"); msg.NamesOf(err) != "proxy_password_missing" {
		t.Errorf("Build = %v", msg.NamesOf(err))
	}
	if err := (Builds{b}).Rebuild(t.Context(), "foo"); msg.NamesOf(err) != "proxy_password_missing" {
		t.Errorf("Rebuild = %v", msg.NamesOf(err))
	}
	if len(f.Calls()) > 0 {
		t.Errorf("calls = %q", f.Methods())
	}
	b.Tools.Proxy.Password = func(context.Context, string) (string, error) { return "pa55", nil }
	if err := (Builds{b}).Build(t.Context(), "foo"); err != nil {
		t.Errorf("saved: %v", msg.NamesOf(err))
	}
}

// Below the free space building base wants, or another image, nothing of
// it is made: the backend only pins base's origin and is asked which
// images it has.
func TestBuildRefusesLowSpace(t *testing.T) {
	b, f := testEnv(t)
	b.Backend = backendtest.Stored{Fake: f, Dirs: []string{"/lima"}}
	free := uint64(12<<30 - 1)
	b.FreeSpace = func(dir string) (uint64, error) {
		if dir != "/lima" {
			t.Errorf("checked %s", dir)
		}
		return free, nil
	}
	if err := (Builds{b}).Build(t.Context(), "foo"); err == nil || msg.NamesOf(err) != "disk_low dir=/lima free=11.9 name=base need=12" {
		t.Errorf("Build with %d free = %v", free, err)
	}
	if ms := f.Methods(); !slices.Equal(ms, []string{"PinOrigin", "Images", "Images"}) {
		t.Errorf("calls = %q", ms)
	}
	free = 12 << 30
	if err := (Builds{b}).Build(t.Context(), "base"); err != nil {
		t.Fatal(err)
	}
	free = 8<<30 - 1
	n := len(f.Calls())
	if err := (Builds{b}).Build(t.Context(), "foo"); err == nil || msg.NamesOf(err) != "disk_low dir=/lima free=7.9 name=foo need=8" {
		t.Errorf("Build of foo with %d free = %v", free, err)
	}
	if slices.Contains(f.Methods()[n:], "BuildImage") {
		t.Errorf("built anyway: %q", f.Methods()[n:])
	}
	key, err := b.Key("foo")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.ReadImage(b.Dirs, key); err == nil {
		t.Error("foo has a record")
	}
}

// An image the backend has, with its record, needs nothing of building:
// a backend that can't build it now still uses it. One without its record
// is refused before it's deleted.
func TestBuiltNeedsNoBuild(t *testing.T) {
	b, f := testEnv(t)
	if err := (Builds{b}).Build(t.Context(), "base"); err != nil {
		t.Fatal(err)
	}
	delete(f.Capabilities, backend.FeatureBuildConfinement)
	before := len(f.Calls())
	if err := (Builds{b}).Build(t.Context(), "base"); err != nil {
		t.Errorf("Build of a built image = %v", err)
	}
	key, err := b.Key("base")
	if err != nil {
		t.Fatal(err)
	}
	if err := state.RemoveImage(b.Dirs, key); err != nil {
		t.Fatal(err)
	}
	if err := (Builds{b}).Build(t.Context(), "base"); msg.IDOf(err) != "backend_lacks" {
		t.Errorf("Build of a leftover = %v, want backend_lacks", err)
	}
	if ms := f.Methods()[before:]; slices.Contains(ms, "DeleteImage") || slices.Contains(ms, "BuildImage") {
		t.Errorf("calls = %q", ms)
	}
}

// An image another devvm is building (no record, its lock held) is left
// alone: a build of it waits, saying so, rebuild and Delete don't delete
// it, and it's listed as building. Once its lock is free, it's a
// leftover, deleted and built again.
func TestBuildInProgress(t *testing.T) {
	b, f := testEnv(t)
	ctx := t.Context()
	if err := (Builds{b}).Build(ctx, "foo"); err != nil {
		t.Fatal(err)
	}
	key, err := b.Key("foo+bar")
	if err != nil {
		t.Fatal(err)
	}
	f.ImageKeys = append(f.ImageKeys, key) // another devvm's, unfinished
	path, err := state.ImageLock(b.Dirs, key)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := platform.LockFile(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	deleted := func() bool {
		for _, c := range f.Calls() {
			if c.Method == "DeleteImage" && c.Args[0] == key {
				return true
			}
		}
		return false
	}
	var said msg.Recorder
	fake(b).OnInfo = said.Add
	b.WaitNote = time.Millisecond

	soon, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if err := (Builds{b}).Build(soon, "foo+bar"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Build while another builds it = %v", err)
	}
	if !slices.Contains(said.Names(), "image_build_waiting image=foo+bar") {
		t.Errorf("said %q", said.Names())
	}
	ls, err := (Images{b}).Listing(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.IndexFunc(ls, func(l Listed) bool { return l.Key == key }); i < 0 || ls[i].State != StateBuilding {
		t.Errorf("listed %+v", ls)
	}
	if err := (Images{b}).Delete(ctx, []string{key}); msg.IDOf(err) != "image_building" {
		t.Errorf("Delete while it builds = %v", err)
	}
	b.Force = true
	if err := (Builds{b}).Rebuild(ctx, "foo"); err != nil {
		t.Fatal(err)
	}
	if deleted() {
		t.Fatal("deleted while another devvm builds it")
	}

	unlock()
	if ls, err = (Images{b}).Listing(ctx); err != nil {
		t.Fatal(err)
	}
	if i := slices.IndexFunc(ls, func(l Listed) bool { return l.Key == key }); i < 0 || ls[i].State != StateUnfinished {
		t.Errorf("listed once let go %+v", ls)
	}
	if err := (Images{b}).Delete(ctx, []string{key}); err != nil || !deleted() {
		t.Errorf("Delete once let go = %v, deleted %v", err, deleted())
	}
}

// Two builds of one image take turns: the second finds it built.
func TestBuildTakesTurns(t *testing.T) {
	b, f := testEnv(t)
	ctx := t.Context()
	errs := make(chan error, 2)
	for range 2 {
		go func() { errs <- (Builds{b}).Build(ctx, "foo") }()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	for _, m := range f.Methods() {
		if m == "BuildImage" {
			n++
		}
	}
	if n != 2 { // base and foo, once each
		t.Errorf("built %d images: %q", n, f.Methods())
	}
}

// A failed build writes no record, so the next build starts over.
func TestFailedBuildHasNoRecord(t *testing.T) {
	b, f := testEnv(t)
	f.Errs = map[string]error{"BuildImage": backend.Logged(msg.ScriptFailed("base.sh", "x"))}
	if err := (Builds{b}).Build(t.Context(), "base"); err == nil {
		t.Fatal("Build succeeded")
	}
	if found, err := state.AnyImage(b.Dirs); found || err != nil {
		t.Errorf("records after a failed build: %v, %v", found, err)
	}
}

func TestList(t *testing.T) {
	b, f := testEnv(t)
	ctx := t.Context()
	if err := (Builds{b}).Build(ctx, "foo"); err != nil {
		t.Fatal(err)
	}
	f.ImageKeys = append(f.ImageKeys, "0123456789ab") // no record: unfinished
	write(t, filepath.Join(b.Dirs.Layers(), "foo.sh"), "#!/bin/bash\necho edited\n")
	imgs, err := (Images{b}).List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, img := range imgs {
		got[img.Name] = img.Current
	}
	if want := map[string]bool{"base": true, "foo": false, "": false}; !maps.Equal(got, want) {
		t.Errorf("List: %v, want %v", got, want)
	}
}

// Rebuild gives the image a new key, and deletes the old one and those on
// it, never the others.
func TestRebuild(t *testing.T) {
	b, f := testEnv(t)
	ctx := t.Context()
	if err := (Builds{b}).Build(ctx, "foo+bar"); err != nil {
		t.Fatal(err)
	}
	old, err := b.Chain("foo+bar")
	if err != nil {
		t.Fatal(err)
	}
	// foo+bar is built on foo: without a terminal, only with --force.
	if err := (Builds{b}).Rebuild(ctx, "foo"); msg.NamesOf(err) != "no_input answer=--force" {
		t.Fatalf("rebuild without a terminal or --force: %v", err)
	}
	b.Force = true
	if err := (Builds{b}).Rebuild(ctx, "foo"); err != nil {
		t.Fatal(err)
	}
	newFoo, err := b.Key("foo")
	if err != nil {
		t.Fatal(err)
	}
	if newFoo == old[1].Key {
		t.Error("foo kept its key")
	}
	var deleted []string
	for _, c := range f.Calls() {
		if c.Method == "DeleteImage" {
			deleted = append(deleted, c.Args[0].(string))
		}
	}
	if want := []string{old[1].Key, old[2].Key}; !slices.Equal(deleted, want) {
		t.Errorf("deleted %q, want %q", deleted, want)
	}
	if want := []string{old[0].Key, newFoo}; !slices.Equal(f.ImageKeys, want) {
		t.Errorf("images left %q, want %q", f.ImageKeys, want)
	}
	for _, k := range []string{old[1].Key, old[2].Key} {
		if _, err := state.ReadImage(b.Dirs, k); err == nil {
			t.Errorf("%s kept its record", k)
		}
	}
}

// A build has the backend pin what base starts from when nothing is
// pinned, once a run and before any key, and says what it pinned; a
// rebuild of base pins the newest, a rebuild of a layer nothing more. A
// pin that fails builds nothing.
func TestBuildPinsOrigin(t *testing.T) {
	b, f := testEnv(t)
	ctx := t.Context()
	var said []string
	fake(b).OnInfo = func(m msg.M) { said = append(said, m.Names()) }
	f.Pinned = "release-20260101"
	pins := func() []backend.Pin {
		var out []backend.Pin
		for _, c := range f.Calls() {
			if c.Method == "PinOrigin" {
				out = append(out, c.Args[1].(backend.Pin))
			}
		}
		return out
	}
	if err := (Builds{b}).Build(ctx, "foo"); err != nil {
		t.Fatal(err)
	}
	if err := (Builds{b}).Build(ctx, "bar"); err != nil {
		t.Fatal(err)
	}
	if got := pins(); !slices.Equal(got, []backend.Pin{backend.PinFirst}) {
		t.Errorf("builds pinned %v, want one PinFirst", got)
	}
	if !slices.Contains(said, "origin_pinned origin=release-20260101") {
		t.Errorf("said %q", said)
	}
	b.Force = true
	if err := (Builds{b}).Rebuild(ctx, "foo"); err != nil {
		t.Fatal(err)
	}
	if err := (Builds{b}).Rebuild(ctx, "base"); err != nil {
		t.Fatal(err)
	}
	if got := pins(); !slices.Equal(got, []backend.Pin{backend.PinFirst, backend.PinNewest}) {
		t.Errorf("rebuilds pinned %v, want PinNewest for base alone", got)
	}

	b2, f2 := testEnv(t)
	f2.Errs = map[string]error{"PinOrigin": msg.UbuntuUnreachable("x", "26.04")}
	if err := (Builds{b2}).Build(ctx, "base"); msg.IDOf(err) != "ubuntu_unreachable" {
		t.Errorf("a failed pin: %v", err)
	}
	if slices.Contains(f2.Methods(), "BuildImage") {
		t.Errorf("built after a failed pin: %q", f2.Methods())
	}
}

// base that doesn't boot from what the backend pinned in this run says
// so, pins the newest older one and builds on that, once; one that
// doesn't boot from what was pinned before, or that fails otherwise,
// fails as it is.
func TestBuildFallsBack(t *testing.T) {
	booted := backend.Logged(msg.StartFailed("devvm-img-x"))
	for _, c := range []struct {
		name   string
		pinned string // what PinOrigin pins in this run
		fails  int    // how many builds fail
		err    error
		ok     bool
		pins   []backend.Pin
		builds int
	}{
		{"pinned now, then the older", "release-20260201", 1, &backend.BootError{Err: booted}, true, []backend.Pin{backend.PinFirst, backend.PinOlder}, 2},
		{"the older doesn't boot either", "release-20260201", 2, &backend.BootError{Err: booted}, false, []backend.Pin{backend.PinFirst, backend.PinOlder}, 2},
		{"pinned before", "", 1, &backend.BootError{Err: booted}, false, []backend.Pin{backend.PinFirst}, 1},
		{"another failure", "release-20260201", 1, booted, false, []backend.Pin{backend.PinFirst}, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			b, f := testEnv(t)
			var warned []string
			fake(b).OnWarn = func(m msg.M) { warned = append(warned, m.Names()) }
			f.Pinned = c.pinned
			n := 0
			f.BuildErr = func(backend.ImageSpec) error {
				if n++; n <= c.fails {
					return c.err
				}
				return nil
			}
			err := (Builds{b}).Build(t.Context(), "base")
			if (err == nil) != c.ok {
				t.Fatalf("got %v, want ok %v", err, c.ok)
			}
			if !c.ok {
				if _, logged := errors.AsType[*backend.LoggedError](err); !logged {
					t.Errorf("lost the logged error: %v", err)
				}
			}
			var pins []backend.Pin
			for _, call := range f.Calls() {
				if call.Method == "PinOrigin" {
					pins = append(pins, call.Args[1].(backend.Pin))
				}
			}
			if !slices.Equal(pins, c.pins) || n != c.builds {
				t.Errorf("pins %v and %d builds, want %v and %d", pins, n, c.pins, c.builds)
			}
			if fell := slices.Contains(warned, "origin_no_boot origin="+c.pinned); fell != (len(c.pins) == 2) {
				t.Errorf("warned %q", warned)
			}
		})
	}
}

// A rebuild on one backend changes that backend's key alone: another
// backend's images on the same data folder stay current, so its VMs
// aren't outdated.
func TestRebuildIsPerBackend(t *testing.T) {
	b, _ := testEnv(t)
	ctx := t.Context()
	caps := backend.Caps{}
	for _, f := range backend.Features {
		caps[f] = backend.Native
	}
	other := quiet(&Env{Dirs: b.Dirs, Assets: b.Assets, Backend: backendtest.New("other", caps), Tools: b.Tools})
	if err := (Builds{other}).Build(ctx, "base"); err != nil {
		t.Fatal(err)
	}
	before, err := other.Chain("base")
	if err != nil {
		t.Fatal(err)
	}
	if err := (Builds{b}).Rebuild(ctx, "base"); err != nil {
		t.Fatal(err)
	}
	if after, err := other.Chain("base"); err != nil || after.String() != before.String() {
		t.Errorf("the other backend's base: %s, then %s, %v", before, after, err)
	}
	var warned msg.Recorder
	fake(other).OnWarn = warned.Add
	if err := (Builds{other}).WarnIfOutdated(before.String()); err != nil || len(warned.IDs()) != 0 {
		t.Errorf("the other backend's VM warned %v, %v", warned.IDs(), err)
	}
	if imgs, err := (Images{other}).List(ctx); err != nil || len(imgs) != 1 || !imgs[0].Current {
		t.Errorf("the other backend's images: %+v, %v", imgs, err)
	}
}

// An old image the backend won't delete (a VM uses it) stays.
func TestRebuildKeepsImagesInUse(t *testing.T) {
	b, f := testEnv(t)
	ctx := t.Context()
	if err := (Builds{b}).Build(ctx, "base"); err != nil {
		t.Fatal(err)
	}
	old := f.ImageKeys[0]
	f.Errs = map[string]error{"DeleteImage": msg.ImageInUse(old, "devvm-p-0123456789abcd")}
	if err := (Builds{b}).Rebuild(ctx, "base"); err != nil {
		t.Fatal(err)
	}
	if _, err := state.ReadImage(b.Dirs, old); err != nil {
		t.Errorf("the image in use lost its record: %v", err)
	}
}

// fromEnv is testEnv with --from node:24, the conversion's
// scripts, and a registry whose digest for it is *digest; lookups counts
// what it's asked.
func fromEnv(t *testing.T, digest *string, lookups *int) (*Env, *backendtest.Fake) {
	t.Helper()
	b, f := testEnv(t)
	for _, s := range []string{"build.sh", "bootify.sh", "kernel.sh"} {
		write(t, filepath.Join(assetsDir(b), "oci", s), "#!/bin/bash\n")
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*lookups++
		w.Header().Set("Docker-Content-Digest", *digest)
	}))
	t.Cleanup(srv.Close)
	reg := from.NewRegistry(nil)
	if tr, ok := srv.Client().Transport.(*http.Transport); ok {
		reg.Client.Transport().TLSClientConfig = tr.TLSClientConfig // trusts the test server's certificate
	}
	reg.Base = func(string) string { return srv.URL }
	b.From = "node:24"
	b.Lookup = &from.Lookup{
		Dirs:     b.Dirs,
		Registry: reg,
		DockerID: func(context.Context, string) (string, bool) { t.Error("Docker asked"); return "", false },
		Reporter: &msg.Fake{OnWarn: func(m msg.M) { t.Errorf("warned: %s", m.Names()) }},
	}
	return b, f
}

// --from: base starts from the image's digest, looked up when building;
// every image on it records both.
func TestBuildFrom(t *testing.T) {
	digest, lookups := "sha256:"+strings.Repeat("a", 64), 0
	b, f := fromEnv(t, &digest, &lookups)
	f.Built = backend.Built{Origin: "node@" + digest, Libc: "musl"}
	if err := (Builds{b}).Build(t.Context(), "foo"); err != nil {
		t.Fatal(err)
	}
	if lookups != 1 {
		t.Errorf("looked up %d times", lookups)
	}
	chain, err := b.Chain("foo")
	if err != nil {
		t.Fatal(err)
	}
	var specs []backend.ImageSpec
	for _, c := range f.Calls() {
		if c.Method == "BuildImage" {
			specs = append(specs, c.Args[0].(backend.ImageSpec))
		}
	}
	if len(specs) != 2 || specs[0].From == nil || *specs[0].From != (backend.From{Ref: "node:24", Digest: "node@" + digest}) || specs[1].From != nil {
		t.Fatalf("specs: %+v", specs)
	}
	for i, name := range []string{"base", "foo"} {
		rec := record(t, b, chain[i].Key)
		if want := (state.Image{Name: name, Chain: chain[:i+1].String(), From: "node:24", Digest: "node@" + digest, Libc: "musl"}); rec != want {
			t.Errorf("%s's record: %+v; want %+v", name, rec, want)
		}
	}
	// Ubuntu's base is another image.
	ubuntu := &Env{Dirs: b.Dirs, Assets: b.Assets, Backend: b.Backend, Tools: b.Tools}
	if k, err := ubuntu.Key("base"); err != nil || k == chain[0].Key {
		t.Errorf("Ubuntu's base: %s, %v", k, err)
	}
	// Keys, lists and outdated checks use the recorded digest: no lookups.
	if imgs, err := (Images{b}).List(t.Context()); err != nil || len(imgs) != 2 || !imgs[0].Current || !imgs[1].Current || imgs[0].From != "node:24" {
		t.Errorf("List: %+v, %v", imgs, err)
	}
	if err := (Builds{b}).WarnIfOutdated(chain.String()); err != nil || lookups != 1 {
		t.Errorf("WarnIfOutdated: %v, %d lookups", err, lookups)
	}

	// The tag moved: the next build looks it up, and base and foo are new.
	digest = "sha256:" + strings.Repeat("b", 64)
	again := quiet(&Env{Dirs: b.Dirs, Assets: b.Assets, Backend: b.Backend, Tools: b.Tools, From: b.From, Lookup: b.Lookup})
	if err := (Builds{again}).Build(t.Context(), "foo"); err != nil {
		t.Fatal(err)
	}
	moved, err := again.Chain("foo")
	if err != nil {
		t.Fatal(err)
	}
	if moved[0].Key == chain[0].Key || moved[1].Key == chain[1].Key {
		t.Errorf("a moved tag kept the keys: %s, %s", moved, chain)
	}
	if imgs, err := (Images{again}).List(t.Context()); err != nil || len(imgs) != 4 || imgs[0].Current || imgs[1].Current {
		t.Errorf("after the move, the old images are stale: %+v, %v", imgs, err)
	}
}

// A pinned digest is never looked up, and rebuilds count apart from
// other bases'.
func TestBuildFromPinned(t *testing.T) {
	digest, lookups := "", 0
	b, f := fromEnv(t, &digest, &lookups)
	b.From = "node:24@sha256:" + strings.Repeat("c", 64)
	if err := (Builds{b}).Rebuild(t.Context(), "base"); err != nil {
		t.Fatal(err)
	}
	if lookups != 0 {
		t.Errorf("looked up %d times", lookups)
	}
	rs, err := state.ReadRebuilds(b.Dirs)
	if err != nil {
		t.Fatal(err)
	}
	if rs.Counter(b.Backend.Name(), b.From, "base") == "" {
		t.Error("no counter for the pinned base")
	}
	if rs.Counter(b.Backend.Name(), "", "base") != "" {
		t.Error("Ubuntu's base was rebuilt")
	}
	for _, c := range f.Calls() {
		if c.Method == "BuildImage" && c.Args[0].(backend.ImageSpec).From.Digest != b.From {
			t.Errorf("built %+v", c.Args[0])
		}
	}
}

// A backend without --from is refused before anything is built.
func TestBuildFromRequires(t *testing.T) {
	digest, lookups := "sha256:"+strings.Repeat("a", 64), 0
	b, f := fromEnv(t, &digest, &lookups)
	delete(f.Capabilities, backend.FeatureFrom)
	if err := (Builds{b}).Build(t.Context(), "foo"); msg.IDOf(err) != "backend_lacks" {
		t.Errorf("Build = %v, want backend_lacks", err)
	}
	if slices.Contains(f.Methods(), "BuildImage") {
		t.Error("built anyway")
	}
}
