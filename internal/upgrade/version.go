package upgrade

import (
	"cmp"
	"regexp"
	"strings"
)

// releaseVersion is a release's version, as install.sh's valid_version
// takes it: x.y.z, maybe with a -suffix. It goes into addresses and
// folder names, so nothing else gets through.
var releaseVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$`)

// Valid reports whether v is a release's version.
func Valid(v string) bool { return releaseVersion.MatchString(v) }

// Prerelease reports whether v is a prerelease's version, such as
// 0.2.0-rc.1: one with a -suffix, which GitHub marks as a prerelease, so
// its releases/latest skips it, as the newest release's lookup and the
// daily check do too. Only devvm upgrade --to installs one.
func Prerelease(v string) bool { return Valid(v) && strings.Contains(v, "-") }

// Compare compares two release versions (Valid) as semver orders them:
// numbers as numbers, a version with a -suffix before the same one
// without, and suffixes part by part, numbers below words (rc.9 before
// rc.10): -1, 0 or 1.
func Compare(a, b string) int {
	ac, as, aPre := strings.Cut(a, "-")
	bc, bs, bPre := strings.Cut(b, "-")
	if c := compareParts(strings.Split(ac, "."), strings.Split(bc, ".")); c != 0 {
		return c
	}
	switch {
	case aPre && !bPre:
		return -1
	case !aPre && bPre:
		return 1
	}
	return compareParts(strings.Split(as, "."), strings.Split(bs, "."))
}

// compareParts compares dot-separated parts: numbers as numbers (longer
// is larger, so no number overflows), below words, which compare as text;
// with every part equal, more parts are larger.
func compareParts(a, b []string) int {
	for i := range min(len(a), len(b)) {
		an, bn := isNumber(a[i]), isNumber(b[i])
		var c int
		switch {
		case an && bn:
			x, y := strings.TrimLeft(a[i], "0"), strings.TrimLeft(b[i], "0")
			c = cmp.Or(cmp.Compare(len(x), len(y)), strings.Compare(x, y))
		case an:
			c = -1
		case bn:
			c = 1
		default:
			c = strings.Compare(a[i], b[i])
		}
		if c != 0 {
			return c
		}
	}
	return cmp.Compare(len(a), len(b))
}

// isNumber reports whether s is digits alone.
func isNumber(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}
