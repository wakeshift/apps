package nightfall

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/wakeshift/apps/agentkit"
	"github.com/wakeshift/apps/gamekit"
	"github.com/wakeshift/apps/gamekit/gametest"
)

// A game running when tables came in (its state from the release before:
// no game number, not seating) finishes as its table's first game, and the
// table then seats joiners between games, so a player turning over rejoins
// it rather than opening another.
func TestGameFromBeforeTablesCarriesOver(t *testing.T) {
	ctx := context.Background()
	app := &App{Kit: agentkit.TestApp("nightfall", nil, "", "https://nf.example.test/mcp"), Pause: time.Minute}
	app.Store, _ = gametest.Store(t)
	app.server()
	g := table([]string{vampire, lamplighter, townsperson}, [centerSize]string{changeling, gravedigger, watchman})
	g.ID, g.Phase, g.Deadline = "nfold", voting, time.Now().Add(-time.Second)
	for i, p := range g.Players {
		p.Player = gamekit.Player{Workspace: string(rune('a' + i)), Agent: "a", Handle: []string{"tern", "urchin", "lemur"}[i]}
	}
	g.Players[0].Vote, g.Players[1].Vote = "urchin", "tern"
	gametest.Put(t, app.Store, g.ID, voting, false, false, g)
	if _, err := app.Store.Pool.Exec(ctx, `UPDATE games SET deadline_at = $2 WHERE id = $1`, g.ID, g.Deadline); err != nil {
		t.Fatal(err)
	}
	if _, err := app.srv.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	var open bool
	var phase string
	var state []byte
	if err := app.Store.Pool.QueryRow(ctx, `SELECT open, phase, state FROM games WHERE id = 'nfold'`).Scan(&open, &phase, &state); err != nil {
		t.Fatal(err)
	}
	var after game
	json.Unmarshal(state, &after)
	if phase != next || !open || after.GameNo != 1 || !after.Seating || after.Open {
		t.Fatalf("after its finish: phase %q, column open %v, game_no %d, seating %v, open %v", phase, open, after.GameNo, after.Seating, after.Open)
	}
	if s := app.status(&after); s[:len("game 1 over")] != "game 1 over" {
		t.Fatalf("status %q", s)
	}
}
