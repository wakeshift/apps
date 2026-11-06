package wakeshift

// Attaching Wakeshift to an MCP server (design §2.3). Attach wraps the
// server at the protocol layer, never a vendor callback: at the HTTP layer
// (Handler: the wake arguments stripped before the MCP library parses the
// body, events/*, the wait tool, the POST as the unit of identity) and in
// go-sdk's receiving middleware (tools/list amended, each call of a tool
// that subscribes bound to its scope, and the fallback strip for stdio).

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"weak"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/wakeshift-go/internal/adapt"
)

// AttachOptions configure Attach (and mcpgo.Attach, and Client.Handler).
// The zero value is the default: no user, anonymous callers allowed, the
// built-in tools subscribe_events, list_subscriptions and
// unsubscribe_events on, and the wait tool off.
type AttachOptions struct {
	// Identify tells who is behind a request, from the endpoint's own
	// authentication. It runs lazily, at most once per HTTP request (the
	// POST), with that POST's own ctx (carrying your middleware's values)
	// and its request: r.Token is the bearer token RequireBearer (or
	// go-sdk's auth middleware) verified, nil without one. It runs only
	// when Wakeshift needs to know: a call of a tool that subscribes, a
	// built-in tool, events/*, the wait tool. An error, a panic or a run
	// over 10 seconds refuses Wakeshift's part of the request (Subscribe
	// returns CodeIdentifyFailed); your tool's own code still runs. A
	// returned Identity.Agent must come from a verified credential.
	Identify func(ctx context.Context, r Request) (Identity, error)
	// RequireUser refuses Wakeshift's part of a request whose Identify
	// gives no user, the same way (Subscribe returns CodeUserRequired), so
	// a session_key is never a bearer for its conversation. Set it
	// whenever your server requires sign-in.
	RequireUser bool
	// Wrap names tools registered elsewhere (mcp.AddTool, srv.AddTool,
	// a generated catalog) that subscribe, with the events each subscribes
	// to: Wrap: map[string][]wakeshift.ToolOption{"start_crawl":
	// {wakeshift.Subscribes(crawlFinished)}}. Find the call from inside the
	// handler with ToolCallFrom(ctx).
	Wrap map[string][]ToolOption
	// WrapFunc is Wrap as a function, for tools you know only by name at
	// run time: it's asked on each tools/list and tools/call about a tool
	// AddTool and Wrap don't declare (never a built-in), and returns the
	// tool's options as Wrap's map would, or nil for a tool that doesn't
	// subscribe. Its answer isn't kept: keep it fast. A nil event or
	// another client's in its options logs wrap_bad_event, and the tool is
	// served unwrapped.
	WrapFunc func(tool string) []ToolOption
	// BuiltinTools picks Wakeshift's own tools.
	BuiltinTools BuiltinTools
	// IsNativeClient, for house agents only, says that a caller the SDK
	// can't recognize subscribes to events itself (MCP Events). False
	// leaves it to the SDK's own checks.
	IsNativeClient func(ctx context.Context, r Request) bool
}

// BuiltinTools picks Wakeshift's own tools. The zero value is the default:
// subscribe_events (listed only when an event is open to it: Personal,
// Authorize or Public), list_subscriptions and unsubscribe_events, and not
// the wait tool.
type BuiltinTools struct {
	// Wait turns on wakeshift_wait, Claude Code's wake path with nothing
	// installed, on protocol 2026-07-28 (MCP tasks). While a conversation
	// waits, its client holds a request to your endpoint for about 20
	// seconds at a time: give the endpoint a write timeout of at least 30
	// seconds.
	Wait bool
	// DisableSubscribe, DisableList and DisableUnsubscribe turn off
	// subscribe_events, list_subscriptions and unsubscribe_events.
	DisableSubscribe, DisableList, DisableUnsubscribe bool
	// DisableAll turns off every built-in tool, the wait tool included.
	DisableAll bool
}

// enabled is which built-in tools besides the wait tool are on.
func (b BuiltinTools) enabled() map[string]bool {
	return map[string]bool{subscribeEventsTool: !b.DisableSubscribe && !b.DisableAll, listSubscriptionsTool: !b.DisableList && !b.DisableAll,
		unsubscribeEventsTool: !b.DisableUnsubscribe && !b.DisableAll}
}

// settings are what serving a request needs of AttachOptions, shared by an
// Attachment and Client.Handler.
type settings struct {
	app         *Client
	identify    func(context.Context, Request) (Identity, error)
	requireUser bool
	isNative    func(context.Context, Request) bool
	wait        bool
	builtins    map[string]bool
	// eventsKept: the server declares MCP Events itself, so Wakeshift serves
	// none (events_capability_kept).
	eventsKept atomic.Bool
}

func (c *Client) settingsOf(opts AttachOptions) *settings {
	return &settings{app: c, identify: opts.Identify, requireUser: opts.RequireUser, isNative: opts.IsNativeClient,
		wait: opts.BuiltinTools.Wait && !opts.BuiltinTools.DisableAll, builtins: opts.BuiltinTools.enabled()}
}

// Attachment is Wakeshift attached to one MCP server (Attach).
type Attachment struct {
	app *Client
	srv *mcp.Server // nil for an adapter's (mcpgo)
	set *settings

	tools []adapt.Tool // the built-ins, for an adapter to add; by name in builtinByName
	// builtinTools are the built-in tools as Attach registered them, by
	// name: tools/list replaces any other tool of the same name with these.
	builtinTools map[string]*mcp.Tool

	declMu sync.Mutex
	decls  map[string]*declaration // the tools that subscribe, by name (AddTool, Wrap)
	wrap   map[string]*declaration // Wrap's, by name
	// unwrapped are Wrap's tools whose schema names a wake argument: left
	// as they are (wrap_arg_conflict).
	unwrapped map[string]bool
	wrapFunc  func(string) []ToolOption // AttachOptions.WrapFunc

	// stdinArgs are Wakeshift's arguments Stdin took, until their calls'
	// tools take them; adapterArgs, those an adapter took before its
	// library's middleware ran (Stash), by session.
	stdinArgs, adapterArgs pendingArgs

	schemas sync.Map // *mcp.Tool → its input schema, decoded
}

// attachedServers is every server attached, so a second Attach is caught.
var attachedServers sync.Map // weak.Pointer[T] → true

// markAttached records p as attached, reporting false if it already was.
func markAttached[T any](p *T) bool {
	w := weak.Make(p)
	if _, dup := attachedServers.LoadOrStore(w, true); dup {
		return false
	}
	runtime.AddCleanup(p, func(w weak.Pointer[T]) { attachedServers.Delete(w) }, w)
	return true
}

// Attach attaches Wakeshift to an official Go SDK server: the one call
// that connects the two. Call it once per server, after the last
// DefineEvent, then register the tools that subscribe with AddTool, and
// serve the server's HTTP handler through Handler (on stdio, its stdin
// through Stdin), so no middleware of yours sees Wakeshift's arguments:
//
//	attached := wake.Attach(srv, wakeshift.AttachOptions{Identify: identify, RequireUser: true})
//	wakeshift.AddTool(attached, tool, handler, wakeshift.Subscribes(crawlFinished))
//	http.Handle("/mcp", attached.Handler(mcp.NewStreamableHTTPHandler(…)))
//
// Without Handler or Stdin, receiving middleware added after Attach runs
// first and sees them.
//
// It adds Wakeshift's built-in tools (BuiltinTools), Wakeshift's
// arguments to each tool that subscribes in tools/list (stripped before
// your code sees them), and MCP Events for clients that speak it (unless
// the server already serves its own). On a disabled client it adds
// nothing, and Handler returns its handler as it is. A second Attach on the
// same server panics.
func (c *Client) Attach(srv *mcp.Server, opts AttachOptions) *Attachment {
	if c == nil || srv == nil {
		panic("wakeshift: Attach needs the client New returned and a server")
	}
	if !markAttached(srv) {
		c.configPanic("wakeshift: this server is attached already: attach each server once")
	}
	at := c.newAttachment(opts, "wakeshift.Attach")
	at.srv = srv
	if !c.Enabled() {
		return at
	}
	at.builtinTools = map[string]*mcp.Tool{}
	for _, t := range at.tools {
		tool, h := goSDKTool(t)
		at.builtinTools[t.Name] = tool
		srv.AddTool(tool, h)
	}
	srv.AddReceivingMiddleware(at.middleware)
	return at
}

// newAttachment is the Attachment for opts, without an MCP library: its
// settings, Wrap's declarations and the built-in tools. It logs what
// attaching without events or identify means, once per client.
func (c *Client) newAttachment(opts AttachOptions, what string) *Attachment {
	at := &Attachment{app: c, set: c.settingsOf(opts), decls: map[string]*declaration{}, wrap: map[string]*declaration{}, unwrapped: map[string]bool{},
		wrapFunc: opts.WrapFunc}
	for name, topts := range opts.Wrap {
		d := at.declarationOf(what+" Wrap["+name+"]", topts)
		if d == nil {
			d = &declaration{events: []*eventType{}}
		}
		at.wrap[name] = d
		at.decls[name] = d
	}
	if !c.Enabled() {
		return at
	}
	if len(c.events()) == 0 {
		c.logOnce(context.Background(), "no_events", "", "attach with no events defined: there's nothing to subscribe to")
	}
	if opts.Identify == nil {
		c.logOnce(context.Background(), "no_identify", "", "attach without identify: a session_key is a bearer for its conversation")
	}
	at.tools = c.builtinTools(at.set)
	// A process that serves the app's tools sends the manifest
	// automatically (manifest.go).
	c.mu.Lock()
	c.attached = true
	c.mu.Unlock()
	return at
}

// middleware is Attach's receiving middleware on a go-sdk server.
func (at *Attachment) middleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		switch method {
		case "tools/list":
			res, err := next(ctx, method, req)
			if list, ok := res.(*mcp.ListToolsResult); ok && err == nil {
				lr, _ := req.(*mcp.ListToolsRequest)
				complete := list.NextCursor == "" && (lr == nil || lr.Params == nil || lr.Params.Cursor == "")
				at.amendList(ctx, list, complete)
			}
			return res, err
		case "tools/call":
			if call, ok := req.(*mcp.CallToolRequest); ok && call.Params != nil {
				return at.serveCall(ctx, next, call)
			}
		}
		return next(ctx, method, req)
	}
}

// serveCall serves one tools/call on a go-sdk server: a built-in tool, a
// tool that subscribes (in its scope, then its block or rollback), or any
// other tool with Wakeshift's arguments kept from it.
func (at *Attachment) serveCall(ctx context.Context, next mcp.MethodHandler, call *mcp.CallToolRequest) (res mcp.Result, err error) {
	r := requestOf(call)
	post := at.app.postOf(r.Header)
	if post == nil && r.Header != nil {
		at.app.logOnce(ctx, "handler_missing", "", "a tool call came over HTTP to a handler not wrapped with Handler, so Wakeshift's arguments were stripped late and identify ran with the session's context")
	}
	var wake wakeArgs
	if post != nil {
		wake = post.take(r.Tool, call.Params.Arguments)
	} else if w, ok := at.stdinArgs.take(callKey(r.Tool, call.Params.Arguments)); ok {
		wake = w
	} else {
		var stripped []byte
		if wake, stripped = stripWakeArgs(call.Params.Arguments); stripped != nil {
			call.Params.Arguments = stripped
		}
	}
	if t, ok := at.builtin(r.Tool); ok {
		return goSDKResult(t.Run(ctx, r, call.Params.Arguments, wake)), nil
	}
	decl := at.declared(r.Tool)
	if decl == nil {
		// Not a tool that subscribes: the arguments are its own, unless its
		// own schema names one of Wakeshift's (it's yours, then).
		if wake.Present {
			if schema := at.inputSchema(ctx, next, call); schema != nil {
				if b := restoreWakeArgs(wake, schema, call.Params.Arguments); b != nil {
					call.Params.Arguments = b
				}
			}
		}
		at.dropUndeclared(ctx, next, call)
		return next(ctx, "tools/call", call)
	}
	at.dropUndeclared(ctx, next, call)
	s := at.newScope(ctx, r, wake, decl, post)
	ctx = context.WithValue(ctx, scopeKey{}, s)
	defer func() {
		if p := recover(); p != nil {
			s.end()
			at.rollback(ctx, s)
			panic(p)
		}
	}()
	res, err = next(ctx, "tools/call", call)
	s.end()
	out, _ := res.(*mcp.CallToolResult)
	// A result that asks the client for input (go-sdk's multi-round trip)
	// isn't final: the client calls again with the answers, and the
	// handler subscribes again then.
	failed := err != nil || out == nil || out.IsError || ctx.Err() != nil || out.NeedsInput() || len(out.InputRequests) > 0
	blk, has := at.finish(ctx, s, failed)
	switch {
	case failed && out != nil && s.id.refused != "" && refusalShown(out):
		// The handler passed on call.Subscribe's identify refusal: the
		// agent reads neutral words, never the developer's fix (the log
		// has that, identify_error).
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: s.id.refusedText()}}}, nil
	case failed:
		return res, err
	case has:
		at.applyBlock(s, out, blk)
	default:
		// Nothing to add, but a beacon line ending the app's text is still dropped.
		dropTrailers(out)
	}
	return res, err
}

// hiddenBuiltins is the built-in tools tools/list leaves out, by name:
// list_subscriptions and unsubscribe_events, when nothing on this server
// can make a subscription: no tool declared with Subscribes naming an
// event (AddTool or Wrap; for WrapFunc, none of the listed tools), and no
// event open to subscribe_events. They still answer a call.
func (at *Attachment) hiddenBuiltins(listed []string) map[string]bool {
	if len(at.app.openEvents()) > 0 {
		return nil
	}
	at.declMu.Lock()
	some := false
	for _, d := range at.decls {
		some = some || d.subscribes()
	}
	at.declMu.Unlock()
	if some {
		return nil
	}
	if at.wrapFunc != nil {
		for _, name := range listed {
			if d, _ := at.lookup(name); d != nil && d.subscribes() {
				return nil
			}
		}
	}
	return map[string]bool{listSubscriptionsTool: true, unsubscribeEventsTool: true}
}

// builtin is the built-in tool named name, if it's on.
func (at *Attachment) builtin(name string) (adapt.Tool, bool) {
	for _, t := range at.tools {
		if t.Name == name {
			return t, true
		}
	}
	return adapt.Tool{}, false
}

// declared is the declaration of the tool named name, if it subscribes
// (AddTool, or Wrap or WrapFunc without a conflict).
func (at *Attachment) declared(name string) *declaration {
	at.declMu.Lock()
	unwrapped := at.unwrapped[name]
	at.declMu.Unlock()
	if unwrapped {
		return nil
	}
	d, _ := at.lookup(name)
	return d
}

// lookup is the declaration of the tool named name (nil if it doesn't
// subscribe), and whether Wrap or WrapFunc declares it: AddTool's or
// Wrap's, else WrapFunc's answer, asked now (never for a built-in). A
// mistake in WrapFunc's options (a nil event, or another client's) is
// found while serving, so it doesn't panic as Wrap's does at Attach: it's
// logged once per tool (wrap_bad_event), and the tool is served unwrapped.
func (at *Attachment) lookup(name string) (d *declaration, wrapped bool) {
	at.declMu.Lock()
	d, wrapped = at.decls[name], at.wrap[name] != nil
	at.declMu.Unlock()
	if d != nil || at.wrapFunc == nil || isBuiltinName(name) {
		return d, wrapped
	}
	opts := at.wrapFunc(name)
	if opts == nil {
		return nil, false
	}
	d, bad := at.checkDeclaration(opts)
	if bad != "" {
		at.app.logOnce(context.Background(), "wrap_bad_event", name, fmt.Sprintf("WrapFunc's options for %s are wrong (%s), so it's served unwrapped", name, bad),
			strAttr("tool", name))
		return nil, false
	}
	if d == nil {
		d = &declaration{events: []*eventType{}}
	}
	return d, true
}

// logOnce logs code once per client, for this attachment's client.
func (at *Attachment) logOnce(ctx context.Context, code, key, what string, attrs ...slog.Attr) {
	at.app.logOnce(ctx, code, key, what, attrs...)
}

// showsRefusal reports whether text carries the identify refusal's own
// message: CodeIdentifyFailed's or CodeUserRequired's fix.
func showsRefusal(text string) bool {
	return strings.Contains(text, fixes[CodeIdentifyFailed]) || strings.Contains(text, fixes[CodeUserRequired])
}

// refusalShown reports whether an error result shows the identify
// refusal's own message (CodeIdentifyFailed's or CodeUserRequired's fix):
// a handler that passed call.Subscribe's error on.
func refusalShown(out *mcp.CallToolResult) bool {
	for _, c := range out.Content {
		if t, ok := c.(*mcp.TextContent); ok && showsRefusal(t.Text) {
			return true
		}
	}
	return false
}
