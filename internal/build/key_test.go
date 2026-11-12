package build

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/assets"
	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/backend/backendtest"
	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/state"
)

// keyEnv is testEnv with devvm's features fzf, omb and tools, devvm's
// docker layer and base's policy, and the features fzf and tools chosen;
// with from, base starts from it, its digest recorded. (The
// backend's own files, such as Lima's base.yaml, are its origin's: Lima's
// tests check them.)
func keyEnv(t *testing.T, from string) *Env {
	t.Helper()
	b, _ := testEnv(t)
	own := assetsDir(b)
	for name, data := range map[string]string{
		"features/fzf.sh": "# fzf\n", "features/omb.sh": "# omb\n", "features/tools.sh": "# tools\n",
		"layers/docker.sh": "# docker\n", "layers/base.policy": "example.com\n",
	} {
		write(t, filepath.Join(own, name), data)
	}
	saveFeatures(t, b.Dirs, []string{"fzf", "tools"})
	if from != "" {
		b.From = from
		if err := state.SetFromDigest(b.Dirs, from, "node@sha256:aaaa"); err != nil {
			t.Fatal(err)
		}
	}
	return b
}

var keyImages = []string{"base", "foo", "foo+bar"}

// keys are b's keys of keyImages.
func keys(t *testing.T, b *Env) map[string]string {
	t.Helper()
	keys := map[string]string{}
	for _, img := range keyImages {
		k, err := b.Key(image.Name(img))
		if err != nil {
			t.Fatalf("%s: %v", img, err)
		}
		keys[img] = k
	}
	return keys
}

// edit appends a line to a file, making it if need be.
func edit(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // the test's own folder
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	write(t, path, string(data)+"# edit\n")
}

func remove(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

func bump(t *testing.T, b *Env, backendName, from, name string) {
	t.Helper()
	if err := state.BumpRebuild(b.Dirs, backendName, from, name); err != nil {
		t.Fatal(err)
	}
}

// Each image's key changes with each of its inputs, and with its parent's
// key, and with nothing else.
func TestKeyChangesWithEachInput(t *testing.T) {
	all, above := keyImages, []string{"foo", "foo+bar"}
	own := func(b *Env, name string) string { return filepath.Join(assetsDir(b), name) }
	layer := func(b *Env, name string) string { return filepath.Join(b.Dirs.Layers(), name) }
	tests := []struct {
		name   string
		from   string
		change func(t *testing.T, b *Env)
		want   []string // the images whose keys change
	}{
		{"layers/base.sh", "", func(t *testing.T, b *Env) { edit(t, own(b, "layers/base.sh")) }, all},
		{"layers/apt.sh", "", func(t *testing.T, b *Env) { edit(t, own(b, "layers/apt.sh")) }, all},
		{"the prelude", "", func(t *testing.T, b *Env) { edit(t, own(b, "prelude.sh")) }, all},
		{"the backend", "", func(_ *testing.T, b *Env) { b.Backend = backendtest.New("other", b.Backend.Caps()) }, all},
		{"a backend's origin step", "", func(_ *testing.T, b *Env) {
			b.Backend.(*backendtest.Fake).OriginSteps = []image.Step{{Name: "ubuntu.img", Args: []string{"x"}}}
		}, all},
		{"no origin steps is none", "", func(_ *testing.T, b *Env) { b.Backend.(*backendtest.Fake).OriginSteps = []image.Step{} }, nil},
		{"a chosen built-in feature's script", "", func(t *testing.T, b *Env) { edit(t, own(b, "features/fzf.sh")) }, all},
		{"a chosen feature's script gone", "", func(t *testing.T, b *Env) { remove(t, own(b, "features/tools.sh")) }, all},
		{"a script in ~/.config/devvm/features", "", func(t *testing.T, b *Env) {
			write(t, filepath.Join(b.Dirs.Config, "features", "tools.sh"), "# mine\n")
		}, nil},
		{"a feature chosen", "", func(t *testing.T, b *Env) { saveFeatures(t, b.Dirs, []string{"fzf", "tools", "omb"}) }, all},
		{"a feature dropped", "", func(t *testing.T, b *Env) { saveFeatures(t, b.Dirs, []string{"fzf"}) }, all},
		{"the features' order", "", func(t *testing.T, b *Env) { saveFeatures(t, b.Dirs, []string{"tools", "fzf"}) }, all},
		{"--from", "", func(_ *testing.T, b *Env) {
			b.From = "node:24@sha256:" + strings.Repeat("a", 64)
		}, all},
		{"base's rebuild counter", "", func(t *testing.T, b *Env) { bump(t, b, "fake", "", "base") }, all},
		{"a layer's script", "", func(t *testing.T, b *Env) { edit(t, layer(b, "foo.sh")) }, above},
		{"a layer's script deleted", "", func(t *testing.T, b *Env) { remove(t, layer(b, "foo.sh")) }, above},
		{"the top layer's script", "", func(t *testing.T, b *Env) { edit(t, layer(b, "bar.sh")) }, []string{"foo+bar"}},
		{"a layer's rebuild counter", "", func(t *testing.T, b *Env) { bump(t, b, "fake", "", "foo") }, above},
		{"the top layer's rebuild counter", "", func(t *testing.T, b *Env) { bump(t, b, "fake", "", "foo+bar") }, []string{"foo+bar"}},

		{"an unchosen feature's script", "", func(t *testing.T, b *Env) { edit(t, own(b, "features/omb.sh")) }, nil},
		{"a policy file", "", func(t *testing.T, b *Env) { edit(t, own(b, "layers/base.policy")) }, nil},
		{"another layer", "", func(t *testing.T, b *Env) { edit(t, layer(b, "baz.sh")) }, nil},
		{"a rebuild counter for --from", "", func(t *testing.T, b *Env) { bump(t, b, "fake", "node:24", "foo") }, nil},
		{"another backend's rebuild counter", "", func(t *testing.T, b *Env) { bump(t, b, "qemu", "", "base") }, nil},
		{"the tools' build script", "", func(t *testing.T, b *Env) { edit(t, own(b, "shared-tools/build.sh")) }, nil},

		{"the --from digest (a moved tag)", "node:24", func(t *testing.T, b *Env) {
			if err := state.SetFromDigest(b.Dirs, "node:24", "node@sha256:bbbb"); err != nil {
				t.Fatal(err)
			}
		}, all},
		{"another --from", "node:24", func(_ *testing.T, b *Env) { b.From = "node:25" }, all},
		{"a rebuild counter with --from", "node:24", func(t *testing.T, b *Env) { bump(t, b, "fake", "node:24", "foo") }, above},
		{"another --from's counter, once written alike", "node:24", func(t *testing.T, b *Env) { bump(t, b, "fake", "node-24", "foo") }, nil},
		{"another backend's counter with --from", "node:24", func(t *testing.T, b *Env) { bump(t, b, "qemu", "node:24", "base") }, nil},
		{"a rebuild counter without --from", "node:24", func(t *testing.T, b *Env) { bump(t, b, "fake", "", "foo") }, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := keyEnv(t, tt.from)
			before := keys(t, b)
			tt.change(t, b)
			after := keys(t, b)
			for _, img := range keyImages {
				changed := before[img] != after[img]
				if want := slices.Contains(tt.want, img); changed != want {
					t.Errorf("%s: changed %v, want %v", img, changed, want)
				}
			}
		})
	}
}

// The same files elsewhere give the same keys, each 12 hex digits, and
// different images different ones.
func TestKeyIsStable(t *testing.T) {
	a, b := keyEnv(t, ""), keyEnv(t, "")
	first := keys(t, a)
	for _, ks := range []map[string]string{keys(t, a), keys(t, b)} {
		for _, img := range keyImages {
			if ks[img] != first[img] {
				t.Errorf("%s: %s, then %s", img, first[img], ks[img])
			}
		}
	}
	for img, k := range first {
		if !image.IsKey(k) {
			t.Errorf("%s: key %q isn't 12 hex digits", img, k)
		}
	}
	if first["base"] == first["foo"] || first["foo"] == first["foo+bar"] {
		t.Errorf("keys repeat: %v", first)
	}
}

// A chain is each image's key, base first.
func TestChain(t *testing.T) {
	b := keyEnv(t, "")
	ks := keys(t, b)
	c, err := b.Chain("foo+bar")
	if err != nil {
		t.Fatal(err)
	}
	if want := (image.Chain{{Name: "base", Key: ks["base"]}, {Name: "foo", Key: ks["foo"]}, {Name: "foo+bar", Key: ks["foo+bar"]}}); !slices.Equal(c, want) {
		t.Fatalf("got %v, want %v", c, want)
	}
}

// What a key can't be made of is an error: an image name that isn't
// one, a backend without a name, a feature's name that isn't one, and
// devvm's own scripts missing.
func TestKeyRefuses(t *testing.T) {
	b := keyEnv(t, "")
	for _, name := range []image.Name{"", "Foo", "foo+", "+foo", "base+foo", "foo+base", "foo/bar", "../x", "-x"} {
		if _, err := b.Key(name); err == nil {
			t.Errorf("%q: no error", name)
		}
	}
	in, err := b.inputs()
	if err != nil {
		t.Fatal(err)
	}
	in.features = []string{"../x"}
	if _, err := b.plan(in, image.Base); err == nil {
		t.Error("feature ../x: no error")
	}
	// b on another backend: Env holds locks, so it isn't copied.
	on := func(be backend.Backend) *Env {
		return &Env{Dirs: b.Dirs, Assets: b.Assets, Backend: be, Review: b.Review, Tools: b.Tools, From: b.From, Version: b.Version}
	}
	unnamed := on(backendtest.New("", b.Backend.Caps()))
	if _, err := unnamed.Key(image.Base); err == nil {
		t.Error("no backend: no error")
	}
	for _, name := range []string{"layers/docker.sh", "layers/base.sh"} {
		remove(t, filepath.Join(assetsDir(b), name))
		if _, err := b.Key("foo+docker"); err == nil {
			t.Errorf("no %s: no error", name)
		}
	}
	// The backend's origin failing.
	f := backendtest.New("fake", b.Backend.Caps())
	f.Errs = map[string]error{"Origin": errors.New("no origin")}
	if _, err := on(f).Key(image.Base); err == nil {
		t.Error("no origin: no error")
	}
}

// A deleted layer's key differs from an empty one's, and a user's
// docker.sh changes nothing: a built-in layer is devvm's script.
func TestKeyOfMissingAndBuiltinLayers(t *testing.T) {
	b := keyEnv(t, "")
	remove(t, filepath.Join(b.Dirs.Layers(), "foo.sh"))
	missing, err := b.Key("foo")
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(b.Dirs.Layers(), "foo.sh"), "")
	if empty, err := b.Key("foo"); err != nil || empty == missing {
		t.Errorf("a missing layer and an empty one: %s, %s, %v", missing, empty, err)
	}
	first, _ := b.Key("foo+docker")
	edit(t, filepath.Join(b.Dirs.Layers(), "docker.sh"))
	if k, _ := b.Key("foo+docker"); k != first {
		t.Error("a user's docker.sh changed the built-in layer's key")
	}
}

// A user's layer runs with the prelude before it, which its step holds,
// so its image's key differs from the one an image built without it
// recorded; a deleted one is still missing.
func TestUserLayerGetsThePrelude(t *testing.T) {
	b := keyEnv(t, "")
	if data, err := b.LayerData("foo"); err != nil || string(data) != ran("#!/bin/bash\necho foo\n") {
		t.Errorf("LayerData(foo) = %q, %v", data, err)
	}
	in, err := b.inputs()
	if err != nil {
		t.Fatal(err)
	}
	plans, err := b.plan(in, "foo")
	if err != nil {
		t.Fatal(err)
	}
	foo := plans[1]
	without := foo.Input
	without.Recipe.Steps = slices.Clone(foo.Recipe.Steps)
	last := &without.Recipe.Steps[len(without.Recipe.Steps)-1]
	data, ok := strings.CutPrefix(string(last.Data), testPreludeLine)
	if last.Name != "foo.sh" || !ok {
		t.Fatalf("foo's last step %s: %q", last.Name, last.Data)
	}
	last.Data = []byte("#!/bin/bash\n" + data) // the user's file, as it was run
	if without.Key() == foo.Key() {
		t.Error("an image of foo built without the prelude isn't outdated")
	}
	remove(t, filepath.Join(b.Dirs.Layers(), "foo.sh"))
	if _, err := b.LayerData("foo"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a deleted layer: %v", err)
	}
}

// devvm's own files, embedded, give base and every built-in layer a key.
func TestKeyOfTheRealFiles(t *testing.T) {
	b, _ := testEnv(t)
	b.Assets = assets.FS
	for _, name := range append([]string{image.Base}, image.Builtins...) {
		if key, err := b.Key(image.Name(name)); err != nil || !image.IsKey(key) {
			t.Errorf("%s: key %q, %v", name, key, err)
		}
	}
}

// A VM's chain is outdated where a key changed: that image and every one
// above it, never those below.
func TestOutdated(t *testing.T) {
	b := keyEnv(t, "")
	chain, err := b.Chain("foo+bar")
	if err != nil {
		t.Fatal(err)
	}
	bs := Builds{b}
	if got, err := bs.Outdated(chain.String()); err != nil || got != nil {
		t.Errorf("Outdated before a change = %q, %v", got, err)
	}
	edit(t, filepath.Join(b.Dirs.Layers(), "foo.sh"))
	if got, err := bs.Outdated(chain.String()); err != nil || !slices.Equal(got, []string{"foo", "foo+bar"}) {
		t.Errorf("Outdated after editing foo = %q, %v", got, err)
	}
	if _, err := bs.Outdated("Bad=0123456789ab"); err == nil {
		t.Error("Outdated took an invalid name")
	}
}

// sent builds names on a fresh fake backend named backendName, as b is
// otherwise, and returns, by image name, what each build sent the backend
// and the image's key. The user's CAs (Trust) aren't what it sent: the
// image keeps none of them, so its key holds none.
func sent(t *testing.T, b *Env, backendName string, names ...image.Name) (sends, keys map[string]string) {
	t.Helper()
	f := backendtest.New(backendName, b.Backend.Caps())
	f.Built = backend.Built{Libc: "gnu"}
	e := quiet(&Env{Dirs: b.Dirs, Assets: b.Assets, Backend: f, Tools: b.Tools, From: b.From, Lookup: b.Lookup, Version: testVersion})
	sends, keys = map[string]string{}, map[string]string{}
	for _, name := range names {
		if err := (Builds{e}).Build(t.Context(), name); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		chain, err := e.Chain(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range chain {
			keys[l.Name] = l.Key
		}
	}
	for _, c := range f.Calls() {
		if c.Method != "BuildImage" {
			continue
		}
		spec := c.Args[0].(backend.ImageSpec)
		from := backend.From{}
		if spec.From != nil {
			from = *spec.From
		}
		// A base's packages are its key's own: what its first build
		// recorded, which every later build of the key is given
		// (packages.go).
		spec.Key, spec.Trust, spec.From, spec.Packages = "", backend.Trust{}, nil, nil
		sends[spec.Image] = fmt.Sprintf("%#v %#v", spec, from)
	}
	return sends, keys
}

// files lists the regular files under each folder.
func files(t *testing.T, dirs ...string) []string {
	t.Helper()
	var out []string
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
			if err == nil && e.Type().IsRegular() {
				out = append(out, p)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// Every byte and argument a build sends its backend changes the image's
// key: whatever changes what a build of an image sends (a script of
// devvm's or the user's, a feature chosen or its order, --from's digest)
// changes its key, and so does building on another backend. Each change
// is checked on its own, against the images as they were.
func TestKeyHashesWhatABuildSends(t *testing.T) {
	digest, lookups := "sha256:"+strings.Repeat("a", 64), 0
	for _, from := range []string{"", "node:24"} {
		b := featureEnv(t)
		if from != "" {
			b, _ = fromEnv(t, &digest, &lookups)
			write(t, filepath.Join(assetsDir(b), "features", "omb.sh"), "#!/bin/bash\n# oh-my-bash: a prompt\n")
			write(t, filepath.Join(assetsDir(b), "features", "tools.sh"), "#!/bin/bash\n# my tools\n")
		}
		write(t, filepath.Join(assetsDir(b), "layers", "docker.sh"), "#!/bin/bash\necho docker\n")
		saveFeatures(t, b.Dirs, []string{"tools", "omb", "nope"})
		names := []image.Name{"foo+bar", "docker", "foo+docker"}
		before, keys := sent(t, b, "fake", names...)
		if len(before) != 5 {
			t.Fatalf("%q: built %d images, want 5", from, len(before))
		}
		differed := map[string]bool{} // the images some change sent otherwise
		check := func(change string, after, changed map[string]string) {
			t.Helper()
			for img, s := range after {
				if s == before[img] {
					continue
				}
				differed[img] = true
				if changed[img] == keys[img] {
					t.Errorf("%q: %s changes what %s's build sends, not its key %s", from, change, img, keys[img])
				}
			}
		}
		defer func() {
			if len(differed) != len(before) {
				t.Errorf("%q: only %v were sent otherwise by any change", from, differed)
			}
		}()
		// Each file of devvm's and the user's, edited, and then put back.
		for _, p := range files(t, assetsDir(b), b.Dirs.Layers()) {
			data, err := os.ReadFile(p) //nolint:gosec // the test's own folder
			if err != nil {
				t.Fatal(err)
			}
			write(t, p, string(data)+"\n# edited\n")
			after, changed := sent(t, b, "fake", names...)
			check("editing "+p, after, changed)
			write(t, p, string(data))
		}
		// A feature's script that wasn't there.
		nope := filepath.Join(assetsDir(b), "features", "nope.sh")
		write(t, nope, "#!/bin/bash\n")
		after, changed := sent(t, b, "fake", names...)
		check("adding nope.sh", after, changed)
		if err := os.Remove(nope); err != nil {
			t.Fatal(err)
		}
		// The features in another order.
		saveFeatures(t, b.Dirs, []string{"omb", "tools"})
		after, changed = sent(t, b, "fake", names...)
		check("the features' order", after, changed)
		saveFeatures(t, b.Dirs, []string{"tools", "omb", "nope"})
		// Another backend sends the same, but builds its own images.
		if _, other := sent(t, b, "other", names...); other["base"] == keys["base"] {
			t.Errorf("%q: another backend's base has the same key", from)
		}
		if from != "" {
			// --from's tag moved to another image.
			old := digest
			digest = "sha256:" + strings.Repeat("b", 64)
			after, changed = sent(t, b, "fake", names...)
			check("--from's digest", after, changed)
			digest = old
		}
	}
}
