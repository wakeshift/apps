package agentchat

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/wakeshift/apps/agentpicker"
	"github.com/wakeshift/apps/legal"
	"github.com/wakeshift/apps/linkpreview"
)

// Agent Chat's site: the landing page, sign-in, and for signed-in people
// their orgs, channels outside orgs, connected agents and account.

//go:embed templates/*.html
var templateFiles embed.FS

//go:embed static/*
var staticFiles embed.FS

var pages = template.Must(template.New("").Funcs(template.FuncMap{
	"date":      func(t time.Time) string { return t.UTC().Format("Jan 2, 2006") },
	"until":     func(t time.Time) string { return t.UTC().Format("Jan 2 15:04 UTC") },
	"asset":     asset,
	"inc":       func(i int) int { return i + 1 },
	"firstName": firstName,
	"initial":   initial,
	"color":     colorOf,
	"plural": func(n int, one, many string) string {
		if n == 1 {
			return "1 " + one
		}
		return strconv.Itoa(n) + " " + many
	},
	"tenth": func(n, of int) int { // how much of of n is, in tenths, for a bar's width
		if of <= 0 {
			return 0
		}
		return min(10, (n*10+of-1)/of)
	},
	"inDays": func(t time.Time) string {
		d := int(time.Until(t).Hours() / 24)
		switch {
		case time.Until(t) < 24*time.Hour:
			return "today"
		case d == 1:
			return "in 1 day"
		}
		return "in " + strconv.Itoa(d) + " days"
	},
	"short":   func(t time.Time) string { return t.UTC().Format("Jan 2") },
	"clock":   func(t time.Time) string { return t.UTC().Format("15:04") },
	"rfc3339": func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
	"stamp":   func(t time.Time) string { return t.UTC().Format("Jan 2, 15:04 UTC") },
	"host":    func(u string) string { return strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://") },
	"title": func(s string) string {
		if s == "" {
			return s
		}
		return strings.ToUpper(s[:1]) + s[1:]
	},
}).ParseFS(templateFiles, "templates/*.html"))

// The static files' content types, and their versions (a hash of each,
// for the URLs pages use, so a new version is fetched at once).
var (
	staticTypes = map[string]string{".css": "text/css; charset=utf-8", ".js": "text/javascript; charset=utf-8", ".svg": "image/svg+xml",
		".png": "image/png", ".ico": "image/x-icon"}
	staticVersions = map[string]string{}
)

func init() {
	entries, _ := staticFiles.ReadDir("static")
	for _, e := range entries {
		b, _ := staticFiles.ReadFile("static/" + e.Name())
		sum := sha256.Sum256(b)
		staticVersions[e.Name()] = hex.EncodeToString(sum[:6])
	}
}

// asset is a static file's URL, with its version.
func asset(name string) string { return "/static/" + name + "?v=" + staticVersions[name] }

func serveStatic(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ct, ok := staticTypes[path.Ext(name)]
	b, err := staticFiles.ReadFile("static/" + name)
	if !ok || err != nil || strings.Contains(name, "/") {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// A URL from asset names the file's content, so it's cached for good.
	if v := r.URL.Query().Get("v"); v != "" && v == staticVersions[name] {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=3600")
	}
	w.Write(b)
}

func (a *App) pageRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", a.landing)
	mux.Handle("GET /agent-picker/", agentpicker.Handler())
	mux.HandleFunc("GET /static/{name}", serveStatic)
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		r.SetPathValue("name", "favicon.ico")
		serveStatic(w, r)
	})

	mux.HandleFunc("GET /signin", a.signinPage)
	mux.HandleFunc("POST /signin", a.signinPost)
	mux.HandleFunc("GET /signin/link", a.linkPage)
	mux.HandleFunc("POST /signin/link", a.linkPost)
	mux.HandleFunc("GET /welcome", a.welcomePage)
	mux.HandleFunc("POST /welcome", a.form(a.welcomePost))
	mux.HandleFunc("POST /signout", a.form(a.signout))

	mux.HandleFunc("GET /home", a.signedIn(a.home))
	mux.HandleFunc("GET /api/state", a.signedInAPI(a.state))
	mux.HandleFunc("GET /orgs/new", a.signedIn(a.newOrgPage))
	mux.HandleFunc("POST /orgs", a.form(a.createOrg))
	mux.HandleFunc("GET /orgs/{slug}", a.signedIn(a.orgChat))
	mux.HandleFunc("GET /orgs/{slug}/c/{name}", a.signedIn(a.orgChat))
	mux.HandleFunc("GET /orgs/{slug}/settings", a.signedIn(a.orgSettings))
	mux.HandleFunc("GET /orgs/{slug}/settings/{tab}", a.signedIn(a.orgSettings))
	mux.HandleFunc("POST /orgs/{slug}/{action}", a.form(a.orgForm))
	mux.HandleFunc("POST /orgs/{slug}/{what}/{id}/{action}", a.form(a.orgForm))
	mux.HandleFunc("GET /channels/new", a.signedIn(a.newChannelPage))
	mux.HandleFunc("POST /channels", a.form(a.createLoneChannel))
	mux.HandleFunc("GET /channels/{id}", a.signedIn(a.loneChat))
	mux.HandleFunc("GET /channels/{id}/live", a.live)
	mux.HandleFunc("GET /dm/{agent}", a.signedIn(a.dmChat))
	mux.HandleFunc("POST /channels/{id}/messages", a.postMessage)
	mux.HandleFunc("POST /channels/{id}/ask/{agent}", a.form(a.askToJoin))
	mux.HandleFunc("GET /channels/{id}/settings", a.signedIn(a.loneSettings))
	mux.HandleFunc("GET /channels/{id}/settings/{tab}", a.signedIn(a.loneSettings))
	mux.HandleFunc("POST /channels/{id}/{action}", a.form(a.channelForm))
	mux.HandleFunc("POST /channels/{id}/{what}/{target}/{action}", a.form(a.channelForm))
	mux.HandleFunc("GET /join/{code}", a.joinPage)
	mux.HandleFunc("POST /join/{code}", a.form(a.joinPost))
	mux.HandleFunc("POST /agents/{id}/disconnect", a.form(a.disconnect))
	mux.HandleFunc("GET /settings", a.signedIn(a.youSettings))
	mux.HandleFunc("GET /settings/{tab}", a.signedIn(a.youSettings))
	mux.HandleFunc("GET /account/delete", a.signedIn(a.deletePage))
	mux.HandleFunc("POST /account/delete", a.form(a.deleteAccount))
}

// ── Rendering ────────────────────────────────────────────────────────────

type pageData struct {
	CSRF, MCPURL, Wakeshift string
	Person                  *Person
	Data                    any
	Picker, PickerHead      template.HTML
}

func (a *App) picker() template.HTML {
	return agentpicker.HTML(agentpicker.Options{
		Name: "agent-chat", URL: a.MCPURL(), Ask: "List my channels in Agent Chat.",
		Each: "each message for your agent", Interactive: true,
		SignIn: "The first time, sign in to Agent Chat with your email in the browser, then connect the agent.",
	})
}

func (a *App) render(w http.ResponseWriter, r *http.Request, status int, name string, data any) {
	var p *Person
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" && a.Store != nil {
		if got, _, err := a.Store.Session(r.Context(), c.Value); err == nil {
			p = &got
		}
	}
	var b bytes.Buffer
	if err := pages.ExecuteTemplate(&b, name, pageData{CSRF: a.csrf(r), MCPURL: a.MCPURL(), Wakeshift: legal.Wakeshift, Person: p, Data: data,
		Picker: a.picker(), PickerHead: agentpicker.Head()}); err != nil {
		slog.Error("agentchat: rendering a page failed", "page", name, "err", err)
		http.Error(w, "Something went wrong.", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	// Styles, scripts and images from this site only (no inline ones);
	// fetches (a waiting page's) here only. No form-action: Authorize
	// redirects to the agent's own address, which it would block.
	h.Set("Content-Security-Policy", csp)
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(b.Bytes())
}

// csp is every page's content security policy.
const csp = "default-src 'none'; style-src 'self'; script-src 'self'; img-src 'self'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'"

type messageView struct{ Text, Next string }

func (a *App) message(w http.ResponseWriter, r *http.Request, status int, text, next string) {
	a.render(w, r, status, "message", messageView{Text: text, Next: next})
}

func (a *App) failed(w http.ResponseWriter, r *http.Request, err error) {
	// The route's pattern, not its path: a path can carry a code (/join/{code}).
	slog.Error("agentchat: request failed", "route", r.Pattern, "err", err)
	a.message(w, r, http.StatusInternalServerError, "Something went wrong. Try again.", "/home")
}

func (a *App) notYours(w http.ResponseWriter, r *http.Request) {
	a.message(w, r, http.StatusNotFound, "That isn't there, or isn't yours to change.", "/home")
}

// landingView is the landing page's own data: its link preview.
type landingView struct{ Preview template.HTML }

func (a *App) landing(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, http.StatusOK, "landing", landingView{Preview: linkpreview.Card{
		// The title doesn't start with "Agent Chat": that's og:site_name,
		// which apps such as iMessage cut from the title.
		SiteName: "Agent Chat", Title: "Group chat for AI agents",
		Description: "Put Claude Code, Codex and Cursor in one channel and let them work together. They post, reply in threads and @-mention each other, and a mention wakes the agent it names.",
		URL:         linkpreview.Join(a.URL, "/"), Image: linkpreview.Join(a.URL, asset("preview.png")),
		ImageAlt: "Agent Chat: group chat for AI agents. In #release, Codex asks @claude-code to take a failing test, and Claude Code is on it.",
	}.Tags()})
}

// ── Making orgs and channels ─────────────────────────────────────────────

func (a *App) createOrg(w http.ResponseWriter, r *http.Request, p Person) {
	name := cleanText(r.PostForm.Get("name"), maxName)
	base := slugify(name, maxSlug)
	if name == "" || base == "" {
		a.message(w, r, http.StatusBadRequest, "Give the org a name with at least one letter or digit.", "/home")
		return
	}
	o, err := a.Store.CreateOrg(r.Context(), p.ID, name, base)
	if errors.Is(err, errTooMany) {
		a.message(w, r, http.StatusBadRequest, "You own as many orgs as you can. Delete one first.", "/home")
		return
	}
	if err != nil {
		a.failed(w, r, err)
		return
	}
	// Every org starts with #general (if it can't be made now, the owner
	// makes channels in the settings).
	if _, err := a.createChannel(r.Context(), p.ID, o.Slug, "general", cleanText("Everything "+name+": announcements, questions, hellos", maxTopic)); err != nil {
		slog.Warn("agentchat: making a new org's general channel", "err", err)
	}
	http.Redirect(w, r, "/orgs/"+o.Slug+"?done=created", http.StatusSeeOther)
}

func (a *App) createLoneChannel(w http.ResponseWriter, r *http.Request, p Person) {
	name, err := channelName(r.PostForm.Get("name"), 34)
	if err != nil {
		a.message(w, r, http.StatusBadRequest, "Channel names are lowercase letters, digits and dashes, up to 34 characters.", "/home")
		return
	}
	c, err := a.createChannel(r.Context(), p.ID, "", name, cleanText(r.PostForm.Get("topic"), maxTopic))
	if err != nil {
		a.message(w, r, http.StatusBadRequest, sentence(err), "/home")
		return
	}
	http.Redirect(w, r, "/channels/"+c.ID+"?done=created", http.StatusSeeOther)
}

// sentence is an error as a sentence on a page.
func sentence(err error) string {
	s := err.Error()
	if s == "" {
		return "That didn't work."
	}
	s = strings.ToUpper(s[:1]) + s[1:]
	if !strings.HasSuffix(s, ".") {
		s += "."
	}
	return s
}
