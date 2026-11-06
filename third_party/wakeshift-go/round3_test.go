package wakeshift_test

// Round-3 gate fixes (FIXPASS-SDK.md): the subscribing call's context in
// OnSubscribe (A1), CodeOf (A2), plain registration (B1), Close (B2), the
// Monitor line budget (B3), the identify log split (B5), and built-ins
// without execution (B10).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/wakeshift-go"
)

func TestCodeOf(t *testing.T) {
	api := &wakeshift.APIError{Status: 429, Code: wakeshift.CodeRateLimited, Message: "slow down"}
	cases := []struct {
		err  error
		want wakeshift.Code
	}{
		{api, wakeshift.CodeRateLimited},
		{fmt.Errorf("emit: %w", api), wakeshift.CodeRateLimited},
		{&wakeshift.UsageError{Code: wakeshift.CodeBadArguments, Message: "x"}, wakeshift.CodeBadArguments},
		{errors.New("plain"), ""},
		{nil, ""},
	}
	for _, c := range cases {
		if got := wakeshift.CodeOf(c.err); got != c.want {
			t.Errorf("CodeOf(%v) = %q, want %q", c.err, got, c.want)
		}
		if c.want != "" && !wakeshift.IsCode(c.err, wakeshift.CodeOf(c.err)) {
			t.Errorf("IsCode disagrees with CodeOf for %v", c.err)
		}
	}
	// Against a real refusal.
	core := newPathCore()
	core.answer("/events/emit", func(w http.ResponseWriter, _ map[string]any) {
		w.WriteHeader(401)
		io.WriteString(w, `{"error": {"code": "unauthenticated", "message": "no"}}`)
	})
	cs := httptest.NewServer(core)
	t.Cleanup(cs.Close)
	var quiet logLines
	wake := testClient(t, cs.URL, wakeshift.WithLogger(quiet.logger()))
	ev, _ := wakeshift.DefineEvent(wake, "table.news", wakeshift.EventDef[tableArgs, turnMsg]{Description: "News", Public: true})
	if _, err := ev.Emit(context.Background(), tableArgs{Table: "oak"}, turnMsg{}); wakeshift.CodeOf(err) != wakeshift.CodeUnauthenticated {
		t.Errorf("CodeOf(%v) = %q", err, wakeshift.CodeOf(err))
	}
}

type regionKey struct{}

// subscribedRecord is what an OnSubscribe saw.
type subscribedRecord struct {
	s      wakeshift.Subscribed[tableArgs]
	region any
	ctxErr error
}

// The tool path: OnSubscribe gets the Context the subscribing call passed,
// and runs with the values of the ctx that Subscribe was given, not
// canceled with the request. The Context reaches neither Wakeshift nor a
// log line.
func TestSubscribeContextReachesOnSubscribe(t *testing.T) {
	var logs logLines
	got := make(chan subscribedRecord, 4)
	var watched *wakeshift.Event[tableArgs, turnMsg]
	w := newWrapped(t, wakeshift.AttachOptions{}, wakeshift.WithLogger(logs.logger()), func(_ *wakeshift.AttachOptions, w *wrapped) {
		var err error
		watched, err = wakeshift.DefineEvent(w.app, "crawl.finished", wakeshift.EventDef[tableArgs, turnMsg]{Description: "A crawl finished",
			OnSubscribe: func(ctx context.Context, s wakeshift.Subscribed[tableArgs]) error {
				time.Sleep(20 * time.Millisecond) // past the request's end
				got <- subscribedRecord{s: s, region: ctx.Value(regionKey{}), ctxErr: ctx.Err()}
				return nil
			}})
		if err != nil {
			t.Fatal(err)
		}
	})
	type secret struct{ token string }
	wakeshift.AddTool(w.at, &mcp.Tool{Name: "start_crawl"}, handler[tableIn](func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) error {
		ctx = context.WithValue(ctx, regionKey{}, "eu-1")
		_, err := watched.Subscribe(ctx, call, tableArgs{Table: in.Table}, wakeshift.Context(secret{"context-canary"}))
		return err
	}), wakeshift.Subscribes(watched))
	w.callTool(t, "ada", "", "start_crawl", map[string]any{"table": "j1"}, nil)
	select {
	case r := <-got:
		if r.s.Via != "tool" || r.s.Tool != "start_crawl" || r.s.User != "ada" || r.s.Args.Table != "j1" {
			t.Errorf("subscribed: %+v", r.s)
		}
		if c, ok := r.s.Context.(secret); !ok || c.token != "context-canary" {
			t.Errorf("Context = %#v", r.s.Context)
		}
		if r.region != "eu-1" || r.ctxErr != nil {
			t.Errorf("ctx: region %v, err %v", r.region, r.ctxErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OnSubscribe didn't run")
	}
	for _, b := range w.core.bodies("/event-subscriptions") {
		if strings.Contains(fmt.Sprint(b), "context-canary") {
			t.Errorf("the Context reached Wakeshift: %v", b)
		}
	}
	if strings.Contains(logs.String(), "context-canary") {
		t.Errorf("the Context was logged: %s", logs.String())
	}
	// A Subscribe without Context: nil.
	wakeshift.AddTool(w.at, &mcp.Tool{Name: "start_plain"}, handler[tableIn](func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) error {
		_, err := watched.Subscribe(ctx, call, tableArgs{Table: in.Table})
		return err
	}), wakeshift.Subscribes(watched))
	w.callTool(t, "ada", "", "start_plain", map[string]any{"table": "j2"}, nil)
	select {
	case r := <-got:
		if r.s.Context != nil || r.s.Args.Table != "j2" {
			t.Errorf("without Context: %+v", r.s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OnSubscribe didn't run")
	}
}

// B1: a tool added without Subscribes is a plain registration, with a key
// or without one: it never panics, and with a key it takes session_key
// for Unsubscribe, as Subscribes() with no events does.
func TestAddToolWithoutSubscribes(t *testing.T) {
	for _, key := range []string{"", testKey} {
		t.Run(fmt.Sprintf("key=%t", key != ""), func(t *testing.T) {
			core := newPathCore()
			var unsubSession any
			core.answer("/event-subscriptions/unsubscribe", func(w http.ResponseWriter, b map[string]any) {
				unsubSession = b["call"].(map[string]any)["session"]
				io.WriteString(w, `{"ended": true}`)
			})
			cs := httptest.NewServer(core)
			t.Cleanup(cs.Close)
			wake, _ := wakeshift.New(wakeshift.WithKey(key), wakeshift.WithCore(cs.URL), wakeshift.WithMaxRetries(0))
			ev, _ := wakeshift.DefineEvent(wake, "crawl.finished", wakeshift.EventDef[tableArgs, turnMsg]{Description: "A crawl finished"})
			srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "1"}, nil)
			at := wake.Attach(srv, wakeshift.AttachOptions{})
			var ended bool
			var unsubErr error
			wakeshift.AddTool(at, &mcp.Tool{Name: "unwatch", Description: "Stop watching."}, handler[tableIn](func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) error {
				ended, unsubErr = ev.Unsubscribe(ctx, call, tableArgs{Table: in.Table})
				return unsubErr
			}))
			hs := httptest.NewServer(at.Handler(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})))
			t.Cleanup(hs.Close)
			list := fmt.Sprint(post(t, hs.URL, "claude-code/2.1", "tools/list", map[string]any{}))
			if strings.Contains(list, "notifies you") || strings.Contains(list, "harness") {
				t.Errorf("listed as a tool that subscribes: %s", list)
			}
			if key != "" && !strings.Contains(list, "session_key") {
				t.Errorf("with a key, no session_key: %s", list)
			}
			args := map[string]any{"table": "oak"}
			if key != "" {
				args["session_key"] = "wsc_x"
			}
			res := post(t, hs.URL, "claude-code/2.1", "tools/call", map[string]any{"name": "unwatch", "arguments": args})
			if res["error"] != nil || unsubErr != nil {
				t.Fatalf("%v %v", res, unsubErr)
			}
			if key != "" && (!ended || unsubSession != "wsc_x") {
				t.Errorf("with a key, Unsubscribe: ended %v, session %v", ended, unsubSession)
			}
		})
	}
}

// A registration mistake that panics is logged too (config): a framework
// may turn the panic into a bare error.
func TestRegistrationMistakeIsLogged(t *testing.T) {
	var logs logLines
	wake := testClient(t, "https://core.example", wakeshift.WithLogger(logs.logger()))
	wakeshift.DefineEvent(wake, "table.news", wakeshift.EventDef[tableArgs, turnMsg]{Description: "News", Public: true})
	at := wake.Attach(mcp.NewServer(&mcp.Implementation{Name: "t", Version: "1"}, nil), wakeshift.AttachOptions{})
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("no panic")
			}
		}()
		wakeshift.AddTool(at, &mcp.Tool{Name: "list_subscriptions"}, handler[tableIn](nil))
	}()
	if c := logs.codes(); len(c) != 1 || c[0] != "config" || !strings.Contains(logs.String(), "list_subscriptions is one of Wakeshift's own tools") {
		t.Errorf("logged %v: %s", c, logs.String())
	}
}

// B2: Close stops every running Watch and waits for it, cancels the
// OnSubscribe goroutines' ctx and waits for them; after it, Watch returns
// at once. No goroutine of the SDK's outlives it.
func TestCloseStopsWatchesAndWaits(t *testing.T) {
	defer wakeshift.SetWatchHooks(time.Millisecond, nil)()
	before := runtime.NumGoroutine()
	core := newPathCore()
	cs := httptest.NewServer(core)
	defer cs.Close()
	wake := testClient(t, cs.URL)
	ev, _ := wakeshift.DefineEvent(wake, "table.news", wakeshift.EventDef[tableArgs, turnMsg]{Description: "News", Public: true})
	checking := make(chan struct{}, 100)
	var returned sync.WaitGroup
	var watchErr error
	returned.Add(1)
	go func() {
		defer returned.Done()
		watchErr = ev.Watch(context.Background(), tableArgs{Table: "oak"}, func(ctx context.Context) (turnMsg, bool, error) {
			checking <- struct{}{}
			return turnMsg{}, false, nil
		}, wakeshift.Every(time.Hour))
	}()
	<-checking // the first check ran; the watch now waits an hour
	closed := make(chan struct{})
	go func() { wake.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close didn't return")
	}
	// Close returned only once the Watch had: it's done now.
	done := make(chan struct{})
	go func() { returned.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the watch outlived Close")
	}
	if !errors.Is(watchErr, context.Canceled) {
		t.Errorf("watch returned %v", watchErr)
	}
	if err := ev.Watch(context.Background(), tableArgs{Table: "elm"}, func(context.Context) (turnMsg, bool, error) {
		t.Error("a watch checked after Close")
		return turnMsg{}, true, nil
	}); err != nil {
		t.Errorf("after Close: %v", err)
	}
	if err := wake.Close(); err != nil {
		t.Errorf("a second Close: %v", err)
	}
	cs.Close()
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before+2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before+2 {
		t.Errorf("%d goroutines after Close, %d before", n, before)
	}
}

// Close cancels an OnSubscribe's ctx and waits for it to return.
func TestCloseWaitsForOnSubscribe(t *testing.T) {
	started, finished := make(chan struct{}), make(chan struct{})
	var w *wrapped
	var ev *wakeshift.Event[tableArgs, turnMsg]
	w = newWrapped(t, wakeshift.AttachOptions{}, func(_ *wakeshift.AttachOptions, w *wrapped) {
		ev, _ = wakeshift.DefineEvent(w.app, "crawl.finished", wakeshift.EventDef[tableArgs, turnMsg]{Description: "A crawl finished",
			OnSubscribe: func(ctx context.Context, _ wakeshift.Subscribed[tableArgs]) error {
				close(started)
				<-ctx.Done()
				time.Sleep(20 * time.Millisecond)
				close(finished)
				return nil
			}})
	})
	wakeshift.AddTool(w.at, &mcp.Tool{Name: "start_crawl"}, handler[tableIn](func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) error {
		_, err := ev.Subscribe(ctx, call, tableArgs{Table: in.Table})
		return err
	}), wakeshift.Subscribes(ev))
	w.callTool(t, "ada", "", "start_crawl", map[string]any{"table": "j1"}, nil)
	<-started
	w.app.Close()
	select {
	case <-finished:
	default:
		t.Fatal("Close returned before OnSubscribe did")
	}
}

// B10: Wakeshift's own tools carry no execution in tools/list.
func TestBuiltinsCarryNoExecution(t *testing.T) {
	w := newWrapped(t, wakeshift.AttachOptions{BuiltinTools: wakeshift.BuiltinTools{Wait: true}})
	res := post(t, w.url, "claude-code/2.1", "tools/list", map[string]any{}, "Authorization", "Bearer ada")
	if s := fmt.Sprint(res); !strings.Contains(s, "subscribe_events") || strings.Contains(s, "execution") {
		t.Errorf("tools/list: %s", s)
	}
}

// Close ends a check in flight (its ctx), and waits for it.
func TestCloseEndsACheckInFlight(t *testing.T) {
	defer wakeshift.SetWatchHooks(time.Millisecond, nil)()
	wake := testClient(t, "https://core.example")
	ev, _ := wakeshift.DefineEvent(wake, "table.news", wakeshift.EventDef[tableArgs, turnMsg]{Description: "News", Public: true})
	checking, returned := make(chan struct{}), make(chan error, 1)
	go func() {
		returned <- ev.Watch(context.Background(), tableArgs{Table: "oak"}, func(ctx context.Context) (turnMsg, bool, error) {
			close(checking)
			<-ctx.Done()
			return turnMsg{}, false, ctx.Err()
		})
	}()
	<-checking
	closed := make(chan struct{})
	go func() { wake.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close didn't end the check")
	}
	select {
	case err := <-returned:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("watch: %v", err)
		}
	default:
		t.Fatal("Close returned before the watch")
	}
}
