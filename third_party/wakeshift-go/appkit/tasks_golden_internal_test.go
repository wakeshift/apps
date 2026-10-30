package appkit

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/wakeshift-go/internal/harnesses"
)

type joinIn struct {
	WakeArgs
}

// mcp-tasks-wire.json (with the TypeScript and Python SDKs): what Wrap
// serves a client on protocol 2026-07-28 that takes MCP tasks, in front of
// a real stateless go-sdk handler, and what it asks Wakeshift.
func TestMCPTasksWireGolden(t *testing.T) {
	var g struct {
		Extension   string         `json:"extension"`
		HoldSeconds int            `json:"hold_seconds"`
		WaitTool    map[string]any `json:"wait_tool"`
		Lists       []struct {
			Name     string         `json:"name"`
			Meta     map[string]any `json:"meta"`
			WaitTool bool           `json:"wait_tool"`
		} `json:"lists"`
		Cases []struct {
			Name         string          `json:"name"`
			Method       string          `json:"method"`
			Params       map[string]any  `json:"params"`
			CorePath     string          `json:"core_path"`
			CoreRequest  map[string]any  `json:"core_request"`
			CoreResponse json.RawMessage `json:"core_response"`
			Result       map[string]any  `json:"result"`
			Error        *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		} `json:"cases"`
	}
	readSharedFixture(t, "mcp-tasks-wire.json", &g)
	if g.Extension != TasksExtension || g.HoldSeconds != taskHoldSeconds || g.WaitTool["name"] != harnesses.WaitTool || g.WaitTool["description"] != harnesses.WaitToolDescription {
		t.Fatalf("constants: %v %v %v", g.Extension, g.HoldSeconds, g.WaitTool["name"])
	}
	if !jsonEqual(g.WaitTool, plainJSON(t, waitToolDefinition())) {
		t.Errorf("the wait tool\n got %v\nwant %v", waitToolDefinition(), g.WaitTool)
	}

	var mu sync.Mutex
	asked := map[string]map[string]any{}
	var reply json.RawMessage
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		defer mu.Unlock()
		path := strings.TrimPrefix(r.URL.Path, "/api/apps/v1")
		asked[path] = body
		if strings.HasPrefix(path, "/calls/") && reply != nil {
			w.Write(reply)
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer core.Close()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	app := &App{ID: "chat", Key: key, Core: core.URL}
	if _, err := DefineEvent(app, "chat.message", EventDef[wireArgs, wireMsg]{Description: "A new message",
		Text: func(m wireMsg, _ wireArgs) string { return m.Text }}); err != nil {
		t.Fatal(err)
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "chat", Version: "1"}, nil)
	attached := app.Attach(srv, AttachOptions{
		Identify: func(_ context.Context, ti *auth.TokenInfo) (Identity, error) { return Identity{User: ti.UserID}, nil }})
	mcp.AddTool(srv, &mcp.Tool{Name: "join", Description: "Join"}, Events(attached,
		func(context.Context, *ToolCall, joinIn) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "joined"}}}, nil, nil
		}))
	h := auth.RequireBearerToken(func(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		return &auth.TokenInfo{UserID: token, Expiration: time.Now().Add(time.Hour)}, nil
	}, nil)(attached.Wrap(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})))

	type rpcResp struct {
		Result map[string]any `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	send := func(method string, params map[string]any) rpcResp {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
		req := httptest.NewRequest("POST", "/mcp", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer u1")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if meta, _ := params["_meta"].(map[string]any); meta != nil {
			if v, _ := meta["io.modelcontextprotocol/protocolVersion"].(string); v != "" {
				req.Header.Set("Mcp-Protocol-Version", v)
				// 2026-07-28's headers, as Claude Code 2.1.314 sends them
				// (docs/harnesses.md): Mcp-Method on every request, and
				// Mcp-Name with a tool call's name or a task request's ID.
				req.Header.Set("Mcp-Method", method)
				if name, _ := params["name"].(string); name != "" {
					req.Header.Set("Mcp-Name", name)
				}
				if task, _ := params["taskId"].(string); task != "" {
					req.Header.Set("Mcp-Name", task)
				}
			}
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var out rpcResp
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s: %d %s", method, rec.Code, rec.Body)
		}
		return out
	}

	d := send("server/discover", map[string]any{"_meta": g.Lists[0].Meta})
	caps, _ := d.Result["capabilities"].(map[string]any)
	ext, _ := caps["extensions"].(map[string]any)
	if v, ok := ext[TasksExtension]; !ok || !reflect.DeepEqual(v, map[string]any{}) {
		t.Errorf("server/discover: %v %+v", d.Result, d.Error)
	}
	for _, l := range g.Lists {
		res := send("tools/list", map[string]any{"_meta": l.Meta})
		tools, _ := res.Result["tools"].([]any)
		var found map[string]any
		for _, tl := range tools {
			if m, _ := tl.(map[string]any); m["name"] == WaitTool {
				found = m
			}
		}
		if (found != nil) != l.WaitTool {
			t.Errorf("tools/list: %s: wait tool %v (%+v)", l.Name, found != nil, res.Error)
		}
		if found != nil && !reflect.DeepEqual(found, g.WaitTool) {
			t.Errorf("tools/list: %s: %v", l.Name, found)
		}
	}

	for _, c := range g.Cases {
		mu.Lock()
		asked = map[string]map[string]any{}
		reply = c.CoreResponse
		mu.Unlock()
		res := send(c.Method, c.Params)
		mu.Lock()
		body, ok := asked[c.CorePath]
		mu.Unlock()
		if !ok {
			t.Errorf("%s: didn't ask %s (asked %v; %+v %+v)", c.Name, c.CorePath, asked, res.Result, res.Error)
			continue
		}
		if call, _ := body["call"].(map[string]any); call != nil {
			delete(call, "user_agent")
		}
		if !reflect.DeepEqual(body, c.CoreRequest) {
			t.Errorf("%s: asked Wakeshift\n got %v\nwant %v", c.Name, body, c.CoreRequest)
		}
		switch {
		case c.Error != nil:
			if res.Error == nil || res.Error.Code != c.Error.Code || !strings.Contains(res.Error.Message, c.Error.Message) {
				t.Errorf("%s: error %+v", c.Name, res.Error)
			}
		case c.Result != nil:
			if !reflect.DeepEqual(res.Result, c.Result) {
				t.Errorf("%s: result\n got %v\nwant %v", c.Name, res.Result, c.Result)
			}
		}
	}

	// A task ID that isn't Wakeshift's goes on to go-sdk, never to Wakeshift.
	for _, m := range []string{"tasks/get", "tasks/cancel"} {
		mu.Lock()
		asked = map[string]map[string]any{}
		mu.Unlock()
		send(m, map[string]any{"taskId": "app-own-task", "_meta": g.Lists[0].Meta})
		mu.Lock()
		for path := range asked {
			if strings.HasPrefix(path, "/calls/task") {
				t.Errorf("%s for another ID asked Wakeshift %s", m, path)
			}
		}
		mu.Unlock()
	}
}
