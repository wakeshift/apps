package appkit

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The manifest the three SDKs send, and when (sdk/typescript/test/manifest.json):
// an explicit publish replaces it; the automatic send, only from a process
// that serves the app's tools (Attach), merges the settings the app
// published and its events into the one Wakeshift has.
func TestManifestGolden(t *testing.T) {
	type answer struct {
		Status int             `json:"status"`
		Body   json.RawMessage `json:"body"`
	}
	var fixture struct {
		Emit struct {
			Arguments sampleChatArgs `json:"arguments"`
			Payload   sampleChat     `json:"payload"`
		} `json:"emit"`
		Cases []struct {
			Name   string   `json:"name"`
			Define []string `json:"define"`
			Steps  []struct {
				Attach   bool      `json:"attach"`
				Emit     bool      `json:"emit"`
				Status   bool      `json:"status"`
				Forget   bool      `json:"forget_user"`
				Publish  *Manifest `json:"publish"`
				Define   string    `json:"define"`
				FailNext bool      `json:"fail_next"`
				Answer   *answer   `json:"answer"`
				Error    string    `json:"error"`
			} `json:"steps"`
			Requests []struct {
				Method string         `json:"method"`
				Path   string         `json:"path"`
				Body   map[string]any `json:"body"`
			} `json:"requests"`
		} `json:"cases"`
	}
	readSharedFixture(t, "manifest.json", &fixture)
	unavailable := &answer{Status: 503, Body: json.RawMessage(`{"error": {"code": "unavailable", "message": "try again"}}`)}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			type request struct {
				Method string
				Path   string
				Body   any
			}
			var (
				mu    sync.Mutex
				got   = []request{}
				reply *answer
			)
			a, _ := testApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				path := strings.TrimPrefix(r.URL.Path, "/api/apps/v1")
				if path != "/manifest" && !strings.HasPrefix(path, "/manifest/") {
					io.WriteString(w, `{"matched": 0, "subscriptions": 0, "connected": 0, "forgotten": false}`)
					return
				}
				var body any
				b, _ := io.ReadAll(r.Body)
				json.Unmarshal(b, &body)
				mu.Lock()
				got = append(got, request{r.Method, path, body})
				next := reply
				reply = nil
				mu.Unlock()
				switch {
				case r.Method == "GET":
					io.WriteString(w, `{"description": ""}`)
				case next != nil:
					w.WriteHeader(next.Status)
					w.Write(next.Body)
				default:
					w.Write(b)
				}
			}))
			chat := map[string]*DefinedEvent[sampleChatArgs, sampleChat]{}
			define := func(name string) {
				t.Helper()
				ev := defineSample(t, a, name)
				if c, ok := ev.(*DefinedEvent[sampleChatArgs, sampleChat]); ok {
					chat[name] = c
				}
			}
			for _, name := range c.Define {
				define(name)
			}
			for i, s := range c.Steps {
				var err error
				switch {
				case s.Attach:
					a.Attach(mcp.NewServer(&mcp.Implementation{Name: "sample", Version: "1"}, nil), AttachOptions{})
				case s.Emit:
					_, err = chat["chat.message"].Emit(context.Background(), fixture.Emit.Arguments, fixture.Emit.Payload)
				case s.Status:
					_, err = chat["chat.message"].Status(context.Background(), fixture.Emit.Arguments)
				case s.Forget:
					_, err = a.ForgetUser(context.Background(), "ada")
				case s.Publish != nil:
					mu.Lock()
					reply = s.Answer
					mu.Unlock()
					err = a.PublishManifest(context.Background(), *s.Publish)
				case s.Define != "":
					define(s.Define)
				case s.FailNext:
					mu.Lock()
					reply = unavailable
					mu.Unlock()
				}
				switch {
				case s.Error == "" && err != nil:
					t.Fatalf("step %d: %v", i, err)
				case s.Error != "" && (err == nil || !strings.Contains(err.Error(), s.Error)):
					t.Fatalf("step %d: %v, want an error saying %q", i, err, s.Error)
				}
			}
			entries := map[string]any{}
			for _, ev := range a.events() {
				entries[ev.name] = canonical(t, ev.manifestEntry())
			}
			want := []request{}
			for _, r := range c.Requests {
				var body any
				if r.Body != nil {
					if evs, ok := r.Body["events"].([]any); ok {
						for i, e := range evs {
							if name, ok := e.(string); ok {
								evs[i] = entries[name]
							}
						}
					}
					body = r.Body
				}
				want = append(want, request{r.Method, r.Path, body})
			}
			mu.Lock()
			defer mu.Unlock()
			if g, w := canonical(t, got), canonical(t, want); !jsonEqual(g, w) {
				gb, _ := json.MarshalIndent(g, "", "  ")
				wb, _ := json.MarshalIndent(w, "", "  ")
				t.Fatalf("manifest requests:\n%s\nwant:\n%s", gb, wb)
			}
		})
	}
}

// A call about events from a process with none defined never sends the
// manifest, even one marked as serving (Attach refuses with none): an
// empty list would say the app has no events.
func TestNoEventsNeverSendsTheManifest(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	a, _ := testApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+strings.TrimPrefix(r.URL.Path, "/api/apps/v1"))
		mu.Unlock()
		io.WriteString(w, `{}`)
	}))
	for _, serving := range []bool{false, true} {
		a.mu.Lock()
		a.attached = serving
		a.mu.Unlock()
		if err := a.eventsCall(context.Background(), "GET", "/api/apps/v1/manifest", nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(paths, ","); got != "GET /manifest,GET /manifest" {
		t.Fatalf("requests: %s", got)
	}
}

// defineSample defines one of events-list.json's sample event types.
func defineSample(t *testing.T, a *App, name string) any {
	t.Helper()
	var ev any
	var err error
	switch name {
	case "chat.message":
		ev, err = DefineEvent(a, "chat.message", EventDef[sampleChatArgs, sampleChat]{
			Title: "Chat message", Description: "A new message in a channel.",
			Text: func(m sampleChat, _ sampleChatArgs) string { return m.From + ": " + m.Text }})
	case "game.turn":
		ev, err = DefineEvent(a, "game.turn", EventDef[sampleTurnArgs, sampleTurn]{
			Description: "It's your move at a table.", Personal: true,
			Text: func(tu sampleTurn, a sampleTurnArgs) string { return "Your move at " + a.Table }})
	case "ci.run":
		ev, err = DefineEvent(a, "ci.run", EventDef[sampleRunArgs, sampleRun]{
			Description: "A CI run on a pull request finished.",
			Text:        func(r sampleRun, a sampleRunArgs) string { return "CI " + r.Status }})
	default:
		t.Fatalf("no sample event %s", name)
	}
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
