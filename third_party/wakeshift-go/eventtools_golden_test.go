package wakeshift

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
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The TypeScript SDK's tests read the same files, so the two SDKs can't
// drift: event-tools.json is the event tools' fixed wording, and
// events-list.json is what events/list and tools/list answer for the
// sample event types below (sdk/typescript defines the same ones with
// zod). WAKESHIFT_UPDATE_GOLDEN=1 rewrites them from this SDK.
var goldenDir = sharedGoldenDir()

// sharedGoldenDir is where the golden files the three SDKs share are:
// beside the TypeScript SDK's tests in Wakeshift's repository, or
// testdata/golden in the published Go SDK, where opensource/export.sh copies
// them.
func sharedGoldenDir() string {
	if here := filepath.Join("..", "..", "sdk", "typescript", "test"); exists(here) {
		return here
	}
	return filepath.Join("testdata", "golden")
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// golden checks got against a shared golden file as JSON values, keeping
// the file's top-level keys that start with _ (comments and markers, which
// the SDKs don't produce). The TypeScript SDK owns the files' formatting, so
// key order and indentation don't count. WAKESHIFT_UPDATE_GOLDEN=1 rewrites
// a file whose content differs (then check it against the TypeScript and
// Python SDKs' tests too).
func golden(t *testing.T, name string, got any) {
	t.Helper()
	if _, err := os.Stat(goldenDir); err != nil {
		t.Skip("no shared golden files here")
	}
	path := filepath.Join(goldenDir, name)
	want, err := os.ReadFile(path)
	if err != nil && os.Getenv("WAKESHIFT_UPDATE_GOLDEN") != "1" {
		t.Fatalf("%s: %v (WAKESHIFT_UPDATE_GOLDEN=1 writes it)", path, err)
	}
	out := canonical(t, got)
	var file any
	if err == nil {
		if err := json.Unmarshal(want, &file); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
	if m, ok := out.(map[string]any); ok {
		if fm, ok := file.(map[string]any); ok {
			for k, v := range fm {
				if strings.HasPrefix(k, "_") {
					m[k] = v
				}
			}
		}
	}
	if reflect.DeepEqual(file, out) {
		return
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	if os.Getenv("WAKESHIFT_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Fatalf("%s differs from the Go SDK's (WAKESHIFT_UPDATE_GOLDEN=1 rewrites it; then make the TypeScript and Python SDKs match):\n%s", path, b)
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

// sampleRunArgs and sampleRun are defined out of alphabetical order, so
// the files catch an SDK that lists arguments (or required fields) in
// definition order instead of name order.
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

// sampleApp is an enabled client with events-list.json's sample events,
// against a core that answers nothing but the app's name (none).
func sampleApp(t *testing.T) *Client {
	t.Helper()
	app, _ := testApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	for _, name := range []string{"chat.message", "game.turn", "ci.run", "deploy.finished"} {
		defineSample(t, app, name) // manifest_golden_internal_test.go
	}
	return app
}

func TestEventsListGolden(t *testing.T) {
	app := sampleApp(t)
	srv := mcp.NewServer(&mcp.Implementation{Name: "sample", Version: "1"}, nil)
	attached := app.Attach(srv, AttachOptions{})
	hs := httptest.NewServer(attached.Handler(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
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
	var builtinArgs any
	for _, tl := range result("tools/list")["tools"].([]any) {
		m := tl.(map[string]any)
		// subscribe_events' and unsubscribe_events' arguments are exactly
		// builtin_arguments_schema, on the wire.
		if props, _ := m["inputSchema"].(map[string]any)["properties"].(map[string]any); props["arguments"] != nil {
			if builtinArgs != nil && !reflect.DeepEqual(builtinArgs, props["arguments"]) {
				t.Errorf("%s: arguments is %v, unlike the other built-in's %v", m["name"], props["arguments"], builtinArgs)
			}
			builtinArgs = props["arguments"]
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
	golden(t, "events-list.json", map[string]any{"events": result("events/list")["events"], "tools": tools, "builtin_arguments_schema": builtinArgs,
		"manifest": app.manifestEvents(), "vouchers": vouchersList(t)})
}

// vouchersList is events-list.json's vouchers: events/list to a native
// request (ChatGPT's clientInfo) while the core reports subscribe_vouchers,
// for an app the core names Sample.
func vouchersList(t *testing.T) map[string]any {
	t.Helper()
	app, _ := testApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/apps/v1/manifest/merge":
			w.Write([]byte(`{"features":["subscribe_vouchers"]}`))
		case "/api/apps/v1/app":
			w.Write([]byte(`{"id":"app-test","name":"Sample"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	for _, name := range []string{"chat.message", "game.turn", "ci.run", "deploy.finished"} {
		defineSample(t, app, name)
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "sample", Version: "1"}, nil)
	attached := app.Attach(srv, AttachOptions{})
	hs := httptest.NewServer(attached.Handler(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})))
	defer hs.Close()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "events/list",
		"params": map[string]any{"_meta": map[string]any{"io.modelcontextprotocol/clientInfo": map[string]any{"name": "openai-mcp", "version": "1"}}}})
	req, _ := http.NewRequestWithContext(context.Background(), "POST", hs.URL, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Result struct {
			Events []any `json:"events"`
		} `json:"result"`
	}
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("events/list: %s", raw)
	}
	return map[string]any{"app": "Sample", "events_native": out.Result.Events}
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

// manifestEvents is what PublishManifest adds: the routing both SDKs give
// the sample events by default.
func (c *Client) manifestEvents() []ManifestEvent {
	var out []ManifestEvent
	for _, t := range c.events() {
		out = append(out, t.manifestEntry())
	}
	return out
}

// tool-annotations.json is the annotations of Attach's own tools and the
// wait tool, and tools/list on a go-sdk server gives exactly those, for a
// client that takes tasks (so the wait tool is listed).
func TestToolAnnotationsGolden(t *testing.T) {
	want := map[string]any{}
	for name, a := range toolAnnotations {
		want[name] = a.Wire()
	}
	golden(t, "tool-annotations.json", want)

	app := sampleApp(t)
	srv := mcp.NewServer(&mcp.Implementation{Name: "sample", Version: "1"}, nil)
	attached := app.Attach(srv, AttachOptions{BuiltinTools: BuiltinTools{Wait: true}})
	hs := httptest.NewServer(attached.Handler(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})))
	defer hs.Close()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": map[string]any{
		"_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": tasksProtocol,
			"io.modelcontextprotocol/clientCapabilities": map[string]any{"extensions": map[string]any{tasksExtension: map[string]any{}}}}}})
	req, _ := http.NewRequestWithContext(context.Background(), "POST", hs.URL, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", tasksProtocol)
	req.Header.Set("Mcp-Method", "tools/list")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Result struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	got := map[string]any{}
	for _, tl := range out.Result.Tools {
		got[tl["name"].(string)] = tl["annotations"]
	}
	var fixture map[string]any
	b, err = os.ReadFile(filepath.Join(goldenDir, "tool-annotations.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &fixture); err != nil {
		t.Fatal(err)
	}
	if g, w := canonical(t, got), canonical(t, fixture); !reflect.DeepEqual(g, w) {
		t.Fatalf("tools/list's annotations\n got %v\nwant %v", g, w)
	}
}

// tool-output-schemas.json is the output schemas of Attach's own tools
// (subscribe_events, unsubscribe_events, list_subscriptions), and tools/list
// on a go-sdk server gives exactly those.
func TestToolOutputSchemasGolden(t *testing.T) {
	golden(t, "tool-output-schemas.json", toolOutputSchemas)
	app := sampleApp(t)
	srv := mcp.NewServer(&mcp.Implementation{Name: "sample", Version: "1"}, nil)
	app.Attach(srv, AttachOptions{})
	ct, st := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "1"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	list, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]any{}
	for _, tl := range list.Tools {
		if _, ok := toolOutputSchemas[tl.Name]; ok {
			got[tl.Name] = tl.OutputSchema
		}
	}
	if g, w := canonical(t, got), canonical(t, toolOutputSchemas); !reflect.DeepEqual(g, w) {
		t.Fatalf("tools/list's output schemas\n got %v\nwant %v", g, w)
	}
}
