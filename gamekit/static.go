package gamekit

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"strings"
)

// Cache lifetimes for the game's static files: a URL from StaticURL names
// the file's content (?v=), so it's cached for good; one without, a day.
const (
	staticCache    = "public, max-age=86400"
	immutableCache = "public, max-age=31536000, immutable"
)

// StaticURL is the URL path of one of the game's static files, with its
// content's hash, so browsers keep it until it changes.
func (s *Server) StaticURL(name string) string {
	s.hashStatic()
	if h, ok := s.staticHash[name]; ok {
		return "/static/" + name + "?v=" + h
	}
	return "/static/" + name
}

func (s *Server) hashStatic() {
	s.staticOnce.Do(func() {
		s.staticHash = map[string]string{}
		if s.Static == nil {
			return
		}
		fs.WalkDir(s.Static, ".", func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			b, err := fs.ReadFile(s.Static, path)
			if err == nil {
				sum := sha256.Sum256(b)
				s.staticHash[path] = hex.EncodeToString(sum[:6])
			}
			return nil
		})
	})
}

func (s *Server) staticRoutes(mux *http.ServeMux) {
	if s.Static == nil {
		return
	}
	s.hashStatic()
	files := http.StripPrefix("/static/", http.FileServerFS(s.Static))
	mux.HandleFunc("GET /static/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/static/")
		h, ok := s.staticHash[name]
		if !ok { // no directory listings, nothing that isn't a file
			http.NotFound(w, r)
			return
		}
		cache := staticCache
		if r.URL.Query().Get("v") == h {
			cache = immutableCache
		}
		w.Header().Set("Cache-Control", cache)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		files.ServeHTTP(w, r)
	})
}

// PageHeaders sets a page's security headers: everything from this
// origin only (no external requests), inline styles, and inline scripts
// only if the page has one.
func PageHeaders(w http.ResponseWriter, script bool) {
	csp := "default-src 'none'; style-src 'unsafe-inline'; img-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
	if script {
		csp += "; script-src 'unsafe-inline'; connect-src 'self'"
	}
	pageHeaders(w, csp)
}

// LandingHeaders sets a landing page's security headers: as PageHeaders,
// plus the agent picker's script and stylesheet from this origin (no inline
// script).
func LandingHeaders(w http.ResponseWriter) {
	pageHeaders(w, "default-src 'none'; style-src 'self' 'unsafe-inline'; script-src 'self'; img-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
}

// StrictHeaders sets a page's security headers with the public pages'
// strict policy: script, styles and images from this origin only, nothing
// inline.
func StrictHeaders(w http.ResponseWriter) { pageHeaders(w, spectateCSP) }

func pageHeaders(w http.ResponseWriter, csp string) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", csp)
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
}
