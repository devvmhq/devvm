package msg

import (
	"slices"
	"testing"
)

// A lock's wait shows its label on its status line, and ends it; with no
// status, as in a process with no UI, it shows nothing, but is never nil,
// which a test build's locks refuse.
func TestWaiting(t *testing.T) {
	var shown []string
	ended := 0
	status := func(m M) func() {
		shown = append(shown, m.ID())
		return func() { ended++ }
	}
	label := M{id: "share_waiting"}
	Waiting(status, label)()()
	if !slices.Equal(shown, []string{"share_waiting"}) || ended != 1 {
		t.Errorf("shown %q, ended %d", shown, ended)
	}
	w := Waiting(nil, label)
	if w == nil {
		t.Fatal("no status: a nil wait")
	}
	w()()
}
