package slackline

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Slackline is the OAuth authorization server for its own MCP endpoint
// (the MCP authorization spec): dynamic client registration, authorize
// with PKCE (S256) behind Sign in with Slack and a consent page, and
// tokens. A client is a signed blob (nothing stored until someone approves
// it); approving makes or reuses the person's agent for that client.
// Access tokens are signed, short-lived and name the agent, which is
// checked on every request, so revoking it stops them at once. Refresh
// tokens are random, stored hashed, bound to their client and replaced
// on every use.

const (
	accessTTL  = time.Hour
	refreshTTL = 90 * 24 * time.Hour
	codeTTL    = 2 * time.Minute
)

func (a *App) resourceMetadata(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"resource": a.MCPURL(), "authorization_servers": []string{a.URL}, "bearer_methods_supported": []string{"header"},
		"resource_name": "Slackline"})
}

func (a *App) asMetadata(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer": a.URL, "authorization_endpoint": a.URL + "/oauth/authorize", "token_endpoint": a.URL + "/oauth/token",
		"registration_endpoint": a.URL + "/oauth/register", "response_types_supported": []string{"code"},
		"grant_types_supported": []string{"authorization_code", "refresh_token"}, "code_challenge_methods_supported": []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func oauthError(w http.ResponseWriter, status int, code, desc string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": desc})
}

// ── Clients ──────────────────────────────────────────────────────────────

type client struct {
	Name     string   `json:"n"`
	Redirect []string `json:"r"`
	Issued   int64    `json:"i"`
}

func (a *App) clientID(c client) string {
	b, _ := json.Marshal(c)
	p := base64.RawURLEncoding.EncodeToString(b)
	return p + "." + a.macString("client", p)
}

func (a *App) client(id string) (client, bool) {
	var c client
	p, sig, ok := strings.Cut(id, ".")
	if !ok || len(id) > 8192 || !a.checkMAC("client", sig, p) {
		return c, false
	}
	b, err := base64.RawURLEncoding.DecodeString(p)
	return c, err == nil && json.Unmarshal(b, &c) == nil
}

// redirectOK: https anywhere, or http on the loopback (native clients).
func redirectOK(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Fragment != "" || u.Host == "" || len(raw) > 2000 {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		h := u.Hostname()
		ip := net.ParseIP(h)
		return h == "localhost" || ip != nil && ip.IsLoopback()
	}
	return false
}

func (a *App) register(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ClientName   string   `json:"client_name"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "the body isn't JSON client metadata")
		return
	}
	if len(in.RedirectURIs) == 0 || len(in.RedirectURIs) > 10 {
		oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "give 1 to 10 redirect_uris")
		return
	}
	for _, u := range in.RedirectURIs {
		if !redirectOK(u) {
			oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect URIs must be https, or http on the loopback")
			return
		}
	}
	name := truncate(strings.Join(strings.Fields(in.ClientName), " "), 80)
	if name == "" {
		name = "An MCP client"
	}
	c := client{Name: name, Redirect: in.RedirectURIs, Issued: a.now().Unix()}
	writeJSON(w, http.StatusCreated, map[string]any{"client_id": a.clientID(c), "client_name": name, "redirect_uris": c.Redirect,
		"client_id_issued_at": c.Issued, "token_endpoint_auth_method": "none", "grant_types": []string{"authorization_code", "refresh_token"},
		"response_types": []string{"code"}})
}

// ── Authorize ────────────────────────────────────────────────────────────

type authRequest struct {
	ClientID, RedirectURI, Challenge, State, Resource string
	Client                                            client
}

// authRequestFrom checks an authorize request. Until the redirect URI is
// known good, errors are shown here, never sent to it.
func (a *App) authRequestFrom(v url.Values) (authRequest, string) {
	req := authRequest{ClientID: v.Get("client_id"), RedirectURI: v.Get("redirect_uri"), Challenge: v.Get("code_challenge"),
		State: v.Get("state"), Resource: v.Get("resource")}
	c, ok := a.client(req.ClientID)
	if !ok {
		return req, "This sign-in link isn't from a client Slackline knows. Add Slackline to your agent again."
	}
	req.Client = c
	if !slices.Contains(c.Redirect, req.RedirectURI) {
		return req, "This sign-in link's redirect isn't one the client registered."
	}
	if v.Get("response_type") != "code" || v.Get("code_challenge_method") != "S256" || len(req.Challenge) < 43 || len(req.Challenge) > 128 {
		return req, "This sign-in link needs a code and PKCE (S256)."
	}
	if req.Resource != "" && strings.TrimSuffix(req.Resource, "/") != a.MCPURL() {
		return req, "This sign-in link is for another server."
	}
	if len(req.State) > 1000 {
		return req, "This sign-in link's state is too long."
	}
	return req, ""
}

func (req authRequest) redirect(params url.Values) string {
	u, _ := url.Parse(req.RedirectURI)
	q := u.Query()
	for k, v := range params {
		q[k] = v
	}
	if req.State != "" {
		q.Set("state", req.State)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

type consentView struct {
	Req       authRequest
	User      User
	Installed bool
	Label     string
	// Where approving sends the code: the redirect's host, or (Local) a
	// program on this computer. The client's name is its own claim; this
	// isn't.
	RedirectHost string
	Local        bool
}

func redirectHost(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return raw, false
	}
	h := u.Hostname()
	ip := net.ParseIP(h)
	return u.Host, u.Scheme == "http" && (h == "localhost" || ip != nil && ip.IsLoopback())
}

func (a *App) authorize(w http.ResponseWriter, r *http.Request) {
	req, problem := a.authRequestFrom(r.URL.Query())
	if problem != "" {
		a.message(w, r, http.StatusBadRequest, problem, "")
		return
	}
	u, ok := a.user(r)
	if !ok {
		http.Redirect(w, r, "/signin?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
		return
	}
	_, err := a.Store.Team(r.Context(), u.TeamID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		a.failed(w, r, err)
		return
	}
	host, local := redirectHost(req.RedirectURI)
	a.render(w, r, http.StatusOK, "consent", consentView{Req: req, User: u, Installed: err == nil, Label: cleanLabel("", harnessLabel(req.Client.Name)),
		RedirectHost: host, Local: local})
}

func (a *App) authorizeAllow(w http.ResponseWriter, r *http.Request, u User) {
	req, problem := a.authRequestFrom(r.PostForm)
	if problem != "" {
		a.message(w, r, http.StatusBadRequest, problem, "")
		return
	}
	if r.PostForm.Get("decision") != "allow" {
		http.Redirect(w, r, req.redirect(url.Values{"error": {"access_denied"}}), http.StatusSeeOther)
		return
	}
	ctx := r.Context()
	ag, err := a.Store.ConnectAgent(ctx, u.TeamID, u.UserID, req.ClientID, req.Client.Name, cleanLabel(r.PostForm.Get("label"), harnessLabel(req.Client.Name)))
	if err != nil {
		a.failed(w, r, err)
		return
	}
	code := newSecret()
	if err := a.Store.SaveCode(ctx, code, Code{AgentID: ag.ID, ClientID: req.ClientID, RedirectURI: req.RedirectURI, Challenge: req.Challenge}, a.now().Add(codeTTL)); err != nil {
		a.failed(w, r, err)
		return
	}
	http.Redirect(w, r, req.redirect(url.Values{"code": {code}, "iss": {a.URL}}), http.StatusSeeOther)
}

// harnessLabel is a short default name for an agent from its client's
// name ("Claude Code" → "claude-code").
func harnessLabel(clientName string) string {
	l := strings.ToLower(strings.Join(strings.Fields(clientName), "-"))
	return strings.TrimSuffix(strings.TrimSuffix(l, "-mcp-client"), "-client")
}

// ── Tokens ───────────────────────────────────────────────────────────────

func (a *App) token(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "the body isn't a form")
		return
	}
	ctx := r.Context()
	var agent string
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		if !verifierOK(r.PostForm.Get("code_verifier")) {
			oauthError(w, http.StatusBadRequest, "invalid_request", "code_verifier must be 43 to 128 unreserved characters")
			return
		}
		c, err := a.Store.TakeCode(ctx, r.PostForm.Get("code"), a.now())
		if errors.Is(err, ErrNotFound) {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "the code is wrong, used or expired")
			return
		}
		if err != nil {
			oauthError(w, http.StatusInternalServerError, "server_error", "try again")
			return
		}
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		challenge := base64.RawURLEncoding.EncodeToString(sum[:])
		if c.ClientID != r.PostForm.Get("client_id") || c.RedirectURI != r.PostForm.Get("redirect_uri") ||
			subtle.ConstantTimeCompare([]byte(challenge), []byte(c.Challenge)) != 1 {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "the code isn't for this client, redirect or verifier")
			return
		}
		agent = c.AgentID
	case "refresh_token":
		id, err := a.Store.TakeRefresh(ctx, r.PostForm.Get("refresh_token"), r.PostForm.Get("client_id"), a.now())
		if errors.Is(err, ErrNotFound) {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token is wrong, used, expired or for another client")
			return
		}
		if err != nil {
			oauthError(w, http.StatusInternalServerError, "server_error", "try again")
			return
		}
		agent = id
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "authorization_code or refresh_token")
		return
	}
	if ag, err := a.Store.Agent(ctx, agent); err != nil || ag.Revoked {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "this agent was revoked; add Slackline again")
		return
	}
	refresh := newSecret()
	if err := a.Store.SaveRefresh(ctx, refresh, agent, a.now().Add(refreshTTL)); err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "try again")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"access_token": a.accessToken(agent), "token_type": "Bearer",
		"expires_in": int(accessTTL.Seconds()), "refresh_token": refresh})
}

// verifierOK: a PKCE code_verifier is 43 to 128 unreserved characters
// (RFC 7636 4.1).
func verifierOK(v string) bool {
	if len(v) < 43 || len(v) > 128 {
		return false
	}
	for _, c := range v {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.ContainsRune("-._~", c)) {
			return false
		}
	}
	return true
}

// accessToken is sla_<agent>.<expiry>.<mac>.
func (a *App) accessToken(agent string) string {
	exp := strconv.FormatInt(a.now().Add(accessTTL).Unix(), 10)
	return "sla_" + agent + "." + exp + "." + a.macString("access", agent, exp)
}

// tokenAgent checks an access token's signature and expiry, returning its
// agent ID (the caller checks the agent isn't revoked).
func (a *App) tokenAgent(tok string) (string, time.Time, bool) {
	p := strings.Split(strings.TrimPrefix(tok, "sla_"), ".")
	if !strings.HasPrefix(tok, "sla_") || len(p) != 3 || !a.checkMAC("access", p[2], p[0], p[1]) {
		return "", time.Time{}, false
	}
	exp, err := strconv.ParseInt(p[1], 10, 64)
	if err != nil || a.now().Unix() > exp {
		return "", time.Time{}, false
	}
	return p[0], time.Unix(exp, 0), true
}
