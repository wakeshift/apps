package wakeshift_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/wakeshift-go"
)

// warnings.json: which situations log, at which level and with which code
// and fields, in every SDK. Each op runs its situation with its own
// logger; the lines at info or above must be exactly the fixture's, in
// order, and every line's message contains its code's fix.
func TestWarningsGolden(t *testing.T) {
	var f struct {
		Codes []struct {
			Code      string   `json:"code"`
			Level     string   `json:"level"`
			Fix       string   `json:"fix"`
			Languages []string `json:"languages"`
		} `json:"codes"`
		Situations struct {
			Cases []struct {
				Op   string            `json:"op"`
				Name string            `json:"name"`
				Logs []map[string]any  `json:"logs"`
				Skip map[string]string `json:"skip"`
			} `json:"cases"`
		} `json:"situations"`
	}
	readFixture(t, "warnings.json", &f)
	fixes, levels := map[string]string{}, map[string]string{}
	levelNames := map[string]string{"debug": "DEBUG", "info": "INFO", "warn": "WARN", "error": "ERROR"}
	for _, c := range f.Codes {
		fixes[c.Code], levels[c.Code] = c.Fix, levelNames[c.Level]
	}
	defer wakeshift.SetTimeLimits(50 * time.Millisecond)()
	for _, c := range f.Situations.Cases {
		if c.Skip["go"] != "" {
			if warningOps[c.Op] != nil {
				t.Errorf("%s: skipped for Go, but Go has it", c.Op)
			}
			continue
		}
		t.Run(c.Op, func(t *testing.T) {
			op := warningOps[c.Op]
			if op == nil {
				t.Fatalf("no op %s here", c.Op)
			}
			var logs logLines
			op(t, &logs)
			var got []map[string]any
			for _, m := range logs.lines() {
				if m["level"] == "DEBUG" {
					continue
				}
				code, _ := m["code"].(string)
				if code == "" {
					t.Errorf("a line without a code: %v", m)
					continue
				}
				if m["level"] != levels[code] {
					t.Errorf("%s logged at %v, want %s", code, m["level"], levels[code])
				}
				if msg, _ := m["msg"].(string); !strings.Contains(msg, fixes[code]) || !strings.HasPrefix(msg, "wakeshift: ") {
					t.Errorf("%s: message %q lacks its fix %q", code, msg, fixes[code])
				}
				if strings.Contains(logs.String(), "boom-canary") {
					t.Errorf("a log carries the error's message")
				}
				got = append(got, m)
			}
			if len(got) != len(c.Logs) {
				t.Fatalf("logged %d lines, want %d:\n%s", len(got), len(c.Logs), logs.String())
			}
			for i, want := range c.Logs {
				for k, v := range want {
					switch k {
					case "error_type":
						// Go names the error's type (fmt %T), never its message.
						if s, _ := got[i][k].(string); s == "" {
							t.Errorf("line %d: no error_type", i)
						}
					default:
						if fmt.Sprint(got[i][k]) != fmt.Sprint(v) {
							t.Errorf("line %d: %s = %v, want %v", i, k, got[i][k], v)
						}
					}
				}
			}
		})
	}
}

// quickstart is design §"The end state" on one client and server, called
// by Claude Code: tools/list, a call of start_crawl, an emit.
func quickstart(t *testing.T, logs *logLines, key string, identify bool) {
	t.Helper()
	core := newPathCore()
	cs := httptest.NewServer(core)
	t.Cleanup(cs.Close)
	wake, err := wakeshift.New(wakeshift.WithKey(key), wakeshift.WithCore(cs.URL), wakeshift.WithLogger(logs.logger()), wakeshift.WithMaxRetries(0))
	if err != nil {
		t.Fatal(err)
	}
	type jobArgs struct {
		Job string `json:"job"`
	}
	type crawl struct {
		Status string `json:"status"`
		Pages  int    `json:"pages"`
	}
	crawlFinished, err := wakeshift.DefineEvent(wake, "crawl.finished", wakeshift.EventDef[jobArgs, crawl]{Description: "A crawl finished"})
	if err != nil {
		t.Fatal(err)
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "crawler", Version: "1.0.0"}, nil)
	opts := wakeshift.AttachOptions{}
	if identify {
		opts.Identify = func(context.Context, wakeshift.Request) (wakeshift.Identity, error) {
			return wakeshift.Identity{User: "ada"}, nil
		}
	}
	attached := wake.Attach(srv, opts)
	type startIn struct {
		URL string `json:"url"`
	}
	type startOut struct {
		Job string `json:"job"`
	}
	wakeshift.AddTool(attached, &mcp.Tool{Name: "start_crawl", Description: "Crawl a site."},
		func(ctx context.Context, call *wakeshift.ToolCall, in startIn) (*mcp.CallToolResult, startOut, error) {
			job := "j1"
			if _, err := crawlFinished.Subscribe(ctx, call, jobArgs{Job: job}, wakeshift.Once()); err != nil {
				return nil, startOut{}, err
			}
			return nil, startOut{Job: job}, nil
		}, wakeshift.Subscribes(crawlFinished))
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	hs := httptest.NewServer(attached.Handler(h))
	t.Cleanup(hs.Close)
	post(t, hs.URL, "claude-code/2.1", "tools/list", map[string]any{})
	post(t, hs.URL, "claude-code/2.1", "tools/call", map[string]any{"name": "start_crawl", "arguments": map[string]any{"url": "https://x.example"}})
	if _, err := crawlFinished.Emit(context.Background(), jobArgs{Job: "j1"}, crawl{Status: "completed", Pages: 3}); err != nil {
		t.Fatal(err)
	}
}

// post sends one JSON-RPC request to url, as ua, and returns the answer.
func post(t *testing.T, url, ua, method string, params any, header ...string) map[string]any {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, _ := http.NewRequest("POST", url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", ua)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	json.Unmarshal(raw, &out)
	return out
}

// pathCore is a fake core: answers by path, with overrides, recording
// what it's asked.
type pathCore struct {
	mu      sync.Mutex
	answers map[string]func(http.ResponseWriter, map[string]any)
	asked   []string
}

func newPathCore() *pathCore {
	return &pathCore{answers: map[string]func(http.ResponseWriter, map[string]any){}}
}

func (p *pathCore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	path := strings.TrimPrefix(r.URL.Path, "/api/apps/v1")
	p.mu.Lock()
	p.asked = append(p.asked, path)
	answer := p.answers[path]
	p.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if answer != nil {
		answer(w, body)
		return
	}
	switch path {
	case "/event-subscriptions":
		io.WriteString(w, `{"event_subscription": {"id": "s1", "created": true}, "session": "wsc_fakefakefakefakefakefakefa", "pending": "pa_1"}`)
	case "/calls/addition":
		io.WriteString(w, `{"text": "Woken by Monitor."}`)
	case "/events/emit":
		io.WriteString(w, `{"matched": 1}`)
	default:
		io.WriteString(w, `{}`)
	}
}

func (p *pathCore) answer(path string, f func(http.ResponseWriter, map[string]any)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.answers[path] = f
}

// warningOps are warnings.json's situations, in Go.
var warningOps = map[string]func(t *testing.T, logs *logLines){
	"quickstart_keyless":       func(t *testing.T, logs *logLines) { quickstart(t, logs, "", false) },
	"quickstart_with_key":      func(t *testing.T, logs *logLines) { quickstart(t, logs, testKey, false) },
	"quickstart_with_identify": func(t *testing.T, logs *logLines) { quickstart(t, logs, testKey, true) },
	"no_identify": func(t *testing.T, logs *logLines) {
		w := newWrapped(t, wakeshift.AttachOptions{}, func(o *wakeshift.AttachOptions, _ *wrapped) { o.Identify = nil }, wakeshift.WithLogger(logs.logger()))
		wakeshift.AddTool(w.at, &mcp.Tool{Name: "watch"}, handler[tableIn](func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) error {
			_, err := w.shared.Subscribe(ctx, call, tableArgs{Table: in.Table})
			return err
		}), wakeshift.Subscribes(w.shared))
		w.tools(t)
		w.callTool(t, "ada", "", "watch", map[string]any{"table": "oak"}, nil)
	},
	"no_key_twice": func(t *testing.T, logs *logLines) {
		disabledClient(t, wakeshift.WithLogger(logs.logger()))
		disabledClient(t, wakeshift.WithLogger(logs.logger()))
	},
	"attach_no_events": func(t *testing.T, logs *logLines) {
		wake := testClient(t, "https://core.example", wakeshift.WithLogger(logs.logger()))
		wake.Attach(mcp.NewServer(&mcp.Implementation{Name: "t", Version: "1"}, nil), wakeshift.AttachOptions{
			Identify: func(context.Context, wakeshift.Request) (wakeshift.Identity, error) { return wakeshift.Identity{}, nil }})
	},
	"attach_per_request": func(t *testing.T, logs *logLines) {
		wake := testClient(t, "https://core.example", wakeshift.WithLogger(logs.logger()))
		if _, err := wakeshift.DefineEvent(wake, "table.news", wakeshift.EventDef[tableArgs, turnMsg]{Description: "News"}); err != nil {
			t.Fatal(err)
		}
		for range 3 {
			wake.Attach(mcp.NewServer(&mcp.Implementation{Name: "t", Version: "1"}, nil), wakeshift.AttachOptions{})
		}
	},
	"text_trimmed": func(t *testing.T, logs *logLines) {
		core := newPathCore()
		core.answer("/events/emit", func(w http.ResponseWriter, _ map[string]any) {
			io.WriteString(w, `{"matched": 1, "text_trimmed": true}`)
		})
		cs := httptest.NewServer(core)
		t.Cleanup(cs.Close)
		wake := testClient(t, cs.URL, wakeshift.WithLogger(logs.logger()))
		chat, _ := wakeshift.DefineEvent(wake, "chat.message", wakeshift.EventDef[tableArgs, turnMsg]{Description: "A message", Public: true,
			Text: func(m turnMsg, _ tableArgs) string { return m.Text }})
		turn, _ := wakeshift.DefineEvent(wake, "game.turn", wakeshift.EventDef[tableArgs, turnMsg]{Description: "Your move", Personal: true,
			Text: func(m turnMsg, _ tableArgs) string { return m.Text }})
		long := turnMsg{Text: strings.Repeat("x", 1500)}
		ctx := context.Background()
		chat.Emit(ctx, tableArgs{Table: "oak"}, long)
		chat.Emit(ctx, tableArgs{Table: "oak"}, long)
		turn.Emit(ctx, tableArgs{Table: "oak"}, long, wakeshift.To("ada"))
	},
	"rollback_failed":              func(t *testing.T, logs *logLines) { rollbackFailed(t, logs, false) },
	"rollback_failed_after_window": func(t *testing.T, logs *logLines) { rollbackFailed(t, logs, true) },
	"addition_failed": func(t *testing.T, logs *logLines) {
		w := newWrapped(t, wakeshift.AttachOptions{}, wakeshift.WithLogger(logs.logger()))
		now := time.Now()
		wakeshift.SetClock(w.app, func() time.Time { return now })
		w.core.fail = map[string]bool{"/calls/addition": true}
		wakeshift.AddTool(w.at, &mcp.Tool{Name: "watch"}, func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) (*mcp.CallToolResult, any, error) {
			if _, err := w.shared.Subscribe(ctx, call, tableArgs{Table: in.Table}); err != nil {
				return nil, nil, err
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Watching."}}}, nil, nil
		}, wakeshift.Subscribes(w.shared))
		for i := range 3 {
			res := w.callTool(t, "ada", "", "watch", map[string]any{"table": fmt.Sprintf("t%d", i)}, nil)
			if res.IsError || strings.Join(res.texts(), "|") != "Watching." {
				t.Fatalf("watch: %+v", res)
			}
			now = now.Add(10 * time.Second)
		}
	},
	"authorize_throws": func(t *testing.T, logs *logLines) {
		w := newWrapped(t, wakeshift.AttachOptions{}, wakeshift.WithLogger(logs.logger()))
		deploy, err := wakeshift.DefineEvent(w.app, "deploy.finished", wakeshift.EventDef[tableArgs, turnMsg]{Description: "A deploy finished",
			Authorize: func(context.Context, wakeshift.Subscriber, tableArgs) (bool, error) {
				return false, errors.New("boom-canary")
			}})
		if err != nil {
			t.Fatal(err)
		}
		_ = deploy
		for range 2 {
			res := post(t, w.url, "some-draft-client/1", "events/poll", map[string]any{"name": "deploy.finished", "arguments": map[string]any{"table": "oak"}},
				"Authorization", "Bearer ada")
			if res["error"] == nil {
				t.Fatalf("a poll authorize refused was answered: %v", res)
			}
		}
	},
	"identify_throws": func(t *testing.T, logs *logLines) {
		ran := false
		w := newWrapped(t, wakeshift.AttachOptions{Identify: func(context.Context, wakeshift.Request) (wakeshift.Identity, error) {
			return wakeshift.Identity{}, errors.New("boom-canary")
		}}, wakeshift.WithLogger(logs.logger()))
		var subErr error
		wakeshift.AddTool(w.at, &mcp.Tool{Name: "watch"}, handler[tableIn](func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) error {
			ran = true
			_, subErr = w.shared.Subscribe(ctx, call, tableArgs{Table: in.Table})
			return nil
		}), wakeshift.Subscribes(w.shared))
		w.callTool(t, "ada", "", "watch", map[string]any{"table": "oak"}, nil)
		if !ran || !wakeshift.IsCode(subErr, wakeshift.CodeIdentifyFailed) {
			t.Errorf("ran %v, subscribe %v", ran, subErr)
		}
	},
	"require_user_no_user": func(t *testing.T, logs *logLines) {
		w := newWrapped(t, wakeshift.AttachOptions{RequireUser: true, Identify: func(context.Context, wakeshift.Request) (wakeshift.Identity, error) {
			return wakeshift.Identity{}, nil
		}}, wakeshift.WithLogger(logs.logger()))
		var errs []error
		wakeshift.AddTool(w.at, &mcp.Tool{Name: "start_crawl"}, handler[tableIn](func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) error {
			_, err := w.shared.Subscribe(ctx, call, tableArgs{Table: in.Table})
			errs = append(errs, err)
			return nil
		}), wakeshift.Subscribes(w.shared))
		for range 2 {
			w.callTool(t, "ada", "", "start_crawl", map[string]any{"table": "oak"}, nil)
		}
		if len(errs) != 2 || !wakeshift.IsCode(errs[0], wakeshift.CodeUserRequired) || !wakeshift.IsCode(errs[1], wakeshift.CodeUserRequired) {
			t.Errorf("subscribe: %v", errs)
		}
	},
	"tool_without_subscribes": func(t *testing.T, logs *logLines) {
		for _, key := range []string{"", testKey} {
			core := newPathCore()
			cs := httptest.NewServer(core)
			t.Cleanup(cs.Close)
			wake, err := wakeshift.New(wakeshift.WithKey(key), wakeshift.WithCore(cs.URL), wakeshift.WithLogger(logs.logger()), wakeshift.WithMaxRetries(0))
			if err != nil {
				t.Fatal(err)
			}
			ev, _ := wakeshift.DefineEvent(wake, "crawl.finished", wakeshift.EventDef[tableArgs, turnMsg]{Description: "A crawl finished"})
			srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "1"}, nil)
			at := wake.Attach(srv, wakeshift.AttachOptions{})
			wakeshift.AddTool(at, &mcp.Tool{Name: "unwatch"}, handler[tableIn](func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) error {
				_, err := ev.Unsubscribe(ctx, call, tableArgs{Table: in.Table})
				return err
			}))
			h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
			hs := httptest.NewServer(at.Handler(h))
			t.Cleanup(hs.Close)
			list := post(t, hs.URL, "claude-code/2.1", "tools/list", map[string]any{})
			args := map[string]any{"table": "oak"}
			if key != "" {
				args["session_key"] = "wsc_x" // listed with a key, as for Subscribes()
			}
			res := post(t, hs.URL, "claude-code/2.1", "tools/call", map[string]any{"name": "unwatch", "arguments": args})
			if res["error"] != nil || fmt.Sprint(res["result"]) == "" || strings.Contains(fmt.Sprint(res), `isError:true`) {
				t.Errorf("key %q: %v", key, res)
			}
			if strings.Contains(fmt.Sprint(list), "harness") {
				t.Errorf("key %q: a tool that subscribes to nothing lists more than session_key: %v", key, list)
			}
		}
	},
	"monitor_line_long": func(t *testing.T, logs *logLines) {
		core := newPathCore()
		cs := httptest.NewServer(core)
		t.Cleanup(cs.Close)
		wake := testClient(t, cs.URL, wakeshift.WithLogger(logs.logger()))
		news, _ := wakeshift.DefineEvent(wake, "table.news", wakeshift.EventDef[tableArgs, turnMsg]{Description: "News", Public: true,
			Text: func(m turnMsg, _ tableArgs) string { return m.Text }})
		ctx := context.Background()
		news.Emit(ctx, tableArgs{Table: "oak"}, turnMsg{Text: strings.Repeat("y", 200)})
		news.Emit(ctx, tableArgs{Table: "oak"}, turnMsg{Text: strings.Repeat("x", 300)})
		news.Emit(ctx, tableArgs{Table: "oak"}, turnMsg{Text: strings.Repeat("x", 300)})
	},
	"identify_not_called_for_list": func(t *testing.T, logs *logLines) {
		called := false
		w := newWrapped(t, wakeshift.AttachOptions{Identify: func(context.Context, wakeshift.Request) (wakeshift.Identity, error) {
			called = true
			return wakeshift.Identity{}, errors.New("boom-canary")
		}}, wakeshift.WithLogger(logs.logger()))
		post(t, w.url, "claude-code/2.1", "initialize", map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{},
			"clientInfo": map[string]any{"name": "claude-code", "version": "2.1"}}, "Authorization", "Bearer ada")
		post(t, w.url, "claude-code/2.1", "ping", map[string]any{}, "Authorization", "Bearer ada")
		w.tools(t)
		if called {
			t.Error("identify ran for initialize, ping or tools/list")
		}
	},
	"native_unavailable": func(t *testing.T, logs *logLines) {
		w := newWrapped(t, wakeshift.AttachOptions{Identify: func(context.Context, wakeshift.Request) (wakeshift.Identity, error) {
			return wakeshift.Identity{}, nil
		}}, wakeshift.WithLogger(logs.logger()))
		crawl, _ := wakeshift.DefineEvent(w.app, "crawl.finished", wakeshift.EventDef[tableArgs, turnMsg]{Description: "A crawl finished"})
		chat, _ := wakeshift.DefineEvent(w.app, "chat.message", wakeshift.EventDef[tableArgs, turnMsg]{Description: "A message", Public: true})
		wakeshift.AddTool(w.at, &mcp.Tool{Name: "start_crawl"}, handler[struct {
			Which string `json:"which"`
		}](func(ctx context.Context, call *wakeshift.ToolCall, in struct {
			Which string `json:"which"`
		}) error {
			if in.Which == "chat" {
				_, err := chat.Subscribe(ctx, call, tableArgs{Table: "eng"})
				return err
			}
			_, err := crawl.Subscribe(ctx, call, tableArgs{Table: "j1"})
			return err
		}), wakeshift.Subscribes(crawl, chat))
		for _, which := range []string{"crawl", "crawl", "chat"} {
			w.callTool(t, "anon", "openai-mcp/1.0.0", "start_crawl", map[string]any{"which": which}, nil)
		}
	},
	"event_closed": func(t *testing.T, logs *logLines) {
		manifestChanges(t, logs, `{"changed": [{"event": "crawl.finished", "from": "any", "to": "tools", "ended": 0}, {"event": "build.finished", "from": "any", "to": "tools", "ended": 3}]}`)
	},
	"event_opened": func(t *testing.T, logs *logLines) {
		manifestChanges(t, logs, `{"changed": [{"event": "build.finished", "from": "tools", "to": "any", "ended": 0}]}`)
	},
	"wrap_unknown_tool": func(t *testing.T, logs *logLines) {
		w := newWrapped(t, wakeshift.AttachOptions{}, func(o *wakeshift.AttachOptions, w *wrapped) {
			o.Wrap = map[string][]wakeshift.ToolOption{"stat_crawl": {wakeshift.Subscribes(w.shared)}}
		}, wakeshift.WithLogger(logs.logger()))
		w.srv.AddTool(&mcp.Tool{Name: "start_crawl", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{}, nil
		})
		w.tools(t)
		w.tools(t)
	},
	"wrap_arg_conflict": func(t *testing.T, logs *logLines) {
		w := newWrapped(t, wakeshift.AttachOptions{}, func(o *wakeshift.AttachOptions, w *wrapped) {
			o.Wrap = map[string][]wakeshift.ToolOption{"legacy_watch": {wakeshift.Subscribes(w.shared)}}
		}, wakeshift.WithLogger(logs.logger()))
		w.srv.AddTool(&mcp.Tool{Name: "legacy_watch", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"session_key": map[string]any{"type": "string"}}}},
			func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{}, nil
			})
		w.tools(t)
		w.tools(t)
	},
	"builtin_name_conflict": func(t *testing.T, logs *logLines) {
		w := newWrapped(t, wakeshift.AttachOptions{}, wakeshift.WithLogger(logs.logger()))
		w.srv.AddTool(&mcp.Tool{Name: "list_subscriptions", InputSchema: map[string]any{"type": "object"}},
			func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{}, nil
			})
		w.tools(t)
		w.tools(t)
	},
	"events_capability_kept": func(t *testing.T, logs *logLines) {
		wake := testClient(t, "https://core.example", wakeshift.WithLogger(logs.logger()))
		wakeshift.DefineEvent(wake, "table.news", wakeshift.EventDef[tableArgs, turnMsg]{Description: "News", Public: true})
		at := wake.Attach(mcp.NewServer(&mcp.Implementation{Name: "t", Version: "1"}, nil), wakeshift.AttachOptions{
			Identify: func(context.Context, wakeshift.Request) (wakeshift.Identity, error) { return wakeshift.Identity{}, nil }})
		// The server's own handler serves MCP Events itself.
		own := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			var req struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			result := map[string]any{"events": []any{map[string]any{"name": "own.event"}}}
			if req.Method == "initialize" {
				result = map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"events": map[string]any{"listChanged": false}}}
			}
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		})
		hs := httptest.NewServer(at.Handler(own))
		t.Cleanup(hs.Close)
		post(t, hs.URL, "x/1", "initialize", map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "x", "version": "1"}})
		res := post(t, hs.URL, "x/1", "events/list", map[string]any{})
		if !strings.Contains(fmt.Sprint(res), "own.event") {
			t.Errorf("events/list wasn't the server's own: %v", res)
		}
		post(t, hs.URL, "x/1", "initialize", map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "x", "version": "1"}})
	},
	"handler_missing": func(t *testing.T, logs *logLines) {
		wake := testClient(t, "https://core.example", wakeshift.WithLogger(logs.logger()))
		news, _ := wakeshift.DefineEvent(wake, "table.news", wakeshift.EventDef[tableArgs, turnMsg]{Description: "News", Public: true})
		srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "1"}, nil)
		at := wake.Attach(srv, wakeshift.AttachOptions{Identify: func(context.Context, wakeshift.Request) (wakeshift.Identity, error) { return wakeshift.Identity{}, nil }})
		wakeshift.AddTool(at, &mcp.Tool{Name: "look"}, handler[tableIn](nil), wakeshift.Subscribes(news))
		hs := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true}))
		t.Cleanup(hs.Close)
		for range 2 {
			post(t, hs.URL, "claude-code/2.1", "tools/call", map[string]any{"name": "look", "arguments": map[string]any{"table": "oak"}})
		}
	},
	"key_refused": func(t *testing.T, logs *logLines) {
		core := newPathCore()
		core.answer("/events/emit", func(w http.ResponseWriter, _ map[string]any) {
			w.WriteHeader(401)
			io.WriteString(w, `{"error": {"code": "unauthenticated", "message": "invalid assertion"}}`)
		})
		cs := httptest.NewServer(core)
		t.Cleanup(cs.Close)
		wake := testClient(t, cs.URL, wakeshift.WithLogger(logs.logger()))
		chat, _ := wakeshift.DefineEvent(wake, "chat.message", wakeshift.EventDef[tableArgs, turnMsg]{Description: "A message", Public: true})
		for range 3 {
			if _, err := chat.Emit(context.Background(), tableArgs{Table: "oak"}, turnMsg{}); !wakeshift.IsCode(err, wakeshift.CodeUnauthenticated) {
				t.Errorf("emit: %v", err)
			}
		}
	},
	"undeclared_subscribe_doesnt_log": func(t *testing.T, logs *logLines) {
		w := newWrapped(t, wakeshift.AttachOptions{}, wakeshift.WithLogger(logs.logger()))
		wakeshift.AddTool(w.at, &mcp.Tool{Name: "watch"}, handler[tableIn](func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) error {
			_, err := w.turn.Subscribe(ctx, call, tableArgs{Table: in.Table})
			if !wakeshift.IsCode(err, wakeshift.CodeUndeclaredSubscribe) {
				t.Errorf("subscribe: %v", err)
			}
			return nil
		}), wakeshift.Subscribes(w.shared))
		w.callTool(t, "ada", "", "watch", map[string]any{"table": "oak"}, nil)
	},
}

// rollbackFailed is a tool that subscribed and then failed, three times in
// a minute, with Wakeshift refusing the rollback; after, once more 5
// minutes later.
func rollbackFailed(t *testing.T, logs *logLines, after bool) {
	t.Helper()
	w := newWrapped(t, wakeshift.AttachOptions{}, wakeshift.WithLogger(logs.logger()))
	now := time.Now()
	wakeshift.SetClock(w.app, func() time.Time { return now })
	w.core.fail = map[string]bool{"/event-subscriptions/unsubscribe": true}
	wakeshift.AddTool(w.at, &mcp.Tool{Name: "watch"}, func(ctx context.Context, call *wakeshift.ToolCall, in tableIn) (*mcp.CallToolResult, any, error) {
		if _, err := w.shared.Subscribe(ctx, call, tableArgs{Table: in.Table}); err != nil {
			return nil, nil, err
		}
		return nil, nil, errors.New("boom-canary")
	}, wakeshift.Subscribes(w.shared))
	for i := range 3 {
		w.callTool(t, "ada", "", "watch", map[string]any{"table": fmt.Sprintf("t%d", i)}, nil)
		now = now.Add(10 * time.Second)
	}
	if after {
		now = now.Add(5 * time.Minute)
		w.callTool(t, "ada", "", "watch", map[string]any{"table": "t9"}, nil)
	}
}

// manifestChanges publishes a manifest Wakeshift answers with answer.
func manifestChanges(t *testing.T, logs *logLines, answer string) {
	t.Helper()
	core := newPathCore()
	core.answer("/manifest", func(w http.ResponseWriter, _ map[string]any) { io.WriteString(w, answer) })
	cs := httptest.NewServer(core)
	t.Cleanup(cs.Close)
	wake := testClient(t, cs.URL, wakeshift.WithLogger(logs.logger()))
	wakeshift.DefineEvent(wake, "crawl.finished", wakeshift.EventDef[tableArgs, turnMsg]{Description: "A crawl finished"})
	if err := wake.PublishManifest(context.Background(), wakeshift.Manifest{Description: "Crawls"}); err != nil {
		t.Fatal(err)
	}
}
