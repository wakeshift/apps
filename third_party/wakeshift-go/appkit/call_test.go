package appkit_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/wakeshift-go/appkit"
	"github.com/wakeshift/wakeshift-go/internal/harnesses"
)

// A Go app passes the agent's conversation_id and harness on with the
// call, as the core reads them.
func TestCallWithConversation(t *testing.T) {
	c := appkit.CallFrom(nil, "ada", "wsc_x").WithConversation(" 4f0c9a52-6f0e-4bde-8a43-1d0a7e39d2a1 ").WithHarness(" Grok Bot ")
	b, _ := json.Marshal(c)
	var got map[string]any
	json.Unmarshal(b, &got)
	if got["conversation"] != "4f0c9a52-6f0e-4bde-8a43-1d0a7e39d2a1" || got["harness"] != "Grok Bot" || got["session"] != "wsc_x" || got["user"] != "ada" {
		t.Fatalf("%s", b)
	}
	if b, _ := json.Marshal(appkit.CallFrom(nil, "", "")); string(b) != `{"client":""}` {
		t.Fatalf("without one: %s", b)
	}
	a := appkit.Args{SessionKey: "wsc_y", Harness: "cursor", ConversationID: "c1"}
	if c := a.Call(nil, "bo"); c.Session != "wsc_y" || c.Harness != "cursor" || c.Conversation != "c1" || c.User != "bo" {
		t.Fatalf("Args.Call: %+v", c)
	}
}

// Args, embedded in a tool's input, adds exactly session_key, harness and
// conversation_id, described as the core and the TypeScript SDK describe
// them (the harness list comes from internal/harnesses).
func TestArgsSchema(t *testing.T) {
	type in struct {
		Repo string `json:"repo"`
		appkit.Args
	}
	s, err := jsonschema.For[in](nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"session_key":     harnesses.SessionDescription,
		"harness":         harnesses.ArgDescription(),
		"conversation_id": harnesses.ConversationDescription,
	}
	for name, desc := range want {
		p, ok := s.Properties[name]
		if !ok || p.Description != desc {
			t.Errorf("%s: %+v, want %q", name, p, desc)
		}
	}
	if len(s.Properties) != 4 || !reflect.DeepEqual(s.Required, []string{"repo"}) {
		t.Errorf("properties %v, required %v", s.Properties, s.Required)
	}
}

// ConnectorBeacons and InboxNotes change only the results of the tools an
// app names (or every tool's, if it says AllTools), and refuse to be set up
// for none.
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
	for _, mw := range []mcp.Middleware{appkit.ConnectorBeacons(agent, "watch"), appkit.InboxNotes(agent, "", "watch")} {
		called = 0
		h := mw(next)
		h(context.Background(), "tools/call", req("post"))
		if called != 0 {
			t.Fatal("ran for a tool it wasn't given")
		}
		h(context.Background(), "tools/call", req("watch"))
		if called != 1 {
			t.Fatal("didn't run for the tool it was given")
		}
	}
	called = 0
	appkit.ConnectorBeacons(agent, appkit.AllTools)(next)(context.Background(), "tools/call", req("post"))
	if called != 1 {
		t.Fatal("AllTools: didn't run")
	}
	for name, mk := range map[string]func(){
		"ConnectorBeacons": func() { appkit.ConnectorBeacons(agent) },
		"InboxNotes":       func() { appkit.InboxNotes(agent, "") },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s with no tools: no panic", name)
				}
			}()
			mk()
		}()
	}
}
