package wakeshift

// MCP Events for clients that speak it (docs/mcp-events-bridge.md), at the
// HTTP layer (Handler): the events capability on server/discover and
// initialize (under both the draft's extension key and the top-level key
// ChatGPT reads), events/list, and events/subscribe, events/unsubscribe and
// events/poll passed to Wakeshift, which verifies the client's callback and
// POSTs occurrences to it, signed, in order. A tools-only event is refused
// here, on every request (design §3); so is a native client without a
// user, and one an event's Authorize refuses, asked on every request.

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/wakeshift/wakeshift-go/internal/mcpevents/event"
)

// serveEvents answers one events/* request.
func (set *settings) serveEvents(w http.ResponseWriter, r *http.Request, post *postState, m rpcMessage) {
	a := set.app
	if m.Method == "events/list" {
		// To a native request while the core hands out subscribe vouchers,
		// the tools-only events too, each with a required wakeshift_voucher
		// (docs/subscribe-vouchers.md §4.3).
		var app *string
		events := a.events()
		if len(a.openEvents()) < len(events) && nativeClient(r, m.Params) && a.vouchersOffered(r.Context()) {
			name := a.appName(r.Context())
			app = &name
		}
		defs := []any{}
		for _, t := range events {
			switch {
			case t.open():
				defs = append(defs, t.definition())
			case app != nil:
				defs = append(defs, t.definitionFor(app))
			}
		}
		rpcResult(w, m.ID, map[string]any{"resultType": event.ResultType, "events": defs})
		return
	}
	var p eventParams
	if err := json.Unmarshal(m.Params, &p); err != nil {
		rpcError(w, m.ID, event.InvalidParams, "invalid params", nil)
		return
	}
	base := wireCall{Meta: p.Meta, UserAgent: r.Header.Get("User-Agent")}
	if info, ok := p.Meta["io.modelcontextprotocol/clientInfo"].(map[string]any); ok {
		base.Client, _ = info["name"].(string)
	}
	// The dialect before identity: a refusal needs it, and identify runs
	// only once it's needed.
	dialect := event.DialectOf(base.Client, base.UserAgent)
	codes := event.CodesFor(dialect)
	if h := r.Header.Get(event.NameHeader); h != "" {
		if v, ok := event.HeaderValue(h); !ok || v != p.Name {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{
				"code": event.HeaderMismatch, "message": "Header mismatch: Mcp-Name doesn't match params.name"}})
			return
		}
	}
	if m.Method == "events/stream" {
		rpcError(w, m.ID, codes.Unsupported, "push delivery isn't offered: use a webhook (events/subscribe) or poll (events/poll)",
			map[string]string{"feature": "deliveryMode", "value": strings.TrimPrefix(m.Method, "events/")})
		return
	}
	t := a.eventByName(p.Name)
	if t == nil {
		nameJSON, _ := json.Marshal(p.Name)
		rpcError(w, m.ID, codes.NotFound, fill("native_unknown_event", "{event}", string(nameJSON)), map[string]string{"kind": "event"})
		return
	}
	// The voucher leaves the arguments before anything reads them
	// (Authorize, OnSubscribe, logs, the subscription key).
	var offered string
	p.Arguments, offered = takeVoucher(p.Arguments)
	creates := m.Method == "events/subscribe" || m.Method == "events/poll"
	// Only the app's tools subscribe to a tools-only event: every poll is
	// refused, and every subscribe but one with the subscribe voucher a
	// tool handed out, while the core hands them out. An open event ignores
	// a voucher.
	voucher := ""
	if !t.open() && m.Method != "events/poll" && offered != "" && a.vouchersOffered(r.Context()) {
		voucher = offered
	}
	if m.Method == "events/poll" && !t.open() {
		// Never a poll, voucher or not: the refusal says a webhook is the way.
		rpcError(w, m.ID, codes.Forbidden, fill("native_tools_only_poll", "{event}", t.name), nil)
		return
	}
	if creates && !t.open() && voucher == "" {
		rpcError(w, m.ID, codes.Forbidden, fill("native_tools_only", "{event}", t.name), nil)
		return
	}
	p.Arguments = t.known(p.Arguments)
	if m.Method == "events/subscribe" && p.Delivery.Mode != "webhook" {
		rpcError(w, m.ID, codes.Unsupported, `delivery.mode must be "webhook": the only delivery offered`,
			map[string]string{"feature": "deliveryMode", "value": p.Delivery.Mode})
		return
	}
	id := post.identity(Request{ClientName: base.Client, Meta: p.Meta})
	id.logRequired(r.Context(), a, "")
	if id.refused != "" {
		rpcError(w, m.ID, codes.Forbidden, eventTools["native_no_user"], nil)
		return
	}
	call := id.id.apply(base)
	// With a voucher, no user is needed: the voucher is the authorization,
	// and Wakeshift compares the user when it was handed to one.
	if call.User == "" && voucher == "" {
		rpcError(w, m.ID, codes.Forbidden, eventTools["native_no_user"], nil)
		return
	}
	if voucher != "" {
		call.TransportSession = transportSession(r.Header.Get("Mcp-Session-Id"))
	}
	p.voucher = voucher
	// A poll has no subscribe step (each poll can start the subscription),
	// so every poll asks Authorize: access is checked again each time.
	if creates && t.authorize != nil {
		args, _ := json.Marshal(p.Arguments)
		sub := Subscriber{User: call.User, Client: call.Client, Native: true,
			Request: Request{Token: post.req.Token, Header: post.req.Header, ClientName: base.Client, Meta: p.Meta}}
		if !a.authorized(post.ctx, t, sub, args) {
			rpcError(w, m.ID, codes.Forbidden, fill("native_refused", "{event}", t.name), nil)
			return
		}
	}
	out, _, _, err := a.nativeCall(r.Context(), t, m.Method, call, dialect, p)
	if err != nil {
		code, msg, data := rpcErrorOf(err, codes, t.name)
		rpcError(w, m.ID, code, msg, data)
		return
	}
	rpcResult(w, m.ID, out)
}

// nativeCall passes a native request on to Wakeshift and, when it started
// a subscription, runs OnSubscribe: a redeemed voucher's subscription is
// the handing-out tool's, with the Context its call gave Subscribe if this
// process remembers it (taken, so only this exact voucher's redemption
// ever gets it); any other is the native client's.
func (c *Client) nativeCall(ctx context.Context, t *eventType, method string, call wireCall, dialect string, p eventParams) (out map[string]any, created bool, done <-chan struct{}, err error) {
	out, created, tool, err := c.eventCall(ctx, method, call, dialect, p)
	if err != nil || !created {
		return out, created, nil, err
	}
	args, _ := json.Marshal(p.Arguments)
	if tool != "" {
		value, _ := c.vouchers.take(p.voucher, c.now())
		return out, true, c.onSubscribed(ctx, t, args, subscribedInfo{user: call.User, via: viaTool, tool: tool, value: value}), nil
	}
	return out, true, c.onSubscribed(ctx, t, args, subscribedInfo{user: call.User, via: viaNative}), nil
}

// apply puts the identity in a call: the user, the agent's token (whose
// user is the agent's) if there's an agent, and the client's name if the
// call has none.
func (id Identity) apply(call wireCall) wireCall {
	call.User = id.User
	if call.Client == "" {
		call.Client = id.Client
	}
	if id.Agent != nil {
		call.User, call.AgentToken = id.Agent.User(), id.Agent.token
	}
	return call
}

// nativeClient reports whether the request comes from a client that
// subscribes with MCP Events itself: one that declares the extension in
// its capabilities (the request's _meta, as stateless clients send them),
// or that is known to (ChatGPT); never a harness Wakeshift wakes itself
// (bridgedClient).
func nativeClient(r *http.Request, params json.RawMessage) bool {
	var p struct {
		Meta map[string]any `json:"_meta"`
	}
	json.Unmarshal(params, &p)
	var client string
	if info, ok := p.Meta["io.modelcontextprotocol/clientInfo"].(map[string]any); ok {
		client, _ = info["name"].(string)
	}
	if event.DialectOf(client, r.Header.Get("User-Agent")) == event.DialectChatGPT {
		return true
	}
	if bridgedClient(client, r.Header.Get("User-Agent")) {
		return false
	}
	caps, _ := p.Meta["io.modelcontextprotocol/clientCapabilities"].(map[string]any)
	ext, _ := caps["extensions"].(map[string]any)
	_, ok := ext[event.ExtensionKey]
	return ok
}

// eventParams are events/subscribe's, events/unsubscribe's and
// events/poll's params.
type eventParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
	Delivery  struct {
		Mode   string `json:"mode"`
		URL    string `json:"url"`
		Secret string `json:"secret,omitempty"`
	} `json:"delivery"`
	TTLMs     *int64         `json:"ttlMs"`
	Cursor    *string        `json:"cursor"`
	MaxAgeMs  *int64         `json:"maxAgeMs"`
	MaxEvents *int           `json:"maxEvents"`
	Meta      map[string]any `json:"_meta"`
	// voucher is the subscribe voucher the client passed for a tools-only
	// event (wakeshift_voucher, taken out of Arguments), sent as voucher.
	voucher string
}

// transportSession is the SHA-256 (hex) of a request's Mcp-Session-Id, or
// "" without one: what binds an anonymous subscribe voucher to the
// transport session it was handed out in.
func transportSession(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

// errNoSubscription is an events/unsubscribe that matched nothing.
var errNoSubscription = errors.New("no subscription matches")

// eventCall passes a native request on to Wakeshift; created: it started a
// subscription (events/subscribe, or a poll's first); tool: the tool that
// handed out the voucher it redeemed, if it did.
func (c *Client) eventCall(ctx context.Context, method string, call wireCall, dialect string, p eventParams) (map[string]any, bool, string, error) {
	if method == "events/poll" {
		res, created, err := c.pollCall(ctx, call, p)
		return res, created, "", err
	}
	body := map[string]any{"call": call, "name": p.Name, "arguments": p.Arguments, "delivery": p.Delivery, "dialect": dialect}
	if p.voucher != "" {
		body["voucher"] = p.voucher
	}
	if method == "events/unsubscribe" {
		var out struct {
			Ended bool `json:"ended"`
		}
		if err := c.eventsCall(ctx, "POST", "/api/apps/v1/events/unsubscribe", body, &out); err != nil {
			return nil, false, "", err
		}
		// The draft says NotFound for a key that matches nothing; ChatGPT
		// gets the empty result it always has.
		if !out.Ended && dialect != event.DialectChatGPT {
			return nil, false, "", errNoSubscription
		}
		return map[string]any{"resultType": event.ResultType}, false, "", nil
	}
	if p.TTLMs != nil {
		body["ttl_ms"] = *p.TTLMs
	}
	if p.Cursor != nil {
		body["cursor"] = *p.Cursor
	}
	if p.MaxAgeMs != nil {
		body["max_age_ms"] = *p.MaxAgeMs
	}
	var out struct {
		ID             string          `json:"id"`
		RefreshBefore  time.Time       `json:"refresh_before"`
		Cursor         string          `json:"cursor"`
		Truncated      bool            `json:"truncated"`
		DeliveryStatus json.RawMessage `json:"delivery_status"`
		Created        bool            `json:"created"`
		Via            string          `json:"via"`
		Tool           string          `json:"tool"`
	}
	if err := c.eventsCall(ctx, "POST", "/api/apps/v1/events/subscribe", body, &out); err != nil {
		return nil, false, "", err
	}
	tool := ""
	if rest, ok := strings.CutPrefix(out.Via, "tool:"); ok {
		tool = cmp.Or(out.Tool, rest)
	}
	res := map[string]any{"resultType": event.ResultType, "id": out.ID, "refreshBefore": out.RefreshBefore.UTC().Format(time.RFC3339),
		"cursor": out.Cursor, "truncated": out.Truncated}
	if len(out.DeliveryStatus) > 0 && string(out.DeliveryStatus) != "null" {
		res["deliveryStatus"] = out.DeliveryStatus
	}
	return res, out.Created, tool, nil
}

// pollCall passes an events/poll on to Wakeshift, which keeps the
// subscription the first poll starts and the events waiting for it
// (sdk/typescript/test/mcp-events-wire.json has both sides of each).
func (c *Client) pollCall(ctx context.Context, call wireCall, p eventParams) (map[string]any, bool, error) {
	body := map[string]any{"call": call, "name": p.Name, "arguments": p.Arguments}
	if p.Cursor != nil {
		body["cursor"] = *p.Cursor
	}
	if p.MaxAgeMs != nil {
		body["max_age_ms"] = *p.MaxAgeMs
	}
	if p.MaxEvents != nil {
		body["max_events"] = *p.MaxEvents
	}
	var out struct {
		Events     []json.RawMessage `json:"events"`
		Cursor     string            `json:"cursor"`
		Truncated  bool              `json:"truncated"`
		HasMore    bool              `json:"has_more"`
		NextPollMs int64             `json:"next_poll_ms"`
		Created    bool              `json:"created"`
	}
	if err := c.eventsCall(ctx, "POST", "/api/apps/v1/events/poll", body, &out); err != nil {
		return nil, false, err
	}
	if out.Events == nil {
		out.Events = []json.RawMessage{}
	}
	return map[string]any{"resultType": event.ResultType, "events": out.Events, "cursor": out.Cursor, "truncated": out.Truncated,
		"hasMore": out.HasMore, "nextPollMs": out.NextPollMs}, out.Created, nil
}

// rpcInternal is JSON-RPC's internal error.
const rpcInternal = -32603

// rpcErrorOf is the JSON-RPC error for a failed call, in the client's
// dialect's codes. A conversation that isn't the caller's is Forbidden,
// whether it exists or not, so a handle can't be probed.
func rpcErrorOf(err error, codes event.Codes, name string) (code int, msg string, data any) {
	if errors.Is(err, errNoSubscription) {
		return codes.NotFound, "no subscription matches that event, arguments and delivery URL", map[string]string{"kind": "subscription"}
	}
	var e *APIError
	if !errors.As(err, &e) || e.Code == "unreachable" {
		return rpcInternal, "Wakeshift couldn't be reached", nil
	}
	switch e.Code {
	case "bad_voucher":
		// One answer, whatever the reason (docs/subscribe-vouchers.md §6.2).
		return codes.Forbidden, fill("native_bad_voucher", "{event}", name), nil
	case "callback_failed":
		return codes.CallbackEndpoint, e.Message, map[string]string{"reason": e.Reason}
	case "unknown_event":
		return codes.NotFound, e.Message, map[string]string{"kind": "event"}
	case "unsupported":
		return codes.Unsupported, e.Message, map[string]string{"feature": "deliveryMode"}
	case "subscription_limit":
		return codes.ResourceExhausted, e.Message, map[string]string{"limit": "subscriptions"}
	case "subscription_stopped":
		return codes.Forbidden, e.Message, map[string]string{"reason": "stopped"}
	case "bad_request", "bad_arguments":
		return event.InvalidParams, e.Message, nil
	case "unknown_session", "other_client", "unknown_conversation":
		return codes.Forbidden, "another client of yours holds that subscription", nil
	}
	return rpcInternal, "Wakeshift refused the subscription: " + e.Message, nil
}

func rpcResult(w http.ResponseWriter, id json.RawMessage, result any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func rpcError(w http.ResponseWriter, id json.RawMessage, code int, msg string, data any) {
	e := map[string]any{"code": code, "message": msg}
	if data != nil {
		e["data"] = data
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "error": e})
}

// rewrite serves a request with next, passing each JSON-RPC message of the
// answer (a JSON one, or an SSE one's data lines) through f. A vendor's own
// handler answers too (a server/discover it serves itself): an answer
// without a Content-Type is tried as JSON, and an SSE data line may omit
// the space after "data:". f leaves anything it can't parse as it was.
func rewrite(next http.Handler, w http.ResponseWriter, r *http.Request, f func([]byte) []byte) {
	rec := &recorder{header: http.Header{}, code: http.StatusOK}
	next.ServeHTTP(rec, r)
	out := rec.body.Bytes()
	switch ct := rec.header.Get("Content-Type"); {
	case ct == "", strings.HasPrefix(ct, "application/json"):
		out = f(out)
	case strings.HasPrefix(ct, "text/event-stream"):
		lines := bytes.Split(out, []byte("\n"))
		for i, l := range lines {
			if data, ok := bytes.CutPrefix(l, []byte("data:")); ok {
				data = bytes.TrimPrefix(data, []byte(" "))
				lines[i] = append([]byte("data: "), f(data)...)
			}
		}
		out = bytes.Join(lines, []byte("\n"))
	}
	maps.Copy(w.Header(), rec.header)
	w.Header().Del("Content-Length")
	w.WriteHeader(rec.code)
	w.Write(out)
}

// recorder buffers a handler's answer, for rewrite.
type recorder struct {
	header http.Header
	code   int
	wrote  bool
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header { return r.header }

func (r *recorder) WriteHeader(code int) {
	if !r.wrote {
		r.code, r.wrote = code, true
	}
}

func (r *recorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.body.Write(b)
}

// Flush is a no-op: the answer is sent once it's rewritten.
func (r *recorder) Flush() {}

// withEventsCapability is a discover or initialize answer with the events
// capability added, unless the server declares MCP Events itself: then
// Wakeshift serves none, and says so once (events_capability_kept). It's
// added whenever the client has a key (Handler serves events/* only then)
// and the app defines any event, open or tools-only (a tools-only one is
// subscribed to with a voucher); with no event defined, it isn't.
func (set *settings) withEventsCapability(ctx context.Context, b []byte) []byte {
	if set.eventsKept.Load() {
		return b
	}
	if declaresEvents(b) {
		set.eventsKept.Store(true)
		set.app.logOnce(ctx, "events_capability_kept", "", "the server declares MCP Events itself, so Wakeshift serves none and leaves its capability as it is")
		return b
	}
	// Any event: one open to MCP Events clients, or a tools-only one, which
	// a native client subscribes to with the subscribe voucher a tool handed
	// it, while the core hands them out (docs/subscribe-vouchers.md §4.3).
	if len(set.app.events()) == 0 {
		return b
	}
	return addEventsCapability(b)
}

// declaresEvents reports whether a discover or initialize answer already
// declares MCP Events.
func declaresEvents(b []byte) bool {
	var resp struct {
		Result struct {
			Capabilities map[string]json.RawMessage `json:"capabilities"`
		} `json:"result"`
	}
	if json.Unmarshal(b, &resp) != nil {
		return false
	}
	caps := resp.Result.Capabilities
	if _, ok := caps[event.TopLevelKey]; ok {
		return true
	}
	var ext map[string]json.RawMessage
	json.Unmarshal(caps["extensions"], &ext)
	_, ok := ext[event.ExtensionKey]
	return ok
}

// addEventsCapability adds the capability to a JSON-RPC result that has
// capabilities (anything else comes back as it was): the draft's
// extensions["io.modelcontextprotocol/events"], and the top-level "events"
// ChatGPT reads.
func addEventsCapability(b []byte) []byte {
	var resp map[string]any
	if json.Unmarshal(b, &resp) != nil {
		return b
	}
	res, ok := resp["result"].(map[string]any)
	if !ok {
		return b
	}
	caps, _ := res["capabilities"].(map[string]any)
	if caps == nil {
		caps = map[string]any{}
	}
	settings := map[string]any{"listChanged": false}
	caps[event.TopLevelKey] = settings
	ext, _ := caps["extensions"].(map[string]any)
	if ext == nil {
		ext = map[string]any{}
	}
	ext[event.ExtensionKey] = settings
	caps["extensions"] = ext
	res["capabilities"] = caps
	out, err := json.Marshal(resp)
	if err != nil {
		return b
	}
	return out
}

// bridgedClients are the harnesses Wakeshift wakes through its own paths (a
// Monitor, hooks, a connector, a plugin), by MCP clientInfo name or HTTP
// User-Agent product token, compared case-insensitively
// (internal/harnesses). A call from one is never treated as a native MCP
// Events client, whatever capabilities it declares: Claude Code, for one,
// isn't woken by MCP Events, so treating it as native would leave it with
// nothing listening. The same list in every SDK.
var bridgedClients = []string{"claude-code", "codex-mcp-client", "cursor", "cursor-vscode", "copilot-cli", "opencode", "antigravity-client", "openclaw-bundle-mcp", "hermes-agent", "capy", "muse-custom-connector"}

// bridgedClient reports whether a client is one of bridgedClients, by its
// clientInfo name or its User-Agent's first product token.
func bridgedClient(client, userAgent string) bool {
	if slices.Contains(bridgedClients, strings.ToLower(strings.TrimSpace(client))) {
		return true
	}
	product, _, _ := strings.Cut(userAgent, "/")
	product = strings.ToLower(strings.TrimSpace(product))
	return product != "" && slices.Contains(bridgedClients, product)
}
