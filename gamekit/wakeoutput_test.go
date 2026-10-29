package gamekit

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/wakeshift-go/appkit"
)

// A game tool's structured output carries Wakeshift's block as wakeshift
// (appkit.WakeOutput), without the beacon line, beside the game's own
// fields, for hosts that show the model only structuredContent; the block
// stays in the content, beacon last, and appkit logs no warning about it.
// The tool's output schema names wakeshift and takes any other field.
func TestToolOutputCarriesWakeshift(t *testing.T) {
	ctx := context.Background()
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	const block = "Wakeshift: this conversation is woken by Monitor."
	const trailer = "(For Wakeshift's hook on this computer, not for you: wakeshift-beacon: aaaaaaaaaa.bbbbbbbbbb.cccccccccc)"
	core := newRecCore()
	core.on("POST /api/apps/v1/calls/addition", func(map[string]any) (int, any) {
		return 200, map[string]any{"text": block, "trailer": trailer}
	})
	hs := httptest.NewServer(core)
	t.Cleanup(hs.Close)
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	s := &Server{Title: "Nightfall", Name: "nightfall", Kit: &appkit.App{ID: "nightfall", Key: key, Core: hs.URL}}
	srv := mcp.NewServer(&mcp.Implementation{Name: "t"}, nil)
	s.events() // Attach needs an event
	s.attached = s.Kit.Attach(srv, appkit.AttachOptions{})
	AddTool(s, srv, &mcp.Tool{Name: "join"}, func(context.Context, *appkit.ToolCall, noIn) (*mcp.CallToolResult, map[string]any, error) {
		return nil, map[string]any{"game": "abc", "you": "ada"}, nil
	})
	AddTool(s, srv, &mcp.Tool{Name: "canvas"}, func(context.Context, *appkit.ToolCall, noIn) (*mcp.CallToolResult, map[string]any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "The drawing."}}}, nil, nil
	})

	ct, st := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "c"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	listed := 0
	for _, tool := range tools.Tools {
		if tool.Name != "join" && tool.Name != "canvas" {
			continue // Wakeshift's own
		}
		listed++
		b, _ := json.Marshal(tool.OutputSchema)
		var schema struct {
			Type       string                     `json:"type"`
			Properties map[string]json.RawMessage `json:"properties"`
			Additional *json.RawMessage           `json:"additionalProperties"`
		}
		json.Unmarshal(b, &schema)
		if schema.Type != "object" || schema.Properties["wakeshift"] == nil || schema.Additional == nil || (string(*schema.Additional) != "{}" && string(*schema.Additional) != "true") {
			t.Errorf("%s's output schema: %s", tool.Name, b)
		}
	}
	if listed != 2 {
		t.Fatalf("listed %d of the game's tools", listed)
	}

	call := func(tool string) (map[string]any, []string) {
		t.Helper()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: map[string]any{"session_key": "wsc_s1"}})
		if err != nil || res.IsError {
			t.Fatalf("%s: %v %+v", tool, err, res)
		}
		var texts []string
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				texts = append(texts, tc.Text)
			}
		}
		b, _ := json.Marshal(res.StructuredContent)
		var out map[string]any
		json.Unmarshal(b, &out)
		return out, texts
	}
	out, texts := call("join")
	if out["game"] != "abc" || out["you"] != "ada" || out["wakeshift"] != block {
		t.Errorf("join's structured content: %v", out)
	}
	if n := len(texts); n < 2 || texts[n-2] != block || texts[n-1] != trailer {
		t.Errorf("join's content: %q", texts)
	}
	out, texts = call("canvas")
	if len(out) != 1 || out["wakeshift"] != block {
		t.Errorf("canvas's structured content: %v", out)
	}
	if len(texts) != 3 || texts[0] != "The drawing." || texts[1] != block || texts[2] != trailer {
		t.Errorf("canvas's content: %q", texts)
	}
	if strings.Contains(logs.String(), "WakeOutput") {
		t.Errorf("appkit warned: %s", logs.String())
	}
}
