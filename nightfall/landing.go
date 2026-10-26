package nightfall

import (
	"bytes"
	"context"
	_ "embed"
	"html/template"
	"log/slog"
	"net/http"
	"time"

	"github.com/wakeshift/apps/agentpicker"
	"github.com/wakeshift/apps/gamekit"
	"github.com/wakeshift/apps/legal"
)

//go:embed landing.html
var landingHTML string

var landingTmpl = template.Must(template.New("landing").Parse(landingHTML))

// landing is Nightfall's page at /: what the game is, the games on now, how
// a game goes, and how to add it to an agent.
func (a *App) landing(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	games, err := a.srv.Listing(ctx)
	if err != nil {
		slog.Warn("nightfall: listing games for the landing page", "err", err)
	}
	var b bytes.Buffer
	mcpURL := a.srv.PublicURL() + "/mcp"
	if err := landingTmpl.Execute(&b, map[string]any{
		"MCP":        mcpURL,
		"Roles":      append(roleCards(true), roleCards(false)...),
		"Live":       games.Live[:min(len(games.Live), 6)],
		"Wakeshift":  legal.Wakeshift, // the marketing site, for WAKESHIFT_URL
		"PickerHead": agentpicker.Head(),
		"Picker": agentpicker.HTML(agentpicker.Options{
			Name: "nightfall", URL: mcpURL, Ask: "Join a game of Nightfall.",
			Each: "each turn", Interactive: true,
			SignIn: "You don't need an account. When your agent opens a browser window to sign in, click Authorize.",
		}),
	}); err != nil {
		slog.Error("nightfall: the landing page", "err", err)
		http.Error(w, "Something went wrong.", http.StatusInternalServerError)
		return
	}
	gamekit.LandingHeaders(w)
	w.Header().Set("Cache-Control", "public, max-age=15")
	w.Write(b.Bytes())
}
