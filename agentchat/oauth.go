package agentchat

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/wakeshift/wakeshift-go/appkit"
)

// Agent Chat is the OAuth authorization server for its own MCP endpoint
// (the MCP authorization spec): dynamic client registration, authorize
// with PKCE (S256) behind email sign-in and a Connect page, and tokens. A
// client is a signed blob (nothing stored until someone approves it);
// approving makes or reuses the person's agent for that client, in their
// Wakeshift workspace. Access tokens are signed, last an hour and name the
// agent, which is checked on every request, so disconnecting it stops
// them at once. Refresh tokens are random, stored hashed, bound to their
// client and replaced on every use.

const (
	accessTTL  = time.Hour
	refreshTTL = 30 * 24 * time.Hour
	codeTTL    = 2 * time.Minute
)

func (a *App) resourceMetadata(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"resource": a.MCPURL(), "authorization_servers": []string{a.URL}, "bearer_methods_supported": []string{"header"},
		"resource_name": "Agent Chat"})
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
	Nonce    string   `json:"x"` // each registration its own client
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

// redirectOK: https anywhere, or http on the loopback (native clients). A
// URI that's ambiguous (spaces, control characters, backslashes, user
// info) or doesn't read back as written is refused: the one registered is
// compared byte for byte, and must mean the same to every parser.
func redirectOK(raw string) bool {
	if len(raw) > 2000 || strings.ContainsFunc(raw, func(r rune) bool { return r <= ' ' || r == 0x7f || r == '\\' }) {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Fragment != "" || u.Host == "" || u.User != nil || u.Opaque != "" || u.String() != raw {
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
	name := cleanText(in.ClientName, 80)
	if name == "" {
		name = "An MCP client"
	}
	c := client{Name: name, Redirect: in.RedirectURIs, Issued: time.Now().Unix(), Nonce: newCode(16)}
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
		return req, "This sign-in link isn't from a client Agent Chat knows. Add Agent Chat to your agent again."
	}
	req.Client = c
	if !slices.Contains(c.Redirect, req.RedirectURI) {
		return req, "This sign-in link's redirect isn't one the client registered."
	}
	if v.Get("response_type") != "code" || v.Get("code_challenge_method") != "S256" || !challengeOK(req.Challenge) {
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

// Values are the request's parameters again, for the Connect form.
func (req authRequest) Values() map[string]string {
	return map[string]string{"response_type": "code", "client_id": req.ClientID, "redirect_uri": req.RedirectURI,
		"code_challenge": req.Challenge, "code_challenge_method": "S256", "state": req.State, "resource": req.Resource}
}

type consentView struct {
	Req    authRequest
	Person Person
	// Where approving sends the code: the redirect's host, or (Local) a
	// program on this computer. The client's name is its own claim; this
	// isn't.
	RedirectHost string
	Local        bool
	Again        bool // this client is connected already
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
	a.signedIn(func(w http.ResponseWriter, r *http.Request, p Person) {
		_, err := a.Store.LiveAgentFor(r.Context(), p.ID, a.clientHash(req.ClientID))
		if err != nil && !errors.Is(err, ErrNotFound) {
			a.failed(w, r, err)
			return
		}
		host, local := redirectHost(req.RedirectURI)
		a.render(w, r, http.StatusOK, "consent", consentView{Req: req, Person: p, RedirectHost: host, Local: local, Again: err == nil})
	})(w, r)
}

// clientHash is how a client is stored: a hash of its ID.
func (a *App) clientHash(clientID string) []byte { return a.mac("client-id", clientID) }

func (a *App) authorizeAllow(w http.ResponseWriter, r *http.Request, p Person) {
	req, problem := a.authRequestFrom(r.PostForm)
	if problem != "" {
		a.message(w, r, http.StatusBadRequest, problem, "")
		return
	}
	if r.PostForm.Get("decision") != "allow" {
		http.Redirect(w, r, req.redirect(url.Values{"error": {"access_denied"}}), http.StatusSeeOther)
		return
	}
	if p.Handle == "" {
		http.Redirect(w, r, "/welcome", http.StatusSeeOther)
		return
	}
	ag, err := a.connectAgent(r.Context(), p, req.ClientID, req.Client.Name)
	if errors.Is(err, errTooMany) {
		a.message(w, r, http.StatusBadRequest, "You have too many agents connected. Disconnect one first.", "/home")
		return
	}
	if err != nil {
		a.failed(w, r, err)
		return
	}
	// Its DM, always on (dm.go): ready for the person to write in before
	// the agent's first session, which joins it. Made later if this fails.
	if _, err := a.dmWith(r.Context(), p.ID, ag.ID); err != nil {
		slog.Warn("agentchat: making a new agent's DM", "err", friendly(err, dmRef))
	}
	code := newSecret()
	if err := a.Store.SaveCode(r.Context(), code, Code{AgentID: ag.ID, ClientHash: a.clientHash(req.ClientID), RedirectURI: req.RedirectURI,
		Challenge: req.Challenge}, time.Now().Add(codeTTL)); err != nil {
		a.failed(w, r, err)
		return
	}
	http.Redirect(w, r, req.redirect(url.Values{"code": {code}, "iss": {a.URL}}), http.StatusSeeOther)
}

// connectAgent makes (or reuses, for the same client) a person's agent:
// their Wakeshift workspace (made on first use), and the agent there with
// its grant.
func (a *App) connectAgent(ctx context.Context, p Person, clientID, clientName string) (Agent, error) {
	ch := a.clientHash(clientID)
	if ag, err := a.Store.LiveAgentFor(ctx, p.ID, ch); err == nil {
		return ag, nil
	} else if !errors.Is(err, ErrNotFound) {
		return Agent{}, err
	}
	if p.Workspace == "" {
		ws, err := a.Kit.Workspace(ctx, externalID(p.ID), "")
		if err != nil {
			return Agent{}, err
		}
		if err := a.Store.SetWorkspace(ctx, p.ID, ws); err != nil {
			return Agent{}, err
		}
		p.Workspace = ws
	}
	ag, err := a.Store.ConnectAgent(ctx, p, ch, clientName, appkit.HarnessAgent(clientName))
	if err != nil {
		return Agent{}, err
	}
	if _, err := a.Kit.AddAgent(ctx, ag.Workspace, ag.Agent, clientName); err != nil {
		// A concurrent approval for the same client may have added it.
		if _, aerr := a.Kit.ActAs(ctx, ag.Workspace, ag.Agent); aerr == nil {
			return ag, nil
		}
		// Not connected after all: no agent left that can't act.
		if derr := a.Store.DisconnectAgent(context.WithoutCancel(ctx), p.ID, ag.ID); derr != nil {
			slog.Warn("agentchat: undoing an agent that Wakeshift refused", "err", derr)
		}
		return Agent{}, err
	}
	return ag, nil
}

// ── Tokens ───────────────────────────────────────────────────────────────

func (a *App) token(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "the body isn't a form")
		return
	}
	ctx := r.Context()
	ch := a.clientHash(r.PostForm.Get("client_id"))
	var agent string
	refresh := newSecret()
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		if !verifierOK(r.PostForm.Get("code_verifier")) {
			oauthError(w, http.StatusBadRequest, "invalid_request", "code_verifier must be 43 to 128 unreserved characters")
			return
		}
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		challenge := base64.RawURLEncoding.EncodeToString(sum[:])
		// Spent only with its client, redirect and verifier: a wrong one
		// leaves it for the client it was issued to.
		id, err := a.Store.TakeCode(ctx, r.PostForm.Get("code"), ch, r.PostForm.Get("redirect_uri"), challenge)
		if errors.Is(err, ErrNotFound) {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "the code is wrong, used or expired, or not for this client, redirect or verifier")
			return
		}
		if err != nil {
			oauthError(w, http.StatusInternalServerError, "server_error", "try again")
			return
		}
		agent = id
	case "refresh_token":
		// The old token is spent and its replacement stored at once.
		id, err := a.Store.RotateRefresh(ctx, r.PostForm.Get("refresh_token"), ch, refresh, time.Now().Add(refreshTTL))
		if errors.Is(err, ErrNotFound) {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token is wrong, used, expired or for another client, or its agent was disconnected")
			return
		}
		if err != nil {
			oauthError(w, http.StatusInternalServerError, "server_error", "try again")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"access_token": a.accessToken(id), "token_type": "Bearer",
			"expires_in": int(accessTTL.Seconds()), "refresh_token": refresh})
		return
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "authorization_code or refresh_token")
		return
	}
	if err := a.Store.SaveRefresh(ctx, refresh, agent, ch, time.Now().Add(refreshTTL)); errors.Is(err, ErrNotFound) {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "this agent was disconnected; add Agent Chat again")
		return
	} else if err != nil {
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

// challengeOK says whether c is an S256 code challenge: a SHA-256 hash in
// unpadded base64url (43 characters).
func challengeOK(c string) bool {
	b, err := base64.RawURLEncoding.DecodeString(c)
	return err == nil && len(b) == sha256.Size
}

// accessToken is aca_<agent>.<expiry>.<mac>.
func (a *App) accessToken(agent string) string {
	exp := strconv.FormatInt(time.Now().Add(accessTTL).Unix(), 10)
	return "aca_" + agent + "." + exp + "." + a.macString("access", agent, exp)
}

// tokenAgent checks an access token's signature and expiry, returning its
// agent ID (the caller checks the agent is still connected).
func (a *App) tokenAgent(tok string) (string, time.Time, bool) {
	p := strings.Split(strings.TrimPrefix(tok, "aca_"), ".")
	if !strings.HasPrefix(tok, "aca_") || len(p) != 3 || !a.checkMAC("access", p[2], p[0], p[1]) {
		return "", time.Time{}, false
	}
	exp, err := strconv.ParseInt(p[1], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return "", time.Time{}, false
	}
	return p[0], time.Unix(exp, 0), true
}
