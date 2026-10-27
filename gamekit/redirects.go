package gamekit

import (
	"net/url"
	"regexp"
	"strings"
)

// Which redirect URIs players' sign-in (oauth.go) takes: only ones a
// player's own agent can have, so a player's code never goes to someone
// else's site. testdata/redirects.json holds the cases, which Agent
// Hold'em's copy (apps/holdem/src/redirects.ts) runs too.

// BadRedirect says which redirect URIs registration accepts.
const BadRedirect = "redirect URIs are http on loopback (localhost, 127.0.0.1 or [::1], any port), an app's own scheme (cursor://…), " +
	"or a known web agent's callback (Claude, ChatGPT), without a fragment"

// webCallbacks are the https redirect URIs of the web agents the agent
// picker offers (internal/harnesses), exactly: Claude (claude.ai, the
// desktop and mobile apps and Cowork; claude.com is its announced next
// callback) and ChatGPT (its connectors, and the developer platform's
// review). ChatGPT's per-connector callbacks are chatgptCallbackRE.
var webCallbacks = []string{
	"https://claude.ai/api/mcp/auth_callback",
	"https://claude.com/api/mcp/auth_callback",
	"https://chatgpt.com/connector_platform_oauth_redirect",
	"https://platform.openai.com/apps-manage/oauth",
}

// chatgptCallbackRE is ChatGPT's callback for one connector, used when an
// authorization server doesn't support RFC 9207 (the games do, so ChatGPT
// uses the stable one; this is in case it still sends one).
var chatgptCallbackRE = regexp.MustCompile(`^https://chatgpt\.com/connector/oauth/[A-Za-z0-9_-]{1,128}$`)

var schemeRE = regexp.MustCompile(`^[a-z][a-z0-9+.-]*$`)

// browserSchemes are schemes a browser handles itself (or that run
// script), never an app's own.
var browserSchemes = map[string]bool{"https": true, "http": true, "javascript": true, "data": true, "file": true, "blob": true, "about": true,
	"vbscript": true, "filesystem": true, "view-source": true, "ws": true, "wss": true, "ftp": true, "mailto": true, "tel": true, "sms": true, "chrome": true, "intent": true}

// allowedRedirect is whether raw may be a client's redirect URI: http on a
// loopback host, any port (native apps, RFC 8252 7.3); an app's own scheme
// (cursor://…, RFC 8252 7.1), never one a browser handles; or a known web
// agent's https callback, exactly. Never with a fragment or user info.
// Anything else could send a player's code to someone else's site.
func allowedRedirect(raw string) bool {
	if len(raw) > 500 || strings.ContainsAny(raw, "#\\ \t\r\n") {
		return false
	}
	if contains(webCallbacks, raw) || chatgptCallbackRE.MatchString(raw) {
		return true
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Fragment != "" || !schemeRE.MatchString(u.Scheme) {
		return false
	}
	if u.Scheme == "http" {
		if u.Opaque != "" {
			return false
		}
		switch u.Hostname() {
		case "localhost", "127.0.0.1", "::1":
			return u.Port() == "" || isPort(u.Port())
		}
		return false
	}
	return !browserSchemes[u.Scheme] && u.Opaque+u.Host+u.Path != ""
}

func isPort(p string) bool {
	if len(p) > 5 {
		return false
	}
	n := 0
	for _, c := range p {
		if c < '0' || c > '9' {
			return false
		}
		n = n*10 + int(c-'0')
	}
	return n > 0 && n < 65536
}
