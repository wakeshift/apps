package gamekit

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/apps/internal/pgtest"
	"github.com/wakeshift/wakeshift-go/appkit"
)

// Reasoning is cleaned to one line and cut to ReasoningMax characters.
func TestCleanReasoning(t *testing.T) {
	if got := CleanReasoning("  a\n\tb\u202ec  "); got != "a b c" {
		t.Errorf("cleaned %q", got)
	}
	if got := CleanReasoning(" \n "); got != "" {
		t.Errorf("blank %q", got)
	}
	long := CleanReasoning(strings.Repeat("é", 2000))
	if n := len([]rune(long)); n != ReasoningMax || !strings.HasSuffix(long, "…") {
		t.Errorf("long: %d runes", n)
	}
}

// A player's reasoning is kept with its game, capped per player, shown on
// that player's own follow-along page (page and state) and nobody else's,
// never on the public page, and goes when its game does or grows old.
func TestThinkingOnlyForItsOperator(t *testing.T) {
	ctx := context.Background()
	st, err := OpenStore(ctx, pgtest.EmptyURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	me := Player{Workspace: "w", Agent: "a", Session: "s", Handle: "raven"}
	other := Player{Workspace: "w2", Agent: "b", Session: "s", Handle: "otter"}
	now := time.Now()
	s := &Server{Name: "g", Kit: &appkit.App{ID: "g", Key: key}, Store: st, Now: func() time.Time { return now },
		Follow: func(_ context.Context, game, player string) (View, error) {
			return View{Game: game, Status: "playing", Players: []ViewPlayer{{Handle: "raven"}, {Handle: "otter"}}}, nil
		},
		Spectate: func(g State) (View, bool) {
			return View{Status: "playing", Players: []ViewPlayer{{Handle: "raven"}, {Handle: "otter"}}, Thinking: []Thought{{Text: "LEAKED-BY-GAME"}}}, true
		},
		NewState: func() State { return &Game{} }}
	hs := httptest.NewServer(s.Handler(func(*mcp.Server) {}))
	t.Cleanup(hs.Close)
	s.Kit.MCPURL = hs.URL + "/mcp"
	if _, err := st.Pool.Exec(ctx, `INSERT INTO games (id, phase, max_seats, state) VALUES ('g1', 'playing', 3, '{"id":"g1","phase":"playing"}')`); err != nil {
		t.Fatal(err)
	}

	s.ThinkAfterSay(ctx, "g1", me.Key(), "say", "  I hold the SECRET-PLAN card\n")
	s.ThinkAfterSay(ctx, "g1", me.Key(), "vote", "")
	s.ThinkAfterSay(ctx, "nope", me.Key(), "say", "no such game: dropped, not a failure")
	s.ThinkAfterSay(ctx, "g1", other.Key(), "say", "OTHER-PLAYER-PLAN")

	_, mine := get(t, s.FollowURL("g1", me)+"/state")
	var v View
	if err := json.Unmarshal([]byte(mine), &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Thinking) != 1 || v.Thinking[0].Text != "I hold the SECRET-PLAN card" || v.Thinking[0].Tool != "say" {
		t.Fatalf("my follow-along state: %s", mine)
	}
	if strings.Contains(mine, "OTHER-PLAYER-PLAN") {
		t.Fatalf("another player's reasoning on my page: %s", mine)
	}
	_, theirs := get(t, s.FollowURL("g1", other)+"/state")
	if strings.Contains(theirs, "SECRET-PLAN") || !strings.Contains(theirs, "OTHER-PLAYER-PLAN") {
		t.Fatalf("the other player's follow-along state: %s", theirs)
	}
	for _, path := range []string{"/games/g1", "/games/g1/state", "/api/games", "/games", "/api/lobby"} {
		_, body := get(t, hs.URL+path)
		for _, secret := range []string{"SECRET-PLAN", "OTHER-PLAYER-PLAN", "LEAKED-BY-GAME", `"thinking"`} {
			if strings.Contains(body, secret) {
				t.Errorf("%s shows %q", path, secret)
			}
		}
	}

	// At most thinkingKeep entries per player in a game: the oldest go.
	for i := range thinkingKeep + 5 {
		s.ThinkAfterSay(ctx, "g1", me.Key(), "say", fmt.Sprintf("plan %d", i))
	}
	th, err := s.Thinking(ctx, "g1", me.Key())
	if err != nil || len(th) != thinkingKeep || th[len(th)-1].Text != fmt.Sprintf("plan %d", thinkingKeep+4) {
		t.Fatalf("%d kept (%v)", len(th), err)
	}

	// Old reasoning is forgotten; the rest goes with its game.
	now = now.Add(thinkingFor + time.Minute)
	if err := s.forgetThinking(ctx, now); err != nil {
		t.Fatal(err)
	}
	var n int
	st.Pool.QueryRow(ctx, `SELECT count(*) FROM thinking`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d old entries left", n)
	}
	s.ThinkAfterSay(ctx, "g1", other.Key(), "say", "fresh")
	st.Pool.Exec(ctx, `DELETE FROM games WHERE id = 'g1'`)
	st.Pool.QueryRow(ctx, `SELECT count(*) FROM thinking`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d entries outlived their game", n)
	}
}
