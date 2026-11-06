package wakeshift

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/wakeshift-go/internal/mcpevents/event"
)

// errors.json is how every SDK reads Wakeshift's answers: which refusals
// it retries, the error each answer becomes, and the JSON-RPC error each
// becomes for an MCP Events client. The TypeScript and Python SDKs test
// against the same file.
type goldenAnswer struct {
	Name        string `json:"name"`
	Method      string `json:"method"`
	Status      int    `json:"status"`
	ContentType string `json:"content_type"`
	Body        string `json:"body"`
}

type errorsGolden struct {
	Retry struct {
		PausesMS []int `json:"pauses_ms"`
		Cases    []struct {
			goldenAnswer
			Retried bool `json:"retried"`
		} `json:"cases"`
	} `json:"retry"`
	Responses []struct {
		goldenAnswer
		Want struct {
			Status  int    `json:"status"`
			Code    Code   `json:"code"`
			Message string `json:"message"`
			Reason  string `json:"reason"`
			Text    string `json:"text"`
		} `json:"want"`
	} `json:"responses"`
	BadResponse struct {
		Code    Code           `json:"code"`
		Message string         `json:"message"`
		Cases   []goldenAnswer `json:"cases"`
	} `json:"bad_response"`
	Unreachable struct {
		Status        int    `json:"status"`
		Code          Code   `json:"code"`
		MessagePrefix string `json:"message_prefix"`
		MessageSuffix string `json:"message_suffix"`
	} `json:"unreachable"`
	RPC []struct {
		Name  string `json:"name"`
		Error *struct {
			Code    Code   `json:"code"`
			Message string `json:"message"`
			Reason  string `json:"reason"`
		} `json:"error"`
		Want struct {
			Code    map[string]int    `json:"code"`
			Message string            `json:"message"`
			Data    map[string]string `json:"data"`
		} `json:"want"`
	} `json:"rpc"`
}

func readErrorsGolden(t *testing.T) errorsGolden {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(goldenDir, "errors.json"))
	if os.IsNotExist(err) {
		t.Skip("no shared golden files here")
	}
	if err != nil {
		t.Fatal(err)
	}
	var g errorsGolden
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

func TestRetryGolden(t *testing.T) {
	g := readErrorsGolden(t)
	var pauses []int
	for _, p := range retryPauses {
		pauses = append(pauses, int(p/time.Millisecond))
	}
	if !reflect.DeepEqual(pauses, g.Retry.PausesMS) {
		t.Errorf("pauses %v, want %v", pauses, g.Retry.PausesMS)
	}
	for _, c := range g.Retry.Cases {
		req, _ := http.NewRequest(c.Method, "http://core.test/x", nil)
		resp := &http.Response{StatusCode: c.Status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(c.Body))}
		if c.ContentType != "" {
			resp.Header.Set("Content-Type", c.ContentType)
		}
		if got := refused(req, resp); got != c.Retried {
			t.Errorf("%s: retried %v, want %v", c.Name, got, c.Retried)
		}
		if rest, _ := io.ReadAll(resp.Body); string(rest) != c.Body {
			t.Errorf("%s: the body no longer reads whole", c.Name)
		}
	}
}

func TestResponseErrorsGolden(t *testing.T) {
	g := readErrorsGolden(t)
	answer := func(status int, ctype, body string) *Client {
		a, _ := testApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ctype != "" {
				w.Header().Set("Content-Type", ctype)
			} else {
				w.Header()["Content-Type"] = nil
			}
			w.WriteHeader(status)
			io.WriteString(w, body)
		}))
		return a
	}
	for _, c := range g.Responses {
		var out struct{}
		err := answer(c.Status, c.ContentType, c.Body).call(context.Background(), "POST", "/api/apps/v1/x", "t", map[string]any{}, &out)
		var e *APIError
		if !errors.As(err, &e) {
			t.Errorf("%s: %v, want an APIError", c.Name, err)
			continue
		}
		w := c.Want
		if e.Status != w.Status || e.Code != w.Code || e.Message != w.Message || e.Reason != w.Reason || e.Error() != w.Text {
			t.Errorf("%s:\n got %d %q %q %q %q\nwant %d %q %q %q %q", c.Name, e.Status, e.Code, e.Message, e.Reason, e.Error(), w.Status, w.Code, w.Message, w.Reason, w.Text)
		}
	}
	for _, c := range g.BadResponse.Cases {
		a := answer(c.Status, c.ContentType, c.Body)
		var out struct{}
		err := a.call(context.Background(), "GET", "/api/apps/v1/x", "t", nil, &out)
		var e *APIError
		want := strings.NewReplacer("{core}", a.core, "{status}", strconv.Itoa(c.Status)).Replace(g.BadResponse.Message)
		if !errors.As(err, &e) || e.Status != c.Status || e.Code != g.BadResponse.Code || e.Message != want {
			t.Errorf("%s: %v, want %s (%d): %s", c.Name, err, g.BadResponse.Code, c.Status, want)
		}
	}
}

func TestUnreachableGolden(t *testing.T) {
	g := readErrorsGolden(t).Unreachable
	check := func(name string, a *Client, wantWhy string) {
		t.Helper()
		err := a.call(context.Background(), "POST", "/api/apps/v1/x", "t", map[string]any{}, nil)
		var e *APIError
		if !errors.As(err, &e) {
			t.Fatalf("%s: %v, want an APIError", name, err)
		}
		prefix := strings.ReplaceAll(g.MessagePrefix, "{core}", a.core)
		if e.Status != g.Status || e.Code != g.Code || !strings.HasPrefix(e.Message, prefix) || !strings.Contains(e.Message, g.MessageSuffix) ||
			!strings.HasSuffix(e.Message, anchor(CodeUnreachable)) ||
			!strings.Contains(e.Message, wantWhy) || e.Error() != "wakeshift: "+e.Message || !IsCode(err, "unreachable") || errors.Unwrap(e) == nil {
			t.Errorf("%s: %d %q %q (%v), want %d %q %q…%q with %q", name, e.Status, e.Code, e.Message, e.Error(), g.Status, g.Code, prefix, g.MessageSuffix, wantWhy)
		}
	}

	// Nothing listening.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	check("refused", clientAt(t, "http://"+addr, WithHTTPClient(&http.Client{}), WithMaxRetries(0)), "connection refused")

	// No answer within the client's timeout: timeout, not unreachable.
	stall := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-stall }))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(stall) })
	a := clientAt(t, srv.URL, WithHTTPClient(&http.Client{Timeout: 50 * time.Millisecond}))
	err = a.call(context.Background(), "POST", "/api/apps/v1/x", "t", map[string]any{}, nil)
	var e *APIError
	if !errors.As(err, &e) || e.Status != 0 || e.Code != CodeTimeout || !IsCode(err, CodeTimeout) || errors.Unwrap(e) == nil ||
		!strings.Contains(e.Error(), "didn't answer in time") || !strings.Contains(e.Error(), fixes[CodeTimeout]) || !strings.HasSuffix(e.Error(), anchor(CodeTimeout)) {
		t.Errorf("timeout: %v", err)
	}

	// A caller's own cancellation is never an APIError.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = clientAt(t, srv.URL).call(ctx, "POST", "/api/apps/v1/x", "t", map[string]any{}, nil)
	if !errors.Is(err, context.Canceled) || errors.As(err, &e) {
		t.Errorf("canceled: %v, want context.Canceled and no APIError", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err = clientAt(t, srv.URL).call(ctx, "POST", "/api/apps/v1/x", "t", map[string]any{}, nil)
	if !errors.Is(err, context.DeadlineExceeded) || errors.As(err, &e) {
		t.Errorf("deadline: %v, want context.DeadlineExceeded and no APIError", err)
	}
}

func TestRPCErrorsGolden(t *testing.T) {
	g := readErrorsGolden(t)
	for _, c := range g.RPC {
		var err error = errors.New("dial tcp: connection refused")
		if c.Error != nil {
			err = &APIError{Status: 400, Code: c.Error.Code, Message: c.Error.Message, Reason: c.Error.Reason}
		}
		for _, dialect := range []string{event.DialectDraft, event.DialectChatGPT} {
			code, msg, data := rpcErrorOf(err, event.CodesFor(dialect), "")
			got, _ := data.(map[string]string)
			if code != c.Want.Code[dialect] || msg != c.Want.Message || !reflect.DeepEqual(got, c.Want.Data) {
				t.Errorf("%s (%s): %d %q %v, want %d %q %v", c.Name, dialect, code, msg, data, c.Want.Code[dialect], c.Want.Message, c.Want.Data)
			}
		}
	}
}

// errors.json codes is the one list of codes: Go's constants and fixes
// list exactly them, in its order, each with its fix; the SDK's own
// messages say what failed, the fix, and the code's anchor.
func TestErrorCodesGolden(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(goldenDir, "errors.json"))
	if os.IsNotExist(err) {
		t.Skip("no shared golden files here")
	}
	var g struct {
		Codes []struct {
			Code  Code `json:"code"`
			Class struct {
				BeforeSending *string `json:"before_sending"`
				FromCore      *string `json:"from_core"`
			} `json:"class"`
			Fix    string `json:"fix"`
			Anchor string `json:"anchor"`
		} `json:"codes"`
		Messages struct {
			Format string `json:"format"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	all := []Code{CodeBadRequest, CodeBadArguments, CodeBadPayload, CodeUnknownEvent, CodeUnknownSession, CodeUnknownConversation,
		CodeOtherClient, CodePrivateTopic, CodeSubscriptionLimit, CodeSubscriptionStopped, CodeIDReused, CodeRateLimited, CodeTooLarge,
		CodeUnauthenticated, CodeNoGrant, CodeUnknownWorkspace, CodeUnknownPublishURL, CodeBadSigningSecret, CodeUnsupported,
		CodeInternal, CodeBadResponse, CodeCallbackFailed, CodeUnreachable, CodeTimeout, CodeConfig, CodeDisabled,
		CodeUndeclaredSubscribe, CodeNoCall, CodeCallEnded, CodeIdentifyFailed, CodeUserRequired}
	if len(all) != len(g.Codes) || len(fixes) != len(g.Codes) {
		t.Fatalf("%d constants and %d fixes, want %d codes", len(all), len(fixes), len(g.Codes))
	}
	for i, c := range g.Codes {
		if all[i] != c.Code {
			t.Errorf("code %d: %q, want %q", i, all[i], c.Code)
		}
		if fixes[c.Code] != c.Fix {
			t.Errorf("%s: fix %q, want %q", c.Code, fixes[c.Code], c.Fix)
		}
		if anchor(c.Code) != c.Anchor {
			t.Errorf("%s: anchor %q, want %q", c.Code, anchor(c.Code), c.Anchor)
		}
		if c.Class.BeforeSending != nil && *c.Class.BeforeSending == "UsageError" {
			u := usage(c.Code, "something failed")
			want := strings.NewReplacer("{what}", "something failed", "{fix}", c.Fix, "{anchor}", c.Anchor).Replace(g.Messages.Format)
			if u.Error() != want || !IsCode(u, c.Code) {
				t.Errorf("%s: %q, want %q", c.Code, u.Error(), want)
			}
		}
	}
	if IsCode(errors.New("x"), CodeConfig) || IsCode(nil, CodeConfig) {
		t.Error("IsCode matched an error that isn't the SDK's")
	}
	if !IsCode(fmt.Errorf("wrapped: %w", &APIError{Code: CodeIDReused}), CodeIDReused) {
		t.Error("IsCode didn't see through wrapping")
	}
}
