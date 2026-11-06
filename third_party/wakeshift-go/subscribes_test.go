package wakeshift_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/wakeshift-go"
)

func handler[In any](sub func(context.Context, *wakeshift.ToolCall, In) error) func(context.Context, *wakeshift.ToolCall, In) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, call *wakeshift.ToolCall, in In) (*mcp.CallToolResult, any, error) {
		if sub != nil {
			if err := sub(ctx, call, in); err != nil {
				return nil, nil, err
			}
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
	}
}

// tools lists the tools, returning each one as tools/list gives it, by name.
func (w *wrapped) tools(t *testing.T) map[string]map[string]any {
	t.Helper()
	return listTools(t, w.url, "Bearer ada")
}

// listTools asks the endpoint at url for tools/list.
func listTools(t *testing.T, url, authorization string) map[string]map[string]any {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": map[string]any{}})
	req, _ := http.NewRequest("POST", url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
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
	tools := map[string]map[string]any{}
	for _, tool := range out.Result.Tools {
		tools[tool["name"].(string)] = tool
	}
	return tools
}

func (w *wrapped) descriptions(t *testing.T) map[string]string {
	t.Helper()
	d := map[string]string{}
	for name, tool := range w.tools(t) {
		d[name], _ = tool["description"].(string)
	}
	return d
}

// shape is a schema as the shared fixtures compare it (README.md): its
// type, each property's type, description and enum, and what's required,
// sorted (left out when empty).
func shape(v any) any {
	s, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := map[string]any{}
	if t, ok := s["type"]; ok {
		out["type"] = t
	}
	if ps, ok := s["properties"].(map[string]any); ok {
		props := map[string]any{}
		for name, p := range ps {
			pm, _ := p.(map[string]any)
			prop := map[string]any{}
			for _, k := range []string{"type", "description", "enum"} {
				if v, ok := pm[k]; ok {
					prop[k] = v
				}
			}
			if items, ok := pm["items"].(map[string]any); ok && items["type"] != nil {
				prop["items"] = map[string]any{"type": items["type"]}
			}
			props[name] = prop
		}
		if len(props) > 0 { // README: properties left out when it has none
			out["properties"] = props
		}
	}
	if r, ok := s["required"].([]any); ok && len(r) > 0 {
		names := make([]string, len(r))
		for i, n := range r {
			names[i], _ = n.(string)
		}
		sort.Strings(names)
		req := make([]any, len(names))
		for i, n := range names {
			req[i] = n
		}
		out["required"] = req
	}
	return out
}

// toolShape is a listed tool as subscribing-tools-list.json compares it.
func toolShape(tool map[string]any) map[string]any {
	out := map[string]any{"name": tool["name"], "description": tool["description"], "inputSchema": shape(tool["inputSchema"])}
	if title, ok := tool["title"]; ok {
		out["title"] = title
	}
	if o, ok := tool["outputSchema"]; ok && o != nil {
		out["outputSchema"] = shape(o)
	}
	if a, ok := tool["annotations"].(map[string]any); ok && len(a) > 0 {
		out["annotations"] = a
	}
	return out
}

// subscribingFixture is subscribing-tools-list.json.
type subscribingFixture struct {
	App    string `json:"app"`
	Events map[string]struct {
		Description string          `json:"description"`
		Access      string          `json:"access"`
		Args        json.RawMessage `json:"args"`
		Payload     json.RawMessage `json:"payload"`
	} `json:"events"`
	VendorTools map[string]struct {
		Description  string          `json:"description"`
		InputSchema  json.RawMessage `json:"inputSchema"`
		OutputSchema json.RawMessage `json:"outputSchema"`
		Subscribes   []string        `json:"subscribes"`
	} `json:"vendor_tools"`
	Scenarios []struct {
		Name         string           `json:"name"`
		Enabled      bool             `json:"enabled"`
		Events       []string         `json:"events"`
		Tools        []string         `json:"tools"`
		BuiltinTools json.RawMessage  `json:"builtin_tools"`
		ToolsList    []map[string]any `json:"tools_list"`
	} `json:"scenarios"`
}

func readFixture(t *testing.T, name string, v any) {
	t.Helper()
	b, err := os.ReadFile(sharedGolden(name))
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

// builtinsOf is a fixture's builtin_tools as BuiltinTools.
// null: the default; false: every built-in off; else an object of flags.
func builtinsOf(b json.RawMessage) wakeshift.BuiltinTools {
	if string(b) == "false" {
		return wakeshift.BuiltinTools{DisableAll: true}
	}
	var m map[string]bool
	json.Unmarshal(b, &m)
	off := func(k string) bool { v, ok := m[k]; return ok && !v }
	return wakeshift.BuiltinTools{Wait: m["wait"], DisableSubscribe: off("subscribe"), DisableList: off("list"), DisableUnsubscribe: off("unsubscribe")}
}

// anyEvent is a fixture's event, its arguments and payload as maps.
type anyEvent = *wakeshift.Event[map[string]any, map[string]any]

// defineFixtureEvents defines a scenario's events on wake.
func defineFixtureEvents(t *testing.T, f subscribingFixture, wake *wakeshift.Client, names []string) map[string]anyEvent {
	t.Helper()
	out := map[string]anyEvent{}
	for _, name := range names {
		e := f.Events[name]
		def := wakeshift.EventDef[map[string]any, map[string]any]{Description: e.Description, ArgsSchema: e.Args, PayloadSchema: e.Payload}
		switch e.Access {
		case "personal":
			def.Personal = true
		case "public":
			def.Public = true
		case "authorize":
			def.Authorize = func(context.Context, wakeshift.Subscriber, map[string]any) (bool, error) { return true, nil }
		}
		ev, err := wakeshift.DefineEvent(wake, name, def)
		if err != nil {
			t.Fatal(err)
		}
		out[name] = ev
	}
	return out
}

// The tools/list a server serves (subscribing-tools-list.json), for each
// scenario, registering the tools that subscribe through the attachment
// (AddTool) and, separately, with go-sdk and naming them in Wrap: both
// the same.
func TestSubscribingToolsListGolden(t *testing.T) {
	var f subscribingFixture
	readFixture(t, "subscribing-tools-list.json", &f)
	if len(f.Scenarios) == 0 {
		t.Fatal("no scenarios")
	}
	for _, sc := range f.Scenarios {
		for _, path := range []string{"AddTool", "Wrap"} {
			t.Run(sc.Name+"/"+path, func(t *testing.T) {
				core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if strings.HasSuffix(r.URL.Path, "/app") {
						json.NewEncoder(w).Encode(map[string]any{"id": "app-test", "name": f.App})
						return
					}
					w.Write([]byte(`{}`))
				}))
				defer core.Close()
				var wake *wakeshift.Client
				if sc.Enabled {
					wake = testClient(t, core.URL)
				} else {
					wake = disabledClient(t)
				}
				events := defineFixtureEvents(t, f, wake, sc.Events)
				srv := mcp.NewServer(&mcp.Implementation{Name: "crawler", Version: "1"}, nil)
				opts := wakeshift.AttachOptions{BuiltinTools: builtinsOf(sc.BuiltinTools)}
				if path == "Wrap" {
					opts.Wrap = map[string][]wakeshift.ToolOption{}
					for _, name := range sc.Tools {
						if vt := f.VendorTools[name]; vt.Subscribes != nil {
							opts.Wrap[name] = []wakeshift.ToolOption{subscribesOf(events, vt.Subscribes)}
						}
					}
				}
				at := wake.Attach(srv, opts)
				for _, name := range sc.Tools {
					vt := f.VendorTools[name]
					tool := &mcp.Tool{Name: name, Description: vt.Description, InputSchema: json.RawMessage(vt.InputSchema)}
					if len(vt.OutputSchema) > 0 {
						tool.OutputSchema = json.RawMessage(vt.OutputSchema)
					}
					switch {
					case vt.Subscribes == nil || path == "Wrap":
						srv.AddTool(tool, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
							return &mcp.CallToolResult{}, nil
						})
					default:
						wakeshift.AddTool(at, tool, handler[map[string]any](nil), subscribesOf(events, vt.Subscribes))
					}
				}
				hs := httptest.NewServer(at.Handler(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
					&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})))
				defer hs.Close()
				listed := listTools(t, hs.URL, "")
				got := map[string]any{}
				for name, tool := range listed {
					got[name] = toolShape(tool)
				}
				want := map[string]any{}
				for _, tool := range sc.ToolsList {
					want[tool["name"].(string)] = toolShape(tool)
				}
				if g, w := canonicalJSON(t, got), canonicalJSON(t, want); !reflect.DeepEqual(g, w) {
					gb, _ := json.MarshalIndent(g, "", "  ")
					wb, _ := json.MarshalIndent(w, "", "  ")
					t.Fatalf("tools/list:\n%s\nwant:\n%s", gb, wb)
				}
			})
		}
	}
}

// subscribesOf is Subscribes for the named events.
func subscribesOf(events map[string]anyEvent, names []string) wakeshift.ToolOption {
	var subs []wakeshift.Subscribable
	for _, n := range names {
		subs = append(subs, events[n])
	}
	return wakeshift.Subscribes(subs...)
}

func canonicalJSON(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	json.Unmarshal(b, &out)
	return out
}

// The app's name, for the subscribes sentence, is asked of Wakeshift at
// most once per retry window, and tools/list waits for it only so long.
func TestSubscribesAppNameFetchIsBounded(t *testing.T) {
	defer wakeshift.SetNameFetch(100*time.Millisecond, time.Minute)()
	unnamed := "Watch a table. This tool is how this app notifies you in this conversation of this event: News at a table. This app delivers its notifications through Wakeshift; the result may ask you to start a background listener and to offer the user a one-time install. Follow it."
	appCalls := func(w *wrapped) int {
		w.core.mu.Lock()
		defer w.core.mu.Unlock()
		n := 0
		for _, c := range w.core.calls {
			if c.Path == "/app" {
				n++
			}
		}
		return n
	}
	register := func(w *wrapped) {
		wakeshift.AddTool(w.at, &mcp.Tool{Name: "watch", Description: "Watch a table."}, handler[tableIn](nil), wakeshift.Subscribes(w.shared))
	}

	// Unanswered: concurrent lists don't wait for long, and the next ones
	// don't ask again.
	slow := newWrapped(t, wakeshift.AttachOptions{})
	register(slow)
	slow.core.mu.Lock()
	slow.core.appName, slow.core.appDelay = "Shipyard", 10*time.Second
	slow.core.mu.Unlock()
	start := time.Now()
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if got := slow.descriptions(t)["watch"]; got != unnamed {
				t.Errorf("slow: %q", got)
			}
		})
	}
	wg.Wait()
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("4 concurrent tools/list took %s", d)
	}
	slow.descriptions(t)
	if n := appCalls(slow); n != 1 {
		t.Fatalf("slow: %d GET /app", n)
	}

	// Failing: asked once within the retry window, then again after it.
	failing := newWrapped(t, wakeshift.AttachOptions{})
	register(failing)
	failing.core.mu.Lock()
	failing.core.fail = map[string]bool{"/app": true}
	failing.core.mu.Unlock()
	failing.descriptions(t)
	failing.descriptions(t)
	if n := appCalls(failing); n != 1 {
		t.Fatalf("failing: %d GET /app", n)
	}
	defer wakeshift.SetNameFetch(100*time.Millisecond, 0)()
	failing.core.mu.Lock()
	failing.core.fail, failing.core.appName = nil, "Shipyard"
	failing.core.mu.Unlock()
	if got := failing.descriptions(t)["watch"]; got != "Watch a table. This tool is how Shipyard notifies you in this conversation of this event: News at a table. Shipyard delivers its notifications through Wakeshift; the result may ask you to start a background listener and to offer the user a one-time install. Follow it." {
		t.Fatalf("after the window: %q", got)
	}
	// Once known, it's never asked again.
	failing.descriptions(t)
	failing.descriptions(t)
	if n := appCalls(failing); n != 2 {
		t.Fatalf("after the name was known: %d GET /app", n)
	}
}

// A Subscribe to an event the tool doesn't name in Subscribes is refused
// (it used to warn), on the AddTool path and under Wrap, without asking
// Wakeshift and without logging.
func TestUndeclaredSubscribeIsAnError(t *testing.T) {
	var logs logLines
	var errs []error
	var mu sync.Mutex
	record := func(err error) {
		mu.Lock()
		errs = append(errs, err)
		mu.Unlock()
	}
	w := newWrapped(t, wakeshift.AttachOptions{}, nil, wakeshift.WithLogger(logs.logger()))
	wakeshift.AddTool(w.at, &mcp.Tool{Name: "watch"}, handler[tableIn](func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) error {
		_, err := w.turn.Subscribe(ctx, call, tableArgs{Table: in.Table})
		record(err)
		return nil
	}), wakeshift.Subscribes(w.shared))
	wakeshift.AddTool(w.at, &mcp.Tool{Name: "look"}, handler[tableIn](func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) error {
		_, err := w.shared.Subscribe(ctx, call, tableArgs{Table: in.Table})
		record(err)
		return nil
	}), wakeshift.Subscribes())
	w.callTool(t, "ada", "", "watch", map[string]any{"table": "oak"}, nil)
	w.callTool(t, "ada", "", "look", map[string]any{"table": "oak"}, nil)
	if len(errs) != 2 {
		t.Fatalf("errors: %v", errs)
	}
	for _, err := range errs {
		if !wakeshift.IsCode(err, wakeshift.CodeUndeclaredSubscribe) || !strings.Contains(err.Error(), "isn't in its subscribes") {
			t.Errorf("%v, want undeclared_subscribe", err)
		}
	}
	if slices.Contains(w.core.paths(), "/event-subscriptions") {
		t.Errorf("asked Wakeshift: %v", w.core.paths())
	}
	if got := logs.codes(); len(got) != 0 {
		t.Errorf("logged %v", got)
	}
}

// AddTool panics on a mistake in the registration, as mcp.AddTool does: a
// built-in's name, an input schema naming one of Wakeshift's arguments, an
// output schema naming wakeshift, another client's event, nil arguments.
// A second Attach on one server panics too.
func TestRegistrationPanics(t *testing.T) {
	w := newWrapped(t, wakeshift.AttachOptions{})
	other := testClient(t, "https://other.example")
	foreign, err := wakeshift.DefineEvent(other, "x.y", wakeshift.EventDef[tableArgs, turnMsg]{Description: "X"})
	if err != nil {
		t.Fatal(err)
	}
	type keyIn struct {
		SessionKey string `json:"session_key"`
	}
	type wakeOut struct {
		Wakeshift string `json:"wakeshift"`
	}
	panics := func(name, says string, f func()) {
		t.Helper()
		defer func() {
			r := recover()
			if s, _ := r.(string); r == nil || !strings.Contains(s, says) {
				t.Errorf("%s: %v, want a panic saying %q", name, r, says)
			}
		}()
		f()
	}
	panics("a built-in's name", "subscribe_events", func() {
		wakeshift.AddTool(w.at, &mcp.Tool{Name: "subscribe_events"}, handler[tableIn](nil), wakeshift.Subscribes(w.shared))
	})
	panics("session_key in the input", "session_key", func() {
		wakeshift.AddTool(w.at, &mcp.Tool{Name: "a"}, handler[keyIn](nil), wakeshift.Subscribes(w.shared))
	})
	panics("session_key in a given input schema", "session_key", func() {
		wakeshift.AddTool(w.at, &mcp.Tool{Name: "b", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"session_key": map[string]any{"type": "string"}}}},
			handler[map[string]any](nil), wakeshift.Subscribes(w.shared))
	})
	panics("wakeshift in the output", "wakeshift", func() {
		wakeshift.AddTool(w.at, &mcp.Tool{Name: "c"}, func(context.Context, *wakeshift.ToolCall, tableIn) (*mcp.CallToolResult, wakeOut, error) {
			return nil, wakeOut{}, nil
		}, wakeshift.Subscribes(w.shared))
	})
	panics("another client's event", "another", func() {
		wakeshift.AddTool(w.at, &mcp.Tool{Name: "d"}, handler[tableIn](nil), wakeshift.Subscribes(foreign))
	})
	panics("a nil handler", "handler", func() {
		wakeshift.AddTool[tableIn, any](w.at, &mcp.Tool{Name: "e"}, nil)
	})
	panics("a second attach", "attached already", func() { w.app.Attach(w.srv, wakeshift.AttachOptions{}) })
	// The same on a disabled client.
	off := disabledClient(t)
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "1"}, nil)
	off.Attach(srv, wakeshift.AttachOptions{})
	panics("a second attach, disabled", "attached already", func() { off.Attach(srv, wakeshift.AttachOptions{}) })
}

// Under Wrap, a tool whose schema names one of Wakeshift's arguments is
// served unwrapped (logged once, at error), and a name the server doesn't
// list is logged once; a tool named like a built-in, registered outside
// Wakeshift, is replaced by the built-in (logged once).
func TestWrapAndBuiltinConflicts(t *testing.T) {
	var logs logLines
	w := newWrapped(t, wakeshift.AttachOptions{}, func(o *wakeshift.AttachOptions, w *wrapped) {
		o.Wrap = map[string][]wakeshift.ToolOption{
			"legacy_watch": {wakeshift.Subscribes(w.shared)},
			"stat_crawl":   {wakeshift.Subscribes(w.shared)},
		}
	}, wakeshift.WithLogger(logs.logger()))
	type legacyIn struct {
		Table      string `json:"table"`
		SessionKey string `json:"session_key,omitempty"`
	}
	var got legacyIn
	mcp.AddTool(w.srv, &mcp.Tool{Name: "legacy_watch", Description: "Watch."}, func(_ context.Context, _ *mcp.CallToolRequest, in legacyIn) (*mcp.CallToolResult, any, error) {
		got = in
		return &mcp.CallToolResult{}, nil, nil
	})
	mcp.AddTool(w.srv, &mcp.Tool{Name: "list_subscriptions", Description: "Mine."}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "mine"}}}, nil, nil
	})
	for range 2 {
		tools := w.tools(t)
		if d := tools["legacy_watch"]["description"]; d != "Watch." {
			t.Errorf("legacy_watch was amended: %v", d)
		}
		if d := tools["list_subscriptions"]["description"]; d != wakeshift.CompactStrings.ListDescription {
			t.Errorf("list_subscriptions: %v", d)
		}
	}
	// The tool's own session_key reaches it, as its own argument.
	w.callTool(t, "ada", "", "legacy_watch", map[string]any{"table": "oak", "session_key": "mine"}, nil)
	if got.SessionKey != "mine" {
		t.Errorf("legacy_watch's own session_key: %+v", got)
	}
	// Wakeshift's list_subscriptions is the one served.
	res := w.callTool(t, "ada", "", "list_subscriptions", map[string]any{}, nil)
	if strings.Join(res.texts(), "") == "mine" {
		t.Errorf("the vendor's list_subscriptions was served")
	}
	want := map[string]int{"wrap_arg_conflict": 1, "wrap_unknown_tool": 1, "builtin_name_conflict": 1}
	n := map[string]int{}
	for _, m := range logs.lines() {
		if c, _ := m["code"].(string); want[c] > 0 {
			n[c]++
			wantTool := map[string]string{"wrap_arg_conflict": "legacy_watch", "wrap_unknown_tool": "stat_crawl", "builtin_name_conflict": "list_subscriptions"}[c]
			if m["tool"] != wantTool {
				t.Errorf("%s: tool %v, want %s", c, m["tool"], wantTool)
			}
			wantLevel := map[string]string{"wrap_arg_conflict": "ERROR", "wrap_unknown_tool": "WARN", "builtin_name_conflict": "ERROR"}[c]
			if m["level"] != wantLevel {
				t.Errorf("%s: level %v, want %s", c, m["level"], wantLevel)
			}
		}
	}
	if !reflect.DeepEqual(n, want) {
		t.Errorf("logged %v, want %v", n, want)
	}
}

// A tool that doesn't subscribe never sees Wakeshift's arguments: they're
// stripped from every call whose tool doesn't name them, before go-sdk
// validates (its inferred schema would refuse them).
func TestToolsThatDontSubscribeNeverSeeWakeArgs(t *testing.T) {
	w := newWrapped(t, wakeshift.AttachOptions{})
	var raw json.RawMessage
	mcp.AddTool(w.srv, &mcp.Tool{Name: "get_page"}, func(_ context.Context, req *mcp.CallToolRequest, in tableIn) (*mcp.CallToolResult, any, error) {
		raw = req.Params.Arguments
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "page"}}}, nil, nil
	})
	res := w.callTool(t, "ada", "", "get_page", map[string]any{"table": "oak", "session_key": "wsc_secretsecretsecret", "harness": "codex", "conversation_id": "c1"}, nil)
	if res.IsError || res.RPCError != nil || strings.Contains(string(raw), "session_key") || strings.Contains(string(raw), "harness") ||
		strings.Contains(string(raw), "conversation_id") || !strings.Contains(string(raw), "oak") {
		t.Fatalf("get_page saw %s (%+v)", raw, res)
	}
}
