package agentchat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/wakeshift-go/appkit"
)

// Agents' tools for channels: listing, making, joining, leaving and closing them.

// addChannelTools: listing, making, joining and leaving channels.
func (a *App) addChannelTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{Name: "list_channels", Description: "List dm (your direct messages with your user, always on) and the channels you can join (your orgs' channels, and the channels outside orgs you were invited to), with their topics, and the members of those you've joined."},
		func(ctx context.Context, req *mcp.CallToolRequest, _ empty) (*mcp.CallToolResult, map[string]any, error) {
			me, err := connected(req)
			if err != nil {
				return nil, nil, err
			}
			ag, err := a.actAs(ctx, me)
			if err != nil {
				return nil, nil, friendly(err, "")
			}
			chans, err := a.Store.Channels(ctx, me.PersonID)
			if err != nil {
				return nil, nil, err
			}
			groups, err := ag.Groups(ctx)
			if err != nil {
				return nil, nil, friendly(err, "")
			}
			var workspaces []string
			for _, g := range groups {
				for _, m := range g.Members {
					workspaces = append(workspaces, m.Workspace)
				}
			}
			people, err := a.Store.PeopleByWorkspace(ctx, workspaces)
			if err != nil {
				return nil, nil, err
			}
			// Its DM with its person first (dm.go): there whether or not
			// it's been started.
			owner, err := a.Store.PeopleByID(ctx, []string{me.PersonID})
			if err != nil {
				return nil, nil, err
			}
			dm := channelOut{Channel: dmRef, Topic: "Direct messages with " + owner[me.PersonID].Name + ", your user, on Agent Chat's site. They can message you here at any time; every message wakes you, and replies go to dm."}
			if c, ready, err := a.Store.dmWith(ctx, me.PersonID, me.ID); err == nil && ready {
				if g := groupOf(groups, c.GroupType); g != nil {
					dm.Joined = true
					for _, m := range g.Members {
						if !a.isReader(m) {
							dm.Members = append(dm.Members, memberOut{Handle: m.Handle, You: true})
						}
					}
				}
			} else if err != nil && !errors.Is(err, ErrNotFound) {
				return nil, nil, err
			}
			out := []channelOut{dm}
			for _, c := range chans {
				co := channelOut{Channel: c.Ref(), Org: c.OrgName, Topic: c.Topic, Owner: c.Manages()}
				if g := groupOf(groups, c.GroupType); g != nil {
					co.Joined = true
					for _, m := range g.Members {
						if a.isReader(m) {
							continue // the site's reader (archive.go)
						}
						co.Members = append(co.Members, memberOut{Handle: m.Handle, Person: people[m.Workspace].Name,
							You: m.Workspace == ag.Workspace && m.Agent == ag.ID})
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
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "create_channel", Description: "Create a channel and join it as this conversation (pass session, from the watch tool). Every message in it wakes you; with wake=mentions, only @-mentions of you. In an org you manage (org), or outside orgs."},
		func(ctx context.Context, req *mcp.CallToolRequest, in createIn) (*mcp.CallToolResult, map[string]any, error) {
			me, err := connected(req)
			if err != nil {
				return nil, nil, err
			}
			if in.Wake == "" {
				in.Wake = "all"
			}
			if in.Wake != "mentions" && in.Wake != "all" {
				return nil, nil, errors.New(`wake is "all" or "mentions"`)
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
			ag, err := a.actAs(ctx, me)
			if err != nil {
				return nil, nil, friendly(err, "")
			}
			session, delivery, err := a.joinSession(ctx, ag, req, in.Session)
			if err != nil {
				return nil, nil, friendly(err, "")
			}
			a.noteLabel(ctx, me, session)
			c, err := a.createChannel(ctx, me.PersonID, in.Org, name, cleanText(in.Topic, maxTopic))
			if err != nil {
				return nil, nil, err
			}
			if err := ag.SubscribeAs(ctx, appkit.Subscription{Type: c.GroupType, Wake: in.Wake, Reason: "Member of " + c.Ref() + " in Agent Chat",
				Session: session, Handle: me.Handle}); err != nil {
				// Not joined: no channel left behind with no one in it.
				if cerr := a.Store.CloseChannel(context.WithoutCancel(ctx), me.PersonID, c.ID); cerr != nil {
					slog.Warn("agentchat: closing a channel its creator couldn't join", "err", cerr)
				}
				a.kickOutbox()
				return nil, nil, friendly(err, c.Ref())
			}
			out := map[string]any{"channel": c.Ref(), "created": true, "joined": true, "wake": in.Wake}
			if c.OrgID == "" {
				out["note"] = "You own " + c.Ref() + ". Invite other people with the invite tool; their agents join with the code."
			}
			return nil, subscribeNote(req, withDelivery(out, delivery), c.Ref()), nil
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "join", Description: "Join a channel as this conversation (pass session). Every message in it wakes you (wake=all, the default); with wake=mentions, only @-mentions of you do. " +
		"Any of your orgs' channels can be joined; a channel outside orgs needs an invite code from its owner the first time. You don't need to join dm, your direct messages with your user: it's always on."},
		func(ctx context.Context, req *mcp.CallToolRequest, in joinIn) (*mcp.CallToolResult, map[string]any, error) {
			me, err := connected(req)
			if err != nil {
				return nil, nil, err
			}
			if in.Invite == "" && isDMRef(in.Channel) {
				in.Wake = "all" // a DM's messages are all for it
			} else if in.Wake == "" {
				in.Wake = "all"
			}
			if in.Wake != "mentions" && in.Wake != "all" {
				return nil, nil, errors.New(`wake is "all" or "mentions"`)
			}
			// The session first: an invite is used only once joining can
			// follow.
			ag, err := a.actAs(ctx, me)
			if err != nil {
				return nil, nil, friendly(err, "")
			}
			session, delivery, err := a.joinSession(ctx, ag, req, in.Session)
			if err != nil {
				return nil, nil, friendly(err, "")
			}
			a.noteLabel(ctx, me, session)
			var c Channel
			if in.Invite != "" {
				if c, err = a.joinByCode(ctx, req, me, in.Invite); err != nil {
					return nil, nil, err
				}
			} else if c, err = a.resolveFor(ctx, me, in.Channel); err != nil {
				return nil, nil, err
			}
			if err := ag.SubscribeAs(ctx, appkit.Subscription{Type: c.GroupType, Wake: in.Wake, Reason: "Member of " + c.Ref() + " in Agent Chat",
				Session: session, Handle: me.Handle}); err != nil {
				return nil, nil, friendly(err, c.Ref())
			}
			out := map[string]any{"channel": c.Ref(), "joined": true, "wake": in.Wake}
			if c.DMAgent != "" {
				out["note"] = dmNote
			}
			return nil, subscribeNote(req, withDelivery(out, delivery), c.Ref()), nil
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "leave", Description: "Leave a channel (this session's membership; without a session, every one of yours). dm, your direct messages with your user, can't be left."},
		func(ctx context.Context, req *mcp.CallToolRequest, in leaveIn) (*mcp.CallToolResult, map[string]any, error) {
			me, err := connected(req)
			if err != nil {
				return nil, nil, err
			}
			if isDMRef(in.Channel) {
				// Always on (dm.go): nothing is left.
				return nil, map[string]any{"channel": dmRef, "left": false,
					"note": "dm is always on: your user can message you there at any time, so it can't be left. To stop being woken at all, use unwatch."}, nil
			}
			ag, err := a.actAs(ctx, me)
			if err != nil {
				return nil, nil, friendly(err, "")
			}
			typ, ref, err := a.channelOrGroup(ctx, me, ag, in.Channel)
			if err != nil {
				return nil, nil, err
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
				return nil, nil, friendly(err, ref)
			}
			return nil, map[string]any{"channel": ref, "left": true, "session": session}, nil
		})

}
