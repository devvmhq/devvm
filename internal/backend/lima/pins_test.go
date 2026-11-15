package lima

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/leaktest"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/platform"
)

// TestMain makes the test binary devvm's _qemu when it's run as that,
// which the probe and the pin tests run in a namespace: _qemu probe DIR,
// _qemu pinned QEMU ARGS and _qemu drop QEMU ARGS, as devvm runs them.
func TestMain(m *testing.M) {
	if len(os.Args) > 3 && os.Args[1] == "_qemu" {
		var err error
		switch os.Args[2] {
		case "probe":
			err = ProbePin(os.Args[3])
		case "pinned":
			self, _ := os.Executable()
			err = RunPinned(self, os.Args[3], os.Args[4:])
		case "drop":
			err = Drop(os.Args[3], os.Args[4:])
		}
		if err != nil {
			_, _ = os.Stderr.WriteString(err.Error() + "\n") //nolint:forbidigo // the test binary as a helper process
			os.Exit(1)                                       //nolint:forbidigo // the test binary as a helper process
		}
		os.Exit(0) //nolint:forbidigo // the test binary as a helper process
	}
	// Its children write their coverage where the test's goes.
	flag.Parse()
	if f := flag.Lookup("test.gocoverdir"); f != nil && f.Value.String() != "" {
		_ = os.Setenv("GOCOVERDIR", f.Value.String()) // the test binary's children's coverage
	}
	leaktest.Main(m)
}

// pinsFile writes data, a set of pins as encodePins writes them, to a
// file of the test's own, and returns it, as PinsEnv holds it.
func pinsFile(t *testing.T, data string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "git-pins.json")
	if err := os.WriteFile(f, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

// Pins go folders first, so a folder's pin never hides one below it, and
// what _qemu reads back is what was written, private, its events file
// included.
func TestPinsOrder(t *testing.T) {
	p := backend.Pins{
		Root:     "/p",
		Fixed:    []string{".git/modules/a", ".git", "sub/.git"},
		ReadOnly: []string{".git/modules/a/config", ".git/config", ".git/hooks", "sub/.git/hooks"},
		Events:   "/d/git-pins.jsonl",
		File:     filepath.Join(t.TempDir(), "git-pins.json"),
	}
	v, err := encodePins(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(v); err != nil || v != p.File || fi.Mode().Perm() != 0o600 {
		t.Errorf("file %s: %v", v, err)
	}
	s, err := decodePins(v)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range s.Pins {
		mark := "rw "
		if e.ReadOnly {
			mark = "ro "
		}
		got = append(got, mark+e.Path)
	}
	want := []string{
		"rw .git", "rw sub/.git", "ro .git/config", "ro .git/hooks",
		"rw .git/modules/a", "ro sub/.git/hooks", "ro .git/modules/a/config",
	}
	if s.Root != "/p" || s.Events != p.Events || !slices.Equal(got, want) {
		t.Errorf("pins %s %q %s, want %q", s.Root, got, s.Events, want)
	}
}

// A folder that isn't absolute and clean, a path that leaves it, an
// events file that isn't absolute, and what isn't a set of pins are
// refused, writing and reading.
func TestPinsRefused(t *testing.T) {
	for _, p := range []backend.Pins{
		{Root: "p", ReadOnly: []string{".git/config"}},
		{Root: "/p/../q", ReadOnly: []string{".git/config"}},
		{Root: "/p", ReadOnly: []string{"../q/.git/config"}},
		{Root: "/p", ReadOnly: []string{"/etc/passwd"}},
		{Root: "/p", ReadOnly: []string{".git//config"}},
		{Root: "/p", Fixed: []string{"."}},
		{Root: "/p", Fixed: []string{".."}},
		{Root: "/p", ReadOnly: []string{".git/config"}, Events: "events"},
		{Root: "/p"},
		{Root: "/p", ReadOnly: []string{".git/config"}, File: "git-pins.json"},
	} {
		if p.File == "" {
			p.File = filepath.Join(t.TempDir(), "git-pins.json")
		}
		if _, err := encodePins(p); !errors.As(err, new(msg.M)) {
			t.Errorf("%+v: %v", p, err)
		}
	}
	for _, v := range []string{"", "x", `{"root":"/p","pins":[{"path":"../x"}]}`, `{"root":"/p","pins":[]}`} {
		if _, err := decodePins(pinsFile(t, v)); err == nil {
			t.Errorf("%q read", v)
		}
	}
	// A file that isn't absolute, isn't there, or is a symlink.
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(pinsFile(t, `{"root":"/p","pins":[{"path":".git"}]}`), link); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"", "git-pins.json", "/no/git-pins.json", link} {
		if _, err := decodePins(v); err == nil {
			t.Errorf("%q read", v)
		}
	}
}

// Without pins it can read, or off Linux, nothing pins and QEMU doesn't
// start: _qemu exec and pinned refuse, and so does the probe.
func TestPinsRefuseToStart(t *testing.T) {
	t.Setenv(PinsEnv, pinsFile(t, `{"root":"/p","pins":[{"path":"../x"}]}`))
	var m msg.M
	if err := runPinned("/no/devvm", "/no/qemu", nil); !errors.As(err, &m) || m.ID() != "qemu_pins_invalid" {
		t.Errorf("exec: %v", err)
	}
	if err := RunPinned("/no/devvm", "/no/qemu", nil); !errors.As(err, &m) || m.ID() != "qemu_pins_invalid" {
		t.Errorf("pinned: %v", err)
	}
	// A folder that isn't there, or a link.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dir, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{filepath.Join(dir, "gone"), filepath.Join(dir, "link")} {
		v, err := encodePins(backend.Pins{Root: root, ReadOnly: []string{".git/config"}, File: filepath.Join(t.TempDir(), "git-pins.json")})
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv(PinsEnv, v)
		if err := RunPinned("/no/devvm", "/no/qemu", nil); !errors.As(err, &m) || m.ID() != "qemu_pins_failed" {
			t.Errorf("%s: pinned: %v", root, err)
		}
	}
	// An events file it can't write: what changes would go unrecorded.
	v, err := encodePins(backend.Pins{Root: dir, ReadOnly: []string{".git/config"}, Events: filepath.Join(dir, "gone", "git-pins.jsonl"), File: filepath.Join(t.TempDir(), "git-pins.json")})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(PinsEnv, v)
	if err := RunPinned("/no/devvm", "/no/qemu", nil); !errors.As(err, &m) || m.ID() != "qemu_pins_failed" {
		t.Errorf("no events file: %v", err)
	}
	if err := Drop("/no/such/qemu", nil); err == nil {
		t.Error("ran a QEMU that isn't there")
	}
	if !platform.HasPins {
		if err := probePins("/no/devvm"); !errors.Is(err, errors.ErrUnsupported) {
			t.Errorf("probe off Linux: %v", err)
		}
		t.Setenv(PinsEnv, pinsFile(t, `{"root":"/p","pins":[{"path":".git"}]}`))
		if err := runPinned("/no/devvm", "/no/qemu", nil); err == nil {
			t.Error("a namespace off Linux")
		}
	}
}

// A pin event goes in the events file, a JSON object a line, and on
// QEMU's error output; with no file, on its error output alone.
func TestRecordPinEvent(t *testing.T) {
	file := filepath.Join(t.TempDir(), "git-pins.jsonl")
	recordPinEvent(file, backend.PinEvent{Path: ".git/config", What: platform.PinAgain})
	recordPinEvent(file, backend.PinEvent{Path: ".git/hooks", What: platform.PinLost})
	recordPinEvent("", backend.PinEvent{Path: ".git/x", What: platform.PinLost})
	b, err := os.ReadFile(file) //nolint:gosec // the test's own file
	if want := `{"path":".git/config","what":"pinned-again"}` + "\n" + `{"path":".git/hooks","what":"lost"}` + "\n"; err != nil || string(b) != want {
		t.Errorf("events %q, %v", b, err)
	}
	// A file it can't add to: said on the error output alone.
	dir := t.TempDir()
	recordPinEvent(dir, backend.PinEvent{Path: ".git/config", What: platform.PinAgain})
	if err := addPinEvent(dir, backend.PinEvent{What: backend.PinKeeping}); err == nil {
		t.Error("added to a folder")
	}
}

// What QEMU's process sees through mounts of its own, from its
// mountinfo: its pins, the read-only ones apart, their escapes undone;
// nothing without a QEMU to read.
func TestMountPoints(t *testing.T) {
	info := "284 280 0:42 /p/.git /p/.git rw,nosuid - tmpfs tmpfs rw\n" +
		"287 284 0:42 /p/.git/config /p/.git/config ro,nosuid - tmpfs tmpfs rw\n" +
		"288 284 0:42 /a\\040b/.git/hooks /a\\040b/.git/hooks ro - tmpfs tmpfs rw\n" +
		"short line\n"
	ro, all := mountPoints(info)
	if !slices.Equal(ro, []string{"/p/.git/config", "/a b/.git/hooks"}) || !slices.Equal(all, []string{"/p/.git", "/p/.git/config", "/a b/.git/hooks"}) {
		t.Errorf("read-only %q, all %q", ro, all)
	}
	if ro, all := mountPoints(""); ro == nil || all == nil || len(ro)+len(all) > 0 {
		t.Errorf("none: %q, %q", ro, all)
	}
	if ro, all := qemuMounts(filepath.Join(t.TempDir(), "qemu.pid")); ro != nil || all != nil {
		t.Errorf("no QEMU: %q, %q", ro, all)
	}
	pid := filepath.Join(t.TempDir(), "qemu.pid")
	if err := os.WriteFile(pid, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ro, all := qemuMounts(pid); ro != nil || all != nil {
		t.Errorf("no pid: %q, %q", ro, all)
	}
	if unescapeMount(`a\040b\`) != `a b\` {
		t.Error("escapes")
	}
}

// The probe, on a Linux host that lets devvm in a user namespace: it
// needs devvm's _qemu probe, which the test binary stands in for
// (TestMain), so here it runs ProbePin in a namespace of its own, as the
// real probe does.
func TestProbePin(t *testing.T) {
	if !platform.HasPins {
		if err := ProbePin(t.TempDir()); err == nil {
			t.Error("pinned off Linux")
		}
		return
	}
	if limit := platform.UsernsLimit(); limit != "" {
		t.Skipf("this host limits user namespaces (%s)", limit)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := probePins(self); err != nil {
		t.Skipf("this host can't pin: %v", err)
	}
}

// pinnedProject is a project folder whose .git has a config and a hooks
// folder, and a file elsewhere in it, pinned through PinsEnv, its events
// in events.
func pinnedProject(t *testing.T) (dir, events string) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{".git/hooks", "elsewhere"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{".git/config", "elsewhere/config"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("[core]\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	events = filepath.Join(t.TempDir(), "git-pins.jsonl")
	v, err := encodePins(backend.Pins{Root: dir, Fixed: []string{".git"}, ReadOnly: []string{".git/config", ".git/hooks"}, Events: events, File: filepath.Join(t.TempDir(), "git-pins.json")})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(PinsEnv, v)
	return dir, events
}

// QEMU in its namespace, a shell standing in for it: what's read-only
// can't be written, removed, renamed over or linked, a fixed folder can't
// be renamed but takes new files, and QEMU has no capability left, none
// to inherit either. A file this machine renames over a pinned one is
// pinned again, and recorded. A symlink or a file with another name is
// never pinned, and QEMU doesn't start then. Nothing stays pinned once it
// ends.
func TestRunPinned(t *testing.T) {
	if !platform.HasPins || platform.UsernsLimit() != "" {
		t.Skip("pins need a Linux host that lets devvm in a user namespace")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := probePins(self); err != nil {
		t.Skipf("this host can't pin: %v", err)
	}
	dir, events := pinnedProject(t)
	out := filepath.Join(dir, "out")
	script := `cd "$1"; exec > out; r() { if "$@" 2>/dev/null; then echo "ok $*"; else echo "refused $*"; fi; }
r sh -c 'echo x >> .git/config'
r sh -c 'echo x > .git/hooks/pre-commit'
r mv .git .moved
r mv .git/hooks .git/h
r rm -f .git/config
r mv elsewhere/config .git/config
r ln .git/config elsewhere/linked
r touch .git/index
grep -E '^Cap(Inh|Prm|Eff|Amb)' /proc/self/status | tr -s '\t' ' '
grep NoNewPrivs /proc/self/status | tr -s '\t' ' '
# This machine replaces the config, as git config does, and the new one is pinned again.
: > ready; i=0; while [ ! -e replaced ] && [ $i -lt 200 ]; do sleep 0.05; i=$((i+1)); done
i=0; while [ $i -lt 100 ]; do grep -q new .git/config && ! (echo y >> .git/config) 2>/dev/null && break; sleep 0.05; i=$((i+1)); done
r sh -c 'echo y >> .git/config'`
	done := make(chan error, 1)
	go func() { done <- runPinned(self, "/bin/sh", []string{"-c", script, "sh", dir}) }()
	waitFor(t, filepath.Join(dir, "ready"))
	cfg := filepath.Join(dir, ".git", "config")
	if err := os.WriteFile(cfg+".lock", []byte("[core]\n# new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(cfg+".lock", cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "replaced"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out) //nolint:gosec // the test's own file
	want := "refused sh -c echo x >> .git/config\nrefused sh -c echo x > .git/hooks/pre-commit\nrefused mv .git .moved\n" +
		"refused mv .git/hooks .git/h\nrefused rm -f .git/config\nrefused mv elsewhere/config .git/config\n" +
		"refused ln .git/config elsewhere/linked\nok touch .git/index\n" +
		"CapInh: 0000000000000000\nCapPrm: 0000000000000000\nCapEff: 0000000000000000\nCapAmb: 0000000000000000\nNoNewPrivs: 1\n" +
		"refused sh -c echo y >> .git/config\n"
	if string(b) != want {
		t.Errorf("in the namespace:\n%s\nwant\n%s", b, want)
	}
	ev, _ := os.ReadFile(events) //nolint:gosec // the test's own file
	if !strings.HasPrefix(string(ev), `{"what":"keeping"}`+"\n") || !strings.Contains(string(ev), `{"path":".git/config","what":"pinned-again"}`) {
		t.Errorf("events %q", ev)
	}
	if err := os.WriteFile(cfg, []byte("x"), 0o600); err != nil {
		t.Errorf("pinned after QEMU ended: %v", err)
	}
	// Another name for the config, or a symlink where a pin goes: refused,
	// and QEMU doesn't start.
	if err := os.Link(cfg, filepath.Join(dir, "elsewhere", "linked")); err != nil {
		t.Fatal(err)
	}
	if err := runPinned(self, "/bin/sh", []string{"-c", `echo started > "$1"`, "sh", out}); err == nil {
		t.Error("pinned a file with another name")
	}
	if err := os.Remove(filepath.Join(dir, "elsewhere", "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(dir, ".git/hooks")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "elsewhere"), filepath.Join(dir, ".git/hooks")); err != nil {
		t.Fatal(err)
	}
	if err := runPinned(self, "/bin/sh", []string{"-c", `echo started > "$1"`, "sh", out}); err == nil {
		t.Error("pinned through a symlink")
	}
	if b, _ := os.ReadFile(out); string(b) == "started\n" { //nolint:gosec // the test's own file
		t.Error("QEMU started with a pin refused")
	}
}

// waitFor waits for path to be there.
func waitFor(t *testing.T, path string) {
	t.Helper()
	for range 200 {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("%s never came", path)
}
