package paths

import (
	"os"
	"path/filepath"
	"strings"
)

// A path the host checks against a folder is resolved one of three ways,
// each for its own question:
//
//   - Physical: every symlink in it, as far as the path exists. For where a
//     folder or a file really is, made yet or not: a project folder against
//     devvm's own, a key against the folders VMs mount.
//   - PhysicalParent: its folder's symlinks, the last element as named. For
//     a path that may itself be a symlink devvm mustn't follow, or that the
//     VM could make: a path under .git to lock, a key's recorded path, a
//     Seatbelt rule.
//   - filepath.EvalSymlinks itself, which fails when the path doesn't
//     exist. For a path that must: the folder devvm runs in, a link's
//     TARGET, mise's folder.
//
// Then Within and Below compare it with a folder that is resolved the
// same way, by name; In and Under by identity too, for a check that keeps
// a VM away from a folder.

// Physical returns path with its symlinks resolved, as far as it exists:
// the part that doesn't is joined, as named, onto the deepest folder that
// does, so a folder devvm hasn't made yet is where it will be.
func Physical(path string) string {
	rest := ""
	for p := filepath.Clean(path); ; p = filepath.Dir(p) {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(r, rest)
		}
		if filepath.Dir(p) == p {
			return path
		}
		rest = filepath.Join(filepath.Base(p), rest)
	}
}

// PhysicalParent returns path with its folder's symlinks resolved and its
// last element as named, or path cleaned when the folder doesn't exist
// (yet: a VM could make it).
func PhysicalParent(path string) string {
	if d, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
		return filepath.Join(d, filepath.Base(path))
	}
	return filepath.Clean(path)
}

// Within reports whether path is dir or inside it: by name, so both are
// clean and resolved the same way. Every path is within /.
func Within(path, dir string) bool {
	return path == dir || Below(path, dir)
}

// Below reports whether path is inside dir, and isn't dir itself: by
// name, so both are clean and resolved the same way.
func Below(path, dir string) bool {
	return path != dir && strings.HasPrefix(path, strings.TrimSuffix(dir, "/")+"/")
}

// In reports whether path is dir or inside it: by name, as Within, or by
// identity, path or a folder above it being the same folder as dir, so a
// name that differs only in case on a volume that ignores it, such as
// ~/.Config on macOS, is still dir. A check that keeps a VM away from a
// folder uses In, never Within alone. An empty dir holds nothing.
func In(path, dir string) bool {
	if dir == "" {
		return false
	}
	if Within(path, dir) {
		return true
	}
	d, err := os.Stat(dir)
	if err != nil {
		return false
	}
	for p := filepath.Clean(path); ; p = filepath.Dir(p) {
		if fi, err := os.Stat(p); err == nil && os.SameFile(fi, d) {
			return true
		}
		if filepath.Dir(p) == p {
			return false
		}
	}
}

// Under reports whether path is inside dir, and isn't dir itself, as In
// finds it: its folder is In dir.
func Under(path, dir string) bool {
	path = filepath.Clean(path)
	return filepath.Dir(path) != path && In(filepath.Dir(path), dir)
}
