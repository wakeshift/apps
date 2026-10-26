package gamekit

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Games live in the game app's own Postgres database, so any number of
// instances can serve them and a restart or a deploy loses none:
//
//   - A game is a row (games) with the game's own state as JSON. Every move
//     is a transaction holding the game's row lock (Update).
//   - Matchmaking (Join) reserves a seat under one advisory lock, so
//     players spread over instances fill the same games; the room is then
//     joined in Wakeshift outside any transaction, and the seat confirmed
//     under the game's lock.
//   - A phase's end is the row's deadline_at. Every instance's ticker
//     claims due games with FOR UPDATE SKIP LOCKED and re-checks the
//     deadline under the lock, so each step runs once.
//   - The moderator's announcements are written to the room's log in the
//     move's transaction and posted to Wakeshift after it commits, in order,
//     by whichever instance gets there first (deliver), retried if posting
//     fails: an announcement isn't lost to a crash or a deploy.

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Store is a game app's database.
type Store struct {
	Pool *pgxpool.Pool
}

// OpenStore connects to url (pool size from its pool_max_conns) and applies
// the kit's migrations.
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
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7338)`); err != nil {
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

// State is a game's own state, kept as JSON: a struct embedding Game.
type State interface{ base() *Game }

// Game is what the kit keeps of every game. Embed it in the game's state.
type Game struct {
	ID    string `json:"id"`
	Phase string `json:"phase"`
	// Deadline is when the game's Step runs next (zero: never).
	Deadline time.Time `json:"deadline,omitzero"`
	// Open is whether the game takes players (Join).
	Open bool `json:"open"`

	pending []Entry // announcements, written when the move commits
	end     time.Duration
	ended   bool
}

func (g *Game) base() *Game { return g }

// Announce has the moderator say something to the room, once the move
// commits.
func (g *Game) Announce(subject, body string, mentions []string) {
	g.pending = append(g.pending, Entry{From: "", Subject: subject, Text: body, Mentions: mentions})
}

// End ends the game: it takes no more moves from the ticker, and its room
// closes after linger (the game stays readable for a while longer, for the
// follow-along page).
func (g *Game) End(linger time.Duration) {
	g.Open, g.ended, g.end = false, true, linger
}

// Ended reports whether End was called in this move.
func (g *Game) Ended() bool { return g.ended }

// Errors a game's tools can return as they are.
var (
	// ErrNoGame: no such game (any more).
	ErrNoGame = errors.New("that game is over and gone; call the `join` tool to play another")
	// ErrJoining: the player's join is still in progress (on another call).
	ErrJoining = errors.New("you're joining a game already; call the `game` tool in a few seconds")
)

// StoppedError is a game that had to stop, and why.
type StoppedError struct{ Reason string }

func (e *StoppedError) Error() string {
	return "this game had to stop: " + e.Reason + " Call the `join` tool to play another."
}

const (
	reserveFor  = time.Minute         // a seat reservation is abandoned after this
	keepEnded   = time.Hour           // a game is kept this long after it ends
	stopLinger  = time.Minute         // a stopped game's room closes after this
	closeRetry  = 30 * time.Second    // retrying closing a room
	postLease   = 30 * time.Second    // posting a game's announcements
	maxAttempts = 8                   // posting one announcement: the queries in ticker.go and migration 001 have it
	logLimit    = 300                 // entries a game's log shows
	recentFor   = 30 * 24 * time.Hour // what a player has had lately is forgotten after this without a game
	matchLock   = 7339                // the advisory lock matchmaking holds
)

// Tx is a move's transaction.
type Tx struct {
	pgx.Tx
	Ctx context.Context
	Now time.Time
	// Away is who in the room has dropped off (nothing delivers to them),
	// as the caller found before taking the lock: Wakeshift isn't called
	// while a game is locked.
	Away map[string]bool
}

// Recent is what each of players has had lately of kind.
func (tx *Tx) Recent(kind string, players []string) (map[string][]string, error) {
	rows, _ := tx.Query(tx.Ctx, `SELECT player, items FROM recent WHERE kind = $1 AND player = ANY($2)`, kind, players)
	out := map[string][]string{}
	var p string
	var items []string
	_, err := pgx.ForEachRow(rows, []any{&p, &items}, func() error { out[p] = items; return nil })
	return out, err
}

// Remember records what a player has had lately of kind.
func (tx *Tx) Remember(kind, player string, items []string) error {
	_, err := tx.Exec(tx.Ctx, `INSERT INTO recent (player, kind, items, updated_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (player, kind) DO UPDATE SET items = excluded.items, updated_at = excluded.updated_at`, player, kind, items, tx.Now)
	return err
}

// row is a game's row as a move reads it.
type row struct {
	state   []byte
	ended   *time.Time
	stopped *string
}

func (s *Server) begin(ctx context.Context) (*Tx, error) {
	t, err := s.Store.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &Tx{Tx: t, Ctx: ctx, Now: s.now()}, nil
}

// decode reads a game's state.
func (s *Server) decode(r row) (State, error) {
	if r.stopped != nil {
		return nil, &StoppedError{Reason: *r.stopped}
	}
	g := s.NewState()
	if err := json.Unmarshal(r.state, g); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", errDecode, s.Name, err)
	}
	return g, nil
}

// Load reads a game without locking it.
func (s *Server) Load(ctx context.Context, id string) (State, error) {
	var r row
	err := s.Store.Pool.QueryRow(ctx, `SELECT state, ended_at, stopped FROM games WHERE id = $1`, id).Scan(&r.state, &r.ended, &r.stopped)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoGame
	}
	if err != nil {
		return nil, err
	}
	g, err := s.decode(r)
	if errors.Is(err, errDecode) {
		if err := s.stopNow(ctx, id, unreadable); err != nil {
			return nil, err
		}
		return nil, &StoppedError{Reason: unreadable}
	}
	return g, err
}

// Update runs a move on game id: fn changes the state, under the game's
// lock, and may announce; the state is saved and the announcements posted
// once it commits. away is what the caller found with Away (or nil). A game
// whose state can't be read is stopped (StoppedError).
func (s *Server) Update(ctx context.Context, id string, away map[string]bool, fn func(tx *Tx, g State) error) (State, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	tx.Away = away
	var r row
	err = tx.QueryRow(ctx, `SELECT state, ended_at, stopped FROM games WHERE id = $1 FOR UPDATE`, id).Scan(&r.state, &r.ended, &r.stopped)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoGame
	}
	if err != nil {
		return nil, err
	}
	g, err := s.decode(r)
	var stopped *StoppedError
	if errors.As(err, &stopped) {
		return nil, err
	}
	if err != nil {
		reason := unreadable
		if err := s.stop(tx, id, reason); err != nil {
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		s.deliver(ctx, id)
		return nil, &StoppedError{Reason: reason}
	}
	if err := fn(tx, g); err != nil {
		return nil, err
	}
	announced := len(g.base().pending) > 0
	if err := s.save(tx, g, r.ended != nil); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	if announced {
		s.deliver(ctx, id)
	}
	return g, nil
}

// save writes a game's state and its announcements. Called under its lock.
func (s *Server) save(tx *Tx, g State, alreadyEnded bool) error {
	b := g.base()
	state, err := json.Marshal(g)
	if err != nil {
		return err
	}
	var deadline *time.Time
	if !b.Deadline.IsZero() {
		deadline = &b.Deadline
	}
	if b.ended && !alreadyEnded {
		end := tx.Now.Add(b.end)
		_, err = tx.Exec(tx.Ctx, `UPDATE games SET phase = $2, open = false, state = $3, ended_at = $4, deadline_at = $5, updated_at = $4 WHERE id = $1`,
			b.ID, b.Phase, state, tx.Now, end)
	} else {
		// Once a game has ended, the kit's lifecycle owns its deadline.
		_, err = tx.Exec(tx.Ctx, `UPDATE games SET phase = $2, open = $3 AND ended_at IS NULL, state = $4,
			deadline_at = CASE WHEN ended_at IS NULL THEN $5 ELSE deadline_at END, updated_at = $6 WHERE id = $1`,
			b.ID, b.Phase, b.Open, state, deadline, tx.Now)
	}
	if err != nil {
		return err
	}
	for _, e := range b.pending {
		if _, err := tx.Exec(tx.Ctx, `INSERT INTO room_log (game_id, at, subject, text, mentions) VALUES ($1, $2, $3, $4, $5)`,
			b.ID, tx.Now, e.Subject, e.Text, nonNil(e.Mentions)); err != nil {
			return err
		}
	}
	b.pending = nil
	return nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// stop ends a game that can't go on, and tells its players why. Called
// under its lock.
func (s *Server) stop(tx *Tx, id, reason string) error {
	if _, err := tx.Exec(tx.Ctx, `INSERT INTO room_log (game_id, at, subject, text) VALUES ($1, $2, 'Game stopped', $3)`,
		id, tx.Now, "This game had to stop: "+reason+" Call the `join` tool to play another."); err != nil {
		return err
	}
	_, err := tx.Exec(tx.Ctx, `UPDATE games SET open = false, stopped = $2, ended_at = coalesce(ended_at, $3), deadline_at = $4, updated_at = $3 WHERE id = $1`,
		id, reason, tx.Now, tx.Now.Add(stopLinger))
	return err
}

// Find is the conversation's current (or last) game whose room is still
// open, and its player key: its session's, or, without a session, the
// agent's only one (none if it has several: it has to say which).
func (s *Server) Find(ctx context.Context, workspace, agent, session string) (game, player string, err error) {
	q := `SELECT s.game_id, s.player, s.session FROM seats s JOIN games g ON g.id = s.game_id
		WHERE s.workspace = $1 AND s.agent = $2 AND s.handle IS NOT NULL AND g.closed_at IS NULL`
	args := []any{workspace, agent}
	if session != "" {
		q += ` AND s.session = $3`
		args = append(args, session)
	}
	rows, _ := s.Store.Pool.Query(ctx, q+` ORDER BY s.seated_at DESC LIMIT 100`, args...)
	type seat struct{ Game, Player, Session string }
	seats, err := pgx.CollectRows(rows, pgx.RowToStructByPos[seat])
	if err != nil || len(seats) == 0 {
		return "", "", err
	}
	if session != "" {
		return seats[0].Game, seats[0].Player, nil
	}
	players := map[string]bool{}
	for _, st := range seats {
		if st.Session == "" {
			return st.Game, st.Player, nil
		}
		players[st.Player] = true
	}
	if len(players) != 1 {
		return "", "", nil
	}
	return seats[0].Game, seats[0].Player, nil
}

// Log is what's been said in a game's room (the last logLimit entries).
func (s *Server) Log(ctx context.Context, id string) ([]Entry, error) {
	rows, _ := s.Store.Pool.Query(ctx, `SELECT at, from_handle, text FROM (
		SELECT seq, at, from_handle, text FROM room_log WHERE game_id = $1 ORDER BY seq DESC LIMIT $2) l ORDER BY seq`, id, logLimit)
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Entry, error) {
		var e Entry
		err := r.Scan(&e.At, &e.From, &e.Text)
		return e, err
	})
}

// reservation is a seat matchmaking has kept for a player.
type reservation struct {
	game, name string
	roomReady  bool
	current    bool   // the player's already seated in this game
	handle     string // its handle there, if current
}

// reserve finds the player's current game, or reserves a seat in the
// oldest open game with one free (making a game if there's none). A game
// never seats two players of one workspace: one person's agents (or
// conversations) would pool what each is told. Workspaces in SharedSeats
// are exempt, for demos. It holds
// the matchmaking lock: joins from every instance go through here one at a
// time, so they fill the same games.
func (s *Server) reserve(ctx context.Context, workspace, agent, session string) (reservation, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return reservation{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, matchLock); err != nil {
		return reservation{}, err
	}
	key := PlayerKey(workspace, agent, session)
	stale := tx.Now.Add(-reserveFor)
	var r reservation
	var handle *string
	err = tx.QueryRow(ctx, `SELECT s.game_id, s.handle FROM seats s JOIN games g ON g.id = s.game_id
		WHERE s.player = $1 AND g.ended_at IS NULL AND (s.handle IS NOT NULL OR s.reserved_at > $2)
		ORDER BY s.reserved_at DESC LIMIT 1`, key, stale).Scan(&r.game, &handle)
	switch {
	case err == nil && handle != nil:
		r.current, r.handle = true, *handle
		return r, nil
	case err == nil:
		return r, ErrJoining
	case !errors.Is(err, pgx.ErrNoRows):
		return r, err
	}
	var names []string
	err = tx.QueryRow(ctx, `SELECT id, names, room_ready FROM games g WHERE open AND ended_at IS NULL
		AND (SELECT count(*) FROM seats s WHERE s.game_id = g.id AND (s.handle IS NOT NULL OR s.reserved_at > $1)) < g.max_seats
		AND ($3 OR NOT EXISTS (SELECT 1 FROM seats s WHERE s.game_id = g.id AND s.workspace = $2 AND (s.handle IS NOT NULL OR s.reserved_at > $1)))
		ORDER BY created_at LIMIT 1`, stale, workspace, s.SharedSeats[workspace]).Scan(&r.game, &names, &r.roomReady)
	if errors.Is(err, pgx.ErrNoRows) {
		r.game, err = s.newGame(tx)
	}
	if err != nil {
		return r, err
	}
	if s.PlayerName != nil {
		r.name = s.PlayerName(key)
	} else {
		r.name = pickName(names)
	}
	// One statement, to keep the matchmaking lock briefly: drop the game's
	// abandoned reservations, note the name, reserve the seat.
	if _, err := tx.Exec(ctx, `WITH stale AS (
			DELETE FROM seats WHERE game_id = $1 AND handle IS NULL AND reserved_at <= $2 AND player <> $3),
		named AS (UPDATE games SET names = array_append(names, $7) WHERE id = $1)
		INSERT INTO seats (game_id, player, workspace, agent, session, name, reserved_at) VALUES ($1, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (game_id, player) DO UPDATE SET name = excluded.name, reserved_at = excluded.reserved_at, handle = NULL, seated_at = NULL`,
		r.game, stale, key, workspace, agent, session, r.name, tx.Now); err != nil {
		return r, err
	}
	return r, tx.Commit(ctx)
}

// newGame makes a game for matchmaking. Called with the matchmaking lock.
func (s *Server) newGame(tx *Tx) (string, error) {
	for range 5 {
		g := s.NewState()
		b := g.base()
		b.ID, b.Open = NewID(), true
		s.NewGame(g, tx.Now)
		state, err := json.Marshal(g)
		if err != nil {
			return "", err
		}
		var deadline *time.Time
		if !b.Deadline.IsZero() {
			deadline = &b.Deadline
		}
		tag, err := tx.Exec(tx.Ctx, `INSERT INTO games (id, phase, max_seats, state, deadline_at, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $6)
			ON CONFLICT (id) DO NOTHING`, b.ID, b.Phase, s.MaxSeats, state, deadline, tx.Now)
		if err != nil {
			return "", err
		}
		if tag.RowsAffected() == 1 {
			return b.ID, nil
		}
	}
	return "", errors.New("gamekit: no free game ID")
}

// unreserve gives up a reserved seat.
func (s *Server) unreserve(ctx context.Context, game, player string) {
	s.Store.Pool.Exec(context.WithoutCancel(ctx), `DELETE FROM seats WHERE game_id = $1 AND player = $2 AND handle IS NULL`, game, player)
}
