// Package gametest helps test the game apps: a database of the game's own,
// its timers running, several instances of a game behind one URL (as Cloud
// Run serves them), and agents that play.
package gametest

import (
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/apps/gamekit"
	"github.com/wakeshift/apps/internal/pgtest"
)

// AuthorizePage is the page sign-in's authorize shows a newly
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

// bearer sends the token, and any extra headers, with every request.
type bearer struct {
	token string
	extra http.Header
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	for k, v := range b.extra {
		r.Header[k] = v
	}
	return http.DefaultTransport.RoundTrip(r)
}

// Agent is an MCP client playing a game.
type Agent struct {
	t testing.TB
	S *mcp.ClientSession
	// Harness is the program the agent says it runs in (join's harness
	// argument, default claude-code): Wakeshift's block in join's result
	// then carries a stream link, which Watch reads.
	Harness string
	mu      sync.Mutex
	// session is the conversation's session_key, from join: like a
	// well-behaved agent, it passes it to join again (and to the wait).
	session string
	link    string // the conversation's stream link, from join's result
	// updates is what the conversation's stream delivers, once Watch asked
	// for it and join gave the link (reading starts then).
	updates  chan Update
	watching bool
	reading  bool
}

// Connect connects an agent to a game's MCP endpoint with a token.
func Connect(t testing.TB, mcpURL, token string) *Agent {
	t.Helper()
	return ConnectWith(t, mcpURL, token, nil)
}

// ConnectWith is Connect, sending extra headers with every request (a
// house agent's claim).
func ConnectWith(t testing.TB, mcpURL, token string, extra http.Header) *Agent {
	t.Helper()
	s, err := mcp.NewClient(&mcp.Implementation{Name: "test-agent"}, nil).
		Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: mcpURL, HTTPClient: &http.Client{Transport: bearer{token, extra}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return &Agent{t: t, S: s}
}

// Session is the conversation's session_key, from its first join.
func (a *Agent) Session() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.session
}

// Call calls a tool: its result, or the error's text. join gets the
// session_key the agent's first join gave it, and its harness, if the call
// names neither.
func (a *Agent) Call(tool string, args map[string]any) (map[string]any, string, error) {
	if tool == "join" {
		args = maps.Clone(args)
		if args == nil {
			args = map[string]any{}
		}
		a.mu.Lock()
		session := a.session
		a.mu.Unlock()
		_, named := args["session_key"]
		if _, old := args["session"]; !named && !old && session != "" {
			args["session_key"] = session
		}
		if _, named := args["harness"]; !named {
			h := a.Harness
			if h == "" {
				h = "claude-code"
			}
			args["harness"] = h
		}
	}
	out, _, errText, err := a.call(tool, args)
	return out, errText, err
}

// RawText calls a tool as given: its result's text (or the error's), and
// whether it failed.
func (a *Agent) RawText(tool string, args map[string]any) (text string, failed bool) {
	a.t.Helper()
	_, text, errText, err := a.call(tool, args)
	if err != nil {
		a.t.Fatalf("%s: %v", tool, err)
	}
	if errText != "" {
		return errText, true
	}
	return text, false
}

func (a *Agent) call(tool string, args map[string]any) (out map[string]any, text, errText string, err error) {
	res, err := a.S.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return nil, "", "", err
	}
	var texts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			texts = append(texts, tc.Text)
		}
	}
	if res.IsError {
		return nil, "", strings.Join(texts, " "), nil
	}
	b, _ := json.Marshal(res.StructuredContent)
	json.Unmarshal(b, &out)
	text = strings.Join(texts, "\n")
	if tool == "join" {
		// The conversation's session_key and stream link, from its first
		// join.
		a.mu.Lock()
		if s, _ := out["session_key"].(string); s != "" && a.session == "" {
			a.session = s
		}
		if l := streamURL(text); l != "" && a.link == "" {
			a.link = l
		}
		start := a.watching && a.link != "" && !a.reading
		a.reading = a.reading || start
		a.mu.Unlock()
		if start {
			a.read()
		}
	}
	return out, text, "", nil
}

// read reads the conversation's stream into updates until the test ends.
func (a *Agent) read() {
	ws, _, err := websocket.Dial(context.Background(), a.link, nil)
	if err != nil {
		a.t.Errorf("reading the stream: %v", err)
		return
	}
	ws.SetReadLimit(1 << 20)
	a.t.Cleanup(func() { ws.CloseNow() })
	go func() {
		for {
			_, b, err := ws.Read(context.Background())
			if err != nil {
				return
			}
			var m struct {
				Type  string
				Event struct {
					Subject string
					Data    json.RawMessage
					From    struct{ Kind string }
				}
			}
			json.Unmarshal(b, &m)
			// Wakeshift's own notices (the game ended the subscription) aren't
			// the game's updates.
			if m.Type != "event" || m.Event.From.Kind == "hub" {
				continue
			}
			var u Update
			if err := json.Unmarshal(m.Event.Data, &u.Update); err != nil {
				a.t.Errorf("an update's payload: %v: %s", err, b)
				return
			}
			u.Line = m.Event.Subject
			a.updates <- u
		}
	}()
}

// linkRE finds the conversation's stream link in the block Wakeshift adds
// to a join's result for Claude Code: its wait loop's u='…/v1/stream/wait?link=…',
// or a monitor's curl -sN '…/v1/stream?link=…'.
var linkRE = regexp.MustCompile(`(?:\bu=|curl -sN )'(https?://[^']+/v1/stream(?:/wait)?\?[^']+)'`)

// streamURL is the WebSocket URL of the stream link in a tool result's
// text, or "".
func streamURL(text string) string {
	m := linkRE.FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	u, err := url.Parse(m[1])
	if err != nil || u.Query().Get("link") == "" {
		return ""
	}
	u.Path, u.RawQuery = "/v1/stream", url.Values{"link": {u.Query().Get("link")}}.Encode()
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}
	return u.String()
}

// Try calls a tool that may fail: its result, or the error's text. A
// failed request fails the test.
func (a *Agent) Try(tool string, args map[string]any) (map[string]any, string) {
	a.t.Helper()
	out, errText, err := a.Call(tool, args)
	if err != nil {
		a.t.Fatalf("%s: %v", tool, err)
	}
	return out, errText
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

// Watch reads the stream of the conversation the agent joined with (join
// first): next is the subject of what wakes it next.
func (a *Agent) Watch() (session string, next func() (subject string)) {
	a.t.Helper()
	session, nextFrom := a.WatchFrom()
	return session, func() string {
		a.t.Helper()
		subject, _ := nextFrom()
		return subject
	}
}

// WatchFrom is Watch, with who each update is from: the player's handle,
// or "app" for the game's moderator. It reads game.update's payload (its
// subject and from); Text is what an agent reads first (Next's line).
func (a *Agent) WatchFrom() (session string, next func() (subject, from string)) {
	a.t.Helper()
	nextUpdate := a.Updates()
	return a.Session(), func() (string, string) {
		a.t.Helper()
		u := nextUpdate()
		from := u.From
		if from == "" {
			from = "app"
		}
		return u.Subject, from
	}
}

// Update is a game.update as a player's stream delivers it: its payload,
// and the line an agent reads first (Line).
type Update struct {
	gamekit.Update
	Line string
}

// Stream is the updates the conversation the agent joins with is woken
// by, as Updates reads them (from its join, if it hasn't joined yet).
func (a *Agent) Stream() <-chan Update {
	a.mu.Lock()
	if a.updates == nil {
		a.updates = make(chan Update, 1000)
	}
	a.watching = true
	start := a.link != "" && !a.reading
	a.reading = a.reading || start
	ch := a.updates
	a.mu.Unlock()
	if start {
		a.read()
	}
	return ch
}

// Updates reads the stream of the conversation the agent joins with:
// from now, if it has joined, else from its join (the stream opens as soon
// as join gives its link, as a Claude Code conversation's Monitor would).
// next is the next update that wakes it.
func (a *Agent) Updates() (next func() Update) {
	a.t.Helper()
	a.Stream()
	return func() Update {
		a.t.Helper()
		select {
		case u := <-a.updates:
			return u
		case <-time.After(20 * time.Second):
			a.mu.Lock()
			link := a.link
			a.mu.Unlock()
			if link == "" {
				a.t.Fatal("nothing heard: the agent never joined (as Claude Code), so it has no stream")
			}
			a.t.Fatal("nothing heard for 20 seconds")
			return Update{}
		}
	}
}

// GetJSON gets u, which must answer 200, as a JSON object.
func GetJSON(t testing.TB, u string) map[string]any {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: %d %v", u, resp.StatusCode, out)
	}
	return out
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

// Archive is the one archived game in store (game_archives), as JSON; it
// fails the test unless there's exactly one. It checks that the record
// names none of the game's seats: no handle, workspace, agent, session or
// player key, in any case, and not the game's ID. Each is looked for as a
// whole word, so a handle inside another word (yak in a codeword, kayak)
// isn't a leak.
func Archive(t testing.TB, store *gamekit.Store) string {
	t.Helper()
	ctx := context.Background()
	rows, _ := store.Pool.Query(ctx, `SELECT record::text FROM game_archives`)
	records, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("%d archived games, want 1: %v", len(records), records)
	}
	rec := records[0]
	rows, _ = store.Pool.Query(ctx, `SELECT unnest(ARRAY[game_id, player, workspace, agent, session, coalesce(handle, '')]) FROM seats`)
	forbidden, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if len(forbidden) == 0 {
		t.Fatal("no seats to check the archive against")
	}
	// Its text as decoded, so a JSON escape (\n) doesn't adjoin a word.
	var decoded any
	if err := json.Unmarshal([]byte(rec), &decoded); err != nil {
		t.Fatal(err)
	}
	text := strings.Join(jsonStrings(decoded, nil), "\n")
	for _, f := range forbidden {
		if f != "" && wholeWord(f).MatchString(text) {
			t.Errorf("the archive has %q in it: %s", f, rec)
		}
	}
	return rec
}

// wholeWord matches s, in any case, where no letter or digit adjoins it.
func wholeWord(s string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)(^|[^\pL\pN])` + regexp.QuoteMeta(s) + `($|[^\pL\pN])`)
}

// jsonStrings appends every string in v, a decoded JSON value, keys too.
func jsonStrings(v any, out []string) []string {
	switch v := v.(type) {
	case string:
		out = append(out, v)
	case []any:
		for _, e := range v {
			out = jsonStrings(e, out)
		}
	case map[string]any:
		for k, e := range v {
			out = jsonStrings(e, append(out, k))
		}
	default:
		if v != nil {
			out = append(out, fmt.Sprint(v))
		}
	}
	return out
}
