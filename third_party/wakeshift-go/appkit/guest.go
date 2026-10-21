package appkit

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/wakeshift/wakeshift-go/internal/harnesses"
)

// GuestAuth signs MCP clients (Claude Code, Codex…) in to a headless app as
// guests: no Wakeshift account, no account with the app. It's a small OAuth
// server inside the app (dynamic client registration, authorize with PKCE,
// tokens), and it keeps no state: clients, codes and tokens are signed by
// the app's key.
//
// Each registered client (an install of a harness) is one guest: its own
// workspace, with an agent named after the harness (claude-code). Sign-in
// shows nothing: the user added the app to their agent, and that's the
// consent (docs/headless.md).
//
// Since authorize has no user step, knowing a client_id must not be enough
// to become its guest (client_ids travel in authorize URLs, which end up in
// logs and browser history). Registration also gives the client a secret
// (derived from the client_id with the app's key, so nothing is stored),
// and the token endpoint requires it with every code and refresh token.
// The guest's workspace and agent are made at the code exchange, once the
// client has authenticated, not at authorize.
//
// Mount Routes on the app's mux, point MCP clients at it with
// ResourceMetadata, and check tokens with Verify.
type GuestAuth struct {
	App *App
	// Base is the app's public URL (default: its MCP URL without /mcp).
	Base string
	// AccessTTL (default an hour) and RefreshTTL (default 30 days). Each
	// refresh returns a new refresh token, good for RefreshTTL from then.
	AccessTTL, RefreshTTL time.Duration
	// Revoked, if set, is asked at each refresh whether the guest's refresh
	// token issued at issuedAt is revoked: an app's own denylist, or a
	// per-guest generation (refuse those issued before a cutoff). A refresh
	// is also refused once the guest's grant isn't live (Wakeshift answers
	// no_grant), so revoking the grant, or forgetting the workspace, signs
	// the guest out within AccessTTL.
	Revoked func(ctx context.Context, g Guest, issuedAt time.Time) bool
	// ProxyHops is how many trusted proxies in front append to
	// X-Forwarded-For (behind Google's load balancer: 2), for the
	// per-address limits on registering and on token requests. 0: the
	// connection's own address.
	ProxyHops int
	// RegisterLimit (default 20) registrations and TokenLimit (default 60)
	// token requests per address per RateWindow (default 10 minutes), per
	// instance; a negative limit turns it off.
	RegisterLimit, TokenLimit int
	RateWindow                time.Duration

	limitsOnce sync.Once
	registers  *rateLimiter
	tokens     *rateLimiter
}

// Guest is who a guest access token is for.
type Guest struct {
	Workspace string
	Agent     string
	Client    string // the client's name, e.g. "Claude Code"
	Expires   time.Time
}

func (g *GuestAuth) base() string {
	if g.Base != "" {
		return strings.TrimSuffix(g.Base, "/")
	}
	return strings.TrimSuffix(g.App.MCPURL, "/mcp")
}

func (g *GuestAuth) accessTTL() time.Duration  { return or(g.AccessTTL, time.Hour) }
func (g *GuestAuth) refreshTTL() time.Duration { return or(g.RefreshTTL, 30*24*time.Hour) }

// guestCodeTTL is how long an authorization code is good for.
const guestCodeTTL = time.Minute

func or(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

func (g *GuestAuth) limits() (registers, tokens *rateLimiter) {
	g.limitsOnce.Do(func() {
		n := func(v, def int) int {
			if v == 0 {
				return def
			}
			return v
		}
		window := or(g.RateWindow, 10*time.Minute)
		g.registers = &rateLimiter{max: n(g.RegisterLimit, 20), window: window}
		g.tokens = &rateLimiter{max: n(g.TokenLimit, 60), window: window}
	})
	return g.registers, g.tokens
}

// Routes are the OAuth endpoints, and the protected-resource metadata that
// points MCP clients at them.
func (g *GuestAuth) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", g.ResourceMetadata)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", g.ResourceMetadata)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", g.metadata)
	mux.HandleFunc("POST /oauth/register", g.register)
	mux.HandleFunc("GET /oauth/authorize", g.authorize)
	mux.HandleFunc("POST /oauth/token", g.token)
}

// ResourceMetadata (RFC 9728) points MCP clients at the app's own sign-in.
func (g *GuestAuth) ResourceMetadata(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"resource": g.App.MCPURL, "authorization_servers": []string{g.base()}, "bearer_methods_supported": []string{"header"}})
}

func (g *GuestAuth) metadata(w http.ResponseWriter, _ *http.Request) {
	b := g.base()
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer": b, "authorization_endpoint": b + "/oauth/authorize", "token_endpoint": b + "/oauth/token", "registration_endpoint": b + "/oauth/register",
		"response_types_supported": []string{"code"}, "grant_types_supported": []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_post", "client_secret_basic"},
	})
}

// Signed blobs: a purpose, then base64 JSON and an HMAC keyed from the app's
// key, so each kind can't pass for another.
func (g *GuestAuth) mac(purpose string) []byte {
	h := hmac.New(sha256.New, g.App.Key.Seed())
	h.Write([]byte("appkit-guest:" + purpose))
	return h.Sum(nil)
}

func (g *GuestAuth) sign(purpose string, v any) string {
	b, _ := json.Marshal(v)
	p := base64.RawURLEncoding.EncodeToString(b)
	h := hmac.New(sha256.New, g.mac(purpose))
	h.Write([]byte(p))
	return p + "." + base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

func (g *GuestAuth) open(purpose, s string, v any) bool {
	p, sig, ok := strings.Cut(s, ".")
	if !ok {
		return false
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	h := hmac.New(sha256.New, g.mac(purpose))
	h.Write([]byte(p))
	if err != nil || !hmac.Equal(got, h.Sum(nil)) {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(p)
	return err == nil && json.Unmarshal(b, v) == nil
}

// clientSecret is the secret registration gave a client: an HMAC of its
// client_id, so it's never stored and only the app can make it.
func (g *GuestAuth) clientSecret(clientID string) string {
	h := hmac.New(sha256.New, g.mac("client-secret"))
	h.Write([]byte(clientID))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

// clientAuth is the client a token request authenticated as, by HTTP Basic
// (RFC 6749 2.3.1: each part form-encoded first) or client_id and
// client_secret in the form, if its secret is right. r.ParseForm must have
// run.
func (g *GuestAuth) clientAuth(r *http.Request) (string, bool) {
	id, secret, basic := r.BasicAuth()
	if basic {
		var err1, err2 error
		id, err1 = url.QueryUnescape(id)
		secret, err2 = url.QueryUnescape(secret)
		if err1 != nil || err2 != nil || r.PostForm.Get("client_secret") != "" {
			return "", false // one method at a time (RFC 6749 2.3)
		}
		if f := r.PostForm.Get("client_id"); f != "" && f != id {
			return "", false
		}
	} else {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	var c guestClient
	if id == "" || secret == "" || !g.open("client", id, &c) {
		return "", false
	}
	if subtle.ConstantTimeCompare([]byte(secret), []byte(g.clientSecret(id))) != 1 {
		return "", false
	}
	return id, true
}

type guestClient struct {
	Name      string   `json:"n"`
	Redirects []string `json:"r"`
	Nonce     string   `json:"x"` // makes each registration its own guest (128 bits)
}

func (g *GuestAuth) register(w http.ResponseWriter, r *http.Request) {
	if regs, _ := g.limits(); !regs.allow(clientAddr(r, g.ProxyHops), time.Now()) {
		oauthErr(w, http.StatusTooManyRequests, "slow_down", "too many registrations from this address; try again later")
		return
	}
	var in struct {
		ClientName   string   `json:"client_name"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil || len(in.RedirectURIs) == 0 || len(in.RedirectURIs) > 5 {
		oauthErr(w, http.StatusBadRequest, "invalid_client_metadata", "redirect_uris is required")
		return
	}
	for _, u := range in.RedirectURIs {
		if p, err := url.Parse(u); err != nil || p.Scheme == "" || len(u) > 500 {
			oauthErr(w, http.StatusBadRequest, "invalid_redirect_uri", "bad redirect URI")
			return
		}
	}
	name := strings.TrimSpace(in.ClientName)
	if name == "" || len(name) > 80 {
		name = "MCP client"
	}
	id := g.sign("client", guestClient{Name: name, Redirects: in.RedirectURIs, Nonce: nonce()})
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, map[string]any{"client_id": id, "client_secret": g.clientSecret(id), "client_secret_expires_at": 0,
		"client_name": name, "redirect_uris": in.RedirectURIs, "token_endpoint_auth_method": "client_secret_post",
		"grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"}})
}

// pending is an authorization: what its code carries.
type pending struct {
	Client    string `json:"c"` // client_id
	Redirect  string `json:"r"`
	State     string `json:"s"`
	Challenge string `json:"p"`
	Expires   int64  `json:"e"`
}

type guestCode struct {
	pending
	Code bool `json:"k"`
}

// pkceRE is a PKCE code verifier, or an S256 challenge (RFC 7636 4.1, 4.2).
var pkceRE = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)

// authorize issues a code to a registered client with no user step (the
// user added the app to their agent). It creates nothing: the guest is made
// at the code exchange, once the client proves it holds its secret.
func (g *GuestAuth) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var c guestClient
	if !g.open("client", q.Get("client_id"), &c) {
		http.Error(w, "Unknown client. Register it again.", http.StatusBadRequest)
		return
	}
	redirect := q.Get("redirect_uri")
	if !contains(c.Redirects, redirect) {
		http.Error(w, "That redirect URI isn't registered for this client.", http.StatusBadRequest)
		return
	}
	if q.Get("response_type") != "code" || !pkceRE.MatchString(q.Get("code_challenge")) || q.Get("code_challenge_method") != "S256" {
		redirectErr(w, r, redirect, q.Get("state"), "invalid_request", "PKCE (S256) is required")
		return
	}
	p := pending{Client: q.Get("client_id"), Redirect: redirect, State: q.Get("state"), Challenge: q.Get("code_challenge"),
		Expires: time.Now().Add(guestCodeTTL).Unix()}
	u, _ := url.Parse(p.Redirect)
	v := u.Query()
	v.Set("code", g.sign("code", guestCode{pending: p, Code: true}))
	if p.State != "" {
		v.Set("state", p.State)
	}
	u.RawQuery = v.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

type guestToken struct {
	Workspace string `json:"w"`
	Agent     string `json:"a"`
	Client    string `json:"c"` // client name
	ClientID  string `json:"i,omitempty"`
	IssuedAt  int64  `json:"t,omitempty"`
	Nonce     string `json:"n,omitempty"` // each refresh token its own
	Expires   int64  `json:"e"`
}

func (g *GuestAuth) token(w http.ResponseWriter, r *http.Request) {
	if _, toks := g.limits(); !toks.allow(clientAddr(r, g.ProxyHops), time.Now()) {
		oauthErr(w, http.StatusTooManyRequests, "slow_down", "too many token requests from this address; try again later")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	if err := r.ParseForm(); err != nil {
		oauthErr(w, http.StatusBadRequest, "invalid_request", "bad form")
		return
	}
	f := r.PostForm
	clientID, ok := g.clientAuth(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Basic realm="oauth"`)
		oauthErr(w, http.StatusUnauthorized, "invalid_client", "client authentication failed: send the client_id and client_secret registration gave you")
		return
	}
	var ws, agent, name string
	switch f.Get("grant_type") {
	case "authorization_code":
		var c guestCode
		if !g.open("code", f.Get("code"), &c) || !c.Code || time.Now().Unix() > c.Expires || c.Client != clientID || c.Redirect != f.Get("redirect_uri") {
			oauthErr(w, http.StatusBadRequest, "invalid_grant", "the code is invalid or expired")
			return
		}
		verifier := f.Get("code_verifier")
		sum := sha256.Sum256([]byte(verifier))
		if !pkceRE.MatchString(verifier) || subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(c.Challenge)) != 1 {
			oauthErr(w, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
			return
		}
		var cl guestClient
		g.open("client", clientID, &cl)
		// One guest per registered client: its workspace, and its agent
		// named after the harness.
		name, agent = cl.Name, HarnessAgent(cl.Name)
		var err error
		if ws, err = g.App.Workspace(r.Context(), "guest:"+guestID(clientID), "Guest ("+cl.Name+")"); err == nil {
			_, err = g.App.AddAgent(r.Context(), ws, agent, cl.Name)
		}
		if IsCode(err, "workspace_limit") {
			oauthErr(w, http.StatusForbidden, "access_denied", "this app can't take more guests right now")
			return
		}
		if err != nil {
			slog.Error("appkit: making a guest", "err", err)
			oauthErr(w, http.StatusServiceUnavailable, "temporarily_unavailable", "couldn't sign in; try again")
			return
		}
	case "refresh_token":
		var t guestToken
		if !g.open("refresh", f.Get("refresh_token"), &t) || time.Now().Unix() > t.Expires || t.ClientID != clientID {
			oauthErr(w, http.StatusBadRequest, "invalid_grant", "the refresh token is invalid or expired")
			return
		}
		guest := Guest{Workspace: t.Workspace, Agent: t.Agent, Client: t.Client, Expires: time.Unix(t.Expires, 0)}
		if g.Revoked != nil && g.Revoked(r.Context(), guest, time.Unix(t.IssuedAt, 0)) {
			oauthErr(w, http.StatusBadRequest, "invalid_grant", "the refresh token was revoked; sign in again")
			return
		}
		// The guest's grant must still be live: revoking it (or forgetting
		// the workspace) ends its refreshes.
		if _, err := g.App.actAsFresh(r.Context(), t.Workspace, t.Agent); err != nil {
			if IsCode(err, "no_grant") || IsCode(err, "unknown_workspace") {
				oauthErr(w, http.StatusBadRequest, "invalid_grant", "this guest's access was revoked; sign in again")
				return
			}
			slog.Error("appkit: checking a guest's grant", "err", err)
			oauthErr(w, http.StatusServiceUnavailable, "temporarily_unavailable", "couldn't refresh; try again")
			return
		}
		ws, agent, name = t.Workspace, t.Agent, t.Client
	default:
		oauthErr(w, http.StatusBadRequest, "unsupported_grant_type", "use authorization_code or refresh_token")
		return
	}
	now := time.Now()
	access := g.sign("access", guestToken{Workspace: ws, Agent: agent, Client: name, Expires: now.Add(g.accessTTL()).Unix()})
	// A new refresh token every time (rotation), dated, so Revoked can
	// refuse those issued before a cutoff.
	refresh := g.sign("refresh", guestToken{Workspace: ws, Agent: agent, Client: name, ClientID: clientID, IssuedAt: now.Unix(), Nonce: nonce(), Expires: now.Add(g.refreshTTL()).Unix()})
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": int(g.accessTTL().Seconds()), "refresh_token": refresh})
}

// Verify checks a guest access token from an MCP request.
func (g *GuestAuth) Verify(token string) (Guest, error) {
	var t guestToken
	if !g.open("access", token, &t) || time.Now().Unix() > t.Expires {
		return Guest{}, errors.New("appkit: invalid or expired guest token")
	}
	return Guest{Workspace: t.Workspace, Agent: t.Agent, Client: t.Client, Expires: time.Unix(t.Expires, 0)}, nil
}

// rateLimiter allows max events per window for each key, in memory (per
// instance), pruning idle keys as it goes.
type rateLimiter struct {
	max    int
	window time.Duration
	keys   int // most addresses tracked at once (default limiterKeys)
	mu     sync.Mutex
	seen   map[string][]time.Time
	pruned time.Time
}

// limiterKeys bounds a limiter's memory: past it, a new address is refused
// until old ones expire (the endpoints are unauthenticated, and IPv6 makes
// addresses cheap).
const limiterKeys = 100_000

func (l *rateLimiter) allow(key string, now time.Time) bool {
	if l.max < 0 {
		return true
	}
	key = limitKey(key)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.seen == nil {
		l.seen = map[string][]time.Time{}
	}
	cut := now.Add(-l.window)
	prune := func() {
		for k, ts := range l.seen {
			if len(ts) == 0 || !ts[len(ts)-1].After(cut) {
				delete(l.seen, k)
			}
		}
		l.pruned = now
	}
	if now.Sub(l.pruned) > l.window {
		prune()
	}
	if _, ok := l.seen[key]; !ok {
		limit := l.keys
		if limit <= 0 {
			limit = limiterKeys
		}
		if len(l.seen) >= limit {
			prune()
			if len(l.seen) >= limit {
				return false
			}
		}
	}
	ts := l.seen[key]
	i := 0
	for i < len(ts) && !ts[i].After(cut) {
		i++
	}
	ts = ts[i:]
	if len(ts) >= l.max {
		l.seen[key] = ts
		return false
	}
	l.seen[key] = append(ts, now)
	return true
}

// limitKey is what an address counts as for the limits: an IPv6 address's
// /64 (one host usually has a whole /64), anything else as it is.
func limitKey(addr string) string {
	ip, err := netip.ParseAddr(addr)
	if err != nil || ip.Is4() || ip.Is4In6() {
		return addr
	}
	p, err := ip.Prefix(64)
	if err != nil {
		return addr
	}
	return p.String()
}

// clientAddr is the request's client address: with hops trusted proxies in
// front (each appending to X-Forwarded-For), the entry the outermost one
// added; entries before it are the client's own claims.
func clientAddr(r *http.Request, hops int) string {
	if f := r.Header.Get("X-Forwarded-For"); f != "" && hops > 0 {
		// Only an entry the proxies added: with fewer entries than hops, or
		// one that isn't an address, the header isn't the proxies' (a
		// request that reached the app some other way), so it's ignored.
		parts := strings.Split(f, ",")
		if len(parts) >= hops {
			if ip, err := netip.ParseAddr(strings.TrimSpace(parts[len(parts)-hops])); err == nil {
				return ip.String()
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// HarnessAgent is the agent ID a client is named by: its harness's ID
// ("copilot-cli" → copilot, "Claude Code" → claude-code), as topics and
// Sign in with Wakeshift name it, else its name as an ID (lowercase
// letters, digits and dashes; "agent" if nothing's left).
func HarnessAgent(client string) string { return harnesses.AgentID(client) }

func guestID(clientID string) string {
	sum := sha256.Sum256([]byte(clientID))
	return base64.RawURLEncoding.EncodeToString(sum[:18])
}

func nonce() string {
	b := make([]byte, 16)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func oauthErr(w http.ResponseWriter, status int, code, desc string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": desc})
}

func redirectErr(w http.ResponseWriter, r *http.Request, redirect, state, code, desc string) {
	u, err := url.Parse(redirect)
	if err != nil {
		http.Error(w, desc, http.StatusBadRequest)
		return
	}
	v := u.Query()
	v.Set("error", code)
	v.Set("error_description", desc)
	if state != "" {
		v.Set("state", state)
	}
	u.RawQuery = v.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}
