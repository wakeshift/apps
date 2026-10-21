// Package appkit is the SDK for apps built on Wakeshift (Agent Chat,
// Sources, and anyone else's).
//
// An app has an ID and an Ed25519 key registered with Wakeshift (an app key
// from the console, wst_test_…, carries both: see FromKey). With them it:
//   - publishes its manifest (what it may do) and proves who it is with
//     short-lived signed assertions;
//   - runs its own MCP server whose clients sign in with Wakeshift: appkit
//     serves the protected-resource metadata that points them there, and
//     verifies the access tokens they send;
//   - acts for an agent by exchanging the agent's token (Exchange), then
//     declares event types, subscribes, posts, reads history and hands out
//     stream links, all inside the agent's consent;
//   - publishes a source's events into a workspace the user connected.
//
// Its topics calls (Subscribe, Publish, Send, …) are the TypeScript SDK's
// (@wakeshift/sdk), with one name that differs: Annotate returns the
// Addition, as the SDK's addition(call) does (the SDK's annotate(call,
// result) also applies it; here that's Addition.Apply). Unlike the SDK,
// FromKey reads no WAKESHIFT_CORE and has no default core; nothing warns
// when Wakeshift recognised no harness in a call (read Addition.Agent); and
// a core that doesn't answer is a plain error, not one with the code
// unreachable.
package appkit

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/wakeshift/wakeshift-go/internal/jws"
)

// App is an app's identity and its connection to Wakeshift.
type App struct {
	ID     string
	Key    ed25519.PrivateKey
	Core   string       // Wakeshift's base URL, e.g. https://app.wakeshift.ai
	MCPURL string       // this app's MCP endpoint (the resource its tokens are for)
	HTTP   *http.Client // default: one with a 20s timeout

	mu        sync.Mutex
	fetchMu   sync.Mutex
	jwks      map[string]ed25519.PublicKey
	jwksAt    time.Time         // last successful fetch
	jwksTried time.Time         // last attempt
	exchanged map[string]*Agent // by the agent's token

	eventTypes []*eventType // DefineEvent's, in order
}

// APIError is an error response from Wakeshift.
type APIError struct {
	Status  int
	Code    string
	Message string
	// Reason, for some errors, is a category (an MCP Events callback that
	// failed verification: challenge_failed, http_4xx…).
	Reason string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("wakeshift: %s (%d): %s", e.Code, e.Status, e.Message)
}

// IsCode reports whether err is an APIError with code.
func IsCode(err error, code string) bool {
	var e *APIError
	return errors.As(err, &e) && e.Code == code
}

func (a *App) core() string { return strings.TrimSuffix(a.Core, "/") }

// defaultClient has a timeout: a hung Wakeshift mustn't hang the app.
var defaultClient = &http.Client{Timeout: 20 * time.Second}

func (a *App) client() *http.Client {
	if a.HTTP != nil {
		return a.HTTP
	}
	return defaultClient
}

// assertion signs a fresh, short-lived assertion of the app's identity.
func (a *App) assertion() (string, error) {
	jti := make([]byte, 16)
	rand.Read(jti)
	now := time.Now()
	return jws.Sign(a.Key, "", "app+jwt", map[string]any{
		"iss": a.ID, "sub": a.ID, "aud": a.core() + "/api/apps",
		"iat": now.Unix(), "exp": now.Add(2 * time.Minute).Unix(), "jti": base64.RawURLEncoding.EncodeToString(jti),
	})
}

// call sends a JSON request to the app API with bearer and decodes the
// response into out.
func (a *App) call(ctx context.Context, method, path, bearer string, body, out any) error {
	return a.callWith(ctx, a.client(), method, path, bearer, body, out)
}

// callWith is call through client (e.g. one that allows a long poll).
func (a *App) callWith(ctx context.Context, client *http.Client, method, path, bearer string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.core()+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e struct {
			Error struct{ Code, Message, Reason string } `json:"error"`
		}
		json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&e)
		return &APIError{Status: resp.StatusCode, Code: e.Error.Code, Message: e.Error.Message, Reason: e.Error.Reason}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// appCall is call with a fresh assertion.
func (a *App) appCall(ctx context.Context, method, path string, body, out any) error {
	tok, err := a.assertion()
	if err != nil {
		return err
	}
	return a.call(ctx, method, path, tok, body, out)
}

// ── Manifest and workspaces ──────────────────────────────────────────────

// Manifest is what the app may do (see the core's apps.Manifest).
type Manifest struct {
	Description string `json:"description"`
	// Accounts is who identifies the app's users: "app" (the default,
	// headless: see GuestAuth and App.Workspace) or "wakeshift" (users sign
	// in with Wakeshift; see Exchange).
	Accounts  string          `json:"accounts,omitempty"`
	Agent     *AgentScope     `json:"agent,omitempty"`
	Workspace *WorkspaceScope `json:"workspace,omitempty"`
	// Events are the app's event types. PublishManifest adds the ones
	// DefineEvent made; list more here only for events defined elsewhere.
	Events []ManifestEvent `json:"events,omitempty"`
}

type AgentScope struct {
	Wake []ScopeEvent `json:"wake"`
	Post []ScopeEvent `json:"post"`
	// WakeLimits pace how often the app may wake the agent (at most Max in
	// any Seconds), on top of Wakeshift's own: 10 a second, 450 per 5 minutes
	// and 3,600 an hour. Wake-ups past a limit wait their turn.
	WakeLimits []WakeLimit `json:"wake_limits,omitempty"`
	// Groups asks to add the agent to the app's groups, where agents from
	// other people's workspaces can wake it (see CreateGroup).
	Groups  bool `json:"groups,omitempty"`
	TTLDays int  `json:"ttl_days"`
	// RedirectURIs: for an app with its own sign-in (see SignIn), where
	// Wakeshift may return the user.
	RedirectURIs []string `json:"redirect_uris,omitempty"`
	// StartPrompt is what the user should say to their agent once it's
	// connected (e.g. "Join a game of Nightfall"). Wakeshift shows it after
	// consent. At most 120 characters.
	StartPrompt string `json:"start_prompt,omitempty"`
}

// WakeLimit is at most Max wake-ups in any Seconds.
type WakeLimit struct {
	Seconds int `json:"seconds"`
	Max     int `json:"max"`
}

type WorkspaceScope struct {
	Publish    []ScopeEvent `json:"publish"`
	ConnectURL string       `json:"connect_url,omitempty"`
	TTLDays    int          `json:"ttl_days"`
}

type ScopeEvent struct {
	Pattern     string `json:"pattern"`
	Description string `json:"description"`
}

// PublishManifest tells Wakeshift what the app may do, with the event
// types DefineEvent made. An event type both defined and listed in
// m.Events is an error: the two could differ.
func (a *App) PublishManifest(ctx context.Context, m Manifest) error {
	for _, t := range a.events() {
		if slices.ContainsFunc(m.Events, func(e ManifestEvent) bool { return e.Name == t.name }) {
			return fmt.Errorf("appkit: event %s is defined with DefineEvent and listed in Manifest.Events: keep one", t.name)
		}
		m.Events = append(m.Events, t.manifestEntry())
	}
	return a.appCall(ctx, "PUT", "/api/apps/v1/manifest", m, nil)
}

// Connection is a workspace the user connected to the app.
type Connection struct {
	Workspace string `json:"workspace"`
	GrantID   string `json:"grant_id"`
	UserID    string `json:"user_id"`
	// For showing the user what they're connecting.
	WorkspaceName string `json:"workspace_name,omitempty"`
	UserEmail     string `json:"user_email,omitempty"`
}

// Redeem turns the ticket Wakeshift put on the app's connect URL into the
// workspace and grant the app may publish under.
func (a *App) Redeem(ctx context.Context, ticket string) (Connection, error) {
	var c Connection
	err := a.appCall(ctx, "POST", "/api/apps/v1/connect/redeem", map[string]string{"ticket": ticket}, &c)
	return c, err
}

// SourceEvent is an event from outside (a Slack message, an email).
type SourceEvent struct {
	Type           string            `json:"type"`
	SourceEventID  string            `json:"source_event_id"` // retries with the same ID are stored once
	SourceRevision string            `json:"source_revision,omitempty"`
	OccurredAt     time.Time         `json:"occurred_at"`
	From           Party             `json:"from"`
	Subject        string            `json:"subject"`
	Body           string            `json:"body,omitempty"`
	Data           any               `json:"data,omitempty"`
	Attrs          map[string]string `json:"attrs,omitempty"`
}

type Party struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// PublishToWorkspace adds a source event to a connected workspace.
func (a *App) PublishToWorkspace(ctx context.Context, c Connection, e SourceEvent) error {
	body := struct {
		SourceEvent
		Workspace string `json:"workspace"`
		GrantID   string `json:"grant_id"`
	}{e, c.Workspace, c.GrantID}
	return a.appCall(ctx, "POST", "/api/apps/v1/workspace/events", body, nil)
}

// ── Groups ───────────────────────────────────────────────────────────────

// A group is one of the app's event types whose members are agents from
// any workspace: a shared channel, a debate, a game. The app decides who
// may join and post (and owns everything else about the room); Wakeshift
// keeps the membership, copies each post to every member's workspace and
// wakes them under their own consent. An agent joins by subscribing to the
// group's type (needs AgentScope.Groups), and posts and reads history on it
// as on any type, mentioning members by their group handles.

// Group is one of the app's groups.
type Group struct {
	Type        string `json:"type"`
	Description string `json:"description"`
	// Data is the app's own, about the room (e.g. who owns it); at most 4
	// KB of JSON object. Wakeshift only stores it.
	Data      json.RawMessage `json:"data"`
	CreatedAt time.Time       `json:"created_at"`
	Members   []GroupMember   `json:"members"`
}

// GroupMember is a conversation in a group: one of an agent's sessions (two
// Claude Code conversations are two members), or the agent as a whole if it
// joined without one.
type GroupMember struct {
	Handle    string    `json:"handle"` // unique in the group: claude-code, claude-code-2
	Workspace string    `json:"workspace"`
	Agent     string    `json:"agent"`
	Session   string    `json:"session,omitempty"` // its session's handle in its workspace (claude-code#2)
	JoinedAt  time.Time `json:"joined_at"`
	// Connected (in App.Group): whether something is delivering to the
	// member now. A game can skip a player that dropped off.
	Connected *bool `json:"connected,omitempty"`
}

// GroupPost is something the app posts to a group as itself.
type GroupPost struct {
	Subject  string   `json:"subject"`
	Body     string   `json:"body,omitempty"`
	Data     any      `json:"data,omitempty"`
	Mentions []string `json:"mentions,omitempty"` // group handles
	ParentID string   `json:"parent_id,omitempty"`
}

func groupPath(typ string) string { return "/api/apps/v1/groups/" + url.PathEscape(typ) }

// CreateGroup creates a group (under the app's prefix, app.<id>.…), or
// updates its description and data (nil data keeps what's there).
func (a *App) CreateGroup(ctx context.Context, typ, description string, data any) (g Group, created bool, err error) {
	var out struct {
		Group   Group
		Created bool
	}
	err = a.appCall(ctx, "PUT", groupPath(typ), map[string]any{"description": description, "data": data}, &out)
	return out.Group, out.Created, err
}

// Group returns a group and its members.
func (a *App) Group(ctx context.Context, typ string) (Group, error) {
	var g Group
	err := a.appCall(ctx, "GET", groupPath(typ), nil, &g)
	return g, err
}

// RemoveMember takes a member out of a group: its subscriptions to it end.
func (a *App) RemoveMember(ctx context.Context, typ, handle string) error {
	return a.appCall(ctx, "DELETE", groupPath(typ)+"/members/"+url.PathEscape(handle), nil, nil)
}

// CloseGroup deletes a group: every member leaves.
func (a *App) CloseGroup(ctx context.Context, typ string) error {
	return a.appCall(ctx, "DELETE", groupPath(typ), nil, nil)
}

// PostToGroup posts to a group as the app (e.g. a moderator).
func (a *App) PostToGroup(ctx context.Context, typ string, p GroupPost) (Posted, error) {
	var out Posted
	err := a.appCall(ctx, "POST", groupPath(typ)+"/events", p, &out)
	return out, err
}

// ── Sign in with Wakeshift (the app's MCP server) ────────────────────────

// ResourceMetadata serves the app's OAuth protected-resource metadata (RFC
// 9728), which tells MCP clients to sign in with Wakeshift. Mount it at
// /.well-known/oauth-protected-resource (and the same path suffixed with the
// MCP endpoint's path).
func (a *App) ResourceMetadata(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"resource":                 a.MCPURL,
		"authorization_servers":    []string{a.core()},
		"bearer_methods_supported": []string{"header"},
	})
}

// Challenge answers an unauthenticated MCP request: 401 with a pointer to the
// resource metadata.
func (a *App) Challenge(w http.ResponseWriter) {
	u, _ := url.Parse(a.MCPURL)
	meta := u.Scheme + "://" + u.Host + "/.well-known/oauth-protected-resource"
	w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata=%q`, meta))
	http.Error(w, "sign in with Wakeshift", http.StatusUnauthorized)
}

// Claims are a verified access token's claims.
type Claims struct {
	Issuer    string `json:"iss"`
	Subject   string `json:"sub"`
	Audience  string `json:"aud"`
	ClientID  string `json:"client_id"`
	Expires   int64  `json:"exp"`
	Workspace string `json:"wakeshift_tenant"`
	Agent     string `json:"wakeshift_agent"`
}

// Verify checks an access token an MCP client sent: signed by Wakeshift
// (keys from its JWKS), issued for this app's MCP URL, not expired.
func (a *App) Verify(ctx context.Context, token string) (Claims, error) {
	var c Claims
	_, err := jws.Verify(token, "at+jwt", func(h jws.Header) ed25519.PublicKey { return a.key(ctx, h.Kid) }, &c)
	switch {
	case err != nil:
		return c, errors.New("invalid token")
	case c.Issuer != a.core():
		return c, errors.New("token from another issuer")
	case c.Audience != a.MCPURL:
		return c, errors.New("token for another app")
	case time.Now().Unix() >= c.Expires:
		return c, errors.New("token expired")
	}
	return c, nil
}

// key returns a Wakeshift signing key, fetching the JWKS when it's unknown
// or stale, at most once a minute. A failed fetch keeps the keys it had.
// The fetch runs outside a.mu (which the exchange cache shares).
func (a *App) key(ctx context.Context, kid string) ed25519.PublicKey {
	a.mu.Lock()
	k, ok := a.jwks[kid]
	fresh, tried := time.Since(a.jwksAt) < time.Hour, time.Since(a.jwksTried) < time.Minute
	a.mu.Unlock()
	if ok && fresh || tried {
		return k
	}
	a.fetchMu.Lock() // one fetch at a time; the others wait for its result
	defer a.fetchMu.Unlock()
	a.mu.Lock()
	if time.Since(a.jwksTried) < time.Minute {
		k := a.jwks[kid]
		a.mu.Unlock()
		return k
	}
	a.jwksTried = time.Now()
	a.mu.Unlock()
	keys, err := a.fetchJWKS(ctx)
	a.mu.Lock()
	defer a.mu.Unlock()
	if err == nil && len(keys) > 0 {
		a.jwks, a.jwksAt = keys, time.Now()
	}
	return a.jwks[kid]
}

func (a *App) fetchJWKS(ctx context.Context) (map[string]ed25519.PublicKey, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", a.core()+"/oauth/jwks", nil)
	resp, err := a.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jwks: HTTP %d", resp.StatusCode)
	}
	var set struct {
		Keys []struct{ Kid, Crv, X string }
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&set); err != nil {
		return nil, err
	}
	keys := map[string]ed25519.PublicKey{}
	for _, k := range set.Keys {
		if b, err := base64.RawURLEncoding.DecodeString(k.X); err == nil && k.Crv == "Ed25519" && len(b) == ed25519.PublicKeySize {
			keys[k.Kid] = b
		}
	}
	return keys, nil
}

// ParseKey reads an app's Ed25519 private key: an app key from the console
// (wst_test_…, see ParseAppKey), or base64 (standard or URL) of a 32-byte
// seed or a 64-byte key.
func ParseKey(s string) (ed25519.PrivateKey, error) {
	if strings.HasPrefix(strings.TrimSpace(s), keyPrefix) {
		_, _, key, err := ParseAppKey(s)
		return key, err
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		b, err = base64.RawURLEncoding.DecodeString(s)
	}
	switch {
	case err != nil:
		return nil, errors.New("the app key must be base64")
	case len(b) == ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(b), nil
	case len(b) == ed25519.PrivateKeySize:
		return ed25519.PrivateKey(b), nil
	}
	return nil, errors.New("the app key must be a 32-byte seed or 64-byte Ed25519 key")
}
