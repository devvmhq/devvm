package vm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/devcontainer"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/settings"
	"github.com/devvmhq/devvm/internal/state"
	"github.com/devvmhq/devvm/internal/tools"
)

// A reread devcontainer.json's parts replace what the file gave the VM,
// layers out of its image and toolchains out of its tools, and leave the
// user's as they are.
func TestRereadParts(t *testing.T) {
	e, _, _ := upEnv(t, allNative())
	for _, c := range []struct {
		name, img         string
		names             []string
		was               devcontainerInVM
		now               []string
		wantImg           string
		wantNames, wantGv []string
	}{
		{
			name: "the file's replaced", img: "docker+desktop", names: []string{"node", "claude"},
			was: devcontainerInVM{Parts: []string{"docker", "node"}, Gave: []string{"docker", "node"}}, now: []string{"go"},
			wantImg: "desktop", wantNames: []string{"go", "claude"}, wantGv: []string{"go"},
		},
		{
			name: "the user's kept", img: "docker", names: []string{"node"},
			was: devcontainerInVM{Parts: []string{"docker", "node"}}, now: nil,
			wantImg: "docker", wantNames: []string{"node"},
		},
		{
			name: "a layer added on top", img: "desktop", names: nil,
			now:     []string{"docker"},
			wantImg: "desktop+docker", wantGv: []string{"docker"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := vmState(t, e.Dirs, "devvm-x-0123456789abcd", func(st *state.VM) error {
				st.Image = c.img
				return tools.Set(st, tools.InVM{Names: c.names})
			})
			img, names, gave := rereadParts(st, c.was, c.now)
			if img != c.wantImg || !slices.Equal(names, c.wantNames) || !slices.Equal(gave, c.wantGv) {
				t.Errorf("rereadParts = %s, %q, %q; want %s, %q, %q", img, names, gave, c.wantImg, c.wantNames, c.wantGv)
			}
			if got := tools.Names(st); !slices.Equal(got, c.names) {
				t.Errorf("its state's tools changed: %q", got)
			}
		})
	}
}

// A devcontainer.json's ports join the VM's as devvm ports add adds
// them, and read again they replace only the file's: the user's own
// stay, and one the user removed stays removed.
func TestFilePorts(t *testing.T) {
	for _, c := range []struct {
		name, cur string
		was, gave []string
		now       string
		want      string
		wantGave  []string
	}{
		{name: "new, with every port", cur: "", now: "3000", want: "all"},
		{name: "new, with none", cur: "none", now: "3000,5173", want: "3000,5173", wantGave: []string{"3000", "5173"}},
		{name: "new, one the user has", cur: "5173", now: "3000,5173", want: "5173,3000", wantGave: []string{"3000"}},
		{name: "a range of the user's", cur: "3000-3010", now: "3005", want: "3000-3010"},
		{name: "none in the file", cur: "8080", want: "8080"},
		{
			name: "the file's replaced, the user's kept", cur: "3000,8080", was: []string{"3000"}, gave: []string{"3000"},
			now: "4000", want: "8080,4000", wantGave: []string{"4000"},
		},
		{
			name: "removed by the user stays removed", cur: "8080", was: []string{"3000", "4000"}, gave: []string{"3000", "4000"},
			now: "3000,4000,5000", want: "8080,5000", wantGave: []string{"5000"},
		},
		{name: "the file gone, the last one", cur: "3000", was: []string{"3000"}, gave: []string{"3000"}, want: "none"},
		{name: "every port since", cur: "all", was: []string{"3000"}, gave: []string{"3000"}, want: "all"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, gave, err := filePorts(c.cur, c.was, c.gave, c.now)
			if err != nil || got != c.want || !slices.Equal(gave, c.wantGave) {
				t.Errorf("filePorts = %q, %q, %v; want %q, %q", got, gave, err, c.want, c.wantGave)
			}
		})
	}
}

// What a new VM gets from its devcontainer.json alone: not a part whose
// flag chose its list, nor one devvm setup's layers or the folder's
// other files call for, nor one it didn't get.
func TestFileParts(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	dc := &devcontainer.Plan{Layers: []string{"docker", "node", "go", "rust"}}
	got := Parts{Toolchains: []string{"node", "go"}, Layers: []string{"docker"}}
	for _, c := range []struct {
		name string
		o    Options
		def  settings.Defaults
		want []string
	}{
		{name: "defaults", want: []string{"docker", "go"}},
		{name: "--tools", o: Options{Toolchains: &[]string{"go"}}, want: []string{"docker"}},
		{name: "--layers", o: Options{Layers: &[]string{"docker"}}, want: []string{"go"}},
		{name: "setup's layer", def: settings.Defaults{Layers: settings.List{Names: []string{"docker"}}}, want: []string{"go"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if gave := fileParts(c.o, dir, dc, c.def, got); !slices.Equal(gave, c.want) {
				t.Errorf("fileParts = %q, want %q", gave, c.want)
			}
		})
	}
	if gave := fileParts(Options{}, dir, nil, settings.Defaults{}, got); gave != nil {
		t.Errorf("without a file: %q", gave)
	}
}

// devvm update reads a changed devcontainer.json again: a failed remake
// puts the old machine back with its from, tools and devcontainer
// section, and one that finishes gives the VM what the file says now.
func TestUpdateDevcontainer(t *testing.T) {
	e, f, _ := devcontainerTestEnv(t, `{"image": "node:24", "features": {"ghcr.io/devcontainers/features/docker-in-docker:2": {}}, "postCreateCommand": "echo one"}`)
	st := existingVM(t, e, Options{Network: "open"})
	vm := st.Name()
	before := readState(t, e.Dirs, vm)
	if before.Image != "docker" || !slices.Equal(sectionOf[devcontainerInVM](before, devcontainerSection).Gave, []string{"docker"}) {
		t.Fatalf("made %s, %+v", before.Image, sectionOf[devcontainerInVM](before, devcontainerSection))
	}
	if err := os.WriteFile(filepath.Join(e.Dir, ".devcontainer.json"), []byte(`{"image": "node:22", "postCreateCommand": "echo two"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	pinned := func(ref string) string { return ref + "@sha256:" + strings.Repeat("a", 64) }
	e.PublicImage = func(_ context.Context, ref string) (string, error) { return pinned(ref), nil }
	var said []string
	fake(e).OnInfo = func(m msg.M) { said = append(said, m.Names()) }
	var cleanups []func(context.Context)
	fake(e).OnCleanup = func(c func(context.Context)) func() {
		i := len(cleanups)
		cleanups = append(cleanups, c)
		return func() { cleanups[i] = nil }
	}
	lc := Lifecycle{e}

	// Its new machine fails to start: put back as it was.
	f.Errs = map[string]error{"Start": errors.New("no boot")}
	o := RemakeOptions{Force: true, CleanHome: true}
	if err := lc.Update(t.Context(), "devvm update", o); err == nil || err.Error() != "no boot" {
		t.Fatalf("Update: %v", msg.NamesOf(err))
	}
	if !slices.Contains(said, "devcontainer_reread file=.devcontainer.json vm="+vm.String()) {
		t.Errorf("said %q", said)
	}
	for _, c := range slices.Backward(cleanups) {
		if c != nil {
			c(t.Context())
		}
	}
	f.Errs = nil
	after := readState(t, e.Dirs, vm)
	for _, k := range []string{keyFrom, KeyDevcontainer, KeyDevcontainerCreate, keyToolNames, KeyTools} {
		if get(after, k) != get(before, k) {
			t.Errorf("undone: %s = %q, was %q", k, get(after, k), get(before, k))
		}
	}
	if d := sectionOf[devcontainerInVM](after, devcontainerSection); !d.Made.Created || remakePending(after) {
		t.Errorf("undone: made %+v, pending %v", d.Made, remakePending(after))
	}

	// Made again: the file's image, without its docker layer, its new
	// command run on the new disk.
	cleanups = nil
	if err := lc.Update(t.Context(), "devvm update", o); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	after = readState(t, e.Dirs, vm)
	d := sectionOf[devcontainerInVM](after, devcontainerSection)
	if after.Image != "base" || after.From != pinned("node:22") || d.Image != "node:22" || len(d.Gave) != 0 || remakePending(after) {
		t.Errorf("remade: %s from %s, %+v", after.Image, after.From, d)
	}
	if cmds, _ := commandsRun(f); cmds[len(cmds)-1] != "sh -c echo two" {
		t.Errorf("ran %q", cmds)
	}
	if r := sectionOf[remakeInVM](after, remakeSection); r.Reread != nil || r.Was != nil {
		t.Errorf("remake left %+v", r)
	}
}
