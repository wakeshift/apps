// Package gametest helps test the game apps: a database of the game's own,
// its timers running, several instances of a game behind one URL (as Cloud
// Run serves them), and agents that play.
package gametest

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/apps/gamekit"
	"github.com/wakeshift/apps/internal/pgtest"
)

// AuthorizePage is the page guest sign-in's authorize shows a newly
// registered client of the game at appBase (fails the test if it
// redirects instead).
func AuthorizePage(t testing.TB, appBase string) string {
	t.Helper()
	const redirect = "http://127.0.0.1:9/cb"
	resp, err := http.Post(appBase+"/oauth/register", "application/json", strings.NewReader(`{"client_name":"Claude Code","redirect_uris":["`+redirect+`"]}`))
	if err != nil {
		t.Fatal(err)
	}
	var reg struct {
		ClientID string `json:"client_id"`
	}
	json.NewDecoder(resp.Body).Decode(&reg)
	resp.Body.Close()
	q := url.Values{"response_type": {"code"}, "client_id": {reg.ClientID}, "redirect_uri": {redirect},
		"code_challenge": {"E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"}, "code_challenge_method": {"S256"}}
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if resp, err = noFollow.Get(appBase + "/oauth/authorize?" + q.Encode()); err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authorize: %d %s, not the page", resp.StatusCode, resp.Header.Get("Location"))
	}
	return string(b)
}

// Store opens a new, empty database for a game (dropped when the test
// ends); url is for opening more stores on it (more instances).
func Store(t testing.TB) (store *gamekit.Store, url string) {
	t.Helper()
	url = pgtest.EmptyURL(t)
	return Open(t, url), url
}

// Open opens a store on a game's database, closed when the test ends.
func Open(t testing.TB, url string) *gamekit.Store {
	t.Helper()
	s, err := gamekit.OpenStore(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

// Run runs a game's timers (its Run) until the test ends, or until the
// returned stop is called (an instance going away).
func Run(t testing.TB, run func(context.Context)) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); run(ctx) }()
	stopped := false
	stop = func() {
		if !stopped {
			stopped = true
			cancel()
			<-done
		}
	}
	t.Cleanup(stop)
	return stop
}

// Balancer serves several instances of a game behind one URL, a request
// to each in turn.
type Balancer struct {
	*httptest.Server
	backends []atomic.Pointer[http.Handler]
	next     atomic.Uint64
}

// NewBalancer starts a balancer for n instances; set each with Set.
func NewBalancer(t testing.TB, n int) *Balancer {
	b := &Balancer{backends: make([]atomic.Pointer[http.Handler], n)}
	b.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for range len(b.backends) {
			h := b.backends[b.next.Add(1)%uint64(len(b.backends))].Load()
			if h != nil {
				(*h).ServeHTTP(w, r)
				return
			}
		}
		http.Error(w, "no instance", http.StatusBadGateway)
	}))
	t.Cleanup(b.Close)
	return b
}

// Set makes h instance i (nil: it's down, and gets no requests).
func (b *Balancer) Set(i int, h http.Handler) {
	if h == nil {
		b.backends[i].Store(nil)
		return
	}
	b.backends[i].Store(&h)
}

// Proxy is a handler that forwards to a separate process at u.
func Proxy(u string) http.Handler {
	target, _ := url.Parse(u)
	return httputil.NewSingleHostReverseProxy(target)
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// Agent is an MCP client playing a game.
type Agent struct {
	t testing.TB
	S *mcp.ClientSession
	// session is the conversation's, from watch: like a well-behaved agent,
	// it passes it to join (and watches it again).
	mu      sync.Mutex
	session string
}

// Connect connects an agent to a game's MCP endpoint with a token.
func Connect(t testing.TB, mcpURL, token string) *Agent {
	t.Helper()
	s, err := mcp.NewClient(&mcp.Implementation{Name: "test-agent"}, nil).
		Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: mcpURL, HTTPClient: &http.Client{Transport: bearer{token}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return &Agent{t: t, S: s}
}

// Call calls a tool: its result, or the error's text. join and watch get
// the session the agent's watch started, if the call doesn't name one.
func (a *Agent) Call(tool string, args map[string]any) (map[string]any, string, error) {
	if _, named := args["session"]; !named && (tool == "join" || tool == "watch") {
		a.mu.Lock()
		session := a.session
		a.mu.Unlock()
		if session != "" {
			args = maps.Clone(args)
			if args == nil {
				args = map[string]any{}
			}
			args["session"] = session
		}
	}
	out, errText, err := a.RawCall(tool, args)
	if s, _ := out["session"].(string); tool == "watch" && s != "" {
		a.mu.Lock()
		if a.session == "" {
			a.session = s
		}
		a.mu.Unlock()
	}
	return out, errText, err
}

// RawCall calls a tool as given.
func (a *Agent) RawCall(tool string, args map[string]any) (map[string]any, string, error) {
	res, err := a.S.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return nil, "", err
	}
	if res.IsError {
		var text []string
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				text = append(text, tc.Text)
			}
		}
		return nil, strings.Join(text, " "), nil
	}
	b, _ := json.Marshal(res.StructuredContent)
	var out map[string]any
	json.Unmarshal(b, &out)
	return out, "", nil
}

// Must calls a tool that must succeed.
func (a *Agent) Must(tool string, args map[string]any) map[string]any {
	a.t.Helper()
	out, errText, err := a.Call(tool, args)
	if err != nil || errText != "" {
		a.t.Fatalf("%s: %v %s", tool, err, errText)
	}
	return out
}

// Watch starts the agent's session and reads its stream: next is the
// subject of what wakes it next.
func (a *Agent) Watch() (session string, next func() (subject string)) {
	a.t.Helper()
	w := a.Must("watch", nil)
	ws, _, err := websocket.Dial(context.Background(), w["url"].(string), nil)
	if err != nil {
		a.t.Fatal(err)
	}
	ws.SetReadLimit(1 << 20)
	a.t.Cleanup(func() { ws.CloseNow() })
	return w["session"].(string), func() string {
		a.t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		for {
			_, b, err := ws.Read(ctx)
			if err != nil {
				a.t.Fatal(err)
			}
			var m struct {
				Type  string
				Event struct{ Subject string }
			}
			json.Unmarshal(b, &m)
			if m.Type == "event" {
				return m.Event.Subject
			}
		}
	}
}

// Put writes a game as it stands (its state, as JSON: a pointer to the
// game's own struct), skipping play: open takes players; ended has ended.
// Then what each of said says, in order (moderator: from "").
func Put(t testing.TB, store *gamekit.Store, id, phase string, open, ended bool, state any, said ...gamekit.Entry) {
	t.Helper()
	b, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var endedAt *time.Time
	if ended {
		now := time.Now()
		endedAt = &now
	}
	ctx := context.Background()
	if _, err := store.Pool.Exec(ctx, `INSERT INTO games (id, phase, open, max_seats, state, ended_at) VALUES ($1, $2, $3, 8, $4, $5)`,
		id, phase, open, b, endedAt); err != nil {
		t.Fatal(err)
	}
	for _, e := range said {
		if _, err := store.Pool.Exec(ctx, `INSERT INTO room_log (game_id, at, from_handle, text, posted_at) VALUES ($1, now(), $2, $3, now())`, id, e.From, e.Text); err != nil {
			t.Fatal(err)
		}
	}
}

// Think writes a player's (Player.Key) operator note in game id, as a
// move's note argument would.
func Think(t testing.TB, store *gamekit.Store, id, player, text string) {
	t.Helper()
	if _, err := store.Pool.Exec(context.Background(), `INSERT INTO thinking (game_id, player, at, tool, text) VALUES ($1, $2, now(), 'say', $3)`, id, player, text); err != nil {
		t.Fatal(err)
	}
}

// Note checks a tool's input struct ends with the operator note argument,
// described as gamekit.NoteDescription, after the move's own arguments.
func Note(t testing.TB, in any) {
	t.Helper()
	typ := reflect.TypeOf(in)
	if typ.Kind() != reflect.Struct || typ.NumField() < 2 {
		t.Fatalf("%T isn't a move's input", in)
	}
	f := typ.Field(typ.NumField() - 1)
	if f.Name != "Note" || f.Tag.Get("json") != gamekit.NoteArg+",omitempty" || f.Tag.Get("jsonschema") != gamekit.NoteDescription {
		t.Errorf("%T: the last argument is %s %q, not note as gamekit.NoteDescription", in, f.Name, f.Tag)
	}
}

// asksReasoning matches what makes a model's safeguards take a tool argument
// as asking for its reasoning (Opus refuses every request that carries such
// a tool list: reasoning_extraction).
var asksReasoning = regexp.MustCompile(`(?i)reason|thought|thinking|private`)

// PublicTools checks a game's tools/list as a client sees it: with the
// operator note on, each tool in moves offers note, described exactly as
// gamekit.NoteDescription; off, no tool does. Either way, no argument's
// name or description, and no tool's description, asks for reasoning,
// thoughts or anything private.
func PublicTools(t testing.TB, s *mcp.ClientSession, moves []string, on bool) {
	t.Helper()
	res, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, tool := range res.Tools {
		seen[tool.Name] = true
		if asksReasoning.MatchString(tool.Description) {
			t.Errorf("%s's description: %q", tool.Name, tool.Description)
		}
		b, _ := json.Marshal(tool.InputSchema)
		var schema struct {
			Properties map[string]struct {
				Description string `json:"description"`
			} `json:"properties"`
		}
		json.Unmarshal(b, &schema)
		for name, p := range schema.Properties {
			if asksReasoning.MatchString(name) || asksReasoning.MatchString(p.Description) {
				t.Errorf("%s.%s: %q", tool.Name, name, p.Description)
			}
		}
		note, has := schema.Properties[gamekit.NoteArg]
		switch move := slices.Contains(moves, tool.Name); {
		case on && move && (!has || note.Description != gamekit.NoteDescription):
			t.Errorf("%s: note %v %q", tool.Name, has, note.Description)
		case has && (!on || !move):
			t.Errorf("%s offers note (on: %v)", tool.Name, on)
		}
	}
	for _, m := range moves {
		if !seen[m] {
			t.Errorf("no tool %s", m)
		}
	}
}

// Spectated is everything a spectator can read of a game app at base (the
// listing, as a page and as JSON, and the landing page), and of game id
// (its public page and state), by name. Each page must have the strict
// policy: this origin's script and styles only, nothing inline.
func Spectated(t testing.TB, base, id string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for name, path := range map[string]string{"landing": "/", "list": "/games", "api": "/api/games", "page": "/games/" + id, "state": "/games/" + id + "/state"} {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, resp.StatusCode, b)
		}
		out[name] = string(b)
		if name == "list" || name == "page" {
			csp := resp.Header.Get("Content-Security-Policy")
			if !strings.Contains(csp, "script-src 'self';") || !strings.Contains(csp, "style-src 'self';") || strings.Contains(csp, "unsafe") ||
				strings.Contains(out[name], "<script>") || strings.Contains(out[name], "<style") || strings.Contains(out[name], " style=") {
				t.Fatalf("GET %s: not strict (CSP %q): %s", path, csp, b)
			}
		}
	}
	return out
}
