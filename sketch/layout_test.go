package sketch

// The pages' layout, in a real browser: nothing on a page may cover
// anything else. A game is set up mid-round with eight players (the longest
// names), a drawing, a hint and a long list of 40-character
// guesses; the public page, a guesser's watch page (with its guess box), the
// drawer's (with its word), the landing page and the leaderboard are each
// loaded live at several sizes. The test measures the page's parts (the bar,
// the scorebug and its clock, the canvas, the hint, the guess box, the
// scoreboard's rows, the guesses, the footer) and fails if any two
// intersect, or if the page scrolls sideways. Needs Chrome (CHROME_PATH, or
// the usual install paths); it's skipped without one, except in CI
// (apps/layouttest).
//
// SKETCH_LAYOUT_SHOTS=<dir> also saves a screenshot of every page at every
// size.

import (
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/apps/gamekit"
	"github.com/wakeshift/apps/layouttest"
)

// measure runs in the page: the parts that must stay clear of each other,
// and what overlaps.
const measure = `(() => {
  const out = { violations: [], items: 0, overflow: document.documentElement.scrollWidth - window.innerWidth };
  const vis = (el) => { if (!el) return false; const r = el.getBoundingClientRect(); const cs = getComputedStyle(el); return r.width > 0 && r.height > 0 && cs.visibility !== 'hidden' && cs.display !== 'none'; };
  const box = (el) => { const r = el.getBoundingClientRect(); return { l: r.left, t: r.top + scrollY, r: r.right, b: r.bottom + scrollY }; };
  const hit = (p, q) => Math.min(p.r, q.r) - Math.max(p.l, q.l) > 0.5 && Math.min(p.b, q.b) - Math.max(p.t, q.t) > 0.5;
  // Groups whose members mustn't overlap each other.
  const groups = [
    ['.bar', '.scorebug', '.pitch', '.hintbar', '.mine', '.scores', '.feed', '.result', '.foot', '.hero-copy', '.hero-art', '.live-now', '.how', '.play', '.top', '.leaders table', '.gone', '.matchlist'],
    ['.brand', '.bar nav'], ['.bar nav a'],
    ['.live-dot', '.status', '.clock'], ['.hint', '.hint-label'], ['.who', '.guess', '.drawing-note', '.note'], ['.guess input', '.guess button'],
    ['.steps li'], ['.matches li'], ['.foot > span'], ['.section-head h2', '.section-head a'],
  ];
  for (const g of groups) {
    const items = [];
    for (const sel of g) document.querySelectorAll(sel).forEach((el, i) => { if (vis(el)) items.push({ name: sel + (i ? '#' + i : ''), ...box(el) }); });
    out.items += items.length;
    for (let i = 0; i < items.length; i++) for (let j = i + 1; j < items.length; j++) if (hit(items[i], items[j])) out.violations.push(items[i].name + ' overlaps ' + items[j].name);
  }
  // Each scoreboard row's name, label, pen and points; each guess's parts.
  document.querySelectorAll('.scores li').forEach((li, n) => {
    const parts = [...li.children].filter(vis).map((el) => ({ name: 'row' + n + ' .' + el.className, ...box(el) }));
    const row = box(li);
    for (let i = 0; i < parts.length; i++) {
      if (parts[i].l < row.l - 0.5 || parts[i].r > row.r + 0.5) out.violations.push(parts[i].name + ' leaves its row');
      for (let j = i + 1; j < parts.length; j++) if (hit(parts[i], parts[j])) out.violations.push(parts[i].name + ' overlaps ' + parts[j].name);
    }
  });
  document.querySelectorAll('.feed li, .panel, .scorebug, .hintbar, .mine').forEach((el) => {
    if (el.scrollWidth > el.clientWidth + 1) out.violations.push((el.className || el.tagName) + ' overflows sideways');
  });
  if (out.overflow > 1) out.violations.push('the page scrolls sideways by ' + out.overflow + 'px');
  return out;
})()`

// layoutGame sets up the game the layout test loads: round 3, a
// lighthouse half drawn, the first hint out, and a long list of guesses.
func layoutGame(t *testing.T) (*fixture, *game) {
	f := newFixture(t, 0)
	f.set(time.Now())
	names := []string{"fearless-flamingo", "breezy-narwhal-2", "chipper-aardvark", "curious-penguin", "spiffy-chameleon", "mellow-quokka", "dashing-pelican", "plucky-raccoon"}
	houses := []string{"", "Claude Sonnet 5 Thinking", "", "GPT-6 Luna Preview Long", "", "", "Haiku 4.5", ""}
	g := f.move(nil, func(tx *gamekit.Tx, g *game) error {
		for i, n := range names {
			p := gamekit.Player{Workspace: fmt.Sprintf("w%d", i), Agent: "a", Session: "s", Handle: n, House: houses[i]}
			g.seat(p)
			if _, err := tx.Exec(tx.Ctx, `INSERT INTO seats (game_id, player, workspace, agent, session, name, handle, reserved_at, seated_at) VALUES ($1, $2, $3, $4, $5, $6, $6, now(), now())`,
				g.ID, p.Key(), p.Workspace, p.Agent, p.Session, n); err != nil {
				return err
			}
		}
		if err := f.a.nextRound(tx, g); err != nil {
			return err
		}
		for i, p := range g.Players {
			p.Score = []int{3, 2, 2, 1, 1, 0, 0, 0}[i]
		}
		// Round 3, the lighthouse, with a minute and a bit left (the
		// screenshots are taken first).
		g.Round, g.Word, g.Tier, g.Turn = 3, "lighthouse", medium, 2
		g.Drawer = g.Order[2]
		g.Rounds = []summary{{Round: 1, Word: "kite", Outcome: "guessed"}, {Round: 2, Word: "snail", Outcome: "timeout"}}
		g.RoundStart = tx.Now.Add(-35 * time.Second)
		g.RoundEnd = g.RoundStart.Add(f.a.round())
		g.Hints = 1
		g.Strokes = lighthouse()
		g.Drawn, g.Announced = true, len(g.Strokes)
		texts := []string{"tower", "rocket ship", "candle", "a very tall striped birthday candle", "castle", "lamp post on a windy hill",
			"lighthouse keeper's cottage on the rocks", "chess piece", "pencil", "space needle tower with lights"}
		for i, tx2 := range texts {
			by := g.Players[(i*3)%len(g.Players)]
			if by.Key() == g.Drawer {
				by = g.Players[(i*3+1)%len(g.Players)]
			}
			g.Guesses = append(g.Guesses, guess{By: by.Handle, Text: tx2, Owner: i == 4, At: tx.Now})
		}
		g.changed()
		f.a.schedule(g)
		return nil
	})
	for i := range 14 {
		if _, err := f.a.Store.Pool.Exec(bg, `INSERT INTO sketch_best (game_id, player, name, best, rounds, reached_at, updated_at) VALUES ($1, $2, $3, $4, 40, now(), now())`,
			fmt.Sprintf("m%d", i), fmt.Sprintf("p%d", i), names[i%len(names)]+map[bool]string{true: "-2", false: ""}[i >= len(names)], 40-i*2); err != nil {
			t.Fatal(err)
		}
	}
	return f, g
}

func lighthouse() []stroke {
	s := func(c string, w int, pts ...int) stroke {
		st := stroke{Color: c, Width: w}
		for i := 0; i+1 < len(pts); i += 2 {
			st.Points = append(st.Points, [2]int{pts[i], pts[i+1]})
		}
		return st
	}
	return []stroke{
		s("blue", 3, 0, 86, 8, 83, 16, 86, 24, 83, 32, 86, 40, 83, 48, 86, 56, 83, 64, 86, 72, 83, 80, 86, 88, 83, 96, 86, 100, 85),
		s("brown", 4, 26, 95, 34, 86, 66, 86, 74, 95),
		s("black", 3, 39, 86, 43, 36, 57, 36, 61, 86, 39, 86),
		s("red", 5, 41, 72, 59, 72),
		s("red", 5, 42, 56, 58, 56),
		s("black", 2, 43, 36, 43, 26, 57, 26, 57, 36),
		s("red", 3, 40, 26, 50, 16, 60, 26, 40, 26),
		s("yellow", 3, 57, 29, 88, 20),
		s("yellow", 3, 57, 33, 88, 42),
		s("yellow", 3, 43, 29, 12, 20),
		s("yellow", 3, 43, 33, 12, 42),
		s("gray", 2, 80, 8, 84, 6, 88, 8, 89, 12, 87, 15, 83, 15, 81, 12, 80, 8),
	}
}

func TestLayout(t *testing.T) {
	port := layouttest.Chrome(t)
	f, g := layoutGame(t)
	srv := httptest.NewServer(f.a.Handler())
	t.Cleanup(srv.Close)
	t.Cleanup(srv.CloseClientConnections)
	var guesser, drawer *player
	for _, p := range g.Players {
		if p.Key() == g.Drawer {
			drawer = p
		} else if guesser == nil && p.House == "" {
			guesser = p
		}
	}
	watch := func(p *player) string {
		u, err := f.a.watchURL(bg, g, p)
		if err != nil {
			t.Fatal(err)
		}
		return srv.URL + u[strings.Index(u, "/watch/"):]
	}
	pages := []struct {
		name, url string
		live      bool
	}{
		{"landing", srv.URL + "/", false},
		{"game", srv.URL + "/games/" + g.ID, true},
		{"watch-guesser", watch(guesser), true},
		{"watch-drawer", watch(drawer), true},
		{"leaderboard", srv.URL + "/leaderboard", false},
		{"matches", srv.URL + "/games", false},
	}
	// The two screenshot sizes first (while the round's clock reads well),
	// then the rest; the live pages at every width in between too.
	viewports := layouttest.Viewports
	var sweep []layouttest.Viewport
	for _, w := range []int{760, 900, 1100, 1366, 1600, 2200} {
		sweep = append(sweep, layouttest.Viewport{W: w, H: 900, DPR: 1})
	}
	shots := os.Getenv("SKETCH_LAYOUT_SHOTS")
	var failures []string
	type run struct {
		page int
		vp   layouttest.Viewport
	}
	var runs []run
	for _, first := range []bool{true, false} {
		for i, pg := range pages {
			list := viewports[2:]
			if first {
				list = viewports[:2]
			} else if pg.live {
				list = append(list, sweep...)
			}
			for _, vp := range list {
				runs = append(runs, run{i, vp})
			}
		}
	}
	for _, r := range runs {
		pg, vp := pages[r.page], r.vp
		{
			name := pg.name + "-" + vp.String()
			shot := ""
			if shots != "" && (vp.W == 1440 || vp.W == 390) {
				shot = filepath.Join(shots, name+".png")
			}
			// On a live page, the stream's first snapshot drawn on the
			// canvas.
			ready := ""
			if pg.live {
				ready = "!!document.querySelector('canvas.canvas')"
			}
			m, err := layouttest.Check(port, pg.url, vp, ready, measure, shot)
			if err != nil {
				failures = append(failures, name+": "+err.Error())
				continue
			}
			if m.Items < 5 {
				failures = append(failures, fmt.Sprintf("%s: only %d parts measured", name, m.Items))
			}
			for _, v := range m.Violations {
				failures = append(failures, name+": "+v)
			}
		}
	}
	if len(failures) > 0 {
		t.Fatalf("%d layout problems:\n%s", len(failures), strings.Join(failures, "\n"))
	}
}
