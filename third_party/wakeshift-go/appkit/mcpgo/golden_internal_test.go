package mcpgo

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/wakeshift/wakeshift-go/appkit"
	"github.com/wakeshift/wakeshift-go/appkit/internal/adapt"
)

// The golden files the three SDKs share (sdk/typescript/test here,
// appkit/testdata/golden in the published Go SDK), for mcp-go: a reply once
// Wakeshift's block is added (wrapped-block.json) and Attach's own tools as
// tools/list lists them (events-list.json).

func goldenPath(name string) string {
	here := filepath.Join("..", "..", "..", "sdk", "typescript", "test")
	if _, err := os.Stat(here); err == nil {
		return filepath.Join(here, name)
	}
	return filepath.Join("..", "testdata", "golden", name)
}

func readGolden(t *testing.T, name string, v any) {
	t.Helper()
	b, err := os.ReadFile(goldenPath(name))
	if os.IsNotExist(err) {
		t.Skip("no shared golden files here")
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

func plainJSON(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestWrappedBlockGolden(t *testing.T) {
	type addition struct {
		Text    string         `json:"text"`
		Trailer string         `json:"trailer"`
		Meta    map[string]any `json:"meta"`
	}
	var f struct {
		Addition addition `json:"addition"`
		Cases    []struct {
			Name     string          `json:"name"`
			Addition *addition       `json:"addition"`
			Result   json.RawMessage `json:"result"`
			Want     json.RawMessage `json:"want"`
		} `json:"cases"`
		OutputSchemas []struct {
			Name   string         `json:"name"`
			Schema map[string]any `json:"schema"`
			Want   map[string]any `json:"want"`
		} `json:"output_schemas"`
	}
	readGolden(t, "wrapped-block.json", &f)
	for _, c := range f.Cases {
		var s string
		if json.Unmarshal(c.Result, &s) == nil {
			continue // a string result: Go's handlers return a CallToolResult
		}
		add := f.Addition
		if c.Addition != nil {
			add = *c.Addition
		}
		var res mcp.CallToolResult
		if err := json.Unmarshal(c.Result, &res); err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		if res.Content == nil {
			res.Content = []mcp.Content{}
		}
		// What run does with /calls/addition's answer.
		mergeBlock(&res, adapt.Block{Text: add.Text, Trailer: add.Trailer, Meta: add.Meta})
		if add.Text != "" {
			setWakeshift(&res, add.Text)
		}
		var want map[string]any
		json.Unmarshal(c.Want, &want)
		if _, ok := want["content"]; !ok {
			want["content"] = []any{} // mcp-go always sends content
		}
		if got := plainJSON(t, &res); !reflect.DeepEqual(got, want) {
			gb, _ := json.Marshal(got)
			t.Errorf("%s:\n got %s\nwant %s", c.Name, gb, c.Want)
		}
	}
	for _, c := range f.OutputSchemas {
		raw, _ := json.Marshal(c.Schema)
		tool := wakeSchemas(mcp.Tool{Name: "t", RawOutputSchema: raw})
		got := map[string]any{}
		json.Unmarshal(tool.RawOutputSchema, &got)
		if len(tool.RawOutputSchema) == 0 {
			got = c.Schema // unchanged
		}
		if !reflect.DeepEqual(got, c.Want) {
			gb, _ := json.Marshal(got)
			wb, _ := json.Marshal(c.Want)
			t.Errorf("%s:\n got %s\nwant %s", c.Name, gb, wb)
		}
	}
}

// The sample event types every SDK defines for events-list.json
// (appkit's eventtools_golden_test.go).
type sampleChatArgs struct {
	Channel string `json:"channel" jsonschema:"The channel, lowercase"`
	From    string `json:"from,omitempty" jsonschema:"Only messages from this person"`
}

type sampleChat struct {
	From string `json:"from" jsonschema:"Who wrote it"`
	Text string `json:"text" jsonschema:"The message"`
}

type sampleTurnArgs struct {
	Table string `json:"table" jsonschema:"The table you're seated at"`
}

type sampleTurn struct {
	Street string   `json:"street" jsonschema:"The betting round"`
	Hand   []string `json:"hand,omitempty" jsonschema:"Your hole cards"`
}

type sampleRunArgs struct {
	Repo   string `json:"repo" jsonschema:"The repository"`
	PR     int    `json:"pr" jsonschema:"The pull request"`
	Branch string `json:"branch,omitempty" jsonschema:"Only runs on this branch"`
}

type sampleRun struct {
	Status string `json:"status" jsonschema:"How it ended"`
	Branch string `json:"branch" jsonschema:"The branch it ran on"`
}

// events/list and Attach's own tools on an mcp-go server are what every
// SDK lists (the tools' properties, descriptions, enums and what's
// required; list_subscriptions' annotations).
func TestEventsListGolden(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	app := &appkit.App{ID: "sample", Key: key, Core: "http://127.0.0.1:1"}
	must := func(_ any, err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(appkit.DefineEvent(app, "chat.message", appkit.EventDef[sampleChatArgs, sampleChat]{
		Title: "Chat message", Description: "A new message in a channel.",
		Text: func(m sampleChat, _ sampleChatArgs) string { return m.From + ": " + m.Text }}))
	must(appkit.DefineEvent(app, "game.turn", appkit.EventDef[sampleTurnArgs, sampleTurn]{
		Description: "It's your move at a table.", Personal: true,
		Text: func(_ sampleTurn, a sampleTurnArgs) string { return "Your move at " + a.Table }}))
	must(appkit.DefineEvent(app, "ci.run", appkit.EventDef[sampleRunArgs, sampleRun]{
		Description: "A CI run on a pull request finished.",
		Text:        func(r sampleRun, _ sampleRunArgs) string { return "CI " + r.Status }}))
	srv := server.NewMCPServer("sample", "1")
	at := Attach(app, srv, AttachOptions{})
	hs := httptest.NewServer(at.Wrap(server.NewStreamableHTTPServer(srv)))
	defer hs.Close()
	result := func(method string) map[string]any {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": map[string]any{
			"_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientCapabilities": map[string]any{}}}})
		req, _ := http.NewRequestWithContext(context.Background(), "POST", hs.URL, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Mcp-Protocol-Version", "2026-07-28")
		req.Header.Set("Mcp-Method", method)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var out struct {
			Result map[string]any `json:"result"`
		}
		if err := json.Unmarshal(raw, &out); err != nil || out.Result == nil {
			t.Fatalf("%s: %s", method, raw)
		}
		return out.Result
	}
	var want struct {
		Events []any `json:"events"`
		Tools  []any `json:"tools"`
	}
	readGolden(t, "events-list.json", &want)
	tools := []any{}
	for _, tl := range result("tools/list")["tools"].([]any) {
		m := tl.(map[string]any)
		tool := map[string]any{"name": m["name"], "description": m["description"], "inputSchema": inputShape(m["inputSchema"])}
		if a, ok := m["annotations"].(map[string]any); ok && len(a) > 0 {
			tool["annotations"] = a
		}
		tools = append(tools, tool)
	}
	if got, w := canonical(t, tools), canonical(t, want.Tools); !reflect.DeepEqual(got, w) {
		gb, _ := json.MarshalIndent(got, "", "  ")
		t.Errorf("tools/list differs from events-list.json's tools:\n%s", gb)
	}
	if got, w := canonical(t, result("events/list")["events"]), canonical(t, want.Events); !reflect.DeepEqual(got, w) {
		gb, _ := json.MarshalIndent(got, "", "  ")
		t.Errorf("events/list differs from events-list.json's events:\n%s", gb)
	}
}

// inputShape is what the golden file keeps of a tool's input schema
// (appkit's eventtools_golden_test.go).
func inputShape(v any) any {
	s, _ := v.(map[string]any)
	props := map[string]any{}
	ps, _ := s["properties"].(map[string]any)
	for name, p := range ps {
		pm, _ := p.(map[string]any)
		out := map[string]any{"type": pm["type"], "description": pm["description"]}
		if e, ok := pm["enum"]; ok {
			out["enum"] = e
		}
		props[name] = out
	}
	shape := map[string]any{"properties": props}
	if r, ok := s["required"]; ok {
		shape["required"] = r
	}
	return shape
}

func canonical(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
