package nightfall

// The public pages' layout, in a real browser (apps/layouttest): the table
// by night, by day with everyone talking at once (the longest handles,
// long messages), at the vote with four players, with three,
// and at the reveal; the list of games and the landing page; each at the
// usual sizes. Nothing on a page may cover anything else (no bubble over a
// seat, no seat over the middle), nor may the page scroll sideways.
// NIGHTFALL_LAYOUT_SHOTS=<dir> also saves screenshots.

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/wakeshift/apps/gamekit"
	"github.com/wakeshift/apps/gamekit/gametest"
	"github.com/wakeshift/apps/layouttest"
	"github.com/wakeshift/wakeshift-go/appkit"
)

// What mustn't overlap on the public pages, and on the landing page.
var (
	publicMeasure = layouttest.Measure([][]string{
		{".top", "main", ".foot"}, {".crumbs", ".top h1", ".top .sub"}, {".crumbs a"},
		{".card", ".stage"}, {".log .entry"},
		{".nf-table .middle", ".nf-table .plate", ".nf-table .bubble", ".nf-table .tokens"}, {".nf-table .pile .name"},
		{".nf-table .phase-row", ".nf-table .pile", ".nf-table .announce"}, {".nf-table .pile .tcard"},
		{".nf-table .in-play", ".nf-table .outcome"},
		{".games li"}, {".g-status", ".g-players", ".g-watch"},
		{".foot .wrap > span:first-child", ".foot .links a"},
	}, []string{".card", ".games .row", ".nf-table .arena", ".nf-table .plate", ".nf-table .tokens"})
	landingMeasure = layouttest.Measure([][]string{
		{".hero", "section.part", "body > footer"}, {".top .logo", ".top-links a"}, {".hero-copy .button", ".hero-copy .watch-link"},
		{".live-list li"}, {".l-status", ".l-players", ".l-watch"}, {".live-head h2", ".live-head a"},
		{"footer .logo", "footer .links a"},
	}, []string{".live-list a", ".live-empty"})
	rulesMeasure = layouttest.Measure([][]string{
		{".hero", "section.part", "body > footer"}, {".top .logo", ".top-links a"}, {".toc a"},
		{".side"}, {".deck th", ".deck td"}, {".role"}, {".rcard", ".role-text"}, {".points li"},
		{"footer .logo", "footer .links a"},
	}, []string{".role", ".side", ".deck", ".points"})
)

// What players say in the layout test: short, long, and unbroken.
var talk = []string{"I'm the lamplighter.", "Hmm, I'd say the changeling swapped with someone on the far side of the table, which means the vampire is probably sitting right next to the gravedigger tonight, and I won't hear otherwise.", "ok",
	"Supercalifragilisticexpialidocious-and-then-some-more-letters-without-spaces"}

func TestLayout(t *testing.T) {
	port := layouttest.Chrome(t)
	app := &App{Kit: &appkit.App{ID: "nightfall", MCPURL: "https://nf.example.test/mcp"}}
	app.Store, _ = gametest.Store(t)
	srv := httptest.NewServer(app.Handler())
	t.Cleanup(srv.Close)
	handles := []string{"exuberant-hippopotamus-9", "quiet-otter", "magnificent-salamander-2", "owl", "breezy-narwhal-22", "curious-penguin"}
	houses := []string{"", "Claude Sonnet 5 Thinking", "", "GPT-6 Luna Preview Long", "Haiku 4.5", ""}
	put := func(id, phase string, n int, house, chatter bool) {
		roles := deck(n)
		g := table(roles[:n], [centerSize]string(roles[n:]))
		g.ID, g.Phase, g.Deadline, g.Roles = id, phase, time.Now().Add(time.Hour), roles
		for i, p := range g.Players {
			p.Player = gamekit.Player{Workspace: fmt.Sprint(i), Agent: "a", Handle: handles[i], House: houses[i]}
			if house && p.House == "" {
				p.House = "Haiku 4.5"
			}
			if phase == voting || phase == over || phase == day && i%2 == 0 {
				p.Vote = handles[(i+1)%n]
			}
		}
		if phase == over {
			g.Players[2].Vote = handles[1]                                       // two votes for one player: someone is staked
			g.Center = [centerSize]string{townsperson, sleepwalker, lamplighter} // the longest names, face up
			g.tally()
		}
		said := []gamekit.Entry{{Text: "Nightfall game " + id + " has started. The cards in play: a long list of them, and three in the center."}}
		for i := range 24 {
			said = append(said, gamekit.Entry{From: handles[i%n], Text: talk[i%len(talk)]})
		}
		gametest.Put(t, app.Store, id, phase, false, phase == over, g, said...)
		if chatter { // everyone has just spoken, for as long as the test runs
			for i := range n {
				if _, err := app.Store.Pool.Exec(context.Background(), `INSERT INTO room_log (game_id, at, from_handle, text, posted_at)
					VALUES ($1, now() + interval '1 hour', $2, $3, now())`, id, handles[i], talk[(i+1)%len(talk)]); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	put("nfnight", night, 6, false, false)
	put("nflive", day, 6, false, true)
	put("nfhous", voting, 4, true, true)
	put("nfthree", day, 3, false, true)
	put("nfover", over, 6, false, false)
	ready := "!!document.querySelector('#nf-table[data-live]')"
	pages := []layouttest.Page{
		{Name: "night", URL: srv.URL + "/games/nfnight", Ready: ready, Measure: publicMeasure},
		{Name: "day", URL: srv.URL + "/games/nflive", Live: true, Ready: ready + " && !!document.querySelector('.bubble')", Measure: publicMeasure},
		{Name: "vote", URL: srv.URL + "/games/nfhous", Ready: ready, Measure: publicMeasure},
		{Name: "three", URL: srv.URL + "/games/nfthree", Ready: ready, Measure: publicMeasure},
		{Name: "over", URL: srv.URL + "/games/nfover", Live: true, Ready: ready, Measure: publicMeasure},
		{Name: "games", URL: srv.URL + "/games", Measure: publicMeasure},
		{Name: "landing", URL: srv.URL + "/", Measure: landingMeasure},
		{Name: "rules", URL: srv.URL + "/rules", Measure: rulesMeasure},
	}
	layouttest.Run(t, port, pages, []int{760, 900, 1100, 1180, 1366, 2200}, os.Getenv("NIGHTFALL_LAYOUT_SHOTS"))
}
