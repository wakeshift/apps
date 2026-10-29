package agentchat

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Agent Chat's own database (migrations/): who people are, their sessions,
// orgs, channels, invites and bans, the agents they connected, and the
// outboxes of what Wakeshift still has to do. Never message text.

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Store is Agent Chat's database.
type Store struct {
	Pool *pgxpool.Pool
}

// ErrNotFound: no such row, or not one the caller may see.
var ErrNotFound = errors.New("not found")

// OpenStore connects to url (pool size from its pool_max_conns) and applies
// the migrations.
func OpenStore(ctx context.Context, url string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	s := &Store{Pool: pool}
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
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7339)`); err != nil {
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

// ── Helpers ──────────────────────────────────────────────────────────────

// crypto/rand.Read never fails (since Go 1.24 it crashes the program
// instead), so the IDs, secrets and codes below are always random.

func newID() string {
	b := make([]byte, 10)
	rand.Read(b)
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
}

// newSecret is 256 random bits, URL-safe.
func newSecret() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// codeAlphabet has no look-alikes (0/o, 1/l/i).
const codeAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"

// newCode is n characters of codeAlphabet (about 4.95 bits each).
func newCode(n int) string {
	b := make([]byte, n)
	out := make([]byte, n)
	for i := 0; i < n; {
		rand.Read(b[:1])
		if int(b[0]) >= 256-256%len(codeAlphabet) {
			continue // no bias
		}
		out[i] = codeAlphabet[int(b[0])%len(codeAlphabet)]
		i++
	}
	return string(out)
}

func hash(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func uniqueViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}

// limitLock holds a lock on key for the rest of tx, so a count against a
// limit and the insert after it can't interleave with another's.
func limitLock(ctx context.Context, tx pgx.Tx, key string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('agentchat-limit:' || $1, 0))`, key)
	return err
}

func audit(ctx context.Context, tx pgx.Tx, person, org, action string, detail map[string]any) error {
	if detail == nil {
		detail = map[string]any{}
	}
	b, _ := json.Marshal(detail)
	_, err := tx.Exec(ctx, `INSERT INTO audit_log (person_id, org_id, action, detail) VALUES ($1, nullif($2, ''), $3, $4)`, person, org, action, b)
	return err
}

// ── People and sessions ──────────────────────────────────────────────────

// Person is someone who signed in.
type Person struct {
	ID, Email, Name, Handle, Workspace string
	CreatedAt                          time.Time
}

const personCols = `id, email, name, coalesce(handle, ''), workspace, created_at`

func scanPerson(row pgx.Row) (Person, error) {
	var p Person
	err := row.Scan(&p.ID, &p.Email, &p.Name, &p.Handle, &p.Workspace, &p.CreatedAt)
	return p, notFound(err)
}

// CountRequests counts sign-in ledger entries of kind since: for the email
// (its HMAC), if given, else for the address.
func (s *Store) CountRequests(ctx context.Context, kind string, emailHash []byte, addr string, since time.Time) (int, error) {
	var n int
	var err error
	if emailHash != nil {
		err = s.Pool.QueryRow(ctx, `SELECT count(*) FROM sign_in_requests WHERE email_hash = $1 AND kind = $2 AND at > $3`, emailHash, kind, since).Scan(&n)
	} else {
		err = s.Pool.QueryRow(ctx, `SELECT count(*) FROM sign_in_requests WHERE kind = $1 AND addr = $2 AND at > $3`, kind, addr, since).Scan(&n)
	}
	return n, err
}

// TakeRequest records a request of kind from addr (and for emailHash, if
// given) in the last hour's ledger, if addr has fewer than addrMax: ok
// says it did. emailOK says the email, counting this one, is within
// emailMax. It's one transaction under the address's (and email's) lock,
// so concurrent requests can't all pass one count.
func (s *Store) TakeRequest(ctx context.Context, kind string, emailHash []byte, addr string, addrMax, emailMax int) (ok, emailOK bool, err error) {
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := limitLock(ctx, tx, "requests:"+kind+":"+addr); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM sign_in_requests WHERE kind = $1 AND addr = $2 AND at > now() - interval '1 hour'`, kind, addr).Scan(&n); err != nil {
			return err
		}
		if n >= addrMax {
			return nil
		}
		ok = true
		if _, err := tx.Exec(ctx, `INSERT INTO sign_in_requests (kind, email_hash, addr) VALUES ($1, $2, $3)`, kind, emailHash, addr); err != nil {
			return err
		}
		if emailHash == nil {
			return nil
		}
		if err := limitLock(ctx, tx, "requests:"+kind+":email:"+hex.EncodeToString(emailHash)); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM sign_in_requests WHERE kind = $1 AND email_hash = $2 AND at > now() - interval '1 hour'`, kind, emailHash).Scan(&n); err != nil {
			return err
		}
		emailOK = n <= emailMax
		return nil
	})
	return ok, emailOK, err
}

// RecordRequest adds to the ledger.
func (s *Store) RecordRequest(ctx context.Context, kind string, emailHash []byte, addr string) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO sign_in_requests (kind, email_hash, addr) VALUES ($1, $2, $3)`, kind, emailHash, addr)
	return err
}

// CreateLink stores a magic link (its token's hash).
func (s *Store) CreateLink(ctx context.Context, token, email, next string, expires time.Time) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO sign_in_links (token_hash, email, next, expires_at) VALUES ($1, $2, $3, $4)`, hash(token), email, next, expires)
	return err
}

// UseLink spends a magic link (once, before it expires) and signs its
// person in, making them if they're new, with a session (sessionToken,
// good until sessionExpires). It returns them and where the link leads.
func (s *Store) UseLink(ctx context.Context, token, sessionToken string, sessionExpires time.Time) (p Person, next string, err error) {
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var email string
		err := tx.QueryRow(ctx, `UPDATE sign_in_links SET used_at = now() WHERE token_hash = $1 AND used_at IS NULL AND expires_at > now()
			RETURNING email, next`, hash(token)).Scan(&email, &next)
		if err != nil {
			return notFound(err)
		}
		p, err = scanPerson(tx.QueryRow(ctx, `INSERT INTO people (id, email) VALUES ($1, $2)
			ON CONFLICT (email) DO UPDATE SET last_seen_at = now() RETURNING `+personCols, newID(), email))
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO sessions (token_hash, person_id, expires_at) VALUES ($1, $2, $3)`, hash(sessionToken), p.ID, sessionExpires); err != nil {
			return err
		}
		return audit(ctx, tx, p.ID, "", "person.signed_in", nil)
	})
	return p, next, err
}

// Session is who a session token is for, and when it was last rotated.
func (s *Store) Session(ctx context.Context, token string) (Person, time.Time, error) {
	var rotated time.Time
	var p Person
	err := s.Pool.QueryRow(ctx, `SELECT p.id, p.email, p.name, coalesce(p.handle, ''), p.workspace, p.created_at, s.rotated_at
		FROM sessions s JOIN people p ON p.id = s.person_id WHERE s.token_hash = $1 AND s.expires_at > now()`, hash(token)).
		Scan(&p.ID, &p.Email, &p.Name, &p.Handle, &p.Workspace, &p.CreatedAt, &rotated)
	return p, rotated, notFound(err)
}

// RotateSession replaces a session's token with next (good until
// expires); the old one stays good for a minute, for requests already on
// their way. ok is false if the old one was rotated (or ended) meanwhile.
func (s *Store) RotateSession(ctx context.Context, old, next string, expires time.Time) (ok bool, err error) {
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var person string
		err := tx.QueryRow(ctx, `UPDATE sessions SET expires_at = least(expires_at, now() + interval '1 minute'), rotated_at = '9999-01-01'
			WHERE token_hash = $1 AND expires_at > now() AND rotated_at < '9999-01-01' RETURNING person_id`, hash(old)).Scan(&person)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		ok = true
		_, err = tx.Exec(ctx, `INSERT INTO sessions (token_hash, person_id, expires_at) VALUES ($1, $2, $3)`, hash(next), person, expires)
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE people SET last_seen_at = now() WHERE id = $1`, person)
		}
		return err
	})
	return ok, err
}

// EndSession signs one browser out.
func (s *Store) EndSession(ctx context.Context, token string) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, hash(token))
	return err
}

// EndSessions signs a person out everywhere.
func (s *Store) EndSessions(ctx context.Context, person string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE person_id = $1`, person); err != nil {
			return err
		}
		return audit(ctx, tx, person, "", "person.signed_out_everywhere", nil)
	})
}

// SetName sets a person's name, and their handle (from base, made unique
// with -2, -3…) if they have none yet.
func (s *Store) SetName(ctx context.Context, person, name, base string) (Person, error) {
	for i := 1; i <= 50; i++ {
		h := base
		if i > 1 {
			suffix := "-" + strconv.Itoa(i)
			h = strings.TrimRight(base[:min(len(base), maxHandle-len(suffix))], "-") + suffix
		}
		p, err := scanPerson(s.Pool.QueryRow(ctx, `UPDATE people SET name = $2, handle = coalesce(handle, $3) WHERE id = $1 RETURNING `+personCols, person, name, h))
		if uniqueViolation(err) {
			continue
		}
		return p, err
	}
	return Person{}, errors.New("no free handle")
}

// SetWorkspace records a person's Wakeshift workspace.
func (s *Store) SetWorkspace(ctx context.Context, person, ws string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE people SET workspace = $2 WHERE id = $1`, person, ws)
	return err
}
