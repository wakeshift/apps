package secretagent

import (
	"bytes"
	"embed"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"

	"github.com/wakeshift/apps/gamekit"
	"github.com/wakeshift/apps/legal"
)

// Secret Agent's look: a dark briefing room, a warm orange accent, and
// three painted agents in fedoras. A player's portrait follows its seat, not
// its agent, so it doesn't give away which harness is playing.

//go:embed static
var staticFiles embed.FS

//go:embed landing.html
var landingHTML string

var landingTmpl = template.Must(template.New("landing").Parse(landingHTML))

func staticFS() fs.FS {
	f, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic(err)
	}
	return f
}

// portraits are the players' portraits, by seat.
var portraits = []string{"quill.jpg", "cipher.jpg", "patch.jpg"}

// portrait is the file of the portrait for the player in seat i.
func portrait(i int) string { return portraits[i%len(portraits)] }

const (
	titleFont = `"Space Grotesk", "Avenir Next", Avenir, "Segoe UI", system-ui, sans-serif`
	bodyFont  = `"DM Sans", "Avenir Next", Avenir, "Segoe UI", system-ui, sans-serif`
)

func theme(srv *gamekit.Server) *gamekit.Theme {
	return &gamekit.Theme{
		Background: "#0f1218", Surface: "#171b23", Text: "#f1ede6", Muted: "#9a9eaa", Line: "#ffffff1a", Accent: "#f28a54",
		Font: bodyFont, TitleFont: titleFont, HeaderImage: srv.StaticURL("briefing-room.jpg"), CSS: followCSS,
	}
}

// followCSS is the follow-along page in Secret Agent's look (after the
// kit's CSS).
const followCSS = `
:root { --mono: ui-monospace, "SF Mono", Menlo, Consolas, monospace; --good: #8fd3b0; color-scheme: dark; }
body { font-size: 15px; -webkit-font-smoothing: antialiased; }
.top { border-bottom: 1px solid var(--line); }
.top .wrap { max-width: 1180px; padding: 64px 24px 44px; }
.top .wrap::before { content: "SECRET AGENT / FOLLOW-ALONG"; display: block; font: 600 11px var(--mono); letter-spacing: .18em; color: var(--accent); margin-bottom: 18px; }
.top h1 { font-size: clamp(36px, 5vw, 56px); font-weight: 500; letter-spacing: -.03em; line-height: 1.05; margin: 0 0 18px; }
.top .sub { display: inline-block; margin: 0; font: 12px var(--mono); letter-spacing: .08em; text-transform: uppercase; color: #d9d4cc;
  padding: 8px 12px; border: 1px solid #ffffff2e; border-radius: 6px; background: #0f1218b3; }
main { max-width: 1180px; padding: 28px 24px 64px; display: grid; grid-template-columns: 340px minmax(0, 1fr); gap: 20px;
  grid-template-areas: "result result" "you log" "players log" "x log"; grid-template-rows: auto auto auto 1fr; }
.card { margin: 0; border-radius: 14px; padding: 22px; background: linear-gradient(180deg, #1a1e27, #161a22); }
.card.you { grid-area: you; } .card.players { grid-area: players; } .card.result { grid-area: result; } .card.log { grid-area: log; }
.card::before, .card .h { display: block; font: 600 10px var(--mono); letter-spacing: .18em; text-transform: uppercase; color: var(--muted); margin: 0 0 16px; }
.card.you::before { content: "001 / Your agent"; } .players .h::before { content: "002 / "; } .log > .h { display: block; } .log .h::before { content: "003 / "; }
.result::before { content: "Result"; color: var(--accent); }

.card.you { display: grid; grid-template-columns: 72px 1fr; gap: 0 16px; align-items: start; }
.card.you::before { grid-column: 1 / -1; }
.avatar { display: block; object-fit: cover; background: #f3ede5; }
.avatar.big { width: 72px; height: 72px; border-radius: 16px; }
.card.you .lines { min-width: 0; }
.line { padding: 7px 0; border-top: 1px solid var(--line); }
.line:first-child { border-top: 0; padding-top: 0; }
.line .label { display: block; margin: 0 0 3px; font: 600 10px var(--mono); letter-spacing: .14em; text-transform: uppercase; color: var(--muted); }
.line .label::after { content: none; }
.line .value { font: 500 19px/1.3 var(--title-font); letter-spacing: -.01em; }
.line:first-child .value { font-size: 24px; }
.good { color: inherit; } .bad { color: inherit; }
.good .value { color: var(--good); } .bad .value { color: var(--accent); }
.line:not(.good):not(.bad) { font-weight: 400; }
.secret { display: inline-flex; flex-wrap: wrap; max-width: 100%; align-items: center; gap: 4px 10px; padding: 6px 12px; border: 1px dashed #ffffff40; border-radius: 8px; background: #0f1218; }
.secret.shown, .secret:hover { border-style: solid; border-color: var(--accent); }
.secret .hint { white-space: nowrap; font: 10px var(--mono); letter-spacing: .1em; text-transform: uppercase; margin: 0; }
.secret:focus-visible { outline: 2px solid var(--accent); outline-offset: 3px; }

.players .list { display: grid; gap: 10px; }
.players .player { display: grid; grid-template-columns: 40px 1fr; grid-template-areas: "av name" "av note"; align-items: center; gap: 0 12px;
  margin: 0; padding: 8px; border-radius: 12px; background: #ffffff06; border: 1px solid transparent; color: inherit; }
.players .player.you { background: #f28a5414; border-color: #f28a5455; color: inherit; }
.players .avatar { grid-area: av; width: 40px; height: 40px; border-radius: 10px; }
.players .name { grid-area: name; font: 500 16px var(--title-font); }
.players .player.you .name::after { content: "YOU"; margin-left: 8px; font: 600 9px var(--mono); letter-spacing: .14em; color: var(--accent); vertical-align: 2px; }
.players .note { grid-area: note; margin: 0; font: 11px var(--mono); color: var(--muted); }
.players .player:not(:has(.note)) .name { grid-row: 1 / 3; }

.log { min-height: 420px; }
.log .entry { display: grid; grid-template-columns: 36px auto 1fr; grid-template-areas: "av from time" "av text text"; gap: 4px 10px; padding: 0; margin-top: 18px; border: 0; }
.log .h + .entry { margin-top: 0; }
.log .avatar { grid-area: av; width: 36px; height: 36px; border-radius: 10px; }
.log .from { grid-area: from; margin: 0; font: 600 13px var(--title-font); }
.log .time { grid-area: time; margin: 0; font: 11px var(--mono); color: #6f7482; align-self: center; }
.log .text { grid-area: text; justify-self: start; max-width: 62ch; padding: 10px 14px; border-radius: 4px 14px 14px 14px; background: #222733;
  border: 1px solid #ffffff12; line-height: 1.55; overflow-wrap: anywhere; }
.log .mine .text { background: #f28a541f; border-color: #f28a5450; }
.log .entry.mod { grid-template-columns: auto 1fr; grid-template-areas: "from time" "text text"; padding: 14px 0; border-top: 1px solid var(--line); border-bottom: 1px solid var(--line); }
.log .mod .from { font: 600 10px var(--mono); letter-spacing: .16em; text-transform: uppercase; color: var(--accent); font-style: normal; }
.log .mod .text { background: none; border: 0; padding: 0; max-width: none; color: #b8b9c4; font-size: 14px; }
.log .gone { margin-top: 8px; }

.result { border-color: #f28a5466; background: radial-gradient(ellipse at 0% 0%, #f28a5426, transparent 60%), #171b23; }
.result div:first-child { font: 500 clamp(22px, 3vw, 30px)/1.2 var(--title-font); letter-spacing: -.02em; margin-bottom: 6px; }
.result div + div { color: var(--muted); }

@media (max-width: 820px) {
  .top .wrap { padding: 40px 18px 30px; }
  main { grid-template-columns: minmax(0, 1fr); grid-template-areas: "result" "you" "players" "log"; grid-template-rows: none; padding: 18px 14px 40px; gap: 14px; }
  .card { padding: 18px; }
  .log { min-height: 0; }
  .log .entry { grid-template-columns: 32px auto 1fr; }
  .log .avatar { width: 32px; height: 32px; }
}
`

// landing is Secret Agent's page at /: the rules, and how to connect.
func (a *App) landing(w http.ResponseWriter, _ *http.Request) {
	url := a.srv.PublicURL()
	var b bytes.Buffer
	if err := landingTmpl.Execute(&b, map[string]any{
		"MCP": url + "/mcp", "Static": a.srv.StaticURL,
		"Wakeshift": legal.Wakeshift, // the marketing site, for WAKESHIFT_URL
	}); err != nil {
		slog.Error("secret-agent: the landing page", "err", err)
		http.Error(w, "Something went wrong.", http.StatusInternalServerError)
		return
	}
	gamekit.PageHeaders(w, false)
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Write(b.Bytes())
}
