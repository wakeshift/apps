package wakeshift

import (
	"context"
	"time"
)

// SetNameFetch sets how long tools/list waits for the app's name and how
// long after a failure it doesn't ask again, for the external tests; the
// returned func restores them.
func SetNameFetch(wait, retry time.Duration) (restore func()) {
	prev := nameFetch
	nameFetch.wait, nameFetch.retry = wait, retry
	return func() { nameFetch = prev }
}

// SetTimeLimits sets how long Identify and Authorize may run, for the
// external tests; the returned func restores them.
func SetTimeLimits(d time.Duration) (restore func()) {
	pi, pa := identifyTimeout, authorizeTimeout
	identifyTimeout, authorizeTimeout = d, d
	return func() { identifyTimeout, authorizeTimeout = pi, pa }
}

// SetClock sets c's clock, for the external tests.
func SetClock(c *Client, now func() time.Time) { c.now = now }

// CompactStrings is the pending compact tools/list strings, for the
// external tests.
var CompactStrings = &compactStrings

// EventTools is the built-in tools' wording, for the external tests.
var EventTools = eventTools

// call is callWith through the client's own HTTP client.
func (c *Client) call(ctx context.Context, method, path, bearer string, body, out any) error {
	return c.callWith(ctx, c.client(), method, path, bearer, body, out)
}

// SetWatchHooks lets the external tests run Watch fast: min is the
// shortest Every, sleep (if not nil) replaces its waits, and jitter is off.
// The returned func restores them.
func SetWatchHooks(min time.Duration, sleep func(ctx context.Context, d time.Duration) bool) (restore func()) {
	pm, ps, pj := minWatchEvery, watchSleep, watchJitter
	minWatchEvery, watchJitter = min, false
	if sleep != nil {
		watchSleep = sleep
	}
	return func() { minWatchEvery, watchSleep, watchJitter = pm, ps, pj }
}

// WatchDelay is watchDelay, for the external tests.
var WatchDelay = watchDelay
