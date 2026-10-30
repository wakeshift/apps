package gamekit

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"

	"github.com/wakeshift/apps/internal/pgtest"
	"github.com/wakeshift/apps/legal"
	"github.com/wakeshift/wakeshift-go/appkit"
)

// fakeCore is the part of Wakeshift's app API sign-in uses: workspaces,
// agents, agents' tokens and forgetting users.
type fakeCore struct {
	mu         sync.Mutex
	workspaces map[string]string // external ID → workspace
	agents     []string          // workspace/agent
	forgot     []string
	revoked    bool // agents' tokens answer no_grant
	rateLimit  bool // new workspaces answer workspace_rate_limited
}

func (f *fakeCore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var in map[string]string
	json.NewDecoder(r.Body).Decode(&in)
	reply := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(v)
	}
	refuse := func(status int, code string) {
		reply(status, map[string]any{"error": map[string]string{"code": code, "message": code}})
	}
	p := r.URL.Path
	switch {
	case p == "/api/apps/v1/workspaces":
		if f.rateLimit {
			refuse(429, "workspace_rate_limited")
			return
		}
		ws, ok := f.workspaces[in["external_id"]]
		if !ok {
			ws = "ws-" + in["external_id"]
			f.workspaces[in["external_id"]] = ws
		}
		reply(200, map[string]string{"workspace": ws})
	case p == "/api/apps/v1/users/forget":
		f.forgot = append(f.forgot, in["user"])
		_, ok := f.workspaces[in["user"]]
		delete(f.workspaces, in["user"])
		reply(200, map[string]bool{"forgotten": ok})
	case strings.HasSuffix(p, "/token"):
		if f.revoked {
			refuse(403, "no_grant")
			return
		}
		reply(200, map[string]any{"access_token": "agent-token", "expires_in": 3600})
	case strings.HasSuffix(p, "/agents"):
		ws := strings.TrimSuffix(strings.TrimPrefix(p, "/api/apps/v1/workspaces/"), "/agents")
		f.agents = append(f.agents, ws+"/"+in["agent"])
		reply(200, map[string]string{"grant": "g1"})
	default:
		refuse(404, "not_found")
	}
}

type signInApp struct {
	t    *testing.T
	s    *Server
	core *fakeCore
	srv  *httptest.Server
}

// newSignInApp serves a game's sign-in (with terms, if withLegal), on a
// database of its own and a fake Wakeshift.
func newSignInApp(t *testing.T, withLegal bool) *signInApp {
	t.Helper()
	st, err := OpenStore(context.Background(), pgtest.EmptyURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	core := &fakeCore{workspaces: map[string]string{}}
	coreSrv := httptest.NewServer(core)
	t.Cleanup(coreSrv.Close)
	_, key, _ := ed25519.GenerateKey(nil)
	srv := httptest.NewUnstartedServer(nil)
	s := &Server{Kit: &appkit.App{ID: "quiz", Key: key, Core: coreSrv.URL, MCPURL: "http://" + srv.Listener.Addr().String() + "/mcp"}, Name: "quiz", Title: "Agent's Quiz",
		Store: st, Static: fstest.MapFS{"logo.svg": {Data: []byte("<svg/>")}}, Logo: "logo.svg"}
	if withLegal {
		p, err := legal.New(legal.Config{App: "Quiz", Terms: []byte("# Terms\n"), Privacy: []byte("# Privacy\n")})
		if err != nil {
			t.Fatal(err)
		}
		s.Legal = p
	}
	mux := http.NewServeMux()
	s.oauthRoutes(mux)
	srv.Config.Handler = mux
	srv.Start()
	t.Cleanup(srv.Close)
	return &signInApp{t: t, s: s, core: core, srv: srv}
}

type registration struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	AuthMethod   string `json:"token_endpoint_auth_method"`
}

// register registers a client, returning the response's status and the
// registration (or the OAuth error).
func (a *signInApp) register(name string, redirects ...string) (int, registration, string) {
	a.t.Helper()
	return a.registerWith(map[string]any{"client_name": name, "redirect_uris": redirects})
}

// registerWith registers a client with the given metadata.
func (a *signInApp) registerWith(meta map[string]any) (int, registration, string) {
	a.t.Helper()
	b, _ := json.Marshal(meta)
	resp, err := http.Post(a.srv.URL+"/oauth/register", "application/json", bytes.NewReader(b))
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var reg registration
	var e struct {
		Error string `json:"error"`
	}
	json.Unmarshal(raw, &reg)
	json.Unmarshal(raw, &e)
	return resp.StatusCode, reg, e.Error
}

const testVerifier = "v-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func challengeOf(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// authorize runs authorize for a client as a browser would, clicking
// Authorize on the page if there is one, and returns where it sent the
// browser back to.
func (a *signInApp) authorize(clientID, redirect string) *url.URL {
	a.t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(a.srv.URL + "/oauth/authorize?" + url.Values{"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirect},
		"state": {"st"}, "code_challenge": {challengeOf(testVerifier)}, "code_challenge_method": {"S256"}}.Encode())
	if err != nil {
		a.t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		m := csrfField.FindStringSubmatch(string(body))
		if m == nil {
			a.t.Fatalf("the page has no form:\n%s", body)
		}
		if resp, err = c.PostForm(a.srv.URL+"/oauth/authorize", url.Values{"csrf": {m[1]}}); err != nil {
			a.t.Fatal(err)
		}
		resp.Body.Close()
	}
	if resp.StatusCode != http.StatusFound {
		a.t.Fatalf("authorize: %d", resp.StatusCode)
	}
	u, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		a.t.Fatal(err)
	}
	return u
}

type tokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Error        string `json:"error"`
}

func (a *signInApp) token(form url.Values) (int, tokens) {
	a.t.Helper()
	resp, err := http.PostForm(a.srv.URL+"/oauth/token", form)
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()
	var t tokens
	json.NewDecoder(resp.Body).Decode(&t)
	return resp.StatusCode, t
}

func codeForm(reg registration, redirect, code, verifier string) url.Values {
	return url.Values{"grant_type": {"authorization_code"}, "client_id": {reg.ClientID}, "client_secret": {reg.ClientSecret},
		"redirect_uri": {redirect}, "code": {code}, "code_verifier": {verifier}}
}

func refreshForm(reg registration, refresh string) url.Values {
	return url.Values{"grant_type": {"refresh_token"}, "client_id": {reg.ClientID}, "client_secret": {reg.ClientSecret}, "refresh_token": {refresh}}
}

// signIn registers a client, authorizes and exchanges the code.
func (a *signInApp) signIn(name string) (registration, tokens) {
	a.t.Helper()
	const redirect = "http://127.0.0.1:9/callback"
	code, reg, _ := a.register(name, redirect)
	if code != http.StatusCreated {
		a.t.Fatalf("register: %d", code)
	}
	back := a.authorize(reg.ClientID, redirect)
	status, tok := a.token(codeForm(reg, redirect, back.Query().Get("code"), testVerifier))
	if status != 200 || tok.AccessToken == "" || tok.RefreshToken == "" {
		a.t.Fatalf("token: %d %+v", status, tok)
	}
	return reg, tok
}

func (a *signInApp) sessions() int {
	var n int
	if err := a.s.Store.Pool.QueryRow(context.Background(), `SELECT count(*) FROM player_sessions`).Scan(&n); err != nil {
		a.t.Fatal(err)
	}
	return n
}

// The whole sign-in: register, the authorize page and its Authorize click
// (which starts the session), the code exchange (which makes the player in
// Wakeshift, named by the session), access tokens naming the session, and
// refreshes that replace the refresh token.
func TestSignIn(t *testing.T) {
	a := newSignInApp(t, true)
	const redirect = "http://localhost:43123/callback"
	code, reg, _ := a.register("Claude Code", redirect)
	if code != http.StatusCreated || reg.ClientID == "" || reg.ClientSecret == "" {
		t.Fatalf("register: %d", code)
	}
	if a.sessions() != 0 {
		t.Fatal("registering started a session")
	}
	back := a.authorize(reg.ClientID, redirect)
	if !strings.HasPrefix(back.String(), redirect+"?") || back.Query().Get("state") != "st" || back.Query().Get("iss") != a.srv.URL || back.Query().Get("code") == "" {
		t.Fatalf("authorize sent the browser to %s", back)
	}
	if a.sessions() != 1 || len(a.core.workspaces) != 0 {
		t.Fatalf("after Authorize: %d sessions, %d workspaces", a.sessions(), len(a.core.workspaces))
	}
	c := back.Query().Get("code")

	// Refused: the wrong verifier (which uses up the code), another
	// redirect URI, the wrong secret.
	if status, tok := a.token(codeForm(reg, redirect, c, "v-"+strings.Repeat("c", 44))); status != 400 || tok.Error != "invalid_grant" {
		t.Errorf("a wrong verifier: %d %s", status, tok.Error)
	}
	if status, tok := a.token(codeForm(reg, redirect, c, testVerifier)); status != 400 || tok.Error != "invalid_grant" {
		t.Errorf("a used code: %d %s", status, tok.Error)
	}
	back = a.authorize(reg.ClientID, redirect)
	c = back.Query().Get("code")
	if status, tok := a.token(codeForm(reg, "http://localhost:1/other", c, testVerifier)); status != 400 || tok.Error != "invalid_grant" {
		t.Errorf("another redirect: %d %s", status, tok.Error)
	}
	back = a.authorize(reg.ClientID, redirect)
	c = back.Query().Get("code")
	bad := codeForm(reg, redirect, c, testVerifier)
	bad.Set("client_secret", "nope")
	if status, tok := a.token(bad); status != 401 || tok.Error != "invalid_client" {
		t.Errorf("a wrong secret: %d %s", status, tok.Error)
	}
	// Another client can't use the code.
	_, other, _ := a.register("Codex", redirect)
	if status, tok := a.token(codeForm(other, redirect, c, testVerifier)); status != 400 || tok.Error != "invalid_grant" {
		t.Errorf("another client's code: %d %s", status, tok.Error)
	}

	back = a.authorize(reg.ClientID, redirect)
	status, tok := a.token(codeForm(reg, redirect, back.Query().Get("code"), testVerifier))
	if status != 200 || tok.ExpiresIn != 3600 {
		t.Fatalf("token: %d %+v", status, tok)
	}
	ti, err := a.s.verify(context.Background(), tok.AccessToken, nil)
	if err != nil {
		t.Fatal(err)
	}
	player := ti.UserID
	if !strings.HasPrefix(player, "p_") || ti.Extra["workspace"] != "ws-"+player || ti.Extra["agent"] != "claude-code" {
		t.Fatalf("access token: %+v", ti)
	}
	if len(a.core.agents) != 1 || a.core.agents[0] != "ws-"+player+"/claude-code" {
		t.Fatalf("agents: %v", a.core.agents)
	}
	if _, err := a.s.verify(context.Background(), tok.AccessToken+"x", nil); err == nil {
		t.Error("a tampered access token")
	}
	// A refresh token isn't an access token.
	if _, err := a.s.verify(context.Background(), tok.RefreshToken, nil); err == nil {
		t.Error("a refresh token as an access token")
	}

	// Refresh: a new refresh token, the same player; the old one stops
	// working, and only its own client may use one.
	status, tok2 := a.token(refreshForm(reg, tok.RefreshToken))
	if status != 200 || tok2.RefreshToken == "" || tok2.RefreshToken == tok.RefreshToken {
		t.Fatalf("refresh: %d %+v", status, tok2)
	}
	if ti2, err := a.s.verify(context.Background(), tok2.AccessToken, nil); err != nil || ti2.UserID != player {
		t.Fatalf("refreshed: %+v %v", ti2, err)
	}
	if status, t2 := a.token(refreshForm(reg, tok.RefreshToken)); status != 400 || t2.Error != "invalid_grant" {
		t.Errorf("a replaced refresh token: %d %s", status, t2.Error)
	}
	if status, t2 := a.token(refreshForm(other, tok2.RefreshToken)); status != 400 || t2.Error != "invalid_grant" {
		t.Errorf("another client's refresh token: %d %s", status, t2.Error)
	}
	if status, t3 := a.token(refreshForm(reg, tok2.RefreshToken)); status != 200 || t3.AccessToken == "" {
		t.Errorf("the current refresh token: %d %s", status, t3.Error)
	}
	// HTTP Basic works too.
	req, _ := http.NewRequest("POST", a.srv.URL+"/oauth/token", strings.NewReader(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"x"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(url.QueryEscape(reg.ClientID), url.QueryEscape(reg.ClientSecret))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 { // authenticated; the refresh token is wrong
		t.Errorf("basic auth: %d", resp.StatusCode)
	}

	// Each Authorize is a new session: a new player.
	_, tokB := a.signIn("Claude Code")
	if tiB, _ := a.s.verify(context.Background(), tokB.AccessToken, nil); tiB == nil || tiB.UserID == player {
		t.Error("a second sign-in is the same player")
	}
}

// A player whose grant is gone (forgotten, or revoked) can't refresh: its
// session ends, and Wakeshift is told to forget it.
func TestRefreshRevoked(t *testing.T) {
	a := newSignInApp(t, true)
	reg, tok := a.signIn("Claude Code")
	ti, _ := a.s.verify(context.Background(), tok.AccessToken, nil)
	a.core.revoked = true
	if status, t2 := a.token(refreshForm(reg, tok.RefreshToken)); status != 400 || t2.Error != "invalid_grant" {
		t.Fatalf("refresh: %d %s", status, t2.Error)
	}
	if a.sessions() != 0 || len(a.core.forgot) != 1 || a.core.forgot[0] != ti.UserID {
		t.Fatalf("sessions %d, forgot %v", a.sessions(), a.core.forgot)
	}
}

// Wakeshift's limit on new workspaces is said as OAuth says it.
func TestSignInWorkspaceRate(t *testing.T) {
	a := newSignInApp(t, true)
	const redirect = "http://127.0.0.1:9/callback"
	_, reg, _ := a.register("Claude Code", redirect)
	back := a.authorize(reg.ClientID, redirect)
	a.core.rateLimit = true
	if status, tok := a.token(codeForm(reg, redirect, back.Query().Get("code"), testVerifier)); status != 503 || tok.Error != "temporarily_unavailable" {
		t.Fatalf("token: %d %s", status, tok.Error)
	}
}

// Sessions idle for SessionIdle are forgotten, in Wakeshift and here; so
// are sessions whose exchange didn't finish, and sessions whose code was
// never exchanged are deleted. A session in use stays.
func TestExpireSessions(t *testing.T) {
	ctx := context.Background()
	a := newSignInApp(t, true)
	regIdle, idle := a.signIn("Claude Code")
	_, live := a.signIn("Codex")
	idleTI, _ := a.s.verify(ctx, idle.AccessToken, nil)
	liveTI, _ := a.s.verify(ctx, live.AccessToken, nil)
	// A session whose code was never exchanged.
	_, reg, _ := a.register("Cursor", "http://127.0.0.1:9/callback")
	a.authorize(reg.ClientID, "http://127.0.0.1:9/callback")
	now := time.Now()
	if _, err := a.s.Store.Pool.Exec(ctx, `UPDATE player_sessions SET active_at = $2 WHERE id = $1`, idleTI.UserID, now.Add(-SessionIdle-time.Hour)); err != nil {
		t.Fatal(err)
	}
	// Each has joined a game (joins.go): the idle one's joins go with it.
	for _, ti := range []*auth.TokenInfo{idleTI, liveTI} {
		if _, err := a.s.Store.Pool.Exec(ctx, `INSERT INTO joins (at, player, agent, name, returned, seated) VALUES (now(), $1, 'claude-code', 'raven', false, 1)`,
			ti.Extra["workspace"]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.s.Store.Pool.Exec(ctx, `UPDATE player_sessions SET created_at = $1 WHERE refresh_hash IS NULL`, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	n, err := a.s.expireSessions(ctx, now, 100)
	if err != nil || n != 1 {
		t.Fatalf("expired %d, %v", n, err)
	}
	if len(a.core.forgot) != 1 || a.core.forgot[0] != idleTI.UserID {
		t.Fatalf("forgot %v", a.core.forgot)
	}
	var ids []string
	rows, _ := a.s.Store.Pool.Query(ctx, `SELECT id FROM player_sessions`)
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	if len(ids) != 1 || ids[0] != liveTI.UserID {
		t.Fatalf("left: %v, want only %s", ids, liveTI.UserID)
	}
	var joinsLeft []string
	jrows, _ := a.s.Store.Pool.Query(ctx, `SELECT player FROM joins`)
	for jrows.Next() {
		var p string
		jrows.Scan(&p)
		joinsLeft = append(joinsLeft, p)
	}
	if len(joinsLeft) != 1 || joinsLeft[0] != liveTI.Extra["workspace"] {
		t.Fatalf("joins left: %v, want only the live session's", joinsLeft)
	}
	if status, tok := a.token(refreshForm(regIdle, idle.RefreshToken)); status != 400 || tok.Error != "invalid_grant" {
		t.Errorf("a forgotten session's refresh: %d %s", status, tok.Error)
	}

	// An exchange that failed (the code used, no tokens) is forgotten too,
	// after a while.
	_, reg, _ = a.register("Cursor", "http://127.0.0.1:9/callback")
	back := a.authorize(reg.ClientID, "http://127.0.0.1:9/callback")
	a.core.rateLimit = true
	a.token(codeForm(reg, "http://127.0.0.1:9/callback", back.Query().Get("code"), testVerifier))
	a.core.rateLimit = false
	if n, _ := a.s.expireSessions(ctx, now, 100); n != 0 {
		t.Fatal("a failed exchange forgotten at once")
	}
	if n, _ := a.s.expireSessions(ctx, now.Add(unclaimedFor+time.Minute), 100); n != 1 || a.sessions() != 1 {
		t.Fatalf("a failed exchange: forgot %d, %d left", n, a.sessions())
	}
}

// Registration accepts only redirect URIs a player's own agent can have:
// loopback, an app's own scheme, or a known web agent's callback
// (testdata/redirects.json, which Agent Hold'em runs too).
func TestRedirectValidation(t *testing.T) {
	var cases struct{ Accept, Reject []string }
	b, err := os.ReadFile("testdata/redirects.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &cases); err != nil || len(cases.Accept) == 0 || len(cases.Reject) == 0 {
		t.Fatalf("testdata/redirects.json: %v", err)
	}
	for _, u := range cases.Accept {
		if !allowedRedirect(u) {
			t.Errorf("refused %s", u)
		}
	}
	for _, u := range cases.Reject {
		if allowedRedirect(u) {
			t.Errorf("accepted %s", u)
		}
	}

	a := newSignInApp(t, true)
	if status, _, e := a.register("Evil", "http://127.0.0.1:9/cb", "https://evil.example/cb"); status != 400 || e != "invalid_redirect_uri" {
		t.Errorf("an evil redirect: %d %s", status, e)
	}
	if status, _, e := a.register("Lookalike", "https://claude.ai.evil.example/api/mcp/auth_callback"); status != 400 || e != "invalid_redirect_uri" {
		t.Errorf("a lookalike: %d %s", status, e)
	}
	if status, _, _ := a.register("Claude", "https://claude.ai/api/mcp/auth_callback"); status != http.StatusCreated {
		t.Errorf("Claude's callback: %d", status)
	}

	// A client registered before the check (guest sign-in took any https
	// redirect) is refused at authorize.
	old := a.s.seal(labelClient, client{Name: "Old", Redirects: []string{"https://evil.example/cb"}, Nonce: "n"})
	resp, err := noRedirects.Get(a.srv.URL + "/oauth/authorize?" + url.Values{"response_type": {"code"}, "client_id": {old}, "redirect_uri": {"https://evil.example/cb"},
		"code_challenge": {challengeOf(testVerifier)}, "code_challenge_method": {"S256"}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 || resp.Header.Get("Location") != "" {
		t.Errorf("an old client's evil redirect: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	// An exact match only.
	_, reg, _ := a.register("Claude Code", "http://127.0.0.1:9/cb")
	resp, err = noRedirects.Get(a.srv.URL + "/oauth/authorize?" + url.Values{"response_type": {"code"}, "client_id": {reg.ClientID}, "redirect_uri": {"http://127.0.0.1:9/cb/"},
		"code_challenge": {challengeOf(testVerifier)}, "code_challenge_method": {"S256"}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("a redirect URI that isn't exactly registered: %d", resp.StatusCode)
	}
}

// Registering and starting sessions are limited per address.
func TestSignInLimits(t *testing.T) {
	a := newSignInApp(t, true)
	a.s.registerLimit, a.s.sessionLimit = 2, 1
	const redirect = "http://127.0.0.1:9/cb"
	_, reg, _ := a.register("A", redirect)
	a.register("B", redirect)
	if status, _, e := a.register("C", redirect); status != 429 || e != "slow_down" {
		t.Errorf("a third registration: %d %s", status, e)
	}
	a.authorize(reg.ClientID, redirect)
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, _ := c.Get(a.srv.URL + "/oauth/authorize?" + url.Values{"response_type": {"code"}, "client_id": {reg.ClientID}, "redirect_uri": {redirect},
		"code_challenge": {challengeOf(testVerifier)}, "code_challenge_method": {"S256"}}.Encode())
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	m := csrfField.FindStringSubmatch(string(body))
	if m == nil {
		t.Fatal("no page")
	}
	if resp, _ = c.PostForm(a.srv.URL+"/oauth/authorize", url.Values{"csrf": {m[1]}}); resp.StatusCode != 429 {
		t.Errorf("a second session from one address: %d", resp.StatusCode)
	}
	if a.sessions() != 1 {
		t.Errorf("%d sessions", a.sessions())
	}
}

// The metadata points MCP clients at the game's own sign-in, and says it
// sends the issuer back (RFC 9207, which ChatGPT needs for its stable
// callback).
func TestOAuthMetadata(t *testing.T) {
	a := newSignInApp(t, true)
	resp, err := http.Get(a.srv.URL + "/.well-known/oauth-authorization-server")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	json.NewDecoder(resp.Body).Decode(&m)
	if m["issuer"] != a.srv.URL || m["registration_endpoint"] != a.srv.URL+"/oauth/register" || m["authorization_response_iss_parameter_supported"] != true {
		t.Errorf("metadata: %v", m)
	}
	if methods, _ := m["token_endpoint_auth_methods_supported"].([]any); !slices.Contains(methods, any("none")) {
		t.Errorf("public clients aren't advertised: %v", methods)
	}
}

// A client that registers as public (token_endpoint_auth_method "none")
// gets no secret, and signs in and refreshes with its client ID alone.
// Codex registers this way and keeps no client secret: its refresh sends
// only the client ID, which a confidential registration refused
// (invalid_client), so its tool calls failed an hour after sign-in, the
// game took its player out as away and the user had to sign in again.
func TestPublicClient(t *testing.T) {
	a := newSignInApp(t, true)
	const redirect = "http://127.0.0.1:1455/callback"
	code, reg, _ := a.registerWith(map[string]any{"client_name": "Codex", "redirect_uris": []string{redirect}, "token_endpoint_auth_method": "none"})
	if code != http.StatusCreated || reg.ClientID == "" || reg.ClientSecret != "" || reg.AuthMethod != "none" {
		t.Fatalf("register: %d %+v", code, reg)
	}
	back := a.authorize(reg.ClientID, redirect)
	form := codeForm(reg, redirect, back.Query().Get("code"), testVerifier)
	form.Del("client_secret")
	status, tok := a.token(form)
	if status != 200 || tok.RefreshToken == "" {
		t.Fatalf("code exchange: %d %+v", status, tok)
	}
	ti, err := a.s.verify(context.Background(), tok.AccessToken, nil)
	if err != nil || ti.Extra["agent"] != "codex" {
		t.Fatalf("access token: %+v %v", ti, err)
	}

	// PKCE still binds the code.
	back = a.authorize(reg.ClientID, redirect)
	form = codeForm(reg, redirect, back.Query().Get("code"), "v-"+strings.Repeat("c", 44))
	form.Del("client_secret")
	if status, t2 := a.token(form); status != 400 || t2.Error != "invalid_grant" {
		t.Errorf("a wrong verifier: %d %s", status, t2.Error)
	}

	// The refresh Codex sends: the client ID, no secret.
	refresh := url.Values{"grant_type": {"refresh_token"}, "client_id": {reg.ClientID}, "refresh_token": {tok.RefreshToken}}
	status, tok2 := a.token(refresh)
	if status != 200 || tok2.RefreshToken == "" || tok2.RefreshToken == tok.RefreshToken {
		t.Fatalf("refresh: %d %+v", status, tok2)
	}
	if ti2, err := a.s.verify(context.Background(), tok2.AccessToken, nil); err != nil || ti2.UserID != ti.UserID {
		t.Fatalf("refreshed: %+v %v", ti2, err)
	}
	// Single use: the replaced one stops working.
	if status, t2 := a.token(refresh); status != 400 || t2.Error != "invalid_grant" {
		t.Errorf("a replaced refresh token: %d %s", status, t2.Error)
	}

	// A public client has no secret to send: one sent, or Basic, is refused.
	withSecret := url.Values{"grant_type": {"refresh_token"}, "client_id": {reg.ClientID}, "client_secret": {"x"}, "refresh_token": {tok2.RefreshToken}}
	if status, t2 := a.token(withSecret); status != 401 || t2.Error != "invalid_client" {
		t.Errorf("a public client with a secret: %d %s", status, t2.Error)
	}
	req, _ := http.NewRequest("POST", a.srv.URL+"/oauth/token", strings.NewReader(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok2.RefreshToken}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(url.QueryEscape(reg.ClientID), "")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("a public client by Basic: %d", resp.StatusCode)
	}

	// Another client can't use its refresh token, public or not.
	_, other, _ := a.registerWith(map[string]any{"client_name": "Codex", "redirect_uris": []string{redirect}, "token_endpoint_auth_method": "none"})
	if status, t2 := a.token(url.Values{"grant_type": {"refresh_token"}, "client_id": {other.ClientID}, "refresh_token": {tok2.RefreshToken}}); status != 400 || t2.Error != "invalid_grant" {
		t.Errorf("another public client's refresh token: %d %s", status, t2.Error)
	}

	// A client registered with a secret still needs it.
	conf, ctok := a.signIn("Claude Code")
	if conf.ClientSecret == "" || conf.AuthMethod != "client_secret_post" {
		t.Fatalf("a confidential registration: %+v", conf)
	}
	if status, t2 := a.token(url.Values{"grant_type": {"refresh_token"}, "client_id": {conf.ClientID}, "refresh_token": {ctok.RefreshToken}}); status != 401 || t2.Error != "invalid_client" {
		t.Errorf("a confidential client without its secret: %d %s", status, t2.Error)
	}
	if status, t2 := a.token(refreshForm(conf, ctok.RefreshToken)); status != 200 {
		t.Errorf("a confidential client with its secret: %d %s", status, t2.Error)
	}
}
