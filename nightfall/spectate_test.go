package nightfall

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/apps/gamekit"
	"github.com/wakeshift/apps/gamekit/gametest"
	"github.com/wakeshift/wakeshift-go/appkit"
)

// spectated sets up a game in each phase behind a server: dealt, with the
// night's choices made (from the night on) and resolved (from the day on),
// votes cast (from the vote on) and tallied (once it's over).
func spectated(t *testing.T) (*httptest.Server, map[string]*game) {
	t.Helper()
	app := &App{Kit: &appkit.App{ID: "nightfall", MCPURL: "https://nf.example.test/mcp"}}
	app.Store, _ = gametest.Store(t)
	srv := httptest.NewServer(app.Handler())
	t.Cleanup(srv.Close)
	games := map[string]*game{}
	for i, phase := range []string{waiting, night, day, voting, over} {
		g := table([]string{vampire, lamplighter, changeling, gravedigger}, [centerSize]string{townsperson, watchman, vampire})
		g.ID, g.Phase, g.Deadline, g.Open = fmt.Sprintf("nf%d", i), phase, time.Now().Add(time.Minute), phase == waiting
		for j, p := range g.Players {
			p.Workspace, p.Agent, p.Session = fmt.Sprintf("ws-hidden-%d", j), "agent-hidden", "sess-hidden"
			p.Handle = []string{"wolfish-fox", "seeing-owl", "sly-magpie", "busy-bee"}[j]
		}
		g.Players[3].House = "Haiku 4.5"
		if phase == waiting {
			g.Roles, g.Center = nil, [centerSize]string{}
			for _, p := range g.Players {
				p.Dealt, p.Card = "", ""
			}
		} else {
			g.Roles = []string{vampire, vampire, lamplighter, changeling, gravedigger, townsperson, watchman}
			g.Players[0].Learned = []string{"You're the only vampire among the players. You may look at one center card."}
			g.Players[0].Action = &action{Center: []int{3}}
			g.Players[1].Action = &action{Players: []string{"wolfish-fox"}}
			g.Players[2].Action = &action{Players: []string{"busy-bee"}}
			g.Players[3].Action = &action{Players: []string{"wolfish-fox", "seeing-owl"}}
		}
		if phase == day || phase == voting || phase == over {
			g.resolveNight()
		}
		if phase == voting || phase == over {
			g.Players[1].Vote, g.Players[2].Vote = "busy-bee", "busy-bee"
		}
		if phase == over {
			g.tally()
		}
		said := []gamekit.Entry{{Text: "Nightfall game " + g.ID + " has started."}, {From: "seeing-owl", Text: "I looked at someone's card, " + phase + "."}}
		gametest.Put(t, app.Store, g.ID, phase, g.Open, phase == over, g, said...)
		for _, p := range g.Players {
			gametest.Think(t, app.Store, g.ID, p.Key(), "PRIVATE-REASONING of "+p.Handle)
		}
		games[phase] = g
	}
	closed := &game{Result: []string{"Not enough players joined."}}
	closed.ID, closed.Phase = "nfclosed", over
	gametest.Put(t, app.Store, closed.ID, over, false, true, closed)
	return srv, games
}

// Anyone can watch a game, but who holds which card, what each player did
// and saw at night, the center and whom each voted for wait until it's
// over, on every page and response a spectator can read.
func TestSpectatorsSeeNoSecrets(t *testing.T) {
	srv, games := spectated(t)
	for _, phase := range []string{waiting, night, day, voting, over} {
		g := games[phase]
		pages := gametest.Spectated(t, srv.URL, g.ID)
		var secrets []string // what the players learned at night, as text and as HTML
		for _, gg := range games {
			for _, p := range gg.Players {
				for _, l := range p.Learned {
					secrets = append(secrets, l, html.EscapeString(l))
				}
			}
		}
		for name, body := range pages {
			// A house agent (busy-bee) looks like any other player.
			for _, label := range []string{"House", `"house`, "l-house", `class="house"`, `class="via">`} {
				if strings.Contains(body, label) {
					t.Errorf("%s game: %s shows a house label (%q)", phase, name, label)
				}
			}
			for _, secret := range []string{"ws-hidden", "agent-hidden", "sess-hidden", `"dealt"`, `"card"`, `"learned"`, `"action"`, `"center"`, `"vote"`, `"workspace"`, "Dealt", "Saw:", "PRIVATE-REASONING", `"thinking"`} {
				if strings.Contains(body, secret) {
					t.Errorf("%s game: %s shows %q", phase, name, secret)
				}
			}
			over := phase == over && (name == "page" || name == "state")
			for _, s := range secrets {
				if strings.Contains(body, s) {
					t.Errorf("%s game: %s shows what a player learned: %q", phase, name, s)
				}
			}
			// The follow-along page's own seat is never on the public one.
			for _, secret := range []string{`"your_role"`, `"you":true`, "Your agent ·"} {
				if strings.Contains(body, secret) {
					t.Errorf("%s game: %s shows %q", phase, name, secret)
				}
			}
			// What waits for the end: the players' cards, the center and the
			// votes, in the result, in the players' notes and on the table.
			reveal := []string{"→ busy-bee", "Center: ", "Cards (dealt"}
			switch name {
			case "state":
				reveal = append(reveal, "(vampire)", "(lamplighter)", "(changeling)", "(gravedigger)",
					`"started_as"`, `"ended_as"`, `"team"`, `"voted_for"`, `"staked"`, `"won"`, `"center_cards"`, `"outcome"`)
			case "page":
				reveal = append(reveal, `class="role">Vampire`, `class="role">Lamplighter`, "voted busy-bee", `class="tcard up`, `<b>V</b>`, `class="outcome"`)
			}
			for _, secret := range reveal {
				if strings.Contains(body, secret) != over {
					t.Errorf("%s game: %s shows %q: %v", phase, name, secret, over)
				}
			}
		}
		for _, want := range []string{"Game " + g.ID, "wolfish-fox", "busy-bee", "someone&#39;s card, " + phase, `<noscript><meta http-equiv="refresh"`} {
			if !strings.Contains(pages["page"], want) {
				t.Errorf("%s game: the page has no %q", phase, want)
			}
		}
		// The cards in play were announced to the room: they're public.
		if (phase != waiting) != strings.Contains(pages["page"], "vampire, vampire, lamplighter, changeling, gravedigger, townsperson, watchman (3 in the center)") {
			t.Errorf("%s game: the cards in play", phase)
		}
	}

	var l gamekit.Listing
	if err := json.Unmarshal([]byte(gametest.Spectated(t, srv.URL, "nf1")["api"]), &l); err != nil {
		t.Fatal(err)
	}
	var live, finished []string
	for _, g := range l.Live {
		live = append(live, g.ID)
	}
	for _, g := range l.Finished {
		finished = append(finished, g.ID)
	}
	if strings.Join(live, " ") != "nf3 nf2 nf1" || strings.Join(finished, " ") != "nf4" {
		t.Errorf("live %v, finished %v", live, finished)
	}
	landing := gametest.Spectated(t, srv.URL, "nf1")["landing"]
	for _, want := range []string{`href="/games/nf1"`, "night · ", "busy-bee</span>", "Watch live"} {
		if !strings.Contains(landing, want) {
			t.Errorf("the landing page has no %q", want)
		}
	}
	if resp, err := http.Get(srv.URL + "/games/nfclosed"); err != nil || resp.StatusCode != 404 {
		t.Errorf("a game that never started: %v %v", resp, err)
	}
}

// Every move that can carry an operator note has it last, described alike.
func TestNoteArguments(t *testing.T) {
	gametest.Note(t, sayIn{})
	gametest.Note(t, voteIn{})
	gametest.Note(t, nightIn{})
}
