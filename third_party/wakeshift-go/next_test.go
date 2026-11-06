package wakeshift_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/wakeshift-go"
)

// disabled-client.json methods and option_checks: each method on a
// disabled client (no key), and the checks that hold on every client.
func TestDisabledClientGolden(t *testing.T) {
	var f struct {
		OptionChecks struct {
			Cases []struct {
				Op    string            `json:"op"`
				Error wakeshift.Code    `json:"error"`
				Says  string            `json:"says"`
				Go    string            `json:"go"`
				Skip  map[string]string `json:"skip"`
			} `json:"cases"`
		} `json:"option_checks"`
		Methods struct {
			Cases []struct {
				Op   string            `json:"op"`
				Want map[string]any    `json:"want"`
				Skip map[string]string `json:"skip"`
			} `json:"cases"`
		} `json:"methods"`
	}
	readFixture(t, "disabled-client.json", &f)

	// Every check, on an enabled and a disabled client alike.
	for _, enabled := range []bool{true, false} {
		for _, c := range f.OptionChecks.Cases {
			if c.Skip["go"] != "" {
				continue
			}
			wake := disabledClient(t)
			if enabled {
				wake = testClient(t, "https://core.example")
			}
			name := fmt.Sprintf("%s (enabled %v)", c.Op, enabled)
			err, panicked := optionCheck(t, wake, c.Op)
			switch {
			case c.Go == "panics":
				if !strings.Contains(panicked, c.Says) {
					t.Errorf("%s: panic %q, want one saying %q", name, panicked, c.Says)
				}
			case !wakeshift.IsCode(err, c.Error) || !strings.Contains(err.Error(), c.Says):
				t.Errorf("%s: %v, want %s saying %q", name, err, c.Error, c.Says)
			}
		}
	}

	// Each method on a disabled client: no request is ever made.
	var asked atomic.Int32
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { asked.Add(1) }))
	defer core.Close()
	d := newDisabled(t, core.URL)
	for _, c := range f.Methods.Cases {
		if c.Skip["go"] != "" {
			continue
		}
		want := c.Want
		if g, ok := want["go"].(map[string]any); ok {
			want = g
		}
		t.Run(c.Op, func(t *testing.T) {
			check := disabledOps[c.Op]
			if check == nil {
				t.Fatalf("no op %s here", c.Op)
			}
			check(t, d, want)
		})
	}
	if asked.Load() != 0 {
		t.Fatalf("a disabled client asked Wakeshift %d times", asked.Load())
	}
}

// optionCheck runs one of option_checks' ops on wake: its error, or what
// it panicked with.
func optionCheck(t *testing.T, wake *wakeshift.Client, op string) (err error, panicked string) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			panicked = fmt.Sprint(r)
		}
	}()
	allow := func(context.Context, wakeshift.Subscriber, tableArgs) (bool, error) { return true, nil }
	news, _ := wakeshift.DefineEvent(wake, "table.news", wakeshift.EventDef[tableArgs, turnMsg]{Description: "News"})
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "1"}, nil)
	switch op {
	case "define_event_public_and_personal":
		_, err = wakeshift.DefineEvent(wake, "x.y", wakeshift.EventDef[tableArgs, turnMsg]{Description: "X", Public: true, Personal: true})
	case "define_event_public_and_authorize":
		_, err = wakeshift.DefineEvent(wake, "x.y", wakeshift.EventDef[tableArgs, turnMsg]{Description: "X", Public: true, Authorize: allow})
	case "attach_twice":
		wake.Attach(srv, wakeshift.AttachOptions{})
		wake.Attach(srv, wakeshift.AttachOptions{})
	case "register_builtin_name":
		at := wake.Attach(srv, wakeshift.AttachOptions{})
		wakeshift.AddTool(at, &mcp.Tool{Name: "subscribe_events"}, handler[tableIn](nil), wakeshift.Subscribes(news))
	case "register_schema_names_wake_arg":
		at := wake.Attach(srv, wakeshift.AttachOptions{})
		wakeshift.AddTool(at, &mcp.Tool{Name: "t"}, handler[struct {
			SessionKey string `json:"session_key"`
		}](nil), wakeshift.Subscribes(news))
	case "register_output_names_wakeshift":
		at := wake.Attach(srv, wakeshift.AttachOptions{})
		wakeshift.AddTool(at, &mcp.Tool{Name: "t"}, func(context.Context, *wakeshift.ToolCall, tableIn) (*mcp.CallToolResult, struct {
			Wakeshift string `json:"wakeshift"`
		}, error) {
			return nil, struct {
				Wakeshift string `json:"wakeshift"`
			}{}, nil
		}, wakeshift.Subscribes(news))
	case "register_event_from_another_client":
		other := testClient(t, "https://other.example")
		foreign, _ := wakeshift.DefineEvent(other, "x.y", wakeshift.EventDef[tableArgs, turnMsg]{Description: "X"})
		at := wake.Attach(srv, wakeshift.AttachOptions{})
		wakeshift.AddTool(at, &mcp.Tool{Name: "t"}, handler[tableIn](nil), wakeshift.Subscribes(foreign))
	default:
		t.Fatalf("no option check %s here", op)
	}
	return err, ""
}

// disabledApp is a disabled client with a shared and a personal event, a
// server it's attached to, and a tool that subscribes, behind Handler.
type disabledApp struct {
	wake       *wakeshift.Client
	news, turn *wakeshift.Event[tableArgs, turnMsg]
	srv        *mcp.Server
	at         *wakeshift.Attachment
	next       http.Handler
	url        string
	identified atomic.Bool
	mu         sync.Mutex
	calls      []*wakeshift.ToolCall
	inTool     func(ctx context.Context, call *wakeshift.ToolCall)
}

func newDisabled(t *testing.T, core string) *disabledApp {
	t.Helper()
	d := &disabledApp{wake: disabledClient(t, wakeshift.WithCore(core))}
	var err error
	if d.news, err = wakeshift.DefineEvent(d.wake, "table.news", wakeshift.EventDef[tableArgs, turnMsg]{Description: "News", Public: true}); err != nil {
		t.Fatal(err)
	}
	if d.turn, err = wakeshift.DefineEvent(d.wake, "game.turn", wakeshift.EventDef[tableArgs, turnMsg]{Description: "Your move", Personal: true}); err != nil {
		t.Fatal(err)
	}
	d.srv = mcp.NewServer(&mcp.Implementation{Name: "t", Version: "1"}, nil)
	d.at = d.wake.Attach(d.srv, wakeshift.AttachOptions{Identify: func(context.Context, wakeshift.Request) (wakeshift.Identity, error) {
		d.identified.Store(true)
		return wakeshift.Identity{User: "ada"}, nil
	}})
	wakeshift.AddTool(d.at, &mcp.Tool{Name: "watch", Description: "Watch a table."}, func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) (*mcp.CallToolResult, any, error) {
		d.mu.Lock()
		d.calls = append(d.calls, call)
		f := d.inTool
		d.mu.Unlock()
		if f != nil {
			f(ctx, call)
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
	}, wakeshift.Subscribes(d.news))
	d.next = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return d.srv }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	hs := httptest.NewServer(d.at.Handler(d.next))
	t.Cleanup(hs.Close)
	d.url = hs.URL
	return d
}

// inCall runs f in the watch tool's handler.
func (d *disabledApp) inCall(t *testing.T, f func(ctx context.Context, call *wakeshift.ToolCall)) {
	t.Helper()
	d.mu.Lock()
	d.inTool = f
	d.mu.Unlock()
	// Nothing advertised session_key, so no agent passes it.
	res := post(t, d.url, "claude-code/2.1", "tools/call", map[string]any{"name": "watch", "arguments": map[string]any{"table": "oak"}})
	if res["result"] == nil {
		t.Fatalf("watch: %v", res)
	}
}

func wantDisabledErr(t *testing.T, err error) {
	t.Helper()
	if !wakeshift.IsCode(err, wakeshift.CodeDisabled) {
		t.Errorf("%v, want disabled", err)
	}
}

// disabledOps are disabled-client.json's methods, in Go.
var disabledOps = map[string]func(t *testing.T, d *disabledApp, want map[string]any){
	"enabled": func(t *testing.T, d *disabledApp, _ map[string]any) {
		if d.wake.Enabled() {
			t.Error("enabled")
		}
	},
	"define_event": func(t *testing.T, d *disabledApp, _ map[string]any) {
		if _, err := wakeshift.DefineEvent(d.wake, "x.y", wakeshift.EventDef[tableArgs, turnMsg]{Description: "X"}); err != nil {
			t.Error(err)
		}
	},
	"attach": func(t *testing.T, d *disabledApp, _ map[string]any) {
		tools := listTools(t, d.url, "")
		if len(tools) != 1 || tools["watch"] == nil || tools["watch"]["description"] != "Watch a table." {
			t.Errorf("tools/list: %v", tools)
		}
		if props := tools["watch"]["inputSchema"].(map[string]any)["properties"].(map[string]any); props["session_key"] != nil {
			t.Errorf("Wakeshift's arguments were added: %v", props)
		}
		init := post(t, d.url, "x/1", "initialize", map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "x", "version": "1"}})
		if strings.Contains(fmt.Sprint(init), "events") {
			t.Errorf("the events capability was added: %v", init)
		}
	},
	"handler": func(t *testing.T, d *disabledApp, _ map[string]any) {
		if d.at.Handler(d.next) != d.next || d.wake.Handler(d.next, wakeshift.AttachOptions{}) != d.next {
			t.Error("Handler didn't return next")
		}
	},
	"register_tool": func(t *testing.T, d *disabledApp, _ map[string]any) {
		var got *wakeshift.ToolCall
		d.inCall(t, func(_ context.Context, call *wakeshift.ToolCall) { got = call })
		if got == nil || got.Tool != "watch" {
			t.Errorf("the handler's call: %v", got)
		}
	},
	"identify": func(t *testing.T, d *disabledApp, _ map[string]any) {
		d.inCall(t, func(context.Context, *wakeshift.ToolCall) {})
		if d.identified.Load() {
			t.Error("identify ran on a disabled client")
		}
	},
	"current_call": func(t *testing.T, d *disabledApp, _ map[string]any) {
		ctx := context.Background()
		if sub, err := d.news.Subscribe(ctx, wakeshift.ToolCallFrom(ctx), tableArgs{Table: "oak"}); err != nil || sub.State != wakeshift.SubscriptionDisabled {
			t.Errorf("outside a call: %+v %v", sub, err)
		}
	},
	"current_call_in_tool": func(t *testing.T, d *disabledApp, _ map[string]any) {
		var got *wakeshift.ToolCall
		d.inCall(t, func(ctx context.Context, _ *wakeshift.ToolCall) { got = wakeshift.ToolCallFrom(ctx) })
		if got == nil {
			t.Error("ToolCallFrom in a tool: nil")
		}
	},
	"call_user": func(t *testing.T, d *disabledApp, _ map[string]any) {
		d.inCall(t, func(_ context.Context, call *wakeshift.ToolCall) {
			if call.User != "" {
				t.Errorf("call.User %q", call.User)
			}
		})
	},
	"call_subscribe": func(t *testing.T, d *disabledApp, _ map[string]any) {
		d.inCall(t, func(ctx context.Context, call *wakeshift.ToolCall) {
			if sub, err := d.news.Subscribe(ctx, call, tableArgs{Table: "oak"}); err != nil || sub.State != wakeshift.SubscriptionDisabled {
				t.Errorf("%+v %v", sub, err)
			}
		})
	},
	"call_subscribe_undeclared": func(t *testing.T, d *disabledApp, _ map[string]any) {
		d.inCall(t, func(ctx context.Context, call *wakeshift.ToolCall) {
			if _, err := d.turn.Subscribe(ctx, call, tableArgs{Table: "oak"}); !wakeshift.IsCode(err, wakeshift.CodeUndeclaredSubscribe) {
				t.Errorf("%v", err)
			}
			if _, err := d.news.Subscribe(ctx, call, tableArgs{Table: "oak"}, wakeshift.Replay(25*time.Hour)); !wakeshift.IsCode(err, wakeshift.CodeBadArguments) {
				t.Errorf("replay bounds on a disabled client: %v", err)
			}
		})
	},
	"call_unsubscribe": func(t *testing.T, d *disabledApp, _ map[string]any) {
		d.inCall(t, func(ctx context.Context, call *wakeshift.ToolCall) {
			if ended, err := d.news.Unsubscribe(ctx, call, tableArgs{Table: "oak"}); err != nil || ended {
				t.Errorf("%v %v", ended, err)
			}
		})
	},
	"call_resolve_session": func(t *testing.T, d *disabledApp, _ map[string]any) {
		d.inCall(t, func(ctx context.Context, call *wakeshift.ToolCall) {
			_, err := call.ResolveSession(ctx)
			wantDisabledErr(t, err)
		})
	},
	"emit": func(t *testing.T, d *disabledApp, _ map[string]any) {
		got, err := d.news.Emit(context.Background(), tableArgs{Table: "oak"}, turnMsg{})
		if err != nil || got.Matched != 0 || got.Duplicate || got.TextTrimmed || got.Unmatched == nil || len(got.Unmatched) != 0 || got.Ended == nil || len(got.Ended) != 0 {
			t.Errorf("%+v %v", got, err)
		}
	},
	"emit_with_id": func(t *testing.T, d *disabledApp, _ map[string]any) {
		if got, err := d.news.Emit(context.Background(), tableArgs{Table: "oak"}, turnMsg{}, wakeshift.WithID("m1")); err != nil || got.Matched != 0 {
			t.Errorf("%+v %v", got, err)
		}
	},
	"emit_personal_without_to": func(t *testing.T, d *disabledApp, _ map[string]any) {
		if _, err := d.turn.Emit(context.Background(), tableArgs{Table: "oak"}, turnMsg{}); !wakeshift.IsCode(err, wakeshift.CodeBadArguments) {
			t.Errorf("%v", err)
		}
	},
	"create_publish_url": func(t *testing.T, d *disabledApp, _ map[string]any) {
		_, err := d.news.CreatePublishURL(context.Background(), tableArgs{Table: "oak"})
		wantDisabledErr(t, err)
	},
	"revoke_publish_url": func(t *testing.T, d *disabledApp, _ map[string]any) {
		wantDisabledErr(t, d.wake.RevokePublishURL(context.Background(), "p1"))
	},
	"status": func(t *testing.T, d *disabledApp, _ map[string]any) {
		_, err := d.news.Status(context.Background(), tableArgs{Table: "oak"})
		wantDisabledErr(t, err)
	},
	"status_of": func(t *testing.T, d *disabledApp, _ map[string]any) {
		_, err := d.news.StatusOf(context.Background(), wakeshift.Recipient{User: "ada", Subscription: "s1"})
		wantDisabledErr(t, err)
	},
	"forget_user": func(t *testing.T, d *disabledApp, _ map[string]any) {
		_, err := d.wake.ForgetUser(context.Background(), "ada")
		wantDisabledErr(t, err)
	},
	"publish_manifest": func(t *testing.T, d *disabledApp, _ map[string]any) {
		wantDisabledErr(t, d.wake.PublishManifest(context.Background(), wakeshift.Manifest{}))
	},
	"event_subscriptions": func(t *testing.T, d *disabledApp, _ map[string]any) {
		_, err := d.wake.EventSubscriptions(context.Background(), "ada")
		wantDisabledErr(t, err)
	},
	"app_id": func(t *testing.T, d *disabledApp, _ map[string]any) {
		_, err := d.wake.AppID(context.Background())
		wantDisabledErr(t, err)
	},
	"key_id": func(t *testing.T, d *disabledApp, _ map[string]any) {
		if d.wake.KeyID() != "" {
			t.Errorf("%q", d.wake.KeyID())
		}
	},
	"act_as": func(t *testing.T, d *disabledApp, _ map[string]any) {
		_, err := d.wake.ActAs(context.Background(), "ws", "a")
		wantDisabledErr(t, err)
	},
	"workspace": func(t *testing.T, d *disabledApp, _ map[string]any) {
		_, err := d.wake.Workspace(context.Background(), "ada", "Ada")
		wantDisabledErr(t, err)
	},
	"add_agent": func(t *testing.T, d *disabledApp, _ map[string]any) {
		_, err := d.wake.AddAgent(context.Background(), "ws", "a", "A")
		wantDisabledErr(t, err)
	},
	"check_agent": func(t *testing.T, d *disabledApp, _ map[string]any) {
		wantDisabledErr(t, d.wake.CheckAgent(context.Background(), "ws", "a"))
	},
}

type tenantKey struct{}

// On a stateful go-sdk session, whose method handlers get the context of
// the request that started the session, Identify still runs per POST with
// that POST's own context and token (Handler makes the POST the unit of
// identity): a second POST with a new token (the same user's: go-sdk binds
// a session to its user) and a new middleware value gets both (design
// §2.8, GO #1).
func TestStatefulSessionIdentityPerPOST(t *testing.T) {
	core := newPathCore()
	cs := httptest.NewServer(core)
	defer cs.Close()
	wake := testClient(t, cs.URL)
	news, _ := wakeshift.DefineEvent(wake, "table.news", wakeshift.EventDef[tableArgs, turnMsg]{Description: "News", Public: true})
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "1"}, nil)
	var mu sync.Mutex
	var identified []string
	at := wake.Attach(srv, wakeshift.AttachOptions{Identify: func(ctx context.Context, r wakeshift.Request) (wakeshift.Identity, error) {
		tenant, _ := ctx.Value(tenantKey{}).(string)
		user := "?"
		if r.Token != nil {
			user, _ = r.Token.Extra["token"].(string)
		}
		mu.Lock()
		identified = append(identified, user+"@"+tenant)
		mu.Unlock()
		return wakeshift.Identity{User: user + "@" + tenant}, nil
	}})
	var users []string
	wakeshift.AddTool(at, &mcp.Tool{Name: "watch"}, func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) (*mcp.CallToolResult, any, error) {
		mu.Lock()
		users = append(users, call.User)
		mu.Unlock()
		_, err := news.Subscribe(ctx, call, tableArgs{Table: in.Table})
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, err
	}, wakeshift.Subscribes(news))
	// A stateful handler, behind the vendor's own middleware (a tenant from
	// a header) and the bearer check.
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{JSONResponse: true})
	verify := func(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		return &auth.TokenInfo{UserID: "ada", Extra: map[string]any{"token": token}, Expiration: time.Now().Add(time.Hour)}, nil
	}
	tenant := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), tenantKey{}, r.Header.Get("X-Tenant"))))
		})
	}
	hs := httptest.NewServer(tenant(auth.RequireBearerToken(verify, nil)(at.Handler(h))))
	defer hs.Close()

	init := map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "claude-code", "version": "2.1"}}
	session := ""
	send := func(token, tnt, method string, params any) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
		if method == "notifications/initialized" {
			b, _ = json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method})
		}
		req, _ := http.NewRequest("POST", hs.URL, strings.NewReader(string(b)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Tenant", tnt)
		if session != "" {
			req.Header.Set("Mcp-Session-Id", session)
			req.Header.Set("Mcp-Protocol-Version", "2025-11-25")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if s := resp.Header.Get("Mcp-Session-Id"); s != "" {
			session = s
		}
		if resp.StatusCode >= 300 {
			t.Fatalf("%s: %d", method, resp.StatusCode)
		}
	}
	send("token1", "acme", "initialize", init)
	if session == "" {
		t.Fatal("not a stateful session")
	}
	send("token1", "acme", "notifications/initialized", nil)
	send("token1", "acme", "tools/list", map[string]any{})
	send("token1", "acme", "tools/call", map[string]any{"name": "watch", "arguments": map[string]any{"table": "oak"}})
	send("token2", "globex", "tools/call", map[string]any{"name": "watch", "arguments": map[string]any{"table": "elm"}})
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(users, ",") != "token1@acme,token2@globex" {
		t.Fatalf("the calls' users: %v (identified %v)", users, identified)
	}
	if strings.Join(identified, ",") != "token1@acme,token2@globex" {
		t.Fatalf("identify ran %v: once per POST that needs it, never for initialize or tools/list", identified)
	}
}

// Identify that fails, panics or runs past its limit refuses Wakeshift's
// part of the request, and only that: the tool's own code still runs, the
// built-in tools answer isError, and native methods answer forbidden.
func TestIdentifyFailureRefusesWakeshiftsPart(t *testing.T) {
	defer wakeshift.SetTimeLimits(50 * time.Millisecond)()
	for name, identify := range map[string]func(context.Context, wakeshift.Request) (wakeshift.Identity, error){
		"error": func(context.Context, wakeshift.Request) (wakeshift.Identity, error) {
			return wakeshift.Identity{}, errors.New("boom-canary")
		},
		"panic": func(context.Context, wakeshift.Request) (wakeshift.Identity, error) { panic("boom-canary") },
		"hangs": func(ctx context.Context, _ wakeshift.Request) (wakeshift.Identity, error) {
			<-ctx.Done()
			time.Sleep(20 * time.Millisecond)
			return wakeshift.Identity{User: "ada"}, nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			var logs logLines
			w := newWrapped(t, wakeshift.AttachOptions{Identify: identify}, wakeshift.WithLogger(logs.logger()))
			ran := false
			var subErr error
			wakeshift.AddTool(w.at, &mcp.Tool{Name: "watch"}, func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) (*mcp.CallToolResult, any, error) {
				ran = true
				_, subErr = w.shared.Subscribe(ctx, call, tableArgs{Table: in.Table})
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "mine"}}}, nil, nil
			}, wakeshift.Subscribes(w.shared))
			res := w.callTool(t, "ada", "", "watch", map[string]any{"table": "oak"}, nil)
			if !ran || res.IsError || !wakeshift.IsCode(subErr, wakeshift.CodeIdentifyFailed) || strings.Contains(subErr.Error(), "boom-canary") {
				t.Errorf("tool ran %v (%+v), subscribe %v", ran, res, subErr)
			}
			if b := w.callTool(t, "ada", "", "list_subscriptions", map[string]any{}, nil); !b.IsError {
				t.Errorf("list_subscriptions: %+v", b)
			}
			// A handler that passes the refusal on: the agent reads neutral
			// words, never the developer's fix.
			wakeshift.AddTool(w.at, &mcp.Tool{Name: "watch_strict"}, func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) (*mcp.CallToolResult, any, error) {
				_, err := w.shared.Subscribe(ctx, call, tableArgs{Table: in.Table})
				return nil, nil, err
			}, wakeshift.Subscribes(w.shared))
			strict := w.callTool(t, "ada", "", "watch_strict", map[string]any{"table": "oak"}, nil)
			if text := strings.Join(strict.texts(), "\n"); !strict.IsError || text != "This app couldn't verify who you are, so it can't subscribe you to anything now." {
				t.Errorf("a handler passing the refusal on: %+v %q", strict, text)
			}
			res2 := post(t, w.url, "some-draft-client/1", "events/poll", map[string]any{"name": "table.news", "arguments": map[string]any{"table": "oak"}}, "Authorization", "Bearer ada")
			if e, _ := res2["error"].(map[string]any); e == nil {
				t.Errorf("events/poll: %v", res2)
			}
			if strings.Contains(logs.String(), "boom-canary") || !strings.Contains(logs.String(), `"code":"identify_error"`) {
				t.Errorf("logs: %s", logs.String())
			}
			for _, p := range w.core.paths() {
				if p == "/event-subscriptions" || p == "/events/poll" || p == "/event-subscriptions/list" {
					t.Errorf("asked Wakeshift %s", p)
				}
			}
		})
	}
}

// RequireUser refuses Wakeshift's part of a request with no user, so a
// session_key is never a bearer for its conversation.
func TestRequireUser(t *testing.T) {
	w := newWrapped(t, wakeshift.AttachOptions{RequireUser: true, Identify: func(_ context.Context, r wakeshift.Request) (wakeshift.Identity, error) {
		if r.Token.UserID == "anon" {
			return wakeshift.Identity{}, nil
		}
		return wakeshift.Identity{User: r.Token.UserID}, nil
	}})
	var errs []error
	wakeshift.AddTool(w.at, &mcp.Tool{Name: "watch"}, handler[tableIn](func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) error {
		_, err := w.shared.Subscribe(ctx, call, tableArgs{Table: in.Table})
		errs = append(errs, err)
		return nil
	}), wakeshift.Subscribes(w.shared))
	w.callTool(t, "ada", "", "watch", map[string]any{"table": "oak"}, nil)
	w.callTool(t, "anon", "", "watch", map[string]any{"table": "oak", "session_key": "wsc_fakefakefakefakefakefakefa"}, nil)
	if len(errs) != 2 || errs[0] != nil || !wakeshift.IsCode(errs[1], wakeshift.CodeUserRequired) || !strings.Contains(errs[1].Error(), "requireUser") {
		t.Fatalf("errors: %v", errs)
	}
}

// Authorize that errors, panics or runs past its limit refuses, and logs
// authorize_failed with the error's type only, at most once per 5 minutes
// per code.
func TestAuthorizeFailureRefuses(t *testing.T) {
	defer wakeshift.SetTimeLimits(50 * time.Millisecond)()
	for name, authorize := range map[string]func(context.Context, wakeshift.Subscriber, tableArgs) (bool, error){
		"error": func(context.Context, wakeshift.Subscriber, tableArgs) (bool, error) {
			return true, errors.New("boom-canary")
		},
		"panic": func(context.Context, wakeshift.Subscriber, tableArgs) (bool, error) { panic("boom-canary") },
		"hangs": func(ctx context.Context, _ wakeshift.Subscriber, _ tableArgs) (bool, error) {
			<-ctx.Done()
			time.Sleep(20 * time.Millisecond)
			return true, nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			var logs logLines
			w := newWrapped(t, wakeshift.AttachOptions{}, wakeshift.WithLogger(logs.logger()))
			if _, err := wakeshift.DefineEvent(w.app, "deploy.finished", wakeshift.EventDef[tableArgs, turnMsg]{Description: "A deploy finished", Authorize: authorize}); err != nil {
				t.Fatal(err)
			}
			for range 3 {
				res := w.callTool(t, "ada", "", "subscribe_events", map[string]any{"event": "deploy.finished", "arguments": map[string]any{"table": "oak"}}, nil)
				if !res.IsError || !strings.Contains(strings.Join(res.texts(), ""), "may not subscribe") {
					t.Errorf("subscribe_events: %+v", res)
				}
			}
			if n := strings.Count(logs.String(), `"code":"authorize_failed"`); n != 1 || strings.Contains(logs.String(), "boom-canary") {
				t.Errorf("logged %d times: %s", n, logs.String())
			}
			for _, p := range w.core.paths() {
				if p == "/event-subscriptions" {
					t.Error("asked Wakeshift")
				}
			}
		})
	}
}

// A go-sdk receiving middleware the vendor adds after Attach (it runs
// before Attach's) never sees Wakeshift's arguments: Handler takes them out
// of the request before go-sdk parses it.
func TestMiddlewareAfterAttachNeverSeesSessionKey(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	w := newWrapped(t, wakeshift.AttachOptions{})
	w.srv.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if call, ok := req.(*mcp.CallToolRequest); ok {
				mu.Lock()
				seen = append(seen, string(call.Params.Arguments))
				mu.Unlock()
			}
			return next(ctx, method, req)
		}
	})
	var session wakeshift.SessionKey
	wakeshift.AddTool(w.at, &mcp.Tool{Name: "watch"}, handler[tableIn](func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) error {
		session = call.Session()
		_, err := w.shared.Subscribe(ctx, call, tableArgs{Table: in.Table})
		return err
	}), wakeshift.Subscribes(w.shared))
	mcp.AddTool(w.srv, &mcp.Tool{Name: "get_page"}, func(context.Context, *mcp.CallToolRequest, tableIn) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	})
	const key = "wsc_canarycanarycanarycanary"
	for _, tool := range []string{"watch", "get_page"} {
		w.callTool(t, "ada", "", tool, map[string]any{"table": "oak", "session_key": key, "harness": "claude-code", "conversation_id": "c1"}, nil)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("the middleware saw %d calls", len(seen))
	}
	for _, s := range seen {
		if strings.Contains(s, "session_key") || strings.Contains(s, key) || strings.Contains(s, "harness") || strings.Contains(s, "conversation_id") {
			t.Errorf("the middleware saw %s", s)
		}
	}
	// The passed session_key goes to Wakeshift, never to the tool: its
	// call's Session is empty until Wakeshift resolves the conversation.
	if session != "" {
		t.Errorf("call.Session() before subscribing: %q", string(session))
	}
	if c := w.core.bodies("/event-subscriptions")[0]["call"].(map[string]any); c["session"] != key || c["harness"] != "claude-code" || c["conversation"] != "c1" {
		t.Errorf("the call Wakeshift got: %v", c)
	}
}

// A server built per request (Buildkite's shape): Attach each server as
// it's built, and one wake.Handler around the HTTP handler that builds
// them. tools/list is amended, calls are identified per POST, Wakeshift's
// arguments are stripped, and events/* is answered.
func TestPerRequestServers(t *testing.T) {
	core := newPathCore()
	cs := httptest.NewServer(core)
	defer cs.Close()
	var logs logLines
	wake := testClient(t, cs.URL, wakeshift.WithLogger(logs.logger()))
	news, _ := wakeshift.DefineEvent(wake, "table.news", wakeshift.EventDef[tableArgs, turnMsg]{Description: "News at a table", Public: true})
	opts := wakeshift.AttachOptions{Identify: func(_ context.Context, r wakeshift.Request) (wakeshift.Identity, error) {
		return wakeshift.Identity{User: r.Token.UserID}, nil
	}}
	var built atomic.Int32
	var mu sync.Mutex
	var users []string
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		built.Add(1)
		srv := mcp.NewServer(&mcp.Implementation{Name: "ci", Version: "1"}, nil)
		at := wake.Attach(srv, opts)
		wakeshift.AddTool(at, &mcp.Tool{Name: "watch_build", Description: "Watch a build."}, func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) (*mcp.CallToolResult, any, error) {
			mu.Lock()
			users = append(users, call.User)
			mu.Unlock()
			_, err := news.Subscribe(ctx, call, tableArgs{Table: in.Table})
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Watching."}}}, nil, err
		}, wakeshift.Subscribes(news))
		return srv
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	verify := func(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		return &auth.TokenInfo{UserID: token, Expiration: time.Now().Add(time.Hour)}, nil
	}
	hs := httptest.NewServer(auth.RequireBearerToken(verify, nil)(wake.Handler(h, opts)))
	defer hs.Close()
	tools := listTools(t, hs.URL, "Bearer ada")
	if d, _ := tools["watch_build"]["description"].(string); !strings.Contains(d, "of this event: News at a table") {
		t.Errorf("watch_build: %q", d)
	}
	for _, user := range []string{"ada", "bo"} {
		res := post(t, hs.URL, "claude-code/2.1", "tools/call", map[string]any{"name": "watch_build", "arguments": map[string]any{"table": "oak", "session_key": "wsc_fakefakefakefakefakefakefa"}},
			"Authorization", "Bearer "+user)
		if res["result"] == nil {
			t.Fatalf("watch_build: %v", res)
		}
	}
	if evs := post(t, hs.URL, "x/1", "events/list", map[string]any{}, "Authorization", "Bearer ada"); !strings.Contains(fmt.Sprint(evs), "table.news") {
		t.Errorf("events/list: %v", evs)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(users, ",") != "ada,bo" || built.Load() < 3 {
		t.Errorf("users %v, %d servers built", users, built.Load())
	}
	if n := strings.Count(logs.String(), `"code":"handler_missing"`); n != 0 {
		t.Errorf("handler_missing logged under wake.Handler")
	}
	if n := strings.Count(logs.String(), `"code":"no_identify"`); n != 0 {
		t.Errorf("no_identify logged with identify")
	}
}

// Wrap works for servers built per request too: each server's Attach
// takes the same opts (with Wrap) as the one wake.Handler, so a tool
// registered with mcp.AddTool and named in Wrap is listed with the
// subscribes sentence, and its handler finds its call (ToolCallFrom) and
// subscribes.
func TestPerRequestServersWrap(t *testing.T) {
	core := &wakeCore{}
	cs := httptest.NewServer(http.HandlerFunc(core.handler))
	defer cs.Close()
	var logs logLines
	wake := testClient(t, cs.URL, wakeshift.WithLogger(logs.logger()))
	news, _ := wakeshift.DefineEvent(wake, "table.news", wakeshift.EventDef[tableArgs, turnMsg]{Description: "News at a table"})
	opts := wakeshift.AttachOptions{
		Identify: func(_ context.Context, r wakeshift.Request) (wakeshift.Identity, error) {
			return wakeshift.Identity{User: r.Token.UserID}, nil
		},
		Wrap: map[string][]wakeshift.ToolOption{"watch_build": {wakeshift.Subscribes(news)}},
	}
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		srv := mcp.NewServer(&mcp.Implementation{Name: "ci", Version: "1"}, nil)
		mcp.AddTool(srv, &mcp.Tool{Name: "watch_build", Description: "Watch a build."}, func(ctx context.Context, _ *mcp.CallToolRequest, in tableIn) (*mcp.CallToolResult, any, error) {
			call := wakeshift.ToolCallFrom(ctx)
			if call == nil {
				return nil, nil, errors.New("no call")
			}
			_, err := news.Subscribe(ctx, call, tableArgs{Table: in.Table})
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Watching."}}}, nil, err
		})
		wake.Attach(srv, opts)
		return srv
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	verify := func(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		return &auth.TokenInfo{UserID: token, Expiration: time.Now().Add(time.Hour)}, nil
	}
	hs := httptest.NewServer(auth.RequireBearerToken(verify, nil)(wake.Handler(h, opts)))
	defer hs.Close()
	tools := listTools(t, hs.URL, "Bearer ada")
	if d, _ := tools["watch_build"]["description"].(string); !strings.Contains(d, "of this event: News at a table") {
		t.Errorf("watch_build: %q", d)
	}
	res := post(t, hs.URL, "claude-code/2.1", "tools/call", map[string]any{"name": "watch_build", "arguments": map[string]any{"table": "oak", "session_key": "wsc_fakefakefakefakefakefakefa"}},
		"Authorization", "Bearer ada")
	if r, _ := res["result"].(map[string]any); r == nil || r["isError"] == true {
		t.Fatalf("watch_build: %v", res)
	}
	bodies := core.bodies("/event-subscriptions")
	if len(bodies) != 1 {
		t.Fatalf("subscriptions: %v", bodies)
	}
	if c, _ := bodies[0]["call"].(map[string]any); c["session"] != "wsc_fakefakefakefakefakefakefa" || c["user"] != "ada" {
		t.Errorf("the call Wakeshift got: %v", c)
	}
	if got := logs.codes(); len(got) != 0 {
		t.Errorf("logged %v", got)
	}
}

// WrapFunc declares tools by name at run time: asked on every tools/list
// and tools/call about a tool nothing else declares (never a built-in),
// its answer not kept. A tool it names is listed with the subscribes
// sentence and subscribes; one it answers nil for is left alone.
func TestWrapFunc(t *testing.T) {
	core := &wakeCore{}
	cs := httptest.NewServer(http.HandlerFunc(core.handler))
	defer cs.Close()
	wake := testClient(t, cs.URL, wakeshift.WithLogger(slog.New(slog.DiscardHandler)))
	news, _ := wakeshift.DefineEvent(wake, "table.news", wakeshift.EventDef[tableArgs, turnMsg]{Description: "News at a table"})
	var mu sync.Mutex
	asked := map[string]int{}
	srv := mcp.NewServer(&mcp.Implementation{Name: "ci", Version: "1"}, nil)
	for _, name := range []string{"watch_build", "get_page"} {
		mcp.AddTool(srv, &mcp.Tool{Name: name, Description: "A tool."}, func(ctx context.Context, _ *mcp.CallToolRequest, in tableIn) (*mcp.CallToolResult, any, error) {
			if call := wakeshift.ToolCallFrom(ctx); call != nil {
				if _, err := news.Subscribe(ctx, call, tableArgs{Table: in.Table}); err != nil {
					return nil, nil, err
				}
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
		})
	}
	attached := wake.Attach(srv, wakeshift.AttachOptions{WrapFunc: func(tool string) []wakeshift.ToolOption {
		mu.Lock()
		asked[tool]++
		mu.Unlock()
		if strings.HasPrefix(tool, "watch_") {
			return []wakeshift.ToolOption{wakeshift.Subscribes(news)}
		}
		return nil
	}})
	hs := httptest.NewServer(attached.Handler(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})))
	defer hs.Close()
	for range 2 {
		tools := listTools(t, hs.URL, "")
		if d, _ := tools["watch_build"]["description"].(string); !strings.Contains(d, "of this event: News at a table") {
			t.Errorf("watch_build: %q", d)
		}
		if d, _ := tools["get_page"]["description"].(string); d != "A tool." {
			t.Errorf("get_page: %q", d)
		}
	}
	for _, tool := range []string{"watch_build", "get_page"} {
		res := post(t, hs.URL, "claude-code/2.1", "tools/call", map[string]any{"name": tool, "arguments": map[string]any{"table": "oak"}})
		if r, _ := res["result"].(map[string]any); r == nil || r["isError"] == true {
			t.Fatalf("%s: %v", tool, res)
		}
	}
	if n := len(core.bodies("/event-subscriptions")); n != 1 {
		t.Errorf("%d subscriptions, want watch_build's", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if asked["watch_build"] < 3 || asked["get_page"] < 3 {
		t.Errorf("asked %v: want each tool asked on each tools/list and its call", asked)
	}
	for _, b := range []string{"subscribe_events", "list_subscriptions", "unsubscribe_events", "wakeshift_wait"} {
		if asked[b] != 0 {
			t.Errorf("asked about the built-in %s", b)
		}
	}
}

// A mistake in WrapFunc's options (a nil event, or another client's) is
// found while serving, so it never panics: it's logged once per tool
// (wrap_bad_event), and the tool is served unwrapped.
func TestWrapFuncBadEvent(t *testing.T) {
	var logs logLines
	wake := testClient(t, "https://core.example", wakeshift.WithLogger(logs.logger()))
	if _, err := wakeshift.DefineEvent(wake, "table.news", wakeshift.EventDef[tableArgs, turnMsg]{Description: "News at a table"}); err != nil {
		t.Fatal(err)
	}
	other := testClient(t, "https://core.example", wakeshift.WithLogger(slog.New(slog.DiscardHandler)))
	theirs, _ := wakeshift.DefineEvent(other, "their.news", wakeshift.EventDef[tableArgs, turnMsg]{Description: "Theirs"})
	var none *wakeshift.Event[tableArgs, turnMsg]
	srv := mcp.NewServer(&mcp.Implementation{Name: "ci", Version: "1"}, nil)
	for _, name := range []string{"watch_nil", "watch_theirs"} {
		mcp.AddTool(srv, &mcp.Tool{Name: name, Description: "A tool."}, func(ctx context.Context, _ *mcp.CallToolRequest, in tableIn) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
		})
	}
	attached := wake.Attach(srv, wakeshift.AttachOptions{
		Identify: func(context.Context, wakeshift.Request) (wakeshift.Identity, error) { return wakeshift.Identity{}, nil },
		WrapFunc: func(tool string) []wakeshift.ToolOption {
			if tool == "watch_nil" {
				return []wakeshift.ToolOption{wakeshift.Subscribes(none)}
			}
			return []wakeshift.ToolOption{wakeshift.Subscribes(theirs)}
		}})
	hs := httptest.NewServer(attached.Handler(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})))
	defer hs.Close()
	for range 2 {
		tools := listTools(t, hs.URL, "")
		for _, name := range []string{"watch_nil", "watch_theirs"} {
			props, _ := tools[name]["inputSchema"].(map[string]any)["properties"].(map[string]any)
			if d, _ := tools[name]["description"].(string); d != "A tool." || props["session_key"] != nil {
				t.Errorf("%s isn't unwrapped: %v", name, tools[name])
			}
		}
	}
	for _, name := range []string{"watch_nil", "watch_theirs"} {
		res := post(t, hs.URL, "claude-code/2.1", "tools/call", map[string]any{"name": name, "arguments": map[string]any{"table": "oak"}})
		if r, _ := res["result"].(map[string]any); r == nil || r["isError"] == true {
			t.Fatalf("%s: %v", name, res)
		}
	}
	if got := logs.codes(); !slices.Equal(got, []string{"wrap_bad_event", "wrap_bad_event"}) {
		t.Errorf("logged %v, want wrap_bad_event once per tool", got)
	}
	if !strings.Contains(logs.String(), `"tool":"watch_nil"`) || !strings.Contains(logs.String(), `"tool":"watch_theirs"`) {
		t.Errorf("logs: %s", logs.String())
	}
}

// A tool that subscribes whose description, with Wakeshift's sentence,
// is over 2048 characters logs description_too_long once per tool. It
// counts characters, not bytes; a tool that subscribes to nothing, or a
// shorter one, logs nothing.
func TestDescriptionTooLong(t *testing.T) {
	core := &wakeCore{}
	cs := httptest.NewServer(http.HandlerFunc(core.handler))
	defer cs.Close()
	var logs logLines
	wake := testClient(t, cs.URL, wakeshift.WithLogger(logs.logger()))
	news, _ := wakeshift.DefineEvent(wake, "table.news", wakeshift.EventDef[tableArgs, turnMsg]{Description: "News at a table"})
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "1"}, nil)
	attached := wake.Attach(srv, wakeshift.AttachOptions{
		Identify: func(context.Context, wakeshift.Request) (wakeshift.Identity, error) { return wakeshift.Identity{}, nil }})
	h := func(context.Context, *wakeshift.ToolCall, tableIn) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	}
	wakeshift.AddTool(attached, &mcp.Tool{Name: "long", Description: strings.Repeat("é", 2000)}, h, wakeshift.Subscribes(news))
	wakeshift.AddTool(attached, &mcp.Tool{Name: "short_in_characters", Description: strings.Repeat("é", 1500)}, h, wakeshift.Subscribes(news))
	wakeshift.AddTool(attached, &mcp.Tool{Name: "subscribes_to_nothing", Description: strings.Repeat("a", 3000)}, h, wakeshift.Subscribes())
	hs := httptest.NewServer(attached.Handler(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})))
	defer hs.Close()
	for range 2 {
		listTools(t, hs.URL, "")
	}
	if got := logs.codes(); !slices.Equal(got, []string{"description_too_long"}) {
		t.Fatalf("logged %v", got)
	}
	if !strings.Contains(logs.String(), `"tool":"long"`) || !strings.Contains(logs.String(), `"level":"WARN"`) {
		t.Errorf("logs: %s", logs.String())
	}
}

// An emit without an id gets the SDK's own while retries are on, so a
// retry of one whose answer was lost can't wake twice; it isn't reported
// as a duplicate. With retries off, it gets none.
func TestRetriedEmitWakesOnce(t *testing.T) {
	core := newPathCore()
	var ids []string
	var mu sync.Mutex
	core.answer("/events/emit", func(w http.ResponseWriter, body map[string]any) {
		mu.Lock()
		defer mu.Unlock()
		id, _ := body["id"].(string)
		ids = append(ids, id)
		if len(ids) == 2 {
			fmt.Fprint(w, `{"matched": 1, "duplicate": true}`)
			return
		}
		fmt.Fprint(w, `{"matched": 1}`)
	})
	cs := httptest.NewServer(core)
	defer cs.Close()
	wake, err := wakeshift.New(wakeshift.WithKey(testKey), wakeshift.WithCore(cs.URL))
	if err != nil {
		t.Fatal(err)
	}
	chat, _ := wakeshift.DefineEvent(wake, "chat.message", wakeshift.EventDef[tableArgs, turnMsg]{Description: "A message", Public: true})
	chat.Emit(context.Background(), tableArgs{Table: "oak"}, turnMsg{})
	got, err := chat.Emit(context.Background(), tableArgs{Table: "oak"}, turnMsg{})
	if err != nil || got.Duplicate {
		t.Errorf("an internal id's duplicate: %+v %v", got, err)
	}
	mu.Lock()
	if len(ids) != 2 || !strings.HasPrefix(ids[0], "wsi_") || ids[0] == ids[1] {
		t.Errorf("ids: %q", ids)
	}
	ids = nil
	mu.Unlock()
	chat.Emit(context.Background(), tableArgs{Table: "oak"}, turnMsg{}, wakeshift.WithID("m1"))
	off := testClient(t, cs.URL)
	plain, _ := wakeshift.DefineEvent(off, "chat.message", wakeshift.EventDef[tableArgs, turnMsg]{Description: "A message", Public: true})
	plain.Emit(context.Background(), tableArgs{Table: "oak"}, turnMsg{})
	mu.Lock()
	defer mu.Unlock()
	if len(ids) != 2 || ids[0] != "m1" || ids[1] != "" {
		t.Errorf("ids: %q", ids)
	}
}

// Without Text, the agent reads the event's description and its
// arguments.
func TestDefaultText(t *testing.T) {
	core := newPathCore()
	var texts []string
	core.answer("/events/emit", func(w http.ResponseWriter, body map[string]any) {
		texts = append(texts, body["text"].(string))
		fmt.Fprint(w, `{"matched": 0}`)
	})
	cs := httptest.NewServer(core)
	defer cs.Close()
	wake := testClient(t, cs.URL)
	type jobArgs struct {
		Job  string `json:"job"`
		Page int    `json:"page"`
	}
	ev, _ := wakeshift.DefineEvent(wake, "crawl.finished", wakeshift.EventDef[jobArgs, turnMsg]{Description: "A crawl finished.",
		Topic: "ev/crawl.finished/{job}"})
	ev.Emit(context.Background(), jobArgs{Job: "3f2a", Page: 2}, turnMsg{})
	if len(texts) != 1 || texts[0] != "A crawl finished (job 3f2a, page 2)" {
		t.Fatalf("text: %q", texts)
	}
}
