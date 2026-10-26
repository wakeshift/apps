// Package legal serves an app's terms and privacy policy, from Markdown
// embedded in the app, at /terms (and /tos) and /privacy on the app's own
// host. The pages are rendered once and styled with the app's colors.
package legal

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/yuin/goldmark"
)

// Wakeshift is the marketing site, where Wakeshift's own terms and privacy
// policy are: the one for the Wakeshift the app runs on (WAKESHIFT_URL). In
// an app's Markdown, {wakeshift} stands for it.
var Wakeshift = Site(os.Getenv("WAKESHIFT_URL"))

// sites are Wakeshift's environments: the console at app.<site> has its
// marketing site at <site>.
var sites = map[string]bool{"wakeshift.ai": true, "staging.wakeshift.ai": true}

// Site is the marketing site for a Wakeshift console URL: its host without
// "app." (https://app.staging.wakeshift.ai is https://staging.wakeshift.ai),
// for Wakeshift's known environments only. Anything else, such as a local
// console, gets the production site.
func Site(core string) string {
	u, err := url.Parse(strings.TrimSpace(core))
	if err == nil && u.Scheme == "https" && u.Port() == "" {
		if host, ok := strings.CutPrefix(u.Hostname(), "app."); ok && sites[host] {
			return "https://" + host
		}
	}
	return "https://wakeshift.ai"
}

// Style is the app's look: CSS colors and a font stack.
type Style struct {
	Background, Text, Muted, Accent, Line string
	Font                                  string
	Dark                                  bool // color-scheme: dark
}

// Config is one app's documents.
type Config struct {
	App            string // the app's name, e.g. "Nightfall"
	Home           string // the app's home page, e.g. "/" (none if empty)
	Terms, Privacy []byte // Markdown
	Style          Style
}

// Pages are an app's rendered legal pages.
type Pages struct {
	pages map[string]page // by path
}

type page struct {
	body []byte
	etag string
}

// New renders the documents.
func New(c Config) (*Pages, error) {
	if c.App == "" || len(c.Terms) == 0 || len(c.Privacy) == 0 {
		return nil, fmt.Errorf("legal: an app name, terms and a privacy policy are required")
	}
	p := &Pages{pages: map[string]page{}}
	for _, doc := range []struct {
		src   []byte
		title string
		paths []string
	}{
		{c.Terms, "Terms of Service", []string{"/terms", "/tos"}},
		{c.Privacy, "Privacy Policy", []string{"/privacy"}},
	} {
		src := bytes.ReplaceAll(doc.src, []byte("{wakeshift}"), []byte(Wakeshift))
		var body bytes.Buffer
		if err := goldmark.Convert(src, &body); err != nil {
			return nil, fmt.Errorf("legal: rendering %s's %s: %w", c.App, doc.title, err)
		}
		var out bytes.Buffer
		err := tmpl.Execute(&out, map[string]any{
			"App": c.App, "Home": c.Home, "Title": doc.title, "Style": c.Style, "Wakeshift": Wakeshift,
			"Body": template.HTML(body.String()), // the app's own Markdown, from its binary
		})
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(out.Bytes())
		pg := page{body: out.Bytes(), etag: `"` + hex.EncodeToString(sum[:8]) + `"`}
		for _, path := range doc.paths {
			p.pages[path] = pg
		}
	}
	return p, nil
}

// Must is New for documents embedded in the binary, which a test renders.
func Must(p *Pages, err error) *Pages {
	if err != nil {
		panic(err)
	}
	return p
}

// Routes serves the pages: GET /terms, /tos and /privacy.
func (p *Pages) Routes(mux *http.ServeMux) {
	for path := range p.pages {
		mux.HandleFunc("GET "+path, p.serve)
	}
}

func (p *Pages) serve(w http.ResponseWriter, r *http.Request) {
	pg, ok := p.pages[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	// Inline styles only: no scripts, images or anything from elsewhere.
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "public, max-age=300")
	h.Set("ETag", pg.etag)
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(pg.body))
}

var tmpl = template.Must(template.New("legal").Funcs(template.FuncMap{
	// Style values are ours (set in code), but keep them to what a CSS
	// value can hold.
	"css": func(s string) template.CSS {
		if strings.ContainsAny(s, "<>{};\\") {
			return ""
		}
		return template.CSS(s)
	},
}).Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}} · {{.App}}</title>
<meta name="description" content="{{.App}}'s {{.Title}}.">
<style>
:root { color-scheme: {{if .Style.Dark}}dark{{else}}light{{end}}; --bg: {{css .Style.Background}}; --text: {{css .Style.Text}}; --muted: {{css .Style.Muted}}; --accent: {{css .Style.Accent}}; --line: {{css .Style.Line}}; }
* { box-sizing: border-box; }
body { margin: 0; background: var(--bg); color: var(--text); font: 16px/1.65 {{css .Style.Font}}; -webkit-font-smoothing: antialiased; }
.wrap { max-width: 46rem; margin: 0 auto; padding: 0 22px; }
nav { display: flex; justify-content: space-between; align-items: center; padding-top: 26px; padding-bottom: 10px; font-size: 14px; }
nav a { color: var(--muted); text-decoration: none; } nav a:hover { color: var(--text); }
nav .home { color: var(--text); font-weight: 700; font-size: 17px; }
article { padding: 8px 0 40px; }
article h1 { font-size: 34px; line-height: 1.2; margin: 26px 0 6px; }
article h2 { font-size: 21px; margin: 34px 0 8px; }
article h3 { font-size: 17px; margin: 24px 0 6px; }
article p, article li { margin: 0 0 12px; }
article ul { padding-left: 22px; margin: 0 0 14px; }
article a { color: var(--accent); }
article code { font: 14px ui-monospace, "SF Mono", Menlo, Consolas, monospace; }
article em { color: var(--muted); }
footer { border-top: 1px solid var(--line); padding-top: 20px; padding-bottom: 34px; display: flex; flex-wrap: wrap; gap: 22px; font-size: 14px; color: var(--muted); }
footer a { color: inherit; text-decoration: none; } footer a:hover { color: var(--text); }
</style>
</head>
<body>
<nav class="wrap">{{if .Home}}<a class="home" href="{{.Home}}">{{.App}}</a>{{else}}<span class="home">{{.App}}</span>{{end}}<a href="{{.Wakeshift}}">Built on Wakeshift</a></nav>
<main class="wrap">
<article>{{.Body}}</article>
</main>
<footer class="wrap">{{if .Home}}<a href="{{.Home}}">{{.App}}</a>{{end}}<a href="/terms">Terms</a><a href="/privacy">Privacy</a><a href="{{.Wakeshift}}/terms">Wakeshift's terms</a><a href="{{.Wakeshift}}/privacy">Wakeshift's privacy policy</a></footer>
</body>
</html>
`))
