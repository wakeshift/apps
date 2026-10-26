package sketch

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/apps/gamekit"
)

// playRound plays the round under way to its end and starts the next: a
// second in, everyone but quiet calls a tool, and the drawer (unless
// quiet) draws. Nobody guesses, so it runs to the clock (or, with a quiet
// drawer, is forfeited at 30 seconds). It returns the game as the next
// round starts (or the room waits, or closes).
func (f *fixture) playRound(quiet ...string) *game {
	f.t.Helper()
	g := f.load()
	f.set(g.RoundStart.Add(time.Second))
	for _, p := range g.active() {
		if slices.Contains(quiet, p.Key()) {
			continue
		}
		f.touch(p.Key())
		if p.Key() == g.Drawer {
			f.move(nil, func(tx *gamekit.Tx, g *game) error { f.a.drew(tx, g, []stroke{line(1, 1, 2, 2)}); return nil })
		}
	}
	for r := g.Round; g.Round == r && g.Phase != over && g.Phase != waiting; {
		g = f.step(nil)
	}
	return g
}

// guesserAt is the player in the rotation n places after this round's
// drawer: one that only guesses for the next n rounds.
func guesserAt(g *game, n int) string {
	return g.Order[(g.Turn+n)%len(g.Order)]
}

// A guesser that does nothing while drawings are up (Wakeshift still
// delivering to it, as it does for an agent whose service restarted) is
// away for each of those rounds, and after two in a row it's taken out,
// as if it had left: announced, its seat gone, out of the room in
// Wakeshift. The others, who call tools, play on.
func TestSilentPlayerIsRemoved(t *testing.T) {
	f := newFixture(t, 4)
	g := f.step(nil)
	silent := g.player(guesserAt(g, 2))
	g = f.playRound(silent.Key())
	if p := g.player(silent.Key()); p == nil || p.Missed != 1 {
		t.Fatalf("after one silent round: %+v", p)
	}
	for _, p := range g.active() {
		if p.Key() != silent.Key() && p.Missed != 0 {
			t.Fatalf("a player who played is counted away: %+v", p)
		}
	}
	g = f.playRound(silent.Key())
	if g.player(silent.Key()) != nil || g.Phase != drawing || len(g.active()) != 3 {
		t.Fatalf("after two silent rounds: %s, %d players, it's %+v", g.Phase, len(g.active()), g.player(silent.Key()))
	}
	if !strings.Contains(f.said(), silent.Handle+" left the room: away for 2 rounds in a row") {
		t.Fatalf("not announced:\n%s", f.said())
	}
	var seats, removes int
	f.a.Store.Pool.QueryRow(bg, `SELECT count(*) FROM seats WHERE game_id = $1 AND player = $2`, f.id, silent.Key()).Scan(&seats)
	f.a.Store.Pool.QueryRow(bg, `SELECT count(*) FROM sketch_outbox WHERE game_id = $1 AND kind = 'remove' AND data->>'handle' = $2`, f.id, silent.Handle).Scan(&removes)
	if seats != 0 || removes != 1 {
		t.Fatalf("seats %d, room removals %d", seats, removes)
	}
}

// A player that's been away a round and calls a tool again (it looks at
// the canvas) is back: its count starts again, and it stays.
func TestReturningPlayerStays(t *testing.T) {
	f := newFixture(t, 5)
	g := f.step(nil)
	p := g.player(guesserAt(g, 4))
	g = f.playRound(p.Key())
	if g.player(p.Key()).Missed != 1 {
		t.Fatalf("after a silent round: %+v", g.player(p.Key()))
	}
	g = f.playRound() // it calls a tool this round
	if q := g.player(p.Key()); q == nil || q.Missed != 0 {
		t.Fatalf("back: %+v", q)
	}
	g = f.playRound(p.Key()) // quiet again: once, not twice in a row
	if q := g.player(p.Key()); q == nil || q.Missed != 1 || q.Left {
		t.Fatalf("after another silent round: %+v", q)
	}
	// A guess from its owner's watch page counts as it being there too.
	f.set(g.RoundStart.Add(time.Second))
	f.move(nil, func(tx *gamekit.Tx, g *game) error { f.a.drew(tx, g, []stroke{line(1, 1, 2, 2)}); return nil })
	f.set(g.RoundStart.Add(5 * time.Second))
	f.move(nil, func(tx *gamekit.Tx, g *game) error {
		_, err := f.a.guess(tx, g, g.player(p.Key()), "zzzz", true)
		return err
	})
	for _, q := range g.active() {
		if q.Key() != p.Key() {
			f.touch(q.Key())
		}
	}
	for r := g.Round; g.Round == r; {
		g = f.step(nil)
	}
	if q := g.player(p.Key()); q == nil || q.Missed != 0 {
		t.Fatalf("after its owner guessed: %+v", q)
	}
}

// A drawer that hasn't drawn after 30 seconds is skipped, and the next
// starts at once rather than after the round's 2 minutes: one that never
// called a tool didn't respond; one that asked for its word but drew
// nothing didn't start. Each was away for its round.
func TestAbsentDrawerIsSkipped(t *testing.T) {
	f := newFixture(t, 4)
	g := f.step(nil)
	start, first := g.RoundStart, g.Drawer
	g = f.step(nil)
	if g.Round != 2 || g.Phase != drawing || !f.clock().Equal(start.Add(30*time.Second)) || g.Rounds[0].Outcome != "forfeit" {
		t.Fatalf("after the absent drawer: round %d %s at %v", g.Round, g.Phase, f.clock().Sub(start))
	}
	if !strings.Contains(f.said(), g.player(first).Handle+" didn't respond within 30 seconds") || g.player(first).Missed != 1 {
		t.Fatalf("the absent drawer: %+v\n%s", g.player(first), f.said())
	}
	second := g.drawer()
	f.set(g.RoundStart.Add(2 * time.Second))
	f.touch(second.Key()) // it calls `game` for its word, and draws nothing
	g = f.step(nil)
	if g.Round != 3 || !strings.Contains(f.said(), second.Handle+" didn't start drawing within 30 seconds") || g.player(second.Key()).Missed != 1 {
		t.Fatalf("the drawer that drew nothing: round %d %+v", g.Round, g.player(second.Key()))
	}
}

// A room whose players have all gone (none calls a tool, though Wakeshift
// may still say they're connected) skips each drawer at 30 seconds, takes
// players out as they miss their turns, and closes once nobody has played
// for 5 minutes, long before the half hour a waiting room gives players to
// come back. While it lasts it's open to matchmaking only as long as
// someone could still be there; closed, it's in no list and takes nobody.
func TestDesertedRoomCloses(t *testing.T) {
	f := newFixture(t, 3)
	start := f.clock()
	for _, p := range f.load().Players {
		f.touch(p.Key()) // they joined, then went
	}
	g := f.step(nil)
	for g.Phase != over && f.clock().Sub(start) < 20*time.Minute {
		g = f.step(nil)
	}
	took := f.clock().Sub(start)
	if g.Phase != over || !g.Ended() || took < idleClose || took > idleClose+waitCheck+f.a.lobby() {
		t.Fatalf("after %v: %s", took, g.Phase)
	}
	if !strings.Contains(f.lastSaid(), "Room closed") || !strings.Contains(f.lastSaid(), "Nobody has played in this room for 5 minutes") {
		t.Fatalf("said %q", f.lastSaid())
	}
	if l := f.a.games(bg); len(l.Live) != 0 {
		t.Fatalf("still on now: %+v", l.Live)
	}
}

// Matchmaking skips a room whose players have all gone: a waiting room
// left with a player that's missed its turn and done nothing for 5
// minutes closes at its next check, so no newcomer is seated there. One
// with a player still there (it called a tool lately) stays open.
func TestMatchmakingSkipsGhostRooms(t *testing.T) {
	f := newFixture(t, 3)
	start := f.clock()
	for _, p := range f.load().Players {
		f.touch(p.Key())
	}
	g := f.step(nil)
	quiet := g.Drawer
	// The drawer is skipped, and the others leave: the room waits with the
	// drawer, away once.
	g = f.step(nil)
	for _, p := range g.active() {
		if p.Key() != quiet {
			g = f.leave(p.Key())
		}
	}
	for g.Phase != waiting {
		g = f.step(nil)
	}
	if g.player(quiet) == nil || g.player(quiet).Missed != 1 || !g.Open {
		t.Fatalf("waiting with the absent drawer: %s %+v", g.Phase, g.player(quiet))
	}
	open := func() bool {
		var ok bool
		f.a.Store.Pool.QueryRow(bg, `SELECT open AND ended_at IS NULL FROM games WHERE id = $1`, f.id).Scan(&ok)
		return ok
	}
	lobby := func() int {
		l, err := f.a.srv.Lobby(bg)
		if err != nil {
			t.Fatal(err)
		}
		return len(l.Waiting)
	}
	// Within 5 minutes of its last call it could still be there: open to
	// matchmaking, as a room waiting for players.
	g = f.step(nil)
	if g.Phase != waiting || !open() || lobby() != 1 {
		t.Fatalf("a minute on: %s open %v", g.Phase, open())
	}
	for g.Phase == waiting && f.clock().Before(g.Until) {
		g = f.step(nil)
	}
	took := f.clock().Sub(start)
	if g.Phase != over || open() || lobby() != 0 || took > idleClose+waitCheck || !strings.Contains(f.lastSaid(), "Nobody has played in this room") {
		t.Fatalf("the ghost room after %v: %s open %v %q", took, g.Phase, open(), f.lastSaid())
	}
}
