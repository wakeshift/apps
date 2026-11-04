package nightfall

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/apps/gamekit"
	"github.com/wakeshift/wakeshift-go/appkit"
)

// A new table waits 30 minutes for its first three players; a table that's
// played and is short of players waits 10 for them to come back. Each says
// so when it closes.
func TestTableWaits(t *testing.T) {
	app := &App{Kit: &appkit.App{ID: "nightfall", MCPURL: "https://nf.example.test/mcp"}}
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

	g := &game{}
	app.server().NewGame(g, now)
	if g.Phase != waiting || !g.Until.Equal(now.Add(30*time.Minute)) || !g.Deadline.Equal(g.Until) {
		t.Fatalf("a new table: phase %q, until %v, deadline %v", g.Phase, g.Until, g.Deadline)
	}
	g.Players = []*player{{Player: gamekit.Player{Handle: "tern"}}}
	if err := app.step(&gamekit.Tx{Now: g.Until}, g); err != nil {
		t.Fatal(err)
	}
	if g.Phase != over || len(g.Result) != 1 || g.Result[0] != "Not enough players joined in 30 minutes, so this table is closed." {
		t.Fatalf("a new table that closed: %q %q", g.Phase, g.Result)
	}

	g = &game{Players: []*player{{Player: gamekit.Player{Handle: "tern"}}, {Player: gamekit.Player{Handle: "urchin"}}}}
	g.GameNo = 1
	app.waitForPlayers(&gamekit.Tx{Now: now}, g)
	if g.Phase != waiting || !g.Until.Equal(now.Add(10*time.Minute)) || !g.Deadline.Equal(g.Until) || !g.Open {
		t.Fatalf("a table short of players: phase %q, until %v, deadline %v, open %v", g.Phase, g.Until, g.Deadline, g.Open)
	}
	if err := app.step(&gamekit.Tx{Now: now.Add(9 * time.Minute)}, g); err != nil || g.Phase != waiting {
		t.Fatalf("closed early: %q %v", g.Phase, err)
	}
	if err := app.step(&gamekit.Tx{Now: g.Until}, g); err != nil {
		t.Fatal(err)
	}
	if g.Phase != over || len(g.Result) != 1 || g.Result[0] != "Not enough players came back in 10 minutes, so this table is closed." {
		t.Fatalf("a short table that closed: %q %q", g.Phase, g.Result)
	}
}

// A table's close is logged, once it commits, with why, the games it
// played and how long it waited, and nothing about its players.
func TestTableCloseLogged(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	app := &App{Kit: &appkit.App{ID: "nightfall", MCPURL: "https://nf.example.test/mcp"}}
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

	g := &game{}
	g.ID = "t-new"
	app.server().NewGame(g, now)
	g.Players = []*player{{Player: gamekit.Player{Handle: "tern"}}}
	tx := &gamekit.Tx{Now: g.Until}
	if err := app.step(tx, g); err != nil {
		t.Fatal(err)
	}
	if logs.Len() != 0 {
		t.Fatalf("logged before the move committed: %s", logs.String())
	}
	tx.Committed()

	g = &game{Players: []*player{{Player: gamekit.Player{Handle: "tern"}}, {Player: gamekit.Player{Handle: "urchin"}}}}
	g.ID, g.GameNo = "t-short", 3
	app.waitForPlayers(&gamekit.Tx{Now: now}, g)
	tx = &gamekit.Tx{Now: g.Until.Add(5 * time.Second)}
	if err := app.step(tx, g); err != nil {
		t.Fatal(err)
	}
	tx.Committed()
	tx.Committed() // once

	got := strings.Split(strings.TrimSpace(logs.String()), "\n")
	want := []string{
		`level=INFO msg="nightfall: table closed" game=nightfall table=t-new reason=never_filled games_played=0 waited_s=1800`,
		`level=INFO msg="nightfall: table closed" game=nightfall table=t-short reason=short_of_players games_played=3 waited_s=605`,
	}
	if len(got) != len(want) {
		t.Fatalf("%d lines: %s", len(got), logs.String())
	}
	for i, line := range got {
		if _, rest, _ := strings.Cut(line, " level="); "level="+rest != want[i] {
			t.Errorf("line %d: %s, want %s", i, line, want[i])
		}
	}
	if strings.Contains(logs.String(), "tern") || strings.Contains(logs.String(), "urchin") {
		t.Errorf("a player in the log: %s", logs.String())
	}
}
