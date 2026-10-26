package gamekit

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// The follow-along page: a player's operator watches its game live, with
// what their own agent knows. The link is signed by the app and names one
// game and one player; the game hands it to the agent to give its user.

const followTTL = 24 * time.Hour

// View is a game as one player's operator sees it.
type View struct {
	Game    string       `json:"game"`
	Status  string       `json:"status"` // "voting · 41s left"
	You     []Line       `json:"you"`    // what the player knows: its role, what it learned
	Players []ViewPlayer `json:"players"`
	Result  []string     `json:"result,omitempty"` // once it's over
	Log     []Entry      `json:"log"`
	// About is what the public page says of the game (spectate.go): the
	// cards in play, say. Everyone in the room knows it.
	About []Line `json:"about,omitempty"`
	// Table is the game's own picture of the table (Server.Table), for
	// its script: on the public page, only what About may say.
	Table any `json:"table,omitempty"`
	// Thinking is the player's own private reasoning (thinking.go): set by
	// the kit on the follow-along page only, never on a public one.
	Thinking []Thought `json:"thinking,omitempty"`
}

// Line is one thing the player knows; Tone colors it ("good", "bad").
// With a Label, the page shows it as "Label: Text"; a Secret one's Text is
// hidden until the operator hovers over it or taps it (a codeword, say).
type Line struct {
	Label  string `json:"label,omitempty"`
	Text   string `json:"text"`
	Tone   string `json:"tone,omitempty"`
	Secret bool   `json:"secret,omitempty"`
}

// ViewPlayer is a player as the page lists it.
type ViewPlayer struct {
	Handle string `json:"handle"`
	Note   string `json:"note,omitempty"` // "voted", "→ codex", "(vampire)"
	You    bool   `json:"you,omitempty"`
	// Avatar is the URL of the player's picture, if the game has them; the
	// page shows it by the player and by what it says.
	Avatar string `json:"avatar,omitempty"`
}

// Theme is a game's look for the follow-along page and the public pages:
// colors and fonts (CSS values; empty ones keep the kit's), an image behind
// the page's header, and more CSS of its own, after the kit's (PublicCSS
// for the public pages only, after CSS).
type Theme struct {
	Background, Surface, Text, Muted, Line, Accent string
	Font, TitleFont                                string // font stacks
	HeaderImage                                    string // a URL (StaticURL)
	CSS, PublicCSS                                 string
	Icon                                           string // the public pages' icon (a URL), if any
}

func (t *Theme) css() string {
	if t == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(":root{")
	for _, v := range [][2]string{{"--bg", t.Background}, {"--surface", t.Surface}, {"--text", t.Text}, {"--muted", t.Muted},
		{"--line", t.Line}, {"--accent", t.Accent}, {"--font", t.Font}, {"--title-font", t.TitleFont}} {
		if v[1] != "" {
			b.WriteString(v[0] + ":" + v[1] + ";")
		}
	}
	b.WriteString("}\n")
	if t.HeaderImage != "" {
		b.WriteString(`.top{background:linear-gradient(#0b0d12b3,#0b0d12e6),url("` + t.HeaderImage + `") center 62%/cover no-repeat}` + "\n")
	}
	b.WriteString(t.CSS)
	return b.String()
}

type followToken struct {
	Game    string `json:"g"`
	Player  string `json:"p"`
	Expires int64  `json:"e"`
}

func (s *Server) followPurpose() string { return s.Name + "-follow:" }

// FollowURL is a player's follow-along link.
func (s *Server) FollowURL(game string, p Player) string {
	b, _ := json.Marshal(followToken{Game: game, Player: p.Key(), Expires: s.now().Add(followTTL).Unix()})
	payload := base64.RawURLEncoding.EncodeToString(b)
	sig := ed25519.Sign(s.Kit.Key, []byte(s.followPurpose()+payload))
	return s.PublicURL() + "/follow/" + payload + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func (s *Server) readFollow(token string) (followToken, bool) {
	payload, sig, ok := strings.Cut(token, ".")
	b, err := base64.RawURLEncoding.DecodeString(sig)
	if !ok || err != nil || !ed25519.Verify(s.Kit.Key.Public().(ed25519.PublicKey), []byte(s.followPurpose()+payload), b) {
		return followToken{}, false
	}
	raw, _ := base64.RawURLEncoding.DecodeString(payload)
	var t followToken
	if json.Unmarshal(raw, &t) != nil || s.now().Unix() > t.Expires {
		return followToken{}, false
	}
	return t, true
}

func (s *Server) followRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /follow/{token}", func(w http.ResponseWriter, r *http.Request) {
		t, ok := s.readFollow(r.PathValue("token"))
		if !ok {
			http.Error(w, "This link isn't valid, or it has expired. Ask your agent for a new one (its game tool has it).", http.StatusNotFound)
			return
		}
		if s.Table != nil {
			s.followTable(w, r, t)
			return
		}
		PageHeaders(w, true)
		w.Header().Set("Cache-Control", "no-store")
		w.Write([]byte(s.followHTML()))
	})
	mux.HandleFunc("GET /follow/{token}/state", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		t, ok := s.readFollow(r.PathValue("token"))
		if !ok || s.Follow == nil {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":"This game is no longer available."}`))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		v, err := s.Follow(ctx, t.Game, t.Player)
		var stopped *StoppedError
		switch {
		case errors.Is(err, ErrNoGame):
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":"This game is no longer available."}`))
			return
		case errors.As(err, &stopped):
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "This game had to stop: " + stopped.Reason})
			return
		case err != nil:
			// Busy, or the database is: the page tries again.
			slog.Warn(s.Name+": the follow-along page", "err", err)
			w.Header().Set("Retry-After", "5")
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"error":"Busy right now; trying again…","retry":true}`))
			return
		}
		if v.Log == nil {
			v.Log = []Entry{}
		}
		s.addThinking(ctx, &v, t)
		json.NewEncoder(w).Encode(v)
	})
}

// followTable is the follow-along page of a game with a table: drawn like
// its public page (spectate.go), with what the player knows above it.
func (s *Server) followTable(w http.ResponseWriter, r *http.Request, t followToken) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	w.Header().Set("Cache-Control", "no-store")
	if s.Follow == nil {
		s.renderSpectate(w, http.StatusNotFound, "gone.html", map[string]any{"Title": "No such game", "Text": "This game is no longer available."})
		return
	}
	v, err := s.Follow(ctx, t.Game, t.Player)
	var stopped *StoppedError
	switch {
	case errors.Is(err, ErrNoGame):
		s.renderSpectate(w, http.StatusNotFound, "gone.html", map[string]any{"Title": "No such game", "Text": "This game is no longer available."})
		return
	case errors.As(err, &stopped):
		s.renderSpectate(w, http.StatusNotFound, "gone.html", map[string]any{"Title": "This game had to stop", "Text": stopped.Reason})
		return
	case err != nil:
		slog.Warn(s.Name+": the follow-along page", "err", err)
		w.Header().Set("Retry-After", "5")
		s.renderSpectate(w, http.StatusServiceUnavailable, "gone.html", map[string]any{"Title": "Busy right now", "Text": "Reload the page in a moment."})
		return
	}
	if v.Log == nil {
		v.Log = []Entry{}
	}
	s.addThinking(ctx, &v, t)
	pics := map[string]string{}
	for _, p := range v.Players {
		if p.Avatar != "" {
			pics[p.Handle] = p.Avatar
		}
	}
	data := map[string]any{"V": v, "Pics": pics, "Title": "Game " + v.Game, "Follow": true, "State": r.URL.Path + "/state"}
	if err := s.table(data, v); err != nil {
		slog.Error(s.Name+": a game's table", "err", err)
		http.Error(w, "Something went wrong.", http.StatusInternalServerError)
		return
	}
	s.renderSpectate(w, http.StatusOK, "game.html", data)
}

// addThinking adds the player's own reasoning to its follow-along view: t
// is a link readFollow verified (signed by the app for this one game and
// player, unexpired). It's read for every request, never cached (the
// follow cache holds only the game and its log, never a View), and set on
// this request's own View. Without it (the database is busy), the page
// shows the rest.
func (s *Server) addThinking(ctx context.Context, v *View, t followToken) {
	if s.Store == nil {
		return
	}
	th, err := s.Thinking(ctx, t.Game, t.Player)
	if err != nil {
		slog.Warn(s.Name+": a player's reasoning for its follow-along page", "err", err)
		return
	}
	v.Thinking = th
}

// followCache keeps what the follow-along page reads of a game for a
// second: every viewer polls, and they needn't each read the database.
type followCache struct {
	mu      sync.Mutex
	entries map[string]cachedGame
}

type cachedGame struct {
	at    time.Time
	state State
	log   []Entry
	err   error
}

const followFresh = time.Second

// Snapshot is a game and its log, as the follow-along page shows them:
// at most a second old. Treat the state as read-only (it's shared).
func (s *Server) Snapshot(ctx context.Context, id string) (State, []Entry, error) {
	c := &s.follows
	c.mu.Lock()
	if e, ok := c.entries[id]; ok && time.Since(e.at) < followFresh {
		c.mu.Unlock()
		return e.state, e.log, e.err
	}
	c.mu.Unlock()
	g, err := s.Load(ctx, id)
	var log []Entry
	if err == nil {
		log, err = s.Log(ctx, id)
	}
	if err != nil && !errors.Is(err, ErrNoGame) {
		return nil, nil, err // not cached: the next poll tries again
	}
	c.mu.Lock()
	if c.entries == nil || len(c.entries) > 10000 {
		c.entries = map[string]cachedGame{}
	}
	c.entries[id] = cachedGame{at: time.Now(), state: g, log: log, err: err}
	c.mu.Unlock()
	return g, log, err
}

func (s *Server) followHTML() string {
	class := ""
	if s.Theme != nil {
		class = "themed"
	}
	return strings.NewReplacer("{{TITLE}}", html.EscapeString(s.Title), "{{CLASS}}", class,
		"{{THEME}}", strings.ReplaceAll(s.Theme.css(), "</", `<\/`)).Replace(followPage)
}
