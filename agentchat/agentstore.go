package agentchat

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Connected agents and their OAuth records.

// ── Agents and OAuth ─────────────────────────────────────────────────────

// Agent is an agent someone connected.
type Agent struct {
	ID, PersonID, ClientName, Workspace, Agent, Handle string
	// Label is the name its conversation gave itself (watch's label,
	// "pipe"), shown as "Pipe".
	Label               string
	CreatedAt, LastUsed time.Time
	Revoked             bool
}

const agentCols = `id, person_id, client_name, workspace, agent, handle, label, created_at, last_used_at, revoked_at IS NOT NULL`

func scanAgent(row pgx.Row) (Agent, error) {
	var a Agent
	err := row.Scan(&a.ID, &a.PersonID, &a.ClientName, &a.Workspace, &a.Agent, &a.Handle, &a.Label, &a.CreatedAt, &a.LastUsed, &a.Revoked)
	return a, notFound(err)
}

// AgentByID is an agent.
func (s *Store) AgentByID(ctx context.Context, id string) (Agent, error) {
	return scanAgent(s.Pool.QueryRow(ctx, `SELECT `+agentCols+` FROM agents WHERE id = $1`, id))
}

// LiveAgentFor is a person's connected agent for an OAuth client.
func (s *Store) LiveAgentFor(ctx context.Context, person string, clientHash []byte) (Agent, error) {
	return scanAgent(s.Pool.QueryRow(ctx, `SELECT `+agentCols+` FROM agents WHERE person_id = $1 AND client_hash = $2 AND revoked_at IS NULL`, person, clientHash))
}

// AgentByWakeshift is the live agent acting as agent in workspace.
func (s *Store) AgentByWakeshift(ctx context.Context, workspace, agent string) (Agent, error) {
	return scanAgent(s.Pool.QueryRow(ctx, `SELECT `+agentCols+` FROM agents WHERE workspace = $1 AND agent = $2 AND revoked_at IS NULL`, workspace, agent))
}

// PeopleByWorkspace maps Wakeshift workspaces to their people.
func (s *Store) PeopleByWorkspace(ctx context.Context, workspaces []string) (map[string]Person, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+personCols+` FROM people WHERE workspace = ANY($1) AND workspace <> ''`, workspaces)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Person, error) { return scanPerson(r) })
	out := map[string]Person{}
	for _, p := range list {
		out[p.Workspace] = p
	}
	return out, err
}

// AgentsOf are a person's connected agents, newest first.
func (s *Store) AgentsOf(ctx context.Context, person string) ([]Agent, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+agentCols+` FROM agents WHERE person_id = $1 AND revoked_at IS NULL ORDER BY created_at DESC`, person)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Agent, error) { return scanAgent(r) })
}

// ConnectAgent records a person's new agent for an OAuth client: agent
// (a free Wakeshift agent ID in their workspace, from base) and handle in
// channels (their handle and the agent's ID).
func (s *Store) ConnectAgent(ctx context.Context, p Person, clientHash []byte, clientName, base string) (Agent, error) {
	for i := 1; i <= 50; i++ {
		id := base
		if i > 1 {
			suffix := "-" + strconv.Itoa(i)
			id = strings.TrimRight(base[:min(len(base), maxAgentID-len(suffix))], "-") + suffix
		}
		a := Agent{ID: newID(), PersonID: p.ID, ClientName: clientName, Workspace: p.Workspace, Agent: id, Handle: memberHandle(p.Handle, id)}
		err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
			if err := limitLock(ctx, tx, "agents:"+p.ID); err != nil {
				return err
			}
			var live int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM agents WHERE person_id = $1 AND revoked_at IS NULL`, p.ID).Scan(&live); err != nil {
				return err
			}
			if live >= maxAgents {
				return errTooMany
			}
			if _, err := tx.Exec(ctx, `INSERT INTO agents (id, person_id, client_hash, client_name, workspace, agent, handle) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
				a.ID, p.ID, clientHash, clientName, a.Workspace, a.Agent, a.Handle); err != nil {
				return err
			}
			return audit(ctx, tx, p.ID, "", "agent.connected", map[string]any{"agent": a.ID, "client": clientName, "as": id})
		})
		if uniqueViolation(err) {
			if again, err := s.LiveAgentFor(ctx, p.ID, clientHash); err == nil {
				return again, nil // the same client, approved twice at once
			}
			continue
		}
		return a, err
	}
	return Agent{}, errors.New("no free agent name")
}

// DisconnectAgent stops a person's agent: its tokens and codes end, its DM
// closes, and it leaves every channel (outbox).
func (s *Store) DisconnectAgent(ctx context.Context, person, id string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var ws, agent string
		err := tx.QueryRow(ctx, `UPDATE agents SET revoked_at = now() WHERE id = $1 AND person_id = $2 AND revoked_at IS NULL RETURNING workspace, agent`, id, person).
			Scan(&ws, &agent)
		if err != nil {
			return notFound(err)
		}
		for _, q := range []string{`DELETE FROM oauth_codes WHERE agent_id = $1`, `DELETE FROM refresh_tokens WHERE agent_id = $1`} {
			if _, err := tx.Exec(ctx, q, id); err != nil {
				return err
			}
		}
		// Its DM closes (and its group with it).
		if err := closeDMs(ctx, tx, "disconnected", "dm_agent = $1", id); err != nil {
			return err
		}
		// group_type '': every group the agent is in.
		if _, err := tx.Exec(ctx, `INSERT INTO membership_ends (group_type, workspace, agent, reason) VALUES ('', $1, $2, 'disconnected')`, ws, agent); err != nil {
			return err
		}
		return audit(ctx, tx, person, "", "agent.disconnected", map[string]any{"agent": id})
	})
}

// SetLabel records the label an agent's conversation gave itself.
func (s *Store) SetLabel(ctx context.Context, id, label string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE agents SET label = $2 WHERE id = $1 AND revoked_at IS NULL AND label <> $2`, id, label)
	return err
}

// TouchAgent notes an agent was used (at most every few minutes).
func (s *Store) TouchAgent(ctx context.Context, id string) {
	s.Pool.Exec(ctx, `UPDATE agents SET last_used_at = now() WHERE id = $1 AND last_used_at < now() - interval '5 minutes'`, id)
}

// Code is what an authorization code stands for.
type Code struct {
	AgentID, RedirectURI, Challenge string
	ClientHash                      []byte
}

// SaveCode stores an authorization code (hashed).
func (s *Store) SaveCode(ctx context.Context, code string, c Code, expires time.Time) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO oauth_codes (code_hash, agent_id, client_hash, redirect_uri, code_challenge, expires_at) VALUES ($1, $2, $3, $4, $5, $6)`,
		hash(code), c.AgentID, c.ClientHash, c.RedirectURI, c.Challenge, expires)
	return err
}

// TakeCode spends an authorization code: once, before it expires, and only
// by its client, with its redirect URI and the PKCE challenge of the
// verifier sent (a wrong one leaves the code for the right client).
func (s *Store) TakeCode(ctx context.Context, code string, clientHash []byte, redirectURI, challenge string) (string, error) {
	var agent string
	err := s.Pool.QueryRow(ctx, `DELETE FROM oauth_codes WHERE code_hash = $1 AND expires_at > now() AND client_hash = $2 AND redirect_uri = $3 AND code_challenge = $4
		AND agent_id IN (SELECT id FROM agents WHERE revoked_at IS NULL)
		RETURNING agent_id`, hash(code), clientHash, redirectURI, challenge).Scan(&agent)
	return agent, notFound(err)
}

// SaveRefresh stores a refresh token (hashed).
func (s *Store) SaveRefresh(ctx context.Context, token, agent string, clientHash []byte, expires time.Time) error {
	// Only for an agent still connected, read under a share lock: a
	// disconnect (which updates the agent, then deletes its tokens) waits for
	// this, so a token can't be stored for an agent being disconnected.
	tag, err := s.Pool.Exec(ctx, `INSERT INTO refresh_tokens (token_hash, agent_id, client_hash, expires_at)
		SELECT $1, id, $3, $4 FROM agents WHERE id = $2 AND revoked_at IS NULL FOR SHARE`, hash(token), agent, clientHash, expires)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// RotateRefresh spends a refresh token of the client (once, before it
// expires) and stores next in its place (good until expires), in one
// transaction, for an agent still connected: it returns the agent. If it
// fails, the old token stays good (unless its agent was disconnected).
func (s *Store) RotateRefresh(ctx context.Context, token string, clientHash []byte, next string, expires time.Time) (agent string, err error) {
	gone := false
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `DELETE FROM refresh_tokens WHERE token_hash = $1 AND client_hash = $2 AND expires_at > now() RETURNING agent_id`,
			hash(token), clientHash).Scan(&agent); err != nil {
			return notFound(err)
		}
		tag, err := tx.Exec(ctx, `INSERT INTO refresh_tokens (token_hash, agent_id, client_hash, expires_at)
			SELECT $1, id, $3, $4 FROM agents WHERE id = $2 AND revoked_at IS NULL FOR SHARE`, hash(next), agent, clientHash, expires)
		if err == nil && tag.RowsAffected() == 0 {
			gone = true // its agent was disconnected: the old token stays spent
		}
		return err
	})
	if err == nil && gone {
		return "", ErrNotFound
	}
	return agent, err
}

// LiveAgents are the connected agents acting as the given Wakeshift agents
// (keys workspace/agent), by that key.
func (s *Store) LiveAgents(ctx context.Context, keys []string) (map[string]Agent, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+agentCols+` FROM agents WHERE workspace || '/' || agent = ANY($1) AND revoked_at IS NULL`, keys)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Agent, error) { return scanAgent(r) })
	out := map[string]Agent{}
	for _, a := range list {
		out[a.Workspace+"/"+a.Agent] = a
	}
	return out, err
}

// AgentsOfPeople are the given people's connected agents, oldest first, by
// person.
func (s *Store) AgentsOfPeople(ctx context.Context, people []string) (map[string][]Agent, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+agentCols+` FROM agents WHERE person_id = ANY($1) AND revoked_at IS NULL ORDER BY created_at`, people)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Agent, error) { return scanAgent(r) })
	out := map[string][]Agent{}
	for _, a := range list {
		out[a.PersonID] = append(out[a.PersonID], a)
	}
	return out, err
}

// PeopleByID are the given people, by ID.
func (s *Store) PeopleByID(ctx context.Context, ids []string) (map[string]Person, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+personCols+` FROM people WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Person, error) { return scanPerson(r) })
	out := map[string]Person{}
	for _, p := range list {
		out[p.ID] = p
	}
	return out, err
}
