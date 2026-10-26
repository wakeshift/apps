package gamekit

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// The authorize page: what a game shows when a player's agent signs in as a
// guest. Guest sign-in (appkit.GuestAuth) redirects back with a code at
// once; a game with terms (Server.Legal) puts this page in front of it: the
// game's name and logo, "By authorizing, you agree to <Game>'s Terms and
// Privacy Policy" linking its own /terms and /privacy, and an Authorize
// button. That button is where players agree to the game's terms.
//
// The page is the game's, not the SDK's. GET /oauth/authorize runs the
// SDK's authorize to check the request (client, redirect URI, PKCE) and, if
// it would issue a code, shows the page instead, so the code never leaves;
// anything else (an unknown client, a bad redirect) is the SDK's answer as
// it is. The Authorize button posts back with a CSRF token (signed with a
// key derived from the app's key, good for authorizePageTTL, and bound to a
// SameSite=Strict, HttpOnly cookie); with a valid one, the original
// authorize request goes to the SDK unchanged, which redirects back with
// the code. Nothing is stored, and nothing is created until the code
// exchange, as before. Agent Hold'em has the same page in its own server
// (apps/holdem/src/authorize.ts).

// authorizePageStyle is the page's only styling, allowed by its hash in
// the page's CSP: no other style, script or third-party asset loads.
const authorizePageStyle = `*{box-sizing:border-box}body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;padding:24px;background:#f5f1e8;color:#1d1b17;font:16px/1.5 system-ui,-apple-system,"Segoe UI",Roboto,sans-serif}main{width:100%;max-width:400px;padding:32px;background:#fff;border:1px solid #ddd5c4;border-radius:16px;text-align:center}img{width:64px;height:64px;border-radius:12px;object-fit:contain}h1{margin:12px 0 8px;font-size:22px;line-height:1.3}p{margin:0 0 20px;color:#57524a}.terms{font-size:14px}a{color:#c25418}button{width:100%;padding:12px 16px;border:0;border-radius:10px;background:#1d1b17;color:#fff;font:inherit;font-weight:600;cursor:pointer}button:hover{background:#3a362f}button:focus-visible{outline:3px solid #c25418;outline-offset:2px}`

// authorizePageCSP has no form-action: browsers apply it to the redirect
// after the form, which goes to the client's own redirect URI (a port on
// localhost, or an app's scheme).
var authorizePageCSP = func() string {
	sum := sha256.Sum256([]byte(authorizePageStyle))
	return "default-src 'none'; style-src 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'; img-src 'self'; base-uri 'none'; frame-ancestors 'none'"
}()

var authorizePageHTML = template.Must(template.New("authorize").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Authorize {{.Name}}</title>
<style>` + authorizePageStyle + `</style>
</head>
<body>
<main>
{{if .Logo}}<img src="{{.Logo}}" alt="">
{{end}}<h1>{{.Name}}</h1>
<p>Your agent is signing in to {{.Name}}.</p>
<form method="post" action="{{.Action}}">
<input type="hidden" name="csrf" value="{{.CSRF}}">
<p class="terms">By authorizing, you agree to {{.Name}}'s <a href="{{.Terms}}" target="_blank" rel="noopener noreferrer">Terms</a> and <a href="{{.Privacy}}" target="_blank" rel="noopener noreferrer">Privacy Policy</a>.</p>
<button type="submit">Authorize</button>
</form>
</main>
</body>
</html>
`))

const (
	// authorizeCookie holds the nonce an authorize page's form is bound to.
	authorizeCookie = "wakeshift_guest_csrf"
	// authorizePageTTL is how long an authorize page's form can be sent.
	authorizePageTTL = 15 * time.Minute
)

var nonceRE = regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`)

// guestRoutes mounts guest sign-in: the SDK's, with the authorize page in
// front of its authorize when the game has terms.
func (s *Server) guestRoutes(mux *http.ServeMux) {
	if s.Legal == nil {
		s.guest().Routes(mux)
		return
	}
	sdk := http.NewServeMux()
	s.guest().Routes(sdk)
	mux.Handle("/.well-known/", sdk)
	mux.Handle("/oauth/", sdk)
	// More specific than /oauth/ (a strict subset of its requests), so these
	// take precedence over it rather than conflict (TestAuthorizePage
	// mounts them).
	mux.HandleFunc("GET /oauth/authorize", func(w http.ResponseWriter, r *http.Request) { s.authorizePage(w, r, sdk) })
	mux.HandleFunc("POST /oauth/authorize", func(w http.ResponseWriter, r *http.Request) { s.authorizeClick(w, r, sdk) })
}

// authorizeForm is what the page's CSRF token carries: the authorize
// request to send the SDK, the nonce in the browser's cookie it's bound
// to, and until when (Unix seconds).
type authorizeForm struct {
	Query   string `json:"q"`
	Nonce   string `json:"n"`
	Expires int64  `json:"e"`
}

func (s *Server) authorizeKey() []byte {
	h := hmac.New(sha256.New, s.Kit.Key.Seed())
	h.Write([]byte("gamekit:authorize-page"))
	return h.Sum(nil)
}

func (s *Server) signForm(f authorizeForm) string {
	b, _ := json.Marshal(f)
	p := base64.RawURLEncoding.EncodeToString(b)
	h := hmac.New(sha256.New, s.authorizeKey())
	h.Write([]byte(p))
	return p + "." + base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

func (s *Server) openForm(token string) (authorizeForm, bool) {
	var f authorizeForm
	p, sig, ok := strings.Cut(token, ".")
	if !ok {
		return f, false
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	h := hmac.New(sha256.New, s.authorizeKey())
	h.Write([]byte(p))
	if err != nil || !hmac.Equal(got, h.Sum(nil)) {
		return f, false
	}
	b, err := base64.RawURLEncoding.DecodeString(p)
	return f, err == nil && json.Unmarshal(b, &f) == nil
}

// recorder keeps the SDK's answer to an authorize request.
type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header { return r.header }

// Write and WriteHeader as net/http's: a write before any WriteHeader is a
// 200, and only the first status counts.
func (r *recorder) Write(b []byte) (int, error) {
	r.WriteHeader(http.StatusOK)
	return r.body.Write(b)
}

func (r *recorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
}

// authorizePage answers GET /oauth/authorize: the page, if the SDK would
// issue a code for the request; else the SDK's answer.
func (s *Server) authorizePage(w http.ResponseWriter, r *http.Request, sdk http.Handler) {
	rec := &recorder{header: http.Header{}}
	sdk.ServeHTTP(rec, r)
	if loc, err := url.Parse(rec.header.Get("Location")); rec.status != http.StatusFound || err != nil || loc.Query().Get("code") == "" {
		for k, v := range rec.header {
			w.Header()[k] = v
		}
		rec.WriteHeader(http.StatusOK) // the SDK wrote nothing: an empty 200, as net/http would send
		w.WriteHeader(rec.status)
		w.Write(rec.body.Bytes())
		return
	}
	n := ""
	if c, err := r.Cookie(authorizeCookie); err == nil && nonceRE.MatchString(c.Value) {
		n = c.Value // another tab's page stays good
	} else {
		b := make([]byte, 16)
		rand.Read(b)
		n = base64.RawURLEncoding.EncodeToString(b)
	}
	base := s.PublicURL()
	http.SetCookie(w, &http.Cookie{Name: authorizeCookie, Value: n, Path: "/", MaxAge: int(authorizePageTTL.Seconds()),
		HttpOnly: true, Secure: strings.HasPrefix(base, "https://"), SameSite: http.SameSiteStrictMode})
	logo := ""
	if s.Logo != "" {
		logo = s.StaticURL(s.Logo)
	}
	var b bytes.Buffer
	if err := authorizePageHTML.Execute(&b, map[string]string{"Name": s.Title, "Logo": logo, "Action": base + "/oauth/authorize",
		"Terms": base + "/terms", "Privacy": base + "/privacy",
		"CSRF": s.signForm(authorizeForm{Query: r.URL.RawQuery, Nonce: n, Expires: time.Now().Add(authorizePageTTL).Unix()})}); err != nil {
		slog.Error("gamekit: the authorize page", "err", err)
		http.Error(w, "Something went wrong. Try again.", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy", authorizePageCSP)
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	w.Write(b.Bytes())
}

// authorizeClick answers the Authorize button: with a valid CSRF token, the
// original authorize request goes to the SDK, which redirects back to the
// client with the code.
func (s *Server) authorizeClick(w http.ResponseWriter, r *http.Request, sdk http.Handler) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form.", http.StatusBadRequest)
		return
	}
	f, ok := s.openForm(r.PostForm.Get("csrf"))
	c, err := r.Cookie(authorizeCookie)
	if !ok || time.Now().Unix() > f.Expires || f.Nonce == "" || err != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(f.Nonce)) != 1 {
		http.Error(w, "This page has expired. Go back to your agent and sign in again.", http.StatusForbidden)
		return
	}
	get := r.Clone(r.Context())
	get.Method = http.MethodGet
	get.URL.RawQuery = f.Query
	get.Body, get.ContentLength, get.Form, get.PostForm = http.NoBody, 0, nil, nil
	get.Header.Del("Content-Type")
	w.Header().Set("Cache-Control", "no-store")
	sdk.ServeHTTP(w, get)
}
