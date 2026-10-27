package sketch

import (
	"errors"
	"fmt"
	"image/color"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/wakeshift/apps/gamekit"
)

// Phases. A room plays round after round with no end: it waits for
// players before its first round and whenever too few are left, and it's
// over only once it closes (nobody came, or came back, in time).
const (
	waiting = "waiting for players"
	drawing = "drawing"
	reveal  = "reveal" // between rounds: the word's out
	over    = "over"   // the room has closed
)

const (
	// minPlayers start a new room's first round; keepPlayers (a drawer and
	// someone to guess) keep a room playing, and bring a waiting one back.
	minPlayers  = 3
	maxPlayers  = 8
	keepPlayers = 2
	// scoreWindow is the rounds a room's scores count: the last 20 it
	// played. Older rounds' points drop off.
	scoreWindow = 20

	canvasSize      = 100 // the canvas is 100×100
	maxBatch        = 10  // strokes a draw call adds, at most
	maxPoints       = 32  // points in a stroke, at most
	maxRoundStrokes = 200 // strokes in a round, at most
	maxWidth        = 6
	awayRounds      = 2  // rounds in a row a player may be away before it's taken out
	maxGuesses      = 30 // guesses a player makes in a round, at most
	guessGap        = 2 * time.Second
	recentWords     = 150 // words a player won't see again soon
	roomWords       = 250 // words a room won't have again soon

	// A guesser that does nothing in a round whose drawing was up this
	// long was away for it (rounds.go, checkAway).
	respondWithin = 20 * time.Second
	// A room that's played closes once nobody in it has done anything for
	// idleClose and none is otherwise known to be there (deserted).
	idleClose = 5 * time.Minute
	// How often a room waiting for players checks that someone in it is
	// still there.
	waitCheck = time.Minute
)

// palette is the colors a stroke may have.
var palette = map[string]color.NRGBA{
	"black":  {0x1d, 0x1d, 0x1f, 0xff},
	"gray":   {0x8d, 0x99, 0xae, 0xff},
	"white":  {0xff, 0xff, 0xff, 0xff},
	"red":    {0xe5, 0x38, 0x3b, 0xff},
	"orange": {0xf7, 0x7f, 0x00, 0xff},
	"yellow": {0xfc, 0xbf, 0x49, 0xff},
	"green":  {0x2a, 0x9d, 0x55, 0xff},
	"blue":   {0x26, 0x67, 0xff, 0xff},
	"purple": {0x7b, 0x2c, 0xbf, 0xff},
	"pink":   {0xff, 0x70, 0xa6, 0xff},
	"brown":  {0x8d, 0x55, 0x24, 0xff},
}

// colors is the palette's names, in the order the tool lists them.
var colors = []string{"black", "gray", "white", "red", "orange", "yellow", "green", "blue", "purple", "pink", "brown"}

// stroke is a mark on the canvas, in the order drawn: a line or polyline
// from the draw tool's strokes (points (x, y) from 0 to 100), or a shape
// from its svg (Shape: svg.go's flattening, never the agent's markup).
type stroke struct {
	Points [][2]int `json:"p,omitempty"`
	Color  string   `json:"c,omitempty"`
	Width  int      `json:"w,omitempty"`
	Shape  *shape   `json:"sh,omitempty"`
}

// marks counts a drawing's strokes and shapes, and its shapes' points.
func marks(ms []stroke) (strokes, shapes, points int) {
	for _, m := range ms {
		if m.Shape != nil {
			shapes++
			points += m.Shape.points()
		} else {
			strokes++
		}
	}
	return
}

// marksSaid is a drawing's marks said: "3 strokes", "12 shapes", "2 strokes and 5 shapes".
func marksSaid(ms []stroke) string {
	st, sh, _ := marks(ms)
	switch {
	case sh == 0:
		return plural(st, "stroke", "strokes")
	case st == 0:
		return plural(sh, "shape", "shapes")
	}
	return plural(st, "stroke", "strokes") + " and " + plural(sh, "shape", "shapes")
}

type game struct {
	gamekit.Game
	Players []*player `json:"players"`
	// Order is the drawers' rotation (player keys): the players in the
	// room, in the order they draw; a joiner goes at the end.
	Order []string `json:"order,omitempty"`
	Turn  int      `json:"turn"`  // the last drawer's place in Order
	Round int      `json:"round"` // rounds started, 1 for the first
	// The round being drawn (or just revealed).
	Word   string `json:"word,omitempty"`
	Tier   string `json:"tier,omitempty"`
	Drawer string `json:"drawer,omitempty"` // a player key
	// RoundStart is when the round started, DrawnAt its first drawing: the
	// round's clock starts there. RoundEnd is when the round ends: the
	// drawer's deadline to start (RoundStart + grace) until it draws, then
	// DrawnAt + the round's length.
	RoundStart time.Time `json:"round_start,omitzero"`
	DrawnAt    time.Time `json:"drawn_at,omitzero"`
	RoundEnd   time.Time `json:"round_end,omitzero"`
	Strokes    []stroke  `json:"strokes,omitempty"`
	Drawn      bool      `json:"drawn,omitempty"`
	Hints      int       `json:"hints,omitempty"` // hints given: the length, then the first letter
	// Strokes are announced to the guessers at most every coalesce:
	// Announced of them so far, the last time at LastFlush, the next at
	// FlushAt (zero: none waiting).
	Announced int       `json:"announced,omitempty"`
	LastFlush time.Time `json:"last_flush,omitzero"`
	FlushAt   time.Time `json:"flush_at,omitzero"`
	Guesses   []guess   `json:"guesses,omitempty"`
	// Shown is when the round's first strokes reached the guessers, and
	// RoundOver when the round ended: what a guesser had to answer.
	Shown     time.Time `json:"shown,omitzero"`
	RoundOver time.Time `json:"round_over,omitzero"`
	Outcome   string    `json:"outcome,omitempty"` // how the round ended, once it has
	// The round's draw calls and hints, when they came, for its archive
	// (archive.go).
	Draws     []drawCallAt `json:"draws,omitempty"`
	HintTimes []time.Time  `json:"hint_times,omitempty"`
	Rounds    []summary    `json:"rounds,omitempty"` // the room's last scoreWindow rounds, oldest first
	Recent    []string     `json:"recent,omitempty"` // the room's last roomWords words, newest first
	Result    []string     `json:"result,omitempty"` // why the room closed, once it has
	// Until is when a room waiting for players closes if too few are
	// still here (zero: at its deadline, as a new room's lobby does).
	Until time.Time `json:"until,omitzero"`
	// Seq counts changes, for the live pages.
	Seq int `json:"seq"`
}

type player struct {
	gamekit.Player
	// Score is its points from the room's last scoreWindow rounds.
	Score  int  `json:"score"`
	Missed int  `json:"missed,omitempty"` // rounds in a row it's been away (checkAway)
	Left   bool `json:"left,omitempty"`   // it left, or was taken out
	// Guesses this round, and when it last guessed.
	Guessed   int       `json:"guessed,omitempty"`
	LastGuess time.Time `json:"last_guess,omitzero"`
}

// drawCallAt is a draw call: when, and how many marks it added.
type drawCallAt struct {
	At    time.Time `json:"at"`
	Marks int       `json:"marks"`
}

// guess is a guess (a right one is shown without its text).
type guess struct {
	By    string    `json:"by"` // handle
	Text  string    `json:"text,omitempty"`
	Right bool      `json:"right,omitempty"`
	Owner bool      `json:"owner,omitempty"` // typed by the player's owner on its watch page
	At    time.Time `json:"at"`
}

// summary is a finished round: who drew it, who guessed it, and the
// points they got.
type summary struct {
	Round   int    `json:"round"`
	Drawer  string `json:"drawer"` // handle
	Word    string `json:"word"`
	Winner  string `json:"winner,omitempty"` // handle
	Outcome string `json:"outcome"`
	Tier    string `json:"tier,omitempty"`
	// Points went to the winner and to the drawer, if someone guessed it
	// (after Seconds). The keys find them, as handles may change when a
	// player leaves and comes back; they're never shown.
	Points    int    `json:"points,omitempty"`
	Seconds   int    `json:"seconds,omitempty"`
	DrawerKey string `json:"drawer_key,omitempty"`
	WinnerKey string `json:"winner_key,omitempty"`
}

func (g *game) player(key string) *player {
	for _, p := range g.Players {
		if p.Key() == key {
			return p
		}
	}
	return nil
}

// active is the players in the room.
func (g *game) active() []*player {
	var out []*player
	for _, p := range g.Players {
		if !p.Left {
			out = append(out, p)
		}
	}
	return out
}

func (g *game) handles() []string {
	var out []string
	for _, p := range g.active() {
		out = append(out, p.Handle)
	}
	return out
}

func (g *game) drawer() *player {
	if g.Drawer == "" {
		return nil
	}
	return g.player(g.Drawer)
}

// clockStart is when the round's clock started: its first drawing (a
// round started before DrawnAt was kept: its start).
func (g *game) clockStart() time.Time {
	if !g.DrawnAt.IsZero() {
		return g.DrawnAt
	}
	return g.RoundStart
}

// waitingForDrawer reports whether the round is waiting for its drawer's
// first drawing: the clock hasn't started.
func (g *game) waitingForDrawer() bool { return g.Phase == drawing && !g.Drawn }

// isDrawing reports whether p is drawing the round under way.
func (g *game) isDrawing(p *player) bool {
	return g.Phase == drawing && p != nil && p.Key() == g.Drawer
}

func (g *game) currentWord() word {
	for _, w := range words {
		if w.Word == g.Word {
			return w
		}
	}
	return word{Word: g.Word}
}

// hint is the word as guessers see it now: blanks for its letters after
// the first hint, the first letter shown after the second; "" before.
func (g *game) hint() string {
	if g.Hints == 0 || g.Word == "" {
		return ""
	}
	var parts []string
	first := true
	for _, w := range strings.Fields(g.Word) {
		var letters []string
		for _, r := range w {
			if first && g.Hints >= 2 {
				letters = append(letters, strings.ToUpper(string(r)))
			} else if r == '-' {
				letters = append(letters, "-")
			} else {
				letters = append(letters, "_")
			}
			first = false
		}
		parts = append(parts, strings.Join(letters, " "))
	}
	return strings.Join(parts, "   ")
}

// letterCount is the word's letters, said: "6 letters", "3 + 6 letters".
func (g *game) letterCount() string {
	var n []string
	for _, w := range strings.Fields(g.Word) {
		n = append(n, fmt.Sprint(len(strings.ReplaceAll(w, "-", ""))))
	}
	return strings.Join(n, " + ") + " letters"
}

// strokeText is a drawing's marks as an agent reads them, one a line,
// numbered from first (1 for a round's first): a stroke with its points,
// "#3 blue w2: 10,20 30,40"; a shape (from svg) by its colors only, "#4
// shape: fill red, outline black w1.5", as its points would run to
// hundreds: the image shows it.
func strokeText(strokes []stroke, first int) string {
	var b strings.Builder
	for i, s := range strokes {
		if sh := s.Shape; sh != nil {
			fmt.Fprintf(&b, "#%d shape:", first+i)
			var parts []string
			if sh.Fill != "" {
				parts = append(parts, " fill "+sh.Fill+alphaSaid(sh.FillAlpha))
			}
			if sh.Color != "" {
				parts = append(parts, fmt.Sprintf(" outline %s w%s%s", sh.Color, strconv.FormatFloat(sh.Width, 'f', -1, 64), alphaSaid(sh.StrokeAlpha)))
			}
			b.WriteString(strings.Join(parts, ","))
			b.WriteByte('\n')
			continue
		}
		fmt.Fprintf(&b, "#%d %s w%d:", first+i, s.Color, s.Width)
		for _, p := range s.Points {
			fmt.Fprintf(&b, " %d,%d", p[0], p[1])
		}
		b.WriteByte('\n')
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// alphaSaid is a shape's fill or outline opacity, said if it isn't opaque.
func alphaSaid(a float64) string {
	if a <= 0 || a >= 1 {
		return ""
	}
	return fmt.Sprintf(" (%d%% opaque)", int(math.Round(a*100)))
}

// strokeIn is a stroke as the draw tool takes it.
type strokeIn struct {
	Points [][]float64 `json:"points" jsonschema:"The stroke's points, [x, y] from 0 to 100 ([0, 0] is the top left): 2 for a straight line, up to 32 for a polyline (1 for a dot)"`
	Color  string      `json:"color,omitempty" jsonschema:"black (default), gray, white, red, orange, yellow, green, blue, purple, pink or brown"`
	Width  int         `json:"width,omitempty" jsonschema:"The line's width, 1 to 6 (default 2)"`
}

// checkStrokes checks a draw call's strokes and makes them canvas strokes.
func checkStrokes(in []strokeIn) ([]stroke, error) {
	switch {
	case len(in) == 0:
		return nil, errors.New("strokes is required: at least one")
	case len(in) > maxBatch:
		return nil, fmt.Errorf("at most %d strokes a call; send the rest in another call", maxBatch)
	}
	out := make([]stroke, 0, len(in))
	for i, s := range in {
		n := i + 1
		if len(s.Points) == 0 || len(s.Points) > maxPoints {
			return nil, fmt.Errorf("stroke %d: 1 to %d points", n, maxPoints)
		}
		c := strings.ToLower(strings.TrimSpace(s.Color))
		if c == "" {
			c = "black"
		}
		if _, ok := palette[c]; !ok {
			return nil, fmt.Errorf("stroke %d: no color %q; the colors are %s", n, s.Color, strings.Join(colors, ", "))
		}
		w := s.Width
		if w == 0 {
			w = 2
		}
		if w < 1 || w > maxWidth {
			return nil, fmt.Errorf("stroke %d: width is 1 to %d", n, maxWidth)
		}
		st := stroke{Color: c, Width: w}
		for j, p := range s.Points {
			if len(p) != 2 {
				return nil, fmt.Errorf("stroke %d, point %d: a point is [x, y]", n, j+1)
			}
			var xy [2]int
			for k, v := range p {
				if math.IsNaN(v) || v < 0 || v > canvasSize {
					return nil, fmt.Errorf("stroke %d, point %d: x and y are 0 to %d", n, j+1, canvasSize)
				}
				xy[k] = int(math.Round(v))
			}
			st.Points = append(st.Points, xy)
		}
		out = append(out, st)
	}
	return out, nil
}

// nextDrawer is the place in Order of the next player to draw: the first
// still in the room after the last drawer, going round; -1 if none is.
func (g *game) nextDrawer() int {
	n := len(g.Order)
	for i := 1; i <= n; i++ {
		at := ((g.Turn+i)%n + n) % n
		if p := g.player(g.Order[at]); p != nil && !p.Left {
			return at
		}
	}
	return -1
}

// prune forgets the players who've left: out of the rotation and off the
// scoreboard (their points stay in the rounds, should they come back).
// Anyone in the room the rotation lacks joins its end.
func (g *game) prune() {
	var keep []string
	turn := g.Turn
	for i, k := range g.Order {
		if p := g.player(k); p != nil && !p.Left && !slices.Contains(keep, k) {
			keep = append(keep, k)
		} else if i <= g.Turn {
			turn--
		}
	}
	g.Players = slices.DeleteFunc(g.Players, func(p *player) bool { return p.Left })
	for _, p := range g.Players {
		if !slices.Contains(keep, p.Key()) {
			keep = append(keep, p.Key())
		}
	}
	g.Order, g.Turn = keep, turn
}

// seat puts a player who's joined in the room: at the end of the rotation,
// or back in its old place if it left and has come back.
func (g *game) seat(p gamekit.Player) {
	if old := g.player(p.Key()); old != nil {
		old.Player, old.Left, old.Missed = p, false, 0
	} else {
		g.Players = append(g.Players, &player{Player: p})
	}
	if !slices.Contains(g.Order, p.Key()) {
		g.Order = append(g.Order, p.Key())
	}
	g.rescore()
}

// playing is how many players the room needs to play: a new room's first
// round waits for minPlayers, after that keepPlayers will do.
func (g *game) playing() int {
	if g.Round == 0 {
		return minPlayers
	}
	return keepPlayers
}

// tierPoints is what a right guess is worth, to the guesser and to the
// drawer: harder words, more points.
func tierPoints(tier string) int {
	switch tier {
	case medium:
		return 2
	case hard:
		return 3
	}
	return 1
}

// rescore works out each player's score: its points from the room's last
// scoreWindow rounds (only those are kept).
func (g *game) rescore() {
	for _, p := range g.Players {
		p.Score = 0
	}
	for _, r := range g.Rounds {
		if r.Points == 0 {
			continue
		}
		for _, k := range []string{r.WinnerKey, r.DrawerKey} {
			if p := g.player(k); k != "" && p != nil {
				p.Score += r.Points
			}
		}
	}
}

// standings are the players by score, best first.
func (g *game) standings() []*player {
	ps := slices.Clone(g.Players)
	slices.SortStableFunc(ps, func(a, b *player) int { return b.Score - a.Score })
	return ps
}
