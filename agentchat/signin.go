package agentchat

import (
	"context"
	"crypto/hmac"
	"errors"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"net/mail"
	"net/netip"
	"net/url"
	"strings"
	"time"
	"unicode"
)

// Signing in: a person asks for a link by
// email; it's good for 15 minutes, once. Opening it shows a Sign in button
// (mail scanners open links, but don't press buttons), which spends it and
// starts a session: a random token in an HttpOnly, SameSite=Lax cookie,
// stored hashed, for 30 days, rotated daily. Every form is CSRF-checked:
// before sign-in, against a token bound to a SameSite=Strict cookie; after,
// against one derived from the session; and its Origin must be this site.

const (
	sessionCookie = "agentchat_session"
	sessionTTL    = 30 * 24 * time.Hour
	rotateAfter   = 24 * time.Hour
	csrfCookie    = "agentchat_csrf"
	linkTTL       = 15 * time.Minute

	// Rate limits, per hour: links asked for per email and per address,
	// links tried per address, invite codes looked up and not found per
	// address.
	linksPerEmail    = 5
	linksPerAddr     = 20
	verifiesPerAddr  = 30
	badInvitesPerAdr = 30
)

// ── Sessions ─────────────────────────────────────────────────────────────

func (a *App) setSession(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", MaxAge: int(sessionTTL.Seconds()), HttpOnly: true,
		Secure: a.secure(), SameSite: http.SameSiteLaxMode})
}

// person is who's signed in in this browser, if anyone. A session over a
// day old is rotated: a new token in the cookie, the old one ending in a
// minute.
func (a *App) person(w http.ResponseWriter, r *http.Request) (Person, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || len(c.Value) < 40 || len(c.Value) > 64 {
		return Person{}, false
	}
	p, rotated, err := a.Store.Session(r.Context(), c.Value)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			slog.Error("agentchat: reading a session", "err", err)
		}
		return Person{}, false
	}
	if time.Since(rotated) > rotateAfter && w != nil {
		next := newSecret()
		if ok, err := a.Store.RotateSession(r.Context(), c.Value, next, time.Now().Add(sessionTTL)); err != nil {
			slog.Warn("agentchat: rotating a session", "err", err)
		} else if ok {
			a.setSession(w, next)
			// This request's page gets forms for the new cookie.
			setCookieValue(r, sessionCookie, next)
		}
	}
	return p, true
}

// setCookieValue replaces a cookie's value in r's header (AddCookie adds a
// second one, and Cookie returns the first).
func setCookieValue(r *http.Request, name, value string) {
	var keep []string
	for _, c := range r.Cookies() {
		if c.Name != name {
			keep = append(keep, c.Name+"="+c.Value)
		}
	}
	r.Header.Set("Cookie", strings.Join(append(keep, name+"="+value), "; "))
}

// csrf is the form token for this browser: from its session if it has one,
// else from its pre-sign-in cookie.
func (a *App) csrf(r *http.Request) string {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		return a.macString("csrf", c.Value)
	}
	if c, err := r.Cookie(csrfCookie); err == nil && c.Value != "" {
		return a.macString("precsrf", c.Value)
	}
	return ""
}

// preCSRF makes sure this browser has a pre-sign-in CSRF cookie, and
// returns the form token for it.
func (a *App) preCSRF(w http.ResponseWriter, r *http.Request) string {
	c, err := r.Cookie(csrfCookie)
	if err != nil || len(c.Value) < 40 || len(c.Value) > 64 {
		v := newSecret()
		http.SetCookie(w, &http.Cookie{Name: csrfCookie, Value: v, Path: "/", MaxAge: 3600, HttpOnly: true, Secure: a.secure(), SameSite: http.SameSiteStrictMode})
		setCookieValue(r, csrfCookie, v) // this request's page gets forms for it
		return a.macString("precsrf", v)
	}
	return a.macString("precsrf", c.Value)
}

// sameOrigin: a POST's Origin, when the browser sends one, is this site.
func (a *App) sameOrigin(r *http.Request) bool {
	if s := r.Header.Get("Sec-Fetch-Site"); s != "" && s != "same-origin" && s != "none" {
		return false // the browser says it came from another site
	}
	o := r.Header.Get("Origin")
	return o == "" || o == a.URL
}

// checkPreForm checks a form posted before sign-in: same origin, and the
// pre-sign-in token.
func (a *App) checkPreForm(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if !a.sameOrigin(r) || r.ParseForm() != nil {
		a.message(w, r, http.StatusForbidden, "This form expired. Go back and try again.", "/signin")
		return false
	}
	c, err := r.Cookie(csrfCookie)
	if err != nil || c.Value == "" || !a.checkMAC("precsrf", r.PostForm.Get("csrf"), c.Value) {
		a.message(w, r, http.StatusForbidden, "This form expired. Go back and try again.", "/signin")
		return false
	}
	return true
}

// form wraps a signed-in POST: same origin, and the session's form token.
func (a *App) form(h func(http.ResponseWriter, *http.Request, Person)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.sameOrigin(r) {
			http.Error(w, "cross-origin form", http.StatusForbidden)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		c, err := r.Cookie(sessionCookie)
		if err != nil || !hmac.Equal([]byte(r.PostForm.Get("csrf")), []byte(a.macString("csrf", c.Value))) {
			a.message(w, r, http.StatusForbidden, "This form expired. Reload the page and try again.", "/home")
			return
		}
		p, ok := a.person(nil, r) // no rotation mid-form: the next page rotates
		if !ok {
			http.Redirect(w, r, "/signin", http.StatusSeeOther)
			return
		}
		h(w, r, p)
	}
}

// signedIn wraps a signed-in GET: to sign-in (and back) without a session,
// to choosing a name without one.
func (a *App) signedIn(h func(http.ResponseWriter, *http.Request, Person)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := a.person(w, r)
		if !ok {
			http.Redirect(w, r, "/signin?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		if p.Handle == "" {
			http.Redirect(w, r, "/welcome?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		h(w, r, p)
	}
}

// signedInAPI wraps a signed-in GET that answers JSON: 401 without a
// session.
func (a *App) signedInAPI(h func(http.ResponseWriter, *http.Request, Person)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := a.person(nil, r)
		if !ok || p.Handle == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "sign in first"})
			return
		}
		h(w, r, p)
	}
}

// localPath is a path on this site to go to after signing in, or "/home".
// Browsers drop tabs and newlines from URLs and treat backslashes as
// slashes, so any control character or backslash, or anything with a
// scheme or host, is refused.
func localPath(next string) string {
	if next == "" || len(next) >= 2048 || next[0] != '/' || strings.HasPrefix(next, "//") {
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

// ── Asking for a link ────────────────────────────────────────────────────

type signinView struct {
	CSRF, Next, Email string
	MailReady         bool
	// Agent is the name an agent's OAuth client gave itself, when signing
	// in is the first step of connecting it (its own claim, shown as one).
	Agent string
}

// connecting is the name of the agent a sign-in's next page connects, if
// it's an authorize request from a client Agent Chat signed.
func (a *App) connecting(next string) string {
	u, err := url.Parse(next)
	if err != nil || u.Path != "/oauth/authorize" {
		return ""
	}
	c, ok := a.client(u.Query().Get("client_id"))
	if !ok {
		return ""
	}
	return c.Name
}

func (a *App) signinPage(w http.ResponseWriter, r *http.Request) {
	if p, ok := a.person(w, r); ok && p.Handle != "" {
		http.Redirect(w, r, localPath(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	csrf := a.preCSRF(w, r)
	next := localPath(r.URL.Query().Get("next"))
	a.render(w, r, http.StatusOK, "signin", signinView{CSRF: csrf, Next: next, MailReady: a.mailer().Ready(r.Context()), Agent: a.connecting(next)})
}

// normalEmail is an email address as Agent Chat keeps it (trimmed,
// lowercased), if it's one.
func normalEmail(raw string) (string, bool) {
	e := strings.ToLower(strings.TrimSpace(raw))
	if len(e) < 3 || len(e) > 254 || strings.ContainsFunc(e, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return "", false
	}
	addr, err := mail.ParseAddress(e)
	if err != nil || addr.Address != e || addr.Name != "" {
		return "", false
	}
	local, domain, ok := strings.Cut(e, "@")
	return e, ok && local != "" && strings.Contains(domain, ".") && !strings.ContainsAny(e, `"<>`)
}

func (a *App) signinPost(w http.ResponseWriter, r *http.Request) {
	if !a.checkPreForm(w, r) {
		return
	}
	ctx := r.Context()
	next := localPath(r.PostForm.Get("next"))
	email, ok := normalEmail(r.PostForm.Get("email"))
	if !ok {
		a.render(w, r, http.StatusBadRequest, "signin", signinView{CSRF: a.preCSRF(w, r), Next: next, Email: r.PostForm.Get("email"), MailReady: true})
		return
	}
	if !a.mailer().Ready(ctx) {
		a.message(w, r, http.StatusServiceUnavailable, "Email isn't set up yet, so Agent Chat can't send sign-in links. Try again later.", "/")
		return
	}
	addr := a.clientAddr(r)
	ok, emailOK, err := a.Store.TakeRequest(ctx, "link", a.mac("email", email), addr, linksPerAddr, linksPerEmail)
	if err != nil {
		a.failed(w, r, err)
		return
	}
	if !ok {
		a.message(w, r, http.StatusTooManyRequests, "Too many sign-in links were asked for from here. Try again in an hour.", "/")
		return
	}
	// Past the per-email limit, nothing is sent, and the answer is the same:
	// it says nothing about the address.
	if emailOK {
		token := newSecret()
		if err := a.Store.CreateLink(ctx, token, email, next, time.Now().Add(linkTTL)); err != nil {
			a.failed(w, r, err)
			return
		}
		// Sent in the background, so the answer takes as long whether or not
		// the address has an account (and whether the provider is slow).
		go a.sendLink(context.WithoutCancel(ctx), email, a.URL+"/signin/link#"+token)
	}
	a.render(w, r, http.StatusOK, "sent", map[string]string{"Email": email})
}

var linkText = template.Must(template.New("").Parse(`Sign in to Agent Chat with this link. It works once, for 15 minutes.

{{.Link}}

If you didn't ask to sign in, ignore this email.
`))

var linkHTML = template.Must(template.New("").Parse(`<!doctype html><html><body style="font-family:system-ui,sans-serif;color:#1f1b2e">
<p>Sign in to Agent Chat with this link. It works once, for 15 minutes.</p>
<p><a href="{{.Link}}" style="display:inline-block;background:#1f1b2e;color:#fff;padding:12px 20px;border-radius:999px;text-decoration:none;font-weight:700">Sign in to Agent Chat</a></p>
<p style="color:#5d566e">If you didn't ask to sign in, ignore this email.</p>
</body></html>`))

// sendLink emails a sign-in link. Failures are logged without the link.
func (a *App) sendLink(ctx context.Context, email, link string) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var text, html strings.Builder
	linkText.Execute(&text, map[string]string{"Link": link})
	linkHTML.Execute(&html, map[string]string{"Link": link})
	if err := a.mailer().Send(ctx, Mail{To: email, Subject: "Sign in to Agent Chat", Text: text.String(), HTML: html.String()}); err != nil {
		slog.Warn("agentchat: sending a sign-in link failed", "err", err)
	}
}

// ── Using a link ─────────────────────────────────────────────────────────

// linkPage is the page a link opens: the token is in the URL's fragment
// (never sent to a server, logged or passed on as a Referer); a script
// from this site moves it into the form, and the button posts it.
func (a *App) linkPage(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, http.StatusOK, "link", map[string]string{"CSRF": a.preCSRF(w, r)})
}

func (a *App) linkPost(w http.ResponseWriter, r *http.Request) {
	if !a.checkPreForm(w, r) {
		return
	}
	ctx := r.Context()
	addr := a.clientAddr(r)
	if ok, _, err := a.Store.TakeRequest(ctx, "verify", nil, addr, verifiesPerAddr, 0); err != nil {
		a.failed(w, r, err)
		return
	} else if !ok {
		a.message(w, r, http.StatusTooManyRequests, "Too many sign-in attempts from here. Try again in an hour.", "/signin")
		return
	}
	token := strings.TrimSpace(r.PostForm.Get("token"))
	session := newSecret()
	var p Person
	var next string
	err := ErrNotFound // a token that long was never made: nothing to spend
	if len(token) <= 100 {
		p, next, err = a.Store.UseLink(ctx, token, session, time.Now().Add(sessionTTL))
	}
	if errors.Is(err, ErrNotFound) {
		a.message(w, r, http.StatusBadRequest, "This sign-in link doesn't work: it was used already, or it's expired. Ask for a new one.", "/signin")
		return
	}
	if err != nil {
		a.failed(w, r, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: csrfCookie, Path: "/", MaxAge: -1})
	a.setSession(w, session)
	if p.Handle == "" {
		http.Redirect(w, r, "/welcome?next="+url.QueryEscape(localPath(next)), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, localPath(next), http.StatusSeeOther)
}

// ── Your name ────────────────────────────────────────────────────────────

type welcomeView struct {
	CSRF, Next, Name, Problem string
}

func (a *App) welcomePage(w http.ResponseWriter, r *http.Request) {
	p, ok := a.person(w, r)
	if !ok {
		http.Redirect(w, r, "/signin", http.StatusSeeOther)
		return
	}
	a.render(w, r, http.StatusOK, "welcome", welcomeView{CSRF: a.csrf(r), Next: localPath(r.URL.Query().Get("next")), Name: p.Name})
}

func (a *App) welcomePost(w http.ResponseWriter, r *http.Request, p Person) {
	name := cleanText(r.PostForm.Get("name"), maxName)
	base := slugify(name, maxHandle)
	next := localPath(r.PostForm.Get("next"))
	if name == "" || base == "" {
		a.render(w, r, http.StatusBadRequest, "welcome", welcomeView{CSRF: a.csrf(r), Next: next, Name: name,
			Problem: "Give a name with at least one letter or digit (a to z, 0 to 9)."})
		return
	}
	if _, err := a.Store.SetName(r.Context(), p.ID, name, base); err != nil {
		a.failed(w, r, err)
		return
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (a *App) signout(w http.ResponseWriter, r *http.Request, p Person) {
	var err error
	if r.PostForm.Get("everywhere") != "" {
		err = a.Store.EndSessions(r.Context(), p.ID)
	} else if c, cerr := r.Cookie(sessionCookie); cerr == nil {
		err = a.Store.EndSession(r.Context(), c.Value)
	}
	if err != nil {
		a.failed(w, r, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ── Addresses ────────────────────────────────────────────────────────────

// clientAddr is the request's client address (with ProxyHops trusted
// proxies in front, the entry the outermost added to X-Forwarded-For). An
// IPv6 address counts as its /64.
func (a *App) clientAddr(r *http.Request) string {
	addr := ""
	if f := r.Header.Get("X-Forwarded-For"); f != "" && a.ProxyHops > 0 {
		if parts := strings.Split(f, ","); len(parts) >= a.ProxyHops {
			if ip, err := netip.ParseAddr(strings.TrimSpace(parts[len(parts)-a.ProxyHops])); err == nil {
				addr = ip.String()
			}
		}
	}
	if addr == "" {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		addr = host
	}
	if ip, err := netip.ParseAddr(addr); err == nil && ip.Is6() && !ip.Is4In6() {
		if p, err := ip.Prefix(64); err == nil {
			return p.String()
		}
	}
	return addr
}
