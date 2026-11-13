package backend

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// ValidPort reports whether n is a TCP or UDP port: 1 to 65535.
func ValidPort(n int) bool { return n >= 1 && n <= 65535 }

// ParsePortRanges reads ports and ranges as devvm lists them
// (5173,8000-8010), spaces ignored: each port is 1 to 5 digits, 1 to
// 65535, and a range's first port is at most its last. Repeated entries
// are dropped. Anything else, an empty list included, is an error.
func ParsePortRanges(v string) ([]PortRange, error) {
	var out []PortRange
	for e := range strings.SplitSeq(strings.ReplaceAll(v, " ", ""), ",") {
		a, b, ok := strings.Cut(e, "-")
		if !ok {
			b = a
		}
		from, ok1 := parsePort(a)
		to, ok2 := parsePort(b)
		if !ok1 || !ok2 || from > to {
			return nil, fmt.Errorf("%q: not a port or a range", e)
		}
		if r := (PortRange{From: from, To: to}); !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	return out, nil
}

// parsePort reads one port: 1 to 5 digits, 1 to 65535.
func parsePort(s string) (int, bool) {
	if s == "" || len(s) > 5 || strings.Trim(s, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	return n, err == nil && ValidPort(n)
}

// CountPorts returns how many ports rs has.
func CountPorts(rs []PortRange) int {
	n := 0
	for _, r := range rs {
		n += r.To - r.From + 1
	}
	return n
}

// FormatPortRanges is ranges as devvm lists them (ParsePortRanges): a
// range of one port as that port.
func FormatPortRanges(rs []PortRange) string {
	es := make([]string, 0, len(rs))
	for _, r := range rs {
		e := strconv.Itoa(r.From)
		if r.To != r.From {
			e += "-" + strconv.Itoa(r.To)
		}
		es = append(es, e)
	}
	return strings.Join(es, ",")
}

// Has reports whether p has port: every port when All, else those in its
// ranges.
func (p Ports) Has(port int) bool {
	if p.All {
		return ValidPort(port)
	}
	return slices.ContainsFunc(p.Ranges, func(r PortRange) bool { return r.From <= port && port <= r.To })
}
