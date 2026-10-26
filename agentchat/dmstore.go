package agentchat

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Direct messages between a person and one of their own agents (dm.go).
// A DM is a channels row with dm_agent set, outside orgs, created_by its
// person, and with no channel_members: the channel queries (visible) never
// return it, so no page, tool, invite or setting for channels reaches it.
// Only these queries do, and each requires the person and the agent to be
// the DM's own, the agent still connected.

const dmSelect = `
	SELECT c.id, '', '', '', c.name, c.topic, c.group_type, coalesce(c.created_by, ''), '', c.dm_agent, c.ready
	FROM channels c JOIN agents a ON a.id = c.dm_agent AND a.person_id = c.created_by AND a.revoked_at IS NULL
	WHERE c.dm_agent IS NOT NULL AND c.closed_at IS NULL`

func scanDM(row pgx.Row) (Channel, bool, error) {
	var c Channel
	var ready bool
	err := row.Scan(&c.ID, &c.OrgID, &c.OrgSlug, &c.OrgName, &c.Name, &c.Topic, &c.GroupType, &c.CreatedBy, &c.Role, &c.DMAgent, &ready)
	return c, ready, notFound(err)
}

// DMFor is a person's DM by its ID (ready, theirs, with their agent).
func (s *Store) DMFor(ctx context.Context, person, id string) (Channel, error) {
	c, _, err := scanDM(s.Pool.QueryRow(ctx, dmSelect+` AND c.ready AND c.created_by = $1 AND c.id = $2`, person, id))
	return c, err
}

// dmWith is a person's DM with one of their agents, ready or not.
func (s *Store) dmWith(ctx context.Context, person, agent string) (Channel, bool, error) {
	return scanDM(s.Pool.QueryRow(ctx, dmSelect+` AND c.created_by = $1 AND c.dm_agent = $2`, person, agent))
}

// DMs are a person's ready DMs: their channel IDs, by agent.
func (s *Store) DMs(ctx context.Context, person string) (map[string]string, error) {
	rows, err := s.Pool.Query(ctx, dmSelect+` AND c.ready AND c.created_by = $1`, person)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	defer rows.Close()
	for rows.Next() {
		c, _, err := scanDM(rows)
		if err != nil {
			return nil, err
		}
		out[c.DMAgent] = c.ID
	}
	return out, rows.Err()
}

// CreateDM stores a person's DM with one of their connected agents (not
// ready: making its group is the caller's). ErrNotFound if the agent isn't
// theirs or is disconnected; a unique violation if it has one already.
func (s *Store) CreateDM(ctx context.Context, person, agent string, groupType func(id string) string) (Channel, error) {
	c := Channel{ID: newID(), CreatedBy: person, DMAgent: agent}
	c.GroupType, c.Name = groupType(c.ID), "dm-"+c.ID
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		// The agent's row, shared: a disconnect (which updates it, then
		// closes its DM) waits, or this sees it disconnected.
		var held string
		if err := tx.QueryRow(ctx, `SELECT id FROM agents WHERE id = $1 AND person_id = $2 AND revoked_at IS NULL FOR SHARE`, agent, person).Scan(&held); err != nil {
			return notFound(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO channels (id, name, created_by, group_type, dm_agent) VALUES ($1, $2, $3, $4, $5)`,
			c.ID, c.Name, person, c.GroupType, agent); err != nil {
			return err
		}
		return audit(ctx, tx, person, "", "dm.created", map[string]any{"channel": c.ID, "agent": agent})
	})
	return c, err
}

// closeDMs closes the open DMs a query's rows name (by channel ID): each
// group closes (outbox).
func closeDMs(ctx context.Context, tx pgx.Tx, why, where string, args ...any) error {
	rows, err := tx.Query(ctx, `UPDATE channels SET closed_at = now() WHERE dm_agent IS NOT NULL AND closed_at IS NULL AND `+where+` RETURNING group_type`, args...)
	if err != nil {
		return err
	}
	groups, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, g := range groups {
		if _, err := tx.Exec(ctx, `INSERT INTO membership_ends (group_type, workspace, reason) VALUES ($1, '', $2)`, g, why); err != nil {
			return err
		}
	}
	return nil
}
