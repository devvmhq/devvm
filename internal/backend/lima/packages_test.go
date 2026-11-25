package lima

import (
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/msg"
)

// A build's packages go into its VM before the recipe's steps, unpacked
// as root into an emptied PackagesDir, and its out/ comes back after the
// steps as a tar stream, before PackagesDir is deleted.
func TestBuildImagePackages(t *testing.T) {
	l, f := buildLima(t, "darwin", "")
	inst := "devvm-img-" + baseKey
	f.stdout = map[string]string{"limactl shell --start=false --workdir / " + inst + " sh -c cd '/' && exec 'sudo' 'sh' '-c' 'mkdir -p": "what came out"}
	var took string
	spec := baseSpec(t, l)
	spec.Packages = &backend.Packages{
		Send: func(w io.Writer) error { _, err := io.WriteString(w, "what goes in"); return err },
		Take: func(r io.Reader) error { b, err := io.ReadAll(r); took = string(b); return err },
	}
	if _, err := l.BuildImage(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	if took != "what came out" {
		t.Errorf("took %q", took)
	}
	var steps []string
	for _, c := range f.calls {
		line := c.line()
		switch {
		case strings.Contains(line, "tar -x -C"):
			steps = append(steps, "in")
			d := `'\''/var/cache/devvm/packages'\''`
			if c.stdin != "what goes in" || !strings.Contains(line, "'rm -rf "+d+" && mkdir -p "+d+" && tar -x -C "+d+"'") {
				t.Errorf("in: %s, %q", line, c.stdin)
			}
		case strings.Contains(line, "tar -c -C"):
			steps = append(steps, "out")
		case strings.Contains(line, "'sudo' 'rm' '-rf' '/var/cache/devvm/packages'"):
			steps = append(steps, "rm")
		case strings.HasSuffix(line, inst+" bash -s"):
			steps = append(steps, "base.sh")
		}
	}
	if want := []string{"in", "base.sh", "out", "rm"}; !slices.Equal(steps, want) {
		t.Errorf("steps %q, want %q", steps, want)
	}
}

// What Send or Take fails with fails the build, and so does a VM that
// can't take its packages in or give them back.
func TestBuildImagePackagesFail(t *testing.T) {
	bad := errors.New("bad")
	for name, c := range map[string]struct {
		p    backend.Packages
		fail string
		want string
	}{
		"send":     {p: backend.Packages{Send: func(io.Writer) error { return bad }}},
		"take":     {p: backend.Packages{Take: func(io.Reader) error { return bad }}},
		"in":       {p: backend.Packages{Send: func(io.Writer) error { return nil }}, fail: "tar -x", want: "packages_send_failed"},
		"out":      {p: backend.Packages{Take: func(io.Reader) error { return nil }}, fail: "tar -c", want: "packages_take_failed"},
		"deleting": {p: backend.Packages{}, fail: "'rm' '-rf'", want: "packages_take_failed"},
	} {
		t.Run(name, func(t *testing.T) {
			l, f := buildLima(t, "darwin", "")
			inst := "devvm-img-" + baseKey
			if c.fail != "" {
				prefix := "limactl shell --start=false --workdir / " + inst + " sh -c cd '/' && exec 'sudo' "
				if c.fail == "'rm' '-rf'" {
					f.fail = []string{prefix + c.fail}
				} else {
					f.fail = []string{prefix + "'sh' '-c' '" + map[string]string{"tar -x": "rm -rf", "tar -c": "mkdir -p"}[c.fail]}
				}
			}
			spec := baseSpec(t, l)
			spec.Packages = &c.p
			_, err := l.BuildImage(t.Context(), spec)
			switch {
			case c.want == "" && !errors.Is(err, bad):
				t.Errorf("got %v, want %v", err, bad)
			case c.want != "" && msg.IDOf(err) != c.want:
				t.Errorf("got %v, want %s", err, c.want)
			}
		})
	}
}

// base from Ubuntu's image whose VM doesn't start is a BootError, which
// keeps the logged failure; a layer's isn't.
func TestBuildImageNoBoot(t *testing.T) {
	l, f := buildLima(t, "darwin", "")
	f.fail = []string{"sandbox-exec"}
	_, err := l.BuildImage(t.Context(), baseSpec(t, l))
	if _, logged := errors.AsType[*backend.LoggedError](err); !errors.Is(err, backend.ErrBoot) || !logged || msg.IDOf(err) != "start_failed" {
		t.Errorf("base: %v", err)
	}
	l, f = buildLima(t, "darwin", "Stopped")
	f.fail = []string{"sandbox-exec"}
	spec := backend.ImageSpec{Image: "docker", Key: layerKey, Parent: baseKey, Net: backend.NetInternet}
	if _, err := l.BuildImage(t.Context(), spec); err == nil || errors.Is(err, backend.ErrBoot) {
		t.Errorf("a layer: %v", err)
	}
}

// A step that reached no package source (it exits 75) fails the build
// with build_sources_unreachable, not as a script that failed.
func TestBuildImageSourcesUnreachable(t *testing.T) {
	l, f := buildLima(t, "darwin", "")
	inst := "devvm-img-" + baseKey
	f.exit = map[string]int{"limactl shell --start=false " + inst + " bash -s": backend.ExitUnreachable}
	_, err := l.BuildImage(t.Context(), baseSpec(t, l))
	if e, ok := errors.AsType[*backend.LoggedError](err); !ok || e.Msg.Names() != "build_sources_unreachable script=base.sh vm="+inst {
		t.Errorf("got %v", err)
	}
	f.exit = map[string]int{"limactl shell --start=false " + inst + " bash -s": 1}
	_, err = l.BuildImage(t.Context(), baseSpec(t, l))
	if msg.IDOf(err) == "build_sources_unreachable" {
		t.Errorf("exit 1: %v", err)
	}
}
