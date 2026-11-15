// Package vmlog is a log a VM can drive: one that a VM's output, its
// servers' messages or its serial console reach, which must not fill the
// host's disk however much the VM writes. Limit holds the writes of
// devvm's own processes to a rate and a length; Capped, Trim and Keep
// hold a log's file to a size while it's written, copying its end to
// <log>.1 and emptying it, whether devvm's process appends to it or
// another program (Lima, QEMU, Apple's Virtualization framework) writes
// it at its own offset. The proxy keeps its own logs (proxy.rotating), as
// it imports only the standard library.
package vmlog

import (
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/devvmhq/devvm/internal/msg"
)

// What the VM can make a limited log take (Limit): Burst writes at once,
// then one per Every, each at most WriteMax bytes.
const (
	Burst    = 200
	Every    = 10 * time.Second
	WriteMax = 512
)

// Limit is a log the VM can make devvm's process write to (its ports, its
// ssh's errors, its sshd's stderr): Burst writes at once, then one per
// Every, each cut at WriteMax bytes, so it can't fill the host's disk,
// and without control characters but newlines and tabs (msg.Printable).
// The writes it drops are counted, and the next it takes says how many.
type Limit struct {
	w    io.Writer
	name string
	now  func() time.Time

	mu      sync.Mutex
	tokens  int
	at      time.Time
	dropped int
}

// NewLimit limits what goes to w, the log of devvm's process name ("ports", "links"), which the
// line that counts the writes dropped names, on the clock now.
func NewLimit(w io.Writer, name string, now func() time.Time) *Limit {
	return &Limit{w: w, name: name, now: now, tokens: Burst, at: now()}
}

// take takes one of the limit's writes, refilled since, and reports
// whether there was one; a write it can't take is counted as dropped.
// l.mu is held.
func (l *Limit) take() bool {
	if n := int(l.now().Sub(l.at) / Every); n > 0 {
		l.tokens, l.at = min(Burst, l.tokens+n), l.at.Add(time.Duration(n)*Every)
	}
	if l.tokens == 0 {
		l.dropped++
		return false
	}
	l.tokens--
	return true
}

// Allow takes one of the limit's writes for something said another way,
// such as a warning through the process's Reporter, which goes to the
// same log: false when there's none, and it's dropped, counted as the
// log's writes are.
func (l *Limit) Allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.take()
}

func (l *Limit) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.take() {
		return len(p), nil
	}
	var b []byte
	if l.dropped > 0 {
		b = fmt.Appendf(b, "devvm %s: %d writes not logged\n", l.name, l.dropped)
		l.dropped = 0
	}
	b = append(b, msg.Printable(string(p[:min(len(p), WriteMax)]))...)
	if len(p) > WriteMax {
		b = append(b, "…\n"...)
	}
	_, err := l.w.Write(b)
	return len(p), err
}
