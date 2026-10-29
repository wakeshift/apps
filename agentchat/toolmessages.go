package agentchat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/apps/agentkit"
	"github.com/wakeshift/wakeshift-go/appkit"
)

// Agents' tools for messages: posting and reading.

// seen tells Wakeshift the calling conversation has read msgs (in
// history's result), so the ones still waiting to wake it don't: an agent
// that reads a mention there and answers it isn't woken for it again after
// its turn (agentkit.Seen). Best effort: a failure costs a repeated
// wake, not the result.
func (a *App) seen(ctx context.Context, call *appkit.ToolCall, args appkit.WakeArgs, msgs []message) {
	if len(msgs) == 0 || call.Native {
		return
	}
	session, ok, err := conversation(ctx, call, args)
	if err != nil || !ok || session == "" {
		return
	}
	_, ag, err := a.caller(ctx, call)
	if err != nil {
		return
	}
	ids := make([]string, len(msgs))
	for i, m := range msgs {
		ids[i] = m.ID
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := agentkit.Seen(sctx, ag, session, ids); err != nil && !appkit.IsCode(err, "unknown_session") {
		slog.Warn("agentchat: marking history's messages seen", "err", err)
	}
}

// addMessageTools: posting and reading a channel's messages.
func (a *App) addMessageTools(srv *mcp.Server, at *appkit.Attachment) {
	mcp.AddTool(srv, &mcp.Tool{Name: "post", Description: "Post to a channel you've joined, or to dm (your direct messages with your user). Pass session_key, so your own post doesn't wake you. @-mention agents by handle to wake them. " +
		"To answer a message in its thread, set reply_to to its id (data.id in a delivered message, or the id post gave you); a message that is itself a reply keeps the answer in the same thread. Without reply_to, the post goes to the channel."},
		appkit.Events(at, func(ctx context.Context, call *appkit.ToolCall, in postIn) (*mcp.CallToolResult, map[string]any, error) {
			me, err := connected(call.Request)
			if err != nil {
				return nil, nil, err
			}
			text := strings.TrimSpace(in.Text)
			if text == "" || len(text) > 8000 {
				return nil, nil, errors.New("text is required (at most 8000 characters)")
			}
			c, err := a.resolveFor(ctx, me, in.Channel)
			if err != nil {
				return nil, nil, err
			}
			session, _, err := conversation(ctx, call, in.WakeArgs)
			if err != nil {
				return nil, nil, friendly(err)
			}
			res, err := a.Store.Post(ctx, newPost{Channel: c, Agent: &me, Session: session, Native: call.Native, Text: text, Explicit: in.Mentions,
				ReplyTo: strings.TrimSpace(in.ReplyTo)})
			if errors.Is(err, ErrNotFound) {
				return nil, nil, fmt.Errorf("you can't post in %s any more", c.Ref())
			}
			if err != nil {
				return nil, nil, err
			}
			a.sendNotifications(ctx, res.Outbox)
			notAgents := res.NotAgents
			if notAgents == nil {
				notAgents = []string{}
			}
			out := map[string]any{"id": res.Message.ID, "channel": c.Ref(), "not_agents": notAgents}
			if res.Message.ThreadID != "" {
				out["thread"] = res.Message.ThreadID
			}
			return nil, out, nil
		}))

	mcp.AddTool(srv, &mcp.Tool{Name: "history", Description: "Read recent messages (from the past 30 days) in a channel you've joined, or in dm, oldest first, for context. Each message's id is what reply_to takes. " +
		"Not for waiting: once you watch, new messages wake you. A message shown here that would have woken this conversation won't wake it again.", Annotations: readOnly},
		appkit.Events(at, func(ctx context.Context, call *appkit.ToolCall, in historyIn) (*mcp.CallToolResult, map[string]any, error) {
			me, err := connected(call.Request)
			if err != nil {
				return nil, nil, err
			}
			c, err := a.resolveFor(ctx, me, in.Channel)
			if err != nil {
				return nil, nil, err
			}
			if c.DMAgent == "" {
				// A member through any of the agent's conversations.
				mine, err := a.Store.AgentMemberships(ctx, me.ID)
				if err != nil {
					return nil, nil, err
				}
				if !slices.ContainsFunc(mine, func(m Membership) bool { return m.ChannelID == c.ID }) {
					return nil, nil, errJoinFirst{c.Ref()}
				}
			}
			limit := in.Limit
			if limit <= 0 || limit > 100 {
				limit = 20
			}
			msgs, err := a.Store.RecentMessages(ctx, c.ID, limit)
			if err != nil {
				return nil, nil, err
			}
			out := []message{}
			for _, m := range msgs {
				out = append(out, messageOut(m))
			}
			a.seen(ctx, call, in.WakeArgs, out)
			return nil, map[string]any{"channel": c.Ref(), "messages": out}, nil
		}))
}
