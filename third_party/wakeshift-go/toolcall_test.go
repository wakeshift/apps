package wakeshift_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/wakeshift-go"
)

// The wrapper (wakeshift.Events) against a fake core that records what the SDK
// asks it: one block per call, rollback decided from the final result, the
// scope guard, native callers, the session alias and typed output.

const testTrailer = "(For Wakeshift's hook on this computer, not for you: wakeshift-beacon: aaaaaaaaaa.bbbbbbbbbb.cccccccccc)"

// wakeCore answers the app API's calls the wrapper makes, and records them.
type wakeCore struct {
	mu          sync.Mutex
	calls       []wakeCoreCall
	subs        int
	seen        map[string]bool
	addition    map[string]any // /calls/addition's answer
	unsubscribe map[string]any // /event-subscriptions/unsubscribe's, if set
	fail        map[string]bool
	broken      map[string]string // a path that hangs ("hang"), drops the connection ("unreachable") or answers 401 ("401")
	native      []wakeshift.EventSubscription
	appName     string        // GET /app's name ("" answers 404)
	appDelay    time.Duration // how long GET /app takes (or until the caller gives up)
}

type wakeCoreCall struct {
	Path string
	Body map[string]any
}

func (f *wakeCore) handler(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	path := strings.TrimPrefix(r.URL.Path, "/api/apps/v1")
	f.mu.Lock()
	delay := f.appDelay
	f.mu.Unlock()
	if path == "/app" && delay > 0 {
		f.mu.Lock()
		f.calls = append(f.calls, wakeCoreCall{Path: path})
		f.mu.Unlock()
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
		}
		return
	}
	f.mu.Lock()
	broken := f.broken[path]
	if broken != "" {
		f.calls = append(f.calls, wakeCoreCall{Path: path, Body: body})
	}
	f.mu.Unlock()
	switch broken {
	case "hang":
		<-r.Context().Done()
		return
	case "unreachable":
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			conn.Close()
		}
		return
	case "401":
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "unauthenticated", "message": "key revoked"}})
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if path == "/manifest" || path == "/manifest/merge" { // the automatic send (manifest_test.go tests it)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{}`)
		return
	}
	f.calls = append(f.calls, wakeCoreCall{Path: path, Body: body})
	if f.fail[path] {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "unavailable", "message": "down"}})
		return
	}
	reply := func(v any) { w.Header().Set("Content-Type", "application/json"); json.NewEncoder(w).Encode(v) }
	call, _ := body["call"].(map[string]any)
	switch path {
	case "/app":
		if f.appName == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		reply(map[string]any{"id": "app_test", "name": f.appName})
	case "/event-subscriptions":
		args, _ := json.Marshal(body["arguments"])
		key := fmt.Sprint(body["name"], string(args))
		created := !f.seen[key]
		if f.seen == nil {
			f.seen = map[string]bool{}
		}
		f.seen[key] = true
		f.subs++
		pending, _ := body["pending"].(string)
		if pending == "" {
			pending = "pa_first"
		}
		reply(map[string]any{"event_subscription": map[string]any{"id": fmt.Sprintf("sub-%d", f.subs), "user": call["user"], "created": created},
			"session": "wsc_s1", "agent": map[string]any{"harness": "claude-code", "via": "user_agent"}, "pending": pending})
	case "/calls/session":
		reply(map[string]any{"session": "wsc_resolved", "pending": "pa_session", "agent": map[string]any{"harness": "codex", "via": "meta"}})
	case "/calls/addition":
		if f.addition != nil {
			reply(f.addition)
			return
		}
		reply(map[string]any{"text": "Wakeshift: this conversation is woken by Monitor.", "trailer": testTrailer, "meta": map[string]any{"wakeshift/connector": "tok", "other": "x"}})
	case "/event-subscriptions/unsubscribe":
		// addition: what unsubscribe_events adds (the core sends a wrapped
		// tool's only when the call passed no key: f.unsubscribe).
		if f.unsubscribe != nil {
			reply(f.unsubscribe)
			return
		}
		if call["tool"] == "unsubscribe_events" {
			reply(map[string]any{"ended": true, "addition": map[string]any{"text": "Keep passing session_key."}})
			return
		}
		reply(map[string]any{"ended": true})
	case "/event-subscriptions/list":
		reply(map[string]any{"subscriptions": f.native})
	case "/event-subscriptions/end":
		reply(map[string]any{"ended": true})
	case "/events/subscribe":
		reply(map[string]any{"id": "n1", "refresh_before": "2026-11-05T00:00:00Z", "cursor": "c0", "truncated": false, "created": true})
	case "/events/poll":
		reply(map[string]any{"events": []any{}, "cursor": "c0", "created": true})
	case "/events/emit":
		reply(map[string]any{"matched": 2, "duplicate": false, "unmatched": []any{"u3", map[string]string{"native": "u4"}},
			"ended": []any{map[string]string{"user": "u2", "subscription": "s2"}}})
	case "/events/publish-urls":
		out := map[string]any{"id": "p1", "url": "https://x"}
		if body["signing_secret"] != nil {
			out["signing"] = map[string]any{"fingerprint": "abc123def456", "set_at": "2026-11-04T00:00:00Z"}
		}
		reply(out)
	case "/events/status":
		if body["subscriptions"] == nil {
			reply(map[string]any{"subscriptions": 3, "connected": 2, "via": map[string]int{"stream": 1, "poll": 1, "hooks": 0, "webhook": 0}})
			return
		}
		var out []any
		for _, ref := range body["subscriptions"].([]any) {
			r := ref.(map[string]any)
			out = append(out, map[string]any{"id": r["id"], "user": r["user"], "live": r["id"] == "s1", "connected": r["id"] == "s1", "via": map[bool]string{true: "stream"}[r["id"] == "s1"]})
		}
		reply(map[string]any{"subscriptions": out})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *wakeCore) paths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, c.Path)
	}
	return out
}

func (f *wakeCore) bodies(path string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for _, c := range f.calls {
		if c.Path == path {
			out = append(out, c.Body)
		}
	}
	return out
}

func (f *wakeCore) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

type tableIn struct {
	Table string `json:"table"`
}

type tableArgs struct {
	Table string `json:"table" jsonschema:"The table"`
}

type turnMsg struct {
	Text string `json:"text"`
}

type view struct {
	Table string `json:"table"`
}

type plainView struct {
	Table string `json:"table"`
}

// wrapped is an app with a personal and a shared event, behind Attach on a
// stateless MCP server whose bearer token is the user's ID.
type wrapped struct {
	core   *wakeCore
	app    *wakeshift.Client
	srv    *mcp.Server
	at     *wakeshift.Attachment
	turn   *wakeshift.Event[tableArgs, turnMsg]
	shared *wakeshift.Event[tableArgs, turnMsg]
	url    string
}

// newWrapped's extras: receiving middleware added before Attach
// (mcp.Middleware), a func that adjusts the options once the events are
// defined (func(*wakeshift.AttachOptions, *wrapped)), and the client's
// options (wakeshift.Option); nil is skipped.
func newWrapped(t *testing.T, opts wakeshift.AttachOptions, extras ...any) *wrapped {
	t.Helper()
	var before []mcp.Middleware
	var adjust []func(*wakeshift.AttachOptions, *wrapped)
	var clientOpts []wakeshift.Option
	for _, x := range extras {
		switch x := x.(type) {
		case nil:
		case mcp.Middleware:
			before = append(before, x)
		case func(mcp.MethodHandler) mcp.MethodHandler:
			before = append(before, x)
		case func(*wakeshift.AttachOptions, *wrapped):
			adjust = append(adjust, x)
		case wakeshift.Option:
			clientOpts = append(clientOpts, x)
		default:
			t.Fatalf("newWrapped: %T", x)
		}
	}
	w := &wrapped{core: &wakeCore{}}
	cs := httptest.NewServer(http.HandlerFunc(w.core.handler))
	t.Cleanup(cs.Close)
	w.app = testClient(t, cs.URL, clientOpts...)
	var err error
	if w.turn, err = wakeshift.DefineEvent(w.app, "game.turn", wakeshift.EventDef[tableArgs, turnMsg]{Description: "Your turn.", Personal: true,
		Text: func(m turnMsg, _ tableArgs) string { return m.Text }}); err != nil {
		t.Fatal(err)
	}
	// Public, so subscribe_events and native clients may subscribe to it too.
	if w.shared, err = wakeshift.DefineEvent(w.app, "table.news", wakeshift.EventDef[tableArgs, turnMsg]{Description: "News at a table.", Public: true,
		Text: func(m turnMsg, _ tableArgs) string { return m.Text }}); err != nil {
		t.Fatal(err)
	}
	w.srv = mcp.NewServer(&mcp.Implementation{Name: "t", Version: "1"}, nil)
	w.srv.AddReceivingMiddleware(before...)
	if opts.Identify == nil {
		opts.Identify = func(_ context.Context, r wakeshift.Request) (wakeshift.Identity, error) {
			return wakeshift.Identity{User: r.Token.UserID}, nil
		}
	}
	for _, f := range adjust {
		f(&opts, w)
	}
	w.at = w.app.Attach(w.srv, opts)
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return w.srv }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	verify := func(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		return &auth.TokenInfo{UserID: token, Expiration: time.Now().Add(time.Hour)}, nil
	}
	hs := httptest.NewServer(auth.RequireBearerToken(verify, nil)(w.at.Handler(h)))
	t.Cleanup(hs.Close)
	w.url = hs.URL
	return w
}

// rpcResult is a tools/call's answer.
type rpcResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Structured map[string]any `json:"structuredContent"`
	Meta       map[string]any `json:"_meta"`
	IsError    bool           `json:"isError"`
	RPCError   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"-"`
}

func (r rpcResult) texts() []string {
	var out []string
	for _, c := range r.Content {
		out = append(out, c.Text)
	}
	return out
}

// callTool calls a tool as user, from Claude Code unless ua says otherwise.
func (w *wrapped) callTool(t *testing.T, user, ua, tool string, args map[string]any, meta map[string]any) rpcResult {
	t.Helper()
	params := map[string]any{"name": tool, "arguments": args}
	if meta != nil {
		params["_meta"] = meta
	}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": params})
	req, _ := http.NewRequest("POST", w.url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+user)
	if ua == "" {
		ua = "claude-code/2.1.280"
	}
	req.Header.Set("User-Agent", ua)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out struct {
		Result rpcResult `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s: %d %s", tool, resp.StatusCode, raw)
	}
	out.Result.RPCError = out.Error
	return out.Result
}

func eqStrings(a, b []string) bool {
	return strings.Join(a, "\x00") == strings.Join(b, "\x00")
}

// Two subscribes in one call: the second passes the first's pending
// record, and the reply gets one block from one /calls/addition: the app's
// text, then Wakeshift's, then the beacon line last, and _meta merged.
func TestEventsOneBlockPerCall(t *testing.T) {
	w := newWrapped(t, wakeshift.AttachOptions{})
	var got []wakeshift.Subscription
	wakeshift.AddTool(w.at, &mcp.Tool{Name: "join"}, func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) (*mcp.CallToolResult, any, error) {
		if call.Recognized != nil || call.Session() != "" || call.User != "ada" || call.Tool != "join" || call.Native {
			return nil, nil, fmt.Errorf("before subscribing: %+v", call)
		}
		a, err := w.turn.Subscribe(ctx, call, tableArgs{Table: in.Table}, wakeshift.Reason("Playing at "+in.Table), wakeshift.TTL(time.Hour))
		if err != nil {
			return nil, nil, err
		}
		b, err := w.shared.Subscribe(ctx, call, tableArgs{Table: in.Table}, wakeshift.Label("poker"))
		if err != nil {
			return nil, nil, err
		}
		got = append(got, a, b)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Seated at " + in.Table + "."}}}, nil, nil
	}, wakeshift.Subscribes(w.turn, w.shared))
	res := w.callTool(t, "ada", "", "join", map[string]any{"table": "oak"}, nil)
	if res.IsError || res.RPCError != nil {
		t.Fatalf("join: %+v", res)
	}
	if want := []string{"Seated at oak.", "Wakeshift: this conversation is woken by Monitor.", testTrailer}; !eqStrings(res.texts(), want) {
		t.Fatalf("content: %q", res.texts())
	}
	if res.Meta["wakeshift/connector"] != "tok" || res.Meta["other"] != "x" {
		t.Fatalf("meta: %v", res.Meta)
	}
	if want := []string{"/event-subscriptions", "/event-subscriptions", "/calls/addition"}; !eqStrings(w.core.paths(), want) {
		t.Fatalf("requests: %v", w.core.paths())
	}
	subs := w.core.bodies("/event-subscriptions")
	if subs[0]["defer_addition"] != true || subs[0]["pending"] != nil || subs[0]["reason"] != "Playing at oak" || subs[0]["ttl"] != 3600.0 {
		t.Fatalf("first subscribe: %v", subs[0])
	}
	if subs[1]["pending"] != "pa_first" || subs[1]["label"] != "poker" || subs[1]["call"].(map[string]any)["session"] != "wsc_s1" {
		t.Fatalf("second subscribe: %v", subs[1])
	}
	if c := subs[0]["call"].(map[string]any); c["user"] != "ada" || c["tool"] != "join" || c["user_agent"] != "claude-code/2.1.280" {
		t.Fatalf("the call: %v", c)
	}
	add := w.core.bodies("/calls/addition")[0]
	if add["pending"] != "pa_first" || add["native"] != nil || add["call"].(map[string]any)["session"] != "wsc_s1" {
		t.Fatalf("addition: %v", add)
	}
	if len(got) != 2 || got[0].ID != "sub-1" || got[0].User != "ada" || got[0].State != wakeshift.SubscriptionCreated || got[0].Session != "wsc_s1" || got[0].Recognized.Harness != "claude-code" {
		t.Fatalf("subscriptions: %+v", got)
	}
}

// A wrapped tool that subscribes to nothing still asks for what Wakeshift
// adds to other results, when something names the conversation, without a
// pending record; one that names none asks nothing. Unwrapped tools are
// left alone.
func TestEventsWithoutSubscribing(t *testing.T) {
	w := newWrapped(t, wakeshift.AttachOptions{})
	w.core.addition = map[string]any{"text": "Wakeshift: 2 messages wait."}
	wakeshift.AddTool(w.at, &mcp.Tool{Name: "look"}, func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "A table."}}}, nil, nil
	}, wakeshift.Subscribes(w.turn, w.shared))
	mcp.AddTool(w.srv, &mcp.Tool{Name: "plain"}, func(ctx context.Context, _ *mcp.CallToolRequest, in tableIn) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Plain."}}}, nil, nil
	})
	res := w.callTool(t, "ada", "", "look", map[string]any{"table": "oak"}, nil)
	if !eqStrings(res.texts(), []string{"A table."}) || len(w.core.paths()) != 0 {
		t.Fatalf("nothing names a conversation: %q %v", res.texts(), w.core.paths())
	}
	res = w.callTool(t, "ada", "", "look", map[string]any{"table": "oak", "session_key": "wsc_s1"}, nil)
	if !eqStrings(res.texts(), []string{"A table.", "Wakeshift: 2 messages wait."}) {
		t.Fatalf("with a session: %q", res.texts())
	}
	if add := w.core.bodies("/calls/addition"); len(add) != 1 || add[0]["pending"] != nil {
		t.Fatalf("addition: %v", add)
	}
	w.core.reset()
	res = w.callTool(t, "ada", "", "plain", map[string]any{"table": "oak", "session_key": "wsc_s1"}, nil)
	if !eqStrings(res.texts(), []string{"Plain."}) || len(w.core.paths()) != 0 {
		t.Fatalf("an unwrapped tool: %q %v", res.texts(), w.core.paths())
	}
}

// ResolveSession makes the call's record without subscribing; a subscribe
// after it passes the record on; the block reads it.
// Middleware added before Attach runs inside it, and ToolCallFrom gives it
// a wrapped tool's call, with the session the handler resolved; nil for a
// tool Events didn't wrap.
func TestToolCallFrom(t *testing.T) {
	var seen []string
	record := func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			if method == "tools/call" {
				if c := wakeshift.ToolCallFrom(ctx); c != nil {
					seen = append(seen, c.Tool+":"+string(c.Session()))
				} else {
					seen = append(seen, "none")
				}
			}
			return res, err
		}
	}
	w := newWrapped(t, wakeshift.AttachOptions{}, record)
	wakeshift.AddTool(w.at, &mcp.Tool{Name: "join"}, func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) (*mcp.CallToolResult, any, error) {
		_, err := call.ResolveSession(ctx)
		return nil, nil, err
	}, wakeshift.Subscribes(w.turn, w.shared))
	mcp.AddTool(w.srv, &mcp.Tool{Name: "plain"}, func(context.Context, *mcp.CallToolRequest, tableIn) (*mcp.CallToolResult, any, error) {
		return nil, nil, nil
	})
	w.callTool(t, "ada", "", "join", map[string]any{"table": "oak", "session_key": "wsc_s1"}, nil)
	w.callTool(t, "ada", "", "plain", map[string]any{"table": "oak"}, nil)
	if want := []string{"join:wsc_resolved", "none"}; !eqStrings(seen, want) {
		t.Fatalf("ToolCallFrom: %v, want %v", seen, want)
	}
	if wakeshift.ToolCallFrom(context.Background()) != nil {
		t.Fatal("ToolCallFrom outside a call")
	}
}

func TestEventsResolveSession(t *testing.T) {
	w := newWrapped(t, wakeshift.AttachOptions{})
	wakeshift.AddTool(w.at, &mcp.Tool{Name: "join"}, func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) (*mcp.CallToolResult, any, error) {
		s, err := call.ResolveSession(ctx)
		if err != nil || s != "wsc_resolved" || call.Session() != s || call.Recognized == nil || call.Recognized.Harness != "codex" {
			return nil, nil, fmt.Errorf("resolve: %q %v %+v", s, err, call)
		}
		if again, err := call.ResolveSession(ctx); err != nil || again != s {
			return nil, nil, fmt.Errorf("again: %q %v", again, err)
		}
		if _, err := w.shared.Subscribe(ctx, call, tableArgs{Table: "oak"}); err != nil {
			return nil, nil, err
		}
		return nil, nil, nil
	}, wakeshift.Subscribes(w.turn, w.shared))
	if res := w.callTool(t, "ada", "", "join", map[string]any{"table": "oak", "session_key": "wsc_s1"}, nil); res.IsError {
		t.Fatalf("join: %q", res.texts())
	}
	if want := []string{"/calls/session", "/event-subscriptions", "/calls/addition"}; !eqStrings(w.core.paths(), want) {
		t.Fatalf("requests: %v", w.core.paths())
	}
	if p := w.core.bodies("/event-subscriptions")[0]["pending"]; p != "pa_session" {
		t.Fatalf("the subscribe's pending: %v", p)
	}
	if p := w.core.bodies("/calls/addition")[0]["pending"]; p != "pa_session" {
		t.Fatalf("the addition's pending: %v", p)
	}
}

// ResolveSession returns "", with no request, when nothing names the
// conversation; a session the call already resolved comes back; and
// anything that names it (a session_key, a conversation_id, a harness's
// conversation in _meta: conversation-meta.json) asks Wakeshift.
func TestResolveSessionNamesNothing(t *testing.T) {
	w := newWrapped(t, wakeshift.AttachOptions{})
	var got []string
	wakeshift.AddTool(w.at, &mcp.Tool{Name: "look"}, func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) (*mcp.CallToolResult, any, error) {
		s, err := call.ResolveSession(ctx)
		got = append(got, string(s))
		return nil, nil, err
	}, wakeshift.Subscribes(w.turn, w.shared))
	wakeshift.AddTool(w.at, &mcp.Tool{Name: "join"}, func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) (*mcp.CallToolResult, any, error) {
		if _, err := w.shared.Subscribe(ctx, call, tableArgs{Table: "oak"}); err != nil {
			return nil, nil, err
		}
		s, err := call.ResolveSession(ctx)
		got = append(got, string(s))
		return nil, nil, err
	}, wakeshift.Subscribes(w.turn, w.shared))
	if res := w.callTool(t, "ada", "", "look", map[string]any{"table": "oak"}, nil); res.IsError || !eqStrings(got, []string{""}) || len(w.core.paths()) != 0 {
		t.Fatalf("nothing names it: %q %q %v", res.texts(), got, w.core.paths())
	}
	got = nil
	if res := w.callTool(t, "ada", "", "join", map[string]any{"table": "oak"}, nil); res.IsError || !eqStrings(got, []string{"wsc_s1"}) {
		t.Fatalf("after a subscribe: %q %q", res.texts(), got)
	}
	raw, err := os.ReadFile(sharedGolden("conversation-meta.json"))
	if err != nil {
		t.Skip("no shared golden files here")
	}
	var f struct{ Keys []string }
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	type naming struct{ args, meta map[string]any }
	cases := []naming{
		{map[string]any{"session_key": "wsc_s1"}, nil},
		{map[string]any{"conversation_id": "bc-1234"}, nil},
		{nil, map[string]any{"threadId": "t0"}},
	}
	for _, k := range f.Keys {
		parts := strings.Split(k, ".")
		var v any = "c1"
		for i := len(parts) - 1; i > 0; i-- {
			v = map[string]any{parts[i]: v}
		}
		cases = append(cases, naming{nil, map[string]any{parts[0]: v}})
	}
	for _, c := range cases {
		w.core.reset()
		got = nil
		args := map[string]any{"table": "oak"}
		for k, v := range c.args {
			args[k] = v
		}
		res := w.callTool(t, "ada", "", "look", args, c.meta)
		if paths := w.core.paths(); res.IsError || !eqStrings(got, []string{"wsc_resolved"}) || len(paths) == 0 || paths[0] != "/calls/session" {
			t.Fatalf("%v %v: %q %q %v", c.args, c.meta, res.texts(), got, paths)
		}
	}
}

// Rollback is decided from the final result: a handler error, an isError
// result, a JSON-RPC error and an output that fails the output schema each
// end the subscriptions the call created (and only those), with reason
// rolled_back, and the reply gets no block.
func TestEventsRollback(t *testing.T) {
	w := newWrapped(t, wakeshift.AttachOptions{})
	subscribeBoth := func(ctx context.Context, call *wakeshift.ToolCall) error {
		if _, err := w.shared.Subscribe(ctx, call, tableArgs{Table: "old"}); err != nil { // renewed: not ended
			return err
		}
		_, err := w.shared.Subscribe(ctx, call, tableArgs{Table: "new"})
		return err
	}
	// "old" exists before the calls.
	w.core.seen = map[string]bool{`table.news{"table":"old"}`: true}
	wakeshift.AddTool(w.at, &mcp.Tool{Name: "fails"}, func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) (*mcp.CallToolResult, any, error) {
		if err := subscribeBoth(ctx, call); err != nil {
			return nil, nil, err
		}
		return nil, nil, errors.New("the table is full")
	}, wakeshift.Subscribes(w.turn, w.shared))
	wakeshift.AddTool(w.at, &mcp.Tool{Name: "is_error"}, func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) (*mcp.CallToolResult, any, error) {
		if err := subscribeBoth(ctx, call); err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "no"}}}, nil, nil
	}, wakeshift.Subscribes(w.turn, w.shared))
	wakeshift.AddTool(w.at, &mcp.Tool{Name: "rpc_error"}, func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) (*mcp.CallToolResult, any, error) {
		if err := subscribeBoth(ctx, call); err != nil {
			return nil, nil, err
		}
		return nil, nil, &jsonrpc.Error{Code: -32602, Message: "bad table"}
	}, wakeshift.Subscribes(w.turn, w.shared))
	strict := map[string]any{"type": "object", "properties": map[string]any{"table": map[string]any{"type": "string"}}, "required": []any{"table"}}
	wakeshift.AddTool(w.at, &mcp.Tool{Name: "bad_output", OutputSchema: strict}, func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) (*mcp.CallToolResult, map[string]any, error) {
		if err := subscribeBoth(ctx, call); err != nil {
			return nil, nil, err
		}
		return nil, map[string]any{"seat": 3}, nil // no table: the output schema refuses it
	}, wakeshift.Subscribes(w.turn, w.shared))
	for _, tool := range []string{"fails", "is_error", "rpc_error", "bad_output"} {
		w.core.reset()
		res := w.callTool(t, "ada", "", tool, map[string]any{"table": "x"}, nil)
		if !res.IsError && res.RPCError == nil {
			t.Fatalf("%s: didn't fail: %+v", tool, res)
		}
		for _, text := range res.texts() {
			if strings.Contains(text, "Wakeshift:") || strings.Contains(text, "wakeshift-beacon") {
				t.Fatalf("%s: a block on a failed reply: %q", tool, res.texts())
			}
		}
		if slicesContain(w.core.paths(), "/calls/addition") {
			t.Fatalf("%s: asked for a block: %v", tool, w.core.paths())
		}
		ends := w.core.bodies("/event-subscriptions/unsubscribe")
		if len(ends) != 1 || ends[0]["reason"] != "rolled_back" || ends[0]["name"] != "table.news" ||
			ends[0]["arguments"].(map[string]any)["table"] != "new" || ends[0]["call"].(map[string]any)["session"] != "wsc_s1" ||
			ends[0]["pending"] != "pa_first" { // the call's record: the core spares a subscription another call renewed
			t.Fatalf("%s: rollback: %v", tool, ends)
		}
		delete(w.core.seen, `table.news{"table":"new"}`)
	}
}

func slicesContain(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// Wakeshift's part failing never changes the tool's result: the tool may
// have done paid or visible work, so an error, or text telling the agent to
// try again, would repeat it. The result goes out as the tool gave it (the
// same as a tool registered without Wakeshift); what the call subscribed is
// rolled back, so the conversation isn't left subscribed without knowing how
// it's woken, and block_failed is logged, rate-limited.
func TestEventsBlockFailureKeepsTheToolsResult(t *testing.T) {
	type seatOut struct {
		Table string `json:"table"`
		Seat  int    `json:"seat"`
	}
	for _, c := range []struct {
		name string
		brk  func(*wakeCore)
	}{
		{"the addition fails", func(f *wakeCore) { f.fail = map[string]bool{"/calls/addition": true} }},
		{"the addition times out", func(f *wakeCore) { f.broken = map[string]string{"/calls/addition": "hang"} }},
		{"the core is unreachable", func(f *wakeCore) { f.broken = map[string]string{"/calls/addition": "unreachable"} }},
		{"the key is refused", func(f *wakeCore) { f.broken = map[string]string{"/calls/addition": "401"} }},
	} {
		t.Run(c.name, func(t *testing.T) {
			var logs logLines
			w := newWrapped(t, wakeshift.AttachOptions{}, wakeshift.WithLogger(logs.logger()), wakeshift.WithHTTPClient(&http.Client{Timeout: 200 * time.Millisecond}))
			c.brk(w.core)
			wakeshift.AddTool(w.at, &mcp.Tool{Name: "join"}, func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) (*mcp.CallToolResult, any, error) {
				if _, err := w.shared.Subscribe(ctx, call, tableArgs{Table: in.Table}); err != nil {
					return nil, nil, err
				}
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Seated."}}}, nil, nil
			}, wakeshift.Subscribes(w.turn, w.shared))
			wakeshift.AddTool(w.at, &mcp.Tool{Name: "seat"}, func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) (*mcp.CallToolResult, seatOut, error) {
				if _, err := w.shared.Subscribe(ctx, call, tableArgs{Table: in.Table}); err != nil {
					return nil, seatOut{}, err
				}
				return nil, seatOut{Table: in.Table, Seat: 3}, nil
			}, wakeshift.Subscribes(w.shared))
			// The same tools without Wakeshift: what the agent must get.
			mcp.AddTool(w.srv, &mcp.Tool{Name: "plain_join"}, func(context.Context, *mcp.CallToolRequest, tableIn) (*mcp.CallToolResult, any, error) {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Seated."}}}, nil, nil
			})
			mcp.AddTool(w.srv, &mcp.Tool{Name: "plain_seat"}, func(_ context.Context, _ *mcp.CallToolRequest, in tableIn) (*mcp.CallToolResult, seatOut, error) {
				return nil, seatOut{Table: in.Table, Seat: 3}, nil
			})
			for _, pair := range [][2]string{{"join", "plain_join"}, {"seat", "plain_seat"}} {
				got := w.callTool(t, "ada", "", pair[0], map[string]any{"table": "oak"}, nil)
				want := w.callTool(t, "ada", "", pair[1], map[string]any{"table": "oak"}, nil)
				gb, _ := json.Marshal(got)
				wb, _ := json.Marshal(want)
				if got.IsError || got.RPCError != nil || string(gb) != string(wb) {
					t.Fatalf("%s: got %s, want %s", pair[0], gb, wb)
				}
			}
			if got := w.callTool(t, "ada", "", "seat", map[string]any{"table": "elm"}, nil); got.Structured["seat"] != float64(3) || got.Structured["wakeshift"] != nil {
				t.Fatalf("seat: %+v", got)
			}
			ends := w.core.bodies("/event-subscriptions/unsubscribe")
			if len(ends) != 2 { // the third call renewed: a renewal isn't rolled back
				t.Fatalf("rollbacks: %v", ends)
			}
			for _, e := range ends {
				if e["reason"] != "rolled_back" {
					t.Fatalf("rollback: %v", e)
				}
			}
			var blocks []map[string]any
			for _, l := range logs.lines() {
				if l["code"] == "block_failed" {
					blocks = append(blocks, l)
				}
			}
			if len(blocks) != 1 || blocks[0]["level"] != "WARN" || blocks[0]["tool"] != "join" || blocks[0]["count"] != float64(1) ||
				!strings.Contains(blocks[0]["msg"].(string), "Nothing to change; the tool's result went out as the tool gave it, without Wakeshift's addition.") {
				t.Fatalf("block_failed: %v", blocks)
			}
			if strings.Contains(logs.String(), "key revoked") || strings.Contains(logs.String(), "down") {
				t.Fatalf("the core's message logged: %s", logs.String())
			}
		})
	}
}

// The vendor's own error result passes through untouched when Wakeshift is
// down, and a Subscribe error the vendor returns is its own: neither asks
// the agent to call again.
func TestEventsVendorErrorsPassThrough(t *testing.T) {
	w := newWrapped(t, wakeshift.AttachOptions{})
	w.core.fail = map[string]bool{"/calls/addition": true}
	wakeshift.AddTool(w.at, &mcp.Tool{Name: "refuse"}, func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) (*mcp.CallToolResult, any, error) {
		if _, err := w.shared.Subscribe(ctx, call, tableArgs{Table: in.Table}); err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "The table is full."}}}, nil, nil
	}, wakeshift.Subscribes(w.shared))
	wakeshift.AddTool(w.at, &mcp.Tool{Name: "other"}, func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) (*mcp.CallToolResult, any, error) {
		_, err := w.turn.Subscribe(ctx, call, tableArgs{Table: in.Table}) // not in its Subscribes: a bug the developer must see
		return nil, nil, err
	}, wakeshift.Subscribes(w.shared))
	res := w.callTool(t, "ada", "", "refuse", map[string]any{"table": "oak"}, nil)
	if !res.IsError || strings.Join(res.texts(), "|") != "The table is full." {
		t.Fatalf("refuse: %+v", res)
	}
	if slicesContain(w.core.paths(), "/calls/addition") {
		t.Fatal("an addition for a failed call")
	}
	res = w.callTool(t, "ada", "", "other", map[string]any{"table": "oak"}, nil)
	said := strings.Join(res.texts(), " ")
	if res.RPCError != nil {
		said += res.RPCError.Message
	}
	if !strings.Contains(said, "isn't in its subscribes") || strings.Contains(strings.ToLower(said), "again") {
		t.Fatalf("other: %+v", res)
	}
}

// The scope guard: a ToolCall Events didn't make, or one used after its
// handler returned, does nothing; a wrapped handler run without its
// Attachment's middleware refuses.
func TestToolCallScope(t *testing.T) {
	w := newWrapped(t, wakeshift.AttachOptions{})
	ctx := context.Background()
	if _, err := w.turn.Subscribe(ctx, &wakeshift.ToolCall{}, tableArgs{Table: "oak"}); !wakeshift.IsCode(err, wakeshift.CodeNoCall) {
		t.Fatalf("a zero ToolCall: %v", err)
	}
	if _, err := w.turn.Subscribe(ctx, nil, tableArgs{Table: "oak"}); !wakeshift.IsCode(err, wakeshift.CodeNoCall) || !strings.Contains(err.Error(), "outside a tool call") {
		t.Fatalf("a nil ToolCall: %v", err)
	}
	if _, err := w.turn.Unsubscribe(ctx, &wakeshift.ToolCall{User: "ada"}, tableArgs{Table: "oak"}); !wakeshift.IsCode(err, wakeshift.CodeNoCall) {
		t.Fatalf("unsubscribe: %v", err)
	}
	if _, err := (&wakeshift.ToolCall{}).ResolveSession(ctx); !wakeshift.IsCode(err, wakeshift.CodeNoCall) {
		t.Fatalf("resolve: %v", err)
	}
	var kept *wakeshift.ToolCall
	late := make(chan error, 1)
	wakeshift.AddTool(w.at, &mcp.Tool{Name: "join"}, func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) (*mcp.CallToolResult, any, error) {
		kept = call
		return nil, nil, nil
	}, wakeshift.Subscribes(w.turn, w.shared))
	w.callTool(t, "ada", "", "join", map[string]any{"table": "oak"}, nil)
	if kept == nil {
		t.Fatal("the handler didn't run")
	}
	go func() { _, err := w.turn.Subscribe(ctx, kept, tableArgs{Table: "oak"}); late <- err }()
	if err := <-late; !wakeshift.IsCode(err, wakeshift.CodeCallEnded) || !strings.Contains(err.Error(), "already returned") {
		t.Fatalf("after the handler returned: %v", err)
	}
	if _, err := kept.ResolveSession(ctx); !wakeshift.IsCode(err, wakeshift.CodeCallEnded) {
		t.Fatalf("resolve after: %v", err)
	}
	if slicesContain(w.core.paths(), "/event-subscriptions") || slicesContain(w.core.paths(), "/calls/session") {
		t.Fatalf("the core was asked: %v", w.core.paths())
	}
}

// A personal event needs a user; an event of another app is refused.
func TestSubscribeChecks(t *testing.T) {
	w := newWrapped(t, wakeshift.AttachOptions{Identify: func(context.Context, wakeshift.Request) (wakeshift.Identity, error) { return wakeshift.Identity{}, nil }})
	otherApp := testClient(t, "https://other.example")
	foreign, _ := wakeshift.DefineEvent(otherApp, "x.y", wakeshift.EventDef[tableArgs, turnMsg]{Description: "X.", Text: func(turnMsg, tableArgs) string { return "" }})
	var errs []error
	wakeshift.AddTool(w.at, &mcp.Tool{Name: "join"}, func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) (*mcp.CallToolResult, any, error) {
		_, e1 := w.turn.Subscribe(ctx, call, tableArgs{Table: "oak"})
		_, e2 := foreign.Subscribe(ctx, call, tableArgs{Table: "oak"})
		errs = []error{e1, e2}
		return nil, nil, nil
	}, wakeshift.Subscribes(w.turn, w.shared))
	w.callTool(t, "ada", "", "join", map[string]any{"table": "oak"}, nil)
	if len(errs) != 2 || !wakeshift.IsCode(errs[0], "bad_arguments") || !strings.Contains(errs[0].Error(), "personal") ||
		!wakeshift.IsCode(errs[1], "unknown_event") || !strings.Contains(errs[1].Error(), "another app") {
		t.Fatalf("errors: %v", errs)
	}
	if slicesContain(w.core.paths(), "/event-subscriptions") {
		t.Fatalf("subscribed: %v", w.core.paths())
	}
}

// Native callers (ChatGPT; a client that declares MCP Events; the app's
// own override) aren't subscribed: the block names the events to subscribe
// to. Unsubscribe ends a native caller's own webhook subscriptions with
// those arguments.
func TestEventsNative(t *testing.T) {
	w := newWrapped(t, wakeshift.AttachOptions{IsNativeClient: func(_ context.Context, r wakeshift.Request) bool {
		return r.Header.Get("X-House") == "yes"
	}})
	w.core.addition = map[string]any{"text": "To be woken by game.turn with {\"table\":\"oak\"}, subscribe to it (MCP Events)."}
	w.core.native = []wakeshift.EventSubscription{
		{ID: "n1", Event: "game.turn", Arguments: json.RawMessage(`{"table":"oak"}`), Destination: "webhook"},
		{ID: "n2", Event: "game.turn", Arguments: json.RawMessage(`{"table":"elm"}`), Destination: "webhook"},
		{ID: "c1", Event: "game.turn", Arguments: json.RawMessage(`{"table":"oak"}`), Destination: "conversation"},
	}
	type seenCall struct {
		Native             bool
		NativeConversation string
	}
	var seen []seenCall
	var subs []wakeshift.Subscription
	var ended []bool
	wakeshift.AddTool(w.at, &mcp.Tool{Name: "join"}, func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) (*mcp.CallToolResult, any, error) {
		seen = append(seen, seenCall{call.Native, call.NativeConversation})
		a, err := w.turn.Subscribe(ctx, call, tableArgs{Table: in.Table})
		if err != nil {
			return nil, nil, err
		}
		b, err := w.turn.Subscribe(ctx, call, tableArgs{Table: in.Table}) // once in the block
		if err != nil {
			return nil, nil, err
		}
		subs = append(subs, a, b)
		return nil, nil, nil
	}, wakeshift.Subscribes(w.turn, w.shared))
	wakeshift.AddTool(w.at, &mcp.Tool{Name: "leave"}, func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) (*mcp.CallToolResult, any, error) {
		ok, err := w.turn.Unsubscribe(ctx, call, tableArgs{Table: in.Table})
		ended = append(ended, ok)
		return nil, nil, err
	}, wakeshift.Subscribes(w.turn, w.shared))
	cases := []struct {
		name, ua string
		meta     map[string]any
		conv     string
	}{
		{"ChatGPT's user agent", "openai-mcp/1.0.0", nil, ""},
		{"ChatGPT's conversation", "some-client/1", map[string]any{"openai/session": "chat-9"}, "chat-9"},
		{"a client that declares MCP Events", "some-client/1", map[string]any{"io.modelcontextprotocol/clientCapabilities": map[string]any{
			"extensions": map[string]any{"io.modelcontextprotocol/events": map[string]any{}}}}, ""},
		{"ChatGPT's client name", "x/1", map[string]any{"io.modelcontextprotocol/clientInfo": map[string]any{"name": "openai-mcp", "version": "1"}}, ""},
	}
	for _, c := range cases {
		w.core.reset()
		seen, subs = nil, nil
		res := w.callTool(t, "ada", c.ua, "join", map[string]any{"table": "oak"}, c.meta)
		if res.IsError || len(seen) != 1 || !seen[0].Native || seen[0].NativeConversation != c.conv {
			t.Fatalf("%s: %+v %+v", c.name, res, seen)
		}
		if len(subs) != 2 || subs[0].State != wakeshift.SubscriptionNative || subs[0].ID != "" || subs[0].User != "ada" {
			t.Fatalf("%s: subscriptions: %+v", c.name, subs)
		}
		if want := []string{"/calls/addition"}; !eqStrings(w.core.paths(), want) {
			t.Fatalf("%s: requests %v", c.name, w.core.paths())
		}
		lines, _ := w.core.bodies("/calls/addition")[0]["native"].([]any)
		if len(lines) != 1 || lines[0].(map[string]any)["name"] != "game.turn" || lines[0].(map[string]any)["arguments"].(map[string]any)["table"] != "oak" {
			t.Fatalf("%s: native lines: %v", c.name, lines)
		}
		if !strings.Contains(strings.Join(res.texts(), "\n"), "subscribe to it (MCP Events)") {
			t.Fatalf("%s: %q", c.name, res.texts())
		}
	}
	// The app's override.
	seen = nil
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "join", "arguments": map[string]any{"table": "oak"}}})
	req, _ := http.NewRequest("POST", w.url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer ada")
	req.Header.Set("X-House", "yes")
	if resp, err := http.DefaultClient.Do(req); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
	}
	if len(seen) != 1 || !seen[0].Native {
		t.Fatalf("the override: %+v", seen)
	}
	// Claude Code isn't native, even when it declares MCP Events (its name,
	// or on a stateless server its User-Agent, wins over capabilities).
	declares := map[string]any{"io.modelcontextprotocol/clientCapabilities": map[string]any{"extensions": map[string]any{"io.modelcontextprotocol/events": map[string]any{}}}}
	named := map[string]any{"io.modelcontextprotocol/clientInfo": map[string]any{"name": "claude-code", "version": "2.1.316"},
		"io.modelcontextprotocol/clientCapabilities": declares["io.modelcontextprotocol/clientCapabilities"]}
	for name, c := range map[string]struct {
		ua   string
		meta map[string]any
	}{"no capabilities": {"", nil}, "declares, by name": {"x/1", named}, "declares, by User-Agent": {"claude-code/2.1.316 (sdk-cli)", declares}} {
		seen = nil
		w.callTool(t, "ada", c.ua, "join", map[string]any{"table": "oak"}, c.meta)
		if len(seen) != 1 || seen[0].Native {
			t.Fatalf("Claude Code, %s: %+v", name, seen)
		}
	}
	// Unsubscribing natively ends the user's webhook subscription with those arguments only.
	w.core.reset()
	w.callTool(t, "ada", "openai-mcp/1.0.0", "leave", map[string]any{"table": "oak"}, nil)
	endsBodies := w.core.bodies("/event-subscriptions/end")
	if len(ended) != 1 || !ended[0] || len(endsBodies) != 1 || endsBodies[0]["id"] != "n1" || endsBodies[0]["user"] != "ada" {
		t.Fatalf("native unsubscribe: %v %v", ended, endsBodies)
	}
}

// Every structured result of a wrapped tool gets the block's text (not the
// beacon line) in its wakeshift field, whether or not its output embeds
// WakeOutput; the block stays in the content, with the beacon last. tools/list
// names the field in each wrapped tool's output schema (go-sdk's inferred
// schemas refuse fields they don't name), and leaves other tools' alone.
func TestEventsStructuredOutput(t *testing.T) {
	w := newWrapped(t, wakeshift.AttachOptions{})
	wakeshift.AddTool(w.at, &mcp.Tool{Name: "join"}, func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) (*mcp.CallToolResult, view, error) {
		_, err := w.shared.Subscribe(ctx, call, tableArgs{Table: in.Table})
		return nil, view{Table: in.Table}, err
	}, wakeshift.Subscribes(w.turn, w.shared))
	wakeshift.AddTool(w.at, &mcp.Tool{Name: "join_plain"}, func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) (*mcp.CallToolResult, plainView, error) {
		_, err := w.shared.Subscribe(ctx, call, tableArgs{Table: in.Table})
		return nil, plainView{Table: in.Table}, err
	}, wakeshift.Subscribes(w.turn, w.shared))
	wakeshift.AddTool(w.at, &mcp.Tool{Name: "join_raw"}, func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) (*mcp.CallToolResult, any, error) {
		_, err := w.shared.Subscribe(ctx, call, tableArgs{Table: in.Table})
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Seated."}}, StructuredContent: map[string]any{"table": in.Table}}, nil, err
	}, wakeshift.Subscribes(w.turn, w.shared))
	mcp.AddTool(w.srv, &mcp.Tool{Name: "unwrapped"}, func(context.Context, *mcp.CallToolRequest, struct {
		Table string `json:"table"`
	}) (*mcp.CallToolResult, plainView, error) {
		return nil, plainView{Table: "oak"}, nil
	})
	// Wakeshift's argument names without WakeArgs: not a wrapped tool.
	mcp.AddTool(w.srv, &mcp.Tool{Name: "lookalike"}, func(context.Context, *mcp.CallToolRequest, struct {
		SessionKey     string `json:"session_key"`
		Harness        string `json:"harness"`
		ConversationID string `json:"conversation_id"`
	}) (*mcp.CallToolResult, plainView, error) {
		return nil, plainView{Table: "oak"}, nil
	})
	const woken = "Wakeshift: this conversation is woken by Monitor."
	for _, name := range []string{"join", "join_plain", "join_raw"} {
		res := w.callTool(t, "ada", "", name, map[string]any{"table": "oak"}, nil)
		if res.IsError || res.Structured["table"] != "oak" || res.Structured["wakeshift"] != woken {
			t.Fatalf("%s structured: %v %q", name, res.Structured, res.texts())
		}
		texts := res.texts()
		if len(texts) != 3 || texts[1] != woken || texts[2] != testTrailer {
			t.Fatalf("%s content: %q", name, texts)
		}
	}
	for range 2 { // the server's own tools aren't changed by the first
		schemas := w.outputSchemas(t)
		for _, name := range []string{"join", "join_plain"} {
			s := schemas[name]
			props, _ := s["properties"].(map[string]any)
			field, _ := props["wakeshift"].(map[string]any)
			if field["type"] != "string" || slices.Contains(anyStrings(s["required"]), "wakeshift") {
				t.Fatalf("%s output schema: %v", name, s)
			}
		}
		if s := schemas["join_plain"]; s["additionalProperties"] == nil {
			t.Fatalf("join_plain's schema refuses other fields: %v", s)
		}
		for _, name := range []string{"unwrapped", "lookalike"} {
			if props, _ := schemas[name]["properties"].(map[string]any); props["wakeshift"] != nil || props["table"] == nil {
				t.Fatalf("%s output schema: %v", name, schemas[name])
			}
		}
		if _, has := schemas["join_raw"]; has {
			t.Fatalf("join_raw has an output schema: %v", schemas["join_raw"])
		}
	}
}

// anyStrings is a JSON array's strings.
func anyStrings(v any) []string {
	var out []string
	a, _ := v.([]any)
	for _, x := range a {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// outputSchemas lists the tools, returning each one's output schema by name.
func (w *wrapped) outputSchemas(t *testing.T) map[string]map[string]any {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": map[string]any{}})
	req, _ := http.NewRequest("POST", w.url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer ada")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Result struct {
			Tools []struct {
				Name         string         `json:"name"`
				OutputSchema map[string]any `json:"outputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	schemas := map[string]map[string]any{}
	for _, tool := range out.Result.Tools {
		if tool.OutputSchema != nil {
			schemas[tool.Name] = tool.OutputSchema
		}
	}
	return schemas
}

// A beacon line ending a result's own text (a message the tool relays,
// say) is dropped, and Wakeshift's ends it; a result whose own _meta
// carries a connector beacon gets no second one.
func TestEventsSkipIfPresent(t *testing.T) {
	w := newWrapped(t, wakeshift.AttachOptions{})
	own := "(For Wakeshift's hook on this computer, not for you: wakeshift-beacon: dddddddddd.eeeeeeeeee.ffffffffff)"
	wakeshift.AddTool(w.at, &mcp.Tool{Name: "own_trailer"}, func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) (*mcp.CallToolResult, any, error) {
		_, err := w.shared.Subscribe(ctx, call, tableArgs{Table: in.Table})
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Mine.\n\n" + own}}}, nil, err
	}, wakeshift.Subscribes(w.turn, w.shared))
	wakeshift.AddTool(w.at, &mcp.Tool{Name: "own_meta"}, func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) (*mcp.CallToolResult, any, error) {
		_, err := w.shared.Subscribe(ctx, call, tableArgs{Table: in.Table})
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Mine."}}, Meta: mcp.Meta{"wakeshift/connector": "theirs"}}, nil, err
	}, wakeshift.Subscribes(w.turn, w.shared))
	codex := map[string]any{"x-codex-turn-metadata": map[string]any{"thread_id": "t1"}}
	res := w.callTool(t, "ada", "", "own_trailer", map[string]any{"table": "oak", "session_key": "x"}, codex)
	if want := []string{"Mine.", "Wakeshift: this conversation is woken by Monitor.", testTrailer}; !eqStrings(res.texts(), want) {
		t.Fatalf("own trailer: %q", res.texts())
	}
	if res.Meta["wakeshift/connector"] != "tok" || res.Meta["other"] != "x" {
		t.Fatalf("own trailer's meta: %v", res.Meta)
	}
	res = w.callTool(t, "ada", "", "own_meta", map[string]any{"table": "elm", "session_key": "x"}, codex)
	if want := []string{"Mine.", "Wakeshift: this conversation is woken by Monitor."}; !eqStrings(res.texts(), want) || res.Meta["wakeshift/connector"] != "theirs" {
		t.Fatalf("own meta: %q %v", res.texts(), res.Meta)
	}
}

// Identity.Client names the client for a request that doesn't (a
// stateless server never sees initialize): a wrapped tool's call and
// attach's own tools send it, and it decides whether the caller is native.
// A name the request carries wins, and an identity without one sends none.
func TestIdentityClient(t *testing.T) {
	clients := map[string]string{"ada": "Claude Code", "gpt": "openai-mcp"}
	w := newWrapped(t, wakeshift.AttachOptions{Identify: func(_ context.Context, r wakeshift.Request) (wakeshift.Identity, error) {
		return wakeshift.Identity{User: r.Token.UserID, Client: clients[r.Token.UserID]}, nil
	}})
	var native []bool
	wakeshift.AddTool(w.at, &mcp.Tool{Name: "join"}, func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) (*mcp.CallToolResult, any, error) {
		native = append(native, call.Native)
		_, err := w.shared.Subscribe(ctx, call, tableArgs{Table: in.Table})
		return nil, nil, err
	}, wakeshift.Subscribes(w.turn, w.shared))
	clientOf := func() any {
		subs := w.core.bodies("/event-subscriptions")
		if len(subs) != 1 {
			t.Fatalf("subscribes: %v", subs)
		}
		return subs[0]["call"].(map[string]any)["client"]
	}
	w.callTool(t, "ada", "", "join", map[string]any{"table": "oak"}, nil)
	if got := clientOf(); got != "Claude Code" || native[0] {
		t.Fatalf("identify's client: %v native %v", got, native)
	}
	w.core.reset()
	w.callTool(t, "ada", "", "join", map[string]any{"table": "oak"}, map[string]any{"io.modelcontextprotocol/clientInfo": map[string]any{"name": "Cursor", "version": "1"}})
	if got := clientOf(); got != "Cursor" {
		t.Fatalf("the request's own name: %v", got)
	}
	w.core.reset()
	w.callTool(t, "house", "", "join", map[string]any{"table": "oak"}, nil)
	if got := clientOf(); got != "" {
		t.Fatalf("no name: %v", got)
	}
	w.core.reset()
	w.callTool(t, "gpt", "x/1", "join", map[string]any{"table": "oak"}, nil)
	if !native[len(native)-1] || len(w.core.bodies("/event-subscriptions")) != 0 {
		t.Fatalf("ChatGPT by identify's name: native %v, %v", native, w.core.paths())
	}
	w.core.reset()
	w.callTool(t, "ada", "", "subscribe_events", map[string]any{"event": "table.news", "arguments": map[string]any{"table": "oak"}}, nil)
	if got := clientOf(); got != "Claude Code" {
		t.Fatalf("subscribe_events: %v", got)
	}
}

// AttachOptions.BuiltinTools picks which of Attach's own tools it adds: all
// three by default, each unless disabled.
func TestAttachTools(t *testing.T) {
	listed := func(w *wrapped) []string {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": map[string]any{}})
		req, _ := http.NewRequest("POST", w.url, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", "Bearer ada")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out struct {
			Result struct {
				Tools []struct {
					Name string `json:"name"`
				} `json:"tools"`
			} `json:"result"`
		}
		json.NewDecoder(resp.Body).Decode(&out)
		var names []string
		for _, tool := range out.Result.Tools {
			names = append(names, tool.Name)
		}
		slices.Sort(names)
		return names
	}
	// The zero value is the default; each of the three is on unless
	// disabled, and the wait tool (not in a plain tools/list) changes none.
	for _, c := range []struct {
		tools wakeshift.BuiltinTools
		want  []string
	}{
		{wakeshift.BuiltinTools{}, []string{"list_subscriptions", "subscribe_events", "unsubscribe_events"}},
		{wakeshift.BuiltinTools{Wait: true}, []string{"list_subscriptions", "subscribe_events", "unsubscribe_events"}},
		{wakeshift.BuiltinTools{DisableSubscribe: true}, []string{"list_subscriptions", "unsubscribe_events"}},
		{wakeshift.BuiltinTools{DisableSubscribe: true, DisableUnsubscribe: true}, []string{"list_subscriptions"}},
		{wakeshift.BuiltinTools{DisableSubscribe: true, DisableList: true, DisableUnsubscribe: true}, nil},
	} {
		if got := listed(newWrapped(t, wakeshift.AttachOptions{BuiltinTools: c.tools})); !eqStrings(got, c.want) {
			t.Fatalf("BuiltinTools %+v: listed %q", c.tools, got)
		}
	}
}

// unsubscribe_events adds what Wakeshift answers the unsubscribe with (that
// the conversation's session key stays the same) after its own reply.
func TestUnsubscribeEventsAddsWakeshiftsNote(t *testing.T) {
	w := newWrapped(t, wakeshift.AttachOptions{})
	res := w.callTool(t, "ada", "", "unsubscribe_events", map[string]any{"event": "table.news", "arguments": map[string]any{"table": "oak"}, "session_key": "wsc_s1"}, nil)
	if res.IsError || res.RPCError != nil || strings.Join(res.texts(), "|") != "Stopped table.news {\"table\":\"oak\"}.\n\nKeep passing session_key." {
		t.Fatalf("unsubscribe_events: %q", res.texts())
	}
}

// A wrapped tool's Unsubscribe that Wakeshift answers with a note (the
// call passed no session key, so nothing ended) puts it first in the
// call's block, even when nothing names the conversation and no block
// would be asked for.
func TestUnsubscribeWithoutTheKeyNoteInTheBlock(t *testing.T) {
	const note = "Wakeshift: nothing was unsubscribed. Call this tool again with the session_key you were given."
	w := newWrapped(t, wakeshift.AttachOptions{})
	var ended []bool
	wakeshift.AddTool(w.at, &mcp.Tool{Name: "unwatch"}, func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) (*mcp.CallToolResult, any, error) {
		ok, err := w.shared.Unsubscribe(ctx, call, tableArgs{Table: in.Table})
		ended = append(ended, ok)
		text := "Stopped watching."
		if !ok {
			text = "This chat wasn't watching."
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, err
	}, wakeshift.Subscribes(w.turn, w.shared))
	w.core.unsubscribe = map[string]any{"ended": false, "addition": map[string]any{"text": note}}
	w.core.reset()
	res := w.callTool(t, "ada", "", "unwatch", map[string]any{"table": "oak"}, nil)
	if got := strings.Join(res.texts(), "|"); res.IsError || got != "This chat wasn't watching.|"+note {
		t.Fatalf("keyless: %q", got)
	}
	if want := []string{"/event-subscriptions/unsubscribe"}; !eqStrings(w.core.paths(), want) {
		t.Fatalf("keyless: requests %v", w.core.paths())
	}
	w.core.reset()
	res = w.callTool(t, "ada", "", "unwatch", map[string]any{"table": "oak", "session_key": "wsc_s1"}, nil)
	if got := strings.Join(res.texts(), "|"); got != "This chat wasn't watching.|"+note+"\n\nWakeshift: this conversation is woken by Monitor.|"+testTrailer {
		t.Fatalf("with a block: %q", got)
	}
	// No note: no block for a keyless call, as before.
	w.core.unsubscribe = nil
	w.core.reset()
	res = w.callTool(t, "ada", "", "unwatch", map[string]any{"table": "oak"}, nil)
	if got := strings.Join(res.texts(), "|"); got != "Stopped watching." {
		t.Fatalf("no note: %q", got)
	}
	if len(ended) != 3 || ended[0] || !ended[2] {
		t.Fatalf("ended: %v", ended)
	}
}

// session_key is the one name for the session argument: session, its old
// name, is ignored like any other argument the tool doesn't take.
func TestWakeArgsNoSessionAlias(t *testing.T) {
	w := newWrapped(t, wakeshift.AttachOptions{})
	session := "unset"
	wakeshift.AddTool(w.at, &mcp.Tool{Name: "join"}, func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) (*mcp.CallToolResult, any, error) {
		session = string(call.Session())
		return nil, nil, nil
	}, wakeshift.Subscribes(w.turn, w.shared))
	res := w.callTool(t, "ada", "", "join", map[string]any{"table": "oak", "session": "claude-code#2"}, nil)
	if res.IsError || res.RPCError != nil || session != "" {
		t.Fatalf("session taken as the session (%q), or refused: %q", session, res.texts())
	}
}

// Emit's recipients: one plain user goes as a string (the single-user
// path); several, subscriptions and native recipients as a list; the
// response's unmatched and ended are read. PublishURL takes exactly one
// plain user.
func TestEmitRecipients(t *testing.T) {
	w := newWrapped(t, wakeshift.AttachOptions{})
	ctx := context.Background()
	if _, err := w.turn.Emit(ctx, tableArgs{Table: "oak"}, turnMsg{Text: "go"}, wakeshift.To("u1")); err != nil {
		t.Fatal(err)
	}
	if to := w.core.bodies("/events/emit")[0]["to"]; to != "u1" {
		t.Fatalf("one user: %v", to)
	}
	w.core.reset()
	got, err := w.turn.Emit(ctx, tableArgs{Table: "oak"}, turnMsg{Text: "go"}, wakeshift.To("u1", "u3"), wakeshift.ToSubscription("u2", "s2"),
		wakeshift.ToNative("u4"), wakeshift.WithID("m1:mention"))
	if err != nil {
		t.Fatal(err)
	}
	body := w.core.bodies("/events/emit")[0]
	b, _ := json.Marshal(body["to"])
	if string(b) != `["u1","u3",{"subscription":"s2","user":"u2"},{"native":"u4"}]` || body["id"] != "m1:mention" {
		t.Fatalf("the list: %s %v", b, body)
	}
	if got.Matched != 2 || len(got.Unmatched) != 2 || got.Unmatched[0] != (wakeshift.Recipient{User: "u3"}) || got.Unmatched[1] != (wakeshift.Recipient{User: "u4", Native: true}) ||
		len(got.Ended) != 1 || got.Ended[0] != (wakeshift.Recipient{User: "u2", Subscription: "s2"}) {
		t.Fatalf("emitted: %+v", got)
	}
	w.core.reset()
	if _, err := w.turn.Emit(ctx, tableArgs{Table: "oak"}, turnMsg{}, wakeshift.ToSubscription("u2", "s2")); err != nil {
		t.Fatal(err)
	}
	if to, _ := w.core.bodies("/events/emit")[0]["to"].([]any); len(to) != 1 {
		t.Fatalf("one subscription goes as a list: %v", to)
	}
	for name, opts := range map[string][]wakeshift.EmitOption{"none": nil, "empty user": {wakeshift.To("")}, "To()": {wakeshift.To()}} {
		if _, err := w.turn.Emit(ctx, tableArgs{Table: "oak"}, turnMsg{}, opts...); err == nil {
			t.Errorf("personal, %s: no error", name)
		}
	}
	if _, err := w.shared.Emit(ctx, tableArgs{Table: "oak"}, turnMsg{}, wakeshift.ToNative("u1")); err == nil {
		t.Error("a shared event took a recipient")
	}
	for name, opts := range map[string][]wakeshift.PublishURLOption{"two users": {wakeshift.To("a", "b")}, "a subscription": {wakeshift.ToSubscription("a", "s")},
		"native": {wakeshift.ToNative("a")}, "none": nil} {
		if _, err := w.turn.CreatePublishURL(ctx, tableArgs{Table: "oak"}, opts...); err == nil {
			t.Errorf("PublishURL, %s: no error", name)
		}
	}
	w.core.reset()
	if _, err := w.turn.CreatePublishURL(ctx, tableArgs{Table: "oak"}, wakeshift.To("a")); err != nil {
		t.Fatal(err)
	}
	if to := w.core.bodies("/events/publish-urls")[0]["to"]; to != "a" {
		t.Fatalf("PublishURL's to: %v", to)
	}
}

// Status counts the conversations subscribed with some arguments (a
// personal event's: for exactly one user), and how many are connected.
func TestStatus(t *testing.T) {
	w := newWrapped(t, wakeshift.AttachOptions{})
	ctx := context.Background()
	got, err := w.turn.Status(ctx, tableArgs{Table: "oak"}, wakeshift.To("u1"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Subscriptions != 3 || got.Connected != 2 || got.Via["stream"] != 1 || got.Via["poll"] != 1 {
		t.Fatalf("status: %+v", got)
	}
	body := w.core.bodies("/events/status")[0]
	if b, _ := json.Marshal(body); string(b) != `{"arguments":{"table":"oak"},"name":"game.turn","to":"u1"}` {
		t.Fatalf("the request: %s", b)
	}
	for name, opts := range map[string][]wakeshift.StatusOption{"two users": {wakeshift.To("a", "b")}, "a subscription": {wakeshift.ToSubscription("a", "s")},
		"native": {wakeshift.ToNative("a")}, "none": nil} {
		if _, err := w.turn.Status(ctx, tableArgs{Table: "oak"}, opts...); err == nil {
			t.Errorf("Status, %s: no error", name)
		}
	}
}

// StatusOf reads subscriptions by ID, each with its user, in one call: live
// and connected, or not. It needs each one's user and ID, and at most
// 1,000.
func TestStatusOf(t *testing.T) {
	w := newWrapped(t, wakeshift.AttachOptions{})
	ctx := context.Background()
	got, err := w.turn.StatusOf(ctx, wakeshift.Recipient{User: "u1", Subscription: "s1"}, wakeshift.Recipient{User: "u2", Subscription: "s2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != (wakeshift.SubscriptionStatus{ID: "s1", User: "u1", Live: true, Connected: true, Via: "stream"}) ||
		got[1] != (wakeshift.SubscriptionStatus{ID: "s2", User: "u2"}) {
		t.Fatalf("status: %+v", got)
	}
	body := w.core.bodies("/events/status")[0]
	if b, _ := json.Marshal(body); string(b) != `{"name":"game.turn","subscriptions":[{"id":"s1","user":"u1"},{"id":"s2","user":"u2"}]}` {
		t.Fatalf("the request: %s", b)
	}
	if got, err := w.turn.StatusOf(ctx); err != nil || got != nil {
		t.Fatalf("none: %v %v", got, err)
	}
	for name, subs := range map[string][]wakeshift.Recipient{"no user": {{Subscription: "s1"}}, "no ID": {{User: "u1"}},
		"too many": make([]wakeshift.Recipient, 1001)} {
		if _, err := w.turn.StatusOf(ctx, subs...); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// The mistakes every SDK refuses before asking Wakeshift (errors.json's
// client_checks and client_checks_next, which the TypeScript and Python
// SDKs test too): each a *UsageError with the code, its message saying
// what failed, the fix and the code's anchor.
func TestClientChecksGolden(t *testing.T) {
	b, err := os.ReadFile(sharedGolden("errors.json"))
	if os.IsNotExist(err) {
		t.Skip("no shared golden files here")
	}
	if err != nil {
		t.Fatal(err)
	}
	type check struct {
		Op, Name, Says string
		Code           wakeshift.Code
		MustNotContain string `json:"must_not_contain"`
		Skip           map[string]string
	}
	var g struct {
		ClientChecks struct {
			Cases []check
		} `json:"client_checks"`
		ClientChecksNext struct {
			Cases []check
		} `json:"client_checks_next"`
	}
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	if len(g.ClientChecks.Cases) == 0 || len(g.ClientChecksNext.Cases) == 0 {
		t.Fatal("errors.json has no client checks")
	}
	defer wakeshift.SetTimeLimits(50 * time.Millisecond)()
	ctx := context.Background()
	oak, msg := tableArgs{Table: "oak"}, turnMsg{Text: "go"}
	w := newWrapped(t, wakeshift.AttachOptions{})
	anon := newWrapped(t, wakeshift.AttachOptions{Identify: func(context.Context, wakeshift.Request) (wakeshift.Identity, error) { return wakeshift.Identity{}, nil }})
	required := newWrapped(t, wakeshift.AttachOptions{RequireUser: true, Identify: func(context.Context, wakeshift.Request) (wakeshift.Identity, error) { return wakeshift.Identity{}, nil }})
	throws := newWrapped(t, wakeshift.AttachOptions{Identify: func(context.Context, wakeshift.Request) (wakeshift.Identity, error) {
		return wakeshift.Identity{}, errors.New("boom-canary")
	}})
	hangs := newWrapped(t, wakeshift.AttachOptions{Identify: func(ctx context.Context, _ wakeshift.Request) (wakeshift.Identity, error) {
		<-ctx.Done()
		time.Sleep(10 * time.Millisecond)
		return wakeshift.Identity{User: "ada"}, nil
	}})
	// Another app's event, by the same name as one of this app's.
	foreign, err := wakeshift.DefineEvent(testClient(t, "https://other.example"), "game.turn", wakeshift.EventDef[tableArgs, turnMsg]{Description: "X.", Personal: true})
	if err != nil {
		t.Fatal(err)
	}
	tools := 0
	// inCall runs f in a tool's handler that subscribes to table.news only,
	// called by ada; ran reports whether the handler ran.
	inCall := func(w *wrapped, f func(context.Context, *wakeshift.ToolCall) error, decl ...wakeshift.Subscribable) error {
		tools++
		name := fmt.Sprintf("check%d", tools)
		var got error
		ran := false
		if decl == nil {
			decl = []wakeshift.Subscribable{w.shared}
		}
		wakeshift.AddTool(w.at, &mcp.Tool{Name: name}, func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) (*mcp.CallToolResult, any, error) {
			ran = true
			got = f(ctx, call)
			return nil, nil, nil
		}, wakeshift.Subscribes(decl...))
		w.callTool(t, "ada", "", name, map[string]any{"table": "oak"}, nil)
		if !ran {
			t.Errorf("%s: the vendor's tool code didn't run", name)
		}
		return got
	}
	var kept *wakeshift.ToolCall
	inCall(w, func(_ context.Context, call *wakeshift.ToolCall) error { kept = call; return nil })
	ops := map[string]func() error{
		"emit_personal_without_recipient": func() error { _, err := w.turn.Emit(ctx, oak, msg); return err },
		"emit_shared_with_recipient":      func() error { _, err := w.shared.Emit(ctx, oak, msg, wakeshift.To("ada")); return err },
		"emit_recipient_without_user":     func() error { _, err := w.turn.Emit(ctx, oak, msg, wakeshift.To(" ")); return err },
		"emit_recipient_without_subscription": func() error {
			_, err := w.turn.Emit(ctx, oak, msg, wakeshift.ToSubscription("ada", " "))
			return err
		},
		"publish_url_personal_without_to": func() error { _, err := w.turn.CreatePublishURL(ctx, oak); return err },
		"publish_url_shared_with_to":      func() error { _, err := w.shared.CreatePublishURL(ctx, oak, wakeshift.To("ada")); return err },
		"publish_url_two_users":           func() error { _, err := w.turn.CreatePublishURL(ctx, oak, wakeshift.To("ada", "bo")); return err },
		"status_personal_without_to":      func() error { _, err := w.turn.Status(ctx, oak); return err },
		"status_shared_with_to":           func() error { _, err := w.shared.Status(ctx, oak, wakeshift.To("ada")); return err },
		"status_two_users":                func() error { _, err := w.turn.Status(ctx, oak, wakeshift.To("ada", "bo")); return err },
		"status_of_too_many": func() error {
			subs := make([]wakeshift.Recipient, 1001)
			for i := range subs {
				subs[i] = wakeshift.Recipient{User: "ada", Subscription: "s"}
			}
			_, err := w.turn.StatusOf(ctx, subs...)
			return err
		},
		"status_of_without_user": func() error {
			_, err := w.turn.StatusOf(ctx, wakeshift.Recipient{Subscription: "s1"})
			return err
		},
		"subscribe_another_apps_event": func() error {
			return inCall(w, func(ctx context.Context, call *wakeshift.ToolCall) error {
				_, err := foreign.Subscribe(ctx, call, oak)
				return err
			})
		},
		"unsubscribe_another_apps_event": func() error {
			return inCall(w, func(ctx context.Context, call *wakeshift.ToolCall) error {
				_, err := foreign.Unsubscribe(ctx, call, oak)
				return err
			})
		},
		"subscribe_personal_without_user": func() error {
			return inCall(anon, func(ctx context.Context, call *wakeshift.ToolCall) error {
				_, err := anon.turn.Subscribe(ctx, call, oak)
				return err
			}, anon.turn)
		},
		"subscribe_undeclared": func() error {
			return inCall(w, func(ctx context.Context, call *wakeshift.ToolCall) error {
				_, err := w.turn.Subscribe(ctx, call, oak)
				return err
			})
		},
		"subscribe_nil_call": func() error {
			_, err := w.shared.Subscribe(ctx, wakeshift.ToolCallFrom(ctx), oak)
			return err
		},
		"subscribe_after_return": func() error {
			_, err := w.shared.Subscribe(ctx, kept, oak)
			return err
		},
		"identify_throws": func() error {
			return inCall(throws, func(ctx context.Context, call *wakeshift.ToolCall) error {
				_, err := throws.shared.Subscribe(ctx, call, oak)
				return err
			})
		},
		"identify_times_out": func() error {
			return inCall(hangs, func(ctx context.Context, call *wakeshift.ToolCall) error {
				_, err := hangs.shared.Subscribe(ctx, call, oak)
				return err
			})
		},
		"require_user_anonymous": func() error {
			return inCall(required, func(ctx context.Context, call *wakeshift.ToolCall) error {
				_, err := required.shared.Subscribe(ctx, call, oak)
				return err
			})
		},
	}
	for _, c := range append(g.ClientChecks.Cases, g.ClientChecksNext.Cases...) {
		if why := c.Skip["go"]; why != "" {
			if ops[c.Op] != nil {
				t.Errorf("%s: skipped for Go (%s), but Go checks it", c.Name, why)
			}
			continue
		}
		op := ops[c.Op]
		if op == nil {
			t.Errorf("%s: no op %s here", c.Name, c.Op)
			continue
		}
		cores := []*wakeCore{w.core, anon.core, required.core, throws.core, hangs.core}
		for _, core := range cores {
			core.reset()
		}
		err := op()
		var e *wakeshift.UsageError
		if !errors.As(err, &e) || e.Code != c.Code || !strings.Contains(e.Message, c.Says) || !wakeshift.IsCode(err, c.Code) ||
			!strings.HasPrefix(e.Message, "wakeshift: ") || !strings.HasSuffix(e.Message, " https://wakeshift.ai/docs/errors#"+string(c.Code)) {
			t.Errorf("%s: %v, want a UsageError %s saying %q", c.Name, err, c.Code, c.Says)
		}
		if c.MustNotContain != "" && err != nil && strings.Contains(err.Error(), c.MustNotContain) {
			t.Errorf("%s: the message has %q", c.Name, c.MustNotContain)
		}
		for _, core := range cores {
			for _, p := range core.paths() {
				if p != "/calls/addition" && p != "/app" {
					t.Errorf("%s: Wakeshift was asked: %s", c.Name, p)
				}
			}
		}
	}
}

// A result that asks the client for input (go-sdk's multi-round trip, on
// protocol 2026-07-28) isn't final: the client calls again with the
// answers. What the round subscribed is rolled back, and it gets no block
// (go-sdk refuses content beside inputRequests).
func TestEventsInputRequiredIsNotFinal(t *testing.T) {
	w := newWrapped(t, wakeshift.AttachOptions{})
	wakeshift.AddTool(w.at, &mcp.Tool{Name: "confirm_join"},
		func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) (*mcp.CallToolResult, any, error) {
			if _, err := w.shared.Subscribe(ctx, call, tableArgs{Table: in.Table}); err != nil {
				return nil, nil, err
			}
			return &mcp.CallToolResult{InputRequests: mcp.InputRequestMap{
				"confirm": &mcp.ElicitParams{Message: "Join " + in.Table + "?", RequestedSchema: map[string]any{"type": "object", "properties": map[string]any{}}},
			}}, nil, nil
		}, wakeshift.Subscribes(w.shared))
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "confirm_join", "arguments": map[string]any{"table": "oak", "session_key": "wsc_s1"}, "_meta": map[string]any{
			"io.modelcontextprotocol/protocolVersion":    "2026-07-28",
			"io.modelcontextprotocol/clientInfo":         map[string]any{"name": "claude-code", "version": "2.1.280"},
			"io.modelcontextprotocol/clientCapabilities": map[string]any{"elicitation": map[string]any{}},
		}}})
	req, _ := http.NewRequest("POST", w.url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer ada")
	req.Header.Set("Mcp-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", "tools/call")
	req.Header.Set("Mcp-Name", "confirm_join")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var out struct {
		Result map[string]any `json:"result"`
	}
	json.Unmarshal(raw, &out)
	if out.Result["inputRequests"] == nil {
		t.Fatalf("no inputRequests: %s", raw)
	}
	if content, _ := out.Result["content"].([]any); len(content) > 0 {
		t.Fatalf("an input_required result got Wakeshift's block: %s", raw)
	}
	if got := strings.Join(w.core.paths(), ","); got != "/event-subscriptions,/event-subscriptions/unsubscribe" {
		t.Fatalf("core calls: %s, want the round's subscription rolled back and no block", got)
	}
	if un := w.core.bodies("/event-subscriptions/unsubscribe"); un[0]["reason"] != "rolled_back" {
		t.Fatalf("unsubscribe: %v", un[0])
	}
}
