package analytics

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

var testPages = compile([]string{"/", "/rules", "/games", "/games/{id}", "/watch/{secret}", "/docs/quickstart"})

func TestMatch(t *testing.T) {
	for path, want := range map[string]string{
		"/":                   "/",
		"/rules":              "/rules",
		"/games/abc123":       "/games/:id",
		"/watch/s3cr3t-T0k3n": "/watch/:secret",
		"/docs/quickstart":    "/docs/quickstart",
		"/games/":             "",
		"/games/a/b":          "",
		"/rules/":             "",
		"/oauth/authorize":    "",
		"/docs/other":         "",
		"":                    "",
	} {
		got, ok := match(testPages, path)
		if ok != (want != "") || got != want {
			t.Errorf("match(%q) = %q, %v; want %q", path, got, ok, want)
		}
	}
}

func TestEventSanitises(t *testing.T) {
	body := `{"event":"$pageview","distinct_id":"0b0e2a3c-1d2e-4f5a-8b9c-0d1e2f3a4b5c","properties":{
		"$current_url":"https://nightfall.example/games/g-8f3a9c?invite=SECRET#frag",
		"$referrer":"https://www.google.com/search?q=private+words",
		"$ip":"203.0.113.9","email":"someone@example.com","$set":{"name":"x"}}}`
	ev, ok := event(testPages, "nightfall.example", []byte(body))
	if !ok {
		t.Fatal("dropped a page view")
	}
	props := ev["properties"].(map[string]any)
	want := map[string]any{
		"$current_url":            "https://nightfall.example/games/:id",
		"$host":                   "nightfall.example",
		"$pathname":               "/games/:id",
		"$referrer":               "https://www.google.com/",
		"$referring_domain":       "www.google.com",
		"$lib":                    "wakeshift-web",
		"$process_person_profile": false,
		"$geoip_disable":          true,
	}
	if len(props) != len(want) {
		t.Errorf("properties = %v, want exactly %v", props, want)
	}
	for k, v := range want {
		if props[k] != v {
			t.Errorf("%s = %v, want %v", k, props[k], v)
		}
	}
	if ev["distinct_id"] != "0b0e2a3c-1d2e-4f5a-8b9c-0d1e2f3a4b5c" || ev["event"] != "$pageview" {
		t.Errorf("event = %v", ev)
	}
	if out, _ := json.Marshal(ev); strings.Contains(string(out), "SECRET") || strings.Contains(string(out), "private") ||
		strings.Contains(string(out), "203.0.113.9") || strings.Contains(string(out), "someone@") || strings.Contains(string(out), "8f3a9c") {
		t.Errorf("leaked: %s", out)
	}
}

func TestEventReferrer(t *testing.T) {
	for ref, want := range map[string][2]string{
		"":                                      {"$direct", "$direct"},
		"$direct":                               {"$direct", "$direct"},
		"not a url":                             {"$direct", "$direct"},
		"javascript:alert(1)":                   {"$direct", "$direct"},
		"https://site.example/watch/abc?x=1#y":  {"https://site.example/watch/:secret", "site.example"},
		"https://site.example/signin?code=abc":  {"https://site.example/", "site.example"},
		"https://Other.Example/path/123?q=word": {"https://other.example/", "other.example"},
	} {
		ref1, dom := cleanReferrer(testPages, "site.example", ref)
		if ref1 != want[0] || dom != want[1] {
			t.Errorf("cleanReferrer(%q) = %q, %q; want %q, %q", ref, ref1, dom, want[0], want[1])
		}
	}
}

func TestEventDrops(t *testing.T) {
	for name, body := range map[string]string{
		"not json":         `{`,
		"unknown event":    `{"event":"$identify","properties":{"$current_url":"https://h.example/"}}`,
		"autocapture":      `{"event":"$autocapture","properties":{"$current_url":"https://h.example/"}}`,
		"other host":       `{"event":"$pageview","properties":{"$current_url":"https://evil.example/"}}`,
		"page not tracked": `{"event":"$pageview","properties":{"$current_url":"https://h.example/oauth/authorize?state=x"}}`,
		"no url":           `{"event":"$pageview","properties":{}}`,
		"bad scheme":       `{"event":"$pageview","properties":{"$current_url":"ftp://h.example/"}}`,
		"unknown cta":      `{"event":"cta_clicked","properties":{"$current_url":"https://h.example/","cta":"someone@example.com"}}`,
		"missing cta":      `{"event":"cta_clicked","properties":{"$current_url":"https://h.example/"}}`,
		"agent typed text": `{"event":"agent_picked","properties":{"$current_url":"https://h.example/","agent":"My Secret Text"}}`,
	} {
		if ev, ok := event(testPages, "h.example", []byte(body)); ok {
			t.Errorf("%s: passed %v", name, ev)
		}
	}
}

func TestEventNamed(t *testing.T) {
	ev, ok := event(testPages, "h.example", []byte(`{"event":"cta_clicked","distinct_id":"x","properties":{"$current_url":"https://h.example/","cta":"start_building","label":"typed"}}`))
	if !ok {
		t.Fatal("dropped cta_clicked")
	}
	props := ev["properties"].(map[string]any)
	if props["cta"] != "start_building" || props["label"] != nil {
		t.Errorf("properties = %v", props)
	}
	if id, _ := ev["distinct_id"].(string); !uuidRe.MatchString(id) {
		t.Errorf("distinct_id %q: a non-UUID ID wasn't replaced", id)
	}
	ev, ok = event(testPages, "h.example", []byte(`{"event":"agent_picked","properties":{"$current_url":"https://h.example/rules","agent":"claude-code"}}`))
	if !ok || ev["properties"].(map[string]any)["agent"] != "claude-code" {
		t.Errorf("agent_picked = %v, %v", ev, ok)
	}
	if _, ok := event(testPages, "h.example", []byte(`{"event":"contact_submitted","properties":{"$current_url":"https://h.example/"}}`)); !ok {
		t.Error("dropped contact_submitted")
	}
}

func TestAllowSelf(t *testing.T) {
	for in, want := range map[string]string{
		"default-src 'self'; frame-ancestors 'none'":                            "default-src 'self'; frame-ancestors 'none'",
		"default-src 'none'; style-src 'unsafe-inline'; img-src 'self'":         "default-src 'none'; style-src 'unsafe-inline'; img-src 'self'; script-src 'self'; connect-src 'self'",
		"default-src 'none'; script-src 'unsafe-inline'; connect-src 'self'":    "default-src 'none'; script-src 'unsafe-inline' 'self'; connect-src 'self'",
		"default-src 'none'; script-src 'self' 'nonce-abc'; connect-src 'none'": "default-src 'none'; script-src 'self' 'nonce-abc'; connect-src 'self'",
		"default-src https://a.example; form-action 'none'":                     "default-src https://a.example; form-action 'none'; script-src https://a.example 'self'; connect-src https://a.example 'self'",
		"frame-ancestors 'none'":                                                "frame-ancestors 'none'",
	} {
		if got := AllowSelf(in); got != want {
			t.Errorf("AllowSelf(%q)\n = %q\nwant %q", in, got, want)
		}
	}
}

func TestInject(t *testing.T) {
	got := string(Inject([]byte("<html><head><title>x</title></HEAD><body></body></html>")))
	if want := "<title>x</title>" + string(scriptTag) + "</HEAD>"; !strings.Contains(got, want) {
		t.Errorf("Inject = %s", got)
	}
	if got := string(Inject([]byte("<p>hi</p></body>"))); got != "<p>hi</p>"+string(scriptTag)+"</body>" {
		t.Errorf("Inject without head = %s", got)
	}
}

func page(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/plain":
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "</head>")
		return
	case "/games/missing":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, "<html><head></head></html>")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.Header().Set("Content-Length", "26")
	io.WriteString(w, "<html><head></head></html>")
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec
}

func TestWrapOffWithoutKey(t *testing.T) {
	next := http.HandlerFunc(page)
	h := Wrap(next, Config{}, "/")
	if rec := get(t, h, "/"); strings.Contains(rec.Body.String(), "<script") {
		t.Errorf("script without a key: %s", rec.Body)
	}
	if rec := get(t, h, ScriptPath); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "sendBeacon") {
		// The page handler answers; /ingest isn't served.
		t.Errorf("/ingest served without a key: %d %s", rec.Code, rec.Body)
	}
}

func TestWrapOnlyOnPages(t *testing.T) {
	h := Wrap(http.HandlerFunc(page), Config{Key: "phc_test"}, "/", "/games/{id}")
	rec := get(t, h, "/")
	if !strings.Contains(rec.Body.String(), `<script src="/ingest/p.js?v=`) {
		t.Fatalf("no script on a tracked page: %s", rec.Body)
	}
	if got := rec.Header().Get("Content-Security-Policy"); got != "default-src 'none'; style-src 'unsafe-inline'; script-src 'self'; connect-src 'self'" {
		t.Errorf("CSP = %q", got)
	}
	if rec.Header().Get("Content-Length") != itoa(rec.Body.Len()) {
		t.Errorf("Content-Length %q for %d bytes", rec.Header().Get("Content-Length"), rec.Body.Len())
	}
	for _, path := range []string{"/oauth/authorize", "/signin", "/privacy", "/games/x/state", "/games/missing", "/plain"} {
		rec := get(t, h, path)
		if strings.Contains(rec.Body.String(), "<script") {
			t.Errorf("%s: script on a page that isn't tracked", path)
		}
		if strings.Contains(rec.Header().Get("Content-Security-Policy"), "script-src") {
			t.Errorf("%s: CSP changed", path)
		}
	}
	rec = get(t, h, ScriptPath)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "sendBeacon") || rec.Header().Get("Content-Type") != "text/javascript; charset=utf-8" {
		t.Errorf("script: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestProxy(t *testing.T) {
	var mu sync.Mutex
	var got []*http.Request
	var bodies []map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		got, bodies = append(got, r), append(bodies, b)
		mu.Unlock()
		io.WriteString(w, `{"status":"Ok"}`)
	}))
	defer upstream.Close()
	h := Wrap(http.HandlerFunc(page), Config{Key: "phc_test", Host: upstream.URL + "/"}, "/", "/games/{id}")

	post := func(path, body string, hdr map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		req.Host = "games.example"
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	ok := `{"event":"$pageview","properties":{"$current_url":"https://games.example/games/123?x=1"}}`
	rec := post(CapturePath, ok, map[string]string{
		"Cookie":          "ws_session=secret",
		"X-Forwarded-For": "203.0.113.9, 10.0.0.1",
		"X-Real-Ip":       "203.0.113.9",
		"User-Agent":      "Visitor Browser/1.0",
		"Referer":         "https://games.example/games/123?x=1",
		"Content-Type":    "application/json",
	})
	if rec.Code != http.StatusOK || rec.Body.String() != `{"status":"Ok"}` {
		t.Fatalf("capture: %d %s", rec.Code, rec.Body)
	}
	if len(got) != 1 {
		t.Fatalf("upstream got %d requests", len(got))
	}
	r := got[0]
	if r.URL.Path != "/i/v0/e/" || r.Method != "POST" {
		t.Errorf("forwarded to %s %s", r.Method, r.URL.Path)
	}
	for _, k := range []string{"Cookie", "X-Forwarded-For", "X-Real-Ip", "Referer", "Forwarded"} {
		if v := r.Header.Get(k); v != "" {
			t.Errorf("forwarded %s: %q", k, v)
		}
	}
	if ua := r.Header.Get("User-Agent"); strings.Contains(ua, "Visitor") {
		t.Errorf("forwarded the visitor's User-Agent %q", ua)
	}
	b := bodies[0]
	if b["api_key"] != "phc_test" || b["properties"].(map[string]any)["$current_url"] != "https://games.example/games/:id" {
		t.Errorf("forwarded body %v", b)
	}

	// Only the capture endpoint and the script, and only same-origin posts.
	for _, c := range []struct {
		method, path string
		hdr          map[string]string
		want         int
	}{
		{"POST", "/ingest/batch/", nil, http.StatusNotFound},
		{"POST", "/ingest/decide/", nil, http.StatusNotFound},
		{"POST", "/ingest/e/", nil, http.StatusNotFound},
		{"GET", "/ingest/static/array.js", nil, http.StatusNotFound},
		{"GET", CapturePath, nil, http.StatusNotFound},
		{"POST", CapturePath, map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
	} {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(ok))
		req.Host = "games.example"
		for k, v := range c.hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s %s: %d, want %d", c.method, c.path, rec.Code, c.want)
		}
	}
	// Dropped events and oversized bodies never reach PostHog.
	if rec := post(CapturePath, `{"event":"$pageview","properties":{"$current_url":"https://games.example/signin?code=x"}}`, nil); rec.Code != http.StatusNoContent {
		t.Errorf("untracked page: %d", rec.Code)
	}
	if rec := post(CapturePath, `{"event":"$pageview","properties":{"$current_url":"https://games.example/","pad":"`+strings.Repeat("x", maxBody)+`"}}`, nil); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized: %d", rec.Code)
	}
	if len(got) != 1 {
		t.Errorf("upstream got %d requests, want 1", len(got))
	}
}

func TestEventHostHeader(t *testing.T) {
	for _, host := range []string{`evil.example"><x`, "a@b.example", "", strings.Repeat("a.", 130) + "example", "white space", "h.example:99999999", "h.example/"} {
		body := `{"event":"$pageview","properties":{"$current_url":"https://` + host + `/"}}`
		if ev, ok := event(testPages, host, []byte(body)); ok {
			t.Errorf("Host %q: passed %v", host, ev)
		}
	}
	for _, host := range []string{"h.example", "H.Example", "localhost:8080", "127.0.0.1:8370"} {
		body := `{"event":"$pageview","properties":{"$current_url":"http://` + host + `/"}}`
		if _, ok := event(testPages, host, []byte(body)); !ok {
			t.Errorf("Host %q: dropped", host)
		}
	}
}

func TestLimiter(t *testing.T) {
	l := &limiter{perAddress: 2, total: 3}
	now := time.Now()
	for i, c := range []struct {
		addr string
		want bool
	}{{"a", true}, {"a", true}, {"a", false}, {"b", true}, {"c", false}, {"", false}} {
		if got := l.allow(c.addr, now); got != c.want {
			t.Errorf("%d: allow(%q) = %v, want %v", i, c.addr, got, c.want)
		}
	}
	if !l.allow("a", now.Add(time.Minute)) {
		t.Error("a new minute didn't reset the counts")
	}
	l = &limiter{perAddress: 1, total: 10}
	if !l.allow("", now) || !l.allow("", now) {
		t.Error("an unknown address was limited per address")
	}
}

func TestClientAddr(t *testing.T) {
	for _, c := range []struct {
		xff, remote string
		hops        int
		want        string
	}{
		{"", "192.0.2.1:1234", 0, "192.0.2.1"},
		{"203.0.113.9, 10.0.0.1", "192.0.2.1:1234", 0, "192.0.2.1"},
		{"203.0.113.9, 10.0.0.1", "192.0.2.1:1234", 2, "203.0.113.9"},
		{"spoofed, 203.0.113.9, 10.0.0.1", "192.0.2.1:1234", 2, "203.0.113.9"},
		{"10.0.0.1", "192.0.2.1:1234", 2, "192.0.2.1"}, // fewer entries than hops: not the proxies'
		{"not-an-ip, 10.0.0.1", "192.0.2.1:1234", 2, "192.0.2.1"},
		{"2001:db8:1:2:3:4:5:6, 10.0.0.1", "192.0.2.1:1234", 2, "2001:db8:1:2::/64"},
		{"::ffff:203.0.113.9, 10.0.0.1", "192.0.2.1:1234", 2, "203.0.113.9"},
		{"203.0.113.9, 10.0.0.1", "192.0.2.1:1234", -1, ""},
	} {
		r := httptest.NewRequest("POST", CapturePath, nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := clientAddr(r, c.hops); got != c.want {
			t.Errorf("clientAddr(%q, %d) = %q, want %q", c.xff, c.hops, got, c.want)
		}
	}
}

func TestFromEnvProxyHops(t *testing.T) {
	t.Setenv("WAKESHIFT_ANALYTICS_PROXY_HOPS", "")
	if got := FromEnv().ProxyHops; got != -1 {
		t.Errorf("unset: ProxyHops %d, want -1", got)
	}
	t.Setenv("WAKESHIFT_ANALYTICS_PROXY_HOPS", "2")
	if got := FromEnv().ProxyHops; got != 2 {
		t.Errorf("2: ProxyHops %d", got)
	}
}

// TestProxyLimits: /ingest can't be used to flood PostHog, to make it
// follow a redirect, or to hold a request open with a slow body.
func TestProxyLimits(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	count := func() int { mu.Lock(); defer mu.Unlock(); return hits }
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		if r.URL.Path != "/i/v0/e/" {
			t.Errorf("followed to %s", r.URL.Path)
		}
		io.WriteString(w, `{"status":"Ok"}`)
	}))
	defer upstream.Close()
	ok := `{"event":"$pageview","properties":{"$current_url":"https://games.example/"}}`
	post := func(h http.Handler, client string) int {
		req := httptest.NewRequest("POST", CapturePath, strings.NewReader(ok))
		req.Host = "games.example"
		req.Header.Set("X-Forwarded-For", client+", 10.0.0.1")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	h := Wrap(http.HandlerFunc(page), Config{Key: "phc_test", Host: upstream.URL, ProxyHops: 2}, "/").(*handler)
	h.limit = &limiter{perAddress: 3, total: 5}
	for range 10 { // dropped events don't count
		req := httptest.NewRequest("POST", CapturePath, strings.NewReader(`{"event":"$identify"}`))
		req.Host = "games.example"
		req.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("dropped event: %d", rec.Code)
		}
	}
	for i := range 3 {
		if code := post(h, "203.0.113.9"); code != http.StatusOK {
			t.Fatalf("event %d: %d", i, code)
		}
	}
	for _, c := range []struct {
		client string
		want   int
	}{
		{"203.0.113.9", http.StatusTooManyRequests}, // past the per-address limit
		{"2001:db8::1", http.StatusOK},
		{"2001:db8::2", http.StatusOK},               // the same /64: its second
		{"198.51.100.1", http.StatusTooManyRequests}, // past the overall limit
	} {
		if code := post(h, c.client); code != c.want {
			t.Errorf("%s: %d, want %d", c.client, code, c.want)
		}
	}
	if n := count(); n != 5 {
		t.Errorf("PostHog got %d events, want 5", n)
	}

	// PostHog's redirect isn't followed.
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, upstream.URL+"/elsewhere", http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	if code := post(Wrap(http.HandlerFunc(page), Config{Key: "phc_test", Host: redirect.URL}, "/"), "203.0.113.9"); code != http.StatusTemporaryRedirect {
		t.Errorf("redirect: %d", code)
	}
	if n := count(); n != 5 {
		t.Errorf("followed a redirect")
	}

	// Requests to PostHog at once are capped.
	h = Wrap(http.HandlerFunc(page), Config{Key: "phc_test", Host: upstream.URL, ProxyHops: -1}, "/").(*handler)
	h.inFlight = make(chan struct{}, 1)
	h.inFlight <- struct{}{} // one already waiting on PostHog
	if code := post(h, "203.0.113.9"); code != http.StatusServiceUnavailable {
		t.Errorf("past the in-flight cap: %d", code)
	}

	// A body that never finishes is cut off.
	h = Wrap(http.HandlerFunc(page), Config{Key: "phc_test", Host: upstream.URL}, "/").(*handler)
	h.readTimeout = 200 * time.Millisecond
	srv := httptest.NewServer(h)
	defer srv.Close()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: games.example\r\nContent-Length: 100\r\n\r\n{", CapturePath)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("slow body: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("slow body: %d", resp.StatusCode)
	}
	if n := count(); n != 5 {
		t.Errorf("PostHog got %d events, want 5", n)
	}
}
