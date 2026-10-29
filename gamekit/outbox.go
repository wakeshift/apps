package gamekit

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/wakeshift-go/appkit"
)

// The outbox: what's said in a room (room_log) is sent to the game's other
// seated players as game.update, in order, by whichever instance holds the
// game's posting lease (deliver), retried until Wakeshift accepts it. Each
// entry goes in at most two emits, the players it mentions and the others,
// so each recipient's Mentioned is right, with IDs <game>:<seq>:m and
// <game>:<seq>:o that make a retry safe. An entry's recipients are fixed on
// its first try (room_log.recipients): Wakeshift dedupes by the emit's ID
// and keeps the first's list, so a retry must send what the first did, not
// a list recomputed after someone joined or left. A seated player whose
// conversation subscribed is addressed by its subscription ID; a native seat
// (a client that subscribes itself) by {native: user}. The speaker never
// hears itself.

// recipient is one recipient of an entry, as room_log.recipients keeps it.
type recipient struct {
	User         string `json:"user,omitempty"`
	Subscription string `json:"subscription,omitempty"`
	Native       string `json:"native,omitempty"`
}

// halves are an entry's recipients: the players it mentions, and the
// others.
type halves struct {
	M []recipient `json:"m,omitempty"`
	O []recipient `json:"o,omitempty"`
}

// entry is an unsent room_log row, as deliver sends it.
type entry struct {
	Seq           int64
	At            time.Time
	From          string
	Subject, Text string
	Mentions      []string
	Recipients    []byte
	RetryAt       *time.Time
}

// An entry's emits are tried 8 times (rate_limited doesn't count) before
// it's given up: the queries say "tries < 8" as a literal, as migration
// 008's partial index does, so the planner can use the index.

// deliver sends game id's unsent entries, in order, unless another instance
// is sending them. A failed emit is left for the ticker to retry; the ones
// after it wait, to keep the order.
func (s *Server) deliver(ctx context.Context, id string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	for range 3 {
		now := s.now()
		// The lease is the time it runs to (to the microsecond, as Postgres
		// keeps it): renewing or clearing it checks it's still this one's.
		lease := now.Add(postLease).Truncate(time.Microsecond)
		tag, err := s.Store.Pool.Exec(ctx, `UPDATE games SET posting_until = $2 WHERE id = $1 AND (posting_until IS NULL OR posting_until < $3)
			AND EXISTS (SELECT 1 FROM room_log WHERE game_id = $1 AND posted_at IS NULL AND tries < 8 AND (retry_at IS NULL OR retry_at <= $3))`, id, lease, now)
		if err != nil || tag.RowsAffected() == 0 {
			return // nothing to send, or another instance is sending
		}
		rows, _ := s.Store.Pool.Query(ctx, `SELECT seq, at, from_handle, subject, text, mentions, recipients, retry_at FROM room_log
			WHERE game_id = $1 AND posted_at IS NULL AND tries < 8 ORDER BY seq LIMIT 50`, id)
		entries, err := pgx.CollectRows(rows, pgx.RowToStructByPos[entry])
		failed := err != nil
		for _, e := range entries {
			// Each entry renews the lease, so while Wakeshift is slow no
			// other instance sends the same entries.
			renewed := s.now().Add(postLease).Truncate(time.Microsecond)
			tag, err := s.Store.Pool.Exec(ctx, `UPDATE games SET posting_until = $3 WHERE id = $1 AND posting_until = $2`, id, lease, renewed)
			if err != nil || tag.RowsAffected() == 0 {
				return // the lease was lost: whoever holds it sends the rest
			}
			lease = renewed
			if e.RetryAt != nil && e.RetryAt.After(s.now()) || !s.sendEntry(ctx, id, e) {
				failed = true
				break
			}
		}
		s.Store.Pool.Exec(ctx, `UPDATE games SET posting_until = NULL WHERE id = $1 AND posting_until = $2`, id, lease)
		if failed {
			return
		}
		// Loop: a move may have said more while this was sending.
	}
}

// sendEntry makes an entry's emits and records what came of it: sent
// (forgetting subscriptions Wakeshift says have ended), or failed and put
// off. It reports whether it was sent.
func (s *Server) sendEntry(ctx context.Context, game string, e entry) bool {
	var to halves
	if len(e.Recipients) > 0 {
		if err := json.Unmarshal(e.Recipients, &to); err != nil {
			slog.Error(s.Name+": an entry's recipients are unreadable; dropping it", "game", game, "err", err)
			s.Store.Pool.Exec(ctx, `UPDATE room_log SET posted_at = $3 WHERE game_id = $1 AND seq = $2`, game, e.Seq, s.now())
			return true
		}
	} else {
		var err error
		if to, err = s.recipients(ctx, game, e); err != nil {
			slog.Warn(s.Name+": finding an entry's recipients", "game", game, "err", err)
			return false
		}
		b, _ := json.Marshal(to)
		if _, err := s.Store.Pool.Exec(ctx, `UPDATE room_log SET recipients = $3 WHERE game_id = $1 AND seq = $2`, game, e.Seq, b); err != nil {
			slog.Warn(s.Name+": noting an entry's recipients", "game", game, "err", err)
			return false
		}
	}
	var ended []appkit.Recipient
	for _, h := range []struct {
		half      string
		list      []recipient
		mentioned bool
	}{{"m", to.M, true}, {"o", to.O, false}} {
		if len(h.list) == 0 {
			continue
		}
		u := Update{ID: game + ":" + strconv.FormatInt(e.Seq, 10), Game: game, From: e.From, Subject: e.Subject, Text: e.Text,
			Mentions: e.Mentions, Mentioned: h.mentioned, At: e.At.UTC()}
		if len(u.Mentions) == 0 {
			u.Mentions = nil
		}
		emitID := u.ID + ":" + h.half
		opts := []appkit.EmitOption{appkit.WithID(emitID)}
		for _, r := range h.list {
			if r.Native != "" {
				opts = append(opts, appkit.ToNative(r.Native))
			} else {
				opts = append(opts, appkit.ToSubscription(r.User, r.Subscription))
			}
		}
		res, err := s.emit(ctx, UpdateArgs{}, u, opts)
		if err != nil {
			limited := appkit.IsCode(err, "rate_limited")
			slog.Warn(s.Name+": sending an update; will retry", "game", game, "limited", limited, "err", err)
			if limited {
				// Wakeshift's per-app limit refused the whole emit before
				// recording anything: not an attempt. It waits for the next
				// minute.
				s.Store.Pool.Exec(ctx, `UPDATE room_log SET retry_at = date_trunc('minute', $3::timestamptz) + interval '1 minute' WHERE game_id = $1 AND seq = $2`,
					game, e.Seq, s.now())
			} else {
				s.Store.Pool.Exec(ctx, `UPDATE room_log SET tries = tries + 1, retry_at = NULL WHERE game_id = $1 AND seq = $2`, game, e.Seq)
			}
			return false
		}
		ended = append(ended, res.Ended...)
	}
	if err := s.sent(ctx, game, e.Seq, ended); err != nil {
		// Left until the lease runs out: clearing it would have the entry
		// sent again at once (which the emits' IDs make harmless).
		slog.Warn(s.Name+": noting an update sent", "game", game, "err", err)
		return false
	}
	return true
}

func (s *Server) emit(ctx context.Context, args UpdateArgs, u Update, opts []appkit.EmitOption) (appkit.Emitted, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return s.events().Emit(ctx, args, u, opts...)
}

// recipients are who hears an entry: the game's seated players but its
// speaker, whose conversation is subscribed (by its subscription) or who
// subscribe themselves (native), split by whether it mentions them.
func (s *Server) recipients(ctx context.Context, game string, e entry) (halves, error) {
	type seat struct {
		Handle, Subscription, Subscriber, User string
		Native                                 bool
	}
	rows, _ := s.Store.Pool.Query(ctx, `SELECT handle, subscription, subscriber, workspace || '/' || agent, native FROM seats
		WHERE game_id = $1 AND handle IS NOT NULL AND handle <> $2 AND (native OR subscription <> '') ORDER BY seated_at, player`, game, e.From)
	seats, err := pgx.CollectRows(rows, pgx.RowToStructByPos[seat])
	if err != nil {
		return halves{}, err
	}
	var to halves
	for _, st := range seats {
		r := recipient{User: st.Subscriber, Subscription: st.Subscription}
		if st.Native {
			r = recipient{Native: st.User}
		}
		list := &to.O
		if slices.Contains(e.Mentions, st.Handle) {
			list = &to.M
		}
		if !slices.Contains(to.M, r) && !slices.Contains(to.O, r) {
			*list = append(*list, r)
		}
	}
	return to, nil
}

// sent marks an entry sent, and forgets the subscriptions Wakeshift said had
// ended (an emit's ended list, a duplicate's too): their seats aren't woken
// any more, so they're away until their players join again. One
// transaction.
func (s *Server) sent(ctx context.Context, game string, seq int64, ended []appkit.Recipient) error {
	return pgx.BeginFunc(ctx, s.Store.Pool, func(tx pgx.Tx) error {
		for _, r := range ended {
			if r.Subscription == "" {
				continue
			}
			if _, err := tx.Exec(ctx, `UPDATE seats SET subscription = '' WHERE subscription = $1 AND subscriber = $2`, r.Subscription, r.User); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `UPDATE room_log SET posted_at = $3 WHERE game_id = $1 AND seq = $2`, game, seq, s.now())
		return err
	})
}
