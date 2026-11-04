package appkit

import "time"

// SetNameFetch sets how long tools/list waits for the app's name and how
// long after a failure it doesn't ask again, for the external tests; the
// returned func restores them.
func SetNameFetch(wait, retry time.Duration) (restore func()) {
	prev := nameFetch
	nameFetch.wait, nameFetch.retry = wait, retry
	return func() { nameFetch = prev }
}

// SetUndeclaredIsError sets whether a Subscribe the tool doesn't declare is
// refused (the next release's behavior), for the external tests; the
// returned func restores it.
func SetUndeclaredIsError(v bool) (restore func()) {
	prev := undeclaredIsError
	undeclaredIsError = v
	return func() { undeclaredIsError = prev }
}
