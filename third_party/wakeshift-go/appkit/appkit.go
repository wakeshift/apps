// Package appkit is the SDK for apps built on Wakeshift (Agent Chat, the
// games, and anyone else's).
//
// An app has an ID and an Ed25519 key registered with Wakeshift (an app key
// from the console, wst_…; Open finds the app it belongs to). With them it:
//   - publishes its manifest (what it may do) and proves who it is with
//     short-lived signed assertions;
//   - identifies its own users, with its own sign-in, in workspaces it owns
//     (Workspace, AddAgent); RequireBearer checks the tokens MCP clients
//     send;
//   - defines events (DefineEvent) and attaches them to its MCP server
//     (Attach), so an agent that subscribes wakes when the app emits one;
//   - subscribes the conversation calling one of its own tools, from a tool
//     wrapped by Events (DefinedEvent.Subscribe), and emits to the
//     conversations it stored (Emit with ToSubscription);
//   - acts for a user's agent (ActAs), so a wrapped tool's subscriptions are
//     the agent's, inside the grant the app's manifest asks for.
//
// Its calls are the TypeScript and Python SDKs' (@wakeshift/sdk,
// wakeshift), in Go's shape. Unlike them, only Open reads WAKESHIFT_CORE
// and defaults to DefaultCore.
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
	"net"
	"net/http"
	"net/url"
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
	exchanged map[string]*Agent // ActAs's, by workspace and agent

	eventTypes []*eventType // DefineEvent's, in order

	// The manifest (manifest.go): what the app last gave PublishManifest
	// (nil for nothing), and whether Wakeshift has the manifest for the
	// event types defined now (sent at generation sentGen; DefineEvent
	// moves gen on). sendMu makes one send at a time.
	sendMu       sync.Mutex
	base         *Manifest
	gen, sentGen uint64
	sent         bool
}

// APIError is an error response from Wakeshift, or a mistake the SDK
// catches before asking it (an emit without a recipient, another app's
// event), with the code and status Wakeshift gives for the same mistake.
// A request that got no answer at all (a wrong core, a network problem, the
// HTTP client's timeout) is one with Status 0 and the code unreachable,
// which unwraps to the transport's error.
type APIError struct {
	Status  int
	Code    string
	Message string
	// Reason, for some errors, is a category (an MCP Events callback that
	// failed verification: challenge_failed, http_4xx…).
	Reason string

	err error // unreachable's cause
}

func (e *APIError) Error() string {
	if e.Code == "unreachable" && e.Status == 0 {
		return "wakeshift: " + e.Message
	}
	code := e.Code
	if code == "" {
		code = "error"
	}
	return fmt.Sprintf("wakeshift: %s (%d): %s", code, e.Status, e.Message)
}

// Unwrap is the transport's error, for an unreachable core.
func (e *APIError) Unwrap() error { return e.err }

// unreachable is the APIError for a request with ctx through client that
// got no answer. It names the core it went to: a wrong or missing
// WAKESHIFT_CORE is the usual cause.
func (a *App) unreachable(ctx context.Context, client *http.Client, err error) *APIError {
	cause := err
	var ue *url.Error
	if errors.As(err, &ue) {
		cause = ue.Err // without the method and URL, which the message names
	}
	why := cause.Error()
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() && client.Timeout > 0 && ctx.Err() == nil {
		why = fmt.Sprintf("no answer within %s", client.Timeout)
	}
	return &APIError{Code: "unreachable", Message: fmt.Sprintf("can't reach Wakeshift at %s (%s). "+
		"Check that WAKESHIFT_CORE, or the core option, is the address of the console your key came from", a.core(), why), err: err}
}

// refuse is the APIError for a mistake caught before asking Wakeshift:
// the status and code Wakeshift gives for the same mistake.
func refuse(status int, code, format string, a ...any) *APIError {
	return &APIError{Status: status, Code: code, Message: fmt.Sprintf(format, a...)}
}

// badArguments is Wakeshift's bad_arguments: an event's arguments,
// payload or recipients that don't fit it.
func badArguments(format string, a ...any) *APIError {
	return refuse(http.StatusBadRequest, "bad_arguments", format, a...)
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

// assertion signs a fresh, short-lived assertion of the app's identity,
// naming the key by its kid. iss and sub are the app's ID, or the kid
// while the ID isn't known yet (Open asking which app the key is).
func (a *App) assertion() (string, error) {
	jti := make([]byte, 16)
	rand.Read(jti)
	now := time.Now()
	kid := a.kid()
	iss := a.ID
	if iss == "" {
		iss = kid
	}
	return jws.Sign(a.Key, kid, "app+jwt", map[string]any{
		"iss": iss, "sub": iss, "aud": a.core() + "/api/apps",
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
		return a.unreachable(ctx, client, err) // a core address that isn't a URL
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.do(client, req)
	if err != nil {
		return a.unreachable(ctx, client, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return errorOf(resp.StatusCode, b)
	}
	if out == nil {
		return nil
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	// A 2xx that isn't Wakeshift's JSON: a proxy's page, or a wrong core.
	if t := bytes.TrimSpace(b); len(t) == 0 || t[0] != '{' || !json.Valid(t) {
		return &APIError{Status: resp.StatusCode, Code: "bad_response", Message: fmt.Sprintf("Wakeshift at %s answered %d without a JSON object. "+
			"Check that WAKESHIFT_CORE, or the core option, is the address of the console your key came from", a.core(), resp.StatusCode)}
	}
	return json.Unmarshal(b, out)
}

// errorOf is the APIError for an answer with status that isn't a success,
// with body (its start): Wakeshift's code, message and reason where it sent
// them as strings, else the body's first 200 bytes as the message, else the
// status's own text, as every SDK does (sdk/typescript/test/errors.json).
func errorOf(status int, body []byte) *APIError {
	var e struct {
		Error any `json:"error"`
	}
	json.Unmarshal(body, &e)
	fields, _ := e.Error.(map[string]any)
	str := func(k string) string { s, _ := fields[k].(string); return s }
	msg := str("message")
	if msg == "" {
		msg = string(body[:min(len(body), 200)])
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	return &APIError{Status: status, Code: str("code"), Message: msg, Reason: str("reason")}
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
	// The app identifies its own users (with its own sign-in) and
	// acts for their agents in workspaces Wakeshift keeps for them
	// (App.Workspace, AddAgent, ActAs).
	Agent *AgentScope `json:"agent,omitempty"`
	// Events are event types defined elsewhere, not with DefineEvent: the
	// manifest Wakeshift gets lists these, then the ones DefineEvent made.
	// Listing one DefineEvent made is an error.
	Events []ManifestEvent `json:"events,omitempty"`
	// OthersCanWake: your app wakes its users' agents with what other
	// people and their agents do there (messages, moves), so the consent
	// screen tells the person that agents of people they don't know can
	// wake theirs.
	OthersCanWake bool `json:"others_can_wake,omitempty"`
	// PreapproveTools names your MCP tools (as your server lists them)
	// that agents' programs can run without asking each time: tools that
	// only read, and routine ones the agent calls on every wake that reach
	// only what it already joined. Never one that deletes, spends, or
	// reaches people the user hasn't chosen. When a user connects Claude
	// Code or Cursor CLI, Wakeshift offers to allow exactly these, by
	// name, in the same question as its install. Letters, digits, _ and -;
	// at most 20.
	PreapproveTools []string `json:"preapprove_tools,omitempty"`
}

type AgentScope struct {
	Wake []ScopeEvent `json:"wake"`
	Post []ScopeEvent `json:"post"`
	// WakeLimits pace how often the app may wake the agent (at most Max in
	// any Seconds), on top of Wakeshift's own: 10 a second, 450 per 5 minutes
	// and 3,600 an hour. Wake-ups past a limit wait their turn.
	WakeLimits []WakeLimit `json:"wake_limits,omitempty"`
	TTLDays    int         `json:"ttl_days"`
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

type ScopeEvent struct {
	Pattern     string `json:"pattern"`
	Description string `json:"description"`
}

// ParseKey reads an app's Ed25519 private key: an app key from the console
// (wst_…, either format, see Open), or base64 (standard or URL) of a 32-byte
// seed or a 64-byte key.
func ParseKey(s string) (ed25519.PrivateKey, error) {
	if strings.HasPrefix(strings.TrimSpace(s), keyPrefix) {
		_, key, err := parseAppKey(s)
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
