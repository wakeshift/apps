// Package agentkit is what Wakeshift's own apps (Agent Chat, the games,
// Slackline) do for their users' agents beyond the public Go SDK (appkit):
// an agent's sessions and stream
// links, pull delivery for harnesses nothing can wake (a wait tool, and
// notes on other tools' results while messages wait), and loading the app
// from its deployed key. Each call acts for an agent the app got from
// appkit.App.ActAs, under its grant (appkit.Agent.Request).
package agentkit

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"time"

	"github.com/wakeshift/wakeshift-go/appkit"
)

// Load returns the App for key: an app key from the console (wst_…), or a
// bare base64 Ed25519 key (appkit.ParseKey) for the app id, the way
// Wakeshift's own apps are deployed. An older key (wst_test_<app>_…,
// wst_live_<app>_…) names its app; for a current one, which doesn't, the
// app is id. It never asks Wakeshift.
func Load(id, key, core, mcpURL string) (*appkit.App, error) {
	k, err := appkit.ParseKey(key)
	if err != nil {
		return nil, err
	}
	// ParseKey has checked an older key's format: wst_<test|live>_<app id>_<seed>,
	// where app IDs have no underscores.
	if rest, ok := strings.CutPrefix(strings.TrimSpace(key), "wst_"); ok {
		if mode, after, ok := strings.Cut(rest, "_"); ok && (mode == "test" || mode == "live") {
			if named, _, ok := strings.Cut(after, "_"); ok && named != "" {
				id = named
			}
		}
	}
	return &appkit.App{ID: id, Key: k, Core: core, MCPURL: mcpURL}, nil
}

// StreamLink returns a link that streams the agent's deliveries (for e.g.
// Claude Code's Monitor). It's revoked with the agent's consent.
func StreamLink(ctx context.Context, ag *appkit.Agent, ack bool) (link string, expires time.Time, err error) {
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
func SessionLink(ctx context.Context, ag *appkit.Agent, r LinkRequest) (Link, error) {
	var out Link
	err := ag.Request(ctx, "POST", "/api/apps/v1/agent/stream-links", r, &out)
	return out, err
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
	// Connector, for a conversation session: the local app that delivers
	// to it (Wakeshift for Mac), and while it isn't running, how the user
	// installs it.
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
// the Codex thread in a tool call's _meta, appkit.CodexThread), registering
// it if new. Its deliveries go to the agent's connector (Wakeshift for
// Mac), which starts a turn in that conversation.
func ConversationSession(ctx context.Context, ag *appkit.Agent, conversation string) (Session, error) {
	var out Session
	err := ag.Request(ctx, "POST", "/api/apps/v1/agent/sessions", map[string]string{"conversation": conversation, "runtime": "connector"}, &out)
	return out, err
}

// Sessions lists the agent's sessions, most recently active first.
func Sessions(ctx context.Context, ag *appkit.Agent) ([]Session, error) {
	var out struct {
		Sessions []Session `json:"sessions"`
	}
	err := ag.Request(ctx, "GET", "/api/apps/v1/agent/sessions", nil, &out)
	return out.Sessions, err
}

// Unwatch stops one of the agent's sessions being woken: its stream links
// are revoked, so a Monitor or connector on them stops.
func Unwatch(ctx context.Context, ag *appkit.Agent, session string) error {
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
