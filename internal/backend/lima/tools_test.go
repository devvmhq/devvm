package lima

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/backend"
)

func toolsSpec(t *testing.T, out string) backend.ToolsSpec {
	t.Helper()
	return backend.ToolsSpec{
		Base: baseKey, Out: out, OutAt: "/mnt/devvm-tools", Net: backend.NetInternet,
		Script: backend.Script{Name: "build.sh", Data: []byte("#!/bin/bash\n")},
		Args:   []string{"node", "24.21.0", "node-24.21.0-0123456789ab.erofs"},
	}
}

// The tools build: base stopped first, a leftover build VM deleted, then a
// clone of base whose only mount is the build's own folder, writable and
// uncached, started confined, the script run with its arguments, and the VM deleted.
func TestBuildTools(t *testing.T) {
	l, f := buildLima(t, "linux", "Running")
	f.out["limactl list --json"] += `{"name":"` + string(ToolsVM) + `","status":"Stopped"}` + "\n"
	out := t.TempDir()
	if err := l.BuildTools(t.Context(), toolsSpec(t, out)); err != nil {
		t.Fatal(err)
	}
	checkLines(t, f.lines(), []string{
		"limactl stop devvm-img-" + baseKey,
		"limactl delete -f " + string(ToolsVM),
		`limactl clone --tty=false --mount-none --set .mounts = [{"location":"` + out +
			`","mountPoint":"/mnt/devvm-tools","writable":true,"9p":{"cache":"none"}}] devvm-img-` + baseKey + " " + string(ToolsVM),
		"limactl edit --tty=false " + string(ToolsVM) + " --set .ssh.localPort=",
		"limactl start --tty=false " + string(ToolsVM),
		"limactl shell --workdir / " + string(ToolsVM) + " bash -s -- node 24.21.0 node-24.21.0-0123456789ab.erofs",
		"limactl delete -f " + string(ToolsVM),
	})
}

// With CAs, their script runs first and the build script starts with
// their variables, as an image build's steps do.
func TestBuildToolsTrust(t *testing.T) {
	l, f := buildLima(t, "linux", "Stopped")
	spec := toolsSpec(t, t.TempDir())
	spec.Trust = backend.Trust{Add: backend.Script{Name: "ca.sh", Data: []byte("add\n")}, Env: []byte("export CA=1\n")}
	if err := l.BuildTools(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	var inputs []string
	for _, c := range f.calls {
		if slices.Contains(c.argv, "-s") {
			inputs = append(inputs, c.stdin)
		}
	}
	if want := []string{"add\n", "export CA=1\n#!/bin/bash\n"}; !slices.Equal(inputs, want) {
		t.Errorf("inputs %q, want %q", inputs, want)
	}
}

// A failed script is a logged tools_build_failed; the VM is left to the
// run's cleanup, so the run log ends with the script's output.
func TestBuildToolsFails(t *testing.T) {
	l, f := buildLima(t, "darwin", "Stopped")
	f.fail = []string{"limactl shell --workdir / " + string(ToolsVM) + " bash -s"}
	err := l.BuildTools(t.Context(), toolsSpec(t, t.TempDir()))
	if e, ok := errors.AsType[*backend.LoggedError](err); !ok || e.Msg.ID() != "tools_build_failed" {
		t.Fatalf("BuildTools = %v", err)
	}
	lines := f.lines()
	if last := lines[len(lines)-1]; !strings.HasPrefix(last, "limactl shell --workdir / "+string(ToolsVM)) {
		t.Errorf("last call %q, want the script", last)
	}
}

func TestBuildToolsRefuses(t *testing.T) {
	for name, change := range map[string]func(*backend.ToolsSpec){
		"a missing base":     func(s *backend.ToolsSpec) { s.Base = layerKey },
		"a comma in its out": func(s *backend.ToolsSpec) { s.Out = "/tmp/a,b" },
	} {
		t.Run(name, func(t *testing.T) {
			l, f := buildLima(t, "darwin", "Stopped")
			spec := toolsSpec(t, t.TempDir())
			change(&spec)
			if err := l.BuildTools(t.Context(), spec); err == nil {
				t.Error("BuildTools succeeded")
			}
			for _, line := range f.lines() {
				if strings.Contains(line, "clone") || strings.Contains(line, "start") {
					t.Errorf("made a VM: %s", line)
				}
			}
		})
	}
}
