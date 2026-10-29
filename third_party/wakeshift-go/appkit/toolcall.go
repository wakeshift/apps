package appkit

// One way to subscribe from a tool (docs/subscribe-from-tools.md): a tool
// handler wrapped by Events gets a *ToolCall, and subscribes the calling
// conversation with event.Subscribe (ends it with Unsubscribe). The
// wrapper adds one Wakeshift block to whatever the handler returns, so a
// conversation can't be left unwakeable by a forgotten argument, a wrong
// tool name or an unreturned result; and a call that fails ends the
// subscriptions it made.
//
//	type JoinIn struct {
//		Game string `json:"game"`
//		appkit.WakeArgs // session_key, harness, conversation_id
//	}
//
//	attached := app.Attach(srv, appkit.AttachOptions{Identify: identify}) // after the server's own middleware
//	mcp.AddTool(srv, &mcp.Tool{Name: "join", Description: "…"}, appkit.Events(attached,
//		func(ctx context.Context, call *appkit.ToolCall, in JoinIn) (*mcp.CallToolResult, View, error) {
//			sub, err := updates.Subscribe(ctx, call, UpdateArgs{}, appkit.Reason("Playing "+in.Game))
//			if err != nil {
//				return nil, View{}, err
//			}
//			return nil, View{Game: in.Game, Session: sub.Session}, nil
//		}))

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/wakeshift-go/internal/mcpevents/event"
)

// WakeArgs are the arguments a wrapped tool takes so Wakeshift can find the
// calling conversation: embed it in the tool's input (Events requires it).
type WakeArgs struct {
	SessionKey     string `json:"session_key,omitempty" jsonschema:"This conversation's session_key, from an earlier result (a secret token, or a handle such as claude-code#2). Keep it to this conversation; don't share or log it."`
	Harness        string `json:"harness,omitempty" jsonschema:"The program you're running in: codex, claude-code, cursor (local CLI or IDE), cursor-cloud (Cursor cloud agent), grok-bot, copilot-cli, opencode, antigravity, openclaw (even when it runs your turns in Codex), hermes, chatgpt, or your program's name if it isn't listed"`
	ConversationID string "json:\"conversation_id,omitempty\" jsonschema:\"Your own ID for this conversation, if your program gives you one. Cursor and Grok Bot: the output of `printenv CURSOR_CONVERSATION_ID`. Codex and Antigravity send it automatically; others leave it empty\""
}

func (w WakeArgs) wakeArgs() WakeArgs { return w }

// WakeInput is a wrapped tool's input: a struct that embeds WakeArgs (only
// WakeArgs has the method, so nothing else satisfies it).
type WakeInput interface{ wakeArgs() WakeArgs }

// WakeOutput, embedded in a wrapped tool's output, opts the tool in to
// carrying Wakeshift's block in its structured output too: Events sets
// Wakeshift to the block's text (without a beacon line), for hosts that show
// the model only structuredContent. The block stays in the content as well.
type WakeOutput struct {
	Wakeshift string `json:"wakeshift,omitempty" jsonschema:"What Wakeshift adds for this conversation: how it's woken by this app's events"`
}

func (WakeOutput) wakeOutput() {}

type wakeOutputter interface{ wakeOutput() }

// Errors from a *ToolCall used where it can't be.
var (
	// ErrNoCall: the ToolCall wasn't made by Events (a zero ToolCall, or
	// one built by hand). Only a wrapped tool's handler gets a working one.
	ErrNoCall = errors.New("appkit: this ToolCall wasn't made by appkit.Events: subscribe only from a wrapped tool's handler")
	// ErrCallEnded: the handler that got the ToolCall has returned.
	ErrCallEnded = errors.New("appkit: the tool call has ended: subscribe before the handler returns")
)

// errNotAttached: a wrapped tool ran without Attach's middleware in front.
var errNotAttached = errors.New("appkit: Events runs only behind its Attachment's middleware: add the tool to the server you passed to Attach")

// ToolCall is one call of a wrapped tool (Events): who's calling, and the
// calls that subscribe the calling conversation.
type ToolCall struct {
	// Request is the MCP request.
	Request *mcp.CallToolRequest
	// User is who AttachOptions.Identify says is calling ("" for none).
	User string
	// Agent is the agent the app acts for (Identity.Agent), if any.
	Agent *Agent
	// Tool is the tool being called.
	Tool string
	// Native: the client subscribes to the app's events itself (MCP Events:
	// ChatGPT, the house agents). Subscribe then makes no conversation
	// subscription, and the block tells it what to subscribe to.
	Native bool
	// NativeConversation is a native client's conversation, when it names
	// one (ChatGPT's _meta["openai/session"]), else "".
	NativeConversation string
	// Session is the session_key passed; after Subscribe or
	// ResolveSession, the conversation's session.
	Session string
	// Recognised is what Wakeshift recognised the agent's program as: nil
	// until a Subscribe or ResolveSession.
	Recognised *Recognised

	s *callScope
}

// Subscription is the calling conversation's subscription, made or renewed
// by Subscribe. ID and User address it later: an Emit's
// ToSubscription(User, ID), or End(ctx, User, ID).
type Subscription struct {
	ID   string `json:"id"`
	User string `json:"user"`
	// Created is false for a renewal.
	Created bool `json:"created"`
	// Session is the conversation's session.
	Session string `json:"session"`
	// Recognised is what Wakeshift recognised the agent's program as.
	Recognised Recognised `json:"recognised"`
	// Native: the caller subscribes itself (ToolCall.Native); nothing was
	// made, ID is "", and the block tells it what to subscribe to.
	Native bool `json:"native"`
}

// SubscribeOption shapes a Subscribe.
type SubscribeOption func(*subscribeOptions)

type subscribeOptions struct {
	ttl           time.Duration
	reason, label string
}

// TTL ends the subscription after this long without a delivery or a fresh
// subscribe (default 30 days).
func TTL(d time.Duration) SubscribeOption { return func(o *subscribeOptions) { o.ttl = d } }

// Reason says why the app subscribes the conversation, shown in the console
// (at most 200 characters).
func Reason(r string) SubscribeOption { return func(o *subscribeOptions) { o.reason = r } }

// Label names a new session (agent@label), for an app acting for its
// agents.
func Label(l string) SubscribeOption { return func(o *subscribeOptions) { o.label = l } }

// Events wraps a tool handler for mcp.AddTool so it gets a *ToolCall, and its
// reply gets Wakeshift's block. Add the tool after Attach, on the server
// passed to it; register Attach after the server's own receiving
// middleware, so its block stays last.
//
// In must embed WakeArgs. If Out embeds WakeOutput, its wakeshift field
// carries the block's text too.
func Events[In WakeInput, Out any](at *Attachment, h func(context.Context, *ToolCall, In) (*mcp.CallToolResult, Out, error)) mcp.ToolHandlerFor[In, Out] {
	if at == nil || h == nil {
		panic("appkit.Events: needs the Attachment Attach returned, and a handler")
	}
	var zero Out
	// A pointer Out's nil zero still has its type's methods.
	_, wantsOutput := any(zero).(wakeOutputter)
	return func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		s, _ := ctx.Value(scopeKey{}).(*callScope)
		if s == nil || s.at != at || s.req != req {
			return nil, zero, errNotAttached
		}
		args := in.wakeArgs()
		c, id := at.newToolCall(ctx, req, args)
		c.s = s
		s.begin(c, id, args, wantsOutput)
		defer s.end()
		return h(ctx, c, in)
	}
}

// newToolCall is the ToolCall for req, before its handler runs.
func (at *Attachment) newToolCall(ctx context.Context, req *mcp.CallToolRequest, args WakeArgs) (*ToolCall, Identity) {
	var id Identity
	if req.Extra != nil {
		id = at.identify(ctx, req.Extra.TokenInfo)
	}
	c := &ToolCall{Request: req, User: id.User, Agent: id.Agent, Session: args.SessionKey}
	if req.Params != nil {
		c.Tool = req.Params.Name
	}
	c.Native, c.NativeConversation = at.nativeOf(ctx, req, id.Client)
	return c, id
}

// nativeOf says whether a call comes from a client that subscribes to the
// app's events itself, on stateless servers too (which never see
// initialize), in this order: the app's override (EventsOptions.Native);
// ChatGPT's _meta["openai/session"]; a client name (initialize's, or
// _meta's, else Identity.Client) or User-Agent product token of a known
// native client; a client that declares the MCP Events extension.
// conversation is ChatGPT's conversation, when it names one.
func (at *Attachment) nativeOf(ctx context.Context, req *mcp.CallToolRequest, identified string) (native bool, conversation string) {
	var meta map[string]any
	if req.Params != nil {
		meta = req.Params.GetMeta()
	}
	conversation, _ = meta["openai/session"].(string)
	if f := at.events.Native; f != nil && f(ctx, req) {
		return true, conversation
	}
	if conversation != "" {
		return true, conversation
	}
	var ua string
	client := clientName(req, identified)
	if req.Extra != nil && req.Extra.Header != nil {
		ua = req.Extra.Header.Get("User-Agent")
	}
	if event.DialectOf(client, ua) == event.DialectChatGPT {
		return true, ""
	}
	if caps := req.ClientCapabilities(); caps != nil {
		if _, ok := caps.Extensions[event.ExtensionKey]; ok {
			return true, ""
		}
	}
	if metaCaps, _ := meta["io.modelcontextprotocol/clientCapabilities"].(map[string]any); metaCaps != nil {
		ext, _ := metaCaps["extensions"].(map[string]any)
		if _, ok := ext[event.ExtensionKey]; ok {
			return true, ""
		}
	}
	return false, ""
}

// scopeKey is the context key of a tools/call's scope.
type scopeKey struct{}

// callScope is one tools/call as Attach's middleware sees it: what its
// wrapped handler subscribed, for the block and the rollback.
type callScope struct {
	at  *Attachment
	req *mcp.CallToolRequest

	wrapped    atomic.Bool
	call       *ToolCall
	wakeOutput bool

	busy  atomic.Int32 // requests to Wakeshift in flight
	mu    sync.Mutex   // held for each request: one at a time per call
	ended bool

	identity Identity
	args     WakeArgs
	pending  string // the call's pending-addition record
	armed    bool   // a subscribe made or renewed a subscription
	created  []createdSub
	native   []nativeLine
}

// createdSub is a subscription the call made: ended if the call fails.
type createdSub struct {
	name      string
	arguments json.RawMessage
	session   string
	pending   string // the call's pending-addition record, which made it
}

// nativeLine is an event and arguments a native caller is told to
// subscribe to (/calls/addition's native).
type nativeLine struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// maxNativeLines is the most native lines Wakeshift takes for one call.
const maxNativeLines = 20

func (s *callScope) begin(c *ToolCall, id Identity, args WakeArgs, wakeOutput bool) {
	s.call, s.identity, s.args, s.wakeOutput = c, id, args, wakeOutput
	s.wrapped.Store(true)
}

// end closes the call to further requests, after waiting for any in flight
// (warning once per tool: the handler should have waited for them).
func (s *callScope) end() {
	if s.busy.Load() > 0 {
		s.at.warnOnce("inflight:"+s.call.Tool, "appkit: a wrapped tool returned while a subscribe was in flight; it was waited for: wait for Subscribe, Unsubscribe and ResolveSession before returning",
			"tool", s.call.Tool)
	}
	s.mu.Lock()
	s.ended = true
	s.mu.Unlock()
}

// isWrapped reports whether the call's tool was wrapped by Events.
func (s *callScope) isWrapped() bool { return s != nil && s.wrapped.Load() }

// scopeFrom is the tools/call's scope in ctx, if Attach's middleware made
// one.
func scopeFrom(ctx context.Context) *callScope {
	s, _ := ctx.Value(scopeKey{}).(*callScope)
	return s
}

// ToolCallFrom is the wrapped tool's call that ctx belongs to (Events), for
// receiving middleware the server added before Attach, which runs inside
// Attach's: nil for a call of a tool Events didn't wrap, or until its
// handler starts. Once the handler has returned, its Session is the
// conversation's session if it subscribed or resolved one.
func ToolCallFrom(ctx context.Context) *ToolCall {
	if s := scopeFrom(ctx); s.isWrapped() {
		return s.call
	}
	return nil
}

// enter starts a request of the call to Wakeshift; leave ends it.
func (c *ToolCall) enter() (*callScope, error) {
	if c == nil || c.s == nil {
		return nil, ErrNoCall
	}
	s := c.s
	s.busy.Add(1)
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		s.busy.Add(-1)
		return nil, ErrCallEnded
	}
	return s, nil
}

func (s *callScope) leave() {
	s.mu.Unlock()
	s.busy.Add(-1)
}

// wire is the call as Wakeshift takes it: the request's client, _meta and
// User-Agent, the agent's harness and conversation_id arguments, the
// conversation's session, and who's calling (the user, or the agent's
// token).
func (c *ToolCall) wire() wireCall {
	call := callFrom(c.Request, "", c.Session).withArgs(c.s.args)
	return c.s.identity.apply(call)
}

// ResolveSession finds or starts the calling conversation's session without
// subscribing it to anything (it doesn't make the conversation wakeable),
// and returns it.
func (c *ToolCall) ResolveSession(ctx context.Context) (string, error) {
	s, err := c.enter()
	if err != nil {
		return "", err
	}
	defer s.leave()
	if s.pending != "" && c.Session != "" {
		return c.Session, nil
	}
	var out struct {
		Session string     `json:"session"`
		Pending string     `json:"pending"`
		Agent   Recognised `json:"agent"`
	}
	if err := s.at.app.eventsCall(ctx, "POST", "/api/apps/v1/calls/session", map[string]any{"call": c.wire()}, &out); err != nil {
		return "", err
	}
	s.pending, c.Session = out.Pending, out.Session
	rec := out.Agent
	c.Recognised = &rec
	return out.Session, nil
}

// Subscribe subscribes the conversation calling a wrapped tool (call:
// the handler's) to this event with args, or renews its subscription. The
// tool's reply then carries what the conversation needs to be woken (one
// block for the call, however many subscribes it makes), and if the call
// fails, the subscription is ended. For a native caller (call.Native)
// nothing is made: the block tells it to subscribe to the event itself.
// Your tool decides who may: Authorize isn't consulted. A personal event
// needs a user (Identify's). It's the TypeScript SDK's call.subscribe.
func (e *DefinedEvent[A, P]) Subscribe(ctx context.Context, call *ToolCall, args A, opts ...SubscribeOption) (Subscription, error) {
	var o subscribeOptions
	for _, f := range opts {
		f(&o)
	}
	s, err := call.enter()
	if err != nil {
		return Subscription{}, err
	}
	defer s.leave()
	if s.at.app != e.app {
		return Subscription{}, otherAppsEvent(e.t.name)
	}
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return Subscription{}, fmt.Errorf("appkit: %s: encoding the arguments: %w", e.t.name, err)
	}
	known := e.t.knownJSON(argsJSON)
	wire := call.wire()
	if call.Native {
		s.addNative(e.t.name, known)
		return Subscription{User: wire.User, Native: true}, nil
	}
	if e.t.personal && wire.User == "" {
		return Subscription{}, badArguments("%s is personal: Subscribe needs a user (AttachOptions.Identify's), who the subscription belongs to", e.t.name)
	}
	body := map[string]any{"call": wire, "name": e.t.name, "arguments": known, "defer_addition": true}
	if s.pending != "" {
		body["pending"] = s.pending
	}
	if o.ttl > 0 {
		body["ttl"] = o.ttl.Seconds()
	}
	if o.reason != "" {
		body["reason"] = o.reason
	}
	if o.label != "" {
		body["label"] = o.label
	}
	var out struct {
		EventSubscription struct {
			ID      string `json:"id"`
			User    string `json:"user"`
			Created bool   `json:"created"`
		} `json:"event_subscription"`
		Session string     `json:"session"`
		Agent   Recognised `json:"agent"`
		Pending string     `json:"pending"`
	}
	if err := e.app.eventsCall(ctx, "POST", "/api/apps/v1/event-subscriptions", body, &out); err != nil {
		return Subscription{}, err
	}
	if out.Pending != "" {
		s.pending = out.Pending
	}
	s.armed = true
	call.Session = out.Session
	rec := out.Agent
	call.Recognised = &rec
	es := out.EventSubscription
	if es.Created {
		s.created = append(s.created, createdSub{name: e.t.name, arguments: known, session: out.Session, pending: out.Pending})
	}
	user := es.User
	if user == "" {
		user = call.User
	}
	return Subscription{ID: es.ID, User: user, Created: es.Created, Session: out.Session, Recognised: rec}, nil
}

// otherAppsEvent is Subscribe's and Unsubscribe's refusal of an event
// defined on another App: Wakeshift's unknown_event, since this app
// doesn't offer it.
func otherAppsEvent(name string) error {
	return refuse(http.StatusNotFound, "unknown_event", "%s is another app's event: this app doesn't offer it", name)
}

// Unsubscribe ends the calling conversation's subscription to this
// event with args, reporting whether one ended. For a native caller it ends
// the user's own (webhook) subscriptions to the event with those arguments.
func (e *DefinedEvent[A, P]) Unsubscribe(ctx context.Context, call *ToolCall, args A) (bool, error) {
	s, err := call.enter()
	if err != nil {
		return false, err
	}
	defer s.leave()
	if s.at.app != e.app {
		return false, otherAppsEvent(e.t.name)
	}
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return false, fmt.Errorf("appkit: %s: encoding the arguments: %w", e.t.name, err)
	}
	known := e.t.knownJSON(argsJSON)
	wire := call.wire()
	if call.Native {
		return e.app.endNative(ctx, wire.User, e.t.name, known)
	}
	var out struct {
		Ended bool `json:"ended"`
	}
	err = e.app.eventsCall(ctx, "POST", "/api/apps/v1/event-subscriptions/unsubscribe",
		map[string]any{"call": wire, "name": e.t.name, "arguments": known}, &out)
	return out.Ended, err
}

// endNative ends user's webhook subscriptions to event with these
// arguments.
func (a *App) endNative(ctx context.Context, user, name string, args json.RawMessage) (bool, error) {
	if user == "" {
		return false, nil
	}
	subs, err := a.EventSubscriptions(ctx, user)
	if err != nil {
		return false, err
	}
	ended := false
	for _, sub := range subs {
		if sub.Event != name || sub.Destination != "webhook" || !sameJSON(sub.Arguments, args) {
			continue
		}
		ok, err := a.EndEventSubscription(ctx, user, sub.ID)
		if err != nil {
			return ended, err
		}
		ended = ended || ok
	}
	return ended, nil
}

// sameJSON reports whether two JSON values are equal ({} and null alike).
func sameJSON(a, b json.RawMessage) bool {
	var x, y any
	json.Unmarshal(a, &x)
	json.Unmarshal(b, &y)
	if x == nil {
		x = map[string]any{}
	}
	if y == nil {
		y = map[string]any{}
	}
	return reflect.DeepEqual(x, y)
}

// addNative adds a native line, once per event and arguments.
func (s *callScope) addNative(name string, args json.RawMessage) {
	args = json.RawMessage(compact(args))
	for _, l := range s.native {
		if l.Name == name && string(l.Arguments) == string(args) {
			return
		}
	}
	if len(s.native) == maxNativeLines {
		s.at.warnOnce("native:"+s.call.Tool, "appkit: a wrapped tool subscribed a native client to more than 20 events in one call; only 20 are named", "tool", s.call.Tool)
		return
	}
	s.native = append(s.native, nativeLine{Name: name, Arguments: args})
}

// warnOnce logs msg the first time key is seen.
func (at *Attachment) warnOnce(key, msg string, attrs ...any) {
	if _, seen := at.warned.LoadOrStore(key, true); !seen {
		slog.Warn(msg, attrs...)
	}
}
