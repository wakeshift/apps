package wakeshift

// Attach's receiving middleware: for each tools/call it makes the call's
// scope (callScope), which a wrapped tool's handler (Events) fills; once the
// MCP SDK has finished (the output validated, structuredContent and the
// JSON text fallback filled), it decides from the final result whether the
// call failed, and either rolls back the subscriptions the call made or adds
// the call's one block (docs/subscribe-from-tools.md, "Wrapper behavior").

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/wakeshift-go/internal/adapt"
)

// connectorMetaKey is where a tool result carries a connector beacon.
const connectorMetaKey = "wakeshift/connector"

// rollbackTimeout bounds a rollback's requests, which outlive the call.
const rollbackTimeout = 15 * time.Second

// finish ends a wrapped tool's call once its final result is known: a call
// that failed has its subscriptions rolled back; one that didn't gets the
// call's block (has: there's one to add). A block Wakeshift can't give
// never changes the result: it goes out as the tool gave it, as it would
// without Wakeshift (the tool may have done paid or visible work, so an
// error would have the agent repeat it). The conversation wouldn't learn
// how it's woken, so what the call subscribed is rolled back.
func (at *Attachment) finish(ctx context.Context, s *callScope, failed bool) (blk adapt.Block, has bool) {
	if failed {
		at.rollback(ctx, s)
		return adapt.Block{}, false
	}
	blk, has, err := at.blockFor(ctx, s)
	if err != nil && !s.armed && len(s.created) == 0 && len(s.native) > 0 {
		// Only native lines, so nothing to roll back: the tool's result goes
		// out as it would without Wakeshift (docs/subscribe-vouchers.md §7.2).
		at.app.logOnce(ctx, "native_unavailable", s.call.Tool+"\x00addition_failed",
			fmt.Sprintf("Wakeshift's addition to a result of %s failed, so it carries no line for the MCP Events client", s.call.Tool),
			slog.String("tool", s.call.Tool), slog.String("event", s.native[0].Name), slog.String("reason", "addition_failed"))
		return adapt.Block{}, false
	}
	if err != nil {
		at.rollback(ctx, s)
		at.app.logLimited(ctx, "block_failed", fmt.Sprintf("Wakeshift's addition to a result of %s failed (%s), so the result went out without it and the call's subscriptions were rolled back", s.call.Tool, errorType(err)),
			slog.String("tool", s.call.Tool), slog.String("error_type", errorType(err)))
		return adapt.Block{}, false
	}
	// The call succeeded: each subscription it created is made.
	// OnSubscribe gets the ctx its Subscribe was given (its values), and
	// its Context.
	for _, c := range s.created {
		sctx := c.ctx
		if sctx == nil {
			sctx = ctx
		}
		at.app.onSubscribed(sctx, at.app.eventByName(c.name), c.arguments, subscribedInfo{user: s.call.User, via: viaTool, tool: s.call.Tool, value: c.value})
	}
	return blk, has
}

// rollback ends every subscription the call created and no other call has
// renewed since, with reason rolled_back (audited as such, no "ended" notice), even if the request was
// canceled.
func (at *Attachment) rollback(ctx context.Context, s *callScope) {
	s.stopWatches()
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
			at.app.logLimited(ctx, "rollback_failed", fmt.Sprintf("ending a subscription of a failed call of %s failed (%s)", s.call.Tool, errorType(err)),
				slog.String("tool", s.call.Tool), slog.String("event", c.name))
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
	// A call whose every subscription it made ended in the same call (an
	// Unsubscribe) gets no block: nothing is left to wake it for.
	if s.endedAll() && len(s.native) == 0 {
		if len(s.notes) == 0 {
			return adapt.Block{}, false, nil
		}
		return adapt.Block{Text: strings.Join(s.notes, " ")}, true, nil
	}
	blk, has, err = at.fetchBlock(ctx, s)
	if err != nil || len(s.notes) == 0 {
		return blk, has, err
	}
	blk.Text = strings.TrimSpace(strings.Join(s.notes, " ") + "\n\n" + blk.Text)
	return blk, true, nil
}

// fetchBlock is the call's block from /calls/addition (blockFor).
func (at *Attachment) fetchBlock(ctx context.Context, s *callScope) (blk adapt.Block, has bool, err error) {
	if s.call.Native {
		// The manifest's answer first, so a native call's wire knows whether
		// the core hands out vouchers.
		at.app.vouchersOffered(ctx)
	}
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
		return adapt.Block{}, false, nil
	}
	if len(s.native) > 0 {
		// The vouchers the block hands out keep their Subscribe's Context
		// for the redemption's OnSubscribe (vouchers.go).
		at.app.rememberVouchers(add.Text, s.native)
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
	setWakeshift(out, blk.Text)
}

// mergeBlock adds Wakeshift's text to res as a block after the app's
// content, then a text-carried beacon as the last line, and _meta. Beacon
// lines ending the app's own text are dropped (dropTrailers). A result
// whose own _meta carries a connector beacon gets no second one, in _meta
// or its text.
func mergeBlock(res *mcp.CallToolResult, text, trailer string, meta map[string]any) {
	_, metaBeacon := res.Meta[connectorMetaKey]
	dropTrailers(res)
	if text != "" {
		res.Content = append(res.Content, &mcp.TextContent{Text: text})
	}
	if !metaBeacon && trailer != "" {
		res.Content = append(res.Content, &mcp.TextContent{Text: trailer})
	}
	for k, v := range meta {
		if k == connectorMetaKey && metaBeacon {
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
