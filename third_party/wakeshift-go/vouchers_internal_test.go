package wakeshift

// Subscribe vouchers (docs/subscribe-vouchers.md §4.3, §4.4, §7): a tool
// subscribing a native client (ChatGPT) to a tools-only event gives it a
// native line, whose voucher the core renders; the client writes it back
// as wakeshift_voucher, which the SDK takes out of the arguments and sends
// to Wakeshift as voucher (mcp-events-wire.json vouchers).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// voucherCase is one case of mcp-events-wire.json vouchers.
type voucherCase struct {
	Name         string            `json:"name"`
	Method       string            `json:"method"`
	User         *string           `json:"user"`
	Feature      *bool             `json:"feature"`
	Params       map[string]any    `json:"params"`
	CoreRequest  map[string]any    `json:"core_request"`
	CallUser     *string           `json:"call_user"`
	CoreResponse map[string]any    `json:"core_response"`
	CoreStatus   int               `json:"core_status"`
	CoreBody     map[string]any    `json:"core_body"`
	Result       map[string]any    `json:"result"`
	OnSubscribe  map[string]any    `json:"on_subscribe"`
	Error        *voucherCaseError `json:"error"`
}

type voucherCaseError struct {
	Code    map[string]float64 `json:"code"`
	Message string             `json:"message"`
	Data    any                `json:"data"`
}

type voucherFixture struct {
	Vouchers struct {
		Voucher     string        `json:"voucher"`
		Subscribe   []voucherCase `json:"subscribe"`
		Unsubscribe []voucherCase `json:"unsubscribe"`
		Refused     []voucherCase `json:"refused"`
		CoreRefused []voucherCase `json:"core_refused"`
		EventsList  map[string]any
		States      struct {
			Rows []struct {
				Feature    bool           `json:"feature"`
				User       *string        `json:"user"`
				Want       map[string]any `json:"want"`
				NativeLine bool           `json:"native_line"`
				Logs       *string        `json:"logs"`
			} `json:"rows"`
		} `json:"tool_subscribe_states"`
	} `json:"vouchers"`
}

// voucherServer is the fixture's app (crawl.finished tools-only,
// chat.message public, deploy.finished allowing only u1) behind Handler,
// with the user from X-User, against a core that reports features (nil:
// none at all) and answers path with replies' status and body.
type voucherServer struct {
	url    string
	mu     sync.Mutex
	reqs   []voucherReq
	subs   []map[string]any // what OnSubscribe got
	auth   []map[string]any // what Authorize got
	logs   *bytes.Buffer
	result []Subscription
}

type voucherReq struct {
	path string
	body map[string]any
}

type voucherReply struct {
	status int
	body   any
}

func newVoucherServer(t *testing.T, features []string, replies map[string]voucherReply) *voucherServer {
	t.Helper()
	vs := &voucherServer{logs: &bytes.Buffer{}}
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/apps/v1")
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		vs.mu.Lock()
		if !strings.HasPrefix(path, "/manifest") && path != "/app" {
			vs.reqs = append(vs.reqs, voucherReq{path, body})
		}
		vs.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if rep, ok := replies[path]; ok {
			w.WriteHeader(rep.status)
			json.NewEncoder(w).Encode(rep.body)
			return
		}
		switch path {
		case "/manifest/merge", "/manifest":
			out := map[string]any{}
			if features != nil {
				out["features"] = features
			}
			json.NewEncoder(w).Encode(out)
		case "/app":
			io.WriteString(w, `{"id":"app-test","name":"Sample"}`)
		case "/calls/addition":
			io.WriteString(w, `{"text":"Woken."}`)
		default:
			io.WriteString(w, `{}`)
		}
	}))
	t.Cleanup(core.Close)
	var logMu sync.Mutex
	app := clientAt(t, core.URL, WithLogger(slog.New(slog.NewJSONHandler(writerFunc(func(p []byte) (int, error) {
		logMu.Lock()
		defer logMu.Unlock()
		return vs.logs.Write(p)
	}), &slog.HandlerOptions{Level: slog.LevelDebug}))))
	onSub := func(args any, s string, via, tool string) {
		vs.mu.Lock()
		defer vs.mu.Unlock()
		var user any
		if s != "" {
			user = s
		}
		var t any
		if tool != "" {
			t = tool
		}
		b, _ := json.Marshal(args)
		var a map[string]any
		json.Unmarshal(b, &a)
		vs.subs = append(vs.subs, map[string]any{"args": a, "user": user, "via": via, "tool": t})
	}
	crawl, err := DefineEvent(app, "crawl.finished", EventDef[crawlArgs, crawlMsg]{Description: "A crawl finished",
		OnSubscribe: func(_ context.Context, s Subscribed[crawlArgs]) error {
			onSub(s.Args, s.User, s.Via, s.Tool)
			return nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	chat, err := DefineEvent(app, "chat.message", EventDef[wireArgs, wireMsg]{Description: "A new message", Public: true,
		OnSubscribe: func(_ context.Context, s Subscribed[wireArgs]) error {
			onSub(map[string]any{"channel": s.Args.Channel}, s.User, s.Via, s.Tool)
			return nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DefineEvent(app, "deploy.finished", EventDef[deployArgs, deployMsg]{Description: "A deploy finished",
		Authorize: func(_ context.Context, s Subscriber, a deployArgs) (bool, error) {
			vs.mu.Lock()
			vs.auth = append(vs.auth, map[string]any{"user": s.User, "env": a.Env})
			vs.mu.Unlock()
			return s.User == "u1", nil
		}}); err != nil {
		t.Fatal(err)
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "crawler", Version: "1"}, nil)
	at := app.Attach(srv, AttachOptions{Identify: func(_ context.Context, r Request) (Identity, error) {
		return Identity{User: r.Header.Get("X-User")}, nil
	}})
	AddTool(at, &mcp.Tool{Name: "start_crawl"}, func(ctx context.Context, call *ToolCall, in crawlArgs) (*mcp.CallToolResult, any, error) {
		sub, err := crawl.Subscribe(ctx, call, in, Once(), TTL(time.Hour), Replay(10*time.Minute))
		if err != nil {
			return nil, nil, err
		}
		vs.mu.Lock()
		vs.result = append(vs.result, sub)
		vs.mu.Unlock()
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Crawl " + in.Job + " started."}}}, nil, nil
	}, Subscribes(crawl))
	AddTool(at, &mcp.Tool{Name: "watch_chat"}, func(ctx context.Context, call *ToolCall, in wireArgs) (*mcp.CallToolResult, any, error) {
		if _, err := chat.Subscribe(ctx, call, in, Once()); err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
	}, Subscribes(chat))
	hs := httptest.NewServer(at.Handler(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})))
	t.Cleanup(hs.Close)
	vs.url = hs.URL
	return vs
}

func (vs *voucherServer) rpc(t *testing.T, user, ua, method string, params any, header ...string) (map[string]any, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, _ := http.NewRequest("POST", vs.url, bytes.NewReader(b))
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

func (vs *voucherServer) last(path string) map[string]any {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	for i := len(vs.reqs) - 1; i >= 0; i-- {
		if vs.reqs[i].path == path {
			return vs.reqs[i].body
		}
	}
	return nil
}

func (vs *voucherServer) askedEvents() bool {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	for _, r := range vs.reqs {
		if strings.HasPrefix(r.path, "/events/") {
			return true
		}
	}
	return false
}

// noVoucherLogged fails if any log line carries a voucher.
func (vs *voucherServer) noVoucherLogged(t *testing.T, what string) {
	t.Helper()
	if strings.Contains(vs.logs.String(), "wsv_") {
		t.Errorf("%s: a voucher was logged:\n%s", what, vs.logs)
	}
}

func (vs *voucherServer) settled() []map[string]any {
	time.Sleep(50 * time.Millisecond)
	vs.mu.Lock()
	defer vs.mu.Unlock()
	return append([]map[string]any(nil), vs.subs...)
}

var voucherUAs = map[string]string{"draft": "some-draft-client/1", "chatgpt": "openai-mcp/1.0.0"}

func strp(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func readVouchers(t *testing.T) voucherFixture {
	t.Helper()
	var g voucherFixture
	readSharedFixture(t, "mcp-events-wire.json", &g)
	var raw struct {
		Vouchers struct {
			EventsList map[string]any `json:"events_list"`
		} `json:"vouchers"`
	}
	readSharedFixture(t, "mcp-events-wire.json", &raw)
	g.Vouchers.EventsList = raw.Vouchers.EventsList
	return g
}

var featureOn = []string{subscribeVouchers}

func TestVoucherSubscribeWireGolden(t *testing.T) {
	g := readVouchers(t)
	for _, c := range g.Vouchers.Subscribe {
		for dialect, ua := range voucherUAs {
			name := c.Name + " (" + dialect + ")"
			vs := newVoucherServer(t, featureOn, map[string]voucherReply{"/events/subscribe": {200, c.CoreResponse}})
			res, e := vs.rpc(t, strp(c.User), ua, "events/subscribe", c.Params, "Mcp-Session-Id", "sess-1")
			if e != nil || !reflect.DeepEqual(res, c.Result) {
				t.Errorf("%s: %v %v, want %v", name, res, e, c.Result)
				continue
			}
			body := vs.last("/events/subscribe")
			call, _ := body["call"].(map[string]any)
			delete(body, "call")
			delete(body, "dialect")
			if !reflect.DeepEqual(canonical(t, body), canonical(t, c.CoreRequest)) {
				t.Errorf("%s: asked %v, want %v", name, body, c.CoreRequest)
			}
			if got, _ := call["user"].(string); got != strp(c.CallUser) {
				t.Errorf("%s: call.user %q, want %q", name, got, strp(c.CallUser))
			}
			sum := sha256.Sum256([]byte("sess-1"))
			wantTS := ""
			if _, ok := c.CoreRequest["voucher"]; ok {
				wantTS = hex.EncodeToString(sum[:])
			}
			if got, _ := call["transport_session"].(string); got != wantTS {
				t.Errorf("%s: transport_session %q, want %q", name, got, wantTS)
			}
			var want []map[string]any
			if c.OnSubscribe != nil {
				want = []map[string]any{c.OnSubscribe}
			}
			if got := vs.settled(); !reflect.DeepEqual(canonical(t, got), canonical(t, want)) {
				t.Errorf("%s: OnSubscribe got %v, want %v", name, got, want)
			}
			vs.noVoucherLogged(t, name)
		}
	}
	for _, c := range g.Vouchers.Unsubscribe {
		vs := newVoucherServer(t, featureOn, map[string]voucherReply{"/events/unsubscribe": {200, c.CoreResponse}})
		res, e := vs.rpc(t, strp(c.User), "openai-mcp/1.0.0", "events/unsubscribe", c.Params)
		if e != nil || !reflect.DeepEqual(res, c.Result) {
			t.Errorf("%s: %v %v", c.Name, res, e)
			continue
		}
		body := vs.last("/events/unsubscribe")
		delete(body, "call")
		delete(body, "dialect")
		if !reflect.DeepEqual(canonical(t, body), canonical(t, c.CoreRequest)) {
			t.Errorf("%s: asked %v, want %v", c.Name, body, c.CoreRequest)
		}
	}
}

func TestVoucherRefusalsWireGolden(t *testing.T) {
	g := readVouchers(t)
	check := func(name string, e map[string]any, want *voucherCaseError, dialect string) {
		t.Helper()
		if e == nil || e["code"] != want.Code[dialect] || e["message"] != want.Message || !reflect.DeepEqual(canonical(t, e["data"]), canonical(t, want.Data)) {
			t.Errorf("%s: %v, want %v %q %v", name, e, want.Code[dialect], want.Message, want.Data)
		}
	}
	for _, c := range g.Vouchers.Refused {
		for dialect, ua := range voucherUAs {
			name := c.Name + " (" + dialect + ")"
			features := featureOn
			if c.Feature != nil && !*c.Feature {
				features = []string{}
			}
			vs := newVoucherServer(t, features, nil)
			_, e := vs.rpc(t, strp(c.User), ua, c.Method, c.Params)
			check(name, e, c.Error, dialect)
			if vs.askedEvents() {
				t.Errorf("%s: asked Wakeshift", name)
			}
			vs.noVoucherLogged(t, name)
		}
	}
	for _, c := range g.Vouchers.CoreRefused {
		for dialect, ua := range voucherUAs {
			name := c.Name + " (" + dialect + ")"
			vs := newVoucherServer(t, featureOn, map[string]voucherReply{"/events/subscribe": {c.CoreStatus, c.CoreBody}})
			_, e := vs.rpc(t, strp(c.User), ua, c.Method, c.Params)
			check(name, e, c.Error, dialect)
			if msg, _ := e["message"].(string); strings.Contains(msg, "again") || strings.Contains(msg, "retry") || strings.Contains(msg, "poll") {
				t.Errorf("%s: the refusal asks for something: %q", name, msg)
			}
			if got := vs.settled(); len(got) != 0 {
				t.Errorf("%s: OnSubscribe ran: %v", name, got)
			}
			vs.noVoucherLogged(t, name)
		}
	}
}

func TestVoucherEventsList(t *testing.T) {
	g := readVouchers(t)
	for key, c := range map[string]struct {
		ua      string
		feature []string
	}{"native_feature": {"openai-mcp/1.0.0", featureOn}, "bridged_feature": {"claude-code/2.1", featureOn}, "native_no_feature": {"openai-mcp/1.0.0", []string{}}} {
		vs := newVoucherServer(t, c.feature, nil)
		res, e := vs.rpc(t, "u1", c.ua, "events/list", map[string]any{})
		if e != nil {
			t.Fatalf("%s: %v", key, e)
		}
		var names []any
		for _, ev := range res["events"].([]any) {
			m := ev.(map[string]any)
			names = append(names, m["name"])
			props := m["inputSchema"].(map[string]any)["properties"].(map[string]any)
			_, has := props[voucherArg]
			if has != (m["name"] == "crawl.finished") {
				t.Errorf("%s: %s's wakeshift_voucher: %v", key, m["name"], has)
			}
		}
		sort.Slice(names, func(i, j int) bool { return names[i].(string) < names[j].(string) })
		if !reflect.DeepEqual(names, g.Vouchers.EventsList[key]) {
			t.Errorf("%s: %v, want %v", key, names, g.Vouchers.EventsList[key])
		}
	}
}

func TestVoucherToolSubscribeStates(t *testing.T) {
	g := readVouchers(t)
	for _, r := range g.Vouchers.States.Rows {
		features := []string{}
		if r.Feature {
			features = featureOn
		}
		name := "feature " + map[bool]string{true: "on", false: "off"}[r.Feature] + ", user " + strp(r.User)
		vs := newVoucherServer(t, features, nil)
		if _, e := vs.rpc(t, strp(r.User), "openai-mcp/1.0.0", "tools/call", map[string]any{"name": "start_crawl", "arguments": map[string]any{"job": "j1"}}); e != nil {
			t.Fatalf("%s: %v", name, e)
		}
		got := vs.result[0]
		if string(got.State) != r.Want["state"] || (r.Want["user"] != nil && got.User != r.Want["user"]) || (r.Want["reason"] != nil && got.Reason != r.Want["reason"]) {
			t.Errorf("%s: %+v, want %v", name, got, r.Want)
		}
		add := vs.last("/calls/addition")
		native, _ := add["native"].([]any)
		if (len(native) > 0) != r.NativeLine {
			t.Errorf("%s: native lines %v", name, native)
		}
		if r.NativeLine {
			want := []any{map[string]any{"name": "crawl.finished", "arguments": map[string]any{"job": "j1"}, "once": true, "ttl": 3600.0, "replay": 600.0}}
			if !reflect.DeepEqual(native, want) {
				t.Errorf("%s: native %v, want %v", name, native, want)
			}
		}
		if vs.last("/event-subscriptions") != nil {
			t.Errorf("%s: subscribed a conversation", name)
		}
		logged := strings.Count(vs.logs.String(), `"code":"native_unavailable"`)
		if (r.Logs != nil) != (logged == 1) || logged > 1 {
			t.Errorf("%s: native_unavailable logged %d times", name, logged)
		}
	}
}

// The voucher fields go only to a core that reported subscribe_vouchers:
// an older core refuses fields it doesn't know, so without the feature the
// wire is today's.
func TestVoucherFieldsOnlyWithTheFeature(t *testing.T) {
	v := readVouchers(t).Vouchers.Voucher
	for _, features := range [][]string{nil, {}, featureOn} {
		on := len(features) > 0
		vs := newVoucherServer(t, features, map[string]voucherReply{"/events/subscribe": {200, map[string]any{"id": "sub_1",
			"refresh_before": "2026-10-29T13:00:00Z", "cursor": "c1.0", "truncated": false, "created": true}}})
		if _, e := vs.rpc(t, "u1", "openai-mcp/1.0.0", "tools/call", map[string]any{"name": "start_crawl", "arguments": map[string]any{"job": "j1"}}, "Mcp-Session-Id", "sess-1"); e != nil {
			t.Fatalf("features %v: %v", features, e)
		}
		// An open event's native line: an addition whatever the core offers.
		if _, e := vs.rpc(t, "u1", "openai-mcp/1.0.0", "tools/call", map[string]any{"name": "watch_chat", "arguments": map[string]any{"channel": "eng"}}, "Mcp-Session-Id", "sess-1"); e != nil {
			t.Fatalf("features %v: %v", features, e)
		}
		vs.rpc(t, "u1", "openai-mcp/1.0.0", "events/subscribe", map[string]any{"name": "chat.message", "arguments": map[string]any{"channel": "eng", voucherArg: v},
			"delivery": map[string]any{"mode": "webhook", "url": "https://a.example/h", "secret": "whsec_dGVzdC1zZWNyZXQtdGVzdC1zZWNyZXQtdGVzdA=="}}, "Mcp-Session-Id", "sess-1")
		vs.mu.Lock()
		reqs := append([]voucherReq(nil), vs.reqs...)
		vs.mu.Unlock()
		for _, r := range reqs {
			b, _ := json.Marshal(r.body)
			if !on && (bytes.Contains(b, []byte("transport_session")) || bytes.Contains(b, []byte("wsv_")) || bytes.Contains(b, []byte(`"once"`))) {
				t.Errorf("features %v: %s carried a voucher field: %s", features, r.path, b)
			}
		}
		sub := vs.last("/events/subscribe")
		if _, ok := sub["voucher"]; ok {
			t.Errorf("features %v: an open event's voucher was forwarded", features)
		}
		if on {
			call, _ := vs.last("/calls/addition")["call"].(map[string]any)
			sum := sha256.Sum256([]byte("sess-1"))
			if call["transport_session"] != hex.EncodeToString(sum[:]) {
				t.Errorf("features %v: the addition's call %v, want its transport session", features, call)
			}
		}
	}
}

func TestVoucherNativeOnlyAdditionFailure(t *testing.T) {
	vs := newVoucherServer(t, featureOn, map[string]voucherReply{"/calls/addition": {500, map[string]any{"error": map[string]any{"code": "internal", "message": "try again"}}}})
	res, e := vs.rpc(t, "", "openai-mcp/1.0.0", "tools/call", map[string]any{"name": "start_crawl", "arguments": map[string]any{"job": "j1"}})
	if e != nil {
		t.Fatal(e)
	}
	if isErr, _ := res["isError"].(bool); isErr {
		t.Fatalf("isError: %v", res)
	}
	if want := []any{map[string]any{"type": "text", "text": "Crawl j1 started."}}; !reflect.DeepEqual(res["content"], want) {
		t.Errorf("content %v", res["content"])
	}
	if !strings.Contains(vs.logs.String(), `"reason":"addition_failed"`) {
		t.Errorf("no native_unavailable addition_failed:\n%s", vs.logs)
	}
}

func TestVoucherNeverReachesAuthorize(t *testing.T) {
	v := readVouchers(t).Vouchers.Voucher
	vs := newVoucherServer(t, featureOn, map[string]voucherReply{"/events/subscribe": {200, map[string]any{"id": "sub_1",
		"refresh_before": "2026-10-29T13:00:00Z", "cursor": "c1.0", "truncated": false, "created": true}}})
	if _, e := vs.rpc(t, "u1", "openai-mcp/1.0.0", "events/subscribe", map[string]any{"name": "deploy.finished", "arguments": map[string]any{"env": "prod", voucherArg: v},
		"delivery": map[string]any{"mode": "webhook", "url": "https://a.example/h", "secret": "whsec_dGVzdC1zZWNyZXQtdGVzdC1zZWNyZXQtdGVzdA=="}}); e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(vs.auth, []map[string]any{{"user": "u1", "env": "prod"}}) {
		t.Errorf("authorize got %v", vs.auth)
	}
	b, _ := json.Marshal(vs.last("/events/subscribe"))
	if bytes.Contains(b, []byte("wsv_")) {
		t.Errorf("forwarded: %s", b)
	}
	vs.noVoucherLogged(t, "open event")
}

func TestDefineEventRefusesWakeshiftVoucher(t *testing.T) {
	app := clientAt(t, "https://core.example")
	type bad struct {
		Voucher string `json:"wakeshift_voucher"`
	}
	_, err := DefineEvent(app, "x.y", EventDef[bad, crawlMsg]{Description: "X"})
	if !IsCode(err, CodeConfig) || !strings.Contains(err.Error(), voucherArg) {
		t.Fatalf("%v", err)
	}
}
