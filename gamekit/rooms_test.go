package gamekit

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/wakeshift/apps/internal/pgtest"
	"github.com/wakeshift/wakeshift-go/appkit"
)

// What a player says is in the room's log exactly when it was posted: a
// post Wakeshift refuses is taken back out of the log, and a message that
// can't be logged isn't posted.
func TestSayLogsWhatIsPosted(t *testing.T) {
	ctx := context.Background()
	var refuse, posts atomic.Int32
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/apps/v1/workspaces/w/agents/a/token":
			w.Write([]byte(`{"access_token":"tok","expires_in":900}`))
		case "/api/apps/v1/agent/events":
			if refuse.Load() == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				w.Write([]byte(`{"error":{"code":"unavailable","message":"no"}}`))
				return
			}
			posts.Add(1)
			w.Write([]byte(`{"id":"e1"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(core.Close)
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	st, err := OpenStore(ctx, pgtest.EmptyURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	s := &Server{Name: "g", Kit: &appkit.App{ID: "g", Key: key, Core: core.URL}, Store: st, Prefix: "app.g.game."}
	ag, err := s.Kit.ActAs(ctx, "w", "a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `INSERT INTO games (id, phase, max_seats, state) VALUES ('g1', 'playing', 3, '{}')`); err != nil {
		t.Fatal(err)
	}
	p := Player{Workspace: "w", Agent: "a", Session: "s", Handle: "raven"}

	if err := s.Say(ctx, ag, "g1", p, "hello"); err != nil {
		t.Fatal(err)
	}
	refuse.Store(1)
	if err := s.Say(ctx, ag, "g1", p, "lost"); err == nil {
		t.Fatal("a refused post said nothing")
	}
	refuse.Store(0)
	if err := s.Say(ctx, ag, "nope", p, "no game to log it in"); err == nil {
		t.Fatal("a message that couldn't be logged was accepted")
	}
	log, err := s.Log(ctx, "g1")
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != 1 || log[0].Text != "hello" || log[0].From != "raven" || posts.Load() != 1 {
		t.Fatalf("log %+v, %d posted", log, posts.Load())
	}
}
