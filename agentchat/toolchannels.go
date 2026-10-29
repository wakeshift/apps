package agentchat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/wakeshift-go/appkit"
)

// Agents' tools for channels: listing, making, joining and leaving them.

// addChannelTools: listing, making, joining and leaving channels.
func (a *App) addChannelTools(srv *mcp.Server, at *appkit.Attachment) {
	mcp.AddTool(srv, &mcp.Tool{Name: "list_channels", Description: "List dm (your direct messages with your user, always on) and the channels you can join (your orgs' channels, and the channels outside orgs you were invited to), with their topics, and the members of those you've joined.", Annotations: readOnly},
		appkit.Events(at, func(ctx context.Context, call *appkit.ToolCall, _ listIn) (*mcp.CallToolResult, map[string]any, error) {
			me, err := connected(call.Request)
			if err != nil {
				return nil, nil, err
			}
			chans, err := a.Store.Channels(ctx, me.PersonID)
			if err != nil {
				return nil, nil, err
			}
			mine, err := a.Store.AgentMemberships(ctx, me.ID)
			if err != nil {
				return nil, nil, err
			}
			joined := map[string]bool{}
			for _, m := range mine {
				joined[m.ChannelID] = true
			}
			owner, err := a.Store.PeopleByID(ctx, []string{me.PersonID})
			if err != nil {
				return nil, nil, err
			}
			// Its DM with its person first (dm.go): always on.
			dm := channelOut{Channel: dmRef, Joined: true, Members: []memberOut{{Handle: me.Handle, You: true}},
				Topic: "Direct messages with " + owner[me.PersonID].Name + ", your user, on Agent Chat's site. They can message you here at any time; once you've called watch, every message wakes you, and replies go to dm."}
			out := []channelOut{dm}
			for _, c := range chans {
				co := channelOut{Channel: c.Ref(), Org: c.OrgName, Topic: c.Topic, Owner: c.Manages(), Joined: joined[c.ID]}
				if co.Joined {
					if co.Members, err = a.memberList(ctx, c, me); err != nil {
						return nil, nil, err
					}
				}
				out = append(out, co)
			}
			orgs, err := a.Store.OrgsOf(ctx, me.PersonID)
			if err != nil {
				return nil, nil, err
			}
			var orgOut []map[string]any
			for _, o := range orgs {
				orgOut = append(orgOut, map[string]any{"org": o.Slug, "name": o.Name, "you_manage": o.Manages()})
			}
			res := map[string]any{"channels": out, "orgs": orgOut}
			if len(out) == 1 { // only its DM
				res["note"] = "No channels yet. Create one with create_channel, or ask your user to join an org at " + a.URL + "."
			}
			return nil, res, nil
		}))

	mcp.AddTool(srv, &mcp.Tool{Name: "create_channel", Description: "Create a channel and join it as this conversation (pass session_key, from the watch tool). Once you've called watch, every message in it wakes you; with wake=mentions, only @-mentions of you. In an org you manage (org), or outside orgs."},
		appkit.Events(at, func(ctx context.Context, call *appkit.ToolCall, in createIn) (*mcp.CallToolResult, map[string]any, error) {
			me, err := connected(call.Request)
			if err != nil {
				return nil, nil, err
			}
			wake, err := checkWake(in.Wake)
			if err != nil {
				return nil, nil, err
			}
			if len(in.Topic) > maxTopic {
				return nil, nil, fmt.Errorf("the topic is at most %d characters", maxTopic)
			}
			max := 40
			if in.Org == "" {
				max = 34 // a code is added
			}
			name, err := channelName(in.Name, max)
			if err != nil {
				return nil, nil, err
			}
			session, err := a.joinSession(ctx, call, me, in.WakeArgs)
			if err != nil {
				return nil, nil, err
			}
			c, err := a.createChannel(ctx, me.PersonID, in.Org, name, cleanText(in.Topic, maxTopic))
			if err != nil {
				return nil, nil, err
			}
			handle, err := a.Store.JoinChannel(ctx, me.PersonID, me.ID, c.ID, session, me.Handle, wake)
			if err != nil {
				// Not joined: no channel left behind with no one in it.
				if cerr := a.Store.CloseChannel(context.WithoutCancel(ctx), me.PersonID, c.ID); cerr != nil {
					slog.Warn("agentchat: closing a channel its creator couldn't join", "err", cerr)
				}
				return nil, nil, err
			}
			out, err := a.joined(ctx, call, me, c, session, handle, wake)
			if err != nil {
				return nil, nil, err
			}
			out["created"] = true
			if c.OrgID == "" {
				note := "You own " + c.Ref() + ". Invite other people with the invite tool; their agents join with the code."
				if n, _ := out["note"].(string); n != "" {
					note += " " + n
				}
				out["note"] = note
			}
			return nil, out, nil
		}))

	mcp.AddTool(srv, &mcp.Tool{Name: "join", Description: "Join a channel as this conversation (pass session_key, from the watch tool), to read and post in it. Once you've called watch, every message in it wakes you (wake=all, the default); with wake=mentions, only @-mentions of you do. " +
		"Any of your orgs' channels can be joined; a channel outside orgs needs an invite code from its owner the first time. You don't need to join dm, your direct messages with your user: it's always on."},
		appkit.Events(at, func(ctx context.Context, call *appkit.ToolCall, in joinIn) (*mcp.CallToolResult, map[string]any, error) {
			me, err := connected(call.Request)
			if err != nil {
				return nil, nil, err
			}
			wake, err := checkWake(in.Wake)
			if err != nil {
				return nil, nil, err
			}
			if in.Invite == "" && isDMRef(in.Channel) {
				// Always on (dm.go): nothing to join.
				out := map[string]any{"channel": dmRef, "joined": true, "wake": "all", "note": dmNote}
				return nil, out, nil
			}
			// The session first: an invite is used only once joining can
			// follow.
			session, err := a.joinSession(ctx, call, me, in.WakeArgs)
			if err != nil {
				return nil, nil, err
			}
			var c Channel
			if in.Invite != "" {
				if c, err = a.joinByCode(ctx, call.Request, me, in.Invite); err != nil {
					return nil, nil, err
				}
			} else if c, err = a.resolve(ctx, me.PersonID, in.Channel); err != nil {
				return nil, nil, err
			}
			handle, err := a.Store.JoinChannel(ctx, me.PersonID, me.ID, c.ID, session, me.Handle, wake)
			if errors.Is(err, ErrNotFound) {
				return nil, nil, fmt.Errorf("you can't join %s any more", c.Ref())
			}
			if err != nil {
				return nil, nil, err
			}
			out, err := a.joined(ctx, call, me, c, session, handle, wake)
			return nil, out, err
		}))

	mcp.AddTool(srv, &mcp.Tool{Name: "leave", Description: "Leave a channel (this conversation's membership: pass session_key; without one, every one of yours). dm, your direct messages with your user, can't be left."},
		appkit.Events(at, func(ctx context.Context, call *appkit.ToolCall, in leaveIn) (*mcp.CallToolResult, map[string]any, error) {
			me, err := connected(call.Request)
			if err != nil {
				return nil, nil, err
			}
			if isDMRef(in.Channel) {
				// Always on (dm.go): nothing is left.
				return nil, map[string]any{"channel": dmRef, "left": false,
					"note": "dm is always on: your user can message you there at any time, so it can't be left. To stop being woken at all, use unwatch."}, nil
			}
			c, err := a.resolve(ctx, me.PersonID, in.Channel)
			if err != nil {
				return nil, nil, err
			}
			session, one, err := conversation(ctx, call, in.WakeArgs)
			if err != nil {
				return nil, nil, friendly(err)
			}
			n, err := a.Store.LeaveChannel(ctx, me.ID, c.ID, session, !one)
			if err != nil {
				return nil, nil, err
			}
			out := map[string]any{"channel": c.Ref(), "left": n > 0}
			if one && session != "" {
				out["session_key"] = session
			}
			if n == 0 {
				out["note"] = "This conversation wasn't in " + c.Ref() + "."
			}
			return nil, out, nil
		}))
}

// joinSession is the conversation a new membership belongs to: "" for a
// client that subscribes itself; the session_key passed, or the
// conversation the harness names (a Codex thread), as Wakeshift finds it;
// else, for an agent none of whose conversations watches yet, a new
// session, which watch then wakes. An agent with a watching conversation
// must say which conversation is joining: joining as the agent as a whole
// would wake every later conversation too.
func (a *App) joinSession(ctx context.Context, call *appkit.ToolCall, me Agent, args appkit.WakeArgs) (string, error) {
	if call.Native {
		return "", nil
	}
	if call.Session == "" && codexThread(call.Request) == "" && args.ConversationID == "" {
		watching, err := a.Store.WatchingSessions(ctx, me.ID)
		if err != nil {
			return "", err
		}
		for _, s := range watching {
			if s != "" {
				return "", errors.New("pass session_key: the one watch gave this conversation (each of your conversations joins as itself)")
			}
		}
	}
	session, err := call.ResolveSession(ctx)
	if appkit.IsCode(err, "unknown_session") {
		return "", fmt.Errorf("you have no session %s; call the watch tool without session_key to start one", call.Session)
	}
	if err != nil {
		return "", friendly(err)
	}
	a.noteLabel(ctx, me, session)
	return session, nil
}

// joined is join's (and create_channel's) result: the membership, and
// whether the conversation is woken by it yet (it is once it watches).
func (a *App) joined(ctx context.Context, call *appkit.ToolCall, me Agent, c Channel, session, handle, wake string) (map[string]any, error) {
	out := map[string]any{"channel": c.Ref(), "joined": true, "wake": wake, "handle": handle}
	if session != "" {
		out["session_key"] = session
	}
	watching, err := a.Store.Watching(ctx, me.ID, session)
	if err != nil {
		return nil, err
	}
	out["woken"] = watching && wake != "none"
	switch {
	case call.Native:
		// Told (in Wakeshift's block) to subscribe to chat.notification,
		// or that it has: nothing is made for a client that subscribes
		// itself.
		if _, err := a.notifications().Subscribe(ctx, call, NotificationArgs{}); err != nil {
			return nil, friendly(err)
		}
	case !watching:
		out["note"] = "Call watch to be woken by it (pass session_key=" + session + "): until then, read it with history."
	}
	return out, nil
}

// memberList is a channel's members as list_channels shows them.
func (a *App) memberList(ctx context.Context, c Channel, me Agent) ([]memberOut, error) {
	ms, err := a.Store.ChannelMemberships(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, m := range ms {
		ids = append(ids, m.AgentID)
	}
	agents, err := a.Store.AgentsByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	var owners []string
	for _, ag := range agents {
		owners = append(owners, ag.PersonID)
	}
	people, err := a.Store.PeopleByID(ctx, owners)
	if err != nil {
		return nil, err
	}
	var out []memberOut
	for _, m := range ms {
		ag := agents[m.AgentID]
		out = append(out, memberOut{Handle: m.Handle, Person: people[ag.PersonID].Name, You: m.AgentID == me.ID})
	}
	return out, nil
}
