package image

import (
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"path"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/devvmhq/devvm/assets"
)

// OwnScript is the prelude, then the script; without either, an error,
// which for the script matches fs.ErrNotExist.
func TestOwnScript(t *testing.T) {
	own := fstest.MapFS{"prelude.sh": {Data: []byte("p\n")}, "layers/x.sh": {Data: []byte("#!/bin/bash\nx\n")}}
	if got, err := OwnScript(own, "layers/x.sh"); err != nil || string(got) != "eval $'p\\n'\nx\n" {
		t.Errorf("OwnScript = %q, %v", got, err)
	}
	if _, err := OwnScript(own, "layers/y.sh"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("no script: %v", err)
	}
	delete(own, "prelude.sh")
	if _, err := OwnScript(own, "layers/x.sh"); err == nil {
		t.Error("no prelude: no error")
	}
}

// WithPrelude puts the prelude, as one line, in place of a script's #!
// line, or before a script without one, a user's included; without the
// prelude, an error.
func TestWithPrelude(t *testing.T) {
	const line = "eval $'it\\'s\\n'\n"
	for _, tc := range []struct{ name, script, want string }{
		{"a shebang", "#!/bin/bash\necho hi\n", line + "echo hi\n"},
		{"env's shebang", "#!/usr/bin/env bash\necho hi\n", line + "echo hi\n"},
		{"a shebang alone", "#!/bin/bash", line},
		{"no shebang", "echo hi\n", line + "echo hi\n"},
		{"a comment first", "# #!/bin/bash\n", line + "# #!/bin/bash\n"},
		// An editor's UTF-8 byte order mark goes, so the #! line still does.
		{"a BOM, then a shebang", "\xef\xbb\xbf#!/bin/bash\necho hi\n", line + "echo hi\n"},
		{"a BOM, no shebang", "\xef\xbb\xbfecho hi\n", line + "echo hi\n"},
		{"empty", "", line},
		{"its own devvm_arch, after the prelude's", "devvm_arch=x\n", line + "devvm_arch=x\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := WithPrelude(fstest.MapFS{"prelude.sh": {Data: []byte("it's\n")}}, []byte(tc.script))
			if err != nil || string(got) != tc.want {
				t.Errorf("WithPrelude = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	if _, err := WithPrelude(fstest.MapFS{}, []byte("x\n")); err == nil {
		t.Error("no prelude: no error")
	}
}

// layerScript is a layer with pins of its own, under set -euo pipefail,
// that prints the prelude's names and its own line numbers, then fails on
// its line 9.
const layerScript = `#!/bin/bash
set -euo pipefail
t_sha256_arm64=pin-arm64
t_sha256_amd64=pin-amd64
f() { echo "f $1"; }
f ok
echo "$devvm_arch $devvm_libc $(devvm_sha t) $LINENO"
true | true
nosuchcmd
echo unreachable
`

// devvm's prelude, as its one line, runs in each bash here: the layer
// gets its names, keeps its own function and options, and bash's line
// numbers are the file's, or a line further down without a #! line.
func TestWithPreludeRuns(t *testing.T) {
	bashes := []string{}
	for _, b := range []string{"/bin/bash", "bash"} { // macOS's is 3.2
		if p, err := exec.LookPath(b); err == nil {
			bashes = append(bashes, p)
		}
	}
	if len(bashes) == 0 {
		t.Skip("no bash")
	}
	// The prelude's line holds the prelude exactly.
	prelude, err := fs.ReadFile(assets.FS, preludeFile)
	if err != nil {
		t.Fatal(err)
	}
	line, ok := strings.CutPrefix(preludeLine(prelude), "eval ")
	for _, bash := range bashes {
		out, err := exec.CommandContext(t.Context(), bash, "-c", "printf %s "+line).Output() //nolint:gosec // the test's own bash
		if !ok || err != nil || string(out) != string(prelude) {
			t.Errorf("%s: the prelude's line gave %q, %v", bash, out, err)
		}
	}
	// The echo is the file's line 7 and nosuchcmd its line 9; without the
	// #! line, they're the file's 6 and 8, which bash numbers 7 and 9.
	for _, tc := range []struct{ name, script string }{
		{"a #! line", layerScript},
		{"none", strings.TrimPrefix(layerScript, "#!/bin/bash\n")},
	} {
		data, err := WithPrelude(assets.FS, []byte(tc.script))
		if err != nil {
			t.Fatal(err)
		}
		// The backend's exports for a company proxy or the user's CAs share
		// the first line (lima's proxyExports, trust's exports).
		for _, run := range []struct{ bash, pre string }{
			{bashes[0], ""}, {bashes[len(bashes)-1], ""}, {bashes[0], "export A='a b'; export B=1; "},
		} {
			bash := run.bash
			cmd := exec.CommandContext(t.Context(), bash, "-s") //nolint:gosec // the test's own bash
			cmd.Stdin = strings.NewReader(run.pre + string(data))
			var stdout, stderr strings.Builder
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			want := fmt.Sprintf("f ok\n%s gnu pin-%s 7\n", runtime.GOARCH, runtime.GOARCH)
			if err == nil || stdout.String() != want || !strings.Contains(stderr.String(), "line 9: nosuchcmd") {
				t.Errorf("%s, %s: %v, stdout %q (want %q), stderr %q", tc.name, bash, err, stdout.String(), want, stderr.String())
			}
		}
	}
}

// preludeScripts are devvm's scripts that its builds run with bash, and
// so read through OwnScript: the only ones that may use the prelude.
func preludeScripts(name string) bool {
	dir := path.Dir(name)
	return dir == "layers" || dir == "features" || name == "shared-tools/build.sh" || name == "oci/build.sh"
}

// Only the scripts that get the prelude use it, and each one that does
// checks it came first, for each of its variables it reads.
func TestPreludeUsers(t *testing.T) {
	uses := regexp.MustCompile(`\$\{?devvm_(arch|libc)\b|\bdevvm_sha\b`)
	var users []string
	err := fs.WalkDir(assets.FS, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(name, ".sh") || name == preludeFile {
			return err
		}
		data, err := fs.ReadFile(assets.FS, name)
		if err != nil {
			return err
		}
		if !uses.Match(data) {
			return nil
		}
		users = append(users, name)
		if !preludeScripts(name) {
			t.Errorf("%s uses the prelude, which it doesn't get", name)
		}
		for _, v := range []string{"devvm_arch", "devvm_libc"} {
			if regexp.MustCompile(`\$\{?`+v+`\b`).Match(data) && !regexp.MustCompile(`(?m)^: .*"\$\{`+v+`:\?\}"`).Match(data) {
				t.Errorf("%s reads $%s without checking it's set first", name, v)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The five that pin downloads per architecture, at least.
	for _, name := range []string{"layers/base.sh", "layers/nix.sh", "features/cli-tools.sh", "shared-tools/build.sh", "oci/build.sh"} {
		if !slices.Contains(users, name) {
			t.Errorf("%s doesn't use the prelude", name)
		}
	}
}
