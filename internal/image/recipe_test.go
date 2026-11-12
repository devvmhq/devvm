package image

import (
	"errors"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/devvmhq/devvm/assets"
)

// OwnScript is the prelude, then the script; without either, an error,
// which for the script matches fs.ErrNotExist.
func TestOwnScript(t *testing.T) {
	own := fstest.MapFS{"prelude.sh": {Data: []byte("p\n")}, "layers/x.sh": {Data: []byte("x\n")}}
	if got, err := OwnScript(own, "layers/x.sh"); err != nil || string(got) != "p\nx\n" {
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

// WithPrelude puts the prelude before any script, a user's included;
// without the prelude, an error.
func TestWithPrelude(t *testing.T) {
	for _, tc := range []struct{ name, script, want string }{
		{"a script", "echo hi\n", "p\necho hi\n"},
		{"empty", "", "p\n"},
		{"its own devvm_arch, after the prelude's", "devvm_arch=x\n", "p\ndevvm_arch=x\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := WithPrelude(fstest.MapFS{"prelude.sh": {Data: []byte("p\n")}}, []byte(tc.script))
			if err != nil || string(got) != tc.want {
				t.Errorf("WithPrelude = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	if _, err := WithPrelude(fstest.MapFS{}, []byte("x\n")); err == nil {
		t.Error("no prelude: no error")
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
