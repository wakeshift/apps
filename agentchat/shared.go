package agentchat

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/wakeshift-go/appkit"
)

// A shared channel is a Wakeshift group (app.agentchat.room.<name>-<code>):
// agents from other people's workspaces can be in it. Agent Chat owns the
// room: the agent that created it is its owner, the only one who can invite
// (with a signed invite code, good for anyone who has it for a week) and
// remove members. Removing a member ends the invites made before, so the
// member can't join again with the code it has. That's kept on the group
// (its data); Agent Chat itself stores nothing.

const inviteTTL = 7 * 24 * time.Hour

type roomData struct {
	Owner struct {
		Workspace string `json:"workspace"`
		Agent     string `json:"agent"`
	} `json:"owner"`
	// Invites is the generation of invite codes: only this one's work.
	Invites int `json:"invites,omitempty"`
}

func (d roomData) ownedBy(ag *appkit.Agent) bool {
	return d.Owner.Workspace == ag.Workspace && d.Owner.Agent == ag.ID
}

// sharedBase checks the name asked for a new shared channel, which gets a
// random code added (roomCode), since the namespace is everyone's.
func sharedBase(base string) (string, error) {
	base = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(base)), "#")
	if len(base) > 34 || !channelRE.MatchString(base) {
		return "", errors.New("shared channel names are lowercase letters, digits and dashes, up to 34 characters")
	}
	return base, nil
}

// roomCode is a shared channel's random code (a variable for tests).
var roomCode = func() string {
	const alphabet = "abcdefghjkmnpqrstuvwxyz23456789"
	b := make([]byte, 5)
	rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

// channel resolves a channel name to its event type: one of the agent's
// shared channels if it's in one by that name, else the workspace's.
func (a *App) channel(ctx context.Context, ag *appkit.Agent, raw string) (typ string, room *appkit.Group, err error) {
	typ, err = a.channelType(raw)
	if err != nil {
		return "", nil, err
	}
	rooms, err := ag.Groups(ctx)
	if err != nil {
		return "", nil, err
	}
	for i, g := range rooms {
		if g.Type == roomPrefix(a.Kit.ID)+name(typ) {
			return g.Type, &rooms[i], nil
		}
	}
	return typ, nil, nil
}

func roomOf(g *appkit.Group) roomData {
	var d roomData
	json.Unmarshal(g.Data, &d)
	return d
}

// invite is a signed code naming a room, its generation of invites, and
// when it stops working.
type invite struct {
	Room    string `json:"r"`
	Gen     int    `json:"g,omitempty"`
	Expires int64  `json:"e"`
}

func (a *App) signInvite(room string, gen int) string {
	b, _ := json.Marshal(invite{Room: room, Gen: gen, Expires: time.Now().Add(inviteTTL).Unix()})
	payload := base64.RawURLEncoding.EncodeToString(b)
	sig := ed25519.Sign(a.Kit.Key, []byte("agentchat-invite:"+payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(sig)
}

var errBadInvite = errors.New("that invite isn't valid (or it's expired); ask the channel's owner for a new one")

// readInvite is the room an invite code is for, if it's still good: signed,
// not expired, and of the room's current generation (not made before a
// member was removed).
func (a *App) readInvite(ctx context.Context, code string) (string, error) {
	payload, sig, ok := strings.Cut(strings.TrimSpace(code), ".")
	s, err := base64.RawURLEncoding.DecodeString(sig)
	if !ok || err != nil || !ed25519.Verify(a.Kit.Key.Public().(ed25519.PublicKey), []byte("agentchat-invite:"+payload), s) {
		return "", errBadInvite
	}
	b, _ := base64.RawURLEncoding.DecodeString(payload)
	var in invite
	if json.Unmarshal(b, &in) != nil || time.Now().Unix() > in.Expires || !strings.HasPrefix(in.Room, roomPrefix(a.Kit.ID)) {
		return "", errBadInvite
	}
	g, err := a.Kit.Group(ctx, in.Room)
	if appkit.IsCode(err, "unknown_group") {
		return "", errBadInvite
	} else if err != nil {
		return "", friendly(err, name(in.Room))
	}
	if roomOf(&g).Invites != in.Gen {
		return "", errBadInvite
	}
	return in.Room, nil
}

type inviteIn struct {
	Channel string `json:"channel" jsonschema:"A shared channel you created"`
}

type removeIn struct {
	Channel string `json:"channel" jsonschema:"A shared channel you created"`
	Member  string `json:"member" jsonschema:"The member's handle, as list_channels shows it (e.g. claude-code-2)"`
}

func (a *App) addSharedTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{Name: "invite", Description: "Get an invite code for a shared channel you created, for another person's agent: " +
		"they join with the join tool (invite=…). Anyone with the code can join for a week, so give it only to whom you mean to."},
		func(ctx context.Context, req *mcp.CallToolRequest, in inviteIn) (*mcp.CallToolResult, map[string]any, error) {
			ag, err := a.agent(ctx, req)
			if err != nil {
				return nil, nil, friendly(err, "")
			}
			_, room, err := a.channel(ctx, ag, in.Channel)
			if err != nil {
				return nil, nil, friendly(err, in.Channel)
			}
			if room == nil || !roomOf(room).ownedBy(ag) {
				return nil, nil, errors.New("only the agent that created a shared channel can invite to it")
			}
			code := a.signInvite(room.Type, roomOf(room).Invites)
			return nil, map[string]any{"channel": name(room.Type), "invite": code, "expires_in_days": int(inviteTTL.Hours() / 24),
				"note": "Give this to the person whose agent should join (or to their agent): it calls the join tool with invite=" + code + "."}, nil
		})

	mcp.AddTool(srv, &mcp.Tool{Name: "remove_member", Description: "Remove a member from a shared channel you created."},
		func(ctx context.Context, req *mcp.CallToolRequest, in removeIn) (*mcp.CallToolResult, map[string]any, error) {
			ag, err := a.agent(ctx, req)
			if err != nil {
				return nil, nil, friendly(err, "")
			}
			_, room, err := a.channel(ctx, ag, in.Channel)
			if err != nil {
				return nil, nil, friendly(err, in.Channel)
			}
			if room == nil || !roomOf(room).ownedBy(ag) {
				return nil, nil, errors.New("only the agent that created a shared channel can remove members")
			}
			handle := strings.TrimPrefix(in.Member, "@")
			if !slices.ContainsFunc(room.Members, func(m appkit.GroupMember) bool { return m.Handle == handle }) {
				return nil, nil, fmt.Errorf("no member %s in #%s", handle, name(room.Type))
			}
			// End the invites made so far first: the member has one, and
			// could join again with it.
			d := roomOf(room)
			d.Invites++
			if _, _, err := a.Kit.CreateGroup(ctx, room.Type, room.Description, d); err != nil {
				return nil, nil, friendly(err, in.Channel)
			}
			if err := a.Kit.RemoveMember(ctx, room.Type, handle); appkit.IsCode(err, "unknown_member") {
				return nil, nil, fmt.Errorf("no member %s in #%s", handle, name(room.Type))
			} else if err != nil {
				return nil, nil, friendly(err, in.Channel)
			}
			return nil, map[string]any{"channel": name(room.Type), "removed": handle,
				"note": "Invite codes made before no longer work. Make a new one with the invite tool for anyone else you want to join."}, nil
		})
}
