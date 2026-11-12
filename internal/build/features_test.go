package build

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/devvmhq/devvm/assets"
	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/backend/backendtest"
	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/paths"
	"github.com/devvmhq/devvm/internal/settings"
)

// featureEnv is testEnv with devvm's features fzf, omb and tools, beside
// files that aren't features, and a script in ~/.config/devvm/features,
// which isn't one either: a user's extras are layers.
func featureEnv(t *testing.T) *Env {
	t.Helper()
	b, _ := testEnv(t)
	own := filepath.Join(assetsDir(b), "features")
	write(t, filepath.Join(own, "omb.sh"), "#!/bin/bash\n# oh-my-bash: a prompt\n")
	write(t, filepath.Join(own, "fzf.sh"), "#!/bin/bash\n#fuzzy search  \n")
	write(t, filepath.Join(own, "tools.sh"), "#!/bin/bash\n# my tools\n")
	write(t, filepath.Join(own, "Bad_Name.sh"), "")
	write(t, filepath.Join(own, "notes.txt"), "")
	if err := os.Mkdir(filepath.Join(own, "dir.sh"), 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(b.Dirs.Config, "features", "mine.sh"), "#!/bin/bash\n# mine\n")
	return b
}

// The features are devvm's own regular <name>.sh files, sorted; a
// script in ~/.config/devvm/features isn't one. Each step is its script
// with the prelude before it.
func TestFeatures(t *testing.T) {
	b := featureEnv(t)
	if got, want := b.Features(), []string{"fzf", "omb", "tools"}; !slices.Equal(got, want) {
		t.Errorf("Features() = %q, want %q", got, want)
	}
	if got, err := b.featureStep("fzf"); err != nil || !sameScripts([]image.Step{got}, []image.Step{script("fzf.sh", "#!/bin/bash\n#fuzzy search  \n")}) {
		t.Errorf("featureStep(fzf) = %+v, %v", got, err)
	}
	for _, name := range []string{"gone", "dir", "mine"} {
		if _, err := b.featureStep(name); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("featureStep(%s): %v, want none", name, err)
		}
	}
}

// A feature's description is its script's second line, a comment.
func TestDescription(t *testing.T) {
	for _, tc := range []struct{ name, script, want string }{
		{"comment", "#!/bin/bash\n# oh-my-bash: a prompt\nset -e\n", "oh-my-bash: a prompt"},
		{"no space", "#!/bin/bash\n#fuzzy  \n", "fuzzy"},
		{"not a comment", "#!/bin/bash\nset -e\n", ""},
		{"one line", "#!/bin/bash\n", ""},
		{"empty", "", ""},
	} {
		if got := description([]byte(tc.script)); got != tc.want {
			t.Errorf("%s: description = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// saveFeatures saves base's features as devvm setup does; nil leaves them
// unchosen.
func saveFeatures(t *testing.T, d paths.Dirs, names []string) {
	t.Helper()
	if names == nil {
		if err := os.Remove(d.Settings()); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
		return
	}
	if _, err := settings.Save(d, settings.Values{Features: &names}); err != nil {
		t.Fatal(err)
	}
}

// savedFeatures reads base's features as builds do: nil until chosen.
func savedFeatures(d paths.Dirs) ([]string, error) {
	def, err := settings.Read(d)
	return def.Features.Names, err
}

// Until they're chosen, base's key has every feature; the first build
// chooses them (without a terminal: all of them) and saves the choice,
// which later builds keep. config.toml's choice wins, and a build never
// asks while it's there.
func TestChooseFeatures(t *testing.T) {
	b := featureEnv(t)
	saveFeatures(t, b.Dirs, nil)
	in, err := b.inputs()
	if err != nil || !slices.Equal(in.features, []string{"fzf", "omb", "tools"}) {
		t.Fatalf("unchosen: %q, %v", in.features, err)
	}
	before, err := b.Key("base")
	if err != nil {
		t.Fatal(err)
	}
	if err := (Builds{b}).Build(t.Context(), "base"); err != nil {
		t.Fatal(err)
	}
	got, err := savedFeatures(b.Dirs)
	if err != nil || !slices.Equal(got, []string{"fzf", "omb", "tools"}) {
		t.Errorf("saved %q, %v", got, err)
	}
	if after, err := b.Key("base"); err != nil || after != before {
		t.Errorf("base's key went from %s to %s (%v)", before, after, err)
	}
	saveFeatures(t, b.Dirs, []string{"omb"})
	if err := (Builds{b}).Build(t.Context(), "base"); err != nil {
		t.Fatal(err)
	}
	if got, err := savedFeatures(b.Dirs); err != nil || !slices.Equal(got, []string{"omb"}) {
		t.Errorf("a saved choice changed: %q, %v", got, err)
	}
	saveFeatures(t, b.Dirs, nil)
	write(t, b.Dirs.ConfigFile(), "features = [\"fzf\"]\n")
	if in, err := b.inputs(); err != nil || !slices.Equal(in.features, []string{"fzf"}) {
		t.Errorf("config.toml's: %q, %v", in.features, err)
	}
	if err := (Builds{b}).Build(t.Context(), "base"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(b.Dirs.Settings()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("chose features over config.toml's: %v", err)
	}
	if _, err := (Choice{b}).SetFeatures([]string{"omb"}); msg.IDOf(err) != "set_in_config" {
		t.Errorf("SetFeatures with config.toml's: %v", err)
	}
}

// devvm's own features are cli-tools, fzf and omb: desktop is a layer.
// Until a choice is made, base gets every one of them, and a choice of
// desktop, or of a script in ~/.config/devvm/features, is refused.
func TestOwnFeatures(t *testing.T) {
	b := featureEnv(t)
	b.Assets = assets.FS
	saveFeatures(t, b.Dirs, nil)
	want := []string{"cli-tools", "fzf", "omb"}
	if got := b.Features(); !slices.Equal(got, want) {
		t.Errorf("Features() = %q, want %q", got, want)
	}
	if in, err := b.inputs(); err != nil || !slices.Equal(in.features, want) {
		t.Errorf("unchosen: %q, %v; want %q", in.features, err, want)
	}
	for _, name := range []string{"desktop", "mine"} {
		if _, err := (Choice{b}).SetFeatures([]string{name}); msg.IDOf(err) != "no_feature" {
			t.Errorf("SetFeatures(%s) = %v, want no_feature", name, err)
		}
	}
}

// PickFeatures starts from the current choice (kept by the test's Pick),
// dropping names without a script; every feature until there's one.
func TestPickFeatures(t *testing.T) {
	b := featureEnv(t)
	for _, tc := range []struct {
		name    string
		saved   []string // nil: no file
		want    []string
		changed bool
	}{
		{"unchosen", nil, []string{"fzf", "omb", "tools"}, false},
		{"none", []string{}, nil, false},
		{"kept", []string{"tools", "fzf"}, []string{"fzf", "tools"}, true},
		{"in order", []string{"fzf", "tools"}, []string{"fzf", "tools"}, false},
		{"no script", []string{"omb", "nope", "mine"}, []string{"omb"}, true},
	} {
		saveFeatures(t, b.Dirs, tc.saved)
		changed, err := (Choice{b}).PickFeatures()
		got, rerr := savedFeatures(b.Dirs)
		if err != nil || rerr != nil || !slices.Equal(got, tc.want) || changed != tc.changed {
			t.Errorf("%s: saved %q, changed %v (%v, %v); want %q, %v", tc.name, got, changed, err, rerr, tc.want, tc.changed)
		}
	}
}

// SetFeatures saves the features named, in the order features run, and
// refuses a name that isn't one.
func TestSetFeatures(t *testing.T) {
	b := featureEnv(t)
	saveFeatures(t, b.Dirs, []string{"omb"})
	for _, tc := range []struct {
		name     string
		set      []string
		want     []string
		changed  bool
		refusing string
	}{
		{"same", []string{"omb"}, []string{"omb"}, false, ""},
		{"ordered", []string{"tools", "fzf", "tools"}, []string{"fzf", "tools"}, true, ""},
		{"none", nil, nil, true, ""},
		{"unknown", []string{"fzf", "nope"}, nil, false, "no_feature"},
	} {
		changed, err := (Choice{b}).SetFeatures(tc.set)
		if tc.refusing != "" {
			if msg.IDOf(err) != tc.refusing {
				t.Errorf("%s: err %v, want %s", tc.name, err, tc.refusing)
			}
			continue
		}
		got, rerr := savedFeatures(b.Dirs)
		if err != nil || rerr != nil || !slices.Equal(got, tc.want) || changed != tc.changed {
			t.Errorf("%s: saved %q, changed %v (%v, %v); want %q, %v", tc.name, got, changed, err, rerr, tc.want, tc.changed)
		}
	}
	if got, _ := savedFeatures(b.Dirs); len(got) != 0 {
		t.Errorf("a refused choice saved %q", got)
	}
}

// base runs base.sh, then its chosen features' scripts in the order
// chosen, each with the prelude, skipping one without a script (mine, in
// ~/.config/devvm/features, among them); a layer runs only its own.
func TestBuildRunsFeatures(t *testing.T) {
	b := featureEnv(t)
	saveFeatures(t, b.Dirs, []string{"tools", "nope", "mine", "fzf"})
	if err := (Builds{b}).Build(t.Context(), "foo"); err != nil {
		t.Fatal(err)
	}
	var got [][]image.Step
	for _, c := range b.Backend.(*backendtest.Fake).Calls() {
		if c.Method == "BuildImage" {
			got = append(got, c.Args[0].(backend.ImageSpec).Recipe.Steps)
		}
	}
	want := [][]image.Step{
		aptSteps(
			script("base.sh", "#!/bin/bash\n"),
			script("tools.sh", "#!/bin/bash\n# my tools\n"),
			script("fzf.sh", "#!/bin/bash\n#fuzzy search  \n"),
		),
		layerSteps(script("foo.sh", "#!/bin/bash\necho foo\n")),
	}
	if !slices.EqualFunc(got, want, sameScripts) {
		t.Errorf("scripts %+v, want %+v", got, want)
	}
}
