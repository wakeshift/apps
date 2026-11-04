// Package mcpgo attaches Wakeshift to an MCP server built with
// github.com/mark3labs/mcp-go, as appkit's Attach and Events do for the
// official Go SDK (github.com/modelcontextprotocol/go-sdk).
//
// It's its own package so an app on the official SDK never builds mcp-go,
// and the calls take mcp-go's own types: its *server.MCPServer, its
// mcp.Tool and mcp.CallToolRequest. Everything else is appkit's: define
// events with appkit.DefineEvent, subscribe from a wrapped tool with
// event.Subscribe, emit with event.Emit.
//
//	wake, err := appkit.Open(ctx, os.Getenv("WAKESHIFT_KEY"), "")
//	ciFinished, err := appkit.DefineEvent(wake, "ci.finished", appkit.EventDef[repoArgs, ciRun]{…})
//
//	srv := server.NewMCPServer("ci", "1.0.0")
//	attached := mcpgo.Attach(wake, srv, mcpgo.AttachOptions{
//		Identify: func(_ context.Context, ti *auth.TokenInfo) (appkit.Identity, error) {
//			if ti == nil {
//				return appkit.Identity{}, nil
//			}
//			return appkit.Identity{User: ti.UserID}, nil
//		},
//	})
//	srv.AddTools(mcpgo.Events(attached, mcp.NewTool("watch_ci", mcp.WithDescription("Wake me when CI finishes")),
//		func(ctx context.Context, call *appkit.ToolCall, _ mcp.CallToolRequest, in watchIn) (*mcp.CallToolResult, error) {
//			if _, err := ciFinished.Subscribe(ctx, call, repoArgs{Owner: in.Owner, Repo: in.Repo}); err != nil {
//				return nil, err
//			}
//			return mcp.NewToolResultText("Watching " + in.Owner + "/" + in.Repo + "."), nil
//		}))
//	mux.Handle("/mcp", appkit.RequireBearer(verify, metadataURL)(attached.Wrap(server.NewStreamableHTTPServer(srv))))
package mcpgo

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/modelcontextprotocol/go-sdk/auth"

	"github.com/wakeshift/wakeshift-go/appkit"
	"github.com/wakeshift/wakeshift-go/appkit/internal/adapt"
)

// appkitSide is the calls appkit makes for its adapters (appkit's
// adapters.go): the scope, rollback and block appkit.Events gives a
// go-sdk tool, for an mcp-go one.
type appkitSide interface {
	Attach(a *appkit.App, identify func(context.Context, *auth.TokenInfo) appkit.Identity, native func(context.Context, adapt.Request) bool, tools appkit.Tools) (*appkit.Attachment, []adapt.Tool)
	ArgsOf(in appkit.WakeInput) appkit.WakeArgs
	WakeArgsProperties() map[string]any
	Begin(ctx context.Context, at *appkit.Attachment, r adapt.Request, args appkit.WakeArgs) (context.Context, *appkit.ToolCall)
	End(c *appkit.ToolCall)
	Finish(ctx context.Context, c *appkit.ToolCall, failed bool) (blk adapt.Block, has bool, failure string)
	WarnStructured(c *appkit.ToolCall)
	Declare(at *appkit.Attachment, name string, opts []appkit.EventsOption)
	Describe(ctx context.Context, at *appkit.Attachment, name, description string) string
}

// side is appkit's. The two packages are always built together, from one
// module, so a mismatch is a bug here, which every test of this package
// catches at init: it can't reach an app.
var side = func() appkitSide {
	s, ok := adapt.Impl.(appkitSide)
	if !ok {
		panic("appkit/mcpgo: appkit's adapter side doesn't match this package's (pkg/appkit/adapters.go)")
	}
	return s
}()

// AttachOptions configure Attach: appkit.AttachOptions, for mcp-go.
type AttachOptions struct {
	// Identify tells who's behind a request. ti is the bearer token
	// appkit.RequireBearer (or go-sdk's auth.RequireBearerToken) verified in
	// front of the endpoint, nil without one. Unlike appkit.Attach's, it's
	// called for every request, ti or not, so an app that authenticates its
	// own way reads its own values from ctx: put that authentication in
	// HTTP middleware in front of Wrap, so Wrap's requests carry it too. An
	// error counts as no user. Nil: no request has a user.
	Identify func(ctx context.Context, ti *auth.TokenInfo) (appkit.Identity, error)
	// Tools picks which of Attach's own tools it adds, as appkit's
	// AttachOptions.Tools does: the zero value adds subscribe_events,
	// unsubscribe_events and list_subscriptions, and not the wait tool.
	Tools appkit.Tools
	// Events configures the event features.
	Events EventsOptions
}

// EventsOptions configure Attach's event features.
type EventsOptions struct {
	// Native, if set, is asked first whether a wrapped tool's call comes
	// from a client that subscribes to the app's events itself
	// (ToolCall.Native), for callers the SDK can't tell. False leaves it to
	// the SDK's own checks.
	Native func(ctx context.Context, req mcp.CallToolRequest) bool
}

// Attachment is Wakeshift attached to an mcp-go server (Attach). Its Wrap
// (appkit.Attachment's) puts Wakeshift in front of the endpoint's handler.
type Attachment struct {
	*appkit.Attachment
}

// Attach attaches Wakeshift to an mcp-go server, as appkit's Attach does
// to a go-sdk one: it adds subscribe_events, unsubscribe_events and
// list_subscriptions to srv (AttachOptions.Tools picks), and returns the
// attachment, whose Wrap serves MCP Events, and Wakeshift's wait tool for
// clients that take MCP tasks, in front of the endpoint's handler
// (server.NewStreamableHTTPServer(srv)). A call's arguments that its
// tool's input schema doesn't declare are dropped before mcp-go checks
// them, for every tool, as appkit's Attach does.
//
// Call it once per server, after the last appkit.DefineEvent (with none
// defined it panics), and add wrapped tools (Events) after it.
func Attach(app *appkit.App, srv *server.MCPServer, opts AttachOptions) *Attachment {
	identify := func(ctx context.Context, ti *auth.TokenInfo) appkit.Identity {
		if opts.Identify == nil {
			return appkit.Identity{}
		}
		id, err := opts.Identify(ctx, ti)
		if err != nil {
			return appkit.Identity{}
		}
		return id
	}
	var native func(context.Context, adapt.Request) bool
	if f := opts.Events.Native; f != nil {
		native = func(ctx context.Context, r adapt.Request) bool {
			req, ok := r.Lib.(mcp.CallToolRequest)
			return ok && f(ctx, req)
		}
	}
	at, tools := side.Attach(app, identify, native, opts.Tools)
	for _, t := range tools {
		srv.AddTools(builtinTool(t))
	}
	hooks := srv.GetHooks()
	if hooks == nil {
		hooks = &server.Hooks{}
		server.WithHooks(hooks)(srv)
	}
	hooks.AddBeforeCallTool(func(_ context.Context, _ any, req *mcp.CallToolRequest) { dropUndeclared(srv, req) })
	// tools/list: each tool that declares events (Events' appkit.Subscribes)
	// says it's how the app notifies the agent, and a Notifies or
	// subscribes sentence names the app as the console has it.
	hooks.AddAfterListTools(func(ctx context.Context, _ any, _ *mcp.ListToolsRequest, res *mcp.ListToolsResult) {
		if res == nil {
			return
		}
		for i := range res.Tools {
			res.Tools[i].Description = side.Describe(ctx, at, res.Tools[i].Name, res.Tools[i].Description)
		}
	})
	return &Attachment{Attachment: at}
}

// Events wraps a tool for srv.AddTools, as appkit.Events does for go-sdk:
// its handler gets a *appkit.ToolCall, which event.Subscribe takes to
// subscribe the calling conversation, and Wakeshift adds one block to
// whatever the handler returns: what the agent needs to be woken. A call
// that fails (an error, an isError result, a panic, a canceled request)
// ends the subscriptions it made, and its reply gets nothing added.
//
// In must embed appkit.WakeArgs; the handler gets the call's arguments
// bound to it, and mcp-go's request (ToolCall.Request is go-sdk's, so
// it's nil here). Events adds WakeArgs' three arguments to tool's input
// schema, and an optional wakeshift string to an object output schema,
// which a structured result carries the block's text in. A nil result with
// no error replies with the block alone.
//
// Middleware added with srv.Use runs outside the wrapper: it sees the
// result with the block, whose beacon line must stay last, so don't add
// content after it there.
//
// appkit.Subscribes declares the events the handler subscribes to, as for
// appkit.Events: tools/list then says the tool is how the app notifies the
// agent of them, and a Subscribe to an event the tool doesn't declare warns
// (from the next release, it returns an error).
//
//	srv.AddTools(mcpgo.Events(at, mcp.NewTool("watch_build", mcp.WithDescription("Watch a build until it finishes.")), watchBuild, appkit.Subscribes(buildFinished)))
func Events[In appkit.WakeInput](at *Attachment, tool mcp.Tool, h func(ctx context.Context, call *appkit.ToolCall, req mcp.CallToolRequest, in In) (*mcp.CallToolResult, error), opts ...appkit.EventsOption) server.ServerTool {
	if at == nil || h == nil {
		panic("mcpgo.Events: needs the Attachment Attach returned, and a handler")
	}
	side.Declare(at.Attachment, tool.Name, opts)
	return server.ServerTool{Tool: wakeSchemas(tool), Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in In
		if err := req.BindArguments(&in); err != nil {
			return mcp.NewToolResultError("failed to bind arguments: " + err.Error()), nil
		}
		return at.run(ctx, req, side.ArgsOf(in), func(ctx context.Context, call *appkit.ToolCall) (*mcp.CallToolResult, error) {
			return h(ctx, call, req, in)
		})
	}}
}

// run runs a wrapped tool's handler in its call's scope, then rolls the
// call back or adds its block.
func (at *Attachment) run(ctx context.Context, req mcp.CallToolRequest, args appkit.WakeArgs, h func(context.Context, *appkit.ToolCall) (*mcp.CallToolResult, error)) (res *mcp.CallToolResult, err error) {
	ctx, call := side.Begin(ctx, at.Attachment, requestOf(ctx, req), args)
	ended := false
	end := func() {
		if !ended {
			ended = true
			side.End(call)
		}
	}
	defer func() {
		if r := recover(); r != nil {
			end()
			side.Finish(ctx, call, true)
			panic(r)
		}
	}()
	res, err = h(ctx, call)
	end()
	if res == nil && err == nil {
		res = &mcp.CallToolResult{Content: []mcp.Content{}}
	}
	// A result that asks the client for more input isn't final: the
	// handler runs again with the answers, and subscribes again.
	failed := err != nil || res.IsError || ctx.Err() != nil || res.ResultType == mcp.ResultTypeInputRequired
	blk, has, failure := side.Finish(ctx, call, failed)
	switch {
	case failed:
		return res, err
	case failure != "":
		return mcp.NewToolResultError(failure), nil
	case has:
		mergeBlock(res, blk)
		if blk.Text != "" && !setWakeshift(res, blk.Text) {
			side.WarnStructured(call)
		}
	default:
		// Nothing to add, but a beacon line ending the tool's text is still dropped.
		dropTrailers(res)
	}
	return res, nil
}

// setWakeshift sets a structured result's wakeshift field to the block's
// text, reporting false if it has structuredContent that isn't an object
// (true for none).
func setWakeshift(res *mcp.CallToolResult, text string) bool {
	var src any = res.StructuredContent
	if len(res.RawStructuredContent) > 0 {
		src = res.RawStructuredContent // what mcp-go sends, when set
	}
	if src == nil {
		return true
	}
	out, ok := adapt.SetWakeshift(src, text)
	if ok {
		res.StructuredContent, res.RawStructuredContent = out, out
	}
	return ok
}

// requestOf is an mcp-go tool call's request in adapt's form: the client's
// name and capabilities from its _meta (protocol 2026-07-28), else its
// session's initialize, and the bearer token RequireBearer verified.
func requestOf(ctx context.Context, req mcp.CallToolRequest) adapt.Request {
	r := adapt.Request{Tool: req.Params.Name, Header: req.Header, Token: auth.TokenInfoFromContext(ctx), Lib: req}
	if m := req.Params.Meta; m != nil {
		r.Meta = map[string]any{}
		for k, v := range m.AdditionalFields {
			r.Meta[k] = v
		}
		if m.ProgressToken != nil {
			r.Meta["progressToken"] = m.ProgressToken
		}
		if info := m.ClientInfo(); info != nil {
			r.Client = info.Name
		}
		if caps := m.ClientCapabilities(); caps != nil {
			r.Extensions = caps.Extensions
		}
	}
	if s, ok := server.ClientSessionFromContext(ctx).(server.SessionWithClientInfo); ok {
		if r.Client == "" {
			r.Client = s.GetClientInfo().Name
		}
		if r.Extensions == nil {
			r.Extensions = s.GetClientCapabilities().Extensions
		}
	}
	return r
}

// builtinTool is one of Attach's own tools for srv.AddTools.
func builtinTool(t adapt.Tool) server.ServerTool {
	schema, _ := json.Marshal(t.InputSchema)
	a := t.Annotations
	tool := mcp.Tool{Name: t.Name, Description: t.Description, RawInputSchema: schema,
		Annotations: mcp.ToolAnnotation{Title: a.Title, ReadOnlyHint: &a.ReadOnly, DestructiveHint: &a.Destructive,
			IdempotentHint: &a.Idempotent, OpenWorldHint: &a.OpenWorld}}
	return server.ServerTool{Tool: tool, Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		out := t.Run(ctx, requestOf(ctx, req), rawArguments(req))
		res := &mcp.CallToolResult{Content: []mcp.Content{}, IsError: out.IsError}
		if out.Text != "" {
			res.Content = append(res.Content, mcp.NewTextContent(out.Text))
		}
		if len(out.Meta) > 0 {
			res.Meta = &mcp.Meta{AdditionalFields: map[string]any{}}
			for k, v := range out.Meta {
				res.Meta.AdditionalFields[k] = v
			}
		}
		return res, nil
	}}
}

// rawArguments is a call's arguments as JSON.
func rawArguments(req mcp.CallToolRequest) json.RawMessage {
	if len(req.Params.RawArguments) > 0 {
		return req.Params.RawArguments
	}
	if req.Params.Arguments == nil {
		return nil
	}
	b, _ := json.Marshal(req.Params.Arguments)
	return b
}

// wakeSchemas is tool with WakeArgs' three arguments in its input schema
// (replacing any of the same names, so they're described as every SDK
// describes them), and an optional wakeshift string in an object output
// schema.
func wakeSchemas(tool mcp.Tool) mcp.Tool {
	in := schemaOf(tool.RawInputSchema, tool.InputSchema)
	if in == nil {
		in = map[string]any{}
	}
	in["type"] = "object"
	props, _ := in["properties"].(map[string]any)
	if props == nil {
		props = map[string]any{}
	}
	for name, p := range side.WakeArgsProperties() {
		props[name] = p
	}
	in["properties"] = props
	tool.RawInputSchema, _ = json.Marshal(in)
	tool.InputSchema = mcp.ToolInputSchema{}
	var out map[string]any
	if len(tool.RawOutputSchema) > 0 || tool.OutputSchema.Type != "" {
		out = schemaOf(tool.RawOutputSchema, tool.OutputSchema)
	}
	if amended, changed := adapt.WithWakeshiftField(out); changed {
		tool.RawOutputSchema, _ = json.Marshal(amended)
		tool.OutputSchema = mcp.ToolOutputSchema{}
	}
	return tool
}

// schemaOf is a tool's schema as a map: raw's if it has one, else typed's.
func schemaOf(raw json.RawMessage, typed any) map[string]any {
	if len(raw) > 0 {
		m, _ := adapt.SchemaMap(raw)
		return m
	}
	m, _ := adapt.SchemaMap(typed)
	return m
}

// mergeBlock adds Wakeshift's block to res after the tool's content: its
// text, then a text-carried beacon as the last line, and _meta. Beacon
// lines ending the tool's own text are dropped (dropTrailers). A result
// whose own _meta carries a connector beacon gets no second one, in _meta
// or its text.
func mergeBlock(res *mcp.CallToolResult, blk adapt.Block) {
	metaBeacon := false
	if res.Meta != nil {
		_, metaBeacon = res.Meta.AdditionalFields[appkit.ConnectorMetaKey]
	}
	dropTrailers(res)
	if blk.Text != "" {
		res.Content = append(res.Content, mcp.NewTextContent(blk.Text))
	}
	if !metaBeacon && blk.Trailer != "" {
		res.Content = append(res.Content, mcp.NewTextContent(blk.Trailer))
	}
	for k, v := range blk.Meta {
		if k == appkit.ConnectorMetaKey && metaBeacon {
			continue
		}
		if res.Meta == nil {
			res.Meta = &mcp.Meta{}
		}
		if res.Meta.AdditionalFields == nil {
			res.Meta.AdditionalFields = map[string]any{}
		}
		res.Meta.AdditionalFields[k] = v
	}
}

// dropTrailers takes every beacon line off the end of res's last text
// (dropping its block if nothing else is in it). Only Wakeshift's own
// trailer may end a wrapped tool's text: a hook acts on the beacon there,
// and one in the tool's text may be someone else's, relayed (a chat
// message carrying a beacon minted for another session).
func dropTrailers(res *mcp.CallToolResult) {
	for {
		i := len(res.Content) - 1
		var text string
		for ; i >= 0; i-- {
			if c, ok := res.Content[i].(mcp.TextContent); ok {
				text = c.Text
				break
			}
			if c, ok := res.Content[i].(*mcp.TextContent); ok {
				text = c.Text
				break
			}
		}
		if i < 0 {
			return
		}
		rest, line := adapt.SplitTrailer(text)
		if line == "" {
			return
		}
		if rest == "" {
			res.Content = append(res.Content[:i:i], res.Content[i+1:]...)
			continue
		}
		switch c := res.Content[i].(type) {
		case mcp.TextContent:
			c.Text = rest
			res.Content[i] = c
		case *mcp.TextContent:
			c.Text = rest
		}
	}
}

// dropUndeclared removes from a call's arguments every property its tool's
// input schema closes off (additionalProperties false), before mcp-go
// checks them against it (server.WithInputSchemaValidation), as the
// TypeScript and Python SDKs ignore them.
func dropUndeclared(srv *server.MCPServer, req *mcp.CallToolRequest) {
	raw := []byte(strings.TrimSpace(string(rawArguments(*req))))
	if len(raw) < 3 || raw[0] != '{' {
		return
	}
	st := srv.GetTool(req.Params.Name)
	if st == nil {
		return
	}
	schema := schemaOf(st.Tool.RawInputSchema, st.Tool.InputSchema)
	if schema == nil {
		return
	}
	var args any
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.UseNumber()
	if d.Decode(&args) != nil || !adapt.Strip(schema, args) {
		return
	}
	b, err := json.Marshal(args)
	if err != nil {
		return
	}
	var plain any
	if json.Unmarshal(b, &plain) != nil {
		return
	}
	req.Params.RawArguments, req.Params.Arguments = b, plain
}
