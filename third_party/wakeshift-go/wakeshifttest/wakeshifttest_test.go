package wakeshifttest_test

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	wakeshift "github.com/wakeshift/wakeshift-go"
	"github.com/wakeshift/wakeshift-go/wakeshifttest"
)

type jobArgs struct {
	Job string `json:"job"`
}

type crawl struct {
	Status string `json:"status"`
}

type startIn struct {
	URL string `json:"url"`
}

func TestNewToolCall(t *testing.T) {
	wake, _ := wakeshift.New(wakeshift.WithKey(""))
	ev, _ := wakeshift.DefineEvent(wake, "crawl.finished", wakeshift.EventDef[jobArgs, crawl]{Description: "A crawl finished"})
	other, _ := wakeshift.DefineEvent(wake, "crawl.started", wakeshift.EventDef[jobArgs, crawl]{Description: "A crawl started"})
	call, rec := wakeshifttest.NewToolCall(wakeshifttest.ToolCallOptions{Tool: "start_crawl", User: "u1", Events: []wakeshift.Subscribable{ev}})
	ctx := context.Background()
	if call.Session() != "" {
		t.Fatal("a session before subscribing")
	}
	sub, err := ev.Subscribe(ctx, call, jobArgs{Job: "j1"}, wakeshift.Once(), wakeshift.Replay(3600e9))
	if err != nil || sub.State != wakeshift.SubscriptionCreated || sub.ID == "" || sub.User != "u1" || sub.Session != "wsc_test_session" || call.Session() != "wsc_test_session" {
		t.Fatalf("%+v %v", sub, err)
	}
	if sub, _ := ev.Subscribe(ctx, call, jobArgs{Job: "j1"}); sub.State != wakeshift.SubscriptionRenewed {
		t.Fatalf("again: %+v", sub)
	}
	if _, err := other.Subscribe(ctx, call, jobArgs{Job: "j1"}); !wakeshift.IsCode(err, wakeshift.CodeUndeclaredSubscribe) {
		t.Fatalf("undeclared: %v", err)
	}
	if ended, _ := ev.Unsubscribe(ctx, call, jobArgs{Job: "j1"}); !ended {
		t.Fatal("not ended")
	}
	subs := rec.Subscriptions()
	if len(subs) != 2 || subs[0].Event != "crawl.finished" || subs[0].Args["job"] != "j1" || !subs[0].Once || subs[0].Replay.Hours() != 1 || !subs[0].Created || subs[1].Created || subs[0].Tool != "start_crawl" {
		t.Fatalf("%+v", subs)
	}
	if u := rec.Unsubscribes(); len(u) != 1 || !u[0].Ended {
		t.Fatalf("%+v", u)
	}
	native, _ := wakeshifttest.NewToolCall(wakeshifttest.ToolCallOptions{User: "u1", Native: true})
	if sub, _ := ev.Subscribe(ctx, native, jobArgs{Job: "j1"}); sub.State != wakeshift.SubscriptionNative {
		t.Fatalf("native: %+v", sub)
	}
	// A tool named in Wrap finds it with ToolCallFrom.
	if wakeshift.ToolCallFrom(wakeshifttest.ContextWithCall(ctx, call)) != call {
		t.Fatal("ToolCallFrom")
	}
}

// The test client: attach, call a tool that subscribes, emit, all offline.
func TestNewClient(t *testing.T) {
	ctx := context.Background()
	wake := wakeshifttest.NewClient(wakeshifttest.App("Crawler"))
	if !wake.Enabled() {
		t.Fatal("disabled")
	}
	ev, err := wakeshift.DefineEvent(wake.Client, "crawl.finished", wakeshift.EventDef[jobArgs, crawl]{Description: "A crawl finished"})
	if err != nil {
		t.Fatal(err)
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "crawler", Version: "1"}, nil)
	attached := wake.Attach(srv, wakeshift.AttachOptions{})
	wakeshift.AddTool(attached, &mcp.Tool{Name: "start_crawl", Description: "Crawl a site."},
		func(ctx context.Context, call *wakeshift.ToolCall, in startIn) (*mcp.CallToolResult, any, error) {
			_, err := ev.Subscribe(ctx, call, jobArgs{Job: "j1"}, wakeshift.Once())
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "started"}}}, nil, err
		}, wakeshift.Subscribes(ev))
	st, ct := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "claude-code", Version: "2"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	list, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var desc string
	for _, tool := range list.Tools {
		if tool.Name == "start_crawl" {
			desc = tool.Description
		}
	}
	if !strings.Contains(desc, "This tool is how Crawler notifies you") {
		t.Fatalf("description: %q", desc)
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "start_crawl", Arguments: map[string]any{"url": "https://example.com", "session_key": "wsc_x"}})
	if err != nil || res.IsError || len(res.Content) < 2 {
		t.Fatalf("%+v %v", res, err)
	}
	subs := wake.Subscriptions()
	if len(subs) != 1 || subs[0].Event != "crawl.finished" || subs[0].Tool != "start_crawl" || !subs[0].Once || subs[0].Via != "tool" {
		t.Fatalf("%+v", subs)
	}
	got, err := ev.Emit(ctx, jobArgs{Job: "j1"}, crawl{Status: "done"})
	if err != nil || got.Matched != 1 || len(wake.Emits()) != 1 || wake.Emits()[0].Text != "A crawl finished (job j1)" {
		t.Fatalf("%+v %v %+v", got, err, wake.Emits())
	}
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_subscriptions", Arguments: map[string]any{"session_key": "wsc_x"}})
	if err != nil || res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "crawl.finished") {
		t.Fatalf("list: %+v %v", res, err)
	}
	wake.Clear()
	if len(wake.Subscriptions())+len(wake.Emits()) != 0 {
		t.Fatal("Clear")
	}
}

// connect is an in-memory session to srv from a client named name.
func connect(t *testing.T, srv *mcp.Server, name string) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: name, Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

type apiKey string

type keyArgs struct {
	Job string `json:"job"`
	Key string `json:"key"`
}

var voucherRE = regexp.MustCompile(`wsv_[a-z2-7]{32}`)

// A ChatGPT-shaped conversation: the tool hands it a voucher, and its
// redemption runs OnSubscribe with the Context of the call that handed it
// out, and no other call's; a refresh at the same URL runs nothing, and
// any other use is refused.
func TestVoucherCarriesTheCallsContext(t *testing.T) {
	ctx := context.Background()
	wake := wakeshifttest.NewClient()
	got := make(chan wakeshift.Subscribed[jobArgs], 4)
	ev, _ := wakeshift.DefineEvent(wake.Client, "crawl.finished", wakeshift.EventDef[jobArgs, crawl]{Description: "A crawl finished",
		OnSubscribe: func(_ context.Context, s wakeshift.Subscribed[jobArgs]) error { got <- s; return nil }})
	srv := mcp.NewServer(&mcp.Implementation{Name: "crawler", Version: "1"}, nil)
	attached := wake.Attach(srv, wakeshift.AttachOptions{})
	wakeshift.AddTool(attached, &mcp.Tool{Name: "start_crawl", Description: "Crawl."},
		func(ctx context.Context, call *wakeshift.ToolCall, in keyArgs) (*mcp.CallToolResult, any, error) {
			sub, err := ev.Subscribe(ctx, call, jobArgs{Job: in.Job}, wakeshift.Context(apiKey(in.Key)))
			if err != nil || sub.State != wakeshift.SubscriptionNative {
				t.Errorf("subscribe: %+v %v", sub, err)
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "started"}}}, nil, err
		}, wakeshift.Subscribes(ev))
	cs := connect(t, srv, "openai-mcp")
	voucherFor := func(job, key string) string {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "start_crawl", Arguments: map[string]any{"job": job, "key": key}})
		if err != nil || res.IsError {
			t.Fatalf("%+v %v", res, err)
		}
		var text string
		for _, c := range res.Content {
			text += c.(*mcp.TextContent).Text + "\n"
		}
		v := voucherRE.FindString(text)
		if v == "" || !strings.Contains(text, "To be woken by crawl.finished with {\"job\":\""+job+"\"}") {
			t.Fatalf("no voucher line: %s", text)
		}
		return v
	}
	va, vb := voucherFor("j1", "key-a"), voucherFor("j2", "key-b")
	if vs := wake.Vouchers(); len(vs) != 2 || vs[0].Voucher != va || vs[0].Event != "crawl.finished" || vs[0].Args["job"] != "j1" {
		t.Fatalf("Vouchers: %+v", vs)
	}
	// B first: its own context, not A's.
	if _, created, err := wake.Redeem(ctx, vb, "https://agent.example/hook"); err != nil || !created {
		t.Fatalf("redeem B: %v %v", created, err)
	}
	if s := <-got; s.Context != apiKey("key-b") || s.Via != "tool" || s.Tool != "start_crawl" || s.Args.Job != "j2" {
		t.Errorf("B: %+v", s)
	}
	id, created, err := wake.Redeem(ctx, va, "https://agent.example/hook")
	if err != nil || !created || id == "" {
		t.Fatalf("redeem A: %v %v", created, err)
	}
	if s := <-got; s.Context != apiKey("key-a") || s.Args.Job != "j1" {
		t.Errorf("A: %+v", s)
	}
	// Again at the same URL: a refresh, no OnSubscribe; elsewhere: refused.
	if id2, created, err := wake.Redeem(ctx, va, "https://agent.example/hook"); err != nil || created || id2 != id {
		t.Errorf("refresh: %v %v %v", id2, created, err)
	}
	if _, _, err := wake.Redeem(ctx, va, "https://other.example/hook"); wakeshift.CodeOf(err) != "bad_voucher" {
		t.Errorf("reuse elsewhere: %v", err)
	}
	if _, _, err := wake.Redeem(ctx, "wsv_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "https://agent.example/hook"); wakeshift.CodeOf(err) != "bad_voucher" {
		t.Errorf("unknown: %v", err)
	}
	select {
	case s := <-got:
		t.Errorf("an extra OnSubscribe: %+v", s)
	default:
	}
	subs := wake.Subscriptions()
	if len(subs) != 2 || subs[0].Via != "tool" || subs[0].Tool != "start_crawl" {
		t.Errorf("subscriptions: %+v", subs)
	}
}

// The stand-in plays ChatGPT too: a native call with Client hands out a
// voucher, recorded, that Client.Redeem redeems with the Subscribe's
// Context.
func TestStandInVoucher(t *testing.T) {
	ctx := context.Background()
	wake := wakeshifttest.NewClient()
	got := make(chan wakeshift.Subscribed[jobArgs], 1)
	ev, _ := wakeshift.DefineEvent(wake.Client, "crawl.finished", wakeshift.EventDef[jobArgs, crawl]{Description: "A crawl finished",
		OnSubscribe: func(_ context.Context, s wakeshift.Subscribed[jobArgs]) error { got <- s; return nil }})
	call, rec := wakeshifttest.NewToolCall(wakeshifttest.ToolCallOptions{Tool: "start_crawl", Native: true, Client: wake})
	if sub, err := ev.Subscribe(ctx, call, jobArgs{Job: "j1"}, wakeshift.Context(apiKey("k1"))); err != nil || sub.State != wakeshift.SubscriptionNative {
		t.Fatalf("%+v %v", sub, err)
	}
	vs := rec.Vouchers()
	if len(vs) != 1 || vs[0].Event != "crawl.finished" || !voucherRE.MatchString(vs[0].Voucher) {
		t.Fatalf("%+v", vs)
	}
	if _, created, err := wake.Redeem(ctx, vs[0].Voucher, "https://agent.example/hook"); err != nil || !created {
		t.Fatalf("%v %v", created, err)
	}
	if s := <-got; s.Context != apiKey("k1") || s.Tool != "start_crawl" || s.Via != "tool" {
		t.Errorf("%+v", s)
	}
}

// FailNext: the next Subscribe (or Unsubscribe) fails as chosen, on the
// test client and on a stand-in; the one after goes ahead.
func TestFailNext(t *testing.T) {
	ctx := context.Background()
	wake := wakeshifttest.NewClient(wakeshifttest.With(wakeshift.WithLogger(slog.New(slog.DiscardHandler))))
	ev, _ := wakeshift.DefineEvent(wake.Client, "crawl.finished", wakeshift.EventDef[jobArgs, crawl]{Description: "A crawl finished"})
	srv := mcp.NewServer(&mcp.Implementation{Name: "crawler", Version: "1"}, nil)
	attached := wake.Attach(srv, wakeshift.AttachOptions{})
	var errs []error
	wakeshift.AddTool(attached, &mcp.Tool{Name: "start_crawl", Description: "Crawl."},
		func(ctx context.Context, call *wakeshift.ToolCall, in jobArgs) (*mcp.CallToolResult, any, error) {
			_, err := ev.Subscribe(ctx, call, in)
			errs = append(errs, err)
			if err == nil {
				_, err = ev.Unsubscribe(ctx, call, in)
				errs = append(errs, err)
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
		}, wakeshift.Subscribes(ev))
	cs := connect(t, srv, "claude-code")
	callIt := func() {
		if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "start_crawl", Arguments: map[string]any{"job": "j1", "session_key": "wsc_x"}}); err != nil {
			t.Fatal(err)
		}
	}
	wake.FailNext("subscribe", wakeshifttest.FailUnreachable)
	callIt()
	wake.FailNext("subscribe", wakeshifttest.FailRefused)
	callIt()
	wake.FailNext("unsubscribe", wakeshifttest.FailUnreachable)
	callIt()
	callIt()
	want := []wakeshift.Code{wakeshift.CodeUnreachable, wakeshift.CodeUnauthenticated, "", wakeshift.CodeUnreachable, "", ""}
	if len(errs) != len(want) {
		t.Fatalf("%v", errs)
	}
	for i, w := range want {
		if wakeshift.CodeOf(errs[i]) != w || (w == "") != (errs[i] == nil) {
			t.Errorf("%d: %v, want %q", i, errs[i], w)
		}
	}
	call, rec := wakeshifttest.NewToolCall(wakeshifttest.ToolCallOptions{})
	rec.FailNext("subscribe", wakeshifttest.FailRefused)
	if _, err := ev.Subscribe(ctx, call, jobArgs{Job: "j1"}); wakeshift.CodeOf(err) != wakeshift.CodeUnauthenticated {
		t.Errorf("stand-in: %v", err)
	}
	if _, err := ev.Subscribe(ctx, call, jobArgs{Job: "j1"}); err != nil {
		t.Errorf("stand-in after: %v", err)
	}
	rec.FailNext("unsubscribe", wakeshifttest.FailUnreachable)
	if _, err := ev.Unsubscribe(ctx, call, jobArgs{Job: "j1"}); wakeshift.CodeOf(err) != wakeshift.CodeUnreachable {
		t.Errorf("stand-in unsubscribe: %v", err)
	}
}

// The test client's Close lets a check in flight, and its emit, finish:
// a check that's done shows in Emits after Close.
func TestCloseWaitsForChecksInFlight(t *testing.T) {
	ctx := context.Background()
	wake := wakeshifttest.NewClient()
	ev, _ := wakeshift.DefineEvent(wake.Client, "crawl.finished", wakeshift.EventDef[jobArgs, crawl]{Description: "A crawl finished",
		OnSubscribe: func(ctx context.Context, s wakeshift.Subscribed[jobArgs]) error {
			return nil
		}})
	started, release := make(chan struct{}), make(chan struct{})
	go ev.Watch(ctx, jobArgs{Job: "j1"}, func(ctx context.Context) (crawl, bool, error) {
		close(started)
		<-release
		return crawl{Status: "done"}, true, nil
	})
	<-started
	go func() { time.Sleep(30 * time.Millisecond); close(release) }()
	wake.Close()
	if e := wake.Emits(); len(e) != 1 || e[0].Payload["status"] != "done" {
		t.Fatalf("emits after Close: %+v", e)
	}
}

// Finish runs OnSubscribe for what the stand-in created and still holds,
// with each Subscribe's Context, and waits.
func TestStandInFinish(t *testing.T) {
	ctx := context.Background()
	wake := wakeshifttest.NewClient()
	var mu sync.Mutex
	var seen []wakeshift.Subscribed[jobArgs]
	ev, _ := wakeshift.DefineEvent(wake.Client, "crawl.finished", wakeshift.EventDef[jobArgs, crawl]{Description: "A crawl finished",
		OnSubscribe: func(_ context.Context, s wakeshift.Subscribed[jobArgs]) error {
			time.Sleep(10 * time.Millisecond)
			mu.Lock()
			seen = append(seen, s)
			mu.Unlock()
			return nil
		}})
	call, rec := wakeshifttest.NewToolCall(wakeshifttest.ToolCallOptions{Tool: "start_crawl", User: "u1"})
	ev.Subscribe(ctx, call, jobArgs{Job: "j1"}, wakeshift.Context(apiKey("k1")))
	ev.Subscribe(ctx, call, jobArgs{Job: "j2"})
	ev.Unsubscribe(ctx, call, jobArgs{Job: "j2"})
	if subs := rec.Subscriptions(); len(subs) != 2 || subs[0].Context != apiKey("k1") || subs[1].Context != nil {
		t.Fatalf("the recorded subscribes' Context: %+v", subs)
	}
	rec.Finish(ctx)
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0].Args.Job != "j1" || seen[0].Context != apiKey("k1") || seen[0].Via != "tool" || seen[0].Tool != "start_crawl" || seen[0].User != "u1" {
		t.Fatalf("%+v", seen)
	}
}

// The test client's Close stops a watch waiting between checks at once.
func TestCloseStopsAWaitingWatch(t *testing.T) {
	wake := wakeshifttest.NewClient()
	ev, _ := wakeshift.DefineEvent(wake.Client, "crawl.finished", wakeshift.EventDef[jobArgs, crawl]{Description: "A crawl finished"})
	checked, returned := make(chan struct{}, 10), make(chan error, 1)
	go func() {
		returned <- ev.Watch(context.Background(), jobArgs{Job: "j1"}, func(context.Context) (crawl, bool, error) {
			checked <- struct{}{}
			return crawl{}, false, nil
		}, wakeshift.Every(time.Hour))
	}()
	<-checked
	closed := make(chan struct{})
	go func() { wake.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close waited for the next check")
	}
	if err := <-returned; err == nil || len(wake.Emits()) != 0 {
		t.Errorf("watch: %v, emits %v", err, wake.Emits())
	}
}
