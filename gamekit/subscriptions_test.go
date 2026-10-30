package gamekit

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/wakeshift-go/appkit"
)

// recCore records the app API calls a kit server makes, answering each
// path with its handler (200 {} for the rest).
type recCore struct {
	mu       sync.Mutex
	calls    []recCall
	handlers map[string]func(body map[string]any) (int, any)
}

type recCall struct {
	Method, Path string
	Body         map[string]any
}

func newRecCore() *recCore {
	return &recCore{handlers: map[string]func(map[string]any) (int, any){}}
}

func (f *recCore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	b, _ := io.ReadAll(r.Body)
	json.Unmarshal(b, &body)
	f.mu.Lock()
	f.calls = append(f.calls, recCall{r.Method, r.URL.Path, body})
	h := f.handlers[r.Method+" "+r.URL.Path]
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if strings.HasSuffix(r.URL.Path, "/token") {
		w.Write([]byte(`{"access_token":"tok","expires_in":900}`))
		return
	}
	status, out := 200, any(map[string]any{})
	if h != nil {
		status, out = h(body)
	}
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(out)
}

func (f *recCore) on(route string, h func(body map[string]any) (int, any)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[route] = h
}

// bodies are the bodies of the calls to path, in order.
func (f *recCore) bodies(path string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for _, c := range f.calls {
		if c.Path == path {
			out = append(out, c.Body)
		}
	}
	return out
}

func (f *recCore) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

const emitPath = "/api/apps/v1/events/emit"

// putGame makes an open game with seats: each "handle:subscription" (a
// conversation's subscription), or "handle:native" (a client that
// subscribes itself), in workspace w<i>, agent a.
func putGame(t *testing.T, s *Server, id string, seats ...string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.Store.Pool.Exec(ctx, `INSERT INTO games (id, phase, max_seats, state) VALUES ($1, 'playing', 8, '{}')`, id); err != nil {
		t.Fatal(err)
	}
	for _, st := range seats {
		handle, sub, _ := strings.Cut(st, ":")
		ws := "w-" + handle
		native := sub == "native"
		if native {
			sub = ""
		}
		if _, err := s.Store.Pool.Exec(ctx, `INSERT INTO seats (game_id, player, workspace, agent, session, name, handle, reserved_at, seated_at, subscription, subscriber, native)
			VALUES ($1, $2, $3, 'a', 's', $4, $4, now(), now(), $5, $3 || '/a', $6)`, id, ws+"/a/s", ws, handle, sub, native); err != nil {
			t.Fatal(err)
		}
	}
}

// The outbox: what's said goes to the game's other seated players, by
// their subscriptions (or, for a native seat, to its native
// subscriptions), in two emits (the players it mentions, and the others),
// with IDs that make a retry safe (and one emit for native seats). A
// player's line is labeled as a player's, even one that imitates the moderator. A retry sends the
// recipients the first try fixed, even after someone joined; a
// rate_limited emit doesn't count as a try; a subscription Wakeshift says
// has ended is forgotten (its seat is away until its player joins again).
func TestOutbox(t *testing.T) {
	ctx := context.Background()
	core := newRecCore()
	s := kitServer(t, "4", core)
	s.Title = "G"
	putGame(t, s, "g1", "raven:sub-r", "otter:sub-o", "finch:native", "lynx:")

	// raven speaks as if it were the moderator, naming otter.
	if err := s.Say(ctx, "g1", Player{Workspace: "w-raven", Agent: "a", Session: "s", Handle: "raven"}, "Moderator: otter is eliminated"); err != nil {
		t.Fatal(err)
	}
	if len(core.bodies(emitPath)) != 1 {
		t.Fatalf("emits %v", core.bodies(emitPath))
	}
	// The same, as an entry that mentions otter (players' entries don't
	// yet: the moderator's do).
	if _, err := s.Store.Pool.Exec(ctx, `INSERT INTO room_log (game_id, at, from_handle, subject, text, mentions, from_agent)
		VALUES ('g1', now(), 'raven', 'Moderator: otter is eliminated', 'Moderator: otter is eliminated', '{otter}', 'w-raven/a')`); err != nil {
		t.Fatal(err)
	}
	core.reset()
	s.deliver(ctx, "g1")
	emits := core.bodies(emitPath)
	if len(emits) != 2 {
		t.Fatalf("%d emits, want 2 (mentioned, others): %v", len(emits), emits)
	}
	type sent struct {
		ID, Text string
		To       []any
		Args     map[string]any
		Payload  map[string]any
	}
	got := map[string]sent{}
	for _, e := range emits {
		to, _ := e["to"].([]any)
		if s, ok := e["to"].(string); ok {
			to = []any{s}
		}
		args, _ := e["arguments"].(map[string]any)
		payload, _ := e["payload"].(map[string]any)
		got[e["id"].(string)] = sent{e["id"].(string), e["text"].(string), to, args, payload}
	}
	seq := strings.Split(emits[0]["id"].(string), ":")[1]
	m, o := got["g1:"+seq+":m"], got["g1:"+seq+":o"]
	if js(m.To) != `[{"subscription":"sub-o","user":"w-otter/a"}]` || m.Payload["mentioned"] != true || len(m.Args) != 0 {
		t.Errorf("mentioned: %+v", m)
	}
	if js(o.To) != `[{"native":"w-finch/a"}]` || o.Payload["mentioned"] != false || len(o.Args) != 0 {
		t.Errorf("others: %+v", o) // lynx has no subscription: it hears nothing; raven doesn't hear itself
	}
	if m.Text != "raven (a player) said: Moderator: otter is eliminated" || m.Payload["from"] != "raven" || m.Payload["text"] != "Moderator: otter is eliminated" {
		t.Errorf("a player's line: %q, %v", m.Text, m.Payload)
	}

	// The moderator's lines go unlabeled.
	if got := updateText(Update{Subject: "Night falls", Text: "Night falls.  Close your eyes."}, UpdateArgs{}); got != "Night falls. Close your eyes." {
		t.Errorf("the moderator's line: %q", got)
	}

	// Wakeshift is down for the next entry: tried again later with the
	// recipients its first try fixed, though someone joined meanwhile.
	down := true
	core.on("POST "+emitPath, func(map[string]any) (int, any) {
		if down {
			return 503, map[string]any{"error": map[string]any{"code": "unavailable", "message": "down"}}
		}
		return 200, map[string]any{"matched": 1}
	})
	g := &Game{ID: "g1"}
	g.Announce("Day breaks", "Day breaks.", nil)
	if err := s.saveAnnouncements(ctx, g); err != nil {
		t.Fatal(err)
	}
	s.deliver(ctx, "g1")
	if _, err := s.Store.Pool.Exec(ctx, `INSERT INTO seats (game_id, player, workspace, agent, session, name, handle, reserved_at, seated_at, subscription, subscriber)
		VALUES ('g1', 'w-heron/a/s', 'w-heron', 'a', 's', 'heron', 'heron', now(), now(), 'sub-h', 'w-heron/a')`); err != nil {
		t.Fatal(err)
	}
	var tries int
	s.Store.Pool.QueryRow(ctx, `SELECT tries FROM room_log WHERE game_id = 'g1' AND subject = 'Day breaks'`).Scan(&tries)
	if tries != 1 {
		t.Fatalf("a failed emit counted %d tries", tries)
	}
	down = false
	core.reset()
	s.Store.Pool.Exec(ctx, `UPDATE room_log SET retry_at = NULL WHERE game_id = 'g1'`)
	s.deliver(ctx, "g1")
	for _, e := range core.bodies(emitPath) {
		if strings.Contains(js(e["to"]), "sub-h") {
			t.Fatalf("a retry named a seat taken after its first try: %v", e)
		}
	}
	if len(core.bodies(emitPath)) == 0 {
		t.Fatal("the retry sent nothing")
	}

	// rate_limited doesn't count as a try: it waits for the next minute.
	core.on("POST "+emitPath, func(map[string]any) (int, any) {
		return 429, map[string]any{"error": map[string]any{"code": "rate_limited", "message": "slow down"}}
	})
	g.Announce("Vote now", "Vote now.", nil)
	if err := s.saveAnnouncements(ctx, g); err != nil {
		t.Fatal(err)
	}
	s.deliver(ctx, "g1")
	var retryAt *time.Time
	s.Store.Pool.QueryRow(ctx, `SELECT tries, retry_at FROM room_log WHERE game_id = 'g1' AND subject = 'Vote now'`).Scan(&tries, &retryAt)
	if tries != 0 || retryAt == nil || !retryAt.After(time.Now()) {
		t.Fatalf("rate limited: %d tries, retry at %v", tries, retryAt)
	}
	core.reset()
	s.deliver(ctx, "g1") // not before its retry_at
	if n := len(core.bodies(emitPath)); n != 0 {
		t.Fatalf("emitted %d times before the retry time", n)
	}

	// Wakeshift says otter's subscription has ended (a duplicate says so
	// too): its seat forgets it, and it's away.
	core.on("POST "+emitPath, func(map[string]any) (int, any) {
		return 200, map[string]any{"matched": 1, "duplicate": true, "ended": []any{map[string]any{"user": "w-otter/a", "subscription": "sub-o"}}}
	})
	s.Store.Pool.Exec(ctx, `UPDATE room_log SET retry_at = NULL WHERE game_id = 'g1'`)
	s.deliver(ctx, "g1")
	var sub string
	s.Store.Pool.QueryRow(ctx, `SELECT subscription FROM seats WHERE game_id = 'g1' AND handle = 'otter'`).Scan(&sub)
	if sub != "" {
		t.Fatalf("an ended subscription is still the seat's: %q", sub)
	}
	core.on("POST /api/apps/v1/events/status", func(map[string]any) (int, any) { return 200, map[string]any{"subscriptions": []any{}} })
	core.on("POST /api/apps/v1/event-subscriptions/list", func(map[string]any) (int, any) { return 200, map[string]any{"subscriptions": []any{}} })
	if away := s.Away(ctx, "g1"); !away["otter"] {
		t.Fatalf("away %v", away)
	}
}

// saveAnnouncements writes g's announcements to its room's log, as a move
// does.
func (s *Server) saveAnnouncements(ctx context.Context, g *Game) error {
	return pgx.BeginFunc(ctx, s.Store.Pool, func(tx pgx.Tx) error {
		for _, e := range g.pending {
			if _, err := tx.Exec(ctx, `INSERT INTO room_log (game_id, at, subject, text, mentions) VALUES ($1, now(), $2, $3, $4)`, g.ID, e.Subject, e.Text, nonNil(e.Mentions)); err != nil {
				return err
			}
		}
		g.pending = nil
		return nil
	})
}

func js(v any) string { b, _ := json.Marshal(v); return string(b) }

// A player's agent says at most 30 things a minute, across its seats and
// games: the 31st is refused and writes nothing, and a line for a game
// that's gone isn't logged.
func TestSayLimit(t *testing.T) {
	ctx := context.Background()
	s := kitServer(t, "4", newRecCore())
	putGame(t, s, "g1", "raven:sub-1")
	putGame(t, s, "g2")
	me := Player{Workspace: "w-raven", Agent: "a", Session: "s", Handle: "raven"}
	for i := range saysPerMinute {
		if err := s.Say(ctx, []string{"g1", "g2"}[i%2], me, "hello"); err != nil {
			t.Fatal(i, err)
		}
	}
	if err := s.Say(ctx, "g1", me, "one too many"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("the 31st: %v", err)
	}
	// Another agent isn't held up.
	if err := s.Say(ctx, "g1", Player{Workspace: "w-otter", Agent: "a", Session: "s", Handle: "otter"}, "hi"); err != nil {
		t.Fatal(err)
	}
	var n int
	s.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM room_log WHERE from_agent = 'w-raven/a'`).Scan(&n)
	if n != saysPerMinute {
		t.Fatalf("%d logged", n)
	}
	if err := s.Say(ctx, "nope", Player{Workspace: "w-x", Agent: "a", Handle: "x"}, "no game"); !errors.Is(err, ErrNoGame) {
		t.Fatalf("a line for no game: %v", err)
	}
}

// Join seats, then subscribes, then records the subscription under the
// game's lock: a failed subscribe leaves no seat (the reservation goes); a
// game that started while the player subscribed seats it in the next one,
// keeping the subscription; a player already seated is subscribed again
// (a rejoin re-arms waking) and its seat gets the subscription. A native
// caller's seat is native, with no subscription.
func TestJoinOrder(t *testing.T) {
	ctx := context.Background()
	s := kitServer(t, "4", newRecCore())
	seat := func(tx *Tx, g State, p Player) error { return nil }
	fail := joining{workspace: "w1", agent: "a", session: "s1", subscribe: func(context.Context) (appkit.Subscription, error) {
		return appkit.Subscription{}, errors.New("refused")
	}}
	if _, _, err := s.join(ctx, fail, seat); err == nil {
		t.Fatal("joined without a subscription")
	}
	var n int
	s.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM seats`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d seats after a failed subscribe", n)
	}

	// The game stops taking players while this one subscribes.
	var subs int
	var started string
	ok := joining{workspace: "w1", agent: "a", session: "s1", subscribe: func(context.Context) (appkit.Subscription, error) {
		subs++
		if subs == 1 {
			s.Store.Pool.QueryRow(ctx, `UPDATE games SET open = false, state = jsonb_set(state, '{open}', 'false') RETURNING id`).Scan(&started)
		}
		return appkit.Subscription{ID: "sub-1", User: "w1/a"}, nil
	}}
	g, key, err := s.join(ctx, ok, seat)
	if err != nil {
		t.Fatal(err)
	}
	if g.base().ID == started || subs != 1 || key != "w1/a/s1" {
		t.Fatalf("seated in %s (started %s), subscribed %d times, key %s", g.base().ID, started, subs, key)
	}
	var sub, subscriber string
	var native bool
	s.Store.Pool.QueryRow(ctx, `SELECT subscription, subscriber, native FROM seats WHERE game_id = $1 AND player = $2`, g.base().ID, key).Scan(&sub, &subscriber, &native)
	if sub != "sub-1" || subscriber != "w1/a" || native {
		t.Fatalf("the seat: %q %q %v", sub, subscriber, native)
	}
	s.Store.Pool.QueryRow(ctx, `SELECT count(*) FROM seats WHERE game_id = $1`, started).Scan(&n)
	if n != 0 {
		t.Fatalf("a seat left in the game that started")
	}

	// Joining again: subscribed again, and a new subscription (the old
	// one ended) is recorded.
	again := ok
	again.subscribe = func(context.Context) (appkit.Subscription, error) {
		return appkit.Subscription{ID: "sub-2", User: "w1/a"}, nil
	}
	if g2, _, err := s.join(ctx, again, func(*Tx, State, Player) error { t.Fatal("seated again"); return nil }); err != nil || g2.base().ID != g.base().ID {
		t.Fatalf("rejoining: %v", err)
	}
	s.Store.Pool.QueryRow(ctx, `SELECT subscription FROM seats WHERE game_id = $1 AND player = $2`, g.base().ID, key).Scan(&sub)
	if sub != "sub-2" {
		t.Fatalf("after rejoining, the seat's subscription is %q", sub)
	}

	// A native caller (house agents, ChatGPT): a native seat.
	nat := joining{workspace: "w2", agent: "h", session: nativeSession(""), native: true, subscribe: func(context.Context) (appkit.Subscription, error) {
		return appkit.Subscription{User: "w2/h", Native: true}, nil
	}}
	if _, key, err = s.join(ctx, nat, seat); err != nil || key != "w2/h/native:" {
		t.Fatalf("a native join: %q %v", key, err)
	}
	s.Store.Pool.QueryRow(ctx, `SELECT subscription, native FROM seats WHERE player = $1`, key).Scan(&sub, &native)
	if sub != "" || !native {
		t.Fatalf("a native seat: %q %v", sub, native)
	}
}

// A client that subscribes itself may subscribe if it has any open seat,
// which marks every one native. Nobody else may, and its live subscription
// reaches every game.
func TestAuthorizeUpdates(t *testing.T) {
	ctx := context.Background()
	core := newRecCore()
	s := kitServer(t, "4", core)
	putGame(t, s, "g1", "raven:sub-1")
	putGame(t, s, "g2", "raven:sub-2")
	putGame(t, s, "g3", "otter:sub-3")
	native := func() []string {
		rows, _ := s.Store.Pool.Query(ctx, `SELECT game_id || ':' || handle FROM seats WHERE native ORDER BY 1`)
		got, _ := pgx.CollectRows(rows, pgx.RowTo[string])
		return got
	}
	raven := appkit.Subscriber{User: "w-raven/a", Native: true}
	if s.authorizeUpdates(ctx, appkit.Subscriber{User: "w-raven/a"}, UpdateArgs{}) {
		t.Fatal("a conversation subscribed itself")
	}
	if s.authorizeUpdates(ctx, appkit.Subscriber{User: "w-nobody/a", Native: true}, UpdateArgs{}) {
		t.Fatal("a client with no seat subscribed")
	}
	if !s.authorizeUpdates(ctx, raven, UpdateArgs{}) || !slices.Equal(native(), []string{"g1:raven", "g2:raven"}) {
		t.Fatalf("{}: %v", native())
	}

	subs := []any{}
	core.on("POST /api/apps/v1/event-subscriptions/list", func(map[string]any) (int, any) { return 200, map[string]any{"subscriptions": subs} })
	if s.subscribedTo(ctx, "g1", "w-raven/a") {
		t.Fatal("subscribed with no subscriptions")
	}
	subs = []any{map[string]any{"id": "x", "event": "game.update", "arguments": map[string]any{}, "destination": "stream"}}
	if s.subscribedTo(ctx, "g1", "w-raven/a") {
		t.Fatal("a conversation's subscription counted as native")
	}
	subs = []any{map[string]any{"id": "x", "event": "game.update", "arguments": map[string]any{}, "destination": "webhook"}}
	if !s.subscribedTo(ctx, "g1", "w-raven/a") || !s.subscribedTo(ctx, "g2", "w-raven/a") {
		t.Fatal("{} reaches every game")
	}
}

// A seat's end ends its subscription unless the conversation is seated in
// another open game.
func TestSeatEnded(t *testing.T) {
	ctx := context.Background()
	core := newRecCore()
	s := kitServer(t, "4", core)
	putGame(t, s, "g1", "raven:sub-1")
	putGame(t, s, "g2", "raven:sub-1", "otter:sub-2")
	if _, err := s.Store.Pool.Exec(ctx, `UPDATE games SET closed_at = now() WHERE id = 'g2'`); err != nil {
		t.Fatal(err)
	}
	if err := s.closeRoom(ctx, "g2"); err != nil {
		t.Fatal(err)
	}
	var ended []string
	for _, b := range core.bodies("/api/apps/v1/event-subscriptions/end") {
		ended = append(ended, b["user"].(string)+" "+b["id"].(string))
	}
	if !slices.Equal(ended, []string{"w-otter/a sub-2"}) {
		t.Fatalf("ended %v (raven is still seated in g1)", ended)
	}
}

// A game closing while one of its players joins the next game must
// not end the subscription that join just renewed and is about to record.
func TestCloseRoomDuringRejoin(t *testing.T) {
	ctx := context.Background()
	core := newRecCore()
	s := kitServer(t, "4", core)
	putGame(t, s, "old", "raven:sub-1")
	if _, err := s.Store.Pool.Exec(ctx, `UPDATE games SET ended_at = now(), closed_at = now() WHERE id = 'old'`); err != nil {
		t.Fatal(err)
	}
	j := joining{workspace: "w-raven", agent: "a", session: "s", subscribe: func(context.Context) (appkit.Subscription, error) {
		// Wakeshift renews the conversation's one game.update subscription;
		// the ticker closes the old game's room before the seat records it.
		if err := s.closeRoom(ctx, "old"); err != nil {
			t.Fatal(err)
		}
		return appkit.Subscription{ID: "sub-1", User: "w-raven/a"}, nil
	}}
	g, key, err := s.join(ctx, j, func(*Tx, State, Player) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	var sub string
	s.Store.Pool.QueryRow(ctx, `SELECT subscription FROM seats WHERE game_id = $1 AND player = $2`, g.base().ID, key).Scan(&sub)
	ended := core.bodies("/api/apps/v1/event-subscriptions/end")
	if sub == "sub-1" && len(ended) > 0 {
		t.Fatalf("the new seat records sub-1, which closing the old room ended: %v", ended)
	}
}
