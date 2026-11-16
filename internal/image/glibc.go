package image

import (
	"regexp"
	"strconv"
	"strings"
)

// glibcPattern is a glibc version as devvm records one: major.minor, as
// getconf GNU_LIBC_VERSION and the GLIBC_ versions programs need have it.
var glibcPattern = regexp.MustCompile(`^[0-9]{1,3}\.[0-9]{1,3}$`)

// ValidGlibc reports whether s is a glibc version as devvm records one: a
// --from base's (2.36) and the newest a tool image needs.
func ValidGlibc(s string) bool { return glibcPattern.MatchString(s) }

// GlibcOlder reports whether glibc version a is older than b: 2.9 is
// older than 2.28. ok is false, and older with it, unless both are
// ValidGlibc, so a version that isn't one is never read as 0.
func GlibcOlder(a, b string) (older, ok bool) {
	am, an, aok := glibcParts(a)
	bm, bn, bok := glibcParts(b)
	if !aok || !bok {
		return false, false
	}
	return am < bm || am == bm && an < bn, true
}

// glibcParts is a ValidGlibc version's major and minor numbers; ok is
// false for one that isn't.
func glibcParts(v string) (major, minor int, ok bool) {
	a, b, _ := strings.Cut(v, ".")
	major, errMajor := strconv.Atoi(a)
	minor, errMinor := strconv.Atoi(b)
	return major, minor, errMajor == nil && errMinor == nil && ValidGlibc(v)
}
