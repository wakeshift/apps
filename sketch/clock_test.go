package sketch

import (
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/apps/gamekit"
)

// The round's 60 seconds run from the drawer's first drawing, not the
// round's start; before it, the round waits 30 seconds for the drawer (and
// shows that clock), with no hints; the hints come 30 and 45 seconds after
// the first drawing; and the round ends 60 seconds after it.
func TestRoundClockStartsAtFirstDrawing(t *testing.T) {
	f := newFixture(t, 3)
	start := f.clock()
	g := f.step(nil)
	if !g.RoundEnd.Equal(start.Add(30*time.Second)) || !g.Deadline.Equal(start.Add(30*time.Second)) || !g.DrawnAt.IsZero() {
		t.Fatalf("before drawing: ends %v, deadline %v", g.RoundEnd.Sub(start), g.Deadline.Sub(start))
	}
	if said := f.lastSaid(); !strings.Contains(said, "within 30 seconds, or the turn passes to the next drawer") || !strings.Contains(said, "The round lasts 1 minute from the first drawing") {
		t.Fatalf("round start: %q", said)
	}
	drawer := g.drawer()
	var guesser *player
	for _, p := range g.Players {
		if p != drawer {
			guesser = p
		}
	}
	// Waiting for the drawer: the pages show the drawer's clock, the views
	// say so, and nobody's told a round clock.
	f.set(start.Add(10 * time.Second))
	g = f.load()
	s := snapshot(g, nil)
	if !s.Waiting || s.EndsAt != start.Add(30*time.Second).UnixMilli() || !strings.Contains(s.Status, "waiting for") {
		t.Fatalf("waiting snapshot %+v", s)
	}
	dv := f.a.view(bg, g, drawer)
	if dv["seconds_to_start"] != 20 || dv["round_seconds"] != 60 || dv["seconds_left"] != nil || !strings.Contains(dv["note"].(string), "the turn passes to the next drawer") {
		t.Fatalf("drawer's view %v", dv)
	}
	gv := f.a.view(bg, g, guesser)
	if gv["seconds_left"] != nil || !strings.Contains(gv["note"].(string), "the drawer has 20s to start") {
		t.Fatalf("guesser's view %v", gv)
	}
	page := httpGet(t, f.a, "/games/"+f.id)
	if !strings.Contains(page, `class="clock waiting"`) || !strings.Contains(page, "Waiting for the drawer") {
		t.Fatalf("the page doesn't show the waiting clock:\n%s", page)
	}

	// The first drawing, 20 seconds in, starts the round's clock.
	drawnAt := start.Add(20 * time.Second)
	f.set(drawnAt)
	g = f.move(nil, func(tx *gamekit.Tx, g *game) error { f.a.drew(tx, g, []stroke{line(10, 10, 90, 90)}); return nil })
	if !g.DrawnAt.Equal(drawnAt) || !g.RoundEnd.Equal(drawnAt.Add(time.Minute)) || !g.Deadline.Equal(drawnAt.Add(30*time.Second)) {
		t.Fatalf("after drawing: drawn %v, ends %v, deadline %v", g.DrawnAt.Sub(start), g.RoundEnd.Sub(start), g.Deadline.Sub(start))
	}
	if s := snapshot(g, nil); s.Waiting || s.EndsAt != drawnAt.Add(time.Minute).UnixMilli() {
		t.Fatalf("drawing snapshot %+v", s)
	}
	if v := f.a.view(bg, g, guesser); v["seconds_left"] != 60 {
		t.Fatalf("guesser's view after drawing %v", v)
	}
	// More drawing doesn't move it.
	f.set(drawnAt.Add(5 * time.Second))
	g = f.move(nil, func(tx *gamekit.Tx, g *game) error { f.a.drew(tx, g, []stroke{line(1, 1, 9, 9)}); return nil })
	if !g.RoundEnd.Equal(drawnAt.Add(time.Minute)) {
		t.Fatalf("a later drawing moved the end to %v", g.RoundEnd.Sub(start))
	}
	// The hints: 30 and 45 seconds after the first drawing.
	g = f.step(nil)
	if g.Hints != 1 || !f.clock().Equal(drawnAt.Add(30*time.Second)) || !strings.Contains(f.lastSaid(), "30s left") {
		t.Fatalf("first hint at %v: %q", f.clock().Sub(drawnAt), f.lastSaid())
	}
	g = f.step(nil)
	if g.Hints != 2 || !f.clock().Equal(drawnAt.Add(45*time.Second)) || !strings.Contains(f.lastSaid(), "15s left") {
		t.Fatalf("second hint at %v: %q", f.clock().Sub(drawnAt), f.lastSaid())
	}
	// The end: 60 seconds after the first drawing.
	g = f.step(nil)
	if g.Phase != reveal || g.Outcome != "timeout" || !f.clock().Equal(drawnAt.Add(time.Minute)) {
		t.Fatalf("end at %v: %s %s", f.clock().Sub(drawnAt), g.Phase, g.Outcome)
	}
	if ts := g.HintTimes; len(ts) != 2 || !ts[0].Equal(drawnAt.Add(30*time.Second)) || !ts[1].Equal(drawnAt.Add(45*time.Second)) {
		t.Fatalf("hint times %v", ts)
	}
}

// A drawer that draws nothing within 30 seconds of the round starting
// passes the turn: the next drawer's round starts at once, with no hint
// given and no score.
func TestSilentDrawerPassesTheTurn(t *testing.T) {
	f := newFixture(t, 3)
	start := f.clock()
	g := f.step(nil)
	first, order := g.Drawer, g.Order
	g = f.step(nil)
	if !f.clock().Equal(start.Add(30 * time.Second)) {
		t.Fatalf("the turn passed at %v", f.clock().Sub(start))
	}
	next := order[(index(order, first)+1)%len(order)]
	if g.Phase != drawing || g.Round != 2 || g.Drawer != next || !g.RoundStart.Equal(f.clock()) || g.Rounds[0].Outcome != "forfeit" {
		t.Fatalf("after the silent drawer: %s round %d drawer %s (want %s)", g.Phase, g.Round, g.Drawer, next)
	}
	if strings.Contains(f.said(), "Hint") {
		t.Fatal("a hint before any drawing")
	}
	for _, p := range g.Players {
		if p.Score != 0 {
			t.Fatalf("a forfeit scored: %+v", p)
		}
	}
	// The next drawer has its own 30 seconds.
	if !g.Deadline.Equal(f.clock().Add(30 * time.Second)) {
		t.Fatalf("next drawer's deadline %v", g.Deadline.Sub(f.clock()))
	}
}

func index(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

// A round started before the clock moved to the first drawing (no
// DrawnAt) keeps its old timing: hints from its start.
func TestRoundWithoutDrawnAt(t *testing.T) {
	a := &App{}
	start := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	g := &game{RoundStart: start, RoundEnd: start.Add(2 * time.Minute), Drawn: true}
	if !a.hintAt(g, 1).Equal(start.Add(time.Minute)) || !a.hintAt(g, 2).Equal(start.Add(90*time.Second)) {
		t.Fatalf("hints %v %v", a.hintAt(g, 1), a.hintAt(g, 2))
	}
}
