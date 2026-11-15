package ui

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"
)

// InterruptedCode is the exit code of a run ended by Ctrl+C or a closed
// terminal (INT, TERM, HUP).
const InterruptedCode = 130

// exitError is what die panics with, for Main to recover.
type exitError struct{ code int }

func exit(code int) { panic(exitError{code}) }

type cleanup struct {
	id int
	f  func(ctx context.Context)
}

// Interrupted reports whether the run was interrupted (Ctrl+C, TERM, a
// closed terminal). A cleanup that only undoes unfinished work on an
// interrupt checks it.
func (u *UI) Interrupted() bool { return u.interrupted.Load() }

// StopsItsOwn says the run stops what it started itself once its
// context ends: a background process's ssh masters and relays, which
// would outlive it otherwise. A signal then waits for the run to return,
// up to returnGrace, as it does when there are cleanups to run.
func (u *UI) StopsItsOwn() { u.stopsItsOwn.Store(true) }

// AddCleanup adds f to the cleanup stack, which runs, last added first, when
// the run ends: normally, failed, or interrupted. f gets a context of
// its own, as the run's is canceled by then. Call remove once there's
// nothing left to clean up.
func (u *UI) AddCleanup(f func(ctx context.Context)) (remove func()) {
	c := &u.cleanups
	c.Lock()
	defer c.Unlock()
	c.next++
	id := c.next
	c.funcs = append(c.funcs, cleanup{id, f})
	return func() {
		c.Lock()
		defer c.Unlock()
		for i, cl := range c.funcs {
			if cl.id == id {
				c.funcs = append(c.funcs[:i], c.funcs[i+1:]...)
				return
			}
		}
	}
}

// runCleanups runs the cleanup stack, last added first. A cleanup that dies
// or panics doesn't stop the rest.
func (u *UI) runCleanups() {
	c := &u.cleanups
	for {
		c.Lock()
		n := len(c.funcs)
		if n == 0 {
			c.Unlock()
			return
		}
		cl := c.funcs[n-1]
		c.funcs = c.funcs[:n-1]
		c.Unlock()
		func() {
			defer func() { _ = recover() }()
			cl.f(context.Background())
		}()
	}
}

type result struct {
	code  int
	panic any // a panic that isn't die's, with its stack
}

// call runs fn, turning die's panic into its exit code.
func call(ctx context.Context, fn func(context.Context) int) (r result) {
	defer func() {
		if p := recover(); p != nil {
			if e, ok := p.(exitError); ok {
				r = result{code: e.code}
				return
			}
			r = result{panic: fmt.Sprintf("%v\n\n%s", p, debug.Stack())}
		}
	}()
	return result{code: fn(ctx)}
}

// returnGrace is how long an interrupted run waits for fn to return,
// once the commands it ran have ended, before the cleanups run. With its
// context canceled, fn fails at its next step and returns in far less;
// what it can't stop keeps the cleanups waiting no longer than this, so
// Ctrl+C still ends the run promptly. A question waiting for an answer
// isn't waited for at all.
const returnGrace = 2 * time.Second

// Main makes the UI on devvm's own streams and runs fn, the whole of
// devvm's run, with it. It returns the exit code: fn's, or 1 when a
// question has no answer. Then it runs the cleanup stack.
//
// INT, TERM and HUP cancel fn's context, which stops the command running,
// and return InterruptedCode. With cleanups to run, or a run that stops
// what it started itself (StopsItsOwn), they wait first for fn to
// return, or returnGrace after the commands it ran ended, whichever is
// first, unless fn waits for an answer; then the cleanups run. A second
// signal during the cleanups is ignored, so they aren't stopped halfway. fn's own code fails at its next step, as its context is
// canceled, and prints nothing.
func Main(fn func(ctx context.Context, u *UI) int) int {
	u := onTerminal()
	return u.main(func(ctx context.Context) int { return fn(ctx, u) })
}

// main is Main on u.
func (u *UI) main(fn func(ctx context.Context) int) int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)

	done := make(chan result, 1)
	go func() { done <- call(ctx, fn) }()
	var r result
	select {
	case r = <-done:
	case <-sigs:
		r = interrupt(u, cancel, done)
	}
	u.stopStatus()
	u.runCleanups()
	flushLog(u, true)
	if r.panic != nil {
		panic(r.panic)
	}
	return r.code
}

// interrupt ends a run a signal interrupted: it cancels fn's context,
// waits for the commands running to end, then, when there are cleanups
// to run, or the run stops what it started itself (StopsItsOwn), for fn
// to return (done), up to returnGrace. Otherwise nothing could undo fn's
// work, so the run ends at once: the proxy stops as soon as it's told
// to, and a forwarder or a link session once it has stopped its ssh.
// Nor does it wait while fn waits for an answer: a question doesn't
// return at Ctrl+C, and its answer would be refused (readLine).
// The run's code is InterruptedCode, whatever fn returned, but a panic of
// fn's own still panics.
func interrupt(u *UI, cancel context.CancelFunc, done <-chan result) result {
	u.interrupted.Store(true)
	cancel()
	u.stopStatus()
	u.commands.Lock()   // the commands running have ended
	u.commands.Unlock() //nolint:staticcheck // the lock is only waited for: each logged command holds it to read
	r := result{code: InterruptedCode}
	if (hasCleanups(u) || u.stopsItsOwn.Load()) && u.asking.Load() == 0 {
		t := time.NewTimer(returnGrace)
		defer t.Stop()
		select {
		case late := <-done:
			r.panic = late.panic
		case <-t.C:
		}
	}
	u.write(u.stderr, "\n")
	return r
}

// hasCleanups reports whether u's cleanup stack has any left to run.
func hasCleanups(u *UI) bool {
	c := &u.cleanups
	c.Lock()
	defer c.Unlock()
	return len(c.funcs) > 0
}
