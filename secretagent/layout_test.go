package secretagent

// The public pages' layout, in a real browser (apps/layouttest): a game
// mid-discussion and one just over, with the longest handles and a long
// transcript, the list of games and the landing page, each at
// the usual sizes; nothing on a page may cover anything else, nor may the
// page scroll sideways. SECRETAGENT_LAYOUT_SHOTS=<dir> also saves
// screenshots.

import (
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
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
		{".card"}, {".players .player"}, {".player .name", ".player .note", ".player .avatar"},
		{".log .entry"}, {".entry .from", ".entry .text", ".entry .time", ".entry .avatar"},
		{".games li"}, {".g-status", ".g-players", ".g-watch"},
		{".foot .wrap > span:first-child", ".foot .links a"},
	}, []string{".card", ".games .row"})
	landingMeasure = layouttest.Measure([][]string{
		{"body > header", ".hero", "section.part", "body > footer"}, {".logo", "nav a"},
		{".live-list li"}, {".l-status", ".l-players", ".l-watch"}, {".live-head h2", ".live-head a"},
		{".hero-copy .button", ".hero-copy .text-link"}, {"footer .logo", "footer .links a"},
	}, []string{".live-list a", ".live-empty"})
)

func TestLayout(t *testing.T) {
	port := layouttest.Chrome(t)
	app := &App{Kit: &appkit.App{ID: "secretagent", MCPURL: "https://sa.example.test/mcp"}}
	app.Store, _ = gametest.Store(t)
	srv := httptest.NewServer(app.Handler())
	t.Cleanup(srv.Close)
	handles := []string{"exuberant-hippopotamus-9", "quiet-otter", "magnificent-salamander-2"}
	houses := []string{"", "Claude Sonnet 5 Thinking", "GPT-6 Luna Preview Long"}
	put := func(id, phase string, house bool) {
		g := &game{Codeword: "lighthouse"}
		g.ID, g.Phase, g.Deadline = id, phase, time.Now().Add(time.Hour)
		for i, h := range handles {
			p := &player{Player: gamekit.Player{Workspace: fmt.Sprint(i), Agent: "a", Handle: h, House: houses[i]}, Role: civilian}
			if house && p.House == "" {
				p.House = "Haiku 4.5"
			}
			if i == 2 {
				p.Role = secretAgent
			}
			if phase != discussion {
				p.Vote = handles[(i+2)%3]
			}
			g.Players = append(g.Players, p)
		}
		var said []gamekit.Entry
		if phase == over {
			g.Result = "The secret agent wins: magnificent-salamander-2 got away with it."
		}
		said = append(said, gamekit.Entry{Text: "Secret Agent game " + id + " has started. Players: " + strings.Join(handles, ", ") + ". One of you is the secret agent; the other two are civilians who share a codeword the secret agent doesn't know."})
		for i := range 24 {
			text := []string{"It guides ships.", "Hmm, I'd say it's something you'd find on a long and windy coast, mostly at night, when the fog rolls in over the rocks and the keepers climb the stairs.", "ok", "Supercalifragilisticexpialidocious-and-then-some-more-letters-without-spaces"}[i%4]
			said = append(said, gamekit.Entry{From: handles[i%3], Text: text})
		}
		gametest.Put(t, app.Store, id, phase, false, phase == over, g, said...)
	}
	put("salive", discussion, false)
	put("sahous", voting, true)
	put("saover", over, false)
	ready := "!!document.querySelector('.log .entry')"
	pages := []layouttest.Page{
		{Name: "game", URL: srv.URL + "/games/salive", Live: true, Ready: ready, Measure: publicMeasure},
		{Name: "over", URL: srv.URL + "/games/saover", Live: true, Ready: ready, Measure: publicMeasure},
		{Name: "games", URL: srv.URL + "/games", Measure: publicMeasure},
		{Name: "landing", URL: srv.URL + "/", Measure: landingMeasure},
	}
	layouttest.Run(t, port, pages, []int{760, 900, 1100, 1366, 2200}, os.Getenv("SECRETAGENT_LAYOUT_SHOTS"))
}
