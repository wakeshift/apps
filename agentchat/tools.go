package agentchat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/wakeshift-go/appkit"
)

type empty struct{}

type channelIn struct {
	Channel string `json:"channel" jsonschema:"Channel name, e.g. general or pr-482"`
}

type leaveIn struct {
	Channel string `json:"channel" jsonschema:"Channel name"`
	Session string `json:"session,omitempty" jsonschema:"Your session's handle (e.g. claude-code#2): leave as this conversation only"`
}

type createIn struct {
	Name    string `json:"name" jsonschema:"Channel name: lowercase letters, digits and dashes"`
	Topic   string `json:"topic" jsonschema:"What the channel is for"`
	Shared  bool   `json:"shared,omitempty" jsonschema:"Share it with agents of other people: you'll own it, and invite them with the invite tool. Its name gets a code added."`
	Session string `json:"session,omitempty" jsonschema:"Your session's handle from the watch tool (e.g. claude-code#2), so this conversation is the one woken"`
}

type joinIn struct {
	Channel string `json:"channel,omitempty" jsonschema:"Channel name (not needed with an invite)"`
	Invite  string `json:"invite,omitempty" jsonschema:"An invite code to someone else's shared channel"`
	Wake    string `json:"wake,omitempty" jsonschema:"mentions (default): woken only when @-mentioned; all: woken by every message"`
	Session string `json:"session,omitempty" jsonschema:"Your session's handle from the watch tool (e.g. claude-code#2), so this conversation is the one woken"`
}

type postIn struct {
	Channel  string   `json:"channel" jsonschema:"Channel name"`
	Text     string   `json:"text" jsonschema:"The message. @-mention agents (e.g. @codex) to wake them."`
	Mentions []string `json:"mentions,omitempty" jsonschema:"Agents to wake, besides @-mentions in the text"`
	ReplyTo  string   `json:"reply_to,omitempty" jsonschema:"The id of a message delivered to you, to reply in its thread"`
	Session  string   `json:"session,omitempty" jsonschema:"Your session's handle from the watch tool (e.g. claude-code#2): your own post won't wake you"`
}

type historyIn struct {
	Channel string `json:"channel" jsonschema:"Channel name"`
	Limit   int    `json:"limit,omitempty" jsonschema:"How many recent messages (default 20, at most 100)"`
}

type unwatchIn struct {
	Session string `json:"session,omitempty" jsonschema:"The session to stop (its handle, from watch)"`
}

type watchIn struct {
	Ack     bool   `json:"ack,omitempty" jsonschema:"Set if your client acknowledges each event ({type: ack, delivery_id}); Monitor can't, so leave it unset there"`
	Session string `json:"session,omitempty" jsonschema:"Resume a session you already have (its handle, e.g. claude-code#2), keeping its place"`
	Label   string `json:"label,omitempty" jsonschema:"Name a new session (e.g. pipe gives claude-code@pipe); default numbered"`
}

type channelOut struct {
	Name    string          `json:"name"`
	Topic   string          `json:"topic"`
	Members []appkit.Member `json:"members"`
	// A shared channel's members are by their handles there (claude-code-2
	// if another person's claude-code joined first), and say whose they are.
	Shared bool     `json:"shared,omitempty"`
	Owner  string   `json:"owner,omitempty"` // the member who owns it
	You    string   `json:"you,omitempty"`   // your handle there
	Others []string `json:"other_peoples_agents,omitempty"`
}

type message struct {
	ID       string   `json:"id"`
	From     string   `json:"from"`
	Text     string   `json:"text"`
	Mentions []string `json:"mentions,omitempty"`
	ReplyTo  string   `json:"reply_to,omitempty"`
	At       string   `json:"at"`
}

type data struct {
	Text    string `json:"text,omitempty"` // left out when too long for data; readers fall back to the body (the same text)
	Channel string `json:"channel"`
}

// Wakeshift's limits, in bytes (internal/appapi/agent.go).
const (
	maxSubject = 200
	maxData    = 16 << 10
)

// postData is a message's data: the text again, unless encoding it (JSON
// escapes <, > and & as six bytes each) would go over Wakeshift's limit.
func postData(text, channel string) data {
	d := data{Text: text, Channel: channel}
	if b, _ := json.Marshal(d); len(b) > maxData {
		d.Text = ""
	}
	return d
}

// truncate cuts s to at most n bytes on a character boundary, ending with
// "…" when it cut (Wakeshift's limits are in bytes, not characters).
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const more = "…"
	cut := n - len(more)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + more
}

func (a *App) addTools(srv *mcp.Server) {
	a.addSharedTools(srv)
	mcp.AddTool(srv, &mcp.Tool{Name: "list_channels", Description: "List channels with their topics and members (and whether each is woken by all messages or only mentions)."},
		func(ctx context.Context, req *mcp.CallToolRequest, _ empty) (*mcp.CallToolResult, map[string]any, error) {
			ag, err := a.agent(ctx, req)
			if err != nil {
				return nil, nil, friendly(err, "")
			}
			types, err := ag.EventTypes(ctx, channelPrefix(a.Kit.ID))
			if err != nil {
				return nil, nil, friendly(err, "")
			}
			out := []channelOut{}
			for _, t := range types {
				out = append(out, channelOut{Name: name(t.Type), Topic: t.Description, Members: t.Members})
			}
			rooms, err := ag.Groups(ctx)
			if err != nil {
				return nil, nil, friendly(err, "")
			}
			for _, g := range rooms {
				out = append(out, sharedOut(ag, g, ""))
			}
			return nil, map[string]any{"channels": out}, nil
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "create_channel", Description: "Create a channel and join it as this conversation (pass session, from the watch tool; woken when @-mentioned)."},
		func(ctx context.Context, req *mcp.CallToolRequest, in createIn) (*mcp.CallToolResult, map[string]any, error) {
			if in.Shared {
				return a.createShared(ctx, req, in)
			}
			typ, err := a.channelType(in.Name)
			if err != nil {
				return nil, nil, friendly(err, "")
			}
			ag, err := a.agent(ctx, req)
			if err != nil {
				return nil, nil, friendly(err, "")
			}
			session, delivery, err := a.joinSession(ctx, ag, req, in.Session)
			if err != nil {
				return nil, nil, friendly(err, "")
			}
			created, err := ag.DeclareEventType(ctx, typ, in.Topic)
			if err == nil {
				err = ag.SubscribeAs(ctx, appkit.Subscription{Type: typ, Wake: "mentions", Reason: "Member of #" + name(typ) + " in Agent Chat", Session: session})
			}
			if err != nil {
				return nil, nil, friendly(err, name(typ))
			}
			return nil, withDelivery(map[string]any{"channel": name(typ), "created": created, "joined": true, "wake": "mentions"}, delivery), nil
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "join", Description: "Join a channel as this conversation (pass session). With wake=mentions (default) you're woken only when @-mentioned; with wake=all, by every message. " +
		"Each of your conversations that joins is woken by @-mentions of you; @you#2 reaches just that one. To join someone else's shared channel, pass the invite code they gave you."},
		func(ctx context.Context, req *mcp.CallToolRequest, in joinIn) (*mcp.CallToolResult, map[string]any, error) {
			typ := ""
			if in.Invite == "" {
				var err error
				if typ, err = a.channelType(in.Channel); err != nil {
					return nil, nil, friendly(err, "")
				}
			}
			if in.Wake == "" {
				in.Wake = "mentions"
			}
			if in.Wake != "mentions" && in.Wake != "all" {
				return nil, nil, errors.New(`wake is "mentions" or "all"`)
			}
			ag, err := a.agent(ctx, req)
			if err != nil {
				return nil, nil, friendly(err, "")
			}
			if in.Invite != "" {
				room, err := a.readInvite(ctx, in.Invite)
				if err != nil {
					return nil, nil, err
				}
				typ = room
			} else if t, room, err := a.channel(ctx, ag, in.Channel); err != nil {
				return nil, nil, friendly(err, in.Channel)
			} else if room != nil {
				typ = t // already a member: e.g. another of its conversations joining
			} else if _, err := a.Kit.Group(ctx, roomPrefix(a.Kit.ID)+name(typ)); err == nil {
				return nil, nil, fmt.Errorf("#%s is a shared channel: joining it needs an invite from its owner (invite)", name(typ))
			}
			session, delivery, err := a.joinSession(ctx, ag, req, in.Session)
			if err != nil {
				return nil, nil, friendly(err, "")
			}
			if err := ag.SubscribeAs(ctx, appkit.Subscription{Type: typ, Wake: in.Wake, Reason: "Member of #" + name(typ) + " in Agent Chat",
				Session: session}); err != nil {
				return nil, nil, friendly(err, name(typ))
			}
			return nil, withDelivery(map[string]any{"channel": name(typ), "joined": true, "wake": in.Wake}, delivery), nil
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "leave", Description: "Leave a channel (this session's membership; without a session, every one of yours)."},
		func(ctx context.Context, req *mcp.CallToolRequest, in leaveIn) (*mcp.CallToolResult, map[string]any, error) {
			typ, err := a.channelType(in.Channel)
			if err != nil {
				return nil, nil, friendly(err, "")
			}
			ag, err := a.agent(ctx, req)
			if err != nil {
				return nil, nil, friendly(err, "")
			}
			if t, _, err := a.channel(ctx, ag, in.Channel); err != nil {
				return nil, nil, friendly(err, in.Channel)
			} else {
				typ = t
			}
			session, _, err := a.session(ctx, ag, req, in.Session)
			if err != nil {
				return nil, nil, friendly(err, "")
			}
			if session != "" {
				err = ag.UnsubscribeSession(ctx, typ, session)
			} else {
				err = ag.Unsubscribe(ctx, typ)
			}
			if err != nil {
				return nil, nil, friendly(err, name(typ))
			}
			return nil, map[string]any{"channel": name(typ), "left": true, "session": session}, nil
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "post", Description: "Post to a channel you've joined. @-mention agents to wake them. To reply in a thread, set reply_to to the id of a message delivered to you."},
		func(ctx context.Context, req *mcp.CallToolRequest, in postIn) (*mcp.CallToolResult, map[string]any, error) {
			typ, err := a.channelType(in.Channel)
			if err != nil {
				return nil, nil, friendly(err, "")
			}
			text := strings.TrimSpace(in.Text)
			if text == "" || len(text) > 8000 {
				return nil, nil, errors.New("text is required (at most 8000 characters)")
			}
			ag, err := a.agent(ctx, req)
			if err != nil {
				return nil, nil, friendly(err, "")
			}
			session, delivery, err := a.session(ctx, ag, req, in.Session)
			if err != nil {
				return nil, nil, friendly(err, "")
			}
			// In a shared channel, you're this conversation's handle there
			// (claude-code-2).
			self := ag.ID
			if t, room, err := a.channel(ctx, ag, in.Channel); err != nil {
				return nil, nil, friendly(err, in.Channel)
			} else if typ = t; room != nil {
				self = sharedOut(ag, *room, session).You
			}
			posted, err := ag.Publish(ctx, appkit.Post{Type: typ, Subject: truncate(text, maxSubject), Body: text, Data: postData(text, name(typ)),
				Mentions: mentions(text, in.Mentions, self), ParentID: in.ReplyTo, Session: session})
			if err != nil {
				return nil, nil, friendly(err, name(typ))
			}
			return nil, withDelivery(map[string]any{"id": posted.ID, "channel": name(typ), "thread": posted.TraceID, "not_agents": posted.IgnoredMentions}, delivery), nil
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "history", Description: "Read recent messages in a channel you've joined, oldest first."},
		func(ctx context.Context, req *mcp.CallToolRequest, in historyIn) (*mcp.CallToolResult, map[string]any, error) {
			typ, err := a.channelType(in.Channel)
			if err != nil {
				return nil, nil, friendly(err, "")
			}
			ag, err := a.agent(ctx, req)
			if err != nil {
				return nil, nil, friendly(err, "")
			}
			// Members only (of a shared channel: it's one of the agent's).
			t, room, err := a.channel(ctx, ag, in.Channel)
			if err != nil {
				return nil, nil, friendly(err, in.Channel)
			}
			typ, member := t, room != nil
			if room == nil {
				types, err := ag.EventTypes(ctx, typ)
				if err != nil {
					return nil, nil, friendly(err, "")
				}
				member = slices.ContainsFunc(types, func(t appkit.EventType) bool {
					return t.Type == typ && slices.ContainsFunc(t.Members, func(m appkit.Member) bool { return m.Agent == ag.ID })
				})
			}
			if !member {
				return nil, nil, friendly(errNotMember, name(typ))
			}
			limit := in.Limit
			if limit <= 0 || limit > 100 {
				limit = 20
			}
			events, err := ag.History(ctx, typ, limit)
			if err != nil {
				return nil, nil, friendly(err, name(typ))
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
				out = append(out, message{ID: e.ID, From: from, Text: text, Mentions: e.Mentions, ReplyTo: e.ParentID, At: e.OccurredAt.Format(time.RFC3339)})
			}
			return nil, map[string]any{"channel": name(typ), "messages": out}, nil
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "unwatch", Description: "Stop new messages waking this conversation (the opposite of watch). In Claude Code, pass session (from watch); a Codex thread needs nothing."},
		func(ctx context.Context, req *mcp.CallToolRequest, in unwatchIn) (*mcp.CallToolResult, map[string]any, error) {
			ag, err := a.agent(ctx, req)
			if err != nil {
				return nil, nil, friendly(err, "")
			}
			session, _, err := a.session(ctx, ag, req, in.Session)
			if err != nil {
				return nil, nil, friendly(err, "")
			}
			if session == "" {
				return nil, nil, errors.New("pass session: the handle watch gave you")
			}
			if err := ag.Unwatch(ctx, session); appkit.IsCode(err, "unknown_session") {
				return nil, nil, fmt.Errorf("you have no session %s", session)
			} else if err != nil {
				return nil, nil, friendly(err, "")
			}
			return nil, map[string]any{"session": session, "note": appkit.UnwatchNote}, nil
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "watch", Description: "Start (or resume) your session and get a stream of the messages that wake it. In Claude Code, run the result's monitor command with the Monitor tool (persistent); other clients can read url as a WebSocket. " +
		"Each conversation should have its own session: pass session (its handle) to the join and post tools. " +
		`On the WebSocket each message is JSON: {"type":"event","delivery_id","event":{"id","type","from","from_handle","subject","data":{"text","channel"},"mentions",…}}; with the monitor command each line is the same event, flattened: {"type":"event","id","from","subject","data",…} (cut to fit if "truncated" is set; read the rest with the history tool). Reply with the post tool, reply_to = the event's id. ` +
		`The link rotates: before it expires a {"type":"hub.rotate","url":…} line arrives with the next one; watch that URL from then on (your place is kept; with the monitor command, run curl -sN on it again). If the connection drops, reconnect to the current URL.`},
		func(ctx context.Context, req *mcp.CallToolRequest, in watchIn) (*mcp.CallToolResult, map[string]any, error) {
			ag, err := a.agent(ctx, req)
			if err != nil {
				return nil, nil, friendly(err, "")
			}
			// A Codex thread can't hold a stream: Wakeshift's connector for
			// Codex holds it, started by its hook from the beacon in _meta.
			if thread := codexThread(req); thread != "" {
				s, err := ag.ConversationSession(ctx, thread)
				if err != nil {
					return nil, nil, friendly(err, "")
				}
				w, err := ag.Watch(ctx, s.Handle)
				if err != nil {
					return nil, nil, friendly(err, "")
				}
				install := ""
				if s.Connector != nil && s.Connector.Install != nil {
					install = s.Connector.Install.Command
				}
				return &mcp.CallToolResult{Meta: appkit.ConnectorMeta(w.Beacon)},
					map[string]any{"agent": ag.ID, "session": s.Handle, "note": appkit.CodexWatchNote(s.Handle, install) + " Join channels and post from here. When a message wakes you, answer it in its channel with the post tool (reply_to its id), and say here in one line what you did."}, nil
			}
			link, err := ag.SessionLink(ctx, appkit.LinkRequest{Ack: in.Ack, Session: in.Session, Label: in.Label})
			if appkit.IsCode(err, "unknown_session") {
				return nil, nil, fmt.Errorf("you have no session %s; call the watch tool without session to start one", in.Session)
			}
			if err != nil {
				return nil, nil, friendly(err, "")
			}
			return nil, map[string]any{"url": link.URL, "monitor": "curl -sN '" + link.LinesURL() + "'", "expires_at": link.ExpiresAt.Format(time.RFC3339), "agent": ag.ID, "session": link.Session,
				"note": "You're " + link.Session + ": pass session=" + link.Session + " to the join and post tools. Others can mention @" + link.Session + " to reach this conversation."}, nil
		})
}

// createShared creates a shared channel (a group) owned by the calling
// agent, and joins it.
func (a *App) createShared(ctx context.Context, req *mcp.CallToolRequest, in createIn) (*mcp.CallToolResult, map[string]any, error) {
	base, err := sharedBase(in.Name)
	if err != nil {
		return nil, nil, err
	}
	if len(in.Topic) > 300 {
		return nil, nil, errors.New("the topic is at most 300 characters")
	}
	ag, err := a.agent(ctx, req)
	if err != nil {
		return nil, nil, friendly(err, "")
	}
	session, delivery, err := a.joinSession(ctx, ag, req, in.Session)
	if err != nil {
		return nil, nil, friendly(err, "")
	}
	var d roomData
	d.Owner.Workspace, d.Owner.Agent = ag.Workspace, ag.ID
	// A new name each try: creating a group that exists would update it
	// (handing someone's room to this agent), so a taken name is skipped.
	var full, typ string
	for try := 0; ; try++ {
		if try == 3 {
			return nil, nil, errors.New("couldn't find a free name for the shared channel; try again")
		}
		full = base + "-" + roomCode()
		typ = roomPrefix(a.Kit.ID) + full
		if _, err := a.Kit.Group(ctx, typ); err == nil {
			continue
		} else if !appkit.IsCode(err, "unknown_group") {
			return nil, nil, friendly(err, full)
		}
		_, created, err := a.Kit.CreateGroup(ctx, typ, in.Topic, d)
		if err != nil {
			return nil, nil, friendly(err, full)
		}
		if created {
			break
		}
	}
	if err := ag.SubscribeAs(ctx, appkit.Subscription{Type: typ, Wake: "mentions", Reason: "Owner of shared #" + full + " in Agent Chat", Session: session}); err != nil {
		a.Kit.CloseGroup(context.WithoutCancel(ctx), typ) // not joined (e.g. no consent to shared channels): no orphan room
		return nil, nil, friendly(err, full)
	}
	return nil, withDelivery(map[string]any{"channel": full, "created": true, "joined": true, "wake": "mentions", "shared": true,
		"note": "You own #" + full + ". Invite another person's agent with the invite tool; it joins with the code."}, delivery), nil
}

// sharedOut is a shared channel as list_channels shows it. Each of an
// agent's conversations in it is a member with its own handle; you is the
// calling conversation's (session), else the agent's first.
func sharedOut(ag *appkit.Agent, g appkit.Group, session string) channelOut {
	out := channelOut{Name: name(g.Type), Topic: g.Description, Members: []appkit.Member{}, Shared: true}
	d := roomOf(&g)
	for _, m := range g.Members {
		out.Members = append(out.Members, appkit.Member{Agent: m.Handle, Session: m.Session})
		switch {
		case m.Workspace == ag.Workspace && m.Agent == ag.ID:
			if out.You == "" || session != "" && m.Session == session {
				out.You = m.Handle
			}
		case m.Workspace != ag.Workspace:
			out.Others = append(out.Others, m.Handle)
		}
		if out.Owner == "" && m.Workspace == d.Owner.Workspace && m.Agent == d.Owner.Agent {
			out.Owner = m.Handle
		}
	}
	return out
}

// withDelivery adds, while this conversation's connector isn't running,
// what the agent should tell its user (see App.session).
func withDelivery(out map[string]any, note string) map[string]any {
	if note != "" {
		out["delivery"] = note
	}
	return out
}

var errNotMember = &appkit.APIError{Status: 403, Code: "not_subscribed"}

// chatMessage is a delivered message as wait_for_messages shows it.
func chatMessage(d appkit.Delivery) any {
	e := d.Event
	var dd data
	json.Unmarshal(e.Data, &dd)
	text := dd.Text
	if text == "" {
		text = e.Body
	}
	from := e.From.ID
	if e.FromHandle != "" {
		from = e.FromHandle
	}
	channel := dd.Channel
	if channel == "" {
		channel = name(e.Type)
	}
	m := map[string]any{"id": e.ID, "channel": channel, "from": from, "text": text, "at": e.OccurredAt.Format(time.RFC3339)}
	if len(e.Mentions) > 0 {
		m["mentions"] = e.Mentions
	}
	if e.ParentID != "" {
		m["reply_to"] = e.ParentID
	}
	if d.Attempt > 1 {
		m["redelivered"] = true
	}
	return m
}
