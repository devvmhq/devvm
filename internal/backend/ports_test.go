package backend

import (
	"slices"
	"testing"
)

func TestValidPort(t *testing.T) {
	for n, want := range map[int]bool{-1: false, 0: false, 1: true, 80: true, 65535: true, 65536: false} {
		if got := ValidPort(n); got != want {
			t.Errorf("ValidPort(%d) = %v", n, got)
		}
	}
}

func TestParsePortRanges(t *testing.T) {
	for _, c := range []struct {
		in   string
		want []PortRange
	}{
		{"80", []PortRange{{80, 80}}},
		{"5173,8000-8010", []PortRange{{5173, 5173}, {8000, 8010}}},
		{" 5173 , 8000 - 8010 ", []PortRange{{5173, 5173}, {8000, 8010}}},
		{"0080,3000-3000", []PortRange{{80, 80}, {3000, 3000}}},
		{"80,80,1-65535", []PortRange{{80, 80}, {1, 65535}}},
	} {
		if got, err := ParsePortRanges(c.in); err != nil || !slices.Equal(got, c.want) {
			t.Errorf("ParsePortRanges(%q) = %v, %v; want %v", c.in, got, err, c.want)
		}
	}
	for _, bad := range []string{
		"", " ", ",", "0", "65536", "123456", "000080", "8010-8000", "5173,,80", "80,", "abc", "80-", "-80",
		"1-2-3", "+80", "80-x", "all", "none", "0x50",
	} {
		if got, err := ParsePortRanges(bad); err == nil {
			t.Errorf("ParsePortRanges(%q) = %v: taken", bad, got)
		}
	}
}

func TestFormatPortRanges(t *testing.T) {
	for _, c := range []struct {
		in    []PortRange
		want  string
		count int
	}{
		{nil, "", 0},
		{[]PortRange{{80, 80}}, "80", 1},
		{[]PortRange{{5173, 5173}, {8000, 8010}}, "5173,8000-8010", 12},
	} {
		if got := FormatPortRanges(c.in); got != c.want {
			t.Errorf("FormatPortRanges(%v) = %q, want %q", c.in, got, c.want)
		}
		if n := CountPorts(c.in); n != c.count {
			t.Errorf("CountPorts(%v) = %d, want %d", c.in, n, c.count)
		}
		if c.in == nil {
			continue
		}
		if back, err := ParsePortRanges(c.want); err != nil || !slices.Equal(back, c.in) {
			t.Errorf("ParsePortRanges(FormatPortRanges(%v)) = %v, %v", c.in, back, err)
		}
	}
}

func TestPortsHas(t *testing.T) {
	some := Ports{Ranges: []PortRange{{80, 80}, {8000, 8010}}}
	for _, c := range []struct {
		p    Ports
		port int
		want bool
	}{
		{Ports{All: true}, 1, true},
		{Ports{All: true}, 65535, true},
		{Ports{All: true}, 0, false},
		{Ports{}, 80, false},
		{some, 80, true},
		{some, 81, false},
		{some, 8000, true},
		{some, 8010, true},
		{some, 8011, false},
		{some, 7999, false},
	} {
		if got := c.p.Has(c.port); got != c.want {
			t.Errorf("%+v.Has(%d) = %v", c.p, c.port, got)
		}
	}
}
