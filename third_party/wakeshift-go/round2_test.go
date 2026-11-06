package wakeshift_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/wakeshift-go"
)

// noUser is an Identify that names no one (a server without sign-in).
func noUser(context.Context, wakeshift.Request) (wakeshift.Identity, error) {
	return wakeshift.Identity{}, nil
}

// Without a user, list_subscriptions lists the calling conversation's
// subscriptions and unsubscribe_events ends one by id: both name the call
// (its session_key), never a user. With one, both name the user.
func TestListAndEndByConversation(t *testing.T) {
	w := newWrapped(t, wakeshift.AttachOptions{Identify: noUser})
	res := w.callTool(t, "anon", "", "list_subscriptions", map[string]any{"session_key": "wsc_mine"}, nil)
	if res.IsError {
		t.Fatalf("list_subscriptions: %+v", res)
	}
	b := w.core.bodies("/event-subscriptions/list")
	if len(b) != 1 || b[0]["user"] != nil {
		t.Fatalf("list: %v", b)
	}
	if c, _ := b[0]["call"].(map[string]any); c["session"] != "wsc_mine" || c["tool"] != "list_subscriptions" {
		t.Fatalf("list's call: %v", b[0])
	}
	if res := w.callTool(t, "anon", "", "unsubscribe_events", map[string]any{"id": "sub-1", "session_key": "wsc_mine"}, nil); res.IsError {
		t.Fatalf("unsubscribe_events by id: %+v", res)
	}
	e := w.core.bodies("/event-subscriptions/end")
	if c, _ := e[0]["call"].(map[string]any); len(e) != 1 || e[0]["id"] != "sub-1" || e[0]["user"] != nil || c["session"] != "wsc_mine" {
		t.Fatalf("end: %v", e)
	}

	u := newWrapped(t, wakeshift.AttachOptions{})
	u.callTool(t, "ada", "", "list_subscriptions", map[string]any{}, nil)
	if b := u.core.bodies("/event-subscriptions/list"); len(b) != 1 || b[0]["user"] != "ada" || b[0]["call"] != nil {
		t.Fatalf("list with a user: %v", b)
	}
}

// A refused caller's built-in result is a neutral line for the agent, never
// a developer's instructions.
func TestRefusedBuiltinTextIsNeutral(t *testing.T) {
	w := newWrapped(t, wakeshift.AttachOptions{Identify: noUser, RequireUser: true})
	res := w.callTool(t, "anon", "", "list_subscriptions", map[string]any{}, nil)
	if got := strings.Join(res.texts(), ""); !res.IsError || got != "This app needs you to sign in before it can subscribe you to anything." {
		t.Fatalf("%+v", res)
	}
	f := newWrapped(t, wakeshift.AttachOptions{Identify: func(context.Context, wakeshift.Request) (wakeshift.Identity, error) {
		return wakeshift.Identity{}, errors.New("down")
	}})
	res = f.callTool(t, "ada", "", "list_subscriptions", map[string]any{}, nil)
	if got := strings.Join(res.texts(), ""); !res.IsError || got != "This app couldn't verify who you are, so it can't subscribe you to anything now." {
		t.Fatalf("%+v", res)
	}
}

// A call that subscribes and then unsubscribes the same subscription gets
// no block: nothing is left to wake the conversation for.
func TestSubscribeThenUnsubscribeLeavesNoBlock(t *testing.T) {
	w := newWrapped(t, wakeshift.AttachOptions{})
	wakeshift.AddTool(w.at, &mcp.Tool{Name: "watch"}, func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) (*mcp.CallToolResult, any, error) {
		if _, err := w.shared.Subscribe(ctx, call, tableArgs{Table: in.Table}); err != nil {
			return nil, nil, err
		}
		if _, err := w.shared.Unsubscribe(ctx, call, tableArgs{Table: in.Table}); err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "mine"}}}, nil, nil
	}, wakeshift.Subscribes(w.shared))
	res := w.callTool(t, "ada", "", "watch", map[string]any{"table": "oak"}, nil)
	if res.IsError || strings.Join(res.texts(), "|") != "mine" || len(w.core.bodies("/calls/addition")) != 0 {
		t.Fatalf("%+v, additions %v", res, w.core.bodies("/calls/addition"))
	}
	// One that keeps another subscription still gets its block.
	wakeshift.AddTool(w.at, &mcp.Tool{Name: "watch2"}, func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) (*mcp.CallToolResult, any, error) {
		w.shared.Subscribe(ctx, call, tableArgs{Table: "a"})
		w.shared.Subscribe(ctx, call, tableArgs{Table: "b"})
		w.shared.Unsubscribe(ctx, call, tableArgs{Table: "a"})
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "mine"}}}, nil, nil
	}, wakeshift.Subscribes(w.shared))
	if res := w.callTool(t, "ada", "", "watch2", map[string]any{"table": "oak"}, nil); len(res.texts()) < 2 {
		t.Fatalf("no block: %+v", res)
	}
}

// Session is empty until Wakeshift resolved the conversation: a forged
// session_key never shows through it.
func TestSessionIsTheResolvedOne(t *testing.T) {
	w := newWrapped(t, wakeshift.AttachOptions{})
	var before, after wakeshift.SessionKey
	wakeshift.AddTool(w.at, &mcp.Tool{Name: "watch"}, func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) (*mcp.CallToolResult, any, error) {
		before = call.Session()
		_, err := w.shared.Subscribe(ctx, call, tableArgs{Table: in.Table})
		after = call.Session()
		return nil, nil, err
	}, wakeshift.Subscribes(w.shared))
	w.callTool(t, "ada", "", "watch", map[string]any{"table": "oak", "session_key": "wsc_forgedforgedforged"}, nil)
	if before != "" || after != "wsc_s1" {
		t.Fatalf("before %q, after %q", string(before), string(after))
	}
	if c := w.core.bodies("/event-subscriptions")[0]["call"].(map[string]any); c["session"] != "wsc_forgedforgedforged" {
		t.Fatalf("the core wasn't asked about the passed key: %v", c)
	}
}

// OnSubscribe runs once per new subscription, on every path, never for a
// renewal or a failed call; an error is logged and the subscription stands.
func TestOnSubscribe(t *testing.T) {
	type got struct{ via, tool, user, table string }
	var mu sync.Mutex
	var calls []got
	fired := make(chan struct{}, 10)
	var logs logLines
	fail := false
	w := newWrapped(t, wakeshift.AttachOptions{}, wakeshift.WithLogger(logs.logger()), func(_ *wakeshift.AttachOptions, w *wrapped) {})
	// Its own event, so OnSubscribe is set at DefineEvent.
	ev, err := wakeshift.DefineEvent(w.app, "deploy.done", wakeshift.EventDef[tableArgs, turnMsg]{Description: "A deploy finished.", Public: true,
		OnSubscribe: func(ctx context.Context, s wakeshift.Subscribed[tableArgs]) error {
			defer func() { fired <- struct{}{} }()
			if ctx.Err() != nil {
				t.Error("OnSubscribe's ctx is canceled")
			}
			mu.Lock()
			calls = append(calls, got{s.Via, s.Tool, s.User, s.Args.Table})
			mu.Unlock()
			if fail {
				return errors.New("boom-canary")
			}
			return nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	wakeshift.AddTool(w.at, &mcp.Tool{Name: "deploy"}, func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) (*mcp.CallToolResult, any, error) {
		_, err := ev.Subscribe(ctx, call, tableArgs{Table: in.Table})
		if in.Table == "fails" {
			return nil, nil, errors.New("the deploy failed")
		}
		return nil, nil, err
	}, wakeshift.Subscribes(ev))
	wait := func(n int) {
		t.Helper()
		for range n {
			select {
			case <-fired:
			case <-time.After(2 * time.Second):
				t.Fatal("OnSubscribe didn't run")
			}
		}
		select {
		case <-fired:
			t.Fatal("OnSubscribe ran once more")
		case <-time.After(50 * time.Millisecond):
		}
	}
	w.callTool(t, "ada", "", "deploy", map[string]any{"table": "prod"}, nil)
	wait(1)
	w.callTool(t, "ada", "", "deploy", map[string]any{"table": "prod"}, nil)  // renewed
	w.callTool(t, "ada", "", "deploy", map[string]any{"table": "fails"}, nil) // rolled back
	wait(0)
	w.callTool(t, "ada", "", "subscribe_events", map[string]any{"event": "deploy.done", "arguments": map[string]any{"table": "staging"}}, nil)
	wait(1)
	fail = true
	post(t, w.url, "some-draft-client/1", "events/subscribe", map[string]any{"name": "deploy.done", "arguments": map[string]any{"table": "dev"},
		"delivery": map[string]any{"mode": "webhook", "url": "https://hook.example/x"}}, "Authorization", "Bearer bob")
	wait(1)
	want := []got{{"tool", "deploy", "ada", "prod"}, {"subscribe_events", "", "ada", "staging"}, {"native", "", "bob", "dev"}}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(calls, want) {
		t.Fatalf("calls %v, want %v", calls, want)
	}
	time.Sleep(20 * time.Millisecond)
	if l := logs.String(); !strings.Contains(l, `"code":"on_subscribe_failed"`) || strings.Contains(l, "boom-canary") {
		t.Errorf("logs: %s", l)
	}
}

// A Watch started in a tool call is skipped when the call's Subscribe came
// back unavailable, and stopped when the call fails.
func TestWatchInAToolCall(t *testing.T) {
	fastWatch(t)
	w := newWrapped(t, wakeshift.AttachOptions{})
	ev, err := wakeshift.DefineEvent(w.app, "job.done", wakeshift.EventDef[tableArgs, turnMsg]{Description: "A job finished."})
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 4)
	var checks sync.Map
	check := func(table string) func(context.Context) (turnMsg, bool, error) {
		return func(context.Context) (turnMsg, bool, error) { checks.Store(table, true); return turnMsg{}, false, nil }
	}
	wakeshift.AddTool(w.at, &mcp.Tool{Name: "job"}, func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) (*mcp.CallToolResult, any, error) {
		if _, err := ev.Subscribe(ctx, call, tableArgs{Table: in.Table}); err != nil {
			return nil, nil, err
		}
		go func() {
			results <- ev.Watch(context.WithoutCancel(ctx), tableArgs{Table: in.Table}, check(in.Table), wakeshift.Every(time.Second))
		}()
		time.Sleep(20 * time.Millisecond)
		if in.Table == "fails" {
			return nil, nil, errors.New("the job didn't start")
		}
		return nil, nil, nil
	}, wakeshift.Subscribes(ev))
	// ChatGPT (native), and the event is tools-only: unavailable, so no watch.
	w.callTool(t, "ada", "openai-mcp/1.0.0", "job", map[string]any{"table": "native"}, nil)
	if err := <-results; err != nil {
		t.Fatalf("unavailable: %v", err)
	}
	if _, ok := checks.Load("native"); ok {
		t.Error("checked for an unavailable subscription")
	}
	w.callTool(t, "ada", "", "job", map[string]any{"table": "fails"}, nil)
	select {
	case err := <-results:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("after the call failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the watch of a failed call kept running")
	}
}

func TestBuiltinToolNames(t *testing.T) {
	got := wakeshift.BuiltinToolNames()
	if !slices.Equal(got, []string{"subscribe_events", "unsubscribe_events", "list_subscriptions", "wakeshift_wait"}) {
		t.Fatal(got)
	}
	got[0] = "x"
	if wakeshift.BuiltinToolNames()[0] != "subscribe_events" {
		t.Fatal("shared slice")
	}
}
