package wakeshift

// A client's background work and its end (Close): the OnSubscribe
// goroutines the SDK starts and the Watches running on the client. Close
// stops them and returns once each has returned, so no goroutine of the
// SDK's outlives it; and the subscribe vouchers' contexts it remembers
// (vouchers.go) go with it.

import (
	"context"
	"errors"
	"sync"
)

// errClientClosed ends the work of a client that was closed.
var errClientClosed = errors.New("wakeshift: the client was closed")

// lifeState is a client's running background work and whether it's
// closing.
type lifeState struct {
	once sync.Once
	mu   sync.Mutex
	cond *sync.Cond
	// running counts the OnSubscribe goroutines and Watches in progress.
	running int
	// closed: Close started (or, gracefully, finished draining): new work
	// doesn't start.
	closed bool
	// drain is canceled when Close starts: a Watch stops at its next wait.
	drain       context.Context
	cancelDrain context.CancelFunc
	// hard is canceled when Close starts, or, gracefully (wakeshifttest),
	// once the work in flight has finished: a check and an OnSubscribe see
	// it as their ctx ending.
	hard       context.Context
	cancelHard context.CancelFunc
	// graceful: Close lets a check in flight, and its emit, finish
	// (wakeshifttest's Close).
	graceful bool
}

func (l *lifeState) init() {
	l.once.Do(func() {
		l.cond = sync.NewCond(&l.mu)
		l.drain, l.cancelDrain = context.WithCancel(context.Background())
		l.hard, l.cancelHard = context.WithCancel(context.Background())
	})
}

// add counts one piece of work in, unless the client is closed.
func (l *lifeState) add() bool {
	l.init()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return false
	}
	l.running++
	return true
}

// done counts one piece of work out.
func (l *lifeState) done() {
	l.mu.Lock()
	l.running--
	if l.running == 0 {
		l.cond.Broadcast()
	}
	l.mu.Unlock()
}

// Close stops the client's background work and waits for it: every Watch
// running on it returns (its check's ctx ends, and a wait between checks
// ends at once), and every OnSubscribe the SDK started returns (its ctx
// ends). After Close, Watch returns at once and OnSubscribe isn't called;
// emits and the rest still work. A stdio server's process ends with its
// main anyway, so Close matters where the process goes on: a test, or a
// server that stops serving and keeps running (defer wake.Close() after
// creating the client). It's safe to call more than once, and returns nil.
func (c *Client) Close() error {
	l := &c.life
	l.init()
	l.mu.Lock()
	graceful := l.graceful
	if !graceful {
		l.closed = true
	}
	l.mu.Unlock()
	l.cancelDrain()
	if !graceful {
		l.cancelHard()
	}
	l.mu.Lock()
	for l.running > 0 {
		l.cond.Wait()
	}
	l.closed = true
	l.mu.Unlock()
	l.cancelHard()
	c.vouchers.clear()
	return nil
}

// closing reports whether Close has started.
func (l *lifeState) closing() bool {
	l.init()
	return l.drain.Err() != nil
}

// bound is ctx, also ended when Close stops the client's work (hard).
func (l *lifeState) bound(ctx context.Context) (context.Context, context.CancelFunc) {
	l.init()
	ctx, cancel := context.WithCancelCause(ctx)
	stop := context.AfterFunc(l.hard, func() { cancel(errClientClosed) })
	return ctx, func() { stop(); cancel(nil) }
}

// drained is ctx, also ended when Close starts (drain): a Watch's waits.
func (l *lifeState) drained(ctx context.Context) (context.Context, context.CancelFunc) {
	l.init()
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(l.drain, cancel)
	return ctx, func() { stop(); cancel() }
}
