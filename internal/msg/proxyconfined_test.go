package msg

import (
	"strings"
	"testing"
)

// devvm doctor's message for a proxy whose start it can't read, which netpolicy's tests check by
// name: it renders whole, its values in place, and links to its page.
func TestProxyConfinementUnknown(t *testing.T) {
	m := ProxyConfinementUnknown("devvm-a-0123456789abcd", "no such process")
	if text := m.Text(); strings.Contains(text, "{{") || !strings.Contains(text, "no such process") || m.DocURL() == "" {
		t.Errorf("%q, link %q", text, m.DocURL())
	}
}
