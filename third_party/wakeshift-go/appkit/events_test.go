package appkit_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/wakeshift-go/appkit"
)

// Events in front of a real MCP server (the Go SDK, stateless): the
// capability is added to what the server answers, in JSON or SSE; events/list
// is answered; everything else passes through.
func TestEventsMiddleware(t *testing.T) {
	for _, jsonResponse := range []bool{false, true} {
		srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "1"}, nil)
		mcp.AddTool(srv, &mcp.Tool{Name: "ping"}, func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{}, nil, nil
		})
		h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: jsonResponse})
		app := &appkit.App{ID: "t"}
		hs := httptest.NewServer(app.Events(nil)(h))
		defer hs.Close()

		post := func(method string, params any) string {
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
			req, _ := http.NewRequest("POST", hs.URL, bytes.NewReader(b))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			out, _ := io.ReadAll(resp.Body)
			return string(out)
		}
		init := post("initialize", map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{},
			"clientInfo": map[string]any{"name": "openai-mcp", "version": "1.0.0"}})
		if !strings.Contains(init, `"events":{}`) || !strings.Contains(init, `"io.modelcontextprotocol/events":{}`) || !strings.Contains(init, `"tools"`) {
			t.Fatalf("json=%v initialize: %s", jsonResponse, init)
		}
		if list := post("events/list", map[string]any{}); !strings.Contains(list, `"name":"wakeshift.message"`) || !strings.Contains(list, `"required":["session"]`) {
			t.Fatalf("events/list: %s", list)
		}
		if tools := post("tools/list", map[string]any{}); !strings.Contains(tools, `"ping"`) || strings.Contains(tools, `"events"`) {
			t.Fatalf("json=%v tools/list: %s", jsonResponse, tools)
		}
	}
}
