package gamekit

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/wakeshift/apps/internal/pgtest"
	"github.com/wakeshift/wakeshift-go/appkit"
)

// kitServer is a kit server on a database of its own whose pool has
// conns connections, talking to core (nil: none).
func kitServer(t *testing.T, conns string, core http.Handler) *Server {
	t.Helper()
	ctx := context.Background()
	u, _ := url.Parse(pgtest.EmptyURL(t))
	q := u.Query()
	q.Set("pool_max_conns", conns)
	u.RawQuery = q.Encode()
	st, err := OpenStore(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	s := &Server{Name: "g", Store: st, MaxSeats: 3, Prefix: "app.g.game.",
		NewState: func() State { return &testGame{} },
		NewGame: func(g State, now time.Time) {
			g.(*testGame).Phase = "waiting"
			g.(*testGame).Deadline = now.Add(time.Hour)
		}}
	if core != nil {
		srv := httptest.NewServer(core)
		t.Cleanup(srv.Close)
		_, key, _ := ed25519.GenerateKey(rand.Reader)
		s.Kit = &appkit.App{ID: "g", Key: key, Core: srv.URL}
	}
	return s
}

// A ticker that finds its claimed game no longer due finishes on the one
// connection it holds: with a pool of one, it doesn't wait for a second.
func TestAdvanceNotDueOnOneConnection(t *testing.T) {
	ctx := context.Background()
	s := kitServer(t, "1", nil)
	if n := s.tickers(); n != 1 {
		t.Fatalf("%d tickers on a pool of 1", n)
	}
	r, err := s.reserve(ctx, "w", "a", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.Pool.Exec(ctx, `UPDATE games SET ticking_until = now() + interval '1 minute' WHERE id = $1`, r.game); err != nil {
		t.Fatal(err)
	}
	actx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if ok, err := s.advance(actx, r.game, nil); ok || err != nil {
		t.Fatalf("advancing a game that isn't due: %v, %v", ok, err)
	}
	var claimed bool
	if err := s.Store.Pool.QueryRow(ctx, `SELECT ticking_until IS NOT NULL FROM games WHERE id = $1`, r.game).Scan(&claimed); err != nil || claimed {
		t.Fatalf("still claimed: %v, %v", claimed, err)
	}
}

// What players have had lately is forgotten once they haven't played for
// recentFor, about once an hour.
func TestForgetRecent(t *testing.T) {
	ctx := context.Background()
	s := kitServer(t, "4", nil)
	now := time.Now()
	add := func(player string, at time.Time) {
		t.Helper()
		if _, err := s.Store.Pool.Exec(ctx, `INSERT INTO recent (player, kind, items, updated_at) VALUES ($1, 'codewords', '{harp}', $2)`, player, at); err != nil {
			t.Fatal(err)
		}
	}
	players := func() (out []string) {
		t.Helper()
		rows, err := s.Store.Pool.Query(ctx, `SELECT player FROM recent ORDER BY player`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var p string
			rows.Scan(&p)
			out = append(out, p)
		}
		return out
	}
	add("old", now.Add(-recentFor-time.Hour))
	add("new", now.Add(-time.Hour))
	if err := s.forget(ctx, now); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(players(), ","); got != "new" {
		t.Fatalf("after forgetting: %s", got)
	}
	add("older", now.Add(-recentFor-time.Hour))
	s.forget(ctx, now.Add(time.Minute)) // too soon: not again
	if got := strings.Join(players(), ","); got != "new,older" {
		t.Fatalf("forgot again within the hour: %s", got)
	}
	s.forget(ctx, now.Add(time.Hour+time.Minute))
	if got := strings.Join(players(), ","); got != "new" {
		t.Fatalf("an hour later: %s", got)
	}
}

// Matchmaking never seats two players of one workspace in one game.
func TestOneWorkspaceOneSeat(t *testing.T) {
	ctx := context.Background()
	s := kitServer(t, "4", nil)
	a, err := s.reserve(ctx, "w1", "claude-code", "claude-code#1")
	if err != nil {
		t.Fatal(err)
	}
	for _, other := range [][2]string{{"claude-code", "claude-code#2"}, {"codex", ""}} {
		b, err := s.reserve(ctx, "w1", other[0], other[1])
		if err != nil {
			t.Fatal(err)
		}
		if b.game == a.game {
			t.Fatalf("%s seated with claude-code#1 of the same workspace", other)
		}
	}
	c, err := s.reserve(ctx, "w2", "claude-code", "")
	if err != nil {
		t.Fatal(err)
	}
	if c.game != a.game {
		t.Fatalf("another workspace's player went to %s, not the oldest open game %s", c.game, a.game)
	}
}

const (
	sharedWS = "6f1c2b4e-9a3d-4c1e-8b2a-0d5e7f9a1b3c"
	otherWS  = "0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"
)

// A workspace in SharedSeats fills one game with its own players.
func TestSharedSeatsFillOneGame(t *testing.T) {
	ctx := context.Background()
	s := kitServer(t, "4", nil)
	s.SharedSeats = map[string]bool{sharedWS: true}
	var game string
	names := map[string]bool{}
	for i, p := range [][2]string{{"claude-code", "claude-code#1"}, {"claude-code", "claude-code#2"}, {"codex", ""}} {
		r, err := s.reserve(ctx, sharedWS, p[0], p[1])
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			game = r.game
		} else if r.game != game {
			t.Fatalf("player %d of a shared workspace went to %s, not %s", i, r.game, game)
		}
		if names[r.name] {
			t.Fatalf("name %s given twice", r.name)
		}
		names[r.name] = true
	}
	// The game is full: a fourth goes to a new one.
	r, err := s.reserve(ctx, sharedWS, "cursor", "")
	if err != nil {
		t.Fatal(err)
	}
	if r.game == game {
		t.Fatal("a fourth player seated in a full game")
	}
	// Each player still has one seat: joining again finds its reservation.
	if _, err := s.reserve(ctx, sharedWS, "codex", ""); !errors.Is(err, ErrJoining) {
		t.Fatalf("joining again: %v, want ErrJoining", err)
	}
}

// SharedSeats exempts only the workspaces in it: another workspace still
// can't take two seats in one game, even one a shared workspace is in.
func TestSharedSeatsOnlyListed(t *testing.T) {
	ctx := context.Background()
	s := kitServer(t, "4", nil)
	s.SharedSeats = map[string]bool{sharedWS: true}
	a, err := s.reserve(ctx, otherWS, "claude-code", "claude-code#1")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.reserve(ctx, otherWS, "codex", "")
	if err != nil {
		t.Fatal(err)
	}
	if b.game == a.game {
		t.Fatal("two players of a workspace not in SharedSeats seated in one game")
	}
	// Mixed: the shared workspace's players join the oldest open game,
	// alongside the other workspace's player, and fill it.
	for _, agent := range []string{"claude-code", "codex"} {
		r, err := s.reserve(ctx, sharedWS, agent, "")
		if err != nil {
			t.Fatal(err)
		}
		if r.game != a.game {
			t.Fatalf("shared workspace's %s went to %s, not the oldest open game %s", agent, r.game, a.game)
		}
	}
	// Both open games have one of its players already: a third gets a new one.
	c, err := s.reserve(ctx, otherWS, "cursor", "")
	if err != nil {
		t.Fatal(err)
	}
	if c.game == a.game || c.game == b.game {
		t.Fatalf("a third player of a workspace not in SharedSeats seated with its own (%s)", c.game)
	}
}

func TestParseWorkspaces(t *testing.T) {
	got, err := ParseWorkspaces(" " + strings.ToUpper(sharedWS) + ", ," + otherWS + ",")
	if err != nil || len(got) != 2 || !got[sharedWS] || !got[otherWS] {
		t.Fatalf("got %v, %v", got, err)
	}
	if got, err := ParseWorkspaces(" "); err != nil || got != nil {
		t.Fatalf("empty: %v, %v", got, err)
	}
	for _, bad := range []string{"w1", sharedWS + ",nope", "6f1c2b4e9a3d4c1e8b2a0d5e7f9a1b3c", "6f1c2b4e-9a3d-4c1e-8b2a-0d5e7f9a1b3g"} {
		if _, err := ParseWorkspaces(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}

// Posting announcements renews its lease per post and stops once another
// instance has the lease, leaving that one's lease alone.
func TestDeliverHoldsItsLease(t *testing.T) {
	ctx := context.Background()
	var s *Server
	var posts atomic.Int32
	steal := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
	s = kitServer(t, "4", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/api/apps/v1/groups/app.g.game.g1/events" {
			http.NotFound(w, r)
			return
		}
		if posts.Add(1) == 1 {
			// Wakeshift is slow, and another instance takes over.
			s.Store.Pool.Exec(r.Context(), `UPDATE games SET posting_until = $1 WHERE id = 'g1'`, steal)
		}
		w.Write([]byte(`{"id":"e1"}`))
	}))
	if _, err := s.Store.Pool.Exec(ctx, `INSERT INTO games (id, phase, max_seats, state) VALUES ('g1', 'playing', 3, '{}');
		INSERT INTO room_log (game_id, at, subject, text) VALUES ('g1', now(), 'one', 'one'), ('g1', now(), 'two', 'two')`); err != nil {
		t.Fatal(err)
	}
	s.deliver(ctx, "g1")
	var lease time.Time
	var unposted int
	if err := s.Store.Pool.QueryRow(ctx, `SELECT posting_until, (SELECT count(*) FROM room_log WHERE game_id = 'g1' AND posted_at IS NULL) FROM games WHERE id = 'g1'`).
		Scan(&lease, &unposted); err != nil {
		t.Fatal(err)
	}
	if posts.Load() != 1 || unposted != 1 || !lease.Equal(steal) {
		t.Fatalf("%d posted, %d left, lease %v", posts.Load(), unposted, lease)
	}
}

// Durations in what players read are words, not Go's "2m0s".
func TestHuman(t *testing.T) {
	for d, want := range map[time.Duration]string{
		2 * time.Minute: "2 minutes", 30 * time.Minute: "30 minutes", time.Minute: "1 minute", 90 * time.Second: "1 minute 30 seconds",
		time.Hour: "1 hour", 45 * time.Second: "45 seconds", 0: "0 seconds", 1500 * time.Millisecond: "2 seconds",
	} {
		if got := Human(d); got != want {
			t.Errorf("Human(%v) = %q, want %q", d, got, want)
		}
	}
}

// A subject is cut to Wakeshift's limit in bytes, on a character boundary.
func TestTruncateSubject(t *testing.T) {
	long := strings.Repeat("夜", 100) // 300 bytes
	got := truncate(long, subjectMax)
	if len(got) > subjectMax || !utf8.ValidString(got) || !strings.HasSuffix(got, "…") {
		t.Fatalf("%d bytes, valid %v: %q", len(got), utf8.ValidString(got), got)
	}
	if truncate("short", subjectMax) != "short" {
		t.Fatal("a short subject was cut")
	}
}

// A seat whose reservation was swept while its player was joining the
// room isn't taken: the player goes to the next game, seated there.
func TestJoinSweptReservation(t *testing.T) {
	ctx := context.Background()
	var s *Server
	var swept atomic.Bool
	s = kitServer(t, "4", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/apps/v1/workspaces/w/agents/a/token":
			w.Write([]byte(`{"access_token":"tok","expires_in":900}`))
		case r.Method == "PUT" && strings.HasPrefix(r.URL.Path, "/api/apps/v1/groups/"):
			w.Write([]byte(`{"group":{},"created":true}`))
		case r.URL.Path == "/api/apps/v1/agent/subscriptions":
			w.Write([]byte(`{}`))
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/apps/v1/groups/"):
			if swept.CompareAndSwap(false, true) {
				s.Store.Pool.Exec(r.Context(), `DELETE FROM seats`) // abandoned, as another join sees it
			}
			w.Write([]byte(`{"members":[{"handle":"raven","workspace":"w","agent":"a","session":"s"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	ag, err := s.Kit.ActAs(ctx, "w", "a")
	if err != nil {
		t.Fatal(err)
	}
	var seated []string
	g, p, err := s.Join(ctx, ag, "s", func(tx *Tx, g State, p Player) error {
		seated = append(seated, g.base().ID)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var handle *string
	if err := s.Store.Pool.QueryRow(ctx, `SELECT handle FROM seats WHERE game_id = $1 AND player = $2`, g.base().ID, p.Key()).Scan(&handle); err != nil {
		t.Fatalf("no seat in the game it joined: %v", err)
	}
	if len(seated) != 1 || seated[0] != g.base().ID || handle == nil || *handle != "raven" {
		t.Fatalf("seated in %v, joined %s as %v", seated, g.base().ID, handle)
	}
}

// A seated player whose subscription ended (it unsubscribed, it expired,
// its consent was revoked) is no longer in the room: it's away, so the game
// doesn't wait for it, until it joins again and is back in the room under
// its seat's name.
func TestEndedPlayersAreAwayUntilTheyRejoin(t *testing.T) {
	ctx := context.Background()
	type member struct {
		Handle    string `json:"handle"`
		Workspace string `json:"workspace"`
		Agent     string `json:"agent"`
		Session   string `json:"session"`
		Connected bool   `json:"connected"`
	}
	var mu sync.Mutex
	members := []member{{"raven", "w1", "a", "s", true}, {"finch", "w2", "b", "t", true}}
	var resubscribed appkit.Subscription
	s := kitServer(t, "4", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/apps/v1/workspaces/w1/agents/a/token":
			w.Write([]byte(`{"access_token":"tok","expires_in":900}`))
		case r.Method == "POST" && r.URL.Path == "/api/apps/v1/agent/subscriptions":
			json.NewDecoder(r.Body).Decode(&resubscribed)
			members = append(members, member{resubscribed.Handle, "w1", "a", resubscribed.Session, true})
			w.Write([]byte(`{}`))
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/apps/v1/groups/"):
			json.NewEncoder(w).Encode(map[string]any{"members": members})
		default:
			http.NotFound(w, r)
		}
	}))
	var game string
	for _, p := range []struct{ ws, agent, session, handle string }{{"w1", "a", "s", "raven"}, {"w2", "b", "t", "finch"}} {
		r, err := s.reserve(ctx, p.ws, p.agent, p.session)
		if err != nil {
			t.Fatal(err)
		}
		game = r.game
		if _, err := s.Store.Pool.Exec(ctx, `UPDATE seats SET handle = $2, seated_at = now() WHERE game_id = $1 AND player = $3`,
			r.game, p.handle, PlayerKey(p.ws, p.agent, p.session)); err != nil {
			t.Fatal(err)
		}
	}
	away := func() []string {
		got := slices.Sorted(maps.Keys(s.Away(ctx, game)))
		return got
	}
	if got := away(); len(got) != 0 {
		t.Fatalf("away with both in the room: %v", got)
	}
	// Raven's subscription ends (it leaves the room); finch's agent drops off.
	mu.Lock()
	members = []member{{"finch", "w2", "b", "t", false}}
	mu.Unlock()
	if got := away(); !slices.Equal(got, []string{"finch", "raven"}) {
		t.Fatalf("away: %v", got)
	}
	// Raven joins again: back in the room as raven, from its conversation
	// (another member named raven isn't it).
	mu.Lock()
	members = append(members, member{"raven", "w3", "c", "u", true})
	mu.Unlock()
	ag, err := s.Kit.ActAs(ctx, "w1", "a")
	if err != nil {
		t.Fatal(err)
	}
	g, p, err := s.Join(ctx, ag, "s", func(*Tx, State, Player) error { t.Fatal("seated again"); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if g.base().ID != game || p != (Player{}) {
		t.Fatalf("rejoining returned game %s, player %+v", g.base().ID, p)
	}
	if resubscribed.Handle != "raven" || resubscribed.Session != "s" || resubscribed.Type != s.room(game) {
		t.Fatalf("rejoined the room as %+v", resubscribed)
	}
	if got := away(); !slices.Equal(got, []string{"finch"}) {
		t.Fatalf("away after raven rejoined: %v", got)
	}
	// Joining again while in the room changes nothing.
	resubscribed = appkit.Subscription{}
	if _, _, err := s.Join(ctx, ag, "s", nil); err != nil || resubscribed.Type != "" {
		t.Fatalf("joining again while in the room: %v, %+v", err, resubscribed)
	}
}
