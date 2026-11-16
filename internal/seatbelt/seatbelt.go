// Package seatbelt is macOS's sandbox, Seatbelt, as devvm confines its processes with it:
// sandbox-exec by its full path (Exec), a string and a path as a profile has them (String,
// Path, and the links a lookup goes through), and the profile of a VM's proxy's child
// (ChildProfile, Sandbox). The Lima backend's profile for its hostagent is its own, written with
// these.
package seatbelt

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/devvmhq/devvm/internal/paths"
)

// Exec is macOS's sandbox-exec, by its full path: never one found on PATH.
const Exec = "/usr/bin/sandbox-exec"

// String quotes s as a Seatbelt string: backslashes and double quotes escaped, so s can't end
// the string.
func String(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// Path is a path as Seatbelt matches it, with its folder's links resolved (Seatbelt sees real
// paths, paths.PhysicalParent), as a profile string.
func Path(p string) string {
	return String(paths.PhysicalParent(p))
}

// linksMax bounds how many links Links follows, as the system's own lookups do (ELOOP).
const linksMax = 32

// Links are the symbolic links a lookup of p goes through, p itself included when it's one, and
// those on the way to each link's target, each as Seatbelt sees it (Path's form, unquoted): a
// sandboxed lookup through a link needs its metadata.
func Links(p string) []string {
	var out []string
	seen := map[string]bool{}
	todo := []string{filepath.Clean(p)}
	for len(todo) > 0 && len(seen) < linksMax {
		q := todo[0]
		todo = todo[1:]
		for ; ; q = filepath.Dir(q) {
			if fi, err := os.Lstat(q); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
				l := paths.PhysicalParent(q)
				if !seen[l] {
					seen[l] = true
					out = append(out, l)
					if t, err := os.Readlink(q); err == nil {
						if !filepath.IsAbs(t) {
							t = filepath.Join(filepath.Dir(l), t)
						}
						todo = append(todo, filepath.Clean(t))
					}
				}
			}
			if filepath.Dir(q) == q {
				break
			}
		}
	}
	return out
}
