package wakeshift

// Requests to Wakeshift's app API: signed with a fresh assertion, retried
// when a load balancer refused them before they reached Wakeshift or the
// connection failed, and read into one error hierarchy (errors.go).

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/wakeshift/wakeshift-go/internal/jws"
)

// defaultClient is the SDK's own HTTP client: a hung Wakeshift mustn't
// hang the app.
var defaultClient = &http.Client{Timeout: 20 * time.Second}

func (c *Client) client() *http.Client {
	if c.http != nil {
		return c.http
	}
	return defaultClient
}

// assertion signs a fresh, short-lived assertion of the app's identity,
// naming the key by its kid. iss and sub are the app's ID, or the kid
// while it isn't known.
func (c *Client) assertion() (string, error) {
	if !c.Enabled() {
		return "", errNoKey
	}
	jti := make([]byte, 16)
	rand.Read(jti)
	now := time.Now()
	iss := c.knownAppID()
	if iss == "" {
		iss = c.key.kid
	}
	return jws.Sign(c.key.priv, c.key.kid, "app+jwt", map[string]any{
		"iss": iss, "sub": iss, "aud": c.core + "/api/apps",
		"iat": now.Unix(), "exp": now.Add(2 * time.Minute).Unix(), "jti": base64.RawURLEncoding.EncodeToString(jti),
	})
}

// appCall sends a request to the app API with a fresh assertion and
// decodes the answer into out (nil: ignored).
func (c *Client) appCall(ctx context.Context, method, path string, body, out any) error {
	tok, err := c.assertion()
	if err != nil {
		return err
	}
	return c.callWith(ctx, c.client(), method, path, tok, body, out)
}

// callWith sends a JSON request through client with bearer and decodes the
// answer into out.
func (c *Client) callWith(ctx context.Context, client *http.Client, method, path, bearer string, body, out any) error {
	var b []byte
	if body != nil {
		var err error
		if b, err = json.Marshal(body); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.core+path, bytes.NewReader(b))
	if err != nil {
		return c.noAnswer(ctx, client, err)
	}
	if body == nil {
		req.Body, req.GetBody, req.ContentLength = http.NoBody, nil, 0
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.do(client, req)
	if err != nil {
		return c.noAnswer(ctx, client, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		e := errorOf(resp.StatusCode, raw)
		if e.RequestID == "" {
			e.RequestID = resp.Header.Get("X-Request-Id")
		}
		if e.Status == http.StatusUnauthorized || e.Code == CodeUnauthenticated {
			c.logLimited(ctx, "key_refused", "Wakeshift refused this app's key")
		}
		return e
	}
	if out == nil {
		return nil
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return c.noAnswer(ctx, client, err)
	}
	// A 2xx that isn't Wakeshift's JSON: a proxy's page, or a wrong core.
	if t := bytes.TrimSpace(raw); len(t) == 0 || t[0] != '{' || !json.Valid(t) {
		return &APIError{Status: resp.StatusCode, Code: CodeBadResponse, Message: fmt.Sprintf("Wakeshift at %s answered %d without a JSON object. "+
			"Check that WAKESHIFT_CORE, or the core option, is the address of the console your key came from", c.core, resp.StatusCode)}
	}
	return json.Unmarshal(raw, out)
}

// noAnswer is the error for a request that got no answer: ctx's own error
// if the caller canceled it or its deadline passed (never an APIError),
// else an APIError with Status 0: timeout for the HTTP client's own
// timeout, unreachable for anything else. Either names the core it went
// to: a wrong or missing WAKESHIFT_CORE is the usual cause.
func (c *Client) noAnswer(ctx context.Context, client *http.Client, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("wakeshift: the request to Wakeshift ended: %w", ctxErr)
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return &APIError{Code: CodeTimeout, err: err,
			Message: fmt.Sprintf("Wakeshift at %s didn't answer in time. %s %s", c.core, fixes[CodeTimeout], anchor(CodeTimeout))}
	}
	cause := err
	var ue *url.Error
	if errors.As(err, &ue) {
		cause = ue.Err // without the method and URL, which the message names
	}
	return &APIError{Code: CodeUnreachable, err: err,
		Message: fmt.Sprintf("can't reach Wakeshift at %s (%s). %s %s", c.core, cause, fixes[CodeUnreachable], anchor(CodeUnreachable))}
}

// errorOf is the APIError for an answer with status that isn't a success:
// Wakeshift's code, message and reason where it sent them as strings, else
// the body's first 200 bytes as the message, else the status's own text,
// as every SDK does (errors.json responses).
func errorOf(status int, body []byte) *APIError {
	var e struct {
		Error any `json:"error"`
	}
	json.Unmarshal(body, &e)
	fields, _ := e.Error.(map[string]any)
	str := func(k string) string { s, _ := fields[k].(string); return s }
	msg := str("message")
	if msg == "" {
		msg = string(body[:min(len(body), 200)])
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	return &APIError{Status: status, Code: Code(str("code")), Message: msg, Reason: str("reason"), RequestID: str("request_id")}
}

// Wakeshift runs behind Google's load balancer on Cloud Run. For some
// minutes after a rollout, the load balancer sends a small share of requests
// to a retired revision, which answers at once with Google's own HTML "500
// Server Error" page: the request never reached a Wakeshift instance.
// Retrying is what gets it through.

// retryPauses are the pauses before each retry (the last repeats past
// them).
var retryPauses = []time.Duration{250 * time.Millisecond, time.Second}

func retryPause(attempt int) time.Duration {
	return retryPauses[min(attempt, len(retryPauses)-1)]
}

// do sends req through client, retrying up to maxRetries times a request
// the load balancer refused before it reached Wakeshift (Google's HTML 500
// page, for any request; a 502, 503 or 504 not from Wakeshift for a GET or
// HEAD) and one whose connection failed (not a timeout: its outcome is
// unknown). Wakeshift's own errors (JSON) are never retried. Before a
// retry it drops the SDK's own client's idle connections, never an app's.
func (c *Client) do(client *http.Client, req *http.Request) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		resp, err := client.Do(req)
		if attempt >= c.maxRetries || req.Context().Err() != nil {
			return resp, err
		}
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return resp, err
			}
		} else if !refused(req, resp) {
			return resp, nil
		}
		next, ok := rewound(req)
		if !ok {
			return resp, err
		}
		if resp != nil {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
		}
		if client == defaultClient {
			client.CloseIdleConnections()
		}
		if perr := pause(req.Context(), retryPause(attempt)); perr != nil {
			return nil, perr
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
