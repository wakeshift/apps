package appkit

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The TypeScript SDK's tests read the same files, so the two SDKs can't
// drift: event-tools.json is the event tools' fixed wording, and
// events-list.json is what events/list and tools/list answer for the
// sample event types below (sdk/typescript defines the same ones with
// zod). WAKESHIFT_UPDATE_GOLDEN=1 rewrites them from this SDK. Outside
// this repository (the published Go SDK) the files aren't there, and the
// test is skipped.
var goldenDir = filepath.Join("..", "..", "sdk", "typescript", "test")

func golden(t *testing.T, name string, got any) {
	t.Helper()
	if _, err := os.Stat(goldenDir); err != nil {
		t.Skip("no TypeScript SDK here")
	}
	b, err := json.MarshalIndent(canonical(t, got), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	b = append(b, '\n')
	path := filepath.Join(goldenDir, name)
	if os.Getenv("WAKESHIFT_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v (WAKESHIFT_UPDATE_GOLDEN=1 writes it)", path, err)
	}
	if !bytes.Equal(want, b) {
		t.Fatalf("%s differs from the Go SDK's (WAKESHIFT_UPDATE_GOLDEN=1 rewrites it; then make the TypeScript SDK match):\n%s", path, b)
	}
}

// canonical is v as plain JSON values: map keys sort when marshaled.
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

func TestEventToolsGolden(t *testing.T) {
	golden(t, "event-tools.json", eventTools)
}

// The sample event types both SDKs define for events-list.json.
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

func sampleApp(t *testing.T) *App {
	t.Helper()
	app := &App{ID: "sample"}
	if _, err := DefineEvent(app, "chat.message", EventDef[sampleChatArgs, sampleChat]{
		Title: "Chat message", Description: "A new message in a channel.",
		Text: func(m sampleChat, _ sampleChatArgs) string { return m.From + ": " + m.Text }}); err != nil {
		t.Fatal(err)
	}
	if _, err := DefineEvent(app, "game.turn", EventDef[sampleTurnArgs, sampleTurn]{
		Description: "It's your move at a table.", Personal: true,
		Text: func(tu sampleTurn, a sampleTurnArgs) string { return "Your move at " + a.Table }}); err != nil {
		t.Fatal(err)
	}
	return app
}

func TestEventsListGolden(t *testing.T) {
	app := sampleApp(t)
	srv := mcp.NewServer(&mcp.Implementation{Name: "sample", Version: "1"}, nil)
	events := app.ServeEvents(srv, EventsOptions{})
	hs := httptest.NewServer(events(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})))
	defer hs.Close()
	result := func(method string) map[string]any {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": map[string]any{}})
		req, _ := http.NewRequestWithContext(context.Background(), "POST", hs.URL, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
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
	tools := []any{}
	for _, tl := range result("tools/list")["tools"].([]any) {
		m := tl.(map[string]any)
		tools = append(tools, map[string]any{"name": m["name"], "description": m["description"], "inputSchema": inputShape(m["inputSchema"])})
	}
	golden(t, "events-list.json", map[string]any{"events": result("events/list")["events"], "tools": tools,
		"manifest": app.manifestEvents()})
}

// inputShape is what the golden file keeps of a tool's input schema: each
// property's type, description and enum, and what's required. The rest
// is each MCP SDK's own rendering (the TypeScript one adds $schema, and
// spells an object of anything its own way); sdk/typescript's test does
// the same.
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

// manifestEvents is what PublishManifest adds: the routing both SDKs give
// the sample events by default.
func (a *App) manifestEvents() []ManifestEvent {
	var out []ManifestEvent
	for _, t := range a.events() {
		out = append(out, t.manifestEntry())
	}
	return out
}
