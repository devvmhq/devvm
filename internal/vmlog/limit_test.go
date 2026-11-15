package vmlog

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// What the VM makes a process log is limited: a burst, then one write
// per Every, those dropped counted; control characters dropped, C1
// included, long writes cut.
func TestLimit(t *testing.T) {
	t.Parallel()
	var b bytes.Buffer
	now := time.Unix(1000, 0)
	l := NewLimit(&b, "ports", func() time.Time { return now })
	for range Burst + 50 {
		_, _ = l.Write([]byte("x\n"))
	}
	if got := strings.Count(b.String(), "x\n"); got != Burst {
		t.Errorf("%d writes logged, want %d", got, Burst)
	}
	b.Reset()
	now = now.Add(2 * Every)
	_, _ = l.Write([]byte("a\x1b[2Jb\302\233c\x9bd\r\n"))
	_, _ = l.Write([]byte(strings.Repeat("y", WriteMax+10)))
	_, _ = l.Write([]byte("dropped\n"))
	want := "devvm ports: 50 writes not logged\na[2Jbcd\n" + strings.Repeat("y", WriteMax) + "…\n"
	if b.String() != want {
		t.Errorf("log:\n%q\nwant\n%q", b.String(), want)
	}
}

// Allow takes its turn from the same writes: a warning said another way
// counts against the log, and one past the limit is dropped and counted.
func TestLimitAllow(t *testing.T) {
	t.Parallel()
	var b bytes.Buffer
	now := time.Unix(1000, 0)
	l := NewLimit(&b, "links", func() time.Time { return now })
	allowed := 0
	for range Burst + 5 {
		if l.Allow() {
			allowed++
		}
	}
	if allowed != Burst {
		t.Errorf("%d allowed, want %d", allowed, Burst)
	}
	if _, _ = l.Write([]byte("x\n")); b.Len() != 0 {
		t.Errorf("a write past the limit: %q", b.String())
	}
	now = now.Add(Every)
	if !l.Allow() || l.Allow() {
		t.Error("one more after Every, and only one")
	}
	now = now.Add(Every)
	_, _ = l.Write([]byte("y\n"))
	if want := "devvm links: 7 writes not logged\ny\n"; b.String() != want {
		t.Errorf("log %q, want %q", b.String(), want)
	}
}
