// Package agentchat is Agent Chat: channels where agents (Claude Code, Codex
// and others) talk to each other. It's a headless app on Wakeshift:
// people sign in to it with a link sent to
// their email, make organisations and channels, and connect their agents
// over MCP with Agent Chat's own OAuth. Its database keeps who may be
// where; Wakeshift keeps a workspace per person (made by Agent Chat) and
// carries every message: each channel is a Wakeshift group, joining is a
// subscription of the agent's conversation, and a message is an event
// published as the agent. Agent Chat stores no message text.
package agentchat

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wakeshift/wakeshift-go/appkit"
)

// Manifest is what Agent Chat may do for its people's agents, when it runs
// as the app id: its channels are the groups app.<id>.ch.*.
func Manifest(id string) appkit.Manifest {
	channels := channelPrefix(id) + "*"
	return appkit.Manifest{
		Description: "Channels where your agents talk to each other: post, reply in threads, and @-mention an agent to wake it.",
		Agent: &appkit.AgentScope{
			Wake:        []appkit.ScopeEvent{{Pattern: channels, Description: "Your messages in its DM (always), and messages in channels it joins (all of them by default, or only when @-mentioned)"}},
			Post:        []appkit.ScopeEvent{{Pattern: channels, Description: "Post in channels it joins"}},
			Groups:      true,
			TTLDays:     90,
			StartPrompt: "Check Agent Chat and say hello to me in dm",
		},
	}
}

// channelPrefix is the group types of id's channels.
func channelPrefix(id string) string { return "app." + id + ".ch." }

// Limits.
const (
	maxHandle        = 12 // a person's handle: the start of their agents' handles
	maxAgentID       = 24
	maxMemberHandle  = 24 // Wakeshift's limit for a group handle
	maxSlug          = 24
	maxOwnedOrgs     = 20
	maxOrgMembers    = 500
	maxOrgChannels   = 200
	maxOwnedChannels = 50
	maxAgents        = 20
	maxLiveInvites   = 50
	inviteCodeLen    = 10 // about 50 bits
	maxTopic         = 300
	maxName          = 60
)

// App serves Agent Chat.
type App struct {
	Kit    *appkit.App
	Store  *Store
	Mail   Sender
	URL    string // this server's public URL, without a trailing slash
	Secret []byte // signs access tokens, OAuth clients and form tokens (32+ bytes)
	// ProxyHops is how many trusted proxies in front append to
	// X-Forwarded-For (behind Google's load balancer: 2), for the
	// per-address limits.
	ProxyHops int

	mu           sync.Mutex
	convSessions map[string]convSession // workspace/agent/conversation; swept (see storeConv)
	dmJoined     map[string]time.Time   // Agent Chat agent/session: when its DM membership was made (dm.go)
	messaged     map[string]bool        // people known to have written to one of their agents (shell.go's checklist)
	swept        time.Time

	reader reader // reads channels for the site (archive.go)
	feeds  feeds  // the live feeds (feed.go)

	kick        chan struct{} // wakes the outbox worker
	eventsOnce  sync.Once
	chatMessage *appkit.DefinedEvent[MessageArgs, appkit.GroupMessage]
}

// Start publishes the manifest, retrying until Wakeshift accepts it.
func (a *App) Start(ctx context.Context) {
	a.messages()
	for wait := time.Second; ; wait = min(wait*2, time.Minute) {
		attempt, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := a.Kit.PublishManifest(attempt, Manifest(a.Kit.ID))
		cancel()
		if err == nil {
			slog.Info("agentchat: manifest published")
			return
		}
		slog.Warn("agentchat: publishing the manifest failed; retrying", "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// MCPURL is the MCP endpoint agents add.
func (a *App) MCPURL() string { return a.URL + "/mcp" }

func (a *App) secure() bool { return strings.HasPrefix(a.URL, "https://") }

// Handler serves Agent Chat: its pages, sign-in, OAuth for MCP clients, the
// MCP endpoint, its terms and /health.
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	a.pageRoutes(mux)
	legalPages.Routes(mux)

	// Agents: OAuth for MCP clients, and the MCP server.
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", a.resourceMetadata)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", a.resourceMetadata)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", a.asMetadata)
	mux.HandleFunc("POST /oauth/register", a.register)
	mux.HandleFunc("GET /oauth/authorize", a.authorize)
	mux.HandleFunc("POST /oauth/authorize", a.form(a.authorizeAllow))
	mux.HandleFunc("POST /oauth/token", a.token)
	mux.Handle("/mcp", appkit.LogMCP(a.mcpHandler()))
	return securityHeaders(mux)
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// same-origin, not no-referrer: under no-referrer browsers send
		// "Origin: null" with every form post, which form() refuses.
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("X-Frame-Options", "DENY")
		h.ServeHTTP(w, r)
	})
}

// mac is an HMAC of parts, for one purpose, keyed by the app's secret.
func (a *App) mac(purpose string, parts ...string) []byte {
	m := hmac.New(sha256.New, a.Secret)
	m.Write([]byte("agentchat/" + purpose))
	for _, p := range parts {
		m.Write([]byte{0})
		m.Write([]byte(p))
	}
	return m.Sum(nil)
}

func (a *App) macString(purpose string, parts ...string) string {
	return base64.RawURLEncoding.EncodeToString(a.mac(purpose, parts...))
}

func (a *App) checkMAC(purpose, sig string, parts ...string) bool {
	got, err := base64.RawURLEncoding.DecodeString(sig)
	return err == nil && hmac.Equal(got, a.mac(purpose, parts...))
}

// externalID is a person's ID for Wakeshift: their workspace's external ID
// and the user of their chat.message subscriptions.
func externalID(person string) string { return "person:" + person }

// ── Names ────────────────────────────────────────────────────────────────

var (
	nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)
	// @handle, not inside an email address. (Any case: "@Codex" wakes codex.
	// Lowercased after matching.) A session too: @codex#2, @claude-code@pipe.
	mentionRE = regexp.MustCompile(`(?:^|[^A-Za-z0-9._%+-])@([A-Za-z0-9][A-Za-z0-9-]*(?:#[0-9]+|@[A-Za-z0-9][A-Za-z0-9-]*)?)`)
)

// slugify makes a lowercase name of letters, digits and dashes from s, at
// most n long ("" if nothing's left).
func slugify(s string, n int) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			b.WriteRune(r)
			dash = false
		case b.Len() > 0 && !dash:
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > n {
		out = strings.TrimRight(out[:n], "-")
	}
	return out
}

// channelName checks a channel name: lowercase letters, digits and dashes,
// up to max characters.
func channelName(raw string, max int) (string, error) {
	n := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(raw)), "#")
	if len(n) > max || !nameRE.MatchString(n) {
		return "", fmt.Errorf("channel names are lowercase letters, digits and dashes, up to %d characters", max)
	}
	return n, nil
}

// memberHandle is an agent's handle in channels: its person's handle and
// the agent's ID (ada-claude-code), within Wakeshift's 24 characters.
func memberHandle(person, agent string) string {
	h := person + "-" + agent
	if person == "" {
		h = agent
	}
	if len(h) > maxMemberHandle {
		h = strings.TrimRight(h[:maxMemberHandle], "-")
	}
	return h
}

// cleanText is s with its whitespace runs made single spaces, cut to n
// bytes.
func cleanText(s string, n int) string {
	return truncate(strings.Join(strings.Fields(s), " "), n)
}

// ── Agents' errors ───────────────────────────────────────────────────────

// friendly turns app API errors into messages an agent can act on.
func friendly(err error, channel string) error {
	switch {
	case err == nil:
		return nil
	case appkit.IsCode(err, "unknown_type"), appkit.IsCode(err, "unknown_group"):
		return fmt.Errorf("there's no channel %s any more", channel)
	case appkit.IsCode(err, "not_subscribed"):
		return fmt.Errorf("join %s first (the join tool)", channel)
	case appkit.IsCode(err, "bad_parent"):
		return errors.New("reply_to must be a message delivered to you (or one you posted)")
	case appkit.IsCode(err, "hop_limit"):
		return errors.New("this thread has reached its reply limit; start a new message instead")
	case appkit.IsCode(err, "rate_limited"):
		return errors.New("too many messages; wait a minute")
	case appkit.IsCode(err, "no_grant"):
		return errors.New("Agent Chat can't act for this agent right now; try again")
	}
	var netErr *url.Error
	if errors.As(err, &netErr) {
		// A network failure: don't hand the agent internal addresses.
		slog.Warn("agentchat: calling Wakeshift failed", "err", err)
		return errors.New("Agent Chat couldn't reach Wakeshift; try again shortly")
	}
	return err
}

// mentions are the @handles in text plus explicit ones, without the poster.
func mentions(text string, explicit []string, self string) []string {
	var out []string
	add := func(m string) {
		m = strings.TrimPrefix(strings.ToLower(m), "@")
		if m != "" && m != self && !slices.Contains(out, m) {
			out = append(out, m)
		}
	}
	for _, m := range mentionRE.FindAllStringSubmatch(text, -1) {
		add(m[1])
	}
	for _, m := range explicit {
		add(m)
	}
	return out
}

// ── Codex conversations ──────────────────────────────────────────────────

// maxConvSessions bounds the cache: conversation IDs come from clients.
const maxConvSessions = 100_000

// connectorCheck is how long a conversation's session (and whether its
// connector is running) is cached.
const connectorCheck = 30 * time.Second

type convSession struct {
	handle, note string
	at           time.Time
}

func (a *App) loadConv(key string) (convSession, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	c, ok := a.convSessions[key]
	return c, ok && time.Since(c.at) < connectorCheck
}

// storeConv caches a conversation's session, dropping expired entries
// (at most once per connectorCheck), and everything if it's still full.
func (a *App) storeConv(key string, c convSession) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.convSessions == nil {
		a.convSessions = map[string]convSession{}
	}
	if now := time.Now(); now.Sub(a.swept) >= connectorCheck || len(a.convSessions) >= maxConvSessions {
		a.swept = now
		for k, v := range a.convSessions {
			if now.Sub(v.at) >= connectorCheck {
				delete(a.convSessions, k)
			}
		}
		if len(a.convSessions) >= maxConvSessions {
			clear(a.convSessions)
		}
	}
	a.convSessions[key] = c
}

// codexThread is the Codex thread a tool call came from, if any.
func codexThread(req *mcp.CallToolRequest) string {
	if req.Params == nil {
		return ""
	}
	return appkit.CodexThread(req.Params.GetMeta())
}
