package upgrade

import (
	"os"
	"path/filepath"
	"strings"
)

// Kind is how this devvm was installed.
type Kind string

const (
	// Script is install.sh's: <prefix>/lib/devvm/<version>/devvm, which
	// <prefix>/bin/devvm links to.
	Script Kind = "install.sh"
	// Homebrew is Homebrew's: a keg in a Cellar, Cellar/<formula>/<version>,
	// of the formula devvm or devvm-rc.
	Homebrew Kind = "homebrew"
	// Other is any other: go install, a build from a checkout, a copy.
	Other Kind = "other"
)

// The tap's formulas: devvm, the newest release's, and devvm-rc, the
// newest prerelease's or release's, whichever is newer.
const (
	Formula   = "devvm"
	RCFormula = "devvm-rc"
)

// An Install is where this devvm is and how it was installed.
type Install struct {
	Kind Kind
	// Exe is the running binary, its links resolved.
	Exe string
	// Prefix is install.sh's prefix (DEVVM_PREFIX, ~/.local by default),
	// for Script.
	Prefix string
	// Formula is Homebrew's formula, Formula or RCFormula, for Homebrew.
	Formula string
}

// Prereleases reports whether this install follows prereleases, as
// Homebrew's devvm-rc does: its newest version is the newest release,
// prereleases included (Source.Newest).
func (in Install) Prereleases() bool { return in.Kind == Homebrew && in.Formula == RCFormula }

// Lib is the folder install.sh keeps each version in, a folder each.
func (in Install) Lib() string { return filepath.Join(in.Prefix, "lib", "devvm") }

// Link is install.sh's link to the version it installed last.
func (in Install) Link() string { return filepath.Join(in.Prefix, "bin", "devvm") }

// Detect says how the devvm at exe (os.Executable's) was installed, from
// where it is, never running anything: install.sh's when it's devvm in a
// version's folder of <prefix>/lib/devvm and <prefix>/bin/devvm links to
// it, Homebrew's when it's in a Cellar's devvm or devvm-rc keg, else
// Other.
func Detect(exe string) Install {
	bin, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return Install{Kind: Other, Exe: exe}
	}
	in := Install{Kind: Other, Exe: bin}
	verDir := filepath.Dir(bin)
	devvmDir := filepath.Dir(verDir)
	libDir := filepath.Dir(devvmDir)
	if filepath.Base(bin) == "devvm" && Valid(filepath.Base(verDir)) &&
		filepath.Base(devvmDir) == "devvm" && filepath.Base(libDir) == "lib" {
		cand := Install{Kind: Script, Exe: bin, Prefix: filepath.Dir(libDir)}
		if linked, err := filepath.EvalSymlinks(cand.Link()); err == nil && linked == bin {
			if fi, err := os.Lstat(cand.Link()); err == nil && fi.Mode()&os.ModeSymlink != 0 {
				return cand
			}
		}
	}
	for _, f := range []string{Formula, RCFormula} {
		if strings.Contains(filepath.ToSlash(bin), "/Cellar/"+f+"/") {
			in.Kind, in.Formula = Homebrew, f
		}
	}
	return in
}
