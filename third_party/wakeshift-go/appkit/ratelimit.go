package appkit

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// RateLimiter allows max requests per window from each client address, in
// memory (per instance): an IPv6 address
// counts as its /64, X-Forwarded-For is read only as the trusted proxies
// wrote it, and the addresses tracked at once are capped (past the cap, a
// new address is refused until old ones expire). For an app's own
// unauthenticated endpoints, such as OAuth client registration.
type RateLimiter struct {
	// ProxyHops is how many trusted proxies are in front, each appending to
	// X-Forwarded-For (0: the connection's address).
	ProxyHops int
	l         rateLimiter
}

// NewRateLimiter allows max requests per window from each address.
func NewRateLimiter(max int, window time.Duration, proxyHops int) *RateLimiter {
	return &RateLimiter{ProxyHops: proxyHops, l: rateLimiter{max: max, window: window}}
}

// Allow records a request, reporting whether it's within the limit.
func (l *RateLimiter) Allow(r *http.Request, now time.Time) bool {
	return l.l.allow(clientAddr(r, l.ProxyHops), now)
}

// rateLimiter allows max events per window for each key, in memory (per
// instance), pruning idle keys as it goes.
type rateLimiter struct {
	max    int
	window time.Duration
	keys   int // most addresses tracked at once (default limiterKeys)
	mu     sync.Mutex
	seen   map[string][]time.Time
	pruned time.Time
}

// limiterKeys bounds a limiter's memory: past it, a new address is refused
// until old ones expire (the endpoints are unauthenticated, and IPv6 makes
// addresses cheap).
const limiterKeys = 100_000

func (l *rateLimiter) allow(key string, now time.Time) bool {
	if l.max < 0 {
		return true
	}
	key = limitKey(key)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.seen == nil {
		l.seen = map[string][]time.Time{}
	}
	cut := now.Add(-l.window)
	prune := func() {
		for k, ts := range l.seen {
			if len(ts) == 0 || !ts[len(ts)-1].After(cut) {
				delete(l.seen, k)
			}
		}
		l.pruned = now
	}
	if now.Sub(l.pruned) > l.window {
		prune()
	}
	if _, ok := l.seen[key]; !ok {
		limit := l.keys
		if limit <= 0 {
			limit = limiterKeys
		}
		if len(l.seen) >= limit {
			prune()
			if len(l.seen) >= limit {
				return false
			}
		}
	}
	ts := l.seen[key]
	i := 0
	for i < len(ts) && !ts[i].After(cut) {
		i++
	}
	ts = ts[i:]
	if len(ts) >= l.max {
		l.seen[key] = ts
		return false
	}
	l.seen[key] = append(ts, now)
	return true
}

// limitKey is what an address counts as for the limits: an IPv6 address's
// /64 (one host usually has a whole /64), anything else as it is.
func limitKey(addr string) string {
	ip, err := netip.ParseAddr(addr)
	if err != nil || ip.Is4() || ip.Is4In6() {
		return addr
	}
	p, err := ip.Prefix(64)
	if err != nil {
		return addr
	}
	return p.String()
}

// clientAddr is the request's client address: with hops trusted proxies in
// front (each appending to X-Forwarded-For), the entry the outermost one
// added; entries before it are the client's own claims.
func clientAddr(r *http.Request, hops int) string {
	if f := r.Header.Get("X-Forwarded-For"); f != "" && hops > 0 {
		// Only an entry the proxies added: with fewer entries than hops, or
		// one that isn't an address, the header isn't the proxies' (a
		// request that reached the app some other way), so it's ignored.
		parts := strings.Split(f, ",")
		if len(parts) >= hops {
			if ip, err := netip.ParseAddr(strings.TrimSpace(parts[len(parts)-hops])); err == nil {
				return ip.String()
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
