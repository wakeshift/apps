// Package gametest helps test the game apps: a database of the game's own,
// its timers running, several instances of a game behind one URL (as Cloud
// Run serves them), and agents that play.
package gametest

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
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
