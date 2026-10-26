package agentchat

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Invite links, to an org or a channel outside orgs.

// ── Invites ──────────────────────────────────────────────────────────────

// Invite is an invite link's record.
type Invite struct {
	Code, OrgID, ChannelID, CreatedBy string
	Expires                           time.Time
	MaxUses, Uses                     int
	// What it's for, for its page.
	OrgName, OrgSlug, ChannelName, InviterName string
}

// CreateInvite makes an invite to an org or a channel (one of them).
func (s *Store) CreateInvite(ctx context.Context, by, org, channel string, ttl time.Duration, maxUses int) (Invite, error) {
	in := Invite{Code: newCode(inviteCodeLen), OrgID: org, ChannelID: channel, CreatedBy: by, Expires: time.Now().Add(ttl), MaxUses: maxUses}
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := limitLock(ctx, tx, "invites:"+org+":"+channel); err != nil {
			return err
		}
		if err := authorize(ctx, tx, by, org, channel, manager); err != nil {
			return err
		}
		if channel != "" {
			// Only channels outside orgs have their own invites: an org's
			// members join its channels, people join the org.
			var lone bool
			if err := tx.QueryRow(ctx, `SELECT org_id IS NULL FROM channels WHERE id = $1 AND closed_at IS NULL`, channel).Scan(&lone); err != nil {
				return notFound(err)
			}
			if !lone {
				return ErrNotFound
			}
		}
		var live int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM invites WHERE ((org_id = $1 AND $1 <> '') OR (channel_id = $2 AND $2 <> '')) AND revoked_at IS NULL AND expires_at > now()`,
			org, channel).Scan(&live); err != nil {
			return err
		}
		if live >= maxLiveInvites {
			return errTooMany
		}
		if _, err := tx.Exec(ctx, `INSERT INTO invites (code, org_id, channel_id, created_by, expires_at, max_uses) VALUES ($1, nullif($2, ''), nullif($3, ''), $4, $5, $6)`,
			in.Code, org, channel, by, in.Expires, maxUses); err != nil {
			return err
		}
		return audit(ctx, tx, by, org, "invite.created", map[string]any{"channel": channel, "max_uses": maxUses, "expires_at": in.Expires.UTC().Format(time.RFC3339)})
	})
	return in, err
}

// Invites are an org's or a channel's live invites.
func (s *Store) Invites(ctx context.Context, org, channel string) ([]Invite, error) {
	rows, err := s.Pool.Query(ctx, `SELECT i.code, coalesce(i.org_id, ''), coalesce(i.channel_id, ''), i.created_by, i.expires_at, i.max_uses, i.uses, coalesce(p.name, '')
		FROM invites i LEFT JOIN people p ON p.id = i.created_by
		WHERE ((i.org_id = $1 AND $1 <> '') OR (i.channel_id = $2 AND $2 <> '')) AND i.revoked_at IS NULL AND i.expires_at > now() AND i.uses < i.max_uses
		ORDER BY i.created_at DESC`, org, channel)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Invite, error) {
		var in Invite
		err := r.Scan(&in.Code, &in.OrgID, &in.ChannelID, &in.CreatedBy, &in.Expires, &in.MaxUses, &in.Uses, &in.InviterName)
		return in, err
	})
}

// RevokeInvite ends an invite of an org or channel.
func (s *Store) RevokeInvite(ctx context.Context, by, org, channel, code string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := authorize(ctx, tx, by, org, channel, manager); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE invites SET revoked_at = now() WHERE code = $1 AND ((org_id = $2 AND $2 <> '') OR (channel_id = $3 AND $3 <> '')) AND revoked_at IS NULL`,
			code, org, channel)
		if err != nil || tag.RowsAffected() == 0 {
			return cmpErr(err, ErrNotFound)
		}
		return audit(ctx, tx, by, org, "invite.revoked", map[string]any{"channel": channel})
	})
}

// LiveInvite is an invite that still works (not revoked, expired or used
// up, and its org or channel still open), with what it's for.
func (s *Store) LiveInvite(ctx context.Context, code string) (Invite, error) {
	var in Invite
	err := s.Pool.QueryRow(ctx, `SELECT i.code, coalesce(i.org_id, ''), coalesce(i.channel_id, ''), i.created_by, i.expires_at, i.max_uses, i.uses,
			coalesce(o.name, ''), coalesce(o.slug, ''), coalesce(c.name, ''), p.name
		FROM invites i JOIN people p ON p.id = i.created_by LEFT JOIN orgs o ON o.id = i.org_id LEFT JOIN channels c ON c.id = i.channel_id
		WHERE i.code = $1 AND i.revoked_at IS NULL AND i.expires_at > now() AND i.uses < i.max_uses AND (c.id IS NULL OR c.closed_at IS NULL AND c.org_id IS NULL)`, code).
		Scan(&in.Code, &in.OrgID, &in.ChannelID, &in.CreatedBy, &in.Expires, &in.MaxUses, &in.Uses, &in.OrgName, &in.OrgSlug, &in.ChannelName, &in.InviterName)
	return in, notFound(err)
}

// ErrBanned: the person is banned from where the invite leads.
var ErrBanned = errors.New("banned")

// AcceptInvite admits person where the invite leads, counting a use if
// they're new there. It returns the invite.
func (s *Store) AcceptInvite(ctx context.Context, code, person string) (Invite, error) {
	var in Invite
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT i.code, coalesce(i.org_id, ''), coalesce(i.channel_id, ''), i.created_by FROM invites i LEFT JOIN channels c ON c.id = i.channel_id
			WHERE i.code = $1 AND i.revoked_at IS NULL AND i.expires_at > now() AND i.uses < i.max_uses AND (c.id IS NULL OR c.closed_at IS NULL AND c.org_id IS NULL) FOR UPDATE OF i`, code).
			Scan(&in.Code, &in.OrgID, &in.ChannelID, &in.CreatedBy)
		if err != nil {
			return notFound(err)
		}
		// The org's or channel's row, locked first: a removal with a ban
		// meanwhile is seen (it locks the same row), and an org's member
		// limit holds across invites.
		if in.OrgID != "" {
			var locked string
			if err := tx.QueryRow(ctx, `SELECT id FROM orgs WHERE id = $1 FOR UPDATE`, in.OrgID).Scan(&locked); err != nil {
				return notFound(err)
			}
		} else {
			// Shared: a close waits for this, or this sees it.
			var open bool
			if err := tx.QueryRow(ctx, `SELECT closed_at IS NULL FROM channels WHERE id = $1 FOR SHARE`, in.ChannelID).Scan(&open); err != nil {
				return notFound(err)
			}
			if !open {
				return ErrNotFound
			}
		}
		var banned bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM bans WHERE person_id = $1 AND ((org_id = $2 AND $2 <> '') OR (channel_id = $3 AND $3 <> '')))`,
			person, in.OrgID, in.ChannelID).Scan(&banned); err != nil {
			return err
		}
		if banned {
			return ErrBanned
		}
		var tag pgconn.CommandTag
		if in.OrgID != "" {
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM org_members WHERE org_id = $1`, in.OrgID).Scan(&n); err != nil {
				return err
			}
			tag, err = tx.Exec(ctx, `INSERT INTO org_members (org_id, person_id, role) VALUES ($1, $2, 'member') ON CONFLICT DO NOTHING`, in.OrgID, person)
			if err == nil && tag.RowsAffected() > 0 && n >= maxOrgMembers {
				return errTooMany // new, and no room (a member already in gets through)
			}
		} else {
			tag, err = tx.Exec(ctx, `INSERT INTO channel_members (channel_id, person_id, role) VALUES ($1, $2, 'member') ON CONFLICT DO NOTHING`, in.ChannelID, person)
		}
		if err != nil || tag.RowsAffected() == 0 {
			return err // already in: no use counted
		}
		if _, err := tx.Exec(ctx, `UPDATE invites SET uses = uses + 1 WHERE code = $1`, code); err != nil {
			return err
		}
		return audit(ctx, tx, person, in.OrgID, "invite.accepted", map[string]any{"channel": in.ChannelID, "invited_by": in.CreatedBy})
	})
	return in, err
}
