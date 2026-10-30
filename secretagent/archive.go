package secretagent

import (
	"github.com/wakeshift/apps/gamekit"
)

// sayMax is the longest thing a player may say, in characters: the say
// tool's limit and an archived clue's.
const sayMax = 1000

// An archived game (gamekit's game_archives): the codeword, each player's
// role and vote, the clues in order and the outcome. Players are "player 1"
// to "player 3", in the order they joined, with whether each was a house
// agent and, for a house agent only, its model. Nothing else of a player is
// kept: no handle (one in a clue becomes the player's label), and nothing
// of its workspace, agent or session.
type archived struct {
	Game     string           `json:"game"` // "secret-agent"
	Codeword string           `json:"codeword"`
	Players  []archivedPlayer `json:"players"`
	Clues    []archivedClue   `json:"clues"`
	Winner   string           `json:"winner"` // "civilians" or "secret agent"
	// LengthMs is how long the game ran, from the discussion opening.
	LengthMs int64 `json:"length_ms"`
}

type archivedPlayer struct {
	Label string `json:"label"`           // "player 1"
	House bool   `json:"house,omitempty"` // a house agent
	Model string `json:"model,omitempty"` // a house agent's model; never anyone else's
	Role  string `json:"role"`            // "civilian" or "secret agent"
	Vote  string `json:"vote,omitempty"`  // the label it voted for
}

type archivedClue struct {
	OffsetMs int64  `json:"offset_ms"` // since the discussion opened
	From     string `json:"from"`      // the player's label
	Text     string `json:"text"`
}

// archive keeps the finished game, anonymized. Called under its lock, in
// the move that finishes it.
func archive(tx *gamekit.Tx, g *game) error {
	said, err := tx.Said(g.ID)
	if err != nil {
		return err
	}
	var anon gamekit.Anon
	for _, p := range g.Players {
		anon.Numbered(p.Player, "player")
	}
	start := g.Started
	if start.IsZero() { // a game that started before games were timed
		start = tx.Now
		if len(said) > 0 {
			start = said[0].At
		}
	}
	rec := archived{Game: "secret-agent", Codeword: g.Codeword, Players: []archivedPlayer{}, Clues: []archivedClue{},
		Winner: "secret agent", LengthMs: gamekit.Offset(start, tx.Now)}
	labels := anon.Players()
	for i, p := range g.Players {
		l := labels[i]
		rec.Players = append(rec.Players, archivedPlayer{Label: l.Role, House: l.House, Model: l.Model, Role: p.Role, Vote: anon.Role(p.Vote)})
		if p.Role == secretAgent && caught(g, p) {
			rec.Winner = "civilians"
		}
	}
	for _, e := range said {
		from := anon.Role(e.From)
		if from == "" {
			continue // not a seated player's: nothing to call it
		}
		rec.Clues = append(rec.Clues, archivedClue{OffsetMs: gamekit.Offset(start, e.At), From: from, Text: gamekit.Truncate(anon.Scrub(e.Text), sayMax)})
	}
	return tx.Archive(rec)
}
