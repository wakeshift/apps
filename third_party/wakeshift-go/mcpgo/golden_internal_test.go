package mcpgo

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/wakeshift/wakeshift-go"
	"github.com/wakeshift/wakeshift-go/internal/adapt"
)

// The golden files the three SDKs share (sdk/typescript/test here,
// wakeshift/testdata/golden in the published Go SDK), for mcp-go: a reply once
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
		tool := wakeSchemas(mcp.Tool{Name: "t", RawOutputSchema: raw}, true)
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
// (wakeshift's eventtools_golden_test.go).
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

type sampleDeployArgs struct {
	Env string `json:"env" jsonschema:"The environment"`
}

type sampleDeploy struct {
	Status string `json:"status" jsonschema:"How it ended"`
}

// events/list and Attach's own tools on an mcp-go server are what every
// SDK lists (the tools' properties, descriptions, enums and what's
// required; list_subscriptions' annotations).
func TestEventsListGolden(t *testing.T) {
	app, err := wakeshift.New(wakeshift.WithKey("wst_1116qJFWMMHFy3xDdLmvUeyc2S6FrWRhJP51HsvDYdz9fWXzSV"), wakeshift.WithCore("http://127.0.0.1:1"), wakeshift.WithMaxRetries(0))
	if err != nil {
		t.Fatal(err)
	}
	must := func(_ any, err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(wakeshift.DefineEvent(app, "chat.message", wakeshift.EventDef[sampleChatArgs, sampleChat]{
		Title: "Chat message", Description: "A new message in a channel.", Public: true,
		Text: func(m sampleChat, _ sampleChatArgs) string { return m.From + ": " + m.Text }}))
	must(wakeshift.DefineEvent(app, "game.turn", wakeshift.EventDef[sampleTurnArgs, sampleTurn]{
		Description: "It's your move at a table.", Personal: true,
		Text: func(_ sampleTurn, a sampleTurnArgs) string { return "Your move at " + a.Table }}))
	must(wakeshift.DefineEvent(app, "ci.run", wakeshift.EventDef[sampleRunArgs, sampleRun]{
		Description: "A CI run on a pull request finished.",
		Text:        func(r sampleRun, _ sampleRunArgs) string { return "CI " + r.Status }}))
	must(wakeshift.DefineEvent(app, "deploy.finished", wakeshift.EventDef[sampleDeployArgs, sampleDeploy]{
		Description: "A deploy finished.",
		Authorize:   func(context.Context, wakeshift.Subscriber, sampleDeployArgs) (bool, error) { return true, nil }}))
	srv := server.NewMCPServer("sample", "1")
	at := Attach(app, srv, wakeshift.AttachOptions{})
	hs := httptest.NewServer(at.Handler(server.NewStreamableHTTPServer(srv)))
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
		// subscribe_events' and unsubscribe_events' arguments are exactly
		// builtin_arguments_schema, on the wire.
		if props, _ := m["inputSchema"].(map[string]any)["properties"].(map[string]any); props["arguments"] != nil {
			got, _ := json.Marshal(props["arguments"])
			want, _ := json.Marshal(builtinArgumentsSchema(t))
			if string(got) != string(want) {
				t.Errorf("%s: arguments is %s, want %s", m["name"], got, want)
			}
		}
		tool := map[string]any{"name": m["name"], "description": m["description"], "inputSchema": inputShape(m["inputSchema"])}
		if title, ok := m["title"]; ok {
			tool["title"] = title
		}
		if a, ok := m["annotations"].(map[string]any); ok && len(a) > 0 {
			tool["annotations"] = a
		}
		tools = append(tools, tool)
	}
	sort.Slice(tools, func(i, j int) bool {
		return tools[i].(map[string]any)["name"].(string) < tools[j].(map[string]any)["name"].(string)
	})
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
// (wakeshift's eventtools_golden_test.go).
func inputShape(v any) any {
	s, _ := v.(map[string]any)
	props := map[string]any{}
	ps, _ := s["properties"].(map[string]any)
	for name, p := range ps {
		pm, _ := p.(map[string]any)
		out := map[string]any{}
		for _, k := range []string{"type", "description", "enum"} {
			if v, ok := pm[k]; ok {
				out[k] = v
			}
		}
		props[name] = out
	}
	shape := map[string]any{"type": s["type"]}
	if len(props) > 0 { // README: properties left out when it has none
		shape["properties"] = props
	}
	if r, ok := s["required"].([]any); ok && len(r) > 0 {
		names := make([]string, len(r))
		for i, n := range r {
			names[i], _ = n.(string)
		}
		sort.Strings(names)
		shape["required"] = names
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

// builtinArgumentsSchema is events-list.json's builtin_arguments_schema.
func builtinArgumentsSchema(t *testing.T) map[string]any {
	t.Helper()
	var f struct {
		Schema map[string]any `json:"builtin_arguments_schema"`
	}
	readGolden(t, "events-list.json", &f)
	if f.Schema == nil {
		t.Fatal("events-list.json has no builtin_arguments_schema")
	}
	return f.Schema
}
