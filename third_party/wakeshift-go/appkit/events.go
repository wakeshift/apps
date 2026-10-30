package appkit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/wakeshift-go/internal/mcpevents/event"
)

// Identity is who's behind a request to the app's MCP endpoint, as the app
// knows them (AttachOptions.Identify).
type Identity struct {
	// User is the app's ID for the person ("" for none): their
	// subscriptions belong to them, a personal event's subscriptions need
	// one, and list_subscriptions lists theirs.
	User string
	// Agent is the app's agent behind the request (ActAs, for the user's
	// workspace and agent), if any: every call for the request is then the
	// agent's, with its token, and the user is Agent.User(). A wrapped
	// tool's subscriptions are the agent's sessions'.
	Agent *Agent
	// Client is the MCP client's name, for a request that doesn't carry
	// one: a stateless server never sees initialize, so the app passes the
	// name it knows from its own sign-in (an OAuth registration's
	// client_name, say). Wakeshift reads it to tell which program the agent
	// runs in, and whether the client subscribes to events itself. A name
	// the request carries (initialize's, or its _meta's) wins. Leave it ""
	// for a caller that must have no name.
	Client string
}

// AttachOptions configure Attach.
type AttachOptions struct {
	// Identify tells who's behind a request, from the endpoint's
	// authentication (it isn't called for a request with none). An error
	// counts as no user. Typically the app's own ID for the user its token
	// names (the TokenInfo's UserID), and the agent from ActAs for an app
	// acting for its agents. Nil: no request has a user.
	Identify func(context.Context, *auth.TokenInfo) (Identity, error)
	// Tools names which of Attach's own tools it adds: SubscribeEventsTool,
	// UnsubscribeEventsTool and ListSubscriptionsTool. Nil adds all three;
	// an empty list adds none. Leave out unsubscribe_events or
	// list_subscriptions only if your own tools let a person see and stop
	// their subscriptions. Attach panics on a name that isn't one of them.
	Tools []string
	// Events configures the event features.
	Events EventsOptions
}

// EventsOptions configure Attach's event features.
type EventsOptions struct {
	// Native, if set, is asked first whether a wrapped tool's call comes
	// from a client that subscribes to the app's events itself
	// (ToolCall.Native), for callers the SDK can't tell (an app's own house
	// agents). False leaves it to the SDK's own checks.
	Native func(ctx context.Context, req *mcp.CallToolRequest) bool
}

// Attachment is Wakeshift attached to an MCP server (Attach).
type Attachment struct {
	app      *App
	identify func(context.Context, *auth.TokenInfo) Identity
	events   EventsOptions
	warned   sync.Map // warnOnce's keys
	schemas  sync.Map // *mcp.Tool → its input schema, decoded (undeclared.go)
}

// Attach attaches Wakeshift to the app's MCP server: the one call that
// connects the two. Today it serves MCP Events and the event tools, so
// every client can subscribe to the app's event types (define them first,
// with DefineEvent); Wrap puts it in front of the MCP endpoint's handler.
//
//   - Clients that speak MCP Events (ChatGPT) subscribe natively: Wrap
//     advertises the events capability (server/discover and initialize,
//     under both the draft's extension key and the top-level key ChatGPT
//     reads), answers events/list with the event types, and passes
//     events/subscribe and events/unsubscribe to Wakeshift, which verifies
//     the client's callback and POSTs occurrences to it, signed, in order
//     (docs/mcp-events-bridge.md). It speaks the revision event.Revision
//     names, in two dialects chosen per request by the client's clientInfo
//     (or User-Agent): ChatGPT's and the draft's. An Mcp-Name header, when
//     present, must match params.name.
//   - Every other client subscribes with the subscribe_events tool, which
//     Attach adds to srv (tools/list hides it from native clients), and
//     Wakeshift wakes it the way its harness can be woken.
//   - Every client can list its subscriptions (list_subscriptions) and stop
//     one (unsubscribe_events): a headless app's users have no other way.
//
// AttachOptions.Tools picks which of the three tools it adds (all three by
// default).
//
// Call it once per server, after the last DefineEvent (with none defined it
// panics: it would serve nothing; calling it again replaces the tools, as
// the MCP SDK's AddTool does), and after the server's own receiving
// middleware: its middleware then runs last on a tool's result, so the
// block a wrapped tool's reply gets (Events) stays last. Add wrapped tools
// after it. Each event type's
// Authorize runs before any subscription. The tools' wording is fixed, and
// the TypeScript SDK's is the same (sdk/typescript/test/event-tools.json).
// Wrap the MCP handler after the endpoint's authentication:
//
//	attached := app.Attach(srv, appkit.AttachOptions{
//		Identify: func(_ context.Context, ti *auth.TokenInfo) (appkit.Identity, error) {
//			return appkit.Identity{User: ti.UserID}, nil
//		},
//	})
//	h := mcp.NewStreamableHTTPHandler(…, &mcp.StreamableHTTPOptions{Stateless: true})
//	mux.Handle("/mcp", requireAuth(attached.Wrap(h)))
func (a *App) Attach(srv *mcp.Server, opts AttachOptions) *Attachment {
	if len(a.events()) == 0 {
		panic("appkit: Attach needs at least one event (DefineEvent) defined first")
	}
	at := &Attachment{app: a, events: opts.Events, identify: func(ctx context.Context, ti *auth.TokenInfo) Identity {
		if ti == nil || opts.Identify == nil {
			return Identity{}
		}
		id, err := opts.Identify(ctx, ti)
		if err != nil {
			return Identity{}
		}
		return id
	}}
	a.addEventTools(srv, at.identify, builtinTools(opts.Tools))
	// The MCP server's own pipeline: every request Wrap passes on comes
	// through here. A wrapped tool's call (Events) gets its block here,
	// after the MCP SDK has validated the output and filled
	// structuredContent, and the subscriptions it made are rolled back here
	// if the final result failed.
	srv.AddReceivingMiddleware(at.middleware)
	// A process that serves the app's tools sends the manifest
	// automatically (manifest.go); one that only emits never does.
	a.mu.Lock()
	a.attached = true
	a.mu.Unlock()
	return at
}

// Wrap puts Wakeshift in front of the MCP endpoint's handler, after the
// endpoint's authentication: MCP Events' methods and capability, and for
// clients on protocol 2026-07-28 that take MCP tasks, Wakeshift's wait tool
// and the tasks it answers with (tasks.go).
func (at *Attachment) Wrap(next http.Handler) http.Handler {
	a := at.app
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
		if tooBig := new(http.MaxBytesError); errors.As(err, &tooBig) {
			http.Error(w, "the request is over 4 MB", http.StatusRequestEntityTooLarge)
			return
		}
		if err != nil {
			http.Error(w, "can't read the request", http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(body, &req) != nil || len(req.ID) == 0 {
			next.ServeHTTP(w, r) // a batch, a notification or a response
			return
		}
		switch req.Method {
		case "server/discover":
			// The tasks extension too: the wait tool (tasks.go).
			rewrite(next, w, r, func(b []byte) []byte { return addTasksCapability(addEventsCapability(b)) })
			return
		case "initialize":
			withEventsCapability(next, w, r)
			return
		case "tools/list":
			var p struct {
				Meta map[string]any `json:"_meta"`
			}
			json.Unmarshal(req.Params, &p)
			drop := ""
			if nativeClient(r, req.Params) {
				drop = SubscribeEventsTool
			}
			if wait := takesTasks(p.Meta, r.Header); drop != "" || wait {
				rewrite(next, w, r, editTools(drop, wait))
				return
			}
			next.ServeHTTP(w, r)
			return
		case "tools/call":
			var p struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(req.Params, &p) == nil && p.Name == WaitTool {
				at.serveTask(w, r, req.ID, req.Method, req.Params)
				return
			}
			next.ServeHTTP(w, r)
			return
		case "tasks/get", "tasks/cancel":
			// Only Wakeshift's own tasks: any other ID goes on to the handler.
			var p struct {
				TaskID string `json:"taskId"`
			}
			if json.Unmarshal(req.Params, &p) == nil && strings.HasPrefix(p.TaskID, TaskIDPrefix) {
				at.serveTask(w, r, req.ID, req.Method, req.Params)
				return
			}
			next.ServeHTTP(w, r)
			return
		case "events/list":
			defs := []any{}
			for _, t := range a.events() {
				defs = append(defs, t.definition())
			}
			rpcResult(w, req.ID, map[string]any{"resultType": event.ResultType, "events": defs})
			return
		case "events/subscribe", "events/unsubscribe", "events/poll", "events/stream":
		default:
			next.ServeHTTP(w, r)
			return
		}
		var p eventParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			rpcError(w, req.ID, event.InvalidParams, "invalid params", nil)
			return
		}
		base := wireCall{Meta: p.Meta, UserAgent: r.Header.Get("User-Agent")}
		if info, ok := p.Meta["io.modelcontextprotocol/clientInfo"].(map[string]any); ok {
			base.Client, _ = info["name"].(string)
		}
		call := at.identify(r.Context(), auth.TokenInfoFromContext(r.Context())).apply(base)
		dialect := event.DialectOf(call.Client, call.UserAgent)
		codes := event.CodesFor(dialect)
		if h := r.Header.Get(event.NameHeader); h != "" {
			if v, ok := event.HeaderValue(h); !ok || v != p.Name {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{
					"code": event.HeaderMismatch, "message": "Header mismatch: Mcp-Name doesn't match params.name"}})
				return
			}
		}
		if req.Method == "events/stream" {
			rpcError(w, req.ID, codes.Unsupported, "push delivery isn't offered: use a webhook (events/subscribe) or poll (events/poll)",
				map[string]string{"feature": "deliveryMode", "value": strings.TrimPrefix(req.Method, "events/")})
			return
		}
		t := a.eventByName(p.Name)
		if t == nil {
			nameJSON, _ := json.Marshal(p.Name)
			rpcError(w, req.ID, codes.NotFound, fill("native_unknown_event", "{event}", string(nameJSON)), map[string]string{"kind": "event"})
			return
		}
		p.Arguments = t.known(p.Arguments)
		if req.Method == "events/subscribe" && p.Delivery.Mode != "webhook" {
			rpcError(w, req.ID, codes.Unsupported, `delivery.mode must be "webhook": the only delivery offered`,
				map[string]string{"feature": "deliveryMode", "value": p.Delivery.Mode})
			return
		}
		if call.User == "" {
			rpcError(w, req.ID, codes.Forbidden, eventTools["native_no_user"], nil)
			return
		}
		// A poll has no subscribe step (each poll can start the subscription),
		// so every poll asks Authorize: access is checked again each time.
		if (req.Method == "events/subscribe" || req.Method == "events/poll") && t.authorize != nil {
			args, _ := json.Marshal(p.Arguments)
			if !t.authorize(r.Context(), Subscriber{User: call.User, Client: call.Client, Native: true}, args) {
				rpcError(w, req.ID, codes.Forbidden, fill("native_refused", "{event}", t.name), nil)
				return
			}
		}
		out, err := a.eventCall(r.Context(), req.Method, call, dialect, p)
		if err != nil {
			code, msg, data := rpcErrorOf(err, codes)
			rpcError(w, req.ID, code, msg, data)
			return
		}
		rpcResult(w, req.ID, out)
	})
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
// or that is known to (ChatGPT).
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
}

// errNoSubscription is an events/unsubscribe that matched nothing.
var errNoSubscription = errors.New("no subscription matches")

func (a *App) eventCall(ctx context.Context, method string, call wireCall, dialect string, p eventParams) (map[string]any, error) {
	if method == "events/poll" {
		return a.pollCall(ctx, call, p)
	}
	body := map[string]any{"call": call, "name": p.Name, "arguments": p.Arguments, "delivery": p.Delivery, "dialect": dialect}
	if method == "events/unsubscribe" {
		var out struct {
			Ended bool `json:"ended"`
		}
		if err := a.eventsCall(ctx, "POST", "/api/apps/v1/events/unsubscribe", body, &out); err != nil {
			return nil, err
		}
		// The draft says NotFound for a key that matches nothing; ChatGPT
		// gets the empty result it always has.
		if !out.Ended && dialect != event.DialectChatGPT {
			return nil, errNoSubscription
		}
		return map[string]any{"resultType": event.ResultType}, nil
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
	}
	if err := a.eventsCall(ctx, "POST", "/api/apps/v1/events/subscribe", body, &out); err != nil {
		return nil, err
	}
	res := map[string]any{"resultType": event.ResultType, "id": out.ID, "refreshBefore": out.RefreshBefore.UTC().Format(time.RFC3339),
		"cursor": out.Cursor, "truncated": out.Truncated}
	if len(out.DeliveryStatus) > 0 && string(out.DeliveryStatus) != "null" {
		res["deliveryStatus"] = out.DeliveryStatus
	}
	return res, nil
}

// pollCall passes an events/poll on to Wakeshift, which keeps the
// subscription the first poll starts and the events waiting for it
// (sdk/typescript/test/mcp-events-wire.json has both sides of each).
func (a *App) pollCall(ctx context.Context, call wireCall, p eventParams) (map[string]any, error) {
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
	}
	if err := a.eventsCall(ctx, "POST", "/api/apps/v1/events/poll", body, &out); err != nil {
		return nil, err
	}
	if out.Events == nil {
		out.Events = []json.RawMessage{}
	}
	return map[string]any{"resultType": event.ResultType, "events": out.Events, "cursor": out.Cursor, "truncated": out.Truncated,
		"hasMore": out.HasMore, "nextPollMs": out.NextPollMs}, nil
}

// rpcInternal is JSON-RPC's internal error.
const rpcInternal = -32603

// rpcErrorOf is the JSON-RPC error for a failed call, in the client's
// dialect's codes. A conversation that isn't the caller's is Forbidden,
// whether it exists or not, so a handle can't be probed.
func rpcErrorOf(err error, codes event.Codes) (code int, msg string, data any) {
	if errors.Is(err, errNoSubscription) {
		return codes.NotFound, "no subscription matches that event, arguments and delivery URL", map[string]string{"kind": "subscription"}
	}
	var e *APIError
	if !errors.As(err, &e) || e.Code == "unreachable" {
		return rpcInternal, "Wakeshift couldn't be reached", nil
	}
	switch e.Code {
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

// withEventsCapability serves a discover (or initialize) request with
// next, adding the events capability to the server's capabilities, in a
// JSON answer or an SSE one.
func withEventsCapability(next http.Handler, w http.ResponseWriter, r *http.Request) {
	rewrite(next, w, r, addEventsCapability)
}

// rewrite serves a request with next, passing each JSON-RPC message of the
// answer (a JSON one, or an SSE one's data lines) through f.
func rewrite(next http.Handler, w http.ResponseWriter, r *http.Request, f func([]byte) []byte) {
	rec := httptest.NewRecorder()
	next.ServeHTTP(rec, r)
	out := rec.Body.Bytes()
	switch ct := rec.Header().Get("Content-Type"); {
	case strings.HasPrefix(ct, "application/json"):
		out = f(out)
	case strings.HasPrefix(ct, "text/event-stream"):
		lines := bytes.Split(out, []byte("\n"))
		for i, l := range lines {
			if data, ok := bytes.CutPrefix(l, []byte("data: ")); ok {
				lines[i] = append([]byte("data: "), f(data)...)
			}
		}
		out = bytes.Join(lines, []byte("\n"))
	}
	maps.Copy(w.Header(), rec.Header())
	w.Header().Del("Content-Length")
	w.WriteHeader(rec.Code)
	w.Write(out)
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
