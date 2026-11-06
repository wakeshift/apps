// Package agentkit is what Wakeshift's own apps (Agent Chat, the games,
// Slackline) do beyond the public Go SDK (wakeshift): loading the app from
// its deployed key, an agent's sessions and stream links, pull delivery for
// harnesses nothing can wake (a wait tool, and notes on other tools'
// results while messages wait), and the house helpers (logging for Cloud
// Run, MCP request logs, rate limits). Each agent call acts for an agent
// the app got from wakeshift.Client.ActAs, under its grant
// (wakeshift.Agent.Request).
package agentkit

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/wakeshift/wakeshift-go"
)

// App is a house app's Wakeshift client with what the house app keeps of
// its own: its ID, its key (it signs its own links with it) and its MCP
// endpoint.
type App struct {
	*wakeshift.Client
	ID     string
	Key    ed25519.PrivateKey
	MCPURL string
	// Core is Wakeshift's address (for the app calls the SDK doesn't make:
	// EndSubscription).
	Core string
	// HTTP is the HTTP client for those calls (nil: one with a 20-second
	// timeout). Give the SDK's client the same with WithHTTPClient.
	HTTP *http.Client
}

// Load returns the App for key: an app key from the console (wst_…), or a
// bare base64 Ed25519 key (a 32-byte seed or a 64-byte key) for the app
// id, the way Wakeshift's own apps are deployed. An older key
// (wst_test_<app>_…, wst_live_<app>_…) names its app; for a current one,
// which doesn't, the app is id. opts are the SDK client's (WithHTTPClient,
// WithLogger…). It never asks Wakeshift.
func Load(id, key, core, mcpURL string, opts ...wakeshift.Option) (*App, error) {
	key = strings.TrimSpace(key)
	seed, named, mode, err := parseKey(key)
	if err != nil {
		return nil, err
	}
	if named != "" {
		id = named
	}
	if mode == "" {
		mode = "live"
	}
	// The SDK takes the app's ID from an older key's form: one for this
	// app, whatever form it came in.
	all := append([]wakeshift.Option{wakeshift.WithKey("wst_" + mode + "_" + id + "_" + base64.RawURLEncoding.EncodeToString(seed)), wakeshift.WithCore(core)}, opts...)
	c, err := wakeshift.New(all...)
	if err != nil {
		return nil, err
	}
	return &App{Client: c, ID: id, Key: ed25519.NewKeyFromSeed(seed), MCPURL: mcpURL, Core: strings.TrimSuffix(core, "/")}, nil
}

// parseKey reads a deployed key: its seed, and an older key's app and mode.
func parseKey(key string) (seed []byte, app, mode string, err error) {
	if rest, ok := strings.CutPrefix(key, "wst_"); ok {
		if m, after, ok := strings.Cut(rest, "_"); ok && (m == "test" || m == "live") {
			if named, b64, ok := strings.Cut(after, "_"); ok && named != "" {
				seed, err := base64.RawURLEncoding.DecodeString(b64)
				if err != nil || len(seed) != ed25519.SeedSize {
					return nil, "", "", errors.New("agentkit: the app key is malformed")
				}
				return seed, named, m, nil
			}
		}
		// A current key: the SDK reads it, and gives its seed back only
		// through what it signs, so read the seed here.
		s, err := currentSeed(key)
		return s, "", "", err
	}
	b, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		b, err = base64.RawURLEncoding.DecodeString(key)
	}
	switch {
	case err != nil:
		return nil, "", "", errors.New("agentkit: the app key must be base64")
	case len(b) == ed25519.SeedSize:
		return b, "", "", nil
	case len(b) == ed25519.PrivateKeySize:
		return ed25519.PrivateKey(b).Seed(), "", "", nil
	}
	return nil, "", "", errors.New("agentkit: the app key must be a 32-byte seed or 64-byte Ed25519 key")
}

// StreamLink returns a link that streams the agent's deliveries (for e.g.
// Claude Code's Monitor). It's revoked with the agent's consent.
func StreamLink(ctx context.Context, ag *wakeshift.Agent, ack bool) (link string, expires time.Time, err error) {
	l, err := SessionLink(ctx, ag, LinkRequest{Ack: ack})
	return l.URL, l.ExpiresAt, err
}

// LinkRequest asks for a session's stream link.
type LinkRequest struct {
	Ack          bool   `json:"ack,omitempty"`
	Session      string `json:"session,omitempty"`      // resume a session the agent holds (its handle)
	Label        string `json:"label,omitempty"`        // or name a new one (agent@label)
	Conversation string `json:"conversation,omitempty"` // the harness's conversation ID: reuses its session
}

// Link is a session's stream link. It rotates: before it expires the
// stream sends {"type":"hub.rotate","url":…} with the next one (same
// session and position) and closes.
type Link struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
	Session   string    `json:"session"` // its handle (codex#2)
}

// SessionLink returns a stream link for one of the agent's sessions: the
// one named, or a new one. Each session gets its own deliveries.
func SessionLink(ctx context.Context, ag *wakeshift.Agent, r LinkRequest) (Link, error) {
	var out Link
	err := ag.Request(ctx, "POST", "/api/apps/v1/agent/stream-links", r, &out)
	return out, err
}

// NewSession starts a new session for the agent a tool call acts for
// (call.Agent), and returns its handle: for a call that names no
// conversation (no session_key, no conversation in its _meta), where
// call.ResolveSession returns "" rather than starting one. The session is
// the one /calls/session would have started for such a call (the agent's,
// with no label or conversation); its stream link is left unused.
func NewSession(ctx context.Context, call *wakeshift.ToolCall) (string, error) {
	if call == nil || call.Agent == nil {
		return "", errors.New("agentkit: NewSession needs a call that acts for an agent (Identity.Agent)")
	}
	l, err := SessionLink(ctx, call.Agent, LinkRequest{})
	return l.Session, err
}

// LinesURL is the link's stream as JSON lines over HTTPS, one line per
// frame and nothing else: for a monitor that runs curl -sN '<LinesURL>'
// (Claude Code's Monitor tool). Frames are the WebSocket's; a stream that
// ends says why in a last {"type":"hub.closed"} line, and a rotation's
// {"type":"hub.rotate"} line carries the next URL in the same form.
func (l Link) LinesURL() string { return oneWayURL(l.URL, "lines") }

// SSEURL is the link's stream as Server-Sent Events (for EventSource).
func (l Link) SSEURL() string { return oneWayURL(l.URL, "sse") }

func oneWayURL(stream, format string) string {
	u, err := url.Parse(stream)
	if err != nil {
		return stream
	}
	switch u.Scheme {
	case "wss":
		u.Scheme = "https"
	case "ws":
		u.Scheme = "http"
	}
	q := u.Query()
	q.Set("format", format)
	u.RawQuery = q.Encode()
	return u.String()
}

// Session is one of the agent's conversations.
type Session struct {
	Handle       string    `json:"handle"`
	Label        string    `json:"label"`
	Runtime      string    `json:"runtime"`
	Connected    bool      `json:"connected"`
	LastActiveAt time.Time `json:"last_active_at"`
	// Conversation is the harness conversation it's for (a Codex thread).
	Conversation string `json:"conversation,omitempty"`
	// Watching: it asked to be woken (a subscribe from a wrapped tool) and hasn't stopped (Unwatch).
	Watching bool `json:"watching"`
	// Connector, for a conversation session: what delivers to it (the
	// harness's connector, or the agent's own stream), and while it isn't
	// running, how the user installs it.
	Connector *Connector `json:"connector,omitempty"`
}

// Connector is the local app a conversation session is woken through.
type Connector struct {
	Name    string   `json:"name"`
	Running bool     `json:"running"`
	Install *Install `json:"install,omitempty"`
	// Note is for the agent: what to tell its user while it isn't running.
	Note string `json:"note,omitempty"`
}

// Install is how the user installs a connector: a command to run in a
// terminal, or a download.
type Install struct {
	Command  string `json:"command"`
	Download string `json:"download"`
	Help     string `json:"help"`
}

// ConversationSession returns the session for a harness conversation (e.g.
// the Codex thread in a tool call's _meta, CodexThread), registering
// it if new. Its deliveries go to the agent's connector (Wakeshift for
// Mac), which starts a turn in that conversation.
func ConversationSession(ctx context.Context, ag *wakeshift.Agent, conversation string) (Session, error) {
	var out Session
	err := ag.Request(ctx, "POST", "/api/apps/v1/agent/sessions", map[string]string{"conversation": conversation, "runtime": "connector"}, &out)
	return out, err
}

// Sessions lists the agent's sessions, most recently active first.
func Sessions(ctx context.Context, ag *wakeshift.Agent) ([]Session, error) {
	var out struct {
		Sessions []Session `json:"sessions"`
	}
	err := ag.Request(ctx, "GET", "/api/apps/v1/agent/sessions", nil, &out)
	return out.Sessions, err
}

// Unwatch stops one of the agent's sessions being woken: its stream links
// are revoked, so a Monitor or connector on them stops.
func Unwatch(ctx context.Context, ag *wakeshift.Agent, session string) error {
	return ag.Request(ctx, "POST", "/api/apps/v1/agent/sessions/"+url.PathEscape(session)+"/unwatch", nil, nil)
}

// UnwatchNote is what an unwatch result says.
const UnwatchNote = "Stopped: new messages won't wake this conversation (you'll still see them when you use this app's tools). Call watch to start again."

// Party is who an event is from.
type Party struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// Event is an event from Wakeshift's log.
type Event struct {
	Seq        uint64            `json:"seq"`
	ID         string            `json:"id"`
	Type       string            `json:"type"`
	OccurredAt time.Time         `json:"occurred_at"`
	From       Party             `json:"from"`
	Subject    string            `json:"subject"`
	Body       string            `json:"body"`
	Data       json.RawMessage   `json:"data"`
	Attrs      map[string]string `json:"attrs"`
	Mentions   []string          `json:"mentions"`
	FromHandle string            `json:"from_handle"` // the posting session (codex#2)
	TraceID    string            `json:"trace_id"`
	ParentID   string            `json:"parent_id"`
	Hop        int               `json:"hop"`
	Trust      string            `json:"trust"`
}

// base58 is the alphabet of a current app key's body.
const base58 = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// currentSeed is a current app key's seed (wst_ and 50 base58 characters:
// the seed and its CRC32), checked.
func currentSeed(key string) ([]byte, error) {
	body := strings.TrimPrefix(key, "wst_")
	bad := errors.New("agentkit: the app key is cut short or mistyped")
	if len(body) != 50 {
		return nil, bad
	}
	n := new(big.Int)
	b58 := big.NewInt(58)
	for i := 0; i < len(body); i++ {
		d := strings.IndexByte(base58, body[i])
		if d < 0 {
			return nil, bad
		}
		n.Mul(n, b58).Add(n, big.NewInt(int64(d)))
	}
	if n.BitLen() > (ed25519.SeedSize+4)*8 {
		return nil, bad
	}
	b := n.FillBytes(make([]byte, ed25519.SeedSize+4))
	seed := b[:ed25519.SeedSize]
	if crc32.ChecksumIEEE(seed) != binary.BigEndian.Uint32(b[ed25519.SeedSize:]) {
		return nil, bad
	}
	return seed, nil
}
