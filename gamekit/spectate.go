package gamekit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Spectating: anyone may watch a game, as the players in its room see it
// (Server.Spectate is what that is, game by game). The kit lists the games
// on now and just finished (Listing; /games and /api/games) and serves
// each one's public page (/games/{id}), which shows everything in its HTML,
// so it works without JavaScript (it reloads every few seconds); with it,
// spectate.js fetches /games/{id}/state every two seconds, as the
// follow-along page does. Scripts and styles come from this origin only.
//
// The public view carries only what every player in the room knows: what's
// said there, who's playing, the phase. A game's secrets (roles, codewords,
// night actions, votes' targets) wait until it's over. Spectate builds the
// view from the game alone, never from a player, so nothing in it can be
// one player's.

//go:embed spectate
var spectateFiles embed.FS

var spectatePages = template.Must(template.New("").Funcs(template.FuncMap{
	"clock": func(t time.Time) string { return t.UTC().Format("15:04:05") + " UTC" },
	"iso":   func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
}).ParseFS(spectateFiles, "spectate/pages.html"))

const (
	listedLive     = 30               // games /games lists as on now
	listedFinished = 12               // and as just finished
	finishedFor    = 30 * time.Minute // a game is listed as just finished this long
	listingFresh   = 2 * time.Second  // how old a cached listing may be
)

// Table draws a game as a table, on its public page and its follow-along
// page (Server.Table): HTML is the game's table for a view (with the
// pages' strict policy: no inline styles or scripts), shown with the
// transcript beside it; Script and Style are names of the game's own Static
// files. The kit's script fires a "gamestate" event on document with each
// view it fetches (the event's detail), every two seconds, and the game's
// script redraws the table from it. Without JavaScript, the HTML is what's
// seen, and the page reloads itself.
//
// The follow-along page of a game with a table is drawn like its public
// page, with the strict policy, and what the player knows above the table.
type Table struct {
	HTML          func(v View) (template.HTML, error)
	Script, Style string
}

// table adds a game's table, for v, to a page's data.
func (s *Server) table(data map[string]any, v View) error {
	if s.Table == nil {
		return nil
	}
	h, err := s.Table.HTML(v)
	if err != nil {
		return err
	}
	data["TableHTML"], data["TableJS"], data["TableCSS"] = h, s.StaticURL(s.Table.Script), s.StaticURL(s.Table.Style)
	return nil
}

// spectateCSP is the public pages' policy: this origin's script, styles
// and images, nothing inline.
const spectateCSP = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; " +
	"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// Listed is a game as /games lists it: who's playing and how far it's got.
type Listed struct {
	ID      string         `json:"id"`
	URL     string         `json:"url"`
	Status  string         `json:"status"`
	Players []ListedPlayer `json:"players"`
	Over    bool           `json:"over,omitempty"`
}

// ListedPlayer is a player in a listing.
type ListedPlayer struct {
	Handle string `json:"handle"`
}

// Listing is the games on now (newest first) and the ones just finished.
type Listing struct {
	Live     []Listed `json:"live"`
	Finished []Listed `json:"finished"`
}

// Listing reads the games on now and just finished (at most a couple of
// seconds old). Without Spectate (or a store), there are none.
func (s *Server) Listing(ctx context.Context) (Listing, error) {
	empty := Listing{Live: []Listed{}, Finished: []Listed{}}
	if s.Spectate == nil || s.Store == nil {
		return empty, nil
	}
	c := &s.listing
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.at.IsZero() && time.Since(c.at) < listingFresh {
		return c.l, nil
	}
	l, err := s.readListing(ctx)
	if err != nil {
		return empty, err
	}
	c.at, c.l = time.Now(), l
	return l, nil
}

type listingCache struct {
	mu sync.Mutex
	at time.Time
	l  Listing
}

func (s *Server) readListing(ctx context.Context) (Listing, error) {
	l := Listing{Live: []Listed{}, Finished: []Listed{}}
	read := func(dst *[]Listed, over bool, q string, args ...any) error {
		rows, _ := s.Store.Pool.Query(ctx, q, args...)
		states, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
		if err != nil {
			return err
		}
		for _, b := range states {
			g := s.NewState()
			if json.Unmarshal(b, g) != nil {
				continue
			}
			v, ok := s.Spectate(g)
			if !ok {
				continue
			}
			e := Listed{ID: g.base().ID, URL: "/games/" + url.PathEscape(g.base().ID), Status: v.Status, Players: []ListedPlayer{}, Over: over}
			for _, p := range v.Players {
				e.Players = append(e.Players, ListedPlayer{Handle: p.Handle})
			}
			*dst = append(*dst, e)
		}
		return nil
	}
	// On now: under way, not waiting for players (a game open while it
	// plays, with a LobbyPhase, counts once it's out of that phase).
	if err := read(&l.Live, false, `SELECT state FROM games WHERE ended_at IS NULL AND stopped IS NULL
		AND NOT (open AND ($2::text = '' OR phase = $2::text))
		ORDER BY created_at DESC LIMIT $1`, listedLive, s.LobbyPhase); err != nil {
		return l, err
	}
	err := read(&l.Finished, true, `SELECT state FROM games WHERE ended_at > $1 AND stopped IS NULL
		ORDER BY ended_at DESC LIMIT $2`, s.now().Add(-finishedFor), listedFinished)
	return l, err
}

// Public is game id as anyone may watch it: Spectate's view, and what's
// been said in its room. ErrNoGame if there's no such game, or it never
// got going.
func (s *Server) Public(ctx context.Context, id string) (View, error) {
	if s.Spectate == nil {
		return View{}, ErrNoGame
	}
	g, log, err := s.Snapshot(ctx, id)
	if err != nil {
		return View{}, err
	}
	v, ok := s.Spectate(g)
	if !ok {
		return View{}, ErrNoGame
	}
	v.Game, v.Log, v.You, v.Thinking = g.base().ID, log, nil, nil // never a player's reasoning
	for i := range v.Players {
		v.Players[i].You = false
	}
	if v.Log == nil {
		v.Log = []Entry{}
	}
	if v.Players == nil {
		v.Players = []ViewPlayer{}
	}
	return v, nil
}

// spectateAssets are the public pages' script and stylesheet (the kit's
// look, then the game's Theme), each with a hash of its content.
type spectateAssets struct {
	once            sync.Once
	css, js         []byte
	cssHash, jsHash string
}

func (s *Server) assets() *spectateAssets {
	a := &s.spectate
	a.once.Do(func() {
		base, _ := spectateFiles.ReadFile("spectate/spectate.css")
		a.css = append(append([]byte{}, base...), s.Theme.css()...)
		if s.Theme != nil {
			a.css = append(a.css, s.Theme.PublicCSS...)
		}
		a.js, _ = spectateFiles.ReadFile("spectate/spectate.js")
		hash := func(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:6]) }
		a.cssHash, a.jsHash = hash(a.css), hash(a.js)
	})
	return a
}

func (s *Server) spectateRoutes(mux *http.ServeMux) {
	if s.Spectate == nil || s.OwnPages {
		return
	}
	asset := func(path, kind string, body func() ([]byte, string)) {
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
			b, h := body()
			cache := staticCache
			if r.URL.Query().Get("v") == h {
				cache = immutableCache
			}
			w.Header().Set("Content-Type", kind)
			w.Header().Set("Cache-Control", cache)
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Write(b)
		})
	}
	asset("/spectate.css", "text/css; charset=utf-8", func() ([]byte, string) { a := s.assets(); return a.css, a.cssHash })
	asset("/spectate.js", "text/javascript; charset=utf-8", func() ([]byte, string) { a := s.assets(); return a.js, a.jsHash })

	mux.HandleFunc("GET /games", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		l, err := s.Listing(ctx)
		if err != nil {
			slog.Warn(s.Name+": listing games", "err", err)
			http.Error(w, "The games aren't available right now; try again shortly.", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		s.renderSpectate(w, http.StatusOK, "list.html", map[string]any{"L": l, "Title": "Games"})
	})
	mux.HandleFunc("GET /api/games", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		l, err := s.Listing(ctx)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if err != nil {
			slog.Warn(s.Name+": listing games", "err", err)
			w.Header().Set("Retry-After", "5")
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"error":{"code":"unavailable","message":"try again"}}`))
			return
		}
		json.NewEncoder(w).Encode(l)
	})
	mux.HandleFunc("GET /games/{id}", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		id := r.PathValue("id")
		v, err := s.Public(ctx, id)
		var stopped *StoppedError
		switch {
		case errors.Is(err, ErrNoGame):
			w.Header().Set("Cache-Control", "no-store")
			s.renderSpectate(w, http.StatusNotFound, "gone.html", map[string]any{"Title": "No such game",
				"Text": "This game is over and gone, or never was. There may be another on now."})
			return
		case errors.As(err, &stopped):
			w.Header().Set("Cache-Control", "no-store")
			s.renderSpectate(w, http.StatusNotFound, "gone.html", map[string]any{"Title": "This game had to stop", "Text": stopped.Reason})
			return
		case err != nil:
			slog.Warn(s.Name+": a game's public page", "err", err)
			http.Error(w, "Something went wrong; try again.", http.StatusServiceUnavailable)
			return
		}
		pics := map[string]string{}
		for _, p := range v.Players {
			if p.Avatar != "" {
				pics[p.Handle] = p.Avatar
			}
		}
		data := map[string]any{"V": v, "Pics": pics, "Title": "Game " + v.Game, "State": "/games/" + url.PathEscape(v.Game) + "/state"}
		if err := s.table(data, v); err != nil {
			slog.Error(s.Name+": a game's table", "err", err)
			http.Error(w, "Something went wrong.", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		s.renderSpectate(w, http.StatusOK, "game.html", data)
	})
	mux.HandleFunc("GET /games/{id}/state", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		v, err := s.Public(ctx, r.PathValue("id"))
		var stopped *StoppedError
		switch {
		case errors.Is(err, ErrNoGame):
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":"This game is over and gone."}`))
			return
		case errors.As(err, &stopped):
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "This game had to stop: " + stopped.Reason})
			return
		case err != nil:
			slog.Warn(s.Name+": a game's public state", "err", err)
			w.Header().Set("Retry-After", "5")
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"error":"Busy right now; trying again…","retry":true}`))
			return
		}
		json.NewEncoder(w).Encode(v)
	})
}

func (s *Server) renderSpectate(w http.ResponseWriter, status int, name string, data map[string]any) {
	a := s.assets()
	data["Game"] = s.Title
	data["CSS"] = "/spectate.css?v=" + a.cssHash
	data["JS"] = "/spectate.js?v=" + a.jsHash
	data["Themed"] = s.Theme != nil
	data["Rules"] = s.Rules != nil
	if s.Theme != nil && s.Theme.Icon != "" {
		data["Icon"] = s.Theme.Icon
	}
	var b bytes.Buffer
	if err := spectatePages.ExecuteTemplate(&b, name, data); err != nil {
		slog.Error(s.Name+": a public page", "page", name, "err", err)
		http.Error(w, "Something went wrong.", http.StatusInternalServerError)
		return
	}
	pageHeaders(w, spectateCSP)
	w.WriteHeader(status)
	w.Write(b.Bytes())
}
