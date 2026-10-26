// Package slackline is Slackline: chat with your own agents (Claude Code,
// Codex and the rest) from Slack. A headless app on Wakeshift's events
// (its slack.message event): people sign in with Slack,
// not with Wakeshift.
//
//   - A workspace adds the Slack app; people sign in to Slackline's console
//     with Slack.
//   - An agent adds Slackline's MCP server and signs in through OAuth: the
//     person approves it in the browser, and it's theirs (the console lists
//     it; they can rename or revoke it).
//   - The agent calls watch: its conversation gets a thread in the person's
//     DM with the app, and is woken by their replies there.
//   - In the console, the person chooses channels each agent also listens
//     in: every message or only @Slackline mentions, from them only or from
//     anyone in the channel. Replies in threads an agent posted in reach it
//     too, under the same rule.
//
// What reaches an agent is decided only from Slack's signed events and the
// person's own rules; agents' text is escaped, so it can't mention
// @channel, people or groups. Slackline stores no message text.
package slackline

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wakeshift/apps/agentpicker"
	"github.com/wakeshift/apps/legal"
	"github.com/wakeshift/wakeshift-go/appkit"
)

// App serves Slackline.
type App struct {
	Kit    *appkit.App // the app's Wakeshift client (events)
	Store  *Store
	Slack  *Slack
	URL    string // this server's public URL, without a trailing slash
	Secret []byte // signs sessions, OAuth clients and access tokens (32+ bytes)
	Now    func() time.Time

	limits  sync.Map     // listener ID → *window (agent posts per minute)
	calls   atomic.Int64 // allowPost calls, for sweeping limits
	members sync.Map     // team/user → membership (the channels they're in)
	people  sync.Map     // team/user → cachedPerson (users.info)

	eventsOnce   sync.Once
	slackMessage *appkit.DefinedEvent[MessageArgs, Message]
}

func (a *App) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// Handler serves Slackline's routes.
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.landing)
	mux.Handle("GET /agent-picker/", agentpicker.Handler())
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok") })
	mux.HandleFunc("GET /slack/manifest.json", a.manifest)
	legalPages.Routes(mux) // /terms, /tos, /privacy

	// People: Sign in with Slack, adding the app to a workspace, the console.
	mux.HandleFunc("GET /signin", a.signin)
	mux.HandleFunc("GET /auth/slack/callback", a.signinCallback)
	mux.HandleFunc("POST /signout", a.form(a.signout))
	mux.HandleFunc("POST /understood", a.form(a.understood))
	mux.HandleFunc("GET /install", a.install)
	mux.HandleFunc("GET /slack/callback", a.installCallback)
	mux.HandleFunc("GET /home", a.home)
	mux.HandleFunc("POST /agents/{id}/label", a.form(a.renameAgent))
	mux.HandleFunc("POST /agents/{id}/revoke", a.form(a.revokeAgent))
	mux.HandleFunc("POST /agents/{id}/rules", a.form(a.setRule))
	mux.HandleFunc("POST /agents/{id}/rules/{channel}/remove", a.form(a.removeRule))

	// Slack: events and /slackline.
	mux.HandleFunc("POST /slack/events", a.events)
	mux.HandleFunc("POST /slack/commands", a.command)

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
		// Either way, no URL (with its OAuth code) goes to another site.
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("X-Frame-Options", "DENY")
		h.ServeHTTP(w, r)
	})
}

// MCPURL is the MCP endpoint agents add.
func (a *App) MCPURL() string { return a.URL + "/mcp" }

// ── Identities ───────────────────────────────────────────────────────────

// wsUser is the person's ID in Wakeshift: their workspace (headless, kept by
// Wakeshift for Slackline), and the user of their slack.message events.
func wsUser(team, user string) string {
	return strings.ToLower(team) + "." + strings.ToLower(user)
}

// mac is an HMAC of parts, for one purpose, keyed by the app's secret.
func (a *App) mac(purpose string, parts ...string) []byte {
	m := hmac.New(sha256.New, a.Secret)
	m.Write([]byte("slackline/" + purpose))
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

func newID() string {
	b := make([]byte, 10)
	rand.Read(b)
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
}

func newSecret() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (a *App) secure() bool { return strings.HasPrefix(a.URL, "https://") }

// ── Web sessions ─────────────────────────────────────────────────────────

const (
	sessionCookie = "slackline_session"
	sessionTTL    = 30 * 24 * time.Hour
)

// startSession signs the user in in this browser: team.user.version.exp.mac.
func (a *App) startSession(w http.ResponseWriter, u User) {
	exp := strconv.FormatInt(a.now().Add(sessionTTL).Unix(), 10)
	v := strconv.Itoa(u.SessionVersion)
	val := u.TeamID + "." + u.UserID + "." + v + "." + exp + "." + a.macString("session", u.TeamID, u.UserID, v, exp)
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: val, Path: "/", MaxAge: int(sessionTTL.Seconds()), HttpOnly: true,
		Secure: a.secure(), SameSite: http.SameSiteLaxMode})
}

// user is who's signed in in this browser, if anyone.
func (a *App) user(r *http.Request) (User, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return User{}, false
	}
	p := strings.Split(c.Value, ".")
	if len(p) != 5 || !a.checkMAC("session", p[4], p[0], p[1], p[2], p[3]) {
		return User{}, false
	}
	exp, err := strconv.ParseInt(p[3], 10, 64)
	if err != nil || a.now().Unix() > exp {
		return User{}, false
	}
	u, err := a.Store.User(r.Context(), p[0], p[1])
	if err != nil || strconv.Itoa(u.SessionVersion) != p[2] {
		return User{}, false
	}
	return u, true
}

// csrf is the form token for this browser's session.
func (a *App) csrf(r *http.Request) string {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	return a.macString("csrf", c.Value)
}

// form wraps a signed-in POST: same origin, and the session's form token.
func (a *App) form(h func(http.ResponseWriter, *http.Request, User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); o != "" && o != a.URL {
			http.Error(w, "cross-origin form", http.StatusForbidden)
			return
		}
		u, ok := a.user(r)
		if !ok {
			http.Redirect(w, r, "/signin", http.StatusSeeOther)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		if err := r.ParseForm(); err != nil || !hmac.Equal([]byte(r.PostForm.Get("csrf")), []byte(a.csrf(r))) {
			http.Error(w, "this form expired; reload the page", http.StatusForbidden)
			return
		}
		h(w, r, u)
	}
}

// localPath is a path on this site to go to after signing in, or "/home".
// Browsers drop tabs and newlines from URLs and treat backslashes as
// slashes, so "/\t/evil.example" would become "//evil.example": any control
// character or backslash, or anything with a scheme or host, is refused.
func localPath(next string) string {
	if next == "" || len(next) >= 4096 || next[0] != '/' || strings.HasPrefix(next, "//") {
		return "/home"
	}
	for _, r := range next {
		if r < 0x20 || r == 0x7f || r == '\\' {
			return "/home"
		}
	}
	if u, err := url.Parse(next); err != nil || u.Scheme != "" || u.Host != "" || u.Opaque != "" {
		return "/home"
	}
	return next
}

// ── OAuth state for Slack (sign-in and install) ──────────────────────────

const stateCookie = "slackline_state"

// startState is an OAuth state bound to this browser (a nonce in a
// cookie), carrying where to go after: nonce.exp.next.mac.
func (a *App) startState(w http.ResponseWriter, purpose, path, next string) string {
	nonce := newID()
	exp := strconv.FormatInt(a.now().Add(10*time.Minute).Unix(), 10)
	n := base64.RawURLEncoding.EncodeToString([]byte(next))
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Value: nonce, Path: path, MaxAge: 600, HttpOnly: true, Secure: a.secure(), SameSite: http.SameSiteLaxMode})
	return nonce + "." + exp + "." + n + "." + a.macString("state/"+purpose, nonce, exp, n)
}

// checkState returns where to go after, if the state is this browser's.
func (a *App) checkState(r *http.Request, purpose string) (string, bool) {
	p := strings.Split(r.URL.Query().Get("state"), ".")
	c, err := r.Cookie(stateCookie)
	if len(p) != 4 || err != nil || c.Value != p[0] || !a.checkMAC("state/"+purpose, p[3], p[0], p[1], p[2]) {
		return "", false
	}
	exp, err := strconv.ParseInt(p[1], 10, 64)
	next, err2 := base64.RawURLEncoding.DecodeString(p[2])
	if err != nil || err2 != nil || a.now().Unix() > exp {
		return "", false
	}
	return string(next), true
}

func (a *App) signin(w http.ResponseWriter, r *http.Request) {
	state := a.startState(w, "signin", "/auth/slack/callback", localPath(r.URL.Query().Get("next")))
	q := url.Values{"response_type": {"code"}, "scope": {strings.Join(SignInScopes, " ")}, "client_id": {a.Slack.ClientID},
		"redirect_uri": {a.URL + "/auth/slack/callback"}, "state": {state}}
	if team := r.URL.Query().Get("team"); slackID(team) {
		q.Set("team", team)
	}
	http.Redirect(w, r, a.Slack.api()+"/openid/connect/authorize?"+q.Encode(), http.StatusFound)
}

func (a *App) signinCallback(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("error") != "" {
		a.message(w, r, http.StatusOK, "You didn't sign in.", "")
		return
	}
	next, ok := a.checkState(r, "signin")
	if !ok {
		a.message(w, r, http.StatusBadRequest, "That sign-in was started in another browser, or expired.", "/signin")
		return
	}
	id, err := a.Slack.signIn(r.Context(), r.URL.Query().Get("code"), a.URL+"/auth/slack/callback")
	if err != nil {
		slog.Warn("slackline: sign-in failed", "err", err)
		a.message(w, r, http.StatusBadGateway, "Slack didn't finish signing you in.", "/signin")
		return
	}
	if ok, err := a.mayConnect(r.Context(), User{TeamID: id.TeamID, UserID: id.UserID}); err != nil {
		a.failed(w, r, err)
		return
	} else if !ok {
		a.message(w, r, http.StatusForbidden, notMemberText, "")
		return
	}
	u, err := a.Store.SignIn(r.Context(), User{TeamID: id.TeamID, UserID: id.UserID, Name: id.Name, TeamName: id.TeamName})
	if err != nil {
		a.failed(w, r, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Path: "/auth/slack/callback", MaxAge: -1})
	a.startSession(w, u)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// understood records that the person read where their agents' messages go
// (needed before they connect an agent), audited.
func (a *App) understood(w http.ResponseWriter, r *http.Request, u User) {
	if err := a.Store.Understand(r.Context(), u.TeamID, u.UserID, "console"); err != nil {
		a.failed(w, r, err)
		return
	}
	http.Redirect(w, r, "/home#connect", http.StatusSeeOther)
}

func (a *App) signout(w http.ResponseWriter, r *http.Request, u User) {
	if r.PostForm.Get("everywhere") != "" {
		if err := a.Store.EndSessions(r.Context(), u.TeamID, u.UserID); err != nil {
			a.failed(w, r, err)
			return
		}
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// install adds the app to a workspace (Slack asks an admin if it must).
func (a *App) install(w http.ResponseWriter, r *http.Request) {
	state := a.startState(w, "install", "/slack/callback", "/home")
	q := url.Values{"client_id": {a.Slack.ClientID}, "scope": {strings.Join(BotScopes, ",")}, "redirect_uri": {a.URL + "/slack/callback"}, "state": {state}}
	http.Redirect(w, r, a.Slack.api()+"/oauth/v2/authorize?"+q.Encode(), http.StatusFound)
}

func (a *App) installCallback(w http.ResponseWriter, r *http.Request) {
	if e := r.URL.Query().Get("error"); e != "" {
		a.message(w, r, http.StatusOK, "Slack didn't add Slackline.", "/")
		return
	}
	if _, ok := a.checkState(r, "install"); !ok {
		a.message(w, r, http.StatusBadRequest, "That install was started in another browser, or expired.", "/install")
		return
	}
	in, err := a.Slack.exchange(r.Context(), r.URL.Query().Get("code"), a.URL+"/slack/callback")
	if err != nil {
		slog.Warn("slackline: install failed", "err", err)
		a.message(w, r, http.StatusBadGateway, "Slack didn't finish adding Slackline.", "/install")
		return
	}
	// A workspace reinstalling before its earlier data is deleted: finish
	// deleting it first, so the deletion never takes the new install.
	// It holds the workspace's deletion lock from the check to the save, so
	// no deletion runs in between (and one recorded meanwhile keeps the new
	// install: ForgetTeam deletes only installs from before it).
	release, ok, err := a.Store.LockDeletion(r.Context(), in.TeamID)
	if err != nil {
		a.failed(w, r, err)
		return
	}
	if !ok {
		a.message(w, r, http.StatusServiceUnavailable, "Slackline is still deleting this workspace's earlier data. Try adding it again in a few minutes.", "/install")
		return
	}
	defer release()
	if err := a.deleteTeamLocked(r.Context(), in.TeamID); err != nil {
		slog.Warn("slackline: finishing a deletion before a reinstall failed", "team", in.TeamID, "err", err)
		a.message(w, r, http.StatusServiceUnavailable, "Slackline is still deleting this workspace's earlier data. Try adding it again in a few minutes.", "/install")
		return
	}
	if err := a.Store.SaveTeam(r.Context(), Team{ID: in.TeamID, Name: in.TeamName, BotToken: in.BotToken, BotUserID: in.BotUserID}, in.InstalledBy); err != nil {
		a.failed(w, r, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Path: "/slack/callback", MaxAge: -1})
	slog.Info("slackline: installed", "team", in.TeamID)
	http.Redirect(w, r, "/signin?team="+url.QueryEscape(in.TeamID), http.StatusSeeOther)
}

// ── Console ──────────────────────────────────────────────────────────────

func (a *App) landing(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.user(r); ok {
		http.Redirect(w, r, "/home", http.StatusSeeOther)
		return
	}
	a.render(w, r, http.StatusOK, "landing", nil)
}

type agentView struct {
	Agent
	Listeners []Listener
	Rules     []Rule
}

type homeView struct {
	User      User
	Installed bool
	Agents    []agentView
	Channels  []Channel // the app is in (for new rules)
	NotIn     int       // channels the app can see but isn't in
	Log       []Entry
	Labels    map[string]string // agent ID → label, for the activity log
	Notice    string
}

var notices = map[string]string{
	"renamed": "Renamed.", "revoked": "Revoked: it can't reach Slackline any more.", "rule": "Saved.", "removed": "Removed.",
}

func (a *App) home(w http.ResponseWriter, r *http.Request) {
	u, ok := a.user(r)
	if !ok {
		http.Redirect(w, r, "/signin?next=/home", http.StatusSeeOther)
		return
	}
	ctx := r.Context()
	v := homeView{User: u, Notice: notices[r.URL.Query().Get("done")], Labels: map[string]string{}}
	team, err := a.Store.Team(ctx, u.TeamID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		a.failed(w, r, err)
		return
	}
	v.Installed = err == nil
	agents, err := a.Store.Agents(ctx, u.TeamID, u.UserID)
	if err != nil {
		a.failed(w, r, err)
		return
	}
	for _, ag := range agents {
		v.Labels[ag.ID] = ag.Label
		if ag.Revoked {
			continue
		}
		av := agentView{Agent: ag}
		if av.Listeners, err = a.Store.Listeners(ctx, ag.ID); err != nil {
			a.failed(w, r, err)
			return
		}
		if av.Rules, err = a.Store.Rules(ctx, ag.ID); err != nil {
			a.failed(w, r, err)
			return
		}
		v.Agents = append(v.Agents, av)
	}
	if v.Installed && len(v.Agents) > 0 {
		chans, mine, err := a.choosable(ctx, team, u.UserID)
		if err != nil {
			slog.Warn("slackline: listing channels failed", "err", err)
		}
		v.Channels = chans
		v.NotIn = mine
	}
	if v.Log, err = a.Store.Log(ctx, u.TeamID, u.UserID, 20); err != nil {
		a.failed(w, r, err)
		return
	}
	a.render(w, r, http.StatusOK, "home", v)
}

func cleanLabel(label, fallback string) string {
	label = strings.Join(strings.Fields(label), " ")
	if label == "" {
		label = fallback
	}
	if label == "" {
		label = "agent"
	}
	return truncate(label, 40)
}

// personLabel is the message for a label that's a person's name.
const personLabel = "That's a person's name in your workspace. Choose a name that doesn't look like a person (posts also say whose agent it is)."

func (a *App) renameAgent(w http.ResponseWriter, r *http.Request, u User) {
	label := cleanLabel(r.PostForm.Get("label"), "")
	if taken, err := a.takenLabel(r.Context(), u.TeamID, u.UserID, label); err != nil {
		a.failed(w, r, err)
		return
	} else if taken {
		a.message(w, r, http.StatusBadRequest, personLabel, "/home")
		return
	}
	if err := a.Store.RenameAgent(r.Context(), u.TeamID, u.UserID, r.PathValue("id"), label); err != nil {
		a.formFailed(w, r, err)
		return
	}
	http.Redirect(w, r, "/home?done=renamed", http.StatusSeeOther)
}

func (a *App) revokeAgent(w http.ResponseWriter, r *http.Request, u User) {
	ctx := r.Context()
	id := r.PathValue("id")
	ag, err := a.Store.Agent(ctx, id)
	if err != nil || ag.TeamID != u.TeamID || ag.UserID != u.UserID {
		a.formFailed(w, r, ErrNotFound)
		return
	}
	ended, err := a.Store.RevokeAgent(ctx, u.TeamID, u.UserID, id)
	if err != nil {
		a.formFailed(w, r, err)
		return
	}
	a.announceAllStopped(ctx, u.TeamID, ended, "it was revoked")
	// Its conversations stop being woken (each is told). Ends that fail are
	// retried by EndPending until Wakeshift confirms them.
	go a.EndPending(context.WithoutCancel(ctx))
	http.Redirect(w, r, "/home?done=revoked", http.StatusSeeOther)
}

func (a *App) setRule(w http.ResponseWriter, r *http.Request, u User) {
	ctx := r.Context()
	rule := Rule{AgentID: r.PathValue("id"), ChannelID: r.PostForm.Get("channel"), Mode: r.PostForm.Get("mode"), Senders: r.PostForm.Get("senders")}
	if !slackID(rule.ChannelID) || (rule.Mode != "mentions" && rule.Mode != "all") || (rule.Senders != "me" && rule.Senders != "anyone") {
		a.message(w, r, http.StatusBadRequest, "Choose a channel, what wakes the agent, and from whom.", "/home")
		return
	}
	if rule.Senders == "anyone" && r.PostForm.Get("understood") == "" {
		a.message(w, r, http.StatusBadRequest, "To let anyone in the channel reach this agent, tick the box that says you understand what that allows.", "/home")
		return
	}
	team, err := a.Store.Team(ctx, u.TeamID)
	if err != nil {
		a.formFailed(w, r, err)
		return
	}
	// The channel must be one the person is in (an agent can't read a
	// channel its person can't), and Slackline too (or Slack won't send its
	// messages).
	a.members.Delete(u.TeamID + "/" + u.UserID)
	chans, _, err := a.choosable(ctx, team, u.UserID)
	if err != nil {
		slog.Warn("slackline: listing channels failed", "err", err)
		a.message(w, r, http.StatusBadGateway, "Slack didn't list your channels; try again.", "/home")
		return
	}
	for _, c := range chans {
		if c.ID == rule.ChannelID {
			rule.ChannelName = c.Name
		}
	}
	if rule.ChannelName == "" {
		a.message(w, r, http.StatusBadRequest, "That isn't a channel you and Slackline are both in. Join it, and invite Slackline there (/invite @Slackline).", "/home")
		return
	}
	// The channel is told when an agent starts listening there, or starts
	// hearing more, before the rule is saved: if Slack refuses the notice,
	// nothing changes, and SetRule refuses a broader rule that wasn't
	// announced. If saving then fails, the channel is told it didn't happen.
	ag, prev, err := a.Store.RuleOf(ctx, u.TeamID, u.UserID, rule.AgentID, rule.ChannelID)
	if err != nil {
		a.formFailed(w, r, err)
		return
	}
	announced := broadens(prev, rule)
	if announced {
		if err := a.announce(ctx, team, ag, rule); err != nil {
			slog.Warn("slackline: posting a listening notice failed", "err", err)
			a.message(w, r, http.StatusBadGateway, "Slack didn't post the notice telling the channel your agent is listening, so nothing changed. Try again.", "/home")
			return
		}
	}
	if err := a.Store.SetRule(ctx, u.TeamID, u.UserID, rule, prev, announced); err != nil {
		if announced {
			a.note(context.WithoutCancel(ctx), team, rule.ChannelID, "", ":warning: The change to *"+plain(ag.Label)+
				"* announced above didn't happen. `/slackline here` shows what each agent here hears.")
		}
		if errors.Is(err, ErrUnannounced) {
			a.message(w, r, http.StatusConflict, "The rule changed while you were saving it. Try again.", "/home")
			return
		}
		a.formFailed(w, r, err)
		return
	}
	http.Redirect(w, r, "/home?done=rule", http.StatusSeeOther)
}

// choosable are the channels a person can give an agent: ones both they and
// Slackline are in. mine counts the person's channels Slackline isn't in.
func (a *App) choosable(ctx context.Context, team Team, user string) ([]Channel, int, error) {
	chans, err := a.Slack.channels(ctx, team.BotToken)
	if err != nil {
		return nil, 0, err
	}
	in, err := a.channelsOf(ctx, team, user)
	if err != nil {
		return nil, 0, err
	}
	var out []Channel
	mine := 0
	for _, c := range chans {
		switch {
		case !in[c.ID]:
		case c.IsMember:
			out = append(out, c)
		default:
			mine++
		}
	}
	return out, mine, nil
}

func (a *App) removeRule(w http.ResponseWriter, r *http.Request, u User) {
	ended, err := a.Store.RemoveRule(r.Context(), u.TeamID, u.UserID, r.PathValue("id"), r.PathValue("channel"))
	if err != nil {
		a.formFailed(w, r, err)
		return
	}
	a.announceAllStopped(r.Context(), u.TeamID, []EndedRule{ended}, "")
	http.Redirect(w, r, "/home?done=removed", http.StatusSeeOther)
}

// announceAllStopped tells each ended rule's channel (best effort).
func (a *App) announceAllStopped(ctx context.Context, team string, ended []EndedRule, why string) {
	if len(ended) == 0 {
		return
	}
	t, err := a.Store.Team(ctx, team)
	if err != nil {
		return
	}
	for _, e := range ended {
		a.announceStopped(ctx, t, e, why)
	}
}

// EndPending ends revoked agents' subscriptions that aren't confirmed ended
// yet (after a revoke, and periodically: cmd/slackline). Ending one that
// already ended counts as ended.
func (a *App) EndPending(ctx context.Context) {
	ends, err := a.Store.PendingEnds(ctx, 200)
	if err != nil {
		slog.Warn("slackline: listing subscriptions to end failed", "err", err)
		return
	}
	for _, e := range ends {
		if err := a.endListener(ctx, e); err != nil {
			slog.Warn("slackline: ending a revoked agent's subscription failed; will retry", "err", err)
			continue
		}
		if err := a.Store.MarkEnded(ctx, e.Listener); err != nil {
			slog.Warn("slackline: recording an ended subscription failed", "err", err)
		}
	}
}

// endListener ends a listener's subscriptions: the one watch made (an
// event subscription, or a topic one from before slack.message), and any a
// client that subscribes itself made.
func (a *App) endListener(ctx context.Context, e Ending) error {
	user := wsUser(e.Team, e.User)
	if e.Subscription != "" {
		if err := a.endSubscription(ctx, user, e.Subscription); err != nil {
			return err
		}
	}
	return a.endNative(ctx, user, e.Listener)
}

// endSubscription ends the subscription watch made for one of user's
// listeners: a slack.message one, else a topic one from before it.
func (a *App) endSubscription(ctx context.Context, user, id string) error {
	ended, err := a.messages().End(ctx, user, id)
	if err != nil || ended {
		return err
	}
	_, err = a.Kit.EndSubscription(ctx, id)
	return err
}

// ── Pages ────────────────────────────────────────────────────────────────

//go:embed templates/*.html
var templateFiles embed.FS

var pages = template.Must(template.New("").Funcs(template.FuncMap{
	"since": func(t time.Time) string { return t.UTC().Format("Jan 2 15:04 UTC") },
	"initial": func(label string) string {
		for _, r := range label {
			return strings.ToUpper(string(r))
		}
		return "?"
	},
	"activity": activity,
}).ParseFS(templateFiles, "templates/*.html"))

// activity says what an audit entry records, in words; labels names the
// person's agents by ID.
func activity(e Entry, labels map[string]string) string {
	str := func(k string) string { v, _ := e.Detail[k].(string); return v }
	agent, Agent := "an agent", "An agent" // mid-sentence, starting one
	if l := labels[str("agent")]; l != "" {
		agent, Agent = l, l
	}
	switch e.Action {
	case "user.signed_in":
		return "You signed in"
	case "user.signed_out_everywhere":
		return "You signed out everywhere"
	case "agent.connected":
		if c := str("client"); c != "" {
			return "Connected " + agent + " (" + c + ")"
		}
		return "Connected " + agent
	case "agent.authorized_again":
		return Agent + " signed in again"
	case "agent.renamed":
		return "Renamed an agent to “" + str("label") + "”"
	case "agent.revoked":
		return "Revoked " + agent
	case "rule.set":
		what, who := "@Slackline mentions", "from you only"
		if str("mode") == "all" {
			what = "every message"
		}
		if str("senders") == "anyone" {
			who = "from anyone there"
		}
		return Agent + " now listens in #" + str("channel_name") + " (" + what + ", " + who + ")"
	case "rule.removed":
		if str("why") != "" {
			return "Your agents stopped listening in a channel you left"
		}
		return Agent + " stopped listening in a channel"
	case "user.understood":
		return "You confirmed where your agents' messages go"
	case "user.deactivated":
		return "Your Slack account was deactivated, so your agents were revoked"
	case "workspace.forgotten":
		return "Slackline was removed from your Slack workspace"
	}
	return e.Action
}

type pageData struct {
	CSRF   string
	MCPURL string
	// Wakeshift is Wakeshift's homepage (legal.Wakeshift), linked from the footer.
	Wakeshift string
	Data      any
	// The "connect your agent" picker (apps/agentpicker) and its tags.
	Picker, PickerHead template.HTML
}

func (a *App) render(w http.ResponseWriter, r *http.Request, status int, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Inline styles; the agent picker's script and stylesheet from this origin.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self' 'unsafe-inline'; script-src 'self'; frame-ancestors 'none'")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	picker := agentpicker.HTML(agentpicker.Options{
		Name: "slackline", URL: a.MCPURL(), Ask: "Watch Slack for me.",
		Each: "each Slack message for your agent", Interactive: true,
		SignIn: "The first time, sign in with Slack in your browser, then approve the agent here.",
	})
	if err := pages.ExecuteTemplate(w, name, pageData{CSRF: a.csrf(r), MCPURL: a.MCPURL(), Wakeshift: legal.Wakeshift, Data: data, Picker: picker, PickerHead: agentpicker.Head()}); err != nil {
		slog.Error("slackline: rendering a page failed", "page", name, "err", err)
	}
}

type messageView struct{ Text, Next string }

func (a *App) message(w http.ResponseWriter, r *http.Request, status int, text, next string) {
	a.render(w, r, status, "message", messageView{Text: text, Next: next})
}

func (a *App) failed(w http.ResponseWriter, r *http.Request, err error) {
	slog.Error("slackline: request failed", "path", r.URL.Path, "err", err)
	a.message(w, r, http.StatusInternalServerError, "Something went wrong. Try again.", "/home")
}

func (a *App) formFailed(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, ErrNotFound) {
		a.message(w, r, http.StatusNotFound, "That isn't one of your agents (or it was revoked).", "/home")
		return
	}
	a.failed(w, r, err)
}
