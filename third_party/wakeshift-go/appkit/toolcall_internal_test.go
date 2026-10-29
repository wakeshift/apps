package appkit

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type internalIn struct {
	Table string `json:"table"`
	WakeArgs
}

type internalArgs struct {
	Table string `json:"table"`
}

// rollbackApp is an app with one event and a core that records the paths
// it's asked, answering every subscribe as a new subscription.
func rollbackApp(t *testing.T) (*App, *DefinedEvent[internalArgs, internalArgs], *Attachment, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var paths []string
	a, _ := testApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &body)
		mu.Lock()
		p := strings.TrimPrefix(r.URL.Path, "/api/apps/v1")
		if p == "/manifest" {
			mu.Unlock()
			io.WriteString(w, `{}`)
			return
		}
		if body["reason"] == "rolled_back" {
			p += "?rolled_back"
		}
		paths = append(paths, p)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"event_subscription":{"id":"s1","user":"","created":true},"session":"wsc_1","pending":"pa_1","ended":true}`)
	}))
	ev, err := DefineEvent(a, "t.ev", EventDef[internalArgs, internalArgs]{Description: "E.", Text: func(internalArgs, internalArgs) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "1"}, nil)
	at := a.Attach(srv, AttachOptions{})
	return a, ev, at, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), paths...)
	}
}

// A handler that panics after subscribing: Attach's middleware rolls the
// subscription back and panics on; one whose request is cancelled is rolled
// back too (the rollback outlives the request), and neither gets a block.
func TestRollbackOnPanicAndCancel(t *testing.T) {
	_, ev, at, paths := rollbackApp(t)
	run := func(ctx context.Context, h func(context.Context, *ToolCall, internalIn) (*mcp.CallToolResult, any, error)) (mcp.Result, error) {
		wrapped := Events(at, h)
		next := func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/call" {
				return &mcp.ListToolsResult{}, nil // undeclared.go looking up the tool
			}
			res, _, err := wrapped(ctx, req.(*mcp.CallToolRequest), internalIn{Table: "oak"})
			if res == nil && err == nil {
				res = &mcp.CallToolResult{}
			}
			return res, err
		}
		req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "join", Arguments: json.RawMessage(`{"table":"oak"}`)}}
		return at.middleware(next)(ctx, "tools/call", req)
	}
	func() {
		defer func() {
			if r := recover(); r != "a bug" {
				t.Fatalf("the panic wasn't passed on: %v", r)
			}
		}()
		run(context.Background(), func(ctx context.Context, call *ToolCall, in internalIn) (*mcp.CallToolResult, any, error) {
			if _, err := ev.Subscribe(ctx, call, internalArgs{Table: in.Table}); err != nil {
				t.Fatal(err)
			}
			panic("a bug")
		})
	}()
	if got := strings.Join(paths(), ","); got != "/event-subscriptions,/event-subscriptions/unsubscribe?rolled_back" {
		t.Fatalf("after a panic: %s", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	run(ctx, func(ctx context.Context, call *ToolCall, in internalIn) (*mcp.CallToolResult, any, error) {
		if _, err := ev.Subscribe(ctx, call, internalArgs{Table: in.Table}); err != nil {
			t.Fatal(err)
		}
		cancel()
		return &mcp.CallToolResult{}, nil, nil
	})
	if got := strings.Join(paths(), ","); got != "/event-subscriptions,/event-subscriptions/unsubscribe?rolled_back,/event-subscriptions,/event-subscriptions/unsubscribe?rolled_back" {
		t.Fatalf("after a cancel: %s", got)
	}
}

// A handler that returns while a subscribe it started is in flight: the
// call waits for it, and its block covers it.
func TestInFlightSubscribeIsWaitedFor(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	inCore, release := make(chan struct{}), make(chan struct{})
	a, _ := testApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/api/apps/v1")
		if p == "/manifest" {
			io.WriteString(w, `{}`)
			return
		}
		if p == "/event-subscriptions" {
			close(inCore)
			<-release
		}
		mu.Lock()
		paths = append(paths, p)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"event_subscription":{"id":"s1","created":true},"session":"wsc_1","pending":"pa_1"}`)
	}))
	ev, _ := DefineEvent(a, "t.ev", EventDef[internalArgs, internalArgs]{Description: "E.", Text: func(internalArgs, internalArgs) string { return "" }})
	at := a.Attach(mcp.NewServer(&mcp.Implementation{Name: "t", Version: "1"}, nil), AttachOptions{})
	wrapped := Events(at, func(ctx context.Context, call *ToolCall, in internalIn) (*mcp.CallToolResult, any, error) {
		go ev.Subscribe(ctx, call, internalArgs{Table: in.Table})
		<-inCore
		go func() { time.Sleep(50 * time.Millisecond); close(release) }()
		return &mcp.CallToolResult{}, nil, nil // returns with the subscribe in flight
	})
	next := func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if method != "tools/call" {
			return &mcp.ListToolsResult{}, nil // undeclared.go looking up the tool
		}
		res, _, err := wrapped(ctx, req.(*mcp.CallToolRequest), internalIn{Table: "oak"})
		return res, err
	}
	req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "join", Arguments: json.RawMessage(`{"table":"oak"}`)}}
	if _, err := at.middleware(next)(context.Background(), "tools/call", req); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(paths, ","); got != "/event-subscriptions,/calls/addition" {
		t.Fatalf("the block didn't wait for the subscribe: %s", got)
	}
}

// takeTrailer takes a beacon line (the core's beacon.Text) off the end of
// a result's text, and nothing else.
func TestTakeTrailer(t *testing.T) {
	line := beaconLinePrefix + "aaaaaaaaaa.bbbbbbbbbb.cccccccccc)"
	res := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "a\n\n" + line + "\n"}}}
	if got := takeTrailer(res); got != line || res.Content[0].(*mcp.TextContent).Text != "a" {
		t.Fatalf("takeTrailer: %q %q", got, res.Content[0].(*mcp.TextContent).Text)
	}
	res = &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: line + " and more"}}}
	if got := takeTrailer(res); got != "" {
		t.Fatalf("not a trailer: %q", got)
	}
}
