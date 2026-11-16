package image

import "testing"

func TestGlibc(t *testing.T) {
	for s, ok := range map[string]bool{
		"2.36": true, "2.9": true, "10.100": true,
		"": false, "2": false, "2.36.1": false, "glibc 2.36": false, "2.36\n": false, "2.x": false, "1234.1": false,
	} {
		if ValidGlibc(s) != ok {
			t.Errorf("ValidGlibc(%q) = %v", s, !ok)
		}
	}
	for _, c := range []struct {
		a, b  string
		older bool
	}{
		{"2.9", "2.28", true},
		{"2.28", "2.9", false},
		{"2.31", "2.34", true},
		{"2.36", "2.36", false},
		{"2.43", "2.17", false},
		{"2.99", "3.0", true},
		{"3.0", "2.99", false},
	} {
		if got, ok := GlibcOlder(c.a, c.b); got != c.older || !ok {
			t.Errorf("GlibcOlder(%s, %s) = %v, %v", c.a, c.b, got, ok)
		}
	}
	// A version that isn't one is never read as 0, older than any.
	for _, c := range [][2]string{{"x", "2.36"}, {"2.36", ""}, {"", "2.9"}, {"2.x", "3.0"}} {
		if older, ok := GlibcOlder(c[0], c[1]); older || ok {
			t.Errorf("GlibcOlder(%q, %q) = %v, %v", c[0], c[1], older, ok)
		}
	}
}
