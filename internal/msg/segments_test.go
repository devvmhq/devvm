package msg

import (
	"slices"
	"strings"
	"testing"
)

// values is the segments' values, in order, and their joined text.
func values(segs []Segment) (vals []string, text string) {
	var b strings.Builder
	for _, s := range segs {
		if s.Value {
			vals = append(vals, s.Text)
		}
		b.WriteString(s.Text)
	}
	return vals, b.String()
}

// A message's segments join to its text, and its values are the ones
// given, wherever the template puts them: a VM's name, a path, one in a
// command, a count in either plural form.
func TestSegments(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		m    M
		want []string
	}{
		{StopFailed("devvm-x-1234"), []string{"devvm-x-1234"}},
		{NoVM("/Users/me/my project"), []string{"/Users/me/my project"}},
		{LogEnd("/tmp/run.log"), []string{"/tmp/run.log"}},
		{ImagesStaleNote(1), []string{"1"}},
		{ImagesStaleNote(3), []string{"3"}},
		{DeletedCache("lima", 2), []string{"2", "lima"}},
		{MsgUnknown(""), nil}, // an empty value is no segment
	} {
		segs, link := tc.m.Segments()
		vals, text := values(segs)
		if text != tc.m.Text() {
			t.Errorf("%s: segments join to %q, want its text %q", tc.m.ID(), text, tc.m.Text())
		}
		slices.Sort(vals)
		if !slices.Equal(vals, tc.want) {
			t.Errorf("%s: values %q, want %q", tc.m.ID(), vals, tc.want)
		}
		if link != tc.m.DocURL() {
			t.Errorf("%s: link %q, want %q", tc.m.ID(), link, tc.m.DocURL())
		}
	}
}

// A value can't start or end a marking of its own: its marker runes are
// dropped, and a VM's control characters are dropped (OneLine) as in its
// text.
func TestSegmentsOfHostileValues(t *testing.T) {
	t.Parallel()
	m := StopFailed("a not a value b")
	segs, _ := m.Segments()
	vals, text := values(segs)
	if !slices.Equal(vals, []string{"a not a value b"}) || strings.ContainsAny(text, "") {
		t.Errorf("markers in a value: %+v", segs)
	}
	const hostile = "vm\033[2J\033]0;title\007"
	segs, _ = StopFailed(hostile).Segments()
	if vals, text := values(segs); !slices.Equal(vals, []string{"vm[2J]0;title"}) || strings.ContainsRune(text, '\033') || text != StopFailed(hostile).Text() {
		t.Errorf("escape codes in a value: %+v", segs)
	}
}
