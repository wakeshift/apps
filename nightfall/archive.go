package nightfall

import (
	"slices"
	"strings"

	"github.com/wakeshift/apps/gamekit"
)

// sayMax is the longest thing a player may say, in characters: the say
// tool's limit and an archived line's.
const sayMax = 1000

// An archived game (gamekit's game_archives): the cards dealt and the
// center, each player's night choice and what it saw, the talk with when
// it was said, the votes, who was staked and who won. Players are "player
// 1", "player 2"… in the order they joined, with whether each was a house
// agent and, for a house agent only, its model. Nothing else of a player is
// kept: no handle (one in what's said or seen becomes the player's label),
// and nothing of its workspace, agent or session.
type archived struct {
	Game    string           `json:"game"`   // "nightfall"
	Cards   []string         `json:"cards"`  // the cards in play
	Center  []string         `json:"center"` // the center at the end, after the night's swaps
	Players []archivedPlayer `json:"players"`
	Talk    []archivedLine   `json:"talk"`
	// When day broke, and how long the game ran, from nightfall.
	DayOffsetMs int64 `json:"day_offset_ms"`
	LengthMs    int64 `json:"length_ms"`
	// Winners are the teams that won ("the town", "the coven", "the
	// martyr"); none if nobody did.
	Winners []string `json:"winners"`
}

type archivedPlayer struct {
	Label  string          `json:"label"`           // "player 1"
	House  bool            `json:"house,omitempty"` // a house agent
	Model  string          `json:"model,omitempty"` // a house agent's model; never anyone else's
	Dealt  string          `json:"dealt"`           // the role it was dealt
	Card   string          `json:"card"`            // the card it held at the end
	Night  *archivedAction `json:"night,omitempty"` // its night choice
	Saw    []string        `json:"saw,omitempty"`   // what it learned at night
	Vote   string          `json:"vote,omitempty"`  // the label it voted for
	Staked bool            `json:"staked"`          // voted out (or taken by the avenger)
	Won    bool            `json:"won"`
}

type archivedAction struct {
	Players []string `json:"players,omitempty"` // labels
	Center  []int    `json:"center,omitempty"`  // center cards, from 1
}

type archivedLine struct {
	OffsetMs int64  `json:"offset_ms"` // since nightfall
	From     string `json:"from"`      // the player's label
	Text     string `json:"text"`
}

// archive keeps the finished game, anonymized, once it's tallied. Called
// under its lock, in the move that finishes it.
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
	rec := archived{Game: "nightfall", Cards: slices.Clone(g.Roles), Players: []archivedPlayer{}, Talk: []archivedLine{}, Winners: []string{},
		LengthMs: gamekit.Offset(start, tx.Now)}
	if !g.DayAt.IsZero() {
		rec.DayOffsetMs = gamekit.Offset(start, g.DayAt)
	}
	rec.Center = slices.Clone(g.Center[:])
	labels := anon.Players()
	for i, p := range g.Players {
		l := labels[i]
		ap := archivedPlayer{Label: l.Role, House: l.House, Model: l.Model, Dealt: p.Dealt, Card: p.Card, Vote: anon.Role(p.Vote), Staked: p.Dead, Won: g.won(p)}
		if p.Action != nil {
			ap.Night = &archivedAction{Center: slices.Clone(p.Action.Center)}
			for _, h := range p.Action.Players {
				if lb := anon.Role(strings.TrimPrefix(h, "@")); lb != "" {
					ap.Night.Players = append(ap.Night.Players, lb)
				}
			}
		}
		for _, s := range p.Learned {
			ap.Saw = append(ap.Saw, anon.Scrub(s))
		}
		if ap.Won && !slices.Contains(rec.Winners, team(p.Card)) {
			rec.Winners = append(rec.Winners, team(p.Card))
		}
		rec.Players = append(rec.Players, ap)
	}
	for _, e := range said {
		from := anon.Role(e.From)
		if from == "" {
			continue // not a seated player's: nothing to call it
		}
		rec.Talk = append(rec.Talk, archivedLine{OffsetMs: gamekit.Offset(start, e.At), From: from, Text: gamekit.Truncate(anon.Scrub(e.Text), sayMax)})
	}
	return tx.Archive(rec)
}
