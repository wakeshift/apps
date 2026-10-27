package agentchat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// What tools and pages share: making channels, invites, and taking people
// out (the outbox then takes their agents out of the groups: jobs.go).

const (
	defaultInviteTTL  = 7 * 24 * time.Hour
	maxInviteTTL      = 30 * 24 * time.Hour
	defaultInviteUses = 25
	maxInviteUses     = 500
)

// createChannel makes a channel: in org (by slug, which the person must
// manage), or outside orgs (its name gets a code). Its group is made in
// Wakeshift; if that fails, the channel isn't kept.
func (a *App) createChannel(ctx context.Context, person, orgSlug, name, topic string) (Channel, error) {
	org := ""
	if orgSlug != "" {
		o, err := a.Store.OrgFor(ctx, person, strings.ToLower(strings.TrimSpace(orgSlug)))
		if errors.Is(err, ErrNotFound) {
			return Channel{}, fmt.Errorf("you're not in an org %s", orgSlug)
		}
		if err != nil {
			return Channel{}, err
		}
		if !o.Manages() {
			return Channel{}, fmt.Errorf("only the owner and admins of %s make its channels", o.Name)
		}
		org = o.ID
	}
	var c Channel
	var err error
	for try := 0; try < 3; try++ {
		n := name
		if org == "" {
			n = name + "-" + newCode(5)
		}
		c, err = a.Store.CreateChannel(ctx, person, org, n, topic, func(id string) string { return channelPrefix(a.Kit.ID) + id })
		if !uniqueViolation(err) || org != "" {
			break
		}
	}
	if uniqueViolation(err) {
		return Channel{}, fmt.Errorf("there's a channel %s already", name)
	}
	if errors.Is(err, errTooMany) {
		return Channel{}, errors.New("that's as many channels as you can have; close one first")
	}
	if err != nil {
		return Channel{}, err
	}
	if _, _, err := a.Kit.CreateGroup(ctx, c.GroupType, c.Ref(), nil); err != nil {
		if derr := a.Store.DropChannel(context.WithoutCancel(ctx), c.ID); derr != nil {
			slog.Warn("agentchat: dropping a channel whose group wasn't made", "err", derr)
		}
		return Channel{}, friendly(err, c.Ref())
	}
	if err := a.Store.MarkReady(ctx, c.ID); err != nil {
		// Not usable: its group goes now if it can (and Prune closes it
		// otherwise, with the row, after an hour).
		if cerr := a.Kit.CloseGroup(context.WithoutCancel(ctx), c.GroupType); cerr != nil {
			slog.Warn("agentchat: closing a channel's group that never became ready", "err", cerr)
		}
		return Channel{}, err
	}
	// The site's reader joins now, so the channel's history starts with it.
	if _, err := a.readChannel(ctx, c); err != nil {
		slog.Warn("agentchat: the reader joining a new channel", "err", err)
	}
	return c, nil
}

// inviteCode is the code in an invite code or link.
func inviteCode(raw string) string {
	raw = strings.TrimSpace(raw)
	if i := strings.LastIndex(raw, "/join/"); i >= 0 {
		raw = raw[i+len("/join/"):]
	}
	return strings.ToLower(strings.Trim(raw, "/ "))
}

var errBadInvite = errors.New("that invite doesn't work (it's wrong, expired, used up or revoked); ask for a new one")

// joinByCode admits the calling agent's person to a channel outside orgs
// with an invite code, and returns the channel. Org invites are accepted
// on the join page, which says what joining means.
func (a *App) joinByCode(ctx context.Context, _ *mcp.CallToolRequest, me Agent, raw string) (Channel, error) {
	code := inviteCode(raw)
	addr := "person:" + me.PersonID // agents' wrong codes count per person
	if !a.inviteLookupsLeft(ctx, addr) {
		return Channel{}, errors.New("too many wrong invite codes; try again in an hour")
	}
	in, err := a.Store.LiveInvite(ctx, code)
	if errors.Is(err, ErrNotFound) || len(code) != inviteCodeLen {
		a.badInvite(ctx, addr)
		return Channel{}, errBadInvite
	}
	if err != nil {
		return Channel{}, err
	}
	if in.OrgID != "" {
		return Channel{}, fmt.Errorf("that's an invite to the org %s: your user accepts it at %s/join/%s, then you can join its channels", in.OrgName, a.URL, code)
	}
	if _, err := a.Store.AcceptInvite(ctx, code, me.PersonID); errors.Is(err, ErrBanned) {
		return Channel{}, errors.New("you can't join that channel: you were removed from it")
	} else if errors.Is(err, ErrNotFound) {
		return Channel{}, errBadInvite
	} else if err != nil {
		return Channel{}, err
	}
	return a.Store.ChannelFor(ctx, me.PersonID, in.ChannelID)
}

// inviteLookupsLeft: an address (or person, for agents) hasn't looked up
// too many wrong invite codes this hour.
func (a *App) inviteLookupsLeft(ctx context.Context, addr string) bool {
	if addr == "" {
		return true
	}
	n, err := a.Store.CountRequests(ctx, "invite", nil, addr, time.Now().Add(-time.Hour))
	if err != nil {
		slog.Warn("agentchat: reading the invite limit", "err", err)
		return false // fail closed: no guessing while the count is unknown
	}
	return n < badInvitesPerAdr
}

func (a *App) badInvite(ctx context.Context, addr string) {
	if addr != "" {
		if err := a.Store.RecordRequest(ctx, "invite", nil, addr); err != nil {
			slog.Warn("agentchat: recording a wrong invite code", "err", err)
		}
	}
}

// removeFromChannel takes a person (and all their agents) out of a channel.
func (a *App) removeFromChannel(ctx context.Context, by string, c Channel, person string, ban bool) error {
	err := a.Store.RemoveFromChannel(ctx, by, c, person, ban)
	if errors.Is(err, ErrNotFound) {
		return errors.New("they aren't a member you can remove (a channel's or org's owner can't be removed, and only an org's owner can remove its admins)")
	}
	if err == nil {
		a.kickOutbox()
	}
	return err
}
