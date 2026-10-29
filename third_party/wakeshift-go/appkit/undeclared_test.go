package appkit_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/wakeshift-go/appkit"
)

type seatIn struct {
	Table string `json:"table"`
	Seats []seat `json:"seats,omitempty"`
	appkit.WakeArgs
}

type seat struct {
	Name string `json:"name"`
}

type plainIn struct {
	Table string `json:"table"`
}

// Arguments a tool's schema doesn't declare are dropped before it runs, as
// the TypeScript and Python SDKs' schemas drop them: a wrapped tool, an
// unwrapped one and Attach's own all answer, and none sees them. A schema
// that allows other properties keeps them.
func TestUndeclaredArgumentsIgnored(t *testing.T) {
	w := newWrapped(t, appkit.AttachOptions{})
	var wrappedRaw, plainRaw, openRaw json.RawMessage
	var wrappedIn seatIn
	mcp.AddTool(w.srv, &mcp.Tool{Name: "join"}, appkit.Events(w.at, func(_ context.Context, call *appkit.ToolCall, in seatIn) (*mcp.CallToolResult, any, error) {
		wrappedRaw, wrappedIn = call.Request.Params.Arguments, in
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Seated."}}}, nil, nil
	}))
	mcp.AddTool(w.srv, &mcp.Tool{Name: "look"}, func(_ context.Context, req *mcp.CallToolRequest, in plainIn) (*mcp.CallToolResult, any, error) {
		plainRaw = req.Params.Arguments
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Oak is free."}}}, nil, nil
	})
	w.srv.AddTool(&mcp.Tool{Name: "open", InputSchema: json.RawMessage(`{"type":"object","properties":{"table":{"type":"string"}}}`)},
		func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			openRaw = req.Params.Arguments
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
		})

	res := w.callTool(t, "ada", "", "join", map[string]any{"table": "oak", "colour": "red", "seats": []any{map[string]any{"name": "bo", "age": 3}}}, nil)
	if res.IsError || res.RPCError != nil {
		t.Fatalf("join with an undeclared argument: %+v", res)
	}
	if wrappedIn.Table != "oak" || len(wrappedIn.Seats) != 1 || wrappedIn.Seats[0].Name != "bo" {
		t.Fatalf("join's input: %+v", wrappedIn)
	}
	if string(wrappedRaw) != `{"seats":[{"name":"bo"}],"table":"oak"}` {
		t.Fatalf("join saw %s", wrappedRaw)
	}

	res = w.callTool(t, "ada", "", "look", map[string]any{"table": "oak", "session_key": "wsc_x"}, nil)
	if res.IsError || res.RPCError != nil || len(res.Content) == 0 || res.Content[0].Text != "Oak is free." {
		t.Fatalf("look with an undeclared argument: %+v", res)
	}
	if string(plainRaw) != `{"table":"oak"}` {
		t.Fatalf("look saw %s", plainRaw)
	}

	res = w.callTool(t, "ada", "", "open", map[string]any{"table": "oak", "extra": 1}, nil)
	if res.IsError || res.RPCError != nil {
		t.Fatalf("open: %+v", res)
	}
	var open map[string]any
	json.Unmarshal(openRaw, &open)
	if open["extra"] == nil {
		t.Fatalf("a schema that allows other properties lost one: %s", openRaw)
	}

	res = w.callTool(t, "ada", "", "list_subscriptions", map[string]any{"colour": "red"}, nil)
	if res.IsError || res.RPCError != nil {
		t.Fatalf("list_subscriptions with an undeclared argument: %+v", res)
	}

	// Declared arguments are still checked.
	res = w.callTool(t, "ada", "", "look", map[string]any{"table": 3}, nil)
	if !res.IsError && res.RPCError == nil {
		t.Fatalf("a declared argument of the wrong type passed: %+v", res)
	}
}
