package agentchat

import (
	"cmp"
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// Channels: in orgs, and outside them.

// ── Channels ─────────────────────────────────────────────────────────────

// Channel is a channel, as a person sees it.
type Channel struct {
	ID, OrgID, OrgSlug, OrgName, Name, Topic, CreatedBy string
	// Role is the person's in the channel's org (owner, admin, member), or
	// in the channel outside orgs (owner, member).
	Role string
	// DMAgent, for a person's direct messages with one of their agents
	// (dm.go), is that agent's ID.
	DMAgent string
}

// Ref is how agents name the channel: org/name, or its name outside orgs;
// a DM is "dm" (each agent has one).
func (c Channel) Ref() string {
	if c.DMAgent != "" {
		return dmRef
	}
	if c.OrgSlug != "" {
		return c.OrgSlug + "/" + c.Name
	}
	return c.Name
}

// Manages says whether the person can remove and ban members, invite
// (outside orgs) and close it.
func (c Channel) Manages() bool { return c.Role == "owner" || c.Role == "admin" }

// visible is every open channel a person may use: their orgs' (unless
// banned from the channel), and the channels outside orgs they're in.
// Never a DM (dmstore.go).
const visible = `
	SELECT c.id, coalesce(c.org_id, ''), coalesce(o.slug, ''), coalesce(o.name, ''), c.name, c.topic, coalesce(c.created_by, ''),
		coalesce(om.role, cm.role)
	FROM channels c
	LEFT JOIN orgs o ON o.id = c.org_id
	LEFT JOIN org_members om ON om.org_id = c.org_id AND om.person_id = $1
	LEFT JOIN channel_members cm ON cm.channel_id = c.id AND cm.person_id = $1
	WHERE c.closed_at IS NULL AND c.dm_agent IS NULL AND (om.person_id IS NOT NULL OR cm.person_id IS NOT NULL)
		AND NOT EXISTS (SELECT 1 FROM bans b WHERE b.channel_id = c.id AND b.person_id = $1)`

func scanChannel(row pgx.Row) (Channel, error) {
	var c Channel
	err := row.Scan(&c.ID, &c.OrgID, &c.OrgSlug, &c.OrgName, &c.Name, &c.Topic, &c.CreatedBy, &c.Role)
	return c, notFound(err)
}

// Channels are the channels a person may use, by org then name.
func (s *Store) Channels(ctx context.Context, person string) ([]Channel, error) {
	rows, err := s.Pool.Query(ctx, visible+` ORDER BY o.slug NULLS LAST, c.name`, person)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Channel, error) { return scanChannel(r) })
}

// ChannelFor is a channel by ID, if the person may use it.
func (s *Store) ChannelFor(ctx context.Context, person, id string) (Channel, error) {
	return scanChannel(s.Pool.QueryRow(ctx, visible+` AND c.id = $2`, person, id))
}

// OrgChannels are an org's open channels.
func (s *Store) OrgChannels(ctx context.Context, org string) ([]Channel, error) {
	rows, err := s.Pool.Query(ctx, `SELECT c.id, c.org_id, o.slug, o.name, c.name, c.topic, coalesce(c.created_by, ''), ''
		FROM channels c JOIN orgs o ON o.id = c.org_id WHERE c.org_id = $1 AND c.closed_at IS NULL ORDER BY c.name`, org)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Channel, error) { return scanChannel(r) })
}

// CreateChannel stores a new channel (in org, or outside orgs owned by
// person), ready to use.
func (s *Store) CreateChannel(ctx context.Context, person, org, name, topic string) (Channel, error) {
	c := Channel{ID: newID(), OrgID: org, Name: name, Topic: topic, CreatedBy: person, Role: "owner"}
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := limitLock(ctx, tx, "channels:"+cmp.Or(org, "person:"+person)); err != nil {
			return err
		}
		if org != "" {
			if err := authorize(ctx, tx, person, org, "", manager); err != nil {
				return err
			}
		}
		var n int
		if org != "" {
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM channels WHERE org_id = $1 AND closed_at IS NULL`, org).Scan(&n); err != nil {
				return err
			}
			if n >= maxOrgChannels {
				return errTooMany
			}
		} else {
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM channel_members WHERE person_id = $1 AND role = 'owner'`, person).Scan(&n); err != nil {
				return err
			}
			if n >= maxOwnedChannels {
				return errTooMany
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO channels (id, org_id, name, topic, created_by) VALUES ($1, nullif($2, ''), $3, $4, $5)`,
			c.ID, org, name, topic, person); err != nil {
			return err
		}
		if org == "" {
			if _, err := tx.Exec(ctx, `INSERT INTO channel_members (channel_id, person_id, role) VALUES ($1, $2, 'owner')`, c.ID, person); err != nil {
				return err
			}
		} else if err := tx.QueryRow(ctx, `SELECT o.slug, o.name, m.role FROM orgs o JOIN org_members m ON m.org_id = o.id AND m.person_id = $2 WHERE o.id = $1`,
			org, person).Scan(&c.OrgSlug, &c.OrgName, &c.Role); err != nil {
			return notFound(err)
		}
		return audit(ctx, tx, person, org, "channel.created", map[string]any{"channel": c.ID, "name": name})
	})
	return c, err
}

// OrgRole is a person's role in an org.
func (s *Store) OrgRole(ctx context.Context, org, person string) (string, error) {
	var role string
	err := s.Pool.QueryRow(ctx, `SELECT role FROM org_members WHERE org_id = $1 AND person_id = $2`, org, person).Scan(&role)
	return role, notFound(err)
}

// OrgChannel is one of an org's open channels.
func (s *Store) OrgChannel(ctx context.Context, org, id string) (Channel, error) {
	return scanChannel(s.Pool.QueryRow(ctx, `SELECT c.id, c.org_id, o.slug, o.name, c.name, c.topic, coalesce(c.created_by, ''), ''
		FROM channels c JOIN orgs o ON o.id = c.org_id WHERE c.org_id = $1 AND c.id = $2 AND c.closed_at IS NULL`, org, id))
}

// ChannelsNamed are the channels a person may use called name.
func (s *Store) ChannelsNamed(ctx context.Context, person, name string) ([]Channel, error) {
	rows, err := s.Pool.Query(ctx, visible+` AND c.name = $2 ORDER BY o.slug NULLS LAST`, person, name)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Channel, error) { return scanChannel(r) })
}

// CloseChannel closes a channel: every agent leaves it. Its messages go
// with its row, 30 days later (Prune).
func (s *Store) CloseChannel(ctx context.Context, by, id string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := authorize(ctx, tx, by, "", id, manager); err != nil {
			return err
		}
		var org string
		err := tx.QueryRow(ctx, `UPDATE channels SET closed_at = now() WHERE id = $1 AND closed_at IS NULL RETURNING coalesce(org_id, '')`, id).Scan(&org)
		if err != nil {
			return notFound(err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM channel_agents WHERE channel_id = $1`, id); err != nil {
			return err
		}
		return audit(ctx, tx, by, org, "channel.closed", map[string]any{"channel": id})
	})
}

// ChannelMembers are the people admitted to a channel outside orgs.
func (s *Store) ChannelMembers(ctx context.Context, channel string) ([]Member, error) {
	rows, err := s.Pool.Query(ctx, `SELECT p.id, p.name, coalesce(p.handle, ''), m.role, m.added_at FROM channel_members m JOIN people p ON p.id = m.person_id
		WHERE m.channel_id = $1 ORDER BY m.role DESC, p.name`, channel)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Member, error) {
		var m Member
		err := r.Scan(&m.PersonID, &m.Name, &m.Handle, &m.Role, &m.Since)
		return m, err
	})
}

// RemoveFromChannel takes person's agents out of a channel (by its owner,
// or an org admin; or the person leaving), and out of its members (outside
// orgs). With ban, they can't come back. From an org channel, removing
// someone else always bans them from it: org members may otherwise join any
// of its channels (an admin lifts it on the org's page).
func (s *Store) RemoveFromChannel(ctx context.Context, by string, c Channel, person string, ban bool) error {
	if c.OrgID != "" && by != person {
		ban = true
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		// The channel's row, first: an invite being accepted meanwhile
		// waits, then sees the ban.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM channels WHERE id = $1 FOR UPDATE`, c.ID); err != nil {
			return err
		}
		if by != person {
			if err := authorize(ctx, tx, by, "", c.ID, manager); err != nil {
				return err
			}
		}
		if c.OrgID != "" && by != person {
			// Never the org's owner; an admin only by the owner (as on the
			// org's page, orgKeepOut, but here, in the same transaction, so
			// the agents' remove_member holds to it too, and a change of
			// role meanwhile counts).
			// Both rows locked (authorize locked by's): a promotion meanwhile
			// waits for this removal, or this check sees it. Someone no longer
			// in the org has no role, and may be kept out like a member.
			var target, byRole string
			err := tx.QueryRow(ctx, `SELECT role FROM org_members WHERE org_id = $1 AND person_id = $2 FOR SHARE`, c.OrgID, person).Scan(&target)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if err := tx.QueryRow(ctx, `SELECT role FROM org_members WHERE org_id = $1 AND person_id = $2 FOR SHARE`, c.OrgID, by).Scan(&byRole); err != nil {
				return notFound(err)
			}
			if target == "owner" || target == "admin" && byRole != "owner" {
				return ErrNotFound
			}
		}
		if c.OrgID == "" {
			tag, err := tx.Exec(ctx, `DELETE FROM channel_members WHERE channel_id = $1 AND person_id = $2 AND role <> 'owner'`, c.ID, person)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return ErrNotFound
			}
		}
		if ban {
			if _, err := tx.Exec(ctx, `INSERT INTO bans (channel_id, person_id, banned_by) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, c.ID, person, by); err != nil {
				return err
			}
		}
		if err := endChannelsFor(ctx, tx, person, `SELECT $2::text`, c.ID); err != nil {
			return err
		}
		action := "channel.member_removed"
		if by == person {
			action = "channel.left"
		}
		return audit(ctx, tx, by, c.OrgID, action, map[string]any{"channel": c.ID, "person": person, "banned": ban})
	})
}
