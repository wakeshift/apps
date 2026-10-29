package appkit

import (
	"bytes"
	"context"
	"io"
	"mime"
	"net/http"
	"time"
)

// Wakeshift runs behind Google's load balancer on Cloud Run. For some
// minutes after a rollout, the load balancer sends a small share of requests
// (about 1% on staging, 2026-10-22) to a retired revision, which answers at
// once with Google's own HTML "500 Server Error" page: the request never
// reached a Wakeshift instance. It happens to every client alike (new
// connections, other apps, uptime checks), so it isn't a stale connection:
// retrying is what gets the request through.

// retryPauses are the pauses before each retry of a request the load
// balancer refused (so at most len+1 attempts).
var retryPauses = []time.Duration{250 * time.Millisecond, time.Second}

// do sends req through client, retrying a request the load balancer refused
// before it reached Wakeshift: Google's HTML 500 page, for any request (it
// never ran), and a 502, 503 or 504 not from Wakeshift for a GET or HEAD.
// Wakeshift's own errors (JSON) are never retried. Before a retry it drops
// the client's idle connections, so the retry starts afresh.
func (a *App) do(client *http.Client, req *http.Request) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		resp, err := client.Do(req)
		if err != nil || attempt >= len(retryPauses) || !refused(req, resp) {
			return resp, err
		}
		next, ok := rewound(req)
		if !ok {
			return resp, nil
		}
		// Drain a little so the connection can be reused, then drop the idle ones.
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		client.CloseIdleConnections()
		if err := pause(req.Context(), retryPauses[attempt]); err != nil {
			return nil, err
		}
		req = next
	}
}

// refused reports whether resp is the load balancer's refusal, not
// Wakeshift's answer. It may read the start of the body; resp.Body still
// reads it all.
func refused(req *http.Request, resp *http.Response) bool {
	switch resp.StatusCode {
	case http.StatusInternalServerError:
		if !isHTML(resp) {
			return false
		}
		head, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(head), resp.Body), resp.Body}
		return bytes.Contains(head, []byte("Server Error"))
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return (req.Method == http.MethodGet || req.Method == http.MethodHead) && !isJSON(resp)
	}
	return false
}

func mediaType(resp *http.Response) string {
	t, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	return t
}

func isHTML(resp *http.Response) bool { return mediaType(resp) == "text/html" }
func isJSON(resp *http.Response) bool { return mediaType(resp) == "application/json" }

// rewound is req again, with its body from the start (false if it can't be).
func rewound(req *http.Request) (*http.Request, bool) {
	next := req.Clone(req.Context())
	if req.Body == nil || req.Body == http.NoBody {
		return next, true
	}
	if req.GetBody == nil {
		return nil, false
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, false
	}
	next.Body = body
	return next, true
}

func pause(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
