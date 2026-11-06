package wakeshift_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/wakeshift-go"
)

type roomArgs struct {
	Room string `json:"room" jsonschema:"The room"`
}

type roomPost struct {
	Body string `json:"body"`
}

// Attach in front of a real MCP server (the Go SDK, stateless): the
// capability is added to what the server answers, in JSON or SSE;
// events/list lists the app's event types (and no wakeshift.message);
// tools/list has the event tools, without subscribe_events for a client
// that subscribes natively; everything else passes through.
func TestAttachWrap(t *testing.T) {
	for _, jsonResponse := range []bool{false, true} {
		app := testClient(t, "https://core.example")
		if _, err := wakeshift.DefineEvent(app, "room.post", wakeshift.EventDef[roomArgs, roomPost]{Description: "A post to a room.", Public: true, Text: func(p roomPost, _ roomArgs) string { return p.Body }}); err != nil {
			t.Fatal(err)
		}
		srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "1"}, nil)
		mcp.AddTool(srv, &mcp.Tool{Name: "ping"}, func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{}, nil, nil
		})
		attached := app.Attach(srv, wakeshift.AttachOptions{})
		h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: jsonResponse})
		hs := httptest.NewServer(attached.Handler(h))
		defer hs.Close()

		post := func(ua, method string, params any) string {
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
			req, _ := http.NewRequest("POST", hs.URL, bytes.NewReader(b))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			req.Header.Set("User-Agent", ua)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			out, _ := io.ReadAll(resp.Body)
			return string(out)
		}
		init := post("t", "initialize", map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{},
			"clientInfo": map[string]any{"name": "openai-mcp", "version": "1.0.0"}})
		if !strings.Contains(init, `"events":{"listChanged":false}`) || !strings.Contains(init, `"io.modelcontextprotocol/events":{"listChanged":false}`) ||
			!strings.Contains(init, `"tools"`) {
			t.Fatalf("json=%v initialize: %s", jsonResponse, init)
		}
		list := post("t", "events/list", map[string]any{})
		if !strings.Contains(list, `"name":"room.post"`) || !strings.Contains(list, `"required":["room"]`) ||
			!strings.Contains(list, `"resultType":"complete"`) || strings.Contains(list, "wakeshift.message") {
			t.Fatalf("events/list: %s", list)
		}
		// A bridged client (Claude Code) sees all three tools; ChatGPT, and a
		// client that declares the extension, don't see subscribe_events.
		tools := post("claude-code/2.1.280", "tools/list", map[string]any{})
		for _, name := range []string{"ping", "subscribe_events", "unsubscribe_events", "list_subscriptions"} {
			if !strings.Contains(tools, `"`+name+`"`) {
				t.Fatalf("json=%v bridged tools/list lacks %s: %s", jsonResponse, name, tools)
			}
		}
		if !strings.Contains(tools, "room.post(room): A post to a room.") {
			t.Fatalf("subscribe_events' description lists the events: %s", tools)
		}
		// list_subscriptions only reads, and says so; the others don't.
		if i := strings.Index(tools, `"name":"list_subscriptions"`); i < 0 || !strings.Contains(toolJSON(tools, "list_subscriptions"), `"readOnlyHint":true`) ||
			strings.Contains(toolJSON(tools, "subscribe_events"), `"readOnlyHint":true`) || strings.Contains(toolJSON(tools, "unsubscribe_events"), `"readOnlyHint":true`) {
			t.Fatalf("json=%v readOnlyHint: %s", jsonResponse, tools)
		}
		native := []string{
			post("openai-mcp/1.0.0", "tools/list", map[string]any{}),
			post("t", "tools/list", map[string]any{"_meta": map[string]any{"io.modelcontextprotocol/clientCapabilities": map[string]any{
				"extensions": map[string]any{"io.modelcontextprotocol/events": map[string]any{}}}}}),
		}
		for i, tools := range native {
			if strings.Contains(tools, `"subscribe_events"`) || !strings.Contains(tools, `"list_subscriptions"`) || !strings.Contains(tools, `"ping"`) {
				t.Fatalf("json=%v native client %d tools/list: %s", jsonResponse, i, tools)
			}
		}
		// Claude Code declaring the extension is still bridged: its name wins.
		declaring := post("claude-code/2.1.316", "tools/list", map[string]any{"_meta": map[string]any{"io.modelcontextprotocol/clientCapabilities": map[string]any{
			"extensions": map[string]any{"io.modelcontextprotocol/events": map[string]any{}}}}})
		if !strings.Contains(declaring, `"subscribe_events"`) {
			t.Fatalf("json=%v Claude Code declaring MCP Events tools/list: %s", jsonResponse, declaring)
		}
	}
}

// DefineEvent's schemas and default routing.
func TestDefineEvent(t *testing.T) {
	type args struct {
		Repo   string `json:"repo" jsonschema:"owner/name, lowercase"`
		Branch string `json:"branch,omitempty" jsonschema:"Only this branch"`
	}
	type payload struct {
		Branch string   `json:"branch"`
		Status string   `json:"status"`
		Jobs   []string `json:"jobs,omitempty"`
	}
	app := disabledClient(t)
	text := func(p payload, a args) string { return a.Repo + ": " + p.Status }
	def := func(d wakeshift.EventDef[args, payload]) wakeshift.EventDef[args, payload] {
		if d.Description == "" {
			d.Description = "A CI event."
		}
		d.Text = text
		return d
	}
	if _, err := wakeshift.DefineEvent(app, "ci.run", def(wakeshift.EventDef[args, payload]{Description: "A CI run finished."})); err != nil {
		t.Fatal(err)
	}
	for name, d := range map[string]wakeshift.EventDef[args, payload]{
		"Bad.Name":          {},
		"wakeshift.message": {},
		"ci.run":            {}, // already defined
	} {
		if _, err := wakeshift.DefineEvent(app, name, def(d)); err == nil {
			t.Errorf("DefineEvent(%q) succeeded", name)
		}
	}
	type freeArgs struct {
		Note string `json:"note,omitempty"`
	}
	if _, err := wakeshift.DefineEvent(app, "ci.note", wakeshift.EventDef[freeArgs, payload]{Description: "x", Text: func(payload, freeArgs) string { return "x" }}); err == nil ||
		!strings.Contains(err.Error(), "note") {
		t.Errorf("an optional argument with no payload field to filter on: %v", err)
	}
	type listArgs struct {
		Repo string   `json:"repo"`
		Jobs []string `json:"jobs,omitempty"`
	}
	if _, err := wakeshift.DefineEvent(app, "ci.jobs", wakeshift.EventDef[listArgs, payload]{Description: "x", Text: func(payload, listArgs) string { return "x" }}); err == nil ||
		!strings.Contains(err.Error(), "jobs") {
		t.Errorf("an optional argument that isn't scalar: %v", err)
	}
	if _, err := wakeshift.DefineEvent(app, "ci.incomplete", wakeshift.EventDef[args, payload]{Text: text}); !wakeshift.IsCode(err, wakeshift.CodeConfig) {
		t.Errorf("no description: %v", err)
	}
	// Text is optional.
	if _, err := wakeshift.DefineEvent(app, "ci.plain", wakeshift.EventDef[args, payload]{Description: "x"}); err != nil {
		t.Errorf("no text: %v", err)
	}
	// Who may subscribe: Public alone, never with Personal or Authorize.
	allow := func(context.Context, wakeshift.Subscriber, args) (bool, error) { return true, nil }
	for name, d := range map[string]wakeshift.EventDef[args, payload]{
		"ci.public-personal":  {Description: "x", Public: true, Personal: true},
		"ci.public-authorize": {Description: "x", Public: true, Authorize: allow},
	} {
		if _, err := wakeshift.DefineEvent(app, name, d); !wakeshift.IsCode(err, wakeshift.CodeConfig) || !strings.Contains(err.Error(), "Public") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A custom route is checked too.
	for name, d := range map[string]wakeshift.EventDef[args, payload]{
		"ci.optional-topic":     {Topic: "ci/{branch}"},                                              // optional argument in the topic
		"ci.nope-topic":         {Topic: "ci/{nope}"},                                                // not an argument
		"ci.case-topic":         {Topic: "ci/{Repo}"},                                                // not a valid argument name
		"ci.bad-filter":         {Topic: "ci/{repo}", Filter: map[string]string{"nope": "{branch}"}}, // not a payload field
		"ci.bad-value":          {Topic: "ci/{repo}", Filter: map[string]string{"branch": "{nope}"}}, // not an argument
		"ci.bad-filter-default": {Filter: map[string]string{"nope": "{branch}"}},                     // the default topic's Filter: not a payload field
		"ci.bad-value-default":  {Filter: map[string]string{"status": "{nope}"}},                     // the default topic's Filter: not an argument
	} {
		if _, err := wakeshift.DefineEvent(app, name, def(d)); err == nil {
			t.Errorf("%s: a bad route was accepted", name)
		}
	}
	// A Filter on the field an optional argument already filters is refused,
	// naming the field, whatever its value: neither may quietly win.
	for _, tmpl := range []string{"{repo}", "{branch}"} {
		if _, err := wakeshift.DefineEvent(app, "ci.conflict", def(wakeshift.EventDef[args, payload]{Filter: map[string]string{"branch": tmpl}})); err == nil ||
			!strings.Contains(err.Error(), `Filter on "branch" conflicts with optional argument "branch"`) {
			t.Errorf("a Filter on branch (%s) with the default route: %v", tmpl, err)
		}
	}
	// With a Topic there's no default filter, so the same Filter is the app's own.
	if _, err := wakeshift.DefineEvent(app, "ci.own-filter", def(wakeshift.EventDef[args, payload]{Topic: "ci/{repo}", Filter: map[string]string{"branch": "{branch}"}})); err != nil {
		t.Errorf("a Filter on branch with a Topic: %v", err)
	}
	// Personal events need To, and only they take it.
	shared, _ := wakeshift.DefineEvent(app, "ci.shared", def(wakeshift.EventDef[args, payload]{}))
	if _, err := shared.Emit(context.Background(), args{Repo: "a"}, payload{}, wakeshift.To("u1")); err == nil {
		t.Error("To on a shared event")
	}
	mine, _ := wakeshift.DefineEvent(app, "ci.mine", def(wakeshift.EventDef[args, payload]{Personal: true}))
	if _, err := mine.Emit(context.Background(), args{Repo: "a"}, payload{}); err == nil {
		t.Error("a personal event without To")
	}
	// A payload that can't be encoded is an error before any request.
	type odd struct {
		F func() `json:"f"`
	}
	bad, err := wakeshift.DefineEvent(app, "ci.odd", wakeshift.EventDef[args, odd]{Description: "x", Text: func(odd, args) string { return "x" },
		PayloadSchema: []byte(`{"type":"object"}`), Topic: "ci/{repo}"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bad.Emit(context.Background(), args{Repo: "a"}, odd{F: func() {}}); !wakeshift.IsCode(err, wakeshift.CodeBadArguments) || !strings.Contains(err.Error(), "payload") {
		t.Errorf("an unencodable payload: %v", err)
	}
}

// toolJSON is one tool's object in a tools/list answer (as JSON text).
func toolJSON(list, name string) string {
	var r struct {
		Result struct {
			Tools []json.RawMessage `json:"tools"`
		} `json:"result"`
	}
	for _, line := range strings.Split(list, "\n") {
		line = strings.TrimPrefix(strings.TrimSpace(line), "data: ")
		if json.Unmarshal([]byte(line), &r) == nil && len(r.Result.Tools) > 0 {
			break
		}
	}
	for _, t := range r.Result.Tools {
		var n struct{ Name string }
		if json.Unmarshal(t, &n) == nil && n.Name == name {
			return string(t)
		}
	}
	return ""
}

// Attach with no event defined logs no_events (at warn, once per client)
// instead of refusing; attaching without identify says so only at debug.
func TestAttachWithoutEvents(t *testing.T) {
	var logs logLines
	app := testClient(t, "https://core.example", wakeshift.WithLogger(logs.logger()))
	for range 2 {
		app.Attach(mcp.NewServer(&mcp.Implementation{Name: "t", Version: "1"}, nil), wakeshift.AttachOptions{})
	}
	if got := logs.codes(); !slices.Equal(got, []string{"no_events"}) {
		t.Fatalf("logs: %v", got)
	}
	if n := strings.Count(logs.String(), `"level":"DEBUG","msg":"wakeshift: attach without identify`); n != 1 {
		t.Errorf("no_identify logged %d times at debug, want 1:\n%s", n, logs.String())
	}
}
