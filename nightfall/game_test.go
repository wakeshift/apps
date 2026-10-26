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
	// p0 lamplighter, p1 changeling, p2 gravedigger, p3 sleepwalker, p4 watchman, p5 vampire (alone).
	g := table([]string{lamplighter, changeling, gravedigger, sleepwalker, watchman, vampire}, [centerSize]string{vampire, townsperson, martyr})
	g.p(0).Action = &action{Players: []string{"p1"}}       // the lamplighter looks at the changeling before the swap
	g.p(1).Action = &action{Players: []string{"p5"}}       // the changeling takes the vampire
	g.p(2).Action = &action{Players: []string{"p1", "p4"}} // the gravedigger swaps the changeling and the watchman
	g.p(3).Action = &action{Center: []int{3}}              // the sleepwalker takes the martyr
	g.p(5).Action = &action{Center: []int{1}}              // the lone vampire looks at center 1
	g.resolveNight()

	if l := learned(g.p(0)); !strings.Contains(l, "p1's card is the changeling") {
		t.Errorf("lamplighter: %s", l)
	}
	if l := learned(g.p(1)); !strings.Contains(l, "your card is now the vampire") || !strings.Contains(l, "You're in the coven now") || !strings.Contains(l, "Don't say what you took") {
		t.Errorf("changeling: %s", l)
	}
	if l := learned(g.p(5)); !strings.Contains(l, "Center card 1 is the vampire") {
		t.Errorf("vampire: %s", l)
	}
	// After the swap p1 held the vampire and p5 the changeling; the
	// gravedigger then swapped p1 and p4: p4 holds the vampire.
	if g.p(4).Card != vampire || g.p(1).Card != watchman || g.p(5).Card != changeling {
		t.Errorf("cards: p1 %s, p4 %s, p5 %s", g.p(1).Card, g.p(4).Card, g.p(5).Card)
	}
	if l := learned(g.p(4)); !strings.Contains(l, "your card was the vampire") {
		t.Errorf("watchman: %s", l)
	}
	if g.p(3).Card != martyr || g.Center[2] != sleepwalker || strings.Contains(learned(g.p(3)), "martyr") {
		t.Errorf("sleepwalker: card %s, center %v, learned %s", g.p(3).Card, g.Center, learned(g.p(3)))
	}
}

func TestSleepwalkerMustSwap(t *testing.T) {
	g := table([]string{sleepwalker, townsperson, townsperson}, [centerSize]string{vampire, vampire, lamplighter})
	g.resolveNight()
	if g.p(0).Card == sleepwalker || !slices.Contains(g.Center[:], sleepwalker) {
		t.Fatalf("sleepwalker kept its card: %s, center %v", g.p(0).Card, g.Center)
	}
}

func TestWhoWins(t *testing.T) {
	for name, c := range map[string]struct {
		cards   []string
		votes   []string // by index: whom each voted for
		dead    []string
		winners []string
	}{
		"a vampire dies":           {[]string{vampire, lamplighter, townsperson}, []string{"p1", "p0", "p0"}, []string{"p0"}, []string{"p1", "p2"}},
		"a townsperson dies":       {[]string{vampire, lamplighter, townsperson}, []string{"p1", "p2", "p1"}, []string{"p1"}, []string{"p0"}},
		"no majority":              {[]string{vampire, lamplighter, townsperson}, []string{"p1", "p2", "p0"}, nil, []string{"p0"}},
		"the martyr dies":          {[]string{vampire, martyr, townsperson}, []string{"p1", "p0", "p1"}, []string{"p1"}, []string{"p1"}},
		"the avenger takes one":    {[]string{vampire, avenger, townsperson}, []string{"p1", "p0", "p1"}, []string{"p0", "p1"}, []string{"p1", "p2"}},
		"no vampires, nobody dies": {[]string{lamplighter, townsperson, townsperson}, []string{"p1", "p2", "p0"}, nil, []string{"p0", "p1", "p2"}},
		"no vampires, one dies":    {[]string{thrall, townsperson, townsperson}, []string{"p1", "p0", "p1"}, []string{"p1"}, []string{"p0"}},
	} {
		t.Run(name, func(t *testing.T) {
			g := table(c.cards, [centerSize]string{vampire, changeling, gravedigger})
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
			vampires := 0
			for _, c := range d {
				if c == vampire {
					vampires++
				}
			}
			small := n == minPlayers && (vampires != 1 || slices.Contains(d, thrall) || slices.Contains(d, martyr))
			if len(d) != n+centerSize || !slices.Contains(d, lamplighter) || vampires == 0 || small || n >= 4 && vampires != 2 {
				t.Fatalf("%d players: %v", n, d)
			}
		}
	}
}
