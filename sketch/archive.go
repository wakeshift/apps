package sketch

import (
	"encoding/json"
	"slices"
	"time"

	"github.com/wakeshift/apps/gamekit"
)

// The archive: every finished round, kept for good in sketch_rounds
// (migrations/004_rounds.sql), anonymised: the word, the drawing (its
// marks, as the canvas stores them), and what happened when: draw calls,
// hints and guesses, by role ("drawer", "guesser 1"), with whether each
// player was a house agent and, for one, its model. Nothing in a row names
// or links to a player. A row's room is the room's random archive ID
// (gamekit's Tx.ArchiveRoom), the same for its rows while it's open; the
// link from the game to it goes when the room closes. A round voided for
// writing isn't kept: its drawing may hold text.

// archived is a round as the archive keeps it, before it's written.
type archived struct {
	Room        string
	Round       int
	PlayedOn    time.Time
	Word, Tier  string
	Outcome     string
	FirstDrawMs *int
	GuessedMs   *int
	LengthMs    int
	Players     []gamekit.ArchivePlayer
	Drawing     []stroke
	Timeline    []moment
}

// moment is something that happened in an archived round, ms after its
// start: a draw call (Marks added), a hint (Hint: 1, the length; 2, the
// first letter), or a guess (By a role, its Text, whether it was Right, and
// whether the player's Owner typed it on the watch page).
type moment struct {
	Ms    int    `json:"ms"`
	Kind  string `json:"kind"` // draw, hint or guess
	Marks int    `json:"marks,omitempty"`
	Hint  int    `json:"hint,omitempty"`
	By    string `json:"by,omitempty"`
	Text  string `json:"text,omitempty"`
	Right bool   `json:"right,omitempty"`
	Owner bool   `json:"owner,omitempty"`
}

// archiveRound is g's round, just ended at end, as the archive keeps it.
func archiveRound(g *game, end time.Time) archived {
	ms := func(t time.Time) int { return max(0, int(t.Sub(g.RoundStart).Milliseconds())) }
	r := archived{Round: g.Round, PlayedOn: g.RoundStart.UTC().Truncate(24 * time.Hour), Word: g.Word, Tier: g.Tier,
		Outcome: g.Outcome, LengthMs: ms(end), Drawing: g.Strokes, Timeline: []moment{}}
	if r.Drawing == nil {
		r.Drawing = []stroke{}
	}
	// Roles: the drawer; the guessers in the order they first guessed; then
	// everyone else in the room for the round (who left during it too), in
	// their seats' order. Every one of them is scrubbed from guesses' text.
	var anon gamekit.Anon
	if d := g.drawer(); d != nil {
		anon.As(d.Player, "drawer")
	}
	byHandle := func(h string) gamekit.Player {
		for _, p := range g.Players {
			if p.Handle == h {
				return p.Player
			}
		}
		return gamekit.Player{Handle: h}
	}
	for _, x := range g.Guesses {
		anon.Numbered(byHandle(x.By), "guesser")
	}
	for _, p := range g.Players {
		anon.Numbered(p.Player, "guesser") // the drawer keeps its role
	}
	r.Players = anon.Players()
	for _, d := range g.Draws {
		r.Timeline = append(r.Timeline, moment{Ms: ms(d.At), Kind: "draw", Marks: d.Marks})
	}
	if len(g.Draws) > 0 {
		first := ms(g.Draws[0].At)
		r.FirstDrawMs = &first
	}
	for i, t := range g.HintTimes {
		r.Timeline = append(r.Timeline, moment{Ms: ms(t), Kind: "hint", Hint: i + 1})
	}
	for _, x := range g.Guesses {
		m := moment{Ms: ms(x.At), Kind: "guess", By: anon.Role(x.By), Text: gamekit.Truncate(anon.Scrub(x.Text), maxGuess), Right: x.Right, Owner: x.Owner}
		r.Timeline = append(r.Timeline, m)
		if x.Right && r.GuessedMs == nil {
			at := m.Ms
			r.GuessedMs = &at
		}
	}
	slices.SortStableFunc(r.Timeline, func(a, b moment) int { return a.Ms - b.Ms })
	return r
}

// archive keeps the round g just ended, unless it was voided for writing.
func (a *App) archive(tx *gamekit.Tx, g *game) error {
	if g.Outcome == "writing" || g.Word == "" || g.RoundStart.IsZero() {
		return nil
	}
	r := archiveRound(g, tx.Now)
	room, err := tx.ArchiveRoom(g.ID)
	if err != nil {
		return err
	}
	r.Room = room
	players, _ := json.Marshal(r.Players)
	drawing, _ := json.Marshal(r.Drawing)
	timeline, _ := json.Marshal(r.Timeline)
	_, err = tx.Exec(tx.Ctx, `INSERT INTO sketch_rounds (room, round, played_on, word, tier, outcome, first_draw_ms, guessed_ms, length_ms, players, drawing, timeline)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		r.Room, r.Round, r.PlayedOn, r.Word, r.Tier, r.Outcome, r.FirstDrawMs, r.GuessedMs, r.LengthMs, players, drawing, timeline)
	return err
}
