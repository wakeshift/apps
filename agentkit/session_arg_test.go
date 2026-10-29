package agentkit_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/apps/agentkit"
	"github.com/wakeshift/wakeshift-go/appkit"
)

// sessionCore is a fake core: it hands out an agent's token, and records
// the session each wait and inbox read names.
type sessionCore struct {
	mu   sync.Mutex
	seen []string
}

func (f *sessionCore) handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case strings.HasSuffix(r.URL.Path, "/token"):
		w.Write([]byte(`{"access_token": "tok", "expires_in": 3600}`))
	case r.URL.Path == "/api/apps/v1/agent/wait":
		var in struct{ Session string }
		json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		f.seen = append(f.seen, in.Session)
		f.mu.Unlock()
		w.Write([]byte(`{"session": "` + in.Session + `", "deliveries": []}`))
	case strings.HasSuffix(r.URL.Path, "/inbox"):
		session := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/apps/v1/agent/sessions/"), "/inbox")
		f.mu.Lock()
		f.seen = append(f.seen, session)
		f.mu.Unlock()
		w.Write([]byte(`{"session": "` + session + `", "pending": 0}`))
	default:
		http.NotFound(w, r)
	}
}

func (f *sessionCore) take() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.seen
	f.seen = nil
	return out
}

// The wait tool and InboxNotes take the session as session (what
// InboxNote tells the agent to pass) or session_key (what the app's
// wrapped tools call it); given both, session wins.
func TestSessionArgument(t *testing.T) {
	ctx := context.Background()
	core := &sessionCore{}
	hs := httptest.NewServer(http.HandlerFunc(core.handler))
	t.Cleanup(hs.Close)
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	app := &appkit.App{ID: "chat", Key: key, Core: hs.URL}
	agent := func(ctx context.Context, _ *mcp.CallToolRequest) (*appkit.Agent, error) {
		return app.ActAs(ctx, "ws_1", "claude-code")
	}

	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "1"}, nil)
	srv.AddReceivingMiddleware(agentkit.InboxNotes(agent, "", "other"))
	agentkit.WaitTool{Agent: agent}.Add(srv)
	mcp.AddTool(srv, &mcp.Tool{Name: "other"}, func(context.Context, *mcp.CallToolRequest, map[string]any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
	})
	ct, st := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })

	zero := 0
	for _, c := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"session": "claude-code#1"}, "claude-code#1"},
		{map[string]any{"session_key": "claude-code#2"}, "claude-code#2"},
		{map[string]any{"session": "claude-code#1", "session_key": "claude-code#2"}, "claude-code#1"},
	} {
		for _, tool := range []string{"wait_for_messages", "other"} {
			args := map[string]any{}
			for k, v := range c.args {
				args[k] = v
			}
			if tool == "wait_for_messages" {
				args["wait_seconds"] = zero
			}
			res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
			if err != nil || res.IsError {
				t.Fatalf("%s %v: %v %+v", tool, c.args, err, res)
			}
			if got := core.take(); len(got) != 1 || got[0] != c.want {
				t.Errorf("%s %v: session %v, want %s", tool, c.args, got, c.want)
			}
		}
	}
}
