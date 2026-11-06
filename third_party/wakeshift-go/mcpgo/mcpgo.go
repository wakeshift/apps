// Package mcpgo attaches Wakeshift to an MCP server built with
// github.com/mark3labs/mcp-go, as wakeshift.Attach and wakeshift.AddTool do
// for the official Go SDK. Everything else is wakeshift's: define events
// with wakeshift.DefineEvent, subscribe with event.Subscribe, emit with
// event.Emit.
//
//	wake, err := wakeshift.New()
//	ciFinished, err := wakeshift.DefineEvent(wake, "ci.finished", wakeshift.EventDef[repoArgs, ciRun]{…})
//
//	srv := server.NewMCPServer("ci", "1.0.0")
//	attached := mcpgo.Attach(wake, srv, wakeshift.AttachOptions{})
//	mcpgo.AddTool(attached, mcp.NewTool("watch_ci", mcp.WithDescription("Watch CI on a repo."), mcp.WithString("repo", mcp.Required())),
//		func(ctx context.Context, call *wakeshift.ToolCall, _ mcp.CallToolRequest, in watchIn) (*mcp.CallToolResult, error) {
//			if _, err := ciFinished.Subscribe(ctx, call, repoArgs{Repo: in.Repo}); err != nil {
//				return nil, err
//			}
//			return mcp.NewToolResultText("Watching " + in.Repo + "."), nil
//		}, wakeshift.Subscribes(ciFinished))
//	mux.Handle("/mcp", attached.Handler(server.NewStreamableHTTPServer(srv)))
//
// Handler strips Wakeshift's arguments before mcp-go parses the request,
// and on stdio ServeStdio (or attached.Stdin) strips them before mcp-go
// reads each line, so no hook or middleware of yours sees them. Without
// either (an in-process transport), they're stripped in a BeforeCallTool
// hook Attach adds, before every tool handler middleware: only hooks that
// run earlier (BeforeAny, OnRequestInitialization, and BeforeCallTool hooks
// added before Attach) see them.
package mcpgo

import (
	"context"
	"encoding/json"
	"os"
	"os/signal"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"weak"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/modelcontextprotocol/go-sdk/auth"

	"github.com/wakeshift/wakeshift-go"
	"github.com/wakeshift/wakeshift-go/internal/adapt"
)

// Attachment is Wakeshift attached to an mcp-go server (Attach). Its
// Handler (wakeshift.Attachment's) puts Wakeshift in front of the
// endpoint's handler.
type Attachment struct {
	*wakeshift.Attachment
	srv *server.MCPServer
	at  any      // wakeshift's side of it (adapt.Side)
	own sync.Map // the names AddTool added: their handlers run the call themselves
}

// attached is every server attached, so a second Attach is caught.
var attached sync.Map

// Attach attaches Wakeshift to an mcp-go server, as wakeshift.Attach does to
// a go-sdk one: Wakeshift's built-in tools (opts.BuiltinTools), Wakeshift's
// arguments on each tool that subscribes in tools/list (stripped before
// your code sees them), and, through Handler, MCP Events and the wait tool.
// Call it once per server, after the last wakeshift.DefineEvent, then add
// the tools that subscribe with AddTool. On a disabled client it adds no
// tools and changes no answer: it installs one passthrough tool middleware,
// which gives a tool named in Wrap its disabled call. A second Attach on
// the same server panics.
func Attach(wake *wakeshift.Client, srv *server.MCPServer, opts wakeshift.AttachOptions) *Attachment {
	if wake == nil || srv == nil {
		panic("mcpgo.Attach: needs the client wakeshift.New returned and a server")
	}
	w := weak.Make(srv)
	if _, dup := attached.LoadOrStore(w, true); dup {
		adapt.Impl.ConfigPanic(wake, "mcpgo.Attach: this server is attached already: attach each server once")
	}
	runtime.AddCleanup(srv, func(w weak.Pointer[server.MCPServer]) { attached.Delete(w) }, w)
	side := adapt.Impl
	at, builtins := side.Attach(wake, srv, opts)
	a := &Attachment{Attachment: at.(*wakeshift.Attachment), srv: srv, at: at}
	if !side.Enabled(at) {
		// A tool named in Wrap gets a disabled call, as AddTool's do.
		srv.Use(func(next server.ToolHandlerFunc) server.ToolHandlerFunc {
			return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				if _, own := a.own.Load(req.Params.Name); !own && side.Subscribes(at, req.Params.Name) {
					ctx, _ = side.DisabledCall(ctx, at, req.Params.Name)
				}
				return next(ctx, req)
			}
		})
		return a
	}
	isBuiltin := map[string]bool{}
	for _, t := range builtins {
		isBuiltin[t.Name] = true
		srv.AddTools(a.builtinTool(t))
	}
	hooks := srv.GetHooks()
	if hooks == nil {
		hooks = &server.Hooks{}
		server.WithHooks(hooks)(srv)
	}
	// Before mcp-go checks a call's arguments, and before every tool
	// handler middleware: Wakeshift's are taken out of every call (Handler
	// or Stdin took them already, if one is in front). A tool that
	// subscribes, or a built-in, gets them from its call (Stash); any other
	// keeps them only if its own schema names them.
	hooks.AddBeforeCallTool(func(ctx context.Context, _ any, req *mcp.CallToolRequest) {
		name := req.Params.Name
		r := requestOf(ctx, *req)
		if isBuiltin[name] || side.Subscribes(at, name) {
			w, stripped, held := side.Hold(at, r, rawArguments(*req))
			if stripped != nil {
				setArguments(req, stripped)
			}
			dropUndeclared(srv, req)
			if held {
				side.Stash(at, r, rawArguments(*req), w)
			}
			return
		}
		if w, stripped := side.Take(at, r, rawArguments(*req)); stripped != nil || w.Present {
			if stripped != nil {
				setArguments(req, stripped)
			}
			restore(srv, req, w)
		}
		dropUndeclared(srv, req)
	})
	// A tool registered elsewhere and named in Wrap (or WrapFunc) runs in
	// its call, as AddTool's do: ToolCallFrom(ctx) finds it, and the result
	// gets Wakeshift's block.
	srv.Use(func(next server.ToolHandlerFunc) server.ToolHandlerFunc {
		return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			name := req.Params.Name
			if _, own := a.own.Load(name); own || isBuiltin[name] || !side.Subscribes(at, name) {
				return next(ctx, req)
			}
			r := requestOf(ctx, req)
			w, stripped := side.Take(at, r, rawArguments(req))
			if stripped != nil {
				setArguments(&req, stripped)
			}
			return a.run(ctx, r, w, func(ctx context.Context, _ *wakeshift.ToolCall) (*mcp.CallToolResult, error) {
				return next(ctx, req)
			})
		}
	})
	// tools/list: each tool that subscribes, as Wakeshift gives it.
	hooks.AddAfterListTools(func(ctx context.Context, _ any, _ *mcp.ListToolsRequest, res *mcp.ListToolsResult) {
		if res == nil {
			return
		}
		names := make([]string, len(res.Tools))
		for i, t := range res.Tools {
			names[i] = t.Name
		}
		if hide := side.HiddenBuiltins(at, names); len(hide) > 0 {
			res.Tools = slices.DeleteFunc(res.Tools, func(t mcp.Tool) bool { return hide[t.Name] && isBuiltin[t.Name] })
		}
		for i := range res.Tools {
			t := &res.Tools[i]
			var out map[string]any
			if len(t.RawOutputSchema) > 0 || t.OutputSchema.Type != "" {
				out = schemaOf(t.RawOutputSchema, t.OutputSchema)
			}
			desc, in, outAmended, ok := side.Amend(ctx, at, t.Name, t.Description, schemaOf(t.RawInputSchema, t.InputSchema), out)
			if !ok {
				continue
			}
			t.Description = desc
			t.RawInputSchema, _ = json.Marshal(in)
			t.InputSchema = mcp.ToolInputSchema{}
			if outAmended != nil {
				t.RawOutputSchema, _ = json.Marshal(outAmended)
				t.OutputSchema = mcp.ToolOutputSchema{}
			}
		}
	})
	return a
}

// ServeStdio serves the attachment's server over stdin and stdout, as
// server.ServeStdio does (until SIGTERM or an interrupt), with Wakeshift's
// arguments taken out of each tool call before mcp-go reads it
// (wakeshift.Attachment.Stdin): no hook or middleware of yours sees them.
// To call Listen yourself, give it attached.Stdin(os.Stdin):
//
//	server.NewStdioServer(srv).Listen(ctx, attached.Stdin(os.Stdin), os.Stdout)
func ServeStdio(at *Attachment, opts ...server.StdioOption) error {
	if at == nil {
		panic("mcpgo.ServeStdio: needs the Attachment Attach returned")
	}
	s := server.NewStdioServer(at.srv)
	for _, o := range opts {
		o(s)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer cancel()
	return s.Listen(ctx, at.Stdin(os.Stdin), os.Stdout)
}

// AddTool adds a tool that subscribes to the attachment's server, as
// wakeshift.AddTool does for go-sdk: its handler gets the call, a
// *wakeshift.ToolCall, for event.Subscribe, and mcp-go's request, with the
// call's arguments bound to In (your own type: Wakeshift's arguments never
// reach it). Name the events it subscribes to with wakeshift.Subscribes. A
// call that fails (an error, an isError result, a panic, a canceled
// request) ends the subscriptions it made. A nil result with no error
// replies with Wakeshift's block alone. It panics on a mistake in the
// registration, as wakeshift.AddTool does. On a disabled client it adds the
// tool as written, and the handler gets a disabled call.
func AddTool[In any](at *Attachment, tool mcp.Tool, h func(ctx context.Context, call *wakeshift.ToolCall, req mcp.CallToolRequest, in In) (*mcp.CallToolResult, error), opts ...wakeshift.ToolOption) {
	if at == nil || h == nil {
		panic("mcpgo.AddTool: needs the Attachment Attach returned, and a handler")
	}
	side := adapt.Impl
	var out map[string]any
	if len(tool.RawOutputSchema) > 0 || tool.OutputSchema.Type != "" {
		out = schemaOf(tool.RawOutputSchema, tool.OutputSchema)
	}
	subscribes := side.Declare(at.at, tool.Name, opts, schemaOf(tool.RawInputSchema, tool.InputSchema), out)
	at.own.Store(tool.Name, true)
	if !side.Enabled(at.at) {
		at.srv.AddTools(server.ServerTool{Tool: tool, Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var in In
			if err := req.BindArguments(&in); err != nil {
				return mcp.NewToolResultError("failed to bind arguments: " + err.Error()), nil
			}
			ctx, call := side.DisabledCall(ctx, at.at, tool.Name)
			return h(ctx, call.(*wakeshift.ToolCall), req, in)
		}})
		return
	}
	at.srv.AddTools(server.ServerTool{Tool: wakeSchemas(tool, subscribes), Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		r := requestOf(ctx, req)
		w, stripped := side.Take(at.at, r, rawArguments(req))
		if stripped != nil {
			setArguments(&req, stripped)
		}
		var in In
		if err := req.BindArguments(&in); err != nil {
			return mcp.NewToolResultError("failed to bind arguments: " + err.Error()), nil
		}
		return at.run(ctx, r, w, func(ctx context.Context, call *wakeshift.ToolCall) (*mcp.CallToolResult, error) {
			return h(ctx, call, req, in)
		})
	}})
}

// run runs a tool that subscribes in its call's scope, then rolls the
// call back or adds its block.
func (at *Attachment) run(ctx context.Context, r adapt.Request, w adapt.WakeArgs, h func(context.Context, *wakeshift.ToolCall) (*mcp.CallToolResult, error)) (res *mcp.CallToolResult, err error) {
	side := adapt.Impl
	ctx, c := side.Begin(ctx, at.at, r, w)
	call := c.(*wakeshift.ToolCall)
	ended := false
	end := func() {
		if !ended {
			ended = true
			side.End(call)
		}
	}
	defer func() {
		if p := recover(); p != nil {
			end()
			side.Finish(ctx, call, true)
			panic(p)
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
	blk, has := side.Finish(ctx, call, failed)
	shown := ""
	if err != nil {
		shown = err.Error()
	} else if res.IsError {
		for _, c := range res.Content {
			if t, ok := c.(mcp.TextContent); ok {
				shown += t.Text
			}
		}
	}
	switch {
	case failed && side.Refused(call, shown) != "":
		// The handler passed on call.Subscribe's identify refusal: neutral
		// words for the agent; the log has the developer's fix.
		return mcp.NewToolResultError(side.Refused(call, shown)), nil
	case failed:
		return res, err
	case has:
		mergeBlock(res, blk)
		if blk.Text != "" {
			setWakeshift(res, blk.Text)
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
	if len(r.Header) == 0 {
		// mcp-go gives an empty header on stdio: nil off HTTP, as on go-sdk.
		r.Header = nil
	}
	if s := server.ClientSessionFromContext(ctx); s != nil {
		r.Session = s.SessionID()
	}
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

// builtinTool is one of Wakeshift's own tools for srv.AddTools.
func (at *Attachment) builtinTool(t adapt.Tool) server.ServerTool {
	schema, _ := json.Marshal(t.InputSchema)
	a := t.Annotations
	tool := mcp.Tool{Name: t.Name, Title: a.Title, Description: t.Description, RawInputSchema: schema,
		Annotations: mcp.ToolAnnotation{Title: a.Title, ReadOnlyHint: &a.ReadOnly, DestructiveHint: &a.Destructive,
			IdempotentHint: &a.Idempotent, OpenWorldHint: &a.OpenWorld}}
	if t.OutputSchema != nil {
		tool.RawOutputSchema, _ = json.Marshal(t.OutputSchema)
	}
	return server.ServerTool{Tool: tool, Handler: func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		r := requestOf(ctx, req)
		args := rawArguments(req)
		w, stripped := adapt.Impl.Take(at.at, r, args)
		if stripped != nil {
			args = stripped
		}
		out := t.Run(ctx, r, args, w)
		res := &mcp.CallToolResult{Content: []mcp.Content{}, IsError: out.IsError}
		if out.Text != "" {
			res.Content = append(res.Content, mcp.NewTextContent(out.Text))
		}
		if out.Structured != nil && !out.IsError {
			res.StructuredContent = out.Structured
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

// setArguments sets a call's arguments to raw JSON.
func setArguments(req *mcp.CallToolRequest, b []byte) {
	var plain any
	if json.Unmarshal(b, &plain) != nil {
		return
	}
	req.Params.RawArguments, req.Params.Arguments = b, plain
}

// restore puts back the values of Wakeshift's arguments that a tool's own
// input schema names: they're the tool's own, then.
func restore(srv *server.MCPServer, req *mcp.CallToolRequest, w adapt.WakeArgs) {
	st := srv.GetTool(req.Params.Name)
	if st == nil {
		return
	}
	schema := schemaOf(st.Tool.RawInputSchema, st.Tool.InputSchema)
	props, _ := schema["properties"].(map[string]any)
	var obj map[string]any
	if json.Unmarshal(rawArguments(*req), &obj) != nil || obj == nil {
		obj = map[string]any{}
	}
	changed := false
	for name, v := range map[string]string{"session_key": w.SessionKey, "harness": w.Harness, "conversation_id": w.ConversationID} {
		if _, ok := props[name]; ok && v != "" {
			obj[name], changed = v, true
		}
	}
	if !changed {
		return
	}
	if b, err := json.Marshal(obj); err == nil {
		setArguments(req, b)
	}
}

// wakeSchemas is tool with Wakeshift's arguments in its input schema (all
// three, or session_key alone for a tool that subscribes to nothing), so
// mcp-go's own checks take them until they're stripped, and the wakeshift
// field in an object output schema.
func wakeSchemas(tool mcp.Tool, subscribes bool) mcp.Tool {
	in := schemaOf(tool.RawInputSchema, tool.InputSchema)
	if in == nil {
		in = map[string]any{}
	}
	in["type"] = "object"
	props, _ := in["properties"].(map[string]any)
	if props == nil {
		props = map[string]any{}
	}
	for _, name := range wakeArgNames(subscribes) {
		props[name] = map[string]any{"type": "string"}
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

// wakeArgNames are the arguments tools/list gives a tool that subscribes.
func wakeArgNames(subscribes bool) []string {
	if subscribes {
		return []string{"session_key", "harness", "conversation_id"}
	}
	return []string{"session_key"}
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
		_, metaBeacon = res.Meta.AdditionalFields[connectorMetaKey]
	}
	dropTrailers(res)
	if blk.Text != "" {
		res.Content = append(res.Content, mcp.NewTextContent(blk.Text))
	}
	if !metaBeacon && blk.Trailer != "" {
		res.Content = append(res.Content, mcp.NewTextContent(blk.Trailer))
	}
	for k, v := range blk.Meta {
		if k == connectorMetaKey && metaBeacon {
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

// connectorMetaKey is where a tool result carries a connector beacon.
const connectorMetaKey = "wakeshift/connector"
