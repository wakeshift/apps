package appkit

// Attach's receiving middleware: for each tools/call it makes the call's
// scope (callScope), which a wrapped tool's handler (Events) fills; once the
// MCP SDK has finished (the output validated, structuredContent and the
// JSON text fallback filled), it decides from the final result whether the
// call failed, and either rolls back the subscriptions the call made or adds
// the call's one block (docs/subscribe-from-tools.md, "Wrapper behaviour").

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// beaconLinePrefix starts the line a text-carried beacon is on (the core's
// beacon.Text), which a harness's hook reads only as the last line of a
// result's text.
const beaconLinePrefix = "(For Wakeshift's hook on this computer, not for you: wakeshift-beacon: "

// rollbackTimeout bounds a rollback's requests, which outlive the call.
const rollbackTimeout = 15 * time.Second

func (at *Attachment) middleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (res mcp.Result, err error) {
		call, ok := req.(*mcp.CallToolRequest)
		if !ok || method != "tools/call" || call.Params == nil {
			return next(ctx, method, req)
		}
		// Before the MCP SDK validates them (undeclared.go).
		at.dropUndeclared(ctx, next, call)
		s := &callScope{at: at, req: call}
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
		if err != nil || out == nil || out.IsError || ctx.Err() != nil {
			at.rollback(ctx, s)
			return res, err
		}
		if berr := at.addBlock(ctx, s, out); berr != nil {
			// The conversation is subscribed but wouldn't learn how it's
			// woken: the call fails, as one whose handler failed does.
			at.rollback(ctx, s)
			slog.Warn("appkit: Wakeshift's block for a tool result failed, so the call's subscriptions were rolled back", "tool", s.call.Tool, "err", berr)
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{
				Text: "Wakeshift couldn't finish setting up this conversation's wakes (" + detailOf(berr) + "). Try again."}}}, nil
		}
		return res, err
	}
}

// rollback ends every subscription the call created and no other call has
// renewed since, with reason rolled_back (audited as such, no "ended" notice), even if the request was
// cancelled.
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

// addBlock adds the call's one block to out: what the conversation needs to
// be woken (from the call's pending-addition record, if a subscribe or
// ResolveSession made one; otherwise what any other tool result of the
// app's carries), and a line per event a native caller should subscribe
// to. A call that names no conversation, and subscribed nothing, gets
// nothing. Only a failure for a call that subscribed is returned: without
// a subscription, the block is a courtesy.
func (at *Attachment) addBlock(ctx context.Context, s *callScope, out *mcp.CallToolResult) error {
	wire := s.call.wire()
	if s.pending == "" && len(s.native) == 0 && wire.Session == "" && wire.Conversation == "" && conversationKey(wire.Meta) == "" {
		return nil
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
			return err
		}
		slog.Warn("appkit: Wakeshift's addition to a tool result", "tool", s.call.Tool, "err", err)
		return nil
	}
	text := strings.TrimSpace(add.Text)
	mergeBlock(out, text, add.Trailer, add.Meta)
	if text == "" {
		return nil
	}
	if s.wakeOutput {
		setWakeshift(out, text)
	} else if out.StructuredContent != nil {
		at.warnOnce("output:"+s.call.Tool, "appkit: a wrapped tool returns structured content without appkit.WakeOutput: hosts that show only structuredContent won't see how this conversation is woken",
			"tool", s.call.Tool)
	}
	return nil
}

// mergeBlock adds Wakeshift's text to res as a block after the app's
// content, then a text-carried beacon as the last line, and _meta. A result
// that already carries a beacon (a tool or older middleware added one)
// keeps it, last, and gets no second one.
func mergeBlock(res *mcp.CallToolResult, text, trailer string, meta map[string]any) {
	_, metaBeacon := res.Meta[ConnectorMetaKey]
	existing := takeTrailer(res)
	if text != "" {
		res.Content = append(res.Content, &mcp.TextContent{Text: text})
	}
	has := metaBeacon || existing != ""
	switch {
	case existing != "":
		res.Content = append(res.Content, &mcp.TextContent{Text: existing})
	case !has && trailer != "":
		res.Content = append(res.Content, &mcp.TextContent{Text: trailer})
	}
	for k, v := range meta {
		if k == ConnectorMetaKey && has {
			continue
		}
		if res.Meta == nil {
			res.Meta = mcp.Meta{}
		}
		res.Meta[k] = v
	}
}

// takeTrailer takes a beacon line off the end of res's text (dropping its
// block if nothing else is in it), returning it ("" if there's none).
func takeTrailer(res *mcp.CallToolResult) string {
	if len(res.Content) == 0 {
		return ""
	}
	tc, ok := res.Content[len(res.Content)-1].(*mcp.TextContent)
	if !ok {
		return ""
	}
	text := strings.TrimRight(tc.Text, " \t\r\n")
	rest, line := "", text
	if i := strings.LastIndexByte(text, '\n'); i >= 0 {
		rest, line = text[:i], text[i+1:]
	}
	if !strings.HasPrefix(line, beaconLinePrefix) || !strings.HasSuffix(line, ")") {
		return ""
	}
	if rest = strings.TrimRight(rest, " \t\r\n"); rest == "" {
		res.Content = res.Content[:len(res.Content)-1]
	} else {
		tc.Text = rest
	}
	return line
}

// setWakeshift sets a structured result's wakeshift field (WakeOutput).
func setWakeshift(res *mcp.CallToolResult, text string) {
	var b []byte
	switch v := res.StructuredContent.(type) {
	case nil:
		return
	case json.RawMessage:
		b = v
	default:
		var err error
		if b, err = json.Marshal(v); err != nil {
			return
		}
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(b, &obj) != nil || obj == nil {
		return
	}
	t, _ := json.Marshal(text)
	obj["wakeshift"] = t
	if nb, err := json.Marshal(obj); err == nil {
		res.StructuredContent = json.RawMessage(nb)
	}
}
