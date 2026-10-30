package appkit

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/wakeshift-go/internal/mcpevents/event"
)

// mcp-events-wire.json (with the TypeScript and Python SDKs): the wire
// format every SDK serves a draft client, both sides of each
// events/subscribe and events/poll.
func TestMCPEventsWireGolden(t *testing.T) {
	type wireCase struct {
		Name         string          `json:"name"`
		Params       json.RawMessage `json:"params"`
		CoreRequest  map[string]any  `json:"core_request"`
		CoreResponse json.RawMessage `json:"core_response"`
		Result       map[string]any  `json:"result"`
	}
	var g struct {
		Revision  string     `json:"revision"`
		Delivery  []string   `json:"delivery"`
		Subscribe []wireCase `json:"subscribe"`
		Poll      []wireCase `json:"poll"`
	}
	readSharedFixture(t, "mcp-events-wire.json", &g)
	if g.Revision != event.Revision || !reflect.DeepEqual(g.Delivery, event.Delivery) {
		t.Fatalf("revision %q delivery %v, want %q %v", g.Revision, g.Delivery, event.Revision, event.Delivery)
	}
	for kind, cases := range map[string][]wireCase{"subscribe": g.Subscribe, "poll": g.Poll} {
		for _, c := range cases {
			var mu sync.Mutex
			var asked map[string]any
			core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path != "/api/apps/v1/events/"+kind {
					io.WriteString(w, `{}`)
					return
				}
				var body map[string]any
				json.NewDecoder(r.Body).Decode(&body)
				mu.Lock()
				asked = body
				mu.Unlock()
				w.Write(c.CoreResponse)
			}))
			_, key, _ := ed25519.GenerateKey(rand.Reader)
			app := &App{ID: "chat", Key: key, Core: core.URL}
			if _, err := DefineEvent(app, "chat.message", EventDef[wireArgs, wireMsg]{Description: "A new message in a channel",
				Text: func(m wireMsg, _ wireArgs) string { return m.From + ": " + m.Text }}); err != nil {
				t.Fatal(err)
			}
			attached := app.Attach(mcp.NewServer(&mcp.Implementation{Name: "chat", Version: "1"}, nil), AttachOptions{
				Identify: func(_ context.Context, ti *auth.TokenInfo) (Identity, error) { return Identity{User: ti.UserID}, nil }})
			h := auth.RequireBearerToken(func(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
				return &auth.TokenInfo{UserID: token, Expiration: time.Now().Add(time.Hour)}, nil
			}, nil)(attached.Wrap(http.NotFoundHandler()))
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "events/" + kind, "params": c.Params})
			req := httptest.NewRequest("POST", "/mcp", bytes.NewReader(b))
			req.Header.Set("Authorization", "Bearer u1")
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("User-Agent", "some-draft-client/1")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			core.Close()
			var resp struct {
				Result map[string]any `json:"result"`
				Error  any            `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Error != nil {
				t.Fatalf("%s: %s: %d %s", kind, c.Name, rec.Code, rec.Body)
			}
			if !reflect.DeepEqual(resp.Result, c.Result) {
				t.Errorf("%s: %s: result\n got %v\nwant %v", kind, c.Name, resp.Result, c.Result)
			}
			mu.Lock()
			call, _ := asked["call"].(map[string]any)
			delete(asked, "call")
			delete(asked, "dialect")
			if call["user"] != "u1" || !reflect.DeepEqual(asked, c.CoreRequest) {
				t.Errorf("%s: %s: asked Wakeshift (as %v)\n got %v\nwant %v", kind, c.Name, call["user"], asked, c.CoreRequest)
			}
			mu.Unlock()
		}
	}
}

type wireArgs struct {
	Channel string `json:"channel"`
	From    string `json:"from,omitempty"`
}

type wireMsg struct {
	From string `json:"from"`
	Text string `json:"text"`
}
