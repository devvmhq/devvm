package daemon

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"testing/synctest"
	"time"
)

// gone returns the pid of a process that has ended.
func gone(t *testing.T) int {
	t.Helper()
	h, err := Spawn(Cmd{Path: "true"})
	if err != nil {
		t.Fatal(err)
	}
	<-h.Done()
	return h.PID()
}

func write(t *testing.T, path, s string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A lifeline holds while it holds what it held and its process (the
// first line) runs: not once a new start rewrites it, it's removed, or
// its process ends.
func TestLifelineHolds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.pid")
	if got, err := ReadLifeline(path); got != "" || err != nil {
		t.Errorf("none: %q, %v", got, err)
	}
	me := strconv.Itoa(os.Getpid())
	write(t, path, me+"\n123\n")
	l, ok := FollowLifeline(path, 0, time.Millisecond)
	if !ok || l.Life != me+"\n123" || !l.Holds() {
		t.Fatalf("FollowLifeline = %+v, %v", l, ok)
	}
	write(t, path, me+"\n456\n")
	if l.Holds() {
		t.Error("holds after a new start")
	}
	write(t, path, me+"\n123\n")
	if !l.Holds() {
		t.Error("doesn't hold what it held")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if l.Holds() {
		t.Error("holds once removed")
	}
	dead := strconv.Itoa(gone(t))
	write(t, path, dead+"\n")
	if (Lifeline{Path: path, Life: dead}).Holds() {
		t.Error("holds with its process gone")
	}
	for _, life := range []string{"", "x", "0", "-1", dead} {
		if LifeRuns(life) {
			t.Errorf("LifeRuns(%q)", life)
		}
	}
}

// Following a lifeline waits, a bounded time, for it to name a process
// that runs, as a proxy started before its VM does: past what the file
// held when it first looked (an earlier start's), but not past a new
// start's whose process is already gone (a VM that stopped between two
// looks). With no wait it looks once. Each look is a tick apart on the
// bubble's clock, so each step ends at the look it's due: the test's own
// process stands for one that runs, and ended ones' pids, from before the
// bubble, for those gone.
func TestFollowLifeline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ha.pid")
	earlier, stopped, again := gone(t), gone(t), gone(t)
	me := strconv.Itoa(os.Getpid())
	const tick = 10 * time.Millisecond
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		if _, ok := FollowLifeline(path, 0, tick); ok || time.Since(start) != 0 {
			t.Errorf("none, no wait: followed %v, after %v", ok, time.Since(start))
		}
		// Its last look is the last a tick before the wait ends.
		if _, ok := FollowLifeline(path, 100*time.Millisecond, tick); ok || time.Since(start) != 90*time.Millisecond {
			t.Errorf("none: followed %v, after %v", ok, time.Since(start))
		}
		// An earlier start's, then this one's.
		write(t, path, strconv.Itoa(earlier)+"\n")
		got := make(chan Lifeline, 1)
		go func() {
			l, _ := FollowLifeline(path, 10*time.Second, tick)
			got <- l
		}()
		time.Sleep(time.Second)
		synctest.Wait()
		select {
		case l := <-got:
			t.Fatalf("followed an earlier start's: %+v", l)
		default:
		}
		write(t, path, me+"\n")
		time.Sleep(tick)
		synctest.Wait()
		select {
		case l := <-got:
			if l.Life != me || !l.Holds() {
				t.Errorf("followed %+v", l)
			}
			// It holds until a new start rewrites it: Wait ends at its next
			// look.
			done := make(chan struct{})
			go func() { l.Wait(tick); close(done) }()
			time.Sleep(time.Second)
			synctest.Wait()
			if isClosed(done) {
				t.Fatal("Wait: ended while it held")
			}
			write(t, path, strconv.Itoa(stopped)+"\n")
			time.Sleep(tick)
			synctest.Wait()
			if !isClosed(done) {
				t.Error("Wait: still waiting a tick after it no longer held")
			}
		default:
			t.Fatal("not followed a tick after it named a process that runs")
		}
		// A new start's, already gone, ends the wait at its next look.
		ended := make(chan bool, 1)
		go func() {
			_, ok := FollowLifeline(path, 10*time.Second, tick)
			ended <- ok
		}()
		time.Sleep(time.Second)
		write(t, path, strconv.Itoa(again)+"\n")
		time.Sleep(tick)
		synctest.Wait()
		select {
		case ok := <-ended:
			if ok {
				t.Error("followed a lifeline whose process is gone")
			}
		default:
			t.Error("still waiting a tick past a new start's lifeline whose process is gone")
		}
	})
}

// isClosed reports whether ch is closed.
func isClosed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
