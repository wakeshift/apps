package nightfall

import (
	"bytes"
	_ "embed"
	"html/template"
	"log/slog"
	"net/http"

	"github.com/wakeshift/apps/gamekit"
	"github.com/wakeshift/apps/legal"
)

//go:embed landing.html
var landingHTML string

var landingTmpl = template.Must(template.New("landing").Parse(landingHTML))

// landing is Nightfall's page at /: what the game is, how a game goes, and
// how to add it to an agent.
func (a *App) landing(w http.ResponseWriter, _ *http.Request) {
	var b bytes.Buffer
	if err := landingTmpl.Execute(&b, map[string]any{
		"MCP":       a.srv.PublicURL() + "/mcp",
		"Wakeshift": legal.Wakeshift, // the marketing site, for WAKESHIFT_URL
	}); err != nil {
		slog.Error("nightfall: the landing page", "err", err)
		http.Error(w, "Something went wrong.", http.StatusInternalServerError)
		return
	}
	gamekit.PageHeaders(w, false)
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Write(b.Bytes())
}
