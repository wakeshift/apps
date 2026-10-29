package agentchat

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/apps/agentkit"
	"github.com/wakeshift/wakeshift-go/appkit"
)

// connectedAgents looks agents up at once, alongside answers it already
// has cached, and says which are connected. Run under -race: the lookups
// finish while cached answers are still going into the same result.
func TestConnectedAgentsConcurrent(t *testing.T) {
	// A fake Wakeshift: an agent's token is its name, and agents named
	// "on-…" have a connected session.
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/token"):
			parts := strings.Split(r.URL.Path, "/")
			json.NewEncoder(w).Encode(map[string]any{"access_token": parts[len(parts)-2], "expires_in": 3600})
		case r.Method == "GET" && r.URL.Path == "/api/apps/v1/agent/sessions":
			tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			json.NewEncoder(w).Encode(map[string]any{"sessions": []agentkit.Session{
				{Handle: "a"},
				{Handle: "b", Connected: strings.HasPrefix(tok, "on-")},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer core.Close()
	_, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	a := &App{Kit: &appkit.App{ID: "agentchat", Key: key, Core: core.URL, MCPURL: "https://chat.example.test/mcp"}}

	// Unique IDs, so other tests' cached answers don't interfere.
	prefix := fmt.Sprintf("race-%d-", time.Now().UnixNano())
	var agents []Agent
	want := map[string]bool{}
	// The lookups come first and a long run of cached answers after, so
	// lookups finish while cached answers are still being written.
	const lookups, cached = 16, 9_000 // under the cache's 10,000 limit
	for i := range lookups + cached {
		name := fmt.Sprintf("off-%d", i)
		if i%3 == 0 {
			name = fmt.Sprintf("on-%d", i)
		}
		id := prefix + name
		agents = append(agents, Agent{ID: id, Workspace: "ws", Agent: name})
		want[id] = strings.HasPrefix(name, "on-")
		// A cached answer is the opposite of what a lookup would say, so
		// the result shows which path it came from.
		if i >= lookups {
			statusMu.Lock()
			statusCache[id] = agentStatus{sessions: map[string]bool{"s#1": !want[id]}, at: time.Now()}
			statusMu.Unlock()
			want[id] = !want[id]
		}
	}
	t.Cleanup(func() {
		statusMu.Lock()
		defer statusMu.Unlock()
		for _, ag := range agents {
			delete(statusCache, ag.ID)
		}
	})

	for round := range 3 {
		got := a.connectedAgents(context.Background(), agents)
		if len(got) != len(want) {
			t.Fatalf("round %d: %d answers, want %d", round, len(got), len(want))
		}
		for id, w := range want {
			if got[id] != w {
				t.Errorf("round %d: %s connected = %v, want %v", round, id, got[id], w)
			}
		}
	}
}
