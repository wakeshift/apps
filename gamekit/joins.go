package gamekit

// Joins: each seat an outside player takes is noted in joins (011_joins.sql),
// in the seat's own transaction, for Wakeshift's alerts in Slack: the admin
// service reads the table and posts each join (docs/admin.md, "Game
// joins"). House agents' seats aren't noted. Noting one is best effort: a
// failure is logged and the seat stands.

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// joinsFor is how long a join is kept: as long as an idle sign-in is
// (whose joins go with it, endSession).
const joinsFor = SessionIdle

// JoinsColumns are the columns of joins the admin service may read: every
// one but player (the sign-in).
const JoinsColumns = "id, at, agent, name, returned, prev_at, seated"

// noteJoin notes p's seat in game, in tx (the seating's), unless p is a
// house agent. It runs in a savepoint, so a failure leaves the seating
// as it was.
func noteJoin(tx *Tx, game string, p Player) {
	if p.House != "" {
		return
	}
	err := pgx.BeginFunc(tx.Ctx, tx.Tx, func(sp pgx.Tx) error {
		_, err := sp.Exec(tx.Ctx, `INSERT INTO joins (at, player, agent, name, returned, prev_at, seated)
			SELECT $1, $2, $3, $4, prev IS NOT NULL, prev,
				(SELECT count(*) FROM seats WHERE game_id = $5 AND handle IS NOT NULL AND NOT house)
			FROM (SELECT max(at) AS prev FROM joins WHERE player = $2) j`, tx.Now, p.Workspace, p.Agent, p.Handle, game)
		return err
	})
	if err != nil {
		slog.Warn("gamekit: noting a join for the admin service failed", "err", err)
	}
}

// forgetJoins deletes joins older than joinsFor.
func (s *Server) forgetJoins(ctx context.Context, now time.Time) error {
	_, err := s.Store.Pool.Exec(ctx, `DELETE FROM joins WHERE at < $1`, now.Add(-joinsFor))
	return err
}

// GrantJoins lets login, the admin service's database login (on Cloud SQL,
// its IAM user), connect to the store's database and read JoinsColumns of
// joins, and nothing else. A login that doesn't exist yet is skipped (the
// admin service reads nothing from this game until it does).
func (s *Store) GrantJoins(ctx context.Context, login string) error {
	if login == "" {
		return nil
	}
	if strings.HasPrefix(login, "pg_") || strings.HasPrefix(login, "cloudsql") {
		return errors.New("gamekit: that can't be the admin service's login")
	}
	var exists bool
	if err := s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)`, login).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		slog.Warn("gamekit: the admin service's database login doesn't exist yet; not granting it joins", "login", login)
		return nil
	}
	role := pgx.Identifier{login}.Sanitize()
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var db string
		if err := tx.QueryRow(ctx, `SELECT current_database()`).Scan(&db); err != nil {
			return err
		}
		for _, q := range []string{
			`GRANT CONNECT ON DATABASE ` + pgx.Identifier{db}.Sanitize() + ` TO ` + role,
			`REVOKE ALL ON joins FROM ` + role,
			`GRANT SELECT (` + JoinsColumns + `) ON joins TO ` + role,
		} {
			if _, err := tx.Exec(ctx, q); err != nil {
				return err
			}
		}
		return nil
	})
}
