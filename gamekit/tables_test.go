package gamekit

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// Tables that play game after game: matchmaking fills tables in
// PreferPhases first (between games, waiting for players), then those
// mid-game, which take a joiner (Seating) though the state they save says
// they're not open, so a revision from before tables, which seats only in
// an open game, never puts a player into a game under way.
func TestTablesSeatBetweenGamesFirst(t *testing.T) {
	ctx := context.Background()
	s := kitServer(t, "4", nil)
	s.PreferPhases = []string{"waiting", "next game"}
	put := func(id, phase string, open, seating bool, at time.Time) {
		g := &testGame{}
		g.ID, g.Phase, g.Open, g.Seating, g.GameNo = id, phase, open, seating, 1
		state, _ := json.Marshal(g)
		if _, err := s.Store.Pool.Exec(ctx, `INSERT INTO games (id, phase, open, max_seats, state, created_at) VALUES ($1, $2, true, 3, $3, $4)`,
			id, phase, state, at); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	put("midgame", "night", false, true, now.Add(-time.Hour)) // the oldest
	put("between", "next game", false, true, now)
	r, err := s.reserve(ctx, "w1", "a", "")
	if err != nil || r.game != "between" {
		t.Fatalf("first reservation: %+v %v, want the table between games", r, err)
	}
	r, err = s.reserve(ctx, "w2", "a", "")
	if err != nil || r.game != "between" {
		t.Fatalf("second: %+v %v", r, err)
	}
	r, err = s.reserve(ctx, "w3", "a", "")
	if err != nil || r.game != "between" {
		t.Fatalf("third: %+v %v", r, err)
	}
	// It's full: the table mid-game next, rather than a new one.
	r, err = s.reserve(ctx, "w4", "a", "")
	if err != nil || r.game != "midgame" {
		t.Fatalf("fourth: %+v %v, want the table mid-game", r, err)
	}

	// A move saves a seating table as open to matchmaking, and its state
	// as not open.
	if _, err := s.Update(ctx, "midgame", nil, func(tx *Tx, g State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	var open bool
	var state []byte
	if err := s.Store.Pool.QueryRow(ctx, `SELECT open, state FROM games WHERE id = 'midgame'`).Scan(&open, &state); err != nil {
		t.Fatal(err)
	}
	var saved struct{ Open, Seating bool }
	json.Unmarshal(state, &saved)
	if !open || saved.Open || !saved.Seating {
		t.Fatalf("a seating table saved: column open %v, state %+v", open, saved)
	}
}

func TestNextGamePauseEnv(t *testing.T) {
	for v, want := range map[string]time.Duration{"": 0, "20s": 20 * time.Second, "5s": 5 * time.Second, "5m": 5 * time.Minute} {
		t.Setenv("GAMES_NEXT_GAME_PAUSE", v)
		if d, err := NextGamePauseEnv(); err != nil || d != want {
			t.Errorf("%q: %v %v, want %v", v, d, err, want)
		}
	}
	for _, v := range []string{"4s", "6m", "twenty", "20"} {
		t.Setenv("GAMES_NEXT_GAME_PAUSE", v)
		if _, err := NextGamePauseEnv(); err == nil {
			t.Errorf("%q: no error", v)
		}
	}
}
