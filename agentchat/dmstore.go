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
	SELECT c.id, '', '', '', c.name, c.topic, coalesce(c.created_by, ''), '', c.dm_agent
	FROM channels c JOIN agents a ON a.id = c.dm_agent AND a.person_id = c.created_by AND a.revoked_at IS NULL
	WHERE c.dm_agent IS NOT NULL AND c.closed_at IS NULL`

func scanDM(row pgx.Row) (Channel, error) {
	var c Channel
	err := row.Scan(&c.ID, &c.OrgID, &c.OrgSlug, &c.OrgName, &c.Name, &c.Topic, &c.CreatedBy, &c.Role, &c.DMAgent)
	return c, notFound(err)
}

// DMFor is a person's DM by its ID (theirs, with their agent).
func (s *Store) DMFor(ctx context.Context, person, id string) (Channel, error) {
	return scanDM(s.Pool.QueryRow(ctx, dmSelect+` AND c.created_by = $1 AND c.id = $2`, person, id))
}

// dmWith is a person's DM with one of their agents.
func (s *Store) dmWith(ctx context.Context, person, agent string) (Channel, error) {
	return scanDM(s.Pool.QueryRow(ctx, dmSelect+` AND c.created_by = $1 AND c.dm_agent = $2`, person, agent))
}

// CreateDM stores a person's DM with one of their connected agents.
// ErrNotFound if the agent isn't theirs or is disconnected; a unique
// violation if it has one already.
func (s *Store) CreateDM(ctx context.Context, person, agent string) (Channel, error) {
	c := Channel{ID: newID(), CreatedBy: person, DMAgent: agent}
	c.Name = "dm-" + c.ID
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		// The agent's row, shared: a disconnect (which updates it, then
		// closes its DM) waits, or this sees it disconnected.
		var held string
		if err := tx.QueryRow(ctx, `SELECT id FROM agents WHERE id = $1 AND person_id = $2 AND revoked_at IS NULL FOR SHARE`, agent, person).Scan(&held); err != nil {
			return notFound(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO channels (id, name, created_by, dm_agent) VALUES ($1, $2, $3, $4)`,
			c.ID, c.Name, person, agent); err != nil {
			return err
		}
		return audit(ctx, tx, person, "", "dm.created", map[string]any{"channel": c.ID, "agent": agent})
	})
	return c, err
}

// closeDMs closes the open DMs a query's rows name. A DM has no
// memberships (it reaches its agent's watching conversations); its messages
// go with its row.
func closeDMs(ctx context.Context, tx pgx.Tx, where string, args ...any) error {
	_, err := tx.Exec(ctx, `UPDATE channels SET closed_at = now() WHERE dm_agent IS NOT NULL AND closed_at IS NULL AND `+where, args...)
	return err
}
