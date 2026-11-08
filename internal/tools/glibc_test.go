package tools

import (
	"debug/elf"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/devvmhq/devvm/internal/backend"
	"github.com/devvmhq/devvm/internal/image"
	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/state"
)

// A gnu tool image's record has the newest glibc version its build says
// it needs (FILE.glibc), or none; a musl one's has none, and its build
// needn't say.
func TestBuildGlibc(t *testing.T) {
	e := newEnv(t)
	e.locked(t, fixture(t))
	f := fake()
	f.ToolGlibc = map[string]string{"node": "2.28"}
	files := e.files(t, "node", "bun")
	if err := e.Build(t.Context(), backend.Review{}, f, baseKey, gnuT, files); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	for i, want := range []string{"2.28", ""} {
		if rec, err := state.ReadToolImage(e.Dirs, files[i]); err != nil || rec.Glibc != want {
			t.Errorf("%s's record: %+v, %v, want glibc %q", files[i], rec, err, want)
		}
	}
	e.checkHashes(t, files)

	musl, err := e.File(parsed(t, fixture(t)), "node", muslT)
	if err != nil {
		t.Fatal(err)
	}
	f.ToolsBuild = func(spec backend.ToolsSpec) error {
		write(t, filepath.Join(spec.Out, spec.Args[2]), []byte("image"))
		return nil
	}
	if err := e.Build(t.Context(), backend.Review{}, f, muslKey, muslT, []string{musl}); err != nil {
		t.Fatal(msg.NamesOf(err))
	}
	if rec, err := state.ReadToolImage(e.Dirs, musl); err != nil || rec.Glibc != "" || rec.Libc != "musl" || rec.Base != muslDigest {
		t.Errorf("musl's record: %+v, %v", rec, err)
	}
}

// What a gnu build says it needs must be a plain file with one version,
// or nothing: anything else is refused, and the image stays out of the
// share.
func TestBuildGlibcRefused(t *testing.T) {
	victim := filepath.Join(t.TempDir(), "victim")
	write(t, victim, []byte("2.17\n"))
	for name, out := range map[string]func(t *testing.T, path string){
		"missing":       func(*testing.T, string) {},
		"not a version": func(t *testing.T, p string) { write(t, p, []byte("glibc 2.28\n")) },
		"two versions":  func(t *testing.T, p string) { write(t, p, []byte("2.28\n2.17\n")) },
		"too long":      func(t *testing.T, p string) { write(t, p, []byte(strings.Repeat("9", 100))) },
		"a symlink": func(t *testing.T, p string) {
			if err := os.Symlink(victim, p); err != nil {
				t.Fatal(err)
			}
		},
		"a folder": func(t *testing.T, p string) {
			if err := os.Mkdir(p, 0o700); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.locked(t, fixture(t))
			f := fake()
			f.ToolsBuild = func(spec backend.ToolsSpec) error {
				file := filepath.Join(spec.Out, spec.Args[2])
				write(t, file, []byte("image"))
				out(t, file+".glibc")
				return nil
			}
			node := e.files(t, "node")
			if err := e.Build(t.Context(), backend.Review{}, f, baseKey, gnuT, node); msg.NamesOf(err) != "tool_bad_output file="+node[0]+".glibc" {
				t.Errorf("Build = %v", msg.NamesOf(err))
			}
			if e.Built(node[0]) {
				t.Error("the image reached the share")
			}
			e.noLeftovers(t)
		})
	}
}

// A --from gnu base whose glibc is older than the newest one a tool image
// needs refuses it, naming the tool and both versions; Ubuntu's own base
// and musl aren't checked.
func TestRuns(t *testing.T) {
	e := newEnv(t)
	e.locked(t, fixture(t))
	files := e.files(t, "node", "bun")
	for i, glibc := range []string{"2.28", ""} {
		if err := state.WriteToolImage(e.Dirs, files[i], state.ToolImage{SHA256: "x", Libc: "gnu", Glibc: glibc, Backend: "fake", Built: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	_, v, _ := image.ParseToolFile(files[0])
	for _, c := range []struct {
		tg   Target
		want string
	}{
		{Target{Libc: "gnu", Glibc: "2.24"}, "tool_needs_glibc has=2.24 needs=2.28 tool=node version=" + v},
		{Target{Libc: "gnu", Glibc: "2.9"}, "tool_needs_glibc has=2.9 needs=2.28 tool=node version=" + v},
		{Target{Libc: "gnu", Glibc: "2.28"}, ""},
		{Target{Libc: "gnu", Glibc: "2.36"}, ""},
		{Target{Libc: "gnu"}, ""},
		{Target{Libc: "musl", Glibc: "2.24"}, ""},
	} {
		if got := msg.NamesOf(e.Runs(files, c.tg)); got != c.want {
			t.Errorf("Runs(%+v) = %q, want %q", c.tg, got, c.want)
		}
	}
	if err := e.Runs([]string{e.files(t, "go")[0]}, Target{Libc: "gnu", Glibc: "2.36"}); err == nil {
		t.Error("Runs of an image with no record: no error")
	}
}

// A musl base refuses a tool with builds for glibc alone (pi), whose lock
// entry there is the glibc download, before anything is built, naming it;
// a gnu base takes it, and a musl one every other tool.
func TestHasBuilds(t *testing.T) {
	e := newEnv(t)
	e.locked(t, fixture(t))
	pi, others := e.files(t, "pi"), e.files(t, "node", "claude", "opencode")
	for _, c := range []struct {
		files []string
		tg    Target
		want  string
	}{
		{append(slices.Clone(others), pi...), Target{Libc: "musl", Base: "sha256:x"}, "tool_glibc_only tool=pi"},
		{others, Target{Libc: "musl", Base: "sha256:x"}, ""},
		{pi, Target{Libc: "gnu"}, ""},
		{pi, Target{Libc: "gnu", Glibc: "2.17"}, ""},
	} {
		if got := msg.NamesOf(HasBuilds(c.files, c.tg)); got != c.want {
			t.Errorf("HasBuilds(%v, %+v) = %q, want %q", c.files, c.tg, got, c.want)
		}
	}
	if !image.GlibcOnly("pi") || image.GlibcOnly("opencode") || image.GlibcOnly("bun") {
		t.Error("GlibcOnly isn't pi's alone")
	}
}

// glibcNeedsFunc is build.sh's glibc_needs, to run alone.
func glibcNeedsFunc(t *testing.T) string {
	t.Helper()
	m := regexp.MustCompile(`(?ms)^glibc_needs\(\) \{\n.*?^\}\n`).Find(read(t, repo(t, "assets", "shared-tools", "build.sh")))
	if m == nil {
		t.Fatal("build.sh has no glibc_needs")
	}
	return string(m)
}

// newestNeed is the newest GLIBC_ version an ELF file's version needs
// name, read with Go's debug/elf.
func newestNeed(t *testing.T, path string) string {
	t.Helper()
	f, err := elf.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	needs, _ := f.DynamicVersionNeeds()
	best := ""
	for _, n := range needs {
		for _, d := range n.Needs {
			v, ok := strings.CutPrefix(d.Dep, "GLIBC_")
			if !ok {
				continue
			}
			if p := strings.SplitN(v, ".", 3); len(p) >= 2 {
				v = p[0] + "." + p[1]
			}
			if image.ValidGlibc(v) && (best == "" || image.GlibcOlder(best, v)) {
				best = v
			}
		}
	}
	return best
}

// build.sh's glibc_needs gives what a tool's ELF files need, as Go's
// debug/elf reads it, leaving out test data and files that only mention
// GLIBC_. It needs a glibc host with readelf (Linux), for a real program.
func TestGlibcNeeds(t *testing.T) {
	if _, err := exec.LookPath("readelf"); err != nil {
		t.Skip("no readelf here")
	}
	var prog, want string
	for _, p := range []string{"/bin/ls", "/usr/bin/ls", "/bin/sh"} {
		if want = newestNeed(t, p); want != "" {
			prog = p
			break
		}
	}
	if prog == "" {
		t.Skip("no program here needs glibc")
	}
	run := func(dir string) string {
		t.Helper()
		out, err := exec.CommandContext(t.Context(), "bash", "-c", "set -euo pipefail\n"+glibcNeedsFunc(t)+`glibc_needs "$1"`, "-", dir).CombinedOutput() //nolint:gosec // the repo's own script
		if err != nil {
			t.Fatalf("glibc_needs: %v: %s", err, out)
		}
		return strings.TrimSuffix(string(out), "\n")
	}
	dir := t.TempDir()
	write(t, filepath.Join(dir, "doc", "notes.txt"), []byte("needs GLIBC_2.99\n"))
	if got := run(dir); got != "" {
		t.Errorf("no programs: %q", got)
	}
	data := read(t, prog)
	write(t, filepath.Join(dir, "bin", "tool"), data)
	write(t, filepath.Join(dir, "src", "testdata", "other.elf"), data)
	if got := run(dir); got != want {
		t.Errorf("glibc_needs = %q, want %q (debug/elf, %s)", got, want, prog)
	}
	if err := os.Remove(filepath.Join(dir, "bin", "tool")); err != nil {
		t.Fatal(err)
	}
	if got := run(dir); got != "" {
		t.Errorf("only test data: %q", got)
	}
}
