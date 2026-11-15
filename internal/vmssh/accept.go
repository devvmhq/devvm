package vmssh

import (
	"context"
	"errors"
	"net"
	"time"
)

// The waits of an accept loop between tries after errors that leave its listener open
// (acceptRetry): the first, doubled after each error in a row up to the last.
const (
	acceptRetryFirst = 5 * time.Millisecond
	acceptRetryMax   = time.Second
)

// acceptRetry is an accept loop's state between errors from its listener's Accept, its zero value
// before any. Each loop has its own.
type acceptRetry struct{ wait time.Duration }

// failed is what an accept loop does with err, from its listener's Accept: false once the listener
// is closed or ctx has ended, so the loop ends. Any other error, such as running out of files
// (EMFILE) in a burst, leaves the listener open and listening, so that ending the loop would leave
// its port unanswered: the loop tries again (true) after a wait that starts at acceptRetryFirst and
// doubles with each error in a row, up to acceptRetryMax, as what's accepted goes on. Only the
// first error of a run goes to log, when there is one, so a run of them can't fill it.
func (a *acceptRetry) failed(ctx context.Context, err error, log func(error)) bool {
	if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
		return false
	}
	if a.wait == 0 {
		if log != nil {
			log(err)
		}
		a.wait = acceptRetryFirst
	} else {
		a.wait = min(2*a.wait, acceptRetryMax)
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(a.wait):
		return true
	}
}

// accepted ends a run of errors: the loop accepted a connection.
func (a *acceptRetry) accepted() { a.wait = 0 }
