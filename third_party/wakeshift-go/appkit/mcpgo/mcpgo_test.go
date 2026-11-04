package mcpgo_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/modelcontextprotocol/go-sdk/auth"

	"github.com/wakeshift/wakeshift-go/appkit"
	"github.com/wakeshift/wakeshift-go/appkit/mcpgo"
)

// The adapter against a fake core that records what the SDK asks it, on an
// mcp-go Streamable HTTP server behind Wrap, called as a protocol
// 2026-07-28 client: the block, rollback, native callers, the schemas, the
// built-in tools and what Wrap adds to server/discover and tools/list.

const testTrailer = "(For Wakeshift's hook on this computer, not for you: wakeshift-beacon: aaaaaaaaaa.bbbbbbbbbb.cccccccccc)"

type fakeCore struct {
	mu    sync.Mutex
	calls []fakeCall
	subs  int
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
	appkit.WakeArgs
}

type view struct {
	Table string `json:"table"`
}

type testApp struct {
	core *fakeCore
	app  *appkit.App
	srv  *server.MCPServer
	at   *mcpgo.Attachment
	turn *appkit.DefinedEvent[tableArgs, turnMsg]
	url  string
}

// newTestApp serves an app with a personal event on an mcp-go server
// (srvOpts), whose bearer token is the user's ID, behind Wrap.
func newTestApp(t *testing.T, opts mcpgo.AttachOptions, srvOpts ...server.ServerOption) *testApp {
	t.Helper()
	verify := func(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		return &auth.TokenInfo{UserID: token, Expiration: time.Now().Add(time.Hour)}, nil
	}
	return newTestAppBehind(t, auth.RequireBearerToken(verify, nil), opts, srvOpts...)
}

// newTestAppBehind is newTestApp behind the endpoint's own authentication.
func newTestAppBehind(t *testing.T, authn func(http.Handler) http.Handler, opts mcpgo.AttachOptions, srvOpts ...server.ServerOption) *testApp {
	t.Helper()
	a := &testApp{core: &fakeCore{}}
	cs := httptest.NewServer(http.HandlerFunc(a.core.handler))
	t.Cleanup(cs.Close)
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	a.app = &appkit.App{ID: "app_test", Key: key, Core: cs.URL}
	var err error
	if a.turn, err = appkit.DefineEvent(a.app, "game.turn", appkit.EventDef[tableArgs, turnMsg]{Description: "Your turn.", Personal: true,
		Text: func(m turnMsg, _ tableArgs) string { return m.Text }}); err != nil {
		t.Fatal(err)
	}
	a.srv = server.NewMCPServer("t", "1.0.0", srvOpts...)
	if opts.Identify == nil {
		opts.Identify = func(_ context.Context, ti *auth.TokenInfo) (appkit.Identity, error) {
			if ti == nil {
				return appkit.Identity{}, nil
			}
			return appkit.Identity{User: ti.UserID}, nil
		}
	}
	a.at = mcpgo.Attach(a.app, a.srv, opts)
	hs := httptest.NewServer(authn(a.at.Wrap(server.NewStreamableHTTPServer(a.srv))))
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
		caps["extensions"] = map[string]any{appkit.TasksExtension: map[string]any{}}
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
	a := newTestApp(t, mcpgo.AttachOptions{})
	var got appkit.Subscription
	a.srv.AddTools(mcpgo.Events(a.at, mcp.NewTool("join", mcp.WithDescription("Sit down"), mcp.WithString("table", mcp.Required())),
		func(ctx context.Context, call *appkit.ToolCall, req mcp.CallToolRequest, in tableIn) (*mcp.CallToolResult, error) {
			if call.User != "ada" || call.Tool != "join" || call.Native || call.Request != nil || req.Params.Name != "join" {
				return nil, fmt.Errorf("the call: %+v", call)
			}
			if appkit.ToolCallFrom(ctx) != call {
				return nil, errors.New("ToolCallFrom")
			}
			var err error
			if got, err = a.turn.Subscribe(ctx, call, tableArgs{Table: in.Table}, appkit.Reason("Playing")); err != nil {
				return nil, err
			}
			return mcp.NewToolResultText("Seated at " + in.Table + "."), nil
		}))
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
	a := newTestApp(t, mcpgo.AttachOptions{Tools: appkit.Tools{Wait: appkit.Wait{Enabled: true}}})
	a.srv.AddTools(mcpgo.Events(a.at, mcp.NewTool("join"),
		func(ctx context.Context, call *appkit.ToolCall, _ mcp.CallToolRequest, in tableIn) (*mcp.CallToolResult, error) {
			_, err := a.turn.Subscribe(ctx, call, tableArgs{Table: in.Table})
			return nil, err
		}))
	c := claudeCode
	c.tasks = true
	if _, ok := a.tools(t, c)[appkit.WaitTool]; !ok {
		t.Fatalf("no %s for a tasks client", appkit.WaitTool)
	}
	if _, ok := a.tools(t, claudeCode)[appkit.WaitTool]; ok {
		t.Fatalf("%s for a client without tasks", appkit.WaitTool)
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
	a := newTestApp(t, mcpgo.AttachOptions{})
	a.srv.AddTools(mcpgo.Events(a.at, mcp.NewTool("join"),
		func(ctx context.Context, call *appkit.ToolCall, _ mcp.CallToolRequest, in tableIn) (*mcp.CallToolResult, error) {
			_, err := a.turn.Subscribe(ctx, call, tableArgs{Table: in.Table})
			return nil, err
		}))
	c := claudeCode
	c.tasks = true
	if _, ok := a.tools(t, c)[appkit.WaitTool]; ok {
		t.Fatalf("%s without Wait", appkit.WaitTool)
	}
	a.call(t, c, "join", map[string]any{"table": "oak"})
	if call := a.core.bodies("/event-subscriptions")[0]["call"].(map[string]any); call["tasks"] != nil {
		t.Fatalf("the call: %v", call)
	}
	if _, rpcErr := a.rpc(t, c, "tasks/get", map[string]any{"taskId": appkit.TaskIDPrefix + "x"}); rpcErr == nil {
		t.Fatal("tasks/get for a Wakeshift ID was answered without Wait")
	}
	if n := len(a.core.bodies("/calls/task/get")); n != 0 {
		t.Fatalf("asked Wakeshift %d times", n)
	}
}

// A failed call (an error, an isError result, a panic) ends the
// subscriptions it made, and gets no block.
func TestFailedCallsRollBack(t *testing.T) {
	a := newTestApp(t, mcpgo.AttachOptions{}, server.WithRecovery())
	for _, fail := range []string{"error", "isError", "panic"} {
		a.srv.AddTools(mcpgo.Events(a.at, mcp.NewTool("fails_"+fail),
			func(ctx context.Context, call *appkit.ToolCall, _ mcp.CallToolRequest, in tableIn) (*mcp.CallToolResult, error) {
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
			}))
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
	a := newTestApp(t, mcpgo.AttachOptions{}, server.WithOutputSchemaValidation(), server.WithInputSchemaValidation())
	a.srv.AddTools(mcpgo.Events(a.at, mcp.NewTool("look", mcp.WithInputSchema[tableIn](), mcp.WithOutputSchema[view]()),
		func(ctx context.Context, call *appkit.ToolCall, _ mcp.CallToolRequest, in tableIn) (*mcp.CallToolResult, error) {
			if _, err := a.turn.Subscribe(ctx, call, tableArgs{Table: in.Table}); err != nil {
				return nil, err
			}
			return mcp.NewToolResultStructuredOnly(view{Table: in.Table}), nil
		}))
	look := a.tools(t, claudeCode)["look"]
	if ap := look["inputSchema"].(map[string]any)["additionalProperties"]; ap == nil || ap == true {
		t.Fatalf("the input schema isn't closed, so it can't show an undeclared argument dropped: %v", look["inputSchema"])
	}
	props := look["inputSchema"].(map[string]any)["properties"].(map[string]any)
	for _, name := range []string{"table", appkit.SessionArg, appkit.HarnessArg, appkit.ConversationArg} {
		if props[name] == nil {
			t.Fatalf("input schema: no %s: %v", name, props)
		}
	}
	if d, _ := props[appkit.HarnessArg].(map[string]any)["description"].(string); !strings.HasPrefix(d, "The program you're running in: codex, claude-code,") {
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
	a := newTestApp(t, mcpgo.AttachOptions{Events: mcpgo.EventsOptions{Native: func(_ context.Context, req mcp.CallToolRequest) bool {
		return req.Header.Get("User-Agent") == "house/1"
	}}})
	a.srv.AddTools(mcpgo.Events(a.at, mcp.NewTool("join"),
		func(ctx context.Context, call *appkit.ToolCall, _ mcp.CallToolRequest, in tableIn) (*mcp.CallToolResult, error) {
			if !call.Native {
				return nil, errors.New("not native")
			}
			sub, err := a.turn.Subscribe(ctx, call, tableArgs{Table: in.Table})
			if err != nil || !sub.Native {
				return nil, fmt.Errorf("%+v %v", sub, err)
			}
			return mcp.NewToolResultText("ok"), nil
		}))
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
	if res, rpcErr := newTestApp(t, mcpgo.AttachOptions{Tools: appkit.Tools{Wait: appkit.Wait{Enabled: true}}}).rpc(t, claudeCode, "server/discover", nil); rpcErr != nil {
		t.Fatalf("discover: %v", rpcErr)
	} else if ext, _ := res["capabilities"].(map[string]any)["extensions"].(map[string]any); ext[appkit.TasksExtension] == nil {
		t.Fatalf("with Wait, capabilities: %v", res["capabilities"])
	}
	a := newTestApp(t, mcpgo.AttachOptions{})
	res, rpcErr := a.rpc(t, claudeCode, "server/discover", nil)
	if rpcErr != nil {
		t.Fatalf("discover: %v", rpcErr)
	}
	caps := res["capabilities"].(map[string]any)
	ext, _ := caps["extensions"].(map[string]any)
	if ext["io.modelcontextprotocol/events"] == nil || ext[appkit.TasksExtension] != nil || caps["tools"] == nil {
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
	for _, name := range []string{appkit.SubscribeEventsTool, appkit.UnsubscribeEventsTool, appkit.ListSubscriptionsTool} {
		if tools[name] == nil {
			t.Fatalf("no %s: %v", name, tools)
		}
	}
	evs, rpcErr := a.rpc(t, claudeCode, "events/list", nil)
	if rpcErr != nil || len(evs["events"].([]any)) != 1 {
		t.Fatalf("events/list: %v %v", evs, rpcErr)
	}
	sub := a.call(t, claudeCode, appkit.SubscribeEventsTool, map[string]any{"event": "game.turn", "arguments": map[string]any{"table": "oak"}})
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
	for name, tool := range newTestApp(t, mcpgo.AttachOptions{Tools: appkit.Tools{Wait: appkit.Wait{Enabled: true}}}).tools(t, c) {
		got[name] = tool["annotations"]
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tools/list's annotations\n got %v\nwant %v", got, want)
	}
}

// A call with no conversation and no subscription gets nothing added.
func TestNothingToAdd(t *testing.T) {
	a := newTestApp(t, mcpgo.AttachOptions{})
	a.srv.AddTools(mcpgo.Events(a.at, mcp.NewTool("peek"),
		func(context.Context, *appkit.ToolCall, mcp.CallToolRequest, tableIn) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("nothing here"), nil
		}))
	res := a.call(t, claudeCode, "peek", map[string]any{"table": "oak"})
	if strings.Join(texts(res), "|") != "nothing here" || len(a.core.paths()) != 0 {
		t.Fatalf("peek: %q %v", texts(res), a.core.paths())
	}
}

// A client on an earlier protocol, through initialize: Wrap adds the
// events capability to its answer, and a wrapped tool's call in the
// session names the client initialize named.
func TestInitializeSession(t *testing.T) {
	a := newTestApp(t, mcpgo.AttachOptions{})
	a.srv.AddTools(mcpgo.Events(a.at, mcp.NewTool("join"),
		func(ctx context.Context, call *appkit.ToolCall, _ mcp.CallToolRequest, in tableIn) (*mcp.CallToolResult, error) {
			_, err := a.turn.Subscribe(ctx, call, tableArgs{Table: in.Table})
			return mcp.NewToolResultText("ok"), err
		}))
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
	a := newTestAppBehind(t, authn, mcpgo.AttachOptions{Identify: func(ctx context.Context, ti *auth.TokenInfo) (appkit.Identity, error) {
		if ti != nil {
			return appkit.Identity{}, errors.New("a token")
		}
		u, _ := ctx.Value(userKey{}).(string)
		return appkit.Identity{User: u}, nil
	}})
	var users []string
	a.srv.AddTools(mcpgo.Events(a.at, mcp.NewTool("whoami"),
		func(_ context.Context, call *appkit.ToolCall, _ mcp.CallToolRequest, _ tableIn) (*mcp.CallToolResult, error) {
			users = append(users, call.User)
			return mcp.NewToolResultText("hi"), nil
		}))
	anon := client{ua: "claude-code/2.1.280", name: "claude-code"}
	a.call(t, claudeCode, "whoami", nil)
	a.call(t, anon, "whoami", nil)
	if strings.Join(users, ",") != "ada," {
		t.Fatalf("users: %q", users)
	}
	if res := a.call(t, claudeCode, appkit.ListSubscriptionsTool, nil); res["isError"] == true {
		t.Fatalf("list_subscriptions as ada: %v", res)
	}
	if res := a.call(t, anon, appkit.ListSubscriptionsTool, nil); res["isError"] != true {
		t.Fatalf("list_subscriptions without a user: %v", res)
	}
}

// A tool that declares its events (appkit.Subscribes) says in tools/list
// that it's how the app notifies the agent, in the events' own words,
// naming the app; a Notifies description is named too, and keeps its own
// sentence; a tool that declares nothing is as it was. A declared Subscribe
// works as before.
func TestSubscribes(t *testing.T) {
	a := newTestApp(t, mcpgo.AttachOptions{})
	join := func(ctx context.Context, call *appkit.ToolCall, _ mcp.CallToolRequest, in tableIn) (*mcp.CallToolResult, error) {
		if _, err := a.turn.Subscribe(ctx, call, tableArgs{Table: in.Table}); err != nil {
			return nil, err
		}
		return mcp.NewToolResultText("Seated."), nil
	}
	a.srv.AddTools(
		mcpgo.Events(a.at, mcp.NewTool("join", mcp.WithDescription("Sit down."), mcp.WithString("table", mcp.Required())), join, appkit.Subscribes(a.turn)),
		mcpgo.Events(a.at, mcp.NewTool("watch", mcp.WithDescription(appkit.Notifies("Watch a table.", appkit.NotifyWhen("it's your turn"))), mcp.WithString("table", mcp.Required())), join, appkit.Subscribes(a.turn)),
		mcpgo.Events(a.at, mcp.NewTool("peek", mcp.WithDescription("Peek.")), join, appkit.Subscribes()),
	)
	tools := a.tools(t, claudeCode)
	tail := " Shipyard delivers its notifications through Wakeshift; the result may ask you to start a background listener and to offer the user a one-time install. Follow it."
	if got, want := tools["join"]["description"], "Sit down. This tool is how Shipyard notifies you in this conversation of this event: Your turn."+tail; got != want {
		t.Errorf("join:\n got %q\nwant %q", got, want)
	}
	if got, want := tools["watch"]["description"], "Watch a table. This tool is how Shipyard notifies you in this conversation when it's your turn."+tail; got != want {
		t.Errorf("watch:\n got %q\nwant %q", got, want)
	}
	if got := tools["peek"]["description"]; got != "Peek." {
		t.Errorf("peek: %q", got)
	}
	if res := a.call(t, claudeCode, "join", map[string]any{"table": "oak"}); res["isError"] == true {
		t.Fatalf("join: %v", res)
	}
	if res := a.call(t, claudeCode, "peek", map[string]any{"table": "oak"}); res["isError"] == true {
		t.Fatalf("peek (undeclared: it warns, and works): %v", res)
	}
}
