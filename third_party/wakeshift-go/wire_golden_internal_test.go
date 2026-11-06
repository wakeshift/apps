package wakeshift

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/wakeshift-go/internal/mcpevents/event"
)

// mcp-events-wire.json (with the TypeScript and Python SDKs): the wire
// format every SDK serves a draft client, both sides of each
// events/subscribe and events/poll.
func TestMCPEventsWireGolden(t *testing.T) {
	type wireCase struct {
		Name         string          `json:"name"`
		Params       json.RawMessage `json:"params"`
		CoreRequest  map[string]any  `json:"core_request"`
		CoreResponse json.RawMessage `json:"core_response"`
		Result       map[string]any  `json:"result"`
	}
	var g struct {
		Revision  string     `json:"revision"`
		Delivery  []string   `json:"delivery"`
		Subscribe []wireCase `json:"subscribe"`
		Poll      []wireCase `json:"poll"`
	}
	readSharedFixture(t, "mcp-events-wire.json", &g)
	if g.Revision != event.Revision || !reflect.DeepEqual(g.Delivery, event.Delivery) {
		t.Fatalf("revision %q delivery %v, want %q %v", g.Revision, g.Delivery, event.Revision, event.Delivery)
	}
	for kind, cases := range map[string][]wireCase{"subscribe": g.Subscribe, "poll": g.Poll} {
		for _, c := range cases {
			var mu sync.Mutex
			var asked map[string]any
			core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path != "/api/apps/v1/events/"+kind {
					io.WriteString(w, `{}`)
					return
				}
				var body map[string]any
				json.NewDecoder(r.Body).Decode(&body)
				mu.Lock()
				asked = body
				mu.Unlock()
				w.Write(c.CoreResponse)
			}))
			app := clientAt(t, core.URL)
			if _, err := DefineEvent(app, "chat.message", EventDef[wireArgs, wireMsg]{Description: "A new message in a channel", Public: true,
				Text: func(m wireMsg, _ wireArgs) string { return m.From + ": " + m.Text }}); err != nil {
				t.Fatal(err)
			}
			attached := app.Attach(mcp.NewServer(&mcp.Implementation{Name: "chat", Version: "1"}, nil), AttachOptions{
				Identify: func(_ context.Context, r Request) (Identity, error) { return Identity{User: r.Token.UserID}, nil }})
			h := auth.RequireBearerToken(func(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
				return &auth.TokenInfo{UserID: token, Expiration: time.Now().Add(time.Hour)}, nil
			}, nil)(attached.Handler(http.NotFoundHandler()))
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "events/" + kind, "params": c.Params})
			req := httptest.NewRequest("POST", "/mcp", bytes.NewReader(b))
			req.Header.Set("Authorization", "Bearer u1")
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("User-Agent", "some-draft-client/1")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			core.Close()
			var resp struct {
				Result map[string]any `json:"result"`
				Error  any            `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Error != nil {
				t.Fatalf("%s: %s: %d %s", kind, c.Name, rec.Code, rec.Body)
			}
			if !reflect.DeepEqual(resp.Result, c.Result) {
				t.Errorf("%s: %s: result\n got %v\nwant %v", kind, c.Name, resp.Result, c.Result)
			}
			mu.Lock()
			call, _ := asked["call"].(map[string]any)
			delete(asked, "call")
			delete(asked, "dialect")
			if call["user"] != "u1" || !reflect.DeepEqual(asked, c.CoreRequest) {
				t.Errorf("%s: %s: asked Wakeshift (as %v)\n got %v\nwant %v", kind, c.Name, call["user"], asked, c.CoreRequest)
			}
			mu.Unlock()
		}
	}
}

type wireArgs struct {
	Channel string `json:"channel"`
	From    string `json:"from,omitempty"`
}

type wireMsg struct {
	From string `json:"from"`
	Text string `json:"text"`
}

// whoServer is the next release's who-may-subscribe app (mcp-events-wire.json
// next): chat.message public, crawl.finished tools-only, deploy.finished
// with an authorize that allows only u1, and game.turn personal, behind
// Handler, with the user from the X-User header (none without one). It
// records what it asks Wakeshift.
type whoServer struct {
	url   string
	mu    sync.Mutex
	asked []string // paths
	auth  []Subscriber
	logs  *bytes.Buffer
}

type crawlArgs struct {
	Job string `json:"job"`
}

type crawlMsg struct {
	Pages float64 `json:"pages"`
}

type deployArgs struct {
	Env string `json:"env"`
}

type deployMsg struct {
	Status string `json:"status"`
}

type turnArgs struct {
	Table string `json:"table"`
}

func newWhoServer(t *testing.T, tools func(*Attachment, *whoEvents)) *whoServer {
	t.Helper()
	ws := &whoServer{logs: &bytes.Buffer{}}
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/apps/v1")
		if !strings.HasPrefix(path, "/manifest") && path != "/app" {
			ws.mu.Lock()
			ws.asked = append(ws.asked, path)
			ws.mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		switch path {
		case "/events/unsubscribe", "/event-subscriptions/unsubscribe":
			io.WriteString(w, `{"ended": true}`)
		case "/event-subscriptions":
			io.WriteString(w, `{"event_subscription": {"id": "s1", "user": "u1", "created": true}, "session": "wsc_fakefakefakefakefakefakefa", "pending": "pa_1"}`)
		case "/calls/addition":
			io.WriteString(w, `{"text": "Woken by Monitor."}`)
		default:
			io.WriteString(w, `{}`)
		}
	}))
	t.Cleanup(core.Close)
	var logMu sync.Mutex
	app := clientAt(t, core.URL, WithLogger(slog.New(slog.NewJSONHandler(writerFunc(func(p []byte) (int, error) {
		logMu.Lock()
		defer logMu.Unlock()
		return ws.logs.Write(p)
	}), nil))))
	ev := &whoEvents{}
	var err error
	if ev.chat, err = DefineEvent(app, "chat.message", EventDef[wireArgs, wireMsg]{Description: "A new message in a channel", Public: true}); err != nil {
		t.Fatal(err)
	}
	if ev.crawl, err = DefineEvent(app, "crawl.finished", EventDef[crawlArgs, crawlMsg]{Description: "A crawl finished"}); err != nil {
		t.Fatal(err)
	}
	if ev.deploy, err = DefineEvent(app, "deploy.finished", EventDef[deployArgs, deployMsg]{Description: "A deploy finished",
		Authorize: func(_ context.Context, s Subscriber, a deployArgs) (bool, error) {
			ws.mu.Lock()
			ws.auth = append(ws.auth, s)
			ws.mu.Unlock()
			return s.User == "u1", nil
		}}); err != nil {
		t.Fatal(err)
	}
	if ev.turn, err = DefineEvent(app, "game.turn", EventDef[turnArgs, wireMsg]{Description: "Your move", Personal: true}); err != nil {
		t.Fatal(err)
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "who", Version: "1"}, nil)
	at := app.Attach(srv, AttachOptions{
		Identify: func(_ context.Context, r Request) (Identity, error) {
			return Identity{User: r.Header.Get("X-User")}, nil
		},
		IsNativeClient: func(_ context.Context, r Request) bool { return r.Header.Get("X-Native") == "1" },
	})
	if tools != nil {
		tools(at, ev)
	}
	hs := httptest.NewServer(at.Handler(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})))
	t.Cleanup(hs.Close)
	ws.url = hs.URL
	return ws
}

type whoEvents struct {
	chat   *Event[wireArgs, wireMsg]
	crawl  *Event[crawlArgs, crawlMsg]
	deploy *Event[deployArgs, deployMsg]
	turn   *Event[turnArgs, wireMsg]
}

// rpc sends one JSON-RPC request as user (none for ""), from ua.
func (ws *whoServer) rpc(t *testing.T, user, ua, method string, params any, header ...string) (result map[string]any, rpcErr map[string]any) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, _ := http.NewRequest("POST", ws.url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", ua)
	if user != "" {
		req.Header.Set("X-User", user)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out struct {
		Result map[string]any `json:"result"`
		Error  map[string]any `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s: %d %s", method, resp.StatusCode, raw)
	}
	return out.Result, out.Error
}

func (ws *whoServer) askedPaths() []string {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return append([]string(nil), ws.asked...)
}

// Who may subscribe, on the MCP Events wire and through the built-in
// tools: events/list names only open events; every refusal for a
// tools-only, public-without-a-user or refused event is the SDK's own,
// without asking Wakeshift; unsubscribing stays allowed.
func TestWhoMaySubscribeWireGolden(t *testing.T) {
	var g struct {
		Next struct {
			EventsList []string `json:"events_list"`
			Refused    []struct {
				Name   string         `json:"name"`
				Method string         `json:"method"`
				User   *string        `json:"user"`
				Params map[string]any `json:"params"`
				Error  struct {
					Code    map[string]float64 `json:"code"`
					Message string             `json:"message"`
					Data    any                `json:"data"`
				} `json:"error"`
			} `json:"refused"`
			UnsubscribeToolsOnly struct {
				Method string         `json:"method"`
				User   string         `json:"user"`
				Params map[string]any `json:"params"`
				Result map[string]any `json:"result"`
			} `json:"unsubscribe_tools_only"`
			BuiltinCalls []struct {
				Name      string         `json:"name"`
				Tool      string         `json:"tool"`
				User      string         `json:"user"`
				Arguments map[string]any `json:"arguments"`
				IsError   bool           `json:"isError"`
				Text      string         `json:"text"`
				CoreAsked bool           `json:"core_asked"`
			} `json:"builtin_calls"`
		} `json:"next"`
	}
	readSharedFixture(t, "mcp-events-wire.json", &g)
	ws := newWhoServer(t, nil)
	res, _ := ws.rpc(t, "u1", "some-draft-client/1", "events/list", map[string]any{})
	var names []string
	for _, e := range res["events"].([]any) {
		names = append(names, e.(map[string]any)["name"].(string))
	}
	// game.turn (personal) is open too; the fixture's app has none.
	if want := append(append([]string{}, g.Next.EventsList...), "game.turn"); !reflect.DeepEqual(names, want) {
		t.Errorf("events/list: %v, want %v", names, want)
	}
	for _, c := range g.Next.Refused {
		for dialect, ua := range map[string]string{"draft": "some-draft-client/1", "chatgpt": "openai-mcp/1.0.0"} {
			before := len(ws.askedPaths())
			user := ""
			if c.User != nil {
				user = *c.User
			}
			_, e := ws.rpc(t, user, ua, c.Method, c.Params)
			if e == nil || e["code"] != c.Error.Code[dialect] || e["message"] != c.Error.Message || e["data"] != c.Error.Data {
				t.Errorf("%s (%s): %v, want %v %q", c.Name, dialect, e, c.Error.Code[dialect], c.Error.Message)
			}
			if after := ws.askedPaths(); len(after) != before {
				t.Errorf("%s (%s): asked Wakeshift %v", c.Name, dialect, after[before:])
			}
		}
	}
	u := g.Next.UnsubscribeToolsOnly
	if res, e := ws.rpc(t, u.User, "some-draft-client/1", u.Method, u.Params); e != nil || !reflect.DeepEqual(res, u.Result) {
		t.Errorf("unsubscribe from a tools-only event: %v %v, want %v", res, e, u.Result)
	}
	for _, c := range g.Next.BuiltinCalls {
		before := len(ws.askedPaths())
		res, e := ws.rpc(t, c.User, "claude-code/2.1", "tools/call", map[string]any{"name": c.Tool, "arguments": c.Arguments})
		if e != nil {
			t.Fatalf("%s: %v", c.Name, e)
		}
		isErr, _ := res["isError"].(bool)
		text := ""
		if content, _ := res["content"].([]any); len(content) > 0 {
			text, _ = content[0].(map[string]any)["text"].(string)
		}
		asked := len(ws.askedPaths()) > before
		if isErr != c.IsError || (c.Text != "" && text != c.Text) || asked != c.CoreAsked {
			t.Errorf("%s: isError %v %q (asked %v), want %v %q (asked %v)", c.Name, isErr, text, asked, c.IsError, c.Text, c.CoreAsked)
		}
	}
}

// call.subscribe's result for each access setting and caller
// (mcp-events-wire.json next.tool_subscribe_states).
func TestToolSubscribeStatesGolden(t *testing.T) {
	var g struct {
		Next struct {
			States struct {
				Rows []struct {
					Access string  `json:"access"`
					Caller string  `json:"caller"`
					User   *string `json:"user"`
					Want   struct {
						State  SubscriptionState `json:"state"`
						Reason string            `json:"reason"`
						Error  Code              `json:"error"`
						Says   string            `json:"says"`
					} `json:"want"`
					NativeLine          bool   `json:"native_line"`
					CoreAsked           bool   `json:"core_asked"`
					Logs                string `json:"logs"`
					AuthorizeCalledWith *struct {
						User   string         `json:"user"`
						Native bool           `json:"native"`
						Args   map[string]any `json:"args"`
					} `json:"authorize_called_with"`
				} `json:"rows"`
			} `json:"tool_subscribe_states"`
		} `json:"next"`
	}
	readSharedFixture(t, "mcp-events-wire.json", &g)
	type outcome struct {
		sub Subscription
		err error
	}
	var mu sync.Mutex
	var last outcome
	ws := newWhoServer(t, func(at *Attachment, ev *whoEvents) {
		AddTool(at, &mcp.Tool{Name: "watch"}, func(ctx context.Context, call *ToolCall, in struct {
			Access string `json:"access"`
		}) (*mcp.CallToolResult, any, error) {
			var o outcome
			switch in.Access {
			case "tools":
				o.sub, o.err = ev.crawl.Subscribe(ctx, call, crawlArgs{Job: "j1"})
			case "personal":
				o.sub, o.err = ev.turn.Subscribe(ctx, call, turnArgs{Table: "oak"})
			case "public":
				o.sub, o.err = ev.chat.Subscribe(ctx, call, wireArgs{Channel: "eng"})
			default:
				o.sub, o.err = ev.deploy.Subscribe(ctx, call, deployArgs{Env: "prod"})
			}
			mu.Lock()
			last = o
			mu.Unlock()
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
		}, Subscribes(ev.crawl, ev.turn, ev.chat, ev.deploy))
	})
	for _, r := range g.Next.States.Rows {
		user := ""
		if r.User != nil {
			user = *r.User
		}
		name := r.Access + "/" + r.Caller + "/" + user
		ws.mu.Lock()
		ws.asked, ws.auth = nil, nil
		ws.mu.Unlock()
		native := "0"
		if r.Caller == "native" {
			native = "1"
		}
		res, e := ws.rpc(t, user, "claude-code/2.1", "tools/call", map[string]any{"name": "watch", "arguments": map[string]any{"access": r.Access}}, "X-Native", native)
		if e != nil {
			t.Fatalf("%s: %v", name, e)
		}
		mu.Lock()
		o := last
		mu.Unlock()
		switch {
		case r.Want.Error != "":
			if !IsCode(o.err, r.Want.Error) || !strings.Contains(o.err.Error(), r.Want.Says) {
				t.Errorf("%s: %v, want %s saying %q", name, o.err, r.Want.Error, r.Want.Says)
			}
		case o.err != nil || o.sub.State != r.Want.State || o.sub.Reason != r.Want.Reason:
			t.Errorf("%s: %+v %v, want %s %q", name, o.sub, o.err, r.Want.State, r.Want.Reason)
		}
		asked := false
		var addition map[string]any
		for _, p := range ws.askedPaths() {
			if p == "/event-subscriptions" {
				asked = true
			}
		}
		if asked != r.CoreAsked {
			t.Errorf("%s: asked Wakeshift %v, want %v", name, asked, r.CoreAsked)
		}
		_ = res
		_ = addition
		if r.AuthorizeCalledWith != nil {
			ws.mu.Lock()
			calls := ws.auth
			ws.mu.Unlock()
			if len(calls) != 1 || calls[0].User != r.AuthorizeCalledWith.User || calls[0].Native != r.AuthorizeCalledWith.Native {
				t.Errorf("%s: authorize called with %v, want %+v", name, calls, r.AuthorizeCalledWith)
			}
		}
	}
	// native_unavailable logs once per tool and reason, at info.
	n := strings.Count(ws.logs.String(), `"code":"native_unavailable"`)
	if n != 3 {
		t.Errorf("native_unavailable logged %d times, want 3 (client_unsupported, no_user, refused):\n%s", n, ws.logs)
	}
}
