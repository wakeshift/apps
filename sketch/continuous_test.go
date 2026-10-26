package sketch

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/apps/gamekit"
)

// join seats a new player in the fixture's game, as the join tool does
// once matchmaking has its seat.
func (f *fixture) join(i int, handle string) gamekit.Player {
	f.t.Helper()
	p := gamekit.Player{Workspace: fmt.Sprintf("j%d", i), Agent: "a", Session: "s", Handle: handle}
	f.move(nil, func(tx *gamekit.Tx, g *game) error {
		if _, err := tx.Exec(tx.Ctx, `INSERT INTO seats (game_id, player, workspace, agent, session, name, handle, reserved_at, seated_at) VALUES ($1, $2, $3, $4, $5, $6, $6, now(), now())`,
			g.ID, p.Key(), p.Workspace, p.Agent, p.Session, p.Handle); err != nil {
			return err
		}
		return f.a.seated(tx, g, p)
	})
	return p
}

// leave has a player leave, as the leave tool does.
func (f *fixture) leave(key string) *game {
	f.t.Helper()
	return f.move(nil, func(tx *gamekit.Tx, g *game) error { return f.a.leave(tx, g, g.player(key)) })
}

// A player who joins a room that's playing guesses at once and draws when
// the rotation reaches it: after everyone who was there.
func TestJoinMidGame(t *testing.T) {
	f := newFixture(t, 3)
	g := f.step(nil)
	before := slices.Clone(g.Order)
	// A room open to joiners while it plays is listed as on now.
	if l := f.a.games(bg); len(l.Live) != 1 || l.Live[0].ID != f.id {
		t.Fatalf("live rooms: %+v", l.Live)
	}
	p := f.join(0, "puffin")
	g = f.load()
	if g.Phase != drawing || g.Round != 1 || g.player(p.Key()) == nil || g.Order[len(g.Order)-1] != p.Key() || !g.Open {
		t.Fatalf("after a join mid-round: %s round %d order %v", g.Phase, g.Round, g.Order)
	}
	if !strings.Contains(f.lastSaid(), "puffin joined the room") {
		t.Fatalf("said %q", f.lastSaid())
	}
	// It can guess this round.
	f.move(nil, func(tx *gamekit.Tx, g *game) error {
		_, err := f.a.guess(tx, g, g.player(p.Key()), "zzzz", false)
		return err
	})
	// Forfeits all round: the three who were there draw, then the joiner.
	var drawers []string
	for range 3 {
		drawers = append(drawers, g.Drawer)
		g = f.step(nil)
	}
	if !slices.Equal(drawers, before) || g.Drawer != p.Key() || g.Round != 4 {
		t.Fatalf("drawers %v then %s, want %v then the joiner", drawers, g.Drawer, before)
	}
}

// A drawer who leaves voids its round and the next player draws; with too
// few left the room waits, resumes when someone joins, and a waiting room
// nobody joins closes.
func TestLeavingIdleAndResume(t *testing.T) {
	f := newFixture(t, 3)
	for _, p := range f.load().Players {
		f.touch(p.Key()) // they joined just now: still there while the room waits
	}
	g := f.step(nil)
	first := g.Drawer
	g = f.leave(first)
	if g.Phase != reveal || g.Rounds[0].Outcome != "void" || !strings.Contains(f.lastSaid(), "left while drawing") || !strings.Contains(f.lastSaid(), "Next round in") {
		t.Fatalf("after the drawer left: %s %+v %q", g.Phase, g.Rounds, f.lastSaid())
	}
	var seats int
	f.a.Store.Pool.QueryRow(bg, `SELECT count(*) FROM seats WHERE game_id = $1 AND player = $2`, f.id, first).Scan(&seats)
	if seats != 0 {
		t.Fatal("the leaver's seat is still there")
	}
	g = f.step(nil)
	if g.Phase != drawing || g.Round != 2 || g.Drawer == first || g.player(first) != nil || len(g.Order) != 2 {
		t.Fatalf("round 2: %s %d drawer %s order %v", g.Phase, g.Round, g.Drawer, g.Order)
	}
	// A guesser leaves: one is left, so the round is void and the room waits.
	var guesser string
	for _, p := range g.Players {
		if p.Key() != g.Drawer {
			guesser = p.Key()
		}
	}
	g = f.leave(guesser)
	if g.Phase != reveal || g.Rounds[len(g.Rounds)-1].Outcome != "void" || !strings.Contains(f.lastSaid(), "the room waits for more") {
		t.Fatalf("after a guesser left: %s %q", g.Phase, f.lastSaid())
	}
	g = f.step(nil)
	if g.Phase != waiting || g.Word != "" || !g.Until.Equal(f.clock().Add(f.a.wait())) || !g.Deadline.Equal(f.clock().Add(waitCheck)) || !strings.Contains(f.lastSaid(), "Waiting for players") || !g.Open {
		t.Fatalf("idle: %s %q deadline %v", g.Phase, f.lastSaid(), g.Deadline.Sub(f.clock()))
	}
	// Someone joins: two will do now, after the short pause.
	f.join(1, "puffin")
	g = f.load()
	if g.Phase != waiting || !g.Deadline.Equal(f.clock().Add(f.a.pause())) || !strings.Contains(f.lastSaid(), "Back on") {
		t.Fatalf("after a join: %s deadline %v %q", g.Phase, g.Deadline.Sub(f.clock()), f.lastSaid())
	}
	g = f.step(nil)
	if g.Phase != drawing || g.Round != 3 {
		t.Fatalf("resumed: %s round %d", g.Phase, g.Round)
	}
	// Both leave (the second between rounds): the room waits at once, and
	// closes when nobody comes.
	for _, p := range slices.Clone(g.Players) {
		f.leave(p.Key())
	}
	g = f.load()
	if g.Phase != waiting || len(g.Players) != 0 || !strings.Contains(f.lastSaid(), "Nobody is here") {
		t.Fatalf("empty: %s %d", g.Phase, len(g.active()))
	}
	start := f.clock()
	for g.Phase == waiting { // checking every minute that someone's here
		g = f.step(nil)
	}
	if g.Phase != over || !f.clock().Equal(start.Add(f.a.wait())) || !g.Ended() || !strings.Contains(f.lastSaid(), "Room closed") || !strings.Contains(f.lastSaid(), "Not enough players came back") {
		t.Fatalf("closed: %s %q", g.Phase, f.lastSaid())
	}
}

// A player who leaves a waiting room's countdown stops it.
func TestLeavingStopsTheCountdown(t *testing.T) {
	f := newFixture(t, 2)
	f.join(0, "puffin")
	g := f.load()
	if !g.Deadline.Equal(f.clock().Add(f.a.lobby())) || !strings.Contains(f.lastSaid(), "first round starts in 30 seconds") {
		t.Fatalf("countdown: %v %q", g.Deadline.Sub(f.clock()), f.lastSaid())
	}
	g = f.leave(g.Players[0].Key())
	if g.Phase != waiting || len(g.Players) != 2 || !g.Deadline.Equal(f.clock().Add(f.a.wait())) {
		t.Fatalf("after a leave: %s %d players, deadline %v", g.Phase, len(g.Players), g.Deadline.Sub(f.clock()))
	}
}

// A player who leaves and comes back is one player again, and its points
// from the rounds it played still count.
func TestRejoin(t *testing.T) {
	f := newFixture(t, 3)
	g := f.step(nil)
	var p *player
	for _, q := range g.Players {
		if q.Key() != g.Drawer {
			p = q
		}
	}
	f.set(f.clock().Add(time.Second))
	g = f.move(nil, func(tx *gamekit.Tx, g *game) error {
		f.a.drew(tx, g, []stroke{line(1, 1, 2, 2)})
		_, err := f.a.guess(tx, g, g.player(p.Key()), g.Word, false)
		return err
	})
	pts := g.player(p.Key()).Score
	f.leave(p.Key())
	g = f.step(nil) // round 2: it's pruned
	if g.player(p.Key()) != nil {
		t.Fatal("the leaver is still on the scoreboard")
	}
	f.move(nil, func(tx *gamekit.Tx, g *game) error { return f.a.seated(tx, g, p.Player) })
	g = f.load()
	n := 0
	for _, q := range g.Players {
		if q.Key() == p.Key() {
			n++
		}
	}
	if n != 1 || g.player(p.Key()).Score != pts || pts == 0 {
		t.Fatalf("back: %d of it, score %d, want %d", n, g.player(p.Key()).Score, pts)
	}
}

// Scores count the room's last 20 rounds: round 21's guess pushes round
// 1's points off, while the leaderboard keeps the best score reached.
func TestRollingScores(t *testing.T) {
	f := newFixture(t, 3)
	g := f.step(nil)
	drawer, guesser := g.Players[0], g.Players[1]
	for i := 1; i <= scoreWindow+1; i++ {
		f.set(f.clock().Add(time.Minute))
		g = f.move(nil, func(tx *gamekit.Tx, g *game) error {
			g.Phase, g.Round, g.Drawer, g.Word, g.RoundStart = drawing, i, drawer.Key(), "kite", tx.Now.Add(-10*time.Second)
			g.Tier = easy
			if i == 1 {
				g.Tier = hard
			}
			return f.a.rightGuess(tx, g, g.player(guesser.Key()), false)
		})
		want := 3 + (i - 1)
		if i > scoreWindow {
			want = scoreWindow // the hard round is gone: twenty easy ones
		}
		if s := g.player(guesser.Key()).Score; s != want || g.player(drawer.Key()).Score != want {
			t.Fatalf("after round %d: guesser %d, drawer %d, want %d", i, s, g.player(drawer.Key()).Score, want)
		}
	}
	if len(g.Rounds) != scoreWindow || g.Rounds[0].Round != 2 {
		t.Fatalf("rounds kept: %d, from %d", len(g.Rounds), g.Rounds[0].Round)
	}
	if !strings.Contains(f.lastSaid(), fmt.Sprintf("Scores (last %d rounds)", scoreWindow)) {
		t.Fatalf("said %q", f.lastSaid())
	}
	rows, err := f.a.leaderboard(bg, 10)
	if err != nil || len(rows) != 2 || rows[0].Points != 3+scoreWindow-1 || rows[0].Rounds != scoreWindow+1 {
		t.Fatalf("leaderboard %+v %v", rows, err)
	}
	if s := snapshot(g, nil); s.Window != scoreWindow || s.Players[0].Score != scoreWindow {
		t.Fatalf("the page's scores: %+v", s.Players)
	}
	if page := httpGet(t, f.a, "/games/"+f.id); !strings.Contains(page, fmt.Sprintf("last %d rounds", scoreWindow)) {
		t.Fatal("the page doesn't say scores count the last 20 rounds")
	}
}

// Words don't come back soon in a room, or to a player who's had them in
// another room.
func TestWordsDontRepeat(t *testing.T) {
	f := newFixture(t, 3)
	seen := map[string]int{}
	for i := range 220 {
		f.move(nil, func(tx *gamekit.Tx, g *game) error {
			w, err := f.a.pickWord(tx, g)
			if err != nil {
				return err
			}
			if at, ok := seen[w.Word]; ok {
				t.Fatalf("%q again at pick %d (first at %d)", w.Word, i, at)
			}
			seen[w.Word] = i
			return nil
		})
	}
	g := f.load()
	if len(g.Recent) != 220 {
		t.Fatalf("the room remembers %d words", len(g.Recent))
	}
	// Another room with one of these players doesn't give it the words it
	// remembers having had.
	p := g.Players[0]
	for i := range 50 {
		f.move(nil, func(tx *gamekit.Tx, g *game) error {
			r, err := tx.Recent("words", []string{p.Key()})
			if err != nil {
				return err
			}
			if len(r[p.Key()]) != recentWords {
				t.Fatalf("the player remembers %d words", len(r[p.Key()]))
			}
			w, err := f.a.pickWord(tx, &game{Players: []*player{{Player: p.Player}}})
			if slices.Contains(r[p.Key()], w.Word) {
				t.Fatalf("pick %d: %q came back to a player who'd had it", i, w.Word)
			}
			return err
		})
	}
}

// A room that guesses quickly gets harder words; one where the clock keeps
// running out, easier ones.
func TestTierOdds(t *testing.T) {
	a := &App{}
	rounds := func(outcome string, secs int, n int) []summary {
		var out []summary
		for range n {
			out = append(out, summary{Outcome: outcome, Seconds: secs})
		}
		return out
	}
	for name, tc := range map[string]struct {
		rounds []summary
		want   []tierOdd
	}{
		"new room":    {nil, tierOdds},
		"quick":       {rounds("guessed", 12, 6), fastOdds},
		"slow":        {append(rounds("timeout", 120, 4), rounds("guessed", 90, 2)...), slowOdds},
		"middling":    {append(rounds("guessed", 60, 4), rounds("timeout", 120, 2)...), tierOdds},
		"voids only":  {rounds("void", 0, 6), tierOdds},
		"quick, then": {append(rounds("timeout", 120, 6), rounds("guessed", 10, 6)...), fastOdds},
	} {
		if got := a.tierOdds(&game{Rounds: tc.rounds}); !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
	// Mostly medium and hard, whatever the room.
	for _, odds := range [][]tierOdd{tierOdds, fastOdds, slowOdds} {
		n := 0
		for _, o := range odds {
			n += o.n
			if o.tier == easy && o.n > 3 {
				t.Errorf("odds %v: too many easy words", odds)
			}
		}
		if n != 10 {
			t.Errorf("odds %v add up to %d", odds, n)
		}
	}
}

// The leaderboard ranks players by their best score, keeps each player's
// best 20 rooms only, and leaves house agents off.
func TestLeaderboardBest(t *testing.T) {
	f := newFixture(t, 2)
	g := f.load()
	for i := range 25 {
		f.set(f.clock().Add(time.Hour))
		f.move(nil, func(tx *gamekit.Tx, _ *game) error {
			m := &game{Players: []*player{
				{Player: g.Players[0].Player, Score: i + 1}, // 1 … 25: the best is 25
				{Player: gamekit.Player{Workspace: "h", Agent: "a", Handle: "house-heron", House: "Haiku 4.5"}, Score: 100},
			}}
			m.ID = fmt.Sprintf("m%02d", i)
			if err := f.a.recordBest(tx, m); err != nil {
				return err
			}
			m.Players[0].Score = 0 // a later round, lower: the best stays
			return f.a.recordBest(tx, m)
		})
	}
	rows, err := f.a.leaderboard(bg, 10)
	if err != nil || len(rows) != 1 || rows[0].Points != 25 || rows[0].Rounds != 40 || rows[0].Name != g.Players[0].Handle {
		t.Fatalf("leaderboard %+v %v (want a best of 25 over 40 kept rounds)", rows, err)
	}
	var n int
	f.a.Store.Pool.QueryRow(bg, `SELECT count(*) FROM sketch_best`).Scan(&n)
	if n != leaderRooms {
		t.Fatalf("%d rooms kept", n)
	}
}
