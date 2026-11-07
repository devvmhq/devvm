package vm

import (
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/devvmhq/devvm/internal/image"
)

// nixMarkers are a Nix project's own files, which call for the nix layer.
var nixMarkers = []string{"flake.nix", "shell.nix", "devenv.nix"}

// marker is a part and the files in a folder's top level that call for it.
type marker struct {
	part  string
	files []string
}

// markers are the files that call for each toolchain (image.SharedTools'
// Detect), then for the nix layer, in the order detectFiles lists them:
// the same mix always gives the same image.
func markers() []marker {
	var ms []marker
	for _, d := range image.SharedTools {
		if len(d.Detect) > 0 {
			ms = append(ms, marker{d.Name, d.Detect})
		}
	}
	return append(ms, marker{image.NixLayer, nixMarkers})
}

// nixShellMax is how much of a default.nix nixShell reads.
const nixShellMax = 64 << 10

// nixShell reports whether dir's default.nix makes a shell (mkShell, or
// mkShellNoCC), as one for nix-shell does; most build a package instead.
func nixShell(dir string) bool {
	p := filepath.Join(dir, "default.nix")
	if fi, err := os.Lstat(p); err != nil || !fi.Mode().IsRegular() {
		return false
	}
	f, err := os.Open(p) //nolint:gosec // a regular file in the project folder, read
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	data, _ := io.ReadAll(io.LimitReader(f, nixShellMax))
	return strings.Contains(string(data), "mkShell")
}

// detected is a toolchain, or the nix layer, that a folder calls for, and
// the first of its marker files that calls for it.
type detected struct{ Name, File string }

// detectFiles returns the toolchains and the nix layer whose marker files
// are in dir's top level, in a fixed order, each with the file that calls
// for it: nix also for a default.nix that makes a shell. A new VM's
// default parts (defaults) start from them.
func detectFiles(dir string) []detected {
	var found []detected
	f := folder{dir: dir}
	for _, m := range markers() {
		file := f.first(m.files)
		if file == "" && m.part == image.NixLayer && nixShell(dir) {
			file = "default.nix"
		}
		if file != "" {
			found = append(found, detected{m.part, file})
		}
	}
	return found
}

// folder is a project folder's top level, as detectFiles looks in it:
// its names are read once, for the first pattern that needs them.
type folder struct {
	dir   string
	names []string
	read  bool
}

// first is the first of a marker's files in the folder: a name that's
// there, or the first name a pattern matches (path.Match's), in name
// order; "" when none is.
func (f *folder) first(files []string) string {
	for _, file := range files {
		if !strings.Contains(file, "*") {
			if _, err := os.Lstat(filepath.Join(f.dir, file)); err == nil {
				return file
			}
			continue
		}
		if !f.read {
			entries, _ := os.ReadDir(f.dir) // sorted by name; none when it can't be read
			for _, e := range entries {
				f.names = append(f.names, e.Name())
			}
			f.read = true
		}
		for _, n := range f.names {
			if ok, _ := path.Match(file, n); ok {
				return n
			}
		}
	}
	return ""
}
