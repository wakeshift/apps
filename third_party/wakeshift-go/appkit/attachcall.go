package appkit

// Attach's receiving middleware: for each tools/call it makes the call's
// scope (callScope), which a wrapped tool's handler (Events) fills; once the
// MCP SDK has finished (the output validated, structuredContent and the
// JSON text fallback filled), it decides from the final result whether the
// call failed, and either rolls back the subscriptions the call made or adds
// the call's one block (docs/subscribe-from-tools.md, "Wrapper behavior").

import (
	"context"
	"log/slog"
	"reflect"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/wakeshift-go/appkit/internal/adapt"
)

// beaconLinePrefix starts the line a text-carried beacon is on (the core's
// beacon.Text), which a harness's hook reads only as the last line of a
// result's text.
const beaconLinePrefix = adapt.BeaconLinePrefix

// rollbackTimeout bounds a rollback's requests, which outlive the call.
const rollbackTimeout = 15 * time.Second

func (at *Attachment) middleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (res mcp.Result, err error) {
		if method == "tools/list" {
			res, err = next(ctx, method, req)
			if list, ok := res.(*mcp.ListToolsResult); ok && err == nil {
				lr, _ := req.(*mcp.ListToolsRequest)
				at.describeTools(list, list.NextCursor == "" && (lr == nil || lr.Params == nil || lr.Params.Cursor == ""))
				amendTools(list)
				at.nameApp(ctx, list)
			}
			return res, err
		}
		call, ok := req.(*mcp.CallToolRequest)
		if !ok || method != "tools/call" || call.Params == nil {
			return next(ctx, method, req)
		}
		// Before the MCP SDK validates them (undeclared.go).
		at.dropUndeclared(ctx, next, call)
		s := at.newScope(requestOf(call))
		ctx = context.WithValue(ctx, scopeKey{}, s)
		defer func() {
			if r := recover(); r != nil {
				if s.isWrapped() {
					at.rollback(ctx, s)
				}
				panic(r)
			}
		}()
		res, err = next(ctx, method, req)
		if !s.isWrapped() {
			return res, err
		}
		out, _ := res.(*mcp.CallToolResult)
		// A result that asks the client for input (go-sdk's multi-round
		// trip) isn't final: the client calls again with the answers, and
		// the handler subscribes again then (as mcpgo's adapter does).
		failed := err != nil || out == nil || out.IsError || ctx.Err() != nil || out.NeedsInput() || len(out.InputRequests) > 0
		blk, has, failure := at.finish(ctx, s, failed)
		switch {
		case failed:
			return res, err
		case failure != "":
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: failure}}}, nil
		case has:
			at.applyBlock(s, out, blk)
		default:
			// Nothing to add, but a beacon line ending the app's text is still dropped.
			dropTrailers(out)
		}
		return res, err
	}
}

// finish ends a wrapped tool's call once its final result is known: a call
// that failed has its subscriptions rolled back; one that didn't gets the
// call's block (has: there's one to add). failure, if not "", is the text
// of the error result that replaces the call's: Wakeshift's block failed
// for a call that subscribed, so it was rolled back.
func (at *Attachment) finish(ctx context.Context, s *callScope, failed bool) (blk adapt.Block, has bool, failure string) {
	if failed {
		at.rollback(ctx, s)
		return adapt.Block{}, false, ""
	}
	blk, has, err := at.blockFor(ctx, s)
	if err != nil {
		// The conversation is subscribed but wouldn't learn how it's
		// woken: the call fails, as one whose handler failed does.
		at.rollback(ctx, s)
		slog.Warn("appkit: Wakeshift's block for a tool result failed, so the call's subscriptions were rolled back", "tool", s.call.Tool, "err", err)
		return adapt.Block{}, false, "Wakeshift couldn't finish setting up this conversation's wakes (" + detailOf(err) + "). Try again."
	}
	return blk, has, ""
}

// rollback ends every subscription the call created and no other call has
// renewed since, with reason rolled_back (audited as such, no "ended" notice), even if the request was
// canceled.
func (at *Attachment) rollback(ctx context.Context, s *callScope) {
	if len(s.created) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()
	for _, c := range s.created {
		wire := s.call.wire()
		wire.Session = c.session
		if err := at.app.eventsCall(ctx, "POST", "/api/apps/v1/event-subscriptions/unsubscribe",
			map[string]any{"call": wire, "name": c.name, "arguments": c.arguments, "reason": "rolled_back", "pending": c.pending}, nil); err != nil {
			slog.Warn("appkit: rolling back a failed tool call's subscription", "tool", s.call.Tool, "event", c.name, "err", err)
		}
	}
}

// blockFor is the call's one block: what the conversation needs to be
// woken (from the call's pending-addition record, if a subscribe or
// ResolveSession made one; otherwise what any other tool result of the
// app's carries), and a line per event a native caller should subscribe
// to. A call that names no conversation, and subscribed nothing, gets
// nothing (has false). Only a failure for a call that subscribed is
// returned: without a subscription, the block is a courtesy.
//
// Notes from the call's unsubscribes come first, whatever else it gets.
func (at *Attachment) blockFor(ctx context.Context, s *callScope) (blk adapt.Block, has bool, err error) {
	blk, has, err = at.fetchBlock(ctx, s)
	if err != nil || len(s.notes) == 0 {
		return blk, has, err
	}
	blk.Text = strings.TrimSpace(strings.Join(s.notes, " ") + "\n\n" + blk.Text)
	return blk, true, nil
}

// fetchBlock is the call's block from /calls/addition (blockFor).
func (at *Attachment) fetchBlock(ctx context.Context, s *callScope) (blk adapt.Block, has bool, err error) {
	wire := s.call.wire()
	if s.pending == "" && len(s.native) == 0 && wire.Session == "" && wire.Conversation == "" && conversationKey(wire.Meta) == "" {
		return adapt.Block{}, false, nil
	}
	body := map[string]any{"call": wire}
	if s.pending != "" {
		body["pending"] = s.pending
	}
	if len(s.native) > 0 {
		body["native"] = s.native
	}
	var add struct {
		Text    string         `json:"text"`
		Trailer string         `json:"trailer"`
		Meta    map[string]any `json:"meta"`
	}
	if err := at.app.eventsCall(ctx, "POST", "/api/apps/v1/calls/addition", body, &add); err != nil {
		if s.armed || len(s.native) > 0 {
			return adapt.Block{}, false, err
		}
		slog.Warn("appkit: Wakeshift's addition to a tool result", "tool", s.call.Tool, "err", err)
		return adapt.Block{}, false, nil
	}
	return adapt.Block{Text: strings.TrimSpace(add.Text), Trailer: add.Trailer, Meta: add.Meta}, true, nil
}

// applyBlock adds the call's block to out, and its text to a structured
// result's wakeshift field.
func (at *Attachment) applyBlock(s *callScope, out *mcp.CallToolResult, blk adapt.Block) {
	mergeBlock(out, blk.Text, blk.Trailer, blk.Meta)
	if blk.Text == "" || out.StructuredContent == nil {
		return
	}
	if !setWakeshift(out, blk.Text) {
		at.warnStructured(s)
	}
}

// warnStructured warns, once per tool, that a wrapped tool's structured
// result isn't an object.
func (at *Attachment) warnStructured(s *callScope) {
	at.warnOnce("output:"+s.call.Tool, "appkit: a wrapped tool returns structuredContent that isn't an object, so it can't carry how this conversation is woken: hosts that show only structuredContent won't see it",
		"tool", s.call.Tool)
}

// amendTools gives each wrapped tool's object output schema in a tools/list
// answer an optional wakeshift string (withWakeshiftField), which its
// structured results carry, so a client that checks structuredContent
// against the schema takes it. A wrapped tool is one whose input takes
// WakeArgs (Events requires it: takesWakeArgs), and its structured
// results are the only ones that get the field. The tools are the
// server's own, so each one changed is a copy.
func amendTools(list *mcp.ListToolsResult) {
	for i, t := range list.Tools {
		if t == nil || t.OutputSchema == nil || !takesWakeArgs(t.InputSchema) {
			continue
		}
		schema, ok := schemaMap(t.OutputSchema)
		if !ok {
			continue
		}
		if amended, changed := withWakeshiftField(schema); changed {
			c := *t
			c.OutputSchema = amended
			list.Tools[i] = &c
		}
	}
}

// takesWakeArgs reports whether an input schema has WakeArgs' three
// properties, each with WakeArgs' own description: the input of a tool
// Events wraps (an embedded WakeArgs), not one that happens to use the
// names.
func takesWakeArgs(input any) bool {
	m, ok := schemaMap(input)
	if !ok {
		return false
	}
	props, _ := m["properties"].(map[string]any)
	for name, description := range wakeArgDescriptions {
		p, _ := props[name].(map[string]any)
		if p == nil || p["description"] != description {
			return false
		}
	}
	return true
}

// wakeArgDescriptions is WakeArgs' properties' descriptions, by JSON name.
var wakeArgDescriptions = func() map[string]string {
	out := map[string]string{}
	t := reflect.TypeFor[WakeArgs]()
	for i := range t.NumField() {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		out[name] = f.Tag.Get("jsonschema")
	}
	return out
}()

// schemaMap is a schema (a *jsonschema.Schema, raw JSON or a map) as a
// fresh map.
func schemaMap(schema any) (map[string]any, bool) { return adapt.SchemaMap(schema) }

// withWakeshiftField is an object schema with an optional wakeshift string
// added, and whether it changed (adapt.WithWakeshiftField).
func withWakeshiftField(schema map[string]any) (map[string]any, bool) {
	return adapt.WithWakeshiftField(schema)
}

// mergeBlock adds Wakeshift's text to res as a block after the app's
// content, then a text-carried beacon as the last line, and _meta. Beacon
// lines ending the app's own text are dropped (dropTrailers). A result
// whose own _meta carries a connector beacon gets no second one, in _meta
// or its text.
func mergeBlock(res *mcp.CallToolResult, text, trailer string, meta map[string]any) {
	_, metaBeacon := res.Meta[ConnectorMetaKey]
	dropTrailers(res)
	if text != "" {
		res.Content = append(res.Content, &mcp.TextContent{Text: text})
	}
	if !metaBeacon && trailer != "" {
		res.Content = append(res.Content, &mcp.TextContent{Text: trailer})
	}
	for k, v := range meta {
		if k == ConnectorMetaKey && metaBeacon {
			continue
		}
		if res.Meta == nil {
			res.Meta = mcp.Meta{}
		}
		res.Meta[k] = v
	}
}

// dropTrailers takes every beacon line off the end of res's last text
// (dropping its block if nothing else is in it). Only Wakeshift's own
// trailer may end a wrapped tool's text: a hook acts on the beacon there,
// and one in the app's text may be someone else's, relayed (a chat message
// carrying a beacon minted for another session).
func dropTrailers(res *mcp.CallToolResult) {
	for {
		i := len(res.Content) - 1
		for i >= 0 {
			if _, ok := res.Content[i].(*mcp.TextContent); ok {
				break
			}
			i--
		}
		if i < 0 {
			return
		}
		tc := res.Content[i].(*mcp.TextContent)
		rest, line := adapt.SplitTrailer(tc.Text)
		if line == "" {
			return
		}
		if rest == "" {
			res.Content = append(res.Content[:i:i], res.Content[i+1:]...)
		} else {
			tc.Text = rest
		}
	}
}

// setWakeshift sets a structured result's wakeshift field to the block's
// text, reporting false if structuredContent isn't an object.
func setWakeshift(res *mcp.CallToolResult, text string) bool {
	out, ok := adapt.SetWakeshift(res.StructuredContent, text)
	if ok {
		res.StructuredContent = out
	}
	return ok
}
