package appkit

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The manifest the three SDKs send, and when (sdk/typescript/test/manifest.json):
// an explicit publish replaces it; the automatic send (from a process that
// serves the app's tools, Attach, or after an emit's unknown_event) merges
// the settings the app published and its events into the one Wakeshift has.
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
			Name         string   `json:"name"`
			Define       []string `json:"define"`
			UnknownEmits *int     `json:"unknown_emits"`
			Steps        []struct {
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
				mu      sync.Mutex
				got     = []request{}
				reply   *answer
				unknown int
			)
			if c.UnknownEmits != nil {
				unknown = *c.UnknownEmits
			}
			a, _ := testApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				path := strings.TrimPrefix(r.URL.Path, "/api/apps/v1")
				if path == "/events/emit" && c.UnknownEmits != nil {
					mu.Lock()
					got = append(got, request{r.Method, path, nil})
					refuse := unknown > 0
					if refuse {
						unknown--
					}
					mu.Unlock()
					if refuse {
						w.WriteHeader(http.StatusNotFound)
						io.WriteString(w, `{"error": {"code": "unknown_event", "message": "no such event"}}`)
						return
					}
					io.WriteString(w, `{"matched": 0}`)
					return
				}
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

// Emits that fail together, before Wakeshift has the event (an app
// emitting at startup, before any agent has connected), send the manifest
// once.
func TestConcurrentUnknownEmitsMergeOnce(t *testing.T) {
	var (
		mu                    sync.Mutex
		known                 bool
		merges, emits         int
		release               = make(chan struct{})
		allRefused, closeOnce = make(chan struct{}), sync.Once{}
	)
	const n = 5
	a, _ := testApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch strings.TrimPrefix(r.URL.Path, "/api/apps/v1") {
		case "/manifest/merge":
			<-release // until every first emit has been refused
			mu.Lock()
			merges++
			known = true
			mu.Unlock()
			io.WriteString(w, `{}`)
		case "/events/emit":
			mu.Lock()
			emits++
			refuse := !known
			if emits == n {
				closeOnce.Do(func() { close(allRefused) })
			}
			mu.Unlock()
			if refuse {
				w.WriteHeader(http.StatusNotFound)
				io.WriteString(w, `{"error": {"code": "unknown_event", "message": "no such event"}}`)
				return
			}
			io.WriteString(w, `{"matched": 0}`)
		}
	}))
	chat := defineSample(t, a, "chat.message").(*DefinedEvent[sampleChatArgs, sampleChat])
	go func() { <-allRefused; close(release) }()
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Go(func() {
			_, err := chat.Emit(context.Background(), sampleChatArgs{Channel: "eng"}, sampleChat{From: "ada", Text: "shipped"})
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if merges != 1 || emits != 2*n {
		t.Fatalf("%d merges and %d emits, want 1 and %d", merges, emits, 2*n)
	}
}

// Calls about events that wait for one manifest send share its outcome:
// when it fails, they fail with its error rather than each sending again,
// one after another (with Wakeshift slow, each up to a timeout). The next
// call sends again.
func TestFailedManifestSendIsShared(t *testing.T) {
	var mu sync.Mutex
	merges := 0
	a, _ := testApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/apps/v1/manifest/merge" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		merges++
		mu.Unlock()
		time.Sleep(300 * time.Millisecond) // the others queue meanwhile
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, `{"error": {"code": "unavailable", "message": "down"}}`)
	}))
	defineSample(t, a, "chat.message")
	a.attached = true
	const n = 5
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Go(func() { errs <- a.ensureManifest(context.Background()) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		var e *APIError
		if !errors.As(err, &e) || e.Code != "unavailable" {
			t.Fatalf("err = %v, want the send's unavailable", err)
		}
	}
	if merges != 1 {
		t.Fatalf("%d concurrent calls sent %d merges, want 1", n, merges)
	}
	if err := a.ensureManifest(context.Background()); err == nil || merges != 2 {
		t.Fatalf("the next call: %v, %d merges, want an error and 2", err, merges)
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
