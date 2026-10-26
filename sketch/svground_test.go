package sketch

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/apps/gamekit"
)

// A round drawn with svg: the guessers hear how many shapes and their
// colors, never the markup; strokes drawn after them still go out as
// strokes; the pages and the canvas image get the shapes; the writing check
// looks at them. Nothing the drawer wrote in the markup (a comment, an id)
// reaches anyone.
func TestSVGRound(t *testing.T) {
	f := newFixture(t, 3)
	g := f.step(nil)
	f.set(f.clock().Add(time.Second))
	d, err := checkDraw(drawIn{SVG: `<svg viewBox="0 0 100 100"><!-- LEAKY --><g id="LEAKY" stroke="black">` +
		`<circle cx="50" cy="50" r="20" fill="red"/><rect x="10" y="80" width="80" height="10" fill="#2a9d55" fill-opacity="0.5"/></g></svg>`})
	if err != nil {
		t.Fatal(err)
	}
	g = f.move(nil, func(tx *gamekit.Tx, g *game) error {
		if err := d.fits(g.Strokes); err != nil {
			return err
		}
		f.a.drew(tx, g, d.marks)
		return nil
	})
	said := f.lastSaid()
	if !strings.Contains(said, "drew 2 shapes |") || !strings.Contains(said, "#1 shape: fill red, outline black w1\n#2 shape: fill green (50% opaque), outline black w1") ||
		!strings.Contains(said, "(marks 1–2;") {
		t.Fatalf("the shapes' announcement: %q", said)
	}

	// Strokes after them: announced as strokes, numbered on.
	f.set(f.clock().Add(6 * time.Second))
	g = f.move(nil, func(tx *gamekit.Tx, g *game) error { f.a.drew(tx, g, []stroke{line(5, 5, 20, 5)}); return nil })
	if said := f.lastSaid(); !strings.Contains(said, "drew 1 stroke |") || !strings.Contains(said, "#3 black w2: 5,5 20,5") {
		t.Fatalf("a stroke after shapes: %q", said)
	}

	drawer := g.drawer()
	for _, p := range g.Players {
		v := f.a.view(bg, g, p)
		if v["strokes_drawn"] != 3 || v["shapes_drawn"] != 2 {
			t.Errorf("%s's view: %v drawn, %v shapes", p.Handle, v["strokes_drawn"], v["shapes_drawn"])
		}
		if _, ok := v["svg_rules"]; ok != (p == drawer) {
			t.Errorf("%s (drawer %v) has the svg rules: %v", p.Handle, p == drawer, ok)
		}
	}
	// The page's stream carries the shapes as the server built them.
	s, _ := json.Marshal(snapshot(g, nil))
	if !strings.Contains(string(s), `"sh":{"d":[{"p":[[70,50],`) || strings.Contains(string(s), "LEAKY") || strings.Contains(string(s), "<") {
		t.Errorf("the page's snapshot: %s", s)
	}
	// The canvas image, for the page without script (and the canvas tool).
	png := []byte(httpGet(t, f.a, "/games/"+f.id+"/canvas.png"))
	if got := pixel(t, png, 50, 50); got != rgb("red") {
		t.Errorf("the canvas image at the circle: %v", got)
	}
	if got := pixel(t, f.a.renderCached(g), 50, 50); got != rgb("red") {
		t.Errorf("the canvas tool's image at the circle: %v", got)
	}
	if all := f.said(); strings.Contains(all, "LEAKY") || strings.Contains(all, "<circle") {
		t.Errorf("the markup was announced:\n%s", all)
	}

	// The writing check looks at shapes too.
	f.a.Writing = alwaysWriting(true)
	f.a.checkWriting(bg, drawingCheck{game: g.ID, round: g.Round, strokes: g.Strokes})
	if g := f.load(); g.Phase != reveal || g.Outcome != "writing" {
		t.Fatalf("writing in shapes: %s %s", g.Phase, g.Outcome)
	}
}
