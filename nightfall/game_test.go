package nightfall

import (
	"slices"
	"strings"
	"testing"

	"github.com/wakeshift/apps/gamekit"
)

// The privacy policy says games are kept in the database, as they are.
func TestPrivacyMatchesStorage(t *testing.T) {
	if p := string(privacyMD); strings.Contains(p, "memory") || !strings.Contains(p, "own database") {
		t.Fatalf("privacy policy: %s", p)
	}
}

// table is a game with cards dealt by hand: players named after their
// dealt roles' order here (p0, p1, …).
func table(roles []string, center [centerSize]string) *game {
	g := &game{Game: gamekit.Game{ID: "test"}, Center: center}
	for i, r := range roles {
		g.Players = append(g.Players, &player{Player: gamekit.Player{Handle: "p" + string(rune('0'+i))}, Dealt: r, Card: r})
	}
	return g
}

func (g *game) p(i int) *player { return g.Players[i] }

func learned(p *player) string { return strings.Join(p.Learned, " | ") }

// Night actions resolve in role order, each seeing the cards as the ones
// before left them.
func TestNightResolvesInOrder(t *testing.T) {
	// p0 seer, p1 robber, p2 troublemaker, p3 drunk, p4 insomniac, p5 werewolf (alone).
	g := table([]string{seer, robber, troublemaker, drunk, insomniac, werewolf}, [centerSize]string{werewolf, villager, tanner})
	g.p(0).Action = &action{Players: []string{"p1"}}       // the seer looks at the robber before the robbery
	g.p(1).Action = &action{Players: []string{"p5"}}       // the robber takes the werewolf
	g.p(2).Action = &action{Players: []string{"p1", "p4"}} // the troublemaker swaps the robber and the insomniac
	g.p(3).Action = &action{Center: []int{3}}              // the drunk takes the tanner
	g.p(5).Action = &action{Center: []int{1}}              // the lone werewolf looks at center 1
	g.resolveNight()

	if l := learned(g.p(0)); !strings.Contains(l, "p1's card is the robber") {
		t.Errorf("seer: %s", l)
	}
	if l := learned(g.p(1)); !strings.Contains(l, "your card is now the werewolf") {
		t.Errorf("robber: %s", l)
	}
	if l := learned(g.p(5)); !strings.Contains(l, "Center card 1 is the werewolf") {
		t.Errorf("werewolf: %s", l)
	}
	// After the robbery p1 held the werewolf and p5 the robber; the
	// troublemaker then swapped p1 and p4: p4 holds the werewolf.
	if g.p(4).Card != werewolf || g.p(1).Card != insomniac || g.p(5).Card != robber {
		t.Errorf("cards: p1 %s, p4 %s, p5 %s", g.p(1).Card, g.p(4).Card, g.p(5).Card)
	}
	if l := learned(g.p(4)); !strings.Contains(l, "your card was the werewolf") {
		t.Errorf("insomniac: %s", l)
	}
	if g.p(3).Card != tanner || g.Center[2] != drunk || strings.Contains(learned(g.p(3)), "tanner") {
		t.Errorf("drunk: card %s, center %v, learned %s", g.p(3).Card, g.Center, learned(g.p(3)))
	}
}

func TestDrunkMustSwap(t *testing.T) {
	g := table([]string{drunk, villager, villager}, [centerSize]string{werewolf, werewolf, seer})
	g.resolveNight()
	if g.p(0).Card == drunk || !slices.Contains(g.Center[:], drunk) {
		t.Fatalf("drunk kept its card: %s, center %v", g.p(0).Card, g.Center)
	}
}

func TestWhoWins(t *testing.T) {
	for name, c := range map[string]struct {
		cards   []string
		votes   []string // by index: whom each voted for
		dead    []string
		winners []string
	}{
		"a werewolf dies":        {[]string{werewolf, seer, villager}, []string{"p1", "p0", "p0"}, []string{"p0"}, []string{"p1", "p2"}},
		"a villager dies":        {[]string{werewolf, seer, villager}, []string{"p1", "p2", "p1"}, []string{"p1"}, []string{"p0"}},
		"no majority":            {[]string{werewolf, seer, villager}, []string{"p1", "p2", "p0"}, nil, []string{"p0"}},
		"the tanner dies":        {[]string{werewolf, tanner, villager}, []string{"p1", "p0", "p1"}, []string{"p1"}, []string{"p1"}},
		"the hunter takes one":   {[]string{werewolf, hunter, villager}, []string{"p1", "p0", "p1"}, []string{"p0", "p1"}, []string{"p1", "p2"}},
		"no wolves, nobody dies": {[]string{seer, villager, villager}, []string{"p1", "p2", "p0"}, nil, []string{"p0", "p1", "p2"}},
		"no wolves, one dies":    {[]string{minion, villager, villager}, []string{"p1", "p0", "p1"}, []string{"p1"}, []string{"p0"}},
	} {
		t.Run(name, func(t *testing.T) {
			g := table(c.cards, [centerSize]string{werewolf, robber, troublemaker})
			for i, v := range c.votes {
				g.p(i).Vote = v
			}
			g.tally()
			var dead []string
			for _, p := range g.Players {
				if p.Dead {
					dead = append(dead, p.Handle)
				}
			}
			if !slices.Equal(dead, c.dead) {
				t.Errorf("dead %v, want %v", dead, c.dead)
			}
			want := "Winners: " + strings.Join(c.winners, ", ") + "."
			if len(c.winners) == 0 {
				want = "Nobody won."
			}
			if !strings.HasPrefix(g.Result[0], want) {
				t.Errorf("result %q, want %q", g.Result[0], want)
			}
		})
	}
}

func TestDeck(t *testing.T) {
	for n := minPlayers; n <= maxPlayers; n++ {
		for range 20 {
			d := deck(n)
			wolves := 0
			for _, c := range d {
				if c == werewolf {
					wolves++
				}
			}
			small := n == minPlayers && (wolves != 1 || slices.Contains(d, minion) || slices.Contains(d, tanner))
			if len(d) != n+centerSize || !slices.Contains(d, seer) || wolves == 0 || small || n >= 4 && wolves != 2 {
				t.Fatalf("%d players: %v", n, d)
			}
		}
	}
}
