package appkit

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// The manifest the three SDKs send, and when (sdk/typescript/test/manifest.json):
// automatically, with the settings Wakeshift has or the app last published,
// its listed events then the defined ones, refusing a name in both.
func TestManifestGolden(t *testing.T) {
	var fixture struct {
		Emit struct {
			Arguments sampleChatArgs `json:"arguments"`
			Payload   sampleChat     `json:"payload"`
		} `json:"emit"`
		Cases []struct {
			Name   string          `json:"name"`
			Define []string        `json:"define"`
			Stored json.RawMessage `json:"stored"`
			Steps  []struct {
				Emit    bool      `json:"emit"`
				Publish *Manifest `json:"publish"`
				Define  string    `json:"define"`
				Answer  *struct {
					Status int             `json:"status"`
					Body   json.RawMessage `json:"body"`
				} `json:"answer"`
				Error string `json:"error"`
			} `json:"steps"`
			Requests []struct {
				Method string         `json:"method"`
				Body   map[string]any `json:"body"`
			} `json:"requests"`
		} `json:"cases"`
	}
	readSharedFixture(t, "manifest.json", &fixture)
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			type request struct {
				Method string
				Body   any
			}
			var (
				mu     sync.Mutex
				got    = []request{}
				answer *struct {
					Status int             `json:"status"`
					Body   json.RawMessage `json:"body"`
				}
			)
			stored := c.Stored
			if stored == nil {
				stored = json.RawMessage(`{"description": ""}`)
			}
			a, _ := testApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.TrimPrefix(r.URL.Path, "/api/apps/v1") != "/manifest" {
					io.WriteString(w, `{"matched": 0}`)
					return
				}
				var body any
				b, _ := io.ReadAll(r.Body)
				json.Unmarshal(b, &body)
				mu.Lock()
				got = append(got, request{r.Method, body})
				reply := answer
				answer = nil
				mu.Unlock()
				switch {
				case r.Method == "GET":
					w.Write(stored)
				case reply != nil:
					w.WriteHeader(reply.Status)
					w.Write(reply.Body)
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
				case s.Emit:
					_, err = chat["chat.message"].Emit(context.Background(), fixture.Emit.Arguments, fixture.Emit.Payload)
				case s.Publish != nil:
					mu.Lock()
					answer = s.Answer
					mu.Unlock()
					err = a.PublishManifest(context.Background(), *s.Publish)
				case s.Define != "":
					define(s.Define)
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
				want = append(want, request{r.Method, body})
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
