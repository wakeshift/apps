package nightfall

import "github.com/wakeshift/apps/gamekit"

// Nightfall's look on the follow-along and public pages, as on its landing
// page: candlelight in a dark town, parchment text, crimson by night and
// candle gold. The table has its own stylesheet (static/table.css).
var theme = &gamekit.Theme{
	Background: "#0c0910", Surface: "#17101a", Text: "#f2e6cf", Muted: "#b9a8ad", Line: "#f2e6cf24", Accent: "#e3a94f",
	Font:      `-apple-system, system-ui, "Segoe UI", sans-serif`,
	TitleFont: serif,
	CSS:       themeCSS,
}

const serif = `"Iowan Old Style", "Palatino Linotype", Palatino, "Book Antiqua", Georgia, serif`

const themeCSS = `
:root { color-scheme: dark; }
body { -webkit-font-smoothing: antialiased; }
.top { background: radial-gradient(120% 140% at 85% -20%, #4a1024 0%, #1f0c17 45%, #0c0910 100%); border-bottom: 1px solid var(--line); margin-bottom: 24px; }
.top .wrap { max-width: 960px; padding: 32px 6% 28px; }
body.tabled .top .wrap { max-width: 1440px; padding-left: 24px; padding-right: 24px; }
.top h1 { font-family: var(--title-font); font-size: clamp(34px, 5vw, 52px); font-weight: 700; letter-spacing: -.01em; line-height: 1.05; margin: 0 0 10px; }
.top h1::before { content: ""; display: inline-block; width: .5em; height: .5em; margin-right: .3em; border-radius: 50%; vertical-align: .12em;
  background: radial-gradient(circle at 38% 36%, #fffaf0, #efe6d6 60%, #c9bca2); box-shadow: 0 0 18px 4px #b3122e66; }
.top .sub { margin: 0; color: var(--muted); }
main { max-width: 960px; padding: 0 6% 56px; }
main.game.tabled { padding: 0 24px 56px; }
.card { border-radius: 18px; padding: 18px 20px; }
.card .h, .log > .h { display: block; font-weight: 700; font-size: 11px; letter-spacing: .16em; text-transform: uppercase; color: var(--accent); margin: 0 0 12px; }
.good { color: #e8c27a; } .bad { color: #ec6a7d; }
.players .player { background: #ffffff10; border-radius: 12px; }
.players .player.you { background: var(--accent); color: #1a0f08; }
.log .entry { border-top-color: var(--line); }
.mod .from { color: var(--accent); }
.time { color: #8d7d84; }
.live-dot { background: #ec6a7d; box-shadow: 0 0 8px #b3122e; }
.crumbs a { color: var(--muted); }
.games .row { background: #ffffff08; border-radius: 14px; }
.result div:first-child { font-family: var(--title-font); font-size: 20px; color: var(--accent); }
.foot .wrap { max-width: 960px; padding-left: 6%; padding-right: 6%; }
`
