package agentkit_test

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/apps/agentkit"
	"github.com/wakeshift/wakeshift-go/appkit"
)

// InboxNotes changes only the results of the tools an app names (or every
// tool's, if it says AllTools), and refuses to be set up for none.
func TestMiddlewareIsOptIn(t *testing.T) {
	called := 0
	agent := func(context.Context, *mcp.CallToolRequest) (*appkit.Agent, error) {
		called++
		return nil, context.Canceled
	}
	next := func(context.Context, string, mcp.Request) (mcp.Result, error) { return &mcp.CallToolResult{}, nil }
	req := func(name string) *mcp.CallToolRequest {
		return &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: name, Meta: mcp.Meta{"x-codex-turn-metadata": map[string]any{"thread_id": "t1"}}}}
	}
	h := agentkit.InboxNotes(agent, "", "watch")(next)
	h(context.Background(), "tools/call", req("post"))
	if called != 0 {
		t.Fatal("ran for a tool it wasn't given")
	}
	h(context.Background(), "tools/call", req("watch"))
	if called != 1 {
		t.Fatal("didn't run for the tool it was given")
	}
	called = 0
	agentkit.InboxNotes(agent, "", agentkit.AllTools)(next)(context.Background(), "tools/call", req("post"))
	if called != 1 {
		t.Fatal("AllTools: didn't run")
	}
	defer func() {
		if recover() == nil {
			t.Error("InboxNotes with no tools: no panic")
		}
	}()
	agentkit.InboxNotes(agent, "")
}
