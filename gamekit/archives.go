package gamekit

import (
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
)

// A game that finishes keeps an anonymized record of itself for good, in
// game_archives (migration 005): the game builds the record with Anon
// (archive.go), from its state and what its players said (Said), and writes
// it with Archive in the move that finishes it. The record carries no game
// ID and nothing of a player but its label, so deleting a game, a player or
// a player never needs to touch it.

// Said is what the players said in a game's room, in order, with when
// (the moderator's announcements left out). Called under the game's lock.
func (tx *Tx) Said(game string) ([]Entry, error) {
	rows, _ := tx.Query(tx.Ctx, `SELECT at, from_handle, text FROM room_log WHERE game_id = $1 AND from_handle <> '' ORDER BY seq`, game)
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Entry, error) {
		var e Entry
		err := r.Scan(&e.At, &e.From, &e.Text)
		return e, err
	})
}

// Archive keeps record (JSON) for good, dated with the UTC day the move
// runs on (the day only). It must name no player: build it with Anon.
func (tx *Tx) Archive(record any) error {
	b, err := json.Marshal(record)
	if err != nil {
		return err
	}
	day := tx.Now.UTC().Format(time.DateOnly)
	_, err = tx.Exec(tx.Ctx, `INSERT INTO game_archives (played_on, record) VALUES ($1, $2)`, day, b)
	return err
}

// Offset is at's time since start, in milliseconds (never negative): when
// something happened in an archived game.
func Offset(start, at time.Time) int64 {
	return max(0, at.Sub(start).Milliseconds())
}
