package daemon

import (
	"context"
	"errors"
	"net"
	"os"
	"slices"
	"syscall"
	"testing"
	"time"
)

// An accept loop ends with its listener's close or its context's end,
// and tries again after any other error, waiting longer after each in a
// row, up to a second, and logging only the first of a run.
func TestAcceptRetry(t *testing.T) {
	emfile := &net.OpError{Op: "accept", Err: os.NewSyscallError("accept", syscall.EMFILE)}
	var logged []error
	log := func(err error) { logged = append(logged, err) }
	var a AcceptRetry
	if a.Failed(t.Context(), net.ErrClosed, log) {
		t.Error("a closed listener: tried again")
	}
	var waits []time.Duration
	for range 3 {
		if !a.Failed(t.Context(), emfile, log) {
			t.Fatal("EMFILE: the loop ended")
		}
		waits = append(waits, a.wait)
	}
	if !slices.Equal(waits, []time.Duration{acceptRetryFirst, 2 * acceptRetryFirst, 4 * acceptRetryFirst}) {
		t.Errorf("waits %v, want doubling from %v", waits, acceptRetryFirst)
	}
	if len(logged) != 1 || !errors.Is(logged[0], syscall.EMFILE) {
		t.Errorf("logged %v, want the first EMFILE alone", logged)
	}
	a.wait = acceptRetryMax * 3 / 4
	_ = a.Failed(t.Context(), emfile, log)
	if a.wait != acceptRetryMax {
		t.Errorf("a wait of %v, past its cap of %v", a.wait, acceptRetryMax)
	}
	a.Accepted()
	_ = a.Failed(t.Context(), emfile, log)
	if len(logged) != 2 || a.wait != acceptRetryFirst {
		t.Errorf("a new run: logged %d, waits %v; want it logged and the first wait", len(logged), a.wait)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if a.Failed(ctx, emfile, nil) {
		t.Error("an ended context: tried again")
	}
}
