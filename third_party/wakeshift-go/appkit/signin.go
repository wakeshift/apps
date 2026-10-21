package appkit

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ── An app's own sign-in ─────────────────────────────────────────────────
//
// An app that runs its own OAuth server for its MCP clients (instead of
// using Wakeshift's) signs the user in to Wakeshift inside its flow, as
// OAuth client "app:<id>": AuthorizeURL sends the user to pick the agent
// and consent, and SignIn turns the code Wakeshift returns into an Agent
// the app acts for, plus a refresh token to keep doing so. The redirect
// URI must be in the manifest's Agent.RedirectURIs.

// PKCE returns a new code verifier and its S256 challenge.
func PKCE() (verifier, challenge string) {
	b := make([]byte, 32)
	rand.Read(b)
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

// AuthorizeURL is Wakeshift's authorize page for this app. agentHint names
// the app's own MCP client (e.g. "Claude Code"), so Wakeshift can suggest
// the agent; the user decides.
func (a *App) AuthorizeURL(redirectURI, state, challenge, agentHint string) string {
	q := url.Values{"response_type": {"code"}, "client_id": {"app:" + a.ID}, "redirect_uri": {redirectURI}, "state": {state},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "agent_hint": {agentHint}}
	return a.core() + "/oauth/authorize?" + q.Encode()
}

// SignedIn is what a sign-in (or a refresh) gave the app.
type SignedIn struct {
	Agent        *Agent
	RefreshToken string // rotates: keep the newest
	UserID       string // who signed in
	Grant        string // the agent's grant for the app
	// Connection is the workspace's consent to what the app publishes, if
	// the app has a workspace scope and it's live.
	Connection *Connection
}

// SignIn exchanges the code from Wakeshift's redirect. Wakeshift adds iss
// to the redirect (RFC 9207); check it equals Core.
func (a *App) SignIn(ctx context.Context, code, verifier, redirectURI string) (SignedIn, error) {
	return a.tokens(ctx, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {verifier}, "redirect_uri": {redirectURI}})
}

// Refresh renews the app's tokens for an agent. Wakeshift refuses
// (APIError "invalid_grant") once the agent's consent is revoked: forget
// the sign-in then.
func (a *App) Refresh(ctx context.Context, refreshToken string) (SignedIn, error) {
	return a.tokens(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}})
}

func (a *App) tokens(ctx context.Context, form url.Values) (SignedIn, error) {
	assertion, err := a.assertion()
	if err != nil {
		return SignedIn{}, err
	}
	form.Set("client_id", "app:"+a.ID)
	form.Set("client_assertion_type", "urn:ietf:params:oauth:client-assertion-type:jwt-bearer")
	form.Set("client_assertion", assertion)
	req, _ := http.NewRequestWithContext(ctx, "POST", a.core()+"/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := a.client().Do(req)
	if err != nil {
		return SignedIn{}, err
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken    string `json:"access_token"`
		RefreshToken   string `json:"refresh_token"`
		ExpiresIn      int64  `json:"expires_in"`
		Workspace      string `json:"wakeshift_workspace"`
		Agent          string `json:"wakeshift_agent"`
		Grant          string `json:"wakeshift_grant"`
		WorkspaceGrant string `json:"wakeshift_workspace_grant"`
		User           string `json:"wakeshift_user"`
		WorkspaceName  string `json:"wakeshift_workspace_name"`
		UserEmail      string `json:"wakeshift_user_email"`
		Error          string `json:"error"`
		Description    string `json:"error_description"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != http.StatusOK {
		return SignedIn{}, &APIError{Status: resp.StatusCode, Code: out.Error, Message: out.Description}
	}
	s := SignedIn{
		Agent: &Agent{app: a, token: out.AccessToken, expires: time.Now().Add(time.Duration(out.ExpiresIn) * time.Second),
			Workspace: out.Workspace, ID: out.Agent},
		RefreshToken: out.RefreshToken, UserID: out.User, Grant: out.Grant,
	}
	if out.WorkspaceGrant != "" {
		s.Connection = &Connection{Workspace: out.Workspace, GrantID: out.WorkspaceGrant, UserID: out.User,
			WorkspaceName: out.WorkspaceName, UserEmail: out.UserEmail}
	}
	return s, nil
}

// Expires is when the Agent's token expires; refresh before then.
func (ag *Agent) Expires() time.Time { return ag.expires }
