package sketch

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/apps/agentpicker"
	"github.com/wakeshift/apps/gamekit"
	"github.com/wakeshift/apps/legal"
)

// The pages: the landing page, each game's page (anyone may watch), a
// player's watch link (watch.go), and the leaderboard. Everything a page
// shows is in its HTML, so it works without JavaScript (it reloads every
// few seconds); with it, the page goes live (static/sketch.js). Scripts,
// styles and fonts come from this origin only.

//go:embed static
var staticDir embed.FS

var staticFiles, _ = fs.Sub(staticDir, "static")

//go:embed templates/*.html
var templateFiles embed.FS

var pages = template.Must(template.New("").Funcs(template.FuncMap{
	"display": display,
	"leftms":  leftMs,
	"clock": func(ms int64) string {
		if ms == 0 {
			return ""
		}
		left := int(max(0, time.Until(time.UnixMilli(ms)).Round(time.Second)).Seconds())
		return fmt.Sprintf("%d:%02d", left/60, left%60)
	},
}).ParseFS(templateFiles, "templates/*.html"))

// pageCSP is the pages' policy: this origin's scripts, styles, fonts and
// images, nothing inline, and forms posting only here.
const pageCSP = "default-src 'none'; script-src 'self'; style-src 'self'; font-src 'self'; img-src 'self'; connect-src 'self'; " +
	"form-action 'self'; base-uri 'none'; frame-ancestors 'none'"

// landingCSP is the landing page's: the same, with no forms (the agent
// picker's script and styles are this origin's too).
const landingCSP = "default-src 'none'; script-src 'self'; style-src 'self'; font-src 'self'; img-src 'self'; " +
	"form-action 'none'; base-uri 'none'; frame-ancestors 'none'"

func headers(w http.ResponseWriter, csp string) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", csp)
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
}

func (a *App) render(w http.ResponseWriter, status int, csp, name string, data map[string]any) {
	data["CSS"] = a.srv.StaticURL("sketch.css")
	data["JS"] = a.srv.StaticURL("sketch.js")
	data["Logo"] = a.srv.StaticURL("logo.svg")
	data["Icon"] = a.srv.StaticURL("logo-small.svg")
	data["Icon32"] = a.srv.StaticURL("favicon-32.png")
	data["Icon16"] = a.srv.StaticURL("favicon-16.png")
	data["Touch"] = a.srv.StaticURL("apple-touch-icon.png")
	data["Wakeshift"] = legal.Wakeshift
	var b bytes.Buffer
	if err := pages.ExecuteTemplate(&b, name, data); err != nil {
		slog.Error("sketch: a page", "page", name, "err", err)
		http.Error(w, "Something went wrong.", http.StatusInternalServerError)
		return
	}
	headers(w, csp)
	w.WriteHeader(status)
	w.Write(b.Bytes())
}

func (a *App) notFound(w http.ResponseWriter, title, text string) {
	w.Header().Set("Cache-Control", "no-store")
	a.render(w, http.StatusNotFound, pageCSP, "gone.html", map[string]any{"Title": title, "Text": text})
}

func (a *App) pageRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /games/{id}", func(w http.ResponseWriter, r *http.Request) {
		g, err := a.liveGame(r.Context(), r.PathValue("id"))
		if err != nil {
			a.gameGone(w, err)
			return
		}
		s := snapshot(g, nil)
		w.Header().Set("Cache-Control", "no-store")
		a.render(w, http.StatusOK, pageCSP, "game.html", map[string]any{"S": s, "Title": "Game " + g.ID,
			"Live": "/games/" + g.ID + "/live", "Canvas": canvasURL(g)})
	})
	mux.HandleFunc("GET /games/{id}/live", func(w http.ResponseWriter, r *http.Request) {
		a.stream(w, r, r.PathValue("id"), func(g *game) (snap, bool) { return snapshot(g, nil), true })
	})
	mux.HandleFunc("GET /games/{id}/canvas.png", func(w http.ResponseWriter, r *http.Request) {
		b, err := a.canvasPNG(r.Context(), r.PathValue("id"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Write(b)
	})

	mux.HandleFunc("GET /watch/{secret}", func(w http.ResponseWriter, r *http.Request) {
		g, me, err := a.watched(r.Context(), r.PathValue("secret"))
		if err != nil {
			a.watchGone(w, err)
			return
		}
		secret := r.PathValue("secret")
		s := snapshot(g, me)
		s.Thinking = a.thinking(r.Context(), g.ID, me.Key())
		w.Header().Set("Cache-Control", "no-store")
		a.render(w, http.StatusOK, pageCSP, "game.html", map[string]any{"S": s, "Title": display(me.Handle) + " · Game " + g.ID, "Watch": true,
			"Live": "/watch/" + secret + "/live", "GuessURL": "/watch/" + secret + "/guess", "Canvas": canvasURL(g),
			"Note": guessNote(r.URL.Query().Get("g"))})
	})
	mux.HandleFunc("GET /watch/{secret}/live", func(w http.ResponseWriter, r *http.Request) {
		id, key, err := a.watcher(r.Context(), r.PathValue("secret"))
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// The player's reasoning is read again when the game changes (draw
		// and guess keep it in the move's own transaction, which changes the
		// game), and every few seconds besides; either way the page gets it
		// as soon as it's read (snap.version).
		seq, read, thinking := -1, time.Time{}, []gamekit.Thought(nil)
		a.stream(w, r, id, func(g *game) (snap, bool) {
			me := g.player(key)
			if me == nil || me.Left || g.Phase == over {
				return snap{}, false
			}
			if g.Seq != seq || time.Since(read) > thinkingRefresh {
				seq, read, thinking = g.Seq, time.Now(), a.thinking(r.Context(), id, key)
			}
			s := snapshot(g, me)
			s.Thinking = thinking
			return s, true
		})
	})
	mux.HandleFunc("POST /watch/{secret}/guess", a.ownerGuess)

	mux.HandleFunc("GET /games", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		w.Header().Set("Cache-Control", "no-store")
		a.render(w, http.StatusOK, pageCSP, "games.html", map[string]any{"Title": "Rooms", "L": a.games(ctx)})
	})
	mux.HandleFunc("GET /leaderboard", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		rows, err := a.leaderboard(ctx, 50)
		if err != nil {
			slog.Warn("sketch: reading the leaderboard", "err", err)
			http.Error(w, "The leaderboard isn't available right now; try again shortly.", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=30")
		a.render(w, http.StatusOK, pageCSP, "leaderboard.html", map[string]any{"Title": "Leaderboard", "Rows": rows, "Window": scoreWindow})
	})
}

const (
	thinkingShown   = 12              // how much of its player's reasoning a watch page shows
	thinkingRefresh = 5 * time.Second // how often a watch stream reads it again without a change
)

// thinking is a player's own private reasoning in game, newest first, for
// its watch page only; none if it can't be read just now.
func (a *App) thinking(ctx context.Context, game, player string) []gamekit.Thought {
	th, err := a.srv.Thinking(ctx, game, player)
	if err != nil {
		slog.Warn("sketch: a player's reasoning for its watch page", "err", err)
		return nil
	}
	th = th[max(0, len(th)-thinkingShown):]
	slices.Reverse(th)
	return th
}

// canvasURL is a game's drawing as an image, as it is now (for pages
// without JavaScript).
func canvasURL(g *game) string {
	return "/games/" + url.PathEscape(g.ID) + "/canvas.png?v=" + strconv.Itoa(g.Seq)
}

// watched is a watch link's game and player, if the link still works.
func (a *App) watched(ctx context.Context, secret string) (*game, *player, error) {
	id, key, err := a.watcher(ctx, secret)
	if err != nil {
		return nil, nil, err
	}
	g, err := a.liveGame(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	me := g.player(key)
	if me == nil || me.Left || g.Phase == over {
		return nil, nil, errNoWatch
	}
	return g, me, nil
}

func (a *App) gameGone(w http.ResponseWriter, err error) {
	var stopped *gamekit.StoppedError
	if errors.Is(err, gamekit.ErrNoGame) || errors.As(err, &stopped) {
		a.notFound(w, "No such game", "This room has closed, or never was.")
		return
	}
	slog.Warn("sketch: reading a game", "err", err)
	http.Error(w, "Something went wrong; try again.", http.StatusServiceUnavailable)
}

func (a *App) watchGone(w http.ResponseWriter, err error) {
	var stopped *gamekit.StoppedError
	if errors.Is(err, errNoWatch) || errors.Is(err, gamekit.ErrNoGame) || errors.As(err, &stopped) {
		a.notFound(w, "This watch link has ended", "Watch links stop working when the player leaves or its room closes. Ask your agent for its new one when it joins again.")
		return
	}
	slog.Warn("sketch: reading a watch link", "err", err)
	http.Error(w, "Something went wrong; try again.", http.StatusServiceUnavailable)
}

// guessNote is what the watch page says after a guess (without
// JavaScript, the guess's redirect says how it went).
func guessNote(code string) string {
	switch code {
	case "right":
		return "You got it! A point each for your agent and the drawer."
	case "wrong":
		return "Not it. Try again."
	case "no":
		return "That guess didn't count."
	}
	return ""
}

// ownerGuess is a guess typed on a watch page: counted for its agent,
// marked as its owner's. Only from this site's own pages.
func (a *App) ownerGuess(w http.ResponseWriter, r *http.Request) {
	secret := r.PathValue("secret")
	asJSON := strings.Contains(r.Header.Get("Accept"), "application/json")
	reply := func(status int, code, msg string) {
		if asJSON {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(status)
			json.NewEncoder(w).Encode(map[string]any{"result": code, "message": msg})
			return
		}
		if status == http.StatusNotFound {
			a.watchGone(w, errNoWatch)
			return
		}
		http.Redirect(w, r, "/watch/"+secret+"?g="+code, http.StatusSeeOther)
	}
	if !sameOrigin(r) {
		http.Error(w, "Guesses come from the watch page.", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	text := strings.Join(strings.Fields(r.FormValue("guess")), " ")
	if !validGuess(text) {
		reply(http.StatusBadRequest, "no", "A guess is letters, spaces and hyphens, up to 40 characters.")
		return
	}
	id, key, err := a.watcher(r.Context(), secret)
	if err != nil {
		reply(http.StatusNotFound, "gone", "This watch link has ended.")
		return
	}
	var right bool
	_, err = a.srv.Update(r.Context(), id, nil, func(tx *gamekit.Tx, gs gamekit.State) error {
		g := gs.(*game)
		me := g.player(key)
		if me == nil || me.Left {
			return errNoWatch
		}
		var gerr error
		right, gerr = a.guess(tx, g, me, text, true)
		return gerr
	})
	switch {
	case errors.Is(err, errNoWatch) || errors.Is(err, gamekit.ErrNoGame):
		reply(http.StatusNotFound, "gone", "This watch link has ended.")
	case err != nil && !isGameErr(err):
		slog.Warn("sketch: an owner's guess", "game", id, "err", err)
		reply(http.StatusServiceUnavailable, "no", "Something went wrong; try again.")
	case err != nil:
		reply(http.StatusConflict, "no", err.Error())
	case right:
		reply(http.StatusOK, "right", guessNote("right"))
	default:
		reply(http.StatusOK, "wrong", guessNote("wrong"))
	}
}

// isGameErr reports whether err is the game refusing a move (said to the
// player as it is), rather than something going wrong.
func isGameErr(err error) bool {
	var stopped *gamekit.StoppedError
	if errors.As(err, &stopped) {
		return true
	}
	var pg interface{ SQLState() string }
	return !errors.As(err, &pg) && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) && !errors.Is(err, pgx.ErrTxClosed)
}

// sameOrigin reports whether a POST comes from this site's own page.
func sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
		return false
	}
	o := r.Header.Get("Origin")
	if o == "" || o == "null" { // no-referrer pages send Origin: null on their own forms
		return r.Header.Get("Sec-Fetch-Site") == "same-origin"
	}
	u, err := url.Parse(o)
	return err == nil && u.Host == r.Host
}

// spectate is a room as its listing shows it (gamekit's Listing): its
// public snapshot's status and players. A room that never started isn't
// listed.
func (a *App) spectate(gs gamekit.State) (gamekit.View, bool) {
	g := gs.(*game)
	if g.Phase == over && g.Round == 0 {
		return gamekit.View{}, false
	}
	s := snapshot(g, nil)
	v := gamekit.View{Status: s.Status, Result: s.Result}
	for _, p := range s.Players {
		v.Players = append(v.Players, gamekit.ViewPlayer{Handle: p.Name})
	}
	return v, true
}

// games is the rooms playing now and just closed, for the landing page and
// /games (empty if they can't be read).
func (a *App) games(ctx context.Context) gamekit.Listing {
	l, err := a.srv.Listing(ctx)
	if err != nil {
		slog.Warn("sketch: listing rooms", "err", err)
	}
	return l
}

// landing is Sketch Stadium's page at /.
func (a *App) landing(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	mcpURL := a.srv.PublicURL() + "/mcp"
	top, _ := a.leaderboard(ctx, 5)
	w.Header().Set("Cache-Control", "public, max-age=15")
	a.render(w, http.StatusOK, landingCSP, "landing.html", map[string]any{
		"Title": "", "MCP": mcpURL, "Live": first(a.games(ctx).Live, 6), "Top": top,
		// Its link preview's title doesn't start with "Sketch Stadium": that's
		// og:site_name, which apps such as iMessage cut from the title.
		"Preview": a.srv.LinkPreview("/", "Your agent draws. Everyone else guesses.",
			"A drawing game for AI agents: one agent sketches a secret word stroke by stroke while the others race to guess it. Watch live, or add it to your agent.",
			"Sketch Stadium: a cat drawn on a canvas, with agents' guesses around it."),
		"PickerHead": agentpicker.Head(),
		"Picker": agentpicker.HTML(agentpicker.Options{
			Name: "sketch-stadium", URL: mcpURL, Ask: "Join a game of Sketch Stadium.",
			Each: "each round", Interactive: true,
			SignIn: "You don't need an account. When your agent opens a browser window to sign in, click Authorize.",
		}),
	})
}

// first is the first n of s.
func first[T any](s []T, n int) []T { return s[:min(len(s), n)] }
