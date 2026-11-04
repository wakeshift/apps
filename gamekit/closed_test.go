package gamekit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/apps/internal/pgtest"
)

// closeLines captures the default logger's "table closed" lines as JSON
// objects, until the test ends.
func closeLines(t *testing.T) func() []map[string]any {
	t.Helper()
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return func() []map[string]any {
		var out []map[string]any
		for line := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
			var m map[string]any
			if json.Unmarshal([]byte(line), &m) == nil && strings.HasSuffix(fmt.Sprint(m["msg"]), ": table closed") {
				out = append(out, m)
			}
		}
		return out
	}
}

// A table's close is logged once its move commits, from a tool's move
// (Update) or the ticker's step, and not when the move doesn't commit.
func TestTableClosedLoggedOnCommit(t *testing.T) {
	ctx := context.Background()
	st, err := OpenStore(ctx, pgtest.EmptyURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	now := time.Now()
	s := &Server{Name: "nightfall", Store: st, Now: func() time.Time { return now }, NewState: func() State { return &Game{} }}
	s.Step = func(tx *Tx, gs State) error {
		g := gs.(*Game)
		s.TableClosed(tx, g, ClosedShort, 10*time.Minute)
		if g.ID == "t3" {
			panic("a bug after the close") // rolled back, and the game stopped in a transaction of its own
		}
		g.End(time.Minute)
		return nil
	}
	for _, id := range []string{"t1", "t2", "t3"} {
		if _, err := st.Pool.Exec(ctx, `INSERT INTO games (id, phase, max_seats, state, deadline_at) VALUES ($1, 'waiting', 3, $2, $3)`,
			id, fmt.Sprintf(`{"id":%q,"phase":"waiting","game_no":4}`, id), now.Add(-time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	lines := closeLines(t)

	// A move that fails: rolled back, nothing logged.
	_, err = s.Update(ctx, "t1", nil, func(tx *Tx, gs State) error {
		s.TableClosed(tx, gs.(*Game), ClosedEmpty, 0)
		return errors.New("no")
	})
	if err == nil || len(lines()) != 0 {
		t.Fatalf("a move that didn't commit: %v, %v", err, lines())
	}
	// A move that commits.
	if _, err := s.Update(ctx, "t1", nil, func(tx *Tx, gs State) error {
		s.TableClosed(tx, gs.(*Game), ClosedEmpty, -time.Second) // a clock that went back: 0
		gs.(*Game).End(time.Minute)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// The ticker's step.
	if ok, err := s.advance(ctx, "t2", nil); !ok || err != nil {
		t.Fatalf("advance: %v, %v", ok, err)
	}
	// A step that panicked after queuing the line: nothing logged.
	if ok, err := s.advance(ctx, "t3", nil); !ok || err != nil {
		t.Fatalf("advance a step that panics: %v, %v", ok, err)
	}
	var stopped bool
	if err := st.Pool.QueryRow(ctx, `SELECT stopped IS NOT NULL FROM games WHERE id = 't3'`).Scan(&stopped); err != nil || !stopped {
		t.Fatalf("t3 not stopped: %v, %v", stopped, err)
	}
	got := lines()
	if len(got) != 2 {
		t.Fatalf("%d lines: %v", len(got), got)
	}
	for i, want := range []map[string]any{
		{"msg": "nightfall: table closed", "level": "INFO", "game": "nightfall", "table": "t1", "reason": "empty", "games_played": 4.0, "waited_s": 0.0},
		{"msg": "nightfall: table closed", "level": "INFO", "game": "nightfall", "table": "t2", "reason": "short_of_players", "games_played": 4.0, "waited_s": 600.0},
	} {
		delete(got[i], "time")
		if fmt.Sprint(got[i]) != fmt.Sprint(want) {
			t.Errorf("line %d: %v, want %v", i, got[i], want)
		}
	}
}
