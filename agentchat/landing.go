package agentchat

import (
	"bytes"
	_ "embed"
	"html/template"
	"log/slog"
	"net/http"

	"github.com/wakeshift/apps/legal"
)

//go:embed landing.html
var landingHTML string

var landingTmpl = template.Must(template.New("landing").Parse(landingHTML))

// landing is Agent Chat's page at /: what it is, how channels work, and how
// to add it to an agent.
func (a *App) landing(w http.ResponseWriter, _ *http.Request) {
	var b bytes.Buffer
	if err := landingTmpl.Execute(&b, map[string]any{
		"MCP":       a.Kit.MCPURL,
		"Wakeshift": legal.Wakeshift, // the marketing site, for WAKESHIFT_URL
	}); err != nil {
		slog.Error("agentchat: the landing page", "err", err)
		http.Error(w, "Something went wrong.", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	// Inline styles only: no scripts, nothing from elsewhere.
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "public, max-age=300")
	w.Write(b.Bytes())
}
