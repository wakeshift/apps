package mcpgo_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/wakeshift/wakeshift-go"
	"github.com/wakeshift/wakeshift-go/mcpgo"
)

// spy records what the vendor's hooks, middleware and handlers saw.
type spy struct {
	mu   sync.Mutex
	seen map[string][]string // by who saw it
}

func (s *spy) saw(who string, v any) {
	b, _ := json.Marshal(v)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen == nil {
		s.seen = map[string][]string{}
	}
	s.seen[who] = append(s.seen[who], string(b))
}

// leaked is who saw one of Wakeshift's arguments, among whos.
func (s *spy) leaked(whos ...string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, who := range whos {
		for _, v := range s.seen[who] {
			if strings.Contains(v, "session_key") || strings.Contains(v, "conversation_id") || strings.Contains(v, `"harness"`) || strings.Contains(v, wrapKey) {
				out = append(out, who+": "+v)
			}
		}
		if len(s.seen[who]) == 0 {
			out = append(out, who+": saw nothing (it didn't run)")
		}
	}
	return out
}

const wrapKey = "wsc_wrapwrapwrapwrapwrapwrapwr"

// wrapServer is an mcp-go server whose tools are registered with plain
// srv.AddTool (not mcpgo.AddTool) and declared through Wrap (watch_build)
// and WrapFunc (watch_deploy), with the vendor's hooks and tool handler
// middleware, added before and after Attach, recording what they see.
func wrapServer(t *testing.T) (*fakeCore, *server.MCPServer, *mcpgo.Attachment, *spy) {
	t.Helper()
	core := &fakeCore{}
	cs := httptest.NewServer(http.HandlerFunc(core.handler))
	t.Cleanup(cs.Close)
	app, err := wakeshift.New(wakeshift.WithKey(testKey), wakeshift.WithCore(cs.URL), wakeshift.WithMaxRetries(0))
	if err != nil {
		t.Fatal(err)
	}
	news, err := wakeshift.DefineEvent(app, "table.news", wakeshift.EventDef[tableArgs, turnMsg]{Description: "News at a table"})
	if err != nil {
		t.Fatal(err)
	}
	sp := &spy{}
	hooks := &server.Hooks{}
	hooks.AddBeforeAny(func(_ context.Context, _ any, method mcp.MCPMethod, message any) {
		if method == mcp.MethodToolsCall {
			sp.saw("BeforeAny", message)
		}
	})
	hooks.AddOnRequestInitialization(func(_ context.Context, _ any, message any) error {
		// message is the request's json.RawMessage: marshaled, it's the JSON
		// whatever the Go version (fmt prints it as bytes before Go 1.26).
		if b, _ := json.Marshal(message); strings.Contains(string(b), "tools/call") {
			sp.saw("OnRequestInitialization", message)
		}
		return nil
	})
	hooks.AddBeforeCallTool(func(_ context.Context, _ any, req *mcp.CallToolRequest) {
		sp.saw("BeforeCallTool", req.Params.Arguments)
	})
	mw := func(who string) server.ToolHandlerMiddleware {
		return func(next server.ToolHandlerFunc) server.ToolHandlerFunc {
			return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				sp.saw(who, req.Params.Arguments)
				return next(ctx, req)
			}
		}
	}
	srv := server.NewMCPServer("t", "1.0.0", server.WithHooks(hooks), server.WithToolHandlerMiddleware(mw("middleware before Attach")))
	for _, name := range []string{"watch_build", "watch_deploy"} {
		srv.AddTool(mcp.NewTool(name, mcp.WithDescription("Watch it."), mcp.WithString("table")), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			sp.saw("handler", req.Params.Arguments)
			call := wakeshift.ToolCallFrom(ctx)
			if call == nil {
				return mcp.NewToolResultError("no call"), nil
			}
			if _, err := news.Subscribe(ctx, call, tableArgs{Table: req.GetString("table", "")}); err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return mcp.NewToolResultText("Watching."), nil
		})
	}
	at := mcpgo.Attach(app, srv, wakeshift.AttachOptions{
		Wrap: map[string][]wakeshift.ToolOption{"watch_build": {wakeshift.Subscribes(news)}},
		WrapFunc: func(tool string) []wakeshift.ToolOption {
			if tool == "watch_deploy" {
				return []wakeshift.ToolOption{wakeshift.Subscribes(news)}
			}
			return nil
		},
	})
	srv.Use(mw("middleware after Attach"))
	hooks.AddBeforeCallTool(func(_ context.Context, _ any, req *mcp.CallToolRequest) {
		sp.saw("BeforeCallTool after Attach", req.Params.Arguments)
	})
	return core, srv, at, sp
}

// wrapArgs are a call's arguments with all three of Wakeshift's.
func wrapArgs() map[string]any {
	return map[string]any{"table": "oak", "session_key": wrapKey, "harness": "claude-code", "conversation_id": "c1"}
}

// checkWrapped checks a call of a wrapped tool: it subscribed with the
// session it passed, and its result carries Wakeshift's block.
func checkWrapped(t *testing.T, core *fakeCore, tool string, res map[string]any) {
	t.Helper()
	if res["isError"] == true || !strings.Contains(fmt.Sprint(res["content"]), "Wakeshift: this conversation is woken by Monitor.") {
		t.Errorf("%s: no block: %v", tool, res)
	}
	found := false
	for _, b := range core.bodies("/event-subscriptions") {
		if c, _ := b["call"].(map[string]any); c["tool"] == tool {
			found = true
			if c["session"] != wrapKey || c["harness"] != "claude-code" || c["conversation"] != "c1" {
				t.Errorf("%s: the call Wakeshift got: %v", tool, c)
			}
		}
	}
	if !found {
		t.Errorf("%s didn't subscribe: %v", tool, core.paths())
	}
}

// Over HTTP, through Handler: a tool registered elsewhere and named in
// Wrap or WrapFunc finds its call, subscribes, and gets Wakeshift's block;
// no hook, middleware or handler of the vendor's sees Wakeshift's arguments.
func TestWrapEndToEndHTTP(t *testing.T) {
	core, srv, at, sp := wrapServer(t)
	hs := httptest.NewServer(at.Handler(server.NewStreamableHTTPServer(srv)))
	defer hs.Close()
	a := &testApp{url: hs.URL}
	for _, tool := range []string{"watch_build", "watch_deploy"} {
		res, rpcErr := a.rpc(t, claudeCode, "tools/call", map[string]any{"name": tool, "arguments": wrapArgs()})
		if rpcErr != nil {
			t.Fatalf("%s: %v", tool, rpcErr)
		}
		checkWrapped(t, core, tool, res)
	}
	if l := sp.leaked("BeforeAny", "OnRequestInitialization", "BeforeCallTool", "BeforeCallTool after Attach", "middleware before Attach", "middleware after Attach", "handler"); len(l) > 0 {
		t.Errorf("leaked: %v", l)
	}
}

// stdioSession runs srv's stdio server on in (attached.Stdin's, or the raw
// one) and answers each JSON-RPC request written to it.
type stdioSession struct {
	w   *io.PipeWriter
	out *bufio.Reader
	id  int
}

func startStdio(t *testing.T, srv *server.MCPServer, wrap func(io.Reader) io.Reader) *stdioSession {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.NewStdioServer(srv).Listen(ctx, wrap(inR), outW)
	}()
	t.Cleanup(func() {
		cancel()
		inW.Close()
		outR.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})
	s := &stdioSession{w: inW, out: bufio.NewReader(outR)}
	s.request(t, "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "claude-code", "version": "2"}})
	io.WriteString(inW, `{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n")
	return s
}

func (s *stdioSession) request(t *testing.T, method string, params any) map[string]any {
	t.Helper()
	s.id++
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": s.id, "method": method, "params": params})
	if _, err := s.w.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
	for {
		line, err := s.out.ReadString('\n')
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		var m struct {
			ID     any            `json:"id"`
			Result map[string]any `json:"result"`
			Error  map[string]any `json:"error"`
		}
		if json.Unmarshal([]byte(line), &m) != nil || fmt.Sprint(m.ID) != fmt.Sprint(s.id) {
			continue // a notification
		}
		if m.Error != nil {
			t.Fatalf("%s: %v", method, m.Error)
		}
		return m.Result
	}
}

// On stdio through attached.Stdin (what mcpgo.ServeStdio serves): the same
// as over HTTP, and no hook (BeforeAny, OnRequestInitialization,
// BeforeCallTool, whenever added) or middleware sees Wakeshift's arguments.
func TestWrapEndToEndStdio(t *testing.T) {
	core, srv, at, sp := wrapServer(t)
	s := startStdio(t, srv, func(r io.Reader) io.Reader { return at.Stdin(r) })
	for _, tool := range []string{"watch_build", "watch_deploy"} {
		checkWrapped(t, core, tool, s.request(t, "tools/call", map[string]any{"name": tool, "arguments": wrapArgs()}))
	}
	if l := sp.leaked("BeforeAny", "OnRequestInitialization", "BeforeCallTool", "BeforeCallTool after Attach", "middleware before Attach", "middleware after Attach", "handler"); len(l) > 0 {
		t.Errorf("leaked: %v", l)
	}
}

// Without Handler or Stdin (stdio served on the raw stdin, as an
// in-process transport is): Wrap and WrapFunc still work, and Wakeshift's
// arguments are taken out in Attach's BeforeCallTool hook, before every
// tool handler middleware and the handler. Only hooks that run before it
// see them (documented).
func TestWrapEndToEndRawStdio(t *testing.T) {
	core, srv, _, sp := wrapServer(t)
	s := startStdio(t, srv, func(r io.Reader) io.Reader { return r })
	for _, tool := range []string{"watch_build", "watch_deploy"} {
		checkWrapped(t, core, tool, s.request(t, "tools/call", map[string]any{"name": tool, "arguments": wrapArgs()}))
	}
	if l := sp.leaked("BeforeCallTool after Attach", "middleware before Attach", "middleware after Attach", "handler"); len(l) > 0 {
		t.Errorf("leaked: %v", l)
	}
	// The limitation, and proof the spy catches a leak: a hook that runs
	// before Attach's sees them here.
	if len(sp.leaked("BeforeAny")) == 0 {
		t.Errorf("BeforeAny didn't see Wakeshift's arguments without Stdin: %v", sp.seen["BeforeAny"])
	}
}

// mcpgo.AddTool's tools on stdio through attached.Stdin: hooks and
// middleware never see Wakeshift's arguments either.
func TestAddToolStdioHooksNeverSeeWakeArgs(t *testing.T) {
	core := &fakeCore{}
	cs := httptest.NewServer(http.HandlerFunc(core.handler))
	defer cs.Close()
	app, err := wakeshift.New(wakeshift.WithKey(testKey), wakeshift.WithCore(cs.URL), wakeshift.WithMaxRetries(0))
	if err != nil {
		t.Fatal(err)
	}
	news, err := wakeshift.DefineEvent(app, "table.news", wakeshift.EventDef[tableArgs, turnMsg]{Description: "News at a table"})
	if err != nil {
		t.Fatal(err)
	}
	sp := &spy{}
	hooks := &server.Hooks{}
	hooks.AddBeforeAny(func(_ context.Context, _ any, method mcp.MCPMethod, message any) {
		if method == mcp.MethodToolsCall {
			sp.saw("BeforeAny", message)
		}
	})
	srv := server.NewMCPServer("t", "1.0.0", server.WithHooks(hooks))
	at := mcpgo.Attach(app, srv, wakeshift.AttachOptions{})
	srv.Use(func(next server.ToolHandlerFunc) server.ToolHandlerFunc {
		return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			sp.saw("middleware", req.Params.Arguments)
			return next(ctx, req)
		}
	})
	mcpgo.AddTool(at, mcp.NewTool("watch", mcp.WithString("table")), func(ctx context.Context, call *wakeshift.ToolCall, req mcp.CallToolRequest, in tableIn) (*mcp.CallToolResult, error) {
		sp.saw("handler", req.Params.Arguments)
		if _, err := news.Subscribe(ctx, call, tableArgs{Table: in.Table}); err != nil {
			return nil, err
		}
		return mcp.NewToolResultText("Watching."), nil
	}, wakeshift.Subscribes(news))
	s := startStdio(t, srv, func(r io.Reader) io.Reader { return at.Stdin(r) })
	checkWrapped(t, core, "watch", s.request(t, "tools/call", map[string]any{"name": "watch", "arguments": wrapArgs()}))
	if l := sp.leaked("BeforeAny", "middleware", "handler"); len(l) > 0 {
		t.Errorf("leaked: %v", l)
	}
}

// Over HTTP without Handler: Wrap still works, each call gets its own
// arguments back (they're kept by the request, not shared), and tool
// middleware and handlers never see them.
func TestWrapEndToEndHTTPWithoutHandler(t *testing.T) {
	core, srv, _, sp := wrapServer(t)
	hs := httptest.NewServer(server.NewStreamableHTTPServer(srv))
	defer hs.Close()
	a := &testApp{url: hs.URL}
	for _, tool := range []string{"watch_build", "watch_deploy"} {
		res, rpcErr := a.rpc(t, claudeCode, "tools/call", map[string]any{"name": tool, "arguments": wrapArgs()})
		if rpcErr != nil {
			t.Fatalf("%s: %v", tool, rpcErr)
		}
		checkWrapped(t, core, tool, res)
	}
	if l := sp.leaked("BeforeCallTool after Attach", "middleware before Attach", "middleware after Attach", "handler"); len(l) > 0 {
		t.Errorf("leaked: %v", l)
	}
}

// On stdio, Identify's Request.Header is nil (mcp-go hands an empty one):
// the same as on go-sdk, so r.Header == nil means stdio.
func TestStdioHeaderIsNil(t *testing.T) {
	core := &fakeCore{}
	cs := httptest.NewServer(http.HandlerFunc(core.handler))
	t.Cleanup(cs.Close)
	app, err := wakeshift.New(wakeshift.WithKey(testKey), wakeshift.WithCore(cs.URL), wakeshift.WithMaxRetries(0))
	if err != nil {
		t.Fatal(err)
	}
	news, _ := wakeshift.DefineEvent(app, "table.news", wakeshift.EventDef[tableArgs, turnMsg]{Description: "News at a table"})
	var mu sync.Mutex
	var headers []http.Header
	srv := server.NewMCPServer("t", "1.0.0")
	at := mcpgo.Attach(app, srv, wakeshift.AttachOptions{Identify: func(_ context.Context, r wakeshift.Request) (wakeshift.Identity, error) {
		mu.Lock()
		headers = append(headers, r.Header)
		mu.Unlock()
		return wakeshift.Identity{User: "ada"}, nil
	}})
	mcpgo.AddTool(at, mcp.NewTool("watch", mcp.WithString("table")), func(ctx context.Context, call *wakeshift.ToolCall, _ mcp.CallToolRequest, in tableIn) (*mcp.CallToolResult, error) {
		_, err := news.Subscribe(ctx, call, tableArgs{Table: in.Table})
		return mcp.NewToolResultText("ok"), err
	}, wakeshift.Subscribes(news))
	s := startStdio(t, srv, func(r io.Reader) io.Reader { return at.Stdin(r) })
	s.request(t, "tools/call", map[string]any{"name": "watch", "arguments": map[string]any{"table": "oak"}})
	mu.Lock()
	defer mu.Unlock()
	if len(headers) != 1 || headers[0] != nil {
		t.Fatalf("headers: %#v", headers)
	}
}
