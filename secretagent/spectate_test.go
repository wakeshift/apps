package secretagent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/apps/gamekit"
	"github.com/wakeshift/apps/gamekit/gametest"
	"github.com/wakeshift/wakeshift-go/appkit"
)

// spectated sets up a game in each phase, with its codeword, roles and
// votes, behind a server.
func spectated(t *testing.T) (*App, *httptest.Server, map[string]*game) {
	t.Helper()
	app := &App{Kit: &appkit.App{ID: "secretagent", MCPURL: "https://sa.example.test/mcp"}}
	app.Store, _ = gametest.Store(t)
	srv := httptest.NewServer(app.Handler())
	t.Cleanup(srv.Close)
	words := map[string]string{waiting: "", discussion: "zeppelin", voting: "kumquat", over: "marmalade"}
	games := map[string]*game{}
	for i, phase := range []string{waiting, discussion, voting, over} {
		g := &game{Codeword: words[phase]}
		g.ID, g.Phase, g.Deadline, g.Open = fmt.Sprintf("sa%d", i), phase, time.Now().Add(time.Minute), phase == waiting
		for j, h := range []string{"dusty-heron", "quiet-otter", "mole-" + phase} {
			p := &player{Player: gamekit.Player{Workspace: fmt.Sprintf("ws-hidden-%d-%d", i, j), Agent: "agent-hidden", Session: "sess-hidden", Handle: h}}
			if j == 1 {
				p.House = "Haiku 4.5"
			}
			if phase != waiting {
				p.Role = civilian
				if j == 2 {
					p.Role = secretAgent
				}
			}
			g.Players = append(g.Players, p)
		}
		if phase == voting || phase == over {
			g.Players[0].Vote, g.Players[1].Vote = "mole-"+phase, "mole-"+phase
		}
		if phase == over {
			g.Result = "The civilians win: they caught mole-over."
		}
		said := []gamekit.Entry{{Text: "Secret Agent game " + g.ID + " has started."}, {From: "dusty-heron", Text: "It's something you'd find in a kitchen, " + phase + "."}}
		gametest.Put(t, app.Store, g.ID, phase, g.Open, phase == over, g, said...)
		for _, p := range g.Players {
			gametest.Think(t, app.Store, g.ID, p.Key(), "PRIVATE-REASONING of "+p.Handle)
		}
		games[phase] = g
	}
	// A game of house agents only, and one that closed before it started.
	h := &game{Codeword: "lantern"}
	h.ID, h.Phase, h.Deadline = "sahouse", discussion, time.Now().Add(time.Minute)
	for _, n := range []string{"house-a", "house-b", "house-c"} {
		h.Players = append(h.Players, &player{Player: gamekit.Player{Workspace: "w", Agent: n, Handle: n, House: "Sonnet 5"}, Role: civilian})
	}
	gametest.Put(t, app.Store, h.ID, h.Phase, false, false, h)
	closed := &game{Result: "Nobody else joined."}
	closed.ID, closed.Phase = "saclosed", over
	closed.Players = []*player{{Player: gamekit.Player{Workspace: "w", Agent: "x", Handle: "lonely-lynx"}}}
	gametest.Put(t, app.Store, closed.ID, over, false, true, closed)
	return app, srv, games
}

// Anyone can watch a game, but a game's secrets wait until it's over:
// every page and response a spectator can read, in every phase, has no
// codeword, role or vote's target until then, and nothing that identifies
// a player beyond its handle.
func TestSpectatorsSeeNoSecrets(t *testing.T) {
	_, srv, games := spectated(t)
	for _, phase := range []string{waiting, discussion, voting, over} {
		g := games[phase]
		pages := gametest.Spectated(t, srv.URL, g.ID)
		for name, body := range pages {
			// House agents (quiet-otter, and the house game's players) look
			// like any other player.
			for _, label := range []string{"House", `"house"`, `"house_only"`, "l-house", `class="house"`} {
				if strings.Contains(body, label) {
					t.Errorf("%s game: %s shows a house label (%q)", phase, name, label)
				}
			}
			for _, secret := range []string{"zeppelin", "kumquat", "ws-hidden", "agent-hidden", "sess-hidden", `"role"`, `"codeword"`, `"vote"`, `"workspace"`, "PRIVATE-REASONING", `"thinking"`} {
				if strings.Contains(body, secret) {
					t.Errorf("%s game: %s shows %q", phase, name, secret)
				}
			}
			over := phase == over && (name == "page" || name == "state")
			for _, secret := range []string{"marmalade", "(secret agent)", "(civilian)", "→ mole-", "The civilians win"} {
				if strings.Contains(body, secret) != over {
					t.Errorf("%s game: %s shows %q: %v", phase, name, secret, over)
				}
			}
		}
		// Without JavaScript, the page shows the game as it is.
		for _, want := range []string{"Game " + g.ID, "dusty-heron", "quiet-otter", "kitchen, " + phase, "Moderator", `<noscript><meta http-equiv="refresh"`} {
			if !strings.Contains(pages["page"], want) {
				t.Errorf("%s game: the page has no %q", phase, want)
			}
		}
		if (phase == voting || phase == over) != strings.Contains(pages["page"], "✓ voted") {
			t.Errorf("%s game: votes on the page", phase)
		}
	}

	// The listing: games being played as live (the house game too, unmarked),
	// the one just over as finished; not the one waiting, nor the one that
	// never started.
	var l gamekit.Listing
	if err := json.Unmarshal([]byte(gametest.Spectated(t, srv.URL, "sa1")["api"]), &l); err != nil {
		t.Fatal(err)
	}
	ids := func(gs []gamekit.Listed) (out []string) {
		for _, g := range gs {
			out = append(out, g.ID)
		}
		return out
	}
	if got := strings.Join(ids(l.Live), " "); !strings.Contains(got, "sa1") || !strings.Contains(got, "sa2") || !strings.Contains(got, "sahouse") || len(l.Live) != 3 {
		t.Errorf("live: %s", got)
	}
	if got := strings.Join(ids(l.Finished), " "); got != "sa3" {
		t.Errorf("finished: %s", got)
	}
	landing := gametest.Spectated(t, srv.URL, "sa1")["landing"]
	for _, want := range []string{`href="/games/sa1"`, `href="/games/sahouse"`, "house-a, house-b, house-c</span>", "discussion · "} {
		if !strings.Contains(landing, want) {
			t.Errorf("the landing page has no %q", want)
		}
	}
	for path, code := range map[string]int{"/games/saclosed": 404, "/games/nope": 404, "/games/nope/state": 404} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != code {
			t.Errorf("%s: %d, want %d", path, resp.StatusCode, code)
		}
	}
}

// With no game on, the landing page and the listing say so.
func TestNoGamesOn(t *testing.T) {
	app := &App{Kit: &appkit.App{ID: "secretagent", MCPURL: "https://sa.example.test/mcp"}}
	app.Store, _ = gametest.Store(t)
	srv := httptest.NewServer(app.Handler())
	t.Cleanup(srv.Close)
	g := &game{}
	g.ID, g.Phase, g.Open = "sawait", waiting, true
	gametest.Put(t, app.Store, g.ID, waiting, true, false, g)
	pages := gametest.Spectated(t, srv.URL, g.ID)
	if !strings.Contains(pages["landing"], "No game on right now.") || !strings.Contains(pages["list"], "No game on right now.") ||
		strings.Contains(pages["list"], `id="live-empty" hidden`) || pages["api"] != `{"live":[],"finished":[]}`+"\n" {
		t.Fatalf("no games: %v", pages)
	}
	// The page's script and stylesheet, by content hash, cached for good.
	for _, path := range []string{"/spectate.js", "/spectate.css"} {
		i := strings.Index(pages["page"], path+"?v=")
		if i < 0 {
			t.Fatalf("the page doesn't load %s", path)
		}
		u := pages["page"][i : i+strings.IndexByte(pages["page"][i:], '"')]
		resp, err := http.Get(srv.URL + u)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Cache-Control"), "immutable") {
			t.Errorf("%s: %d %v", u, resp.StatusCode, resp.Header)
		}
	}
}

// Every move that can carry an operator note has it last, described alike.
func TestNoteArguments(t *testing.T) {
	gametest.Note(t, sayIn{})
	gametest.Note(t, voteIn{})
}
