package sketch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wakeshift/apps/gamekit"
)

// The live pages: a game's page and each watch link stream the game as it
// changes (server-sent events), and the page draws new strokes as they
// come. Each instance reads a game at most every half second however many
// are watching it.

const (
	liveFresh = 500 * time.Millisecond // how old a cached read may be
	livePoll  = 700 * time.Millisecond // how often a stream looks for changes
	liveFor   = 4 * time.Minute        // a stream ends after this (the page reconnects)
	liveBeat  = 15 * time.Second
	maxLive   = 400 // streams an instance serves at once
)

// snap is a game as its page shows it: everyone's view, plus the player's
// own on its watch page (You).
type snap struct {
	Seq     int          `json:"seq"`
	Game    string       `json:"game"`
	Phase   string       `json:"phase"`
	Status  string       `json:"status"`
	Round   int          `json:"round,omitempty"`
	Points  int          `json:"points,omitempty"` // what the round's word is worth
	Window  int          `json:"window"`           // the rounds scores count
	Drawer  string       `json:"drawer,omitempty"`
	EndsAt  int64        `json:"ends_at,omitempty"` // the round's end, or while Waiting the drawer's deadline to start (Unix ms)
	Waiting bool         `json:"waiting,omitempty"` // the round waits for the drawer's first drawing: its clock hasn't started
	LeftMs  int64        `json:"left_ms,omitempty"` // and how long until then, as it's sent
	Hint    string       `json:"hint,omitempty"`
	Letters string       `json:"letters,omitempty"` // "10 letters", once the first hint is out
	Word    string       `json:"word,omitempty"`    // once the round's over, or for its drawer's owner
	Outcome string       `json:"outcome,omitempty"`
	Strokes []stroke     `json:"strokes"`
	Guesses []snapGuess  `json:"guesses"`
	Players []snapPlayer `json:"players"`
	Result  []string     `json:"result,omitempty"`
	You     *snapYou     `json:"you,omitempty"`
	// Thinking is the watched player's own private reasoning, newest
	// first: set on its watch page only, never the public one.
	Thinking []gamekit.Thought `json:"thinking,omitempty"`
}

// version is what a stream compares to know a snapshot is new: the game's
// changes, and on a watch page its player's latest reasoning.
func (s snap) version() string {
	if len(s.Thinking) == 0 {
		return strconv.Itoa(s.Seq)
	}
	return strconv.Itoa(s.Seq) + "/" + strconv.Itoa(len(s.Thinking)) + "/" + s.Thinking[0].At.Format(time.RFC3339Nano)
}

type snapGuess struct {
	By    string `json:"by"`
	Text  string `json:"text,omitempty"`
	Right bool   `json:"right,omitempty"`
	Owner bool   `json:"owner,omitempty"`
}

type snapPlayer struct {
	Name    string `json:"name"`
	Score   int    `json:"score"`
	Drawing bool   `json:"drawing,omitempty"`
	Left    bool   `json:"left,omitempty"`
	You     bool   `json:"you,omitempty"`
	Got     bool   `json:"got,omitempty"` // guessed the word in the round just over
}

type snapYou struct {
	Name     string `json:"name"`
	Drawing  bool   `json:"drawing,omitempty"`
	CanGuess bool   `json:"can_guess,omitempty"`
	Left     bool   `json:"left,omitempty"`
}

// snapshot is g as its page shows it; me is the watch link's player (nil:
// the public page). The word shows only once the round is over, or to the
// drawer's owner.
func snapshot(g *game, me *player) snap {
	s := snap{Seq: g.Seq, Game: g.ID, Phase: g.Phase, Window: scoreWindow, Strokes: g.Strokes, Guesses: []snapGuess{}, Players: []snapPlayer{}}
	if s.Strokes == nil {
		s.Strokes = []stroke{}
	}
	d := g.drawer()
	switch g.Phase {
	case waiting:
		s.Status = fmt.Sprintf("Waiting for players · %d here, %d to play", len(g.active()), g.playing())
	case drawing:
		s.Round, s.Points = g.Round, tierPoints(g.Tier)
		s.Status = fmt.Sprintf("Round %d · %s is drawing · %s", s.Round, display(d.Handle), plural(s.Points, "point", "points"))
		if g.waitingForDrawer() {
			s.Waiting = true
			s.Status = fmt.Sprintf("Round %d · waiting for %s to start drawing · %s", s.Round, display(d.Handle), plural(s.Points, "point", "points"))
		}
		s.EndsAt = g.RoundEnd.UnixMilli()
		s.Hint = g.hint()
	case reveal:
		s.Round = g.Round
		s.Status = fmt.Sprintf("Round %d · over · next round soon", s.Round)
		s.Word, s.Outcome = g.Word, g.Outcome
	case over:
		s.Status = "Room closed"
		s.Result = g.Result
	}
	if d != nil && g.Phase != over {
		s.Drawer = display(d.Handle)
	}
	// A right guess shows its text, as typed, beside a "got it" chip: the
	// first right guess ends the round, so by then everyone sees the word.
	// Should one ever be there while the round's still on, its text isn't.
	var got string
	for _, x := range g.Guesses {
		sg := snapGuess{By: display(x.By), Right: x.Right, Owner: x.Owner}
		if !x.Right || g.Phase == reveal {
			sg.Text = x.Text
		}
		if x.Right && g.Phase == reveal {
			got = x.By
		}
		s.Guesses = append(s.Guesses, sg)
	}
	if len(s.Guesses) > 40 {
		s.Guesses = s.Guesses[len(s.Guesses)-40:]
	}
	slices.Reverse(s.Guesses) // newest first
	if g.Phase == drawing && g.Hints > 0 {
		s.Letters = g.letterCount()
	}
	for _, p := range g.standings() {
		s.Players = append(s.Players, snapPlayer{Name: display(p.Handle), Score: p.Score,
			Drawing: g.isDrawing(p), Left: p.Left, You: p == me, Got: got != "" && p.Handle == got})
	}
	if me != nil {
		y := &snapYou{Name: display(me.Handle), Drawing: g.isDrawing(me), Left: me.Left}
		y.CanGuess = g.Phase == drawing && !y.Drawing && !me.Left
		if y.Drawing {
			s.Word = g.Word
		}
		s.You = y
	}
	return s
}

// liveCache is the games the live pages are showing, read at most every
// liveFresh, with each one's latest canvas image.
type liveCache struct {
	mu    sync.Mutex
	games map[string]*cached
}

type cached struct {
	mu     sync.Mutex
	at     time.Time
	read   atomic.Int64 // at, as Unix nanoseconds, for the cache's sweep (which doesn't take mu)
	g      *game
	err    error
	png    []byte
	pngKey pngKey
}

// pngKey is which drawing a cached image is of: its marks' hash, so an
// image is never reused for a drawing that differs in any way.
type pngKey struct {
	marks int
	sum   uint64
}

func keyOf(g *game) pngKey {
	b, _ := json.Marshal(g.Strokes)
	h := fnv.New64a()
	h.Write(b)
	return pngKey{len(g.Strokes), h.Sum64()}
}

// cached is game id's cache entry and the game in it now, read again if
// it's older than liveFresh. The game is taken under the entry's lock: a
// later read replaces it, never changes it.
func (a *App) cached(ctx context.Context, id string) (*cached, *game, error) {
	c := &a.live
	c.mu.Lock()
	if c.games == nil {
		c.games = map[string]*cached{}
	}
	e := c.games[id]
	if e == nil {
		if len(c.games) > 2000 {
			for k, v := range c.games {
				if time.Since(time.Unix(0, v.read.Load())) > time.Minute {
					delete(c.games, k)
				}
			}
		}
		e = &cached{}
		c.games[id] = e
	}
	c.mu.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	if time.Since(e.at) > liveFresh {
		gs, err := a.srv.Load(ctx, id)
		e.at, e.err, e.g = time.Now(), err, nil
		e.read.Store(e.at.UnixNano())
		if err == nil {
			e.g = gs.(*game)
		}
	}
	if e.err != nil {
		return nil, nil, e.err
	}
	return e, e.g, nil
}

// liveGame is game id now (from the cache).
func (a *App) liveGame(ctx context.Context, id string) (*game, error) {
	_, g, err := a.cached(ctx, id)
	return g, err
}

// canvasPNG is game id's drawing as an image, rendered once per change.
func (a *App) canvasPNG(ctx context.Context, id string) ([]byte, error) {
	// The game cached returned, not e.g: another request may have read the
	// entry again since (and a failed read leaves e.g nil).
	e, g, err := a.cached(ctx, id)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if k := keyOf(g); e.png == nil || e.pngKey != k {
		e.png, e.pngKey = renderPNG(g.Strokes, renderSize), k
	}
	return e.png, nil
}

// renderCached is g's drawing as an image (the canvas tool's), rendered
// once per change whichever of the canvas tool and the pages asks first:
// every guesser looks at the same drawing.
func (a *App) renderCached(g *game) []byte {
	c := &a.live
	c.mu.Lock()
	e := c.games[g.ID]
	c.mu.Unlock()
	k := keyOf(g)
	if e != nil {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.png != nil && e.pngKey == k {
			return e.png
		}
	}
	png := renderPNG(g.Strokes, renderSize)
	if e != nil {
		e.png, e.pngKey = png, k
	}
	return png
}

type connCount struct{ n atomic.Int64 }

// stream streams a game's snapshots to a page as they change: view is
// what one viewer sees (nil game: it's gone).
func (a *App) stream(w http.ResponseWriter, r *http.Request, id string, view func(g *game) (snap, bool)) {
	if a.conns.n.Add(1) > maxLive {
		a.conns.n.Add(-1)
		w.Header().Set("Retry-After", "10")
		http.Error(w, "Too many watching right now; try again shortly.", http.StatusServiceUnavailable)
		return
	}
	defer a.conns.n.Add(-1)
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming isn't supported", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	h.Set("X-Content-Type-Options", "nosniff")
	ctx := r.Context()
	end := time.After(liveFor)
	tick := time.NewTicker(livePoll)
	defer tick.Stop()
	last, beat := "", time.Now()
	fmt.Fprint(w, "retry: 2000\n\n")
	for {
		g, err := a.liveGame(ctx, id)
		var stopped *gamekit.StoppedError
		switch {
		case errors.Is(err, gamekit.ErrNoGame) || errors.As(err, &stopped):
			fmt.Fprint(w, "event: gone\ndata: {}\n\n")
			fl.Flush()
			return
		case err != nil:
			if ctx.Err() != nil {
				return
			}
		default:
			s, ok := view(g)
			if !ok {
				fmt.Fprint(w, "event: gone\ndata: {}\n\n")
				fl.Flush()
				return
			}
			if v := s.version(); v != last {
				last, beat = v, time.Now()
				s.LeftMs = leftMs(s.EndsAt)
				b, _ := json.Marshal(s)
				fmt.Fprintf(w, "event: snap\ndata: %s\n\n", b)
				fl.Flush()
			} else if time.Since(beat) > liveBeat {
				beat = time.Now()
				fmt.Fprint(w, ": beat\n\n")
				fl.Flush()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-end:
			return
		case <-tick.C:
		}
	}
}

// leftMs is the milliseconds until endsAt (Unix ms; 0: none).
func leftMs(endsAt int64) int64 {
	if endsAt == 0 {
		return 0
	}
	return max(1, endsAt-time.Now().UnixMilli())
}
