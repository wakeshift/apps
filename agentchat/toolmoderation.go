package agentchat

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/wakeshift-go/appkit"
)

// Agents' tools for moderation: removing and banning.

// addModerationTools: invites to channels outside orgs, and removing
// members (by a channel's owner, or an org's owner and admins).
func (a *App) addModerationTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{Name: "invite", Description: "Get an invite code and link for a channel outside orgs that you own, for another person: their agent joins with the join tool (invite=…), or they open the link. " +
		"It works for a week, for up to 25 people, so give it only to whom you mean to. Org invites are made on Agent Chat's site."},
		func(ctx context.Context, req *mcp.CallToolRequest, in inviteIn) (*mcp.CallToolResult, map[string]any, error) {
			me, err := connected(req)
			if err != nil {
				return nil, nil, err
			}
			c, err := a.resolve(ctx, me.PersonID, in.Channel)
			if err != nil {
				return nil, nil, err
			}
			if c.OrgID != "" {
				return nil, nil, fmt.Errorf("%s is an org channel: every member of the org can join it. People join the org with an invite link made on %s/orgs/%s", c.Ref(), a.URL, c.OrgSlug)
			}
			if c.Role != "owner" {
				return nil, nil, fmt.Errorf("only the owner of %s can invite to it", c.Ref())
			}
			inv, err := a.Store.CreateInvite(ctx, me.PersonID, "", c.ID, defaultInviteTTL, defaultInviteUses)
			if errors.Is(err, errTooMany) {
				return nil, nil, errors.New("this channel has too many live invites; revoke some on Agent Chat's site")
			}
			if err != nil {
				return nil, nil, err
			}
			return nil, map[string]any{"channel": c.Ref(), "invite": inv.Code, "link": a.URL + "/join/" + inv.Code, "expires_at": inv.Expires.UTC().Format(time.RFC3339),
				"note": "Give this to the person whose agent should join: their agent calls the join tool with invite=" + inv.Code + ", or they open the link."}, nil
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "remove_member", Description: "Remove a member's person, with all their agents, from a channel you manage (a channel outside orgs you own, or a channel of an org you manage). From an org channel they're kept out of it (its admins can let them back in on Agent Chat's site); from a channel outside orgs, only with ban."},
		func(ctx context.Context, req *mcp.CallToolRequest, in removeIn) (*mcp.CallToolResult, map[string]any, error) {
			me, err := connected(req)
			if err != nil {
				return nil, nil, err
			}
			c, err := a.resolve(ctx, me.PersonID, in.Channel)
			if err != nil {
				return nil, nil, err
			}
			if !c.Manages() {
				return nil, nil, fmt.Errorf("you don't manage %s", c.Ref())
			}
			g, err := a.Kit.Group(ctx, c.GroupType)
			if err != nil {
				return nil, nil, friendly(err, c.Ref())
			}
			handle := strings.TrimPrefix(strings.TrimSpace(in.Member), "@")
			i := slices.IndexFunc(g.Members, func(m appkit.GroupMember) bool { return m.Handle == handle })
			if i < 0 {
				return nil, nil, fmt.Errorf("no member %s in %s", handle, c.Ref())
			}
			people, err := a.Store.PeopleByWorkspace(ctx, []string{g.Members[i].Workspace})
			if err != nil {
				return nil, nil, err
			}
			p, ok := people[g.Members[i].Workspace]
			if !ok {
				return nil, nil, fmt.Errorf("no member %s in %s", handle, c.Ref())
			}
			if p.ID == me.PersonID {
				return nil, nil, errors.New("that's one of your own agents: use leave")
			}
			if err := a.removeFromChannel(ctx, me.PersonID, c, p.ID, in.Ban); err != nil {
				return nil, nil, err
			}
			return nil, map[string]any{"channel": c.Ref(), "removed": p.Name, "banned": in.Ban}, nil
		})

}
