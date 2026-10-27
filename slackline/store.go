package slackline

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// ErrNotFound is a missing row (or one that isn't the caller's).
var ErrNotFound = errors.New("not found")

// Store is Slackline's own Postgres database.
type Store struct {
	Pool  *pgxpool.Pool
	seals []cipher.AEAD // the first seals; any opens (key rotation)
}

// OpenStore connects, applies migrations, and seals bot tokens with keys:
// the first seals, and any of them opens, so a new key goes first and the
// old one stays until every workspace has reinstalled (each 32 bytes or
// more).
func OpenStore(ctx context.Context, url string, keys ...[]byte) (*Store, error) {
	if len(keys) == 0 {
		return nil, errors.New("no token key")
	}
	var seals []cipher.AEAD
	for _, k := range keys {
		if len(k) < 32 {
			return nil, errors.New("token keys must be at least 32 bytes")
		}
		block, err := aes.NewCipher(k[:32])
		if err != nil {
			return nil, err
		}
		aead, _ := cipher.NewGCM(block)
		seals = append(seals, aead)
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	s := &Store{Pool: pool, seals: seals}
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the pool.
func (s *Store) Close() { s.Pool.Close() }

func (s *Store) migrate(ctx context.Context) error {
	files, _ := fs.Sub(migrationFiles, "migrations")
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7341)`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (name text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
			return err
		}
		rows, _ := tx.Query(ctx, `SELECT name FROM schema_migrations`)
		applied, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		names, _ := fs.Glob(files, "*.sql")
		slices.Sort(names)
		for _, n := range names {
			if slices.Contains(applied, n) {
				continue
			}
			sql, _ := fs.ReadFile(files, n)
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				return fmt.Errorf("migration %s: %w", n, err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (name) VALUES ($1)`, n); err != nil {
				return err
			}
		}
		return nil
	})
}

// tokenAAD binds a sealed bot token to its team, so a sealed token copied
// into another team's row (by someone who can write the database but
// hasn't the key) doesn't open there.
func tokenAAD(team string) []byte { return []byte("slackline/bot-token\x00" + team) }

func (s *Store) sealToken(team, plain string) []byte {
	nonce := make([]byte, s.seals[0].NonceSize())
	rand.Read(nonce)
	return s.seals[0].Seal(nonce, nonce, []byte(plain), tokenAAD(team))
}

// openToken opens a team's sealed bot token. legacy: it was sealed before
// tokens were bound to their team (no AAD); the caller seals it again.
// TODO(remove) once every team's token is resealed (Team reseals on read).
func (s *Store) openToken(team string, sealed []byte) (plain string, legacy bool, err error) {
	for _, aad := range [][]byte{tokenAAD(team), nil} {
		for _, aead := range s.seals {
			n := aead.NonceSize()
			if len(sealed) < n {
				return "", false, errors.New("sealed token too short")
			}
			if p, err := aead.Open(nil, sealed[:n], sealed[n:], aad); err == nil {
				return string(p), aad == nil, nil
			}
		}
	}
	return "", false, errors.New("no token key opens this token (was the key that sealed it removed?)")
}

func hash(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// ── Teams ────────────────────────────────────────────────────────────────

// Team is a Slack workspace that installed Slackline.
type Team struct {
	ID, Name, BotToken, BotUserID string
}

// SaveTeam records an install (a reinstall replaces the token).
func (s *Store) SaveTeam(ctx context.Context, t Team, by string) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO teams (team_id, team_name, bot_token, bot_user_id, installed_by) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (team_id) DO UPDATE SET team_name = EXCLUDED.team_name, bot_token = EXCLUDED.bot_token,
			bot_user_id = EXCLUDED.bot_user_id, installed_by = EXCLUDED.installed_by, installed_at = now()`,
		t.ID, t.Name, s.sealToken(t.ID, t.BotToken), t.BotUserID, by)
	return err
}

// Team returns an installed workspace.
func (s *Store) Team(ctx context.Context, id string) (Team, error) {
	t := Team{ID: id}
	var sealed []byte
	err := s.Pool.QueryRow(ctx, `SELECT team_name, bot_token, bot_user_id FROM teams WHERE team_id = $1`, id).Scan(&t.Name, &sealed, &t.BotUserID)
	if err != nil {
		return t, notFound(err)
	}
	var legacy bool
	if t.BotToken, legacy, err = s.openToken(id, sealed); err != nil {
		return t, err
	}
	if legacy { // bind it to its team from now on (unless a reinstall replaced it)
		if _, err := s.Pool.Exec(ctx, `UPDATE teams SET bot_token = $2 WHERE team_id = $1 AND bot_token = $3`, id, s.sealToken(id, t.BotToken), sealed); err != nil {
			return t, err
		}
	}
	return t, nil
}

// TeamUsers lists the people of a workspace who signed in.
func (s *Store) TeamUsers(ctx context.Context, team string) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `SELECT user_id FROM users WHERE team_id = $1`, team)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// RequestDeletion records that a workspace removed Slackline, and deletes
// its install (the bot token) at once, so nothing more is routed or
// posted for it. The rest is deleted by ForgetTeam, retried until done.
// An uninstall Slack sent before the current install (at) is stale, a
// retry or a late delivery, and is ignored, as is one for a workspace
// Slackline has nothing of.
func (s *Store) RequestDeletion(ctx context.Context, team, why string, at time.Time) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var installed time.Time
		err := tx.QueryRow(ctx, `SELECT installed_at FROM teams WHERE team_id = $1 FOR UPDATE`, team).Scan(&installed)
		switch {
		case err == nil:
			// Slack's event_time is when the removal happened (retries keep
			// it), to the second: one from a second before the install
			// predates it. (One from the same second counts: deleting is safer.)
			if !at.IsZero() && installed.Truncate(time.Second).After(at.Truncate(time.Second)) {
				return nil // removed before this install
			}
		case errors.Is(err, pgx.ErrNoRows):
			var has bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE team_id = $1) OR EXISTS (SELECT 1 FROM agents WHERE team_id = $1)
				OR EXISTS (SELECT 1 FROM threads WHERE team_id = $1) OR EXISTS (SELECT 1 FROM audit_log WHERE team_id = $1 AND action <> 'workspace.forgotten')`,
				team).Scan(&has); err != nil || !has {
				return err
			}
		default:
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO pending_deletions (team_id, why) VALUES ($1, $2) ON CONFLICT (team_id) DO NOTHING`, team, why); err != nil {
			return err
		}
		// Its agents stop at once: their access tokens are checked against
		// revoked_at on every request.
		for _, q := range []string{`DELETE FROM teams WHERE team_id = $1`, `UPDATE agents SET revoked_at = now() WHERE team_id = $1 AND revoked_at IS NULL`,
			`DELETE FROM refresh_tokens r USING agents a WHERE r.agent_id = a.id AND a.team_id = $1`} {
			if _, err := tx.Exec(ctx, q, team); err != nil {
				return err
			}
		}
		return nil
	})
}

// PendingDeletions lists workspaces whose deletion isn't done, fewest
// failed attempts first, then oldest (so failing ones don't starve others).
func (s *Store) PendingDeletions(ctx context.Context, limit int) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `SELECT team_id FROM pending_deletions ORDER BY attempts, requested_at LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// DeletionPending says whether a workspace's deletion isn't done.
func (s *Store) DeletionPending(ctx context.Context, team string) (bool, error) {
	var ok bool
	err := s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pending_deletions WHERE team_id = $1)`, team).Scan(&ok)
	return ok, err
}

// LockDeletion holds a workspace's deletion lock (a session advisory lock,
// so every instance agrees) until release is called; ok is false if
// another worker holds it. Deletions run only under it, so a stale worker
// can't act after another finished (and a reinstall followed).
func (s *Store) LockDeletion(ctx context.Context, team string) (release func(), ok bool, err error) {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	key := "slackline/delete/" + team
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext($1))`, key).Scan(&ok); err != nil || !ok {
		conn.Release()
		return nil, false, err
	}
	return func() {
		if _, err := conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtext($1))`, key); err != nil {
			conn.Conn().Close(context.WithoutCancel(ctx)) // closing the session releases it
		}
		conn.Release()
	}, true, nil
}

// DeletionFailed records a failed attempt: the step that failed, never an
// error's text.
func (s *Store) DeletionFailed(ctx context.Context, team, why string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE pending_deletions SET attempts = attempts + 1, last_error = $2 WHERE team_id = $1`, team, truncate(why, 500))
	return err
}

// ForgetTeam finishes a pending deletion (the caller has had Wakeshift
// forget the workspace's people first): the install, its people, their
// agents, tokens, rules, listeners and threads, and the workspace's audit
// log, which is reduced to one entry saying Slackline was removed and why,
// with no people in it. It does nothing if no deletion is pending (another
// attempt finished it), so it never deletes a reinstall.
func (s *Store) ForgetTeam(ctx context.Context, team string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var why string
		var requested time.Time
		err := tx.QueryRow(ctx, `DELETE FROM pending_deletions WHERE team_id = $1 RETURNING why, requested_at`, team).Scan(&why, &requested)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		// Users cascade to their agents, and those to tokens, codes, rules,
		// listeners, conversations and threads. An install saved after the
		// removal was recorded (a reinstall) stays.
		for _, q := range []string{`DELETE FROM threads WHERE team_id = $1`, `DELETE FROM users WHERE team_id = $1`, `DELETE FROM audit_log WHERE team_id = $1`} {
			if _, err := tx.Exec(ctx, q, team); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM teams WHERE team_id = $1 AND installed_at <= $2`, team, requested); err != nil {
			return err
		}
		return audit(ctx, tx, team, "", "workspace.forgotten", map[string]any{"why": why})
	})
}

// ── Users ────────────────────────────────────────────────────────────────

// User is someone who signed in with Slack.
type User struct {
	TeamID, UserID, Name, TeamName, DMChannel string
	SessionVersion                            int
	Understood                                bool // confirmed where their agents' messages go
}

// SignIn records a sign-in (creating the user the first time), audited.
func (s *Store) SignIn(ctx context.Context, u User) (User, error) {
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO users (team_id, user_id, name, team_name) VALUES ($1, $2, $3, $4)
			ON CONFLICT (team_id, user_id) DO UPDATE SET name = EXCLUDED.name, team_name = EXCLUDED.team_name, last_sign_in_at = now()
			RETURNING dm_channel, session_version, understood_at IS NOT NULL`, u.TeamID, u.UserID, u.Name, u.TeamName).
			Scan(&u.DMChannel, &u.SessionVersion, &u.Understood); err != nil {
			return err
		}
		return audit(ctx, tx, u.TeamID, u.UserID, "user.signed_in", nil)
	})
	return u, err
}

// User returns someone who signed in.
func (s *Store) User(ctx context.Context, team, user string) (User, error) {
	u := User{TeamID: team, UserID: user}
	err := s.Pool.QueryRow(ctx, `SELECT name, team_name, dm_channel, session_version, understood_at IS NOT NULL FROM users WHERE team_id = $1 AND user_id = $2`,
		team, user).Scan(&u.Name, &u.TeamName, &u.DMChannel, &u.SessionVersion, &u.Understood)
	return u, notFound(err)
}

// Understand records, audited, that the user confirmed they understand
// where their agents' messages go (the first time only).
func (s *Store) Understand(ctx context.Context, team, user, where string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE users SET understood_at = now() WHERE team_id = $1 AND user_id = $2 AND understood_at IS NULL`, team, user)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		return audit(ctx, tx, team, user, "user.understood", map[string]any{"where": where})
	})
}

// SetDMChannel remembers the user's DM channel with the app.
func (s *Store) SetDMChannel(ctx context.Context, team, user, channel string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE users SET dm_channel = $3 WHERE team_id = $1 AND user_id = $2`, team, user, channel)
	return err
}

// EndSessions signs the user out of every browser, audited: every sign-out
// does, so a copied session cookie stops working.
func (s *Store) EndSessions(ctx context.Context, team, user string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE users SET session_version = session_version + 1 WHERE team_id = $1 AND user_id = $2`, team, user); err != nil {
			return err
		}
		return audit(ctx, tx, team, user, "user.signed_out", nil)
	})
}

// ── Agents ───────────────────────────────────────────────────────────────

// Agent is an agent someone connected.
type Agent struct {
	ID, TeamID, UserID, ClientName, Label string
	CreatedAt                             time.Time
	Revoked                               bool
}

const agentCols = `id, team_id, user_id, client_name, label, created_at, revoked_at IS NOT NULL`

func scanAgent(row pgx.Row) (Agent, error) {
	var a Agent
	err := row.Scan(&a.ID, &a.TeamID, &a.UserID, &a.ClientName, &a.Label, &a.CreatedAt, &a.Revoked)
	return a, notFound(err)
}

// ConnectAgent returns the user's agent for an OAuth client (an install of
// a harness), creating it the first time, and records it in the audit log.
func (s *Store) ConnectAgent(ctx context.Context, team, user, client, clientName, label string) (Agent, error) {
	var a Agent
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var err error
		a, err = scanAgent(tx.QueryRow(ctx, `SELECT `+agentCols+` FROM agents WHERE team_id = $1 AND user_id = $2 AND client_id = $3 AND revoked_at IS NULL`,
			team, user, hex.EncodeToString(hash(client))))
		if err == nil {
			return audit(ctx, tx, team, user, "agent.authorized_again", map[string]any{"agent": a.ID})
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		a, err = scanAgent(tx.QueryRow(ctx, `INSERT INTO agents (id, team_id, user_id, client_id, client_name, label) VALUES ($1, $2, $3, $4, $5, $6)
			RETURNING `+agentCols, newID(), team, user, hex.EncodeToString(hash(client)), clientName, label))
		if err != nil {
			return err
		}
		return audit(ctx, tx, team, user, "agent.connected", map[string]any{"agent": a.ID, "client": clientName})
	})
	return a, err
}

// Agent returns an agent by ID.
func (s *Store) Agent(ctx context.Context, id string) (Agent, error) {
	return scanAgent(s.Pool.QueryRow(ctx, `SELECT `+agentCols+` FROM agents WHERE id = $1`, id))
}

// Agents lists a user's agents, newest first (revoked ones too, last).
func (s *Store) Agents(ctx context.Context, team, user string) ([]Agent, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+agentCols+` FROM agents WHERE team_id = $1 AND user_id = $2
		ORDER BY revoked_at IS NOT NULL, created_at DESC LIMIT 100`, team, user)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Agent, error) { return scanAgent(r) })
}

// RenameAgent changes an agent's label (the user's own agent only).
func (s *Store) RenameAgent(ctx context.Context, team, user, id, label string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE agents SET label = $4 WHERE id = $3 AND team_id = $1 AND user_id = $2 AND revoked_at IS NULL`, team, user, id, label)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return audit(ctx, tx, team, user, "agent.renamed", map[string]any{"agent": id, "label": label})
	})
}

// RevokeAgent stops an agent: its tokens, codes, listeners and rules end.
// It returns the rules that ended, so their channels can be told.
func (s *Store) RevokeAgent(ctx context.Context, team, user, id string) ([]EndedRule, error) {
	var ended []EndedRule
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE agents SET revoked_at = now() WHERE id = $3 AND team_id = $1 AND user_id = $2 AND revoked_at IS NULL`, team, user, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		if ended, err = endAgent(ctx, tx, id); err != nil {
			return err
		}
		return audit(ctx, tx, team, user, "agent.revoked", map[string]any{"agent": id})
	})
	return ended, err
}

// endAgent ends a revoked agent's refresh tokens, codes, channel rules
// (returned) and listeners.
func endAgent(ctx context.Context, tx pgx.Tx, id string) ([]EndedRule, error) {
	for _, q := range []string{`DELETE FROM refresh_tokens WHERE agent_id = $1`, `DELETE FROM oauth_codes WHERE agent_id = $1`,
		`UPDATE listeners SET closed_at = now() WHERE agent_id = $1 AND closed_at IS NULL`} {
		if _, err := tx.Exec(ctx, q, id); err != nil {
			return nil, err
		}
	}
	rows, err := tx.Query(ctx, `DELETE FROM channel_rules r USING agents a WHERE r.agent_id = a.id AND a.id = $1
		RETURNING r.channel_id, r.channel_name, a.id, a.label, a.user_id`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[EndedRule])
}

// EndUser ends a person's use of Slackline (their Slack account was
// deactivated): every agent of theirs is revoked as RevokeAgent does, and
// their sign-in ends in every browser, audited in the same transaction. It
// returns the rules that ended; ErrNotFound if they never signed in.
func (s *Store) EndUser(ctx context.Context, team, user, why string) ([]EndedRule, error) {
	var ended []EndedRule
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE users SET session_version = session_version + 1 WHERE team_id = $1 AND user_id = $2`, team, user)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		rows, err := tx.Query(ctx, `UPDATE agents SET revoked_at = now() WHERE team_id = $1 AND user_id = $2 AND revoked_at IS NULL RETURNING id`, team, user)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for _, id := range ids {
			rs, err := endAgent(ctx, tx, id)
			if err != nil {
				return err
			}
			ended = append(ended, rs...)
		}
		return audit(ctx, tx, team, user, "user.deactivated", map[string]any{"why": why, "agents": ids})
	})
	return ended, err
}

// PersonRef is someone in a workspace, by ID.
type PersonRef struct{ Team, User string }

// PeopleWithAgents lists people with an agent that isn't revoked, in
// order, after the one given (zero: from the start), up to limit.
func (s *Store) PeopleWithAgents(ctx context.Context, after PersonRef, limit int) ([]PersonRef, error) {
	rows, err := s.Pool.Query(ctx, `SELECT DISTINCT team_id, user_id FROM agents WHERE revoked_at IS NULL AND (team_id, user_id) > ($1, $2)
		ORDER BY team_id, user_id LIMIT $3`, after.Team, after.User, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[PersonRef])
}

// MemberNamed says whether someone who signed in to Slackline in a
// workspace has this name (lower case, spaces collapsed).
func (s *Store) MemberNamed(ctx context.Context, team, name string) (bool, error) {
	var ok bool
	err := s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE team_id = $1 AND lower(regexp_replace(btrim(name), '\s+', ' ', 'g')) = $2)`,
		team, name).Scan(&ok)
	return ok, err
}

// ── OAuth codes and refresh tokens ───────────────────────────────────────

// Code is an authorization code's grant.
type Code struct {
	AgentID, ClientID, RedirectURI, Challenge string
}

// SaveCode stores an authorization code (hashed).
func (s *Store) SaveCode(ctx context.Context, code string, c Code, expires time.Time) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO oauth_codes (code_hash, agent_id, client_id, redirect_uri, code_challenge, expires_at) VALUES ($1, $2, $3, $4, $5, $6)`,
		hash(code), c.AgentID, c.ClientID, c.RedirectURI, c.Challenge, expires)
	return err
}

// TakeCode returns an unexpired code's grant and deletes it: a code works
// once.
func (s *Store) TakeCode(ctx context.Context, code string, now time.Time) (Code, error) {
	var c Code
	err := s.Pool.QueryRow(ctx, `DELETE FROM oauth_codes WHERE code_hash = $1 AND expires_at > $2
		RETURNING agent_id, client_id, redirect_uri, code_challenge`, hash(code), now).Scan(&c.AgentID, &c.ClientID, &c.RedirectURI, &c.Challenge)
	return c, notFound(err)
}

// SaveRefresh stores a refresh token (hashed).
func (s *Store) SaveRefresh(ctx context.Context, token, agent string, expires time.Time) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO refresh_tokens (token_hash, agent_id, expires_at) VALUES ($1, $2, $3)`, hash(token), agent, expires)
	return err
}

// TakeRefresh returns an unexpired refresh token's agent and deletes the
// token (each use gets a new one). The token works only for the client it
// was issued to; another client's attempt leaves it in place.
func (s *Store) TakeRefresh(ctx context.Context, token, client string, now time.Time) (string, error) {
	var agent string
	err := s.Pool.QueryRow(ctx, `DELETE FROM refresh_tokens r USING agents a
		WHERE r.token_hash = $1 AND r.expires_at > $2 AND a.id = r.agent_id AND a.client_id = $3 RETURNING r.agent_id`,
		hash(token), now, hex.EncodeToString(hash(client))).Scan(&agent)
	return agent, notFound(err)
}

// PruneOAuth deletes expired codes and refresh tokens.
func (s *Store) PruneOAuth(ctx context.Context, now time.Time) error {
	if _, err := s.Pool.Exec(ctx, `DELETE FROM oauth_codes WHERE expires_at < $1`, now); err != nil {
		return err
	}
	_, err := s.Pool.Exec(ctx, `DELETE FROM refresh_tokens WHERE expires_at < $1`, now)
	return err
}

// ── Listeners and threads ────────────────────────────────────────────────

// Listener is one conversation of an agent that called watch.
type Listener struct {
	ID, AgentID, DMChannel, DMThreadTS, Label, Subscription string
	CreatedAt, ActiveAt                                     time.Time
	Closed                                                  bool
}

const listenerCols = `id, agent_id, dm_channel, dm_thread_ts, label, subscription, created_at, active_at, closed_at IS NOT NULL`

func scanListener(row pgx.Row) (Listener, error) {
	var l Listener
	err := row.Scan(&l.ID, &l.AgentID, &l.DMChannel, &l.DMThreadTS, &l.Label, &l.Subscription, &l.CreatedAt, &l.ActiveAt, &l.Closed)
	return l, notFound(err)
}

// AddListener records a new listener and its DM thread.
func (s *Store) AddListener(ctx context.Context, team string, l Listener) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO listeners (id, agent_id, dm_channel, dm_thread_ts, label, subscription) VALUES ($1, $2, $3, $4, $5, $6)`,
			l.ID, l.AgentID, l.DMChannel, l.DMThreadTS, l.Label, l.Subscription); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO threads (team_id, channel_id, thread_ts, listener_id) VALUES ($1, $2, $3, $4)`, team, l.DMChannel, l.DMThreadTS, l.ID)
		return err
	})
}

// Listener returns a listener by ID.
func (s *Store) Listener(ctx context.Context, id string) (Listener, error) {
	return scanListener(s.Pool.QueryRow(ctx, `SELECT `+listenerCols+` FROM listeners WHERE id = $1`, id))
}

// ListenerFor is the listener a conversation already has, by the hashes
// of its keys.
func (s *Store) ListenerFor(ctx context.Context, keys [][]byte) (Listener, error) {
	if len(keys) == 0 {
		return Listener{}, ErrNotFound
	}
	return scanListener(s.Pool.QueryRow(ctx, `SELECT l.id, l.agent_id, l.dm_channel, l.dm_thread_ts, l.label, l.subscription, l.created_at, l.active_at, l.closed_at IS NOT NULL
		FROM conversations c JOIN listeners l ON l.id = c.listener_id WHERE c.key_hash = ANY($1) ORDER BY l.created_at DESC LIMIT 1`, keys))
}

// RememberConversation ties a conversation's key hashes to its listener.
func (s *Store) RememberConversation(ctx context.Context, keys [][]byte, listener string) error {
	for _, k := range keys {
		if _, err := s.Pool.Exec(ctx, `INSERT INTO conversations (key_hash, listener_id) VALUES ($1, $2)
			ON CONFLICT (key_hash) DO UPDATE SET listener_id = EXCLUDED.listener_id`, k, listener); err != nil {
			return err
		}
	}
	return nil
}

// SetListenerOpen opens (marking it active now) or closes a listener.
func (s *Store) SetListenerOpen(ctx context.Context, id string, open bool) error {
	q := `UPDATE listeners SET closed_at = now() WHERE id = $1 AND closed_at IS NULL`
	if open {
		q = `UPDATE listeners SET closed_at = NULL, active_at = now() WHERE id = $1`
	}
	_, err := s.Pool.Exec(ctx, q, id)
	return err
}

// Touch marks a listener active now, and records its subscription.
func (s *Store) Touch(ctx context.Context, id, subscription string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE listeners SET active_at = now(), subscription = CASE WHEN $2 = '' THEN subscription ELSE $2 END WHERE id = $1`, id, subscription)
	return err
}

// Listeners lists an agent's open listeners, most recently active first.
func (s *Store) Listeners(ctx context.Context, agent string) ([]Listener, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+listenerCols+` FROM listeners WHERE agent_id = $1 AND closed_at IS NULL ORDER BY active_at DESC LIMIT 50`, agent)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Listener, error) { return scanListener(r) })
}

// ThreadListeners are the listeners a Slack thread leads to, most
// recently active first.
func (s *Store) ThreadListeners(ctx context.Context, team, channel, ts string) ([]Listener, error) {
	rows, err := s.Pool.Query(ctx, `SELECT l.id, l.agent_id, l.dm_channel, l.dm_thread_ts, l.label, l.subscription, l.created_at, l.active_at, l.closed_at IS NOT NULL
		FROM threads t JOIN listeners l ON l.id = t.listener_id WHERE t.team_id = $1 AND t.channel_id = $2 AND t.thread_ts = $3
		ORDER BY l.active_at DESC LIMIT 20`, team, channel, ts)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Listener, error) { return scanListener(r) })
}

// AddThread ties a channel thread a listener posted in to it.
func (s *Store) AddThread(ctx context.Context, team, channel, ts, listener string) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO threads (team_id, channel_id, thread_ts, listener_id) VALUES ($1, $2, $3, $4)
		ON CONFLICT DO NOTHING`, team, channel, ts, listener)
	return err
}

// ── Channel rules ────────────────────────────────────────────────────────

// Rule is where an agent listens besides its DM.
type Rule struct {
	AgentID, ChannelID, ChannelName string
	Mode                            string // "mentions" or "all"
	Senders                         string // "me" or "anyone"
}

// EndedRule is a rule that was removed, for telling its channel.
type EndedRule struct {
	ChannelID, ChannelName, AgentID, Label, UserID string
}

// ErrUnannounced is a rule change that would let more reach an agent than
// its channel was told (the rule changed meanwhile).
var ErrUnannounced = errors.New("the channel wasn't told about this change")

// RuleOf returns the user's own agent and its rule on a channel (nil if it
// has none there).
func (s *Store) RuleOf(ctx context.Context, team, user, agent, channel string) (Agent, *Rule, error) {
	ag, err := scanAgent(s.Pool.QueryRow(ctx, `SELECT `+agentCols+` FROM agents WHERE id = $1 AND team_id = $2 AND user_id = $3 AND revoked_at IS NULL`,
		agent, team, user))
	if err != nil {
		return ag, nil, err
	}
	prev, err := ruleOf(ctx, s.Pool, agent, channel)
	return ag, prev, err
}

type queryRower interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func ruleOf(ctx context.Context, q queryRower, agent, channel string) (*Rule, error) {
	r := Rule{AgentID: agent, ChannelID: channel}
	err := q.QueryRow(ctx, `SELECT channel_name, mode, senders FROM channel_rules WHERE agent_id = $1 AND channel_id = $2`, agent, channel).
		Scan(&r.ChannelName, &r.Mode, &r.Senders)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// SetRule adds or changes a rule for the user's own agent, audited. A rule
// that's new or lets more reach the agent (broadens) needs its channel told
// first (announced); otherwise it's refused with ErrUnannounced, so no
// agent listens unannounced. seen is the rule the caller read before
// (RuleOf): if it changed meanwhile, the change is refused the same way,
// so a notice always describes the rule that was saved.
func (s *Store) SetRule(ctx context.Context, team, user string, r Rule, seen *Rule, announced bool) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := scanAgent(tx.QueryRow(ctx, `SELECT `+agentCols+` FROM agents WHERE id = $1 AND team_id = $2 AND user_id = $3 AND revoked_at IS NULL FOR UPDATE`,
			r.AgentID, team, user)); err != nil {
			return err
		}
		prev, err := ruleOf(ctx, tx, r.AgentID, r.ChannelID)
		if err != nil {
			return err
		}
		same := prev == nil && seen == nil || prev != nil && seen != nil && prev.Mode == seen.Mode && prev.Senders == seen.Senders
		if !same || broadens(prev, r) && !announced {
			return ErrUnannounced
		}
		if _, err := tx.Exec(ctx, `INSERT INTO channel_rules (agent_id, channel_id, channel_name, mode, senders) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (agent_id, channel_id) DO UPDATE SET channel_name = EXCLUDED.channel_name, mode = EXCLUDED.mode, senders = EXCLUDED.senders`,
			r.AgentID, r.ChannelID, r.ChannelName, r.Mode, r.Senders); err != nil {
			return err
		}
		return audit(ctx, tx, team, user, "rule.set", map[string]any{"agent": r.AgentID, "channel": r.ChannelID, "channel_name": r.ChannelName,
			"mode": r.Mode, "senders": r.Senders})
	})
}

// RemoveRule removes one of the user's own agents' rules, audited, and
// returns it.
func (s *Store) RemoveRule(ctx context.Context, team, user, agent, channel string) (EndedRule, error) {
	var e EndedRule
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `DELETE FROM channel_rules r USING agents a WHERE r.agent_id = a.id AND a.id = $3 AND a.team_id = $1 AND a.user_id = $2
			AND r.channel_id = $4 RETURNING r.channel_id, r.channel_name, a.id, a.label, a.user_id`, team, user, agent, channel).
			Scan(&e.ChannelID, &e.ChannelName, &e.AgentID, &e.Label, &e.UserID)
		if err != nil {
			return notFound(err)
		}
		return audit(ctx, tx, team, user, "rule.removed", map[string]any{"agent": agent, "channel": channel})
	})
	return e, err
}

// RemoveMemberRules removes a person's agents' rules on a channel they're
// no longer in, audited, and returns them.
func (s *Store) RemoveMemberRules(ctx context.Context, team, user, channel, why string) ([]EndedRule, error) {
	var ended []EndedRule
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `DELETE FROM channel_rules r USING agents a WHERE r.agent_id = a.id AND a.team_id = $1 AND a.user_id = $2
			AND r.channel_id = $3 RETURNING r.channel_id, r.channel_name, a.id, a.label, a.user_id`, team, user, channel)
		if err != nil {
			return err
		}
		if ended, err = pgx.CollectRows(rows, pgx.RowToStructByPos[EndedRule]); err != nil || len(ended) == 0 {
			return err
		}
		return audit(ctx, tx, team, user, "rule.removed", map[string]any{"channel": channel, "rules": len(ended), "why": why})
	})
	return ended, err
}

// Rules lists an agent's rules.
func (s *Store) Rules(ctx context.Context, agent string) ([]Rule, error) {
	rows, err := s.Pool.Query(ctx, `SELECT agent_id, channel_id, channel_name, mode, senders FROM channel_rules WHERE agent_id = $1 ORDER BY channel_name`, agent)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Rule])
}

// ChannelRule is a rule with its agent, for routing a channel's message.
type ChannelRule struct {
	Rule
	Agent Agent
}

// ChannelRules lists the rules on a channel, of agents in that workspace
// that aren't revoked.
func (s *Store) ChannelRules(ctx context.Context, team, channel string) ([]ChannelRule, error) {
	rows, err := s.Pool.Query(ctx, `SELECT r.agent_id, r.channel_id, r.channel_name, r.mode, r.senders,
			a.id, a.team_id, a.user_id, a.client_name, a.label, a.created_at, a.revoked_at IS NOT NULL
		FROM channel_rules r JOIN agents a ON a.id = r.agent_id
		WHERE r.channel_id = $2 AND a.team_id = $1 AND a.revoked_at IS NULL ORDER BY a.created_at`, team, channel)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ChannelRule, error) {
		var c ChannelRule
		err := r.Scan(&c.AgentID, &c.ChannelID, &c.ChannelName, &c.Mode, &c.Senders,
			&c.Agent.ID, &c.Agent.TeamID, &c.Agent.UserID, &c.Agent.ClientName, &c.Agent.Label, &c.Agent.CreatedAt, &c.Agent.Revoked)
		return c, err
	})
}

// Ending is a revoked agent's conversation whose Wakeshift subscription
// hasn't been confirmed ended.
type Ending struct{ Listener, Subscription, Team, User string }

// PendingEnds lists revoked agents' subscriptions not yet confirmed ended.
func (s *Store) PendingEnds(ctx context.Context, limit int) ([]Ending, error) {
	rows, err := s.Pool.Query(ctx, `SELECT l.id, l.subscription, a.team_id, a.user_id FROM listeners l JOIN agents a ON a.id = l.agent_id
		WHERE a.revoked_at IS NOT NULL AND NOT l.sub_ended ORDER BY a.revoked_at LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Ending])
}

// MarkEnded records that a listener's subscription ended.
func (s *Store) MarkEnded(ctx context.Context, listener string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE listeners SET sub_ended = true WHERE id = $1`, listener)
	return err
}

// ── Audit ────────────────────────────────────────────────────────────────

func audit(ctx context.Context, tx pgx.Tx, team, user, action string, detail map[string]any) error {
	b, _ := json.Marshal(detail)
	_, err := tx.Exec(ctx, `INSERT INTO audit_log (team_id, user_id, action, detail) VALUES ($1, $2, $3, $4)`, team, user, action, b)
	return err
}

// Entry is an audit log entry.
type Entry struct {
	Action string
	Detail map[string]any
	At     time.Time
}

// Log is a user's recent audit log, newest first.
func (s *Store) Log(ctx context.Context, team, user string, limit int) ([]Entry, error) {
	rows, err := s.Pool.Query(ctx, `SELECT action, detail, at FROM audit_log WHERE team_id = $1 AND user_id = $2 ORDER BY id DESC LIMIT $3`, team, user, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Entry, error) {
		var e Entry
		err := r.Scan(&e.Action, &e.Detail, &e.At)
		return e, err
	})
}
