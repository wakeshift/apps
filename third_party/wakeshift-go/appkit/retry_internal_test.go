package appkit

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// gfePage is Google's front end's 500 page, as a retired Cloud Run revision
// answers through the load balancer.
const gfePage = `<html><head>
<meta http-equiv="content-type" content="text/html;charset=utf-8">
<title>500 Server Error</title>
</head>
<body text=#000000 bgcolor=#ffffff>
<h1>Error: Server Error</h1>
<h2>The server encountered an error and could not complete your request.<p>Please try again in 30 seconds.</h2>
<h2></h2>
</body></html>`

func fastRetries(t *testing.T) {
	old := retryPauses
	retryPauses = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { retryPauses = old })
}

func testApp(t *testing.T, h http.Handler) (*App, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	// Its own transport, so the test sees its own connections.
	return &App{ID: "app_test", Key: key, Core: srv.URL, HTTP: &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{}}}, srv
}

func gfe500(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=UTF-8")
	w.WriteHeader(http.StatusInternalServerError)
	io.WriteString(w, gfePage)
}

// A call the load balancer refuses (Google's 500 page) on the connection it
// had is sent again, body and all, on a fresh connection, and gets through:
// an emit, a POST, included, since the refused one never reached Wakeshift.
func TestRefusedCallRetriesOnAFreshConnection(t *testing.T) {
	fastRetries(t)
	var mu sync.Mutex
	var first string
	var bodies []string
	var addrs []string
	a, _ := testApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		addrs = append(addrs, r.RemoteAddr)
		if first == "" {
			first = r.RemoteAddr
		}
		stale := r.RemoteAddr == first
		mu.Unlock()
		if stale {
			gfe500(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"matched":2,"duplicate":false}`)
	}))
	// The first connection is the one refused.
	var out Emitted
	err := a.appCall(context.Background(), "POST", "/api/apps/v1/events/emit", map[string]any{"name": "game.update"}, &out)
	if err != nil || out.Matched != 2 {
		t.Fatalf("emit: %+v, %v", out, err)
	}
	if len(bodies) != 2 || bodies[0] != bodies[1] || bodies[1] != `{"name":"game.update"}` {
		t.Fatalf("bodies sent: %q", bodies)
	}
	if addrs[0] == addrs[1] {
		t.Fatalf("the retry reused the refused connection (%s)", addrs[0])
	}
}

// Requests on a connection the load balancer keeps refusing: a GET is tried
// three times, then the refusal is the error.
func TestRefusedCallGivesUp(t *testing.T) {
	fastRetries(t)
	n := 0
	a, _ := testApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		gfe500(w)
	}))
	err := a.appCall(context.Background(), "GET", "/api/apps/v1/workspaces", nil, nil)
	var e *APIError
	if !errors.As(err, &e) || e.Status != 500 || n != 3 {
		t.Fatalf("err %v after %d requests", err, n)
	}
}

// Wakeshift's own errors are answers: never retried, whatever the method.
// A 503 not from Wakeshift is retried for a GET, not for a POST (it might
// have run).
func TestWhatIsRetried(t *testing.T) {
	fastRetries(t)
	cases := []struct {
		name, method string
		status       int
		ctype, body  string
		want         int // requests
	}{
		{"core's 500", "POST", 500, "application/json", `{"error":{"code":"internal","message":"x"}}`, 1},
		{"core's 500, GET", "GET", 500, "application/json", `{"error":{"code":"internal","message":"x"}}`, 1},
		{"core's 503", "GET", 503, "application/json", `{"error":{"code":"unavailable","message":"x"}}`, 1},
		{"an HTML 500 that isn't the load balancer's", "POST", 500, "text/html", `<p>oops</p>`, 1},
		{"Cloud Run's 503, GET", "GET", 503, "text/html", `Service Unavailable`, 3},
		{"Cloud Run's 503, POST", "POST", 503, "text/html", `Service Unavailable`, 1},
		{"a 502, GET", "GET", 502, "text/html", `Bad Gateway`, 3},
		{"the load balancer's 500, POST", "POST", 500, "text/html; charset=UTF-8", gfePage, 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := 0
			a, _ := testApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n++
				w.Header().Set("Content-Type", c.ctype)
				w.WriteHeader(c.status)
				io.WriteString(w, c.body)
			}))
			var body any
			if c.method == "POST" {
				body = map[string]any{"x": 1}
			}
			err := a.appCall(context.Background(), c.method, "/api/apps/v1/x", body, nil)
			var e *APIError
			if !errors.As(err, &e) || e.Status != c.status || n != c.want {
				t.Fatalf("err %v after %d requests, want %d", err, n, c.want)
			}
		})
	}
}

// A retry waits within the request's context: canceling it ends the wait.
func TestRetryPauseHonoursContext(t *testing.T) {
	old := retryPauses
	retryPauses = []time.Duration{time.Hour}
	t.Cleanup(func() { retryPauses = old })
	a, _ := testApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { gfe500(w) }))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := a.appCall(ctx, "GET", "/api/apps/v1/workspaces", nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v", err)
	}
}
