package gamekit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/wakeshift-go/appkit"
)

// The ticker: what happens when nobody's calling. Every instance runs it
// (Run), and Cloud Scheduler's ping (POST /tick) keeps an instance's CPU
// on for it between players' requests. Due games are claimed with FOR
// UPDATE SKIP LOCKED, and each step checks the deadline again under the
// game's lock, so any number of tickers can run at once and each step
// happens once.

const (
	tickEvery   = time.Second
	duePerTick  = 200              // games advanced per tick, at most
	tickWorkers = 8                // at once, per instance
	sweepAfter  = 2 * time.Second  // times (attempts² + 1): about 5 minutes over maxAttempts
	tickHold    = 55 * time.Second // how long POST /tick keeps ticking
	tickLease   = 30 * time.Second // a claimed step another instance may take over after
)

// Run ticks until ctx is done.
func (s *Server) Run(ctx context.Context) {
	for {
		if _, err := s.Tick(ctx); err != nil && ctx.Err() == nil {
			slog.Warn(s.Name+": ticking", "err", err)
		}
		// A little jitter, so instances don't all query at once.
		select {
		case <-ctx.Done():
			return
		case <-time.After(tickEvery + time.Duration(rand.IntN(200))*time.Millisecond):
		}
	}
}

// Tick advances every game whose deadline has passed, and posts
// announcements left unposted (an instance stopped, or Wakeshift failed). It
// reports how many games it advanced.
func (s *Server) Tick(ctx context.Context) (int, error) {
	now := s.now()
	type due struct {
		ID    string
		Ended bool
	}
	// Claim them (a lease, so other instances don't do the same work), as
	// internal/pipeline's runner does: FOR UPDATE SKIP LOCKED.
	rows, _ := s.Store.Pool.Query(ctx, `UPDATE games SET ticking_until = $2 WHERE id IN (
		SELECT id FROM games WHERE deadline_at <= $1 AND (ticking_until IS NULL OR ticking_until < $1)
		ORDER BY deadline_at LIMIT $3 FOR UPDATE SKIP LOCKED)
		RETURNING id, ended_at IS NOT NULL`, now, now.Add(tickLease), duePerTick)
	games, err := pgx.CollectRows(rows, pgx.RowToStructByPos[due])
	if err != nil {
		return 0, err
	}
	var n int
	var mu sync.Mutex
	work := make(chan due)
	var wg sync.WaitGroup
	for range min(tickWorkers, len(games), s.tickers()) {
		wg.Go(func() {
			for g := range work {
				var away map[string]bool
				if !g.Ended {
					away = s.Away(ctx, g.ID) // before the lock: Wakeshift isn't called under it
				}
				actx, cancel := context.WithTimeout(ctx, tickLease)
				ok, err := s.advance(actx, g.ID, away)
				cancel()
				if err != nil {
					slog.Warn(s.Name+": advancing a game", "game", g.ID, "err", err)
				}
				if ok {
					mu.Lock()
					n++
					mu.Unlock()
				}
			}
		})
	}
	for _, g := range games {
		work <- g
	}
	close(work)
	wg.Wait()

	// Announcements nobody posted: a failed post is retried, backing off.
	rows, _ = s.Store.Pool.Query(ctx, `SELECT DISTINCT game_id FROM room_log WHERE posted_at IS NULL AND attempts < 8
		AND at < $1::timestamptz - make_interval(secs => $2::float8 * (attempts * attempts + 1)) LIMIT 100`, now, sweepAfter.Seconds())
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return n, err
	}
	for _, id := range ids {
		s.deliver(ctx, id)
	}
	return n, s.forget(ctx, now)
}

// tickers is how many games an instance advances at once: each holds one
// of the pool's connections, and one is left for players' moves.
func (s *Server) tickers() int {
	return max(1, int(s.Store.Pool.Config().MaxConns)-1)
}

// forget deletes what players have had lately (recent) once they haven't
// played for recentFor. It runs about once an hour per instance.
func (s *Server) forget(ctx context.Context, now time.Time) error {
	last := s.forgotAt.Load()
	if last != 0 && now.Sub(time.Unix(0, last)) < time.Hour || !s.forgotAt.CompareAndSwap(last, now.UnixNano()) {
		return nil
	}
	_, err := s.Store.Pool.Exec(ctx, `DELETE FROM recent WHERE updated_at < $1`, now.Add(-recentFor))
	return err
}

// advance runs game id's due step, if it's still due (a player's move may
// have moved it on): the game's Step, or for a game that's over, closing
// its room and, later, forgetting it. It reports whether it ran one.
func (s *Server) advance(ctx context.Context, id string, away map[string]bool) (bool, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	tx.Away = away
	var state []byte
	var endedAt *time.Time
	var roomClosed bool
	err = tx.QueryRow(ctx, `SELECT state, ended_at, room_closed FROM games
		WHERE id = $1 AND deadline_at <= $2 FOR UPDATE`, id, tx.Now).Scan(&state, &endedAt, &roomClosed)
	if errors.Is(err, pgx.ErrNoRows) {
		// Not due any more. On the transaction's own connection: taking a
		// second from the pool while holding this one can wait forever.
		if _, err := tx.Exec(ctx, `UPDATE games SET ticking_until = NULL WHERE id = $1`, id); err != nil {
			return false, err
		}
		return false, tx.Commit(ctx)
	}
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE games SET ticking_until = NULL WHERE id = $1`, id); err != nil {
		return false, err
	}
	switch {
	case roomClosed: // kept a while for the follow-along page: forget it
		if _, err := tx.Exec(ctx, `DELETE FROM games WHERE id = $1`, id); err != nil {
			return false, err
		}
		return true, tx.Commit(ctx)
	case endedAt != nil: // over: close its room, retrying until it's closed
		if _, err := tx.Exec(ctx, `UPDATE games SET closed_at = coalesce(closed_at, $2), deadline_at = $3 WHERE id = $1`, id, tx.Now, tx.Now.Add(closeRetry)); err != nil {
			return false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return false, err
		}
		s.deliver(ctx, id) // the last word first
		if err := s.closeRoom(ctx, id); err != nil {
			return true, err
		}
		_, err := s.Store.Pool.Exec(ctx, `UPDATE games SET room_closed = true, deadline_at = greatest($2, ended_at + make_interval(secs => $3::float8)) WHERE id = $1`,
			id, s.now(), keepEnded.Seconds())
		return true, err
	}

	g, derr := s.decode(row{state: state})
	if derr == nil {
		derr = s.step(tx, g)
	}
	var stepErr stepError
	switch {
	case errors.As(derr, &stepErr):
		slog.Error(s.Name+": a game's step failed", "game", id, "err", derr)
		tx.Rollback(ctx) // what the step changed is discarded
		if tx, err = s.begin(ctx); err != nil {
			return false, err
		}
		defer tx.Rollback(context.WithoutCancel(ctx))
		if err := tx.QueryRow(ctx, `SELECT id FROM games WHERE id = $1 FOR UPDATE`, id).Scan(&id); err != nil {
			return false, err
		}
		if err := s.stop(tx, id, "something went wrong on our side."); err != nil {
			return false, err
		}
	case derr != nil && errors.Is(derr, errDecode):
		if err := s.stop(tx, id, unreadable); err != nil {
			return false, err
		}
	case derr != nil:
		return false, derr // the database: try again next tick
	default:
		if b := g.base(); !b.ended && (b.Deadline.IsZero() || !b.Deadline.After(tx.Now)) {
			// A step must move the game on; one that didn't would run
			// every tick.
			slog.Error(s.Name+": a game's step didn't move it on", "game", id, "phase", b.Phase)
			tx.Rollback(ctx)
			return false, s.stopNow(ctx, id, "something went wrong on our side.")
		}
		if err := s.save(tx, g, false); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	s.deliver(ctx, id)
	return true, nil
}

// stopNow stops a game in a transaction of its own.
func (s *Server) stopNow(ctx context.Context, id, reason string) error {
	tx, err := s.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if err := tx.QueryRow(ctx, `SELECT id FROM games WHERE id = $1 FOR UPDATE`, id).Scan(&id); err != nil {
		return err
	}
	if err := s.stop(tx, id, reason); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	s.deliver(ctx, id)
	return nil
}

var errDecode = errors.New("gamekit: undecodable state")

// unreadable is why a game whose state can't be read stops.
const unreadable = "its saved state couldn't be read after an update on our side."

// stepError is a game's Step panicking: a bug, not the database.
type stepError struct{ v any }

func (e stepError) Error() string { return fmt.Sprintf("gamekit: step panicked: %v", e.v) }

// step runs the game's Step, turning a panic into a stepError.
func (s *Server) step(tx *Tx, g State) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = stepError{v}
		}
	}()
	return s.Step(tx, g)
}

// deliver posts game id's unposted announcements to its room, in order,
// unless another instance is posting them. A failed post is left for the
// ticker to retry; the ones after it wait, to keep the order.
func (s *Server) deliver(ctx context.Context, id string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	for range 3 {
		now := s.now()
		// The lease is the time it runs to (to the microsecond, as Postgres
		// keeps it): renewing or clearing it checks it's still this one's.
		lease := now.Add(postLease).Truncate(time.Microsecond)
		tag, err := s.Store.Pool.Exec(ctx, `UPDATE games SET posting_until = $2 WHERE id = $1 AND (posting_until IS NULL OR posting_until < $3)
			AND EXISTS (SELECT 1 FROM room_log WHERE game_id = $1 AND posted_at IS NULL AND attempts < 8)`, id, lease, now)
		if err != nil || tag.RowsAffected() == 0 {
			return // nothing to post, or another instance is posting
		}
		type entry struct {
			Seq           int64
			Subject, Text string
			Mentions      []string
		}
		rows, _ := s.Store.Pool.Query(ctx, `SELECT seq, subject, text, mentions FROM room_log
			WHERE game_id = $1 AND posted_at IS NULL AND attempts < 8 ORDER BY seq LIMIT 50`, id)
		entries, err := pgx.CollectRows(rows, pgx.RowToStructByPos[entry])
		failed := err != nil
		for _, e := range entries {
			// Each post renews the lease, so while Wakeshift is slow no
			// other instance posts the same entries.
			renewed := s.now().Add(postLease).Truncate(time.Microsecond)
			tag, err := s.Store.Pool.Exec(ctx, `UPDATE games SET posting_until = $3 WHERE id = $1 AND posting_until = $2`, id, lease, renewed)
			if err != nil || tag.RowsAffected() == 0 {
				return // the lease was lost: whoever holds it posts the rest
			}
			lease = renewed
			post, cancel := context.WithTimeout(ctx, 15*time.Second)
			_, err = s.Kit.PostToGroup(post, s.room(id), appkit.GroupPost{Subject: e.Subject, Body: e.Text,
				Data: map[string]any{"game": id, "text": e.Text}, Mentions: e.Mentions})
			cancel()
			if err != nil && !appkit.IsCode(err, "unknown_group") {
				slog.Warn(s.Name+": announcing", "game", id, "err", err)
				s.Store.Pool.Exec(ctx, `UPDATE room_log SET attempts = attempts + 1 WHERE game_id = $1 AND seq = $2`, id, e.Seq)
				failed = true
				break
			}
			if _, err := s.Store.Pool.Exec(ctx, `UPDATE room_log SET posted_at = $3 WHERE game_id = $1 AND seq = $2`, id, e.Seq, s.now()); err != nil {
				// Left until the lease runs out: clearing it would have the
				// entry posted again at once.
				slog.Warn(s.Name+": noting an announcement posted", "game", id, "err", err)
				return
			}
		}
		s.Store.Pool.Exec(ctx, `UPDATE games SET posting_until = NULL WHERE id = $1 AND posting_until = $2`, id, lease)
		if failed {
			return
		}
		// Loop: a move may have announced more while this was posting.
	}
}

// closeRoom closes a game's group in Wakeshift (one already gone counts).
func (s *Server) closeRoom(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if err := s.Kit.CloseGroup(ctx, s.room(id)); err != nil && !appkit.IsCode(err, "unknown_group") {
		return err
	}
	return nil
}

// tickRoutes serves POST /tick, Cloud Scheduler's ping: it keeps ticking
// for most of a minute (one at a time per instance), so the instance has
// CPU for the timers even when no player is calling.
func (s *Server) tickRoutes(mux *http.ServeMux) {
	if s.TickAuth == nil {
		return
	}
	mux.HandleFunc("POST /tick", func(w http.ResponseWriter, r *http.Request) {
		if err := s.TickAuth(r); err != nil {
			http.Error(w, "not allowed", http.StatusForbidden)
			return
		}
		if !s.ticking.CompareAndSwap(false, true) {
			w.WriteHeader(http.StatusNoContent) // this instance is ticking already
			return
		}
		defer s.ticking.Store(false)
		ctx, cancel := context.WithTimeout(r.Context(), tickHold)
		defer cancel()
		for {
			if _, err := s.Tick(ctx); err != nil && ctx.Err() == nil {
				slog.Warn(s.Name+": ticking", "err", err)
			}
			select {
			case <-ctx.Done():
				w.WriteHeader(http.StatusNoContent)
				return
			case <-time.After(tickEvery):
			}
		}
	})
}

// GoogleTickAuth accepts a ping signed by Google for account (Cloud
// Scheduler's OIDC token, for audience: the /tick URL).
func GoogleTickAuth(audience, account string) func(*http.Request) error {
	var mu sync.Mutex
	var provider *oidc.Provider
	return func(r *http.Request) error {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			return errors.New("no token")
		}
		mu.Lock()
		p := provider
		mu.Unlock()
		if p == nil {
			var err error
			if p, err = oidc.NewProvider(context.WithoutCancel(r.Context()), "https://accounts.google.com"); err != nil {
				return err
			}
			mu.Lock()
			provider = p
			mu.Unlock()
		}
		idt, err := p.Verifier(&oidc.Config{ClientID: audience}).Verify(r.Context(), token)
		if err != nil {
			return err
		}
		var claims struct {
			Email    string `json:"email"`
			Verified bool   `json:"email_verified"`
		}
		if err := idt.Claims(&claims); err != nil || !claims.Verified || claims.Email != account {
			return errors.New("not signed by the expected service account")
		}
		return nil
	}
}
