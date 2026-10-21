package nightfall

import (
	"fmt"
	"slices"
	"strings"

	"github.com/wakeshift/apps/gamekit"
)

const (
	minPlayers = 3
	maxPlayers = 6
	centerSize = 3

	waiting = "waiting for players"
	night   = "night"
	day     = "day"
	voting  = "voting"
	over    = "over"
)

// Roles.
const (
	werewolf     = "werewolf"
	minion       = "minion"
	seer         = "seer"
	robber       = "robber"
	troublemaker = "troublemaker"
	drunk        = "drunk"
	insomniac    = "insomniac"
	tanner       = "tanner"
	hunter       = "hunter"
	villager     = "villager"
)

// help is what each role does, for the agent holding it.
var help = map[string]string{
	werewolf:     "Team werewolf. At night you see the other werewolves; a lone werewolf may look at one center card (night tool, center=[n]). You win if no werewolf dies.",
	minion:       "Team werewolf. At night you see who the werewolves are (they don't know you). You win with them; if you die and no werewolf does, they still win.",
	seer:         "Village. At night look at one other player's card (night tool, players=[handle]) or two center cards (center=[a,b]).",
	robber:       "Village (until you rob). At night you may swap your card with another player's and see your new card (night tool, players=[handle]). You're on the team of the card you end with.",
	troublemaker: "Village. At night you may swap two other players' cards without seeing them (night tool, players=[a,b]).",
	drunk:        "Village. At night you must swap your card with a center card, without seeing it (night tool, center=[n]); if you don't choose, one is picked for you.",
	insomniac:    "Village. At the end of the night you see your own card, in case it was swapped.",
	tanner:       "On your own team. You hate your job: you win only if you're voted out.",
	hunter:       "Village. If you're voted out, the player you voted for dies too.",
	villager:     "Village. No night ability: use the day to work out who the werewolves are.",
}

// nightOrder is the order night actions resolve in: each sees the cards as
// the ones before left them.
var nightOrder = []string{werewolf, minion, seer, robber, troublemaker, drunk, insomniac}

// deck is the game's cards: one more set per player count, plus three in
// the center. Curated, since some mixes don't work with few players (two
// werewolves among three players outvote the village; a minion or tanner
// unbalances a small game). Some counts pick one of two cards.
func deck(players int) []string {
	cards := []string{werewolf, seer, robber, troublemaker, villager, insomniac} // 3 players: one werewolf
	if players >= 4 {
		cards = append(cards, werewolf) // two can't outvote three
	}
	if players >= 5 {
		cards = append(cards, pick(drunk, tanner))
	}
	if players >= 6 {
		cards = append(cards, pick(minion, hunter))
	}
	return cards
}

func pick(a, b string) string {
	if gamekit.RandInt(2) == 0 {
		return a
	}
	return b
}

type action struct {
	Players []string `json:"players,omitempty"`
	Center  []int    `json:"center,omitempty"`
}

// player is a seat, and what only the app knows about it.
type player struct {
	gamekit.Player
	Dealt   string   `json:"dealt,omitempty"`   // the role it started with: its night ability
	Card    string   `json:"card,omitempty"`    // the card it holds now (its team, at the end)
	Action  *action  `json:"action,omitempty"`  // its night choice
	Learned []string `json:"learned,omitempty"` // what it saw at night
	Vote    string   `json:"vote,omitempty"`
	Dead    bool     `json:"dead,omitempty"`
}

// game is one game: the kit's part (its ID, phase and deadline) and
// Nightfall's, kept as JSON in the database.
type game struct {
	gamekit.Game
	Players []*player          `json:"players"`
	Roles   []string           `json:"roles,omitempty"` // the cards in this game (public)
	Center  [centerSize]string `json:"center"`
	Result  []string           `json:"result,omitempty"`
}

func (g *game) handles() []string {
	var out []string
	for _, p := range g.Players {
		out = append(out, p.Handle)
	}
	return out
}

func (g *game) player(key string) *player {
	for _, p := range g.Players {
		if p.Key() == key {
			return p
		}
	}
	return nil
}

func (g *game) byHandle(h string) *player {
	for _, p := range g.Players {
		if p.Handle == h {
			return p
		}
	}
	return nil
}

func (g *game) dealt(role string) []*player {
	var out []*player
	for _, p := range g.Players {
		if p.Dealt == role {
			out = append(out, p)
		}
	}
	return out
}

// needsAction reports whether a player's night ability wants a choice.
func (g *game) needsAction(p *player) bool {
	switch p.Dealt {
	case seer, robber, troublemaker, drunk:
		return true
	case werewolf:
		return len(g.dealt(werewolf)) == 1
	}
	return false
}

// checkAction validates a night choice for p's role.
func (g *game) checkAction(p *player, a action) error {
	others := func(hs []string) error {
		for _, h := range hs {
			q := g.byHandle(strings.TrimPrefix(h, "@"))
			if q == nil {
				return fmt.Errorf("no player %s; the players are %s", h, strings.Join(g.handles(), ", "))
			}
			if q == p {
				return fmt.Errorf("choose other players, not yourself")
			}
		}
		if len(hs) == 2 && strings.TrimPrefix(hs[0], "@") == strings.TrimPrefix(hs[1], "@") {
			return fmt.Errorf("choose two different players")
		}
		return nil
	}
	center := func(cs []int) error {
		for _, c := range cs {
			if c < 1 || c > centerSize {
				return fmt.Errorf("center cards are 1 to %d", centerSize)
			}
		}
		if len(cs) == 2 && cs[0] == cs[1] {
			return fmt.Errorf("choose two different center cards")
		}
		return nil
	}
	switch {
	case p.Dealt == seer && len(a.Players) == 1 && len(a.Center) == 0:
		return others(a.Players)
	case p.Dealt == seer && len(a.Center) == 2 && len(a.Players) == 0:
		return center(a.Center)
	case p.Dealt == seer:
		return fmt.Errorf("as the seer, look at one player's card (players=[handle]) or two center cards (center=[a,b])")
	case p.Dealt == robber && len(a.Players) == 1 && len(a.Center) == 0:
		return others(a.Players)
	case p.Dealt == robber:
		return fmt.Errorf("as the robber, choose one player to rob (players=[handle])")
	case p.Dealt == troublemaker && len(a.Players) == 2 && len(a.Center) == 0:
		return others(a.Players)
	case p.Dealt == troublemaker:
		return fmt.Errorf("as the troublemaker, choose two other players to swap (players=[a,b])")
	case (p.Dealt == drunk || p.Dealt == werewolf && g.needsAction(p)) && len(a.Center) == 1 && len(a.Players) == 0:
		return center(a.Center)
	case p.Dealt == drunk:
		return fmt.Errorf("as the drunk, choose a center card to swap with (center=[n])")
	case p.Dealt == werewolf && g.needsAction(p):
		return fmt.Errorf("as the lone werewolf, you may look at one center card (center=[n])")
	}
	return fmt.Errorf("as the %s you have nothing to do at night", p.Dealt)
}

// deal deals the cards and tells the werewolves and the minion what they
// see at once. Called under the game's lock.
func (g *game) deal() {
	g.Roles = deck(len(g.Players))
	cards := slices.Clone(g.Roles)
	gamekit.Shuffle(cards)
	for i, p := range g.Players {
		p.Dealt, p.Card = cards[i], cards[i]
	}
	copy(g.Center[:], cards[len(g.Players):])
	wolves := g.dealt(werewolf)
	names := func(ps []*player) string {
		var hs []string
		for _, p := range ps {
			hs = append(hs, p.Handle)
		}
		return strings.Join(hs, " and ")
	}
	for _, w := range wolves {
		if len(wolves) == 1 {
			w.Learned = append(w.Learned, "You're the only werewolf among the players. You may look at one center card.")
		} else {
			others := slices.DeleteFunc(slices.Clone(wolves), func(q *player) bool { return q == w })
			w.Learned = append(w.Learned, "The other werewolf is "+names(others)+".")
		}
	}
	for _, m := range g.dealt(minion) {
		if len(wolves) == 0 {
			m.Learned = append(m.Learned, "There are no werewolves among the players: both are in the center.")
		} else {
			m.Learned = append(m.Learned, "The werewolves are "+names(wolves)+".")
		}
	}
}

// resolveNight carries out the night's choices in order. Called under the
// game's lock.
func (g *game) resolveNight() {
	for _, role := range nightOrder {
		for _, p := range g.dealt(role) {
			a := p.Action
			switch role {
			case werewolf:
				if a != nil && len(a.Center) == 1 {
					n := a.Center[0]
					p.Learned = append(p.Learned, fmt.Sprintf("Center card %d is the %s.", n, g.Center[n-1]))
				}
			case seer:
				switch {
				case a != nil && len(a.Players) == 1:
					q := g.byHandle(strings.TrimPrefix(a.Players[0], "@"))
					p.Learned = append(p.Learned, fmt.Sprintf("%s's card is the %s.", q.Handle, q.Card))
				case a != nil && len(a.Center) == 2:
					p.Learned = append(p.Learned, fmt.Sprintf("Center card %d is the %s, and center card %d is the %s.",
						a.Center[0], g.Center[a.Center[0]-1], a.Center[1], g.Center[a.Center[1]-1]))
				}
			case robber:
				if a != nil && len(a.Players) == 1 {
					q := g.byHandle(strings.TrimPrefix(a.Players[0], "@"))
					p.Card, q.Card = q.Card, p.Card
					p.Learned = append(p.Learned, fmt.Sprintf("You robbed %s: your card is now the %s.", q.Handle, p.Card))
				}
			case troublemaker:
				if a != nil && len(a.Players) == 2 {
					x, y := g.byHandle(strings.TrimPrefix(a.Players[0], "@")), g.byHandle(strings.TrimPrefix(a.Players[1], "@"))
					x.Card, y.Card = y.Card, x.Card
					p.Learned = append(p.Learned, fmt.Sprintf("You swapped %s's and %s's cards.", x.Handle, y.Handle))
				}
			case drunk:
				n := gamekit.RandInt(centerSize) + 1
				if a != nil && len(a.Center) == 1 {
					n = a.Center[0]
				}
				p.Card, g.Center[n-1] = g.Center[n-1], p.Card
				p.Learned = append(p.Learned, fmt.Sprintf("You swapped your card with center card %d, without looking.", n))
			case insomniac:
				p.Learned = append(p.Learned, "When you woke, your card was the "+p.Card+".")
			}
		}
	}
}

// tally works out who died and who won, by the cards held at the end.
// Called under the game's lock.
func (g *game) tally() {
	count := map[string]int{}
	for _, p := range g.Players {
		if p.Vote != "" {
			count[p.Vote]++
		}
	}
	top := 0
	for _, n := range count {
		top = max(top, n)
	}
	if top >= 2 {
		for _, p := range g.Players {
			p.Dead = count[p.Handle] == top
		}
	}
	// A hunter who dies takes the player they voted for.
	for _, p := range slices.Clone(g.Players) {
		if p.Dead && p.Card == hunter && p.Vote != "" {
			g.byHandle(p.Vote).Dead = true
		}
	}
	var dead []string
	wolvesAmong, wolfDied, tannerDied, anyDied := false, false, false, false
	for _, p := range g.Players {
		wolvesAmong = wolvesAmong || p.Card == werewolf
		if p.Dead {
			dead = append(dead, p.Handle)
			anyDied = true
			wolfDied = wolfDied || p.Card == werewolf
			tannerDied = tannerDied || p.Card == tanner
		}
	}
	var winners []string
	for _, p := range g.Players {
		won := false
		switch p.Card {
		case werewolf:
			won = !wolfDied && !tannerDied
		case minion:
			if wolvesAmong {
				won = !wolfDied && !tannerDied
			} else {
				won = anyDied && !p.Dead && !tannerDied
			}
		case tanner:
			won = p.Dead
		default:
			won = wolvesAmong && wolfDied || !wolvesAmong && !anyDied
		}
		if won {
			winners = append(winners, p.Handle)
		}
	}
	died := "Nobody died."
	if len(dead) > 0 {
		died = "Voted out: " + strings.Join(dead, ", ") + "."
	}
	won := "Nobody won."
	if len(winners) > 0 {
		won = "Winners: " + strings.Join(winners, ", ") + "."
	}
	var cards []string
	for _, p := range g.Players {
		c := p.Handle + ": " + p.Dealt
		if p.Card != p.Dealt {
			c += " → " + p.Card
		}
		cards = append(cards, c)
	}
	var votes []string
	for _, p := range g.Players {
		if p.Vote != "" {
			votes = append(votes, p.Handle+" → "+p.Vote)
		} else {
			votes = append(votes, p.Handle+" didn't vote")
		}
	}
	g.Result = []string{won + " " + died, "Cards (dealt → at the end): " + strings.Join(cards, "; ") + ".",
		fmt.Sprintf("Center: %s.", strings.Join(g.Center[:], ", ")), "Votes: " + strings.Join(votes, "; ") + "."}
}
