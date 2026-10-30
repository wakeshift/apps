package slackline

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"html/template"
	"net/http"
	"path"
	"strings"

	"github.com/wakeshift/apps/linkpreview"
)

// Slackline's static files: its icons and its link preview's image.
//
//go:embed static/*
var staticFiles embed.FS

// The static files' content types, and their versions (a hash of each, for
// the URLs pages use, which are cached for good).
var (
	staticTypes    = map[string]string{".svg": "image/svg+xml", ".png": "image/png", ".ico": "image/x-icon"}
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

func serveStatic(w http.ResponseWriter, r *http.Request, name string) {
	ct, ok := staticTypes[path.Ext(name)]
	b, err := staticFiles.ReadFile("static/" + name)
	if !ok || err != nil || strings.Contains(name, "/") {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if v := r.URL.Query().Get("v"); v != "" && v == staticVersions[name] {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=86400")
	}
	w.Write(b)
}

func staticRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /static/{name}", func(w http.ResponseWriter, r *http.Request) { serveStatic(w, r, r.PathValue("name")) })
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) { serveStatic(w, r, "favicon.ico") })
}

// preview is the landing page's link preview. Its title doesn't start with
// "Slackline": that's og:site_name, which apps such as iMessage cut from
// the title.
func (a *App) preview() template.HTML {
	return linkpreview.Card{
		SiteName: "Slackline", Title: "Talk to your agents in Slack",
		Description: "Each agent you connect gets its own thread in your DM with Slackline. Reply there and it wakes up, does the work on your machine, and answers in the thread.",
		URL:         linkpreview.Join(a.URL, "/"), Image: linkpreview.Join(a.URL, asset("preview.png")),
		ImageAlt: "Slackline: talk to your agents in Slack. In a direct message, Ada asks why the checkout tests fail, and her laptop's agent answers in the thread.",
	}.Tags()
}
