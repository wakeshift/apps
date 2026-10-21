package appkit

import (
	"fmt"
	"net/http/httptest"
	"testing"
	"time"
)

// The guest limits count an IPv6 /64 as one address, and track a bounded
// number of addresses: past it a new one is refused until old ones expire.
func TestRateLimiterBounds(t *testing.T) {
	now := time.Now()
	l := &rateLimiter{max: 2, window: time.Minute}
	if !l.allow("2001:db8:1:2::1", now) || !l.allow("2001:db8:1:2::ffff", now) || l.allow("2001:db8:1:2:abcd::9", now) {
		t.Fatal("one /64 isn't one address")
	}
	if !l.allow("2001:db8:1:3::1", now) || !l.allow("192.0.2.1", now) {
		t.Fatal("other addresses")
	}

	l = &rateLimiter{max: 5, window: time.Minute, keys: 3}
	for i := range 3 {
		if !l.allow(fmt.Sprintf("192.0.2.%d", i), now) {
			t.Fatalf("address %d", i)
		}
	}
	if l.allow("192.0.2.200", now) {
		t.Fatal("a new address past the cap")
	}
	if !l.allow("192.0.2.1", now) {
		t.Fatal("a tracked address past the cap")
	}
	if !l.allow("192.0.2.200", now.Add(2*time.Minute)) || len(l.seen) != 1 {
		t.Fatalf("after the old ones expired: %d tracked", len(l.seen))
	}
}

// Behind 2 proxies the address is the entry the outer proxy added; a header
// too short for the proxies, or an entry that isn't an address, is ignored
// for the connection's own address.
func TestClientAddr(t *testing.T) {
	for xff, want := range map[string]string{
		"spoofed, 203.0.113.7, 10.0.0.1": "203.0.113.7",
		"203.0.113.7, 10.0.0.1":          "203.0.113.7",
		"10.0.0.1":                       "198.51.100.2", // fewer than the hops
		"   ":                            "198.51.100.2",
		"not-an-ip, 10.0.0.1":            "198.51.100.2",
	} {
		r := httptest.NewRequest("POST", "/oauth/token", nil)
		r.RemoteAddr = "198.51.100.2:5555"
		r.Header.Set("X-Forwarded-For", xff)
		if got := clientAddr(r, 2); got != want {
			t.Errorf("%q: %s, want %s", xff, got, want)
		}
	}
}
