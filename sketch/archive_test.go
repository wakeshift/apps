package sketch

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/apps/gamekit"
)

// archivedRow is a sketch_rounds row as the tests read it.
type archivedRow struct {
	Room, Word, Tier, Outcome  string
	Round                      int
	PlayedOn                   time.Time
	FirstDrawMs, GuessedMs     *int
	LengthMs                   int
	Players, Drawing, Timeline []byte
}

func (f *fixture) archived() []archivedRow {
	f.t.Helper()
	rows, err := f.a.Store.Pool.Query(bg, `SELECT room, word, tier, outcome, round, played_on, first_draw_ms, guessed_ms, length_ms, players, drawing, timeline
		FROM sketch_rounds ORDER BY id`)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []archivedRow
	for rows.Next() {
		var r archivedRow
		if err := rows.Scan(&r.Room, &r.Word, &r.Tier, &r.Outcome, &r.Round, &r.PlayedOn, &r.FirstDrawMs, &r.GuessedMs, &r.LengthMs, &r.Players, &r.Drawing, &r.Timeline); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// A finished round is archived with its word, drawing and timeline, and
// nothing that names or links to a player: roles only, the house flag and
// model for house agents, handles in guesses replaced by roles. A forfeit
// is kept (with no drawing); a round voided for writing isn't.
func TestArchive(t *testing.T) {
	f := newFixture(t, 4)
	// One house player, so its model is kept.
	f.move(nil, func(tx *gamekit.Tx, g *game) error { g.Players[3].House = "Haiku 5.5"; return nil })
	start := f.clock()
	g := f.step(nil)
	word, tier := g.Word, g.Tier
	drawer := g.drawer()
	var gs []*player
	for _, p := range g.Players {
		if p != drawer {
			gs = append(gs, p)
		}
	}
	f.set(start.Add(2 * time.Second))
	f.move(nil, func(tx *gamekit.Tx, g *game) error {
		f.a.drew(tx, g, []stroke{line(10, 10, 90, 90), line(1, 1, 5, 5)})
		return nil
	})
	f.set(start.Add(4 * time.Second))
	f.move(nil, func(tx *gamekit.Tx, g *game) error {
		// A guess that names players, by handle and as shown.
		_, err := f.a.guess(tx, g, g.player(gs[0].Key()), display(gs[1].Handle)+" "+drawer.Handle+" fish", false)
		return err
	})
	f.set(start.Add(9 * time.Second))
	f.move(nil, func(tx *gamekit.Tx, g *game) error { f.a.drew(tx, g, []stroke{line(50, 0, 50, 100)}); return nil })
	f.set(start.Add(12 * time.Second))
	f.move(nil, func(tx *gamekit.Tx, g *game) error {
		_, err := f.a.guess(tx, g, g.player(gs[1].Key()), word, true)
		return err
	})
	rows := f.archived()
	if len(rows) != 1 {
		t.Fatalf("%d rows", len(rows))
	}
	r := rows[0]
	if r.Word != word || r.Tier != tier || r.Outcome != "guessed" || r.Round != 1 || len(r.Room) != 26 || r.Room == f.id ||
		!r.PlayedOn.Equal(time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("row %+v", r)
	}
	if r.FirstDrawMs == nil || *r.FirstDrawMs != 2000 || r.GuessedMs == nil || *r.GuessedMs != 12000 || r.LengthMs != 12000 {
		t.Fatalf("timings %v %v %d", r.FirstDrawMs, r.GuessedMs, r.LengthMs)
	}
	var drawing []stroke
	json.Unmarshal(r.Drawing, &drawing)
	if len(drawing) != 3 || drawing[2].Points[0] != [2]int{50, 0} {
		t.Fatalf("drawing %s", r.Drawing)
	}
	var players []gamekit.ArchivePlayer
	json.Unmarshal(r.Players, &players)
	if len(players) != 4 || players[0].Role != "drawer" || players[1].Role != "guesser 1" || players[2].Role != "guesser 2" || players[3].Role != "guesser 3" {
		t.Fatalf("players %s", r.Players)
	}
	houses := 0
	for _, p := range players {
		if p.House {
			houses++
			if p.Model != "Haiku 5.5" {
				t.Errorf("house model %+v", p)
			}
		} else if p.Model != "" {
			t.Errorf("a model for a non-house player %+v", p)
		}
	}
	if houses != 1 {
		t.Errorf("%d house players: %s", houses, r.Players)
	}
	var tl []moment
	json.Unmarshal(r.Timeline, &tl)
	want := []moment{
		{Ms: 2000, Kind: "draw", Marks: 2},
		{Ms: 4000, Kind: "guess", By: "guesser 1", Text: "guesser 2 drawer fish"},
		{Ms: 9000, Kind: "draw", Marks: 1},
		{Ms: 12000, Kind: "guess", By: "guesser 2", Text: word, Right: true, Owner: true},
	}
	if len(tl) != len(want) {
		t.Fatalf("timeline %s", r.Timeline)
	}
	for i := range want {
		if tl[i] != want[i] {
			t.Errorf("moment %d: %+v, want %+v", i, tl[i], want[i])
		}
	}
	// Nothing in the row names a player or links to one.
	all := strings.ToLower(string(r.Players) + string(r.Drawing) + string(r.Timeline) + r.Room)
	for _, p := range g.Players {
		for _, leak := range []string{p.Handle, strings.ToLower(display(p.Handle)), p.Workspace + "/", p.Key()} {
			if strings.Contains(all, leak) {
				t.Errorf("the archive has %q: %s", leak, all)
			}
		}
	}
	if strings.Contains(all, f.id) {
		t.Error("the archive has the game's ID")
	}

	// A forfeit is kept, with the same room ID and no drawing.
	f.step(nil) // the next round starts
	g = f.step(nil)
	rows = f.archived()
	if len(rows) != 2 || rows[1].Outcome != "forfeit" || rows[1].Room != r.Room || rows[1].Round != 2 || rows[1].FirstDrawMs != nil || string(rows[1].Drawing) != "[]" {
		t.Fatalf("forfeit row %+v", rows[len(rows)-1])
	}
	// A round voided for writing isn't.
	f.set(f.clock().Add(time.Second))
	f.move(nil, func(tx *gamekit.Tx, g *game) error {
		f.a.drew(tx, g, []stroke{line(1, 1, 2, 2)})
		return f.a.endRound(tx, g, nil, "writing", "void")
	})
	if n := len(f.archived()); n != 2 {
		t.Fatalf("%d rows after a round voided for writing", n)
	}
}

// Guesses are cut to the game's guess limit after scrubbing.
func TestArchiveTruncatesGuesses(t *testing.T) {
	start := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	long := gamekit.Player{Handle: "dashing-pelican"}
	g := &game{Word: "kite", Tier: medium, Round: 3, RoundStart: start, Drawer: "w/a/s", Outcome: "timeout"}
	g.Players = []*player{{Player: gamekit.Player{Workspace: "w", Agent: "a", Session: "s", Handle: "sleek-otter"}}, {Player: long}}
	g.Guesses = []guess{{By: "dashing-pelican", Text: strings.Repeat("sleek otter ", 8), At: start.Add(time.Second)}}
	r := archiveRound(g, start.Add(time.Minute))
	if got := r.Timeline[0].Text; len([]rune(got)) != maxGuess || strings.Contains(got, "otter") || r.Timeline[0].By != "guesser 1" {
		t.Fatalf("guess %q by %q", got, r.Timeline[0].By)
	}
	if r.GuessedMs != nil || r.LengthMs != 60000 {
		t.Fatalf("%+v", r)
	}
}
