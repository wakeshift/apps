package wakeshift

// Watch: emit once when work finishes, for vendors with no usable webhook
// (sdk/typescript/test/watch.json). It checks the vendor's own status until
// the check says done, then emits, in this process: a restart or a frozen
// serverless instance loses it.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"
)

// WatchOption shapes a Watch: Every, Timeout, MaxErrors, FinalIf, WithID
// and To (a personal event's recipient).
type WatchOption interface{ applyWatch(*watchOptions) }

type watchOptions struct {
	every, timeout time.Duration
	timeoutSet     bool
	maxErrors      int
	maxErrorsSet   bool
	finalIf        func(error) bool
	id             string
	emit           []EmitOption
}

type watchFunc func(*watchOptions)

func (f watchFunc) applyWatch(o *watchOptions) { f(o) }

// Every is how long a Watch waits between checks (default 30 seconds, at
// least 1 second). After n check errors in a row it waits longer:
// Every·2ⁿ, up to 5 minutes (or Every, if that's longer).
func Every(d time.Duration) WatchOption { return watchFunc(func(o *watchOptions) { o.every = d }) }

// Timeout is how long a Watch runs at most, checks and emit retries
// included (default 1 hour). It must be at least Every.
func Timeout(d time.Duration) WatchOption {
	return watchFunc(func(o *watchOptions) { o.timeout, o.timeoutSet = d, true })
}

// MaxErrors is how many check errors in a row stop a Watch (default 5).
func MaxErrors(n int) WatchOption {
	return watchFunc(func(o *watchOptions) { o.maxErrors, o.maxErrorsSet = n, true })
}

// FinalIf marks a check error final: one it reports true for stops the
// Watch at once (a job that no longer exists, a revoked credential).
func FinalIf(f func(error) bool) WatchOption {
	return watchFunc(func(o *watchOptions) { o.finalIf = f })
}

func (id IDOption) applyWatch(o *watchOptions) { o.id = id.id }

func (r RecipientOption) applyWatch(o *watchOptions) { o.emit = append(o.emit, r) }

// Watch defaults and limits (watch.json defaults).
const (
	watchEvery          = 30 * time.Second
	watchTimeout        = time.Hour
	watchMaxErrors      = 5
	watchMaxBackoff     = 5 * time.Minute
	maxConcurrentChecks = 10
)

// Test hooks: the shortest Every, whether delays get jitter, and how a
// Watch waits (it reports false if ctx ended first).
var (
	minWatchEvery = time.Second
	watchJitter   = true
	watchSleep    = func(ctx context.Context, d time.Duration) bool {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-t.C:
			return true
		case <-ctx.Done():
			return false
		}
	}
)

// watchDelay is how long a Watch waits before its next check: every after
// a check that said not yet; after n errors in a row (n ≥ 1),
// min(every·2ⁿ, max(every, 5 minutes)).
func watchDelay(every time.Duration, errorsInARow int) time.Duration {
	if errorsInARow <= 0 {
		return every
	}
	limit := max(every, watchMaxBackoff)
	d := every
	for range errorsInARow {
		d *= 2
		if d >= limit {
			return limit
		}
	}
	return d
}

// jittered is d give or take 10%, in production.
func jittered(d time.Duration) time.Duration {
	if !watchJitter {
		return d
	}
	return time.Duration(float64(d) * (0.9 + 0.2*rand.Float64()))
}

// watchState is a client's running watches, by event and arguments, and
// the slots its checks share.
type watchState struct {
	once    sync.Once
	slots   chan struct{}
	mu      sync.Mutex
	running map[string]bool
}

func (w *watchState) init() {
	w.once.Do(func() {
		w.slots = make(chan struct{}, maxConcurrentChecks)
		w.running = map[string]bool{}
	})
}

// errRolledBack ends a watch whose call failed: the subscription it was
// for was never made.
var errRolledBack = errors.New("wakeshift: the tool call that started this watch failed")

// Watch checks the vendor's own status of some work until check reports
// done, then emits the event once with check's payload: for work that
// finishes with no webhook to tell you. It blocks; run it in a goroutine
// with a context that outlives the request:
//
//	go crawlFinished.Watch(context.WithoutCancel(ctx), jobArgs{Job: job},
//		func(ctx context.Context) (crawl, bool, error) {
//			s, err := api.Status(ctx, job)
//			if err != nil {
//				return crawl{}, false, err
//			}
//			return crawl{Status: s.State, Pages: s.Pages}, s.Done, nil
//		}, wakeshift.Every(10*time.Second), wakeshift.Timeout(time.Hour))
//
// The first check runs at once, then every Every (default 30 seconds);
// check errors back off, and MaxErrors in a row (default 5) or one FinalIf
// marks stop it. At most 10 checks of a client run at once; the rest wait
// their turn. The emit carries WithID's ID, or one the Watch makes, on
// every attempt, so a webhook emitting with the same ID wins a race
// without a second wake; an emit error a retry could fix (Wakeshift
// unreachable, 429, 5xx) is retried until Timeout (default 1 hour).
//
// It returns nil once it emitted (or another emit with the same ID got
// there first), and at once, without checking, on a disabled or closed
// client (Close stops a running Watch and waits for it to return), when
// a Watch of the same event and arguments is already running in this
// process, or when ctx is a tool call's whose Subscribe to these arguments
// came back unavailable or disabled. It returns ctx's error when ctx ends
// (or the call that started it fails), an error matching
// context.DeadlineExceeded at Timeout, and otherwise the check's or the
// emit's error, wrapped. A mistake in its options is a *UsageError.
//
// It runs in this process only: a restart, a deploy or a frozen
// serverless instance ends it. Where that matters, keep the job in your
// own store and check it from a cron or queue.
func (e *Event[A, P]) Watch(ctx context.Context, args A, check func(ctx context.Context) (P, bool, error), opts ...WatchOption) error {
	o := watchOptions{every: watchEvery, timeout: watchTimeout, maxErrors: watchMaxErrors}
	for _, f := range opts {
		if f != nil {
			f.applyWatch(&o)
		}
	}
	switch {
	case o.every < minWatchEvery:
		return usage(CodeBadArguments, "%s: Watch's Every (every) is at least 1 second (got %s)", e.t.name, o.every)
	case o.timeoutSet && o.timeout <= 0:
		return usage(CodeBadArguments, "%s: Watch's Timeout (timeout) is more than 0 (got %s)", e.t.name, o.timeout)
	case o.timeout < o.every:
		return usage(CodeBadArguments, "%s: Watch's Timeout (timeout) is at least its Every (%s < %s)", e.t.name, o.timeout, o.every)
	case o.maxErrorsSet && o.maxErrors < 1:
		return usage(CodeBadArguments, "%s: Watch's MaxErrors (max_errors) is at least 1 (got %d)", e.t.name, o.maxErrors)
	case check == nil:
		return usage(CodeConfig, "%s: Watch needs a check function", e.t.name)
	}
	var eo emitOptions
	for _, f := range o.emit {
		f.applyEmit(&eo)
	}
	if err := e.checkRecipients(trimRecipients(eo)); err != nil {
		return err
	}
	argsJSON, err := e.encodeArgs(args)
	if err != nil {
		return err
	}
	if !e.app.Enabled() {
		return nil
	}
	key := e.t.name + "\x00" + compact(e.t.knownJSON(argsJSON))
	if ctx == nil {
		ctx = context.Background()
	}
	// Close stops the watch and waits for it; after Close it doesn't start.
	life := &e.app.life
	if !life.add() {
		return nil
	}
	defer life.done()
	stopCtx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	defer context.AfterFunc(life.hard, func() { stop(errClientClosed) })()
	if s := scopeFrom(ctx); s != nil {
		if !s.watchFor(key, stop) {
			return nil
		}
		defer s.unwatch(key)
	}
	w := &e.app.watches
	w.init()
	w.mu.Lock()
	if w.running[key] {
		w.mu.Unlock()
		return nil
	}
	w.running[key] = true
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		delete(w.running, key)
		w.mu.Unlock()
	}()
	if o.id == "" {
		o.id = internalEmitID()
	}
	tctx, cancel := context.WithTimeout(stopCtx, o.timeout)
	defer cancel()
	// wctx is tctx for the waits: Close's start ends them at once, while a
	// check in flight (and its emit) may finish (wakeshifttest's Close).
	wctx, wcancel := life.drained(tctx)
	defer wcancel()
	ended := func() error {
		if tctx.Err() == nil && life.closing() {
			return fmt.Errorf("%w: %w", context.Canceled, errClientClosed)
		}
		if err := context.Cause(stopCtx); err != nil {
			if errors.Is(err, errRolledBack) {
				return fmt.Errorf("%w: %w", context.Canceled, err)
			}
			return stopCtx.Err()
		}
		e.app.logOnce(ctx, "watch_timeout", e.t.name, fmt.Sprintf("a watch of %s reached its timeout (%s) without the work finishing", e.t.name, o.timeout),
			slog.String("event", e.t.name))
		return fmt.Errorf("wakeshift: watch of %s timed out after %s: %w", e.t.name, o.timeout, context.DeadlineExceeded)
	}
	failed := func(reason string, err error) error {
		if reason == "check" {
			// FinalIf marked the error final: the watch stopped as asked, nothing to fix.
			e.app.logLimited(ctx, "watch_stopped", fmt.Sprintf("a watch of %s stopped: its check's error was marked final (%s)", e.t.name, errorType(err)),
				slog.String("event", e.t.name), slog.String("error_type", errorType(err)))
			return fmt.Errorf("wakeshift: watch of %s: %w", e.t.name, err)
		}
		e.app.logLimited(ctx, "watch_failed", fmt.Sprintf("a watch of %s gave up (%s, %s)", e.t.name, reason, errorType(err)),
			slog.String("event", e.t.name), slog.String("reason", reason), slog.String("error_type", errorType(err)))
		return fmt.Errorf("wakeshift: watch of %s: %w", e.t.name, err)
	}
	var got func() P
	errs := 0
	for {
		select {
		case w.slots <- struct{}{}:
		case <-wctx.Done():
			return ended()
		}
		payload, done, err := runCheck(tctx, check)
		<-w.slots
		if tctx.Err() != nil {
			return ended()
		}
		if err != nil {
			if o.finalIf != nil && o.finalIf(err) {
				return failed("check", err)
			}
			if errs++; errs >= o.maxErrors {
				return failed("errors", err)
			}
		} else if done {
			got = payload
			break
		} else {
			errs = 0
		}
		if !watchSleep(wctx, jittered(watchDelay(o.every, errs))) {
			return ended()
		}
	}
	emitOpts := append(o.emit, WithID(o.id))
	for tries := 0; ; tries++ {
		_, err := e.Emit(tctx, args, got(), emitOpts...)
		switch {
		case err == nil, IsCode(err, CodeIDReused):
			return nil
		case tctx.Err() != nil:
			return ended()
		case !retryable(err):
			return failed("emit", err)
		}
		if !watchSleep(wctx, jittered(watchDelay(o.every, tries+1))) {
			return ended()
		}
	}
}

// runCheck runs check once, a panic counted as its error; payload returns
// what it gave.
func runCheck[P any](ctx context.Context, check func(context.Context) (P, bool, error)) (payload func() P, done bool, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("the check panicked: %v", p)
		}
	}()
	var v P
	v, done, err = check(ctx)
	return func() P { return v }, done, err
}

// retryable reports whether sending again could fix an error from
// Wakeshift: no answer, 429 or a 5xx.
func retryable(err error) bool {
	var e *APIError
	if !errors.As(err, &e) {
		return false
	}
	return e.Status == 0 || e.Status == http.StatusTooManyRequests || e.Status >= 500 || e.Code == CodeRateLimited
}

// onSubscribed runs t's OnSubscribe for a new subscription, in its own
// goroutine with ctx's values and a context that outlives the request,
// ended by Close (which waits for it). done is closed once it returned
// (nil if it doesn't run).
func (c *Client) onSubscribed(ctx context.Context, t *eventType, args []byte, info subscribedInfo) (done <-chan struct{}) {
	if t == nil || t.onSubscribe == nil || !c.Enabled() || !c.life.add() {
		return nil
	}
	ctx, cancel := c.life.bound(context.WithoutCancel(ctx))
	ch := make(chan struct{})
	go func() {
		var err error
		defer close(ch)
		defer c.life.done()
		defer cancel()
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("OnSubscribe panicked: %v", p)
			}
			if err != nil {
				c.logLimited(ctx, "on_subscribe_failed", fmt.Sprintf("%s's OnSubscribe failed (%s)", t.name, errorType(err)),
					slog.String("event", t.name), slog.String("error_type", errorType(err)))
			}
		}()
		err = t.onSubscribe(ctx, args, info)
	}()
	return ch
}
