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

// Roles: a town with a coven of vampires hidden in it. Each role's
// mechanics are those of the classic one-night hidden-role game; the names
// and the story are Nightfall's own (old name in brackets):
//
//	role         team       night ability
//	vampire      the coven  sees the other vampires; a lone one may look at a center card (werewolf)
//	thrall       the coven  sees the vampires, who don't know it (minion)
//	lamplighter  the town   looks at one player's card, or two center cards (seer)
//	changeling   the town   swaps its card with a player's, and sees its new one (robber)
//	gravedigger  the town   swaps two other players' cards, unseen (troublemaker)
//	sleepwalker  the town   swaps its card with a center card, unseen (drunk)
//	watchman     the town   sees its own card at the end of the night (insomniac)
//	avenger      the town   voted out, takes the player it voted for with it (hunter)
//	martyr       its own    wins only if it's voted out (tanner)
//	townsperson  the town   none (villager)
//
// Games stored under the old names are read as these (legacy.go).
const (
	vampire     = "vampire"
	thrall      = "thrall"
	lamplighter = "lamplighter"
	changeling  = "changeling"
	gravedigger = "gravedigger"
	sleepwalker = "sleepwalker"
	watchman    = "watchman"
	martyr      = "martyr"
	avenger     = "avenger"
	townsperson = "townsperson"
)

// townWins is how the town wins, for the help of every town role.
const townWins = " You win if a vampire is voted out (or, if no vampire is among the players, if nobody is)."

// Team names, as the game says them.
const (
	coven = "the coven"
	town  = "the town"
	alone = "on its own"
)

// role is one of the game's roles: its team, whether it acts at night
// (roles acts in the order they're listed), what it does in a line (for the
// landing page and the rules) and its help (for the agent holding it).
// The game, the landing page and the rules page all read this table.
type role struct {
	ID, Team string
	Night    bool
	Ability  string
	Help     string
}

var roles = []role{
	{vampire, coven, true, "Sees the other vampires. A lone vampire may look at one center card.",
		"The coven. You're a vampire hiding in the town. At night you see the other vampires; a lone vampire may look at one center card (night tool, center=[n]). " +
			"You win if no vampire is voted out (and the martyr isn't either), so keep your role secret: claim a town role, stick to your story and steer the vote onto someone else."},
	{thrall, coven, true, "A human who serves the vampires: sees who they are and wins with them. They don't know who it is.",
		"The coven. You're a living human in the vampires' thrall: at night you see who the vampires are (they don't know you). You win with them: if no vampire is voted out, even if you are; " +
			"if no vampire is among the players, you win if someone other than you is voted out. Either way, not if the martyr is. " +
			"Keep your role secret and never name the vampires: claim a town role, or draw the vote onto yourself."},
	{lamplighter, town, true, "Carries a lamp into the dark: looks at another player's card, or at two center cards.",
		"The town. At night you carry your lamp into the dark: look at one other player's card (night tool, players=[handle]) or two center cards (center=[a,b])." + townWins},
	{changeling, town, true, "May swap cards with another player, and sees what it has become.",
		"The town, until you swap. At night you may swap your card with another player's and see your new card (night tool, players=[handle]): you become what you took, " +
			"and they become the changeling without knowing it. You're on the team of the card you end with, and win as it does: " +
			"if you took a vampire, you're in the coven now, so keep it secret and claim you took a town card."},
	{gravedigger, town, true, "May swap two other players' cards without looking, like two coffins mixed up.",
		"The town. At night you may swap two other players' cards without seeing them (night tool, players=[a,b]), like two coffins mixed up in the dark." + townWins},
	{sleepwalker, town, true, "Wanders off and swaps its card with a center card, without looking.",
		"The town, until you wander. At night you must swap your card with a center card, without seeing it (night tool, center=[n]); if you don't choose, one is picked for you. " +
			"You're on the team of the card you end with, though you don't know what it is, and win as it does."},
	{watchman, town, true, "Never sleeps: sees its own card at the end of the night.",
		"The town. You don't sleep: at the end of the night you see your own card, in case it was swapped." + townWins},
	{avenger, town, false, "If it's staked, the player it voted for is staked too.",
		"The town. If you're voted out, the player you voted for is staked with you." + townWins},
	{martyr, alone, false, "Longs to be staked: it wins only if it's voted out.",
		"On your own. You long to be staked: you win only if you're voted out. Look just suspicious enough, without making it obvious."},
	{townsperson, town, false, "No night ability. It works it out by day.",
		"The town. No night ability: use the day to work out who the vampires are." + townWins},
}

// help is what each role does and how it wins, for the agent holding it;
// nightOrder is the order night actions resolve in: each sees the cards as
// the ones before left them. Both from roles.
var help, nightOrder = func() (map[string]string, []string) {
	h, order := map[string]string{}, []string{}
	for _, r := range roles {
		h[r.ID] = r.Help
		if r.Night {
			order = append(order, r.ID)
		}
	}
	return h, order
}()

// The deck: these cards with three players, and from each player count
// on, one more (one of two, picked at random, where there are two). With
// three in the center. Curated, since some mixes don't work with few
// players (two vampires among three players outvote the town; a thrall or
// martyr unbalances a small game).
var (
	baseDeck = []string{vampire, lamplighter, changeling, gravedigger, townsperson, watchman} // 3 players: one vampire
	moreDeck = []struct {
		From  int
		Cards []string // one of these
	}{
		{4, []string{vampire}}, // two can't outvote three
		{5, []string{sleepwalker, martyr}},
		{6, []string{thrall, avenger}},
	}
)

// deck is a game's cards for this many players, plus three in the center.
func deck(players int) []string {
	cards := slices.Clone(baseDeck)
	for _, m := range moreDeck {
		if players >= m.From {
			cards = append(cards, m.Cards[gamekit.RandInt(len(m.Cards))])
		}
	}
	return cards
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
	case lamplighter, changeling, gravedigger, sleepwalker:
		return true
	case vampire:
		return len(g.dealt(vampire)) == 1
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
	case p.Dealt == lamplighter && len(a.Players) == 1 && len(a.Center) == 0:
		return others(a.Players)
	case p.Dealt == lamplighter && len(a.Center) == 2 && len(a.Players) == 0:
		return center(a.Center)
	case p.Dealt == lamplighter:
		return fmt.Errorf("as the lamplighter, look at one player's card (players=[handle]) or two center cards (center=[a,b])")
	case p.Dealt == changeling && len(a.Players) == 1 && len(a.Center) == 0:
		return others(a.Players)
	case p.Dealt == changeling:
		return fmt.Errorf("as the changeling, choose one player to swap with (players=[handle])")
	case p.Dealt == gravedigger && len(a.Players) == 2 && len(a.Center) == 0:
		return others(a.Players)
	case p.Dealt == gravedigger:
		return fmt.Errorf("as the gravedigger, choose two other players to swap (players=[a,b])")
	case (p.Dealt == sleepwalker || p.Dealt == vampire && g.needsAction(p)) && len(a.Center) == 1 && len(a.Players) == 0:
		return center(a.Center)
	case p.Dealt == sleepwalker:
		return fmt.Errorf("as the sleepwalker, choose a center card to swap with (center=[n])")
	case p.Dealt == vampire && g.needsAction(p):
		return fmt.Errorf("as the lone vampire, you may look at one center card (center=[n])")
	}
	return fmt.Errorf("as the %s you have nothing to do at night", p.Dealt)
}

// deal deals the cards and tells the vampires and the thrall what they
// see at once. Called under the game's lock.
func (g *game) deal() {
	g.Roles = deck(len(g.Players))
	cards := slices.Clone(g.Roles)
	gamekit.Shuffle(cards)
	for i, p := range g.Players {
		p.Dealt, p.Card = cards[i], cards[i]
	}
	copy(g.Center[:], cards[len(g.Players):])
	vampires := g.dealt(vampire)
	names := func(ps []*player) string {
		var hs []string
		for _, p := range ps {
			hs = append(hs, p.Handle)
		}
		return strings.Join(hs, " and ")
	}
	for _, w := range vampires {
		if len(vampires) == 1 {
			w.Learned = append(w.Learned, "You're the only vampire among the players. You may look at one center card.")
		} else {
			others := slices.DeleteFunc(slices.Clone(vampires), func(q *player) bool { return q == w })
			w.Learned = append(w.Learned, "The other vampire is "+names(others)+".")
		}
	}
	for _, m := range g.dealt(thrall) {
		if len(vampires) == 0 {
			m.Learned = append(m.Learned, "There are no vampires among the players: both are in the center.")
		} else {
			m.Learned = append(m.Learned, "The vampires are "+names(vampires)+".")
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
			case vampire:
				if a != nil && len(a.Center) == 1 {
					n := a.Center[0]
					p.Learned = append(p.Learned, fmt.Sprintf("Center card %d is the %s.", n, g.Center[n-1]))
				}
			case lamplighter:
				switch {
				case a != nil && len(a.Players) == 1:
					q := g.byHandle(strings.TrimPrefix(a.Players[0], "@"))
					p.Learned = append(p.Learned, fmt.Sprintf("%s's card is the %s.", q.Handle, q.Card))
				case a != nil && len(a.Center) == 2:
					p.Learned = append(p.Learned, fmt.Sprintf("Center card %d is the %s, and center card %d is the %s.",
						a.Center[0], g.Center[a.Center[0]-1], a.Center[1], g.Center[a.Center[1]-1]))
				}
			case changeling:
				if a != nil && len(a.Players) == 1 {
					q := g.byHandle(strings.TrimPrefix(a.Players[0], "@"))
					p.Card, q.Card = q.Card, p.Card
					took := fmt.Sprintf("You swapped with %s: your card is now the %s.", q.Handle, p.Card)
					if p.Card == vampire || p.Card == thrall {
						took += " You're in the coven now, and win if no vampire is voted out: keep it secret. Don't say what you took; claim a town card and steer the vote onto someone in the town."
					}
					p.Learned = append(p.Learned, took)
				}
			case gravedigger:
				if a != nil && len(a.Players) == 2 {
					x, y := g.byHandle(strings.TrimPrefix(a.Players[0], "@")), g.byHandle(strings.TrimPrefix(a.Players[1], "@"))
					x.Card, y.Card = y.Card, x.Card
					p.Learned = append(p.Learned, fmt.Sprintf("You swapped %s's and %s's cards.", x.Handle, y.Handle))
				}
			case sleepwalker:
				n := gamekit.RandInt(centerSize) + 1
				if a != nil && len(a.Center) == 1 {
					n = a.Center[0]
				}
				p.Card, g.Center[n-1] = g.Center[n-1], p.Card
				p.Learned = append(p.Learned, fmt.Sprintf("You swapped your card with center card %d, without looking.", n))
			case watchman:
				p.Learned = append(p.Learned, "When you woke, your card was the "+p.Card+".")
			}
		}
	}
}

// won is whether p won, by the cards held at the end and who was voted
// out (once the game is tallied).
func (g *game) won(p *player) bool {
	vampiresAmong, vampireDied, martyrDied, anyDied := false, false, false, false
	for _, q := range g.Players {
		vampiresAmong = vampiresAmong || q.Card == vampire
		if q.Dead {
			anyDied = true
			vampireDied = vampireDied || q.Card == vampire
			martyrDied = martyrDied || q.Card == martyr
		}
	}
	switch p.Card {
	case vampire:
		return !vampireDied && !martyrDied
	case thrall:
		if vampiresAmong {
			return !vampireDied && !martyrDied
		}
		return anyDied && !p.Dead && !martyrDied
	case martyr:
		return p.Dead
	}
	return vampiresAmong && vampireDied || !vampiresAmong && !anyDied
}

// team is the side a card plays for: "the coven", "the town", or the
// martyr on its own.
func team(card string) string {
	switch card {
	case vampire, thrall:
		return "the coven"
	case martyr:
		return "the martyr"
	}
	return "the town"
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
	// An avenger who dies takes the player they voted for.
	for _, p := range slices.Clone(g.Players) {
		if p.Dead && p.Card == avenger && p.Vote != "" {
			g.byHandle(p.Vote).Dead = true
		}
	}
	var dead, winners []string
	for _, p := range g.Players {
		if p.Dead {
			dead = append(dead, p.Handle)
		}
		if g.won(p) {
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
