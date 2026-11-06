package wakeshift

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	t.Cleanup(srv.Close)
	// The SDK's own HTTP client: only its idle connections are dropped.
	a := clientAt(t, srv.URL)
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

// WithMaxRetries sets how many retries there are: 0 turns them off, and
// more than the pauses repeat the last.
func TestMaxRetries(t *testing.T) {
	fastRetries(t)
	for _, c := range []struct{ retries, want int }{{0, 1}, {1, 2}, {4, 5}} {
		n := 0
		a, _ := testApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n++
			gfe500(w)
		}), WithMaxRetries(c.retries))
		a.appCall(context.Background(), "GET", "/api/apps/v1/workspaces", nil, nil)
		if n != c.want {
			t.Errorf("WithMaxRetries(%d): %d requests, want %d", c.retries, n, c.want)
		}
	}
	if _, err := New(WithKey(""), WithMaxRetries(-1)); !IsCode(err, CodeConfig) {
		t.Errorf("WithMaxRetries(-1): %v, want config", err)
	}
}

// A connection that fails is retried (the request never got an answer);
// one the HTTP client gave up on (a timeout) isn't: its outcome is unknown.
func TestConnectionFailuresAreRetried(t *testing.T) {
	fastRetries(t)
	var n atomic.Int32
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			n.Add(1)
			conn.Close() // no answer at all
		}
	}()
	t.Cleanup(func() { ln.Close() })
	a := clientAt(t, "http://"+ln.Addr().String(), WithHTTPClient(&http.Client{Transport: &http.Transport{}}))
	err = a.appCall(context.Background(), "POST", "/api/apps/v1/events/emit", map[string]any{"x": 1}, nil)
	if !IsCode(err, CodeUnreachable) || n.Load() != 3 {
		t.Errorf("%v after %d connections, want unreachable after 3", err, n.Load())
	}

	var m atomic.Int32
	stall := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { m.Add(1); <-stall }))
	t.Cleanup(slow.Close)
	t.Cleanup(func() { close(stall) })
	b := clientAt(t, slow.URL, WithHTTPClient(&http.Client{Timeout: 30 * time.Millisecond}))
	err = b.appCall(context.Background(), "POST", "/api/apps/v1/events/emit", map[string]any{"x": 1}, nil)
	if !IsCode(err, CodeTimeout) || m.Load() != 1 {
		t.Errorf("%v after %d requests, want timeout after 1", err, m.Load())
	}
}
