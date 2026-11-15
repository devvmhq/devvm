package proxy

// What the proxy holds for a VM at once, past what each connection holds: its connections (conns,
// maxConns) and its requests upstream (requests, maxRequests), each logged at most once a minute
// when it's reached (everyMinute).

import (
	"log"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"
)

// maxConns is how many connections with the VM's password a VM's proxy holds at once, of every
// kind: plain HTTP's and idle kept-alive ones, and a request's that outlives its client, as well
// as CONNECTs. Twice maxConnects, so a tunnel past its limit gets its 503 first, and plain HTTP gets
// as many again. A request on a connection past it gets 503, and the connection closes.
const maxConns = 2 * maxConnects

// maxWaiting is how many connections a VM's proxy holds at once that have sent no request with the
// VM's password yet: a new one past it closes the one that has waited longest (conns), so another
// VM or program on this machine that opens connections without end can't take the VM's room.
const maxWaiting = 128

// maxRequests is how many requests a VM's proxy has upstream at once, plain HTTP's and those in
// intercepted tunnels alike: each is a connection of its own to the host, so the tunnels' HTTP/2
// streams (streamsMax each) can't add up to more connections than the proxy has descriptors.
const maxRequests = maxConns

// everyMinute lets one caller through at most once a minute: a line that each refusal past a limit
// would log, so a VM that keeps reaching it can't fill the log.
type everyMinute struct{ last atomic.Int64 }

// allow reports whether a line may be logged at now, and if so, counts it.
func (e *everyMinute) allow(now time.Time) bool {
	for {
		last := e.last.Load()
		if last != 0 && now.UnixNano()-last < int64(time.Minute) {
			return false
		}
		if e.last.CompareAndSwap(last, now.UnixNano()) {
			return true
		}
	}
}

// requests are the slots of a VM's requests upstream (maxRequests).
type requests struct {
	slots chan struct{}
	log   *log.Logger
	said  everyMinute
}

func newRequests(n int, l *log.Logger) *requests {
	return &requests{slots: make(chan struct{}, n), log: l}
}

// take holds a slot for a request upstream until release, or answers w with 503 when every one is
// held, which a client may try again, and says so in the proxy's log at most once a minute.
func (q *requests) take(w http.ResponseWriter) (release func(), ok bool) {
	select {
	case q.slots <- struct{}{}:
		return func() { <-q.slots }, true
	default:
	}
	n := strconv.Itoa(cap(q.slots))
	if q.said.allow(time.Now()) {
		q.log.Printf("%s requests upstream at once, the VM's limit: refusing more until one ends", n)
	}
	http.Error(w, "devvm proxy: "+n+" requests upstream at once already; try again", http.StatusServiceUnavailable)
	return nil, false
}
