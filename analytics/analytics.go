// Package analytics counts visits to Wakeshift's public pages, and a few
// actions on them, with PostHog (docs/analytics.md). It's off unless
// WAKESHIFT_POSTHOG_KEY is set.
//
// Wrap puts a small script (client.js) on the pages it's given, and nowhere
// else, and serves /ingest: the script, and a proxy that forwards the
// script's events to PostHog. The proxy, not the browser, decides what
// PostHog gets: only the events and values it knows, with page addresses cut
// to the page's pattern (/games/:id) and no query string or fragment. It
// builds a fresh request, so the visitor's cookies, IP address
// (X-Forwarded-For) and browser never reach PostHog. Nothing is stored in
// the browser, and nothing anyone types is sent. It forwards a limited
// number of events a minute, from each address and in all, so a script
// can't flood the PostHog project.
//
// apps/analytics/analytics.ts is the same for the TypeScript apps; the
// copies in apps/holdem and apps/try must match it (TestTypeScriptCopies).
package analytics

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed client.js
var clientJS []byte

// DefaultHost is PostHog's US ingestion host.
const DefaultHost = "https://us.i.posthog.com"

// Paths under /ingest: the script, and PostHog's capture endpoint.
const (
	ScriptPath  = "/ingest/p.js"
	CapturePath = "/ingest/i/v0/e/"
)

// Config is the PostHog project the events go to.
type Config struct {
	Key  string // the project API key (public by design); empty: off
	Host string // the ingestion host (default DefaultHost)
	// ProxyHops is how many trusted proxies in front append to
	// X-Forwarded-For, to find each visitor's address for the per-address
	// limit (0: the connection's address). Negative: the address isn't
	// known, and only the overall limit applies.
	ProxyHops int
}

// FromEnv reads WAKESHIFT_POSTHOG_KEY, WAKESHIFT_POSTHOG_HOST and
// WAKESHIFT_ANALYTICS_PROXY_HOPS (unset or not a number: the address isn't
// known).
func FromEnv() Config {
	hops, err := strconv.Atoi(strings.TrimSpace(os.Getenv("WAKESHIFT_ANALYTICS_PROXY_HOPS")))
	if err != nil {
		hops = -1
	}
	return Config{Key: strings.TrimSpace(os.Getenv("WAKESHIFT_POSTHOG_KEY")), Host: strings.TrimSpace(os.Getenv("WAKESHIFT_POSTHOG_HOST")), ProxyHops: hops}
}

// The limits on what /ingest forwards, for each instance: events a minute
// from one address (an IPv6 address counts as its /64) and in all, and
// requests to PostHog at once. Past them, events are refused (429, 503).
const (
	PerAddress   = 60
	PerInstance  = 600
	InFlight     = 32
	maxAddresses = 100_000 // addresses tracked in a minute; past it, a new one is refused
	readTimeout  = 10 * time.Second
)

// Enabled is whether a key is set.
func (c Config) Enabled() bool { return c.Key != "" }

var (
	scriptHash = func() string { s := sha256.Sum256(clientJS); return hex.EncodeToString(s[:6]) }()
	scriptTag  = []byte(`<script src="` + ScriptPath + `?v=` + scriptHash + `" defer></script>`)
)

// Wrap serves next, with the analytics script on the pages that match one
// of pages, and /ingest. A pattern is a path whose {name} segments match any
// one segment ("/games/{id}"), and which PostHog sees as :name. With no key,
// it returns next as it is.
func Wrap(next http.Handler, cfg Config, pages ...string) http.Handler {
	if !cfg.Enabled() {
		return next
	}
	host := strings.TrimSuffix(cmp.Or(cfg.Host, DefaultHost), "/")
	return &handler{next: next, key: cfg.Key, capture: host + "/i/v0/e/", pages: compile(pages), hops: cfg.ProxyHops,
		client: &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // only ever the capture URL
		}},
		origin:      http.NewCrossOriginProtection(),
		limit:       &limiter{perAddress: PerAddress, total: PerInstance},
		inFlight:    make(chan struct{}, InFlight),
		readTimeout: readTimeout}
}

type handler struct {
	next     http.Handler
	key      string
	capture  string // PostHog's capture URL
	pages    []pattern
	hops     int
	client   *http.Client
	origin   *http.CrossOriginProtection
	limit    *limiter
	inFlight chan struct{} // a slot per request to PostHog
	// readTimeout is how long a request's body may take to arrive.
	readTimeout time.Duration
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/ingest/") {
		h.ingest(w, r)
		return
	}
	if r.Method != http.MethodGet {
		h.next.ServeHTTP(w, r)
		return
	}
	if _, ok := match(h.pages, r.URL.Path); !ok {
		h.next.ServeHTTP(w, r)
		return
	}
	bw := &bufferWriter{w: w, header: http.Header{}}
	h.next.ServeHTTP(bw, r)
	bw.finish()
}

// ingest serves the script and the capture endpoint, and nothing else.
func (h *handler) ingest(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == ScriptPath && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		cache := "public, max-age=3600"
		if r.URL.Query().Get("v") == scriptHash {
			cache = "public, max-age=31536000, immutable"
		}
		w.Header().Set("Cache-Control", cache)
		w.Write(clientJS)
	case r.URL.Path == CapturePath && r.Method == http.MethodPost:
		h.forward(w, r)
	default:
		http.NotFound(w, r)
	}
}

const maxBody = 4 << 10

// forward sends one event on to PostHog, as event makes it.
func (h *handler) forward(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if err := h.origin.Check(r); err != nil {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	// A body that trickles in doesn't hold the request open.
	http.NewResponseController(w).SetReadDeadline(time.Now().Add(h.readTimeout))
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		if errors.As(err, new(*http.MaxBytesError)) {
			http.Error(w, "too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "unreadable body", http.StatusBadRequest)
		}
		return
	}
	ev, ok := event(h.pages, r.Host, body)
	if !ok {
		w.WriteHeader(http.StatusNoContent) // dropped: nothing for the page to do about it
		return
	}
	// Only events PostHog would get count against the limits, and only the
	// request to PostHog holds a slot.
	select {
	case h.inFlight <- struct{}{}:
		defer func() { <-h.inFlight }()
	default:
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return
	}
	if !h.limit.allow(clientAddr(r, h.hops), time.Now()) {
		http.Error(w, "too many events", http.StatusTooManyRequests)
		return
	}
	ev["api_key"] = h.key
	out, _ := json.Marshal(ev)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.capture, bytes.NewReader(out))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "wakeshift-analytics")
	resp, err := h.client.Do(req)
	if err != nil {
		slog.Warn("analytics: forwarding an event", "err", err)
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	reply, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	w.Write(reply)
}

// limiter counts events in fixed one-minute windows: perAddress from each
// address, and total in all.
type limiter struct {
	perAddress, total int
	mu                sync.Mutex
	window            time.Time
	all               int
	seen              map[string]int
}

// allow records an event from addr ("": not known), reporting whether it's
// within the limits.
func (l *limiter) allow(addr string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.seen == nil || now.Sub(l.window) >= time.Minute || now.Before(l.window) {
		l.window, l.all, l.seen = now, 0, map[string]int{}
	}
	if l.all >= l.total {
		return false
	}
	if addr != "" {
		n, ok := l.seen[addr]
		if n >= l.perAddress || (!ok && len(l.seen) >= maxAddresses) {
			return false
		}
		l.seen[addr] = n + 1
	}
	l.all++
	return true
}

// clientAddr is the visitor's address, as the hops trusted proxies in front
// wrote it in X-Forwarded-For (else the connection's), with an IPv6 address
// as its /64; "" if hops is negative.
func clientAddr(r *http.Request, hops int) string {
	if hops < 0 {
		return ""
	}
	var s string
	if hops > 0 {
		// Only an entry the proxies added: with fewer entries than hops, or
		// one that isn't an address, the header isn't theirs.
		if parts := strings.Split(r.Header.Get("X-Forwarded-For"), ","); len(parts) >= hops {
			s = strings.TrimSpace(parts[len(parts)-hops])
		}
	}
	ip, err := netip.ParseAddr(s)
	if err != nil {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		if ip, err = netip.ParseAddr(host); err != nil {
			return host
		}
	}
	ip = ip.Unmap()
	if ip.Is6() {
		p, _ := ip.Prefix(64)
		return p.String()
	}
	return ip.String()
}

// The events a page may send, and the property each may carry with the
// values allowed. Anything else is dropped.
var (
	events = map[string]string{
		"$pageview":         "",
		"$pageleave":        "",
		"contact_submitted": "",
		"cta_clicked":       "cta",
		"agent_picked":      "agent",
	}
	ctas    = map[string]bool{"start_building": true, "contact_us": true, "quickstart": true, "try_it": true}
	agentID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	// hostRe is a host name or IPv4 address, with an optional port: the Host
	// header is the visitor's to set, and PostHog gets it as $host.
	hostRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*(:[0-9]{1,5})?$`)
	uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

func allowed(prop, v string) bool {
	switch prop {
	case "cta":
		return ctas[v]
	case "agent":
		return agentID.MatchString(v)
	}
	return false
}

// event is what PostHog gets for an event a page on host sent (body, in
// PostHog's capture format), without the API key: ok is false if it's
// dropped. Only known events and values pass; the page's address is cut to
// its pattern, and the referrer to its pattern on this host or to its origin
// elsewhere, neither with a query string or fragment; and the event is
// marked to make no person profile and no location.
func event(pages []pattern, host string, body []byte) (map[string]any, bool) {
	// Fields by their exact names (encoding/json would also match "EVENT"),
	// as analytics.ts reads them.
	var raw map[string]json.RawMessage
	if json.Unmarshal(body, &raw) != nil {
		return nil, false
	}
	var in struct {
		Event      string
		DistinctID string
		Properties map[string]any
	}
	if json.Unmarshal(raw["event"], &in.Event) != nil {
		return nil, false
	}
	json.Unmarshal(raw["distinct_id"], &in.DistinctID)
	json.Unmarshal(raw["properties"], &in.Properties)
	prop, known := events[in.Event]
	name, _, _ := strings.Cut(host, ":") // without the port
	if !known || len(name) > 253 || !hostRe.MatchString(strings.ToLower(host)) {
		return nil, false
	}
	str := func(k string) string { s, _ := in.Properties[k].(string); return s }
	current, ok := cleanURL(pages, host, str("$current_url"))
	if !ok {
		return nil, false
	}
	u, _ := url.Parse(current)
	ref, refDomain := cleanReferrer(pages, host, str("$referrer"))
	props := map[string]any{
		"$current_url":            current,
		"$host":                   u.Host,
		"$pathname":               u.Path,
		"$referrer":               ref,
		"$referring_domain":       refDomain,
		"$lib":                    "wakeshift-web",
		"$process_person_profile": false,
		"$geoip_disable":          true,
	}
	if prop != "" {
		v := str(prop)
		if !allowed(prop, v) {
			return nil, false
		}
		props[prop] = v
	}
	id := in.DistinctID
	if !uuidRe.MatchString(id) {
		id = newID()
	}
	return map[string]any{"event": in.Event, "distinct_id": id, "properties": props}, true
}

func newID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// cleanURL is raw, a page on host, as its pattern: ok is false if it isn't
// on host or matches no page.
func cleanURL(pages []pattern, host, raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || !strings.EqualFold(u.Host, host) {
		return "", false
	}
	p, ok := match(pages, u.EscapedPath())
	if !ok {
		return "", false
	}
	return u.Scheme + "://" + strings.ToLower(u.Host) + p, true
}

// cleanReferrer is the referrer as PostHog gets it, and its domain: a page
// on host as its pattern (the origin alone if it matches none), and another
// site's origin. None is "$direct".
func cleanReferrer(pages []pattern, host, raw string) (string, string) {
	u, err := url.Parse(raw)
	if raw == "" || raw == "$direct" || err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "$direct", "$direct"
	}
	h := strings.ToLower(u.Host)
	if strings.EqualFold(u.Host, host) {
		if p, ok := match(pages, u.EscapedPath()); ok {
			return u.Scheme + "://" + h + p, strings.ToLower(u.Hostname())
		}
	}
	return u.Scheme + "://" + h + "/", strings.ToLower(u.Hostname())
}

// pattern is a page's path, split at "/": a segment in braces matches any
// one segment.
type pattern []string

func compile(pages []string) []pattern {
	var ps []pattern
	for _, p := range pages {
		if !strings.HasPrefix(p, "/") {
			panic(fmt.Sprintf("analytics: page %q doesn't start with /", p))
		}
		ps = append(ps, strings.Split(p[1:], "/"))
	}
	return ps
}

// match is path as the first pattern that matches it ("/games/:id"); ok is
// false if none does.
func match(pages []pattern, path string) (string, bool) {
	if !strings.HasPrefix(path, "/") {
		return "", false
	}
	segs := strings.Split(path[1:], "/")
next:
	for _, p := range pages {
		if len(p) != len(segs) {
			continue
		}
		out := make([]string, len(p))
		for i, s := range p {
			if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
				if segs[i] == "" {
					continue next
				}
				out[i] = ":" + s[1:len(s)-1]
			} else if s != segs[i] {
				continue next
			} else {
				out[i] = s
			}
		}
		return "/" + strings.Join(out, "/"), true
	}
	return "", false
}

// bufferWriter holds a page's response, to add the script to it if it's an
// HTML page (status 200, uncompressed); anything else passes straight
// through.
type bufferWriter struct {
	w           http.ResponseWriter
	header      http.Header
	status      int
	passthrough bool
	buf         bytes.Buffer
}

func (b *bufferWriter) Header() http.Header {
	if b.passthrough {
		return b.w.Header()
	}
	return b.header
}

func (b *bufferWriter) WriteHeader(status int) {
	if b.status != 0 {
		return
	}
	b.status = status
	ct := b.header.Get("Content-Type")
	if status == http.StatusOK && strings.HasPrefix(ct, "text/html") && b.header.Get("Content-Encoding") == "" {
		return // held until finish
	}
	b.passthrough = true
	dst := b.w.Header()
	for k, v := range b.header {
		dst[k] = v
	}
	b.w.WriteHeader(status)
}

func (b *bufferWriter) Write(p []byte) (int, error) {
	if b.status == 0 {
		if b.header.Get("Content-Type") == "" {
			b.header.Set("Content-Type", http.DetectContentType(p))
		}
		b.WriteHeader(http.StatusOK)
	}
	if b.passthrough {
		return b.w.Write(p)
	}
	return b.buf.Write(p)
}

// Unwrap is for http.ResponseController.
func (b *bufferWriter) Unwrap() http.ResponseWriter { return b.w }

func (b *bufferWriter) finish() {
	if b.passthrough {
		return
	}
	if b.status == 0 { // nothing written
		b.WriteHeader(http.StatusOK)
		if b.passthrough {
			return
		}
	}
	body := Inject(b.buf.Bytes())
	dst := b.w.Header()
	for k, v := range b.header {
		dst[k] = v
	}
	if csp := dst.Get("Content-Security-Policy"); csp != "" {
		dst.Set("Content-Security-Policy", AllowSelf(csp))
	}
	dst.Set("Content-Length", strconv.Itoa(len(body)))
	b.w.WriteHeader(b.status)
	b.w.Write(body)
}

// Inject adds the script tag to an HTML page: before </head>, else before
// </body>, else at the end.
func Inject(page []byte) []byte {
	lower := bytes.ToLower(page)
	i := bytes.Index(lower, []byte("</head>"))
	if i < 0 {
		i = bytes.LastIndex(lower, []byte("</body>"))
	}
	if i < 0 {
		i = len(page)
	}
	out := make([]byte, 0, len(page)+len(scriptTag))
	out = append(out, page[:i]...)
	out = append(out, scriptTag...)
	return append(out, page[i:]...)
}

// AllowSelf is a Content-Security-Policy that also lets the page load
// scripts from, and connect to, its own origin: 'self' is added to
// script-src and connect-src, or to what they'd fall back to (default-src).
// Nothing else changes.
func AllowSelf(csp string) string {
	var dirs [][]string
	idx := map[string]int{}
	for _, d := range strings.Split(csp, ";") {
		f := strings.Fields(d)
		if len(f) == 0 {
			continue
		}
		f[0] = strings.ToLower(f[0])
		if _, dup := idx[f[0]]; dup {
			continue // browsers ignore a repeated directive
		}
		idx[f[0]] = len(dirs)
		dirs = append(dirs, f)
	}
	var fallback []string
	if i, ok := idx["default-src"]; ok {
		fallback = dirs[i][1:]
	}
	for _, name := range []string{"script-src", "connect-src"} {
		i, ok := idx[name]
		if !ok {
			if fallback == nil || hasSelf(fallback) {
				continue // allowed already
			}
			dirs = append(dirs, append([]string{name}, fallback...))
			i = len(dirs) - 1
			idx[name] = i
		}
		dirs[i] = withSelf(dirs[i])
	}
	parts := make([]string, len(dirs))
	for i, d := range dirs {
		parts[i] = strings.Join(d, " ")
	}
	return strings.Join(parts, "; ")
}

func hasSelf(sources []string) bool {
	for _, s := range sources {
		if strings.EqualFold(s, "'self'") {
			return true
		}
	}
	return false
}

func withSelf(d []string) []string {
	out := []string{d[0]}
	for _, s := range d[1:] {
		switch strings.ToLower(s) {
		case "'self'":
			return d
		case "'none'":
			continue
		}
		out = append(out, s)
	}
	return append(out, "'self'")
}
