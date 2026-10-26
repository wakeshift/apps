package agentchat

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Deleting accounts, the outboxes of what Wakeshift still has to do, and
// retention.

// ── Deleting an account ──────────────────────────────────────────────────

// DeleteAccount deletes a person and everything of theirs, at once: their
// orgs pass to their longest-standing admin, or are deleted (their
// channels' groups closed); channels outside orgs they own pass to their
// longest-standing member, or are closed. A deletions row (the outbox
// that forgets their Wakeshift workspace) and an audit entry (with no
// email) are written in the same transaction.
func (s *Store) DeleteAccount(ctx context.Context, person string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var held string
		if err := tx.QueryRow(ctx, `SELECT id FROM people WHERE id = $1 FOR UPDATE`, person).Scan(&held); err != nil {
			return notFound(err) // deleted already
		}
		rows, _ := tx.Query(ctx, `SELECT org_id FROM org_members WHERE person_id = $1 AND role = 'owner'`, person)
		owned, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for _, org := range owned {
			var heir string
			err := tx.QueryRow(ctx, `SELECT person_id FROM org_members WHERE org_id = $1 AND role = 'admin' ORDER BY joined_at LIMIT 1`, org).Scan(&heir)
			if errors.Is(err, pgx.ErrNoRows) {
				if err := deleteOrg(ctx, tx, person, org); err != nil {
					return err
				}
				continue
			}
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE org_members SET role = 'owner' WHERE org_id = $1 AND person_id = $2`, org, heir); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE orgs SET owner_id = $2 WHERE id = $1`, org, heir); err != nil {
				return err
			}
			if err := audit(ctx, tx, person, org, "org.owner_changed", map[string]any{"owner": heir, "why": "account_deleted"}); err != nil {
				return err
			}
		}
		rows, _ = tx.Query(ctx, `SELECT channel_id FROM channel_members m JOIN channels c ON c.id = m.channel_id
			WHERE m.person_id = $1 AND m.role = 'owner' AND c.closed_at IS NULL`, person)
		chans, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for _, ch := range chans {
			tag, err := tx.Exec(ctx, `UPDATE channel_members SET role = 'owner' WHERE channel_id = $1 AND person_id = (
				SELECT person_id FROM channel_members WHERE channel_id = $1 AND person_id <> $2 ORDER BY added_at LIMIT 1)`, ch, person)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				var group string
				if err := tx.QueryRow(ctx, `UPDATE channels SET closed_at = now() WHERE id = $1 RETURNING group_type`, ch).Scan(&group); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `INSERT INTO membership_ends (group_type, workspace, reason) VALUES ($1, '', 'owner_deleted')`, group); err != nil {
					return err
				}
			}
		}
		// Their DMs close, with their groups, and go (open or closed: their
		// agents go below).
		if err := closeDMs(ctx, tx, "owner_deleted", "created_by = $1", person); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM channels WHERE dm_agent IN (SELECT id FROM agents WHERE person_id = $1)`, person); err != nil {
			return err
		}
		// Their Wakeshift workspace goes after (deletions), which ends their
		// agents' memberships everywhere: no membership_ends needed.
		if _, err := tx.Exec(ctx, `DELETE FROM membership_ends WHERE workspace = (SELECT workspace FROM people WHERE id = $1) AND workspace <> ''`, person); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO deletions (person_id) VALUES ($1) ON CONFLICT DO NOTHING`, person); err != nil {
			return err
		}
		// Their own audit entries name them only by ID; keep the deletion.
		if _, err := tx.Exec(ctx, `DELETE FROM audit_log WHERE person_id = $1`, person); err != nil {
			return err
		}
		if err := audit(ctx, tx, person, "", "person.deleted", nil); err != nil {
			return err
		}
		// Sessions, links' person, agents, codes, tokens, memberships and
		// bans cascade. Their sign-in links are by email: gone too.
		if _, err := tx.Exec(ctx, `DELETE FROM sign_in_links WHERE email = (SELECT email FROM people WHERE id = $1)`, person); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM people WHERE id = $1`, person)
		return err
	})
}

// PendingDeletions claims (for two minutes) people whose Wakeshift workspace is still to be
// forgotten, fewest attempts first.
func (s *Store) PendingDeletions(ctx context.Context, limit int) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `UPDATE deletions SET claimed_until = now() + interval '2 minutes' WHERE person_id IN (
		SELECT person_id FROM deletions WHERE claimed_until IS NULL OR claimed_until < now() ORDER BY attempts, requested_at LIMIT $1 FOR UPDATE SKIP LOCKED)
		RETURNING person_id`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// DeletionDone finishes a deletion.
func (s *Store) DeletionDone(ctx context.Context, person string) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM deletions WHERE person_id = $1`, person)
	return err
}

// DeletionFailed counts a failed attempt (stage says where, never the
// error's text).
func (s *Store) DeletionFailed(ctx context.Context, person, stage string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE deletions SET attempts = attempts + 1, last_error = $2, claimed_until = NULL WHERE person_id = $1`, person, stage)
	return err
}

// ── Outbox ───────────────────────────────────────────────────────────────

// Ending is a membership_ends row.
type Ending struct {
	ID                               int64
	GroupType, Workspace, Agent, Why string
}

// PendingEnds claims membership ends not done yet (for two minutes, so
// another instance leaves them), fewest attempts first.
func (s *Store) PendingEnds(ctx context.Context, limit int) ([]Ending, error) {
	rows, err := s.Pool.Query(ctx, `UPDATE membership_ends SET claimed_until = now() + interval '2 minutes' WHERE id IN (
		SELECT id FROM membership_ends WHERE claimed_until IS NULL OR claimed_until < now() ORDER BY attempts, id LIMIT $1 FOR UPDATE SKIP LOCKED)
		RETURNING id, group_type, workspace, agent, reason`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Ending, error) {
		var e Ending
		err := r.Scan(&e.ID, &e.GroupType, &e.Workspace, &e.Agent, &e.Why)
		return e, err
	})
}

// EndDone removes a done membership end.
func (s *Store) EndDone(ctx context.Context, id int64) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM membership_ends WHERE id = $1`, id)
	return err
}

// EndFailed counts a failed attempt.
func (s *Store) EndFailed(ctx context.Context, id int64, stage string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE membership_ends SET attempts = attempts + 1, last_error = $2, claimed_until = NULL WHERE id = $1`, id, stage)
	return err
}

// ── Retention ────────────────────────────────────────────────────────────

// Prune deletes what's past keeping.
func (s *Store) Prune(ctx context.Context) error {
	// One transaction: a never-ready channel's group is queued to close in
	// the same one that deletes its row.
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		for _, q := range []string{
			`DELETE FROM sign_in_links WHERE expires_at < now() - interval '1 day'`,
			`DELETE FROM sign_in_requests WHERE at < now() - interval '1 day'`,
			`DELETE FROM sessions WHERE expires_at < now()`,
			`DELETE FROM oauth_codes WHERE expires_at < now() - interval '1 day'`,
			`DELETE FROM refresh_tokens WHERE expires_at < now()`,
			`DELETE FROM invites WHERE expires_at < now() - interval '30 days' OR revoked_at < now() - interval '30 days'`,
			`DELETE FROM channels WHERE closed_at < now() - interval '30 days'`,
			// A channel whose group may exist but that never became ready: its group
			// closes (outbox), and its row goes.
			`INSERT INTO membership_ends (group_type, workspace, reason) SELECT group_type, '', 'never_ready' FROM channels WHERE NOT ready AND created_at < now() - interval '1 hour'`,
			`DELETE FROM channels WHERE NOT ready AND created_at < now() - interval '1 hour'`,
			`DELETE FROM audit_log WHERE at < now() - interval '1 year'`,
		} {
			if _, err := tx.Exec(ctx, q); err != nil {
				return err
			}
		}
		return nil
	})
}

// Log is a person's latest audit entries.
func (s *Store) Log(ctx context.Context, person string, limit int) ([]Entry, error) {
	rows, err := s.Pool.Query(ctx, `SELECT person_id, action, detail, at FROM audit_log WHERE person_id = $1 ORDER BY id DESC LIMIT $2`, person, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Entry, error) {
		var e Entry
		var b []byte
		err := r.Scan(&e.PersonID, &e.Action, &b, &e.At)
		json.Unmarshal(b, &e.Detail)
		return e, err
	})
}

// Entry is an audit entry.
type Entry struct {
	PersonID string // who did it
	Action   string
	Detail   map[string]any
	At       time.Time
}
