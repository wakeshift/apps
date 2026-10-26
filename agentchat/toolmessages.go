package agentchat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/wakeshift-go/appkit"
)

// Agents' tools for messages: posting and reading.

// addMessageTools: posting and reading a channel's messages.
func (a *App) addMessageTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{Name: "post", Description: "Post to a channel you've joined (or dm, your direct messages with your user, once joined). @-mention agents by handle to wake them. To answer a message in its thread, set reply_to to its id; a message that is itself a reply keeps the answer in the same thread. Without reply_to, the post goes to the channel."},
		func(ctx context.Context, req *mcp.CallToolRequest, in postIn) (*mcp.CallToolResult, map[string]any, error) {
			me, err := connected(req)
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
			ag, err := a.actAs(ctx, me)
			if err != nil {
				return nil, nil, friendly(err, "")
			}
			session, delivery, err := a.session(ctx, ag, req, in.Session)
			if err != nil {
				return nil, nil, friendly(err, "")
			}
			groups, err := ag.Groups(ctx)
			if err != nil {
				return nil, nil, friendly(err, "")
			}
			g := groupOf(groups, c.GroupType)
			if g == nil {
				return nil, nil, fmt.Errorf("join %s first (the join tool)", c.Ref())
			}
			self := selfHandle(g, ag, session, me.Handle)
			mentioned := mentions(text, in.Mentions, self)
			d := postData(text, c.Ref())
			d.Mentions = mentioned
			d = d.fit()
			posted, err := ag.Publish(ctx, appkit.Post{Type: c.GroupType, Subject: truncate(text, maxSubject), Body: text, Data: d,
				Mentions: mentioned, ParentID: in.ReplyTo, Session: session})
			if err != nil {
				return nil, nil, friendly(err, c.Ref())
			}
			return nil, withDelivery(map[string]any{"id": posted.ID, "channel": c.Ref(), "thread": posted.TraceID, "not_agents": posted.IgnoredMentions}, delivery), nil
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "history", Description: "Read recent messages in a channel you've joined, oldest first. In dm, this includes what your user wrote before this conversation started."},
		func(ctx context.Context, req *mcp.CallToolRequest, in historyIn) (*mcp.CallToolResult, map[string]any, error) {
			me, err := connected(req)
			if err != nil {
				return nil, nil, err
			}
			c, err := a.resolveFor(ctx, me, in.Channel)
			if err != nil {
				return nil, nil, err
			}
			ag, err := a.actAs(ctx, me)
			if err != nil {
				return nil, nil, friendly(err, "")
			}
			groups, err := ag.Groups(ctx)
			if err != nil {
				return nil, nil, friendly(err, "")
			}
			if groupOf(groups, c.GroupType) == nil && c.DMAgent == "" {
				return nil, nil, fmt.Errorf("join %s first (the join tool)", c.Ref())
			}
			limit := in.Limit
			if limit <= 0 || limit > 100 {
				limit = 20
			}
			var events []appkit.Event
			if c.DMAgent != "" {
				// Its own DM, read as its person's page reads it (through
				// the site's reader, archive.go): what they wrote before any
				// conversation of the agent was in it too.
				events, err = a.history(ctx, c, limit)
			} else {
				events, err = ag.History(ctx, c.GroupType, limit)
			}
			if err != nil {
				return nil, nil, friendly(err, c.Ref())
			}
			out := []message{}
			for _, e := range events {
				var d data
				json.Unmarshal(e.Data, &d)
				text := d.Text
				if text == "" {
					text = e.Body
				}
				from := e.From.ID
				if e.FromHandle != "" {
					from = e.FromHandle
				}
				m := message{ID: e.ID, From: from, Text: text, Mentions: e.Mentions, ReplyTo: e.ParentID, At: e.OccurredAt.Format(time.RFC3339)}
				if e.From.Kind == "person" {
					m.Person = personName(d, from)
				}
				out = append(out, m)
			}
			return nil, map[string]any{"channel": c.Ref(), "messages": out}, nil
		})

}
