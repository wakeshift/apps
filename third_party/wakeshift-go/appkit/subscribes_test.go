package appkit_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/wakeshift-go/appkit"
)

// warnings captures what's logged at warning level while a test runs.
type warnings struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *warnings) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// messages is each warning's message, in order.
func (l *warnings) messages() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for line := range strings.SplitSeq(strings.TrimSpace(l.buf.String()), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && m["level"] == "WARN" {
			out = append(out, m["msg"].(string))
		}
	}
	return out
}

func captureWarnings(t *testing.T) *warnings {
	l := &warnings{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(l, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return l
}

// Each tool its own input type, as tools/list finds a go-sdk tool's
// declaration by its schemas.
type (
	watchIn struct {
		Table string `json:"table"`
		appkit.WakeArgs
	}
	joinIn struct {
		Table string `json:"table"`
		Seat  int    `json:"seat,omitempty"`
		appkit.WakeArgs
	}
	customIn struct {
		Custom string `json:"custom,omitempty"`
		appkit.WakeArgs
	}
	nothingIn struct {
		Look string `json:"look,omitempty"`
		appkit.WakeArgs
	}
	oldIn struct {
		Old string `json:"old,omitempty"`
		appkit.WakeArgs
	}
)

func subscribeTo(ev *appkit.DefinedEvent[tableArgs, turnMsg]) func(context.Context, *appkit.ToolCall, string) error {
	return func(ctx context.Context, call *appkit.ToolCall, table string) error {
		_, err := ev.Subscribe(ctx, call, tableArgs{Table: table})
		return err
	}
}

func handler[In any](sub func(context.Context, *appkit.ToolCall, In) error) func(context.Context, *appkit.ToolCall, In) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, call *appkit.ToolCall, in In) (*mcp.CallToolResult, any, error) {
		if sub != nil {
			if err := sub(ctx, call, in); err != nil {
				return nil, nil, err
			}
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
	}
}

// descriptions lists the tools, returning each one's description by name.
func (w *wrapped) descriptions(t *testing.T) map[string]string {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": map[string]any{}})
	req, _ := http.NewRequest("POST", w.url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer ada")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	d := map[string]string{}
	for _, tool := range out.Result.Tools {
		d[tool.Name] = tool.Description
	}
	return d
}

func notifiesSentence(app, of string) string {
	return "This tool is how " + app + " notifies you in this conversation " + of + ". " + strings.ToUpper(app[:1]) + app[1:] +
		" delivers its notifications through Wakeshift; the result may ask you to start a background listener and to offer the user a one-time install. Follow it."
}

// tools/list adds the sentence from the declared events' descriptions to
// each go-sdk tool that declares them, found by its schemas before it's
// called, naming the app; never twice (a Notifies description keeps its
// own); nothing for a tool that declares none.
func TestSubscribesToolsList(t *testing.T) {
	logs := captureWarnings(t)
	w := newWrapped(t, appkit.AttachOptions{})
	w.core.appName = "Shipyard"
	mcp.AddTool(w.srv, &mcp.Tool{Name: "watch", Description: "Watch a table."}, appkit.Events(w.at,
		handler(func(ctx context.Context, call *appkit.ToolCall, in watchIn) error {
			return subscribeTo(w.shared)(ctx, call, in.Table)
		}),
		appkit.Subscribes(w.shared)))
	mcp.AddTool(w.srv, &mcp.Tool{Name: "join", Description: "Join a table."}, appkit.Events(w.at, handler[joinIn](nil), appkit.Subscribes(w.turn, w.shared)))
	mcp.AddTool(w.srv, &mcp.Tool{Name: "custom", Description: appkit.Notifies("Watch a table.", appkit.NotifyWhen("news breaks"))},
		appkit.Events(w.at, handler[customIn](nil), appkit.Subscribes(w.shared)))
	mcp.AddTool(w.srv, &mcp.Tool{Name: "nothing", Description: "Look at a table."}, appkit.Events(w.at, handler[nothingIn](nil), appkit.Subscribes()))
	mcp.AddTool(w.srv, &mcp.Tool{Name: "old", Description: "Old style."}, appkit.Events(w.at, handler[oldIn](nil)))

	d := w.descriptions(t)
	want := map[string]string{
		"watch":   "Watch a table. " + notifiesSentence("Shipyard", "of this event: News at a table"),
		"join":    "Join a table. " + notifiesSentence("Shipyard", "of these events: Your turn; News at a table"),
		"custom":  "Watch a table. " + notifiesSentence("Shipyard", "when news breaks"),
		"nothing": "Look at a table.",
		"old":     "Old style.",
	}
	for name, wd := range want {
		if d[name] != wd {
			t.Errorf("%s:\n got %q\nwant %q", name, d[name], wd)
		}
	}
	// A declared Subscribe doesn't warn.
	if r := w.callTool(t, "ada", "", "watch", map[string]any{"table": "oak"}, nil); r.IsError {
		t.Fatalf("%+v", r)
	}
	if got := w.descriptions(t)["watch"]; got != want["watch"] {
		t.Fatalf("after the call: %q", got)
	}
	if m := logs.messages(); len(m) != 0 {
		t.Fatalf("warnings: %q", m)
	}
	// No name from Wakeshift: "this app".
	w2 := newWrapped(t, appkit.AttachOptions{})
	mcp.AddTool(w2.srv, &mcp.Tool{Name: "watch", Description: "Watch a table."}, appkit.Events(w2.at, handler[watchIn](nil), appkit.Subscribes(w2.shared)))
	if got := w2.descriptions(t)["watch"]; got != "Watch a table. "+notifiesSentence("this app", "of this event: News at a table") {
		t.Fatalf("unnamed: %q", got)
	}
}

// tools/list waits only so long for the app's name (GET /app), saying
// "this app" without it, and doesn't ask again soon after a failure (an
// unanswered request, an error): a slow or failing Wakeshift neither
// stalls nor multiplies tools/list. A name it gives, even "", is kept.
func TestSubscribesAppNameFetchIsBounded(t *testing.T) {
	defer appkit.SetNameFetch(100*time.Millisecond, time.Minute)()
	unnamed := "Watch a table. " + notifiesSentence("this app", "of this event: News at a table")
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
		mcp.AddTool(w.srv, &mcp.Tool{Name: "watch", Description: "Watch a table."}, appkit.Events(w.at, handler[watchIn](nil), appkit.Subscribes(w.shared)))
	}

	// Unanswered: concurrent lists don't queue behind it, and the next ones
	// don't ask again.
	slow := newWrapped(t, appkit.AttachOptions{})
	register(slow)
	slow.core.mu.Lock()
	slow.core.appName, slow.core.appDelay = "Shipyard", 10*time.Second
	slow.core.mu.Unlock()
	start := time.Now()
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := slow.descriptions(t)["watch"]; got != unnamed {
				t.Errorf("slow: %q", got)
			}
		}()
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
	failing := newWrapped(t, appkit.AttachOptions{})
	register(failing)
	failing.core.mu.Lock()
	failing.core.fail = map[string]bool{"/app": true}
	failing.core.mu.Unlock()
	failing.descriptions(t)
	failing.descriptions(t)
	if n := appCalls(failing); n != 1 {
		t.Fatalf("failing: %d GET /app", n)
	}
	defer appkit.SetNameFetch(100*time.Millisecond, 0)()
	failing.core.mu.Lock()
	failing.core.fail, failing.core.appName = nil, "Shipyard"
	failing.core.mu.Unlock()
	if got := failing.descriptions(t)["watch"]; got != "Watch a table. "+notifiesSentence("Shipyard", "of this event: News at a table") {
		t.Fatalf("named: %q", got)
	}
	failing.descriptions(t)
	if n := appCalls(failing); n != 2 {
		t.Fatalf("after the retry window: %d GET /app", n)
	}
}

// Two tools sharing an input type but declaring differently can't be told
// apart before they're called: neither gets the sentence, with a warning;
// once one is called, its name is known and it gets its own.
func TestSubscribesSharedInputType(t *testing.T) {
	logs := captureWarnings(t)
	w := newWrapped(t, appkit.AttachOptions{})
	mcp.AddTool(w.srv, &mcp.Tool{Name: "watch", Description: "Watch a table."}, appkit.Events(w.at, handler[watchIn](nil), appkit.Subscribes(w.shared)))
	mcp.AddTool(w.srv, &mcp.Tool{Name: "peek", Description: "Peek at a table."}, appkit.Events(w.at, handler[watchIn](nil)))
	d := w.descriptions(t)
	if d["watch"] != "Watch a table." || d["peek"] != "Peek at a table." {
		t.Fatalf("%q", d)
	}
	m := logs.messages()
	if len(m) != 1 || !strings.Contains(m[0], "the tools peek, watch take the same input type but declare different Subscribes") || !strings.Contains(m[0], "Fix: name each with appkit.ToolName") {
		t.Fatalf("warnings: %q", m)
	}
	w.callTool(t, "ada", "", "watch", map[string]any{"table": "oak"}, nil)
	d = w.descriptions(t)
	if d["watch"] != "Watch a table. "+notifiesSentence("this app", "of this event: News at a table") || d["peek"] != "Peek at a table." {
		t.Fatalf("after a call: %q", d)
	}
}

// ToolName names a tool that shares its input type: tools/list finds it by
// name before it's called, with no warning.
func TestSubscribesToolName(t *testing.T) {
	logs := captureWarnings(t)
	w := newWrapped(t, appkit.AttachOptions{})
	mcp.AddTool(w.srv, &mcp.Tool{Name: "watch", Description: "Watch a table."}, appkit.Events(w.at, handler[watchIn](nil), appkit.Subscribes(w.shared), appkit.ToolName("watch")))
	mcp.AddTool(w.srv, &mcp.Tool{Name: "peek", Description: "Peek at a table."}, appkit.Events(w.at, handler[watchIn](nil)))
	d := w.descriptions(t)
	if d["watch"] != "Watch a table. "+notifiesSentence("this app", "of this event: News at a table") || d["peek"] != "Peek at a table." {
		t.Fatalf("%q", d)
	}
	if m := logs.messages(); len(m) != 0 {
		t.Fatalf("warnings: %q", m)
	}
}

// A Subscribe the tool doesn't declare works, and warns once per tool with
// the fix; with undeclared subscribes an error (the next release), it's
// refused with the fix and asks Wakeshift nothing.
func TestSubscribesUndeclared(t *testing.T) {
	logs := captureWarnings(t)
	w := newWrapped(t, appkit.AttachOptions{})
	sub := func(ctx context.Context, call *appkit.ToolCall, in tableIn) error {
		return subscribeTo(w.shared)(ctx, call, in.Table)
	}
	mcp.AddTool(w.srv, &mcp.Tool{Name: "old"}, appkit.Events(w.at, handler(sub)))
	mcp.AddTool(w.srv, &mcp.Tool{Name: "other"}, appkit.Events(w.at, handler(sub), appkit.Subscribes(w.turn)))
	mcp.AddTool(w.srv, &mcp.Tool{Name: "none"}, appkit.Events(w.at, handler(sub), appkit.Subscribes()))
	for range 2 {
		for _, tool := range []string{"old", "other", "none"} {
			if r := w.callTool(t, "ada", "", tool, map[string]any{"table": "oak"}, nil); r.IsError {
				t.Fatalf("%s: %+v", tool, r)
			}
		}
	}
	if n := strings.Count(strings.Join(w.core.paths(), " "), "/event-subscriptions"); n != 6 {
		t.Fatalf("%d subscribes: %q", n, w.core.paths())
	}
	var undeclared []string
	for _, m := range logs.messages() {
		if strings.Contains(m, "subscribed to table.news") {
			undeclared = append(undeclared, m)
		}
	}
	wantMsgs := []string{
		"appkit: the tool old subscribed to table.news without declaring the events it subscribes to, so tools/list doesn't tell the agent that the tool is how this app notifies it. Fix: appkit.Events(attached, handler, appkit.Subscribes(<table.news's event>)). It works for now; from the next release an undeclared Subscribe returns an error.",
		"appkit: the tool other subscribed to table.news, which its Subscribes doesn't list (game.turn), so tools/list doesn't tell the agent that the tool is how this app notifies it. Fix: appkit.Events(attached, handler, appkit.Subscribes(<game.turn's event>, <table.news's event>)). It works for now; from the next release an undeclared Subscribe returns an error.",
		"appkit: the tool none subscribed to table.news, but declares that it subscribes to nothing, so tools/list doesn't tell the agent that the tool is how this app notifies it. Fix: appkit.Events(attached, handler, appkit.Subscribes(<table.news's event>)). It works for now; from the next release an undeclared Subscribe returns an error.",
	}
	if !eqStrings(undeclared, wantMsgs) {
		t.Fatalf("warnings:\n%q", undeclared)
	}

	t.Cleanup(appkit.SetUndeclaredIsError(true))
	e := newWrapped(t, appkit.AttachOptions{})
	esub := func(ctx context.Context, call *appkit.ToolCall, in tableIn) error {
		return subscribeTo(e.shared)(ctx, call, in.Table)
	}
	mcp.AddTool(e.srv, &mcp.Tool{Name: "old"}, appkit.Events(e.at, handler(esub)))
	mcp.AddTool(e.srv, &mcp.Tool{Name: "ok"}, appkit.Events(e.at, handler(esub), appkit.Subscribes(e.shared)))
	r := e.callTool(t, "ada", "", "old", map[string]any{"table": "oak"}, nil)
	if !r.IsError || !strings.Contains(strings.Join(r.texts(), "\n"), "appkit: the tool old subscribed to table.news without declaring the events it subscribes to, so the subscribe was refused. Fix: appkit.Events(attached, handler, appkit.Subscribes(<table.news's event>)).") {
		t.Fatalf("refused: %+v", r)
	}
	if strings.Contains(strings.Join(e.core.paths(), " "), "/event-subscriptions") {
		t.Fatalf("asked Wakeshift: %q", e.core.paths())
	}
	if r := e.callTool(t, "ada", "", "ok", map[string]any{"table": "oak"}, nil); r.IsError {
		t.Fatalf("declared: %+v", r)
	}
}

// Subscribes takes only this app's events: another's panics at startup.
func TestSubscribesOtherAppsEvent(t *testing.T) {
	w := newWrapped(t, appkit.AttachOptions{})
	other := newWrapped(t, appkit.AttachOptions{})
	defer func() {
		if r := recover(); r == nil || !strings.Contains(r.(string), "appkit.Subscribes lists table.news, which isn't one of this app's events") {
			t.Fatalf("recovered %v", r)
		}
	}()
	appkit.Events(w.at, handler[tableIn](nil), appkit.Subscribes(other.shared))
}

// A declaring tool whose listed input schema isn't the one go-sdk infers
// from its input type (set by hand) can't be found: it gets no sentence, and
// one warning names the fix. A tool found by its schema is never warned about.
func TestSubscribesUnmatched(t *testing.T) {
	logs := captureWarnings(t)
	w := newWrapped(t, appkit.AttachOptions{})
	mcp.AddTool(w.srv, &mcp.Tool{Name: "watch", Description: "Watch a table."}, appkit.Events(w.at, handler[watchIn](nil), appkit.Subscribes(w.shared)))
	schema, err := jsonschema.For[oldIn](nil)
	if err != nil {
		t.Fatal(err)
	}
	schema.Properties["extra"] = &jsonschema.Schema{Type: "string"}
	mcp.AddTool(w.srv, &mcp.Tool{Name: "peek", Description: "Peek at a table.", InputSchema: schema}, appkit.Events(w.at, handler[oldIn](nil), appkit.Subscribes(w.shared)))
	for range 2 {
		d := w.descriptions(t)
		if d["watch"] != "Watch a table. "+notifiesSentence("this app", "of this event: News at a table") || d["peek"] != "Peek at a table." {
			t.Fatalf("%q", d)
		}
	}
	m := logs.messages()
	if len(m) != 1 || !strings.Contains(m[0], "appkit.Subscribes(table.news) isn't in tools/list") || !strings.Contains(m[0], "Fix: name the tool with appkit.ToolName") {
		t.Fatalf("warnings: %q", m)
	}
}
