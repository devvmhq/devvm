package lima

import (
	"encoding/json"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/stubtest"
)

// view is an instance as devvm reads it from limactl list --json: what
// the backend makes of it, its mounts, its ssh port, its networks, and
// the edits a start of each kind would make now (startSets), with the
// sandbox's home as ~.
type view struct {
	Instance backend.Instance
	Mounts   []viewMount
	SSHPort  int
	Networks int
	Starts   map[string][]string
}

type viewMount struct {
	Location           string
	Writable, AgentDir bool
}

// views returns every instance in l's Lima home as devvm reads it.
func views(t *testing.T, l *Lima) []view {
	t.Helper()
	ins, err := l.list(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	home := func(p string) string { return strings.Replace(p, l.Dirs.Home, "~", 1) }
	starts := map[string]backend.Confinement{
		"direct":   {Net: backend.NetDirect, Env: map[string]string{"LANG": "C.UTF-8"}},
		"proxy":    {Net: backend.NetProxy, ProxyPort: 3128, ProxyPassword: "pAss-0123_word"},
		"none":     {Net: backend.NetNone},
		"internet": {Net: backend.NetInternet},
	}
	var vs []view
	for _, in := range ins {
		v := view{Instance: in.backend(), SSHPort: in.Config.SSH.LocalPort, Networks: len(in.Config.Networks), Starts: map[string][]string{}}
		v.Instance.Project = home(v.Instance.Project)
		for i, d := range v.Instance.AgentDirs {
			v.Instance.AgentDirs[i] = home(d)
		}
		for _, m := range in.Config.Mounts {
			v.Mounts = append(v.Mounts, viewMount{Location: home(m.Location), Writable: m.writable(), AgentDir: m.agentDir()})
		}
		for name, c := range starts {
			v.Starts[name], _ = l.startSets(in.Config, c, 50022)
		}
		vs = append(vs, v)
	}
	return vs
}

// play runs the scenario on l, returning the instances as devvm reads
// them after each step, and limactl list --json's output after the step
// that leaves the most.
func play(t *testing.T, l *Lima, fx fixture) ([][]view, []byte) {
	t.Helper()
	var out [][]view
	var fullest []byte
	most := 0
	for _, s := range scenario(fx) {
		if err := s.do(t.Context(), l); err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		vs := views(t, l)
		if len(vs) > most {
			most = len(vs)
			var err error
			if fullest, err = l.Runner.Output(l.limactl(t.Context(), "list", "--json")); err != nil {
				t.Fatal(err)
			}
		}
		out = append(out, vs)
	}
	return out, fullest
}

// goldenVar names the folder that TestLimactlStubContract writes real
// Lima's golden fixtures to, when it's set (e2e's --golden sets it
// to testdata/golden/<os>): limactl --version's output, and limactl list
// --json's with the scenario's stopped dummy instances, the sandbox's
// folders and this user's name replaced.
const goldenVar = "STUB_GOLDEN"

// writeGolden writes real Lima's fixtures for TestGoldenShapes.
func writeGolden(t *testing.T, l *Lima, dir string, list []byte) {
	t.Helper()
	version, err := l.Runner.Output(l.limactl(t.Context(), "--version"))
	if err != nil {
		t.Fatal(err)
	}
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"limactl-version.txt": version, "limactl-list-stopped.json": list} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(anonymous(string(data), filepath.Dir(l.Dirs.Home), me.Username)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// anonymous is a fixture without this machine's own: home is /home/me,
// the user's name me, its full name, the timezone and a VM's environment
// made up. e2e's Golden does the same to what it captures.
func anonymous(s, home, name string) string {
	s = strings.ReplaceAll(s, home, "/home/me")
	s = regexp.MustCompile(`\b`+regexp.QuoteMeta(name)+`\b`).ReplaceAllString(s, "me")
	s = regexp.MustCompile(`"comment":"[^"]*"`).ReplaceAllString(s, `"comment":"me"`)
	s = regexp.MustCompile(`"timezone":"[^"]*"`).ReplaceAllString(s, `"timezone":"UTC"`)
	return regexp.MustCompile(`"env":\{[^{}]*\}`).ReplaceAllString(s, `"env":{"NAME":"value"}`)
}

// The limactl stub is a contract with real Lima: the scenario, run on
// dummy instances through real limactl and through the stub, leaves
// every instance the same as devvm reads it, step by step (its status,
// size, mounts, agent forwarding, ssh port and networks, and the edits
// each kind of start would make).
func TestLimactlStubContract(t *testing.T) {
	if _, err := exec.LookPath("limactl"); err != nil {
		t.Skip("limactl isn't on PATH: this test runs real Lima")
	}
	var onLima, stub [][]view
	t.Run("lima", func(t *testing.T) {
		l, fx := dummyLima(t, sandboxDirs(t), realRunner{t})
		var list []byte
		onLima, list = play(t, l, fx)
		if dir := os.Getenv(goldenVar); dir != "" {
			writeGolden(t, l, dir, list)
		}
	})
	t.Run("stub", func(t *testing.T) {
		stubtest.New(t)
		l, fx := dummyLima(t, sandboxDirs(t), stubtest.ExecRunner{})
		stub, _ = play(t, l, fx)
	})
	if t.Failed() {
		return
	}
	for i, s := range scenario(fixture{}) {
		r, st := flatten(t, onLima[i]), flatten(t, stub[i])
		if onlyR, onlyS := missing(r, st), missing(st, r); len(onlyR)+len(onlyS) > 0 {
			t.Errorf("after %s, only Lima has\n\t%s\nand only the stub\n\t%s",
				s.name, strings.Join(onlyR, "\n\t"), strings.Join(onlyS, "\n\t"))
		}
	}
}

// flatten returns v as JSON's leaves, one "path = value" each.
func flatten(t *testing.T, v any) []string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var tree any
	if err := json.Unmarshal(data, &tree); err != nil {
		t.Fatal(err)
	}
	var out []string
	var walk func(path string, x any)
	walk = func(path string, x any) {
		switch x := x.(type) {
		case map[string]any:
			for k, e := range x {
				walk(path+"."+k, e)
			}
		case []any:
			for i, e := range x {
				walk(path+"["+strconv.Itoa(i)+"]", e)
			}
		default:
			b, _ := json.Marshal(x)
			out = append(out, path+" = "+string(b))
		}
	}
	walk("", tree)
	slices.Sort(out)
	return out
}

// missing returns the lines of a that b lacks.
func missing(a, b []string) []string {
	var out []string
	for _, l := range a {
		if !slices.Contains(b, l) {
			out = append(out, l)
		}
	}
	return out
}
