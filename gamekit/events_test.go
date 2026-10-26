package gamekit

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/apps/internal/pgtest"
	"github.com/wakeshift/wakeshift-go/appkit"
)

// The game sends native seats nothing itself: what a player says is posted
// to the room, and Wakeshift brings it to those subscribed to game.update.
// A seat rejoined from a harness its room
// wakes stops being native, and one rejoined from ChatGPT becomes native
// again.
func TestNativeSeats(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	posts, emits := 0, 0
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/api/apps/v1/workspaces/w/agents/a/token":
			w.Write([]byte(`{"access_token":"tok","expires_in":900}`))
		case "/api/apps/v1/agent/events":
			posts++
			w.Write([]byte(`{"id":"e1"}`))
		case "/api/apps/v1/events/emit":
			emits++
			w.Write([]byte(`{"matched":1}`))
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
	s := &Server{Name: "g", Title: "G", Kit: &appkit.App{ID: "g", Key: key, Core: core.URL}, Store: st, Prefix: "app.g.game."}
	ag, err := s.Kit.ActAs(ctx, "w", "a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `INSERT INTO games (id, phase, max_seats, state) VALUES ('g1', 'playing', 3, '{}')`); err != nil {
		t.Fatal(err)
	}
	seat := func(player, workspace, agent, handle string, native bool) {
		if _, err := st.Pool.Exec(ctx, `INSERT INTO seats (game_id, player, workspace, agent, session, name, handle, reserved_at, seated_at, native, native_at)
			VALUES ('g1', $1, $2, $3, 's', $4, $4, now(), now(), $5, CASE WHEN $5 THEN now() END)`, player, workspace, agent, handle, native); err != nil {
			t.Fatal(err)
		}
	}
	seat("w/a/s", "w", "a", "raven", false)
	seat("x/gpt/s", "x", "gpt", "otter", true)

	if err := s.Say(ctx, ag, "g1", Player{Workspace: "w", Agent: "a", Session: "s", Handle: "raven"}, "hello"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	if posts != 1 || emits != 0 {
		t.Fatalf("posted %d, emitted %d (want 1, 0)", posts, emits)
	}
	mu.Unlock()

	native := func() bool {
		var n bool
		if err := st.Pool.QueryRow(ctx, `SELECT native FROM seats WHERE game_id = 'g1' AND player = 'x/gpt/s'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	req := func(ua string) *mcp.CallToolRequest {
		return &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{}, Extra: &mcp.RequestExtra{Header: http.Header{"User-Agent": {ua}}}}
	}
	view := s.Joined(ctx, req("claude-code/2.1.280"), "g1", "x/gpt/s", map[string]any{})
	if native() || view["subscribe"] != nil {
		t.Fatalf("rejoined from Claude Code: native %v, %v", native(), view)
	}
	view = s.Joined(ctx, req("openai-mcp/1.0.0"), "g1", "x/gpt/s", map[string]any{})
	if !native() || view["subscribe"] == nil {
		t.Fatalf("rejoined from ChatGPT: native %v, %v", native(), view)
	}
}
