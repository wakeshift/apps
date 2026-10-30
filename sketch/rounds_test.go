package sketch

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wakeshift/apps/gamekit"
	"github.com/wakeshift/apps/gamekit/gametest"
	"github.com/wakeshift/wakeshift-go/appkit"
)

var bg = context.Background()

// fixture is an app on its own database with no Wakeshift (posts to it
// just fail, and are retried in vain), a clock the test moves, and one
// game whose moves the test makes directly.
type fixture struct {
	t  *testing.T
	a  *App
	id string

	mu  sync.Mutex // the clock is read by what moves start in the background
	now time.Time
}

func (f *fixture) clock() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fixture) set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = t
}

func newFixture(t *testing.T, players int) *fixture {
	t.Helper()
	store, _ := gametest.Store(t)
	if err := Migrate(bg, store); err != nil {
		t.Fatal(err)
	}
	_, key, _ := ed25519.GenerateKey(nil)
	f := &fixture{t: t, now: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC), id: "g" + fmt.Sprint(time.Now().UnixNano()%1e6)}
	f.a = &App{Kit: &appkit.App{ID: "sketch", Key: key, Core: "http://127.0.0.1:1", MCPURL: "http://sketch.test/mcp"}, Store: store,
		Now: f.clock, Coalesce: 5 * time.Second}
	f.a.server()
	g := &game{}
	g.ID, g.Phase, g.Open = f.id, waiting, true
	for i := range players {
		p := gamekit.Player{Workspace: fmt.Sprintf("w%d", i), Agent: "a", Session: "s", Handle: []string{"heron", "otter", "lynx", "wren", "newt"}[i]}
		g.Players = append(g.Players, &player{Player: p})
	}
	state, _ := json.Marshal(g)
	if _, err := store.Pool.Exec(bg, `INSERT INTO games (id, phase, max_seats, state) VALUES ($1, $2, 8, $3)`, f.id, waiting, state); err != nil {
		t.Fatal(err)
	}
	for _, p := range g.Players {
		// Each seat's conversation subscribed to game.update when it joined
		// (its subscription is sub-<handle>, as Wakeshift made it).
		if _, err := store.Pool.Exec(bg, `INSERT INTO seats (game_id, player, workspace, agent, session, name, handle, reserved_at, seated_at, subscription, subscriber)
			VALUES ($1, $2, $3, $4, $5, $6, $6, now(), now(), 'sub-' || $6, $3 || '/' || $4)`,
			f.id, p.Key(), p.Workspace, p.Agent, p.Session, p.Handle); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

// move runs fn on the game under its lock, as a tool or the ticker would.
func (f *fixture) move(away map[string]bool, fn func(tx *gamekit.Tx, g *game) error) *game {
	f.t.Helper()
	gs, err := f.a.srv.Update(bg, f.id, away, func(tx *gamekit.Tx, gs gamekit.State) error { return fn(tx, gs.(*game)) })
	if err != nil {
		f.t.Fatal(err)
	}
	return gs.(*game)
}

// step moves the clock to the game's deadline and runs its step.
func (f *fixture) step(away map[string]bool) *game {
	f.t.Helper()
	g := f.load()
	if g.Deadline.After(f.clock()) {
		f.set(g.Deadline)
	}
	return f.move(away, func(tx *gamekit.Tx, g *game) error { return f.a.step(tx, g) })
}

func (f *fixture) load() *game {
	f.t.Helper()
	gs, err := f.a.srv.Load(bg, f.id)
	if err != nil {
		f.t.Fatal(err)
	}
	return gs.(*game)
}

// said is everything the moderator has said in the game.
func (f *fixture) said() string {
	f.t.Helper()
	var b strings.Builder
	rows, _ := f.a.Store.Pool.Query(bg, `SELECT subject || ' | ' || text FROM room_log WHERE game_id = $1 ORDER BY seq`, f.id)
	defer rows.Close()
	for rows.Next() {
		var s string
		rows.Scan(&s)
		b.WriteString(s + "\n¶\n")
	}
	return b.String()
}

func (f *fixture) lastSaid() string {
	entries := strings.Split(strings.TrimSuffix(f.said(), "\n¶\n"), "\n¶\n")
	return entries[len(entries)-1]
}

// farewellRow is a removed player's last entry (gamekit's Tx.Remove): what
// it says, who it goes to, and the subscription it ends once sent.
type farewellRow struct{ Subject, Text, To, Ends string }

// farewell is the removed player handle's last entry (zero if none).
func (f *fixture) farewell(handle string) farewellRow {
	f.t.Helper()
	var r farewellRow
	f.a.Store.Pool.QueryRow(bg, `SELECT subject, text, recipients::text, farewell::text FROM room_log WHERE game_id = $1 AND farewell IS NOT NULL AND $2 = ANY(mentions)`,
		f.id, handle).Scan(&r.Subject, &r.Text, &r.To, &r.Ends)
	return r
}

// touch has a player call a tool now (as game, canvas or say do).
func (f *fixture) touch(key string) {
	f.t.Helper()
	if err := touch(bg, f.a.Store.Pool, f.id, key, f.clock()); err != nil {
		f.t.Fatal(err)
	}
}

func line(x0, y0, x1, y1 int) stroke {
	return stroke{Points: [][2]int{{x0, y0}, {x1, y1}}, Color: "black", Width: 2}
}

// By default (no coalesce) every batch of strokes reaches the guessers the
// moment it's drawn.
func TestStrokesGoOutAtOnce(t *testing.T) {
	f := newFixture(t, 3)
	f.a.Coalesce = 0
	start := f.clock()
	f.step(nil)
	for i, at := range []time.Duration{2 * time.Second, 2500 * time.Millisecond, 3 * time.Second} {
		f.set(start.Add(at))
		g := f.move(nil, func(tx *gamekit.Tx, g *game) error { f.a.drew(tx, g, []stroke{line(10, 10+i, 90, 90)}); return nil })
		if g.Announced != i+1 || !g.FlushAt.IsZero() {
			t.Fatalf("batch %d: announced %d, flush at %v", i+1, g.Announced, g.FlushAt)
		}
	}
}

// A round: the drawer gets its word, the first strokes reach the guessers
// at once and later ones at most every coalesce, hints come at half and
// three quarters of the round, a wrong guess is everyone's to see, and the
// first right one scores for the guesser and the drawer. Then the next
// player draws.
func TestRound(t *testing.T) {
	f := newFixture(t, 3)
	start := f.clock()
	g := f.step(nil)
	if g.Phase != drawing || g.Round != 1 || g.Word == "" || len(g.Order) != 3 || !g.Open {
		t.Fatalf("after the lobby: %+v", g)
	}
	if !g.Deadline.Equal(start.Add(30 * time.Second)) {
		t.Fatalf("first deadline %v, want the drawer's 30s", g.Deadline.Sub(start))
	}
	drawer := g.drawer()
	var guessers []*player
	for _, p := range g.Players {
		if p.Key() != drawer.Key() {
			guessers = append(guessers, p)
		}
	}
	// The drawer's turn event is queued with its word, and only for it.
	var data []byte
	if err := f.a.Store.Pool.QueryRow(bg, `SELECT data FROM sketch_outbox WHERE game_id = $1 AND kind = 'turn'`, f.id).Scan(&data); err != nil {
		t.Fatal(err)
	}
	var qt queuedTurn
	json.Unmarshal(data, &qt)
	if qt.User != gamekit.PlayerUser(drawer.Player) || qt.Turn.Word != g.Word {
		t.Fatalf("turn queued %+v", qt)
	}
	word := g.Word

	// The first strokes go out at once.
	f.set(start.Add(2 * time.Second))
	g = f.move(nil, func(tx *gamekit.Tx, g *game) error { f.a.drew(tx, g, []stroke{line(10, 10, 90, 90)}); return nil })
	if g.Announced != 1 || !g.FlushAt.IsZero() || !strings.Contains(f.lastSaid(), "#1 black w2: 10,10 90,90") {
		t.Fatalf("first strokes: announced %d, flush at %v, said %q", g.Announced, g.FlushAt, f.lastSaid())
	}
	// More within 5 seconds wait, with whatever else comes meanwhile.
	f.set(start.Add(4 * time.Second))
	f.move(nil, func(tx *gamekit.Tx, g *game) error { f.a.drew(tx, g, []stroke{line(10, 90, 90, 10)}); return nil })
	f.set(start.Add(5 * time.Second))
	g = f.move(nil, func(tx *gamekit.Tx, g *game) error {
		f.a.drew(tx, g, []stroke{line(50, 0, 50, 100), line(0, 50, 100, 50)})
		return nil
	})
	if g.Announced != 1 || !g.FlushAt.Equal(start.Add(7*time.Second)) || !g.Deadline.Equal(g.FlushAt) {
		t.Fatalf("coalescing: announced %d, flush at %v, deadline %v", g.Announced, g.FlushAt.Sub(start), g.Deadline.Sub(start))
	}
	g = f.step(nil)
	if g.Announced != 4 || !g.FlushAt.IsZero() || !strings.Contains(f.lastSaid(), "3 strokes (strokes 2–4") || !strings.Contains(f.lastSaid(), "#4 black w2: 0,50 100,50") {
		t.Fatalf("the coalesced strokes: announced %d, said %q", g.Announced, f.lastSaid())
	}
	// The next deadline is the first hint, at half time.
	if !g.Deadline.Equal(start.Add(32 * time.Second)) { // 30s after the first drawing, at 2s
		t.Fatalf("deadline %v, want the first hint at 32s", g.Deadline.Sub(start))
	}
	g = f.step(nil)
	if g.Hints != 1 || !strings.Contains(f.lastSaid(), g.letterCount()) || regexp.MustCompile(`(?i)\b`+regexp.QuoteMeta(word)+`\b`).MatchString(f.lastSaid()) {
		t.Fatalf("first hint: %q", f.lastSaid())
	}
	g = f.step(nil)
	if g.Hints != 2 || !f.clock().Equal(start.Add(47*time.Second)) || !strings.Contains(f.lastSaid(), "starts with \""+strings.ToUpper(word[:1])) {
		t.Fatalf("second hint at %v: %q", f.clock().Sub(start), f.lastSaid())
	}

	// A wrong guess, then the drawer can't guess, then a right one.
	f.move(nil, func(tx *gamekit.Tx, g *game) error {
		right, err := f.a.guess(tx, g, g.player(guessers[0].Key()), "zzzz", false)
		if right || err != nil {
			t.Fatalf("wrong guess: %v %v", right, err)
		}
		if _, err := f.a.guess(tx, g, g.player(guessers[0].Key()), "zzzz", false); err == nil {
			t.Fatal("a second guess within 2s was taken")
		}
		if _, err := f.a.guess(tx, g, g.player(drawer.Key()), word, false); err == nil {
			t.Fatal("the drawer guessed")
		}
		return nil
	})
	if s := snapshot(f.load(), nil); len(s.Guesses) != 1 || s.Guesses[0].Text != "zzzz" {
		t.Fatalf("public guesses %+v", s.Guesses)
	}
	f.set(f.clock().Add(3 * time.Second))
	g = f.move(nil, func(tx *gamekit.Tx, g *game) error {
		right, err := f.a.guess(tx, g, g.player(guessers[1].Key()), "The "+strings.TrimSuffix(word, "s")+"s", true)
		if !right || err != nil {
			t.Fatalf("right guess: %v %v", right, err)
		}
		return nil
	})
	pts := tierPoints(g.Tier)
	if g.Phase != reveal || g.player(guessers[1].Key()).Score != pts || g.player(drawer.Key()).Score != pts || g.player(guessers[0].Key()).Score != 0 {
		t.Fatalf("after the right guess: %s %+v", g.Phase, g.Players)
	}
	if !strings.Contains(f.lastSaid(), "guessed by its owner") || !strings.Contains(f.lastSaid(), fmt.Sprintf("%q", word)) {
		t.Fatalf("round over: %q", f.lastSaid())
	}
	if s := snapshot(g, nil); s.Word != word || len(s.Guesses) != 2 || !s.Guesses[0].Right || !s.Guesses[0].Owner || s.Guesses[0].Text != "The "+strings.TrimSuffix(word, "s")+"s" || s.Guesses[1].Text != "zzzz" {
		t.Fatalf("revealed: %+v", s)
	}

	// The next player draws.
	g = f.step(nil)
	if g.Phase != drawing || g.Round != 2 || g.drawer() == nil || g.Drawer == drawer.Key() || g.Word == word || len(g.Strokes) != 0 || len(g.Guesses) != 0 {
		t.Fatalf("round 2: %+v", g)
	}
}

// A drawer that doesn't draw within 30 seconds forfeits, and the next one
// starts at once; a round nobody guesses reveals the word and scores
// nothing; a guessed one puts both players' scores on the leaderboard; and
// once everyone has drawn, the rotation starts again: there's no end.
func TestForfeitTimeoutAndRotation(t *testing.T) {
	f := newFixture(t, 3)
	g := f.step(nil)
	first := g.Drawer
	word := g.Word
	g = f.step(nil) // 30s: no strokes
	if g.Phase != drawing || g.Round != 2 || g.Drawer == first || g.Rounds[0].Outcome != "forfeit" {
		t.Fatalf("after the forfeit: %s round %d %+v", g.Phase, g.Round, g.Rounds)
	}
	if !strings.Contains(f.said(), "forfeited and the turn passes to the next drawer. The word was \""+word) {
		t.Fatalf("said %q", f.said())
	}
	start := g.RoundStart
	f.set(start.Add(time.Second))
	f.move(nil, func(tx *gamekit.Tx, g *game) error { f.a.drew(tx, g, []stroke{line(1, 1, 2, 2)}); return nil })
	for _, p := range g.Players { // the guessers look, and stay
		f.touch(p.Key())
	}
	for g.Phase == drawing {
		g = f.step(nil)
	}
	if g.Phase != reveal || !f.clock().Equal(start.Add(61*time.Second)) || g.Rounds[1].Outcome != "timeout" || !strings.Contains(f.lastSaid(), "Nobody scores") {
		t.Fatalf("timeout at %v: %s %q", f.clock().Sub(start), g.Phase, f.lastSaid())
	}
	for _, p := range g.Players {
		if p.Score != 0 {
			t.Fatalf("a timeout scored: %+v", p)
		}
	}
	// The third round: someone guesses.
	g = f.step(nil)
	third := g.drawer()
	pts := tierPoints(g.Tier)
	f.set(f.clock().Add(time.Second))
	f.move(nil, func(tx *gamekit.Tx, g *game) error {
		f.a.drew(tx, g, []stroke{line(1, 1, 2, 2)})
		for _, p := range g.Players {
			if p.Key() != g.Drawer {
				_, err := f.a.guess(tx, g, p, g.Word, false)
				return err
			}
		}
		return nil
	})
	rows, err := f.a.leaderboard(bg, 10)
	if err != nil || len(rows) != 2 || rows[0].Points != pts || rows[0].Rounds != 3 {
		t.Fatalf("leaderboard %+v %v", rows, err)
	}
	if !strings.Contains(rows[0].Name+rows[1].Name, third.Handle) {
		t.Fatalf("the drawer isn't on the leaderboard: %+v", rows)
	}
	// Everyone has drawn once, and the room plays on: the rotation comes
	// back round to the first drawer.
	g = f.step(nil)
	if g.Phase != drawing || g.Round != 4 || g.Drawer != first || g.Ended() || len(g.Result) != 0 {
		t.Fatalf("after everyone drew: %s round %d drawer %s (first %s)", g.Phase, g.Round, g.Drawer, first)
	}
	if strings.Contains(f.said(), "Room closed") {
		t.Fatalf("the room closed:\n%s", f.said())
	}
}

// A player away for two rounds is taken out: its seat goes, so it can join
// another game, and so does its watch link.
func TestAwayPlayersAreRemoved(t *testing.T) {
	f := newFixture(t, 4)
	g := f.step(nil)
	away := g.Players[0]
	if g.Drawer == away.Key() {
		away = g.Players[1]
	}
	u, err := f.a.watchURL(bg, g, g.player(away.Key()))
	if err != nil || u == "" {
		t.Fatal(u, err)
	}
	secret := u[strings.LastIndex(u, "/")+1:]
	if _, _, err := f.a.watcher(bg, secret); err != nil {
		t.Fatal(err)
	}
	gone := map[string]bool{away.Handle: true}
	g = f.step(gone) // a forfeit; the next round starts: away once
	if g.player(away.Key()).Missed != 1 || g.player(away.Key()).Left {
		t.Fatalf("after one round away: %+v", g.player(away.Key()))
	}
	for g.Round < 3 {
		g = f.step(gone)
	}
	if p := g.player(away.Key()); p != nil || slices.Contains(g.Order, away.Key()) || !strings.Contains(f.said(), away.Handle+" left the room: away for 2 rounds in a row") {
		t.Fatalf("after two rounds away: %+v %v\n%s", p, g.Order, f.said())
	}
	var seats int
	f.a.Store.Pool.QueryRow(bg, `SELECT count(*) FROM seats WHERE game_id = $1 AND player = $2`, f.id, away.Key()).Scan(&seats)
	if seats != 0 {
		t.Fatal("the removed player's seat is still there")
	}
	if _, _, err := f.a.watcher(bg, secret); err != errNoWatch {
		t.Fatalf("the removed player's watch link: %v", err)
	}
	// It's told why, to it alone, and its seat's subscription (with its
	// user, as the seat had them) is ended once that's sent.
	fw := f.farewell(away.Handle)
	if !strings.Contains(fw.Text, "away for 2 rounds in a row") || !strings.Contains(fw.Text, "Call the `join` tool to play again") ||
		fw.To != `{"m": [{"user": "`+away.Workspace+"/"+away.Agent+`", "subscription": "sub-`+away.Handle+`"}]}` ||
		fw.Ends != `{"user": "`+away.Workspace+"/"+away.Agent+`", "subscription": "sub-`+away.Handle+`"}` {
		t.Fatalf("the removed player's farewell: %+v", fw)
	}
	if g.Drawer == away.Key() {
		t.Fatal("the removed player draws")
	}
}

// The word reaches the drawer and nobody else: not the guessers' view,
// not the public page or a guesser's watch page (its HTML or its live
// updates, wrong guesses and all), not the moderator's messages, until the
// round's over. The right guess that ends it shows, with a "got it" chip,
// in the same change that reveals the word.
func TestWordStaysSecret(t *testing.T) {
	f := newFixture(t, 3)
	g := f.step(nil)
	word := g.Word
	f.set(f.clock().Add(time.Second))
	g = f.move(nil, func(tx *gamekit.Tx, g *game) error { f.a.drew(tx, g, []stroke{line(1, 1, 50, 50)}); return nil })
	for g.Hints < 2 {
		g = f.step(nil)
	}
	drawer := g.drawer()
	var guessers []*player
	for _, p := range g.Players {
		if p.Key() != drawer.Key() {
			guessers = append(guessers, p)
		}
	}
	g = f.move(nil, func(tx *gamekit.Tx, g *game) error {
		_, err := f.a.guess(tx, g, g.player(guessers[0].Key()), "zzzz", false)
		return err
	})
	if g.Phase != drawing || len(g.Guesses) != 1 {
		t.Fatalf("after a wrong guess: %s %+v", g.Phase, g.Guesses)
	}
	drawer = g.drawer()
	has := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(word) + `\b`)
	for _, p := range g.Players {
		v := jsonText(t, f.a.view(bg, g, p))
		if has.MatchString(v) != (p == drawer) {
			t.Errorf("%s's view (drawer %v): %s", p.Handle, p == drawer, v)
		}
		if s := jsonText(t, snapshot(g, p)); has.MatchString(s) != (p == drawer) {
			t.Errorf("%s's watch page: %s", p.Handle, s)
		}
	}
	if s := jsonText(t, snapshot(g, nil)); has.MatchString(s) {
		t.Errorf("the public page has the word: %s", s)
	}
	if said := f.said(); has.MatchString(said) {
		t.Errorf("the moderator said the word:\n%s", said)
	}
	// The page's text, not its markup: "apple" is a word, and the page links
	// its apple-touch-icon.
	text := regexp.MustCompile(`<[^>]*>`).ReplaceAllString(httpGet(t, f.a, "/games/"+f.id), " ")
	if page := text; has.MatchString(page) || !strings.Contains(page, "Round 1 ·") || !strings.Contains(page, "zzzz") || strings.Contains(page, "got it") {
		t.Errorf("the public page's HTML has the word:\n%s", page)
	}
	u, err := f.a.watchURL(bg, g, g.player(guessers[1].Key()))
	if err != nil {
		t.Fatal(err)
	}
	watch := "/watch/" + u[strings.LastIndex(u, "/")+1:]
	if page := regexp.MustCompile(`<[^>]*>`).ReplaceAllString(httpGet(t, f.a, watch), " "); has.MatchString(page) || !strings.Contains(page, "zzzz") {
		t.Errorf("a guesser's watch page has the word:\n%s", page)
	}

	// The right guess, typed by its owner: the round's over, and the guess
	// shows as typed, with its chip, beside its guesser's name.
	f.set(f.clock().Add(3 * time.Second))
	typed := "The " + strings.ToUpper(word)
	g = f.move(nil, func(tx *gamekit.Tx, g *game) error {
		right, err := f.a.guess(tx, g, g.player(guessers[1].Key()), typed, true)
		if !right || err != nil {
			t.Fatalf("right guess: %v %v", right, err)
		}
		return nil
	})
	winner := display(guessers[1].Handle)
	for _, me := range []*player{nil, g.player(guessers[0].Key()), g.drawer()} {
		s := snapshot(g, me)
		if s.Phase != reveal || s.Word != word || len(s.Guesses) != 2 || !s.Guesses[0].Right || s.Guesses[0].Text != typed ||
			s.Guesses[0].By != winner || s.Guesses[1].Right || s.Guesses[1].Text != "zzzz" {
			t.Errorf("revealed, for %v: %+v", me, s)
		}
		for _, p := range s.Players {
			if p.Got != (p.Name == winner) {
				t.Errorf("scoreboard, for %v: %+v", me, s.Players)
			}
		}
	}
	f.a.live.mu.Lock()
	f.a.live.games = nil // the pages read the game again, not from their cache
	f.a.live.mu.Unlock()
	for _, path := range []string{"/games/" + f.id, watch} {
		page := httpGet(t, f.a, path)
		if !regexp.MustCompile(`(?s)<li class="right"><span class="by">`+regexp.QuoteMeta(winner)+` <em>guessed by its owner</em></span><span class="text">`+
			regexp.QuoteMeta(typed)+` <span class="got">got it</span></span></li>`).MatchString(page) ||
			!strings.Contains(page, `<span class="name">`+winner+`</span><span class="got">got it</span>`) || strings.Count(page, `class="got"`) != 2 {
			t.Errorf("%s after the round: no right guess with its chip:\n%s", path, page)
		}
	}
}

// jsonText is v's string values as JSON has them, one a line: what a
// page or view shows, without its field names ("window" is a word too).
func jsonText(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var x any
	json.Unmarshal(b, &x)
	var out []string
	var walk func(any)
	walk = func(x any) {
		switch x := x.(type) {
		case string:
			out = append(out, x)
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(x)
	return strings.Join(out, "\n")
}

// httpGet is a page of the app's.
func httpGet(t *testing.T, a *App, path string) string {
	t.Helper()
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: %d %s", path, resp.StatusCode, b)
	}
	return string(b)
}

func TestStrokes(t *testing.T) {
	pt := func(x, y float64) []float64 { return []float64{x, y} }
	ok := []strokeIn{{Points: [][]float64{pt(0, 0), pt(100, 100)}}, {Points: [][]float64{pt(50.4, 50.6)}, Color: "Red", Width: 6}}
	got, err := checkStrokes(ok)
	if err != nil || got[0].Color != "black" || got[0].Width != 2 || got[1].Color != "red" || got[1].Points[0] != [2]int{50, 51} {
		t.Fatalf("%+v %v", got, err)
	}
	long := make([][]float64, 33)
	for i := range long {
		long[i] = pt(1, 1)
	}
	for name, in := range map[string][]strokeIn{
		"none":       {},
		"eleven":     make([]strokeIn, 11),
		"no points":  {{}},
		"33 points":  {{Points: long}},
		"off canvas": {{Points: [][]float64{pt(0, 0), pt(101, 5)}}},
		"negative":   {{Points: [][]float64{pt(-1, 0), pt(1, 5)}}},
		"3d":         {{Points: [][]float64{{1, 2, 3}}}},
		"color":      {{Points: [][]float64{pt(1, 1)}, Color: "chartreuse"}},
		"too wide":   {{Points: [][]float64{pt(1, 1)}, Width: 7}},
		"negative w": {{Points: [][]float64{pt(1, 1)}, Width: -1}},
	} {
		if _, err := checkStrokes(in); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if s := strokeText([]stroke{{Points: [][2]int{{1, 2}, {3, 4}}, Color: "blue", Width: 3}}, 7); s != "#7 blue w3: 1,2 3,4" {
		t.Errorf("strokeText %q", s)
	}
}

func TestHint(t *testing.T) {
	g := &game{Word: "hot air balloon"}
	if g.hint() != "" {
		t.Error("a hint before any")
	}
	g.Hints = 1
	if h := g.hint(); h != "_ _ _   _ _ _   _ _ _ _ _ _ _" || g.letterCount() != "3 + 3 + 7 letters" {
		t.Errorf("first hint %q %q", h, g.letterCount())
	}
	g.Hints = 2
	if h := g.hint(); h != "H _ _   _ _ _   _ _ _ _ _ _ _" {
		t.Errorf("second hint %q", h)
	}
}

// Every move that can carry an operator note has it last, described alike.
func TestNoteArguments(t *testing.T) {
	gametest.Note(t, drawIn{})
	gametest.Note(t, guessIn{})
	gametest.Note(t, sayIn{})
}
