package sketch

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/wakeshift/apps/gamekit"
	"github.com/wakeshift/wakeshift-go/appkit"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

const (
	leaderRooms = 20                   // a player's best rooms that are kept
	keepResults = 180 * 24 * time.Hour // a result older than this is deleted
	keepLog     = 6 * time.Hour        // a room's posted messages are kept this long
	outboxEvery = 500 * time.Millisecond
	outboxLease = 30 * time.Second
	outboxTries = 6
	keepSeen    = 24 * time.Hour // when a player last did something in a room, kept this long
)

// Migrate applies Sketch Stadium's own migrations to the game kit's store
// (after the kit's, which OpenStore applies), tracked in sketch_migrations.
func Migrate(ctx context.Context, store *gamekit.Store) error {
	files, _ := fs.Sub(migrationFiles, "migrations")
	return pgx.BeginFunc(ctx, store.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7340)`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS sketch_migrations (name text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
			return err
		}
		rows, _ := tx.Query(ctx, `SELECT name FROM sketch_migrations`)
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
				return fmt.Errorf("sketch migration %s: %w", n, err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO sketch_migrations (name) VALUES ($1)`, n); err != nil {
				return err
			}
		}
		return nil
	})
}

// recordBest notes, after a round, each player's score in the room if
// it's its best there yet, and the round played (not house agents': they're
// not on the leaderboard). Only each player's leaderRooms best rooms are
// kept.
func (a *App) recordBest(tx *gamekit.Tx, g *game) error {
	var keys, names []string
	var scores []int32
	for _, p := range g.active() {
		if p.House != "" {
			continue
		}
		keys, names, scores = append(keys, p.Key()), append(names, p.Handle), append(scores, int32(p.Score))
	}
	if len(keys) == 0 {
		return nil
	}
	if _, err := tx.Exec(tx.Ctx, `INSERT INTO sketch_best (game_id, player, name, best, rounds, reached_at, updated_at)
		SELECT $1, k, n, s, 1, $5, $5 FROM unnest($2::text[], $3::text[], $4::int[]) AS t(k, n, s)
		ON CONFLICT (game_id, player) DO UPDATE SET name = excluded.name, rounds = sketch_best.rounds + 1, updated_at = excluded.updated_at,
			reached_at = CASE WHEN excluded.best > sketch_best.best THEN excluded.reached_at ELSE sketch_best.reached_at END,
			best = greatest(sketch_best.best, excluded.best)`, g.ID, keys, names, scores, tx.Now); err != nil {
		return err
	}
	_, err := tx.Exec(tx.Ctx, `DELETE FROM sketch_best b USING (
		SELECT game_id, player, row_number() OVER (PARTITION BY player ORDER BY best DESC, updated_at DESC, game_id) n
		FROM sketch_best WHERE player = ANY($1)) old
		WHERE b.game_id = old.game_id AND b.player = old.player AND old.n > $2`, keys, leaderRooms)
	return err
}

// leader is a row of the leaderboard.
type leader struct {
	Rank   int
	Name   string
	Points int // its best score: the most points it held over a room's last scoreWindow rounds
	Rounds int // rounds it's played (in its kept rooms)
}

// leaderboard is the players with the best scores, best first: the most
// points each has held at once over a room's last scoreWindow rounds.
func (a *App) leaderboard(ctx context.Context, limit int) ([]leader, error) {
	rows, _ := a.Store.Pool.Query(ctx, `SELECT max(name), max(best)::int, sum(rounds)::int FROM sketch_best
		GROUP BY player HAVING max(best) > 0 ORDER BY max(best) DESC, sum(rounds) ASC, min(reached_at) ASC LIMIT $1`, limit)
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (leader, error) {
		var l leader
		err := r.Scan(&l.Name, &l.Points, &l.Rounds)
		return l, err
	})
	for i := range out {
		out[i].Rank = i + 1
		if i > 0 && out[i].Points == out[i-1].Points && out[i].Rounds == out[i-1].Rounds {
			out[i].Rank = out[i-1].Rank
		}
	}
	return out, err
}

// ── Who's still playing ───────────────────────────────────────────────────

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// touch notes that player did something in game at at: a tool call as
// that player, or joining. Rounds count it as being there (checkAway).
func touch(ctx context.Context, db execer, game, player string, at time.Time) error {
	_, err := db.Exec(ctx, `INSERT INTO sketch_seen (game_id, player, at) VALUES ($1, $2, $3)
		ON CONFLICT (game_id, player) DO UPDATE SET at = greatest(sketch_seen.at, excluded.at)`, game, player, at)
	return err
}

// touchNow is touch for a call that doesn't change the game (game, canvas,
// say, join again), outside its lock. It's best effort: a failure is only
// logged and the call goes on, as one missed note costs at most a round
// counted away, and a player is taken out only after awayRounds in a row.
func (a *App) touchNow(ctx context.Context, game, player string) {
	if err := touch(ctx, a.Store.Pool, game, player, a.now()); err != nil && ctx.Err() == nil {
		slog.Warn("sketch: noting a player's call", "game", game, "err", err)
	}
}

// seen is when each player in g last did something there (none: not since
// this was first recorded).
func seen(tx *gamekit.Tx, g *game) (map[string]time.Time, error) {
	rows, err := tx.Query(tx.Ctx, `SELECT player, at FROM sketch_seen WHERE game_id = $1`, g.ID)
	if err != nil {
		return nil, err
	}
	out := map[string]time.Time{}
	var p string
	var at time.Time
	_, err = pgx.ForEachRow(rows, []any{&p, &at}, func() error { out[p] = at; return nil })
	return out, err
}

// ── What moves leave to send ──────────────────────────────────────────────

// queue leaves something to send once the move commits.
func (a *App) queue(tx *gamekit.Tx, game, kind string, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = tx.Exec(tx.Ctx, `INSERT INTO sketch_outbox (game_id, kind, data, next_at) VALUES ($1, $2, $3, $4)`, game, kind, b, tx.Now)
	return err
}

// turnArgs is a turn subscription's arguments.
type turnArgs struct {
	Game string `json:"game" jsonschema:"The game's ID, from join"`
}

// turnPayload is a drawer's turn: its word, for it alone.
type turnPayload struct {
	Game    string `json:"game" jsonschema:"The game's ID"`
	Round   int    `json:"round" jsonschema:"The round's number"`
	Word    string `json:"word" jsonschema:"The word to draw: never write it, or any letters or numbers"`
	Seconds int    `json:"seconds" jsonschema:"Seconds the round lasts, from your first drawing"`
	Grace   int    `json:"grace" jsonschema:"Seconds you have to start drawing"`
}

type queuedTurn struct {
	User string      `json:"user"`
	Turn turnPayload `json:"turn"`
}

// defineEvents defines turn: a personal event, so only the drawer's own
// subscriptions get its word.
func (a *App) defineEvents() {
	e, err := appkit.DefineEvent(a.Kit, "turn", appkit.EventDef[turnArgs, turnPayload]{
		Title:       "Your turn to draw",
		Description: "Your turn to draw in a Sketch Stadium game you're seated in, with your secret word. Subscribe after join, with the game's ID.",
		Text: func(t turnPayload, _ turnArgs) string {
			return fmt.Sprintf("Sketch Stadium game %s, round %d: it's your turn to draw. Your word: %s. Start drawing it with the `draw` tool within %ds, or the turn passes to the next drawer; "+
				"the round lasts %ds from your first drawing (no letters, numbers or words in the drawing).", t.Game, t.Round, t.Word, t.Grace, t.Seconds)
		},
		Personal:  true,
		Authorize: a.authorizeTurn,
	})
	if err != nil {
		panic(err) // a programming error: the definition is fixed
	}
	a.turn = e
}

// authorizeTurn lets a player subscribe to its own turns in a game it's
// seated in.
func (a *App) authorizeTurn(ctx context.Context, sub appkit.Subscriber, args turnArgs) bool {
	if sub.User == "" || args.Game == "" {
		return false
	}
	var ok bool
	err := a.Store.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM seats WHERE game_id = $1 AND workspace || '/' || agent = $2 AND handle IS NOT NULL)`,
		args.Game, sub.User).Scan(&ok)
	return err == nil && ok
}

// queueTurn leaves the drawer's turn event to send.
func (a *App) queueTurn(tx *gamekit.Tx, g *game, drawer *player) error {
	return a.queue(tx, g.ID, "turn", queuedTurn{User: gamekit.PlayerUser(drawer.Player), Turn: turnPayload{
		Game: g.ID, Round: g.Round, Word: g.Word, Seconds: int(a.round().Seconds()), Grace: int(a.grace().Seconds())}})
}

// runOutbox sends what's due every half second until ctx is done.
func (a *App) runOutbox(ctx context.Context) {
	last := time.Time{}
	for {
		a.sendOutbox(ctx)
		if time.Since(last) > time.Hour {
			last = time.Now()
			if err := a.forget(ctx); err != nil && ctx.Err() == nil {
				slog.Warn("sketch: forgetting old results", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(outboxEvery):
		}
	}
}

// forget deletes results older than keepResults (sketch_results is 001's
// per-match table, no longer written), when players last did something in
// rooms after keepSeen (rooms long closed), and rooms' messages posted more
// than keepLog ago (and players' reasoning as old): a room that plays for
// days would otherwise keep every stroke announcement it ever made.
func (a *App) forget(ctx context.Context) error {
	now := a.now()
	if _, err := a.Store.Pool.Exec(ctx, `DELETE FROM sketch_best WHERE updated_at < $1`, now.Add(-keepResults)); err != nil {
		return err
	}
	if _, err := a.Store.Pool.Exec(ctx, `DELETE FROM sketch_results WHERE ended_at < $1`, now.Add(-keepResults)); err != nil {
		return err
	}
	if _, err := a.Store.Pool.Exec(ctx, `DELETE FROM sketch_seen WHERE at < $1`, now.Add(-keepSeen)); err != nil {
		return err
	}
	if _, err := a.Store.Pool.Exec(ctx, `DELETE FROM room_log WHERE posted_at IS NOT NULL AND at < $1`, now.Add(-keepLog)); err != nil {
		return err
	}
	// Players' private reasoning goes as soon as what's said does.
	_, err := a.Store.Pool.Exec(ctx, `DELETE FROM thinking WHERE at < $1`, now.Add(-keepLog))
	return err
}

// sendOutbox claims what's due and sends it.
func (a *App) sendOutbox(ctx context.Context) {
	now := a.now()
	type item struct {
		ID       int64
		Game     string
		Kind     string
		Data     []byte
		Attempts int
	}
	rows, _ := a.Store.Pool.Query(ctx, `UPDATE sketch_outbox SET next_at = $2, attempts = attempts + 1 WHERE id IN (
		SELECT id FROM sketch_outbox WHERE next_at <= $1 ORDER BY id LIMIT 20 FOR UPDATE SKIP LOCKED)
		RETURNING id, game_id, kind, data, attempts`, now, now.Add(outboxLease))
	items, err := pgx.CollectRows(rows, pgx.RowToStructByPos[item])
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("sketch: reading the outbox", "err", err)
		}
		return
	}
	for _, it := range items {
		sctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := a.send(sctx, it.Game, it.Kind, it.Data)
		cancel()
		if err != nil && it.Attempts < outboxTries {
			slog.Warn("sketch: sending to Wakeshift", "kind", it.Kind, "game", it.Game, "err", err)
			a.Store.Pool.Exec(ctx, `UPDATE sketch_outbox SET next_at = $2 WHERE id = $1`, it.ID, a.now().Add(time.Duration(it.Attempts*it.Attempts)*time.Second))
			continue
		}
		if err != nil {
			slog.Warn("sketch: gave up sending to Wakeshift", "kind", it.Kind, "game", it.Game, "err", err)
		}
		a.Store.Pool.Exec(ctx, `DELETE FROM sketch_outbox WHERE id = $1`, it.ID)
	}
}

func (a *App) send(ctx context.Context, game, kind string, data []byte) error {
	switch kind {
	case "turn":
		var t queuedTurn
		if err := json.Unmarshal(data, &t); err != nil {
			return nil // nothing to retry
		}
		_, err := a.turn.Emit(ctx, turnArgs{Game: t.Turn.Game}, t.Turn, appkit.To(t.User), appkit.WithID(fmt.Sprintf("turn:%s:%d", t.Turn.Game, t.Turn.Round)))
		return err
	case "remove":
		// A removed seat's subscription. One with none (queued before the
		// games had subscriptions) has nothing to end.
		var r removal
		if err := json.Unmarshal(data, &r); err != nil || r.Subscription == "" {
			return nil
		}
		a.srv.SeatEnded(ctx, r.User, r.Subscription)
		return nil
	}
	return nil
}
