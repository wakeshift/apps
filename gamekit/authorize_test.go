package gamekit

import (
	"crypto/ed25519"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/wakeshift/wakeshift-go/appkit"
)

const testChallenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"

type authorizeApp struct {
	*signInApp
	clientID string
	redirect string
}

// newAuthorizeApp serves a game's sign-in (with terms, if withLegal) and
// registers a client.
func newAuthorizeApp(t *testing.T, withLegal bool) *authorizeApp {
	t.Helper()
	a := &authorizeApp{signInApp: newSignInApp(t, withLegal), redirect: "http://127.0.0.1:9/cb"}
	status, reg, _ := a.register("Claude Code", a.redirect)
	if status != http.StatusCreated {
		t.Fatalf("registration: %d", status)
	}
	a.clientID = reg.ClientID
	return a
}

var noRedirects = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func (a *authorizeApp) get(redirect, cookie string) (*http.Response, string) {
	a.t.Helper()
	req, _ := http.NewRequest("GET", a.srv.URL+"/oauth/authorize?"+url.Values{"response_type": {"code"}, "client_id": {a.clientID}, "redirect_uri": {redirect},
		"state": {"st"}, "code_challenge": {testChallenge}, "code_challenge_method": {"S256"}}.Encode(), nil)
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: authorizeCookie, Value: cookie})
	}
	resp, err := noRedirects.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func (a *authorizeApp) post(csrf, cookie string) *http.Response {
	a.t.Helper()
	req, _ := http.NewRequest("POST", a.srv.URL+"/oauth/authorize", strings.NewReader(url.Values{"csrf": {csrf}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: authorizeCookie, Value: cookie})
	}
	resp, err := noRedirects.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

var csrfField = regexp.MustCompile(`<input type="hidden" name="csrf" value="([^"]+)">`)

func TestAuthorizePage(t *testing.T) {
	a := newAuthorizeApp(t, true)

	// The page: the game, the line with its own terms and privacy policy,
	// and a CSRF token; no code yet.
	resp, body := a.get(a.redirect, "")
	if resp.StatusCode != 200 || resp.Header.Get("Location") != "" || strings.Contains(body, "code=") {
		t.Fatalf("authorize: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	for _, want := range []string{"<h1>Agent&#39;s Quiz</h1>", `<img src="/static/logo.svg?v=`,
		`By authorizing, you agree to Agent&#39;s Quiz's <a href="` + a.srv.URL + `/terms" target="_blank" rel="noopener noreferrer">Terms</a> and <a href="` + a.srv.URL + `/privacy" target="_blank" rel="noopener noreferrer">Privacy Policy</a>.`,
		`<form method="post" action="` + a.srv.URL + `/oauth/authorize">`, `<button type="submit">Authorize</button>`} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "<script") {
		t.Error("the page has a script")
	}
	for h, want := range map[string]string{"Content-Security-Policy": authorizePageCSP, "X-Frame-Options": "DENY", "Cache-Control": "no-store", "Content-Type": "text/html; charset=utf-8"} {
		if got := resp.Header.Get(h); got != want {
			t.Errorf("%s: %q, want %q", h, got, want)
		}
	}
	m := csrfField.FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no CSRF token")
	}
	csrf, cookie := m[1], ""
	for _, c := range resp.Cookies() {
		if c.Name == authorizeCookie {
			cookie = c.Value
			if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.MaxAge != 900 {
				t.Errorf("cookie %+v", c)
			}
		}
	}
	if cookie == "" {
		t.Fatal("no CSRF cookie")
	}

	// No page for a bad request: an unregistered redirect.
	if resp, _ := a.get("http://evil.example/cb", ""); resp.StatusCode != 400 || resp.Header.Get("Location") != "" {
		t.Errorf("an unregistered redirect: %d", resp.StatusCode)
	}
	if a.sessions() != 0 {
		t.Error("the page started a session")
	}

	// Refused: no token, a tampered one, no cookie, another browser's
	// cookie, an expired form.
	for name, r := range map[string]*http.Response{
		"no token":       a.post("", cookie),
		"tampered token": a.post(csrf[:len(csrf)-2]+"xx", cookie),
		"no cookie":      a.post(csrf, ""),
		"other cookie":   a.post(csrf, "AAAAAAAAAAAAAAAAAAAAAA"),
		"expired":        a.post(a.s.signForm(authorizeForm{Query: "x", Nonce: cookie, Expires: time.Now().Add(-time.Second).Unix()}), cookie),
	} {
		if r.StatusCode != http.StatusForbidden || r.Header.Get("Location") != "" {
			t.Errorf("%s: %d %q", name, r.StatusCode, r.Header.Get("Location"))
		}
	}

	// Another tab keeps the cookie, so both pages work.
	resp2, _ := a.get(a.redirect, cookie)
	for _, c := range resp2.Cookies() {
		if c.Name == authorizeCookie && c.Value != cookie {
			t.Error("a second page replaced the cookie")
		}
	}

	if a.sessions() != 0 {
		t.Error("a refused click started a session")
	}

	// Authorize: the session starts, and the browser goes back with the
	// code and the state.
	r := a.post(csrf, cookie)
	loc, _ := url.Parse(r.Header.Get("Location"))
	if r.StatusCode != http.StatusFound || !strings.HasPrefix(loc.String(), a.redirect+"?") || loc.Query().Get("code") == "" || loc.Query().Get("state") != "st" {
		t.Fatalf("authorize: %d %s", r.StatusCode, loc)
	}
	if a.sessions() != 1 {
		t.Errorf("%d sessions after Authorize", a.sessions())
	}
}

// A form signed with another app's key is refused like a forged one.
func TestAuthorizeFormKey(t *testing.T) {
	a := newAuthorizeApp(t, true)
	_, key, _ := ed25519.GenerateKey(nil)
	other := &Server{Kit: &appkit.App{Key: key}}
	tok := other.signForm(authorizeForm{Query: "x", Nonce: "AAAAAAAAAAAAAAAAAAAAAA", Expires: time.Now().Add(time.Minute).Unix()})
	if f, ok := other.openForm(tok); !ok || f.Query != "x" {
		t.Fatal("a signed form didn't open")
	}
	if r := a.post(tok, "AAAAAAAAAAAAAAAAAAAAAA"); r.StatusCode != http.StatusForbidden {
		t.Fatalf("another key's form: %d", r.StatusCode)
	}
}

// Without terms, authorize starts the session and redirects back with the
// code at once, and there's nothing to click.
func TestAuthorizeWithoutLegal(t *testing.T) {
	a := newAuthorizeApp(t, false)
	resp, _ := a.get(a.redirect, "")
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusFound || loc.Query().Get("code") == "" || a.sessions() != 1 {
		t.Fatalf("authorize: %d %s", resp.StatusCode, loc)
	}
	if r := a.post("x", "y"); r.StatusCode != http.StatusForbidden {
		t.Errorf("a click without terms: %d", r.StatusCode)
	}
}

// A signature in a non-canonical encoding (the last character's unused
// bits set) is refused, so a token has one encoding: changing a character
// of it always breaks it.
func TestSignaturesAreCanonical(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(nil)
	s := &Server{Kit: &appkit.App{Key: key}}
	tok := s.signForm(authorizeForm{Query: "x", Nonce: "AAAAAAAAAAAAAAAAAAAAAA", Expires: time.Now().Add(time.Minute).Unix()})
	if _, ok := s.openForm(tok); !ok {
		t.Fatal("a signed form didn't open")
	}
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	last := strings.IndexByte(alphabet, tok[len(tok)-1])
	other := tok[:len(tok)-1] + string(alphabet[last|1]) // the same bytes, decoded leniently
	if _, ok := s.openForm(other); ok {
		t.Fatal("a non-canonical signature opened")
	}
}
