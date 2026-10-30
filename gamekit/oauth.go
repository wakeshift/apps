package gamekit

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
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/auth"

	"github.com/wakeshift/wakeshift-go/appkit"
)

// Players' sign-in: the games' own OAuth server, for MCP clients (Claude
// Code, Codex, Cursor, ChatGPT…). Players have no account, with the game or
// with Wakeshift. Clicking Authorize on the game's page (authorize.go)
// starts a session: a random player ID, kept in the game's database
// (player_sessions), which is the player's ID in Wakeshift too, as any
// app's own user is (its workspace's external ID).
//
//   - Registration (RFC 7591) keeps nothing: the client ID is signed with
//     the app's key and carries the client's name and redirect URIs, and its
//     secret is an HMAC of the ID. A client that registers as public
//     (token_endpoint_auth_method "none", as Codex does: it keeps no client
//     secret, so it refreshes with its client ID alone) gets no secret, and
//     its ID says so. Every redirect URI must be one allowedRedirect
//     accepts, checked again at every authorize, which also needs an exact
//     match with one the client registered. Limited per address.
//   - Authorize needs response_type=code and a PKCE S256 challenge. The
//     click makes the session and a code for it (good for codeTTL, once),
//     and redirects back with it, the state and the issuer (RFC 9207).
//     Limited per address.
//   - The token endpoint needs the client's secret (client_secret_post or
//     client_secret_basic), for the code and refresh grants; a public
//     client sends its client ID alone, and PKCE (always required) and
//     single-use refresh tokens bound to its ID stand in for it. The code
//     exchange makes the player's workspace and agent (named after the
//     client: claude-code) in Wakeshift. The access token is signed and
//     lasts accessTTL; the refresh token is random, kept as a hash, lasts
//     refreshTTL and is replaced at every refresh (the one it replaced stops
//     working).
//   - A session that hasn't signed in (exchanged its code or refreshed) for
//     SessionIdle, a day past its refresh token's life, is forgotten:
//     Wakeshift's ForgetUser for its player ID, then its row (expireSessions).
//     Everything else the game keeps about a player is gone well before:
//     games an hour after they end, what a player had lately after 30 days.
//
// Sessions are in the database, not signed, because expiring them needs a
// list of them with their last use, and only stored refresh tokens can be
// used once.

const (
	accessTTL  = time.Hour
	refreshTTL = 30 * 24 * time.Hour
	// SessionIdle is how long a session may go without signing in before
	// it's forgotten, here and in Wakeshift.
	SessionIdle = 31 * 24 * time.Hour
	codeTTL     = time.Minute
	// unclaimedFor is how long a session whose code was never exchanged, or
	// whose exchange failed, is kept.
	unclaimedFor = 10 * time.Minute
	// signInWindow is the per-address limits' window.
	signInWindow = 10 * time.Minute
)

// limits are the per-address limits on registering, on starting sessions
// and on token requests (registerLimit, sessionLimit and tokenLimit set
// them for tests; 0 is the default).
func (s *Server) limits() (registers, sessions, tokens *appkit.RateLimiter) {
	s.limitsOnce.Do(func() {
		n := func(v, def int) int {
			if v == 0 {
				return def
			}
			return v
		}
		s.registers = appkit.NewRateLimiter(n(s.registerLimit, 20), signInWindow, s.ProxyHops)
		s.sessions = appkit.NewRateLimiter(n(s.sessionLimit, 20), signInWindow, s.ProxyHops)
		s.tokens = appkit.NewRateLimiter(n(s.tokenLimit, 60), signInWindow, s.ProxyHops)
	})
	return s.registers, s.sessions, s.tokens
}

// oauthRoutes mounts players' sign-in.
func (s *Server) oauthRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", s.resourceMetadata)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", s.resourceMetadata)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", s.oauthMetadata)
	mux.HandleFunc("POST /oauth/register", s.register)
	mux.HandleFunc("GET /oauth/authorize", s.authorizePage)
	mux.HandleFunc("POST /oauth/authorize", s.authorizeClick)
	mux.HandleFunc("POST /oauth/token", s.token)
}

// resourceMetadata (RFC 9728) points MCP clients at the game's sign-in.
func (s *Server) resourceMetadata(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"resource": s.Kit.MCPURL, "authorization_servers": []string{s.PublicURL()}, "bearer_methods_supported": []string{"header"}})
}

func (s *Server) oauthMetadata(w http.ResponseWriter, _ *http.Request) {
	b := s.PublicURL()
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer": b, "authorization_endpoint": b + "/oauth/authorize", "token_endpoint": b + "/oauth/token", "registration_endpoint": b + "/oauth/register",
		"response_types_supported": []string{"code"}, "grant_types_supported": []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":               []string{"S256"},
		"token_endpoint_auth_methods_supported":          []string{"client_secret_post", "client_secret_basic", "none"},
		"authorization_response_iss_parameter_supported": true,
	})
}

// Signed values: base64 JSON and an HMAC, under a key derived from the
// app's key for each label, so one kind can't pass for another.
func (s *Server) key(label string) []byte {
	h := hmac.New(sha256.New, s.Kit.Key.Seed())
	h.Write([]byte(label))
	return h.Sum(nil)
}

func (s *Server) seal(label string, v any) string {
	b, _ := json.Marshal(v)
	p := base64.RawURLEncoding.EncodeToString(b)
	h := hmac.New(sha256.New, s.key(label))
	h.Write([]byte(p))
	return p + "." + base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

func (s *Server) unseal(label, token string, v any) bool {
	p, sig, ok := strings.Cut(token, ".")
	if !ok {
		return false
	}
	got, err := base64.RawURLEncoding.Strict().DecodeString(sig) // canonical only: one signature, one encoding
	h := hmac.New(sha256.New, s.key(label))
	h.Write([]byte(p))
	if err != nil || !hmac.Equal(got, h.Sum(nil)) {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(p)
	return err == nil && json.Unmarshal(b, v) == nil
}

// The labels. Clients keep the ones guest sign-in (the SDKs' GuestAuth,
// before the games had their own) used, so a client registered then still
// works, if its redirect URIs are ones allowedRedirect accepts.
const (
	labelClient       = "appkit-guest:client"
	labelClientSecret = "appkit-guest:client-secret"
	labelAccess       = "gamekit:access"
	labelForm         = "gamekit:authorize-page"
)

// client is what a client ID carries.
type client struct {
	Name      string   `json:"n"`
	Redirects []string `json:"r"`
	Nonce     string   `json:"x"` // each registration its own client (128 bits)
	// Public: registered with token_endpoint_auth_method "none", so it has
	// no secret and authenticates at the token endpoint by its ID alone.
	Public bool `json:"p,omitempty"`
}

// clientSecret is the secret registration gave a client: an HMAC of its ID.
func (s *Server) clientSecret(clientID string) string {
	h := hmac.New(sha256.New, s.key(labelClientSecret))
	h.Write([]byte(clientID))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

// clientAuth is the client a token request authenticated as, by HTTP Basic
// (RFC 6749 2.3.1: each part form-encoded first) or client_id and
// client_secret in the form, if its secret is right. A public client sends
// its client_id in the form and no secret (it was given none); a client
// that registered with a secret must send it. r.ParseForm must have run.
func (s *Server) clientAuth(r *http.Request) (string, client, bool) {
	id, secret, basic := r.BasicAuth()
	if basic {
		var err1, err2 error
		id, err1 = url.QueryUnescape(id)
		secret, err2 = url.QueryUnescape(secret)
		if err1 != nil || err2 != nil || r.PostForm.Get("client_secret") != "" {
			return "", client{}, false // one method at a time (RFC 6749 2.3)
		}
		if f := r.PostForm.Get("client_id"); f != "" && f != id {
			return "", client{}, false
		}
	} else {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	var c client
	if id == "" || !s.unseal(labelClient, id, &c) {
		return "", client{}, false
	}
	if c.Public {
		// It has no secret to send. An empty client_secret carries none,
		// and some clients send one, so it counts as not sent.
		if basic || secret != "" {
			return "", client{}, false
		}
		return id, c, true
	}
	if secret == "" {
		return "", client{}, false
	}
	if subtle.ConstantTimeCompare([]byte(secret), []byte(s.clientSecret(id))) != 1 {
		return "", client{}, false
	}
	return id, c, true
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	if regs, _, _ := s.limits(); !regs.Allow(r, time.Now()) {
		oauthErr(w, http.StatusTooManyRequests, "slow_down", "too many registrations from this address; try again later")
		return
	}
	var in struct {
		ClientName   string   `json:"client_name"`
		RedirectURIs []string `json:"redirect_uris"`
		AuthMethod   string   `json:"token_endpoint_auth_method"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil || len(in.RedirectURIs) == 0 || len(in.RedirectURIs) > 5 {
		oauthErr(w, http.StatusBadRequest, "invalid_client_metadata", "redirect_uris is required (at most 5)")
		return
	}
	for _, u := range in.RedirectURIs {
		if !allowedRedirect(u) {
			oauthErr(w, http.StatusBadRequest, "invalid_redirect_uri", BadRedirect)
			return
		}
	}
	name := strings.TrimSpace(in.ClientName)
	if name == "" || len(name) > 80 {
		name = "MCP client"
	}
	// A client asking to be public is one; any other method gets a secret
	// (client_secret_post, or Basic: both work).
	public := in.AuthMethod == "none"
	id := s.seal(labelClient, client{Name: name, Redirects: in.RedirectURIs, Nonce: random(16), Public: public})
	out := map[string]any{"client_id": id, "client_name": name, "redirect_uris": in.RedirectURIs, "token_endpoint_auth_method": "none",
		"grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"}}
	if !public {
		out["client_secret"], out["client_secret_expires_at"], out["token_endpoint_auth_method"] = s.clientSecret(id), 0, "client_secret_post"
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, out)
}

// pkceRE is a PKCE code verifier, or an S256 challenge (RFC 7636 4.1, 4.2).
var pkceRE = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)

// authRequest is a checked authorize request.
type authRequest struct {
	clientID  string
	client    client
	redirect  string
	state     string
	challenge string
}

// checkAuthorize checks an authorize request: a client of the game's, one
// of its redirect URIs exactly (still one allowedRedirect accepts), and
// PKCE. problem says what's wrong, for the person: a refused request is
// answered on the game's own page, never by redirecting.
func (s *Server) checkAuthorize(q url.Values) (a authRequest, problem string) {
	a.clientID = q.Get("client_id")
	if !s.unseal(labelClient, a.clientID, &a.client) {
		return a, "Unknown client. Remove the game from your agent and add it again."
	}
	a.redirect = q.Get("redirect_uri")
	if !contains(a.client.Redirects, a.redirect) || !allowedRedirect(a.redirect) {
		return a, "That redirect URI isn't registered for this client."
	}
	a.state, a.challenge = q.Get("state"), q.Get("code_challenge")
	if q.Get("response_type") != "code" || !pkceRE.MatchString(a.challenge) || q.Get("code_challenge_method") != "S256" || len(a.state) > 1000 {
		return a, "This sign-in request is incomplete: PKCE (S256) and response_type=code are required."
	}
	return a, ""
}

// startSession is the Authorize click: a new session, and a redirect back
// to the client with its code.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, a authRequest) {
	if _, sessions, _ := s.limits(); !sessions.Allow(r, time.Now()) {
		http.Error(w, "Too many sign-ins from this address. Try again later.", http.StatusTooManyRequests)
		return
	}
	id, code, now := "p_"+random(16), random(32), s.now()
	if _, err := s.Store.Pool.Exec(r.Context(), `INSERT INTO player_sessions (id, client, client_name, redirect_uri, code_challenge, code_hash, code_expires_at, created_at, active_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)`, id, hash(a.clientID), a.client.Name, a.redirect, a.challenge, hash(code), now.Add(codeTTL), now); err != nil {
		slog.Error(s.Name+": starting a session", "err", err)
		http.Error(w, "Something went wrong. Try again.", http.StatusInternalServerError)
		return
	}
	u, _ := url.Parse(a.redirect)
	v := u.Query()
	v.Set("code", code)
	if a.state != "" {
		v.Set("state", a.state)
	}
	v.Set("iss", s.PublicURL())
	u.RawQuery = v.Encode()
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// accessToken is what an access token carries.
type accessToken struct {
	Session   string `json:"s"`
	Workspace string `json:"w"`
	Agent     string `json:"a"`
	Client    string `json:"c"` // the client's name
	Expires   int64  `json:"e"`
}

// session is a session's row, as the token endpoint reads it.
type session struct {
	ID, Client, ClientName, Redirect, Challenge string
	Workspace, Agent                            *string
	RefreshExpires                              *time.Time
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	if _, _, toks := s.limits(); !toks.Allow(r, time.Now()) {
		oauthErr(w, http.StatusTooManyRequests, "slow_down", "too many token requests from this address; try again later")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	if err := r.ParseForm(); err != nil {
		oauthErr(w, http.StatusBadRequest, "invalid_request", "bad form")
		return
	}
	f := r.PostForm
	clientID, cl, ok := s.clientAuth(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Basic realm="oauth"`)
		oauthErr(w, http.StatusUnauthorized, "invalid_client", "client authentication failed: send the client_id and client_secret registration gave you")
		return
	}
	ctx, now := r.Context(), s.now()
	var sess session
	var oldRefresh string
	switch f.Get("grant_type") {
	case "authorization_code":
		// The code works once, whatever happens next.
		err := s.Store.Pool.QueryRow(ctx, `UPDATE player_sessions SET code_hash = NULL WHERE code_hash = $1 AND code_expires_at > $2
			RETURNING id, client, client_name, redirect_uri, code_challenge, workspace, agent, refresh_expires_at`, hash(f.Get("code")), now).
			Scan(&sess.ID, &sess.Client, &sess.ClientName, &sess.Redirect, &sess.Challenge, &sess.Workspace, &sess.Agent, &sess.RefreshExpires)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && (subtle.ConstantTimeCompare([]byte(sess.Client), []byte(hash(clientID))) != 1 || sess.Redirect != f.Get("redirect_uri")) {
			oauthErr(w, http.StatusBadRequest, "invalid_grant", "the code is invalid or expired")
			return
		}
		if err != nil {
			s.tokenFailed(w, "reading a code", err)
			return
		}
		verifier := f.Get("code_verifier")
		sum := sha256.Sum256([]byte(verifier))
		if !pkceRE.MatchString(verifier) || subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(sess.Challenge)) != 1 {
			oauthErr(w, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
			return
		}
		// The player's workspace (its ID is the session's) and agent, named
		// after the client.
		agent := appkit.HarnessAgent(cl.Name)
		ws, err := s.Kit.Workspace(ctx, sess.ID, s.Title+" player")
		if err == nil {
			_, err = s.Kit.AddAgent(ctx, ws, agent, cl.Name)
		}
		switch {
		case appkit.IsCode(err, "workspace_limit"):
			oauthErr(w, http.StatusForbidden, "access_denied", s.Title+" can't take more players right now")
			return
		case appkit.IsCode(err, "workspace_rate_limited"):
			oauthErr(w, http.StatusServiceUnavailable, "temporarily_unavailable", s.Title+" is taking too many new players right now; try again later")
			return
		case err != nil:
			s.tokenFailed(w, "making a player", err)
			return
		}
		sess.Workspace, sess.Agent = &ws, &agent
	case "refresh_token":
		oldRefresh = hash(f.Get("refresh_token"))
		err := s.Store.Pool.QueryRow(ctx, `SELECT id, client, client_name, workspace, agent, refresh_expires_at FROM player_sessions WHERE refresh_hash = $1`, oldRefresh).
			Scan(&sess.ID, &sess.Client, &sess.ClientName, &sess.Workspace, &sess.Agent, &sess.RefreshExpires)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && (subtle.ConstantTimeCompare([]byte(sess.Client), []byte(hash(clientID))) != 1 ||
			sess.RefreshExpires == nil || !now.Before(*sess.RefreshExpires) || sess.Workspace == nil || sess.Agent == nil) {
			oauthErr(w, http.StatusBadRequest, "invalid_grant", "the refresh token is invalid or expired")
			return
		}
		if err != nil {
			s.tokenFailed(w, "reading a refresh token", err)
			return
		}
		// The player must still be there in Wakeshift: forgotten, or its
		// grant revoked, the session ends.
		if _, err := s.Kit.ActAs(ctx, *sess.Workspace, *sess.Agent); appkit.IsCode(err, "no_grant") || appkit.IsCode(err, "unknown_workspace") {
			s.endSession(ctx, sess.ID)
			oauthErr(w, http.StatusBadRequest, "invalid_grant", "this player's access was revoked; sign in again")
			return
		} else if err != nil {
			s.tokenFailed(w, "checking a player", err)
			return
		}
	default:
		oauthErr(w, http.StatusBadRequest, "unsupported_grant_type", "use authorization_code or refresh_token")
		return
	}
	// A new refresh token every time; the one it replaces stops working
	// (a refresh racing this one finds it gone).
	refresh := random(32)
	q := `UPDATE player_sessions SET workspace = $2, agent = $3, refresh_hash = $4, refresh_expires_at = $5, active_at = $6 WHERE id = $1`
	args := []any{sess.ID, *sess.Workspace, *sess.Agent, hash(refresh), now.Add(refreshTTL), now}
	if oldRefresh != "" {
		q += ` AND refresh_hash = $7`
		args = append(args, oldRefresh)
	}
	tag, err := s.Store.Pool.Exec(ctx, q, args...)
	if err != nil {
		s.tokenFailed(w, "saving a session", err)
		return
	}
	if tag.RowsAffected() == 0 {
		oauthErr(w, http.StatusBadRequest, "invalid_grant", "the refresh token is invalid or expired")
		return
	}
	access := s.seal(labelAccess, accessToken{Session: sess.ID, Workspace: *sess.Workspace, Agent: *sess.Agent, Client: sess.ClientName, Expires: now.Add(accessTTL).Unix()})
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": int(accessTTL.Seconds()), "refresh_token": refresh})
}

func (s *Server) tokenFailed(w http.ResponseWriter, doing string, err error) {
	slog.Error(s.Name+": sign-in: "+doing, "err", err)
	oauthErr(w, http.StatusServiceUnavailable, "temporarily_unavailable", "couldn't sign in; try again")
}

// verify checks an access token from an MCP request. The user is the
// session's player ID.
func (s *Server) verify(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
	var t accessToken
	if !s.unseal(labelAccess, token, &t) || t.Session == "" || t.Workspace == "" {
		return nil, &appkit.TokenError{Reason: appkit.ReasonInvalid}
	}
	exp := time.Unix(t.Expires, 0)
	if now := time.Now(); !now.Before(exp) {
		return nil, &appkit.TokenError{Reason: appkit.ReasonExpired, Past: now.Sub(exp)}
	}
	return &auth.TokenInfo{UserID: t.Session, Expiration: exp,
		Extra: map[string]any{"workspace": t.Workspace, "agent": t.Agent, "client": t.Client}}, nil
}

// endSession forgets a session: Wakeshift's ForgetUser for its player,
// then its joins (joins.go) and its row. It reports whether it's gone (Wakeshift may fail: it's
// tried again on the next pass).
func (s *Server) endSession(ctx context.Context, id string) bool {
	if _, err := s.Kit.ForgetUser(ctx, id); err != nil {
		slog.Warn(s.Name+": forgetting a session's player", "err", err)
		return false
	}
	if _, err := s.Store.Pool.Exec(ctx, `DELETE FROM joins WHERE player = (SELECT workspace FROM player_sessions WHERE id = $1)`, id); err != nil {
		slog.Warn(s.Name+": deleting a session's joins", "err", err)
		return false
	}
	if _, err := s.Store.Pool.Exec(ctx, `DELETE FROM player_sessions WHERE id = $1`, id); err != nil {
		slog.Warn(s.Name+": deleting a session", "err", err)
		return false
	}
	return true
}

// expireSessions forgets sessions idle for SessionIdle, and those whose
// code was exchanged without finishing (unclaimedFor), up to limit of them,
// and deletes sessions whose code was never exchanged (nothing of theirs
// is in Wakeshift). It reports how many it forgot.
func (s *Server) expireSessions(ctx context.Context, now time.Time, limit int) (int, error) {
	if _, err := s.Store.Pool.Exec(ctx, `DELETE FROM player_sessions WHERE code_hash IS NOT NULL AND created_at < $1`, now.Add(-unclaimedFor)); err != nil {
		return 0, err
	}
	rows, _ := s.Store.Pool.Query(ctx, `SELECT id FROM player_sessions WHERE active_at < $1 OR (code_hash IS NULL AND refresh_hash IS NULL AND active_at < $2)
		ORDER BY active_at LIMIT $3`, now.Add(-SessionIdle), now.Add(-unclaimedFor), limit)
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		if ctx.Err() != nil {
			return n, ctx.Err()
		}
		// Only if it's still idle: a refresh meanwhile keeps it.
		var still bool
		if err := s.Store.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM player_sessions WHERE id = $1 AND (active_at < $2 OR (refresh_hash IS NULL AND active_at < $3)))`,
			id, now.Add(-SessionIdle), now.Add(-unclaimedFor)).Scan(&still); err != nil {
			return n, err
		}
		if still && s.endSession(ctx, id) {
			n++
		}
	}
	return n, nil
}

// hash is a token's or code's SHA-256, as stored.
func hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// random is n random bytes, base64url.
func random(n int) string {
	b := make([]byte, n)
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
