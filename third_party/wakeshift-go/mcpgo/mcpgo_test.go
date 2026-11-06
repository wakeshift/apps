package mcpgo_test

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
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/modelcontextprotocol/go-sdk/auth"

	"github.com/wakeshift/wakeshift-go"
	"github.com/wakeshift/wakeshift-go/mcpgo"
)

// The adapter against a fake core that records what the SDK asks it, on an
// mcp-go Streamable HTTP server behind Wrap, called as a protocol
// 2026-07-28 client: the block, rollback, native callers, the schemas, the
// built-in tools and what Wrap adds to server/discover and tools/list.

// testKey is app-keys.json's S key. Fake.
const testKey = "wst_1116qJFWMMHFy3xDdLmvUeyc2S6FrWRhJP51HsvDYdz9fWXzSV"

// tasksExtension is the MCP tasks extension's capability key.
const tasksExtension = "io.modelcontextprotocol/tasks"

const testTrailer = "(For Wakeshift's hook on this computer, not for you: wakeshift-beacon: aaaaaaaaaa.bbbbbbbbbb.cccccccccc)"

type fakeCore struct {
	mu           sync.Mutex
	calls        []fakeCall
	subs         int
	failAddition bool // /calls/addition answers 503
}

type fakeCall struct {
	Path string
	Body map[string]any
}

func (f *fakeCore) handler(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	path := strings.TrimPrefix(r.URL.Path, "/api/apps/v1")
	w.Header().Set("Content-Type", "application/json")
	if path == "/manifest" || path == "/manifest/merge" {
		io.WriteString(w, `{}`)
		return
	}
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{Path: path, Body: body})
	f.subs++
	n := f.subs
	failAddition := f.failAddition
	f.mu.Unlock()
	reply := func(v any) { json.NewEncoder(w).Encode(v) }
	call, _ := body["call"].(map[string]any)
	switch path {
	case "/event-subscriptions":
		if body["defer_addition"] != true { // subscribe_events
			reply(map[string]any{"event_subscription": map[string]any{"arguments": body["arguments"], "created": true},
				"addition": map[string]any{"text": "Wakeshift: woken by Monitor.", "meta": map[string]any{"wakeshift/connector": "tok"}}})
			return
		}
		reply(map[string]any{"event_subscription": map[string]any{"id": fmt.Sprintf("sub-%d", n), "user": call["user"], "created": true},
			"session": "wsc_s1", "agent": map[string]any{"harness": "claude-code", "via": "client"}, "pending": "pa_1"})
	case "/calls/addition":
		if failAddition {
			w.WriteHeader(http.StatusServiceUnavailable)
			reply(map[string]any{"error": map[string]any{"code": "unavailable", "message": "down"}})
			return
		}
		reply(map[string]any{"text": "Wakeshift: this conversation is woken by Monitor.", "trailer": testTrailer, "meta": map[string]any{"wakeshift/connector": "tok"}})
	case "/event-subscriptions/unsubscribe", "/event-subscriptions/end":
		reply(map[string]any{"ended": true})
	case "/event-subscriptions/list":
		reply(map[string]any{"subscriptions": []any{}})
	case "/app":
		reply(map[string]any{"id": "app_test", "name": "Shipyard"})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeCore) bodies(path string) []map[string]any {
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

func (f *fakeCore) paths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, c.Path)
	}
	return out
}

type tableArgs struct {
	Table string `json:"table" jsonschema:"The table"`
}

type turnMsg struct {
	Text string `json:"text"`
}

type tableIn struct {
	Table string `json:"table"`
}

type view struct {
	Table string `json:"table"`
}

type testApp struct {
	core *fakeCore
	app  *wakeshift.Client
	srv  *server.MCPServer
	at   *mcpgo.Attachment
	turn *wakeshift.Event[tableArgs, turnMsg]
	url  string
}

// newTestApp serves an app with a personal event on an mcp-go server
// (srvOpts), whose bearer token is the user's ID, behind Wrap.
func newTestApp(t *testing.T, opts wakeshift.AttachOptions, srvOpts ...server.ServerOption) *testApp {
	t.Helper()
	verify := func(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		return &auth.TokenInfo{UserID: token, Expiration: time.Now().Add(time.Hour)}, nil
	}
	return newTestAppBehind(t, auth.RequireBearerToken(verify, nil), opts, srvOpts...)
}

// newTestAppBehind is newTestApp behind the endpoint's own authentication.
func newTestAppBehind(t *testing.T, authn func(http.Handler) http.Handler, opts wakeshift.AttachOptions, srvOpts ...server.ServerOption) *testApp {
	t.Helper()
	a := &testApp{core: &fakeCore{}}
	cs := httptest.NewServer(http.HandlerFunc(a.core.handler))
	t.Cleanup(cs.Close)
	var err error
	if a.app, err = wakeshift.New(wakeshift.WithKey(testKey), wakeshift.WithCore(cs.URL), wakeshift.WithMaxRetries(0)); err != nil {
		t.Fatal(err)
	}
	if a.turn, err = wakeshift.DefineEvent(a.app, "game.turn", wakeshift.EventDef[tableArgs, turnMsg]{Description: "Your turn.", Personal: true,
		Text: func(m turnMsg, _ tableArgs) string { return m.Text }}); err != nil {
		t.Fatal(err)
	}
	a.srv = server.NewMCPServer("t", "1.0.0", srvOpts...)
	if opts.Identify == nil {
		opts.Identify = func(_ context.Context, r wakeshift.Request) (wakeshift.Identity, error) {
			if r.Token == nil {
				return wakeshift.Identity{}, nil
			}
			return wakeshift.Identity{User: r.Token.UserID}, nil
		}
	}
	a.at = mcpgo.Attach(a.app, a.srv, opts)
	hs := httptest.NewServer(authn(a.at.Handler(server.NewStreamableHTTPServer(a.srv))))
	t.Cleanup(hs.Close)
	a.url = hs.URL
	return a
}

// client is a protocol 2026-07-28 client's request's identity.
type client struct {
	user, ua, name string
	tasks          bool
	meta           map[string]any
}

var claudeCode = client{user: "ada", ua: "claude-code/2.1.280", name: "claude-code"}

// rpc sends one 2026-07-28 request and returns its result (or fails on a
// JSON-RPC error, unless errOK).
func (a *testApp) rpc(t *testing.T, c client, method string, params map[string]any) (map[string]any, map[string]any) {
	t.Helper()
	if params == nil {
		params = map[string]any{}
	}
	caps := map[string]any{}
	if c.tasks {
		caps["extensions"] = map[string]any{tasksExtension: map[string]any{}}
	}
	meta := map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28",
		"io.modelcontextprotocol/clientInfo":         map[string]any{"name": c.name, "version": "1"},
		"io.modelcontextprotocol/clientCapabilities": caps}
	for k, v := range c.meta {
		meta[k] = v
	}
	params["_meta"] = meta
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, _ := http.NewRequest("POST", a.url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", method)
	if name, ok := params["name"].(string); ok {
		req.Header.Set("Mcp-Name", name)
	}
	if c.user != "" {
		req.Header.Set("Authorization", "Bearer "+c.user)
	}
	req.Header.Set("User-Agent", c.ua)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	for _, line := range strings.Split(string(raw), "\n") {
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			raw = []byte(data)
		}
	}
	var out struct {
		Result map[string]any `json:"result"`
		Error  map[string]any `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s: %d %s", method, resp.StatusCode, raw)
	}
	return out.Result, out.Error
}

func (a *testApp) call(t *testing.T, c client, tool string, args map[string]any) map[string]any {
	t.Helper()
	res, rpcErr := a.rpc(t, c, "tools/call", map[string]any{"name": tool, "arguments": args})
	if rpcErr != nil {
		t.Fatalf("%s: %v", tool, rpcErr)
	}
	return res
}

func texts(res map[string]any) []string {
	var out []string
	content, _ := res["content"].([]any)
	for _, c := range content {
		m, _ := c.(map[string]any)
		s, _ := m["text"].(string)
		out = append(out, s)
	}
	return out
}

func (a *testApp) tools(t *testing.T, c client) map[string]map[string]any {
	t.Helper()
	res, rpcErr := a.rpc(t, c, "tools/list", nil)
	if rpcErr != nil {
		t.Fatalf("tools/list: %v", rpcErr)
	}
	out := map[string]map[string]any{}
	for _, tl := range res["tools"].([]any) {
		m := tl.(map[string]any)
		out[m["name"].(string)] = m
	}
	return out
}

// A wrapped tool's call: the handler gets the user, the tool and mcp-go's
// request; the core is told the client, its User-Agent and the tool; the
// reply carries the app's text, then Wakeshift's, then the beacon last, and
// the _meta.
func TestEventsAddsTheBlock(t *testing.T) {
	a := newTestApp(t, wakeshift.AttachOptions{})
	var got wakeshift.Subscription
	mcpgo.AddTool(a.at, mcp.NewTool("join", mcp.WithDescription("Sit down"), mcp.WithString("table", mcp.Required())),
		func(ctx context.Context, call *wakeshift.ToolCall, req mcp.CallToolRequest, in tableIn) (*mcp.CallToolResult, error) {
			if call.User != "ada" || call.Tool != "join" || call.Native || call.Request != nil || req.Params.Name != "join" {
				return nil, fmt.Errorf("the call: %+v", call)
			}
			if wakeshift.ToolCallFrom(ctx) != call {
				return nil, errors.New("ToolCallFrom")
			}
			var err error
			if got, err = a.turn.Subscribe(ctx, call, tableArgs{Table: in.Table}, wakeshift.Reason("Playing")); err != nil {
				return nil, err
			}
			return mcp.NewToolResultText("Seated at " + in.Table + "."), nil
		}, wakeshift.Subscribes(a.turn))
	res := a.call(t, claudeCode, "join", map[string]any{"table": "oak", "harness": "claude-code"})
	if res["isError"] == true {
		t.Fatalf("join: %v", res)
	}
	if want := []string{"Seated at oak.", "Wakeshift: this conversation is woken by Monitor.", testTrailer}; strings.Join(texts(res), "|") != strings.Join(want, "|") {
		t.Fatalf("content: %q", texts(res))
	}
	if meta, _ := res["_meta"].(map[string]any); meta["wakeshift/connector"] != "tok" {
		t.Fatalf("meta: %v", res["_meta"])
	}
	if got.ID != "sub-1" || got.User != "ada" || got.Session != "wsc_s1" {
		t.Fatalf("subscription: %+v", got)
	}
	sub := a.core.bodies("/event-subscriptions")[0]
	c := sub["call"].(map[string]any)
	if c["user"] != "ada" || c["client"] != "claude-code" || c["user_agent"] != "claude-code/2.1.280" || c["tool"] != "join" || c["harness"] != "claude-code" {
		t.Fatalf("the call: %v", c)
	}
	if meta, _ := c["meta"].(map[string]any); meta["io.modelcontextprotocol/protocolVersion"] != "2026-07-28" {
		t.Fatalf("the call's meta: %v", c["meta"])
	}
	if add := a.core.bodies("/calls/addition"); len(add) != 1 || add[0]["pending"] != "pa_1" {
		t.Fatalf("addition: %v", add)
	}
}

// A client on protocol 2026-07-28 that takes MCP tasks is told so (the
// core then gives the wait tool's instructions), and its tools/list has
// Wakeshift's wait tool.
func TestTasksClient(t *testing.T) {
	a := newTestApp(t, wakeshift.AttachOptions{BuiltinTools: wakeshift.BuiltinTools{Wait: true}})
	mcpgo.AddTool(a.at, mcp.NewTool("join"),
		func(ctx context.Context, call *wakeshift.ToolCall, _ mcp.CallToolRequest, in tableIn) (*mcp.CallToolResult, error) {
			_, err := a.turn.Subscribe(ctx, call, tableArgs{Table: in.Table})
			return nil, err
		}, wakeshift.Subscribes(a.turn))
	c := claudeCode
	c.tasks = true
	if _, ok := a.tools(t, c)["wakeshift_wait"]; !ok {
		t.Fatalf("no %s for a tasks client", "wakeshift_wait")
	}
	if _, ok := a.tools(t, claudeCode)["wakeshift_wait"]; ok {
		t.Fatalf("%s for a client without tasks", "wakeshift_wait")
	}
	res := a.call(t, c, "join", map[string]any{"table": "oak"})
	// A nil result replies with the block alone.
	if want := []string{"Wakeshift: this conversation is woken by Monitor.", testTrailer}; strings.Join(texts(res), "|") != strings.Join(want, "|") {
		t.Fatalf("content: %q", texts(res))
	}
	if call := a.core.bodies("/event-subscriptions")[0]["call"].(map[string]any); call["tasks"] != true {
		t.Fatalf("the call: %v", call)
	}
}

// Without Wait (the default), a client that takes tasks gets no wait tool,
// and its calls don't say it takes tasks, so Wakeshift gives it its usual
// wake path.
func TestTasksClientWithoutWait(t *testing.T) {
	a := newTestApp(t, wakeshift.AttachOptions{})
	mcpgo.AddTool(a.at, mcp.NewTool("join"),
		func(ctx context.Context, call *wakeshift.ToolCall, _ mcp.CallToolRequest, in tableIn) (*mcp.CallToolResult, error) {
			_, err := a.turn.Subscribe(ctx, call, tableArgs{Table: in.Table})
			return nil, err
		}, wakeshift.Subscribes(a.turn))
	c := claudeCode
	c.tasks = true
	if _, ok := a.tools(t, c)["wakeshift_wait"]; ok {
		t.Fatalf("%s without Wait", "wakeshift_wait")
	}
	a.call(t, c, "join", map[string]any{"table": "oak"})
	if call := a.core.bodies("/event-subscriptions")[0]["call"].(map[string]any); call["tasks"] != nil {
		t.Fatalf("the call: %v", call)
	}
	if _, rpcErr := a.rpc(t, c, "tasks/get", map[string]any{"taskId": "wkt1_" + "x"}); rpcErr == nil {
		t.Fatal("tasks/get for a Wakeshift ID was answered without Wait")
	}
	if n := len(a.core.bodies("/calls/task/get")); n != 0 {
		t.Fatalf("asked Wakeshift %d times", n)
	}
}

// A failed call (an error, an isError result, a panic) ends the
// subscriptions it made, and gets no block.
func TestFailedCallsRollBack(t *testing.T) {
	a := newTestApp(t, wakeshift.AttachOptions{}, server.WithRecovery())
	for _, fail := range []string{"error", "isError", "panic"} {
		mcpgo.AddTool(a.at, mcp.NewTool("fails_"+fail),
			func(ctx context.Context, call *wakeshift.ToolCall, _ mcp.CallToolRequest, in tableIn) (*mcp.CallToolResult, error) {
				if _, err := a.turn.Subscribe(ctx, call, tableArgs{Table: in.Table}); err != nil {
					return nil, err
				}
				switch fail {
				case "error":
					return nil, errors.New("that table is full")
				case "isError":
					return mcp.NewToolResultError("that table is full"), nil
				}
				panic("boom")
			}, wakeshift.Subscribes(a.turn))
	}
	for i, fail := range []string{"error", "isError", "panic"} {
		res, rpcErr := a.rpc(t, claudeCode, "tools/call", map[string]any{"name": "fails_" + fail, "arguments": map[string]any{"table": "oak"}})
		if rpcErr == nil && res["isError"] != true {
			t.Fatalf("%s: %v", fail, res)
		}
		for _, s := range texts(res) {
			if strings.Contains(s, "Wakeshift") {
				t.Fatalf("%s: a block in a failed call: %q", fail, texts(res))
			}
		}
		rolled := a.core.bodies("/event-subscriptions/unsubscribe")
		if len(rolled) != i+1 || rolled[i]["reason"] != "rolled_back" || rolled[i]["name"] != "game.turn" {
			t.Fatalf("%s: rollbacks %v", fail, rolled)
		}
	}
	if n := len(a.core.bodies("/calls/addition")); n != 0 {
		t.Fatalf("%d additions for failed calls", n)
	}
}

// Wakeshift's three arguments are added to a wrapped tool's input schema,
// described as every SDK describes them, and a structured result carries
// the block in its wakeshift field, which the listed output schema names,
// so mcp-go's output validation takes it.
func TestSchemasAndStructuredOutput(t *testing.T) {
	a := newTestApp(t, wakeshift.AttachOptions{}, server.WithOutputSchemaValidation(), server.WithInputSchemaValidation())
	mcpgo.AddTool(a.at, mcp.NewTool("look", mcp.WithInputSchema[tableIn](), mcp.WithOutputSchema[view]()),
		func(ctx context.Context, call *wakeshift.ToolCall, _ mcp.CallToolRequest, in tableIn) (*mcp.CallToolResult, error) {
			if _, err := a.turn.Subscribe(ctx, call, tableArgs{Table: in.Table}); err != nil {
				return nil, err
			}
			return mcp.NewToolResultStructuredOnly(view{Table: in.Table}), nil
		}, wakeshift.Subscribes(a.turn))
	look := a.tools(t, claudeCode)["look"]
	if ap := look["inputSchema"].(map[string]any)["additionalProperties"]; ap == nil || ap == true {
		t.Fatalf("the input schema isn't closed, so it can't show an undeclared argument dropped: %v", look["inputSchema"])
	}
	props := look["inputSchema"].(map[string]any)["properties"].(map[string]any)
	for _, name := range []string{"table", "session_key", "harness", "conversation_id"} {
		if props[name] == nil {
			t.Fatalf("input schema: no %s: %v", name, props)
		}
	}
	if d, _ := props["harness"].(map[string]any)["description"].(string); d != "Your program: claude-code, codex, cursor, grok-bot, openclaw…" {
		t.Fatalf("harness's description: %q", d)
	}
	out := look["outputSchema"].(map[string]any)["properties"].(map[string]any)
	if out["wakeshift"] == nil || out["table"] == nil {
		t.Fatalf("output schema: %v", out)
	}
	// An argument the input doesn't declare is dropped, not refused.
	res := a.call(t, claudeCode, "look", map[string]any{"table": "oak", "old_flag": true})
	if res["isError"] == true {
		t.Fatalf("look: %v", res)
	}
	sc, _ := res["structuredContent"].(map[string]any)
	if sc["table"] != "oak" || sc["wakeshift"] != "Wakeshift: this conversation is woken by Monitor." {
		t.Fatalf("structuredContent: %v", res["structuredContent"])
	}
	if ts := texts(res); ts[len(ts)-1] != testTrailer {
		t.Fatalf("content: %q", ts)
	}
}

// A client that subscribes to the app's events itself (here, by the
// app's own say) isn't subscribed: the block names what to subscribe to.
func TestNativeCaller(t *testing.T) {
	a := newTestApp(t, wakeshift.AttachOptions{IsNativeClient: func(_ context.Context, r wakeshift.Request) bool {
		return r.Header.Get("User-Agent") == "house/1"
	}})
	mcpgo.AddTool(a.at, mcp.NewTool("join"),
		func(ctx context.Context, call *wakeshift.ToolCall, _ mcp.CallToolRequest, in tableIn) (*mcp.CallToolResult, error) {
			if !call.Native {
				return nil, errors.New("not native")
			}
			sub, err := a.turn.Subscribe(ctx, call, tableArgs{Table: in.Table})
			if err != nil || sub.State != wakeshift.SubscriptionNative {
				return nil, fmt.Errorf("%+v %v", sub, err)
			}
			return mcp.NewToolResultText("ok"), nil
		}, wakeshift.Subscribes(a.turn))
	res := a.call(t, client{user: "ada", ua: "house/1", name: "house"}, "join", map[string]any{"table": "oak"})
	if res["isError"] == true {
		t.Fatalf("join: %v", res)
	}
	if n := len(a.core.bodies("/event-subscriptions")); n != 0 {
		t.Fatalf("%d subscribes for a native caller", n)
	}
	add := a.core.bodies("/calls/addition")
	if len(add) != 1 || add[0]["native"] == nil {
		t.Fatalf("addition: %v", add)
	}
}

// server/discover through Wrap: mcp-go's 2026-07-28 answer (resultType,
// supportedVersions, ttlMs and cacheScope), with the events and tasks
// capabilities added. events/list is answered by Wrap; subscribe_events
// works without a wrapped tool; an anonymous caller gets no user.
func TestDiscoverAndBuiltinTools(t *testing.T) {
	// The tasks extension only with Wait (off by default).
	if res, rpcErr := newTestApp(t, wakeshift.AttachOptions{BuiltinTools: wakeshift.BuiltinTools{Wait: true}}).rpc(t, claudeCode, "server/discover", nil); rpcErr != nil {
		t.Fatalf("discover: %v", rpcErr)
	} else if ext, _ := res["capabilities"].(map[string]any)["extensions"].(map[string]any); ext[tasksExtension] == nil {
		t.Fatalf("with Wait, capabilities: %v", res["capabilities"])
	}
	a := newTestApp(t, wakeshift.AttachOptions{})
	res, rpcErr := a.rpc(t, claudeCode, "server/discover", nil)
	if rpcErr != nil {
		t.Fatalf("discover: %v", rpcErr)
	}
	caps := res["capabilities"].(map[string]any)
	ext, _ := caps["extensions"].(map[string]any)
	if ext["io.modelcontextprotocol/events"] == nil || ext[tasksExtension] != nil || caps["tools"] == nil {
		t.Fatalf("capabilities: %v", caps)
	}
	if res["resultType"] != "complete" || res["ttlMs"] == nil || res["cacheScope"] == nil || res["supportedVersions"] == nil {
		t.Fatalf("discover: %v", res)
	}
	list, rpcErr := a.rpc(t, claudeCode, "tools/list", nil)
	if rpcErr != nil || list["ttlMs"] == nil || list["cacheScope"] == nil {
		t.Fatalf("tools/list: %v %v", list, rpcErr)
	}
	tools := a.tools(t, claudeCode)
	for _, name := range []string{"subscribe_events", "unsubscribe_events", "list_subscriptions"} {
		if tools[name] == nil {
			t.Fatalf("no %s: %v", name, tools)
		}
	}
	evs, rpcErr := a.rpc(t, claudeCode, "events/list", nil)
	if rpcErr != nil || len(evs["events"].([]any)) != 1 {
		t.Fatalf("events/list: %v %v", evs, rpcErr)
	}
	sub := a.call(t, claudeCode, "subscribe_events", map[string]any{"event": "game.turn", "arguments": map[string]any{"table": "oak"}})
	if ts := texts(sub); len(ts) != 1 || !strings.HasPrefix(ts[0], "Subscribed to game.turn {\"table\":\"oak\"}.") {
		t.Fatalf("subscribe_events: %v", sub)
	}
	if strings.Join(a.core.paths(), ",") != "/event-subscriptions" {
		t.Fatalf("requests: %v", a.core.paths())
	}
}

// tools/list gives Attach's own tools and the wait tool exactly the
// annotations every SDK gives (tool-annotations.json): a title and all four
// hints.
func TestBuiltinToolAnnotations(t *testing.T) {
	path := filepath.Join("..", "..", "..", "sdk", "typescript", "test", "tool-annotations.json")
	if _, err := os.Stat(path); err != nil {
		path = filepath.Join("..", "testdata", "golden", "tool-annotations.json")
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		t.Skip("no shared golden files here")
	}
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	if err := json.Unmarshal(b, &want); err != nil {
		t.Fatal(err)
	}
	c := claudeCode
	c.tasks = true
	got := map[string]any{}
	for name, tool := range newTestApp(t, wakeshift.AttachOptions{BuiltinTools: wakeshift.BuiltinTools{Wait: true}}).tools(t, c) {
		got[name] = tool["annotations"]
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tools/list's annotations\n got %v\nwant %v", got, want)
	}
}

// tools/list gives Attach's own tools the output schemas every SDK gives
// (tool-output-schemas.json), and list_subscriptions answers with matching
// structuredContent.
func TestBuiltinToolOutputSchemas(t *testing.T) {
	path := filepath.Join("..", "..", "..", "sdk", "typescript", "test", "tool-output-schemas.json")
	if _, err := os.Stat(path); err != nil {
		path = filepath.Join("..", "testdata", "golden", "tool-output-schemas.json")
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		t.Skip("no shared golden files here")
	}
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	if err := json.Unmarshal(b, &want); err != nil {
		t.Fatal(err)
	}
	for k := range want {
		if strings.HasPrefix(k, "_") {
			delete(want, k)
		}
	}
	a := newTestApp(t, wakeshift.AttachOptions{})
	got := map[string]any{}
	for name, tool := range a.tools(t, claudeCode) {
		if _, ok := want[name]; ok {
			got[name] = tool["outputSchema"]
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tools/list's output schemas\n got %v\nwant %v", got, want)
	}
	res := a.call(t, claudeCode, "list_subscriptions", map[string]any{})
	if sc, _ := res["structuredContent"].(map[string]any); sc == nil || sc["subscriptions"] == nil {
		t.Fatalf("list_subscriptions' structuredContent: %v", res)
	}
}

// A block Wakeshift can't give never changes the tool's result: it goes
// out exactly as the same tool's without Wakeshift, never an error; what
// the call subscribed is rolled back.
func TestBlockFailureKeepsTheToolsResult(t *testing.T) {
	a := newTestApp(t, wakeshift.AttachOptions{})
	a.core.failAddition = true
	result := func() *mcp.CallToolResult {
		return mcp.NewToolResultStructured(map[string]any{"table": "oak", "seat": 3}, `{"table":"oak","seat":3}`)
	}
	mcpgo.AddTool(a.at, mcp.NewTool("join", mcp.WithString("table", mcp.Required())),
		func(ctx context.Context, call *wakeshift.ToolCall, _ mcp.CallToolRequest, in tableIn) (*mcp.CallToolResult, error) {
			if _, err := a.turn.Subscribe(ctx, call, tableArgs{Table: in.Table}); err != nil {
				return nil, err
			}
			return result(), nil
		}, wakeshift.Subscribes(a.turn))
	a.srv.AddTool(mcp.NewTool("plain_join", mcp.WithString("table", mcp.Required())), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return result(), nil
	})
	got := a.call(t, claudeCode, "join", map[string]any{"table": "oak"})
	want := a.call(t, claudeCode, "plain_join", map[string]any{"table": "oak"})
	gb, _ := json.Marshal(got)
	wb, _ := json.Marshal(want)
	if got["isError"] == true || string(gb) != string(wb) {
		t.Fatalf("join: got %s, want %s", gb, wb)
	}
	if ends := a.core.bodies("/event-subscriptions/unsubscribe"); len(ends) != 1 || ends[0]["reason"] != "rolled_back" {
		t.Fatalf("rollback: %v", ends)
	}
}

// A call with no conversation and no subscription gets nothing added.
func TestNothingToAdd(t *testing.T) {
	a := newTestApp(t, wakeshift.AttachOptions{})
	mcpgo.AddTool(a.at, mcp.NewTool("peek"),
		func(context.Context, *wakeshift.ToolCall, mcp.CallToolRequest, tableIn) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("nothing here"), nil
		}, wakeshift.Subscribes(a.turn))
	res := a.call(t, claudeCode, "peek", map[string]any{"table": "oak"})
	if strings.Join(texts(res), "|") != "nothing here" || len(a.core.paths()) != 0 {
		t.Fatalf("peek: %q %v", texts(res), a.core.paths())
	}
}

// A client on an earlier protocol, through initialize: Wrap adds the
// events capability to its answer, and a wrapped tool's call in the
// session names the client initialize named.
func TestInitializeSession(t *testing.T) {
	a := newTestApp(t, wakeshift.AttachOptions{})
	mcpgo.AddTool(a.at, mcp.NewTool("join"),
		func(ctx context.Context, call *wakeshift.ToolCall, _ mcp.CallToolRequest, in tableIn) (*mcp.CallToolResult, error) {
			_, err := a.turn.Subscribe(ctx, call, tableArgs{Table: in.Table})
			return mcp.NewToolResultText("ok"), err
		}, wakeshift.Subscribes(a.turn))
	post := func(session, method string, params map[string]any) (map[string]any, http.Header) {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
		req, _ := http.NewRequest("POST", a.url, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", "Bearer ada")
		req.Header.Set("User-Agent", "some-agent/1")
		if session != "" {
			req.Header.Set("Mcp-Session-Id", session)
			req.Header.Set("Mcp-Protocol-Version", "2025-06-18")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		for _, line := range strings.Split(string(raw), "\n") {
			if data, ok := strings.CutPrefix(line, "data: "); ok {
				raw = []byte(data)
			}
		}
		var out struct {
			Result map[string]any `json:"result"`
		}
		if err := json.Unmarshal(raw, &out); err != nil || out.Result == nil {
			t.Fatalf("%s: %d %s", method, resp.StatusCode, raw)
		}
		return out.Result, resp.Header
	}
	hello, h := post("", "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "cursor-vscode", "version": "1"}})
	if ext, _ := hello["capabilities"].(map[string]any)["extensions"].(map[string]any); ext["io.modelcontextprotocol/events"] == nil {
		t.Fatalf("initialize: %v", hello)
	}
	session := h.Get("Mcp-Session-Id")
	if session == "" {
		t.Fatal("no session")
	}
	if res, _ := post(session, "tools/call", map[string]any{"name": "join", "arguments": map[string]any{"table": "oak"}}); res["isError"] == true {
		t.Fatalf("join: %v", res)
	}
	if c := a.core.bodies("/event-subscriptions")[0]["call"].(map[string]any); c["client"] != "cursor-vscode" || c["user_agent"] != "some-agent/1" {
		t.Fatalf("the call: %v", c)
	}
}

type userKey struct{}

// An app that authenticates its own way, in HTTP middleware in front of
// Wrap, identifies callers from ctx: Identify is called without a token,
// for Attach's tools and wrapped tools alike, and a caller it can't name
// has no user.
func TestIdentifyFromContext(t *testing.T) {
	authn := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if u, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
				r = r.WithContext(context.WithValue(r.Context(), userKey{}, u))
			}
			next.ServeHTTP(w, r)
		})
	}
	a := newTestAppBehind(t, authn, wakeshift.AttachOptions{Identify: func(ctx context.Context, r wakeshift.Request) (wakeshift.Identity, error) {
		if r.Token != nil {
			return wakeshift.Identity{}, errors.New("a token")
		}
		u, _ := ctx.Value(userKey{}).(string)
		return wakeshift.Identity{User: u}, nil
	}})
	var users []string
	mcpgo.AddTool(a.at, mcp.NewTool("whoami"),
		func(_ context.Context, call *wakeshift.ToolCall, _ mcp.CallToolRequest, _ tableIn) (*mcp.CallToolResult, error) {
			users = append(users, call.User)
			return mcp.NewToolResultText("hi"), nil
		}, wakeshift.Subscribes(a.turn))
	anon := client{ua: "claude-code/2.1.280", name: "claude-code"}
	a.call(t, claudeCode, "whoami", nil)
	a.call(t, anon, "whoami", nil)
	if strings.Join(users, ",") != "ada," {
		t.Fatalf("users: %q", users)
	}
	if res := a.call(t, claudeCode, "list_subscriptions", nil); res["isError"] == true {
		t.Fatalf("list_subscriptions as ada: %v", res)
	}
	// Without a user, it lists the calling conversation's subscriptions.
	if res := a.call(t, anon, "list_subscriptions", nil); res["isError"] == true {
		t.Fatalf("list_subscriptions without a user: %v", res)
	}
}

// A tool that subscribes says in tools/list that it's how the app wakes
// the conversation, in its events' own words, naming the app; a tool that
// subscribes to nothing is as it was. A Subscribe the tool doesn't declare
// fails.
func TestSubscribes(t *testing.T) {
	a := newTestApp(t, wakeshift.AttachOptions{})
	join := func(ctx context.Context, call *wakeshift.ToolCall, _ mcp.CallToolRequest, in tableIn) (*mcp.CallToolResult, error) {
		if _, err := a.turn.Subscribe(ctx, call, tableArgs{Table: in.Table}); err != nil {
			return nil, err
		}
		return mcp.NewToolResultText("Seated."), nil
	}
	mcpgo.AddTool(a.at, mcp.NewTool("join", mcp.WithDescription("Sit down."), mcp.WithString("table", mcp.Required())), join, wakeshift.Subscribes(a.turn))
	mcpgo.AddTool(a.at, mcp.NewTool("peek", mcp.WithDescription("Peek.")), join, wakeshift.Subscribes())
	tools := a.tools(t, claudeCode)
	if got, want := tools["join"]["description"], "Sit down. This tool is how Shipyard notifies you in this conversation of this event: Your turn. Shipyard delivers its notifications through Wakeshift; the result may ask you to start a background listener and to offer the user a one-time install. Follow it."; got != want {
		t.Errorf("join:\n got %q\nwant %q", got, want)
	}
	if got := tools["peek"]["description"]; got != "Peek." {
		t.Errorf("peek: %q", got)
	}
	props := tools["peek"]["inputSchema"].(map[string]any)["properties"].(map[string]any)
	if props["session_key"] == nil || props["harness"] != nil || props["conversation_id"] != nil {
		t.Errorf("peek's input schema: %v", props)
	}
	if res := a.call(t, claudeCode, "join", map[string]any{"table": "oak"}); res["isError"] == true {
		t.Fatalf("join: %v", res)
	}
	if res, rpcErr := a.rpc(t, claudeCode, "tools/call", map[string]any{"name": "peek", "arguments": map[string]any{"table": "oak"}}); rpcErr == nil && res["isError"] != true {
		t.Fatalf("peek subscribed to an event it doesn't declare: %v", res)
	}
}

// shape is a schema as the shared fixtures compare it: its type, each
// property's type, description and enum, and what's required, sorted.
func shape(v any) any {
	s, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := map[string]any{}
	if t, ok := s["type"]; ok {
		out["type"] = t
	}
	if ps, ok := s["properties"].(map[string]any); ok {
		props := map[string]any{}
		for name, p := range ps {
			pm, _ := p.(map[string]any)
			prop := map[string]any{}
			for _, k := range []string{"type", "description", "enum"} {
				if v, ok := pm[k]; ok {
					prop[k] = v
				}
			}
			if items, ok := pm["items"].(map[string]any); ok && items["type"] != nil {
				prop["items"] = map[string]any{"type": items["type"]}
			}
			props[name] = prop
		}
		if len(props) > 0 { // README: properties left out when it has none
			out["properties"] = props
		}
	}
	if r, ok := s["required"].([]any); ok && len(r) > 0 {
		names := make([]string, len(r))
		for i, n := range r {
			names[i], _ = n.(string)
		}
		sort.Strings(names)
		req := make([]any, len(names))
		for i, n := range names {
			req[i] = n
		}
		out["required"] = req
	}
	return out
}

// The tools/list an mcp-go server serves (subscribing-tools-list.json), for
// each scenario, through mcpgo.AddTool and through Wrap: the same as on
// go-sdk.
func TestSubscribingToolsListGolden(t *testing.T) {
	path := filepath.Join("..", "..", "..", "sdk", "typescript", "test", "subscribing-tools-list.json")
	if _, err := os.Stat(path); err != nil {
		path = filepath.Join("..", "testdata", "golden", "subscribing-tools-list.json")
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		t.Skip("no shared golden files here")
	}
	var f struct {
		App    string `json:"app"`
		Events map[string]struct {
			Description   string          `json:"description"`
			Access        string          `json:"access"`
			Args, Payload json.RawMessage `json:"-"`
			RawArgs       json.RawMessage `json:"args"`
			RawPayload    json.RawMessage `json:"payload"`
		} `json:"events"`
		VendorTools map[string]struct {
			Description  string          `json:"description"`
			InputSchema  json.RawMessage `json:"inputSchema"`
			OutputSchema json.RawMessage `json:"outputSchema"`
			Subscribes   []string        `json:"subscribes"`
		} `json:"vendor_tools"`
		Scenarios []struct {
			Name         string           `json:"name"`
			Enabled      bool             `json:"enabled"`
			Events       []string         `json:"events"`
			Tools        []string         `json:"tools"`
			BuiltinTools json.RawMessage  `json:"builtin_tools"`
			ToolsList    []map[string]any `json:"tools_list"`
		} `json:"scenarios"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	toolShape := func(tool map[string]any) map[string]any {
		out := map[string]any{"name": tool["name"], "description": tool["description"], "inputSchema": shape(tool["inputSchema"])}
		if title, ok := tool["title"]; ok {
			out["title"] = title
		}
		if o, ok := tool["outputSchema"]; ok && o != nil {
			out["outputSchema"] = shape(o)
		}
		if a, ok := tool["annotations"].(map[string]any); ok && len(a) > 0 && tool["name"] != nil {
			if _, builtin := map[string]bool{"subscribe_events": true, "unsubscribe_events": true, "list_subscriptions": true}[tool["name"].(string)]; builtin {
				out["annotations"] = a
			}
		}
		return out
	}
	for _, sc := range f.Scenarios {
		for _, how := range []string{"AddTool", "Wrap"} {
			t.Run(sc.Name+"/"+how, func(t *testing.T) {
				core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if strings.HasSuffix(r.URL.Path, "/app") {
						json.NewEncoder(w).Encode(map[string]any{"id": "app-test", "name": f.App})
						return
					}
					w.Write([]byte(`{}`))
				}))
				defer core.Close()
				key := testKey
				if !sc.Enabled {
					key = ""
				}
				wake, err := wakeshift.New(wakeshift.WithKey(key), wakeshift.WithCore(core.URL), wakeshift.WithMaxRetries(0))
				if err != nil {
					t.Fatal(err)
				}
				events := map[string]wakeshift.Subscribable{}
				for _, name := range sc.Events {
					e := f.Events[name]
					def := wakeshift.EventDef[map[string]any, map[string]any]{Description: e.Description, ArgsSchema: e.RawArgs, PayloadSchema: e.RawPayload}
					switch e.Access {
					case "personal":
						def.Personal = true
					case "public":
						def.Public = true
					case "authorize":
						def.Authorize = func(context.Context, wakeshift.Subscriber, map[string]any) (bool, error) { return true, nil }
					}
					ev, err := wakeshift.DefineEvent(wake, name, def)
					if err != nil {
						t.Fatal(err)
					}
					events[name] = ev
				}
				subscribes := func(names []string) wakeshift.ToolOption {
					var subs []wakeshift.Subscribable
					for _, n := range names {
						subs = append(subs, events[n])
					}
					return wakeshift.Subscribes(subs...)
				}
				opts := wakeshift.AttachOptions{}
				if string(sc.BuiltinTools) == "false" {
					opts.BuiltinTools = wakeshift.BuiltinTools{DisableAll: true}
				} else if len(sc.BuiltinTools) > 0 && string(sc.BuiltinTools) != "null" {
					var m map[string]bool
					json.Unmarshal(sc.BuiltinTools, &m)
					off := func(k string) bool { v, ok := m[k]; return ok && !v }
					opts.BuiltinTools = wakeshift.BuiltinTools{Wait: m["wait"], DisableSubscribe: off("subscribe"), DisableList: off("list"), DisableUnsubscribe: off("unsubscribe")}
				}
				if how == "Wrap" {
					opts.Wrap = map[string][]wakeshift.ToolOption{}
					for _, name := range sc.Tools {
						if vt := f.VendorTools[name]; vt.Subscribes != nil {
							opts.Wrap[name] = []wakeshift.ToolOption{subscribes(vt.Subscribes)}
						}
					}
				}
				srv := server.NewMCPServer("crawler", "1")
				at := mcpgo.Attach(wake, srv, opts)
				for _, name := range sc.Tools {
					vt := f.VendorTools[name]
					tool := mcp.Tool{Name: name, Description: vt.Description, RawInputSchema: vt.InputSchema}
					if len(vt.OutputSchema) > 0 {
						tool.RawOutputSchema = vt.OutputSchema
					}
					if vt.Subscribes == nil || how == "Wrap" {
						srv.AddTool(tool, func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
							return mcp.NewToolResultText("ok"), nil
						})
						continue
					}
					mcpgo.AddTool(at, tool, func(context.Context, *wakeshift.ToolCall, mcp.CallToolRequest, map[string]any) (*mcp.CallToolResult, error) {
						return mcp.NewToolResultText("ok"), nil
					}, subscribes(vt.Subscribes))
				}
				hs := httptest.NewServer(at.Handler(server.NewStreamableHTTPServer(srv)))
				defer hs.Close()
				b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": map[string]any{
					"_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientCapabilities": map[string]any{}}}})
				req, _ := http.NewRequest("POST", hs.URL, bytes.NewReader(b))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Accept", "application/json, text/event-stream")
				req.Header.Set("Mcp-Protocol-Version", "2026-07-28")
				req.Header.Set("Mcp-Method", "tools/list")
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				var out struct {
					Result struct {
						Tools []map[string]any `json:"tools"`
					} `json:"result"`
				}
				if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
					t.Fatal(err)
				}
				got, want := map[string]any{}, map[string]any{}
				for _, tool := range out.Result.Tools {
					got[tool["name"].(string)] = toolShape(tool)
				}
				for _, tool := range sc.ToolsList {
					want[tool["name"].(string)] = toolShape(tool)
				}
				if g, w := canonical(t, got), canonical(t, want); !reflect.DeepEqual(g, w) {
					gb, _ := json.MarshalIndent(g, "", "  ")
					wb, _ := json.MarshalIndent(w, "", "  ")
					t.Fatalf("tools/list:\n%s\nwant:\n%s", gb, wb)
				}
			})
		}
	}
}

func canonical(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	json.Unmarshal(b, &out)
	return out
}

// A tool handler middleware the vendor adds (WithToolHandlerMiddleware)
// never sees Wakeshift's arguments behind Handler, which takes them out of
// the request before mcp-go parses it; a tool that subscribes still gets
// the session through its call.
func TestToolMiddlewareNeverSeesSessionKey(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	logger := server.WithToolHandlerMiddleware(func(next server.ToolHandlerFunc) server.ToolHandlerFunc {
		return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			b, _ := json.Marshal(req.Params.Arguments)
			mu.Lock()
			seen = append(seen, string(b))
			mu.Unlock()
			return next(ctx, req)
		}
	})
	a := newTestApp(t, wakeshift.AttachOptions{}, logger)
	var session wakeshift.SessionKey
	mcpgo.AddTool(a.at, mcp.NewTool("join", mcp.WithString("table", mcp.Required())),
		func(ctx context.Context, call *wakeshift.ToolCall, _ mcp.CallToolRequest, in tableIn) (*mcp.CallToolResult, error) {
			session = call.Session()
			_, err := a.turn.Subscribe(ctx, call, tableArgs{Table: in.Table})
			return mcp.NewToolResultText("ok"), err
		}, wakeshift.Subscribes(a.turn))
	a.srv.AddTool(mcp.NewTool("look", mcp.WithString("table")), func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ok"), nil
	})
	const key = "wsc_canarycanarycanarycanary"
	for _, tool := range []string{"join", "look"} {
		a.call(t, claudeCode, tool, map[string]any{"table": "oak", "session_key": key, "harness": "claude-code", "conversation_id": "c1"})
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("the middleware saw %d calls", len(seen))
	}
	for _, s := range seen {
		if strings.Contains(s, "session_key") || strings.Contains(s, key) || strings.Contains(s, "harness") || strings.Contains(s, "conversation_id") {
			t.Errorf("the middleware saw %s", s)
		}
	}
	// The passed session_key goes to Wakeshift, never to the tool: its
	// call's Session is empty until Wakeshift resolves the conversation.
	if session != "" {
		t.Errorf("call.Session() before subscribing: %q", string(session))
	}
}

// Wakeshift's events capability is on server/discover only while an event
// is open to MCP Events clients: with every event tools-only it's absent,
// with a public one present.
// The events capability is advertised with any event, tools-only ones
// included: a native client subscribes to those with the subscribe voucher
// a tool handed it (docs/subscribe-vouchers.md §4.3).
func TestEventsCapabilityWithAnyEvent(t *testing.T) {
	for _, open := range []bool{false, true} {
		app, err := wakeshift.New(wakeshift.WithKey(testKey), wakeshift.WithCore("https://core.example"), wakeshift.WithMaxRetries(0))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := wakeshift.DefineEvent(app, "table.news", wakeshift.EventDef[tableArgs, turnMsg]{Description: "News", Public: open}); err != nil {
			t.Fatal(err)
		}
		srv := server.NewMCPServer("t", "1.0.0")
		at := mcpgo.Attach(app, srv, wakeshift.AttachOptions{})
		hs := httptest.NewServer(at.Handler(server.NewStreamableHTTPServer(srv)))
		defer hs.Close()
		a := &testApp{url: hs.URL}
		res, rpcErr := a.rpc(t, client{ua: "claude-code/2", name: "claude-code"}, "server/discover", nil)
		if rpcErr != nil {
			t.Fatalf("discover: %v", rpcErr)
		}
		caps, _ := res["capabilities"].(map[string]any)
		ext, _ := caps["extensions"].(map[string]any)
		if got := caps["events"] != nil && ext["io.modelcontextprotocol/events"] != nil; !got {
			t.Errorf("open %v: capabilities %v", open, caps)
		}
	}
}

// WrapFunc on mcp-go: a tool registered with srv.AddTool that WrapFunc
// names is listed with the subscribes sentence; one it answers nil for is
// left alone.
func TestWrapFunc(t *testing.T) {
	app, err := wakeshift.New(wakeshift.WithKey(testKey), wakeshift.WithCore("https://core.example"), wakeshift.WithMaxRetries(0))
	if err != nil {
		t.Fatal(err)
	}
	news, err := wakeshift.DefineEvent(app, "table.news", wakeshift.EventDef[tableArgs, turnMsg]{Description: "News at a table"})
	if err != nil {
		t.Fatal(err)
	}
	srv := server.NewMCPServer("t", "1.0.0")
	for _, name := range []string{"watch_build", "get_page"} {
		srv.AddTool(mcp.NewTool(name, mcp.WithDescription("A tool."), mcp.WithString("table")), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("ok"), nil
		})
	}
	at := mcpgo.Attach(app, srv, wakeshift.AttachOptions{WrapFunc: func(tool string) []wakeshift.ToolOption {
		if strings.HasPrefix(tool, "watch_") {
			return []wakeshift.ToolOption{wakeshift.Subscribes(news)}
		}
		return nil
	}})
	hs := httptest.NewServer(at.Handler(server.NewStreamableHTTPServer(srv)))
	defer hs.Close()
	a := &testApp{url: hs.URL}
	res, rpcErr := a.rpc(t, client{ua: "claude-code/2", name: "claude-code"}, "tools/list", nil)
	if rpcErr != nil {
		t.Fatalf("tools/list: %v", rpcErr)
	}
	for _, tl := range res["tools"].([]any) {
		m := tl.(map[string]any)
		d, _ := m["description"].(string)
		switch m["name"] {
		case "watch_build":
			if !strings.Contains(d, "of this event: News at a table") {
				t.Errorf("watch_build: %q", d)
			}
		case "get_page":
			if d != "A tool." {
				t.Errorf("get_page: %q", d)
			}
		}
	}
}

// A handler that passes call.Subscribe's identify refusal on (as an error,
// or in an error result): the agent reads neutral words, never the
// developer's fix ("Check identify…", which the log has).
func TestIdentifyRefusalReadsNeutrally(t *testing.T) {
	a := newTestApp(t, wakeshift.AttachOptions{Identify: func(context.Context, wakeshift.Request) (wakeshift.Identity, error) {
		return wakeshift.Identity{}, errors.New("boom-canary")
	}})
	for _, how := range []string{"error", "isError"} {
		mcpgo.AddTool(a.at, mcp.NewTool("strict_"+how),
			func(ctx context.Context, call *wakeshift.ToolCall, _ mcp.CallToolRequest, in tableIn) (*mcp.CallToolResult, error) {
				_, err := a.turn.Subscribe(ctx, call, tableArgs{Table: in.Table})
				if err != nil && how == "isError" {
					return mcp.NewToolResultError(err.Error()), nil
				}
				return nil, err
			}, wakeshift.Subscribes(a.turn))
	}
	for _, how := range []string{"error", "isError"} {
		res, rpcErr := a.rpc(t, claudeCode, "tools/call", map[string]any{"name": "strict_" + how, "arguments": map[string]any{"table": "oak"}})
		got := strings.Join(texts(res), "\n")
		if rpcErr != nil || res["isError"] != true || got != "This app couldn't verify who you are, so it can't subscribe you to anything now." {
			t.Fatalf("%s: %v %v %q", how, res, rpcErr, got)
		}
	}
}
